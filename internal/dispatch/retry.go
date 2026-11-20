package dispatch

import (
	"math/rand/v2"
	"time"
)

// RetryPolicy chooses how long a row waits before its next attempt (architecture §6).
type RetryPolicy struct {
	Base time.Duration  // the ceiling after the first attempt; it doubles with each attempt
	Cap  time.Duration  // no delay is longer
	Soon time.Duration  // "about one window": a Soon delay is drawn between Soon and 2 × Soon
	Rand func() float64 // a number in [0, 1); nil is math/rand/v2's Float64
}

// DelayAfter returns how long the row waits after attempt number attempt ended with v.
//
// Backoff: the ceiling is min(Cap, Base × 2^(attempt−1)), and the delay is drawn between half
// the ceiling and the ceiling, so rows that failed together come back apart. A Retry-After
// that asks for longer is honoured, up to Cap.
func (p RetryPolicy) DelayAfter(attempt int, v Verdict) time.Duration {
	random := p.Rand
	if random == nil {
		random = rand.Float64
	}
	switch v.Delay {
	case Soon:
		return p.Soon + time.Duration(random()*float64(p.Soon))
	case Backoff:
		ceiling := p.Base
		for i := 1; i < attempt && ceiling < p.Cap; i++ { // stops at the cap: no overflow
			ceiling *= 2
		}
		ceiling = min(ceiling, p.Cap)
		half := ceiling / 2
		d := half + time.Duration(random()*float64(ceiling-half))
		return max(d, min(v.RetryAfter, p.Cap))
	default:
		return 0
	}
}
