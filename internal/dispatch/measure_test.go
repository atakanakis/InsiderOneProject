package dispatch

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/provider"
	"github.com/atakanakis/InsiderOneProject/internal/recorder"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

// productionProvider is the provider client of architecture §13's defaults.
func productionProvider(srv *recorder.Server) provider.Options {
	o := providerOptions(srv, false)
	o.SendTimeout, o.GateBudget, o.DialTimeout, o.TLSHandshakeTimeout = 15*time.Second, 5*time.Second, 3*time.Second, 3*time.Second
	return o
}

// I1 through the dispatchers, with a worker's real load: three workers, each with a dispatcher
// per channel (nine loops), all channels at the limit of 100 at once. The provider's arrival
// stamps decide. A4 is measured on the same requests: the time from each permit to the
// request's arrival at the provider.
//
// The guarantee holds while every request arrives within the margin of its permit (A4). Under
// the race detector A4 does not hold, and the lateness cannot be measured either: the permit
// is stamped when Wait returns on a worker that is itself starved, so a request can be late
// by its grant and on time by its stamp. CI run 44750111098 saw a peak of 47 with a limit of
// 40 and only 6 requests counted late, in the fault test. So the peak is checked against the
// limit without the race detector (make guards and CI's measurement step run it so), and
// only logged with it.
func TestTheProviderReceivesAtMostTheLimitThroughTheDispatchers(t *testing.T) {
	const limit, margin, perChannel, workers = 100, 50 * time.Millisecond, 400, 3
	e := testkit.Setup(t)
	srv := testkit.Recorder(t, recorder.Options{})
	for _, ch := range domain.Channels {
		insertRows(t, e.Pool, ch, perChannel, func(i int) domain.Priority { return domain.Priorities[i%3] }, 6)
	}
	st := newStamps()
	opts := testOptions(t)
	for range workers {
		startWorker(t, e, workerSpec{
			channels: domain.Channels, limit: limit, margin: margin, opts: opts,
			provider: productionProvider(srv), stamps: st,
		})
	}
	counts, ok := waitAllFinal(t, e.Pool, 60*time.Second)
	if !ok || counts[domain.Sent] != perChannel*len(domain.Channels) {
		t.Fatalf("statuses %v, want all sent", counts)
	}
	reqs := srv.Requests()
	for _, ch := range domain.Channels {
		peak := srv.Peak(time.Second, string(ch))
		d := st.delays(reqs, string(ch))
		late, _ := st.late(reqs, string(ch), margin)
		t.Logf("%s: peak %d within one second; permit to arrival over %d requests: median %v, p99 %v, max %v; %d later than %v",
			ch, peak, len(d), percentile(d, 50).Round(10*time.Microsecond), percentile(d, 99).Round(10*time.Microsecond),
			percentile(d, 100).Round(10*time.Microsecond), late, margin)
		if peak > limit && !raceDetector {
			t.Errorf("I1: the provider received %d %s requests within one second; limit %d, %d late", peak, ch, limit, late)
		}
	}
}

// A5: the wait at the gate, the refusals and the throughput with 1, 3, 6 and 12 dispatchers
// on one channel, each in its own worker, with the timing budget's defaults (gate budget 5 s)
// and a provider that answers in 20 ms. By default it runs 1 and 12 dispatchers for 6 s;
// A5_FULL=1 runs all four counts for 12 s each:
//
//	A5_FULL=1 go test -count=1 -run TestGateWaitAndThroughputWithManyDispatchers -v ./internal/dispatch
//
// Session 8 measured past those settings with A5_COUNTS (dispatchers, comma-separated),
// A5_LATENCY (the provider's answer time), A5_LIMIT and A5_RUN, for example:
//
//	A5_COUNTS=12,24,36 A5_LATENCY=200ms A5_RUN=12s go test -count=1 -timeout 30m -run TestGateWaitAndThroughputWithManyDispatchers -v ./internal/dispatch
func TestGateWaitAndThroughputWithManyDispatchers(t *testing.T) {
	limit, margin, latency := 100, 50*time.Millisecond, 20*time.Millisecond
	counts, run := []int{1, 12}, 6*time.Second
	if os.Getenv("A5_FULL") != "" {
		counts, run = []int{1, 3, 6, 12}, 12*time.Second
	}
	if v := os.Getenv("A5_COUNTS"); v != "" {
		counts = nil
		for _, f := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil || n < 1 {
				t.Fatalf("A5_COUNTS=%q: %q is not a positive count", v, f)
			}
			counts = append(counts, n)
		}
	}
	for name, dst := range map[string]*time.Duration{"A5_LATENCY": &latency, "A5_RUN": &run} {
		if v := os.Getenv(name); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				t.Fatalf("%s=%q is not a positive duration", name, v)
			}
			*dst = d
		}
	}
	if v := os.Getenv("A5_LIMIT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("A5_LIMIT=%q is not a positive limit", v)
		}
		limit = n
	}
	top := float64(limit) / (time.Second + margin).Seconds()
	for _, w := range counts {
		t.Run(fmt.Sprintf("%d dispatchers", w), func(t *testing.T) {
			e := testkit.Setup(t)
			srv := testkit.Recorder(t, recorder.Options{Respond: func(recorder.Request) recorder.Response {
				return recorder.Response{Delay: latency}
			}})
			insertRows(t, e.Pool, domain.SMS, int(run.Seconds()+3)*limit, always(domain.Normal), 6)
			st := newStamps()
			began := time.Now()
			var running []*runningWorker
			for range w {
				running = append(running, startWorker(t, e, workerSpec{
					channels: []domain.Channel{domain.SMS}, limit: limit, margin: margin, opts: testOptions(t),
					provider: productionProvider(srv), stamps: st,
				}))
			}
			time.Sleep(run)
			for _, r := range running {
				r.stop(30 * time.Second)
			}
			// Throughput by the provider's stamps, after the first second. Permits come free a
			// window's worth at a time, so a count over a fixed span depends on where the span
			// cuts the bursts; the rate is taken over whole multiples of the limit instead: from
			// the first request after the first second to the request limit × m later.
			var arrived []time.Time
			for _, r := range srv.Requests() {
				if r.Arrived.Before(began.Add(run)) {
					arrived = append(arrived, r.Arrived)
				}
			}
			slices.SortFunc(arrived, func(a, b time.Time) int { return a.Compare(b) })
			first := slices.IndexFunc(arrived, func(a time.Time) bool { return !a.Before(began.Add(time.Second)) })
			if first < 0 || len(arrived)-first <= limit {
				t.Fatalf("%d requests in %v: too few to measure", len(arrived), run)
			}
			last := first + (len(arrived)-1-first)/limit*limit
			rate := float64(last-first) / arrived[last].Sub(arrived[first]).Seconds()
			waits := st.sortedWaits()
			peak := srv.Peak(time.Second, string(domain.SMS))
			t.Logf("A5 W=%d, limit %d, provider %v: %.1f sends/s (top speed %.1f); gate wait over %d sends: median %v, p99 %v, max %v; "+
				"refused for no permit in time %d, limiter errors %d; peak %d within one second",
				w, limit, latency, rate, top, len(waits), percentile(waits, 50).Round(time.Millisecond), percentile(waits, 99).Round(time.Millisecond),
				percentile(waits, 100).Round(time.Millisecond), st.noPermit.Load(), st.limiterErr.Load(), peak)
			if st.noPermit.Load() != 0 || st.limiterErr.Load() != 0 {
				t.Errorf("refusals at the gate: %d without a permit in time, %d limiter errors", st.noPermit.Load(), st.limiterErr.Load())
			}
			// The rate is the limiter's; only a starved machine lowers it. Under the race
			// detector, with the other packages' tests on the same CPUs, it is logged only.
			if rate < 0.9*top && !raceDetector {
				t.Errorf("%.1f sends per second, below 90%% of the top speed %.1f", rate, top)
			}
		})
	}
}
