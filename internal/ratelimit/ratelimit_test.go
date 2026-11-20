package ratelimit

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"net"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/redisclient"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

func TestMain(m *testing.M) { testkit.Main(m) }

// newLimiter returns a limiter on the test Redis, emptied.
func newLimiter(t *testing.T, limit int, margin time.Duration) (*Limiter, *redis.Client) {
	t.Helper()
	e := testkit.Setup(t)
	l, err := New(e.Redis, limit, margin)
	if err != nil {
		t.Fatal(err)
	}
	return l, e.Redis
}

func ctxFor(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// redisMicros is the Redis server's clock, in microseconds.
func redisMicros(t *testing.T, rdb *redis.Client) int64 {
	t.Helper()
	now, err := rdb.Time(ctxFor(t, 5*time.Second)).Result()
	if err != nil {
		t.Fatal(err)
	}
	return now.UnixMicro()
}

// permits returns the members and scores of a channel's window, as stored.
func permits(t *testing.T, rdb *redis.Client, ch domain.Channel) []redis.Z {
	t.Helper()
	z, err := rdb.ZRangeWithScores(ctxFor(t, 5*time.Second), key(ch), 0, -1).Result()
	if err != nil {
		t.Fatal(err)
	}
	return z
}

func mustWait(t *testing.T, l *Limiter, ch domain.Channel) int64 {
	t.Helper()
	at, err := l.wait(ctxFor(t, 10*time.Second), ch)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	return at
}

func mustAvailable(t *testing.T, l *Limiter, ch domain.Channel) int {
	t.Helper()
	n, err := l.Available(ctxFor(t, 5*time.Second), ch)
	if err != nil {
		t.Fatalf("available: %v", err)
	}
	return n
}

// A6: many callers, on several connections, as several workers would be, ask for permits as
// fast as they can. Half of them wait in Wait's loop and so arrive when a permit frees; the
// other half arrive at random moments, as new sends do, and ask once each time. In the
// permits' own times (the Redis clock, as stored) no half-open window of one second plus the
// margin holds more than the limit. The run must also reach the limit in a window and grant
// at least three windows' worth, so that the check is not passed by a limiter that grants
// little.
func TestNoWindowHoldsMoreThanTheLimit(t *testing.T) {
	const (
		limit   = 100
		margin  = 50 * time.Millisecond
		clients = 4
		callers = 50 // per client: 200 callers in all, twice the limit
		run     = 3300 * time.Millisecond
	)
	e := testkit.Setup(t)
	window := (time.Second + margin).Microseconds()

	ctx, cancel := context.WithTimeout(context.Background(), run)
	defer cancel()
	var (
		mu  sync.Mutex
		ats []int64
		wg  sync.WaitGroup
	)
	for range clients {
		rdb, err := redisclient.New(e.RedisURL)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rdb.Close() }()
		l, err := New(rdb, limit, margin)
		if err != nil {
			t.Fatal(err)
		}
		record := func(at int64) {
			mu.Lock()
			ats = append(ats, at)
			mu.Unlock()
		}
		for range callers / 2 {
			wg.Go(func() { // waits for each permit
				for ctx.Err() == nil {
					at, err := l.wait(ctx, domain.SMS)
					if err != nil {
						if ctx.Err() == nil {
							t.Errorf("wait: %v", err)
						}
						return
					}
					record(at)
				}
			})
			wg.Go(func() { // arrives at random moments and asks once
				for ctx.Err() == nil {
					granted, at, _, err := l.take(ctx, domain.SMS)
					if err != nil {
						if ctx.Err() == nil {
							t.Errorf("take: %v", err)
						}
						return
					}
					if granted {
						record(at)
					}
					time.Sleep(time.Duration(rand.IntN(20_000)) * time.Microsecond)
				}
			})
		}
	}
	wg.Wait()

	slices.Sort(ats)
	most := 0
	for i := range ats {
		j := i
		for j < len(ats) && ats[j]-ats[i] < window {
			j++
		}
		most = max(most, j-i)
	}
	t.Logf("%d permits in %s; at most %d in a window of %d µs", len(ats), run, most, window)
	for i := 0; i+limit < len(ats); i++ {
		if ats[i+limit]-ats[i] < window {
			t.Fatalf("permits %d and %d are %d µs apart: %d permits in a window of %d µs",
				i, i+limit, ats[i+limit]-ats[i], limit+1, window)
		}
	}
	if len(ats) < 3*limit || most != limit {
		t.Fatalf("%d permits, at most %d in one window: the callers did not keep the limiter full", len(ats), most)
	}
}

func TestAPermitIsRefusedAtTheLimit(t *testing.T) {
	l, rdb := newLimiter(t, 3, 50*time.Millisecond)
	for range 3 {
		mustWait(t, l, domain.SMS)
	}
	if n := mustAvailable(t, l, domain.SMS); n != 0 {
		t.Fatalf("available after 3 of 3 permits: %d", n)
	}
	err := l.Wait(ctxFor(t, 100*time.Millisecond), domain.SMS)
	if !errors.Is(err, ErrNoPermitInTime) {
		t.Fatalf("a fourth permit within the window: err = %v, want ErrNoPermitInTime", err)
	}
	if n := len(permits(t, rdb, domain.SMS)); n != 3 {
		t.Fatalf("%d permits stored, want 3", n)
	}
}

// Wait returns when the oldest permit leaves the window, measured on the permits' own Redis
// times: not earlier (that would break the limit), and not much later (that would waste it).
// The permits are 400 ms apart, so a limiter that waited for the newest one would be late.
func TestWaitReturnsWhenTheOldestPermitLeavesTheWindow(t *testing.T) {
	const margin = 50 * time.Millisecond
	l, _ := newLimiter(t, 2, margin)
	window := (time.Second + margin).Microseconds()
	first := mustWait(t, l, domain.SMS)
	time.Sleep(400 * time.Millisecond)
	mustWait(t, l, domain.SMS)
	third := mustWait(t, l, domain.SMS)
	if gap := third - first; gap < window || gap > window+150_000 {
		t.Fatalf("the third permit came %d µs after the first; want between %d and %d", gap, window, window+150_000)
	}
}

func TestAPermitCountsForOneSecondPlusTheMargin(t *testing.T) {
	const margin = 300 * time.Millisecond
	l, _ := newLimiter(t, 1, margin)
	window := (time.Second + margin).Microseconds()
	first := mustWait(t, l, domain.SMS)
	second := mustWait(t, l, domain.SMS)
	if gap := second - first; gap < window || gap > window+150_000 {
		t.Fatalf("with a limit of 1, the second permit came %d µs after the first; want between %d and %d",
			gap, window, window+150_000)
	}
}

func TestWaitStopsWhenItsContextEnds(t *testing.T) {
	l, rdb := newLimiter(t, 1, 50*time.Millisecond)
	mustWait(t, l, domain.SMS)
	began := time.Now()
	err := l.Wait(ctxFor(t, 100*time.Millisecond), domain.SMS)
	took := time.Since(began)
	if !errors.Is(err, ErrNoPermitInTime) {
		t.Fatalf("err = %v, want ErrNoPermitInTime", err)
	}
	if took > 400*time.Millisecond {
		t.Fatalf("Wait returned %s after its 100 ms deadline", took)
	}
	if n := len(permits(t, rdb, domain.SMS)); n != 1 {
		t.Fatalf("%d permits stored, want 1: a wait that ended took a permit", n)
	}
}

func TestWaitWithAnEndedContextTakesNoPermit(t *testing.T) {
	l, rdb := newLimiter(t, 5, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Wait(ctx, domain.SMS); !errors.Is(err, ErrNoPermitInTime) {
		t.Fatalf("Wait with a cancelled context: err = %v, want ErrNoPermitInTime", err)
	}
	if n := len(permits(t, rdb, domain.SMS)); n != 0 {
		t.Fatalf("%d permits stored, want 0", n)
	}
}

// Only the take script fails while reads still work: a read-only replica after a failover, or
// a Redis out of memory. Available keeps answering (the dispatcher keeps claiming), and Wait
// returns an error, never a permit, so the refusals reach the breaker (architecture §6, §7).
func TestAvailableWorksAndWaitFailsWhenOnlyTheScriptFails(t *testing.T) {
	cases := []struct {
		name            string
		breakIt, mendIt []any
	}{
		{"read-only replica", []any{"replicaof", "127.0.0.1", "1"}, []any{"replicaof", "no", "one"}},
		{"out of memory", []any{"config", "set", "maxmemory", "1"}, []any{"config", "set", "maxmemory", "0"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, rdb := newLimiter(t, 5, 50*time.Millisecond)
			mustWait(t, l, domain.SMS)
			mend := func() {
				if err := rdb.Do(ctxFor(t, 5*time.Second), c.mendIt...).Err(); err != nil {
					t.Errorf("restore redis: %v", err)
				}
			}
			if err := rdb.Do(ctxFor(t, 5*time.Second), c.breakIt...).Err(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(mend)

			if n, err := l.Available(ctxFor(t, 5*time.Second), domain.SMS); err != nil || n != 4 {
				t.Fatalf("Available = %d, %v; want 4 and no error", n, err)
			}
			if err := l.Wait(ctxFor(t, 2*time.Second), domain.SMS); err == nil {
				t.Fatal("Wait returned a permit while the script fails")
			} else if errors.Is(err, ErrNoPermitInTime) {
				t.Fatalf("Wait waited for its deadline instead of returning the script's error: %v", err)
			}
			if n := len(permits(t, rdb, domain.SMS)); n != 1 {
				t.Fatalf("%d permits stored, want 1", n)
			}
		})
	}
}

// Available only reads. The window here holds permits that are past it, which a writing read
// would remove, and the key's expiry, which a writing read would renew.
func TestAvailableChangesNothing(t *testing.T) {
	const limit = 5
	l, rdb := newLimiter(t, limit, 50*time.Millisecond)
	ctx := ctxFor(t, 10*time.Second)
	now := redisMicros(t, rdb)
	k := key(domain.SMS)
	if err := rdb.ZAdd(ctx, k,
		redis.Z{Score: float64(now - 10_000_000), Member: "old-1"},
		redis.Z{Score: float64(now - 5_000_000), Member: "old-2"},
		redis.Z{Score: float64(now), Member: "new-1"},
		redis.Z{Score: float64(now), Member: "new-2"},
	).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.PExpire(ctx, k, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	before := permits(t, rdb, domain.SMS)
	ttl := rdb.PTTL(ctx, k).Val()
	for range 20 {
		if n := mustAvailable(t, l, domain.SMS); n != limit-2 {
			t.Fatalf("Available = %d, want %d", n, limit-2)
		}
	}
	if after := permits(t, rdb, domain.SMS); !slices.Equal(before, after) {
		t.Fatalf("the window changed:\nbefore %v\nafter  %v", before, after)
	}
	if after := rdb.PTTL(ctx, k).Val(); after > ttl || after <= 0 {
		t.Fatalf("the expiry changed from %s to %s", ttl, after)
	}
	if n, _ := rdb.DBSize(ctx).Result(); n != 1 {
		t.Fatalf("%d keys, want 1", n)
	}
}

func TestAvailableCountsTheFreePermits(t *testing.T) {
	l, rdb := newLimiter(t, 5, 50*time.Millisecond)
	if n := mustAvailable(t, l, domain.SMS); n != 5 {
		t.Fatalf("an empty window: Available = %d, want 5", n)
	}
	mustWait(t, l, domain.SMS)
	mustWait(t, l, domain.SMS)
	if n := mustAvailable(t, l, domain.SMS); n != 3 {
		t.Fatalf("after 2 permits: Available = %d, want 3", n)
	}
	// More permits than the limit, written here by hand, is no room, never a negative number.
	now := redisMicros(t, rdb)
	for i := range 4 {
		rdb.ZAdd(ctxFor(t, 5*time.Second), key(domain.SMS), redis.Z{Score: float64(now), Member: "extra-" + strconv.Itoa(i)})
	}
	if n := mustAvailable(t, l, domain.SMS); n != 0 {
		t.Fatalf("7 permits of 5: Available = %d, want 0", n)
	}
}

func TestChannelsHaveSeparateWindows(t *testing.T) {
	l, _ := newLimiter(t, 2, 50*time.Millisecond)
	mustWait(t, l, domain.SMS)
	mustWait(t, l, domain.SMS)
	if n := mustAvailable(t, l, domain.Email); n != 2 {
		t.Fatalf("email after sms used its limit: Available = %d, want 2", n)
	}
	if err := l.Wait(ctxFor(t, 100*time.Millisecond), domain.Email); err != nil {
		t.Fatalf("email after sms used its limit: %v", err)
	}
}

// The window's size is bounded: a refused call adds nothing, so a channel's set never holds
// more than the limit, and the key expires one window after the last permit.
func TestTheWindowHoldsAtMostTheLimitAndExpires(t *testing.T) {
	const margin = 50 * time.Millisecond
	l, rdb := newLimiter(t, 3, margin)
	for range 3 {
		mustWait(t, l, domain.SMS)
	}
	for range 50 {
		_ = l.Wait(ctxFor(t, 5*time.Millisecond), domain.SMS)
	}
	if n := len(permits(t, rdb, domain.SMS)); n != 3 {
		t.Fatalf("%d permits stored after 50 refusals, want 3", n)
	}
	ttl := rdb.PTTL(ctxFor(t, time.Second), key(domain.SMS)).Val()
	if ttl <= 0 || ttl > time.Second+margin {
		t.Fatalf("expiry %s, want between 0 and one window", ttl)
	}
	time.Sleep(time.Second + margin + 100*time.Millisecond)
	if n := rdb.Exists(ctxFor(t, time.Second), key(domain.SMS), settingsKey(domain.SMS)).Val(); n != 0 {
		t.Fatalf("%d of the window's 2 keys are still stored one window after its last permit", n)
	}
}

// The time Wait returns is the permit's stored score, taken while the call ran. (Where the
// test's clock and Redis's are one clock, as on Linux, this cannot tell them apart; I12's
// test is TestTheLimiterSendsRedisNoClockTime.)
func TestAPermitIsStoredWithTheTimeItWasGranted(t *testing.T) {
	l, rdb := newLimiter(t, 5, 50*time.Millisecond)
	before := redisMicros(t, rdb)
	at := mustWait(t, l, domain.SMS)
	after := redisMicros(t, rdb)
	if at < before || at > after {
		t.Fatalf("permit stamped %d, outside the Redis times %d to %d around it", at, before, after)
	}
	stored := permits(t, rdb, domain.SMS)
	if len(stored) != 1 || int64(stored[0].Score) != at {
		t.Fatalf("stored %v, want one permit at %d", stored, at)
	}
}

// argsRecorder records the arguments of every command a client sends.
type argsRecorder struct {
	mu   sync.Mutex
	cmds [][]any
}

func (r *argsRecorder) DialHook(next redis.DialHook) redis.DialHook { return next }

func (r *argsRecorder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		r.mu.Lock()
		r.cmds = append(r.cmds, cmd.Args())
		r.mu.Unlock()
		return next(ctx, cmd)
	}
}

func (r *argsRecorder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		r.mu.Lock()
		for _, c := range cmds {
			r.cmds = append(r.cmds, c.Args())
		}
		r.mu.Unlock()
		return next(ctx, cmds)
	}
}

// I12: the worker's clock never reaches Redis. No argument the limiter sends is a time near
// now, in seconds, milliseconds, microseconds or nanoseconds, so the script can only use
// Redis's own clock.
func TestTheLimiterSendsRedisNoClockTime(t *testing.T) {
	e := testkit.Setup(t)
	rdb, err := redisclient.New(e.RedisURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rdb.Close() }()
	rec := &argsRecorder{}
	rdb.AddHook(rec)
	l, err := New(rdb, 1, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	mustAvailable(t, l, domain.SMS)
	mustWait(t, l, domain.SMS)
	_ = l.Wait(ctxFor(t, 50*time.Millisecond), domain.SMS) // refused: the waiting path too

	now := time.Now()
	units := []struct {
		name string
		now  float64
		day  float64
	}{
		{"s", float64(now.Unix()), 86400},
		{"ms", float64(now.UnixMilli()), 86400e3},
		{"µs", float64(now.UnixMicro()), 86400e6},
		{"ns", float64(now.UnixNano()), 86400e9},
	}
	scripts := 0
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, cmd := range rec.cmds {
		if name, _ := cmd[0].(string); name == "evalsha" || name == "eval" || name == "evalsha_ro" || name == "eval_ro" {
			scripts++
		}
		for _, a := range cmd[1:] {
			v, ok := number(a)
			if !ok {
				continue
			}
			for _, u := range units {
				if math.Abs(v-u.now) < u.day {
					t.Fatalf("%v sends %v, a time in %s", cmd[0], a, u.name)
				}
			}
		}
	}
	if scripts < 3 {
		t.Fatalf("recorded %d script calls; the hook did not see the limiter", scripts)
	}
}

func number(a any) (float64, bool) {
	switch v := a.(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case string:
		f, err := strconv.ParseFloat(v, 64)
		return f, err == nil
	}
	return 0, false
}

// An unreachable Redis is an error from both, never a permit or room.
func TestAnUnreachableRedisIsAnErrorNotAPermit(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	rdb, err := redisclient.New("redis://" + addr + "/0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rdb.Close() }()
	l, err := New(rdb, 100, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := l.Available(ctxFor(t, 2*time.Second), domain.SMS); err == nil {
		t.Fatalf("Available = %d with Redis unreachable", n)
	}
	if err := l.Wait(ctxFor(t, 2*time.Second), domain.SMS); err == nil {
		t.Fatal("Wait returned a permit with Redis unreachable")
	}
}

func TestNewRefusesALimitBelowOneAndAMarginThatIsNotPositive(t *testing.T) {
	for _, c := range []struct {
		limit  int
		margin time.Duration
	}{{0, 50 * time.Millisecond}, {-1, 50 * time.Millisecond}, {100, 0}, {100, -time.Millisecond}} {
		if _, err := New(nil, c.limit, c.margin); err == nil {
			t.Errorf("New(limit %d, margin %s) was accepted", c.limit, c.margin)
		}
	}
	if _, err := New(nil, 1, time.Microsecond); err != nil {
		t.Errorf("New(limit 1, margin 1µs): %v", err)
	}
}

// A Redis that accepts the connection and never answers is the limiter failing, not a wait
// that ran out: the breaker must learn of it (architecture §6, §7). Both end with the
// caller's deadline, so only ErrNoPermitInTime tells them apart.
func TestAHungRedisIsALimiterErrorNotAWaitThatRanOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	rdb, err := redisclient.New("redis://" + ln.Addr().String() + "/0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rdb.Close() }()
	l, err := New(rdb, 100, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	err = l.Wait(ctxFor(t, 200*time.Millisecond), domain.SMS)
	if err == nil || errors.Is(err, ErrNoPermitInTime) {
		t.Fatalf("Wait against a hung Redis: err = %v, want a limiter error", err)
	}
	if took := time.Since(began); took > time.Second {
		t.Fatalf("Wait returned %s after its 200 ms deadline", took)
	}
}

// The window is shared by every worker, but the limit and margin are each worker's settings.
// A worker whose settings differ from the window's is refused by both calls, with an error
// that is not ErrNoPermitInTime, so it claims nothing and its breaker opens; the window keeps
// the settings it was filled with (architecture §7).
func TestWorkersWithDifferentSettingsAreRefused(t *testing.T) {
	e := testkit.Setup(t)
	first, err := New(e.Redis, 5, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	mustWait(t, first, domain.SMS)
	for _, c := range []struct {
		name   string
		limit  int
		margin time.Duration
		own    domain.Channel // a channel whose window this worker fills itself
	}{{"another limit", 6, 50 * time.Millisecond, domain.Email}, {"another margin", 5, time.Millisecond, domain.Push}} {
		other, err := New(e.Redis, c.limit, c.margin)
		if err != nil {
			t.Fatal(err)
		}
		if n, err := other.Available(ctxFor(t, 5*time.Second), domain.SMS); err == nil {
			t.Errorf("%s: Available = %d, want an error", c.name, n)
		}
		if err := other.Wait(ctxFor(t, 2*time.Second), domain.SMS); err == nil || errors.Is(err, ErrNoPermitInTime) {
			t.Errorf("%s: Wait err = %v, want a limiter error", c.name, err)
		}
		// Another channel's window is not this one's.
		if err := other.Wait(ctxFor(t, 2*time.Second), c.own); err != nil {
			t.Errorf("%s: Wait on another channel: %v", c.name, err)
		}
	}
	if n := len(permits(t, e.Redis, domain.SMS)); n != 1 {
		t.Fatalf("%d permits stored, want 1", n)
	}
	if n := mustAvailable(t, first, domain.SMS); n != 4 {
		t.Fatalf("the first worker: Available = %d, want 4", n)
	}
}
