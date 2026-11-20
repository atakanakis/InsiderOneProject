// Command migrate applies the database migrations embedded in the binary and exits.
// docker-compose.yml runs it once before the API and the workers start. Migrations are
// append-only and there is no "down".
package main

import (
	"context"
	"os"
	"time"

	"github.com/atakanakis/InsiderOneProject/internal/config"
	"github.com/atakanakis/InsiderOneProject/internal/observability"
	"github.com/atakanakis/InsiderOneProject/internal/store"
)

func main() {
	cfg, err := config.Load(os.Getenv)
	log := observability.NewLogger(os.Stdout, "migrate", cfg.LogLevel)
	if err != nil {
		log.Error("configuration", "error", err)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("connect", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	version, err := store.Migrate(ctx, pool)
	if err != nil {
		log.Error("migrate", "error", err)
		pool.Close()
		os.Exit(1) //nolint:gocritic // the pool is closed above
	}
	log.Info("migrations applied", "version", version)
}
