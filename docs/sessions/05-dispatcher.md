# Session 5: dispatcher, breaker, reaper; fault tests

- **Date:** 2026-11-20.
- **Lead:** Claude Opus. **Builder:** one Claude Opus subagent, for the mock provider's fault
  injection. **Reviewer:** one fresh Claude Opus instance, run once after the code was green
  ([review record](../reviews/05-session-5-review.md)).
- **Brief:** [plan.md, "5. Dispatcher, breaker, reaper"](../plan.md#5-dispatcher-breaker-reaper).

## What was built

| Part | What it is | Evidence |
|---|---|---|
| The loop (`internal/dispatch`) | `Run` is the loop of architecture §6, 29 lines: `room`, a claim per priority highest first, every row started before the next claim, a pause when there is no room or nothing was claimed. It waits for the rows in flight when its context ends | I4, I9 tests (§11); TestShutdownWaitsForSendsInFlightAndDoesNotCancelThem |
| `room` | The four limits of §6, read cheapest first, each only read: free slots, the claim batch (0 while a row of this dispatcher waits for its permit), `breaker.Allow`, `limiter.Available` (an error is 0) | TestAWorkerHasAtMostOneClaimBatchWaitingForPermits, TestNoRowIsClaimedWhileTheWindowIsFull, TestAnUnavailableRateLimitClaimsNothing |
| `start` and `handle` | The single exit: the send through the dispatcher's own `*provider.Client` (built by `New`, so no fake can skip the gate and no pool is shared) with `limiter.Wait` as the gate, `Classify`, `Report`, then `Finish` retried until the database answers or one lease has passed. `handle` runs on a context without the loop's cancellation | TestAFailedResultWriteIsRetriedAndNotSentAgain, TestInvariantsHoldUnderRandomFaults |
| `Classify` | The verdict table of §6, read top to bottom; a response always wins; three kinds of "no response" | TestClassifyDecidesEveryRowOfTheVerdictTable, TestAResponseAlwaysWins, TestTheVerdictTableHoldsForRealSends |
| Retry policy | Backoff `min(cap, base × 2^(k−1))`, drawn between half and all of it; `Retry-After` honoured up to the cap; "about 1 s" is 1 to 2 s | TestRetryDelaysDoubleWithJitterUpToTheCap, TestRetryAfterIsHonouredUpToTheCap, TestARetriedRowWaitsForItsBackoff |
| Breaker | Closed, open, half-open; `Allow` reads, `Started` marks and returns the probe, `Report` clears it; outside the closed state only the probe's report counts | Five unit tests with a clock; TestTheHalfOpenBreakerLetsExactlyOneRowThrough, TestAClaimThatFindsNothingLeavesNoProbe |
| Reaper | One per worker, every `REAPER_TICK`, in batches of 100 until a short one; publishes each change | TestTheReaperReturnsExpiredRowsAndPublishesTheirStatus, TestAKilledWorkersRowComesBackThroughTheReaper |
| `internal/events` | The narrow `Publisher` interface and `Discard`, until session 7 | — |
| `internal/provider` | `Result.ClosedIdle`, on a reused connection only; `New` refuses a URL that cannot make a request and does not quote it; `X-Correlation-ID` only within its bound | TestAConnectionClosedByTheProviderAtTheGateIsReportedAsClosedIdle, TestNewRefusesAURLThatIsNotAbsoluteHTTP, TestACorrelationIDOutsideItsBoundIsNotSent |
| `cmd/worker` | A dispatcher per channel and a reaper; waits for sends in flight up to `SHUTDOWN_TIMEOUT`; logs only the provider's host | TestTheWorkerDeliversEveryChannel, TestAKilledWorkersRowComesBackThroughTheReaper, TestTheWorkerLogsOnlyTheProvidersHost |
| `cmd/mockprovider` | `MOCK_FAIL_RATE` (503) and `MOCK_THROTTLE_RATE` (429 with `Retry-After: 1`), counted in `/stats` (built by the builder subagent) | TestMockProviderInjectsFailuresAndThrottlesAtTheirRates, TestMockProviderRefusesARateOutsideZeroToOne |
| Compose, Makefile, CI | Two workers; `make enqueue N=...` and `make status` (no API yet); the CI compose job delivers 1,500 rows and checks the peak at the mock provider; the test job measures I1, A4 and A5 without the race detector | CI runs (below) |
| `internal/ratelimit` (after the review) | `Available` lost `flags=no-writes` and `EVAL_RO`: it fails exactly when the take script would, so a Redis that refuses writes makes the dispatchers claim nothing | TestAvailableFailsWhenTheTakeScriptWould, TestARedisThatRefusesWritesClaimsNothing |
| `make guards` | 47 new guards (42 of the dispatcher, the provider's changes and the worker; 2 of the mock provider; 3 after the review), one replaced: 179 | see "Measured" |

## How the tests came first

The design went first (`df18343`): the verdict table's three kinds of "no response", the
retry delays, the breaker's probe in `Report`, the pieces of the loop. Then:

1. The randomised fault test, against a stub of the package that compiled
   (`41de5b9`): its three scenarios failed on `New`.
2. Every other test of the brief, and the three provider tests of the design change, against
   the stub (`b916fc1`): 21 of 21 tests of the package failed, and the three provider tests.
3. The code (`bd3647b`). The first run failed one test, and the test was wrong: it counted
   the result writes before a write whose commit had been lost had finished retrying.
4. The worker's tests (`2afbadf`) failed before the wiring (`bfe4d51`).

## Decisions this session made

- **"No response" has three answers** (session 4's open items F3 and F7). The provider did not
  answer: retry, a failure for the breaker. The provider closed a kept-alive connection while
  the send waited at the gate (Go's "server closed idle connection"; nothing was written):
  retry in about 1 s, nothing for the breaker. A request that cannot be built: removed instead
  of classified; the URL is checked once in `provider.New`, and the correlation id header is
  sent only within its bound.
- **A gate budget that ends during a script call** stays a limiter error (session 3). A5 found
  no limiter error and no refusal of any kind up to 12 dispatchers per channel, so the case
  does not arise under ordinary contention.
- **Waiters at the gate are served in no order** (session 3, F3). A5 measured the longest wait
  as about one window, so neither the claim batch nor the gate needed to change.
- **`Available` against an unreachable Redis** (3.5 s, session 3) is read with the loop's own
  context and no shorter deadline: the loop claims nothing meanwhile, and nothing else waits.
- **The margin stays 50 ms** (session 4's 106 in CI). Through the dispatchers, with three
  workers and all channels at the limit, the time from permit to arrival was at most 2.27 ms
  without the race detector and 28.6 ms with it. The cases that exceed the margin are a starved
  CPU, which no margin bounds; they are the "frozen worker" row of §7's limits table, and the
  I1 tests check the guarantee as that row states it.

## What was measured

| What | Result | Command |
|---|---|---|
| Tests | 172 test functions (137 after session 4; 35 new, of which one replaced a session 3 test); all pass with the race detector | `make test`; count: `grep -rh '^func Test' --include='*_test.go' . \| grep -v TestMain \| wc -l` |
| Lint | 0 issues | `make lint` |
| Guards | 179 guards (132 before the session). On CI (Linux) before the review's fixes: `176 guards, 176 real, 0 failed` (run 44752552222); the reviewer, locally: 176 of 176. After the fixes, each new or changed guard by hand (`-only`), and the whole list on CI (below) | `time make guards`; per prefix: `go run ./tools/guards -only dispatch:` |
| Test names in the documents | all exist | `make check-docs` |
| Randomised fault test | After the review: 53 of 53 scenarios passed (seeds 1 to 3 and 2000 to 2049; 15,900 notifications, 17,626 requests); the peak of every scenario was the limit, 40; one notification was accepted twice, after an attempt with no response (at-least-once case 3); 46 scenarios ended rows `failed`, each rejected (422). Before the review: 52 of 53 (see below). `make test` runs three scenarios | `FAULT_RUNS=50 FAULT_SEED=2000 go test -count=1 -timeout 60m -run TestInvariantsHoldUnderRandomFaults -v ./internal/dispatch \| grep -c 'FAULT_RESULT.*passed=true'` |
| I1 through the dispatchers (3 workers × 3 channels at the limit, 1,200 requests) | peak 100 per channel within one second, without and with `-race` (1 + 3 runs) | `go test [-race] -count=1 -v -run TestTheProviderReceivesAtMostTheLimitThroughTheDispatchers ./internal/dispatch` |
| A4 through the dispatchers | without `-race`: median 60 to 70 µs, max 0.97 to 2.27 ms here, 3.09 ms in the reviewer's run, 7.92 ms on the CI runner (run 44752552222); with `-race`, 3 runs: max 10.5 to 28.6 ms; none past 50 ms | the same; on CI, the test job's last step |
| A5 (1, 3, 6, 12 dispatchers, 12 s each) | 94.1, 94.9, 94.9, 94.9 sends/s (top 95.2); longest wait 6 ms, 0.98 s, 1.03 s, 1.05 s; no refusal. With `-race`: 94.1 to 95.0 sends/s, longest 1.09 s, no refusal | `A5_FULL=1 go test [-race] -count=1 -v -run TestGateWaitAndThroughputWithManyDispatchers ./internal/dispatch` |
| The Compose stack, two workers | 3,000 rows sent in about 10 s; peak 100 per channel at the mock provider. With 20% 503s, 5% 429s and 30 ms latency, 900 more: all sent, 290 error responses retried, peak 100 | `make up; make enqueue N=1000; curl localhost:9090/stats; make status` |

## The fault test's one failure

A run of 53 scenarios (`FAULT_RUNS=50 FAULT_SEED=1000`) passed 52. Seed 1038 had four
notifications that the provider accepted twice. Each had an earlier attempt that ended with no
response. The fault was the test's own: the recorder closes "idle" connections from another
goroutine, and Go's server marks a connection active only after it has read the whole request
(`net/http/server.go`, lines 2079 to 2082). A request could be read, handled and accepted on a
connection that was then closed under it; the answer was lost, the client saw no response and
retried. That is at-least-once case 3 of §6, which the design names. The test now allows one
more acceptance for each attempt of the notification that ended with no response, fails on any
other duplicate, and counts these in its result line (`662a9db`). Seed 1038 ran four more times
with seeds 1 to 3: every duplicate had a "no response" attempt before it.

## What the review found

Three major findings, four minor and five nits; all fixed or answered. The record, with each
resolution and the test or guard that verifies it, is
[05-session-5-review.md](../reviews/05-session-5-review.md). In short:

- **An address that accepts and closes connections drained the queue** (F1). Go reports it with
  the same error as an idle connection the provider closed; `ClosedIdle` now needs a reused
  connection.
- **Rows still reached `failed` without a send** (F2). The Redis half-failure is removed:
  `Available` fails whenever the take script would, and nothing is claimed. For a wrong URL with
  little traffic the per-worker breaker cannot protect every row; §6 says so, and `failed` is the
  dead-letter state that `/retry` requeues.
- **CI was red and one test was flaky** (F3). The three CI failures were already fixed as classes
  in `5a2ec23`; the half-open test, and the 404 test with the same fault, count from when no row
  of before the opening is in flight.
- Smaller: a paused Redis holds the loop 20 s, not 3.5 s (F4); dispatchers shared one connection
  pool (F5, the dispatcher now builds its own client); a provider failing half its requests
  nearly stops the channel (F6, stated); a Postgres stall longer than 10 s holds rows for a lease
  (F7, stated); the provider URL is no longer logged.

## What changed in the design

All in [architecture.md](../architecture.md):

- **§6, before the code:** the verdict table's three kinds of "no response"; retry delays;
  the probe passed from `Started` to `Report`, and only the probe's report counting outside
  the closed state; the pieces of the loop; result write retries, shutdown and the reaper.
- **§7:** `Result.ClosedIdle`; the URL check and the correlation id bound in the client.
- **§6, §11, §12, §13, after the measurements:** why the wait at the gate is about one window;
  the tests of I1, I4, I5, I6 and I9; A4 through the dispatchers and the margin decision; A5
  measured; the gate budget relation measured up to 12 dispatchers.
- **§5, §6, §7, §16, after the review (`49c26c2`, before the fixes):** `ClosedIdle` on a reused
  connection only; `Available` without `no-writes`, the half-failure removed; what the per-worker
  breaker protects and what it does not; a paused Redis; one client per dispatcher; a Postgres
  stall; the provider URL never logged; a shared breaker and a rate-based breaker left out, with
  the reason.

## What is still open

- The API (session 6). Until then rows are added with `make enqueue`.
- Events are dropped; logs exist, metrics do not (session 7): the breaker's state, the wait at
  the gate, refusals, connections closed at the gate, reaped rows.
- A5 is measured up to 12 dispatchers per channel and a provider that answers in 20 ms. A slower
  provider needs more slots or workers (§13); above 12 dispatchers the wait is not measured.
- At-least-once case 3 is visible on a provider that does not honour `Idempotency-Key`; the
  mock provider does not.
- With little traffic or many workers, a wrong URL or credential can use up the attempts of the
  rows that trickle in before every worker's breaker is open (§6). They end `failed` and are
  requeued by `/retry` (session 6). A breaker shared by all workers is left out (§16).
- Read-only Postgres makes a warning per result write retry (the reviewer counted 3,200 in
  75 s). Session 7's metrics should replace most of these lines.
- The dial timeout is still not tested (§7).

## CI

CI on `a9fd5b2`, run 44750111098: lint and compose succeeded; the test job failed on three
tests under the race detector (review F3); the guards job was cancelled by the next push, with
every guard it had run real. CI on `5a2ec23` (the fixes of those three), run 44752552222: all
four jobs succeeded; guards `176 guards, 176 real, 0 failed`; the measurement step without the
race detector: I1 peak 100 per channel, A4 max 7.92 ms, A5 94.1 to 95.2 sends/s with the
longest wait 1.054 s and no refusal; the compose job delivered 1,500 rows with two workers, the
mock provider's peak 100 per channel (`gh run view 44752552222 --log`).
