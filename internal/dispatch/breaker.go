package dispatch

import (
	"math"
	"sync"
	"time"
)

// BreakerState is the state of a circuit breaker.
type BreakerState int

const (
	Closed BreakerState = iota
	Open
	HalfOpen
)

// FailuresToOpen is the number of failures in a row that opens a closed breaker.
const FailuresToOpen = 5

// Breaker is the circuit breaker of one dispatcher (architecture §6). Allow only reads;
// Started marks the probe when a row really starts; Report clears it. Nothing is reserved
// ahead (CLAUDE.md, rule 4): a claim that finds nothing calls neither Started nor Report.
//
// Allow and Started are called on the loop's goroutine, Report on the sends' goroutines.
type Breaker struct {
	open time.Duration
	now  func() time.Time

	mu       sync.Mutex
	failures int       // failures in a row while closed
	openedAt time.Time // zero while closed
	probing  bool      // half-open: the probe is out
}

// NewBreaker returns a closed breaker that stays open for open; now is its clock.
func NewBreaker(open time.Duration, now func() time.Time) *Breaker {
	return &Breaker{open: open, now: now}
}

// state is the state now; b.mu is held.
func (b *Breaker) state() BreakerState {
	switch {
	case b.openedAt.IsZero():
		return Closed
	case b.now().Sub(b.openedAt) < b.open:
		return Open
	default:
		return HalfOpen
	}
}

// State returns the breaker's state now.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state()
}

// Allow returns how many rows may start now: no limit while closed, none while open, and one
// while half-open with no probe out.
func (b *Breaker) Allow() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state() {
	case Closed:
		return math.MaxInt
	case HalfOpen:
		if !b.probing {
			return 1
		}
	}
	return 0
}

// Started is called when a row starts. In the half-open state the row becomes the probe, and
// Started says so; the row's Report passes the answer back.
func (b *Breaker) Started() (probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state() == HalfOpen && !b.probing {
		b.probing = true
		return true
	}
	return false
}

// Report tells the breaker what an attempt taught. While closed every report counts. While
// open or half-open only the probe's report counts: a success closes the breaker, a failure
// opens it again, and "nothing" leaves it half-open for the next row.
func (b *Breaker) Report(probe bool, l Lesson) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.state() == Closed && l == Success:
		b.failures = 0
	case b.state() == Closed && l == Failure:
		if b.failures++; b.failures >= FailuresToOpen {
			b.failures, b.openedAt = 0, b.now()
		}
	case probe && b.probing:
		b.probing = false
		switch l {
		case Success:
			b.openedAt = time.Time{}
		case Failure:
			b.openedAt = b.now()
		}
	}
}
