# Session 2: queue

- **Date:** 2026-11-20.
- **Lead:** Claude Opus. **Reviewer:** one fresh Claude Opus instance, run once after the
  code was green ([review record](../reviews/02-session-2-review.md)).
- **Brief:** [plan.md, "2. Queue"](../plan.md#2-queue).

## What was built

| Part | What it is | Evidence |
|---|---|---|
| `internal/store/queue.go` | Claim, Finish, Reap, Cancel and Requeue of architecture §5; the only file with queue SQL | 28 tests in `queue_test.go` and `queue_plan_test.go` (25 before the review), on a real Postgres |
| Claim | The statement of §5: `id = ANY(ARRAY(...))`, `FOR UPDATE SKIP LOCKED`, one channel and one priority per call, lease from `now()`; the at most `n` rows sorted by `ready_at` in Go | I2, I7, I12, A3 tests (§11, §12); TestClaimSkipsRowsLockedByAnotherTransaction |
| Finish | One transaction: the attempt row (`ON CONFLICT DO NOTHING`), then the row `WHERE id AND attempts = k AND status = 'processing'`. A stale result commits its attempt row as history. A retry on the last allowed attempt becomes `failed` in the same update | I3 tests; TestARetryOnTheLastAttemptIsStoredAsFailed |
| Reap | The claim's form with `SKIP LOCKED`; `pending`, or `failed` at the last attempt; keeps `attempts` and `ready_at` | TestReapReturnsExpiredRowsToPendingOrFailed, TestTwoReapersAtOnceReturnEachRowOnce |
| Cancel, Requeue | Lock the row, then one update, in one transaction; tell "no such row" from "wrong status". Requeue sets `max_attempts = attempts + budget` | TestCancelOnlyFromWaitingStatuses, TestACancelledRowCannotBeRevived, TestRequeueRaisesMaxAttemptsByTheBudget |
| Context | Every operation runs on `context.WithoutCancel` with its own 10 s timeout | TestQueueWritesAreNotCutOffByACancelledCaller, TestQueueWritesHaveTheirOwnTimeout |
| Stored text | Error and outcome (1 KiB) and provider message id (256 bytes): cut, made valid UTF-8 without NUL, cut at a character boundary (§5, §8) | TestFinishStoresAnyErrorTextWithinItsBound, TestFinishStoresTextAtItsBound |
| `make guards` | Postgres and Redis start once per run; each test process creates and migrates its own database on them; the URLs go only to the runner's test processes | 3.3 to 3.6 s per schema guard (was about 10 s); see "Measured" |

## How the tests came first

The first 25 tests were committed with a stub `queue.go` that had the API and returned errors
(`9439afa`): 24 failed (`go test ./internal/store`). The 25th, the own-timeout test, passed on
the stub's immediate error; `make guards` checks it against the real code. The code followed in
`8c23c5a`. The first green run found two mistakes, both in the tests (a test claimed a row it did
not mean to; the random sequence left rows held and claimed too little); the commit says so.

The three tests added after the review were run against the code they guard with the guard
disabled, through `make guards` (for example, plain `FOR UPDATE` makes
TestClaimSkipsRowsLockedByAnotherTransaction fail after the queue's 2 s timeout).

Nothing of the first version's queue was carried over.

## What was measured

| What | Result | Command |
|---|---|---|
| Tests | 103 test functions (75 after session 1, 28 new); all pass with the race detector | `make test`; count: `grep -rh '^func Test' --include='*_test.go' . \| grep -v TestMain \| wc -l` |
| Lint | 0 issues | `make lint` |
| Guards | 90 guards (53 of session 1, 37 of the queue), 90 real, 0 failed; 5 min 13 s for the whole run, where session 1's 53 guards took 246 s | `time make guards` |
| Test names in the documents | all exist | `make check-docs` |
| A3: claim of 10 among 50,000 due rows with one `ready_at`, 200,000 rows not due, 50,000 finished | 0.13 to 0.31 ms, 4 pages of the claim index, under custom and forced generic plans; no sort, no sequential scan, no bitmap scan of the claim index | `go test -count=1 -v -run TestClaimPlanReadsTheIndexInOrder ./internal/store` |
| A3: a claim that finds nothing due above 100,000 future rows | 0.03 ms, 3 pages | the same |
| A3: the rejected `id IN (SELECT ...)` form | forced generic plan: Seq Scan with Hash Join, 32.5 ms for the claim of 10 | the same test with the mutation of the guard "A3, the ANY(ARRAY(...)) form" applied |
| I2 under load | 32 concurrent claimers, 2,000 rows: each row claimed once, attempts 1 | TestConcurrentClaimsNeverTakeTheSameRow |
| `make guards` per guard | 3.3 to 3.6 s for a schema guard, where session 1 measured about 10 s | `go run ./tools/guards -only "schema:"` |

## What the review found

One major finding, one minor and seven nits (the reviewer's F3 was minor). All fixed or
answered; the record, with each resolution and the test or guard that verifies it, is
[02-session-2-review.md](../reviews/02-session-2-review.md). In short:

- **The "skip" half of `SKIP LOCKED` had no test** (major). The guards removed the whole
  clause, which double-claims and so failed the I2 test; plain `FOR UPDATE` passed every test
  while claims waited behind locked rows. The class: a guard that disables more than the line
  it names proves only the larger part. Fixed for both statements that use the clause (Claim
  and Reap), with a test each and guards that remove only `SKIP LOCKED`. The design now says
  which half does what.
- **The text bound had no test at its edge**: an off-by-one made Finish panic on an error of
  exactly 1,024 bytes. Tested at the bound for all three bounded fields; the bounds are in
  the §8 table.
- **The random attempts test went idle** after step 380 of 600. It now keeps rows waiting and
  fails if an operation never took effect.
- Smaller: Cancel and Requeue report the status their update saw; Reap's `last_error` and
  missing attempt row are in §5; the own-timeout test covers all five operations; long text
  is cut before it is cleaned; the guards' server URLs no longer reach the shell.

## What changed in the design

All in [architecture.md](../architecture.md), before the code that follows them:

- **§5, before the code:** Claim sorts its rows in Go; Finish stores a retry on the last
  attempt as `failed`, from the row's own counters; `started_at` is `now()` minus the
  measured duration; a repeated Finish returns "stale"; Reap uses the claim's form and keeps
  `ready_at`; Cancel tells "missing" from "wrong status"; all five operations run on a context
  without the caller's cancellation, with a 10 s timeout; the stored text is bounded.
- **§5, §8, §11, after the review:** the lock and `SKIP LOCKED` are credited separately (I2);
  Reap's `last_error` and the missing attempt row; Cancel and Requeue lock the row first; the
  text bounds in the §8 table.
- **§11, §12, §14:** test names, the A3 measurement, and the test kit's shared servers.

## What is still open

- Session 4 should bound how much of a provider's error body it reads before it builds the
  error text; the queue no longer depends on it (it cuts first), but reading the body is the
  provider client's cost.
- I4's other half, "every claimed row reaches Finish", is the dispatcher's single exit:
  session 5. The queue's part is tested.
- The rate-limit half of I12 is session 3.
- `TestClaimsInPriorityTurnTakeHigherPrioritiesFirst` tests a loop written in the test; the
  dispatcher's own loop and I9 are session 5's (review F9).
- Cancel and Requeue's status report (review F4) is correct by construction; there is no test
  for the race it removed.
