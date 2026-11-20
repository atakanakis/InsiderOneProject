// Command migrate applies the database migrations embedded in the binary and exits.
// docker-compose.yml runs it once before the API and the workers start. Migrations are
// append-only and there is no "down".
package main

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/atakanakis/InsiderOneProject/internal/config"
	"github.com/atakanakis/InsiderOneProject/internal/observability"
	"github.com/atakanakis/InsiderOneProject/internal/store"
)

func main() {
	os.Exit(run(context.Background(), os.Getenv, os.Stdout))
}

func run(ctx context.Context, getenv func(string) string, out io.Writer) int {
	cfg, err := config.Load(getenv)
	log := observability.NewLogger(out, "migrate", cfg.LogLevel)
	if err != nil {
		log.Error("configuration", "error", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("connect", "error", err)
		return 1
	}
	defer pool.Close()
	version, err := store.Migrate(ctx, pool)
	if err != nil {
		log.Error("migrate", "error", err)
		return 1
	}
	log.Info("migrations applied", "version", version)
	return 0
}
