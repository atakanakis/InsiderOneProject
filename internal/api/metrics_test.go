package api

import (
	"bufio"
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/atakanakis/InsiderOneProject/internal/events"
	"github.com/atakanakis/InsiderOneProject/internal/observability"
	"github.com/atakanakis/InsiderOneProject/internal/redisclient"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
}

// queueRows adds rows in the four queue statuses: sms has 2 pending rows due for 90 s, one
// scheduled in an hour, one retrying in a minute, one processing; email has one pending row
// due for 10 s; push has only a row scheduled in an hour, so nothing due. Rows that are final
// are not in the queue.
func queueRows(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	exec(t, pool, `INSERT INTO notifications (id, recipient, channel, content, priority, status, attempts, max_attempts, ready_at, locked_until)
SELECT gen_random_uuid(), '+905551234567', c, 'hello', 1, s, 1, 6, now() + r::interval, CASE WHEN s = 'processing' THEN now() + interval '1 minute' END
FROM (VALUES ('sms', 'pending', '-90 seconds'), ('sms', 'pending', '-30 seconds'), ('sms', 'scheduled', '1 hour'),
             ('sms', 'retrying', '1 minute'), ('sms', 'processing', '-5 seconds'), ('email', 'pending', '-10 seconds'),
             ('sms', 'sent', '-1 hour'), ('push', 'failed', '-1 hour'), ('push', 'scheduled', '1 hour')) AS v(c, s, r)`)
}

// attemptRows adds delivered notifications of a channel created ago, each with one attempt
// that started startedAgo, lasting the given durations, with the outcome.
func attemptRows(t *testing.T, pool *pgxpool.Pool, channel, outcome, created, started string, durations ...int) {
	t.Helper()
	exec(t, pool, `WITH n AS (
    INSERT INTO notifications (id, recipient, channel, content, priority, status, attempts, max_attempts, ready_at, created_at)
    SELECT gen_random_uuid(), '+905551234567', $1, 'hello', 1, 'sent', 1, 6, now(), now() - $3::interval
    FROM unnest($5::int[]) RETURNING id)
INSERT INTO delivery_attempts (notification_id, attempt_number, started_at, duration_ms, status_code, outcome)
SELECT n.id, 1, now() - $4::interval, d.ms, 202, $2
FROM (SELECT id, row_number() OVER () AS i FROM n) n
JOIN (SELECT ms, row_number() OVER () AS i FROM unnest($5::int[]) AS ms) d USING (i)`, channel, outcome, created, started, durations)
}

func number(t *testing.T, v any, path string) float64 {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%s is %v, not a number", path, v)
	}
	return f
}

func obj(t *testing.T, v any, path string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is %v, not an object", path, v)
	}
	return m
}

// §10: GET /v1/metrics gives, per channel, the queue and the attempts of the recent window:
// how many by outcome, per second, the share delivered, and percentiles of the attempt's
// duration and of the delivery latency.
func TestTheMetricsEndpointReportsTheRecentWindow(t *testing.T) {
	a := newAPI(t)
	queueRows(t, a.Pool)
	attemptRows(t, a.Pool, "sms", "sent", "2 minutes", "1 minute", 100, 200, 300)
	attemptRows(t, a.Pool, "sms", "error response", "30 minutes", "1 minute", 50) // not a delivery: no latency
	attemptRows(t, a.Pool, "sms", "sent", "11 minutes", "10 minutes", 400)        // in 15m and 1h only
	attemptRows(t, a.Pool, "sms", "sent", "3 hours", "2 hours", 500)              // in no window

	r := a.get(t, "/v1/metrics")
	if r.code != http.StatusOK {
		t.Fatalf("GET /v1/metrics: %d %s", r.code, r.raw)
	}
	m := r.json(t)
	if m["window"] != "5m" {
		t.Errorf("window %v, want the default 5m", m["window"])
	}
	sms := obj(t, obj(t, m["channels"], "channels")["sms"], "channels.sms")
	queue := obj(t, sms["queue"], "sms.queue")
	for status, want := range map[string]float64{"scheduled": 1, "pending": 2, "retrying": 1, "processing": 1} {
		if got := number(t, queue[status], "sms.queue."+status); got != want {
			t.Errorf("sms queue %s = %v, want %v", status, got, want)
		}
	}
	if len(queue) != 4 {
		t.Errorf("sms queue %v, want the four statuses of the queue only", queue)
	}
	if age := number(t, sms["oldestDueAgeSeconds"], "sms.oldestDueAgeSeconds"); age < 90 || age > 120 {
		t.Errorf("sms oldest due row is %v s old, want about 90", age)
	}
	attempts := obj(t, sms["attempts"], "sms.attempts")
	byOutcome := obj(t, attempts["byOutcome"], "sms.attempts.byOutcome")
	if number(t, attempts["total"], "total") != 4 || number(t, byOutcome["sent"], "sent") != 3 ||
		number(t, byOutcome["error_response"], "error_response") != 1 || len(byOutcome) != len(observability.Outcomes) {
		t.Errorf("sms attempts %v, want 4: 3 sent and 1 error_response, every outcome listed", attempts)
	}
	if ps := number(t, attempts["perSecond"], "perSecond"); math.Abs(ps-4.0/300) > 1e-9 {
		t.Errorf("sms attempts per second %v, want 4/300", ps)
	}
	if share := number(t, sms["deliveredShare"], "deliveredShare"); share != 0.75 {
		t.Errorf("sms delivered share %v, want 0.75", share)
	}
	duration := obj(t, sms["attemptDurationMs"], "attemptDurationMs")
	if duration["p50"] != float64(100) || duration["p95"] != float64(300) || duration["p99"] != float64(300) {
		t.Errorf("sms attempt duration %v, want p50 100, p95 300, p99 300 (50, 100, 200, 300 ms)", duration)
	}
	latency := obj(t, sms["deliveryLatencyMs"], "deliveryLatencyMs")
	if p50 := number(t, latency["p50"], "latency p50"); math.Abs(p50-60200) > 1000 {
		t.Errorf("sms delivery latency p50 %v ms, want about 60,200 (created 2 min ago, delivered 1 min ago in 200 ms)", p50)
	}
	if p95 := number(t, latency["p95"], "latency p95"); math.Abs(p95-60300) > 1000 {
		t.Errorf("sms delivery latency p95 %v ms, want about 60,300: only the attempts that delivered count", p95)
	}
	email := obj(t, obj(t, m["channels"], "channels")["email"], "channels.email")
	if age := number(t, email["oldestDueAgeSeconds"], "email age"); age < 10 || age > 40 {
		t.Errorf("email oldest due row is %v s old, want about 10", age)
	}
	push := obj(t, obj(t, m["channels"], "channels")["push"], "channels.push")
	if push["deliveredShare"] != nil || push["attemptDurationMs"] != nil || number(t, push["oldestDueAgeSeconds"], "push age") != 0 {
		t.Errorf("push, with no attempts and only a row scheduled in an hour: %v; want no share, no percentiles, age 0", push)
	}

	for window, want := range map[string]float64{"1m": 0, "15m": 5, "1h": 5} {
		r := a.get(t, "/v1/metrics?window="+window)
		if r.code != http.StatusOK {
			t.Fatalf("window %s: %d %s", window, r.code, r.raw)
		}
		sms := obj(t, obj(t, r.json(t)["channels"], "channels")["sms"], "sms")
		if got := number(t, obj(t, sms["attempts"], "attempts")["total"], "total"); got != want {
			t.Errorf("window %s: %v attempts, want %v", window, got, want)
		}
	}
	for _, bad := range []string{"2h", "30s", "5", "abc", "1h&window=5m"} {
		if r := a.get(t, "/v1/metrics?window="+bad); r.code != http.StatusBadRequest {
			t.Errorf("window %q: %d, want 400", bad, r.code)
		}
	}
}

// §10: an answer is computed at most once per 5 s per window and instance, however many
// clients ask.
func TestTheMetricsEndpointIsComputedOncePerFiveSeconds(t *testing.T) {
	a := newAPI(t)
	attemptRows(t, a.Pool, "sms", "sent", "1 minute", "30 seconds", 100)
	total := func(window string) float64 {
		r := a.get(t, "/v1/metrics?window="+window)
		if r.code != http.StatusOK {
			t.Fatalf("%d %s", r.code, r.raw)
		}
		return number(t, obj(t, obj(t, obj(t, r.json(t)["channels"], "c")["sms"], "sms")["attempts"], "a")["total"], "total")
	}
	if got := total("5m"); got != 1 {
		t.Fatalf("first answer: %v attempts, want 1", got)
	}
	attemptRows(t, a.Pool, "sms", "sent", "1 minute", "30 seconds", 100)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if got := total("5m"); got != 1 {
				t.Errorf("within 5 s: %v attempts, want the first answer's 1", got)
			}
		})
	}
	wg.Wait()
	if got := total("15m"); got != 2 {
		t.Errorf("another window, asked for the first time: %v attempts, want 2", got)
	}
	time.Sleep(5 * time.Second)
	if got := total("5m"); got != 2 {
		t.Errorf("after 5 s: %v attempts, want 2", got)
	}
}

// scrape returns the API's /metrics as "name{labels}" -> value.
func scrape(t *testing.T, base string) map[string]float64 {
	t.Helper()
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics: %d", resp.StatusCode)
	}
	out := map[string]float64{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndex(line, " ")
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			t.Fatalf("/metrics line %q", line)
		}
		out[line[:i]] = v
	}
	return out
}

// §10: the queue's depth by channel and status and the age of its oldest due row are read
// from Postgres, with the database's clock; /metrics needs no API key.
func TestQueueGaugesComeFromTheDatabase(t *testing.T) {
	a := newAPI(t, "metrics-key-0123456789")
	queueRows(t, a.Pool)
	got := scrape(t, a.url)
	for series, want := range map[string]float64{
		`notifications_queue_depth{channel="sms",status="pending"}`:     2,
		`notifications_queue_depth{channel="sms",status="scheduled"}`:   1,
		`notifications_queue_depth{channel="sms",status="retrying"}`:    1,
		`notifications_queue_depth{channel="sms",status="processing"}`:  1,
		`notifications_queue_depth{channel="email",status="pending"}`:   1,
		`notifications_queue_depth{channel="push",status="pending"}`:    0,
		`notifications_queue_depth{channel="push",status="processing"}`: 0,
		`notifications_queue_depth{channel="push",status="scheduled"}`:  1,
		`notifications_queue_oldest_due_age_seconds{channel="push"}`:    0,
	} {
		if v, ok := got[series]; !ok || v != want {
			t.Errorf("%s = %v (exported %v), want %v", series, v, ok, want)
		}
	}
	if age := got[`notifications_queue_oldest_due_age_seconds{channel="sms"}`]; age < 90 || age > 120 {
		t.Errorf("sms oldest due age %v, want about 90", age)
	}
	for series := range got {
		if strings.Contains(series, `status="sent"`) && strings.HasPrefix(series, "notifications_queue_depth") {
			t.Errorf("a final status in the queue's depth: %s", series)
		}
	}
}

// §10: no label of any metric takes a value from a request: not an id, not a path, not a
// method the client made up.
func TestMetricLabelsHaveBoundedValues(t *testing.T) {
	a := newAPI(t)
	var made []string
	for range 20 {
		id := uuid.NewString()
		made = append(made, id)
		a.get(t, "/v1/notifications/"+id)
		a.get(t, "/no/such/"+id)
		a.do(t, "BREW"+id[:4], "/healthz", nil)
		a.do(t, http.MethodDelete, "/v1/notifications/"+id, nil)
	}
	a.get(t, "/v1/notifications?status=pending")
	routes := map[string]bool{"unmatched": true}
	for _, p := range routePatterns() {
		routes[p] = true
	}
	methods := map[string]bool{"other": true}
	for _, m := range observability.HTTPMethods {
		methods[m] = true
	}
	allowed := map[string]bool{"channel": true, "status": true, "outcome": true, "operation": true, "reason": true,
		"method": true, "route": true, "code": true}
	series := 0
	for name := range scrape(t, a.url) {
		if !strings.HasPrefix(name, "notifications_") {
			continue
		}
		series++
		labels := ""
		if i := strings.Index(name, "{"); i >= 0 {
			labels = name[i+1 : len(name)-1]
		}
		for _, pair := range splitLabels(labels) {
			k, v, _ := strings.Cut(pair, "=")
			v = strings.Trim(v, `"`)
			if !allowed[k] && k != "le" {
				t.Errorf("%s: label %s is not one of the fixed sets", name, k)
			}
			if k == "route" && !routes[v] || k == "method" && !methods[v] {
				t.Errorf("%s: %s=%q is not a value of its fixed set", name, k, v)
			}
			for _, id := range made {
				if strings.Contains(v, id) || strings.Contains(v, id[:4]) && k != "le" && k != "code" {
					t.Errorf("%s: a label carries a value from a request (%s)", name, id)
				}
			}
		}
	}
	t.Logf("%d series after 80 requests with made-up ids, paths and methods", series)
	if series > 2000 {
		t.Errorf("%d series", series)
	}
}

// splitLabels splits `a="x",b="y"` into its pairs; values here hold no quote or comma.
func splitLabels(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// §9, §8: publishing never makes an answer wait. With the real publisher and a Redis that
// accepts connections and never answers, a create, a batch of 1000 and a cancel each answer
// at once.
func TestTheAPIAnswersWhileRedisHangs(t *testing.T) {
	e := testkit.Setup(t)
	hung, err := redisclient.New(testkit.HungRedis(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hung.Close() })
	m := observability.NewAPIMetrics(prometheus.NewRegistry())
	pub := events.NewRedis(hung, events.Options{Metrics: m.Publish})
	h := NewHandler(observability.NewLogger(testLog{t}, "api", "error"), Options{
		Health: Health{Postgres: e.Pool.Ping, Redis: ok}, Pool: e.Pool, MaxAttempts: 6, Events: pub, Metrics: m,
	})
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	srv := apiEnv{Env: e, url: server.URL}
	timed := func(what string, f func() reply) reply {
		t.Helper()
		began := time.Now()
		r := f()
		if d := time.Since(began); d > time.Second {
			t.Errorf("%s took %v while Redis hangs", what, d)
		}
		return r
	}
	one := timed("a create", func() reply { return srv.do(t, http.MethodPost, "/v1/notifications", []byte(smsJSON)) })
	if one.code != http.StatusCreated {
		t.Fatalf("create: %d %s", one.code, one.raw)
	}
	var b strings.Builder
	b.WriteString(`{"notifications": [`)
	for i := range 1000 {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(smsJSON)
	}
	b.WriteString("]}")
	if r := timed("a batch of 1000", func() reply { return srv.do(t, http.MethodPost, "/v1/notifications/batch", []byte(b.String())) }); r.code != http.StatusCreated {
		t.Fatalf("batch: %d %.200s", r.code, r.raw)
	}
	id := one.json(t)["id"].(string)
	if r := timed("a cancel", func() reply { return srv.do(t, http.MethodPost, "/v1/notifications/"+id+"/cancel", nil) }); r.code != http.StatusOK {
		t.Fatalf("cancel: %d %s", r.code, r.raw)
	}
}
