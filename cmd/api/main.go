// Command api serves the HTTP API of architecture §8.
package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/atakanakis/InsiderOneProject/internal/api"
	"github.com/atakanakis/InsiderOneProject/internal/config"
	"github.com/atakanakis/InsiderOneProject/internal/events"
	"github.com/atakanakis/InsiderOneProject/internal/observability"
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
	log := observability.NewLogger(out, "api", cfg.LogLevel)
	redisclient.LogTo(log) // the Redis client's own lines, as JSON and at most one per 10 s
	if err != nil {
		log.Error("configuration", "error", err)
		return 2
	}
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pool, err := store.Connect(connectCtx, cfg.DatabaseURL, api.DatabaseOptions)
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

	// Status events (architecture §9): the publisher and the subscription each have their own
	// Redis client, so neither holds a connection the other or the readiness check needs.
	metrics := api.NewMetrics()
	tracing, stopTracing, err := observability.Tracing(ctx, cfg.TracingEndpoint, "notifications-api", log)
	if err != nil {
		log.Error("tracing", "error", err)
		return 2
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = stopTracing(stopCtx)
	}()
	publishClient, err := redisclient.New(cfg.RedisURL)
	if err != nil {
		log.Error("redis url", "error", err)
		return 2
	}
	defer func() { _ = publishClient.Close() }()
	subscribeClient, err := redisclient.New(cfg.RedisURL)
	if err != nil {
		log.Error("redis url", "error", err)
		return 2
	}
	defer func() { _ = subscribeClient.Close() }()
	publisher := events.NewRedis(publishClient, events.Options{Log: log, Metrics: metrics.Publish})
	hub := api.NewHub(api.HubOptions{Metrics: metrics})
	subscribed, stopSubscription := context.WithCancel(context.Background())
	defer stopSubscription()
	go events.Subscribe(subscribed, subscribeClient, hub.Deliver, events.SubscribeOptions{Log: log, Metrics: metrics.Subscribe})

	srv := api.NewServer(cfg.APIAddr, api.NewHandler(log, api.Options{
		Health:      api.Health{Postgres: pool.Ping, Redis: api.RedisCheck(rdb)},
		Pool:        pool,
		APIKeys:     cfg.APIKeys,
		MaxAttempts: cfg.MaxAttempts,
		Events:      publisher,
		Metrics:     metrics,
		Hub:         hub,
		Tracing:     tracing,
	}))
	srv.RegisterOnShutdown(hub.Close) // every WebSocket is closed with 1001
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("api listening", "addr", cfg.APIAddr)

	select {
	case err := <-errc:
		log.Error("serve", "error", err)
		return 1
	case <-ctx.Done():
	}
	log.Info("api stopping")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("shutdown", "error", err)
		return 1
	}
	stopSubscription()
	flush, cancelFlush := context.WithTimeout(context.Background(), 2*time.Second)
	publisher.Close(flush) // the last answers' events
	cancelFlush()
	log.Info("api stopped")
	return 0
}
