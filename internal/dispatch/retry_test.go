package dispatch

import (
	"testing"
	"time"
)

func fixed(x float64) func() float64 { return func() float64 { return x } }

// Architecture §6: after attempt k the ceiling is min(cap, base × 2^(k−1)), and the delay is
// drawn between half the ceiling and the ceiling. "Soon" is drawn between Soon and 2 × Soon.
func TestRetryDelaysDoubleWithJitterUpToTheCap(t *testing.T) {
	backoff := Verdict{Delay: Backoff}
	ceilings := []time.Duration{2, 4, 8, 16, 32, 64, 120, 120}
	for i, c := range ceilings {
		c *= time.Second
		attempt := i + 1
		low := RetryPolicy{Base: 2 * time.Second, Cap: 2 * time.Minute, Soon: time.Second, Rand: fixed(0)}
		high := low
		high.Rand = fixed(0.999999)
		if d := low.DelayAfter(attempt, backoff); d != c/2 {
			t.Errorf("attempt %d, lowest draw: %v, want %v", attempt, d, c/2)
		}
		if d := high.DelayAfter(attempt, backoff); d < c-time.Millisecond || d > c {
			t.Errorf("attempt %d, highest draw: %v, want just under %v", attempt, d, c)
		}
	}
	p := RetryPolicy{Base: 2 * time.Second, Cap: 2 * time.Minute, Soon: time.Second}
	for _, attempt := range []int{0, 30, 62, 63, 64, 1000, 1 << 40} {
		if d := p.DelayAfter(attempt, backoff); d <= 0 || d > p.Cap {
			t.Errorf("attempt %d: %v, want within (0, %v]", attempt, d, p.Cap)
		}
	}
	for range 1000 {
		if d := p.DelayAfter(3, Verdict{Delay: Soon}); d < time.Second || d >= 2*time.Second {
			t.Fatalf("a Soon delay of %v, want within [1s, 2s)", d)
		}
	}
	if d := p.DelayAfter(3, Verdict{Delay: NoDelay}); d != 0 {
		t.Errorf("a final verdict has a delay of %v", d)
	}
	// The draws spread: rows that failed together do not come back together.
	seen := map[time.Duration]bool{}
	for range 100 {
		seen[p.DelayAfter(4, backoff)] = true
	}
	if len(seen) < 50 {
		t.Errorf("100 delays after attempt 4 took only %d values", len(seen))
	}
}

// A Retry-After is honoured when it asks for longer than the backoff, up to the cap.
func TestRetryAfterIsHonouredUpToTheCap(t *testing.T) {
	p := RetryPolicy{Base: 2 * time.Second, Cap: 2 * time.Minute, Soon: time.Second, Rand: fixed(0)}
	cases := []struct {
		retryAfter, want time.Duration
	}{
		{0, time.Second},                      // no header: the backoff (lowest draw of a 2 s ceiling)
		{500 * time.Millisecond, time.Second}, // shorter than the backoff
		{10 * time.Second, 10 * time.Second},
		{time.Hour, 2 * time.Minute},
		{24 * time.Hour, 2 * time.Minute},
	}
	for _, c := range cases {
		if d := p.DelayAfter(1, Verdict{Delay: Backoff, RetryAfter: c.retryAfter}); d != c.want {
			t.Errorf("Retry-After %v: %v, want %v", c.retryAfter, d, c.want)
		}
	}
}
