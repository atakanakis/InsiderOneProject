package store_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atakanakis/InsiderOneProject/internal/domain"
	"github.com/atakanakis/InsiderOneProject/internal/store"
	"github.com/atakanakis/InsiderOneProject/internal/testkit"
)

// The tests of the five queue operations of architecture §5, on a real Postgres.

const hour = time.Hour

// row describes a notification inserted by a test. Times are offsets from the database's
// now(), so every time in these tests comes from the Postgres clock, as in the queue.
type row struct {
	channel     domain.Channel
	priority    domain.Priority
	status      domain.Status
	readyIn     time.Duration  // ready_at = now() + readyIn; 0 or less is due
	lockedFor   *time.Duration // locked_until = now() + lockedFor; nil for NULL
	attempts    int
	maxAttempts int // 0 means 6
}

func waiting(readyIn time.Duration) row {
	return row{channel: domain.SMS, priority: domain.High, status: domain.Pending, readyIn: readyIn}
}

func held(lockedFor time.Duration, attempts, maxAttempts int) row {
	return row{channel: domain.SMS, priority: domain.High, status: domain.Processing,
		readyIn: -time.Minute, lockedFor: &lockedFor, attempts: attempts, maxAttempts: maxAttempts}
}

func addRow(t *testing.T, pool *pgxpool.Pool, r row) uuid.UUID {
	t.Helper()
	if r.channel == "" {
		r.channel = domain.SMS
	}
	if r.status == "" {
		r.status = domain.Pending
	}
	if r.maxAttempts == 0 {
		r.maxAttempts = 6
	}
	var locked *int64
	if r.lockedFor != nil {
		us := r.lockedFor.Microseconds()
		locked = &us
	}
	id := uuid.New()
	_, err := pool.Exec(ctx(t), `INSERT INTO notifications
		(id, recipient, channel, content, priority, status, attempts, max_attempts, ready_at, locked_until)
		VALUES ($1, 'r', $2, 'c', $3, $4, $5, $6,
		        now() + $7::bigint * interval '1 microsecond',
		        now() + $8::bigint * interval '1 microsecond')`,
		id, string(r.channel), int16(r.priority), string(r.status), r.attempts, r.maxAttempts,
		r.readyIn.Microseconds(), locked)
	if err != nil {
		t.Fatalf("insert row: %v", err)
	}
	return id
}

// addRows inserts n rows of one kind in one statement, so that they share one ready_at.
func addRows(t *testing.T, pool *pgxpool.Pool, n int, r row) {
	t.Helper()
	if r.maxAttempts == 0 {
		r.maxAttempts = 6
	}
	_, err := pool.Exec(ctx(t), `INSERT INTO notifications
		(id, recipient, channel, content, priority, status, attempts, max_attempts, ready_at)
		SELECT gen_random_uuid(), 'r', $2, 'c', $3, $4, $5, $6, now() + $7::bigint * interval '1 microsecond'
		FROM generate_series(1, $1)`,
		n, string(r.channel), int16(r.priority), string(r.status), r.attempts, r.maxAttempts,
		r.readyIn.Microseconds())
	if err != nil {
		t.Fatalf("insert rows: %v", err)
	}
}

type state struct {
	Status      domain.Status
	Attempts    int
	MaxAttempts int
}

func stateOf(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) state {
	t.Helper()
	var s state
	var status string
	if err := pool.QueryRow(ctx(t), `SELECT status, attempts, max_attempts FROM notifications WHERE id = $1`, id).
		Scan(&status, &s.Attempts, &s.MaxAttempts); err != nil {
		t.Fatalf("read row: %v", err)
	}
	s.Status = domain.Status(status)
	return s
}

func attemptRows(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) []int {
	t.Helper()
	rows, err := pool.Query(ctx(t), `SELECT attempt_number FROM delivery_attempts WHERE notification_id = $1 ORDER BY 1`, id)
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// bigPool is a pool with room for many concurrent callers.
func bigPool(t *testing.T, e *testkit.Env, params string) *pgxpool.Pool {
	t.Helper()
	pool, err := store.Connect(ctx(t), e.DatabaseURL+"&pool_max_conns=40"+params, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func ids(cs []store.Claimed) []uuid.UUID {
	out := make([]uuid.UUID, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

func claim(t *testing.T, q *store.Queue, ch domain.Channel, p domain.Priority, n int, lease time.Duration) []store.Claimed {
	t.Helper()
	got, err := q.Claim(ctx(t), ch, p, n, lease)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return got
}

// expire takes a row with a lease that has already ended and reaps it, the way a row comes
// back when its worker died: claim with a 1 µs lease, then Reap.
func claimAndLoseIt(t *testing.T, q *store.Queue, ch domain.Channel, p domain.Priority) store.Claimed {
	t.Helper()
	got := claim(t, q, ch, p, 1, time.Microsecond)
	if len(got) != 1 {
		t.Fatalf("claimed %d rows, want 1", len(got))
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := q.Reap(ctx(t), 100); err != nil {
		t.Fatalf("reap: %v", err)
	}
	return got[0]
}

var sent = store.Result{Status: domain.Sent, Outcome: "delivered", StatusCode: 202, Duration: 30 * time.Millisecond, ProviderMessageID: "m-1"}

// I2: many workers claim at once from the same channel and priority. Every row is taken
// exactly once, and each was claimed once (attempts = 1).
func TestConcurrentClaimsNeverTakeTheSameRow(t *testing.T) {
	e := testkit.Setup(t)
	const rows, workers, batch = 2000, 32, 7
	addRows(t, e.Pool, rows, waiting(-time.Second))
	q := store.NewQueue(bigPool(t, e, ""))

	var (
		mu    sync.Mutex
		taken = map[uuid.UUID]int{}
		wg    sync.WaitGroup
		start = make(chan struct{})
		errs  = make(chan error, workers)
	)
	for range workers {
		wg.Go(func() {
			<-start
			for {
				got, err := q.Claim(context.Background(), domain.SMS, domain.High, batch, hour)
				if err != nil {
					errs <- err
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, c := range got {
					taken[c.ID]++
				}
				mu.Unlock()
			}
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("claim: %v", err)
	}
	twice := 0
	for _, n := range taken {
		if n > 1 {
			twice++
		}
	}
	if twice > 0 {
		t.Errorf("%d rows were returned to more than one claim", twice)
	}
	if len(taken) != rows {
		t.Errorf("%d distinct rows claimed, want %d", len(taken), rows)
	}
	var reclaimed int
	if err := e.Pool.QueryRow(ctx(t), `SELECT count(*) FROM notifications WHERE attempts <> 1 OR status <> 'processing'`).Scan(&reclaimed); err != nil {
		t.Fatal(err)
	}
	if reclaimed != 0 {
		t.Errorf("%d rows are not processing with exactly one attempt", reclaimed)
	}
}

// I2 and §3: only a waiting row is claimed. A processing row is held by a worker, whether or
// not its lease has ended (the reaper returns it first); a final row is never claimed.
func TestClaimTakesExactlyTheClaimableStatuses(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)
	want := map[uuid.UUID]domain.Status{}
	for _, s := range domain.Statuses {
		r := waiting(-time.Minute)
		r.status = s
		if s == domain.Processing {
			live, ended := hour, -time.Minute
			r.lockedFor = &live
			addRow(t, e.Pool, r)
			r.lockedFor = &ended
		}
		id := addRow(t, e.Pool, r)
		if s.IsClaimable() {
			want[id] = s
		}
	}
	got := claim(t, q, domain.SMS, domain.High, 100, hour)
	if len(got) != len(want) {
		t.Errorf("claimed %d rows, want %d (one per claimable status %v)", len(got), len(want), domain.Claimable)
	}
	for _, c := range got {
		if _, ok := want[c.ID]; !ok {
			t.Errorf("claimed row %s, which is not waiting", c.ID)
		}
	}
}

// §5: a row whose ready_at is in the future is not claimed, whatever its status.
func TestClaimReturnsOnlyDueRows(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)
	due := addRow(t, e.Pool, waiting(-time.Millisecond))
	for _, s := range domain.Claimable {
		r := waiting(time.Minute)
		r.status = s
		addRow(t, e.Pool, r)
	}
	got := claim(t, q, domain.SMS, domain.High, 10, hour)
	if !slices.Equal(ids(got), []uuid.UUID{due}) {
		t.Errorf("claimed %v, want only the due row %s", ids(got), due)
	}
}

// §5: a claim is for one channel and one priority.
func TestClaimTakesOnlyItsChannelAndPriority(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)
	kind := map[uuid.UUID][2]string{}
	for _, ch := range domain.Channels {
		for _, p := range domain.Priorities {
			r := waiting(-time.Second)
			r.channel, r.priority = ch, p
			kind[addRow(t, e.Pool, r)] = [2]string{string(ch), p.String()}
		}
	}
	for _, ch := range domain.Channels {
		for _, p := range domain.Priorities {
			got := claim(t, q, ch, p, 10, hour)
			if len(got) != 1 || kind[got[0].ID] != [2]string{string(ch), p.String()} ||
				got[0].Channel != ch || got[0].Priority != p {
				t.Errorf("claim of %s/%s returned %d rows: %+v", ch, p, len(got), got)
			}
		}
	}
}

// §5: a claim takes the n rows that have been due longest, and returns them in ready_at order.
func TestClaimReturnsTheEarliestRowsInReadyAtOrder(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)
	byAge := make([]uuid.UUID, 30) // byAge[i] has been due for 30-i seconds
	for _, i := range rand.Perm(30) {
		byAge[i] = addRow(t, e.Pool, waiting(-time.Duration(30-i)*time.Second))
	}
	got := claim(t, q, domain.SMS, domain.High, 10, hour)
	if !slices.Equal(ids(got), byAge[:10]) {
		t.Errorf("claimed %v,\nwant the 10 oldest in order %v", ids(got), byAge[:10])
	}
	if !slices.IsSortedFunc(got, func(a, b store.Claimed) int { return a.ReadyAt.Compare(b.ReadyAt) }) {
		t.Error("the claimed rows are not in ready_at order")
	}
}

// §5: the claim's order is in its SQL, not only in the index the planner happens to choose.
// With index scans switched off for the session, the rows are read in table order (newest
// first here), and the claim must still take the oldest.
func TestClaimTakesTheEarliestDueRowsWithoutTheIndex(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(bigPool(t, e, "&enable_indexscan=off&enable_bitmapscan=off&enable_indexonlyscan=off"))
	var oldest []uuid.UUID
	for i := range 20 {
		id := addRow(t, e.Pool, waiting(-time.Duration(i+1)*time.Second)) // inserted newest first
		if i >= 15 {
			oldest = append([]uuid.UUID{id}, oldest...)
		}
	}
	got := claim(t, q, domain.SMS, domain.High, 5, hour)
	if !slices.Equal(ids(got), oldest) {
		t.Errorf("claimed %v, want the 5 oldest %v", ids(got), oldest)
	}
}

// §6 and I9 at the queue: the dispatcher claims each priority in turn, highest first, and
// a high-priority row that became due last is still taken before older rows of lower
// priorities.
func TestClaimsInPriorityTurnTakeHigherPrioritiesFirst(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)
	for i, p := range []domain.Priority{domain.Low, domain.Normal, domain.High} { // oldest first
		r := waiting(-time.Duration(3-i) * time.Minute)
		r.priority = p
		addRows(t, e.Pool, 4, r)
	}
	var order []domain.Priority
	for room := 6; ; room = 6 {
		claimed := 0
		for _, p := range domain.Priorities {
			for _, c := range claim(t, q, domain.SMS, p, room-claimed, hour) {
				order = append(order, c.Priority)
				claimed++
			}
			if claimed == room {
				break
			}
		}
		if claimed == 0 {
			break
		}
	}
	want := []domain.Priority{0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2}
	if !slices.Equal(order, want) {
		t.Errorf("claimed in priority order %v, want %v", order, want)
	}
}

// §5, I7, I12: a claim adds one attempt, and the lease ends exactly `lease` after the
// database's now() of the claim.
func TestClaimSetsTheLeaseOnTheDatabaseClock(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)
	id := addRow(t, e.Pool, row{status: domain.Retrying, readyIn: -time.Second, attempts: 2})
	const lease = 61*time.Second + 250*time.Millisecond
	got := claim(t, q, domain.SMS, domain.High, 1, lease)
	if len(got) != 1 || got[0].ID != id || got[0].Attempt != 3 {
		t.Fatalf("claim returned %+v, want the row with attempt 3", got)
	}
	var leaseUS int64
	var lockedUntil time.Time
	if err := e.Pool.QueryRow(ctx(t), `SELECT locked_until, `+us("locked_until - updated_at")+`
		FROM notifications WHERE id = $1`, id).Scan(&lockedUntil, &leaseUS); err != nil {
		t.Fatal(err)
	}
	if leaseInDB := time.Duration(leaseUS) * time.Microsecond; leaseInDB != lease {
		t.Errorf("locked_until - now() of the claim = %v, want %v", leaseInDB, lease)
	}
	if !got[0].LockedUntil.Equal(lockedUntil) {
		t.Errorf("claim returned locked_until %v, the row has %v", got[0].LockedUntil, lockedUntil)
	}
	if s := stateOf(t, e.Pool, id); s.Status != domain.Processing || s.Attempts != 3 {
		t.Errorf("row after the claim: %+v, want processing with 3 attempts", s)
	}
}

// The arguments of Claim are checked before anything is written.
func TestClaimRefusesAnEmptyLeaseAndAsksNothingForZeroRows(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)
	id := addRow(t, e.Pool, waiting(-time.Second))
	if _, err := q.Claim(ctx(t), domain.SMS, domain.High, 1, 0); err == nil {
		t.Error("a claim with a lease of 0 was accepted")
	}
	if got, err := q.Claim(ctx(t), domain.SMS, domain.High, 0, hour); err != nil || len(got) != 0 {
		t.Errorf("a claim of 0 rows returned %d rows, %v", len(got), err)
	}
	if s := stateOf(t, e.Pool, id); s.Status != domain.Pending || s.Attempts != 0 {
		t.Errorf("row after refused claims: %+v, want it untouched", s)
	}
}

// I3: a result of an attempt that is not the row's current attempt changes nothing but the
// history. Here the lease of attempt 1 ended, the reaper returned the row, and attempt 2
// holds it when attempt 1's result arrives.
func TestFinishFromAStaleAttemptChangesNothing(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)
	id := addRow(t, e.Pool, waiting(-time.Second))
	first := claimAndLoseIt(t, q, domain.SMS, domain.High)
	second := claim(t, q, domain.SMS, domain.High, 1, hour)
	if len(second) != 1 || second[0].Attempt != 2 {
		t.Fatalf("second claim: %+v, want attempt 2", second)
	}
	status, changed, err := q.Finish(ctx(t), id, first.Attempt, sent)
	if err != nil || changed {
		t.Fatalf("finish of the stale attempt: changed %v (%s), %v; want stale", changed, status, err)
	}
	if s := stateOf(t, e.Pool, id); s.Status != domain.Processing || s.Attempts != 2 {
		t.Errorf("row after the stale result: %+v, want processing with 2 attempts", s)
	}
	if got := attemptRows(t, e.Pool, id); !slices.Equal(got, []int{1}) {
		t.Errorf("attempt rows %v, want [1]: the stale result is kept as history", got)
	}
	status, changed, err = q.Finish(ctx(t), id, 2, sent)
	if err != nil || !changed || status != domain.Sent {
		t.Errorf("finish of the current attempt: %s, %v, %v; want sent", status, changed, err)
	}
}

// I3: a result for a row that is no longer processing changes nothing, even when it carries
// the row's attempt number: the reaper keeps attempts, so a returned row still has the
// number of the attempt that lost it. A final status stays final.
func TestFinishOnARowThatIsNoLongerProcessingChangesNothing(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)

	returned := addRow(t, e.Pool, waiting(-time.Second))
	lost := claimAndLoseIt(t, q, domain.SMS, domain.High)
	if _, changed, err := q.Finish(ctx(t), returned, lost.Attempt, sent); err != nil || changed {
		t.Errorf("result for a reaped row: changed %v, %v; want stale", changed, err)
	}
	if s := stateOf(t, e.Pool, returned); s.Status != domain.Pending || s.Attempts != 1 {
		t.Errorf("reaped row after a late result: %+v, want pending with 1 attempt", s)
	}

	r := waiting(-time.Second)
	r.priority = domain.Normal
	final := addRow(t, e.Pool, r)
	c := claim(t, q, domain.SMS, domain.Normal, 1, hour)
	failed := store.Result{Status: domain.Failed, Outcome: "rejected", StatusCode: 400, Error: "bad recipient"}
	if status, changed, err := q.Finish(ctx(t), final, c[0].Attempt, failed); err != nil || !changed || status != domain.Failed {
		t.Fatalf("first result: %s, %v, %v", status, changed, err)
	}
	if _, changed, err := q.Finish(ctx(t), final, c[0].Attempt, sent); err != nil || changed {
		t.Errorf("second, different result of the same attempt: changed %v, %v; want stale", changed, err)
	}
	if s := stateOf(t, e.Pool, final); s.Status != domain.Failed {
		t.Errorf("final row after a late result: %s, want failed", s.Status)
	}
}

// I3: the result write is retried when its reply is lost (§6). The repeat adds no second
// attempt row and is not an error.
func TestRepeatedFinishAddsNoSecondAttemptRow(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)
	id := addRow(t, e.Pool, waiting(-time.Second))
	c := claim(t, q, domain.SMS, domain.High, 1, hour)
	for i, want := range []bool{true, false} {
		status, changed, err := q.Finish(ctx(t), id, c[0].Attempt, sent)
		if err != nil || changed != want {
			t.Errorf("finish %d: %s, changed %v, %v; want changed %v and no error", i+1, status, changed, err, want)
		}
	}
	if got := attemptRows(t, e.Pool, id); !slices.Equal(got, []int{1}) {
		t.Errorf("attempt rows %v, want [1]", got)
	}
}

// §5, I12: each result is stored with times from the database clock: Go passes durations.
func TestFinishStoresEachResultOnTheDatabaseClock(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)
	read := func(id uuid.UUID) (status string, retryIn, sentLag time.Duration, msgID, lastErr *string, locked bool) {
		t.Helper()
		var r, s int64
		var sentNull bool
		if err := e.Pool.QueryRow(ctx(t), `SELECT status, `+us("ready_at - updated_at")+`,
				coalesce(`+us("updated_at - sent_at")+`, 0), sent_at IS NULL,
				provider_message_id, last_error, locked_until IS NOT NULL
			FROM notifications WHERE id = $1`, id).Scan(&status, &r, &s, &sentNull, &msgID, &lastErr, &locked); err != nil {
			t.Fatal(err)
		}
		if sentNull {
			s = -1
		}
		return status, time.Duration(r) * time.Microsecond, time.Duration(s) * time.Microsecond, msgID, lastErr, locked
	}

	results := []store.Result{
		sent,
		{Status: domain.Retrying, RetryIn: 4*time.Second + 500*time.Millisecond, Outcome: "server error", StatusCode: 503, Error: "503 Service Unavailable", Duration: 1500 * time.Millisecond},
		{Status: domain.Failed, Outcome: "rejected", StatusCode: 422, Error: "content too long", Duration: 12 * time.Millisecond},
		{Status: domain.Retrying, RetryIn: 2 * time.Second, Outcome: "no response", Error: "connection reset", Duration: 15 * time.Second},
	}
	for _, r := range results {
		id := addRow(t, e.Pool, waiting(-time.Second))
		c := claim(t, q, domain.SMS, domain.High, 1, hour)
		status, changed, err := q.Finish(ctx(t), id, c[0].Attempt, r)
		if err != nil || !changed || status != r.Status {
			t.Fatalf("finish %s: %s, %v, %v", r.Status, status, changed, err)
		}
		st, retryIn, sentLag, msgID, lastErr, locked := read(id)
		if st != string(r.Status) || locked {
			t.Errorf("%s: status %s, lease still set %v", r.Status, st, locked)
		}
		switch r.Status {
		case domain.Sent:
			if sentLag != 0 || msgID == nil || *msgID != r.ProviderMessageID || lastErr != nil {
				t.Errorf("sent: sent_at %v before updated_at, message id %v, last error %v", sentLag, msgID, lastErr)
			}
		case domain.Retrying:
			if retryIn != r.RetryIn || lastErr == nil || *lastErr != r.Error {
				t.Errorf("retrying: ready_at = now() + %v, want + %v; last error %v", retryIn, r.RetryIn, lastErr)
			}
		case domain.Failed:
			if lastErr == nil || *lastErr != r.Error {
				t.Errorf("failed: last error %v, want %q", lastErr, r.Error)
			}
		}

		var startedUS int64
		var durationMS int
		var code *int
		var outcome string
		var errText *string
		if err := e.Pool.QueryRow(ctx(t), `SELECT `+us("n.updated_at - a.started_at")+`,
				a.duration_ms, a.status_code, a.outcome, a.error
			FROM delivery_attempts a JOIN notifications n ON n.id = a.notification_id
			WHERE a.notification_id = $1 AND a.attempt_number = $2`, id, c[0].Attempt).
			Scan(&startedUS, &durationMS, &code, &outcome, &errText); err != nil {
			t.Fatalf("%s: attempt row: %v", r.Status, err)
		}
		startedLag := time.Duration(startedUS) * time.Microsecond
		if startedLag != r.Duration || durationMS != int(r.Duration.Milliseconds()) {
			t.Errorf("%s: attempt started %v before the result, took %d ms; want %v", r.Status, startedLag, durationMS, r.Duration)
		}
		if (r.StatusCode == 0) != (code == nil) || (code != nil && *code != r.StatusCode) {
			t.Errorf("%s: status code %v, want %d (NULL for no response)", r.Status, code, r.StatusCode)
		}
		if outcome != r.Outcome || (r.Error == "") != (errText == nil) {
			t.Errorf("%s: outcome %q, error %v", r.Status, outcome, errText)
		}
	}
}

// §5: Finish stores only the three results of an attempt; anything else is refused before
// any write.
func TestFinishRefusesAStatusThatIsNotAResult(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)
	id := addRow(t, e.Pool, waiting(-time.Second))
	c := claim(t, q, domain.SMS, domain.High, 1, hour)
	for _, s := range []domain.Status{domain.Scheduled, domain.Pending, domain.Processing, domain.Cancelled, "lost"} {
		if _, _, err := q.Finish(ctx(t), id, c[0].Attempt, store.Result{Status: s, Outcome: "x"}); err == nil {
			t.Errorf("finish with status %q was accepted", s)
		}
	}
	if _, _, err := q.Finish(ctx(t), id, c[0].Attempt, store.Result{Status: domain.Sent}); err == nil {
		t.Error("finish without an outcome was accepted")
	}
	if s := stateOf(t, e.Pool, id); s.Status != domain.Processing || len(attemptRows(t, e.Pool, id)) != 0 {
		t.Errorf("row after refused results: %+v, attempt rows %v; want it untouched", s, attemptRows(t, e.Pool, id))
	}
}

// §5, §6: "retry" on the last allowed attempt is stored as failed, decided from the row's own
// counters.
func TestARetryOnTheLastAttemptIsStoredAsFailed(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)
	retry := store.Result{Status: domain.Retrying, RetryIn: time.Second, Outcome: "server error", StatusCode: 500}
	for _, tc := range []struct {
		attempts, max int
		want          domain.Status
	}{{4, 6, domain.Retrying}, {5, 6, domain.Failed}, {0, 1, domain.Failed}} {
		id := addRow(t, e.Pool, row{status: domain.Retrying, readyIn: -time.Second, attempts: tc.attempts, maxAttempts: tc.max})
		c := claim(t, q, domain.SMS, domain.High, 1, hour)
		status, changed, err := q.Finish(ctx(t), id, c[0].Attempt, retry)
		if err != nil || !changed || status != tc.want {
			t.Errorf("retry on attempt %d of %d: %s, %v, %v; want %s", c[0].Attempt, tc.max, status, changed, err, tc.want)
		}
		if s := stateOf(t, e.Pool, id); s.Status != tc.want {
			t.Errorf("row of attempt %d of %d is %s, want %s", c[0].Attempt, tc.max, s.Status, tc.want)
		}
	}
}

// §5 and rule 10: the error text and the provider's message id are bounded, and any bytes
// are stored: Postgres refuses invalid UTF-8 and NUL in text, and a result write that fails
// forever would make the row be sent again after its lease.
func TestFinishStoresAnyErrorTextWithinItsBound(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)
	hostile := "\xff\xfe" + strings.Repeat("é\x00", 3000) + "\xc3"
	for _, status := range []domain.Status{domain.Retrying, domain.Sent} {
		id := addRow(t, e.Pool, waiting(-time.Second))
		c := claim(t, q, domain.SMS, domain.High, 1, hour)
		r := store.Result{Status: status, RetryIn: time.Second, Outcome: "server error", Error: hostile, ProviderMessageID: hostile}
		if _, changed, err := q.Finish(ctx(t), id, c[0].Attempt, r); err != nil || !changed {
			t.Fatalf("%s with hostile text: changed %v, %v", status, changed, err)
		}
		var lastErr, attemptErr, msgID *string
		if err := e.Pool.QueryRow(ctx(t), `SELECT n.last_error, a.error, n.provider_message_id
			FROM notifications n JOIN delivery_attempts a ON a.notification_id = n.id WHERE n.id = $1`, id).
			Scan(&lastErr, &attemptErr, &msgID); err != nil {
			t.Fatal(err)
		}
		for name, v := range map[string]*string{"last_error": lastErr, "attempt error": attemptErr} {
			if v == nil || len(*v) == 0 || len(*v) > store.MaxErrorBytes {
				t.Errorf("%s: %s has %d bytes, want 1 to %d", status, name, lenOf(v), store.MaxErrorBytes)
			}
		}
		if status == domain.Sent && (msgID == nil || len(*msgID) == 0 || len(*msgID) > store.MaxProviderMessageIDBytes) {
			t.Errorf("provider message id has %d bytes, want 1 to %d", lenOf(msgID), store.MaxProviderMessageIDBytes)
		}
	}
}

// us is SQL for an interval in whole microseconds.
func us(interval string) string {
	return "(extract(epoch FROM " + interval + ") * 1000000)::bigint"
}

// §5, §8, rule 10: the text bounds at their edge. Text of exactly the bound is stored whole;
// one byte more is cut to the bound; a two-byte character across the bound is not split.
func TestFinishStoresTextAtItsBound(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)
	type tc struct {
		name           string
		errText, msgID string
		wantErr        int // stored bytes of the error and the outcome
		wantMsgID      int
	}
	cases := []tc{
		{"exactly the bound", strings.Repeat("a", store.MaxErrorBytes), strings.Repeat("m", store.MaxProviderMessageIDBytes),
			store.MaxErrorBytes, store.MaxProviderMessageIDBytes},
		{"one byte over", strings.Repeat("a", store.MaxErrorBytes+1), strings.Repeat("m", store.MaxProviderMessageIDBytes+1),
			store.MaxErrorBytes, store.MaxProviderMessageIDBytes},
		{"a character across the bound", strings.Repeat("a", store.MaxErrorBytes-1) + "é", strings.Repeat("m", store.MaxProviderMessageIDBytes-1) + "é",
			store.MaxErrorBytes - 1, store.MaxProviderMessageIDBytes - 1},
	}
	for _, c := range cases {
		id := addRow(t, e.Pool, waiting(-time.Second))
		cl := claim(t, q, domain.SMS, domain.High, 1, hour)
		r := store.Result{Status: domain.Sent, Outcome: c.errText, Error: c.errText, ProviderMessageID: c.msgID}
		if _, changed, err := q.Finish(ctx(t), id, cl[0].Attempt, r); err != nil || !changed {
			t.Fatalf("%s: changed %v, %v", c.name, changed, err)
		}
		var lastErr, outcome, msgID string
		if err := e.Pool.QueryRow(ctx(t), `SELECT n.last_error, a.outcome, n.provider_message_id
			FROM notifications n JOIN delivery_attempts a ON a.notification_id = n.id WHERE n.id = $1`, id).
			Scan(&lastErr, &outcome, &msgID); err != nil {
			t.Fatal(err)
		}
		if len(lastErr) != c.wantErr || len(outcome) != c.wantErr || len(msgID) != c.wantMsgID {
			t.Errorf("%s: stored error %d, outcome %d, message id %d bytes; want %d, %d, %d",
				c.name, len(lastErr), len(outcome), len(msgID), c.wantErr, c.wantErr, c.wantMsgID)
		}
		if !strings.HasPrefix(c.errText, lastErr) || !strings.HasPrefix(c.msgID, msgID) {
			t.Errorf("%s: the stored text is not a prefix of the input", c.name)
		}
	}
}

func lenOf(s *string) int {
	if s == nil {
		return -1
	}
	return len(*s)
}

// I4: Reap returns rows whose lease has ended to pending, or to failed at their last attempt;
// it leaves rows with a live lease, keeps attempts and ready_at, and takes a bounded batch.
func TestReapReturnsExpiredRowsToPendingOrFailed(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)
	toPending := addRow(t, e.Pool, held(-time.Second, 2, 6))
	toFailed := addRow(t, e.Pool, held(-time.Second, 6, 6))
	live := addRow(t, e.Pool, held(time.Minute, 1, 6))
	var readyBefore time.Time
	if err := e.Pool.QueryRow(ctx(t), `SELECT ready_at FROM notifications WHERE id = $1`, toPending).Scan(&readyBefore); err != nil {
		t.Fatal(err)
	}

	got, err := q.Reap(ctx(t), 100)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(got, func(a, b store.Change) int { return strings.Compare(string(a.Status), string(b.Status)) })
	want := []store.Change{{ID: toFailed, Status: domain.Failed}, {ID: toPending, Status: domain.Pending}}
	if !slices.Equal(got, want) {
		t.Errorf("reap returned %+v, want %+v", got, want)
	}
	for id, w := range map[uuid.UUID]state{
		toPending: {domain.Pending, 2, 6}, toFailed: {domain.Failed, 6, 6}, live: {domain.Processing, 1, 6},
	} {
		if s := stateOf(t, e.Pool, id); s != w {
			t.Errorf("row after reap: %+v, want %+v", s, w)
		}
	}
	var readyAfter time.Time
	var lockCleared bool
	if err := e.Pool.QueryRow(ctx(t), `SELECT ready_at, locked_until IS NULL FROM notifications WHERE id = $1`, toPending).Scan(&readyAfter, &lockCleared); err != nil {
		t.Fatal(err)
	}
	if !readyAfter.Equal(readyBefore) || !lockCleared {
		t.Errorf("reaped row: ready_at %v (was %v), lease cleared %v", readyAfter, readyBefore, lockCleared)
	}
	// The lost attempt leaves no attempt row (only Finish writes one); last_error says why.
	for _, id := range []uuid.UUID{toPending, toFailed} {
		var lastErr *string
		if err := e.Pool.QueryRow(ctx(t), `SELECT last_error FROM notifications WHERE id = $1`, id).Scan(&lastErr); err != nil {
			t.Fatal(err)
		}
		if lastErr == nil || *lastErr != "lease expired" || len(attemptRows(t, e.Pool, id)) != 0 {
			t.Errorf("reaped row: last_error %v, attempt rows %v; want \"lease expired\" and none", lastErr, attemptRows(t, e.Pool, id))
		}
	}

	for range 30 {
		addRow(t, e.Pool, held(-time.Second, 1, 6))
	}
	if got, err := q.Reap(ctx(t), 10); err != nil || len(got) != 10 {
		t.Errorf("reap with a limit of 10 returned %d rows, %v", len(got), err)
	}
	if _, err := q.Reap(ctx(t), 0); err == nil {
		t.Error("reap with a limit of 0 was accepted")
	}
}

// I4: every worker runs the reaper. Reapers at once return each row once and do not wait for
// each other.
func TestTwoReapersAtOnceReturnEachRowOnce(t *testing.T) {
	e := testkit.Setup(t)
	const rows, reapers = 1000, 8
	_, err := e.Pool.Exec(ctx(t), `INSERT INTO notifications
		(id, recipient, channel, content, priority, status, attempts, max_attempts, ready_at, locked_until)
		SELECT gen_random_uuid(), 'r', 'sms', 'c', 0, 'processing', 1, 6, now(), now() - interval '1 second'
		FROM generate_series(1, $1)`, rows)
	if err != nil {
		t.Fatal(err)
	}
	q := store.NewQueue(bigPool(t, e, ""))
	var (
		mu    sync.Mutex
		seen  = map[uuid.UUID]int{}
		wg    sync.WaitGroup
		start = make(chan struct{})
		errs  = make(chan error, reapers)
	)
	for range reapers {
		wg.Go(func() {
			<-start
			for {
				got, err := q.Reap(context.Background(), 25)
				if err != nil {
					errs <- err
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, c := range got {
					seen[c.ID]++
				}
				mu.Unlock()
			}
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("reap: %v", err)
	}
	twice := 0
	for _, n := range seen {
		if n > 1 {
			twice++
		}
	}
	if twice > 0 || len(seen) != rows {
		t.Errorf("%d rows reaped, %d of them more than once; want %d, each once", len(seen), twice, rows)
	}
}

// lockRows holds row locks on the rows that query selects, in a transaction of its own, the
// way a Finish, a Cancel or another claim holds them for a moment. It returns the locked ids.
func lockRows(t *testing.T, pool *pgxpool.Pool, query string) []uuid.UUID {
	t.Helper()
	tx, err := pool.Begin(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	rows, err := tx.Query(ctx(t), query)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	return locked
}

// §5, I2: SKIP LOCKED, not only the lock. A claim that meets rows locked by another
// transaction takes the next due rows at once; it neither waits nor comes back empty.
func TestClaimSkipsRowsLockedByAnotherTransaction(t *testing.T) {
	e := testkit.Setup(t)
	for i := range 20 {
		addRow(t, e.Pool, waiting(-time.Duration(20-i)*time.Second))
	}
	locked := lockRows(t, e.Pool, `SELECT id FROM notifications ORDER BY ready_at LIMIT 10 FOR UPDATE`)
	q := store.NewQueue(e.Pool).WithTimeout(2 * time.Second)
	began := time.Now()
	got, err := q.Claim(ctx(t), domain.SMS, domain.High, 10, hour)
	took := time.Since(began)
	if err != nil || len(got) != 10 {
		t.Fatalf("claim next to locked rows: %d rows after %v, %v; want the 10 unlocked rows at once", len(got), took, err)
	}
	for _, c := range got {
		if slices.Contains(locked, c.ID) {
			t.Errorf("claim returned row %s, which another transaction holds", c.ID)
		}
	}
	if took > time.Second {
		t.Errorf("the claim took %v: it waited for the locked rows", took)
	}
}

// §5, I4: the same for the reaper, which every worker runs.
func TestReapSkipsRowsLockedByAnotherTransaction(t *testing.T) {
	e := testkit.Setup(t)
	for range 15 {
		addRow(t, e.Pool, held(-time.Second, 1, 6))
	}
	locked := lockRows(t, e.Pool, `SELECT id FROM notifications LIMIT 5 FOR UPDATE`)
	q := store.NewQueue(e.Pool).WithTimeout(2 * time.Second)
	began := time.Now()
	got, err := q.Reap(ctx(t), 10)
	took := time.Since(began)
	if err != nil || len(got) != 10 {
		t.Fatalf("reap next to locked rows: %d rows after %v, %v; want the 10 unlocked rows at once", len(got), took, err)
	}
	for _, c := range got {
		if slices.Contains(locked, c.ID) {
			t.Errorf("reap returned row %s, which another transaction holds", c.ID)
		}
	}
	if took > time.Second {
		t.Errorf("the reap took %v: it waited for the locked rows", took)
	}
}

// §5: only a waiting row can be cancelled. Cancel tells a missing row from a row in another
// status, and says which status it found.
func TestCancelOnlyFromWaitingStatuses(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)
	for _, s := range domain.Statuses {
		r := waiting(time.Minute)
		r.status = s
		id := addRow(t, e.Pool, r)
		got, err := q.Cancel(ctx(t), id)
		if s.IsClaimable() {
			if err != nil || got != domain.Cancelled {
				t.Errorf("cancel from %s: %s, %v; want cancelled", s, got, err)
			}
			continue
		}
		if !errors.Is(err, store.ErrWrongStatus) || got != s {
			t.Errorf("cancel from %s: %s, %v; want ErrWrongStatus and the status %s", s, got, err, s)
		}
		if st := stateOf(t, e.Pool, id); st.Status != s {
			t.Errorf("row in %s became %s", s, st.Status)
		}
	}
	if _, err := q.Cancel(ctx(t), uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("cancel of a missing row: %v, want ErrNotFound", err)
	}
}

// §5: a cancelled row stays cancelled. Its old attempt's result, the reaper, a claim and a
// requeue all leave it alone.
func TestACancelledRowCannotBeRevived(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)
	id := addRow(t, e.Pool, waiting(-time.Second))
	lost := claimAndLoseIt(t, q, domain.SMS, domain.High)
	if _, err := q.Cancel(ctx(t), id); err != nil {
		t.Fatalf("cancel of the returned row: %v", err)
	}
	if _, changed, err := q.Finish(ctx(t), id, lost.Attempt, sent); err != nil || changed {
		t.Errorf("late result: changed %v, %v; want stale", changed, err)
	}
	if got, err := q.Reap(ctx(t), 100); err != nil || len(got) != 0 {
		t.Errorf("reap: %v, %v; want nothing", got, err)
	}
	if got := claim(t, q, domain.SMS, domain.High, 10, hour); len(got) != 0 {
		t.Errorf("claim took the cancelled row: %v", ids(got))
	}
	if _, err := q.Requeue(ctx(t), id, 6); !errors.Is(err, store.ErrWrongStatus) {
		t.Errorf("requeue of a cancelled row: %v, want ErrWrongStatus", err)
	}
	if s := stateOf(t, e.Pool, id); s.Status != domain.Cancelled || s.Attempts != 1 {
		t.Errorf("row: %+v, want cancelled with 1 attempt", s)
	}
}

// I7, §5: requeue moves a failed row to pending, due now, keeps attempts, and gives it
// `budget` more attempts. Only a failed row can be requeued.
func TestRequeueRaisesMaxAttemptsByTheBudget(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)
	id := addRow(t, e.Pool, row{status: domain.Failed, readyIn: time.Hour, attempts: 6, maxAttempts: 6})
	got, err := q.Requeue(ctx(t), id, 4)
	if err != nil || got != domain.Pending {
		t.Fatalf("requeue: %s, %v", got, err)
	}
	if s := stateOf(t, e.Pool, id); s != (state{domain.Pending, 6, 10}) {
		t.Errorf("row after requeue: %+v, want pending, 6 attempts, max 10", s)
	}
	c := claim(t, q, domain.SMS, domain.High, 1, hour)
	if len(c) != 1 || c[0].Attempt != 7 || c[0].MaxAttempts != 10 {
		t.Errorf("claim after requeue: %+v, want the row due now with attempt 7 of 10", c)
	}

	for _, s := range domain.Statuses {
		if s == domain.Failed {
			continue
		}
		r := waiting(time.Minute)
		r.status = s
		other := addRow(t, e.Pool, r)
		if got, err := q.Requeue(ctx(t), other, 4); !errors.Is(err, store.ErrWrongStatus) || got != s {
			t.Errorf("requeue from %s: %s, %v; want ErrWrongStatus", s, got, err)
		}
	}
	if _, err := q.Requeue(ctx(t), uuid.New(), 4); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("requeue of a missing row: %v, want ErrNotFound", err)
	}
	failed := addRow(t, e.Pool, row{status: domain.Failed, attempts: 6, maxAttempts: 6})
	if _, err := q.Requeue(ctx(t), failed, 0); err == nil {
		t.Error("requeue with a budget of 0 was accepted")
	}
}

// I7: attempts only goes up. A random sequence of all five operations on six rows: each
// claim returns the previous count plus one, and nothing else changes the count. A row that
// becomes final (sent or cancelled) is replaced by a new one, so the queue stays busy for
// the whole run. The seed changes on every run and is logged.
func TestAttemptsNeverGoDown(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)
	seed := uint64(time.Now().UnixNano())
	t.Logf("seed %d", seed)
	rng := rand.New(rand.NewPCG(seed, 2026))
	fresh := func() uuid.UUID { return addRow(t, e.Pool, row{status: domain.Pending, maxAttempts: 3}) }
	var rows []uuid.UUID
	for range 6 {
		rows = append(rows, fresh())
	}
	retry := store.Result{Status: domain.Retrying, Outcome: "server error", StatusCode: 503} // due again at once
	results := []store.Result{sent, {Status: domain.Failed, Outcome: "rejected", StatusCode: 400}, retry, retry, retry, retry}
	last := map[uuid.UUID]int{} // the attempt number each row's last claim handed out
	var claims, finished, reaped, requeued, cancelled int
	for step := range 600 {
		id := rows[rng.IntN(len(rows))]
		switch rng.IntN(6) {
		case 0, 1:
			lease := time.Microsecond // lost at once: the reaper returns it
			if rng.IntN(4) == 0 {
				lease = hour
			}
			for _, c := range claim(t, q, domain.SMS, domain.High, rng.IntN(3)+1, lease) {
				if c.Attempt != last[c.ID]+1 {
					t.Fatalf("step %d: claim returned attempt %d after %d", step, c.Attempt, last[c.ID])
				}
				last[c.ID] = c.Attempt
				claims++
			}
		case 2:
			if last[id] == 0 {
				continue
			}
			attempt := max(1, last[id]-rng.IntN(2)) // the current attempt, or a stale one
			_, changed, err := q.Finish(ctx(t), id, attempt, results[rng.IntN(len(results))])
			if err != nil {
				t.Fatalf("step %d: finish: %v", step, err)
			}
			if changed {
				finished++
			}
		case 3:
			time.Sleep(time.Millisecond)
			got, err := q.Reap(ctx(t), 10)
			if err != nil {
				t.Fatalf("step %d: reap: %v", step, err)
			}
			reaped += len(got)
		case 4:
			if _, err := q.Requeue(ctx(t), id, 2); err == nil {
				requeued++
			} else if !errors.Is(err, store.ErrWrongStatus) {
				t.Fatalf("step %d: requeue: %v", step, err)
			}
		case 5:
			if rng.IntN(3) == 0 {
				if _, err := q.Cancel(ctx(t), id); err == nil {
					cancelled++
				} else if !errors.Is(err, store.ErrWrongStatus) {
					t.Fatalf("step %d: cancel: %v", step, err)
				}
			}
		}
		for i, r := range rows {
			s := stateOf(t, e.Pool, r)
			if s.Attempts != last[r] {
				t.Fatalf("step %d (op on %s): row %s has %d attempts, the claims handed out %d", step, id, r, s.Attempts, last[r])
			}
			if s.Status == domain.Sent || s.Status == domain.Cancelled {
				rows[i] = fresh()
			}
		}
	}
	t.Logf("%d claims, %d results stored, %d rows reaped, %d requeued, %d cancelled", claims, finished, reaped, requeued, cancelled)
	if claims < 50 || finished == 0 || reaped == 0 || requeued == 0 || cancelled == 0 {
		t.Errorf("the run did not exercise every operation: %d claims, %d results, %d reaped, %d requeued, %d cancelled",
			claims, finished, reaped, requeued, cancelled)
	}
}

// waitForLockWait returns when a session of this database waits for a lock.
func waitForLockWait(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(ctx(t), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the operation never waited for the lock")
}

// blockQueue holds a lock that makes every queue write wait, until release is called.
func blockQueue(t *testing.T, pool *pgxpool.Pool) (release func()) {
	t.Helper()
	tx, err := pool.Begin(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx(t), `LOCK TABLE notifications IN EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Rollback(context.Background()) }) }
	t.Cleanup(release)
	return release
}

// §5, rule 8: a queue write in flight is not cut off when its caller's context is cancelled
// (a worker shutting down). Each operation is made to wait for a lock, its caller's context
// is cancelled while it waits, the lock is released, and the write must complete.
func TestQueueWritesAreNotCutOffByACancelledCaller(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)

	inFlight := func(name string, op func(context.Context) error) {
		t.Helper()
		release := blockQueue(t, e.Pool)
		caller, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- op(caller) }()
		waitForLockWait(t, e.Pool)
		cancel()
		time.Sleep(50 * time.Millisecond)
		release()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("%s with a cancelled caller: %v", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s did not return", name)
		}
	}

	id := addRow(t, e.Pool, waiting(-time.Second))
	var claimed []store.Claimed
	inFlight("claim", func(c context.Context) (err error) {
		claimed, err = q.Claim(c, domain.SMS, domain.High, 1, hour)
		return err
	})
	if len(claimed) != 1 || stateOf(t, e.Pool, id).Status != domain.Processing {
		t.Fatalf("claim: returned %d rows, row is %s", len(claimed), stateOf(t, e.Pool, id).Status)
	}
	inFlight("finish", func(c context.Context) error {
		_, _, err := q.Finish(c, id, claimed[0].Attempt, sent)
		return err
	})
	if s := stateOf(t, e.Pool, id); s.Status != domain.Sent {
		t.Errorf("finish: row is %s, want sent", s.Status)
	}

	expired := addRow(t, e.Pool, held(-time.Second, 1, 6))
	inFlight("reap", func(c context.Context) error {
		_, err := q.Reap(c, 10)
		return err
	})
	if s := stateOf(t, e.Pool, expired); s.Status != domain.Pending {
		t.Errorf("reap: row is %s, want pending", s.Status)
	}
	inFlight("cancel", func(c context.Context) error {
		_, err := q.Cancel(c, expired)
		return err
	})
	failed := addRow(t, e.Pool, row{status: domain.Failed, attempts: 6, maxAttempts: 6})
	inFlight("requeue", func(c context.Context) error {
		_, err := q.Requeue(c, failed, 6)
		return err
	})
	if a, b := stateOf(t, e.Pool, expired).Status, stateOf(t, e.Pool, failed).Status; a != domain.Cancelled || b != domain.Pending {
		t.Errorf("cancel and requeue: rows are %s and %s, want cancelled and pending", a, b)
	}
}

// §5: every queue write that cannot finish ends at its own timeout, even when its caller
// waits forever.
func TestQueueWritesHaveTheirOwnTimeout(t *testing.T) {
	e := testkit.Setup(t)
	q := store.NewQueue(e.Pool)
	addRow(t, e.Pool, waiting(-time.Second))
	c := claim(t, q, domain.SMS, domain.High, 1, hour)
	addRow(t, e.Pool, held(-time.Second, 1, 6))
	waitingRow := addRow(t, e.Pool, waiting(time.Minute))
	failedRow := addRow(t, e.Pool, row{status: domain.Failed, attempts: 6, maxAttempts: 6})

	short := q.WithTimeout(300 * time.Millisecond)
	ops := map[string]func() error{
		"claim": func() error {
			_, err := short.Claim(context.Background(), domain.SMS, domain.High, 1, hour)
			return err
		},
		"finish": func() error {
			_, _, err := short.Finish(context.Background(), c[0].ID, c[0].Attempt, sent)
			return err
		},
		"reap": func() error {
			_, err := short.Reap(context.Background(), 10)
			return err
		},
		"cancel": func() error {
			_, err := short.Cancel(context.Background(), waitingRow)
			return err
		},
		"requeue": func() error {
			_, err := short.Requeue(context.Background(), failedRow, 6)
			return err
		},
	}
	for name, op := range ops {
		release := blockQueue(t, e.Pool)
		done := make(chan error, 1)
		began := time.Now()
		go func() { done <- op() }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: a write blocked past its timeout succeeded", name)
			}
			if took := time.Since(began); took > 3*time.Second {
				t.Errorf("%s returned after %v, want about 300 ms", name, took)
			}
			release()
		case <-time.After(3 * time.Second):
			t.Errorf("%s: a blocked write did not end at its own timeout of 300 ms", name)
			release()
			if err := <-done; err != nil {
				t.Logf("%s after the lock was released: %v", name, err)
			}
		}
	}
}
