package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atakanakis/InsiderOneProject/internal/observability"
	"github.com/atakanakis/InsiderOneProject/internal/redisclient"
	"github.com/atakanakis/InsiderOneProject/internal/store"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

func TestMain(m *testing.M) { testkit.Main(m) }

func ok(context.Context) error { return nil }

func failing(context.Context) error { return errors.New("connection refused") }

func hanging(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

type answer struct {
	code int
	body map[string]string
	hdr  http.Header
}

func get(t *testing.T, h http.Handler, path string, header ...string) answer {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: body %q is not JSON: %v", path, rec.Body.String(), err)
	}
	return answer{rec.Code, body, rec.Header()}
}

func handler(log *bytes.Buffer, postgres, redis Check, timeout time.Duration) http.Handler {
	return NewHandler(observability.NewLogger(log, "api", "debug"), Options{Health: Health{Postgres: postgres, Redis: redis, Timeout: timeout}})
}

func TestHealthzAnswersWhileDependenciesAreDown(t *testing.T) {
	a := get(t, handler(&bytes.Buffer{}, failing, failing, time.Second), "/healthz")
	if a.code != http.StatusOK || a.body["status"] != "ok" {
		t.Fatalf("got %d %v", a.code, a.body)
	}
}

func TestReadyzIsReadyWhenBothDependenciesAnswer(t *testing.T) {
	a := get(t, handler(&bytes.Buffer{}, ok, ok, time.Second), "/readyz")
	if a.code != http.StatusOK || a.body["status"] != "ready" || a.body["postgres"] != "ok" || a.body["redis"] != "ok" {
		t.Fatalf("got %d %v", a.code, a.body)
	}
}

func TestReadyzFailsWhenPostgresIsUnreachable(t *testing.T) {
	a := get(t, handler(&bytes.Buffer{}, failing, ok, time.Second), "/readyz")
	if a.code != http.StatusServiceUnavailable || a.body["postgres"] != "unreachable" || a.body["redis"] != "ok" {
		t.Fatalf("got %d %v", a.code, a.body)
	}
}

// The API needs only Postgres to accept requests; status events through Redis are
// best-effort (architecture §8, §9). Redis being down is reported, and the API stays ready.
func TestReadyzStaysReadyWhenOnlyRedisIsUnreachable(t *testing.T) {
	a := get(t, handler(&bytes.Buffer{}, ok, failing, time.Second), "/readyz")
	if a.code != http.StatusOK || a.body["status"] != "ready" || a.body["redis"] != "unreachable" || a.body["postgres"] != "ok" {
		t.Fatalf("got %d %v", a.code, a.body)
	}
}

// A dependency that does not answer must not hold the probe: readyz answers within its
// timeout (architecture §8: each ping within 2 s; shortened here).
func TestReadyzAnswersWithinItsTimeoutWhenADependencyHangs(t *testing.T) {
	const timeout = 200 * time.Millisecond
	h := handler(&bytes.Buffer{}, hanging, hanging, timeout)
	began := time.Now()
	done := make(chan answer, 1)
	go func() { done <- get(t, h, "/readyz") }()
	select {
	case a := <-done:
		if a.code != http.StatusServiceUnavailable {
			t.Fatalf("got %d %v", a.code, a.body)
		}
		if took := time.Since(began); took > timeout+300*time.Millisecond {
			t.Errorf("answered after %s with a timeout of %s", took, timeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readyz waited for a hanging dependency")
	}
}

func TestReadyzDoesNotShowDependencyErrors(t *testing.T) {
	var log bytes.Buffer
	a := get(t, handler(&log, failing, ok, time.Second), "/readyz", "X-Correlation-ID", "probe-1")
	for k, v := range a.body {
		if strings.Contains(v, "refused") {
			t.Errorf("answer %s=%q shows the internal error", k, v)
		}
	}
	// The error goes to the log instead, on a line that carries the correlation id.
	found := false
	for _, l := range strings.Split(log.String(), "\n") {
		if strings.Contains(l, "connection refused") && strings.Contains(l, `"correlation_id":"probe-1"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("no log line with the error and the correlation id:\n%s", log.String())
	}
}

func TestCorrelationIDIsAcceptedOrReplaced(t *testing.T) {
	var log bytes.Buffer
	h := handler(&log, ok, ok, time.Second)

	valid := strings.Repeat("a", 64)
	if got := get(t, h, "/healthz", "X-Correlation-ID", valid).hdr.Get("X-Correlation-ID"); got != valid {
		t.Errorf("valid id at the bound replaced by %q", got)
	}
	tooLong := strings.Repeat("b", 65)
	got := get(t, h, "/healthz", "X-Correlation-ID", tooLong).hdr.Get("X-Correlation-ID")
	if got == tooLong || !observability.ValidCorrelationID(got) {
		t.Errorf("id over the bound answered with %q", got)
	}
	if got := get(t, h, "/healthz").hdr.Get("X-Correlation-ID"); !observability.ValidCorrelationID(got) {
		t.Errorf("no id sent, answered with %q", got)
	}
	if !strings.Contains(log.String(), valid) {
		t.Errorf("the accepted id is not in the log")
	}
	if strings.Contains(log.String(), tooLong) {
		t.Errorf("a refused id was written to the log")
	}
}

// architecture §8: the request line and headers are bounded at 64 KiB together. The test
// writes raw requests, so it controls every byte, and goes just under and just over.
func TestRequestHeadersOverTheBoundAreRefused(t *testing.T) {
	srv := NewServer("127.0.0.1:0", handler(&bytes.Buffer{}, ok, ok, time.Second))
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	status := func(total int) string {
		head := "GET /healthz HTTP/1.1\r\nHost: x\r\nConnection: close\r\nX-Pad: "
		end := "\r\n\r\n"
		req := head + strings.Repeat("p", total-len(head)-len(end)) + end
		if len(req) != total {
			t.Fatalf("built %d bytes, want %d", len(req), total)
		}
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := fmt.Fprint(conn, req); err != nil {
			return "write: " + err.Error()
		}
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			return "read: " + err.Error()
		}
		return strings.TrimSpace(line)
	}
	if got := status(MaxHeaderBytes - 64); !strings.Contains(got, " 200 ") {
		t.Errorf("%d bytes, under the bound: %q", MaxHeaderBytes-64, got)
	}
	if got := status(MaxHeaderBytes + 64); !strings.Contains(got, " 431 ") {
		t.Errorf("%d bytes, over the bound: %q, want 431", MaxHeaderBytes+64, got)
	}
}

// architecture §8: request headers must arrive within the header timeout (5 s; shortened
// here). A client that sends them slowly is disconnected.
func TestSlowRequestHeadersAreCutOff(t *testing.T) {
	srv := newServer("127.0.0.1:0", handler(&bytes.Buffer{}, ok, ok, time.Second), timeouts{header: 300 * time.Millisecond, request: 30 * time.Second, answer: 30 * time.Second})
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprint(conn, "GET /healthz HTTP/1.1\r\nHost: x\r\n") // headers never finish
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	began := time.Now()
	_, err = bufio.NewReader(conn).ReadByte()
	if err == nil {
		// The server may answer 408 before closing; either way the connection must end.
		_, err = bufio.NewReader(conn).ReadString(0)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("connection held for %s by unfinished headers (err %v)", took, err)
	}
}

// silentServer accepts TCP connections and never answers: a paused or hung dependency. A
// refused port fails at once and would prove nothing about the timeout.
func silentServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	return ln.Addr().String()
}

// The real clients, as cmd/api builds them, against dependencies that hang: readyz still
// answers within its timeout. A fake check that honours its context cannot show this.
func TestReadyzAnswersWithinItsTimeoutWhenRealClientsHang(t *testing.T) {
	addr := silentServer(t)
	pool, err := pgxpool.New(context.Background(), "postgres://x:x@"+addr+"/x?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	rdb, err := redisclient.New("redis://" + addr + "/0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	const timeout = 300 * time.Millisecond
	h := handler(&bytes.Buffer{}, pool.Ping, RedisCheck(rdb), timeout)
	began := time.Now()
	a := get(t, h, "/readyz")
	took := time.Since(began)
	if a.code != http.StatusServiceUnavailable || a.body["postgres"] != "unreachable" || a.body["redis"] != "unreachable" {
		t.Fatalf("got %d %v", a.code, a.body)
	}
	if took > timeout+500*time.Millisecond {
		t.Fatalf("answered after %s with a timeout of %s", took, timeout)
	}
}

func TestReadyzWithRealPostgresAndRedis(t *testing.T) {
	e := testkit.Setup(t)
	h := handler(&bytes.Buffer{}, e.Pool.Ping, RedisCheck(e.Redis), 2*time.Second)
	if a := get(t, h, "/readyz"); a.code != http.StatusOK || a.body["postgres"] != "ok" || a.body["redis"] != "ok" {
		t.Fatalf("got %d %v", a.code, a.body)
	}
}

// architecture §8: the database time of an API request is bounded by a statement timeout
// of 5 s, set on every session of the API's pool.
func TestAPIDatabaseSessionsHaveAFiveSecondStatementTimeout(t *testing.T) {
	e := testkit.Setup(t)
	pool, err := store.Connect(context.Background(), e.DatabaseURL, DatabaseOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var got string
	if err := pool.QueryRow(context.Background(), `SELECT current_setting('statement_timeout')`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "5s" {
		t.Fatalf("statement_timeout %s, want 5s", got)
	}
}
