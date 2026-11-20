// Package testkit starts a real Postgres and a real Redis for integration tests, with
// testcontainers. A package that uses it calls Main from its TestMain; each test calls Setup.
// The containers start on the first Setup; with `go test -short`, Setup skips the test.
//
// The recording provider server is in package recorder; Recorder below starts one for a test.
package testkit

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/atakanakis/InsiderOneProject/internal/recorder"
	"github.com/atakanakis/InsiderOneProject/internal/redisclient"
	"github.com/atakanakis/InsiderOneProject/internal/store"
)

// Images used by the tests; docker-compose.yml uses the same ones.
const (
	PostgresImage = "postgres:17-alpine"
	RedisImage    = "redis:7-alpine"
)

// Env is shared by the tests of one package.
type Env struct {
	Pool        *pgxpool.Pool // the migrated test database
	DatabaseURL string
	Redis       *redis.Client
	RedisURL    string
}

var (
	enabled  bool // Main ran without -short
	once     sync.Once
	env      *Env
	startErr error
	cleanups []func()
)

// Main runs the package's tests. The containers start on the first call of Setup, so a
// package's unit tests do not wait for Docker, and are removed when the tests end.
func Main(m *testing.M) {
	flag.Parse()
	enabled = !testing.Short()
	code := m.Run()
	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanups[i]()
	}
	os.Exit(code)
}

// Setup returns the package's environment with every table and Redis emptied, or skips the
// test under -short.
func Setup(t testing.TB) *Env {
	t.Helper()
	if !enabled {
		t.Skip("integration test: needs Docker; skipped with -short")
	}
	once.Do(func() { env, startErr = start() })
	if startErr != nil {
		t.Fatalf("testkit: %v", startErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := env.Pool.Exec(ctx, `TRUNCATE delivery_attempts, notifications, batches, templates`); err != nil {
		t.Fatalf("empty the tables: %v", err)
	}
	if err := env.Redis.FlushAll(ctx).Err(); err != nil {
		t.Fatalf("empty redis: %v", err)
	}
	return env
}

func start() (*Env, error) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, PostgresImage,
		tcpostgres.WithDatabase("notifications"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		tcpostgres.BasicWaitStrategies(),
	)
	cleanups = append(cleanups, func() { _ = testcontainers.TerminateContainer(pg) })
	if err != nil {
		return nil, fmt.Errorf("start postgres: %w", err)
	}
	rc, err := tcredis.Run(ctx, RedisImage)
	cleanups = append(cleanups, func() { _ = testcontainers.TerminateContainer(rc) })
	if err != nil {
		return nil, fmt.Errorf("start redis: %w", err)
	}
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, fmt.Errorf("postgres url: %w", err)
	}
	pool, err := store.Connect(ctx, dsn, store.Options{})
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	cleanups = append(cleanups, pool.Close)
	if _, err := store.Migrate(ctx, pool); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	redisURL, err := rc.ConnectionString(ctx)
	if err != nil {
		return nil, fmt.Errorf("redis url: %w", err)
	}
	client, err := redisclient.New(redisURL)
	if err != nil {
		return nil, err
	}
	cleanups = append(cleanups, func() { _ = client.Close() })
	return &Env{Pool: pool, DatabaseURL: dsn, Redis: client, RedisURL: redisURL}, nil
}

// EmptyDatabase creates a new database with no schema in the test Postgres and returns its
// URL. It is dropped when the test ends.
func EmptyDatabase(t testing.TB) string {
	t.Helper()
	e := Setup(t)
	name := "t_" + uuid.NewString()[:8]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := e.Pool.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = e.Pool.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})
	u, err := url.Parse(e.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// Recorder starts a recording provider server for one test and stops it when the test ends.
func Recorder(t testing.TB, opts recorder.Options) *recorder.Server {
	t.Helper()
	s, err := recorder.Start(opts)
	if err != nil {
		t.Fatalf("start recorder: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}
