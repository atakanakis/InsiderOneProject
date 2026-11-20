package dispatch

import "time"

// BreakerState is the state of a circuit breaker.
type BreakerState int

const (
	Closed BreakerState = iota
	Open
	HalfOpen
)

// FailuresToOpen is the number of failures in a row that opens a closed breaker.
const FailuresToOpen = 5

// Breaker is the circuit breaker of one dispatcher (architecture §6).
type Breaker struct{}

// NewBreaker returns a closed breaker that stays open for open; now is its clock.
func NewBreaker(open time.Duration, now func() time.Time) *Breaker { return &Breaker{} }

// Allow returns how many rows may start now.
func (b *Breaker) Allow() int { return 0 }

// Started is called when a row starts, and reports whether the row is the probe.
func (b *Breaker) Started() bool { return false }

// Report tells the breaker what an attempt taught.
func (b *Breaker) Report(probe bool, l Lesson) {}

// State returns the breaker's state now.
func (b *Breaker) State() BreakerState { return Closed }
