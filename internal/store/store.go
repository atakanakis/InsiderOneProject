// Package store holds all SQL of the service (architecture §14) and the migrations.
// The queue operations of architecture §5 are added in session 2, in queue.go.
package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var embedded embed.FS

// Migrations returns the migration files, in the order they are applied.
func Migrations() fs.FS {
	sub, err := fs.Sub(embedded, "migrations")
	if err != nil {
		panic(err) // the directory is embedded at build time
	}
	return sub
}

// DefaultIdleInTransactionTimeout ends a session that keeps a transaction open and idle for
// longer than this. The service's transactions take milliseconds; a session that holds one
// open holds back vacuum, which slows the claim (architecture §5).
const DefaultIdleInTransactionTimeout = 10 * time.Second

// Options are per-session settings of a pool.
type Options struct {
	// StatementTimeout cancels any statement that runs longer. 0 means none. The API uses
	// 5 s (architecture §8).
	StatementTimeout time.Duration
	// IdleInTransactionTimeout; 0 means DefaultIdleInTransactionTimeout.
	IdleInTransactionTimeout time.Duration
}

// Connect opens a connection pool and checks that the database answers. Every session of
// the pool carries the timeouts of opts, set when the session starts, so they hold on any
// Postgres server.
func Connect(ctx context.Context, url string, opts Options) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if opts.IdleInTransactionTimeout <= 0 {
		opts.IdleInTransactionTimeout = DefaultIdleInTransactionTimeout
	}
	params := cfg.ConnConfig.RuntimeParams
	params["idle_in_transaction_session_timeout"] = strconv.FormatInt(opts.IdleInTransactionTimeout.Milliseconds(), 10)
	if opts.StatementTimeout > 0 {
		params["statement_timeout"] = strconv.FormatInt(opts.StatementTimeout.Milliseconds(), 10)
	}
	// Times come back in UTC whatever the time zone of the host.
	cfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		conn.TypeMap().RegisterType(&pgtype.Type{
			Name: "timestamptz", OID: pgtype.TimestamptzOID,
			Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC},
		})
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// Migrate applies every migration that has not been applied yet and returns the schema
// version. A Postgres advisory lock lets only one caller migrate at a time.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return 0, fmt.Errorf("migration lock: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, Migrations(), goose.WithSessionLocker(locker))
	if err != nil {
		return 0, fmt.Errorf("load migrations: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return 0, fmt.Errorf("apply migrations: %w", err)
	}
	v, err := p.GetDBVersion(ctx)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return v, nil
}
