package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/events"
	"github.com/atakanakis/InsiderOneProject/internal/observability"
	"github.com/atakanakis/InsiderOneProject/internal/store"
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

// Hub hands status events to the WebSocket clients that follow them (architecture §9). It
// keeps, for each notification and each batch, the clients that named it. Handing it an event
// is a map lookup and, for each such client, a send on the client's queue that never waits: a
// client whose queue is full is let go.
type Hub struct {
	o    HubOptions
	open atomic.Int64 // connections admitted and not yet released

	mu      sync.Mutex
	closed  bool
	byID    map[uuid.UUID]map[*follower]struct{}
	byBatch map[uuid.UUID]map[*follower]struct{}
	all     map[*follower]struct{}
}

// follower is one WebSocket client in the hub.
type follower struct {
	ids, batches []uuid.UUID
	queue        chan events.Event
	gone         chan struct{} // closed when the hub lets the client go
	reason       string        // why: written under Hub.mu before gone is closed
}

// NewHub returns a hub with the bounds of o.
func NewHub(o HubOptions) *Hub {
	if o.MaxConnections <= 0 {
		o.MaxConnections = MaxWebSockets
	}
	if o.Queue <= 0 {
		o.Queue = WebSocketQueue
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = WebSocketWriteTimeout
	}
	if o.PingInterval <= 0 {
		o.PingInterval = WebSocketPingInterval
	}
	if o.PongTimeout <= 0 {
		o.PongTimeout = WebSocketPongTimeout
	}
	if o.Metrics == nil {
		o.Metrics = observability.NewAPIMetrics(prometheus.NewRegistry())
	}
	return &Hub{o: o, byID: map[uuid.UUID]map[*follower]struct{}{}, byBatch: map[uuid.UUID]map[*follower]struct{}{},
		all: map[*follower]struct{}{}}
}

// admit takes one of the instance's connections, or reports that none is free.
func (h *Hub) admit() bool {
	if h.open.Add(1) > int64(h.o.MaxConnections) {
		h.open.Add(-1)
		h.o.Metrics.WebSocketsRefused.Inc()
		return false
	}
	h.o.Metrics.WebSockets.Inc()
	return true
}

func (h *Hub) release() {
	h.open.Add(-1)
	h.o.Metrics.WebSockets.Dec()
}

// follow adds a client that follows ids and batches; false when the hub has shut down.
func (h *Hub) follow(ids, batches []uuid.UUID) (*follower, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, false
	}
	f := &follower{ids: ids, batches: batches, queue: make(chan events.Event, h.o.Queue), gone: make(chan struct{})}
	h.all[f] = struct{}{}
	for _, id := range ids {
		add(h.byID, id, f)
	}
	for _, b := range batches {
		add(h.byBatch, b, f)
	}
	return f, true
}

func add(m map[uuid.UUID]map[*follower]struct{}, id uuid.UUID, f *follower) {
	if m[id] == nil {
		m[id] = map[*follower]struct{}{}
	}
	m[id][f] = struct{}{}
}

func remove(m map[uuid.UUID]map[*follower]struct{}, id uuid.UUID, f *follower) {
	delete(m[id], f)
	if len(m[id]) == 0 {
		delete(m, id)
	}
}

// leave removes a client; it does nothing when the hub already let it go.
func (h *Hub) leave(f *follower) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removeLocked(f)
}

func (h *Hub) removeLocked(f *follower) bool {
	if _, ok := h.all[f]; !ok {
		return false
	}
	delete(h.all, f)
	for _, id := range f.ids {
		remove(h.byID, id, f)
	}
	for _, b := range f.batches {
		remove(h.byBatch, b, f)
	}
	return true
}

// letGoLocked removes a client and tells its goroutine why.
func (h *Hub) letGoLocked(f *follower, reason string) {
	if h.removeLocked(f) {
		f.reason = reason
		close(f.gone)
	}
}

// Deliver hands e to every client that follows its notification or its batch, once each. It
// never waits: a client whose queue is full is let go as too slow.
func (h *Hub) Deliver(e events.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	byID := h.byID[e.ID]
	for f := range byID {
		h.offerLocked(f, e)
	}
	if e.BatchID == uuid.Nil {
		return
	}
	for f := range h.byBatch[e.BatchID] {
		if _, both := byID[f]; !both {
			h.offerLocked(f, e)
		}
	}
}

func (h *Hub) offerLocked(f *follower, e events.Event) {
	select {
	case f.queue <- e:
	default:
		h.letGoLocked(f, "slow")
	}
}

// Close lets every client go with 1001 and refuses new ones.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for f := range h.all {
		h.letGoLocked(f, "shutdown")
	}
}

// statusMessage is a notification's status, as the client receives it.
type statusMessage struct {
	Type    string        `json:"type"`
	ID      uuid.UUID     `json:"id"`
	BatchID *uuid.UUID    `json:"batchId,omitempty"`
	Status  domain.Status `json:"status"`
}

func statusOf(id, batch uuid.UUID, s domain.Status) statusMessage {
	m := statusMessage{Type: "status", ID: id, Status: s}
	if batch != uuid.Nil {
		m.BatchID = &batch
	}
	return m
}

// batchMessage is a batch's summary, sent when the connection opens.
type batchMessage struct {
	Type    string                `json:"type"`
	BatchID uuid.UUID             `json:"batchId"`
	Total   int                   `json:"total"`
	Counts  map[domain.Status]int `json:"counts"`
}

// websocket is GET /v1/ws (architecture §9). Everything that can refuse the request does so
// before the upgrade, in the error form: the filters, the origin, the upgrade itself, the
// connection bound, the database.
func (s *server) websocket(w http.ResponseWriter, r *http.Request) {
	ids, batches, bad := followed(r.URL.Query())
	if bad != nil {
		writeError(w, http.StatusBadRequest, *bad)
		return
	}
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, apiError{Code: "forbidden_origin", Message: "a page of another origin may not open this WebSocket"})
		return
	}
	if !isUpgrade(r) {
		writeError(w, http.StatusBadRequest, apiError{Code: "upgrade_required", Message: "a WebSocket upgrade is required"})
		return
	}
	if !s.hub.admit() {
		writeError(w, http.StatusServiceUnavailable, apiError{Code: "unavailable", Message: "this instance holds its most WebSocket connections"})
		return
	}
	defer s.hub.release()
	f, ok := s.hub.follow(ids, batches) // before the current state is read: no change is missed
	if !ok {
		writeError(w, http.StatusServiceUnavailable, apiError{Code: "unavailable", Message: "this instance is shutting down"})
		return
	}
	defer s.hub.leave(f)
	first, err := s.currentState(r, ids, batches)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // the origin was checked above, in the error form
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return // Accept answered
	}
	reason := s.stream(conn, f, first)
	s.hub.o.Metrics.WebSocketsClosed.WithLabelValues(reason).Inc()
}

// stream writes the current state, then every event of the client's queue, and pings; it
// returns why the connection ended. The client's own goroutine reads, so that pongs, pings
// and the close handshake are handled; a data message ends the connection at once.
func (s *server) stream(conn *websocket.Conn, f *follower, first []any) string {
	o := s.hub.o
	defer func() { _ = conn.CloseNow() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	read := make(chan bool, 1) // true: the client sent a data message
	go func() {
		_, _, err := conn.Reader(ctx) // returns at the first data message, or when the connection ends
		read <- err == nil
	}()
	write := func(v any) error {
		b, _ := json.Marshal(v) // UUIDs, a status and counts: Marshal cannot fail
		wctx, wcancel := context.WithTimeout(ctx, o.WriteTimeout)
		defer wcancel()
		return conn.Write(wctx, websocket.MessageText, b)
	}
	// A write that fails because the hub let the client go is reported as the hub's reason.
	failed := func() string {
		select {
		case <-f.gone:
			return f.reason
		default:
			return "write_error"
		}
	}
	for _, m := range first {
		if write(m) != nil {
			return failed()
		}
	}
	ping := time.NewTicker(o.PingInterval)
	defer ping.Stop()
	for {
		select {
		case <-f.gone:
			code, why := websocket.StatusPolicyViolation, webSocketTooSlowReason
			if f.reason == "shutdown" {
				code, why = websocket.StatusGoingAway, "going away"
			}
			_ = conn.Close(code, why)
			return f.reason
		case data := <-read:
			if data {
				// Closed at once, without the close handshake: the handshake would first read
				// the rest of the message, which can be as long as its header claims
				// (architecture §9).
				return "data_message"
			}
			return "client"
		case e := <-f.queue:
			if write(statusOf(e.ID, e.BatchID, e.Status)) != nil {
				return failed()
			}
		case <-ping.C:
			pctx, pcancel := context.WithTimeout(ctx, o.PongTimeout)
			err := conn.Ping(pctx)
			timedOut := pctx.Err() != nil
			pcancel()
			if err == nil {
				continue
			}
			// A client that hung up while the ping was out is the client's doing: its reader
			// has already returned. Only a peer that is still connected and silent is
			// no_pong (session 7's review, F8).
			select {
			case data := <-read:
				if data {
					return "data_message"
				}
				return "client"
			default:
			}
			if timedOut {
				return "no_pong"
			}
			if <-read { // the connection ended under the ping; the reader returns now
				return "data_message"
			}
			return "client"
		}
	}
}

// currentState reads the status of each named notification and the summary of each named
// batch; an id that does not exist is left out.
func (s *server) currentState(r *http.Request, ids, batches []uuid.UUID) ([]any, error) {
	ctx, cancel := s.database(r)
	defer cancel()
	var out []any
	if len(ids) > 0 {
		changes, err := s.store.StatusesOf(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, c := range changes {
			out = append(out, statusOf(c.ID, c.BatchID, c.Status))
		}
	}
	for _, id := range batches {
		b, err := s.store.Batch(ctx, id)
		switch {
		case errors.Is(err, store.ErrBatchNotFound):
			continue
		case err != nil:
			return nil, err
		}
		out = append(out, batchMessage{Type: "batch", BatchID: id, Total: b.Total, Counts: b.Counts})
	}
	return out, nil
}

// followed reads what a client names: up to MaxFollowedIDs notification ids and
// MaxFollowedBatches batch ids, at least one id, and no other parameter.
func followed(q url.Values) (ids, batches []uuid.UUID, bad *apiError) {
	for name := range q {
		if name != notificationIDParam && name != batchIDParam {
			e := fieldError(domain.Echo(name), "is not a parameter of this endpoint")
			return nil, nil, &e
		}
	}
	parse := func(param string, most int) ([]uuid.UUID, *apiError) {
		values := q[param]
		if len(values) > most {
			e := fieldError(param, "names too many ids")
			return nil, &e
		}
		seen := map[uuid.UUID]bool{}
		var out []uuid.UUID
		for _, v := range values {
			id, err := uuid.Parse(v)
			if err != nil {
				e := fieldError(param, "must be a UUID")
				return nil, &e
			}
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		return out, nil
	}
	if ids, bad = parse(notificationIDParam, MaxFollowedIDs); bad != nil {
		return nil, nil, bad
	}
	if batches, bad = parse(batchIDParam, MaxFollowedBatches); bad != nil {
		return nil, nil, bad
	}
	if len(ids)+len(batches) == 0 {
		e := fieldError(notificationIDParam, "name at least one notificationId or batchId")
		return nil, nil, &e
	}
	return ids, batches, nil
}

// sameOrigin reports whether a request has no Origin (not from a browser page) or one whose
// host is the request's.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

// isUpgrade reports whether r is a WebSocket upgrade that Accept will take (RFC 6455, 4.2.1):
// HTTP/1.1 or later, the two headers, version 13 and exactly one key of 16 bytes. Anything
// Accept would refuse is refused here, in the error form, before the database (session 7's
// review, F6).
func isUpgrade(r *http.Request) bool {
	keys := r.Header.Values("Sec-WebSocket-Key")
	if len(keys) != 1 || !r.ProtoAtLeast(1, 1) {
		return false
	}
	key, err := base64.StdEncoding.DecodeString(keys[0])
	return hasToken(r.Header.Values("Connection"), "upgrade") && hasToken(r.Header.Values("Upgrade"), "websocket") &&
		r.Header.Get("Sec-WebSocket-Version") == "13" && err == nil && len(key) == 16
}

func hasToken(values []string, token string) bool {
	for _, v := range values {
		for t := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}
