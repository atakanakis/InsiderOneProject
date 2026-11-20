package dispatch

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/events"
	"github.com/atakanakis/InsiderOneProject/internal/observability"
	"github.com/atakanakis/InsiderOneProject/internal/provider"
	"github.com/atakanakis/InsiderOneProject/internal/ratelimit"
	"github.com/atakanakis/InsiderOneProject/internal/recorder"
	"github.com/atakanakis/InsiderOneProject/internal/redisclient"
	"github.com/atakanakis/InsiderOneProject/internal/store"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

// The tests of this package run real workers: each has its own Postgres pool, Redis client,
// limiter and provider client, as separate worker processes would. Every send goes through
// the real provider client to a recording server, which stamps each request on arrival
// (CLAUDE.md, rule 9). The queue and the limiter may be wrapped, to inject faults and to
// stamp permits; the real ones are always underneath.

func TestMain(m *testing.M) { testkit.Main(m) }

func bg(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// testLog is a logger for the dispatchers of a test: warnings and errors only, written to the
// test's log so that a failure shows them.
func testLog(t testing.TB) *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

type testWriter struct{ t testing.TB }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	defer func() { _ = recover() }() // a line written after the test ended
	w.t.Log(string(p))
	return len(p), nil
}

// testOptions are a dispatcher's settings in the tests: the defaults of architecture §13,
// with the waits shortened so that a test is quick.
func testOptions(t testing.TB) Options {
	return Options{
		Slots:       50,
		ClaimBatch:  10,
		Lease:       20 * time.Second,
		PauseBusy:   20 * time.Millisecond,
		PauseIdle:   50 * time.Millisecond,
		BreakerOpen: 3500 * time.Millisecond,
		Retry:       RetryPolicy{Base: 100 * time.Millisecond, Cap: time.Second, Soon: time.Second},
		Log:         testLog(t),
		Events:      events.Discard{},
	}
}

func providerOptions(srv *recorder.Server, useTLS bool) provider.Options {
	o := provider.Options{
		URL:                 srv.URL + "/send",
		SendTimeout:         3 * time.Second,
		GateBudget:          2 * time.Second,
		DialTimeout:         time.Second,
		TLSHandshakeTimeout: time.Second,
		IdleConns:           50,
		IdleConnTimeout:     10 * time.Second,
	}
	if useTLS {
		o.TLSConfig = &tls.Config{RootCAs: srv.CertPool(), MinVersion: tls.VersionTLS12}
	}
	return o
}

// accepted counts, per notification id, the requests the provider answered with 2xx.
type accepted struct {
	mu     sync.Mutex
	counts map[string]int
}

func newAccepted() *accepted { return &accepted{counts: map[string]int{}} }

// respond wraps a recorder's Respond so that every 2xx answer it decides is counted.
func (a *accepted) respond(next func(recorder.Request) recorder.Response) func(recorder.Request) recorder.Response {
	return func(r recorder.Request) recorder.Response {
		var resp recorder.Response
		if next != nil {
			resp = next(r)
		}
		if !resp.Close && (resp.Status == 0 || resp.Status/100 == 2) {
			a.mu.Lock()
			a.counts[r.Header.Get("Idempotency-Key")]++
			a.mu.Unlock()
		}
		return resp
	}
}

func (a *accepted) of(id string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.counts[id]
}

// twice returns the ids the provider accepted more than once.
func (a *accepted) twice() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var ids []string
	for id, n := range a.counts {
		if n > 1 {
			ids = append(ids, id)
		}
	}
	return ids
}

// stamps records, for every worker of a test, when each permit was granted and how long each
// wait at the gate took. The notification id reaches the gate in the context, as the
// correlation id: the tests give every row its own id as correlation id.
type stamps struct {
	mu         sync.Mutex
	permits    map[string][]time.Time
	waits      []time.Duration
	noPermit   atomic.Int64 // ErrNoPermitInTime
	limiterErr atomic.Int64 // any other error of Wait
}

func newStamps() *stamps { return &stamps{permits: map[string][]time.Time{}} }

// stampingLimiter is the real limiter, with every Wait stamped.
type stampingLimiter struct {
	Limiter
	s *stamps
}

func (l stampingLimiter) Wait(ctx context.Context, ch domain.Channel) error {
	began := time.Now()
	err := l.Limiter.Wait(ctx, ch)
	now := time.Now()
	l.s.mu.Lock()
	l.s.waits = append(l.s.waits, now.Sub(began))
	if err == nil {
		id := observability.CorrelationID(ctx)
		l.s.permits[id] = append(l.s.permits[id], now)
	}
	l.s.mu.Unlock()
	switch {
	case errors.Is(err, ratelimit.ErrNoPermitInTime):
		l.s.noPermit.Add(1)
	case err != nil:
		l.s.limiterErr.Add(1)
	}
	return err
}

// delays returns, for every request of one channel, the time from its permit to its arrival:
// the latest permit of the same notification granted before the arrival.
func (s *stamps) delays(reqs []recorder.Request, channel string) []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []time.Duration
	for _, r := range reqs {
		if r.Channel != channel {
			continue
		}
		var permit time.Time
		for _, p := range s.permits[r.Header.Get("Idempotency-Key")] {
			if !p.After(r.Arrived) && p.After(permit) {
				permit = p
			}
		}
		if !permit.IsZero() {
			out = append(out, r.Arrived.Sub(permit))
		}
	}
	slices.Sort(out)
	return out
}

// late counts the requests of one channel that arrived more than margin after their permit.
func (s *stamps) late(reqs []recorder.Request, channel string, margin time.Duration) (late int, worst time.Duration) {
	for _, d := range s.delays(reqs, channel) {
		worst = max(worst, d)
		if d > margin {
			late++
		}
	}
	return late, worst
}

// percentile returns the p-th percentile (0 to 100) of sorted durations.
func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[min(len(sorted)-1, len(sorted)*p/100)]
}

func (s *stamps) waitStats() (n int, median, longest time.Duration) {
	ws := s.sortedWaits()
	if len(ws) == 0 {
		return 0, 0, 0
	}
	return len(ws), ws[len(ws)/2], ws[len(ws)-1]
}

func (s *stamps) sortedWaits() []time.Duration {
	s.mu.Lock()
	ws := slices.Clone(s.waits)
	s.mu.Unlock()
	slices.Sort(ws)
	return ws
}

// workerSpec describes one worker of a test.
type workerSpec struct {
	channels   []domain.Channel
	limit      int
	margin     time.Duration
	opts       Options
	provider   provider.Options
	wrapQueue  func(Queue) Queue
	stamps     *stamps
	reaperTick time.Duration // 0: no reaper
}

type runningWorker struct {
	cancel      context.CancelFunc
	done        chan struct{}
	dispatchers map[domain.Channel]*Dispatcher
	limiter     *ratelimit.Limiter
}

// startWorker starts a worker with its own pool, Redis client, limiter and provider client,
// and stops it when the test ends.
func startWorker(t testing.TB, env *testkit.Env, spec workerSpec) *runningWorker {
	t.Helper()
	pool, err := store.Connect(bg(t), env.DatabaseURL, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rdb, err := redisclient.New(env.RedisURL)
	if err != nil {
		t.Fatal(err)
	}
	lim, err := ratelimit.New(rdb, spec.limit, spec.margin)
	if err != nil {
		t.Fatal(err)
	}
	client, err := provider.New(spec.provider)
	if err != nil {
		t.Fatal(err)
	}
	var q Queue = store.NewQueue(pool)
	if spec.wrapQueue != nil {
		q = spec.wrapQueue(q)
	}
	var l Limiter = lim
	if spec.stamps != nil {
		l = stampingLimiter{Limiter: lim, s: spec.stamps}
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &runningWorker{cancel: cancel, done: make(chan struct{}), dispatchers: map[domain.Channel]*Dispatcher{}, limiter: lim}
	var wg sync.WaitGroup
	for _, ch := range spec.channels {
		o := spec.opts
		o.Channel = ch
		d, err := New(o, q, l, client)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		w.dispatchers[ch] = d
		wg.Go(func() { d.Run(ctx) })
	}
	if spec.reaperTick > 0 {
		r := Reaper{Queue: store.NewQueue(pool), Tick: spec.reaperTick, Log: spec.opts.Log, Events: events.Discard{}}
		wg.Go(func() { r.Run(ctx) })
	}
	go func() {
		wg.Wait()
		client.CloseIdleConnections()
		_ = rdb.Close()
		pool.Close()
		close(w.done)
	}()
	t.Cleanup(func() { w.stop(30 * time.Second) })
	return w
}

// stop cancels the worker and waits up to timeout for it to finish; it reports whether it did.
func (w *runningWorker) stop(timeout time.Duration) bool {
	w.cancel()
	select {
	case <-w.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// insertRows adds n pending rows of one channel, due now, and returns their ids. Each row's
// correlation id is its own id, so that the gate's stamps can name the row.
func insertRows(t testing.TB, pool *pgxpool.Pool, ch domain.Channel, n int, priority func(i int) domain.Priority, maxAttempts int) []uuid.UUID {
	t.Helper()
	ids := make([]uuid.UUID, n)
	prios := make([]int16, n)
	for i := range n {
		ids[i] = uuid.New()
		prios[i] = int16(priority(i))
	}
	_, err := pool.Exec(bg(t), `INSERT INTO notifications
    (id, recipient, channel, content, priority, status, max_attempts, ready_at, correlation_id)
SELECT u, '+905551234567', $2, 'Your code is 1234', p, 'pending', $4, now(), u::text
FROM unnest($1::uuid[], $3::smallint[]) AS t(u, p)`, ids, string(ch), prios, maxAttempts)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func always(p domain.Priority) func(int) domain.Priority {
	return func(int) domain.Priority { return p }
}

// statusCounts returns how many rows are in each status.
func statusCounts(t testing.TB, pool *pgxpool.Pool) map[domain.Status]int {
	t.Helper()
	rows, err := pool.Query(bg(t), `SELECT status, count(*) FROM notifications GROUP BY status`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[domain.Status]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			t.Fatal(err)
		}
		out[domain.Status(s)] = n
	}
	return out
}

// waitAllFinal waits until every row is sent, failed or cancelled, and returns the counts.
func waitAllFinal(t testing.TB, pool *pgxpool.Pool, timeout time.Duration) (map[domain.Status]int, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		c := statusCounts(t, pool)
		if c[domain.Scheduled]+c[domain.Pending]+c[domain.Processing]+c[domain.Retrying] == 0 {
			return c, true
		}
		if time.Now().After(deadline) {
			return c, false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitFor polls cond every 10 ms until it holds or timeout passes.
func waitFor(timeout time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(timeout); ; time.Sleep(10 * time.Millisecond) {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// rowState is one row as the queue holds it.
type rowState struct {
	Status    domain.Status
	Attempts  int
	LastError string
}

func rowOf(t testing.TB, pool *pgxpool.Pool, id uuid.UUID) rowState {
	t.Helper()
	var r rowState
	var status string
	var lastErr *string
	if err := pool.QueryRow(bg(t), `SELECT status, attempts, last_error FROM notifications WHERE id = $1`, id).
		Scan(&status, &r.Attempts, &lastErr); err != nil {
		t.Fatal(err)
	}
	r.Status = domain.Status(status)
	if lastErr != nil {
		r.LastError = *lastErr
	}
	return r
}

// attemptOutcomes returns the outcomes of a row's attempt rows, in attempt order.
func attemptOutcomes(t testing.TB, pool *pgxpool.Pool, id uuid.UUID) []string {
	t.Helper()
	rows, err := pool.Query(bg(t), `SELECT outcome FROM delivery_attempts WHERE notification_id = $1 ORDER BY attempt_number`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			t.Fatal(err)
		}
		out = append(out, o)
	}
	return out
}

// everyClaimReachedFinish checks I4's first half from the database: every claim (the sum of
// attempts) has its attempt row, written only by Finish, and no lease expired.
func everyClaimReachedFinish(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	var claims, finished, expired int
	if err := pool.QueryRow(bg(t), `SELECT
    (SELECT coalesce(sum(attempts), 0) FROM notifications),
    (SELECT count(*) FROM delivery_attempts),
    (SELECT count(*) FROM notifications WHERE last_error = 'lease expired')`).Scan(&claims, &finished, &expired); err != nil {
		t.Fatal(err)
	}
	if claims != finished || expired != 0 {
		t.Errorf("%d claims, %d attempt rows written by Finish, %d leases expired: a claimed row did not reach Finish", claims, finished, expired)
	}
}
