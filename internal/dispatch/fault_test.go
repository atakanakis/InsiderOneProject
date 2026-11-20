package dispatch

// The randomised fault test (plan, session 5; lessons 15). The other tests of this package
// each reproduce one known failure. This one combines many at random, on two or three
// workers, and then checks the invariants that must hold whatever happened:
//
//   - I1: the provider never received more than the limit of one channel within one second,
//     by its own arrival stamps. It holds while every request arrives within the margin of
//     its permit (A4), so it is checked without the race detector only; with it, the peak
//     and the late requests are logged;
//   - I5: every notification ended as sent, failed or cancelled;
//   - no notification was accepted by the provider twice, and every sent notification was
//     accepted; within the limits of architecture §6, "at-least-once": a notification may be
//     accepted once more for each of its attempts that ended without a response, because
//     the provider may have processed a request whose answer was lost (case 3). The test's
//     own fault makes that happen: the provider closes "idle" connections from outside, and
//     Go's server marks a connection active only after it has read the whole request
//     (net/http server.go, lines 2079 to 2082), so a request can be read, handled and
//     accepted on a connection that is closed under it;
//   - I4: every claim reached Finish (an attempt row for every attempt, no expired lease).
//
// The faults: claims that return late, result writes that fail before they commit and
// results whose commit is lost on the way back, 503 and 429 answers, latency, answers that
// never come (the provider closes the connection), the provider closing kept-alive
// connections while sends wait at the gate, and slow connection setup (a TLS handshake that
// the provider holds back). Every send goes through the real provider client to the
// recording server.
//
// Timing makes each run different even with the same seed, so a scenario logs its seed and
// settings. FAULT_RUNS=n adds n scenarios with seeds FAULT_SEED, FAULT_SEED+1, ... (a random
// FAULT_SEED by default). Every scenario ends with one FAULT_RESULT line, so a long run can
// be counted:
//
//	FAULT_RUNS=50 go test -count=1 -run TestInvariantsHoldUnderRandomFaults -v ./internal/dispatch | grep -c 'FAULT_RESULT.*passed=true'

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/recorder"
	"github.com/atakanakis/InsiderOneProject/internal/store"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

type scenario struct {
	seed           uint64
	workers        int
	tls            bool          // a TLS provider; slow connection setup needs it
	handshakeDelay time.Duration // the provider holds back every new connection's handshake
	stallP         float64       // a claim that returned rows returns them late
	stallMax       time.Duration
	writeFailP     float64 // a result write fails before it commits
	writeLostP     float64 // a result write commits and its answer is lost
	failP          float64 // the provider answers 503
	throttleP      float64 // the provider answers 429 with Retry-After: 1
	closeP         float64 // the provider closes the connection without an answer
	latencyMax     time.Duration
	closeIdleEvery time.Duration // the provider closes its idle kept-alive connections; 0 never
}

func (s scenario) String() string {
	return fmt.Sprintf("seed=%d workers=%d tls=%v handshake=%v stall=%.2f/%v writeFail=%.2f writeLost=%.2f 503=%.2f 429=%.2f close=%.2f latency<%v closeIdle=%v",
		s.seed, s.workers, s.tls, s.handshakeDelay, s.stallP, s.stallMax, s.writeFailP, s.writeLostP,
		s.failP, s.throttleP, s.closeP, s.latencyMax, s.closeIdleEvery)
}

func randomScenario(seed uint64) scenario {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	s := scenario{
		seed:       seed,
		workers:    2 + r.IntN(2),
		tls:        r.IntN(2) == 0,
		stallP:     0.1 + 0.3*r.Float64(),
		stallMax:   time.Duration(100+r.IntN(500)) * time.Millisecond,
		writeFailP: 0.15 * r.Float64(),
		writeLostP: 0.1 * r.Float64(),
		failP:      0.1 * r.Float64(),
		throttleP:  0.05 * r.Float64(),
		closeP:     0.05 * r.Float64(),
		latencyMax: time.Duration(5+r.IntN(80)) * time.Millisecond,
	}
	if s.tls {
		s.handshakeDelay = time.Duration(r.IntN(150)) * time.Millisecond
	}
	if r.IntN(2) == 0 {
		s.closeIdleEvery = time.Duration(50+r.IntN(250)) * time.Millisecond
	}
	return s
}

// dice is the random source of one scenario, safe for concurrent use.
type dice struct {
	mu sync.Mutex
	r  *rand.Rand
}

func newDice(seed uint64) *dice { return &dice{r: rand.New(rand.NewPCG(seed, seed+1))} }

func (d *dice) chance(p float64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.r.Float64() < p
}

func (d *dice) upTo(limit time.Duration) time.Duration {
	if limit <= 0 {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return time.Duration(d.r.Int64N(int64(limit)))
}

// faultyQueue is the real queue with claims that return late and result writes that fail.
type faultyQueue struct {
	Queue
	d *dice
	s scenario
}

var errInjected = errors.New("fault test: injected result write failure")

func (q faultyQueue) Claim(ctx context.Context, ch domain.Channel, p domain.Priority, n int, lease time.Duration) ([]store.Claimed, error) {
	rows, err := q.Queue.Claim(ctx, ch, p, n, lease)
	if len(rows) > 0 && q.d.chance(q.s.stallP) {
		time.Sleep(q.d.upTo(q.s.stallMax)) // committed: the rows are processing, and late
	}
	return rows, err
}

func (q faultyQueue) Finish(ctx context.Context, id uuid.UUID, attempt int, r store.Result) (domain.Status, bool, error) {
	if q.d.chance(q.s.writeFailP) {
		return "", false, errInjected
	}
	status, changed, err := q.Queue.Finish(ctx, id, attempt, r)
	if err == nil && q.d.chance(q.s.writeLostP) {
		return "", false, errInjected // committed; the answer is lost
	}
	return status, changed, err
}

func TestInvariantsHoldUnderRandomFaults(t *testing.T) {
	scenarios := []scenario{randomScenario(1), randomScenario(2), randomScenario(3)}
	// Both kinds of connection and both idle-close settings run whatever the seeds pick.
	scenarios[0].tls, scenarios[0].handshakeDelay, scenarios[0].closeIdleEvery = true, 120*time.Millisecond, 0
	scenarios[1].tls, scenarios[1].handshakeDelay, scenarios[1].closeIdleEvery = false, 0, 100*time.Millisecond
	if n, _ := strconv.Atoi(os.Getenv("FAULT_RUNS")); n > 0 {
		base, err := strconv.ParseUint(os.Getenv("FAULT_SEED"), 10, 64)
		if err != nil {
			base = rand.Uint64() >> 1
		}
		t.Logf("%d more scenarios from seed %d", n, base)
		for i := range uint64(n) {
			scenarios = append(scenarios, randomScenario(base+i))
		}
	}
	for _, sc := range scenarios {
		t.Run(fmt.Sprintf("seed-%d", sc.seed), func(t *testing.T) { runScenario(t, sc) })
	}
}

func runScenario(t *testing.T, sc scenario) {
	const limit, margin, perChannel = 40, 50 * time.Millisecond, 150
	channels := []domain.Channel{domain.SMS, domain.Email}
	t.Logf("scenario: %s", sc)
	var received, peak, late, twice, case3 int
	allFinal := false
	defer func() {
		t.Logf("FAULT_RESULT seed=%d received=%d peak=%d late=%d limit=%d accepted_twice=%d of_them_case3=%d all_final=%v passed=%v",
			sc.seed, received, peak, late, limit, twice, case3, allFinal, !t.Failed())
	}()

	e := testkit.Setup(t)
	d := newDice(sc.seed)
	acc := newAccepted()
	srv := testkit.Recorder(t, recorder.Options{
		TLS:            sc.tls,
		HandshakeDelay: sc.handshakeDelay,
		Respond: acc.respond(func(recorder.Request) recorder.Response {
			resp := recorder.Response{Delay: d.upTo(sc.latencyMax)}
			switch {
			case d.chance(sc.closeP):
				resp.Close = true
			case d.chance(sc.failP):
				resp.Status = http.StatusServiceUnavailable
			case d.chance(sc.throttleP):
				resp.Status, resp.Header = http.StatusTooManyRequests, http.Header{"Retry-After": {"1"}}
			}
			return resp
		}),
	})
	if sc.closeIdleEvery > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go func() {
			for t := time.NewTicker(sc.closeIdleEvery); ; {
				select {
				case <-ctx.Done():
					t.Stop()
					return
				case <-t.C:
					srv.CloseIdle()
				}
			}
		}()
	}

	for _, ch := range channels {
		insertRows(t, e.Pool, ch, perChannel, func(i int) domain.Priority { return domain.Priorities[i%3] }, 25)
	}
	st := newStamps()
	opts := testOptions(t)
	for range sc.workers {
		startWorker(t, e, workerSpec{
			channels: channels, limit: limit, margin: margin, opts: opts,
			provider: providerOptions(srv, sc.tls), stamps: st, reaperTick: 500 * time.Millisecond,
			wrapQueue: func(q Queue) Queue { return faultyQueue{Queue: q, d: d, s: sc} },
		})
	}

	counts, ok := waitAllFinal(t, e.Pool, 90*time.Second)
	allFinal = ok
	received = srv.Received()
	if !ok {
		t.Fatalf("I5: not every notification ended as sent, failed or cancelled: %v (%s)", counts, sc)
	}

	reqs := srv.Requests()
	for _, ch := range channels {
		p := srv.Peak(time.Second, string(ch))
		l, worst := st.late(reqs, string(ch), margin)
		peak, late = max(peak, p), late+l
		t.Logf("%s: peak %d within one second; %d requests arrived later than %v after their permit, the latest %v",
			ch, p, l, margin, worst.Round(time.Millisecond))
		if p > limit && !raceDetector { // why not under -race: TestTheProviderReceivesAtMostTheLimitThroughTheDispatchers
			t.Errorf("I1: the provider received %d %s requests within one second; limit %d, late requests %d (%s)", p, ch, limit, l, sc)
		}
	}
	for _, id := range acc.twice() {
		twice++
		outcomes := attemptOutcomes(t, e.Pool, uuid.MustParse(id))
		noResponse := 0
		for _, o := range outcomes {
			if o == "no response" {
				noResponse++
			}
		}
		if acc.of(id) > 1+noResponse {
			t.Errorf("the provider accepted %s %d times; its attempts %v explain at most %d (%s)", id, acc.of(id), outcomes, 1+noResponse, sc)
			continue
		}
		case3++
		t.Logf("at-least-once case 3: the provider accepted %s %d times; attempts %v", id, acc.of(id), outcomes)
	}
	rows, err := e.Pool.Query(bg(t), `SELECT id FROM notifications WHERE status = 'sent'`)
	if err != nil {
		t.Fatal(err)
	}
	var sent []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, id)
	}
	rows.Close()
	for _, id := range sent {
		if acc.of(id.String()) == 0 {
			t.Errorf("%s is sent, but the provider never accepted it (%s)", id, sc)
		}
	}
	everyClaimReachedFinish(t, e.Pool)
	n, median, longest := st.waitStats()
	t.Logf("statuses %v; %d requests received; %d waits at the gate, median %v, longest %v; %d refused for no permit in time, %d limiter errors",
		counts, received, n, median.Round(time.Millisecond), longest.Round(time.Millisecond), st.noPermit.Load(), st.limiterErr.Load())
}
