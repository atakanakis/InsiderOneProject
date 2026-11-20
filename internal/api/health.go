// Package api holds the HTTP handlers and middleware (architecture §8): the health endpoints
// and the correlation id (session 1), and the notification, batch and template endpoints
// (session 6, notifications.go).
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/atakanakis/InsiderOneProject/internal/observability"
	"github.com/atakanakis/InsiderOneProject/internal/store"
)

// Bounds of architecture §8 on every request.
const (
	// MaxHeaderBytes bounds the request line and headers together.
	MaxHeaderBytes = 64 << 10
	HeaderTimeout  = 5 * time.Second
	// RequestTimeout bounds the whole request, body included; AnswerTimeout the writing of
	// the answer, from the end of the request's headers.
	RequestTimeout    = 30 * time.Second
	AnswerTimeout     = 30 * time.Second
	ReadinessTimeout  = 2 * time.Second
	correlationHeader = "X-Correlation-ID"

	// Go's server reads 4096 bytes past http.Server.MaxHeaderBytes before it refuses a
	// request, so the server is given the bound minus that slack.
	headerSlack = 4096
)

// DatabaseOptions are the API's database sessions: architecture §8 bounds the database time
// of a request with a statement timeout of 5 s.
var DatabaseOptions = store.Options{StatementTimeout: 5 * time.Second}

// Check reports whether a dependency answers. It must return when ctx ends.
type Check func(ctx context.Context) error

// RedisCheck pings Redis. The client must come from package redisclient, so that the ping
// stops at the context's deadline.
func RedisCheck(c *redis.Client) Check {
	return func(ctx context.Context) error { return c.Ping(ctx).Err() }
}

// Health holds the readiness checks.
type Health struct {
	Postgres Check
	Redis    Check
	Timeout  time.Duration // for each check; 0 means ReadinessTimeout
}

// NewHandler returns the API's HTTP handler.
func NewHandler(log *slog.Logger, o Options) http.Handler {
	h := o.Health
	if h.Timeout <= 0 {
		h.Timeout = ReadinessTimeout
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ready(w, r, log, h)
	})
	register(mux, log, o)
	return correlate(log, mux)
}

// NewServer returns an HTTP server with the request bounds of architecture §8.
func NewServer(addr string, h http.Handler) *http.Server {
	return newServer(addr, h, timeouts{header: HeaderTimeout, request: RequestTimeout, answer: AnswerTimeout})
}

// timeouts bound the time a connection can hold the server (architecture §8): the headers,
// the whole request with its body, and the writing of the answer.
type timeouts struct {
	header, request, answer time.Duration
}

func newServer(addr string, h http.Handler, t timeouts) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: t.header,
		MaxHeaderBytes:    MaxHeaderBytes - headerSlack,
		IdleTimeout:       60 * time.Second,
	}
}

// ready runs both checks at once, each bounded by the timeout, and never shows their errors
// to the caller; the errors are logged. Only Postgres decides readiness: the API writes rows
// and needs nothing else to accept a request, while Redis carries best-effort status events
// (architecture §8, §9). Redis is reported for the operator.
func ready(w http.ResponseWriter, r *http.Request, log *slog.Logger, h Health) {
	checks := map[string]Check{"postgres": h.Postgres, "redis": h.Redis}
	result := make(map[string]string, len(checks)+1)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, check := range checks {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(r.Context(), h.Timeout)
			defer cancel()
			state := "ok"
			if err := check(ctx); err != nil {
				state = "unreachable"
				log.WarnContext(r.Context(), "readiness check failed", "dependency", name, "error", err)
			}
			mu.Lock()
			result[name] = state
			mu.Unlock()
		})
	}
	wg.Wait()
	code, status := http.StatusOK, "ready"
	if result["postgres"] != "ok" {
		code, status = http.StatusServiceUnavailable, "unavailable"
	}
	result["status"] = status
	writeJSON(w, code, result)
}

// correlate accepts the client's X-Correlation-ID when it is within the bound of
// architecture §8, and otherwise creates one. The id is put in the context, so every log
// line of the request carries it, and is sent back in the response.
func correlate(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(correlationHeader)
		if !observability.ValidCorrelationID(id) {
			id = observability.NewCorrelationID()
		}
		ctx := observability.WithCorrelationID(r.Context(), id)
		w.Header().Set(correlationHeader, id)
		began := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r.WithContext(ctx))
		log.DebugContext(ctx, "request", "method", r.Method, "path", r.URL.Path,
			"status", sw.code, "duration_ms", time.Since(began).Milliseconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
