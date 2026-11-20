package api

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/atakanakis/InsiderOneProject/internal/events"
	"github.com/atakanakis/InsiderOneProject/internal/observability"
)

// Bounds of a WebSocket connection (architecture §9).
const (
	MaxWebSockets          = 1000
	WebSocketQueue         = 256
	WebSocketWriteTimeout  = 5 * time.Second
	WebSocketPingInterval  = 30 * time.Second
	WebSocketPongTimeout   = 10 * time.Second
	MaxFollowedIDs         = 100
	MaxFollowedBatches     = 10
	webSocketPath          = "/v1/ws"
	notificationIDParam    = "notificationId"
	batchIDParam           = "batchId"
	webSocketTooSlowReason = "too slow"
)

// HubOptions are the bounds of the hub's connections; a zero value takes the bound of
// architecture §9.
type HubOptions struct {
	MaxConnections int
	Queue          int // messages waiting per client
	WriteTimeout   time.Duration
	PingInterval   time.Duration
	PongTimeout    time.Duration
	Metrics        *observability.APIMetrics // nil: a registry of their own
}

// Hub hands status events to the WebSocket clients that follow them.
type Hub struct {
	o HubOptions
}

// NewHub returns a hub with the bounds of o.
func NewHub(o HubOptions) *Hub {
	if o.Metrics == nil {
		o.Metrics = observability.NewAPIMetrics(prometheus.NewRegistry())
	}
	return &Hub{o: o}
}

// Deliver hands e to every client that follows its notification or its batch. It never
// waits.
func (h *Hub) Deliver(e events.Event) {}

// Close closes every connection with 1001 and refuses new ones.
func (h *Hub) Close() {}

// websocket is GET /v1/ws.
func (s *server) websocket(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, apiError{Code: "not_implemented", Message: "not implemented"})
}

// metrics is GET /v1/metrics.
func (s *server) metrics(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, apiError{Code: "not_implemented", Message: "not implemented"})
}
