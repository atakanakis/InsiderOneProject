# Review of session 5: dispatcher, breaker, reaper; fault tests

- **Date:** 2026-11-20.
- **Reviewer:** a fresh Claude Opus instance, read-only, with the rules of
  [`.claude/agents/reviewer.md`](../../.claude/agents/reviewer.md). It was given the assignment
  text, the session's brief and definition of done in [`plan.md`](../plan.md), the open items
  of sessions 2 to 4, and a clean clone of the repository at `a9fd5b2`. It was not given the
  author's reasoning or conclusions.
- **Scope:** commits `df18343..a9fd5b2` (everything after `5ceec91`, the last commit of
  session 4): `internal/dispatch`, `internal/events`, the changes to `internal/provider`,
  `cmd/worker`, `cmd/mockprovider`, Compose, the Makefile, CI, the guards and the design.
- **Runs:** once, after the code was green locally, as the plan requires. There was no second
  round. Each fix below is verified by its own named test and by `make guards`, not by another
  review.

## What the reviewer ran

- `go build`, `go vet`, `make lint` (0 issues), `make check-docs` (pass), `make test` (failed:
  F3), and `make guards` in a scratch clone: "176 guards, 176 real, 0 failed", about 16 min.
- I1, A4 and A5 measured again; the fault test over 20 seeds; the dispatcher's tests three times
  under `-race` with six CPU burners (all passed).
- On the Compose stack (two workers, the mock provider): Postgres and Redis paused and resumed,
  Postgres made read-only, a worker killed with SIGKILL mid-send, workers stopped gracefully
  mid-send, a slow provider, a failing provider, a wrong URL (404) and a provider that drops
  packets. Throughout, the mock provider's peak was 100 per channel and no row was lost.
- Experiments in a scratch copy: a provider address that accepts and closes connections, a
  Redis out of memory with two rows, a wrong URL with rows trickling in, one shared provider
  client against one per dispatcher, a provider failing half its requests.
- It could not check a real provider over the internet. It did not review `5a2ec23`, pushed
  after `a9fd5b2` to fix the CI failures.

## Verdict, as reported

The dispatcher matches architecture §6 line by line, and the main guarantees held under every
fault injected. CI was red on `a9fd5b2`, and `make test` failed for the reviewer too. The breaker
did not do what the brief required in three situations: rows ended `failed` without reaching the
provider. Three major findings, four minor, five nits.

## Findings and resolutions

| # | Severity | Finding | Verified by the reviewer | Resolution | Verified by |
|---|---|---|---|---|---|
| F1 | major | `ClosedIdle` was set on new connections too. Go reports "server closed idle connection" also when a *new* connection is closed before the request is written: an address that accepts and closes connections (a published port with nothing behind it, a load balancer with no backend). `Classify` then said "retry soon, the breaker learns nothing". Two workers, 40 rows: all 40 `failed` in 8.8 s, 235 of 240 attempts "closed idle", one breaker never opened, nothing reached any server. | Yes | Class: an observation that means two things, told apart by a field the code did not read. `ClosedIdle` requires `ConnReused`; on a new connection it is "no response", a failure. §6 says why. Session 4's record had named `ConnReused` for this. | TestAConnectionClosedByTheProviderAtTheGateIsReportedAsClosedIdle (a server that accepts and closes: 20 sends, none ClosedIdle); guard "ClosedIdle only on a reused connection (session 5's review, F1)" |
| F2 | major | A broken configuration and the Redis half-failure still drained rows into `failed` without a send. Wrong URL, two workers, 6 rows trickling in: all 6 `failed` after 6 attempts each; each worker's breaker needs five failures and a row has six attempts. Redis out of memory: `Available` kept answering, every refusal and then every probe spent an attempt, 2 of 2 rows `failed` after 37 s with no send. The two tests stopped one second after the breaker opened and never reached a half-open period. | Yes | Two parts. **The Redis half-failure is removed, not handled** (CLAUDE.md, rule 11): `Available` is declared `#!lua` without `flags=no-writes` and run with `EVAL`, so Redis refuses it whenever it refuses the take script, nothing is claimed and no attempt is spent; the reviewer had checked this on redis:7. **The per-worker breaker's limit is stated**: §6, "What the breaker protects, and what it does not", says that with little traffic or many workers rows can use their attempts before every breaker is open, and that `failed` is the dead-letter state that `/retry` (session 6) requeues; a shared breaker is in §16 with the reason it is left out. The 404/401 test runs two workers through two open periods and counts the probes. | TestAvailableFailsWhenTheTakeScriptWould (replaces the session 3 test that asserted the opposite); TestARedisThatRefusesWritesClaimsNothing (0 attempts past an open period; 12 before the fix); TestNotFoundAndUnauthorizedOpenTheBreakerAndDoNotDrainTheQueue; guard "Available fails when Redis refuses writes (session 5's review, F2)" |
| F3 | major | CI was red on `a9fd5b2` (run 44750111098): the fault test's I1 check (peak 47, limit 40, 6 counted late), the deadline test's `write` subtest (2.09 s) and TestWorkerBinaryExitsZeroOnSIGTERM. And TestTheHalfOpenBreakerLetsExactlyOneRowThrough was flaky under `-race` (4 of 8 runs failed): it took a request of a row started before the breaker opened for the probe. | Yes | The three CI failures had been fixed as classes in `5a2ec23` before the review returned: every I1 test checks the peak against the limit without the race detector, because "late" is measured on a starved worker and undercounts; the deadline test allows for building its 32 MiB body; the SIGTERM test reads the worker's output through a writer, because `Wait` closes a pipe. The half-open test counts from when no row is in flight. Running the guards found the 404 test had the same fault; same fix. CI on `5a2ec23` was green (run 44752552222). | TestTheHalfOpenBreakerLetsExactlyOneRowThrough (`go test -race -count=10`: 10 of 10); TestNotFoundAndUnauthorizedOpenTheBreakerAndDoNotDrainTheQueue (`-race -count=5`: 10 of 10 subtests) |
| F4 | minor | §6 said `Available` against an unreachable Redis takes about 3.5 s; against a paused Redis it took 20 s (go-redis's read timeout and retries), during which the loop is frozen. | Yes | §6 states both. No code change: the loop claims nothing meanwhile, and the rows in flight are on their own goroutines. | Documents only |
| F5 | minor | One provider client was shared by the worker's three dispatchers, with `IDLE_CONNS = SLOTS = 50`: up to 150 sends on a pool of 50. With a 400 ms provider, 993 connections for 2,362 requests, against 150 with a pool of 150. | Yes | The dispatcher builds its own client from `Options.Provider`; it is not given one, so no two dispatchers can share a pool. The part that allowed sharing is gone. | TestNewRefusesSettingsThatAreNotPositive (a bad provider URL); every dispatcher test builds its client this way |
| F6 | minor | A provider failing about half of its requests nearly stops the channel: 749 of 900 sent after 400 s with 50% 503s and 10% 429s; 100 low-priority rows never claimed. No document said so. | Yes (measured) | §6 states it as the intended reading of a provider that unhealthy, with the reviewer's numbers; a rate-based breaker is in §16 with the reason it is left out. | Documents only |
| F7 | minor | A Postgres stall longer than the queue's 10 s timeout leaves rows held: the claim commits after the worker stopped waiting. A 20 s pause kept 40 rows `processing` for their 60 s lease. | Yes | §5's known behaviours state it: nothing is lost, the delay is one lease. | Documents only |
| N1 | nit | The fault test's "no lease expired" check read `last_error`, which a later Finish overwrites. | Reasoned | The test counts the reapers' events instead. | TestInvariantsHoldUnderRandomFaults |
| N2 | nit | The fault test never produced `failed` rows and never injected a 4xx. | Yes | Scenarios inject 422 (0 to 3%); a `failed` row must have been rejected on its last attempt. F1 and F2 have their own tests. | TestInvariantsHoldUnderRandomFaults |
| N3 | nit | §12 said the A5 test used "the defaults of section 13"; the dispatcher used the tests' shorter pauses and open time. | Yes | §12 says which settings are which. | Documents only |
| N4 | nit | The worker logged the provider URL, and `provider.New` quoted it in its error; a webhook.site path or credentials would reach the logs. | Yes | The worker logs scheme and host only; the error does not quote the URL. | TestTheWorkerLogsOnlyTheProvidersHost, TestNewRefusesAURLThatIsNotAbsoluteHTTP; guards "the worker logs only the provider's host", "New's error does not quote the URL" |
| N5 | nit | Read-only Postgres for 75 s: 3,200 "result write failed" warnings and 941 claim errors. The correlation id was not tested at exactly its bound. | Yes | Log volume: open for session 7 (metrics instead of a line per retry). The bound: a 64-character id is sent. | TestACorrelationIDOutsideItsBoundIsNotSent |

## Claims the reviewer reproduced

- I1 through the dispatchers: peak 100 per channel in every run.
- A4: maximum 3.09 ms in one run, against the record's 0.97 to 2.27 ms; the record's range
  is widened.
- A5: 94.0, 94.6, 95.0 and 94.5 sends/s; longest waits 5 ms, 0.99 s, 1.03 s and 1.06 s; no
  refusal.
- "3,000 sent in about 10 s" on the Compose stack: 10 s, peak 100 per channel.
- At-least-once cases 1 and 2 behave as §6 says: after a SIGKILL, 150 rows were delivered
  twice and everything was final after 65 s; after 75 s of read-only Postgres, 200 rows were
  delivered twice and everything ended `sent`.
- The loop: 27 lines.

## The hard questions, answered

1. *With `FailuresToOpen` × workers greater than `MAX_ATTEMPTS`, what does the breaker protect
   with little traffic?* The provider and the backlog, not the few rows that trickle in. Those
   can reach `failed`, the dead-letter state, and are requeued by `/retry` (§6).
2. *Why does `Available` need `no-writes`?* It did not; the flag was removed (F2).
3. *Is a provider that rejects half of its requests "down"?* The design reads it as unhealthy;
   a failure-rate breaker is left out (§16).
4. *Should a probe be allowed to use a row's last attempt?* Yes: every attempt is one claim, and
   nothing is given back (CLAUDE.md, rule 4). The cost is bounded by one row per open period
   per worker.
