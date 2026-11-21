package events

import (
	"context"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/observability"
	"github.com/atakanakis/InsiderOneProject/internal/redisclient"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

func TestMain(m *testing.M) { testkit.Main(m) }

func bg(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func client(t testing.TB, rawURL string) *redis.Client {
	t.Helper()
	c, err := redisclient.New(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func waitFor(timeout time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(timeout); ; time.Sleep(10 * time.Millisecond) {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// subscription runs Subscribe until the test ends and collects what it delivers.
type subscription struct {
	got     chan Event
	metrics *observability.SubscribeMetrics
}

func subscribe(t testing.TB, c *redis.Client, wait time.Duration) *subscription {
	t.Helper()
	s := &subscription{got: make(chan Event, 10000), metrics: observability.NewSubscribeMetrics(prometheus.NewRegistry())}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Subscribe(ctx, c, func(e Event) { s.got <- e }, SubscribeOptions{Metrics: s.metrics, Wait: wait})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second): // a read waiting for its 10 s must not hold up a shutdown
			t.Error("Subscribe did not return within 2 s of its context's end")
			<-done
		}
	})
	return s
}

// subscribed waits until Redis counts n subscribers of the channel.
func subscribed(t testing.TB, c *redis.Client, n int64) {
	t.Helper()
	ok := waitFor(10*time.Second, func() bool {
		m, err := c.PubSubNumSub(context.Background(), Channel).Result()
		return err == nil && m[Channel] == n
	})
	if !ok {
		t.Fatalf("Redis does not count %d subscribers of %s", n, Channel)
	}
}

func (s *subscription) next(t testing.TB, timeout time.Duration) (Event, bool) {
	t.Helper()
	select {
	case e := <-s.got:
		return e, true
	case <-time.After(timeout):
		return Event{}, false
	}
}

// An event survives its wire form; a notification created alone has no batch id on the wire.
func TestAnEventSurvivesItsWireForm(t *testing.T) {
	for _, e := range []Event{
		{ID: uuid.New(), Status: domain.Pending},
		{ID: uuid.New(), BatchID: uuid.New(), Status: domain.Sent},
	} {
		b := e.Encode()
		got, err := Decode(string(b))
		if err != nil || got != e {
			t.Errorf("Decode(Encode(%+v)) = %+v, %v", e, got, err)
		}
		if e.BatchID == uuid.Nil && strings.Contains(string(b), "batchId") {
			t.Errorf("an event without a batch has a batchId on the wire: %s", b)
		}
	}
}

// §9: Publish never waits. Against a Redis that accepts connections and never answers, a
// burst of three buffers' worth of events is published at once; what does not fit in the
// buffer is dropped and counted, and Close returns at the end of its context.
func TestPublishNeverBlocksWhenRedisHangs(t *testing.T) {
	m := observability.NewPublishMetrics(prometheus.NewRegistry())
	p := NewRedis(client(t, testkit.HungRedis(t)), Options{Metrics: m})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	began := time.Now()
	var slowest time.Duration
	n := 0
	for ; n < 3*Buffer && time.Since(began) < 2*time.Second; n++ {
		t0 := time.Now()
		p.Publish(ctx, Event{ID: uuid.New(), Status: domain.Sent})
		slowest = max(slowest, time.Since(t0))
	}
	total := time.Since(began)
	t.Logf("%d events published in %v; the slowest call took %v", n, total, slowest)
	// The slowest single call is compared with 100 ms, not 10: under make test every package
	// runs at once and a call took 10.018 ms once (session 7's review, F5). A Publish that
	// waits for Redis takes the context's 2 s.
	if n < 3*Buffer || slowest > 100*time.Millisecond || total > time.Second {
		t.Fatalf("Publish waited for Redis: %d of %d events in %v, the slowest call %v", n, 3*Buffer, total, slowest)
	}
	// The sender's first pipeline gives up at PublishTimeout (1 s), not at the client's own
	// read timeout (3 s), and its events are counted.
	failedAt := waitFor(2*time.Second, func() bool { return testutil.ToFloat64(m.Dropped.WithLabelValues("redis_error")) > 0 })
	if !failedAt {
		t.Error("no pipeline to the hung Redis was given up within 2 s")
	}
	dropped := func() float64 { return testutil.ToFloat64(m.Dropped.WithLabelValues("buffer_full")) }
	// The buffer holds Buffer events and the sender holds one pipeline; the rest was dropped.
	if !waitFor(time.Second, func() bool { return dropped() >= float64(3*Buffer-Buffer-PublishBatch) }) {
		t.Errorf("%v events dropped as buffer_full, want at least %d", dropped(), 3*Buffer-Buffer-PublishBatch)
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer closeCancel()
	t0 := time.Now()
	p.Close(closeCtx)
	if d := time.Since(t0); d > 1500*time.Millisecond {
		t.Errorf("Close took %v against a hung Redis, want about its context's 200 ms", d)
	}
}

// §9: events reach a subscriber through Redis, whole and in the order one process published
// them.
func TestEventsReachSubscribersThroughRedis(t *testing.T) {
	e := testkit.Setup(t)
	s := subscribe(t, client(t, e.RedisURL), 0)
	subscribed(t, e.Redis, 1)
	m := observability.NewPublishMetrics(prometheus.NewRegistry())
	p := NewRedis(client(t, e.RedisURL), Options{Metrics: m})
	a, b := uuid.New(), uuid.New()
	want := []Event{
		{ID: a, Status: domain.Pending},
		{ID: b, BatchID: uuid.New(), Status: domain.Processing},
		{ID: a, Status: domain.Processing},
		{ID: a, Status: domain.Sent},
	}
	for _, ev := range want {
		p.Publish(bg(t), ev)
	}
	for i, w := range want {
		got, ok := s.next(t, 5*time.Second)
		if !ok || got != w {
			t.Fatalf("event %d: %+v (received %v), want %+v", i, got, ok, w)
		}
	}
	if !waitFor(time.Second, func() bool { return testutil.ToFloat64(m.Published) == float64(len(want)) }) {
		t.Errorf("published counter %v, want %d", testutil.ToFloat64(m.Published), len(want))
	}
	if got := testutil.ToFloat64(s.metrics.Received); got != float64(len(want)) {
		t.Errorf("received counter %v, want %d", got, len(want))
	}
}

// §9: the publisher sends what it holds when it is closed, so a worker's last results are
// not lost on a clean shutdown.
// holdFirstPipeline is a go-redis hook that holds the first pipeline until release is
// closed, so that a test can call Close while events are certainly still in the buffer.
type holdFirstPipeline struct {
	once    sync.Once
	entered chan struct{} // closed when the first pipeline is held
	release chan struct{}
}

func newHold() *holdFirstPipeline {
	return &holdFirstPipeline{entered: make(chan struct{}), release: make(chan struct{})}
}

func (h *holdFirstPipeline) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *holdFirstPipeline) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }

func (h *holdFirstPipeline) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		first := false
		h.once.Do(func() { first = true })
		if first {
			close(h.entered)
			<-h.release
		}
		return next(ctx, cmds)
	}
}

// heldPublisher publishes n events to a publisher whose first pipeline is held, and returns
// once that pipeline is held: the other events wait in the buffer.
func heldPublisher(t *testing.T, redisURL string, n int) (*Redis, *holdFirstPipeline) {
	t.Helper()
	c := client(t, redisURL)
	h := newHold()
	c.AddHook(h)
	p := NewRedis(c, Options{})
	for range n {
		p.Publish(bg(t), Event{ID: uuid.New(), Status: domain.Sent})
	}
	select {
	case <-h.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first pipeline was never sent")
	}
	return p, h
}

// §9: Close sends what is still buffered, through Close's context. The first pipeline is held
// while Close is called, so that the rest of the events are in the buffer when it is; until
// session 8 the sender could send everything before the test called Close, and the test passed
// with the drain removed (CI run 44924691249).
func TestThePublisherSendsWhatIsBufferedWhenClosed(t *testing.T) {
	e := testkit.Setup(t)
	s := subscribe(t, client(t, e.RedisURL), 0)
	subscribed(t, e.Redis, 1)
	const n = 1000
	p, h := heldPublisher(t, e.RedisURL, n)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	closed := make(chan struct{})
	go func() { defer close(closed); p.Close(ctx) }()
	<-p.stop // Close has been called
	close(h.release)
	for i := range n {
		if _, ok := s.next(t, 5*time.Second); !ok {
			t.Fatalf("%d of %d events arrived after Close", i, n)
		}
	}
	<-closed
}

// §9: once Close is called, nothing is sent after Close's context has ended; what is still
// buffered then is dropped and counted. The sender's own context must not send it later. Each
// round holds the first pipeline, calls Close with a context that has ended, and then lets the
// pipeline go: only that pipeline's events may be published. Until session 8 the sender chose
// at random between the buffer and the stop signal and went on sending; 20 rounds make a pass
// by chance about one in a million.
func TestCloseSendsNothingAfterItsContextEnds(t *testing.T) {
	e := testkit.Setup(t)
	const n, rounds = 1000, 20
	for round := range rounds {
		p, h := heldPublisher(t, e.RedisURL, n)
		ended, cancel := context.WithCancel(context.Background())
		cancel()
		p.Close(ended) // returns at once: its context has ended
		close(h.release)
		<-p.done
		published := testutil.ToFloat64(p.o.Metrics.Published)
		dropped := testutil.ToFloat64(p.o.Metrics.Dropped.WithLabelValues("redis_error"))
		if published > PublishBatch || published+dropped != n {
			t.Fatalf("round %d: %v published and %v dropped of %d after Close's context ended; at most the held pipeline of %d may be published",
				round, published, dropped, n, PublishBatch)
		}
	}
}

// §9: when Redis drops the subscription's connection, the subscription comes back by itself,
// and the events published after it are delivered.
func TestTheSubscriptionComesBackAfterRedisDropsIt(t *testing.T) {
	e := testkit.Setup(t)
	s := subscribe(t, client(t, e.RedisURL), 0)
	subscribed(t, e.Redis, 1)
	if err := e.Redis.Do(bg(t), "CLIENT", "KILL", "TYPE", "pubsub").Err(); err != nil {
		t.Fatal(err)
	}
	if !waitFor(10*time.Second, func() bool { return testutil.ToFloat64(s.metrics.Subscriptions) >= 2 }) {
		t.Fatalf("subscriptions %v after the connection was killed, want 2", testutil.ToFloat64(s.metrics.Subscriptions))
	}
	subscribed(t, e.Redis, 1)
	want := Event{ID: uuid.New(), Status: domain.Sent}
	NewRedis(client(t, e.RedisURL), Options{}).Publish(bg(t), want)
	if got, ok := s.next(t, 5*time.Second); !ok || got != want {
		t.Fatalf("after the subscription came back: %+v (received %v), want %+v", got, ok, want)
	}
}

// proxy stands between the subscription and Redis, to make outages: refuse closes every
// connection and refuses new ones; freeze stops forwarding on the connections it has, as a
// paused Redis would, while new ones are forwarded.
type proxy struct {
	ln     net.Listener
	target string

	mu      sync.Mutex
	refuse  bool
	conns   []*piped
	conWait sync.WaitGroup
}

type piped struct {
	client, server net.Conn
	frozen         atomic.Bool
}

func newProxy(t testing.TB, target string) *proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{ln: ln, target: target}
	go p.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		p.mu.Lock()
		for _, c := range p.conns {
			_ = c.client.Close()
			_ = c.server.Close()
		}
		p.mu.Unlock()
		p.conWait.Wait()
	})
	return p
}

func (p *proxy) url() string { return "redis://" + p.ln.Addr().String() + "/0" }

func (p *proxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		refuse := p.refuse
		p.mu.Unlock()
		if refuse {
			_ = c.Close()
			continue
		}
		s, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		pc := &piped{client: c, server: s}
		p.mu.Lock()
		p.conns = append(p.conns, pc)
		p.mu.Unlock()
		p.conWait.Go(func() { pc.copy(s, c) })
		p.conWait.Go(func() { pc.copy(c, s) })
	}
}

// copy forwards src to dst until either closes, dropping what it reads while frozen.
func (pc *piped) copy(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 && !pc.frozen.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	_ = dst.Close()
	_ = src.Close()
}

// down closes every connection and refuses new ones.
func (p *proxy) down() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refuse = true
	for _, c := range p.conns {
		_ = c.client.Close()
		_ = c.server.Close()
	}
	p.conns = nil
}

func (p *proxy) up() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refuse = false
}

// freeze stops forwarding on every open connection, without closing it.
func (p *proxy) freeze() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.frozen.Store(true)
	}
}

func redisAddr(t testing.TB, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// §9: an outage of Redis, then a Redis that stops answering on its open connection (paused):
// after each, the subscription comes back by itself and delivers what is published.
func TestTheSubscriptionComesBackAfterAnOutage(t *testing.T) {
	e := testkit.Setup(t)
	px := newProxy(t, redisAddr(t, e.RedisURL))
	const wait = 300 * time.Millisecond
	s := subscribe(t, client(t, px.url()), wait)
	subscribed(t, e.Redis, 1)
	publisher := NewRedis(client(t, e.RedisURL), Options{})
	delivered := func(stage string) {
		t.Helper()
		// Published until one arrives: the subscription may still be on its way back.
		want := Event{ID: uuid.New(), Status: domain.Sent}
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			publisher.Publish(bg(t), want)
			// Copies of an earlier stage's event may still arrive; only this one counts.
			if got, ok := s.next(t, 200*time.Millisecond); ok && got == want {
				return
			}
		}
		t.Fatalf("%s: no event delivered within 15 s", stage)
	}

	px.down()
	time.Sleep(time.Second) // a second of refused connections: several tries fail
	px.up()
	delivered("after an outage")
	before := testutil.ToFloat64(s.metrics.Subscriptions)

	px.freeze() // the open connection goes silent, as a paused Redis
	delivered("after Redis stopped answering")
	if got := testutil.ToFloat64(s.metrics.Subscriptions); got <= before {
		t.Errorf("subscriptions %v, want more than %v: the silent connection was not replaced", got, before)
	}
}

// §9, §8: a message on the channel that is larger than 1 KiB or is not an event is dropped
// and counted; the subscription goes on.
func TestAMalformedOrOversizedEventIsDropped(t *testing.T) {
	e := testkit.Setup(t)
	s := subscribe(t, client(t, e.RedisURL), 0)
	subscribed(t, e.Redis, 1)
	id := uuid.NewString()
	bad := []string{
		`not json`,
		`{"id":"not-a-uuid","status":"sent"}`,
		`{"id":"` + id + `","status":"delivered"}`,
		`{"id":"` + id + `"}`,
		`{"status":"sent"}`,
		`{"id":"` + id + `","status":"sent","pad":"` + strings.Repeat("x", MaxEventBytes) + `"}`,
		`{"id":"` + id + `","batchId":"x","status":"sent"}`,
	}
	for _, b := range bad {
		if err := e.Redis.Publish(bg(t), Channel, b).Err(); err != nil {
			t.Fatal(err)
		}
	}
	good := Event{ID: uuid.New(), Status: domain.Failed}
	if err := e.Redis.Publish(bg(t), Channel, good.Encode()).Err(); err != nil {
		t.Fatal(err)
	}
	got, ok := s.next(t, 5*time.Second)
	if !ok || got != good {
		t.Fatalf("first event delivered: %+v (received %v), want only the good one %+v", got, ok, good)
	}
	if n := testutil.ToFloat64(s.metrics.Malformed); n != float64(len(bad)) {
		t.Errorf("malformed counter %v, want %d", n, len(bad))
	}
	if n := testutil.ToFloat64(s.metrics.Received); n != 1 {
		t.Errorf("received counter %v, want 1", n)
	}
}

// §8, §9: an event is read up to MaxEventBytes, and one byte more is refused.
func TestAnEventIsReadUpToItsBound(t *testing.T) {
	at := func(size int) string {
		e := `{"id":"` + uuid.NewString() + `","status":"sent","pad":"`
		return e + strings.Repeat("x", size-len(e)-2) + `"}`
	}
	if _, err := Decode(at(MaxEventBytes)); err != nil {
		t.Errorf("an event of %d bytes: %v, want it read", MaxEventBytes, err)
	}
	if _, err := Decode(at(MaxEventBytes + 1)); err == nil {
		t.Errorf("an event of %d bytes was read", MaxEventBytes+1)
	}
}
