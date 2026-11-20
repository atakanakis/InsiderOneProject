package observability

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
)

// The metrics of architecture §10. Every name starts with "notifications_". Labels take values
// from the fixed sets below, never from a request: no id, recipient or text is a label. Every
// series of a fixed set is created at zero when its metrics are made, so rate() and
// increase() see the first event, and a rule can name a value before it has happened.

// Outcomes are the rows of the verdict table (architecture §6), as label values.
var Outcomes = []string{"sent", "rejected", "error_response", "limiter_error", "no_permit_in_time", "closed_idle", "no_response"}

// FinishedStatuses say what became of an attempt: the row's new status, "stale" when the row
// was no longer this attempt's, "not_stored" when the result could not be written.
var FinishedStatuses = []string{"sent", "failed", "retrying", "stale", "not_stored"}

// Operations are the worker's calls to a dependency that can fail.
var Operations = []string{"claim", "rate_limit", "result_write", "reap"}

// QueueStatuses are the statuses counted in the queue's depth.
var QueueStatuses = []domain.Status{domain.Scheduled, domain.Pending, domain.Retrying, domain.Processing}

// DropReasons say why a status event was not published.
var DropReasons = []string{"buffer_full", "redis_error"}

// WebSocketCloseReasons say why the API closed a WebSocket.
var WebSocketCloseReasons = []string{"client", "slow", "no_pong", "data_message", "write_error", "shutdown"}

// NewRegistry returns a registry with the Go runtime's and the process's collectors.
func NewRegistry() *prometheus.Registry {
	r := prometheus.NewRegistry()
	r.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return r
}

// PublishMetrics count the status events a process publishes.
type PublishMetrics struct {
	Published prometheus.Counter
	Dropped   *prometheus.CounterVec // reason
}

// NewPublishMetrics registers the publisher's metrics on reg.
func NewPublishMetrics(reg prometheus.Registerer) *PublishMetrics {
	m := &PublishMetrics{
		Published: prometheus.NewCounter(prometheus.CounterOpts{Name: "notifications_events_published_total",
			Help: "Status events sent to Redis."}),
		Dropped: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "notifications_events_dropped_total",
			Help: "Status events dropped: the buffer was full, or Redis did not take them."}, []string{"reason"}),
	}
	reg.MustRegister(m.Published, m.Dropped)
	for _, r := range DropReasons {
		m.Dropped.WithLabelValues(r)
	}
	return m
}

// SubscribeMetrics count what the API's subscription receives.
type SubscribeMetrics struct {
	Received      prometheus.Counter
	Malformed     prometheus.Counter
	Subscriptions prometheus.Counter
}

// NewSubscribeMetrics registers the subscription's metrics on reg.
func NewSubscribeMetrics(reg prometheus.Registerer) *SubscribeMetrics {
	m := &SubscribeMetrics{
		Received: prometheus.NewCounter(prometheus.CounterOpts{Name: "notifications_events_received_total",
			Help: "Status events received from Redis and handed to the WebSocket hub."}),
		Malformed: prometheus.NewCounter(prometheus.CounterOpts{Name: "notifications_events_malformed_total",
			Help: "Messages on the status channel that were too large or not an event."}),
		Subscriptions: prometheus.NewCounter(prometheus.CounterOpts{Name: "notifications_events_subscriptions_total",
			Help: "Subscriptions to the status channel, the first included."}),
	}
	reg.MustRegister(m.Received, m.Malformed, m.Subscriptions)
	return m
}

// WorkerMetrics are the worker's metrics.
type WorkerMetrics struct {
	Attempts        *prometheus.CounterVec   // channel, outcome
	Finished        *prometheus.CounterVec   // channel, status
	GateWait        *prometheus.HistogramVec // channel
	ProviderLatency *prometheus.HistogramVec // channel
	Reaped          *prometheus.CounterVec   // status
	Errors          *prometheus.CounterVec   // operation

	watched *watched
}

// NewWorkerMetrics registers the worker's metrics on reg.
func NewWorkerMetrics(reg prometheus.Registerer) *WorkerMetrics {
	m := &WorkerMetrics{
		Attempts: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "notifications_attempts_total",
			Help: "Attempts by the verdict of architecture §6."}, []string{"channel", "outcome"}),
		Finished: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "notifications_finished_total",
			Help: "What became of each attempt: the row's new status, stale, or not_stored."}, []string{"channel", "status"}),
		GateWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "notifications_gate_wait_seconds",
			Help:    "Time a send waited at the send gate for its permit.",
			Buckets: []float64{.001, .005, .01, .05, .1, .25, .5, 1, 1.5, 2, 3, 5}}, []string{"channel"}),
		ProviderLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "notifications_provider_latency_seconds",
			Help:    "Time from the permit to the end of the send, for sends that got a permit.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 15}}, []string{"channel"}),
		Reaped: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "notifications_reaped_total",
			Help: "Rows whose lease expired, returned by the reaper, by their new status."}, []string{"status"}),
		Errors: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "notifications_worker_errors_total",
			Help: "Failed calls to a dependency, by operation."}, []string{"operation"}),
		watched: &watched{
			breaker: prometheus.NewDesc("notifications_breaker_state",
				"The breaker of the channel's dispatcher: 0 closed, 1 open, 2 half-open.", []string{"channel"}, nil),
			inFlight: prometheus.NewDesc("notifications_sends_in_flight",
				"Sends in flight of the channel's dispatcher.", []string{"channel"}, nil),
			funcs: map[domain.Channel]watch{},
		},
	}
	reg.MustRegister(m.Attempts, m.Finished, m.GateWait, m.ProviderLatency, m.Reaped, m.Errors, m.watched)
	for _, ch := range domain.Channels {
		for _, o := range Outcomes {
			m.Attempts.WithLabelValues(string(ch), o)
		}
		for _, s := range FinishedStatuses {
			m.Finished.WithLabelValues(string(ch), s)
		}
		m.GateWait.WithLabelValues(string(ch))
		m.ProviderLatency.WithLabelValues(string(ch))
	}
	for _, s := range []domain.Status{domain.Pending, domain.Failed} {
		m.Reaped.WithLabelValues(string(s))
	}
	for _, o := range Operations {
		m.Errors.WithLabelValues(o)
	}
	return m
}

// Watch exports a dispatcher's breaker state and sends in flight, read when scraped. A second
// call for the same channel replaces the first.
func (m *WorkerMetrics) Watch(ch domain.Channel, breakerState func() int, inFlight func() int) {
	m.watched.mu.Lock()
	defer m.watched.mu.Unlock()
	m.watched.funcs[ch] = watch{breakerState, inFlight}
}

type watch struct{ breaker, inFlight func() int }

// watched is a collector of values that are read when scraped.
type watched struct {
	breaker, inFlight *prometheus.Desc
	mu                sync.Mutex
	funcs             map[domain.Channel]watch
}

func (w *watched) Describe(ch chan<- *prometheus.Desc) {
	ch <- w.breaker
	ch <- w.inFlight
}

func (w *watched) Collect(ch chan<- prometheus.Metric) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for c, f := range w.funcs {
		ch <- prometheus.MustNewConstMetric(w.breaker, prometheus.GaugeValue, float64(f.breaker()), string(c))
		ch <- prometheus.MustNewConstMetric(w.inFlight, prometheus.GaugeValue, float64(f.inFlight()), string(c))
	}
}

// MetricsHandler serves the registry in Prometheus's text format.
func MetricsHandler(g prometheus.Gatherer) http.Handler {
	return promhttp.HandlerFor(g, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
}

// NewMetricsServer returns the worker's server of /metrics, with the header and time bounds
// of the API's server (architecture §8).
func NewMetricsServer(addr string, g prometheus.Gatherer) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", MetricsHandler(g))
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    60 << 10,
	}
}
