// Command mockprovider is the local stand-in for the external provider in the Compose stack.
// It runs the recording server of package recorder: it answers every POST with the
// provider's 202 contract and counts what arrives. It can inject failures and throttles at
// random, so that the worker's retries and circuit breaker can be watched locally.
//
//	POST /<any path>  a message; answered with {"messageId", "status": "accepted", "timestamp"},
//	                  or with an injected 503 or 429 (see below)
//	GET  /stats       requests received, and the most that arrived in any one-second window,
//	                  per channel, over the requests kept (at least the last 100,000); and
//	                  how many injected failures and throttles were answered
//	POST /reset       forget everything
//	GET  /healthz
//
// Environment:
//
//	MOCK_ADDR           listen address; default :9090
//	MOCK_LATENCY        a duration added before every answer, injected or not; default 0
//	MOCK_FAIL_RATE      the share of POSTs, between 0 and 1, answered 503 with
//	                    {"error":"injected failure"}; default 0
//	MOCK_THROTTLE_RATE  the share of POSTs, between 0 and 1, answered 429 with Retry-After: 1
//	                    and {"error":"injected throttle"}; default 0
//
// Each POST draws the failure first and, only if it did not fail, draws the throttle
// separately, so the two rates need not add up to 1.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/observability"
	"github.com/atakanakis/InsiderOneProject/internal/recorder"
)

// kept bounds the memory of the mock provider: it holds at most twice this many requests
// between trims, each without body or headers, and headers are bounded at 64 KiB.
const kept = 100_000

// maxTrackedKeys bounds the Idempotency-Keys the mock provider remembers, to count what it
// accepted twice; a variable so that a test can lower it.
var maxTrackedKeys = 1_000_000

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
	var s settings
	if v := getenv("MOCK_LATENCY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			log.Error("MOCK_LATENCY is not a duration such as 40ms", "value", v)
			return 2
		}
		s.latency = d
	}
	var ok bool
	if s.failRate, ok = rate(getenv, "MOCK_FAIL_RATE"); !ok {
		log.Error("MOCK_FAIL_RATE is not a number between 0 and 1", "value", getenv("MOCK_FAIL_RATE"))
		return 2
	}
	if s.throttleRate, ok = rate(getenv, "MOCK_THROTTLE_RATE"); !ok {
		log.Error("MOCK_THROTTLE_RATE is not a number between 0 and 1", "value", getenv("MOCK_THROTTLE_RATE"))
		return 2
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           newHandler(s, rand.Float64),
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    60 << 10, // 64 KiB with Go's 4 KiB of slack
		ConnContext:       recorder.ConnContext,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("mock provider listening", "addr", addr, "latency", s.latency.String(),
		"failRate", s.failRate, "throttleRate", s.throttleRate)
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

// rate reads a share between 0 and 1 from the environment; unset is 0.
func rate(getenv func(string) string, name string) (float64, bool) {
	v := getenv(name)
	if v == "" {
		return 0, true
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || !(f >= 0 && f <= 1) { // written this way so that NaN is refused too
		return 0, false
	}
	return f, true
}

// settings are what the environment configures.
type settings struct {
	latency                time.Duration
	failRate, throttleRate float64
}

// newHandler returns the mock provider. random returns a number in [0, 1) and must be safe
// for concurrent use; a POST is injected when its draw is below the rate.
func newHandler(s settings, random func() float64) http.Handler {
	var failures, throttles atomic.Int64
	rec := recorder.New(recorder.Options{
		MaxKept: kept,
		Lean:    true,
		Respond: func(recorder.Request) recorder.Response {
			switch {
			case random() < s.failRate:
				failures.Add(1)
				return recorder.Response{Status: http.StatusServiceUnavailable, Delay: s.latency,
					Header: http.Header{"Content-Type": {"application/json"}},
					Body:   []byte(`{"error":"injected failure"}`)}
			case random() < s.throttleRate:
				throttles.Add(1)
				return recorder.Response{Status: http.StatusTooManyRequests, Delay: s.latency,
					Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"1"}},
					Body:   []byte(`{"error":"injected throttle"}`)}
			}
			return recorder.Response{Delay: s.latency}
		},
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
			"injected":                map[string]int64{"failures": failures.Load(), "throttles": throttles.Load()},
		})
	})
	mux.HandleFunc("POST /reset", func(w http.ResponseWriter, _ *http.Request) {
		rec.Reset()
		failures.Store(0)
		throttles.Store(0)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
