package observability

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Every writes at most one log line per period: a dependency that fails on every try is
// counted by a metric and logged now and then, not once per try (architecture §10). The
// lines it holds back are counted, and the next line it writes says how many.
type Every struct {
	period time.Duration
	now    func() time.Time

	mu      sync.Mutex
	last    time.Time // when the last line was written; zero before the first
	skipped int       // lines held back since then
}

// NewEvery returns an Every with the given period.
func NewEvery(period time.Duration) *Every {
	return &Every{period: period, now: time.Now}
}

// Log writes the line, unless a line was written less than one period ago.
func (e *Every) Log(ctx context.Context, log *slog.Logger, level slog.Level, msg string, args ...any) {
	e.mu.Lock()
	now := e.now()
	if !e.last.IsZero() && now.Sub(e.last) < e.period {
		e.skipped++
		e.mu.Unlock()
		return
	}
	skipped := e.skipped
	e.last, e.skipped = now, 0
	e.mu.Unlock()
	if skipped > 0 {
		args = append(args[:len(args):len(args)], "suppressed", skipped)
	}
	log.Log(ctx, level, msg, args...)
}
