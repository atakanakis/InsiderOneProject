// Command mockprovider is the local stand-in for the external provider in the Compose stack.
// It runs the recording server of package recorder: it answers every POST with the
// provider's 202 contract and counts what arrives.
//
//	POST /<any path>  a message; answered with {"messageId", "status": "accepted", "timestamp"}
//	GET  /stats       requests received, and the most that arrived in any one-second window,
//	                  per channel, over the requests kept (at least the last 100,000)
//	POST /reset       forget everything
//	GET  /healthz
//
// Environment: MOCK_ADDR (default :9090), MOCK_LATENCY (a duration added before each
// answer, default 0).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/observability"
	"github.com/atakanakis/InsiderOneProject/internal/recorder"
)

// kept bounds the memory of the mock provider: it holds at most twice this many requests
// between trims, each without body or headers, and headers are bounded at 64 KiB.
const kept = 100_000

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Getenv, os.Stdout)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, getenv func(string) string, out io.Writer) int {
	log := observability.NewLogger(out, "mockprovider", "info")
	addr := getenv("MOCK_ADDR")
	if addr == "" {
		addr = ":9090"
	}
	var latency time.Duration
	if v := getenv("MOCK_LATENCY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			log.Error("MOCK_LATENCY is not a duration such as 40ms", "value", v)
			return 2
		}
		latency = d
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           newHandler(latency),
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    60 << 10, // 64 KiB with Go's 4 KiB of slack
		ConnContext:       recorder.ConnContext,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("mock provider listening", "addr", addr, "latency", latency.String())
	select {
	case err := <-errc:
		log.Error("serve", "error", err)
		return 1
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return 1
	}
	return 0
}

func newHandler(latency time.Duration) http.Handler {
	rec := recorder.New(recorder.Options{
		MaxKept: kept,
		Lean:    true,
		Respond: func(recorder.Request) recorder.Response { return recorder.Response{Delay: latency} },
	})
	mux := http.NewServeMux()
	mux.Handle("POST /", rec)
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		peaks := map[string]int{"all": rec.Peak(time.Second, "")}
		for _, c := range domain.Channels {
			peaks[string(c)] = rec.Peak(time.Second, string(c))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"received":                rec.Received(),
			"kept":                    len(rec.Requests()),
			"maxInAnyOneSecondWindow": peaks,
		})
	})
	mux.HandleFunc("POST /reset", func(w http.ResponseWriter, _ *http.Request) {
		rec.Reset()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
