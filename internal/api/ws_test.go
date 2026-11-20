package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/events"
	"github.com/atakanakis/InsiderOneProject/internal/observability"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

// wsAPI is the real handler with a hub whose events the test delivers itself.
type wsAPI struct {
	apiEnv
	hub     *Hub
	metrics *observability.APIMetrics
}

func newWSAPI(t *testing.T, ho HubOptions, keys ...string) wsAPI {
	t.Helper()
	m := observability.NewAPIMetrics(prometheus.NewRegistry())
	ho.Metrics = m
	hub := NewHub(ho)
	a := newAPIWith(t, Options{APIKeys: keys, Metrics: m, Hub: hub})
	return wsAPI{apiEnv: a, hub: hub, metrics: m}
}

func wsURL(base, query string) string {
	return "ws" + strings.TrimPrefix(base, "http") + webSocketPath + "?" + query
}

// dial opens a WebSocket; the connection is closed when the test ends.
func dial(t *testing.T, base, query string, header ...string) *websocket.Conn {
	t.Helper()
	c, resp, err := dialFor(base, query, header...)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (status %d)", query, err, code)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func dialFor(base, query string, header ...string) (*websocket.Conn, *http.Response, error) {
	h := http.Header{}
	for i := 0; i+1 < len(header); i += 2 {
		h.Set(header[i], header[i+1])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, wsURL(base, query), &websocket.DialOptions{HTTPHeader: h})
}

// next reads one message as JSON.
func next(t *testing.T, c *websocket.Conn, timeout time.Duration) (map[string]any, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	if len(b) >= 1024 {
		t.Errorf("a message of %d bytes, the bound is under 1 KiB", len(b))
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not JSON: %q", b)
	}
	return m, nil
}

func mustNext(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	m, err := next(t, c, 5*time.Second)
	if err != nil {
		t.Fatalf("no message: %v", err)
	}
	return m
}

// closeCode reads until the connection ends and returns the close code the server sent.
func closeCode(t *testing.T, c *websocket.Conn, timeout time.Duration) websocket.StatusCode {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		_, _, err := c.Read(ctx)
		cancel()
		if err != nil {
			return websocket.CloseStatus(err)
		}
	}
	t.Fatalf("the connection was still open after %v", timeout)
	return 0
}

func closedBy(m *observability.APIMetrics, reason string) float64 {
	return testutil.ToFloat64(m.WebSocketsClosed.WithLabelValues(reason))
}

// createOne creates a notification through the API and returns its id.
func createOne(t *testing.T, a apiEnv, body string) string {
	t.Helper()
	r := a.do(t, http.MethodPost, "/v1/notifications", []byte(body))
	if r.code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.code, r.raw)
	}
	return r.json(t)["id"].(string)
}

// §9: a client is sent the current state of what it follows, then every change of it, and
// nothing else.
func TestAWebSocketClientGetsTheCurrentStateThenEveryChange(t *testing.T) {
	a := newWSAPI(t, HubOptions{})
	alone := createOne(t, a.apiEnv, smsJSON)
	r := a.do(t, http.MethodPost, "/v1/notifications/batch", []byte(`{"notifications": [`+smsJSON+`, `+smsJSON+`, `+smsJSON+`]}`))
	if r.code != http.StatusCreated {
		t.Fatalf("batch: %d %s", r.code, r.raw)
	}
	out := r.json(t)
	batch := out["batchId"].(string)
	members := out["ids"].([]any)
	unknown := uuid.NewString()

	c := dial(t, a.url, "notificationId="+alone+"&notificationId="+unknown+"&batchId="+batch)
	got := map[string]map[string]any{}
	for range 2 { // the notification's status and the batch's summary; nothing for the unknown id
		m := mustNext(t, c)
		got[m["type"].(string)] = m
	}
	if s := got["status"]; s["id"] != alone || s["status"] != "pending" || s["batchId"] != nil {
		t.Errorf("current status: %v, want %s pending without a batch", s, alone)
	}
	b := got["batch"]
	if b["batchId"] != batch || b["total"] != float64(3) || b["counts"].(map[string]any)["pending"] != float64(3) {
		t.Errorf("batch summary: %v, want %s with 3 pending", b, batch)
	}

	// Changes: one of another notification (not followed), then one of each followed thing.
	other := uuid.New()
	member := uuid.MustParse(members[1].(string))
	batchID := uuid.MustParse(batch)
	a.hub.Deliver(events.Event{ID: other, Status: domain.Sent})
	a.hub.Deliver(events.Event{ID: other, BatchID: uuid.New(), Status: domain.Sent})
	a.hub.Deliver(events.Event{ID: uuid.MustParse(alone), Status: domain.Processing})
	a.hub.Deliver(events.Event{ID: member, BatchID: batchID, Status: domain.Sent})
	first, second := mustNext(t, c), mustNext(t, c)
	if first["type"] != "status" || first["id"] != alone || first["status"] != "processing" {
		t.Errorf("first change: %v, want %s processing", first, alone)
	}
	if second["id"] != member.String() || second["batchId"] != batch || second["status"] != "sent" {
		t.Errorf("second change: %v, want %s of batch %s sent", second, member, batch)
	}
	if m, err := next(t, c, 300*time.Millisecond); err == nil {
		t.Errorf("a message the client did not ask for: %v", m)
	}
}

// §9: what a client may name is bounded, and checked before the upgrade.
func TestWebSocketFiltersAreBounded(t *testing.T) {
	a := newWSAPI(t, HubOptions{})
	ids := func(param string, n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = param + "=" + uuid.NewString()
		}
		return strings.Join(parts, "&")
	}
	for _, tc := range []struct {
		query string
		ok    bool
	}{
		{"", false},
		{ids(notificationIDParam, MaxFollowedIDs), true},
		{ids(notificationIDParam, MaxFollowedIDs+1), false},
		{ids(batchIDParam, MaxFollowedBatches), true},
		{ids(batchIDParam, MaxFollowedBatches+1), false},
		{ids(notificationIDParam, MaxFollowedIDs) + "&" + ids(batchIDParam, MaxFollowedBatches), true},
		{notificationIDParam + "=not-a-uuid", false},
		{batchIDParam + "=", false},
		{ids(notificationIDParam, 1) + "&status=sent", false},
	} {
		c, resp, err := dialFor(a.url, tc.query)
		switch {
		case tc.ok && err != nil:
			t.Errorf("%.60s...: %v, want an upgrade", tc.query, err)
		case !tc.ok && (err == nil || resp == nil || resp.StatusCode != http.StatusBadRequest):
			t.Errorf("%.60s...: %v, want a 400", tc.query, resp)
		}
		if c != nil {
			_ = c.CloseNow()
		}
	}
	// A plain request that is not an upgrade is a 400 in the error form.
	r := a.get(t, webSocketPath+"?"+ids(notificationIDParam, 1))
	if r.code != http.StatusBadRequest || r.json(t)["error"] == nil {
		t.Errorf("a request that is not an upgrade: %d %s, want a 400 in the error form", r.code, r.raw)
	}
}

// §8, §9: with API keys, a WebSocket needs a key like every /v1 path.
func TestWebSocketNeedsAnAPIKeyWhenConfigured(t *testing.T) {
	const key = "ws-key-0123456789"
	a := newWSAPI(t, HubOptions{}, key)
	query := notificationIDParam + "=" + uuid.NewString()
	if _, resp, err := dialFor(a.url, query); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a key: %v, want a 401", resp)
	}
	dial(t, a.url, query, "Authorization", "Bearer "+key)
	dial(t, a.url, query, "X-API-Key", key)
}

// §9: an instance holds at most MaxConnections WebSockets; the next upgrade is a 503 before
// the upgrade, and a closed connection makes room.
func TestWebSocketConnectionsAreBoundedPerInstance(t *testing.T) {
	const most = 3
	a := newWSAPI(t, HubOptions{MaxConnections: most})
	query := notificationIDParam + "=" + uuid.NewString()
	open := make([]*websocket.Conn, most)
	for i := range open {
		open[i] = dial(t, a.url, query)
	}
	_, resp, err := dialFor(a.url, query)
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("connection %d: %v, want a 503", most+1, resp)
	}
	if got := testutil.ToFloat64(a.metrics.WebSocketsRefused); got != 1 {
		t.Errorf("refused counter %v, want 1", got)
	}
	_ = open[0].Close(websocket.StatusNormalClosure, "")
	ok := waitUntil(5*time.Second, func() bool { return testutil.ToFloat64(a.metrics.WebSockets) == most-1 })
	if !ok {
		t.Fatalf("open connections %v after one closed, want %d", testutil.ToFloat64(a.metrics.WebSockets), most-1)
	}
	dial(t, a.url, query)
}

func waitUntil(timeout time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(timeout); ; time.Sleep(10 * time.Millisecond) {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// rawWS performs the upgrade by hand and returns the connection: a client that does only what
// the test makes it do.
func rawWS(t *testing.T, base, query string) (net.Conn, *bufio.Reader) {
	t.Helper()
	host := strings.TrimPrefix(base, "http://")
	c, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	fmt.Fprintf(c, "GET %s?%s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", webSocketPath, query, host, base64.StdEncoding.EncodeToString(key))
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v, %v", resp, err)
	}
	return c, br
}

// frame is a client frame: masked unless told otherwise, with the length it claims.
func frame(opcode byte, payload []byte, claimed uint64, masked bool, rsv byte) []byte {
	b := []byte{0x80 | rsv<<4 | opcode}
	maskBit := byte(0)
	if masked {
		maskBit = 0x80
	}
	switch {
	case claimed < 126:
		b = append(b, maskBit|byte(claimed))
	case claimed <= 0xffff:
		b = append(b, maskBit|126)
		b = binary.BigEndian.AppendUint16(b, uint16(claimed))
	default:
		b = append(b, maskBit|127)
		b = binary.BigEndian.AppendUint64(b, claimed)
	}
	if !masked {
		return append(b, payload...)
	}
	mask := []byte{1, 2, 3, 4}
	b = append(b, mask...)
	for i, p := range payload {
		b = append(b, p^mask[i%4])
	}
	return b
}

// serverClose reads server frames until a close frame, and returns its code; 0 when the
// connection ended without one.
func serverClose(t *testing.T, c net.Conn, br *bufio.Reader, timeout time.Duration) int {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	for {
		h := make([]byte, 2)
		if _, err := io.ReadFull(br, h); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "reset") {
				return 0
			}
			t.Fatalf("waiting for the server to close: %v", err)
		}
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			var l [2]byte
			_, _ = io.ReadFull(br, l[:])
			n = uint64(binary.BigEndian.Uint16(l[:]))
		case 127:
			var l [8]byte
			_, _ = io.ReadFull(br, l[:])
			n = binary.BigEndian.Uint64(l[:])
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(br, payload); err != nil {
			return 0
		}
		if h[0]&0x0f == 0x8 {
			if len(payload) < 2 {
				return 1005
			}
			return int(binary.BigEndian.Uint16(payload))
		}
	}
}

// §9: the client sends nothing but control frames. A data message closes the connection at
// once, whatever its size and before its payload is read, also one that claims 1 GiB and
// sends 100 bytes; a malformed frame closes it, with 1002 or at once; a ping at the 125-byte
// bound of a control frame is answered.
func TestAWebSocketClientMustNotSendDataMessages(t *testing.T) {
	a := newWSAPI(t, HubOptions{})
	query := notificationIDParam + "=" + uuid.NewString()

	c := dial(t, a.url, query)
	if err := c.Write(context.Background(), websocket.MessageText, []byte(`{"subscribe": "everything"}`)); err != nil {
		t.Fatal(err)
	}
	if code := closeCode(t, c, 2*time.Second); code != -1 {
		t.Errorf("a small data message: closed with %d, want the connection cut at once", code)
	}

	for _, tc := range []struct {
		name  string
		bytes []byte
		want  int
	}{
		// A frame that claims 1 GiB and sends 100 bytes of it.
		{"a 1 GiB data message", frame(0x2, make([]byte, 100), 1<<30, true, 0), 0},
		// RFC 6455 5.1: the server must close; the library cuts the connection without a code.
		{"an unmasked frame", frame(0x1, []byte("hi"), 2, false, 0), 0},
		{"reserved bits", frame(0x1, []byte("hi"), 2, true, 0x4), 1002},
		{"a ping over 125 bytes", frame(0x9, make([]byte, 126), 126, true, 0), 1002},
	} {
		conn, br := rawWS(t, a.url, query)
		if _, err := conn.Write(tc.bytes); err != nil {
			t.Fatal(err)
		}
		if got := serverClose(t, conn, br, 2*time.Second); got != tc.want {
			t.Errorf("%s: closed with %d, want %d", tc.name, got, tc.want)
		}
	}

	conn, br := rawWS(t, a.url, query)
	payload := []byte(strings.Repeat("p", 125))
	if _, err := conn.Write(frame(0x9, payload, 125, true, 0)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	h := make([]byte, 2)
	if _, err := io.ReadFull(br, h); err != nil || h[0]&0x0f != 0xA || int(h[1]&0x7f) != 125 {
		t.Errorf("a 125-byte ping: answered %x, %v; want a pong of 125 bytes", h, err)
	}
	if !waitUntil(2*time.Second, func() bool { return closedBy(a.metrics, "data_message") == 2 }) {
		t.Errorf("closed{data_message} = %v, want 2", closedBy(a.metrics, "data_message"))
	}
}

// §9: a peer that does not answer pings is closed: one ping interval and one pong timeout.
// A client that reads answers them, and stays.
func TestAWebSocketClientThatDoesNotAnswerPingsIsClosed(t *testing.T) {
	a := newWSAPI(t, HubOptions{PingInterval: 100 * time.Millisecond, PongTimeout: 100 * time.Millisecond})
	id := uuid.New()
	query := notificationIDParam + "=" + id.String()
	alive := dial(t, a.url, query)
	answered := make(chan map[string]any, 1)
	go func() {
		m, _ := next(t, alive, 3*time.Second) // reading answers the pings
		answered <- m
	}()

	conn, _ := rawWS(t, a.url, query) // never reads, never answers
	began := time.Now()
	ok := waitUntil(3*time.Second, func() bool { return closedBy(a.metrics, "no_pong") == 1 })
	if !ok {
		t.Fatalf("closed{no_pong} = %v 3 s after a peer went silent", closedBy(a.metrics, "no_pong"))
	}
	t.Logf("a silent peer was closed after %v", time.Since(began))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.Copy(io.Discard, conn); err != nil {
		t.Errorf("the silent peer's connection did not end: %v", err)
	}

	time.Sleep(time.Second) // ten ping intervals
	a.hub.Deliver(events.Event{ID: id, Status: domain.Sent})
	if m := <-answered; m == nil || m["status"] != "sent" {
		t.Errorf("the client that answers pings: %v, want the event", m)
	}
}

// §9: a WebSocket is not cut by the server's request timeouts (§8): they end at the upgrade.
func TestAWebSocketOutlivesTheServersRequestTimeouts(t *testing.T) {
	e := testkit.Setup(t)
	hub := NewHub(HubOptions{})
	h := NewHandler(observability.NewLogger(testLog{t}, "api", "error"), Options{
		Health: Health{Postgres: e.Pool.Ping, Redis: ok}, Pool: e.Pool, MaxAttempts: 6, Hub: hub,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer("", h, timeouts{header: 200 * time.Millisecond, request: 300 * time.Millisecond, answer: 300 * time.Millisecond})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	id := uuid.New()
	c := dial(t, "http://"+ln.Addr().String(), notificationIDParam+"="+id.String())
	time.Sleep(time.Second) // three times the longest of the server's timeouts
	hub.Deliver(events.Event{ID: id, Status: domain.Sent})
	if m := mustNext(t, c); m["status"] != "sent" {
		t.Errorf("after 1 s: %v, want the event", m)
	}
}

// §9: a client that does not read is disconnected when its queue is full; the API's memory
// does not grow with what it does not read, and another client is not held up.
func TestASlowWebSocketClientIsDisconnectedAndMemoryStaysBounded(t *testing.T) {
	a := newWSAPI(t, HubOptions{})
	slowID, otherID := uuid.New(), uuid.New()
	conn, br := rawWS(t, a.url, notificationIDParam+"="+slowID.String()) // never reads
	other := dial(t, a.url, notificationIDParam+"="+otherID.String())
	if !waitUntil(5*time.Second, func() bool { return testutil.ToFloat64(a.metrics.WebSockets) == 2 }) {
		t.Fatal("the two clients are not connected")
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	const n = 1_000_000
	began := time.Now()
	for range n {
		a.hub.Deliver(events.Event{ID: slowID, Status: domain.Processing})
	}
	t.Logf("%d events delivered in %v", n, time.Since(began))
	a.hub.Deliver(events.Event{ID: otherID, Status: domain.Sent})
	runtime.GC()
	runtime.ReadMemStats(&after)
	grew := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("heap grew by %d KiB", grew>>10)
	if grew > 16<<20 {
		t.Errorf("the heap grew by %d MiB for a client that does not read", grew>>20)
	}
	if m := mustNext(t, other); m["id"] != otherID.String() {
		t.Errorf("the other client: %v, want its event", m)
	}
	if !waitUntil(10*time.Second, func() bool { return closedBy(a.metrics, "slow") == 1 }) {
		t.Fatalf("closed{slow} = %v, want 1", closedBy(a.metrics, "slow"))
	}
	if got := serverClose(t, conn, br, 10*time.Second); got != 1008 && got != 0 {
		t.Errorf("the slow client was closed with %d, want 1008 or a cut connection", got)
	}
	if got := testutil.ToFloat64(a.metrics.WebSockets); got != 1 {
		t.Errorf("open connections %v, want 1", got)
	}
}

// §9: on shutdown every WebSocket is closed with 1001, going away.
func TestTheHubClosesEveryWebSocketOnShutdown(t *testing.T) {
	a := newWSAPI(t, HubOptions{})
	c := dial(t, a.url, notificationIDParam+"="+uuid.NewString())
	if !waitUntil(5*time.Second, func() bool { return testutil.ToFloat64(a.metrics.WebSockets) == 1 }) {
		t.Fatal("not connected")
	}
	a.hub.Close()
	if code := closeCode(t, c, 5*time.Second); code != websocket.StatusGoingAway {
		t.Errorf("closed with %d, want 1001", code)
	}
	if _, resp, err := dialFor(a.url, notificationIDParam+"="+uuid.NewString()); err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("an upgrade after shutdown: %v, want a 503", resp)
	}
}

// §9: a client that stops reading fills the TCP buffers until the server's write blocks; the
// write ends at its deadline, so the connection is closed and its slot and goroutine freed,
// not held for as long as the client keeps the socket open. Events are delivered at a pace
// the writer can follow, so the write blocks before the queue overflows.
func TestAWriteToAClientThatStopsReadingEndsAtItsDeadline(t *testing.T) {
	a := newWSAPI(t, HubOptions{WriteTimeout: 500 * time.Millisecond})
	id := uuid.New()
	rawWS(t, a.url, notificationIDParam+"="+id.String()) // never reads
	if !waitUntil(5*time.Second, func() bool { return testutil.ToFloat64(a.metrics.WebSockets) == 1 }) {
		t.Fatal("not connected")
	}
	began := time.Now()
	sent := 0
	for testutil.ToFloat64(a.metrics.WebSockets) == 1 && time.Since(began) < 20*time.Second {
		for range 20 {
			a.hub.Deliver(events.Event{ID: id, Status: domain.Processing})
		}
		sent += 20
		time.Sleep(time.Millisecond)
	}
	t.Logf("%d events delivered before the connection was closed, after %v", sent, time.Since(began))
	if got := testutil.ToFloat64(a.metrics.WebSockets); got != 0 {
		t.Fatalf("%v connections open 20 s after the client stopped reading; the write did not end", got)
	}
	if got := closedBy(a.metrics, "slow"); got != 1 {
		t.Errorf("closed{slow} = %v, want 1", got)
	}
}

// rawRequest sends a handshake by hand and returns the answer's status and body.
func rawRequest(t *testing.T, base, request string) (int, string) {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, request); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, string(b)
}

// §9, session 7's review F6: an upgrade the library would refuse (HTTP/1.0, two keys) is a 400
// in the error form before anything is read, not the library's plain-text refusal after the
// database.
func TestAnUpgradeTheLibraryWouldRefuseIsRefusedFirst(t *testing.T) {
	a := newWSAPI(t, HubOptions{})
	host := strings.TrimPrefix(a.url, "http://")
	path := webSocketPath + "?" + notificationIDParam + "=" + uuid.NewString()
	key := base64.StdEncoding.EncodeToString(make([]byte, 16))
	for name, req := range map[string]string{
		"HTTP/1.0": "GET " + path + " HTTP/1.0\r\nHost: " + host + "\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n" +
			"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + key + "\r\n\r\n",
		"two keys": "GET " + path + " HTTP/1.1\r\nHost: " + host + "\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n" +
			"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Key: " + key + "\r\n\r\n",
	} {
		code, body := rawRequest(t, a.url, req)
		if code != http.StatusBadRequest || !strings.Contains(body, `"upgrade_required"`) {
			t.Errorf("%s: %d %.120s; want a 400 in the error form", name, code, body)
		}
	}
}

// §9: a client's queue holds 256 messages; the 257th lets the client go.
func TestAClientQueueHoldsItsBoundAndNoMore(t *testing.T) {
	h := NewHub(HubOptions{})
	id := uuid.New()
	f, ok := h.follow([]uuid.UUID{id}, nil)
	if !ok {
		t.Fatal("the hub refused a client")
	}
	for range WebSocketQueue {
		h.Deliver(events.Event{ID: id, Status: domain.Processing})
	}
	select {
	case <-f.gone:
		t.Fatalf("let go after %d messages, the bound", WebSocketQueue)
	default:
	}
	h.Deliver(events.Event{ID: id, Status: domain.Sent})
	select {
	case <-f.gone:
		if f.reason != "slow" {
			t.Errorf("let go as %q, want slow", f.reason)
		}
	default:
		t.Fatalf("still followed after %d messages", WebSocketQueue+1)
	}
}

// §9, session 7's review F8: a client that closes its TCP connection without a close frame is
// counted as the client's doing, also when a ping of the server was out.
func TestAClientThatHangsUpIsCountedAsTheClients(t *testing.T) {
	a := newWSAPI(t, HubOptions{PingInterval: 20 * time.Millisecond, PongTimeout: 2 * time.Second})
	const n = 20
	for range n {
		conn, _ := rawWS(t, a.url, notificationIDParam+"="+uuid.NewString())
		time.Sleep(30 * time.Millisecond) // a ping is out: the client never answers it
		_ = conn.Close()
	}
	if !waitUntil(10*time.Second, func() bool { return closedBy(a.metrics, "client")+closedBy(a.metrics, "no_pong") == n }) {
		t.Fatalf("%v closes counted, want %d", closedBy(a.metrics, "client")+closedBy(a.metrics, "no_pong"), n)
	}
	if got := closedBy(a.metrics, "client"); got != n {
		t.Errorf("closed{client} = %v, closed{no_pong} = %v; want all %d as client", got, closedBy(a.metrics, "no_pong"), n)
	}
}
