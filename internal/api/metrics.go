package api

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/observability"
	"github.com/atakanakis/InsiderOneProject/internal/store"
)

// The API's metrics (architecture §10).

const (
	// FiguresTTL is how long a figure read from Postgres is reused: any number of scrapes and
	// requests cost one read per FiguresTTL.
	FiguresTTL = 5 * time.Second
	// QueueReadTimeout bounds the read of the queue's figures for a scrape.
	QueueReadTimeout = 2 * time.Second
	// DefaultMetricsWindow is the window of GET /v1/metrics without a window parameter.
	DefaultMetricsWindow = "5m"
)

// MetricsWindows are the windows GET /v1/metrics accepts; the longest is the bound of
// architecture §8.
var MetricsWindows = map[string]time.Duration{"1m": time.Minute, "5m": 5 * time.Minute, "15m": 15 * time.Minute, "1h": time.Hour}

// NewMetrics returns the API's metrics on a new registry with the Go runtime's and the
// process's collectors.
func NewMetrics() *observability.APIMetrics {
	return observability.NewAPIMetrics(observability.NewRegistry())
}

// labelsOf returns a routed request's method and route pattern as label values. A method
// that is not one of observability.HTTPMethods is "other", and a path the router has no
// pattern for is "unmatched": no label takes a value from the request.
func labelsOf(r *http.Request) (method, route string) {
	method = r.Method
	if !slices.Contains(observability.HTTPMethods, method) {
		method = "other"
	}
	route = r.Pattern
	if route == "" {
		route = "unmatched"
	}
	return method, route
}

// cached is a figure read from Postgres, reused for FiguresTTL. One read at a time: a caller
// that finds the figure old waits for the read in progress and takes its result.
type cached[T any] struct {
	mu    sync.Mutex
	at    time.Time
	value T
}

// get returns the figure, read again when it is older than FiguresTTL. A failed read is not
// kept: the next caller reads again, one at a time.
func (c *cached[T]) get(read func() (T, error)) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() || time.Since(c.at) >= FiguresTTL {
		v, err := read()
		if err != nil {
			var zero T
			return zero, err
		}
		c.value, c.at = v, time.Now()
	}
	return c.value, nil
}

// figures reads the queue's figures and the attempts of each window, each at most once per
// FiguresTTL.
type figures struct {
	readQueue    func(context.Context) (store.QueueStats, error)
	readAttempts func(context.Context, time.Duration) (map[domain.Channel]*store.AttemptStats, error)
	log          *slog.Logger
	failures *observability.Every
	queue    cached[store.QueueStats]
	windows  map[string]*cached[map[domain.Channel]*store.AttemptStats]
}

func newFigures(s *store.Notifications, log *slog.Logger) *figures {
	return newFiguresFrom(s.QueueStats, s.AttemptStats, log)
}

// newFiguresFrom returns figures read by the given functions; tests give their own.
func newFiguresFrom(readQueue func(context.Context) (store.QueueStats, error),
	readAttempts func(context.Context, time.Duration) (map[domain.Channel]*store.AttemptStats, error), log *slog.Logger) *figures {
	f := &figures{readQueue: readQueue, readAttempts: readAttempts, log: log, failures: observability.NewEvery(10 * time.Second),
		windows: map[string]*cached[map[domain.Channel]*store.AttemptStats]{}}
	for w := range MetricsWindows {
		f.windows[w] = &cached[map[domain.Channel]*store.AttemptStats]{}
	}
	return f
}

// queueStats reads the queue's figures. The read is shared, so it does not end with the
// caller that started it; it has its own timeout.
func (f *figures) queueStats(ctx context.Context) (store.QueueStats, error) {
	return f.queue.get(func() (store.QueueStats, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), QueueReadTimeout)
		defer cancel()
		return f.readQueue(ctx)
	})
}

// attemptStats reads the attempts of one window, shared like queueStats.
func (f *figures) attemptStats(ctx context.Context, name string, deadline time.Duration) (map[domain.Channel]*store.AttemptStats, error) {
	return f.windows[name].get(func() (map[domain.Channel]*store.AttemptStats, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deadline)
		defer cancel()
		return f.readAttempts(ctx, MetricsWindows[name])
	})
}

// Describe and Collect make the queue's figures a Prometheus collector. A read that fails
// leaves them out of the scrape, and is logged at most once per 10 s.
func (f *figures) Describe(ch chan<- *prometheus.Desc) {
	ch <- observability.QueueDepthDesc
	ch <- observability.OldestDueAgeDesc
}

func (f *figures) Collect(ch chan<- prometheus.Metric) {
	st, err := f.queueStats(context.Background())
	if err != nil {
		f.failures.Log(context.Background(), f.log, slog.LevelWarn, "queue figures not read", "error", err)
		return
	}
	for _, c := range domain.Channels {
		for _, s := range observability.QueueStatuses {
			ch <- prometheus.MustNewConstMetric(observability.QueueDepthDesc, prometheus.GaugeValue,
				float64(st.Depth[c][s]), string(c), string(s))
		}
		ch <- prometheus.MustNewConstMetric(observability.OldestDueAgeDesc, prometheus.GaugeValue,
			st.OldestDue[c].Seconds(), string(c))
	}
}

type percentiles struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

func percentilesOf(p []float64) *percentiles {
	if len(p) != 3 {
		return nil
	}
	return &percentiles{P50: p[0], P95: p[1], P99: p[2]}
}

type channelFigures struct {
	Queue               map[domain.Status]int64 `json:"queue"`
	OldestDueAgeSeconds float64                 `json:"oldestDueAgeSeconds"`
	Attempts            struct {
		Total     int64            `json:"total"`
		PerSecond float64          `json:"perSecond"`
		ByOutcome map[string]int64 `json:"byOutcome"`
	} `json:"attempts"`
	DeliveredShare    *float64     `json:"deliveredShare,omitempty"`
	AttemptDurationMs *percentiles `json:"attemptDurationMs,omitempty"`
	DeliveryLatencyMs *percentiles `json:"deliveryLatencyMs,omitempty"`
}

// metrics is GET /v1/metrics: the main figures of a recent window, per channel
// (architecture §10).
func (s *server) metrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := DefaultMetricsWindow
	if vs, ok := q["window"]; ok {
		if len(vs) != 1 {
			writeError(w, http.StatusBadRequest, fieldError("window", "must be given once"))
			return
		}
		name = vs[0]
	}
	window, ok := MetricsWindows[name]
	if !ok {
		writeError(w, http.StatusBadRequest, fieldError("window", "must be one of 1m, 5m, 15m and 1h"))
		return
	}
	queue, err := s.figures.queueStats(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	attempts, err := s.figures.attemptStats(r.Context(), name, s.deadline)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := map[domain.Channel]*channelFigures{}
	for _, ch := range domain.Channels {
		a := attempts[ch]
		f := &channelFigures{Queue: queue.Depth[ch], OldestDueAgeSeconds: queue.OldestDue[ch].Seconds()}
		f.Attempts.Total = a.Total
		f.Attempts.PerSecond = float64(a.Total) / window.Seconds()
		f.Attempts.ByOutcome = map[string]int64{}
		for _, o := range observability.Outcomes {
			f.Attempts.ByOutcome[o] = a.ByOutcome[strings.ReplaceAll(o, "_", " ")]
		}
		if a.Total > 0 {
			share := float64(a.ByOutcome["sent"]) / float64(a.Total)
			f.DeliveredShare = &share
		}
		f.AttemptDurationMs, f.DeliveryLatencyMs = percentilesOf(a.Duration), percentilesOf(a.Latency)
		out[ch] = f
	}
	s.reply(w, r, http.StatusOK, map[string]any{"window": name, "channels": out})
}
