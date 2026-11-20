// Package events carries status events from whoever changed a status to the API instances
// that forward them to WebSocket clients (architecture §9). Events are best-effort: a lost
// event never affects delivery.
//
// Publishing never waits and never fails: Redis.Publish puts the event in a bounded buffer and
// returns, and one goroutine sends the buffer to Redis. Subscribe keeps one subscription open,
// and opens it again after any error.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
)

// Bounds and timings of architecture §9.
const (
	// Channel is the Redis channel of every status event.
	Channel = "notifications:status"
	// Buffer is how many events wait to be published, per process; more are dropped.
	Buffer = 4096
	// PublishBatch is how many events one pipeline sends at most.
	PublishBatch = 256
	// PublishTimeout bounds one pipeline to Redis.
	PublishTimeout = time.Second
	// MaxEventBytes bounds an event read from Redis; a larger one is dropped unread.
	MaxEventBytes = 1024
)

// Event says that a notification's status changed.
type Event struct {
	ID      uuid.UUID
	BatchID uuid.UUID // uuid.Nil for a notification created alone
	Status  domain.Status
}

// Publisher publishes a status event. It is called after the change is committed. It returns
// nothing and must not wait: a failure to publish is the publisher's to count and log, never
// the caller's to handle.
type Publisher interface {
	Publish(ctx context.Context, e Event)
}

// Discard drops every event.
type Discard struct{}

// Publish does nothing.
func (Discard) Publish(context.Context, Event) {}

// wire is an event as it travels through Redis: {"id", "batchId", "status"}.
type wire struct {
	ID      uuid.UUID  `json:"id"`
	BatchID *uuid.UUID `json:"batchId,omitempty"`
	Status  string     `json:"status"`
}

// Encode returns the event as it is published.
func (e Event) Encode() []byte {
	w := wire{ID: e.ID, Status: string(e.Status)}
	if e.BatchID != uuid.Nil {
		w.BatchID = &e.BatchID
	}
	b, _ := json.Marshal(w) // two UUIDs and a string: Marshal cannot fail
	return b
}

// ErrMalformed is the error of an event that is too large or not an event.
var ErrMalformed = errors.New("events: malformed event")

// Decode reads an event published by Encode.
func Decode(p []byte) (Event, error) {
	var w wire
	if err := json.Unmarshal(p, &w); err != nil {
		return Event{}, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	e := Event{ID: w.ID, Status: domain.Status(w.Status)}
	if w.BatchID != nil {
		e.BatchID = *w.BatchID
	}
	return e, nil
}
