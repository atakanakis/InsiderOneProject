package recorder

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"
)

const message = `{"to":"+905551234567","channel":"sms","content":"hello"}`

func start(t *testing.T, opts Options) *Server {
	t.Helper()
	s, err := Start(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func post(t *testing.T, c *http.Client, url, body string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Idempotency-Key", "n-1")
	resp, err := c.Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	return resp, err
}

func TestRecorderAnswersWithTheProviderContract(t *testing.T) {
	s := start(t, Options{})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, s.URL+"/send", strings.NewReader(message))
	req.Header.Set("Idempotency-Key", "n-1")
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct{ MessageID, Status, Timestamp string }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted || body.MessageID == "" || body.Status != "accepted" {
		t.Fatalf("got %d %+v", resp.StatusCode, body)
	}
	if _, err := time.Parse(time.RFC3339Nano, body.Timestamp); err != nil {
		t.Errorf("timestamp %q is not ISO 8601: %v", body.Timestamp, err)
	}
	got := s.Requests()
	if len(got) != 1 {
		t.Fatalf("recorded %d requests", len(got))
	}
	r := got[0]
	if r.Method != http.MethodPost || r.Path != "/send" || r.Channel != "sms" || string(r.Body) != message ||
		r.Header.Get("Idempotency-Key") != "n-1" || r.Proto != "HTTP/1.1" {
		t.Errorf("recorded %+v", r)
	}
}

// The stamp is the moment the request arrives, not the moment it is answered: a delay
// injected into the response must not move it.
func TestRecorderStampsOnArrivalNotOnResponse(t *testing.T) {
	const delay = 300 * time.Millisecond
	s := start(t, Options{Respond: func(Request) Response { return Response{Delay: delay} }})
	sent := time.Now()
	if _, err := post(t, s.Client(), s.URL, message); err != nil {
		t.Fatal(err)
	}
	answered := time.Since(sent)
	arrived := s.Requests()[0].Arrived.Sub(sent)
	if answered < delay {
		t.Errorf("answered after %s, before the injected delay of %s", answered, delay)
	}
	if arrived > 100*time.Millisecond {
		t.Errorf("stamped %s after sending; the stamp waited for the response", arrived)
	}
}

// The stamp is taken when the headers have arrived, before the body is read: a client that
// is slow to send its body does not move it.
func TestRecorderStampsBeforeTheBodyIsRead(t *testing.T) {
	const pause = 300 * time.Millisecond
	s := start(t, Options{})
	pr, pw := io.Pipe()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, s.URL, pr)
	req.ContentLength = int64(len(message))
	var headersWritten time.Time
	var once sync.Once
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteHeaders: func() { once.Do(func() { headersWritten = time.Now() }) },
	}))
	go func() {
		time.Sleep(pause)
		_, _ = pw.Write([]byte(message))
		_ = pw.Close()
	}()
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	r := s.Requests()[0]
	if string(r.Body) != message {
		t.Fatalf("body %q", r.Body)
	}
	if lag := r.Arrived.Sub(headersWritten); lag > 100*time.Millisecond {
		t.Errorf("stamped %s after the headers were written; it waited for the body", lag)
	}
}

// The recorder is the instrument of the rate-limit tests, so nothing the test does with the
// records may move a stamp: a reader that holds the lock delays the handler, not the stamp.
func TestRecorderStampIsNotDelayedByReaders(t *testing.T) {
	const hold = 300 * time.Millisecond
	s := start(t, Options{})
	s.Recorder.mu.Lock() // the records' lock, not the server's connection lock
	go func() { time.Sleep(hold); s.Recorder.mu.Unlock() }()
	sent := time.Now()
	if _, err := post(t, s.Client(), s.URL, message); err != nil {
		t.Fatal(err)
	}
	if lag := s.Requests()[0].Arrived.Sub(sent); lag > 100*time.Millisecond {
		t.Fatalf("stamped %s after sending, while a reader held the lock for %s", lag, hold)
	}
}

func TestRecorderInjectsStatusCodesAndHeaders(t *testing.T) {
	script := []Response{
		{Status: 503},
		{Status: 429, Header: http.Header{"Retry-After": {"2"}}},
		{Status: 301, Header: http.Header{"Location": {"/elsewhere"}}},
	}
	s := start(t, Options{Respond: func(r Request) Response {
		if r.Seq < len(script) {
			return script[r.Seq]
		}
		return Response{}
	}})
	c := s.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	want := []int{503, 429, 301, 202}
	for i, code := range want {
		resp, err := post(t, c, s.URL, message)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != code {
			t.Errorf("request %d: status %d, want %d", i, resp.StatusCode, code)
		}
		if i == 1 && resp.Header.Get("Retry-After") != "2" {
			t.Errorf("Retry-After not sent")
		}
		if i == 2 && resp.Header.Get("Location") != "/elsewhere" {
			t.Errorf("Location not sent")
		}
	}
	if n := len(s.Requests()); n != len(want) {
		t.Errorf("recorded %d requests, want %d", n, len(want))
	}
}

// A closed connection is "no response": the client gets an error, and the request is still
// recorded, because it did reach the provider.
func TestRecorderClosesTheConnectionWithoutAResponse(t *testing.T) {
	s := start(t, Options{Respond: func(Request) Response { return Response{Close: true} }})
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, s.URL, strings.NewReader(message))
	req.GetBody = nil // no replay by the transport, so exactly one request is sent
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("got a response %d, want a closed connection", resp.StatusCode)
	}
	if n := len(s.Requests()); n != 1 {
		t.Fatalf("recorded %d requests, want 1", n)
	}
}

// Slow connection setup is a slow TLS handshake: the client's connection becomes ready late,
// and once it is ready, the request arrives promptly.
func TestRecorderDelaysTheTLSHandshake(t *testing.T) {
	const delay = 300 * time.Millisecond
	s := start(t, Options{TLS: true, HandshakeDelay: delay})
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: s.CertPool(), MinVersion: tls.VersionTLS12}}
	defer tr.CloseIdleConnections()
	var ready time.Time
	began := time.Now()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, s.URL, strings.NewReader(message))
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { ready = time.Now() },
	}))
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if setup := ready.Sub(began); setup < delay-10*time.Millisecond {
		t.Errorf("connection ready after %s, want at least %s", setup, delay)
	}
	if lag := s.Requests()[0].Arrived.Sub(ready); lag > 100*time.Millisecond {
		t.Errorf("request arrived %s after the connection was ready", lag)
	}
}

func TestRecorderRefusesHandshakeDelayWithoutTLS(t *testing.T) {
	if _, err := Start(Options{HandshakeDelay: time.Second}); err == nil {
		t.Fatal("a handshake delay on a plain HTTP server was accepted")
	}
}

// A TLS recorder offers HTTP/2, so a test can check that a client does not use it.
func TestRecorderOffersHTTP2OverTLS(t *testing.T) {
	s := start(t, Options{TLS: true})
	if _, err := post(t, s.Client(), s.URL, message); err != nil {
		t.Fatal(err)
	}
	if p := s.Requests()[0].Proto; p != "HTTP/2.0" {
		t.Fatalf("a client that offers HTTP/2 got %s", p)
	}
}

// CloseIdle closes kept-alive connections from the server side, so the client's next
// request finds a dead pooled connection.
func TestRecorderClosesIdleConnections(t *testing.T) {
	s := start(t, Options{})
	c := s.Client()
	if _, err := post(t, c, s.URL, message); err != nil {
		t.Fatal(err)
	}
	if _, err := post(t, c, s.URL, message); err != nil {
		t.Fatal(err)
	}
	reqs := s.Requests()
	if reqs[0].ConnID != reqs[1].ConnID {
		t.Fatalf("the second request did not reuse the connection: %d, %d", reqs[0].ConnID, reqs[1].ConnID)
	}
	// The server marks the connection idle just after the client has read the answer.
	closed := 0
	for deadline := time.Now().Add(2 * time.Second); closed == 0 && time.Now().Before(deadline); {
		closed = s.CloseIdle()
		time.Sleep(5 * time.Millisecond)
	}
	if closed != 1 {
		t.Fatalf("CloseIdle closed %d connections, want 1", closed)
	}
	_, _ = post(t, c, s.URL, message) // may fail, or be replayed on a new connection
	reqs = s.Requests()
	if last := reqs[len(reqs)-1]; last.ConnID == reqs[0].ConnID {
		t.Fatalf("a request arrived on the closed connection %d", last.ConnID)
	}
}

func TestRecorderRefusesABodyOverItsBound(t *testing.T) {
	s := start(t, Options{MaxBody: 16})
	resp, err := post(t, s.Client(), s.URL, message)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", resp.StatusCode)
	}
	if r := s.Requests(); len(r) != 1 || len(r[0].Body) > 16 {
		t.Fatalf("recorded %+v", r)
	}
}

func TestRecorderKeepsOnlyTheLastRequestsWhenBounded(t *testing.T) {
	s := start(t, Options{MaxKept: 3, Lean: true})
	for range 5 {
		if _, err := post(t, s.Client(), s.URL, message); err != nil {
			t.Fatal(err)
		}
	}
	got := s.Requests()
	if len(got) != 3 || got[0].Seq != 2 || got[2].Seq != 4 || s.Received() != 5 {
		t.Fatalf("kept %d (first seq %d), received %d", len(got), got[0].Seq, s.Received())
	}
	if got[0].Body != nil || got[0].Header != nil || got[0].Channel != "sms" {
		t.Errorf("Lean: body %q, header %v, channel %q", got[0].Body, got[0].Header, got[0].Channel)
	}
}

func at(ms ...int) []time.Time {
	base := time.Unix(1_700_000_000, 0)
	out := make([]time.Time, len(ms))
	for i, m := range ms {
		out[i] = base.Add(time.Duration(m) * time.Millisecond)
	}
	return out
}

// The windows are half-open, [t, t+window): two requests exactly one window apart are in
// different windows. Order of the input does not matter.
func TestMaxInWindowCountsHalfOpenWindows(t *testing.T) {
	cases := []struct {
		times []time.Time
		want  int
	}{
		{nil, 0},
		{at(0), 1},
		{at(0, 1000), 1},
		{at(0, 999), 2},
		{at(0, 500, 999, 1000, 1500), 3},
		{at(1500, 0, 999, 500, 1000), 3},
		{at(0, 0, 0), 3},
	}
	for _, c := range cases {
		if got := MaxInWindow(c.times, time.Second); got != c.want {
			t.Errorf("MaxInWindow(%v) = %d, want %d", c.times, got, c.want)
		}
	}
}

func TestPeakFiltersByChannel(t *testing.T) {
	s := start(t, Options{})
	for _, ch := range []string{"sms", "sms", "email"} {
		if _, err := post(t, s.Client(), s.URL, `{"channel":"`+ch+`"}`); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.Peak(time.Minute, "sms"); got != 2 {
		t.Errorf("sms peak %d, want 2", got)
	}
	if got := s.Peak(time.Minute, ""); got != 3 {
		t.Errorf("all-channel peak %d, want 3", got)
	}
}

func TestRecorderStopsWaitingWhenTheClientGoesAway(t *testing.T) {
	s := start(t, Options{Respond: func(Request) Response { return Response{Delay: time.Hour} }})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader([]byte(message)))
	_, err := s.Client().Do(req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for the abandoned hour-long response")
	}
}
