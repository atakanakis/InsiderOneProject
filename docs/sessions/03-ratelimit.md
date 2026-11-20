# Session 3: rate limiter

- **Date:** 2026-11-20.
- **Lead:** Claude Opus. **Reviewer:** one fresh Claude Opus instance, run once after the
  code was green ([review record](../reviews/03-session-3-review.md)).
- **Brief:** [plan.md, "3. Rate limiter"](../plan.md#3-rate-limiter).

## What was built

| Part | What it is | Evidence |
|---|---|---|
| `internal/ratelimit` | `New`, `Available` and `Wait` of architecture §7, on Redis | 17 tests in `ratelimit_test.go` (15 before the review), on a real Redis; three run without Docker |
| The take script | `TIME`, remove permits a window old, grant one if fewer than the limit remain; the score is the Redis time in microseconds, the member a token from the caller; the key expires one window after the last permit. When it refuses, it returns the time until the oldest permit leaves the window | A6 and I12 tests (§11, §12); TestWaitReturnsWhenTheOldestPermitLeavesTheWindow, TestAPermitCountsForOneSecondPlusTheMargin, TestTheWindowHoldsAtMostTheLimitAndExpires |
| `Wait` | Runs the take script; when refused, sleeps for the returned time or until its context ends, and asks again. Any error is returned as an error, never as a permit; `ErrNoPermitInTime` means the context ended first, every other error is the limiter's | TestWaitStopsWhenItsContextEnds, TestWaitWithAnEndedContextTakesNoPermit, TestAnUnreachableRedisIsAnErrorNotAPermit, TestAHungRedisIsALimiterErrorNotAWaitThatRanOut |
| `Available` | A second script, declared `#!lua flags=no-writes` and run with `EVAL_RO`: it counts the permits younger than the window on the Redis clock and writes nothing | TestAvailableChangesNothing, TestAvailableCountsTheFreePermits |
| One setting per window | The limit and window are stored next to the window; a worker with other settings is refused by both scripts | TestWorkersWithDifferentSettingsAreRefused |
| The half-failure | On a read-only replica (`REPLICAOF`) and with Redis out of memory (`maxmemory 1`), `Available` answers and `Wait` returns the script's error at once | TestAvailableWorksAndWaitFailsWhenOnlyTheScriptFails |
| `make guards` | 21 guards of the limiter (16 before the review), each disabling one line | see "Measured" |

The limiter is not wired into the worker yet. Its only caller will be the provider client's
send gate (session 4); the dispatcher's `room` reads `Available` (session 5).

## How the tests came first

The design details went into §7 first (`fbea0d8`), after checking each claim on
`redis:7-alpine` with `redis-cli`: the take script is refused with OOM and with READONLY, the
no-writes script answers in both cases, a write inside a no-writes script is refused, and Lua's
`tostring` prints a microsecond time with 14 digits (`1.7951694285988e+15`) where
`string.format('%d')` keeps all 16.

The 15 tests were committed against a stub that compiled and returned errors (`c17ded6`):
13 failed (`go test -count=1 ./internal/ratelimit`). The two that passed expect an error and
got the stub's; against the real code, TestAnUnreachableRedisIsAnErrorNotAPermit is checked by
a guard, and TestWaitWithAnEndedContextTakesNoPermit tests go-redis's own refusal to send on an
ended context, which the limiter relies on instead of a check of its own. The code followed in
`979db42`; all 15 passed on the first run.

**What was carried over from the first version.** The shape of its script
(`InsiderOneProjectDeneme/internal/ratelimit/ratelimit.go`): `TIME`, `ZREMRANGEBYSCORE`,
`ZCARD`, `ZADD`, `PEXPIRE`, and the wait until the oldest permit expires. Changed: one permit
per call instead of a batch, microseconds instead of milliseconds (a millisecond stamp lets two
permits be up to 1 ms less than a window apart in real time), and numbers formatted with `%d`.
Not carried over: `Take` of many permits and `Refund`, which gives permits back (CLAUDE.md,
rule 4). None of its tests were carried over; they tested `Take` and `Refund`.

## The guard that was not real

The first full guard run of the limiter had one failure: moving the window's lower bound 100 ms
early ("a permit counts for the whole window") did not make the A6 test fail. Every caller of
the test was inside `Wait`'s loop, so callers only asked when the script said a permit would be
free, and nobody asked during the 100 ms in which the bug grants. New sends arrive at any
moment, so half of the test's callers now ask once at random moments (`50ca87c`). With the
bound moved only 2 ms early, the test now fails in 3 of 3 runs. The class: a concurrency test
whose callers all follow the system's own timing hint cannot see a bug in that hint's
neighbourhood.

## What was measured

| What | Result | Command |
|---|---|---|
| Tests | 120 test functions (103 after session 2, 17 new); all pass with the race detector | `make test`; count: `grep -rh '^func Test' --include='*_test.go' . \| grep -v TestMain \| wc -l` |
| Lint | 0 issues | `make lint` |
| Guards | 111 guards (90 of sessions 1 and 2, 21 of the limiter), 111 real, 0 failed; 6 min 15 s for the whole run | `time make guards` |
| Test names in the documents | all exist | `make check-docs` |
| A6: 200 callers on 4 Redis clients, limit 100, margin 50 ms, 3.3 s | 400 permits; at most 100 in any window of 1,050,000 µs, on the permits' Redis times | `go test -count=1 -v -run TestNoWindowHoldsMoreThanTheLimit ./internal/ratelimit` |
| A6 with the window's lower bound 2 ms early | fails 3 of 3 runs: 101 permits within 1,048,023 to 1,048,147 µs | the same, with `now - window` changed to `now - window + 2000` by hand |
| Wait against an unreachable Redis (a closed port) | an error from each; 3.5 s for the two calls together, because go-redis retries the dial | `go test -count=1 -v -run TestAnUnreachableRedisIsAnErrorNotAPermit ./internal/ratelimit` |

## What the review found

No critical or major finding; three minor findings and six nits, all fixed or answered. The
record, with each resolution and the test or guard that verifies it, is
[03-session-3-review.md](../reviews/03-session-3-review.md). In short:

- **Two refusals looked alike** (F1). "No permit within the gate budget" and "Redis hangs"
  both wrapped `context.DeadlineExceeded`, but §6 sends only the second to the breaker. `Wait`
  now returns `ErrNoPermitInTime` for the first.
- **Shared state with per-worker settings** (F2). Workers with limits 100 and 150 put 150
  permits in one window. The scripts now store the settings with the window and refuse a
  worker whose settings differ.
- **Waiters are served in no order** (F3), so the wait at the gate is longer than the design
  review's formula: the reviewer measured refusals from 24 workers where the design said 35.
  No queue was added; the figures are now an unmeasured assumption (A5) for session 5.
- Smaller: an I12 test that depended on two clocks differing, renamed and replaced as I12's
  evidence; the `%d` rule is a convention without a guard; a backward clock jump and Redis 7.0
  are in §7; `New` refuses a margin of 0, as the configuration does.

## What changed in the design

All in [architecture.md](../architecture.md):

- **§7, before the code:** "The limiter in detail": the two scripts, microsecond scores, the
  caller's token, Go passing no time, `EVAL_RO` and the no-writes flag, what an error and an
  ended context mean, the bound of the set, no refund.
- **§8:** the rate-limit window's bound in the bounds table.
- **§11, §12:** the test names of I1 (the limiter's part), I12 (Redis) and A6, with the A6
  measurement.
- **§6, §7, §11, §12, §13, after the review:** `ErrNoPermitInTime`; one setting per window;
  the wait at the gate and "35 workers" restated as unmeasured, with the reviewer's figures; I9
  holds at claim time; I12's evidence; the limits table gains a backward clock jump; Redis 7.0.

## What is still open

- I1 at the provider's side, A4 (permit to arrival) and the gate's use of `Wait`: session 4.
- Waiters at the gate are served in no order (review F3). The wait at the gate and the
  number of workers per channel before refusals start (A5) are measured in session 5, on the
  real dispatcher. If they are too long, the claim batch is the lever, not a queue at the gate.
- A rolling update that changes `RATE_LIMIT` or `WINDOW_MARGIN` stops sends on the new workers
  until the old ones are gone (§7). Changing them means stopping all workers of the channel.
- go-redis retries a command when the connection fails with `EOF` after the command was
  written. If that happens to a take script that did grant a permit, the retry can grant a
  second one; neither is used. This costs throughput, never a request (§7). Reasoned from
  go-redis's retry rules, not measured.
- `Available` and `Wait` against an unreachable Redis take 3.5 s together because go-redis
  retries the dial; each stays within its caller's deadline. Session 5 decides whether the
  dispatcher's read of `Available` needs a shorter deadline.
