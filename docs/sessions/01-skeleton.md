# Session 1: skeleton

- **Date:** 2026-11-20.
- **Lead:** Claude Opus. **Reviewer:** one fresh Claude Opus instance, run once after the
  code was green ([review record](../reviews/01-session-1-review.md)).
- **Brief:** [plan.md, "1. Skeleton"](../plan.md#1-skeleton).

## What was built

| Part | What it is | Evidence |
|---|---|---|
| Go module | `github.com/atakanakis/InsiderOneProject`, Go 1.27.4 | `go.mod` |
| `internal/domain` | Channels, priorities, the seven statuses and the allowed transitions of architecture §3 | TestStatusTransitionsFollowTheLifecycle walks all 49 pairs |
| `internal/config` | Every setting of §13 from the environment; `Worker.Check` names every broken relation. Shared settings (`Load`) and the worker's budget (`LoadWorker`) are read separately | One test per relation, at its bound (§13 table) |
| `internal/store` | `Connect` (with per-session timeouts), `Migrate` (goose, advisory lock), four append-only migrations | §4 test table; TestMigrationsAreAppendOnly; TestServiceSessionsCarryTheirTimeouts |
| `cmd/migrate` | Applies the migrations and exits | Compose `migrate` service exits 0, `version 4` in its log |
| `internal/observability` | JSON logs; the correlation id travels in the context and stays at the top level of a line | TestLogLinesCarryTheCorrelationIDFromTheContext, TestCorrelationIDStaysTopLevelUnderGroups |
| `internal/api`, `cmd/api` | `/healthz`, `/readyz` (Postgres decides, Redis is reported), the correlation id middleware, request header bounds | §8; TestAPIServesReadinessAndStopsCleanly |
| `internal/redisclient` | The one place a Redis client is built, with context deadlines on (added after the review) | TestRedisClientHonoursContextDeadlines, TestRedisClientsAreBuiltOnlyHere |
| `cmd/worker` | Checks the budget before connecting to anything (exit 2 if broken), connects to Postgres and Redis, stops on SIGTERM with exit 0 | TestWorkerRefusesToStartWithALeaseShorterThanSendTimeoutPlus30s, TestWorkerBinaryExitsZeroOnSIGTERM |
| `internal/recorder` | The recording provider server: stamps each request on arrival; injects status, headers, delay, closed connections, a slow TLS handshake, server-side closing of idle connections; offers HTTP/2 over TLS | 15 tests in `internal/recorder` |
| `cmd/mockprovider` | The recorder as the Compose stack's provider, with `/stats` (most requests in any one-second window, per channel) | TestMockProviderAnswersTheContractAndCountsPeaks |
| `internal/testkit` | `postgres:17-alpine` and `redis:7-alpine` with testcontainers, started on first use; `EmptyDatabase`; `Recorder` | Used by every integration test |
| `docker-compose.yml`, `Dockerfile` | Postgres, Redis, migrate, API, worker, mock provider | "Measured" below |
| `Makefile` | `test`, `test-unit`, `lint`, `guards`, `check-docs`, `up`, `down` | |
| `.github/workflows/ci.yml` | lint; test + check-docs; guards + `git diff --exit-code`; Compose stack with health probes and a clean worker stop | CI runs listed below |
| `tools/guards` | `make guards`: for each guard, the named test passes, the mutation compiles, the named test fails by name, the file is restored | 53 guards |
| `tools/testnames` | Fails when a test named in `docs/` or `README.md` does not exist | TestTestNamesCheckReportsATestThatDoesNotExist, TestEveryTestNamedInTheDocsExists |

### Carried over from the first version

Each was read in `InsiderOneProjectDeneme` and adapted (module path, names):

- `Dockerfile` and `.dockerignore` (one image, four binaries).
- `.golangci.yml` (linters and exclusions), and the `lint` target's golangci-lint v2.15.0 in
  Docker.
- The shape of `docker-compose.yml` (one image built by `migrate`, health checks,
  `service_completed_successfully`).
- The CI job layout (`actions/checkout@v5`, `actions/setup-go@v6`,
  `golangci-lint-action@v8`).
- The testcontainers setup of `internal/testutil/containers.go`, now `internal/testkit`.
- `store.Connect`'s UTC time codec.
- Module versions from its `go.mod` (pgx 5.13.0, goose 3.30.1, go-redis 9.23.0,
  testcontainers 0.45.0, uuid 1.6.0).

Nothing of its queue, limiter or delivery code was carried over.

## How the tests came first

For each guard, the test was written before the code or together with a stub, and the guard
was then disabled by hand and by `make guards` to see the named test fail. The 49-pair
lifecycle test, for example, was run against the transition table with the reaper's
`processing → pending` edge removed, and failed on exactly that pair, before the table was
committed.

## What was measured

| What | Result | Command |
|---|---|---|
| Tests | 75 test functions; all pass with the race detector | `go test -race -count=1 ./...` (count: `go test -list '.*'` per package) |
| Lint | 0 issues | `make lint` |
| Guards | 53 of 53 real | `make guards` (see the end of this file for the time) |
| Test names in the documents | all exist | `make check-docs` |
| The stack starts | migrate exits 0 (`version 4`); API, worker and mock provider healthy | `make up` |
| `/healthz`, `/readyz` | 200 `{"status":"ok"}`; 200 `{"postgres":"ok","redis":"ok","status":"ready"}`, with the sent `X-Correlation-ID: demo-1` echoed | `curl -si localhost:8080/healthz`, `curl -si -H 'X-Correlation-ID: demo-1' localhost:8080/readyz` |
| Mock provider | 202 with `messageId`, `status`, `timestamp`; `/stats` counts it | `curl -X POST localhost:9090/send -d '{...}'`, `curl localhost:9090/stats` |
| Worker refuses a broken budget | exit 2, `LEASE (40s) must be at least SEND_TIMEOUT (15s) + 30s for the result write` | `docker compose run --rm --no-deps -e LEASE=40s worker` |
| Worker stops cleanly | exit 0, `worker stopped` in the log, 0.16 s | `time docker compose stop worker`; `docker compose ps -a worker` |
| Redis client deadline (before and after the fix) | ping of a silent server returned after 5.00 s with a 200 ms deadline when deadlines were off; within the deadline when on | `go test ./internal/redisclient` with the option flipped by hand |

The Compose measurements of the first build are from before the review. The stack was
started again after the fixes; see "After the review" below.

## What the review found

One major and nine minor findings or nits; all fixed. The record, with each resolution and
the test or guard that verifies it, is [01-session-1-review.md](../reviews/01-session-1-review.md).
In short:

- **Redis clients ignored context deadlines** (major). `/readyz` took 5 s against a paused
  Redis while the design said 2 s. Fixed as a class: one constructor for every Redis client,
  a test that forbids any other, and a readiness test with the real clients against a server
  that never answers. This matters most in session 3, where the limiter waits at the gate on
  the same clients.
- Three documented numbers did not match the system (the 64 KiB header bound, the
  "exactly" of the CHECK test, the Postgres idle-transaction timeout of §5). Each now has a
  test at the stated value.
- The stop-grace relation could be broken from `.env`; the worker now knows the grace period
  and checks it.
- The recorder's arrival stamp could wait for a lock held by a reader; the stamp now comes
  before the lock.

## What changed in the design

All in [architecture.md](../architecture.md), before the code that follows them:

- **§8:** bounds for the `X-Correlation-ID` header and for request headers (64 KiB, request
  line included, within 5 s).
- **§8, after the review:** `/readyz` is decided by Postgres alone; Redis is reported. The API
  needs only Postgres to accept requests, and events are best-effort (§9); the previous rule
  would have taken every API instance out of service during a Redis outage.
- **§8, §14, after the review:** every Redis client is built in `internal/redisclient` with
  context deadlines on.
- **§5, after the review:** every service session sets `idle_in_transaction_session_timeout`
  to 10 s; the API's sessions set the 5 s statement timeout of §8.
- **§13, after the review:** `STOP_GRACE_PERIOD` is a worker setting and a checked relation;
  two relations, not three, remain outside a single worker's reach.
- **§14:** `internal/recorder` (so that `cmd/mockprovider` does not link testcontainers),
  `internal/redisclient`, `tools/guards`, `tools/testnames`.
- **§4:** the list order the existing index serves, for session 6.
- **§3, §4, §8, §11, §13:** test names written in.
- **Process** ([plan.md](../plan.md), [CLAUDE.md](../../CLAUDE.md)): one independent review
  per session, run once after the code is green; fixes are verified by tests and
  `make guards`, not by a second review (the candidate's decision during this session).

## What is still open

- `make guards` restarts Postgres and Redis for every guard of an integration package (about
  10 s each). It is acceptable now; if the list grows a lot, the runner could group guards by
  package.
- The mock provider has no fault injection by environment yet (failure rates, 429s). The
  recorder supports it; `cmd/mockprovider` will expose it when sessions 5 and 8 need it.
- `cmd/migrate` connects once, with no retry. With the TCP health check of Postgres this is
  enough for Compose; a deployment without health checks would need a retry.
- Two commits of this session do not build on their own: `42bb197` and `29b99d7` import
  `internal/config`, which was committed after them, in `34fe04d`. Every later commit builds
  and vets (checked by checking out each commit of `10ff025..HEAD` in a worktree and running
  `go vet ./...`). They were already on `main`, so history was not rewritten.
- Timing thresholds in the recorder's tests (100 ms) are loose on purpose, so that CI does
  not flake; the precise permit-to-arrival measurement is A4, in session 4.

## After the review

Measured again after the fixes, on the author's machine:

| What | Result | Command |
|---|---|---|
| Guards | 53 guards, 53 real, 0 failed; the times printed per guard add up to 246 s | `make guards` |
| Unit guards in `internal/api` | 3 to 4 s each, where they took 8 to 15 s while every test started containers (review F9) | `make guards`, the per-guard times |
| Lint, tests, test names | 0 issues; all pass; all exist | `make lint`, `go test -race -count=1 ./...`, `make check-docs` |
| The stack from an empty volume | healthy in 28.6 s | `make down && time make up` |
| `/readyz` with Redis paused | 200, `"redis":"unreachable"`, in 2.006 s (the review measured 5.01 s and 503 before the fix) | `docker compose pause redis; curl -w '%{http_code} %{time_total}' localhost:8080/readyz` |
| `/readyz` with Postgres paused | 503 in 2.003 s | `docker compose pause postgres; curl ...` |
| `/readyz` with Redis stopped | 200, `"redis":"unreachable"` | `docker compose stop redis; curl localhost:8080/readyz` |
| Worker told a grace period shorter than its shutdown timeout | exit 2, `STOP_GRACE_PERIOD (20s) must be longer than SHUTDOWN_TIMEOUT (25s), ...` | `docker compose run --rm --no-deps -e STOP_GRACE_PERIOD=20s worker` |
| Worker stops cleanly | exit 0 in 0.18 s | `time docker compose stop worker`; `docker compose ps -a worker` |

CI on the final pushed commit: see the commit list on GitHub; the run is checked with
`gh run view` before the session is closed.
