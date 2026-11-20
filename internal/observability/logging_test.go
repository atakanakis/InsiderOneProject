package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func lines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

func TestLogLinesCarryTheCorrelationIDFromTheContext(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, "test", "info")
	ctx := WithCorrelationID(context.Background(), "req-42")

	log.InfoContext(ctx, "with id")
	log.Info("without id")

	got := lines(t, &buf)
	if len(got) != 2 {
		t.Fatalf("want 2 lines, got %d", len(got))
	}
	if got[0]["correlation_id"] != "req-42" || got[0]["service"] != "test" || got[0]["msg"] != "with id" {
		t.Errorf("first line: %v", got[0])
	}
	if _, ok := got[1]["correlation_id"]; ok {
		t.Errorf("a line without a correlation id in its context has one: %v", got[1])
	}
}

// A logger narrowed with With or WithGroup must still put the correlation id at the top
// level of the line, where a search for it looks. Without care, slog puts attributes added
// at Handle time inside the last group.
func TestCorrelationIDStaysTopLevelUnderGroups(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, "test", "info").With("channel", "sms").WithGroup("send").With("attempt", 2)
	log.InfoContext(WithCorrelationID(context.Background(), "req-7"), "sent", "status", 202)

	got := lines(t, &buf)[0]
	if got["correlation_id"] != "req-7" {
		t.Fatalf("correlation_id not at the top level: %v", got)
	}
	if got["channel"] != "sms" {
		t.Errorf("attribute before the group lost: %v", got)
	}
	group, _ := got["send"].(map[string]any)
	if group["attempt"] != float64(2) || group["status"] != float64(202) {
		t.Errorf("group attributes lost: %v", got)
	}
}

func TestLogLevelDropsLowerLines(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, "test", "warn")
	log.Info("dropped")
	log.Warn("kept")
	got := lines(t, &buf)
	if len(got) != 1 || got[0]["msg"] != "kept" {
		t.Fatalf("lines: %v", got)
	}
}

func TestCorrelationIDBound(t *testing.T) {
	valid := []string{"a", strings.Repeat("x", 64), "req-1_2.3", "0b6c7a8e-4c3e-4bd5-9f1e-2a8d1f0c9e11"}
	invalid := []string{"", strings.Repeat("x", 65), "a b", "a\nb", "ä", "<script>", "a/b", "a\x00"}
	for _, s := range valid {
		if !ValidCorrelationID(s) {
			t.Errorf("ValidCorrelationID(%q) = false", s)
		}
	}
	for _, s := range invalid {
		if ValidCorrelationID(s) {
			t.Errorf("ValidCorrelationID(%q) = true", s)
		}
	}
	if id := NewCorrelationID(); !ValidCorrelationID(id) {
		t.Errorf("NewCorrelationID() = %q is not valid", id)
	}
	if NewCorrelationID() == NewCorrelationID() {
		t.Error("two new correlation ids are equal")
	}
}
