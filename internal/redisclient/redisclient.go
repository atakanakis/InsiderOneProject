// Package redisclient is the one place a Redis client is built (architecture §14).
//
// Every client honours the deadline of the context it is called with. go-redis does not do
// this by default: it replaces the context with a background one for socket reads and
// writes, and waits for its own read timeout instead. A readiness probe with a 2 s deadline
// then waited 5 s against a paused Redis, and the rate limiter's wait at the send gate
// (architecture §7) would outlive its gate budget the same way.
package redisclient

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/atakanakis/InsiderOneProject/internal/observability"
)

// New returns a client for a redis:// or rediss:// URL. It does not connect.
func New(url string) (*redis.Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	opts.ContextTimeoutEnabled = true
	return redis.NewClient(opts), nil
}

// LogTo sends the Redis client's own lines to log, at most one per 10 s with the number held
// back (architecture §10). go-redis writes a plain-text line on every failed dial, to one
// logger for the whole process, so this is called once, when a command starts.
func LogTo(log *slog.Logger) {
	redis.SetLogger(&clientLog{log: log, every: observability.NewEvery(10 * time.Second)})
}

type clientLog struct {
	log   *slog.Logger
	every *observability.Every
}

func (l *clientLog) Printf(_ context.Context, format string, v ...any) {
	l.every.Log(context.Background(), l.log, slog.LevelWarn, "redis client", "message", fmt.Sprintf(format, v...))
}
