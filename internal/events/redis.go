package events

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/atakanakis/InsiderOneProject/internal/observability"
)

// logPeriod is how often a repeated failure is logged (architecture §10).
const logPeriod = 10 * time.Second

// Options configure a Redis publisher.
type Options struct {
	Log     *slog.Logger                  // nil: no logs
	Metrics *observability.PublishMetrics // nil: counted on a registry of their own
}

// Redis publishes events to the Redis channel. Publish never waits: it puts the event in a
// buffer of Buffer events, and one goroutine sends the buffer to Redis in pipelines of up to
// PublishBatch events, each bounded by PublishTimeout (architecture §9).
type Redis struct {
	c   *redis.Client
	o   Options
	buf chan Event

	closeOnce sync.Once
	closing   context.Context // set before stop is closed: how long the last events may take
	stop      chan struct{}
	done      chan struct{}

	full, failing *observability.Every
}

// NewRedis returns a publisher on c and starts its sender. The client should be the
// publisher's own, so that a publish never holds a connection that another part needs.
func NewRedis(c *redis.Client, o Options) *Redis {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Metrics == nil {
		o.Metrics = observability.NewPublishMetrics(prometheus.NewRegistry())
	}
	p := &Redis{c: c, o: o, buf: make(chan Event, Buffer), stop: make(chan struct{}), done: make(chan struct{}),
		full: observability.NewEvery(logPeriod), failing: observability.NewEvery(logPeriod)}
	go p.run()
	return p
}

// Publish puts e in the buffer and returns at once. When the buffer is full, e is dropped and
// counted.
func (p *Redis) Publish(_ context.Context, e Event) {
	select {
	case p.buf <- e:
	default:
		p.o.Metrics.Dropped.WithLabelValues("buffer_full").Inc()
		p.full.Log(context.Background(), p.o.Log, slog.LevelWarn, "status events dropped: the buffer is full", "buffer", Buffer)
	}
}

// Close sends the events that wait, until ctx ends; what is left then is dropped and
// counted. Events published after Close are never sent.
func (p *Redis) Close(ctx context.Context) {
	p.closeOnce.Do(func() {
		p.closing = ctx
		close(p.stop)
	})
	select {
	case <-p.done:
	case <-ctx.Done():
	}
}

// run is the sender: one pipeline at a time, as many events as wait, up to PublishBatch.
func (p *Redis) run() {
	defer close(p.done)
	batch := make([]Event, 0, PublishBatch)
	for {
		select {
		case e := <-p.buf:
			batch = p.fill(append(batch[:0], e))
			p.send(context.Background(), batch)
		case <-p.stop:
			p.drain(batch)
			return
		}
	}
}

// fill adds the events that wait, without waiting, up to PublishBatch.
func (p *Redis) fill(batch []Event) []Event {
	for len(batch) < PublishBatch {
		select {
		case e := <-p.buf:
			batch = append(batch, e)
		default:
			return batch
		}
	}
	return batch
}

// drain sends what waits in the buffer, until it is empty or the closing context ends.
func (p *Redis) drain(batch []Event) {
	for {
		batch = p.fill(batch[:0])
		if len(batch) == 0 {
			return
		}
		if p.closing.Err() != nil {
			p.o.Metrics.Dropped.WithLabelValues("redis_error").Add(float64(len(batch) + len(p.buf)))
			return
		}
		p.send(p.closing, batch)
	}
}

// send publishes batch in one pipeline, bounded by PublishTimeout. An event Redis did not
// take is dropped and counted.
func (p *Redis) send(ctx context.Context, batch []Event) {
	ctx, cancel := context.WithTimeout(ctx, PublishTimeout)
	defer cancel()
	cmds, err := p.c.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for _, e := range batch {
			pipe.Publish(ctx, Channel, e.Encode())
		}
		return nil
	})
	sent := 0
	for _, c := range cmds {
		if c.Err() == nil {
			sent++
		}
	}
	p.o.Metrics.Published.Add(float64(sent))
	if lost := len(batch) - sent; lost > 0 {
		p.o.Metrics.Dropped.WithLabelValues("redis_error").Add(float64(lost))
		p.failing.Log(context.Background(), p.o.Log, slog.LevelWarn, "status events dropped: Redis did not take them",
			"events", lost, "error", err)
	}
}

// SubscribeOptions configure a subscription.
type SubscribeOptions struct {
	Log     *slog.Logger                    // nil: no logs
	Metrics *observability.SubscribeMetrics // nil: counted on a registry of their own
	// Wait is how long the subscription waits for a message before it pings, and for the
	// answer before it subscribes again; 0 means 10 s.
	Wait time.Duration
}

// Subscribe hands every event of the channel to deliver, on its own goroutine, until ctx
// ends. After any error it closes the connection and subscribes again, after a pause that
// starts at 100 ms and doubles up to 5 s; a subscription that worked starts the pause again
// at 100 ms (architecture §9).
func Subscribe(ctx context.Context, c *redis.Client, deliver func(Event), o SubscribeOptions) {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Metrics == nil {
		o.Metrics = observability.NewSubscribeMetrics(prometheus.NewRegistry())
	}
	if o.Wait <= 0 {
		o.Wait = 10 * time.Second
	}
	lost := observability.NewEvery(logPeriod)
	malformed := observability.NewEvery(logPeriod)
	pause := 100 * time.Millisecond
	for {
		subscribed, err := subscribeOnce(ctx, c, deliver, o, malformed)
		if ctx.Err() != nil {
			return
		}
		if subscribed {
			pause = 100 * time.Millisecond
		}
		lost.Log(context.Background(), o.Log, slog.LevelWarn, "status events: the subscription was lost; subscribing again",
			"error", err, "retry_in", pause.String())
		t := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		pause = min(2*pause, 5*time.Second)
	}
}

// subscribeOnce subscribes on a new connection and delivers until an error. A silent Wait
// is answered by a PING; a second silent Wait is an error. It reports whether the
// subscription was confirmed.
func subscribeOnce(ctx context.Context, c *redis.Client, deliver func(Event), o SubscribeOptions, malformed *observability.Every) (subscribed bool, err error) {
	ps := c.Subscribe(ctx) // no channel yet: nothing is sent
	defer func() { _ = ps.Close() }()
	// A read does not end when ctx is cancelled, only at its deadline; closing the
	// connection ends it at once.
	stop := context.AfterFunc(ctx, func() { _ = ps.Close() })
	defer stop()
	if err := ps.Subscribe(ctx, Channel); err != nil {
		return false, err
	}
	pinged := false
	for {
		msg, err := ps.ReceiveTimeout(ctx, o.Wait)
		if err != nil {
			if ctx.Err() == nil && timedOut(err) && !pinged {
				pinged = true
				if err := ps.Ping(ctx); err != nil {
					return subscribed, err
				}
				continue
			}
			return subscribed, err
		}
		pinged = false
		switch m := msg.(type) {
		case *redis.Subscription:
			if m.Kind == "subscribe" && m.Channel == Channel {
				subscribed = true
				o.Metrics.Subscriptions.Inc()
			}
		case *redis.Message:
			e, err := Decode(m.Payload)
			if err != nil {
				o.Metrics.Malformed.Inc()
				malformed.Log(context.Background(), o.Log, slog.LevelWarn, "status events: a malformed event was dropped", "error", err)
				continue
			}
			o.Metrics.Received.Inc()
			deliver(e)
		}
	}
}

func timedOut(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
