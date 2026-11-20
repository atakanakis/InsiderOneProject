package dispatch

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/events"
	"github.com/atakanakis/InsiderOneProject/internal/provider"
	"github.com/atakanakis/InsiderOneProject/internal/ratelimit"
	"github.com/atakanakis/InsiderOneProject/internal/recorder"
	"github.com/atakanakis/InsiderOneProject/internal/redisclient"
	"github.com/atakanakis/InsiderOneProject/internal/store"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

func oneWorker(t testing.TB, e *testkit.Env, srv *recorder.Server, opts Options) *runningWorker {
	t.Helper()
	return startWorker(t, e, workerSpec{
		channels: []domain.Channel{domain.SMS}, limit: 100, margin: 50 * time.Millisecond,
		opts: opts, provider: providerOptions(srv, false),
	})
}

// Each row of the verdict table, observed for real: through the real provider client, the
// real limiter and the recording server. Classify sees what a dispatcher would see.
func TestTheVerdictTableHoldsForRealSends(t *testing.T) {
	e := testkit.Setup(t)
	var answer atomic.Value // recorder.Response
	answer.Store(recorder.Response{})
	srv := testkit.Recorder(t, recorder.Options{Respond: func(recorder.Request) recorder.Response {
		return answer.Load().(recorder.Response)
	}})
	client, err := provider.New(providerOptions(srv, false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	newLimiter := func(limit int) *ratelimit.Limiter {
		rdb, err := redisclient.New(e.RedisURL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = rdb.Close() })
		l, err := ratelimit.New(rdb, limit, 50*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	lim := newLimiter(100)
	gate := func(ctx context.Context) error { return lim.Wait(ctx, domain.SMS) }
	msg := func() provider.Message {
		return provider.Message{ID: uuid.New(), To: "+905551234567", Channel: domain.SMS, Content: "x"}
	}
	check := func(name string, r provider.Result, status domain.Status, delay Delay, lesson Lesson) {
		t.Helper()
		v := Classify(r)
		if v.Status != status || v.Delay != delay || v.Lesson != lesson {
			t.Errorf("%s: %q, delay %d, lesson %d; want %q, %d, %d (result %+v)", name, v.Status, v.Delay, v.Lesson,
				status, delay, lesson, r)
		}
	}

	check("202", client.Send(bg(t), msg(), gate), domain.Sent, NoDelay, Success)
	answer.Store(recorder.Response{Status: http.StatusUnprocessableEntity})
	check("422", client.Send(bg(t), msg(), gate), domain.Failed, NoDelay, Success)
	answer.Store(recorder.Response{Status: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {"3"}}})
	r := client.Send(bg(t), msg(), gate)
	check("503", r, domain.Retrying, Backoff, Failure)
	if v := Classify(r); v.RetryAfter != 3*time.Second {
		t.Errorf("503 with Retry-After 3: the verdict carries %v", v.RetryAfter)
	}
	answer.Store(recorder.Response{Status: http.StatusNotFound})
	check("404", client.Send(bg(t), msg(), gate), domain.Retrying, Backoff, Failure)
	answer.Store(recorder.Response{Close: true})
	check("closed without an answer", client.Send(bg(t), msg(), gate), domain.Retrying, Backoff, Failure)
	answer.Store(recorder.Response{})

	// A limiter that returns an error: a worker whose limit differs from the window's.
	other := newLimiter(50)
	check("limiter error", client.Send(bg(t), msg(), func(ctx context.Context) error { return other.Wait(ctx, domain.SMS) }),
		domain.Retrying, Soon, Failure)

	// No permit within the gate budget: a window of 1 on another channel, already full.
	one := newLimiter(1)
	if err := one.Wait(bg(t), domain.Email); err != nil {
		t.Fatal(err)
	}
	o := providerOptions(srv, false)
	o.GateBudget = 200 * time.Millisecond
	short, err := provider.New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(short.CloseIdleConnections)
	check("no permit in time", short.Send(bg(t), msg(), func(ctx context.Context) error { return one.Wait(ctx, domain.Email) }),
		domain.Retrying, Soon, Nothing)

	// The provider closes the kept-alive connection while the send waits at the gate.
	if r := client.Send(bg(t), msg(), gate); !r.Responded {
		t.Fatalf("warming the connection: %+v", r)
	}
	r = client.Send(bg(t), msg(), func(ctx context.Context) error {
		waitFor(2*time.Second, func() bool { return srv.CloseIdle() > 0 })
		time.Sleep(50 * time.Millisecond) // the close reaches the client
		return lim.Wait(ctx, domain.SMS)
	})
	if !r.ClosedIdle || !r.ConnReused {
		t.Errorf("a kept-alive connection closed by the provider at the gate: %+v, want ClosedIdle on a reused connection", r)
	}
	check("closed idle", r, domain.Retrying, Soon, Nothing)

	// Nothing listens: the connection is refused.
	o = providerOptions(srv, false)
	o.URL = "http://127.0.0.1:1/send"
	nobody, err := provider.New(o)
	if err != nil {
		t.Fatal(err)
	}
	check("connection refused", nobody.Send(bg(t), msg(), gate), domain.Retrying, Backoff, Failure)
}

// A broken configuration stops the channel; it does not drain the queue into failed.
// 404 and 401 are failures of the channel: each worker's breaker opens, and while it is open
// a worker spends one attempt per open period, on its probe. Two workers, through two open
// periods: no row of the backlog fails (architecture §6, "What the breaker protects").
func TestNotFoundAndUnauthorizedOpenTheBreakerAndDoNotDrainTheQueue(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusUnauthorized} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			e := testkit.Setup(t)
			srv := testkit.Recorder(t, recorder.Options{Respond: func(recorder.Request) recorder.Response {
				return recorder.Response{Status: code}
			}})
			insertRows(t, e.Pool, domain.SMS, 100, always(domain.Normal), 3)
			opts := testOptions(t)
			opts.BreakerOpen = 1500 * time.Millisecond
			var ds []*Dispatcher
			for range 2 {
				ds = append(ds, oneWorker(t, e, srv, opts).dispatchers[domain.SMS])
			}
			if !waitFor(5*time.Second, func() bool { return ds[0].breaker.State() != Closed && ds[1].breaker.State() != Closed }) {
				t.Fatalf("the breakers did not open; the provider received %d requests", srv.Received())
			}
			// Rows that started before a breaker opened may still arrive; count from when none is
			// in flight (as in TestTheHalfOpenBreakerLetsExactlyOneRowThrough).
			if !waitFor(5*time.Second, func() bool { return statusCounts(t, e.Pool)[domain.Processing] == 0 }) {
				t.Fatal("rows of before the opening are still in flight")
			}
			opened, began := srv.Received(), time.Now()
			time.Sleep(2*opts.BreakerOpen + 500*time.Millisecond) // two open periods and more
			counts := statusCounts(t, e.Pool)
			if counts[domain.Failed] != 0 {
				t.Errorf("%d rows failed: a %d drained the queue", counts[domain.Failed], code)
			}
			// Each worker sends at most one probe per open period that began in the window.
			most := 2 * (int(time.Since(began)/opts.BreakerOpen) + 1)
			if probes := srv.Received() - opened; probes > most {
				t.Errorf("%d requests in %v of two workers whose breakers are open; want at most the probes, %d",
					probes, time.Since(began).Round(100*time.Millisecond), most)
			}
			t.Logf("received %d before the breakers opened, %d after; statuses %v", opened, srv.Received()-opened, counts)
		})
	}
}

// Architecture §7: a Redis that refuses writes (out of memory here) makes Available fail as
// well, so nothing is claimed, no attempt is spent and nothing is sent, for longer than an
// open period of the breaker (session 5's review, F2: with Available still answering, every
// probe spent an attempt and rows ended failed without a send).
func TestARedisThatRefusesWritesClaimsNothing(t *testing.T) {
	e := testkit.Setup(t)
	srv := testkit.Recorder(t, recorder.Options{})
	if err := e.Redis.ConfigSet(bg(t), "maxmemory", "1").Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Redis.ConfigSet(context.Background(), "maxmemory", "0").Err() })
	insertRows(t, e.Pool, domain.SMS, 30, always(domain.Normal), 2)
	opts := testOptions(t)
	opts.BreakerOpen = time.Second
	oneWorker(t, e, srv, opts)
	time.Sleep(3 * time.Second)
	var attempts int
	if err := e.Pool.QueryRow(bg(t), `SELECT sum(attempts) FROM notifications`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	counts := statusCounts(t, e.Pool)
	if srv.Received() != 0 || attempts != 0 || counts[domain.Failed] != 0 {
		t.Errorf("received %d, attempts %d, failed %d while Redis refused writes; want none", srv.Received(), attempts, counts[domain.Failed])
	}
}

// flakyFinish fails the first result writes of every row: the first before it commits, the
// second after it commits (its answer is lost), the third before it commits again.
type flakyFinish struct {
	Queue
	lostAfterCommit bool
	mu              sync.Mutex
	calls           map[uuid.UUID]int
}

func (q *flakyFinish) Finish(ctx context.Context, id uuid.UUID, attempt int, r store.Result) (domain.Status, bool, error) {
	q.mu.Lock()
	q.calls[id]++
	n := q.calls[id]
	q.mu.Unlock()
	switch {
	case n == 1 || n == 3:
		return "", false, errInjected
	case n == 2 && q.lostAfterCommit:
		_, _, err := q.Queue.Finish(ctx, id, attempt, r)
		if err != nil {
			return "", false, err
		}
		return "", false, errInjected
	case n == 2:
		return "", false, errInjected
	}
	return q.Queue.Finish(ctx, id, attempt, r)
}

// Lessons 7: a result write that fails after a successful send is retried until the database
// answers, and the message is not sent again.
func TestAFailedResultWriteIsRetriedAndNotSentAgain(t *testing.T) {
	for _, lost := range []bool{false, true} {
		name := "fails before it commits"
		if lost {
			name = "commits and its answer is lost"
		}
		t.Run(name, func(t *testing.T) {
			e := testkit.Setup(t)
			acc := newAccepted()
			srv := testkit.Recorder(t, recorder.Options{Respond: acc.respond(nil)})
			ids := insertRows(t, e.Pool, domain.SMS, 20, always(domain.Normal), 6)
			q := &flakyFinish{lostAfterCommit: lost, calls: map[uuid.UUID]int{}}
			w := startWorker(t, e, workerSpec{
				channels: []domain.Channel{domain.SMS}, limit: 100, margin: 50 * time.Millisecond,
				opts: testOptions(t), provider: providerOptions(srv, false), reaperTick: 100 * time.Millisecond,
				wrapQueue: func(inner Queue) Queue { q.Queue = inner; return q },
			})
			if counts, ok := waitAllFinal(t, e.Pool, 20*time.Second); !ok || counts[domain.Sent] != len(ids) {
				t.Fatalf("statuses %v, want all %d sent", counts, len(ids))
			}
			// A write whose commit was lost is still being retried when the row shows sent;
			// stopping waits for the rows in flight, and so for their result writes.
			if !w.stop(10 * time.Second) {
				t.Fatal("the worker did not stop")
			}
			for _, id := range ids {
				if n := acc.of(id.String()); n != 1 {
					t.Errorf("%s was accepted %d times, want once", id, n)
				}
				if r := rowOf(t, e.Pool, id); r.Attempts != 1 {
					t.Errorf("%s took %d attempts, want 1", id, r.Attempts)
				}
				q.mu.Lock()
				calls := q.calls[id]
				q.mu.Unlock()
				if calls < 4 {
					t.Errorf("%s: %d result writes, want the write retried until it was answered", id, calls)
				}
			}
			if srv.Received() != len(ids) {
				t.Errorf("the provider received %d requests for %d notifications", srv.Received(), len(ids))
			}
		})
	}
}

// Architecture §6, shutdown: the loop stops claiming, and the sends in flight finish; they
// are not cancelled, and their results are stored.
func TestShutdownWaitsForSendsInFlightAndDoesNotCancelThem(t *testing.T) {
	e := testkit.Setup(t)
	acc := newAccepted()
	srv := testkit.Recorder(t, recorder.Options{Respond: acc.respond(func(recorder.Request) recorder.Response {
		return recorder.Response{Delay: 1500 * time.Millisecond}
	})})
	ids := insertRows(t, e.Pool, domain.SMS, 5, always(domain.Normal), 6)
	w := oneWorker(t, e, srv, testOptions(t))
	if !waitFor(10*time.Second, func() bool { return srv.Received() == len(ids) }) {
		t.Fatalf("the provider received %d of %d", srv.Received(), len(ids))
	}
	began := time.Now()
	if !w.stop(10 * time.Second) {
		t.Fatal("the worker did not stop")
	}
	if took := time.Since(began); took < time.Second {
		t.Errorf("the worker stopped after %v, before its sends in flight (1.5 s) had finished", took)
	}
	for _, id := range ids {
		if r := rowOf(t, e.Pool, id); r.Status != domain.Sent {
			t.Errorf("%s is %s after shutdown, want sent: the send or its result write was cut off (%s)", id, r.Status, r.LastError)
		}
	}
}

// I9: a due row of a higher priority is claimed before a due row of a lower one. High-priority
// rows that become due behind a backlog of low ones overtake it: only the low rows already
// claimed when they arrived go before them.
func TestHighPriorityOvertakesBacklog(t *testing.T) {
	const limit = 20
	e := testkit.Setup(t)
	srv := testkit.Recorder(t, recorder.Options{})
	insertRows(t, e.Pool, domain.SMS, 200, always(domain.Low), 6)
	opts := testOptions(t)
	startWorker(t, e, workerSpec{
		channels: []domain.Channel{domain.SMS}, limit: limit, margin: 50 * time.Millisecond,
		opts: opts, provider: providerOptions(srv, false),
	})
	if !waitFor(10*time.Second, func() bool { return srv.Received() >= limit }) {
		t.Fatal("the backlog is not being sent")
	}
	due := time.Now()
	high := insertRows(t, e.Pool, domain.SMS, 10, always(domain.High), 6)
	isHigh := map[string]bool{}
	for _, id := range high {
		isHigh[id.String()] = true
	}
	var lastHigh time.Time
	if !waitFor(10*time.Second, func() bool {
		n := 0
		for _, r := range srv.Requests() {
			if isHigh[r.Header.Get("Idempotency-Key")] {
				n++
				lastHigh = r.Arrived
			}
		}
		return n == len(high)
	}) {
		t.Fatal("the high-priority rows did not all arrive")
	}
	lowBefore := 0
	for _, r := range srv.Requests() {
		if !isHigh[r.Header.Get("Idempotency-Key")] && r.Arrived.After(due) && r.Arrived.Before(lastHigh) {
			lowBefore++
		}
	}
	t.Logf("%d low-priority requests arrived after the high-priority rows were due and before the last of them (%v later)",
		lowBefore, lastHigh.Sub(due).Round(time.Millisecond))
	if lowBefore > 2*opts.ClaimBatch {
		t.Errorf("%d low-priority rows went before the high-priority ones; at most the rows already claimed (%d) should",
			lowBefore, 2*opts.ClaimBatch)
	}
}

// waitingCounter counts the rows a worker has claimed and that have not come back from the
// gate yet: claimed and not yet permitted.
type waitingCounter struct {
	n, most atomic.Int64
}

type countingQueue struct {
	Queue
	c *waitingCounter
}

func (q countingQueue) Claim(ctx context.Context, ch domain.Channel, p domain.Priority, n int, lease time.Duration) ([]store.Claimed, error) {
	rows, err := q.Queue.Claim(ctx, ch, p, n, lease)
	now := q.c.n.Add(int64(len(rows)))
	for {
		most := q.c.most.Load()
		if now <= most || q.c.most.CompareAndSwap(most, now) {
			break
		}
	}
	return rows, err
}

type countingLimiter struct {
	Limiter
	c *waitingCounter
}

func (l countingLimiter) Wait(ctx context.Context, ch domain.Channel) error {
	err := l.Limiter.Wait(ctx, ch)
	l.c.n.Add(-1)
	return err
}

// I9's second half, the claim batch rule: a worker has at most one claim batch that is
// claimed and not yet permitted. Here every send needs a new connection whose handshake the
// provider holds back, so rows wait before the gate while Available still shows room.
func TestAWorkerHasAtMostOneClaimBatchWaitingForPermits(t *testing.T) {
	e := testkit.Setup(t)
	srv := testkit.Recorder(t, recorder.Options{TLS: true, HandshakeDelay: 300 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { // no kept-alive connection survives: every send dials
		for ctx.Err() == nil {
			srv.CloseIdle()
			time.Sleep(5 * time.Millisecond)
		}
	}()
	insertRows(t, e.Pool, domain.SMS, 300, always(domain.Normal), 6)
	c := &waitingCounter{}
	opts := testOptions(t)
	rdb, err := redisclient.New(e.RedisURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	lim, err := ratelimit.New(rdb, 100, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	opts.Channel, opts.Provider = domain.SMS, providerOptions(srv, true)
	d, err := New(opts, countingQueue{Queue: store.NewQueue(e.Pool), c: c}, countingLimiter{Limiter: lim, c: c})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	d.Run(runCtx)
	t.Logf("the provider received %d; at most %d rows were claimed and not yet permitted", srv.Received(), c.most.Load())
	if srv.Received() < 30 {
		t.Fatalf("only %d sends in 3 s: the test did not run", srv.Received())
	}
	if most := c.most.Load(); most > int64(opts.ClaimBatch) {
		t.Errorf("%d rows were claimed and not yet permitted at once; the claim batch is %d", most, opts.ClaimBatch)
	}
}

// switchable is a provider answer that a test changes while the workers run.
type switchable struct{ v atomic.Value }

func (s *switchable) set(r recorder.Response) { s.v.Store(r) }
func (s *switchable) respond(recorder.Request) recorder.Response {
	r, _ := s.v.Load().(recorder.Response)
	return r
}

// The half-open breaker lets exactly one row through: while the probe is out, nothing else
// reaches the provider; after it succeeds, the channel flows again.
func TestTheHalfOpenBreakerLetsExactlyOneRowThrough(t *testing.T) {
	e := testkit.Setup(t)
	var answer switchable
	answer.set(recorder.Response{Status: http.StatusServiceUnavailable})
	srv := testkit.Recorder(t, recorder.Options{Respond: answer.respond})
	ids := insertRows(t, e.Pool, domain.SMS, 40, always(domain.Normal), 10)
	w := oneWorker(t, e, srv, testOptions(t))
	d := w.dispatchers[domain.SMS]
	if !waitFor(5*time.Second, func() bool { return d.breaker.State() == Open }) {
		t.Fatal("the breaker did not open")
	}
	// Rows that started before the breaker opened may still reach the provider; the probe is
	// the first request after all of them (session 5's review, F3).
	if !waitFor(5*time.Second, func() bool { return statusCounts(t, e.Pool)[domain.Processing] == 0 }) {
		t.Fatal("rows of before the opening are still in flight")
	}
	if d.breaker.State() != Open {
		t.Fatal("the breaker is no longer open; the test came too late")
	}
	opened := srv.Received()
	const probeTime = 800 * time.Millisecond
	answer.set(recorder.Response{Delay: probeTime}) // healthy, and slow
	if counts, ok := waitAllFinal(t, e.Pool, 30*time.Second); !ok || counts[domain.Sent] != len(ids) {
		t.Fatalf("statuses %v after the probe succeeded, want all %d sent", counts, len(ids))
	}
	// The first request after the breaker opened is the probe. Until its answer left the
	// provider, probeTime after it arrived, no other request may arrive. Counted from the
	// provider's stamps, so a slow machine cannot make the test pass or fail by itself.
	after := srv.Requests()[opened:]
	slices.SortFunc(after, func(a, b recorder.Request) int { return a.Arrived.Compare(b.Arrived) })
	during := 0
	for _, r := range after[1:] {
		if r.Arrived.Before(after[0].Arrived.Add(probeTime)) {
			during++
		}
	}
	if during != 0 {
		t.Errorf("%d requests reached the provider while the probe was out, want none", during)
	}
	if d.breaker.State() != Closed {
		t.Errorf("breaker state %d, want closed", d.breaker.State())
	}
}

// A claim that finds nothing marks no probe: a half-open breaker whose claims find an empty
// queue still lets the next row through when one becomes due.
func TestAClaimThatFindsNothingLeavesNoProbe(t *testing.T) {
	e := testkit.Setup(t)
	var answer switchable
	answer.set(recorder.Response{Status: http.StatusServiceUnavailable})
	srv := testkit.Recorder(t, recorder.Options{Respond: answer.respond})
	insertRows(t, e.Pool, domain.SMS, 10, always(domain.Normal), 1) // one attempt: they fail, the queue empties
	w := oneWorker(t, e, srv, testOptions(t))
	d := w.dispatchers[domain.SMS]
	if !waitFor(5*time.Second, func() bool { return d.breaker.State() == Open }) {
		t.Fatal("the breaker did not open")
	}
	if !waitFor(10*time.Second, func() bool { return d.breaker.State() == HalfOpen }) {
		t.Fatal("the breaker did not become half-open")
	}
	time.Sleep(500 * time.Millisecond) // claims that find nothing, every idle pause
	answer.set(recorder.Response{})
	id := insertRows(t, e.Pool, domain.SMS, 1, always(domain.Normal), 6)[0]
	if !waitFor(5*time.Second, func() bool { return rowOf(t, e.Pool, id).Status == domain.Sent }) {
		t.Fatalf("the row is %s: an empty claim left a probe marked", rowOf(t, e.Pool, id).Status)
	}
	if d.breaker.State() != Closed {
		t.Errorf("breaker state %d after the probe succeeded, want closed", d.breaker.State())
	}
}

// recordingEvents keeps every status event it is given.
type recordingEvents struct {
	mu  sync.Mutex
	got map[uuid.UUID]domain.Status
}

func (r *recordingEvents) Publish(_ context.Context, e events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got[e.ID] = e.Status
}

func (r *recordingEvents) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

// The reaper returns rows whose lease has expired, in batches until a batch comes back short,
// and publishes each change.
func TestTheReaperReturnsExpiredRowsAndPublishesTheirStatus(t *testing.T) {
	e := testkit.Setup(t)
	ids := insertRows(t, e.Pool, domain.SMS, 250, always(domain.Normal), 6)
	// A worker held them and died: processing, with leases that ended; 50 at their last attempt.
	if _, err := e.Pool.Exec(bg(t), `UPDATE notifications SET status = 'processing', attempts = 1,
    locked_until = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Pool.Exec(bg(t), `UPDATE notifications SET attempts = 6 WHERE id = ANY($1)`, ids[:50]); err != nil {
		t.Fatal(err)
	}
	ev := &recordingEvents{got: map[uuid.UUID]domain.Status{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Reaper{Queue: store.NewQueue(e.Pool), Tick: time.Second, Log: testLog(t), Events: ev}.Run(ctx)
		close(done)
	}()
	// One tick returns all 250, in batches of 100, before the second tick at 2 s.
	ok := waitFor(1800*time.Millisecond, func() bool { return ev.count() == len(ids) })
	cancel()
	<-done
	if !ok {
		t.Fatalf("%d of %d changes published", ev.count(), len(ids))
	}
	counts := statusCounts(t, e.Pool)
	if counts[domain.Pending] != 200 || counts[domain.Failed] != 50 {
		t.Errorf("statuses %v, want 200 pending and 50 failed", counts)
	}
	for _, id := range ids[:50] {
		if ev.got[id] != domain.Failed {
			t.Errorf("%s published as %s, want failed", id, ev.got[id])
		}
	}
	if ev.got[ids[60]] != domain.Pending {
		t.Errorf("%s published as %s, want pending", ids[60], ev.got[ids[60]])
	}
}

// New refuses settings that would make the loop misbehave, and a missing part.
func TestNewRefusesSettingsThatAreNotPositive(t *testing.T) {
	srv := testkit.Recorder(t, recorder.Options{})
	rdb, err := redisclient.New("redis://127.0.0.1:1/0") // never dialled
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	lim, err := ratelimit.New(rdb, 100, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	q := store.NewQueue(nil)
	valid := func() Options {
		o := testOptions(t)
		o.Channel, o.Provider = domain.SMS, providerOptions(srv, false)
		return o
	}
	if _, err := New(valid(), q, lim); err != nil {
		t.Fatalf("New refused valid options: %v", err)
	}
	for i, mutate := range []func(*Options){
		func(o *Options) { o.Slots = 0 },
		func(o *Options) { o.ClaimBatch = 0 },
		func(o *Options) { o.ClaimBatch = o.Slots + 1 },
		func(o *Options) { o.Lease = 0 },
		func(o *Options) { o.PauseBusy = 0 },
		func(o *Options) { o.PauseIdle = 0 },
		func(o *Options) { o.BreakerOpen = 0 },
		func(o *Options) { o.Retry.Base = 0 },
		func(o *Options) { o.Retry.Cap = 0 },
		func(o *Options) { o.Retry.Soon = 0 },
		func(o *Options) { o.Channel = "fax" },
		func(o *Options) { o.Provider.URL = "" },
	} {
		o := valid()
		mutate(&o)
		if _, err := New(o, q, lim); err == nil {
			t.Errorf("case %d: New accepted %+v", i, o)
		}
	}
	if _, err := New(valid(), nil, lim); err == nil {
		t.Error("New accepted no queue")
	}
	if _, err := New(valid(), q, nil); err == nil {
		t.Error("New accepted no limiter")
	}
}

// Architecture §7: the limiter fails closed. When Available fails (here: a worker whose limit
// differs from the window's), room is 0 and nothing is claimed, so no attempt is spent.
func TestAnUnavailableRateLimitClaimsNothing(t *testing.T) {
	e := testkit.Setup(t)
	srv := testkit.Recorder(t, recorder.Options{})
	other, err := ratelimit.New(e.Redis, 50, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	insertRows(t, e.Pool, domain.SMS, 20, always(domain.Normal), 6)
	stopFill := make(chan struct{})
	filled := make(chan struct{})
	go func() { // the window keeps the other worker's settings: limit 50
		defer close(filled)
		for {
			_ = other.Wait(bg(t), domain.SMS)
			select {
			case <-stopFill:
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
	defer func() { close(stopFill); <-filled }()
	time.Sleep(100 * time.Millisecond)
	oneWorker(t, e, srv, testOptions(t)) // limit 100
	time.Sleep(1500 * time.Millisecond)
	var attempts int
	if err := e.Pool.QueryRow(bg(t), `SELECT sum(attempts) FROM notifications`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || srv.Received() != 0 {
		t.Errorf("%d attempts, %d requests received while the rate limit was unavailable; want none", attempts, srv.Received())
	}
}

// A row that failed waits for its retry delay before its next attempt.
func TestARetriedRowWaitsForItsBackoff(t *testing.T) {
	e := testkit.Setup(t)
	var seen sync.Map
	srv := testkit.Recorder(t, recorder.Options{Respond: func(r recorder.Request) recorder.Response {
		if _, again := seen.LoadOrStore(r.Header.Get("Idempotency-Key"), true); !again {
			return recorder.Response{Status: http.StatusServiceUnavailable}
		}
		return recorder.Response{}
	}})
	id := insertRows(t, e.Pool, domain.SMS, 1, always(domain.Normal), 6)[0]
	opts := testOptions(t)
	opts.Retry.Base, opts.Retry.Cap = 2*time.Second, 10*time.Second // the first retry: between 1 s and 2 s
	oneWorker(t, e, srv, opts)
	if !waitFor(10*time.Second, func() bool { return rowOf(t, e.Pool, id).Status == domain.Sent }) {
		t.Fatalf("the row is %s", rowOf(t, e.Pool, id).Status)
	}
	reqs := srv.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	if gap := reqs[1].Arrived.Sub(reqs[0].Arrived); gap < time.Second {
		t.Errorf("the retry arrived %v after the failed attempt; its delay was at least 1 s", gap)
	}
	if got := attemptOutcomes(t, e.Pool, id); len(got) != 2 || got[0] != "error response" || got[1] != "sent" {
		t.Errorf("attempt history %v, want [error response sent]", got)
	}
}

// room reads Available: while the window is full, nothing is claimed, so no row waits at the
// gate and spends an attempt on a refusal. The window here holds the limit of 1 for 1.05 s,
// and the gate budget is 200 ms: a row claimed in that time would be refused.
func TestNoRowIsClaimedWhileTheWindowIsFull(t *testing.T) {
	e := testkit.Setup(t)
	srv := testkit.Recorder(t, recorder.Options{})
	filler, err := ratelimit.New(e.Redis, 1, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	insertRows(t, e.Pool, domain.SMS, 20, always(domain.Normal), 6)
	if err := filler.Wait(bg(t), domain.SMS); err != nil {
		t.Fatal(err)
	}
	full := time.Now()
	o := providerOptions(srv, false)
	o.GateBudget = 200 * time.Millisecond
	startWorker(t, e, workerSpec{
		channels: []domain.Channel{domain.SMS}, limit: 1, margin: 50 * time.Millisecond,
		opts: testOptions(t), provider: o,
	})
	time.Sleep(time.Until(full.Add(700 * time.Millisecond))) // the window is still full
	var attempts int
	if err := e.Pool.QueryRow(bg(t), `SELECT sum(attempts) FROM notifications`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Errorf("%d attempts while the window was full; want none", attempts)
	}
}
