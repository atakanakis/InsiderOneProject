// Package events carries status events from whoever changed a status to the API instances
// that forward them to WebSocket clients (architecture §9). Events are best-effort: a lost
// event never affects delivery.
//
// Session 5 needs only the publishing side, through the narrow Publisher interface. The
// Redis publisher and the subscriber come in session 7.
package events

import (
	"context"

	"github.com/google/uuid"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
)

// Publisher publishes that a notification's status changed. It is called after the change
// is committed. It returns nothing: a failure to publish is the publisher's to log, never the
// caller's to handle.
type Publisher interface {
	Publish(ctx context.Context, id uuid.UUID, status domain.Status)
}

// Discard drops every event. The worker uses it until session 7 adds the Redis publisher.
type Discard struct{}

// Publish does nothing.
func (Discard) Publish(context.Context, uuid.UUID, domain.Status) {}
