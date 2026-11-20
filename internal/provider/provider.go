// Package provider is the HTTP client that delivers one notification to the external provider,
// with the send gate of architecture §7: the rate-limit permit is taken when the connection is
// ready and before the request is written, once per send.
//
// One send makes one connection (new or reused), calls the gate once, and writes at most one
// request (invariant I8). What happened is returned as a Result, which carries only what was
// observed; what it means for the notification is decided by the dispatcher's Classify.
package provider

import (
	"context"
	"crypto/tls"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
)

// Bounds of what the client reads from the provider (architecture §8).
const (
	// MaxResponseHeaderBytes bounds the header block of one response. Go's default is 10 MiB.
	MaxResponseHeaderBytes = 64 << 10
	// MaxResponseBody bounds the bytes read from one response body. A longer body is cut, and
	// its connection is closed instead of being drained.
	MaxResponseBody = 4 << 10
	// MaxRetryAfter is the largest Retry-After the client reports; a larger value is reported
	// as this, so the duration cannot overflow.
	MaxRetryAfter = 24 * time.Hour
)

// Message is one notification as the provider receives it.
type Message struct {
	ID            uuid.UUID // the notification id, sent as the Idempotency-Key header
	CorrelationID string    // sent as X-Correlation-ID when not empty
	To            string
	Channel       domain.Channel
	Content       string
}

// Gate is called once per send, when the connection is ready and before the request is
// written. A nil error is a permit; any error refuses the send, and nothing is written. Its
// context ends at the gate budget or at the send's deadline, whichever comes first.
type Gate func(ctx context.Context) error

// Result is what one send observed, and nothing else.
type Result struct {
	// Responded is true when a response's status line and headers arrived. A response
	// always decides the verdict (I6), whatever else is set.
	Responded  bool
	StatusCode int
	RetryAfter time.Duration // the Retry-After header as a duration from arrival; 0 when absent or malformed
	MessageID  string        // the messageId of a 2xx JSON body, when there is one
	Body       []byte        // at most MaxResponseBody bytes of the body

	// Refusal is the gate's error when the gate refused the send. Nil when the gate gave a
	// permit or never ran (the connection failed first).
	Refusal error
	// ConnReused is true when the send ran on a kept-alive connection.
	ConnReused bool

	// Err is why no response arrived (connect, timeout, reset, refusal), or, when Responded
	// is true, an error while reading the body.
	Err error
	// Duration is the time Send took, from its start to its return.
	Duration time.Duration
}

// Options configure a Client. The relations between the durations are checked by
// config.Worker.Check (architecture §13).
type Options struct {
	URL                 string
	SendTimeout         time.Duration // one deadline: connect, gate, write and response
	GateBudget          time.Duration // longest wait at the gate
	DialTimeout         time.Duration
	TLSHandshakeTimeout time.Duration
	IdleConns           int // idle connections kept for the provider's host; equal to the slots
	IdleConnTimeout     time.Duration
	TLSConfig           *tls.Config // nil uses the system's roots
}

// Client sends notifications to one provider URL. It is safe for concurrent use.
type Client struct {
	url string
}

// New returns a client with its own transport.
func New(o Options) (*Client, error) {
	return &Client{url: o.URL}, nil
}

// CloseIdleConnections closes the client's kept-alive connections.
func (c *Client) CloseIdleConnections() {}

// Send delivers m, calling gate once when the connection is ready. It never returns an error:
// everything it observed is in the Result.
func (c *Client) Send(ctx context.Context, m Message, gate Gate) Result {
	return Result{Err: errors.New("provider: not built yet")}
}
