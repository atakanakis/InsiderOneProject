package observability

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// propagator reads and writes W3C traceparent headers and the stored traceparent of a row.
var propagator = propagation.TraceContext{}

// Tracing returns the process's tracer provider and its shutdown (architecture §10). With an
// endpoint (OTEL_EXPORTER_OTLP_ENDPOINT, a base URL), spans go to <endpoint>/v1/traces by
// OTLP over HTTP, through the SDK's bounded batch queue; the exporter's errors are logged at
// most once per 10 s. Without one, the provider records nothing: no goroutine, no
// connection, no log.
func Tracing(ctx context.Context, endpoint, service string, log *slog.Logger) (trace.TracerProvider, func(context.Context) error, error) {
	if endpoint == "" {
		return noop.NewTracerProvider(), func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(strings.TrimRight(endpoint, "/")+"/v1/traces"))
	if err != nil {
		return nil, nil, fmt.Errorf("tracing: %w", err)
	}
	every := NewEvery(10 * time.Second)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		every.Log(context.Background(), log, slog.LevelWarn, "tracing: spans not exported", "error", err)
	}))
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp),
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", service))))
	return tp, tp.Shutdown, nil
}

// Traceparent returns the W3C traceparent of the span in ctx, or "" when there is none. The
// API stores it on the row, so that the worker's span joins the request's trace.
func Traceparent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagator.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// WithTraceparent returns ctx with the remote span context of a stored traceparent; a
// malformed or empty one leaves ctx as it is.
func WithTraceparent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return propagator.Extract(ctx, propagation.MapCarrier{"traceparent": traceparent})
}

// FromHeaders returns ctx with the remote span context of a request's traceparent header.
func FromHeaders(ctx context.Context, h propagation.TextMapCarrier) context.Context {
	return propagator.Extract(ctx, h)
}
