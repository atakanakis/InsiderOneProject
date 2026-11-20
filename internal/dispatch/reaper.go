package dispatch

import (
	"context"
	"log/slog"
	"time"

	"github.com/atakanakis/InsiderOneProject/internal/events"
	"github.com/atakanakis/InsiderOneProject/internal/store"
)

// ReapQueue is the part of the queue the reaper uses. *store.Queue is it.
type ReapQueue interface {
	Reap(ctx context.Context, limit int) ([]store.Change, error)
}

// ReapBatch is how many rows one Reap call returns at most.
const ReapBatch = 100

// Reaper returns rows whose lease has expired (architecture §5, §6).
type Reaper struct {
	Queue  ReapQueue
	Tick   time.Duration
	Log    *slog.Logger
	Events events.Publisher
}

// Run reaps every Tick until ctx ends.
func (r Reaper) Run(ctx context.Context) { <-ctx.Done() }
