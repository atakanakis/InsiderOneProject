package dispatch

import "time"

// RetryPolicy chooses how long a row waits before its next attempt (architecture §6).
type RetryPolicy struct {
	Base time.Duration // the delay after the first attempt, before jitter
	Cap  time.Duration // no delay is longer
	Soon time.Duration // "about one window": a Soon delay is drawn between Soon and 2 × Soon
	Rand func() float64
}

// DelayAfter returns how long the row waits after attempt number attempt ended with v.
func (p RetryPolicy) DelayAfter(attempt int, v Verdict) time.Duration {
	return 0
}
