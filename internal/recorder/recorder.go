// Package recorder is the recording provider server. It stands where the external provider
// stands, records every request with the moment it arrived, and answers as it is told.
//
// Tests of delivery guarantees observe the system here, from the provider's side
// (architecture §11): a request counts when it arrives, not when the client thinks it sent it.
// The same handler runs as cmd/mockprovider in the Compose stack.
//
// What can be injected, per request: the status code, headers and body of the answer, a
// delay before answering, and closing the connection without an answer. Per server: a
// slow connection setup (a delayed TLS handshake), and closing the idle kept-alive
// connections from the server side.
package recorder

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// DefaultMaxBody bounds the body the recorder reads from one request.
const DefaultMaxBody = 1 << 20

// Request is one request as the provider saw it.
type Request struct {
	Seq     int       // arrival order, from 0
	Arrived time.Time // when the headers had arrived and the handler began, before the body
	ConnID  int64     // the connection that carried it; equal ids mean a reused connection
	Proto   string    // "HTTP/1.1" or "HTTP/2.0"
	Method  string
	Path    string
	Header  http.Header
	Body    []byte // nil when the server drops bodies
	Channel string // the "channel" field of a JSON body, or ""
}

// Response is what the recorder does with one request. The zero value is the provider's
// normal answer: 202 with {"messageId", "status": "accepted", "timestamp"}.
type Response struct {
	Status int // 0 means the normal 202 answer
	Header http.Header
	Body   []byte
	Delay  time.Duration // wait this long before answering (the stamp is not delayed)
	Close  bool          // close the connection without any answer, after Delay
}

// Options configure a recorder.
type Options struct {
	// Respond decides the answer to each request. Nil answers every request with 202.
	Respond func(Request) Response
	// MaxBody bounds the body read from one request; a larger body is answered with 413.
	// 0 means DefaultMaxBody.
	MaxBody int64
	// MaxKept, when positive, keeps only the most recent requests. Received still counts all.
	MaxKept int
	// Lean keeps neither the body nor the headers of a request, only its stamp, connection,
	// method, path and channel. It bounds what a long-running mock provider holds. Respond
	// still sees the whole request.
	Lean bool

	// TLS serves HTTPS, offering HTTP/2 and HTTP/1.1. Only used by Start.
	TLS bool
	// HandshakeDelay delays every new connection before its TLS handshake, so that the
	// client's connection becomes ready late. It needs TLS: a plain TCP connection is set up
	// by the kernel before the server sees it, so it cannot be slowed from here. Start only.
	HandshakeDelay time.Duration
}

// Recorder is the HTTP handler. It is safe for concurrent use.
type Recorder struct {
	opts     Options
	mu       sync.Mutex
	next     int
	requests []Request
}

// New returns a recorder handler.
func New(opts Options) *Recorder {
	if opts.MaxBody <= 0 {
		opts.MaxBody = DefaultMaxBody
	}
	return &Recorder{opts: opts}
}

// ServeHTTP stamps the request, reads its body, records it, then answers as Respond says.
func (rec *Recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The stamp comes first: before the lock (a reader of the records may hold it), before
	// the body is read and before any injected delay. Seq is the order in which requests
	// took the lock, so it can differ from the order of the stamps by the time two requests
	// wait for the lock; analysis sorts by Arrived.
	arrived := time.Now()
	rec.mu.Lock()
	seq := rec.next
	rec.next++
	rec.mu.Unlock()

	body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, rec.opts.MaxBody))
	req := Request{
		Seq: seq, Arrived: arrived, ConnID: connID(r.Context()),
		Proto: r.Proto, Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(),
		Body: body, Channel: channelOf(body),
	}
	kept := req
	if rec.opts.Lean { // Lean bounds what is kept; Respond still sees the whole request
		kept.Body, kept.Header = nil, nil
	}
	rec.record(kept)

	if readErr != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(readErr, &tooLarge) {
			http.Error(w, `{"error":"request body too large"}`, http.StatusRequestEntityTooLarge)
		}
		return
	}

	var resp Response
	if rec.opts.Respond != nil {
		resp = rec.opts.Respond(req)
	}
	if resp.Delay > 0 {
		t := time.NewTimer(resp.Delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-r.Context().Done():
			return
		}
	}
	if resp.Close {
		panic(http.ErrAbortHandler) // the server closes the connection; nothing is written
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if resp.Status == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"messageId": uuid.NewString(),
			"status":    "accepted",
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		})
		return
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

func (rec *Recorder) record(req Request) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.requests = append(rec.requests, req)
	if max := rec.opts.MaxKept; max > 0 && len(rec.requests) > 2*max {
		// Keep the most recent by Seq: a request with a slow body is recorded late.
		bySeq(rec.requests)
		rec.requests = slices.Clone(rec.requests[len(rec.requests)-max:])
	}
}

// Requests returns the kept requests in Seq order. It copies under the lock and sorts after,
// so that a request arriving meanwhile waits for a copy, not for a sort.
func (rec *Recorder) Requests() []Request {
	rec.mu.Lock()
	out := slices.Clone(rec.requests)
	rec.mu.Unlock()
	bySeq(out)
	if max := rec.opts.MaxKept; max > 0 && len(out) > max {
		out = out[len(out)-max:]
	}
	return out
}

func bySeq(rs []Request) {
	slices.SortFunc(rs, func(a, b Request) int { return a.Seq - b.Seq })
}

// Received returns how many requests have arrived, including any no longer kept.
func (rec *Recorder) Received() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.next
}

// Reset forgets every request.
func (rec *Recorder) Reset() {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.requests, rec.next = nil, 0
}

// Peak returns the largest number of kept requests of one channel ("" for all) that
// arrived within any window of the given length.
func (rec *Recorder) Peak(window time.Duration, channel string) int {
	var times []time.Time
	for _, r := range rec.Requests() {
		if channel == "" || r.Channel == channel {
			times = append(times, r.Arrived)
		}
	}
	return MaxInWindow(times, window)
}

// MaxInWindow returns the largest number of times that fall within one half-open window
// [t, t+window), over every possible start t. The input need not be sorted.
func MaxInWindow(times []time.Time, window time.Duration) int {
	ts := slices.Clone(times)
	slices.SortFunc(ts, func(a, b time.Time) int { return a.Compare(b) })
	best, j := 0, 0
	for i := range ts {
		for j < len(ts) && ts[j].Before(ts[i].Add(window)) {
			j++
		}
		best = max(best, j-i)
	}
	return best
}

func channelOf(body []byte) string {
	var m struct {
		Channel string `json:"channel"`
	}
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	return m.Channel
}

type connKey struct{}

var connCounter atomic.Int64

// ConnContext gives each connection an id that Request.ConnID reports. Set it as the
// http.Server's ConnContext.
func ConnContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, connKey{}, connCounter.Add(1))
}

func connID(ctx context.Context) int64 {
	id, _ := ctx.Value(connKey{}).(int64)
	return id
}
