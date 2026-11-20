// Package observability holds structured logging with the correlation id (architecture §10).
// Metrics and tracing are added in session 7.
package observability

import (
	"context"
	"io"
	"log/slog"

	"github.com/google/uuid"
)

type correlationKey struct{}

// WithCorrelationID returns a context that carries the correlation id. Every line logged
// with that context (InfoContext, ErrorContext, ...) carries it as "correlation_id".
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey{}, id)
}

// CorrelationID returns the id carried by the context, or "".
func CorrelationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey{}).(string)
	return id
}

// MaxCorrelationIDLength is the bound of architecture §8 on a correlation id sent by a client.
const MaxCorrelationIDLength = 64

// ValidCorrelationID reports whether a client-supplied id is within the bound of
// architecture §8: 1 to 64 characters, ASCII letters, digits, '-', '_' and '.'.
func ValidCorrelationID(s string) bool {
	if len(s) == 0 || len(s) > MaxCorrelationIDLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.'
		if !ok {
			return false
		}
	}
	return true
}

// NewCorrelationID returns a new random id.
func NewCorrelationID() string { return uuid.NewString() }

// NewLogger returns a JSON logger that writes to w. Every line carries "service", and the
// correlation id of its context at the top level.
func NewLogger(w io.Writer, service, level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	base := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})
	return slog.New(handler{base: base}).With("service", service)
}

// handler adds the correlation id before any attribute or group of the logger, so that the
// id stays at the top level of the line. It records With and WithGroup calls and replays
// them on every line, after the id.
type handler struct {
	base slog.Handler
	ops  []op
}

type op struct {
	attrs []slog.Attr
	group string
}

func (h handler) Enabled(ctx context.Context, l slog.Level) bool { return h.base.Enabled(ctx, l) }

func (h handler) Handle(ctx context.Context, r slog.Record) error {
	inner := h.base
	if id := CorrelationID(ctx); id != "" {
		inner = inner.WithAttrs([]slog.Attr{slog.String("correlation_id", id)})
	}
	for _, o := range h.ops {
		if o.group != "" {
			inner = inner.WithGroup(o.group)
		} else {
			inner = inner.WithAttrs(o.attrs)
		}
	}
	return inner.Handle(ctx, r)
}

func (h handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return handler{base: h.base, ops: append(h.ops[:len(h.ops):len(h.ops)], op{attrs: attrs})}
}

func (h handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return handler{base: h.base, ops: append(h.ops[:len(h.ops):len(h.ops)], op{group: name})}
}
