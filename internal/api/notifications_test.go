package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/observability"
	"github.com/atakanakis/InsiderOneProject/internal/store"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

type recordedEvents struct {
	mu   sync.Mutex
	seen map[uuid.UUID][]domain.Status
}

func (r *recordedEvents) Publish(_ context.Context, id uuid.UUID, s domain.Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = map[uuid.UUID][]domain.Status{}
	}
	r.seen[id] = append(r.seen[id], s)
}

func (r *recordedEvents) of(id uuid.UUID) []domain.Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[id]
}

type apiEnv struct {
	*testkit.Env
	url    string
	events *recordedEvents
}

func newAPI(t *testing.T, keys ...string) apiEnv {
	t.Helper()
	e := testkit.Setup(t)
	ev := &recordedEvents{}
	h := NewHandler(observability.NewLogger(io.Discard, "api", "error"), Options{
		Health:  Health{Postgres: e.Pool.Ping, Redis: ok},
		Pool:    e.Pool,
		APIKeys: keys, MaxAttempts: 6, Events: ev,
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return apiEnv{Env: e, url: srv.URL, events: ev}
}

type reply struct {
	code int
	raw  []byte
}

func (r reply) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.raw, &m); err != nil {
		t.Fatalf("answer %d is not a JSON object: %v: %.300s", r.code, err, r.raw)
	}
	return m
}

// errorFields returns the field names of an error answer.
func (r reply) errorFields(t *testing.T) []string {
	t.Helper()
	e, _ := r.json(t)["error"].(map[string]any)
	var out []string
	fields, _ := e["fields"].([]any)
	for _, f := range fields {
		out = append(out, f.(map[string]any)["field"].(string))
	}
	return out
}

func (a apiEnv) do(t *testing.T, method, path string, body []byte, header ...string) reply {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, a.url+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply{resp.StatusCode, raw}
}

func (a apiEnv) post(t *testing.T, path string, v any, header ...string) reply {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return a.do(t, http.MethodPost, path, b, header...)
}

func (a apiEnv) get(t *testing.T, path string) reply {
	t.Helper()
	return a.do(t, http.MethodGet, path, nil)
}

func sms(content string) map[string]any {
	return map[string]any{"recipient": "+905551234567", "channel": "sms", "content": content}
}

func items(n int, item func(i int) map[string]any) map[string]any {
	list := make([]map[string]any, n)
	for i := range list {
		list[i] = item(i)
	}
	return map[string]any{"notifications": list}
}

// pad returns body with JSON whitespace appended up to size bytes.
func pad(t *testing.T, v any, size int) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > size {
		t.Fatalf("body of %d bytes is over %d before padding", len(b), size)
	}
	return append(b, bytes.Repeat([]byte(" "), size-len(b))...)
}

// allocated returns the bytes the process allocated while f ran.
func allocated(f func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func count(t *testing.T, a apiEnv, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := a.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateAndReadANotification(t *testing.T) {
	a := newAPI(t)
	r := a.post(t, "/v1/notifications", sms("hello"), "X-Correlation-ID", "corr-1")
	if r.code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.code, r.raw)
	}
	created := r.json(t)
	if created["status"] != "pending" || created["priority"] != "normal" || created["maxAttempts"] != 6.0 {
		t.Errorf("created %v", created)
	}
	got := a.get(t, "/v1/notifications/"+created["id"].(string))
	if got.code != http.StatusOK {
		t.Fatalf("get: %d %s", got.code, got.raw)
	}
	n := got.json(t)
	if n["content"] != "hello" || n["correlationId"] != "corr-1" || n["attemptHistoryTotal"] != 0.0 {
		t.Errorf("read back %v", n)
	}
	if h, ok := n["attemptHistory"].([]any); !ok || len(h) != 0 {
		t.Errorf("attemptHistory %v", n["attemptHistory"])
	}
	if s := a.events.of(uuid.MustParse(created["id"].(string))); len(s) != 1 || s[0] != domain.Pending {
		t.Errorf("events %v", s)
	}
	if r := a.get(t, "/v1/notifications/"+uuid.NewString()); r.code != http.StatusNotFound {
		t.Errorf("unknown id: %d", r.code)
	}
	if r := a.get(t, "/v1/notifications/not-a-uuid"); r.code != http.StatusBadRequest {
		t.Errorf("malformed id: %d", r.code)
	}
}

func TestAScheduledNotificationWaitsUntilItsTime(t *testing.T) {
	a := newAPI(t)
	at := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	body := sms("later")
	body["scheduledAt"] = at.Format(time.RFC3339)
	n := a.post(t, "/v1/notifications", body).json(t)
	if n["status"] != "scheduled" || n["readyAt"] != at.Format(time.RFC3339) {
		t.Errorf("future: %v", n)
	}
	body["scheduledAt"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if n := a.post(t, "/v1/notifications", body).json(t); n["status"] != "pending" {
		t.Errorf("past: %v", n)
	}
}

// I10: identical requests at the same moment end with one notification.
func TestConcurrentIdenticalRequestsCreateOneNotification(t *testing.T) {
	a := newAPI(t)
	body := sms("once")
	body["idempotencyKey"] = "order-42"
	const n = 20
	replies := make([]reply, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { replies[i] = a.post(t, "/v1/notifications", body) })
	}
	wg.Wait()
	ids := map[string]bool{}
	created := 0
	for _, r := range replies {
		switch r.code {
		case http.StatusCreated:
			created++
		case http.StatusOK:
		default:
			t.Fatalf("answer %d %s", r.code, r.raw)
		}
		ids[r.json(t)["id"].(string)] = true
	}
	if created != 1 || len(ids) != 1 {
		t.Errorf("%d created, %d distinct ids", created, len(ids))
	}
	if c := count(t, a, `SELECT count(*) FROM notifications`); c != 1 {
		t.Errorf("%d rows", c)
	}
}

func TestAKeyReusedWithAnotherRequestIsAConflict(t *testing.T) {
	a := newAPI(t)
	body := sms("first")
	body["idempotencyKey"] = "k"
	if r := a.post(t, "/v1/notifications", body); r.code != http.StatusCreated {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	body["content"] = "second"
	if r := a.post(t, "/v1/notifications", body); r.code != http.StatusConflict {
		t.Errorf("another request with the key: %d %s", r.code, r.raw)
	}
	// The same request in another form (priority spelled out) is a repeat, not a conflict.
	body["content"], body["priority"] = "first", "normal"
	if r := a.post(t, "/v1/notifications", body); r.code != http.StatusOK {
		t.Errorf("the same request: %d %s", r.code, r.raw)
	}
	if c := count(t, a, `SELECT count(*) FROM notifications`); c != 1 {
		t.Errorf("%d rows", c)
	}
}

// I10 for batches: identical batches at the same moment end with one batch, and every
// answer has the same ids in the same order.
func TestConcurrentIdenticalBatchesCreateOneBatch(t *testing.T) {
	a := newAPI(t)
	body := items(100, func(i int) map[string]any { return sms(fmt.Sprint("item ", i)) })
	const n = 10
	replies := make([]reply, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { replies[i] = a.post(t, "/v1/notifications/batch", body, "Idempotency-Key", "batch-1") })
	}
	wg.Wait()
	created := 0
	var first string
	for _, r := range replies {
		if r.code == http.StatusCreated {
			created++
		} else if r.code != http.StatusOK {
			t.Fatalf("answer %d %s", r.code, r.raw)
		}
		m := r.json(t)
		ids, _ := json.Marshal(m["ids"])
		key := fmt.Sprint(m["batchId"], string(ids))
		if first == "" {
			first = key
		} else if key != first {
			t.Errorf("answers differ:\n%s\n%s", first, key)
		}
	}
	if created != 1 {
		t.Errorf("%d answers said created", created)
	}
	if c := count(t, a, `SELECT count(*) FROM notifications`); c != 100 {
		t.Errorf("%d rows", c)
	}
	if c := count(t, a, `SELECT count(*) FROM batches`); c != 1 {
		t.Errorf("%d batches", c)
	}
	// The ids are in the order of the items.
	m := replies[0].json(t)
	for i, id := range m["ids"].([]any) {
		n := a.get(t, "/v1/notifications/"+id.(string)).json(t)
		if n["content"] != fmt.Sprint("item ", i) {
			t.Fatalf("id %d has content %v", i, n["content"])
		}
	}
	body["notifications"].([]map[string]any)[0]["content"] = "changed"
	if r := a.post(t, "/v1/notifications/batch", body, "Idempotency-Key", "batch-1"); r.code != http.StatusConflict {
		t.Errorf("another batch with the key: %d %s", r.code, r.raw)
	}
}

func TestABatchIsValidatedWholeBeforeAnyIsStored(t *testing.T) {
	a := newAPI(t)
	body := items(10, func(i int) map[string]any { return sms("x") })
	body["notifications"].([]map[string]any)[7]["recipient"] = "bad"
	r := a.post(t, "/v1/notifications/batch", body)
	if r.code != http.StatusBadRequest || fmt.Sprint(r.errorFields(t)) != "[notifications[7].recipient]" {
		t.Errorf("%d %s", r.code, r.raw)
	}
	if c := count(t, a, `SELECT count(*) FROM notifications`); c != 0 {
		t.Errorf("%d rows stored", c)
	}
	body["notifications"].([]map[string]any)[7]["recipient"] = "+905551234567"
	body["notifications"].([]map[string]any)[3]["idempotencyKey"] = "item-key"
	if r := a.post(t, "/v1/notifications/batch", body); r.code != http.StatusBadRequest {
		t.Errorf("an item with its own key: %d %s", r.code, r.raw)
	}
}

// architecture §8: the request body is bounded, 1 MiB and 8 MiB for a batch; one byte over is
// a 413.
func TestRequestBodiesAreBoundedAtTheirLimit(t *testing.T) {
	a := newAPI(t)
	tmpl := map[string]any{"name": "t", "channel": "sms", "body": "hi"}
	for _, c := range []struct {
		path  string
		body  any
		limit int
	}{
		{"/v1/notifications", sms("x"), MaxBodyBytes},
		{"/v1/notifications/batch", items(1000, func(int) map[string]any { return sms("x") }), MaxBatchBodyBytes},
		{"/v1/templates", tmpl, MaxBodyBytes},
	} {
		for _, size := range []int{c.limit - 1, c.limit, c.limit + 1} {
			_, _ = a.Pool.Exec(context.Background(), `TRUNCATE notifications, batches, templates CASCADE`)
			r := a.do(t, http.MethodPost, c.path, pad(t, c.body, size))
			if over := r.code == http.StatusRequestEntityTooLarge; over != (size > c.limit) {
				t.Errorf("%s with %d bytes (limit %d): %d %.200s", c.path, size, c.limit, r.code, r.raw)
			}
			if size <= c.limit && r.code != http.StatusCreated {
				t.Errorf("%s with %d bytes: %d %.200s", c.path, size, r.code, r.raw)
			}
		}
	}
}

func TestBatchSizeIsBoundedAtAThousand(t *testing.T) {
	a := newAPI(t)
	for _, n := range []int{999, 1000, 1001} {
		r := a.post(t, "/v1/notifications/batch", items(n, func(int) map[string]any { return sms("x") }))
		if (r.code == http.StatusCreated) != (n <= domain.MaxBatchItems) {
			t.Errorf("%d items: %d %.200s", n, r.code, r.raw)
		}
		if n > domain.MaxBatchItems && r.code != http.StatusBadRequest {
			t.Errorf("%d items: %d, want 400", n, r.code)
		}
	}
}

// I11: 8 MiB of empty items is refused at the 1,001st, without decoding millions of items.
func TestABatchOfManyEmptyItemsIsRefusedWithoutDecodingThemAll(t *testing.T) {
	a := newAPI(t)
	var b bytes.Buffer
	b.WriteString(`{"notifications":[{}`)
	for b.Len() < MaxBatchBodyBytes-10 {
		b.WriteString(`,{}`)
	}
	b.WriteString(`]}`)
	var r reply
	alloc := allocated(func() { r = a.do(t, http.MethodPost, "/v1/notifications/batch", b.Bytes()) })
	if r.code != http.StatusBadRequest {
		t.Fatalf("%d %.300s", r.code, r.raw)
	}
	t.Logf("%d items in %d bytes refused; %d MiB allocated", b.Len()/3, b.Len(), alloc>>20)
	if alloc > 64<<20 {
		t.Errorf("allocated %d MiB", alloc>>20)
	}
}

// I11: a million variables are refused at the 51st, while they are decoded.
func TestManyVariablesAreRefusedWhileDecoding(t *testing.T) {
	a := newAPI(t)
	var b bytes.Buffer
	b.WriteString(`{"recipient":"+905551234567","channel":"sms","template":"t","variables":{"v0":""`)
	for i := 1; b.Len() < MaxBodyBytes-20; i++ {
		fmt.Fprintf(&b, `,"v%d":""`, i)
	}
	b.WriteString(`}}`)
	var r reply
	alloc := allocated(func() { r = a.do(t, http.MethodPost, "/v1/notifications", b.Bytes()) })
	if r.code != http.StatusBadRequest {
		t.Fatalf("%d %.300s", r.code, r.raw)
	}
	t.Logf("%d bytes of variables refused; %d MiB allocated", b.Len(), alloc>>20)
	if alloc > 16<<20 {
		t.Errorf("allocated %d MiB", alloc>>20)
	}
}

func createTemplate(t *testing.T, a apiEnv, name, channel, body string) {
	t.Helper()
	r := a.post(t, "/v1/templates", map[string]any{"name": name, "channel": channel, "body": body})
	if r.code != http.StatusCreated {
		t.Fatalf("template %s: %d %.300s", name, r.code, r.raw)
	}
}

func email(content string) map[string]any {
	return map[string]any{"recipient": "ada@example.com", "channel": "email", "content": content}
}

func fromTemplate(name string, vars map[string]string) map[string]any {
	return map[string]any{"recipient": "ada@example.com", "channel": "email", "template": name, "variables": vars}
}

// architecture §8: the rendered content of one request is bounded. Each "amp" item renders
// 16,000 copies of 6 four-byte characters: 384,000 bytes from a 100-byte item. As many as fit
// and one direct item with the rest are exactly the bound.
func TestTheRenderedTotalOfARequestIsBounded(t *testing.T) {
	if domain.MaxRenderedBytes > MaxBatchBodyBytes {
		t.Fatalf("the rendered bound %d is larger than the batch's body bound %d", domain.MaxRenderedBytes, MaxBatchBodyBytes)
	}
	a := newAPI(t)
	createTemplate(t, a, "amp", "email", strings.Repeat("{{.v}}", 16_000))
	emoji := strings.Repeat("😀", 6)
	const amps = domain.MaxRenderedBytes / 384_000
	const rest = domain.MaxRenderedBytes - amps*384_000
	if rest/4 > domain.ContentLimit(domain.Email) {
		t.Fatalf("the direct item of %d bytes is over the email bound", rest)
	}
	batch := func(directBytes int) map[string]any {
		list := []map[string]any{}
		for range amps {
			list = append(list, fromTemplate("amp", map[string]string{"v": emoji}))
		}
		direct := strings.Repeat("😀", directBytes/4) + strings.Repeat("a", directBytes%4)
		return map[string]any{"notifications": append(list, email(direct))}
	}
	for _, n := range []int{rest - 1, rest, rest + 1} {
		r := a.post(t, "/v1/notifications/batch", batch(n))
		if (r.code == http.StatusCreated) != (n <= rest) {
			t.Errorf("total %d bytes (bound %d): %d %.300s", amps*384_000+n, domain.MaxRenderedBytes, r.code, r.raw)
		}
	}
}

// I11: a request that would render far more than the bound is refused before anything is
// rendered: 1000 items that would render 384 MB.
func TestARequestOverTheRenderedTotalIsRefusedBeforeRendering(t *testing.T) {
	a := newAPI(t)
	createTemplate(t, a, "amp", "email", strings.Repeat("{{.v}}", 16_000))
	body := items(1000, func(int) map[string]any {
		return fromTemplate("amp", map[string]string{"v": strings.Repeat("😀", 6)})
	})
	var r reply
	alloc := allocated(func() { r = a.post(t, "/v1/notifications/batch", body) })
	if r.code != http.StatusBadRequest {
		t.Fatalf("%d %.300s", r.code, r.raw)
	}
	t.Logf("refused; %d MiB allocated", alloc>>20)
	if alloc > 64<<20 {
		t.Errorf("allocated %d MiB: the request was rendered", alloc>>20)
	}
}

// architecture §8: a template is compiled once per request. 500 items of a template with
// 16,000 placeholders: compiled per item, that is 8 million parts.
func TestATemplateIsCompiledOncePerRequest(t *testing.T) {
	a := newAPI(t)
	createTemplate(t, a, "many", "email", strings.Repeat("{{.v}}", 16_000))
	body := items(500, func(int) map[string]any { return fromTemplate("many", map[string]string{"v": "a"}) })
	var r reply
	alloc := allocated(func() { r = a.post(t, "/v1/notifications/batch", body) })
	if r.code != http.StatusCreated {
		t.Fatalf("%d %.300s", r.code, r.raw)
	}
	t.Logf("8 MB rendered and stored; %d MiB allocated", alloc>>20)
	if alloc > 256<<20 {
		t.Errorf("allocated %d MiB", alloc>>20)
	}
}

func TestTemplatesAreCreatedOnceAndRendered(t *testing.T) {
	a := newAPI(t)
	tmpl := map[string]any{"name": "welcome", "channel": "email", "body": "Hi {{.name}}!"}
	if r := a.post(t, "/v1/templates", tmpl); r.code != http.StatusCreated {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	if r := a.post(t, "/v1/templates", tmpl); r.code != http.StatusOK {
		t.Errorf("the same template again: %d", r.code)
	}
	tmpl["body"] = "Hello {{.name}}"
	if r := a.post(t, "/v1/templates", tmpl); r.code != http.StatusConflict {
		t.Errorf("another body under the name: %d", r.code)
	}
	if r := a.post(t, "/v1/templates", map[string]any{"name": "bad", "channel": "sms", "body": "{{if .x}}"}); r.code != http.StatusBadRequest {
		t.Errorf("a body with template language: %d", r.code)
	}
	got := a.get(t, "/v1/templates/welcome").json(t)
	if got["body"] != "Hi {{.name}}!" || fmt.Sprint(got["variables"]) != "[name]" {
		t.Errorf("read back %v", got)
	}
	if r := a.get(t, "/v1/templates/nope"); r.code != http.StatusNotFound {
		t.Errorf("unknown template: %d", r.code)
	}
	n := a.post(t, "/v1/notifications", fromTemplate("welcome", map[string]string{"name": "Ada"}))
	if n.code != http.StatusCreated || n.json(t)["content"] != "Hi Ada!" {
		t.Errorf("rendered: %d %s", n.code, n.raw)
	}
	for name, body := range map[string]map[string]any{
		"variables":            fromTemplate("welcome", map[string]string{"name": "Ada", "extra": "x"}),
		"variables.name":       fromTemplate("welcome", map[string]string{}),
		"template":             fromTemplate("nope", map[string]string{}),
		"template (sms)":       {"recipient": "+905551234567", "channel": "sms", "template": "welcome", "variables": map[string]string{"name": "x"}},
		"content and template": {"recipient": "ada@example.com", "channel": "email", "content": "x", "template": "welcome"},
	} {
		if r := a.post(t, "/v1/notifications", body); r.code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, r.code, r.raw)
		}
	}
}

// I11, architecture §8: an error answer is bounded whatever the request: 100 fields, 300
// bytes each, 64 KiB in all, and it never quotes a value of the request.
func TestErrorBodiesStayWithinTheirBoundForAnyRequest(t *testing.T) {
	a := newAPI(t)
	createTemplate(t, a, "t", "sms", "{{.a}}")
	long := strings.Repeat("Q", 1000)
	everyFieldWrong := items(1000, func(int) map[string]any {
		return map[string]any{"recipient": long, "channel": long, "content": long, "priority": long, "scheduledAt": long, "template": long}
	})
	cases := map[string]func() reply{
		"every field of 1000 items": func() reply { return a.post(t, "/v1/notifications/batch", everyFieldWrong) },
		"an unknown field with a long name": func() reply {
			return a.do(t, http.MethodPost, "/v1/notifications", []byte(`{"`+strings.Repeat("Q", 500_000)+`":1}`))
		},
		"a variable name the template does not use": func() reply {
			return a.post(t, "/v1/notifications", map[string]any{"recipient": "+905551234567", "channel": "sms", "template": "t",
				"variables": map[string]string{"a": "x", strings.Repeat("Q", 500_000): "x"}})
		},
		// 1000 items each with a name of control characters, which JSON escapes to 6 bytes
		// each: the largest error the form allows (session 6's reviewer found it).
		"names that JSON escapes": func() reply {
			return a.post(t, "/v1/notifications/batch", items(1000, func(int) map[string]any {
				return map[string]any{"recipient": "+905551234567", "channel": "sms", "template": "t",
					"variables": map[string]string{"a": "x", strings.Repeat("\x01", 100): "x"}}
			}))
		},
		"a long string where an object belongs": func() reply {
			return a.do(t, http.MethodPost, "/v1/notifications", []byte(`{"variables":"`+strings.Repeat("Q", 500_000)+`"}`))
		},
		"invalid JSON after a long string": func() reply {
			return a.do(t, http.MethodPost, "/v1/notifications", []byte(`{"content":"`+strings.Repeat("Q", 500_000)+`"`))
		},
		"a long template name": func() reply {
			return a.post(t, "/v1/templates", map[string]any{"name": long, "channel": long, "body": long})
		},
		"a long list filter":     func() reply { return a.get(t, "/v1/notifications?status="+long+"&cursor="+long+"&channel="+long) },
		"a long template cursor": func() reply { return a.get(t, "/v1/templates?cursor="+long) },
		"a long idempotency key": func() reply {
			return a.post(t, "/v1/notifications/batch", items(1, func(int) map[string]any { return sms("x") }), "Idempotency-Key", long)
		},
		"a long template to read": func() reply { return a.get(t, "/v1/templates/"+long) },
	}
	for name, run := range cases {
		r := run()
		if r.code < 400 || r.code >= 500 {
			t.Errorf("%s: %d %.200s", name, r.code, r.raw)
			continue
		}
		if len(r.raw) > MaxErrorBodyBytes {
			t.Errorf("%s: error body of %d bytes", name, len(r.raw))
		}
		if bytes.Contains(r.raw, []byte(strings.Repeat("Q", maxEchoedName+1))) {
			t.Errorf("%s: the error quotes the request: %.300s", name, r.raw)
		}
		e, _ := r.json(t)["error"].(map[string]any)
		if e == nil || e["code"] == nil || e["message"] == nil {
			t.Errorf("%s: not the error form: %.300s", name, r.raw)
		}
	}
	r := cases["every field of 1000 items"]()
	e := r.json(t)["error"].(map[string]any)
	if n := len(e["fields"].([]any)); n != domain.MaxFieldErrors || e["moreFields"].(float64) < 1000 {
		t.Errorf("%d fields kept, %v more", n, e["moreFields"])
	}
	t.Logf("every field of 1000 items wrong: %d bytes", len(r.raw))
	r = cases["names that JSON escapes"]()
	t.Logf("names that JSON escapes: %d bytes", len(r.raw))
}

func TestUnknownFieldsAndWrongTypesAreRefusedByName(t *testing.T) {
	a := newAPI(t)
	body := sms("x")
	body["colour"] = "red"
	if r := a.post(t, "/v1/notifications", body); r.code != http.StatusBadRequest || fmt.Sprint(r.errorFields(t)) != "[colour]" {
		t.Errorf("unknown field: %d %s", r.code, r.raw)
	}
	if r := a.do(t, http.MethodPost, "/v1/notifications", []byte(`{"recipient":5}`)); r.code != http.StatusBadRequest || fmt.Sprint(r.errorFields(t)) != "[recipient]" {
		t.Errorf("wrong type: %d %s", r.code, r.raw)
	}
	for body, want := range map[string]string{
		`{"Content":"x"}`:               "[Content]",
		`{"content":"a","content":"b"}`: "[content]",
		`[]`:                            "[]",
	} {
		if r := a.do(t, http.MethodPost, "/v1/notifications", []byte(body)); r.code != http.StatusBadRequest || fmt.Sprint(r.errorFields(t)) != want {
			t.Errorf("%s: %d %s, want the fields %s", body, r.code, r.raw, want)
		}
	}
	for body, want := range map[string]string{
		`{"notifications":[5]}`: "[notifications[0]]",
		`{"notifications":[{"recipient":"+905551234567","RECIPIENT":"x"}]}`: "[notifications[0].RECIPIENT]",
	} {
		if r := a.do(t, http.MethodPost, "/v1/notifications/batch", []byte(body)); r.code != http.StatusBadRequest || fmt.Sprint(r.errorFields(t)) != want {
			t.Errorf("%s: %d %s, want the fields %s", body, r.code, r.raw, want)
		}
	}
	if r := a.do(t, http.MethodPost, "/v1/templates", []byte(`{"name":"t","channel":"sms","Body":"x"}`)); r.code != http.StatusBadRequest || fmt.Sprint(r.errorFields(t)) != "[Body]" {
		t.Errorf("a template field in another case: %d %s", r.code, r.raw)
	}
	valid, _ := json.Marshal(sms("x"))
	if r := a.do(t, http.MethodPost, "/v1/notifications", append(valid, []byte(` {}`)...)); r.code != http.StatusBadRequest {
		t.Errorf("a valid object and a second value: %d %s", r.code, r.raw)
	}
}

func TestTheListIsOrderedAndPagedByCursor(t *testing.T) {
	a := newAPI(t)
	// 250 rows of one batch share created_at (one transaction), so the order within a tie
	// is decided by the id.
	b := a.post(t, "/v1/notifications/batch", items(250, func(i int) map[string]any {
		if i%2 == 0 {
			return email("e")
		}
		return sms("s")
	})).json(t)
	for range 10 {
		a.post(t, "/v1/notifications", sms("single"))
	}
	var all []map[string]any
	cursor := ""
	for page := 0; ; page++ {
		r := a.get(t, "/v1/notifications?limit=100&cursor="+cursor)
		if r.code != http.StatusOK {
			t.Fatalf("page %d: %d %s", page, r.code, r.raw)
		}
		m := r.json(t)
		for _, it := range m["items"].([]any) {
			item := it.(map[string]any)
			if _, has := item["content"]; has {
				t.Fatal("a list item carries content")
			}
			all = append(all, item)
		}
		next, _ := m["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(all) != 260 {
		t.Fatalf("listed %d of 260", len(all))
	}
	seen := map[string]bool{}
	for i, it := range all {
		id := it["id"].(string)
		if seen[id] {
			t.Fatalf("%s listed twice", id)
		}
		seen[id] = true
		if i == 0 {
			continue
		}
		prev := all[i-1]
		pc, _ := time.Parse(time.RFC3339Nano, prev["createdAt"].(string))
		c, _ := time.Parse(time.RFC3339Nano, it["createdAt"].(string))
		if c.After(pc) || (c.Equal(pc) && id <= prev["id"].(string)) {
			t.Fatalf("item %d out of order: %v after %v", i, it, prev)
		}
	}
	page := func(query string) int {
		r := a.get(t, "/v1/notifications?limit=100&"+query)
		if r.code != http.StatusOK {
			t.Fatalf("%s: %d %s", query, r.code, r.raw)
		}
		return len(r.json(t)["items"].([]any))
	}
	if n := page("channel=email"); n != 100 {
		t.Errorf("email page: %d", n)
	}
	if n := page("batchId=" + b["batchId"].(string) + "&channel=sms"); n != 100 {
		t.Errorf("batch sms page: %d", n)
	}
	if n := page("status=sent"); n != 0 {
		t.Errorf("sent: %d", n)
	}
	newest := all[0]["createdAt"].(string)
	if n := page("createdFrom=" + newest); n == 0 {
		t.Errorf("createdFrom the newest: %d", n)
	}
	if n := page("createdTo=" + all[len(all)-1]["createdAt"].(string)); n != 0 {
		t.Errorf("createdTo the oldest (exclusive): %d", n)
	}
	if r := a.get(t, "/v1/notifications"); len(r.json(t)["items"].([]any)) != DefaultPage {
		t.Errorf("default page is not %d", DefaultPage)
	}
	for _, bad := range []string{"limit=0", "limit=101", "limit=x", "status=done", "channel=fax", "cursor=zzz", "batchId=1", "createdFrom=today"} {
		if r := a.get(t, "/v1/notifications?"+bad); r.code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, r.code)
		}
	}
}

func TestTheTemplateListIsPagedWithoutBodies(t *testing.T) {
	a := newAPI(t)
	for i := range 150 {
		createTemplate(t, a, fmt.Sprintf("t%03d", i), "sms", "body")
	}
	first := a.get(t, "/v1/templates").json(t)
	list := first["items"].([]any)
	if len(list) != MaxPage || list[0].(map[string]any)["name"] != "t000" {
		t.Fatalf("first page: %d items", len(list))
	}
	if _, has := list[0].(map[string]any)["body"]; has {
		t.Error("a list item carries the body")
	}
	second := a.get(t, "/v1/templates?cursor="+first["nextCursor"].(string)).json(t)
	if n := len(second["items"].([]any)); n != 50 || second["nextCursor"] != nil {
		t.Errorf("second page: %d items, next %v", n, second["nextCursor"])
	}
}

// architecture §8: a status answer carries the last 100 attempts and the total; /retry can
// add attempts without end.
func TestTheAttemptHistoryIsBounded(t *testing.T) {
	a := newAPI(t)
	id := a.post(t, "/v1/notifications", sms("x")).json(t)["id"].(string)
	for _, n := range []int{store.MaxAttemptHistory - 1, store.MaxAttemptHistory, 150} {
		_, _ = a.Pool.Exec(context.Background(), `DELETE FROM delivery_attempts`)
		_, err := a.Pool.Exec(context.Background(), `INSERT INTO delivery_attempts
			(notification_id, attempt_number, started_at, duration_ms, status_code, outcome)
			SELECT $1, k, now(), 5, 503, 'retry' FROM generate_series(1, $2) AS k`, id, n)
		if err != nil {
			t.Fatal(err)
		}
		m := a.get(t, "/v1/notifications/"+id).json(t)
		h := m["attemptHistory"].([]any)
		kept := min(n, store.MaxAttemptHistory)
		if len(h) != kept || m["attemptHistoryTotal"] != float64(n) {
			t.Fatalf("%d attempts: %d shown, total %v", n, len(h), m["attemptHistoryTotal"])
		}
		if f, l := h[0].(map[string]any)["number"], h[kept-1].(map[string]any)["number"]; f != float64(n-kept+1) || l != float64(n) {
			t.Errorf("%d attempts: shown %v to %v", n, f, l)
		}
	}
}

func setStatus(t *testing.T, a apiEnv, id string, s domain.Status) {
	t.Helper()
	if _, err := a.Pool.Exec(context.Background(), `UPDATE notifications SET status = $2, attempts = 6, max_attempts = 6 WHERE id = $1`, id, string(s)); err != nil {
		t.Fatal(err)
	}
}

func TestCancelOnlyFromWaitingStatusesThroughTheAPI(t *testing.T) {
	a := newAPI(t)
	for _, s := range domain.Statuses {
		id := a.post(t, "/v1/notifications", sms("x")).json(t)["id"].(string)
		setStatus(t, a, id, s)
		r := a.do(t, http.MethodPost, "/v1/notifications/"+id+"/cancel", nil)
		if s.IsClaimable() {
			if r.code != http.StatusOK || r.json(t)["status"] != "cancelled" {
				t.Errorf("from %s: %d %s", s, r.code, r.raw)
			}
			if ev := a.events.of(uuid.MustParse(id)); ev[len(ev)-1] != domain.Cancelled {
				t.Errorf("from %s: events %v", s, ev)
			}
			continue
		}
		e, _ := r.json(t)["error"].(map[string]any)
		if r.code != http.StatusConflict || e["status"] != string(s) {
			t.Errorf("from %s: %d %s", s, r.code, r.raw)
		}
	}
	if r := a.do(t, http.MethodPost, "/v1/notifications/"+uuid.NewString()+"/cancel", nil); r.code != http.StatusNotFound {
		t.Errorf("unknown id: %d", r.code)
	}
	if r := a.do(t, http.MethodPost, "/v1/notifications/x/cancel", nil); r.code != http.StatusBadRequest {
		t.Errorf("malformed id: %d", r.code)
	}
}

// architecture §5: /retry moves a failed row to pending with max_attempts = attempts + budget,
// and attempts stays.
func TestRetryRequeuesOnlyAFailedNotification(t *testing.T) {
	a := newAPI(t)
	for _, s := range domain.Statuses {
		id := a.post(t, "/v1/notifications", sms("x")).json(t)["id"].(string)
		setStatus(t, a, id, s)
		r := a.do(t, http.MethodPost, "/v1/notifications/"+id+"/retry", nil)
		if s != domain.Failed {
			if r.code != http.StatusConflict {
				t.Errorf("from %s: %d %s", s, r.code, r.raw)
			}
			continue
		}
		if r.code != http.StatusOK || r.json(t)["status"] != "pending" {
			t.Fatalf("from failed: %d %s", r.code, r.raw)
		}
		n := a.get(t, "/v1/notifications/"+id).json(t)
		if n["attempts"] != 6.0 || n["maxAttempts"] != 12.0 || n["status"] != "pending" {
			t.Errorf("after retry: %v", n)
		}
		if ev := a.events.of(uuid.MustParse(id)); ev[len(ev)-1] != domain.Pending {
			t.Errorf("events %v", ev)
		}
	}
	if r := a.do(t, http.MethodPost, "/v1/notifications/"+uuid.NewString()+"/retry", nil); r.code != http.StatusNotFound {
		t.Errorf("unknown id: %d", r.code)
	}
}

func TestTheBatchSummaryCountsEveryStatus(t *testing.T) {
	a := newAPI(t)
	b := a.post(t, "/v1/notifications/batch", items(len(domain.Statuses), func(int) map[string]any { return sms("x") })).json(t)
	for i, id := range b["ids"].([]any) {
		setStatus(t, a, id.(string), domain.Statuses[i])
	}
	m := a.get(t, "/v1/batches/"+b["batchId"].(string)).json(t)
	counts := m["counts"].(map[string]any)
	for _, s := range domain.Statuses {
		if counts[string(s)] != 1.0 {
			t.Errorf("%s: %v", s, counts[string(s)])
		}
	}
	if m["total"] != float64(len(domain.Statuses)) {
		t.Errorf("total %v", m["total"])
	}
	if r := a.get(t, "/v1/batches/"+uuid.NewString()); r.code != http.StatusNotFound {
		t.Errorf("unknown batch: %d", r.code)
	}
}

func TestAPIKeysAreRequiredWhenConfigured(t *testing.T) {
	a := newAPI(t, "key-one-0123456789", "key-two-0123456789")
	cases := []struct {
		header []string
		want   int
	}{
		{nil, http.StatusUnauthorized},
		{[]string{"Authorization", "Bearer wrong"}, http.StatusUnauthorized},
		{[]string{"X-API-Key", "key-one-012345678"}, http.StatusUnauthorized},
		{[]string{"Authorization", "key-one-0123456789"}, http.StatusUnauthorized},
		{[]string{"Authorization", "Bearer key-one-0123456789"}, http.StatusOK},
		{[]string{"Authorization", "bearer key-one-0123456789"}, http.StatusOK}, // RFC 9110: the scheme in any case
		{[]string{"X-API-Key", "key-two-0123456789"}, http.StatusOK},
	}
	for _, c := range cases {
		if r := a.do(t, http.MethodGet, "/v1/notifications", nil, c.header...); r.code != c.want {
			t.Errorf("%v: %d, want %d", c.header, r.code, c.want)
		}
	}
	for _, open := range []string{"/healthz", "/readyz"} {
		if r := a.get(t, open); r.code != http.StatusOK {
			t.Errorf("%s needs a key: %d", open, r.code)
		}
	}
	if r := newAPI(t).get(t, "/v1/notifications"); r.code != http.StatusOK {
		t.Errorf("no keys configured: %d", r.code)
	}
}

// serve runs the real handler behind a server with the given timeouts and returns its
// address.
func serve(t *testing.T, e *testkit.Env, to timeouts) string {
	t.Helper()
	h := NewHandler(observability.NewLogger(io.Discard, "api", "error"), Options{
		Health: Health{Postgres: e.Pool.Ping, Redis: ok}, Pool: e.Pool, MaxAttempts: 6,
	})
	srv := newServer("127.0.0.1:0", h, to)
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// architecture §8: the whole request, body included, must arrive within its timeout (30 s;
// shortened here). A client that sends the body slowly cannot hold the connection and the
// handler.
func TestSlowRequestBodiesAreCutOff(t *testing.T) {
	e := testkit.Setup(t)
	addr := serve(t, e, timeouts{header: 5 * time.Second, request: 500 * time.Millisecond, answer: 5 * time.Second})
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// The headers arrive at once; the body promises 100,000 bytes and sends 10.
	_, _ = fmt.Fprint(conn, "POST /v1/notifications HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100000\r\n\r\n{\"content\"")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	began := time.Now()
	_, err = io.ReadAll(conn)
	t.Logf("connection ended after %s", time.Since(began).Round(time.Millisecond))
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("connection held for %s by an unfinished body (err %v)", took, err)
	}
}

// architecture §8: the answer must be written within its timeout (30 s; shortened here). A
// client that sends 100 pipelined requests for a 400 KB notification and reads nothing holds
// 40 MB of answers; the server stops writing and closes the connection.
func TestAnswersToAClientThatDoesNotReadAreCutOff(t *testing.T) {
	e := testkit.Setup(t)
	addr := serve(t, e, timeouts{header: 5 * time.Second, request: 30 * time.Second, answer: 500 * time.Millisecond})
	a := apiEnv{Env: e, url: "http://" + addr}
	r := a.post(t, "/v1/notifications", email(strings.Repeat("😀", 100_000)))
	if r.code != http.StatusCreated {
		t.Fatalf("%d %.200s", r.code, r.raw)
	}
	path := "/v1/notifications/" + r.json(t)["id"].(string)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	const requests = 100
	_, _ = fmt.Fprint(conn, strings.Repeat("GET "+path+" HTTP/1.1\r\nHost: x\r\n\r\n", requests))
	time.Sleep(2 * time.Second) // the client reads nothing
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, _ := io.Copy(io.Discard, conn)
	all := int64(requests) * int64(len(r.raw))
	t.Logf("received %d of %d bytes", n, all)
	if n > all/2 {
		t.Errorf("the server kept writing to a client that did not read: %d of %d bytes", n, all)
	}
}

// The server the API binary runs has the timeouts of architecture §8.
func TestTheServerHasTheTimeoutsOfTheDesign(t *testing.T) {
	srv := NewServer(":0", http.NotFoundHandler())
	if srv.ReadHeaderTimeout != 5*time.Second || srv.ReadTimeout != 30*time.Second || srv.WriteTimeout != 30*time.Second {
		t.Errorf("header %s, request %s, answer %s", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout)
	}
}

// A cursor is opaque but comes from the client. One built by hand with a time no row can
// have is a 400; before this test it answered an empty page, as if the list had ended.
func TestACursorOutsideTheTimeRangeIsRefused(t *testing.T) {
	a := newAPI(t)
	for _, micro := range []uint64{1 << 63, 1<<63 - 1, 1 << 62} {
		b := binary.BigEndian.AppendUint64(nil, micro)
		cursor := base64.RawURLEncoding.EncodeToString(append(b, uuid.New().String()[:16]...))
		if r := a.get(t, "/v1/notifications?cursor="+cursor); r.code != http.StatusBadRequest {
			t.Errorf("cursor at %d µs: %d %.200s", micro, r.code, r.raw)
		}
	}
	// A cursor the API gave is accepted, and so is one at the edge of the range it allows.
	at := encodeCursor(store.Cursor{CreatedAt: time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC), ID: uuid.New()})
	if r := a.get(t, "/v1/notifications?cursor="+at); r.code != http.StatusOK {
		t.Errorf("cursor in the year 9999: %d %.200s", r.code, r.raw)
	}
}

// The rendering of many requests at once is bounded, not only each request (session 6's
// review, F1): 40 batches at once, each rendering 8 MB from a 66 KB body. The heap is sampled
// while they run.
func TestConcurrentRequestsRenderInBoundedMemory(t *testing.T) {
	a := newAPI(t)
	createTemplate(t, a, "amp", "email", strings.Repeat("{{.v}}", 16_000))
	body, err := json.Marshal(items(500, func(int) map[string]any { return fromTemplate("amp", map[string]string{"v": "a"}) }))
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			runtime.ReadMemStats(&m)
			if m.HeapInuse > peak.Load() {
				peak.Store(m.HeapInuse)
			}
		}
	}()
	const requests = 40
	codes := make([]int, requests)
	var wg sync.WaitGroup
	for i := range requests {
		wg.Go(func() { codes[i] = a.do(t, http.MethodPost, "/v1/notifications/batch", body).code })
	}
	wg.Wait()
	close(stop)
	<-sampled
	for i, c := range codes {
		if c != http.StatusCreated {
			t.Fatalf("request %d: %d", i, c)
		}
	}
	grew := int64(peak.Load()) - int64(base.HeapInuse)
	t.Logf("%d requests of %d bytes, each rendering 8 MB: the heap grew by %d MiB at its peak", requests, len(body), grew>>20)
	if grew > 400<<20 {
		t.Errorf("the heap grew by %d MiB", grew>>20)
	}
}

// stallingProxy forwards TCP connections to target until stall is called; then it stops
// forwarding in both directions and holds the connections open: a Postgres that stalls, as
// a paused container does. A refused port would fail at once and prove nothing.
func stallingProxy(t *testing.T, target string) (addr string, stall func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var stalled atomic.Bool
	pipe := func(dst, src net.Conn) {
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			for stalled.Load() {
				time.Sleep(10 * time.Millisecond)
			}
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			t.Cleanup(func() { _ = c.Close(); _ = up.Close() })
			go pipe(up, c)
			go pipe(c, up)
		}
	}()
	return ln.Addr().String(), func() { stalled.Store(true) }
}

// architecture §8: a request's database work has a deadline. A statement timeout is
// enforced by Postgres, so a Postgres that stalls held the request until it woke up, and a
// client got no answer at all after 40 s (session 6's review, F5).
func TestARequestEndsAtItsDeadlineWhenPostgresStalls(t *testing.T) {
	e := testkit.Setup(t)
	u, err := url.Parse(e.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy, stall := stallingProxy(t, u.Host)
	u.Host = proxy
	pool, err := store.Connect(context.Background(), u.String(), DatabaseOptions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	const deadline = 500 * time.Millisecond
	srv := httptest.NewServer(NewHandler(observability.NewLogger(io.Discard, "api", "error"), Options{
		Health: Health{Postgres: pool.Ping, Redis: ok}, Pool: pool, MaxAttempts: 6, DatabaseDeadline: deadline,
	}))
	t.Cleanup(srv.Close)
	a := apiEnv{Env: e, url: srv.URL}
	id := a.post(t, "/v1/notifications", sms("x")).json(t)["id"].(string)
	stall()
	client := &http.Client{Timeout: 10 * time.Second}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/notifications/" + id, ""},
		{"POST", "/v1/notifications", `{"recipient":"+905551234567","channel":"sms","content":"y"}`},
		{"GET", "/v1/notifications", ""},
	} {
		req, _ := http.NewRequestWithContext(context.Background(), c.method, srv.URL+c.path, strings.NewReader(c.body))
		began := time.Now()
		resp, err := client.Do(req)
		took := time.Since(began)
		if err != nil {
			t.Errorf("%s %s: no answer after %s: %v", c.method, c.path, took.Round(time.Millisecond), err)
			continue
		}
		_ = resp.Body.Close()
		t.Logf("%s %s: %d after %s", c.method, c.path, resp.StatusCode, took.Round(time.Millisecond))
		if resp.StatusCode != http.StatusInternalServerError || took > deadline+time.Second {
			t.Errorf("%s %s: %d after %s, want 500 within the deadline of %s", c.method, c.path, resp.StatusCode, took, deadline)
		}
	}
}

// An answer is encoded before its status is written: one that cannot be encoded is a 500,
// never a 2xx without a body (session 6's review, F4).
func TestAnAnswerThatCannotBeEncodedIsA500(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]any{"at": time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)})
	if rec.Code != http.StatusInternalServerError || rec.Body.Len() == 0 {
		t.Errorf("%d with %d bytes, want a 500 with the error form", rec.Code, rec.Body.Len())
	}
}

// A repeat of a create answers the stored row as the status endpoint does, with its attempt
// history (session 6's review, F6).
func TestARepeatedCreateAnswersWithItsAttemptHistory(t *testing.T) {
	a := newAPI(t)
	body := sms("x")
	body["idempotencyKey"] = "repeat-1"
	id := a.post(t, "/v1/notifications", body).json(t)["id"].(string)
	if _, err := a.Pool.Exec(context.Background(), `INSERT INTO delivery_attempts
		(notification_id, attempt_number, started_at, duration_ms, status_code, outcome)
		VALUES ($1, 1, now(), 5, 503, 'retry'), ($1, 2, now(), 5, 202, 'sent')`, id); err != nil {
		t.Fatal(err)
	}
	repeat := a.post(t, "/v1/notifications", body)
	got := a.get(t, "/v1/notifications/"+id)
	if repeat.code != http.StatusOK || !bytes.Equal(repeat.raw, got.raw) {
		t.Errorf("repeat %d %s\nstatus %s", repeat.code, repeat.raw, got.raw)
	}
}

// The key of a single notification is a field of its body. A header was ignored, and two
// requests with the same header made two notifications (session 6's review, F9).
func TestAnIdempotencyKeyHeaderOnASingleCreateIsRefused(t *testing.T) {
	a := newAPI(t)
	r := a.post(t, "/v1/notifications", sms("x"), "Idempotency-Key", "k1")
	if r.code != http.StatusBadRequest || fmt.Sprint(r.errorFields(t)) != "[Idempotency-Key]" {
		t.Errorf("%d %s", r.code, r.raw)
	}
	if c := count(t, a, `SELECT count(*) FROM notifications`); c != 0 {
		t.Errorf("%d rows", c)
	}
}
