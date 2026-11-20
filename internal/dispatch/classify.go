package dispatch

import (
	"errors"
	"net/http"
	"time"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/provider"
	"github.com/atakanakis/InsiderOneProject/internal/ratelimit"
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

// Classify decides the verdict from what one send observed: the table of architecture §6,
// read top to bottom. It is the only place that decides (CLAUDE.md, rule 6).
func Classify(r provider.Result) Verdict {
	switch {
	case r.Responded && r.StatusCode >= 200 && r.StatusCode < 300: // a response always wins (I6)
		return Verdict{Status: domain.Sent, Lesson: Success, Outcome: "sent"}
	case r.Responded && rejected(r.StatusCode):
		return Verdict{Status: domain.Failed, Lesson: Success, Outcome: "rejected"}
	case r.Responded:
		return Verdict{Status: domain.Retrying, Delay: Backoff, RetryAfter: r.RetryAfter, Lesson: Failure, Outcome: "error response"}
	case r.Refusal != nil && !errors.Is(r.Refusal, ratelimit.ErrNoPermitInTime):
		return Verdict{Status: domain.Retrying, Delay: Soon, Lesson: Failure, Outcome: "limiter error"}
	case r.Refusal != nil:
		return Verdict{Status: domain.Retrying, Delay: Soon, Lesson: Nothing, Outcome: "no permit in time"}
	case r.ClosedIdle:
		return Verdict{Status: domain.Retrying, Delay: Soon, Lesson: Nothing, Outcome: "closed idle"}
	default:
		return Verdict{Status: domain.Retrying, Delay: Backoff, Lesson: Failure, Outcome: "no response"}
	}
}

// rejected reports whether the provider refused this message, not the channel: resending the
// same message would fail the same way.
func rejected(code int) bool {
	return code == http.StatusBadRequest || code == http.StatusRequestEntityTooLarge || code == http.StatusUnprocessableEntity
}
