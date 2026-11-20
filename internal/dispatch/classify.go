package dispatch

import (
	"time"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/provider"
)

// Lesson is what the circuit breaker learns from one attempt.
type Lesson int

const (
	Nothing Lesson = iota // the attempt says nothing about the provider's health
	Success               // the provider answered as a healthy provider does
	Failure               // the channel may be broken
)

// Delay says how the retry delay of a verdict is chosen.
type Delay int

const (
	NoDelay Delay = iota // the row is final: sent or failed
	Backoff              // exponential backoff with jitter; a Retry-After may ask for longer
	Soon                 // about one window: the reason was ours, not the provider's
)

// Verdict is what one attempt means: for the row, and for the breaker.
type Verdict struct {
	Status     domain.Status // Sent, Failed or Retrying
	Delay      Delay
	RetryAfter time.Duration // the response's Retry-After, for Backoff
	Lesson     Lesson
	Outcome    string // a short name of the table row, stored with the attempt
}

// Classify decides the verdict from what one send observed (architecture §6, the verdict
// table). It is the only place that decides.
func Classify(r provider.Result) Verdict {
	return Verdict{}
}
