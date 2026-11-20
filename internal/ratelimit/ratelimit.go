// Package ratelimit is the shared rate limit of architecture §7: a sliding window of permits
// per channel in Redis, on the Redis clock.
//
// Each channel has one sorted set. A member is one permit; its score is the Redis time at
// which it was granted, in microseconds. A permit counts while it is younger than the window
// (one second plus a margin), so no half-open window of that length holds more than the
// limit (A6, TestNoWindowHoldsMoreThanTheLimit).
//
// Wait is the send gate's call; the provider client (session 4) is the only caller. Available
// is the dispatcher's hint; it only reads.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
)

// takeScript grants one permit if fewer than the limit are in the window, on the Redis clock.
// It returns {1, now, 0} for a permit, or {0, now, wait}: the microseconds until the oldest
// permit leaves the window. Without allow-oom, Redis refuses it before it runs when memory is
// full; on a read-only replica every write in it is refused. Either way the caller gets an
// error, never a permit.
//
// KEYS[1] the channel's set; ARGV[1] the limit; ARGV[2] the window in microseconds; ARGV[3] a
// random token, the new permit's member.
var takeScript = redis.NewScript(`#!lua
local key = KEYS[1]
local limit = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000000 + tonumber(t[2])
redis.call('ZREMRANGEBYSCORE', key, '-inf', string.format('%d', now - window))
if redis.call('ZCARD', key) < limit then
  redis.call('ZADD', key, string.format('%d', now), ARGV[3])
  redis.call('PEXPIRE', key, math.floor(window / 1000) + 1)
  return {1, now, 0}
end
local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
return {0, now, tonumber(oldest[2]) + window - now}
`)

// Limiter grants at most limit permits per channel in any window of one second plus margin.
// One Limiter is safe for concurrent use, and any number of them on any number of Redis
// clients share the limit, because the window is in Redis.
type Limiter struct {
	rdb    *redis.Client
	limit  int
	window time.Duration
}

// New returns a limiter on rdb, which must be built by redisclient.New so that context
// deadlines hold on the socket (architecture §8).
func New(rdb *redis.Client, limit int, margin time.Duration) (*Limiter, error) {
	if limit < 1 {
		return nil, fmt.Errorf("rate limit %d: must be at least 1", limit)
	}
	if margin < 0 {
		return nil, fmt.Errorf("window margin %s: must not be negative", margin)
	}
	return &Limiter{rdb: rdb, limit: limit, window: time.Second + margin}, nil
}

// readScript returns the number of free permits. Declared no-writes and run with EVAL_RO, it
// works on a read-only replica and when memory is full, and Redis refuses any write in it.
//
// KEYS[1] the channel's set; ARGV[1] the limit; ARGV[2] the window in microseconds.
var readScript = redis.NewScript(`#!lua flags=no-writes
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000000 + tonumber(t[2])
local used = redis.call('ZCOUNT', KEYS[1], string.format('(%d', now - tonumber(ARGV[2])), '+inf')
return math.max(0, tonumber(ARGV[1]) - used)
`)

// Available returns the number of free permits of the channel now. It changes nothing; the
// answer is a hint that can be wrong a moment later (architecture §6). An unreachable Redis is
// an error.
func (l *Limiter) Available(ctx context.Context, ch domain.Channel) (int, error) {
	n, err := readScript.RunRO(ctx, l.rdb, []string{key(ch)}, l.limit, l.window.Microseconds()).Int()
	if err != nil {
		return 0, fmt.Errorf("rate limit read: %w", err)
	}
	return n, nil
}

// Wait takes one permit of the channel. It returns nil when it has the permit, and an error,
// with no permit, when its context ends first, when Redis fails or when the script fails. If
// the context ends during the script call, Redis may have granted a permit whose answer was
// lost: it counts against the limit and is never used (architecture §7).
func (l *Limiter) Wait(ctx context.Context, ch domain.Channel) error {
	_, err := l.wait(ctx, ch)
	return err
}

// wait is Wait, and also returns the permit's Redis time in microseconds, for the tests.
func (l *Limiter) wait(ctx context.Context, ch domain.Channel) (int64, error) {
	for {
		granted, at, wait, err := l.take(ctx, ch)
		if err != nil {
			return 0, err // fail closed: an error is never a permit
		}
		if granted {
			return at, nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, fmt.Errorf("rate limit wait: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

// take runs the take script once.
func (l *Limiter) take(ctx context.Context, ch domain.Channel) (granted bool, at int64, wait time.Duration, err error) {
	res, err := takeScript.Run(ctx, l.rdb, []string{key(ch)},
		l.limit, l.window.Microseconds(), uuid.NewString()).Int64Slice()
	if err != nil {
		return false, 0, 0, fmt.Errorf("rate limit script: %w", err)
	}
	if len(res) != 3 {
		return false, 0, 0, errors.New("rate limit script: malformed reply")
	}
	return res[0] == 1, res[1], time.Duration(res[2]) * time.Microsecond, nil
}

// key is the channel's window. One window per channel: the limit is per channel.
func key(ch domain.Channel) string { return "ratelimit:" + string(ch) }
