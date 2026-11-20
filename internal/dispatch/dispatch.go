// Package dispatch is the heart of the worker (architecture §6): the dispatcher's loop that
// claims due rows and hands each to the provider client, Classify, the retry policy, the
// circuit breaker and the reaper.
//
// The rules it keeps (CLAUDE.md): it never takes, holds or returns a rate-limit permit (the
// provider client's gate does, rule 3); nothing is reserved ahead and given back (rule 4);
// every claimed row ends in handle, and handle ends in Finish (rule 5); the verdict is
// Classify's (rule 6); the loop fits on one screen (rule 15).
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/events"
	"github.com/atakanakis/InsiderOneProject/internal/observability"
	"github.com/atakanakis/InsiderOneProject/internal/provider"
	"github.com/atakanakis/InsiderOneProject/internal/store"
)

// Queue is the part of the queue (architecture §5) a dispatcher uses. *store.Queue is it.
type Queue interface {
	Claim(ctx context.Context, ch domain.Channel, p domain.Priority, n int, lease time.Duration) ([]store.Claimed, error)
	Finish(ctx context.Context, id uuid.UUID, attempt int, r store.Result) (domain.Status, bool, error)
}

// Limiter is the rate limit (architecture §7). *ratelimit.Limiter is it.
type Limiter interface {
	Available(ctx context.Context, ch domain.Channel) (int, error)
	Wait(ctx context.Context, ch domain.Channel) error
}

// Options configure one dispatcher. The worker takes them from the timing budget
// (architecture §13).
type Options struct {
	Channel     domain.Channel
	Slots       int           // sends in flight at most
	ClaimBatch  int           // rows claimed and not yet permitted at most
	Lease       time.Duration // a claimed row's lease; also how long a result write is retried
	PauseBusy   time.Duration // the pause when there is no room
	PauseIdle   time.Duration // the pause when a claim found nothing
	BreakerOpen time.Duration
	Retry       RetryPolicy
	Provider    provider.Options // the dispatcher builds its own client: no pool is shared
	Log         *slog.Logger     // nil: no logs
	Events      events.Publisher // nil: events are dropped
	// Metrics are the worker's (architecture §10); nil: counted on a registry of their own.
	Metrics *observability.WorkerMetrics
	// Tracing is the tracer provider (architecture §10); nil: one that records nothing.
	Tracing trace.TracerProvider
}

// Dispatcher serves one channel, with its own slots, breaker and waiting mark.
type Dispatcher struct {
	o        Options
	queue    Queue
	limiter  Limiter
	provider *provider.Client // its own, built by New: no provider can skip the send gate (rule 9)
	breaker  *Breaker
	tracer   trace.Tracer
	slots    chan struct{} // one token per send in flight
	waiting  atomic.Int64  // rows claimed and not yet back from the gate
	inflight sync.WaitGroup

	// A failing dependency is logged at most once per 10 s per operation (architecture §10).
	claims, limits, writes *observability.Every
}

// New returns a dispatcher for o.Channel, with its own provider client built from o.Provider.
func New(o Options, q Queue, l Limiter) (*Dispatcher, error) {
	if _, err := domain.ParseChannel(string(o.Channel)); err != nil {
		return nil, fmt.Errorf("dispatch: %w", err)
	}
	if q == nil || l == nil {
		return nil, errors.New("dispatch: a queue and a limiter are needed")
	}
	if o.Slots <= 0 || o.ClaimBatch <= 0 || o.ClaimBatch > o.Slots || o.Lease <= 0 || o.PauseBusy <= 0 ||
		o.PauseIdle <= 0 || o.BreakerOpen <= 0 || o.Retry.Base <= 0 || o.Retry.Cap <= 0 || o.Retry.Soon <= 0 {
		return nil, errors.New("dispatch: every setting must be positive, and the claim batch at most the slots")
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Events == nil {
		o.Events = events.Discard{}
	}
	if o.Metrics == nil {
		o.Metrics = observability.NewWorkerMetrics(prometheus.NewRegistry())
	}
	if o.Tracing == nil {
		o.Tracing = noop.NewTracerProvider()
	}
	c, err := provider.New(o.Provider)
	if err != nil {
		return nil, fmt.Errorf("dispatch: %w", err)
	}
	d := &Dispatcher{o: o, queue: q, limiter: l, provider: c, breaker: NewBreaker(o.BreakerOpen, time.Now),
		tracer: o.Tracing.Tracer("worker"), slots: make(chan struct{}, o.Slots), claims: observability.NewEvery(logPeriod),
		limits: observability.NewEvery(logPeriod), writes: observability.NewEvery(logPeriod)}
	o.Metrics.Watch(o.Channel, func() int { return int(d.breaker.State()) }, func() int { return len(d.slots) })
	return d, nil
}

// logPeriod is how often a failing dependency is logged (architecture §10).
const logPeriod = 10 * time.Second

// Run claims and sends until ctx ends, then waits for the rows in flight; their sends and
// result writes are not cancelled (architecture §6, shutdown). This is the loop of §6.
func (d *Dispatcher) Run(ctx context.Context) {
	defer d.provider.CloseIdleConnections()
	defer d.inflight.Wait()
	for ctx.Err() == nil {
		n := d.room(ctx)
		if n == 0 {
			d.pause(ctx, d.o.PauseBusy)
			continue
		}
		claimed := 0
		for _, p := range domain.Priorities { // high, normal, low (I9)
			rows, err := d.queue.Claim(ctx, d.o.Channel, p, n-claimed, d.o.Lease)
			if err != nil {
				d.failed(ctx, "claim", d.claims, "claim failed", err)
				break
			}
			for _, row := range rows {
				d.start(ctx, row)
			}
			claimed += len(rows)
			if claimed == n {
				break
			}
		}
		if claimed == 0 {
			d.pause(ctx, d.o.PauseIdle)
		}
	}
}

// room is the smallest of the four limits of architecture §6, read cheapest first. Each only
// reads; none reserves anything.
func (d *Dispatcher) room(ctx context.Context) int {
	n := min(cap(d.slots)-len(d.slots), d.o.ClaimBatch)
	if d.waiting.Load() > 0 { // the claim batch rule: one batch at a time waits for permits
		n = 0
	}
	if n > 0 {
		n = min(n, d.breaker.Allow())
	}
	if n > 0 {
		free, err := d.limiter.Available(ctx, d.o.Channel)
		if err != nil {
			if ctx.Err() == nil {
				d.failed(ctx, "rate_limit", d.limits, "rate limit unavailable; claiming nothing", err)
			}
			return 0
		}
		n = min(n, free)
	}
	return n
}

// start hands a claimed row to its own goroutine. It runs on the loop's goroutine, so the
// probe is marked by one goroutine only.
func (d *Dispatcher) start(ctx context.Context, row store.Claimed) {
	d.slots <- struct{}{} // never blocks: room counted the free slots, and only the loop takes
	probe := d.breaker.Started()
	wait := d.enterWaiting()
	d.inflight.Add(1)
	go d.handle(context.WithoutCancel(ctx), row, probe, wait)
}

// failed counts a failed call to a dependency and logs it at most once per period.
func (d *Dispatcher) failed(ctx context.Context, op string, every *observability.Every, msg string, err error) {
	d.o.Metrics.Errors.WithLabelValues(op).Inc()
	every.Log(ctx, d.o.Log, slog.LevelWarn, msg, "channel", d.o.Channel, "error", err)
}

// handle is the single exit of a claimed row: it ends in Finish (CLAUDE.md, rule 5).
func (d *Dispatcher) handle(ctx context.Context, row store.Claimed, probe bool, wait *mark) {
	defer d.inflight.Done()
	defer func() { <-d.slots }()
	defer wait.leave()
	ctx = observability.WithCorrelationID(ctx, row.CorrelationID)
	// The delivery span joins the trace of the request that created the row (architecture §10).
	ctx, span := d.tracer.Start(observability.WithTraceparent(ctx, row.Traceparent), "deliver",
		trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attribute.String("notification.id", row.ID.String()),
			attribute.String("notification.channel", string(row.Channel)), attribute.Int("notification.attempt", row.Attempt)))
	defer span.End()
	// Publish never waits (architecture §9).
	d.o.Events.Publish(ctx, events.Event{ID: row.ID, BatchID: row.BatchID, Status: domain.Processing})
	var gate gateTimes // set by the gate, on this goroutine (architecture §7, property 1)
	result := d.provider.Send(ctx, messageOf(row), func(gateCtx context.Context) error {
		gate.began = time.Now()
		err := d.limiter.Wait(gateCtx, d.o.Channel) // the send gate (architecture §7)
		gate.ended, gate.permitted = time.Now(), err == nil
		wait.leave()
		return err
	})
	verdict := Classify(result)
	d.measure(gate, verdict)
	span.SetAttributes(attribute.String("notification.outcome", outcomeLabel(verdict.Outcome)),
		attribute.Int("http.response.status_code", result.StatusCode))
	if gate.permitted {
		span.AddEvent("permit", trace.WithTimestamp(gate.ended))
	}
	d.breaker.Report(probe, verdict.Lesson)
	d.finish(ctx, row, result, verdict)
}

// gateTimes are what one send's gate observed: when it began and ended, and whether it gave a
// permit. A send whose connection failed first never reached the gate.
type gateTimes struct {
	began, ended time.Time
	permitted    bool
}

// measure records one attempt (architecture §10): its outcome, its wait at the gate, and the
// provider's latency from the permit to the end of the send. Each is a counter or a histogram
// in memory; nothing here can wait or fail.
func (d *Dispatcher) measure(gate gateTimes, v Verdict) {
	ch := string(d.o.Channel)
	d.o.Metrics.Attempts.WithLabelValues(ch, outcomeLabel(v.Outcome)).Inc()
	if gate.began.IsZero() {
		return
	}
	d.o.Metrics.GateWait.WithLabelValues(ch).Observe(gate.ended.Sub(gate.began).Seconds())
	if gate.permitted {
		d.o.Metrics.ProviderLatency.WithLabelValues(ch).Observe(time.Since(gate.ended).Seconds())
	}
}

// outcomeLabel is a verdict's outcome as a label value: "error response" is "error_response".
func outcomeLabel(outcome string) string { return strings.ReplaceAll(outcome, " ", "_") }

// finish stores the result, retrying until the database answers: either the result is
// stored or the row is no longer this attempt's (stale). It gives up one lease length after
// the send, when the reaper will have returned the row (architecture §6, a lost result write).
func (d *Dispatcher) finish(ctx context.Context, row store.Claimed, r provider.Result, v Verdict) {
	res := store.Result{Status: v.Status, RetryIn: d.o.Retry.DelayAfter(row.Attempt, v), Duration: r.Duration,
		Outcome: v.Outcome, Error: errorText(r), ProviderMessageID: r.MessageID}
	if r.Responded {
		res.StatusCode = r.StatusCode
	}
	log := d.o.Log.With("notification_id", row.ID, "channel", row.Channel, "attempt", row.Attempt, "outcome", v.Outcome)
	ch := string(d.o.Channel)
	giveUp := time.Now().Add(d.o.Lease)
	for delay := 100 * time.Millisecond; ; delay = min(2*delay, 5*time.Second) {
		status, changed, err := d.queue.Finish(ctx, row.ID, row.Attempt, res)
		if err == nil {
			finished := string(status)
			if changed {
				d.o.Events.Publish(ctx, events.Event{ID: row.ID, BatchID: row.BatchID, Status: status})
			} else {
				finished = "stale"
			}
			d.o.Metrics.Finished.WithLabelValues(ch, finished).Inc()
			level := slog.LevelInfo
			if v.Status == domain.Sent {
				level = slog.LevelDebug
			}
			log.Log(ctx, level, "attempt finished", "status", status, "stale", !changed, "status_code", res.StatusCode,
				"duration_ms", r.Duration.Milliseconds(), "error", res.Error)
			return
		}
		// Counted, and logged at most once per period: a Postgres that refuses writes made a
		// line per retry, thousands a minute (session 5's review). The row's own lines are
		// debug lines, with its correlation id.
		d.o.Metrics.Errors.WithLabelValues("result_write").Inc()
		if time.Now().Add(delay).After(giveUp) {
			d.o.Metrics.Finished.WithLabelValues(ch, "not_stored").Inc()
			log.DebugContext(ctx, "result not stored; the reaper will return the row", "error", err)
			d.writes.Log(context.Background(), d.o.Log, slog.LevelError, "result writes failing; a result was not stored, the reaper will return its row",
				"channel", ch, "error", err)
			return
		}
		log.DebugContext(ctx, "result write failed; retrying", "error", err, "retry_in", delay)
		d.writes.Log(context.Background(), d.o.Log, slog.LevelWarn, "result writes failing; retrying", "channel", ch, "error", err)
		time.Sleep(delay)
	}
}

func messageOf(row store.Claimed) provider.Message {
	return provider.Message{ID: row.ID, CorrelationID: row.CorrelationID, To: row.Recipient,
		Channel: row.Channel, Content: row.Content}
}

// errorText is the error stored with an attempt that did not deliver.
func errorText(r provider.Result) string {
	switch {
	case r.Responded && r.StatusCode >= 200 && r.StatusCode < 300:
		return ""
	case r.Responded:
		return fmt.Sprintf("provider answered %d: %s", r.StatusCode, r.Body)
	case r.Err != nil:
		return r.Err.Error()
	}
	return ""
}

func (d *Dispatcher) pause(ctx context.Context, wait time.Duration) {
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// mark is one row's place among the rows claimed and not yet permitted. leave may be called
// more than once: at the gate, and on the way out of handle when the gate never ran.
type mark struct {
	once    sync.Once
	waiting *atomic.Int64
}

func (d *Dispatcher) enterWaiting() *mark {
	d.waiting.Add(1)
	return &mark{waiting: &d.waiting}
}

func (m *mark) leave() { m.once.Do(func() { m.waiting.Add(-1) }) }
