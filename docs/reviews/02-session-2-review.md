# Review of session 2: queue

- **Date:** 2026-11-20.
- **Reviewer:** a fresh Claude Opus instance, read-only, with the rules of
  [`.claude/agents/reviewer.md`](../../.claude/agents/reviewer.md). It was given the assignment
  text, the session's brief and definition of done in [`plan.md`](../plan.md), and the
  repository. It was not given the author's reasoning or conclusions.
- **Scope:** commits `4565155..b48b45f` (everything after `2648127`, the last commit of
  session 1), including the change to `internal/testkit` and `tools/guards`.
- **Runs:** once, after the code was green, as the plan requires. There was no second round.
  Each fix below is verified by its own named test and by `make guards`, not by another
  review.

## What the reviewer ran

- `go build ./...`, `go vet ./...`, `make test` (pass); the concurrency and cancellation
  tests again with `-race -count=3` (pass every time); `make lint` on a clean copy (0 issues);
  `make check-docs` (pass).
- `make guards`, once: "85 guards, 85 real, 0 failed"; the working tree clean afterwards.
- In a throwaway clone: mutations of its own (plain `FOR UPDATE` in Claim and Reap; an
  off-by-one in the text bound), a claim next to rows locked by another transaction, the plan
  of the reap statement under both plan modes with 200,000 live leases, an instrumented run of
  `TestAttemptsNeverGoDown`, three runs of the A3 plan test, and the A3 test with the
  `IN (SELECT ...)` form.
- It could not check CI: the session's commits were not pushed yet.

## Verdict, as reported

The session delivers its brief; the five operations match §5, every required test exists and
passes, and the 32 new guards are real. One major gap: the "skip" half of
`FOR UPDATE SKIP LOCKED` had no test. One minor finding (the stored-text bound had no test at
its edge and was not in §8) and seven nits.

## Findings and resolutions

| # | Severity | Finding | Verified by the reviewer | Resolution | Verified by |
|---|---|---|---|---|---|
| F1 | major | The guards removed the whole `FOR UPDATE SKIP LOCKED`. With plain `FOR UPDATE` in Claim and Reap every store test passed, and a claim next to 10 locked rows returned nothing after its 2 s timeout instead of the next 10 rows in 9 ms. §5's "never wait for each other" had no test, and §11 credited I2 to `SKIP LOCKED` although the lock and the status re-check enforce it. | Yes | Two tests hold row locks in another transaction and expect the next rows at once, for Claim and for Reap. Two new guards change only `SKIP LOCKED` to `FOR UPDATE`; the two old guards stay, renamed, for the lock itself. §5 and the I2 row now say which half does what. | TestClaimSkipsRowsLockedByAnotherTransaction, TestReapSkipsRowsLockedByAnotherTransaction; guards "claims skip locked rows instead of waiting", "reapers skip locked rows instead of waiting" |
| F2 | minor | The text bound had no test at its edge: with `len(s) < limit` all tests passed, and an error of exactly 1,024 bytes made Finish panic (index out of range), the failure the bound exists to prevent. The bounds were only in §5; the outcome was bounded silently. | Yes | A test at the edge: exactly the bound, one byte over, a two-byte character across the bound, for the error, the outcome and the message id. The bounds are in the §8 table; §5 names the outcome. | TestFinishStoresTextAtItsBound; guards "the text bound is inclusive", "text is cut at a character boundary", "the outcome text is bounded", "stored text is bounded" |
| F3 | minor | `TestAttemptsNeverGoDown` went idle: with its fixed seed all rows were `sent` by step 380; 39 claims in all; about 5 cancel steps. §11 said "600 random steps of all five operations". | Yes (instrumented copy) | A row that becomes final is replaced by a new one; the seed changes per run and is logged; the test logs how often each operation took effect and fails if one never did. One local run: 97 claims, 26 results stored, 67 rows reaped, 37 requeued, 3 cancelled (`go test -count=1 -v -run TestAttemptsNeverGoDown ./internal/store`). §11 says what the test does. | TestAttemptsNeverGoDown; guard "a claim is one attempt (I7)" |
| F4 | nit | Cancel and Requeue read the status in a second statement after the update refused; a Finish in between could make Cancel report a status that it would have accepted. | Reasoned | Both lock the row with `SELECT ... FOR UPDATE`, then update, in one transaction; the update's own `WHERE` still decides. §5 says so. | By construction: the reported status is read under the lock the update runs under. No test: the race needs a Finish between two statements of one transaction, which the lock now excludes. TestCancelOnlyFromWaitingStatuses and TestRequeueRaisesMaxAttemptsByTheBudget cover the outcomes. |
| F5 | nit | Reap writes `last_error = 'lease expired'`, which §5 did not say; a reaped attempt leaves no attempt row. | Yes (by reading) | §5 states both: the lost attempt is a missing attempt number in the history. The reap test checks both. | TestReapReturnsExpiredRowsToPendingOrFailed |
| F6 | nit | The own-timeout test exercised only Claim. | Reasoned | The test runs all five operations against a held table lock, each with a 300 ms timeout. | TestQueueWritesHaveTheirOwnTimeout; guard "writes have their own timeout" |
| F7 | nit | `bounded` cleaned the whole input before cutting it: a 100 MB body would be copied twice. | Reasoned | The input is cut to the bound plus 4 bytes first, then cleaned and cut at a character boundary. | TestFinishStoresTextAtItsBound, TestFinishStoresAnyErrorTextWithinItsBound (results unchanged). No guard: it is a cost, not a guarantee. |
| F8 | nit | The testkit variables, once exported in a shell, would make every `Setup` flush the Redis they name, and parallel packages flush each other's. | Reasoned | The runner always starts its own servers and passes `GUARDS_POSTGRES_URL` and `GUARDS_REDIS_URL` only to the `go test` processes it runs, through `cmd.Env`. The testkit comment says they must never name a Redis that holds anything else. | `make guards` (every integration guard runs through these variables) |
| F9 | nit | `TestClaimsInPriorityTurnTakeHigherPrioritiesFirst` tests the loop inside the test; the queue contributes only the priority filter. | Yes (by reading) | Kept. §11 lists it as the queue's part of I9, next to TestClaimTakesOnlyItsChannelAndPriority; the loop itself is session 5's. | |

## Claims the reviewer reproduced

- A3: a claim of 10 reads 4 index pages in 0.125 to 0.301 ms; an empty claim reads 3 pages in
  0.027 to 0.037 ms (three runs). The `IN (SELECT ...)` form: Seq Scan with Hash Join, 32.57 ms.
- The reap statement uses `notifications_reap_idx` under both plan modes, about 1 ms with
  200,000 live leases.
- 32 of 32 queue guards real within its full run of 85.

## Questions the reviewer asked, and the answers

1. *Which test fails if `FOR UPDATE SKIP LOCKED` becomes `FOR UPDATE`?* Now
   TestClaimSkipsRowsLockedByAnotherTransaction and TestReapSkipsRowsLockedByAnotherTransaction.
2. *Would the status filter in the outer UPDATE make Claim safe without the lock?* It would
   stop a double claim, but a claim could then come back short. The lock stays the single
   enforcement, and its guard is real.
3. *An error of exactly 1,024 bytes?* Stored whole (TestFinishStoresTextAtItsBound).
4. *How will the API show an attempt lost to an expired lease?* As a missing attempt number,
   and `last_error` says "lease expired" until the next result (§5). Session 6 shows it.
5. *How many steps of the random test reach a waiting row?* Rows that become final are
   replaced; the counts are logged per run.
6. *What stops a parallel run from flushing a shared Redis?* The variables are now set only
   by the runner, for its own servers, in its children's environment.
7. *Will session 4 bound the error text before Finish?* `bounded` cuts first now, so the
   queue's cost does not depend on it; session 4 should still not read an unbounded response
   body. Listed as open in the session record.
