package api

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atakanakis/InsiderOneProject/internal/events"
)

// Bounds of architecture §8 that the API enforces itself.
const (
	MaxBodyBytes      = 1 << 20
	MaxBatchBodyBytes = 8 << 20
	MaxPage           = 100
	DefaultPage       = 50
	MaxErrorBodyBytes = 64 << 10
	maxErrorText      = 300 // bytes of a field name or a message in an error
	maxEchoedName     = 64  // bytes of a name taken from the request
)

// Options configure the API's handler.
type Options struct {
	Health      Health
	Pool        *pgxpool.Pool    // nil: only the health endpoints and the specification
	APIKeys     []string         // empty: no authentication
	MaxAttempts int              // a new row's max_attempts and the budget of /retry; 0 means 6
	Events      events.Publisher // nil: events.Discard
}

// routePatterns lists every route of the API, as registered on the mux.
func routePatterns() []string {
	return []string{
		"GET /healthz", "GET /readyz", "GET /openapi.yaml", "GET /docs",
		"POST /v1/notifications", "POST /v1/notifications/batch", "GET /v1/notifications/{id}",
		"GET /v1/notifications", "POST /v1/notifications/{id}/cancel", "POST /v1/notifications/{id}/retry",
		"GET /v1/batches/{id}", "POST /v1/templates", "GET /v1/templates", "GET /v1/templates/{name}",
	}
}

// register adds the notification, batch and template routes.
func register(mux *http.ServeMux, log *slog.Logger, o Options) {}
