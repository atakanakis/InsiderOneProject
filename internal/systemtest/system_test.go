package systemtest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/redis/go-redis/v9"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/atakanakis/InsiderOneProject/internal/api"
	"github.com/atakanakis/InsiderOneProject/internal/dispatch"
	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/events"
	"github.com/atakanakis/InsiderOneProject/internal/observability"
	"github.com/atakanakis/InsiderOneProject/internal/provider"
	"github.com/atakanakis/InsiderOneProject/internal/ratelimit"
	"github.com/atakanakis/InsiderOneProject/internal/recorder"
	"github.com/atakanakis/InsiderOneProject/internal/redisclient"
	"github.com/atakanakis/InsiderOneProject/internal/store"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

func TestMain(m *testing.M) { testkit.Main(m) }

// lines is a log destination the test reads while the processes write.
type lines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lines) all(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	text := l.buf.String()
	l.mu.Unlock()
	var out []map[string]any
	for _, s := range strings.Split(strings.TrimSpace(text), "\n") {
		if s == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatalf("not a JSON line: %q", s)
		}
		out = append(out, m)
	}
	return out
}

// system is an API and a worker on the test's Postgres and Redis, with a recording provider.
type system struct {
	env      *testkit.Env
	api      *httptest.Server
	provider *recorder.Server
	apiLog   *lines
	workLog  *lines
}

type setup struct {
	respond    func(recorder.Request) recorder.Response
	apiTracing trace.TracerProvider
	wrkTracing trace.TracerProvider
	events     bool // status events through Redis, and a hub fed by a subscription
}

func start(t *testing.T, s setup) *system {
	t.Helper()
	e := testkit.Setup(t)
	sys := &system{env: e, provider: testkit.Recorder(t, recorder.Options{Respond: s.respond}), apiLog: &lines{}, workLog: &lines{}}
	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	t.Cleanup(func() { cancel(); running.Wait() })

	var apiEvents, workerEvents events.Publisher = events.Discard{}, events.Discard{}
	metrics := api.NewMetrics()
	hub := api.NewHub(api.HubOptions{Metrics: metrics})
	if s.events {
		apiEvents = events.NewRedis(redisClient(t, e.RedisURL), events.Options{Metrics: metrics.Publish})
		workerEvents = events.NewRedis(redisClient(t, e.RedisURL), events.Options{})
		sub := redisClient(t, e.RedisURL)
		running.Go(func() { events.Subscribe(ctx, sub, hub.Deliver, events.SubscribeOptions{Metrics: metrics.Subscribe}) })
	}
	sys.api = httptest.NewServer(api.NewHandler(observability.NewLogger(sys.apiLog, "api", "debug"), api.Options{
		Health: api.Health{Postgres: e.Pool.Ping, Redis: func(context.Context) error { return nil }},
		Pool:   e.Pool, MaxAttempts: 6, Events: apiEvents, Metrics: metrics, Hub: hub, Tracing: s.apiTracing,
	}))
	t.Cleanup(sys.api.Close)

	pool, err := store.Connect(context.Background(), e.DatabaseURL, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	limiter, err := ratelimit.New(redisClient(t, e.RedisURL), 100, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range domain.Channels {
		d, err := dispatch.New(dispatch.Options{
			Channel: ch, Slots: 10, ClaimBatch: 10, Lease: 20 * time.Second,
			PauseBusy: 20 * time.Millisecond, PauseIdle: 50 * time.Millisecond, BreakerOpen: 3500 * time.Millisecond,
			Retry: dispatch.RetryPolicy{Base: 100 * time.Millisecond, Cap: time.Second, Soon: time.Second},
			Provider: provider.Options{URL: sys.provider.URL + "/send", SendTimeout: 3 * time.Second, GateBudget: 2 * time.Second,
				DialTimeout: time.Second, TLSHandshakeTimeout: time.Second, IdleConns: 10, IdleConnTimeout: 10 * time.Second},
			Log: observability.NewLogger(sys.workLog, "worker", "debug"), Events: workerEvents, Tracing: s.wrkTracing,
		}, store.NewQueue(pool), limiter)
		if err != nil {
			t.Fatal(err)
		}
		running.Go(func() { d.Run(ctx) })
	}
	return sys
}

func redisClient(t *testing.T, rawURL string) *redis.Client {
	t.Helper()
	c, err := redisclient.New(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// create posts one notification and returns its id and the answer's correlation id.
func (s *system) create(t *testing.T, body string, header ...string) (string, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.api.URL+"/v1/notifications", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, raw)
	}
	var out struct{ ID string }
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out.ID, resp.Header.Get("X-Correlation-ID")
}

func (s *system) waitStatus(t *testing.T, id string, want domain.Status) {
	t.Helper()
	var got string
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if err := s.env.Pool.QueryRow(context.Background(), `SELECT status FROM notifications WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got == string(want) {
			return
		}
	}
	t.Fatalf("notification %s is %s, want %s", id, got, want)
}

const smsJSON = `{"recipient": "+905551234567", "channel": "sms", "content": "Your code is 1234"}`

// §10: one correlation id follows a message from the API request to the provider: the API
// answers it, stores it, the worker logs it on every line about the notification, and the
// provider receives it on every attempt.
func TestOneCorrelationIDFollowsAMessageFromTheRequestToTheProvider(t *testing.T) {
	var tries atomic.Int64
	sys := start(t, setup{respond: func(recorder.Request) recorder.Response {
		if tries.Add(1) == 1 {
			return recorder.Response{Status: 503} // a second attempt, with its own lines
		}
		return recorder.Response{}
	}})
	const corr = "order-42.retry_1"
	id, answered := sys.create(t, smsJSON, "X-Correlation-ID", corr)
	if answered != corr {
		t.Errorf("the API answered the correlation id %q, want %q", answered, corr)
	}
	sys.waitStatus(t, id, domain.Sent)

	attempts := 0
	for _, r := range sys.provider.Requests() {
		if r.Header.Get("Idempotency-Key") != id {
			continue
		}
		attempts++
		if got := r.Header.Get("X-Correlation-ID"); got != corr {
			t.Errorf("the provider received the correlation id %q, want %q", got, corr)
		}
	}
	if attempts != 2 {
		t.Errorf("the provider received %d attempts, want 2", attempts)
	}
	about := 0
	for _, l := range sys.workLog.all(t) {
		if l["notification_id"] != id {
			continue
		}
		about++
		if l["correlation_id"] != corr {
			t.Errorf("a worker line about the notification has the correlation id %v: %v", l["correlation_id"], l)
		}
	}
	if about < 2 {
		t.Errorf("%d worker lines about the notification, want one per attempt at least", about)
	}
	requestLogged := false
	for _, l := range sys.apiLog.all(t) {
		if l["msg"] == "request" && l["correlation_id"] == corr {
			requestLogged = true
		}
	}
	if !requestLogged {
		t.Error("the API's line about the request does not carry the correlation id")
	}
}

// §10: the worker's delivery span joins the trace of the API request that created the
// notification: the same trace id, and the request's span as its parent. A client's own
// traceparent makes the request's span its child.
func TestTheDeliverySpanJoinsTheAPIRequestsTrace(t *testing.T) {
	apiSpans, workerSpans := tracetest.NewSpanRecorder(), tracetest.NewSpanRecorder()
	sys := start(t, setup{
		apiTracing: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(apiSpans)),
		wrkTracing: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(workerSpans)),
	})
	const client = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	plain, _ := sys.create(t, smsJSON)
	joined, _ := sys.create(t, smsJSON, "traceparent", client)
	sys.waitStatus(t, plain, domain.Sent)
	sys.waitStatus(t, joined, domain.Sent)

	var requests []sdktrace.ReadOnlySpan
	for _, s := range apiSpans.Ended() {
		if s.Name() == "POST /v1/notifications" && s.SpanKind() == trace.SpanKindServer {
			requests = append(requests, s)
		}
	}
	if len(requests) != 2 {
		t.Fatalf("%d spans of POST /v1/notifications, want 2", len(requests))
	}
	stored := map[string]string{}
	for _, id := range []string{plain, joined} {
		var tp string
		if err := sys.env.Pool.QueryRow(context.Background(), `SELECT traceparent FROM notifications WHERE id = $1`, id).Scan(&tp); err != nil {
			t.Fatal(err)
		}
		stored[id] = tp
	}
	deliveries := map[string]sdktrace.ReadOnlySpan{}
	waitSpans := time.Now().Add(5 * time.Second)
	for len(deliveries) < 2 && time.Now().Before(waitSpans) {
		for _, s := range workerSpans.Ended() {
			for _, a := range s.Attributes() {
				if a.Key == "notification.id" {
					deliveries[a.Value.AsString()] = s
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, id := range []string{plain, joined} {
		d, ok := deliveries[id]
		if !ok {
			t.Fatalf("no delivery span for %s", id)
		}
		var request sdktrace.ReadOnlySpan
		for _, r := range requests {
			if strings.Contains(stored[id], r.SpanContext().SpanID().String()) {
				request = r
			}
		}
		if request == nil {
			t.Fatalf("the stored traceparent %q of %s names no request span", stored[id], id)
		}
		if d.SpanContext().TraceID() != request.SpanContext().TraceID() || d.Parent().SpanID() != request.SpanContext().SpanID() {
			t.Errorf("delivery of %s: trace %s, parent %s; want the request's trace %s and span %s", id,
				d.SpanContext().TraceID(), d.Parent().SpanID(), request.SpanContext().TraceID(), request.SpanContext().SpanID())
		}
		if id == joined && request.Parent().TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
			t.Errorf("the request with the client's traceparent is in trace %s, want the client's", request.SpanContext().TraceID())
		}
	}
}

// §9: a WebSocket client that follows a notification receives its current state, then the
// worker's processing and sent, through Redis and the API's subscription.
func TestAWebSocketClientReceivesTheSentEventOfARealDelivery(t *testing.T) {
	sys := start(t, setup{events: true})
	at := time.Now().Add(2 * time.Second).UTC().Format(time.RFC3339Nano)
	id, _ := sys.create(t, `{"recipient": "+905551234567", "channel": "sms", "content": "later", "scheduledAt": "`+at+`"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(sys.api.URL, "http")+"/v1/ws?notificationId="+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseNow() }()
	var seen []string
	for len(seen) == 0 || seen[len(seen)-1] != "sent" {
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("after %v: %v", seen, err)
		}
		var m struct{ Type, ID, Status string }
		if err := json.Unmarshal(b, &m); err != nil || m.Type != "status" || m.ID != id {
			t.Fatalf("message %s", b)
		}
		seen = append(seen, m.Status)
	}
	if seen[0] != "scheduled" || !strings.Contains(strings.Join(seen, ","), "processing,sent") {
		t.Errorf("the client saw %v; want the current state scheduled first, then processing and sent", seen)
	}
}
