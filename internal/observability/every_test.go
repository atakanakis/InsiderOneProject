package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// §10: a failure that repeats is logged at most once per period. The lines held back are
// counted, and the next line written says how many.
func TestEveryWritesAtMostOneLinePerPeriod(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, "worker", "info")
	clock := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	e := NewEvery(10 * time.Second)
	e.now = func() time.Time { return clock }
	// 100 failures over 25 s: one every 250 ms.
	for range 100 {
		e.Log(context.Background(), log, slog.LevelWarn, "result write failed", "error", "read-only")
		clock = clock.Add(250 * time.Millisecond)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines for 100 failures over 25 s, want 3 (at 0 s, 10 s and 20 s):\n%s", len(lines), buf.String())
	}
	var second map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	// The line at 10 s holds back the 39 failures between 0 s and 10 s.
	if second["suppressed"] != float64(39) || second["msg"] != "result write failed" {
		t.Errorf("second line %v, want the message and suppressed 39", second)
	}
}
