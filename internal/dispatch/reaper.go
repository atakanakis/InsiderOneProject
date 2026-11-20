package dispatch

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/atakanakis/InsiderOneProject/internal/events"
	"github.com/atakanakis/InsiderOneProject/internal/observability"
	"github.com/atakanakis/InsiderOneProject/internal/store"
)

// ReapQueue is the part of the queue the reaper uses. *store.Queue is it.
type ReapQueue interface {
	Reap(ctx context.Context, limit int) ([]store.Change, error)
}

// ReapBatch is how many rows one Reap call returns at most.
const ReapBatch = 100

// Reaper returns rows whose lease has expired: their worker died, or could not store a
// result for a whole lease (architecture §5, §6). Every worker runs one.
type Reaper struct {
	Queue   ReapQueue
	Tick    time.Duration
	Log     *slog.Logger                 // nil: no logs
	Events  events.Publisher             // nil: events are dropped
	Metrics *observability.WorkerMetrics // nil: counted on a registry of their own
}

// Run reaps every Tick until ctx ends.
func (r Reaper) Run(ctx context.Context) {
	if r.Log == nil {
		r.Log = slog.New(slog.DiscardHandler)
	}
	if r.Events == nil {
		r.Events = events.Discard{}
	}
	if r.Metrics == nil {
		r.Metrics = observability.NewWorkerMetrics(prometheus.NewRegistry())
	}
	failures := observability.NewEvery(logPeriod)
	tick := time.NewTicker(r.Tick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		r.reap(ctx, failures)
	}
}

// reap calls Reap in batches until a batch comes back short. Each returned row is logged
// with its correlation id, counted and published.
func (r Reaper) reap(ctx context.Context, failures *observability.Every) {
	for ctx.Err() == nil {
		changes, err := r.Queue.Reap(ctx, ReapBatch)
		if err != nil {
			r.Metrics.Errors.WithLabelValues("reap").Inc()
			failures.Log(ctx, r.Log, slog.LevelError, "reap failed", "error", err)
			return
		}
		for _, c := range changes {
			rowCtx := observability.WithCorrelationID(ctx, c.CorrelationID)
			r.Log.WarnContext(rowCtx, "lease expired; row returned", "notification_id", c.ID, "status", c.Status)
			r.Metrics.Reaped.WithLabelValues(string(c.Status)).Inc()
			r.Events.Publish(rowCtx, events.Event{ID: c.ID, BatchID: c.BatchID, Status: c.Status})
		}
		if len(changes) < ReapBatch {
			return
		}
	}
}
