// Package dispatch is the heart of the worker (architecture §6): the dispatcher's loop that
// claims due rows and hands each to the provider client, Classify, the retry policy, the
// circuit breaker and the reaper.
package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/events"
	"github.com/atakanakis/InsiderOneProject/internal/provider"
	"github.com/atakanakis/InsiderOneProject/internal/store"
)

// Queue is the part of the queue (architecture §5) a dispatcher uses. *store.Queue is it.
type Queue interface {
	Claim(ctx context.Context, ch domain.Channel, p domain.Priority, n int, lease time.Duration) ([]store.Claimed, error)
	Finish(ctx context.Context, id uuid.UUID, attempt int, r store.Result) (domain.Status, bool, error)
}

// Limiter is the rate limit (architecture §7). *ratelimit.Limiter is it.
type Limiter interface {
	Available(ctx context.Context, ch domain.Channel) (int, error)
	Wait(ctx context.Context, ch domain.Channel) error
}

// Options configure one dispatcher. The worker takes them from the timing budget
// (architecture §13).
type Options struct {
	Channel     domain.Channel
	Slots       int
	ClaimBatch  int
	Lease       time.Duration
	PauseBusy   time.Duration
	PauseIdle   time.Duration
	BreakerOpen time.Duration
	Retry       RetryPolicy
	Log         *slog.Logger
	Events      events.Publisher
}

// Dispatcher serves one channel.
type Dispatcher struct{ breaker *Breaker }

// New returns a dispatcher.
func New(o Options, q Queue, l Limiter, c *provider.Client) (*Dispatcher, error) {
	return nil, errors.New("not built yet")
}

// Run claims and sends until ctx ends, then waits for the rows in flight.
func (d *Dispatcher) Run(ctx context.Context) { <-ctx.Done() }
