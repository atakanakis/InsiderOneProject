package provider

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
)

// unansweredAddress returns an address on this machine whose connection attempts are never
// answered: a listener with an accept queue of one that never accepts, filled until a new
// connection attempt times out. When the queue is full, Linux and the BSDs drop an incoming
// SYN without an answer, so a dial waits as it does for an address that drops packets.
// (Linux: tcp_conn_request drops the SYN when sk_acceptq_is_full; the BSDs drop it when
// sonewconn finds the queue full.) The connections that filled the queue are closed when the
// test ends.
func unansweredAddress(t *testing.T) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		_ = syscall.Close(fd)
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 1); err != nil {
		_ = syscall.Close(fd)
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fd), "unanswered")
	ln, err := net.FileListener(f) // dups the descriptor
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	addr := ln.Addr().String()
	for i := 0; ; i++ {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return addr
			}
			t.Fatalf("filling the accept queue: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if i == 100 {
			t.Fatal("100 connections did not fill an accept queue of one")
		}
	}
}

// The dial has its own, shorter timeout (architecture §7, one deadline per send): a provider
// address that does not answer a connection attempt ends the send at the dial timeout, not
// at the send's deadline, and the gate never runs. Until session 8 this had no test: a dial
// that hangs needs an address that drops packets, and the accept queue above is one.
func TestTheDialHasItsOwnTimeout(t *testing.T) {
	addr := unansweredAddress(t)
	o := testOptions("http://" + addr + "/send")
	o.DialTimeout = 300 * time.Millisecond // the send timeout stays 5 s
	c := newClient(t, o)
	g := &countingGate{next: allow}
	began := time.Now()
	r := c.Send(bg(t), message(domain.SMS), g.gate)
	took := time.Since(began)
	t.Logf("a send to an address that does not answer ended after %v with a dial timeout of %v and a send timeout of %v: %v",
		took.Round(time.Millisecond), o.DialTimeout, o.SendTimeout, r.Err)
	if took > 2*time.Second {
		t.Errorf("Send took %v with a dial timeout of %v", took, o.DialTimeout)
	}
	if r.Responded || r.Err == nil || g.calls.Load() != 0 || r.ConnReused {
		t.Errorf("result %+v, gate calls %d; want no response, an error, no gate call", r, g.calls.Load())
	}
}
