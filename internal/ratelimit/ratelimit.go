// Package ratelimit is the shared rate limit of architecture §7: a sliding window of permits
// per channel in Redis, on the Redis clock.
package ratelimit

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
)

// Limiter grants at most limit permits per channel in any window of one second plus margin.
type Limiter struct {
	rdb    *redis.Client
	limit  int
	window time.Duration
}

// New returns a limiter on rdb.
func New(rdb *redis.Client, limit int, margin time.Duration) (*Limiter, error) {
	return &Limiter{rdb: rdb, limit: limit, window: time.Second + margin}, nil
}

// Available returns the number of free permits of the channel now.
func (l *Limiter) Available(ctx context.Context, ch domain.Channel) (int, error) {
	return 0, errors.New("not built")
}

// Wait takes one permit of the channel.
func (l *Limiter) Wait(ctx context.Context, ch domain.Channel) error {
	_, err := l.wait(ctx, ch)
	return err
}

func (l *Limiter) wait(ctx context.Context, ch domain.Channel) (int64, error) {
	return 0, errors.New("not built")
}

func key(ch domain.Channel) string { return "ratelimit:" + string(ch) }
