package provider

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/ratelimit"
	"github.com/atakanakis/InsiderOneProject/internal/recorder"
	"github.com/atakanakis/InsiderOneProject/internal/redisclient"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

func TestMain(m *testing.M) { testkit.Main(m) }

// testOptions are the client settings of most tests: short enough that a test that waits
// for a timeout is quick, long enough that a loaded machine does not hit them by accident.
func testOptions(url string) Options {
	return Options{
		URL:                 url,
		SendTimeout:         5 * time.Second,
		GateBudget:          2 * time.Second,
		DialTimeout:         2 * time.Second,
		TLSHandshakeTimeout: 2 * time.Second,
		IdleConns:           64,
		IdleConnTimeout:     10 * time.Second,
	}
}

// tlsOptions are testOptions for a TLS recorder, trusting its certificate.
func tlsOptions(srv *recorder.Server) Options {
	o := testOptions(srv.URL)
	o.TLSConfig = &tls.Config{RootCAs: srv.CertPool(), MinVersion: tls.VersionTLS12}
	return o
}

func newClient(t testing.TB, o Options) *Client {
	t.Helper()
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

func message(ch domain.Channel) Message {
	return Message{ID: uuid.New(), To: "+905551234567", Channel: ch, Content: "Your message"}
}

func allow(context.Context) error { return nil }

var errRefused = errors.New("test: refused")

func refuse(context.Context) error { return errRefused }

// countingGate counts its calls and answers with next.
type countingGate struct {
	calls atomic.Int64
	next  Gate
}

func (g *countingGate) gate(ctx context.Context) error {
	g.calls.Add(1)
	return g.next(ctx)
}

func bg(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// received counts the recorded requests of one channel.
func received(srv *recorder.Server, ch domain.Channel) int {
	n := 0
	for _, r := range srv.Requests() {
		if r.Channel == string(ch) {
			n++
		}
	}
	return n
}

// The request is the one the assignment describes: POST, a JSON body of exactly "to",
// "channel" and "content", and the notification id as the Idempotency-Key.
func TestTheRequestHasTheAssignmentsFormat(t *testing.T) {
	srv := testkit.Recorder(t, recorder.Options{})
	c := newClient(t, testOptions(srv.URL+"/send"))
	with := message(domain.SMS)
	with.CorrelationID = "corr-1"
	without := message(domain.Push)
	for _, m := range []Message{with, without} {
		if r := c.Send(bg(t), m, allow); !r.Responded || r.StatusCode != http.StatusAccepted {
			t.Fatalf("send: %+v", r)
		}
	}
	reqs := srv.Requests()
	if len(reqs) != 2 {
		t.Fatalf("the provider received %d requests, want 2", len(reqs))
	}
	for i, m := range []Message{with, without} {
		req := reqs[i]
		if req.Method != http.MethodPost || req.Path != "/send" {
			t.Errorf("request %d: %s %s, want POST /send", i, req.Method, req.Path)
		}
		var body map[string]any
		if err := json.Unmarshal(req.Body, &body); err != nil {
			t.Fatalf("request %d: body %q: %v", i, req.Body, err)
		}
		want := map[string]any{"to": m.To, "channel": string(m.Channel), "content": m.Content}
		if fmt.Sprint(body) != fmt.Sprint(want) {
			t.Errorf("request %d: body %v, want exactly %v", i, body, want)
		}
		if got := req.Header.Get("Idempotency-Key"); got != m.ID.String() {
			t.Errorf("request %d: Idempotency-Key %q, want the notification id %s", i, got, m.ID)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("request %d: Content-Type %q", i, got)
		}
		if got := req.Header.Get("X-Correlation-ID"); got != m.CorrelationID {
			t.Errorf("request %d: X-Correlation-ID %q, want %q", i, got, m.CorrelationID)
		}
	}
}

// Property 1 of architecture §7: the gate runs once per send, after the connection is ready
// (here after a TLS handshake that the provider holds back) and before any byte of the
// request is written. The content is larger than the transport's 4 KiB write buffer, so a
// gate that ran after the write had begun would find the request's headers at the provider.
func TestTheGateRunsOnceWhenTheConnectionIsReadyAndBeforeTheWrite(t *testing.T) {
	const setup = 300 * time.Millisecond
	srv := testkit.Recorder(t, recorder.Options{TLS: true, HandshakeDelay: setup})
	c := newClient(t, tlsOptions(srv))
	m := message(domain.Email)
	m.Content = strings.Repeat("x", 16<<10)
	for i, wantReused := range []bool{false, true} {
		var calls, receivedAtGate atomic.Int64
		var atGate atomic.Int64 // time from the start of Send to the gate, in ns
		var gateDone atomic.Value
		began := time.Now()
		r := c.Send(bg(t), m, func(context.Context) error {
			calls.Add(1)
			atGate.Store(int64(time.Since(began)))
			time.Sleep(100 * time.Millisecond) // a request written before the gate would arrive meanwhile
			receivedAtGate.Store(int64(srv.Received()))
			gateDone.Store(time.Now())
			return nil
		})
		if !r.Responded || r.StatusCode != http.StatusAccepted {
			t.Fatalf("send %d: %+v", i, r)
		}
		if calls.Load() != 1 {
			t.Errorf("send %d: the gate ran %d times, want 1", i, calls.Load())
		}
		if r.ConnReused != wantReused {
			t.Errorf("send %d: reused connection %v, want %v", i, r.ConnReused, wantReused)
		}
		if at := time.Duration(atGate.Load()); !wantReused && at < setup {
			t.Errorf("send %d: the gate ran %v after the start, before the connection was ready (setup takes %v)", i, at, setup)
		}
		if n := receivedAtGate.Load(); n != int64(i) {
			t.Errorf("send %d: the provider had received %d requests when the gate returned, want %d: the write began before the gate", i, n, i)
		}
		done, _ := gateDone.Load().(time.Time)
		if arrived := srv.Requests()[i].Arrived; arrived.Before(done) {
			t.Errorf("send %d: the request arrived %v before the gate returned", i, done.Sub(arrived))
		}
	}
}

// A1: a send that the gate refuses never reaches the provider, on new connections and on
// reused ones. Cancelling the request instead let 13 of 40,000 refused requests through in
// experiment 01. Every refused send of the "reused" half follows an allowed send on the same
// goroutine, so it usually finds that send's connection idle in the pool. 5,000 per half:
// each refused send leaves one client port in TIME_WAIT, and macOS has about 16,000.
func TestARefusedSendNeverReachesTheProvider(t *testing.T) {
	const perMode, parallel = 5000, 32
	srv := testkit.Recorder(t, recorder.Options{Lean: true})
	for _, reuse := range []bool{false, true} {
		srv.Reset()
		c := newClient(t, testOptions(srv.URL))
		var refused, reused, gateCalls, responses atomic.Int64
		var wrong atomic.Value
		var wg sync.WaitGroup
		work := make(chan struct{})
		for range parallel {
			wg.Go(func() {
				for range work {
					if reuse {
						if r := c.Send(bg(t), message(domain.SMS), allow); !r.Responded {
							wrong.Store(fmt.Sprintf("an allowed send got no response: %v", r.Err))
						}
					}
					g := &countingGate{next: refuse}
					r := c.Send(bg(t), message(domain.Email), g.gate)
					gateCalls.Add(g.calls.Load())
					if r.Responded {
						responses.Add(1)
					}
					if errors.Is(r.Refusal, errRefused) && errors.Is(r.Err, errRefused) {
						refused.Add(1)
					}
					if r.ConnReused {
						reused.Add(1)
					}
				}
			})
		}
		for range perMode {
			work <- struct{}{}
		}
		close(work)
		wg.Wait()
		time.Sleep(200 * time.Millisecond) // a request still on its way would arrive by now

		reached := received(srv, domain.Email)
		t.Logf("reuse=%v: %d refused, %d of them on reused connections, %d reached the provider",
			reuse, refused.Load(), reused.Load(), reached)
		if w := wrong.Load(); w != nil {
			t.Fatal(w)
		}
		if reached != 0 {
			t.Errorf("reuse=%v: %d refused sends reached the provider", reuse, reached)
		}
		if refused.Load() != perMode || gateCalls.Load() != perMode || responses.Load() != 0 {
			t.Errorf("reuse=%v: %d refusals, %d gate calls, %d responses; want %d, %d, 0",
				reuse, refused.Load(), gateCalls.Load(), responses.Load(), perMode, perMode)
		}
		if reuse && reused.Load() < perMode/2 {
			t.Errorf("only %d of %d refused sends ran on a reused connection; the test did not test reuse", reused.Load(), perMode)
		}
		if !reuse && reused.Load() != 0 {
			t.Errorf("%d refused sends ran on a reused connection; the new-connection half did not test new connections", reused.Load())
		}
	}
}

// Property 2: the client closes a refused send's connection inside its hook, before the
// transport goes on to write. Measured at the provider, a send that is only cancelled leaks
// a few times in ten thousand; here the order is checked directly. The test adds its own
// GotConn hook to the context, which httptrace calls right after the client's hook, with the
// same connection; at that moment the connection must already be closed.
func TestARefusedSendsConnectionIsClosedBeforeTheTransportGoesOn(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		srv := testkit.Recorder(t, recorder.Options{TLS: useTLS})
		o := testOptions(srv.URL)
		if useTLS {
			o = tlsOptions(srv)
		}
		c := newClient(t, o)
		for _, warm := range []bool{false, true} {
			if warm {
				if r := c.Send(bg(t), message(domain.SMS), allow); !r.Responded {
					t.Fatalf("tls=%v: an allowed send got no response: %v", useTLS, r.Err)
				}
			}
			closed := errors.New("the test's hook was not called")
			var reused bool
			ctx := httptrace.WithClientTrace(bg(t), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
				reused = info.Reused
				closed = info.Conn.SetReadDeadline(time.Time{})
			}})
			r := c.Send(ctx, message(domain.Email), refuse)
			if !errors.Is(closed, net.ErrClosed) {
				t.Errorf("tls=%v warm=%v: the connection was not closed when the client's hook returned (%v)", useTLS, warm, closed)
			}
			if reused != warm || r.ConnReused != warm {
				t.Errorf("tls=%v warm=%v: reused %v (result %v)", useTLS, warm, reused, r.ConnReused)
			}
			if !errors.Is(r.Refusal, errRefused) || r.Responded {
				t.Errorf("tls=%v warm=%v: %+v", useTLS, warm, r)
			}
		}
		time.Sleep(100 * time.Millisecond)
		if n := received(srv, domain.Email); n != 0 {
			t.Errorf("tls=%v: %d refused sends reached the provider", useTLS, n)
		}
	}
}

// A2, property 3: a reused connection that the provider closes after the client took it from
// the pool fails that send. Go would replay a request that carries an Idempotency-Key on a
// new connection, with a second gate call and a second write; the client turns replays off.
func TestADeadReusedConnectionIsNotReplayed(t *testing.T) {
	srv := testkit.Recorder(t, recorder.Options{})
	c := newClient(t, testOptions(srv.URL))
	if r := c.Send(bg(t), message(domain.SMS), allow); !r.Responded {
		t.Fatalf("the first send got no response: %v", r.Err)
	}
	var closedByProvider atomic.Int64
	g := &countingGate{next: func(context.Context) error {
		// The provider closes the kept-alive connection now: after the client took it from
		// its pool, before the request is written.
		for deadline := time.Now().Add(2 * time.Second); closedByProvider.Load() == 0 && time.Now().Before(deadline); {
			closedByProvider.Add(int64(srv.CloseIdle()))
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond) // the close reaches the client
		return nil
	}}
	r := c.Send(bg(t), message(domain.Email), g.gate)
	time.Sleep(200 * time.Millisecond)

	if closedByProvider.Load() != 1 {
		t.Fatalf("the provider closed %d idle connections, want 1", closedByProvider.Load())
	}
	if !r.ConnReused {
		t.Fatal("the second send did not run on the kept-alive connection")
	}
	if g.calls.Load() != 1 {
		t.Errorf("the gate ran %d times in one send, want 1", g.calls.Load())
	}
	if n := received(srv, domain.Email); n > 1 {
		t.Errorf("the provider received the send %d times, want at most 1", n)
	}
	if r.Responded || r.Err == nil {
		t.Errorf("a send on a dead connection: %+v, want no response and an error", r)
	}
	t.Logf("gate calls %d, received %d, error %v", g.calls.Load(), received(srv, domain.Email), r.Err)
}

// Property 5: a redirect is a response like any other. Go's client would follow 301, 302
// and 303 with a GET without the body, and 307 and 308 with the body, on a second request.
func TestRedirectsAreNotFollowed(t *testing.T) {
	var code atomic.Int64
	srv := testkit.Recorder(t, recorder.Options{Respond: func(req recorder.Request) recorder.Response {
		if req.Path == "/moved" {
			return recorder.Response{}
		}
		return recorder.Response{Status: int(code.Load()), Header: http.Header{"Location": {"/moved"}}}
	}})
	c := newClient(t, testOptions(srv.URL+"/send"))
	for _, status := range []int{301, 302, 303, 307, 308} {
		srv.Reset()
		code.Store(int64(status))
		g := &countingGate{next: allow}
		r := c.Send(bg(t), message(domain.SMS), g.gate)
		if !r.Responded || r.StatusCode != status {
			t.Errorf("%d: the result is %+v, want the response %d", status, r, status)
		}
		if n := srv.Received(); n != 1 || g.calls.Load() != 1 {
			t.Errorf("%d: the provider received %d requests and the gate ran %d times, want 1 and 1", status, n, g.calls.Load())
		}
	}
}

// Property 4: HTTP/1.1 only, against a TLS provider that offers HTTP/2. The client is built
// fresh, as every Client builds its own transport.
func TestTheClientSpeaksHTTP11ToAServerThatOffersHTTP2(t *testing.T) {
	srv := testkit.Recorder(t, recorder.Options{TLS: true})
	c := newClient(t, tlsOptions(srv))
	for range 2 {
		if r := c.Send(bg(t), message(domain.SMS), allow); !r.Responded {
			t.Fatalf("send: %v", r.Err)
		}
	}
	for _, req := range srv.Requests() {
		if req.Proto != "HTTP/1.1" {
			t.Errorf("the provider saw %s, want HTTP/1.1", req.Proto)
		}
	}
}

// One deadline covers connecting, the gate, the write and the response. Each part in turn
// takes longer than the send timeout; Send returns at the deadline with no response.
func TestOneDeadlineCoversConnectGateWriteAndResponse(t *testing.T) {
	const timeout = 500 * time.Millisecond
	check := func(t *testing.T, o Options, m Message, gate Gate) Result {
		t.Helper()
		o.SendTimeout, o.GateBudget, o.TLSHandshakeTimeout = timeout, 5*time.Second, 5*time.Second
		c := newClient(t, o)
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		began := time.Now()
		r := c.Send(ctx, m, gate)
		took := time.Since(began)
		if took < timeout || took > timeout+time.Second {
			t.Errorf("Send took %v, want the send timeout %v", took, timeout)
		}
		if r.Responded || !errors.Is(r.Err, context.DeadlineExceeded) {
			t.Errorf("result %+v, want no response and the deadline", r)
		}
		return r
	}

	t.Run("connect", func(t *testing.T) {
		srv := testkit.Recorder(t, recorder.Options{TLS: true, HandshakeDelay: 3 * time.Second})
		g := &countingGate{next: allow}
		check(t, tlsOptions(srv), message(domain.SMS), g.gate)
		if g.calls.Load() != 0 {
			t.Errorf("the gate ran on a connection that never became ready")
		}
	})
	t.Run("gate", func(t *testing.T) {
		srv := testkit.Recorder(t, recorder.Options{})
		r := check(t, testOptions(srv.URL), message(domain.SMS), func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
		if !errors.Is(r.Refusal, context.DeadlineExceeded) || srv.Received() != 0 {
			t.Errorf("refusal %v, received %d; want the deadline and nothing", r.Refusal, srv.Received())
		}
	})
	t.Run("write", func(t *testing.T) {
		// A provider that accepts the connection and never reads: the write of a body far
		// larger than the socket buffers cannot finish.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		var conns []net.Conn
		var mu sync.Mutex
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				mu.Lock()
				conns = append(conns, conn)
				mu.Unlock()
			}
		}()
		t.Cleanup(func() {
			_ = ln.Close()
			mu.Lock()
			defer mu.Unlock()
			for _, c := range conns {
				_ = c.Close()
			}
		})
		m := message(domain.Email)
		m.Content = strings.Repeat("x", 32<<20)
		check(t, testOptions("http://"+ln.Addr().String()), m, allow)
	})
	t.Run("response", func(t *testing.T) {
		srv := testkit.Recorder(t, recorder.Options{Respond: func(recorder.Request) recorder.Response {
			return recorder.Response{Delay: 3 * time.Second}
		}})
		check(t, testOptions(srv.URL), message(domain.SMS), allow)
		if srv.Received() != 1 {
			t.Errorf("the provider received %d requests, want 1", srv.Received())
		}
	})
}

// The TLS handshake has its own, shorter timeout.
func TestTheTLSHandshakeHasItsOwnTimeout(t *testing.T) {
	srv := testkit.Recorder(t, recorder.Options{TLS: true, HandshakeDelay: 2 * time.Second})
	o := tlsOptions(srv)
	o.TLSHandshakeTimeout = 200 * time.Millisecond
	c := newClient(t, o)
	g := &countingGate{next: allow}
	began := time.Now()
	r := c.Send(bg(t), message(domain.SMS), g.gate)
	if took := time.Since(began); took > time.Second {
		t.Errorf("Send took %v with a handshake timeout of 200ms", took)
	}
	if r.Responded || r.Err == nil || g.calls.Load() != 0 {
		t.Errorf("result %+v, gate calls %d; want no response, an error, no gate call", r, g.calls.Load())
	}
}

// The pool keeps an idle connection for every slot: a second round of as many concurrent
// sends as slots runs entirely on kept-alive connections. Go's default keeps 2 per host.
func TestTheClientKeepsAnIdleConnectionForEverySlot(t *testing.T) {
	const slots = 50
	srv := testkit.Recorder(t, recorder.Options{Lean: true})
	o := testOptions(srv.URL)
	o.IdleConns = slots
	c := newClient(t, o)
	round := func() (reused int) {
		var arrived atomic.Int64
		all := make(chan struct{})
		results := make([]Result, slots)
		var wg sync.WaitGroup
		for i := range slots {
			wg.Go(func() {
				results[i] = c.Send(bg(t), message(domain.SMS), func(ctx context.Context) error {
					// Every send holds its connection until all of them hold one.
					if arrived.Add(1) == slots {
						close(all)
					}
					select {
					case <-all:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			})
		}
		wg.Wait()
		for _, r := range results {
			if !r.Responded {
				t.Fatalf("a send got no response: %v", r.Err)
			}
			if r.ConnReused {
				reused++
			}
		}
		return reused
	}
	if n := round(); n != 0 {
		t.Fatalf("the first round reused %d connections, want 0", n)
	}
	if n := round(); n != slots {
		t.Errorf("the second round reused %d of %d connections", n, slots)
	}
}

// Idle connections are dropped after the idle timeout, so a connection the provider may
// have closed meanwhile is rarely picked (architecture §7, the connection pool).
func TestIdleConnectionsAreDroppedAfterTheIdleTimeout(t *testing.T) {
	srv := testkit.Recorder(t, recorder.Options{Lean: true})
	o := testOptions(srv.URL)
	o.IdleConnTimeout = 200 * time.Millisecond
	c := newClient(t, o)
	send := func() Result {
		r := c.Send(bg(t), message(domain.SMS), allow)
		if !r.Responded {
			t.Fatalf("send: %v", r.Err)
		}
		return r
	}
	send()
	if r := send(); !r.ConnReused {
		t.Fatal("a send right after another did not reuse its connection")
	}
	time.Sleep(500 * time.Millisecond)
	if r := send(); r.ConnReused {
		t.Error("a send after the idle timeout reused the old connection")
	}
}

// The Result carries what was observed: whether a response came, its status, Retry-After,
// message id and body; whether the gate refused and why; whether the connection was reused;
// the error when no response came; the duration.
func TestTheResultCarriesWhatWasObserved(t *testing.T) {
	var respond atomic.Value
	srv := testkit.Recorder(t, recorder.Options{Respond: func(recorder.Request) recorder.Response {
		r, _ := respond.Load().(recorder.Response)
		return r
	}})
	c := newClient(t, testOptions(srv.URL))
	send := func(resp recorder.Response, gate Gate) Result {
		respond.Store(resp)
		return c.Send(bg(t), message(domain.SMS), gate)
	}
	retry := func(v string) recorder.Response {
		return recorder.Response{Status: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {v}}}
	}

	r := send(recorder.Response{}, allow)
	if !r.Responded || r.StatusCode != http.StatusAccepted || r.MessageID == "" || r.Refusal != nil || r.Err != nil || r.Duration <= 0 {
		t.Errorf("an accepted send: %+v", r)
	}
	if r := send(recorder.Response{Status: http.StatusBadRequest, Body: []byte(`{"error":"bad recipient"}`)}, allow); !r.Responded ||
		r.StatusCode != http.StatusBadRequest || string(r.Body) != `{"error":"bad recipient"}` || r.MessageID != "" {
		t.Errorf("a rejected send: %+v", r)
	}
	if r := send(recorder.Response{Status: http.StatusOK, Body: []byte(`{"messageId":"m-1"}`)}, allow); r.MessageID != "m-1" {
		t.Errorf("a 200 with a message id: %+v", r)
	}
	for v, want := range map[string]time.Duration{
		"7": 7 * time.Second, "0": 0, "-3": 0, "soon": 0, "": 0,
		"99999999999999999999": MaxRetryAfter, "9999999999": MaxRetryAfter, "-99999999999999999999": 0,
	} {
		if r := send(retry(v), allow); r.StatusCode != http.StatusServiceUnavailable || r.RetryAfter != want {
			t.Errorf("Retry-After %q: %v, want %v", v, r.RetryAfter, want)
		}
	}
	date := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	if r := send(retry(date), allow); r.RetryAfter < 28*time.Second || r.RetryAfter > 30*time.Second {
		t.Errorf("Retry-After as a date 30 s ahead: %v", r.RetryAfter)
	}
	if r := send(recorder.Response{Close: true}, allow); r.Responded || r.Err == nil || r.Refusal != nil {
		t.Errorf("a connection closed without an answer: %+v", r)
	}
	noPermit := fmt.Errorf("gate: %w", ratelimit.ErrNoPermitInTime)
	if r := send(recorder.Response{}, func(context.Context) error { return noPermit }); r.Responded ||
		!errors.Is(r.Refusal, ratelimit.ErrNoPermitInTime) || !errors.Is(r.Err, ratelimit.ErrNoPermitInTime) {
		t.Errorf("a refused send: %+v", r)
	}

	// Nothing listens: no connection, so the gate never runs.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	closed := newClient(t, testOptions("http://"+addr))
	g := &countingGate{next: allow}
	if r := closed.Send(bg(t), message(domain.SMS), g.gate); r.Responded || r.Err == nil || r.Refusal != nil || g.calls.Load() != 0 {
		t.Errorf("no provider listening: %+v, gate calls %d", r, g.calls.Load())
	}
}

// The body read from a response is bounded (architecture §8); the rest is not read.
func TestTheResponseBodyIsReadUpToItsBound(t *testing.T) {
	var size atomic.Int64
	srv := testkit.Recorder(t, recorder.Options{Respond: func(recorder.Request) recorder.Response {
		return recorder.Response{Status: http.StatusInternalServerError, Body: []byte(strings.Repeat("e", int(size.Load())))}
	}})
	c := newClient(t, testOptions(srv.URL))
	for _, n := range []int{MaxResponseBody - 1, MaxResponseBody, MaxResponseBody + 1, 8 << 20} {
		size.Store(int64(n))
		r := c.Send(bg(t), message(domain.SMS), allow)
		if want := min(n, MaxResponseBody); !r.Responded || len(r.Body) != want {
			t.Errorf("a body of %d bytes: read %d, want %d (%v)", n, len(r.Body), want, r.Err)
		}
	}
}

// The response headers are bounded (architecture §8); a larger header block is not a
// response. Go's default bound is 10 MiB.
func TestResponseHeadersOverTheBoundAreNotAResponse(t *testing.T) {
	var size atomic.Int64
	srv := testkit.Recorder(t, recorder.Options{Respond: func(recorder.Request) recorder.Response {
		return recorder.Response{Header: http.Header{"X-Padding": {strings.Repeat("h", int(size.Load()))}}}
	}})
	c := newClient(t, testOptions(srv.URL))
	size.Store(MaxResponseHeaderBytes - 1024)
	if r := c.Send(bg(t), message(domain.SMS), allow); !r.Responded {
		t.Errorf("headers just under the bound: %v", r.Err)
	}
	size.Store(MaxResponseHeaderBytes)
	if r := c.Send(bg(t), message(domain.SMS), allow); r.Responded || r.Err == nil {
		t.Errorf("headers over the bound: %+v, want no response", r)
	}
}

// A gate that is still waiting when the send's deadline passes stops waiting, and takes no
// permit afterwards: its context is a child of the send's.
func TestAGateThatWaitsPastTheDeadlineTakesNoPermit(t *testing.T) {
	e := testkit.Setup(t)
	lim, err := ratelimit.New(e.Redis, 1, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := lim.Wait(bg(t), domain.SMS); err != nil { // the window is full for 1.05 s
		t.Fatal(err)
	}
	began := time.Now()
	srv := testkit.Recorder(t, recorder.Options{})
	o := testOptions(srv.URL)
	o.SendTimeout, o.GateBudget = 300*time.Millisecond, 5*time.Second
	c := newClient(t, o)
	r := c.Send(bg(t), message(domain.SMS), func(ctx context.Context) error { return lim.Wait(ctx, domain.SMS) })
	if took := time.Since(began); took > 800*time.Millisecond {
		t.Errorf("Send took %v with a send timeout of 300ms", took)
	}
	if r.Responded || !errors.Is(r.Refusal, ratelimit.ErrNoPermitInTime) {
		t.Errorf("result %+v, want a refusal with ErrNoPermitInTime", r)
	}
	time.Sleep(time.Until(began.Add(1500 * time.Millisecond))) // the first permit left the window at 1.05 s
	free, err := lim.Available(bg(t), domain.SMS)
	if err != nil {
		t.Fatal(err)
	}
	if free != 1 {
		t.Errorf("%d free permits 1.5 s later, want 1: the gate took a permit after its send had ended", free)
	}
	if srv.Received() != 0 {
		t.Errorf("the provider received %d requests", srv.Received())
	}
}

// I1 at the provider's side: three workers, each with its own Redis client, limiter and
// provider client, send as fast as their gates allow; the provider never receives more than
// the limit within one second, by its own arrival stamps.
func TestTheProviderReceivesAtMostTheLimitInAnyWindow(t *testing.T) {
	const limit, workers, sendersPerWorker = 100, 3, 50
	const run = 3300 * time.Millisecond
	e := testkit.Setup(t)
	srv := testkit.Recorder(t, recorder.Options{Lean: true})
	stop := time.Now().Add(run)
	var sent, refused atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		rdb, err := redisclient.New(e.RedisURL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = rdb.Close() })
		lim, err := ratelimit.New(rdb, limit, 50*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		o := testOptions(srv.URL)
		o.IdleConns = sendersPerWorker
		c := newClient(t, o)
		gate := func(ctx context.Context) error { return lim.Wait(ctx, domain.SMS) }
		for range sendersPerWorker {
			wg.Go(func() {
				for time.Now().Before(stop) {
					r := c.Send(bg(t), message(domain.SMS), gate)
					if r.Refusal != nil {
						refused.Add(1)
					} else if r.Responded {
						sent.Add(1)
					}
				}
			})
		}
	}
	wg.Wait()
	peak := srv.Peak(time.Second, string(domain.SMS))
	t.Logf("%d sends answered, %d refused at the gate, peak %d within one second at the provider",
		sent.Load(), refused.Load(), peak)
	if peak > limit {
		t.Errorf("the provider received %d requests within one second, limit %d", peak, limit)
	}
	if sent.Load() < 2*limit {
		t.Errorf("only %d sends in %v; the test did not run at the limit", sent.Load(), run)
	}
}

// A4: the time from the permit to the request's arrival at the provider, on new connections
// (a fresh client per send, with a TLS handshake that the provider holds back by more than
// the margin) and on reused ones, against the 50 ms margin of the window.
//
// The bound is checked without the race detector, as make guards runs every named test.
// Under -race the client, the recorder and the other packages' tests share the CPUs at
// several times the cost, and the test only reports its figures (architecture §12, A4).
func TestPermitToArrivalStaysWithinTheMargin(t *testing.T) {
	const margin = 50 * time.Millisecond
	const run, sendersPerGroup = 2 * time.Second, 20
	e := testkit.Setup(t)
	lim, err := ratelimit.New(e.Redis, 100, margin)
	if err != nil {
		t.Fatal(err)
	}
	srv := testkit.Recorder(t, recorder.Options{TLS: true, HandshakeDelay: 2 * margin})
	var permitted, reusedBy sync.Map // message id -> time of the permit; -> the send's ConnReused
	send := func(c *Client) {
		m := message(domain.SMS)
		r := c.Send(bg(t), m, func(ctx context.Context) error {
			if err := lim.Wait(ctx, domain.SMS); err != nil {
				return err
			}
			permitted.Store(m.ID, time.Now())
			return nil
		})
		if r.Responded {
			reusedBy.Store(m.ID, r.ConnReused)
		}
	}
	shared := newClient(t, tlsOptions(srv))
	stop := time.Now().Add(run)
	var wg sync.WaitGroup
	for range sendersPerGroup {
		wg.Go(func() {
			for time.Now().Before(stop) {
				c, err := New(tlsOptions(srv))
				if err != nil {
					panic(err)
				}
				send(c)
				c.CloseIdleConnections()
			}
		})
		wg.Go(func() {
			for time.Now().Before(stop) {
				send(shared)
			}
		})
	}
	wg.Wait()

	delays := map[bool][]time.Duration{}
	for _, req := range srv.Requests() {
		id, err := uuid.Parse(req.Header.Get("Idempotency-Key"))
		if err != nil {
			t.Fatal(err)
		}
		p, ok := permitted.Load(id)
		reused, answered := reusedBy.Load(id)
		if !ok || !answered {
			continue
		}
		delays[reused.(bool)] = append(delays[reused.(bool)], req.Arrived.Sub(p.(time.Time)))
	}
	for _, reused := range []bool{false, true} {
		d := delays[reused]
		if len(d) < 30 {
			t.Errorf("reused=%v: only %d sends measured", reused, len(d))
			continue
		}
		slices.Sort(d)
		at := func(q float64) time.Duration { return d[int(q*float64(len(d)-1))] }
		t.Logf("reused=%v: %d sends; permit to arrival: median %v, p99 %v, max %v",
			reused, len(d), at(0.5), at(0.99), d[len(d)-1])
		if d[len(d)-1] >= margin && !raceDetector {
			t.Errorf("reused=%v: a request arrived %v after its permit, margin %v", reused, d[len(d)-1], margin)
		}
	}
}
