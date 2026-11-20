package main

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

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
		done <- run(ctx, envOf(map[string]string{"DATABASE_URL": e.DatabaseURL, "REDIS_URL": e.RedisURL}), &out)
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
	cmd.Env = []string{"DATABASE_URL=" + e.DatabaseURL, "REDIS_URL=" + e.RedisURL}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			_, _ = out.Write(append(sc.Bytes(), '\n'))
		}
	}()
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
