// Package redisclient is the one place a Redis client is built (architecture §14).
//
// Every client honours the deadline of the context it is called with. go-redis does not do
// this by default: it replaces the context with a background one for socket reads and
// writes, and waits for its own read timeout instead. A readiness probe with a 2 s deadline
// then waited 5 s against a paused Redis, and the rate limiter's wait at the send gate
// (architecture §7) would outlive its gate budget the same way.
package redisclient

import (
	"fmt"

	"github.com/redis/go-redis/v9"
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
