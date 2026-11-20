package dispatch

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/provider"
	"github.com/atakanakis/InsiderOneProject/internal/ratelimit"
)

var (
	errLimiter   = errors.New("rate limit script: OOM command not allowed")
	errNoPermit  = fmt.Errorf("%w (context deadline exceeded)", ratelimit.ErrNoPermitInTime)
	errTransport = errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
)

func responded(code int) provider.Result {
	return provider.Result{Responded: true, StatusCode: code}
}

// Every row of the verdict table of architecture §6, read top to bottom.
func TestClassifyDecidesEveryRowOfTheVerdictTable(t *testing.T) {
	type want struct {
		status domain.Status
		delay  Delay
		lesson Lesson
	}
	sent := want{domain.Sent, NoDelay, Success}
	rejected := want{domain.Failed, NoDelay, Success}
	otherResponse := want{domain.Retrying, Backoff, Failure}
	cases := []struct {
		name string
		r    provider.Result
		want want
	}{
		{"200", responded(200), sent},
		{"202", responded(202), sent},
		{"204", responded(204), sent},
		{"299", responded(299), sent},
		{"400", responded(400), rejected},
		{"413", responded(413), rejected},
		{"422", responded(422), rejected},
		{"101", responded(101), otherResponse},
		{"301", responded(301), otherResponse},
		{"401", responded(401), otherResponse},
		{"403", responded(403), otherResponse},
		{"404", responded(404), otherResponse},
		{"408", responded(408), otherResponse},
		{"409", responded(409), otherResponse},
		{"429", responded(429), otherResponse},
		{"500", responded(500), otherResponse},
		{"503", responded(503), otherResponse},
		{"limiter error", provider.Result{Refusal: errLimiter, Err: errLimiter}, want{domain.Retrying, Soon, Failure}},
		{"no permit in time", provider.Result{Refusal: errNoPermit, Err: errNoPermit}, want{domain.Retrying, Soon, Nothing}},
		{"closed idle", provider.Result{ClosedIdle: true, ConnReused: true, Err: errTransport}, want{domain.Retrying, Soon, Nothing}},
		{"no response", provider.Result{Err: errTransport}, want{domain.Retrying, Backoff, Failure}},
		{"no response on a reused connection", provider.Result{ConnReused: true, Err: errTransport}, want{domain.Retrying, Backoff, Failure}},
	}
	for _, c := range cases {
		v := Classify(c.r)
		if v.Status != c.want.status || v.Delay != c.want.delay || v.Lesson != c.want.lesson {
			t.Errorf("%s: status %q, delay %d, lesson %d; want %q, %d, %d", c.name, v.Status, v.Delay, v.Lesson,
				c.want.status, c.want.delay, c.want.lesson)
		}
		if v.Outcome == "" {
			t.Errorf("%s: no outcome", c.name)
		}
	}
	if v := Classify(provider.Result{Responded: true, StatusCode: 503, RetryAfter: 7 * time.Second}); v.RetryAfter != 7*time.Second {
		t.Errorf("a 503 with Retry-After 7s: the verdict carries %v", v.RetryAfter)
	}
}

// I6: a response always wins. Whatever else the Result says, a response decides.
func TestAResponseAlwaysWins(t *testing.T) {
	cases := []struct {
		name string
		r    provider.Result
		want domain.Status
	}{
		{"202 and a refusal", provider.Result{Responded: true, StatusCode: http.StatusAccepted, Refusal: errLimiter}, domain.Sent},
		{"202 and an error reading the body", provider.Result{Responded: true, StatusCode: http.StatusAccepted, Err: errTransport}, domain.Sent},
		{"202 and closed idle", provider.Result{Responded: true, StatusCode: http.StatusAccepted, ClosedIdle: true}, domain.Sent},
		{"400 and a refusal", provider.Result{Responded: true, StatusCode: http.StatusBadRequest, Refusal: errNoPermit}, domain.Failed},
	}
	for _, c := range cases {
		if v := Classify(c.r); v.Status != c.want || v.Lesson != Success {
			t.Errorf("%s: %q with lesson %d; want %q, learned as a success", c.name, v.Status, v.Lesson, c.want)
		}
	}
	// A 503 with a refusal is still a response: a failure, not the refusal's "nothing".
	if v := Classify(provider.Result{Responded: true, StatusCode: 503, Refusal: errNoPermit}); v.Lesson != Failure || v.Delay != Backoff {
		t.Errorf("503 and a refusal: delay %d, lesson %d; want backoff and a failure", v.Delay, v.Lesson)
	}
}
