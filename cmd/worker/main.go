// Command worker delivers notifications: one dispatcher per channel and one reaper
// (architecture §6), on Postgres, Redis and the provider client.
//
// The worker refuses to start when a relation of the timing budget (architecture §13) does
// not hold. It exits with status 2 and names every broken relation. On a stop signal the
// dispatchers stop claiming and the worker waits for the sends in flight, up to the shutdown
// timeout; it exits 0 when they finished, and 1 when the timeout passed first (the rows it
// still held come back through the reaper).
package main

import (
	"context"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/atakanakis/InsiderOneProject/internal/config"
	"github.com/atakanakis/InsiderOneProject/internal/dispatch"
	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/events"
	"github.com/atakanakis/InsiderOneProject/internal/observability"
	"github.com/atakanakis/InsiderOneProject/internal/provider"
	"github.com/atakanakis/InsiderOneProject/internal/ratelimit"
	"github.com/atakanakis/InsiderOneProject/internal/redisclient"
	"github.com/atakanakis/InsiderOneProject/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Getenv, os.Stdout)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, getenv func(string) string, out io.Writer) int {
	cfg, err := config.Load(getenv)
	log := observability.NewLogger(out, "worker", cfg.LogLevel)
	if err != nil {
		log.Error("configuration", "error", err)
		return 2
	}
	budget, err := config.LoadWorker(getenv)
	if err != nil {
		log.Error("configuration", "error", err)
		return 2
	}
	if err := budget.Check(); err != nil {
		log.Error("refusing to start: the timing budget does not hold", "error", err)
		return 2
	}

	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pool, err := store.Connect(connectCtx, cfg.DatabaseURL, store.Options{})
	if err != nil {
		log.Error("connect postgres", "error", err)
		return 1
	}
	defer pool.Close()
	rdb, err := redisclient.New(cfg.RedisURL)
	if err != nil {
		log.Error("redis url", "error", err)
		return 2
	}
	defer func() { _ = rdb.Close() }()
	if err := rdb.Ping(connectCtx).Err(); err != nil {
		log.Error("connect redis", "error", err)
		return 1
	}

	limiter, err := ratelimit.New(rdb, budget.RateLimit, budget.WindowMargin)
	if err != nil {
		log.Error("rate limit", "error", err)
		return 2
	}
	providerOptions := provider.Options{ // each dispatcher builds its own client from these
		URL: cfg.ProviderURL, SendTimeout: budget.SendTimeout, GateBudget: budget.GateBudget,
		DialTimeout: budget.DialTimeout, TLSHandshakeTimeout: budget.TLSHandshakeTimeout,
		IdleConns: budget.IdleConns, IdleConnTimeout: budget.IdleConnTimeout,
	}
	queue := store.NewQueue(pool)
	tracing, stopTracing, err := observability.Tracing(ctx, cfg.TracingEndpoint, "notifications-worker", log)
	if err != nil {
		log.Error("tracing", "error", err)
		return 2
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = stopTracing(stopCtx)
	}()
	reg := observability.NewRegistry()
	metrics := observability.NewWorkerMetrics(reg)
	// The publisher has its own Redis client: a publish never holds a connection the rate
	// limiter needs (architecture §9).
	eventsClient, err := redisclient.New(cfg.RedisURL)
	if err != nil {
		log.Error("redis url", "error", err)
		return 2
	}
	defer func() { _ = eventsClient.Close() }()
	publisher := events.NewRedis(eventsClient, events.Options{Log: log, Metrics: observability.NewPublishMetrics(reg)})
	metricsListener, err := net.Listen("tcp", cfg.MetricsAddr)
	if err != nil {
		log.Error("metrics listen", "addr", cfg.MetricsAddr, "error", err)
		return 1
	}
	metricsServer := observability.NewMetricsServer(cfg.MetricsAddr, reg)
	go func() { _ = metricsServer.Serve(metricsListener) }()
	defer func() { _ = metricsServer.Close() }()

	var running sync.WaitGroup
	for _, ch := range domain.Channels {
		d, err := dispatch.New(dispatch.Options{
			Channel: ch, Slots: budget.Slots, ClaimBatch: budget.ClaimBatch, Lease: budget.Lease,
			PauseBusy: budget.PauseBusy, PauseIdle: budget.PauseIdle, BreakerOpen: budget.BreakerOpen,
			Retry:    dispatch.RetryPolicy{Base: budget.BackoffBase, Cap: budget.BackoffCap, Soon: time.Second},
			Provider: providerOptions, Log: log, Events: publisher, Metrics: metrics, Tracing: tracing,
		}, queue, limiter)
		if err != nil {
			log.Error("dispatcher", "channel", ch, "error", err)
			return 2
		}
		running.Go(func() { d.Run(ctx) })
	}
	reaper := dispatch.Reaper{Queue: queue, Tick: budget.ReaperTick, Log: log, Events: publisher, Metrics: metrics}
	running.Go(func() { reaper.Run(ctx) })

	log.Info("worker started", "provider", providerHost(cfg.ProviderURL), "metrics", metricsListener.Addr().String(),
		"lease", budget.Lease.String(), "send_timeout", budget.SendTimeout.String(),
		"shutdown_timeout", budget.ShutdownTimeout.String())
	<-ctx.Done()
	log.Info("stopping: no more claims; waiting for the sends in flight")
	deadline := time.Now().Add(budget.ShutdownTimeout)
	stopped := make(chan struct{})
	go func() {
		running.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
		// The last results' events, for at most 2 s and within the shutdown timeout.
		flush, cancel := context.WithTimeout(context.Background(), min(2*time.Second, time.Until(deadline)))
		publisher.Close(flush)
		cancel()
		log.Info("worker stopped")
		return 0
	case <-time.After(budget.ShutdownTimeout):
		log.Error("worker stopped: the shutdown timeout passed with sends in flight; the reaper will return their rows",
			"shutdown_timeout", budget.ShutdownTimeout.String())
		return 1
	}
}

// providerHost is the provider URL's scheme and host. The rest of the URL can hold a secret (a
// webhook.site path, credentials) and is never logged (architecture §7).
func providerHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(not a URL)"
	}
	return u.Scheme + "://" + u.Host
}
