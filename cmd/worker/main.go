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
	publisher := events.Discard{} // the Redis publisher comes in session 7

	var running sync.WaitGroup
	for _, ch := range domain.Channels {
		d, err := dispatch.New(dispatch.Options{
			Channel: ch, Slots: budget.Slots, ClaimBatch: budget.ClaimBatch, Lease: budget.Lease,
			PauseBusy: budget.PauseBusy, PauseIdle: budget.PauseIdle, BreakerOpen: budget.BreakerOpen,
			Retry:    dispatch.RetryPolicy{Base: budget.BackoffBase, Cap: budget.BackoffCap, Soon: time.Second},
			Provider: providerOptions, Log: log, Events: publisher,
		}, queue, limiter)
		if err != nil {
			log.Error("dispatcher", "channel", ch, "error", err)
			return 2
		}
		running.Go(func() { d.Run(ctx) })
	}
	reaper := dispatch.Reaper{Queue: queue, Tick: budget.ReaperTick, Log: log, Events: publisher}
	running.Go(func() { reaper.Run(ctx) })

	log.Info("worker started", "provider", providerHost(cfg.ProviderURL),
		"lease", budget.Lease.String(), "send_timeout", budget.SendTimeout.String(),
		"shutdown_timeout", budget.ShutdownTimeout.String())
	<-ctx.Done()
	log.Info("stopping: no more claims; waiting for the sends in flight")
	stopped := make(chan struct{})
	go func() {
		running.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
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
