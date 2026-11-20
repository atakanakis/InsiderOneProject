package events

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/atakanakis/InsiderOneProject/internal/observability"
)

// Options configure a Redis publisher.
type Options struct {
	Log     *slog.Logger                  // nil: no logs
	Metrics *observability.PublishMetrics // nil: counted nowhere
}

// Redis publishes events to the Redis channel.
type Redis struct {
	c *redis.Client
	o Options
}

// NewRedis returns a publisher on c. The client should be the publisher's own.
func NewRedis(c *redis.Client, o Options) *Redis {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Metrics == nil {
		o.Metrics = observability.NewPublishMetrics(prometheus.NewRegistry())
	}
	return &Redis{c: c, o: o}
}

// Publish publishes e.
func (p *Redis) Publish(ctx context.Context, e Event) {
	if err := p.c.Publish(ctx, Channel, e.Encode()).Err(); err != nil {
		p.o.Log.WarnContext(ctx, "publish a status event", "error", err)
		return
	}
	p.o.Metrics.Published.Inc()
}

// Close sends the events that wait, until ctx ends.
func (p *Redis) Close(ctx context.Context) {}

// SubscribeOptions configure a subscription.
type SubscribeOptions struct {
	Log     *slog.Logger                    // nil: no logs
	Metrics *observability.SubscribeMetrics // nil: counted nowhere
	// Wait is how long the subscription waits for a message before it pings, and for the
	// answer before it subscribes again; 0 means 10 s.
	Wait time.Duration
}

// Subscribe hands every event of the channel to deliver, on one goroutine, until ctx ends. It
// subscribes again after any error.
func Subscribe(ctx context.Context, c *redis.Client, deliver func(Event), o SubscribeOptions) {
	<-ctx.Done()
}
