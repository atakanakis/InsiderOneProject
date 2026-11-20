# Review of session 3: rate limiter

- **Date:** 2026-11-20.
- **Reviewer:** a fresh Claude Opus instance, read-only, with the rules of
  [`.claude/agents/reviewer.md`](../../.claude/agents/reviewer.md). It was given the assignment
  text, the session's brief and definition of done in [`plan.md`](../plan.md), and the
  repository. It was not given the author's reasoning or conclusions.
- **Scope:** commits `fbea0d8..875176b` (everything after `f061405`, the last commit of
  session 2): `internal/ratelimit`, its guards in `tools/guards/list.go`, and the document
  changes. The first version's limiter was read for comparison.
- **Runs:** once, after the code was green, as the plan requires. There was no second round.
  Each fix below is verified by its own named test and by `make guards`, not by another
  review.

## What the reviewer ran

- `go build ./...`, `go vet ./...`, `make test` with `-race` (pass), `make lint` (0 issues),
  `make check-docs` (pass), `make guards` once: "106 guards, 106 real, 0 failed".
- In a throwaway copy: hand mutations (a script that uses a time passed from Go; the read
  script's bound built with `..`; a two-element reply), `Wait` against a Redis that accepts
  connections and never answers, workers with different limits and margins on one channel, a
  model of 6 to 35 workers each keeping 10 rows waiting at the gate, and Lua's number
  formatting on `redis:7-alpine`.
- It could not check CI (the commits were not pushed yet) or a Linux host (F4).

## Verdict, as reported

The limiter is correct for the brief: the window, the clock and fail-closed hold, and the A6
test catches a window that is 2 ms short. No critical or major finding. Three minor findings,
each about how later sessions will use the limiter, and six nits.

## Findings and resolutions

| # | Severity | Finding | Verified by the reviewer | Resolution | Verified by |
|---|---|---|---|---|---|
| F1 | minor | `Wait`'s error could not tell "the limiter failed" from "no permit within the budget", which §6's verdict table needs apart. Against a hung Redis with a 200 ms context, `Wait` returned `rate limit script: context deadline exceeded`, and `errors.Is(err, context.DeadlineExceeded)` was true, as it is for a wait that runs out. | Yes | Class: two refusals that look alike. `Wait` returns `ErrNoPermitInTime` only when its context ends before a script call or while it sleeps; every other error, a context that ends during a script call included, is the limiter's. §7 says why a cut-off call counts as the limiter's: a healthy Redis answers in well under a millisecond. | TestAHungRedisIsALimiterErrorNotAWaitThatRanOut, TestWaitStopsWhenItsContextEnds, TestWaitWithAnEndedContextTakesNoPermit, TestAPermitIsRefusedAtTheLimit; guards "a limiter error is not ErrNoPermitInTime", "an ended context is ErrNoPermitInTime", "Wait stops when its context ends" |
| F2 | minor | The window is one Redis key per channel, but the limit and margin are each worker's settings. Limits 100 and 150: up to 150 permits in a 1.05 s window and 150 members stored, breaking I1 and the §8 bound. Margins 50 ms and 0: up to 200 in a 1.05 s window. | Yes | Class: shared state with per-process settings (the queue's lease and the breaker are passed or kept per worker; the window is the only shared one). Both scripts compare the worker's settings with those stored next to the window and refuse a worker whose settings differ: it claims nothing and its breaker opens. The stored settings expire with the window. §7 says what this means for a rolling update that changes them. | TestWorkersWithDifferentSettingsAreRefused, TestTheWindowHoldsAtMostTheLimitAndExpires; guards "Wait refuses a worker with other settings", "Available refuses a worker with other settings", "the settings expire with the window" |
| F3 | minor | `Wait` serves waiters in no order: all waiters refused against the same oldest permit wake together and the first script to reach Redis wins. In a model with every worker keeping 10 rows waiting, the longest waits were 3.16 s and 2.12 s at 12 workers (the formula gives 2.25 s), 4.3 s at 24 with 3 and 4 refusals in 20 s, and 76 refusals of about 2,100 at 35. §6 and §13 said refusals start at about 35 workers. Redis load stayed low (2.1 to 3.4 script calls per permit). | Yes (a model of the dispatcher with the real limiter) | No queue was added at the gate, as the reviewer also advised. §6, §12 (A5) and §13 now give the formula as the design review's estimate under in-order service, with the reviewer's figures, and say the real figures are A5, measured in session 5. §6 says the gate has no priority order and I9 holds at claim time. | Documents only; A5 is session 5's measurement |
| F4 | nit | The test that compared a permit's stored time with Redis's `TIME` caught a script using Go's time only because the Docker VM's clock was about 1 ms off the host's; on Linux the two are one clock. | Yes (macOS); reasoned (Linux) | Renamed to what it checks, TestAPermitIsStoredWithTheTimeItWasGranted, and removed from I12. I12's evidence in §11: no argument the limiter sends is a time (so a script can read only `TIME`), and a script that reads no clock fails A6. | TestTheLimiterSendsRedisNoClockTime; guard "no worker time is sent to Redis (I12)" |
| F5 | nit | The `string.format('%d')` rule had no guard: with `..` in the read script's bound all tests passed (an error of about 50 µs in a hint). | Yes | §7 now calls it a convention without a guard and gives the size of the error; the take script's bounds are covered by A6. | No guard, by decision: a test at microsecond precision against a live clock would be flaky |
| F6 | nit | A backward jump of the Redis clock was missing from "Limits of the guarantee". | Reasoned | Added: permits look like future permits, the channel sends less or nothing for up to the jump; fails closed. | Documents only |
| F7 | nit | Redis 7.0 is required (`#!lua` flags, `EVAL_RO`) but not stated. | Reasoned | §7 and the package comment say so; on an older Redis every call fails, which fails closed. | Documents only |
| F8 | nit | §7 cited TestAnUnreachableRedisIsAnErrorNotAPermit for "a malformed reply is an error". | Yes | §7 no longer cites a test for the malformed reply; the reviewer found that a two-element reply made other tests fail. | Documents only |
| F9 | nit | The configuration refuses `WINDOW_MARGIN=0`, `New` accepted 0. | Yes | `New` refuses a margin that is not positive. | TestNewRefusesALimitBelowOneAndAMarginThatIsNotPositive; guard "the margin is positive" |

## Claims the reviewer reproduced

- A6 with the window's lower bound 2 ms early: 5 of 5 runs failed
  ("permits 0 and 100 are 1048xxx µs apart"); 400 permits in 3.3 s in every run.
- 106 of 106 guards real, the 16 of the limiter included.
- Lua's `..` prints a microsecond time with 14 significant digits (`1.7951711934506e+15`).
- Session 1's Redis deadline fix holds for the limiter: `Wait` against a hung Redis returned
  202 ms after a 200 ms deadline.

## Questions the reviewer asked, and the answers

1. *How will `Classify` tell a gate budget that ran out from a Redis that hangs?* By
   `ErrNoPermitInTime` (F1).
2. *What stops a rolling update that changes `RATE_LIMIT` or `WINDOW_MARGIN` from breaking I1?*
   The settings check (F2): the new workers are refused until the old ones are gone and the
   window has expired.
3. *Does I9 hold only at claim time?* Yes. The gate has no priority order; the claim batch rule
   keeps the number of rows waiting there small. §6 says so.
4. *Which line enforces "the clock is Redis's", and which guard disables it?* The script's
   `TIME` is the only clock it can read, because Go sends no time; the guard adds a time to
   what Go sends, and TestTheLimiterSendsRedisNoClockTime fails. A script that read no clock
   at all fails A6. A mutation that made the script read a time sent from Go would be caught
   only where the test's clock and Redis's differ, so no such guard was added; the guard is on
   the Go side, where the time would have to come from.
5. *A retried call sends the same token, so a second grant only moves its member's score. Is
   that relied on?* No. With or without it, a lost permit costs throughput and never a
   request; §7 states it.
