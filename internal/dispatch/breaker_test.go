package dispatch

import (
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

const unlimited = int(^uint(0) >> 1)

// openBreaker returns a breaker that has just opened.
func openBreaker(t *testing.T, c *clock) *Breaker {
	t.Helper()
	b := NewBreaker(30*time.Second, c.Now)
	for range FailuresToOpen {
		b.Report(b.Started(), Failure)
	}
	if b.State() != Open {
		t.Fatalf("state %d after %d failures, want open", b.State(), FailuresToOpen)
	}
	return b
}

// Closed: no limit; five failures in a row open it; a success resets the count; "nothing"
// neither counts nor resets.
func TestBreakerOpensAfterFiveFailuresInARow(t *testing.T) {
	c := newClock()
	b := NewBreaker(30*time.Second, c.Now)
	if b.Allow() != unlimited {
		t.Fatalf("a closed breaker allows %d", b.Allow())
	}
	for range FailuresToOpen - 1 {
		b.Report(b.Started(), Failure)
	}
	b.Report(b.Started(), Success)
	for range FailuresToOpen - 1 {
		b.Report(b.Started(), Failure)
		b.Report(b.Started(), Nothing)
	}
	if b.State() != Closed {
		t.Fatalf("state %d: a success reset the count and \"nothing\" did not count, want closed", b.State())
	}
	b.Report(b.Started(), Failure)
	if b.State() != Open || b.Allow() != 0 {
		t.Fatalf("state %d, allow %d after five failures in a row; want open and 0", b.State(), b.Allow())
	}
	c.advance(30*time.Second - time.Millisecond)
	if b.Allow() != 0 {
		t.Fatalf("allow %d before the open time ended", b.Allow())
	}
}

// Half-open lets exactly one row through: Allow is 1 until a row starts, and that row is
// the probe. A success closes the breaker, a failure opens it again.
func TestBreakerHalfOpenLetsOneProbeThrough(t *testing.T) {
	c := newClock()
	b := openBreaker(t, c)
	c.advance(30 * time.Second)
	if b.State() != HalfOpen || b.Allow() != 1 {
		t.Fatalf("state %d, allow %d after the open time; want half-open and 1", b.State(), b.Allow())
	}
	if !b.Started() {
		t.Fatal("the first row to start in half-open is not the probe")
	}
	if b.Allow() != 0 {
		t.Fatalf("allow %d while the probe is out, want 0", b.Allow())
	}
	b.Report(false, Success) // a row that is not the probe teaches nothing here
	if b.State() != HalfOpen || b.Allow() != 0 {
		t.Fatalf("a report that is not the probe's changed the breaker: state %d, allow %d", b.State(), b.Allow())
	}
	b.Report(true, Failure)
	if b.State() != Open || b.Allow() != 0 {
		t.Fatalf("a failed probe: state %d, want open", b.State())
	}
	c.advance(30 * time.Second)
	if !b.Started() {
		t.Fatal("no probe in the second half-open period")
	}
	b.Report(true, Success)
	if b.State() != Closed || b.Allow() != unlimited {
		t.Fatalf("a successful probe: state %d, allow %d; want closed", b.State(), b.Allow())
	}
}

// A probe refused at the gate teaches nothing: the breaker stays half-open and the next row
// is the next probe.
func TestARefusedProbeLeavesTheBreakerHalfOpen(t *testing.T) {
	c := newClock()
	b := openBreaker(t, c)
	c.advance(30 * time.Second)
	b.Report(b.Started(), Nothing)
	if b.State() != HalfOpen || b.Allow() != 1 {
		t.Fatalf("state %d, allow %d after a refused probe; want half-open and 1", b.State(), b.Allow())
	}
	if !b.Started() {
		t.Fatal("the next row is not the probe")
	}
}

// Rows that started before the breaker opened, and report while it is open, change nothing:
// the breaker opened for its whole open time.
func TestReportsWhileOpenChangeNothing(t *testing.T) {
	c := newClock()
	b := openBreaker(t, c)
	b.Report(false, Success)
	b.Report(false, Failure)
	if b.State() != Open {
		t.Fatalf("state %d, want open", b.State())
	}
	c.advance(30 * time.Second)
	if b.State() != HalfOpen {
		t.Fatalf("state %d after the open time, want half-open: a late report moved the open time", b.State())
	}
}

// Allow only reads: asking many times marks no probe, so a claim that finds nothing leaves
// nothing hanging.
func TestBreakerAllowOnlyReads(t *testing.T) {
	c := newClock()
	b := openBreaker(t, c)
	c.advance(30 * time.Second)
	for range 100 {
		if b.Allow() != 1 {
			t.Fatal("Allow changed the breaker")
		}
	}
}
