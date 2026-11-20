package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/recorder"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

func TestMain(m *testing.M) { testkit.Main(m) }

// syncBuffer is a log destination that the test can read while the worker writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func envOf(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

// Architecture §13: a worker whose lease is shorter than the send timeout plus 30 s refuses
// to start, before it connects to anything. The database URL points at a closed port, so a
// worker that skipped the check would fail later, with a different exit status.
func TestWorkerRefusesToStartWithALeaseShorterThanSendTimeoutPlus30s(t *testing.T) {
	var out syncBuffer
	code := run(context.Background(), envOf(map[string]string{
		"LEASE": "44s", "SEND_TIMEOUT": "15s",
		"DATABASE_URL": "postgres://x:x@127.0.0.1:1/x?connect_timeout=1",
		"REDIS_URL":    "redis://127.0.0.1:1/0",
	}), &out)
	if code != 2 {
		t.Fatalf("exit status %d, want 2\n%s", code, out.String())
	}
	log := out.String()
	if !strings.Contains(log, "refusing to start") || !strings.Contains(log, "LEASE (44s)") {
		t.Fatalf("log does not name the broken relation:\n%s", log)
	}
}

func TestWorkerRefusesToStartWithAMalformedSetting(t *testing.T) {
	var out syncBuffer
	if code := run(context.Background(), envOf(map[string]string{"LEASE": "a minute"}), &out); code != 2 {
		t.Fatalf("exit status %d, want 2\n%s", code, out.String())
	}
}

func waitFor(t *testing.T, out *syncBuffer, text string) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if strings.Contains(out.String(), text) {
			return
		}
	}
	t.Fatalf("no %q in the log:\n%s", text, out.String())
}

func TestWorkerStartsAndStopsCleanly(t *testing.T) {
	e := testkit.Setup(t)
	var out syncBuffer
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, envOf(map[string]string{"DATABASE_URL": e.DatabaseURL, "REDIS_URL": e.RedisURL, "METRICS_ADDR": "127.0.0.1:0"}), &out)
	}()
	waitFor(t, &out, "worker started")
	stop()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit status %d\n%s", code, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not stop within 5 s of the stop signal")
	}
	waitFor(t, &out, "worker stopped")
}

// The real binary, stopped the way Docker stops it: SIGTERM, then the exit status.
func TestWorkerBinaryExitsZeroOnSIGTERM(t *testing.T) {
	e := testkit.Setup(t)
	bin := filepath.Join(t.TempDir(), "worker")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin)
	cmd.Env = []string{"DATABASE_URL=" + e.DatabaseURL, "REDIS_URL=" + e.RedisURL, "METRICS_ADDR=127.0.0.1:0"}
	// A writer, not StdoutPipe: Wait closes a pipe as soon as the process exits, and the last
	// lines could be lost (CI run 44750111098); with a writer, Wait returns after the copy.
	var out syncBuffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, &out, "worker started")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("exit: %v\n%s", err, out.String())
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the worker did not exit within 5 s of SIGTERM")
	}
	waitFor(t, &out, "worker stopped")
}

// insertPending adds n pending rows of each channel, due now, and returns their ids. The API
// that creates rows comes in session 6.
func insertPending(t *testing.T, e *testkit.Env, n int) []uuid.UUID {
	t.Helper()
	var ids []uuid.UUID
	for _, ch := range domain.Channels {
		for range n {
			id := uuid.New()
			ids = append(ids, id)
			if _, err := e.Pool.Exec(context.Background(), `INSERT INTO notifications
    (id, recipient, channel, content, priority, status, max_attempts, ready_at)
VALUES ($1, '+905551234567', $2, 'hello', 1, 'pending', 6, now())`, id, string(ch)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return ids
}

func statusOf(t *testing.T, e *testkit.Env, id uuid.UUID) (domain.Status, int) {
	t.Helper()
	var s string
	var attempts int
	if err := e.Pool.QueryRow(context.Background(), `SELECT status, attempts FROM notifications WHERE id = $1`, id).
		Scan(&s, &attempts); err != nil {
		t.Fatal(err)
	}
	return domain.Status(s), attempts
}

func allSent(t *testing.T, e *testkit.Env, ids []uuid.UUID, timeout time.Duration) bool {
	t.Helper()
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		var n int
		if err := e.Pool.QueryRow(context.Background(), `SELECT count(*) FROM notifications WHERE id = ANY($1) AND status = 'sent'`, ids).
			Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == len(ids) {
			return true
		}
	}
	return false
}

// The worker runs a dispatcher per channel and a reaper: rows of every channel are delivered
// to the provider, and the worker stops cleanly.
func TestTheWorkerDeliversEveryChannel(t *testing.T) {
	e := testkit.Setup(t)
	srv := testkit.Recorder(t, recorder.Options{})
	ids := insertPending(t, e, 20)
	var out syncBuffer
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, envOf(map[string]string{"DATABASE_URL": e.DatabaseURL, "REDIS_URL": e.RedisURL, "METRICS_ADDR": "127.0.0.1:0",
			"PROVIDER_URL": srv.URL + "/send"}), &out)
	}()
	if !allSent(t, e, ids, 20*time.Second) {
		stop()
		t.Fatalf("not every row was sent; the provider received %d\n%s", srv.Received(), out.String())
	}
	for _, ch := range domain.Channels {
		if n := srv.Peak(time.Hour, string(ch)); n != 20 {
			t.Errorf("the provider received %d %s requests, want 20", n, ch)
		}
	}
	stop()
	if code := <-done; code != 0 {
		t.Fatalf("exit status %d\n%s", code, out.String())
	}
}

// I4: a worker killed in the middle of a send leaves its row processing; when the lease ends,
// a reaper returns the row and it is delivered. The provider receives it twice with the same
// Idempotency-Key: at-least-once case 1 of architecture §6.
//
// The lease is 60 s (the default). Instead of waiting for it, the test ends it in the
// database after the kill, which is what time would do.
func TestAKilledWorkersRowComesBackThroughTheReaper(t *testing.T) {
	e := testkit.Setup(t)
	var hold atomic.Bool
	hold.Store(true)
	srv := testkit.Recorder(t, recorder.Options{Respond: func(recorder.Request) recorder.Response {
		if hold.Load() {
			return recorder.Response{Delay: time.Minute} // the first worker dies while waiting for this
		}
		return recorder.Response{}
	}})
	id := insertPending(t, e, 1)[0] // one row of each channel; the sms one is followed
	bin := filepath.Join(t.TempDir(), "worker")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin)
	cmd.Env = []string{"DATABASE_URL=" + e.DatabaseURL, "REDIS_URL=" + e.RedisURL, "METRICS_ADDR=127.0.0.1:0", "PROVIDER_URL=" + srv.URL + "/send"}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for srv.Received() < len(domain.Channels) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if srv.Received() < len(domain.Channels) {
		_ = cmd.Process.Kill()
		t.Fatalf("the first worker sent %d requests", srv.Received())
	}
	if err := cmd.Process.Kill(); err != nil { // SIGKILL: no shutdown, no result write
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if s, attempts := statusOf(t, e, id); s != domain.Processing || attempts != 1 {
		t.Fatalf("after the kill the row is %s with %d attempts, want processing with 1", s, attempts)
	}
	if _, err := e.Pool.Exec(context.Background(), `UPDATE notifications SET locked_until = now() - interval '1 second'
    WHERE status = 'processing'`); err != nil {
		t.Fatal(err)
	}
	hold.Store(false)

	var out syncBuffer
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, envOf(map[string]string{"DATABASE_URL": e.DatabaseURL, "REDIS_URL": e.RedisURL, "METRICS_ADDR": "127.0.0.1:0",
			"PROVIDER_URL": srv.URL + "/send", "REAPER_TICK": "100ms"}), &out)
	}()
	var all []uuid.UUID
	rows, err := e.Pool.Query(context.Background(), `SELECT id FROM notifications`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var x uuid.UUID
		if err := rows.Scan(&x); err != nil {
			t.Fatal(err)
		}
		all = append(all, x)
	}
	rows.Close()
	if !allSent(t, e, all, 20*time.Second) {
		t.Fatalf("the killed worker's rows were not delivered\n%s", out.String())
	}
	if s, attempts := statusOf(t, e, id); s != domain.Sent || attempts != 2 {
		t.Errorf("the row is %s after %d attempts, want sent after 2", s, attempts)
	}
	var history []int
	hrows, err := e.Pool.Query(context.Background(), `SELECT attempt_number FROM delivery_attempts WHERE notification_id = $1`, id)
	if err != nil {
		t.Fatal(err)
	}
	for hrows.Next() {
		var n int
		if err := hrows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		history = append(history, n)
	}
	hrows.Close()
	if len(history) != 1 || history[0] != 2 {
		t.Errorf("attempt history %v, want only attempt 2: the killed attempt is a missing number", history)
	}
	same := 0
	for _, r := range srv.Requests() {
		if r.Header.Get("Idempotency-Key") == id.String() {
			same++
		}
	}
	if same != 2 {
		t.Errorf("the provider received the row %d times, want 2 with the same Idempotency-Key", same)
	}
	stop()
	if code := <-done; code != 0 {
		t.Errorf("exit status %d", code)
	}
}

// The provider URL can hold a secret (a webhook.site path, credentials in the URL); the
// worker logs only its scheme and host (session 5's review, a nit).
func TestTheWorkerLogsOnlyTheProvidersHost(t *testing.T) {
	e := testkit.Setup(t)
	var out syncBuffer
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, envOf(map[string]string{"DATABASE_URL": e.DatabaseURL, "REDIS_URL": e.RedisURL, "METRICS_ADDR": "127.0.0.1:0",
			"PROVIDER_URL": "http://user:hunter2@127.0.0.1:1/b8a1f00d-secret-path"}), &out)
	}()
	waitFor(t, &out, "worker started")
	stop()
	<-done
	log := out.String()
	if !strings.Contains(log, "http://127.0.0.1:1") {
		t.Errorf("the log does not name the provider's host:\n%s", log)
	}
	for _, secret := range []string{"hunter2", "b8a1f00d", "user"} {
		if strings.Contains(log, secret) {
			t.Errorf("the log contains %q from the provider URL:\n%s", secret, log)
		}
	}
}

// Architecture §10: the worker serves its metrics on METRICS_ADDR, every series of a fixed
// set present from the start.
func TestTheWorkerServesItsMetrics(t *testing.T) {
	e := testkit.Setup(t)
	var out syncBuffer
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, envOf(map[string]string{"DATABASE_URL": e.DatabaseURL, "REDIS_URL": e.RedisURL, "METRICS_ADDR": "127.0.0.1:0"}), &out)
	}()
	t.Cleanup(func() { stop(); <-done })
	waitFor(t, &out, "worker started")
	var started struct{ Metrics string }
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.Contains(l, "worker started") {
			if err := json.Unmarshal([]byte(l), &started); err != nil {
				t.Fatal(err)
			}
		}
	}
	resp, err := http.Get("http://" + started.Metrics + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		`notifications_breaker_state{channel="sms"} 0`,
		`notifications_sends_in_flight{channel="push"} 0`,
		`notifications_attempts_total{channel="email",outcome="no_permit_in_time"} 0`,
		`notifications_finished_total{channel="sms",status="not_stored"} 0`,
		`notifications_worker_errors_total{operation="result_write"} 0`,
		`notifications_reaped_total{status="failed"} 0`,
		`notifications_events_dropped_total{reason="buffer_full"} 0`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics has no %s", want)
		}
	}
}
