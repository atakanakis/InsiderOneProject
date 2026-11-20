// Command worker delivers notifications. In session 1 it checks its configuration, connects
// to Postgres and Redis, and waits for a stop signal; the dispatcher comes in session 5.
//
// The worker refuses to start when a relation of the timing budget (architecture §13) does
// not hold. It exits with status 2 and names every broken relation.
package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/atakanakis/InsiderOneProject/internal/config"
	"github.com/atakanakis/InsiderOneProject/internal/observability"
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
	if err := cfg.Worker.Check(); err != nil {
		log.Error("refusing to start: the timing budget does not hold", "error", err)
		return 2
	}

	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pool, err := store.Connect(connectCtx, cfg.DatabaseURL)
	if err != nil {
		log.Error("connect postgres", "error", err)
		return 1
	}
	defer pool.Close()
	ropts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		log.Error("redis url", "error", err)
		return 2
	}
	rdb := redis.NewClient(ropts)
	defer func() { _ = rdb.Close() }()
	if err := rdb.Ping(connectCtx).Err(); err != nil {
		log.Error("connect redis", "error", err)
		return 1
	}

	log.Info("worker started", "provider_url", cfg.ProviderURL,
		"lease", cfg.Worker.Lease.String(), "send_timeout", cfg.Worker.SendTimeout.String(),
		"shutdown_timeout", cfg.Worker.ShutdownTimeout.String())
	<-ctx.Done()
	// Nothing is in flight yet. From session 5 on, the worker waits here for sends in flight,
	// up to the shutdown timeout.
	log.Info("worker stopped")
	return 0
}
