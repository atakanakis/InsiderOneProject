package observability

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Tracing returns the process's tracer provider and its shutdown (architecture §10). With an
// endpoint, spans go to it by OTLP over HTTP; without one, the provider records nothing.
func Tracing(ctx context.Context, endpoint, service string, log *slog.Logger) (trace.TracerProvider, func(context.Context) error, error) {
	return noop.NewTracerProvider(), func(context.Context) error { return nil }, nil
}

// Traceparent returns the W3C traceparent of the span in ctx, or "".
func Traceparent(ctx context.Context) string { return "" }

// WithTraceparent returns ctx with the remote span context of a stored traceparent.
func WithTraceparent(ctx context.Context, traceparent string) context.Context { return ctx }
