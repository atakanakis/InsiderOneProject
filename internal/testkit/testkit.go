// Package testkit starts a real Postgres and a real Redis for integration tests, with
// testcontainers. A package that uses it calls Main from its TestMain; each test calls Setup.
// With `go test -short`, no container is started and Setup skips the test.
//
// The recording provider server is in package recorder; Recorder below starts one for a test.
package testkit

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/atakanakis/InsiderOneProject/internal/recorder"
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

var env *Env

// Main starts the containers once for the package, migrates the database, runs the tests
// and removes the containers.
func Main(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run())
	}
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, PostgresImage,
		tcpostgres.WithDatabase("notifications"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		tcpostgres.BasicWaitStrategies(),
	)
	defer terminate(pg)
	if err != nil {
		return fail("start postgres", err)
	}
	rc, err := tcredis.Run(ctx, RedisImage)
	defer terminate(rc)
	if err != nil {
		return fail("start redis", err)
	}

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return fail("postgres url", err)
	}
	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		return fail("connect postgres", err)
	}
	defer pool.Close()
	if _, err := store.Migrate(ctx, pool); err != nil {
		return fail("migrate", err)
	}

	redisURL, err := rc.ConnectionString(ctx)
	if err != nil {
		return fail("redis url", err)
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return fail("parse redis url", err)
	}
	client := redis.NewClient(opts)
	defer func() { _ = client.Close() }()

	env = &Env{Pool: pool, DatabaseURL: dsn, Redis: client, RedisURL: redisURL}
	return m.Run()
}

// Setup returns the package's environment with every table and Redis emptied, or skips the
// test under -short.
func Setup(t testing.TB) *Env {
	t.Helper()
	if env == nil {
		t.Skip("integration test: needs Docker; skipped with -short")
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

func terminate(c testcontainers.Container) {
	if c != nil {
		_ = testcontainers.TerminateContainer(c)
	}
}

func fail(what string, err error) int {
	fmt.Fprintf(os.Stderr, "testkit: %s: %v\n", what, err)
	return 1
}
