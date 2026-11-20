// Package provider is the HTTP client that delivers one notification to the external provider,
// with the send gate of architecture §7: the rate-limit permit is taken when the connection is
// ready and before the request is written, once per send.
//
// One send makes one connection (new or reused), calls the gate once, and writes at most one
// request (invariant I8). What happened is returned as a Result, which carries only what was
// observed; what it means for the notification is decided by the dispatcher's Classify.
package provider

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
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
	url         string
	sendTimeout time.Duration
	gateBudget  time.Duration
	http        *http.Client
}

// New returns a client with its own transport.
func New(o Options) (*Client, error) {
	if o.URL == "" {
		return nil, errors.New("provider: no URL")
	}
	if o.SendTimeout <= 0 || o.GateBudget <= 0 || o.DialTimeout <= 0 || o.TLSHandshakeTimeout <= 0 ||
		o.IdleConns <= 0 || o.IdleConnTimeout <= 0 {
		return nil, errors.New("provider: every duration and the idle connections must be positive")
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	tr := &http.Transport{
		Proxy:                  nil, // straight to the provider
		DialContext:            (&net.Dialer{Timeout: o.DialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:        o.TLSConfig,
		TLSHandshakeTimeout:    o.TLSHandshakeTimeout,
		Protocols:              protocols,   // HTTP/1.1 only: one connection carries one request
		MaxIdleConnsPerHost:    o.IdleConns, // one host: the provider
		IdleConnTimeout:        o.IdleConnTimeout,
		MaxResponseHeaderBytes: MaxResponseHeaderBytes,
	}
	return &Client{
		url:         o.URL,
		sendTimeout: o.SendTimeout,
		gateBudget:  o.GateBudget,
		http: &http.Client{
			Transport: tr,
			// A redirect is a response like any other; following it would send a second
			// request on another connection, without a permit.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// CloseIdleConnections closes the client's kept-alive connections.
func (c *Client) CloseIdleConnections() { c.http.CloseIdleConnections() }

type body struct {
	To      string `json:"to"`
	Channel string `json:"channel"`
	Content string `json:"content"`
}

// Send delivers m, calling gate once when the connection is ready. It never returns an error:
// everything it observed is in the Result.
func (c *Client) Send(ctx context.Context, m Message, gate Gate) Result {
	began := time.Now()
	r := c.send(ctx, m, gate)
	r.Duration = time.Since(began)
	return r
}

func (c *Client) send(ctx context.Context, m Message, gate Gate) Result {
	ctx, cancel := context.WithTimeout(ctx, c.sendTimeout) // the one deadline of the send
	defer cancel()

	payload, err := json.Marshal(body{To: m.To, Channel: string(m.Channel), Content: m.Content})
	if err != nil {
		return Result{Err: fmt.Errorf("provider: encode the request: %w", err)}
	}

	var r Result
	// onConn is the send gate. GotConn calls it on this goroutine, inside Do, after the
	// connection is ready and before the request is handed to the transport's write loop
	// (architecture §7, property 1), so r needs no lock.
	onConn := func(info httptrace.GotConnInfo) {
		r.ConnReused = info.Reused
		gateCtx, gateCancel := context.WithTimeout(ctx, c.gateBudget)
		defer gateCancel()
		if err := gate(gateCtx); err != nil {
			r.Refusal = err
			_ = info.Conn.Close() // the write that follows fails: nothing is sent (property 2)
		}
	}
	trace := &httptrace.ClientTrace{GotConn: onConn}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return Result{Err: fmt.Errorf("provider: build the request: %w", err)}
	}
	req.GetBody = nil // the transport cannot replay the request (property 3)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", m.ID.String())
	if m.CorrelationID != "" {
		req.Header.Set("X-Correlation-ID", m.CorrelationID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		r.Err = err
		if r.Refusal != nil {
			r.Err = fmt.Errorf("refused at the send gate: %w", r.Refusal)
		}
		return r
	}
	defer func() { _ = resp.Body.Close() }()
	r.Responded, r.StatusCode = true, resp.StatusCode
	r.RetryAfter = retryAfter(resp.Header.Get("Retry-After"), time.Now())
	r.Body, r.Err = io.ReadAll(io.LimitReader(resp.Body, MaxResponseBody))
	if r.StatusCode >= 200 && r.StatusCode < 300 {
		var accepted struct {
			MessageID string `json:"messageId"`
		}
		if json.Unmarshal(r.Body, &accepted) == nil {
			r.MessageID = accepted.MessageID
		}
	}
	return r
}

// retryAfter reads a Retry-After value in either form of RFC 9110: seconds, or an HTTP date
// relative to now.
func retryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	secs, err := strconv.ParseInt(v, 10, 64)
	switch {
	case err == nil && secs < 0:
		return 0 // delta-seconds has no sign
	case errors.Is(err, strconv.ErrRange) && v[0] != '-', err == nil && secs > int64(MaxRetryAfter/time.Second):
		return MaxRetryAfter
	case err == nil:
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return min(t.Sub(now), MaxRetryAfter)
	}
	return 0
}
