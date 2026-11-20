package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/events"
	"github.com/atakanakis/InsiderOneProject/internal/observability"
	"github.com/atakanakis/InsiderOneProject/internal/recorder"
	"github.com/atakanakis/InsiderOneProject/internal/redisclient"
	"github.com/atakanakis/InsiderOneProject/internal/store"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

// The tests of architecture §9 and §10 on the worker's side: status events, metrics, logs.

func newMetrics() (*prometheus.Registry, *observability.WorkerMetrics) {
	reg := prometheus.NewRegistry()
	return reg, observability.NewWorkerMetrics(reg)
}

// gauge reads the gauge name{channel=ch} from reg; ok is false when it is not exported.
func gauge(t testing.TB, reg *prometheus.Registry, name string, ch domain.Channel) (float64, bool) {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "channel" && l.GetValue() == string(ch) {
					return m.GetGauge().GetValue(), true
				}
			}
		}
	}
	return 0, false
}

// histogram reads the count, the sum and the cumulative count up to le of name{channel=ch}.
func histogram(t testing.TB, reg *prometheus.Registry, name string, ch domain.Channel, le float64) (count uint64, sum float64, upTo uint64) {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if len(m.GetLabel()) != 1 || m.GetLabel()[0].GetValue() != string(ch) {
				continue
			}
			h := m.GetHistogram()
			for _, b := range h.GetBucket() {
				if b.GetUpperBound() == le {
					upTo = b.GetCumulativeCount()
				}
			}
			return h.GetSampleCount(), h.GetSampleSum(), upTo
		}
	}
	t.Fatalf("%s{channel=%q} is not exported", name, ch)
	return 0, 0, 0
}

// eventLog keeps every status event, in the order it was given.
type eventLog struct {
	mu  sync.Mutex
	all []events.Event
}

func (l *eventLog) Publish(_ context.Context, e events.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all = append(l.all, e)
}

func (l *eventLog) of(id uuid.UUID) []events.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []events.Event
	for _, e := range l.all {
		if e.ID == id {
			out = append(out, e)
		}
	}
	return out
}

func (l *eventLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.all)
}

// newBatch adds a batch row and returns its id.
func newBatch(t testing.TB, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(bg(t), `INSERT INTO batches (id, total) VALUES ($1, 0)`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// lockedBuffer is a log destination that the test reads while workers write.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines returns every JSON line written so far.
func (b *lockedBuffer) lines(t testing.TB) []map[string]any {
	t.Helper()
	b.mu.Lock()
	text := b.buf.String()
	b.mu.Unlock()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not a JSON line: %q", l)
		}
		out = append(out, m)
	}
	return out
}

// §9: a lost status event never affects delivery. Every event of the worker goes to a Redis
// that accepts connections and never answers. Every row is still sent at the rate limit, and
// no send keeps its slot after its result is stored.
func TestLostEventsDoNotAffectDelivery(t *testing.T) {
	e := testkit.Setup(t)
	srv := testkit.Recorder(t, recorder.Options{})
	hung, err := redisclient.New(testkit.HungRedis(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hung.Close() })
	pub := events.NewRedis(hung, events.Options{})

	opts := testOptions(t)
	opts.Events = pub
	opts.Slots, opts.ClaimBatch = 10, 10 // few slots: a send that kept its slot would stop the channel
	w := startWorker(t, e, workerSpec{
		channels: []domain.Channel{domain.SMS}, limit: 100, margin: 50 * time.Millisecond,
		opts: opts, provider: providerOptions(srv, false), reaperTick: 200 * time.Millisecond, reaperEvents: pub,
	})
	const n = 300
	insertRows(t, e.Pool, domain.SMS, n, always(domain.Normal), 6)
	counts, ok := waitAllFinal(t, e.Pool, 15*time.Second) // 3 s at the limit of 100 per second
	if !ok || counts[domain.Sent] != n || srv.Received() != n {
		t.Fatalf("with events going nowhere: %v, the provider received %d; want all %d sent", counts, srv.Received(), n)
	}
	d := w.dispatchers[domain.SMS]
	if !waitFor(time.Second, func() bool { return len(d.slots) == 0 }) {
		t.Errorf("%d sends still hold their slot after every result was stored", len(d.slots))
	}
}

// §9: the worker publishes every status change it makes: processing when a send starts, then
// the result's status; each event carries the notification's batch.
func TestEveryStatusChangeOfTheWorkerIsPublished(t *testing.T) {
	e := testkit.Setup(t)
	var tries atomic.Int64
	srv := testkit.Recorder(t, recorder.Options{Respond: func(recorder.Request) recorder.Response {
		if tries.Add(1) == 1 {
			return recorder.Response{Status: 503}
		}
		return recorder.Response{}
	}})
	batch := newBatch(t, e.Pool)
	id := insertRows(t, e.Pool, domain.SMS, 1, always(domain.Normal), 6)[0]
	if _, err := e.Pool.Exec(bg(t), `UPDATE notifications SET batch_id = $1`, batch); err != nil {
		t.Fatal(err)
	}
	log := &eventLog{}
	opts := testOptions(t)
	opts.Events = log
	oneWorker(t, e, srv, opts)
	if !waitFor(10*time.Second, func() bool { return rowOf(t, e.Pool, id).Status == domain.Sent }) {
		t.Fatalf("row is %s, want sent", rowOf(t, e.Pool, id).Status)
	}
	want := []domain.Status{domain.Processing, domain.Retrying, domain.Processing, domain.Sent}
	var got []domain.Status
	ok := waitFor(time.Second, func() bool {
		got = got[:0]
		for _, ev := range log.of(id) {
			if ev.BatchID != batch {
				t.Fatalf("event %+v does not carry the batch %s", ev, batch)
			}
			got = append(got, ev.Status)
		}
		return len(got) == len(want)
	})
	if !ok || strings.Join(statusNames(got), ",") != strings.Join(statusNames(want), ",") {
		t.Errorf("events %v, want %v", got, want)
	}
}

func statusNames(s []domain.Status) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = string(x)
	}
	return out
}

// §10: every attempt is counted once by its outcome and once by what became of it, and the
// counts agree with the attempt rows that Finish wrote.
func TestAttemptsAndResultsAreCountedOnce(t *testing.T) {
	e := testkit.Setup(t)
	ids := insertRows(t, e.Pool, domain.SMS, 30, always(domain.Normal), 6)
	kind := map[string]int{} // 0: accepted; 1: rejected (422); 2: 503 on the first try, then accepted
	for i, id := range ids {
		kind[id.String()] = i / 10
	}
	var mu sync.Mutex
	tries := map[string]int{}
	srv := testkit.Recorder(t, recorder.Options{Respond: func(r recorder.Request) recorder.Response {
		key := r.Header.Get("Idempotency-Key")
		mu.Lock()
		tries[key]++
		n := tries[key]
		mu.Unlock()
		switch {
		case kind[key] == 1:
			return recorder.Response{Status: 422}
		case kind[key] == 2 && n == 1:
			return recorder.Response{Status: 503}
		}
		return recorder.Response{}
	}})
	_, m := newMetrics()
	opts := testOptions(t)
	opts.Metrics = m
	oneWorker(t, e, srv, opts)
	if counts, ok := waitAllFinal(t, e.Pool, 20*time.Second); !ok {
		t.Fatalf("rows not final: %v", counts)
	}

	wantAttempts := map[string]float64{"sent": 20, "rejected": 10, "error_response": 10}
	wantFinished := map[string]float64{"sent": 20, "failed": 10, "retrying": 10}
	for _, ch := range domain.Channels {
		for _, o := range observability.Outcomes {
			want := 0.0
			if ch == domain.SMS {
				want = wantAttempts[o]
			}
			if got := testutil.ToFloat64(m.Attempts.WithLabelValues(string(ch), o)); got != want {
				t.Errorf("attempts{%s, %s} = %v, want %v", ch, o, got, want)
			}
		}
		for _, s := range observability.FinishedStatuses {
			want := 0.0
			if ch == domain.SMS {
				want = wantFinished[s]
			}
			if got := testutil.ToFloat64(m.Finished.WithLabelValues(string(ch), s)); got != want {
				t.Errorf("finished{%s, %s} = %v, want %v", ch, s, got, want)
			}
		}
	}
	// The database agrees: one attempt row per counted attempt, with the same outcome.
	rows, err := e.Pool.Query(bg(t), `SELECT outcome, count(*) FROM delivery_attempts GROUP BY outcome`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var outcome string
		var n float64
		if err := rows.Scan(&outcome, &n); err != nil {
			t.Fatal(err)
		}
		label := strings.ReplaceAll(outcome, " ", "_")
		if got := testutil.ToFloat64(m.Attempts.WithLabelValues("sms", label)); got != n {
			t.Errorf("%v attempt rows with the outcome %q, the counter says %v", n, outcome, got)
		}
	}
}

// §10: the wait at the gate is measured around the limiter's Wait, for every send that
// reached the gate, and the provider's latency from the permit to the end of the send.
func TestTheGateWaitAndProviderLatencyAreMeasured(t *testing.T) {
	e := testkit.Setup(t)
	srv := testkit.Recorder(t, recorder.Options{Respond: func(recorder.Request) recorder.Response {
		return recorder.Response{Delay: 30 * time.Millisecond}
	}})
	reg, m := newMetrics()
	opts := testOptions(t)
	opts.Metrics = m
	st := newStamps()
	startWorker(t, e, workerSpec{
		channels: []domain.Channel{domain.SMS}, limit: 10, margin: 50 * time.Millisecond,
		opts: opts, provider: providerOptions(srv, false), stamps: st,
	})
	const n = 30
	insertRows(t, e.Pool, domain.SMS, n, always(domain.Normal), 6)
	if counts, ok := waitAllFinal(t, e.Pool, 20*time.Second); !ok || counts[domain.Sent] != n {
		t.Fatalf("rows: %v, want %d sent", counts, n)
	}
	waits := st.sortedWaits()
	var stamped float64
	for _, w := range waits {
		stamped += w.Seconds()
	}
	count, sum, _ := histogram(t, reg, "notifications_gate_wait_seconds", domain.SMS, 0)
	t.Logf("gate: %d waits, %.3f s in all by the metric, %.3f s by the limiter's stamps", count, sum, stamped)
	if int(count) != len(waits) || sum < stamped || sum > stamped+float64(len(waits))*0.005 {
		t.Errorf("gate wait metric: %d observations, %.4f s; the limiter was waited on %d times for %.4f s",
			count, sum, len(waits), stamped)
	}
	count, sum, fast := histogram(t, reg, "notifications_provider_latency_seconds", domain.SMS, 0.025)
	t.Logf("provider latency: %d sends, %.3f s in all, %d under 25 ms", count, sum, fast)
	if count != n || fast != 0 || sum < n*0.030 {
		t.Errorf("provider latency: %d observations, %d under 25 ms, %.3f s in all; want %d, none under the provider's 30 ms",
			count, fast, sum, n)
	}
}

// §10: the breaker's state is read when the metrics are scraped: it turns half-open by the
// clock, with no call that could update a gauge.
func TestTheBreakerStateIsExportedWhenScraped(t *testing.T) {
	e := testkit.Setup(t)
	var healthy atomic.Bool
	srv := testkit.Recorder(t, recorder.Options{Respond: func(recorder.Request) recorder.Response {
		if healthy.Load() {
			return recorder.Response{Delay: 1500 * time.Millisecond} // the probe stays out for 1.5 s
		}
		return recorder.Response{Status: 503}
	}})
	reg, m := newMetrics()
	opts := testOptions(t)
	opts.Metrics, opts.BreakerOpen = m, 700*time.Millisecond
	oneWorker(t, e, srv, opts)
	state := func() float64 {
		v, ok := gauge(t, reg, "notifications_breaker_state", domain.SMS)
		if !ok {
			return -1
		}
		return v
	}
	for _, step := range []struct {
		do   func()
		want float64
		name string
	}{
		{func() {}, 0, "closed before any send"},
		{func() { insertRows(t, e.Pool, domain.SMS, 10, always(domain.Normal), 6) }, 1, "open after five failures"},
		{func() { healthy.Store(true) }, 2, "half-open by the clock, the probe out"},
		{func() {}, 0, "closed after the probe succeeded"},
	} {
		step.do()
		if !waitFor(5*time.Second, func() bool { return state() == step.want }) {
			t.Fatalf("%s: notifications_breaker_state = %v, want %v", step.name, state(), step.want)
		}
	}
	if _, ok := gauge(t, reg, "notifications_sends_in_flight", domain.SMS); !ok {
		t.Error("notifications_sends_in_flight is not exported")
	}
}

// §10: the reaper counts the rows it returns by their new status, and its events carry the
// rows' batch.
func TestTheReaperCountsTheRowsItReturns(t *testing.T) {
	e := testkit.Setup(t)
	batch := newBatch(t, e.Pool)
	ids := insertRows(t, e.Pool, domain.SMS, 5, always(domain.Normal), 6)
	if _, err := e.Pool.Exec(bg(t), `UPDATE notifications SET status = 'processing', attempts = 1, batch_id = $1,
    locked_until = now() - interval '1 second'`, batch); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Pool.Exec(bg(t), `UPDATE notifications SET attempts = 6 WHERE id = ANY($1)`, ids[:2]); err != nil {
		t.Fatal(err)
	}
	_, m := newMetrics()
	log := &eventLog{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Reaper{Queue: store.NewQueue(e.Pool), Tick: 100 * time.Millisecond, Log: testLog(t), Events: log, Metrics: m}.Run(ctx)
		close(done)
	}()
	ok := waitFor(5*time.Second, func() bool { return log.count() == len(ids) })
	cancel()
	<-done
	if !ok {
		t.Fatalf("%d of %d changes published", log.count(), len(ids))
	}
	for _, ev := range log.all {
		if ev.BatchID != batch {
			t.Errorf("event %+v does not carry the batch %s", ev, batch)
		}
	}
	if p, f := testutil.ToFloat64(m.Reaped.WithLabelValues("pending")), testutil.ToFloat64(m.Reaped.WithLabelValues("failed")); p != 3 || f != 2 {
		t.Errorf("reaped{pending} = %v, reaped{failed} = %v; want 3 and 2", p, f)
	}
}

// failingFinish fails every result write, as a Postgres that refuses writes does.
type failingFinish struct {
	Queue
	fails atomic.Int64
}

func (q *failingFinish) Finish(context.Context, uuid.UUID, int, store.Result) (domain.Status, bool, error) {
	q.fails.Add(1)
	return "", false, errors.New("ERROR: cannot execute UPDATE in a read-only transaction (SQLSTATE 25006)")
}

// §10, session 5's open item: result writes that fail are counted, and logged at most once
// per 10 s per dispatcher, not once per retry. The row whose result was not stored says so
// at debug level, with its correlation id.
func TestResultWriteFailuresAreCountedAndLoggedSparsely(t *testing.T) {
	e := testkit.Setup(t)
	srv := testkit.Recorder(t, recorder.Options{})
	const n = 20
	ids := insertRows(t, e.Pool, domain.SMS, n, always(domain.Normal), 6)
	if _, err := e.Pool.Exec(bg(t), `UPDATE notifications SET correlation_id = 'corr-' || left(id::text, 8)`); err != nil {
		t.Fatal(err)
	}
	buf := &lockedBuffer{}
	_, m := newMetrics()
	opts := testOptions(t)
	opts.Log, opts.Metrics = observability.NewLogger(buf, "worker", "debug"), m
	opts.Lease = 1500 * time.Millisecond // a result write is retried for one lease
	ff := &failingFinish{}
	startWorker(t, e, workerSpec{
		channels: []domain.Channel{domain.SMS}, limit: 100, margin: 50 * time.Millisecond,
		opts: opts, provider: providerOptions(srv, false),
		wrapQueue: func(q Queue) Queue { ff.Queue = q; return ff },
	})
	notStored := func() float64 { return testutil.ToFloat64(m.Finished.WithLabelValues("sms", "not_stored")) }
	if !waitFor(10*time.Second, func() bool { return notStored() == n }) {
		t.Fatalf("finished{not_stored} = %v, want %d", notStored(), n)
	}
	if got := testutil.ToFloat64(m.Errors.WithLabelValues("result_write")); got != float64(ff.fails.Load()) {
		t.Errorf("worker_errors{result_write} = %v, the queue refused %d writes", got, ff.fails.Load())
	}
	loud, perRow := 0, map[string]string{}
	for _, l := range buf.lines(t) {
		msg, _ := l["msg"].(string)
		if (l["level"] == "WARN" || l["level"] == "ERROR") && strings.Contains(msg, "result") {
			loud++
		}
		if id, ok := l["notification_id"].(string); ok && strings.Contains(msg, "not stored") {
			perRow[id], _ = l["correlation_id"].(string)
		}
	}
	t.Logf("%d result writes refused; %d warning or error lines about them", ff.fails.Load(), loud)
	if loud > 2 {
		t.Errorf("%d warning or error lines about %d refused writes in under 10 s, want at most 2", loud, ff.fails.Load())
	}
	for _, id := range ids {
		if want := "corr-" + id.String()[:8]; perRow[id.String()] != want {
			t.Errorf("row %s: the line that its result was not stored has correlation id %q, want %q", id, perRow[id.String()], want)
		}
	}
}

// chosenFlakyFinish fails the first result writes of chosen rows.
type chosenFlakyFinish struct {
	Queue
	mu   sync.Mutex
	left map[uuid.UUID]int
}

func (q *chosenFlakyFinish) Finish(ctx context.Context, id uuid.UUID, attempt int, r store.Result) (domain.Status, bool, error) {
	q.mu.Lock()
	fail := q.left[id] > 0
	if fail {
		q.left[id]--
	}
	q.mu.Unlock()
	if fail {
		return "", false, errors.New("connection reset by peer")
	}
	return q.Queue.Finish(ctx, id, attempt, r)
}

// §10: every worker line about a notification carries the notification's correlation id: the
// claim's row, the attempt, a retry, a rejection, a result write that failed, the reaper's
// return.
func TestEveryWorkerLineAboutANotificationCarriesItsCorrelationID(t *testing.T) {
	e := testkit.Setup(t)
	ids := insertRows(t, e.Pool, domain.SMS, 5, always(domain.Normal), 6)
	// ids[4] was held by a worker that died: processing, its lease over.
	if _, err := e.Pool.Exec(bg(t), `UPDATE notifications SET status = 'processing', attempts = 1,
    locked_until = now() - interval '1 second' WHERE id = $1`, ids[4]); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Pool.Exec(bg(t), `UPDATE notifications SET correlation_id = 'corr-' || left(id::text, 8)`); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	tries := map[string]int{}
	srv := testkit.Recorder(t, recorder.Options{Respond: func(r recorder.Request) recorder.Response {
		key := r.Header.Get("Idempotency-Key")
		mu.Lock()
		tries[key]++
		n := tries[key]
		mu.Unlock()
		switch {
		case key == ids[1].String() && n == 1:
			return recorder.Response{Status: 503}
		case key == ids[2].String():
			return recorder.Response{Status: 422}
		}
		return recorder.Response{}
	}})
	buf := &lockedBuffer{}
	opts := testOptions(t)
	opts.Log = observability.NewLogger(buf, "worker", "debug")
	startWorker(t, e, workerSpec{
		channels: []domain.Channel{domain.SMS}, limit: 100, margin: 50 * time.Millisecond,
		opts: opts, provider: providerOptions(srv, false), reaperTick: 200 * time.Millisecond,
		wrapQueue: func(q Queue) Queue { return &chosenFlakyFinish{Queue: q, left: map[uuid.UUID]int{ids[3]: 2}} },
	})
	if counts, ok := waitAllFinal(t, e.Pool, 20*time.Second); !ok {
		t.Fatalf("rows not final: %v", counts)
	}
	about := map[string]int{}
	reaped := false
	for _, l := range buf.lines(t) {
		id, ok := l["notification_id"].(string)
		if !ok {
			continue
		}
		about[id]++
		if msg, _ := l["msg"].(string); strings.Contains(msg, "lease expired") {
			reaped = true
		}
		if want := "corr-" + id[:8]; l["correlation_id"] != want {
			t.Errorf("a line about %s has correlation id %v, want %q: %v", id, l["correlation_id"], want, l)
		}
	}
	for _, id := range ids {
		if about[id.String()] == 0 {
			t.Errorf("no line about %s", id)
		}
	}
	if !reaped {
		t.Error("no line about the reaped row's lease")
	}
}
