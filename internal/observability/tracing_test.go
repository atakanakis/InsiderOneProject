package observability

import (
	"bytes"
	"context"
	"runtime"
	"testing"
	"time"
)

// §10: without an endpoint, tracing is off: the provider records nothing, starts no goroutine,
// and its shutdown has nothing to do and logs nothing.
func TestTracingIsOffWithoutAnEndpoint(t *testing.T) {
	var log bytes.Buffer
	before := runtime.NumGoroutine()
	tp, shutdown, err := Tracing(context.Background(), "", "test", NewLogger(&log, "test", "debug"))
	if err != nil {
		t.Fatal(err)
	}
	_, span := tp.Tracer("test").Start(context.Background(), "a span")
	if span.IsRecording() || span.SpanContext().IsValid() {
		t.Errorf("a span of the provider without an endpoint records or has a context: %+v", span.SpanContext())
	}
	span.End()
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("%d goroutines before, %d after: tracing that is off started some", before, after)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Errorf("shutdown: %v", err)
	}
	if log.Len() != 0 {
		t.Errorf("tracing that is off logged: %s", log.String())
	}
	// A request's traceparent still passes through: the API stores it, the worker joins it.
	const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	if got := Traceparent(WithTraceparent(context.Background(), parent)); got != parent {
		t.Errorf("a traceparent through a context: %q, want %q", got, parent)
	}
	if got := Traceparent(WithTraceparent(context.Background(), "not a traceparent")); got != "" {
		t.Errorf("a malformed traceparent through a context: %q, want none", got)
	}
}
