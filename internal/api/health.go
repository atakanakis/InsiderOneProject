// Package api holds the HTTP handlers and middleware (architecture §8). Session 1 has the
// health endpoints and the correlation id; the notification endpoints come in session 6.
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
)

// Bounds of architecture §8 on every request.
const (
	MaxHeaderBytes    = 64 << 10
	HeaderTimeout     = 5 * time.Second
	ReadinessTimeout  = 2 * time.Second
	correlationHeader = "X-Correlation-ID"
)

// Check reports whether a dependency answers. It must return when ctx ends.
type Check func(ctx context.Context) error

// RedisCheck pings Redis.
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
func NewHandler(log *slog.Logger, h Health) http.Handler {
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
	return correlate(log, mux)
}

// NewServer returns an HTTP server with the request bounds of architecture §8.
func NewServer(addr string, h http.Handler) *http.Server {
	return newServer(addr, h, HeaderTimeout)
}

func newServer(addr string, h http.Handler, headerTimeout time.Duration) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: headerTimeout,
		MaxHeaderBytes:    MaxHeaderBytes,
		IdleTimeout:       60 * time.Second,
	}
}

// ready runs both checks at once, each bounded by the timeout, and never shows their errors
// to the caller; the errors are logged.
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
	for _, state := range result {
		if state != "ok" {
			code, status = http.StatusServiceUnavailable, "unavailable"
		}
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
