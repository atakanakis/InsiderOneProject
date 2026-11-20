package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

func TestMain(m *testing.M) { testkit.Main(m) }

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

// The API binary's wiring: it serves /readyz from real Postgres and Redis and stops with
// status 0 when told to.
func TestAPIServesReadinessAndStopsCleanly(t *testing.T) {
	e := testkit.Setup(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	env := map[string]string{"DATABASE_URL": e.DatabaseURL, "REDIS_URL": e.RedisURL, "API_ADDR": addr}
	var out syncBuffer
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx, func(k string) string { return env[k] }, &out) }()

	var code int
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/readyz", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			continue
		}
		_ = resp.Body.Close()
		code = resp.StatusCode
		break
	}
	if code != http.StatusOK {
		t.Fatalf("/readyz answered %d\n%s", code, out.String())
	}
	stop()
	select {
	case c := <-done:
		if c != 0 {
			t.Fatalf("exit status %d\n%s", c, out.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the API did not stop")
	}
}
