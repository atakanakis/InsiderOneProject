package recorder

import (
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// Server is a recorder listening on a local port, for tests.
type Server struct {
	*Recorder
	URL string

	ts    *httptest.Server
	mu    sync.Mutex
	conns map[net.Conn]http.ConnState
}

// Start starts a recorder on 127.0.0.1 with a free port.
func Start(opts Options) (*Server, error) {
	if opts.HandshakeDelay > 0 && !opts.TLS {
		return nil, errors.New("recorder: HandshakeDelay needs TLS; a plain TCP connection cannot be slowed by the server")
	}
	s := &Server{Recorder: New(opts), conns: map[net.Conn]http.ConnState{}}
	ts := httptest.NewUnstartedServer(s.Recorder)
	ts.Config.ConnContext = ConnContext
	ts.Config.ConnState = s.track
	if opts.HandshakeDelay > 0 {
		ts.Listener = slowListener{Listener: ts.Listener, delay: opts.HandshakeDelay}
	}
	if opts.TLS {
		ts.EnableHTTP2 = true
		ts.StartTLS()
	} else {
		ts.Start()
	}
	s.ts, s.URL = ts, ts.URL
	return s, nil
}

// Client returns a client that trusts the server's certificate. Over TLS it offers HTTP/2.
func (s *Server) Client() *http.Client { return s.ts.Client() }

// CertPool returns a pool holding the server's certificate, for a client built by the test.
func (s *Server) CertPool() *x509.CertPool {
	pool := x509.NewCertPool()
	if c := s.ts.Certificate(); c != nil {
		pool.AddCert(c)
	}
	return pool
}

// CloseIdle closes, from the server side, every connection that is kept alive with no
// request on it, and returns how many it closed.
func (s *Server) CloseIdle() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for c, st := range s.conns {
		if st == http.StateIdle {
			_ = c.Close()
			delete(s.conns, c)
			n++
		}
	}
	return n
}

// Close stops the server and closes every connection, without waiting for delayed answers.
func (s *Server) Close() {
	s.ts.CloseClientConnections()
	s.ts.Close()
}

func (s *Server) track(c net.Conn, st http.ConnState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st == http.StateClosed || st == http.StateHijacked {
		delete(s.conns, c)
		return
	}
	s.conns[c] = st
}

// slowListener delays the first read of every new connection, which holds back the server's
// side of the TLS handshake.
type slowListener struct {
	net.Listener
	delay time.Duration
}

func (l slowListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &slowConn{Conn: c, delay: l.delay}, nil
}

type slowConn struct {
	net.Conn
	delay time.Duration
	once  sync.Once
}

func (c *slowConn) Read(p []byte) (int, error) {
	c.once.Do(func() { time.Sleep(c.delay) })
	return c.Conn.Read(p)
}
