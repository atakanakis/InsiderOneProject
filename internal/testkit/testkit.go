// Package testkit starts a real Postgres and a real Redis for integration tests, with
// testcontainers. A package that uses it calls Main from its TestMain; each test calls Setup.
// The containers start on the first Setup; with `go test -short`, Setup skips the test.
//
// When GUARDS_POSTGRES_URL and GUARDS_REDIS_URL are set, no container is started: the test
// process creates its own new database on that Postgres server, migrates it and drops it at the
// end, and uses that Redis. Only `make guards` sets them: it starts the two servers once
// (StartServers) and gives their URLs to the test processes it runs, one at a time, and to no
// other process. Each Setup empties that Redis, so the variables must never name a Redis that
// holds anything else.
//
// The recording provider server is in package recorder; Recorder below starts one for a test.
package testkit

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
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

// Variables that point the tests at servers that are already running.
const (
	PostgresURLVar = "GUARDS_POSTGRES_URL"
	RedisURLVar    = "GUARDS_REDIS_URL"
)

func start() (*Env, error) {
	ctx := context.Background()
	pgURL, redisURL := os.Getenv(PostgresURLVar), os.Getenv(RedisURLVar)
	if pgURL == "" || redisURL == "" {
		var stop func()
		var err error
		pgURL, redisURL, stop, err = StartServers(ctx)
		cleanups = append(cleanups, stop)
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		if pgURL, err = ownDatabase(ctx, pgURL); err != nil {
			return nil, err
		}
	}
	pool, err := store.Connect(ctx, pgURL, store.Options{})
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	cleanups = append(cleanups, pool.Close)
	if _, err := store.Migrate(ctx, pool); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	client, err := redisclient.New(redisURL)
	if err != nil {
		return nil, err
	}
	cleanups = append(cleanups, func() { _ = client.Close() })
	return &Env{Pool: pool, DatabaseURL: pgURL, Redis: client, RedisURL: redisURL}, nil
}

// ownDatabase creates a new database on the server of serverURL, for this process alone, and
// returns its URL. The database is dropped when the tests end. A new database per process
// means a test always runs the migrations as they are on disk, also when make guards has
// just changed one.
func ownDatabase(ctx context.Context, serverURL string) (string, error) {
	admin, err := store.Connect(ctx, serverURL, store.Options{})
	if err != nil {
		return "", fmt.Errorf("connect postgres server: %w", err)
	}
	name := "p_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close()
		return "", fmt.Errorf("create database: %w", err)
	}
	cleanups = append(cleanups, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		admin.Close()
	})
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

// StartServers starts a Postgres and a Redis container and returns their URLs and a function
// that removes both. The Postgres URL names an empty database "notifications".
func StartServers(ctx context.Context) (postgresURL, redisURL string, stop func(), err error) {
	var terminate []func()
	stop = func() {
		for i := len(terminate) - 1; i >= 0; i-- {
			terminate[i]()
		}
	}
	pg, err := tcpostgres.Run(ctx, PostgresImage,
		tcpostgres.WithDatabase("notifications"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		tcpostgres.BasicWaitStrategies(),
	)
	terminate = append(terminate, func() { _ = testcontainers.TerminateContainer(pg) })
	if err != nil {
		return "", "", stop, fmt.Errorf("start postgres: %w", err)
	}
	rc, err := tcredis.Run(ctx, RedisImage)
	terminate = append(terminate, func() { _ = testcontainers.TerminateContainer(rc) })
	if err != nil {
		return "", "", stop, fmt.Errorf("start redis: %w", err)
	}
	if postgresURL, err = pg.ConnectionString(ctx, "sslmode=disable"); err != nil {
		return "", "", stop, fmt.Errorf("postgres url: %w", err)
	}
	if redisURL, err = rc.ConnectionString(ctx); err != nil {
		return "", "", stop, fmt.Errorf("redis url: %w", err)
	}
	return postgresURL, redisURL, stop, nil
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

// HungRedis returns the URL of a server that accepts connections, reads what it is sent and
// never answers: a paused Redis. It stops when the test ends.
func HungRedis(t testing.TB) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var conns sync.WaitGroup
	var mu sync.Mutex
	var open []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range open {
			_ = c.Close()
		}
		mu.Unlock()
		conns.Wait()
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			open = append(open, c)
			mu.Unlock()
			conns.Go(func() { _, _ = io.Copy(io.Discard, c); _ = c.Close() })
		}
	}()
	return "redis://" + ln.Addr().String() + "/0"
}
