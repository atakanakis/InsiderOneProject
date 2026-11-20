# Review of session 1: skeleton

- **Date:** 2026-11-20.
- **Reviewer:** a fresh Claude Opus instance, read-only, with the rules of
  [`.claude/agents/reviewer.md`](../../.claude/agents/reviewer.md). It was given the assignment
  text, the session's brief and definition of done in [`plan.md`](../plan.md), and the
  repository. It was not given the author's reasoning or conclusions.
- **Scope:** commits `01c20c1..5dcd712` (everything after `10ff025`, the last commit of
  session 0).
- **Runs:** once, after the code was green, as the plan requires. There was no second round.
  Each fix below is verified by its own named test and by `make guards`, not by another
  review.

## What the reviewer ran

- `go build ./...`, `go vet ./...`, `go test -race -count=1 ./...` (pass); the unit packages
  again with `-count=10` (stable); `make lint` (0 issues).
- `make guards` in a copy of the repository: 34 of 34 guards real (one first-run failure was a
  Docker API timeout caused by the reviewer pausing its own Compose stack; re-run with
  `-only`, it passed).
- The Compose stack under its own project name: endpoint probes, `docker compose pause` of
  Redis and of Postgres, `SHOW` of the database settings, `stop worker` (exit 0).
- `pg_isready` timing during the first start of Postgres, and a timing of the recorder's lock
  with 200,000 kept requests.
- Mutation experiments of its own in the copy (an extra status in the CHECK constraint).
- `gh run view 44703472757`: CI green on `5dcd712`, integration tests really ran.

## Verdict, as reported

The session delivers its brief. One major finding: `/readyz` does not answer within 2 s when
Redis hangs. Several minor ones: three documented numbers or claims did not match the running
system.

## Findings and resolutions

| # | Severity | Finding | Verified by the reviewer | Resolution | Verified by |
|---|---|---|---|---|---|
| F1 | major | Redis clients ignore context deadlines: `redis.ParseURL` leaves `ContextTimeoutEnabled` off, and go-redis then uses a background context on the socket. `/readyz` answered after 5.0 s with Redis paused, against a stated 2 s. The test used a fake check that honours its context, and the real-dependency test used a refused port, which fails at once. Same class in `cmd/worker`, `testkit` and the test's own client. | Yes: 5.01 s, three of three | **Class fixed.** New package `internal/redisclient` is the one place a client is built, with deadlines on. Every client in the repository now comes from it, and a test fails if any Go file outside the package builds one. The readiness test now runs the real pgx and Redis clients against a server that accepts connections and never answers. Architecture §8 and §14 updated. | TestRedisClientHonoursContextDeadlines (5.00 s with the option off, reproduced locally before the fix), TestRedisClientsAreBuiltOnlyHere, TestReadyzAnswersWithinItsTimeoutWhenRealClientsHang; guards "redis: clients honour context deadlines", "redis: readyz with real clients is bounded" |
| F2 | minor | Architecture §5 said the database is configured with `idle_in_transaction_session_timeout`; `SHOW` returned 0. No session owned it. | Yes | Set per session by `store.Connect` (10 s for every service session), so it holds on any Postgres, not only in Compose. The API's sessions also carry the 5 s statement timeout of §8. §5 now states the value and why. | TestServiceSessionsCarryTheirTimeouts (checks behaviour: a long statement is cancelled, an idle transaction is ended), TestAPIDatabaseSessionsHaveAFiveSecondStatementTimeout; three guards |
| F3 | minor | The header bound was about 68 KiB, not 64 KiB: Go reads 4096 bytes past `MaxHeaderBytes`. The test sent 63 KiB and 72 KiB, so it never touched the edge. | Yes: 67,000 bytes accepted | The server is given the bound minus Go's slack. The test now writes raw requests of exactly 64 KiB − 64 and 64 KiB + 64 bytes, request line included. §8 says "request line included". | TestRequestHeadersOverTheBoundAreRefused; guard "api: request header size bound" now restores Go's slack as the mutation |
| F4 | minor | The test of the CHECK constraints claimed "exactly" but rejected only a sample; an extra status passed it. Same gap for channel, priority and template channel. | Yes: `'archived'` added, test passed | A second test reads each CHECK from `pg_constraint` and compares its list with `domain`, column by column. The behavioural test is kept. | TestColumnChecksAllowExactlyTheDomainValues; four guards that each add a value the old sample did not contain |
| F5 | minor | The Postgres health check (`pg_isready` on the Unix socket) can pass before Postgres accepts TCP connections during its first start; `migrate` connects over TCP once and has `restart: "no"`, so the stack could stall. | Window measured (socket ready at 0.71 s, TCP at 0.92 s); the stall reasoned | The health check uses `pg_isready -h 127.0.0.1`. | `make up` from an empty volume (session record) and the CI Compose job |
| F6 | minor | The stop-grace relation could be broken from `.env`: raising `SHUTDOWN_TIMEOUT` above 30 s passed both `Worker.Check` and the test that read the Compose file. | Reasoned | The grace period is now a worker setting, `STOP_GRACE_PERIOD`, checked by `Worker.Check` (`> SHUTDOWN_TIMEOUT`). Compose takes the container's `stop_grace_period` and the worker's variable from one `${STOP_GRACE_PERIOD:-30s}`, so they cannot differ. §13 updated: two relations, not three, remain outside the worker's reach. | TestShutdownTimeoutMustEndBeforeTheStopGracePeriod, TestComposeGivesTheWorkerItsStopGracePeriod; two guards |
| F7 | minor | The recorder took its arrival stamp under the lock that `Requests()` holds while it clones and sorts; with 200,000 kept requests that was 8 to 29 ms per call, next to a 50 ms margin. The mock provider kept headers although it claimed to keep "no bodies" for a bounded memory. Trimming was in record order, not `Seq` order. | Lock time measured; the effect on peaks reasoned | The stamp is taken before the lock; `Requests()` copies under the lock and sorts outside it; trimming sorts by `Seq`. `DropBodies` became `Lean`, which drops headers too; the mock bounds headers at 64 KiB. | TestRecorderStampIsNotDelayedByReaders (a reader holds the lock for 300 ms), TestRecorderKeepsOnlyTheLastRequestsWhenBounded; guard "recorder: stamped before the lock" |
| F8 | nit | Several checks were not in `make guards` although their tests catch them: the batch key's uniqueness, the other CHECKs, the foreign keys, the reap index, the recorder's delay, close and handshake injection. | Yes, for the batch key | All added. The list grew from 34 to 53 guards. | `make guards` |
| F9 | nit | `testkit.Main` started Postgres and Redis for every test of a package, unit tests included: unit guards took 8 to 15 s and a Docker hiccup could fail them. | Yes | Containers start on the first `testkit.Setup` call and are removed when the package's tests end. | Unit guards in `internal/api` now run without containers (times in the session record) |
| F10 | nit | `TestNonPositiveSettingsAreRefused` left out four settings; `cmd/api` and `cmd/migrate` refused to start when a worker-only setting in the shared `.env` was malformed. | Yes | The test covers every setting. `config.Load` reads only the shared settings and `config.LoadWorker` the timing budget, so only the worker refuses a broken budget. | TestNonPositiveSettingsAreRefused, TestSharedSettingsIgnoreMalformedWorkerSettings |

## Claims and reality

| Claim | Reviewer's measurement | Now |
|---|---|---|
| §8: readiness answers within 2 s per dependency | 5.0 s with Redis paused | Fixed (F1) |
| §8: request headers bounded at 64 KiB | 67,000 bytes accepted | Fixed (F3) |
| §5: the database has `idle_in_transaction_session_timeout` | 0 | Fixed (F2) |
| §4: the CHECKs accept exactly the domain values | an extra status passed | Fixed (F4) |
| mock provider: bounded memory, no bodies | headers kept, up to 200,000 entries | Fixed (F7); the comment now says "at most twice 100,000 entries, without body or headers" |
| README still says nothing runs | expected at the end of the session | Updated with the session record |
| [Design review](00-design-review.md), round 1, F2: "Finish requires processing, except for a delivered result" disagrees with §5 | reported as a disagreement | Checked: round 2, finding 7 of the same record removed that exception, so the record shows the change. §5 and the record agree. No change. |
| Commit `47e26d4`: 34 guards real | reproduced | Now 53 |

## The reviewer's hard questions

1. **Which session owns `idle_in_transaction_session_timeout` and the API's statement timeout?**
   Session 1, after this review. Both are set per session by `store.Connect` and tested by
   behaviour (F2).
2. **Will every Redis client be built in one place that respects deadlines, before
   `limiter.Wait` is written?** Yes: `internal/redisclient`, enforced by
   TestRedisClientsAreBuiltOnlyHere (F1).
3. **Why does the API stop being ready when Redis is down?** It should not. The API needs only
   Postgres to accept requests, and events are best-effort (§9). The design was wrong here and
   was changed first (§8): Redis is reported by `/readyz` but no longer decides
   (TestReadyzStaysReadyWhenOnlyRedisIsUnreachable). The worker still needs Redis, because the
   limiter fails closed; that is the worker's concern, not the API's readiness.
4. **Which `ORDER BY` will the list cursor use?** `created_at DESC, id ASC`, the order of the
   existing index, with the cursor comparing in that mixed order. Written into §4 for session
   6. A different order would need a new migration.
5. **What limits the gap between a request's real arrival and its stamp?** The stamp is the
   first line of the handler, before the lock, the body and any delay. What remains is the
   time from the headers being parsed to the handler starting, inside Go's server. Tests bound
   it loosely (under 100 ms, TestRecorderStampsOnArrivalNotOnResponse,
   TestRecorderStampsBeforeTheBodyIsRead, TestRecorderStampIsNotDelayedByReaders). The A4
   measurement in session 4 measures from permit to stamp, so this gap is included in it and
   can only make the measured delay larger, not hide one.
6. **How is the grace period checked when both values can change from `.env`?** The worker is
   told the grace period and checks it (F6).

## Definition of done, as the reviewer found it, and after the fixes

| Item | Reviewer | After the fixes |
|---|---|---|
| `make test` (-race), `make lint`, CI green | Met | See the session record for the final commit's CI run |
| Named tests in §11 / §12 | Met as written | Met; readiness now proven with real clients |
| Guards in `make guards`, each turning a test red | Met for 34; some missing | 53 listed |
| Each §8 bound touched has a test at the bound | Not met (headers, readiness) | Met |
| Numbers in documents have their command | Not met (64 KiB, 2 s) | Met |
| Review recorded | Not yet | This file |
| Session record and status table | Not yet | Done with the session record |
