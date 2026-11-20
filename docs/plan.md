# Build plan

The system is built in sessions. One session builds one part of the
[architecture](architecture.md), from the brief below, with a fresh context, and ends with
that part working, tested, reviewed and pushed. The reason is lesson 14 in
[lessons.md](lessons.md): the first version was built in one long session, and details of
early parts were lost by the time later parts were built.

## Status

| # | Session | Status |
|---|---|---|
| 0 | Lessons, architecture, design review, this plan | **done** (2026-11-20) |
| 1 | Skeleton: module, config, migrations, Compose, CI, health | **done** (2026-11-20, [record](sessions/01-skeleton.md)) |
| 2 | Queue: the five operations | **done** (2026-11-20, [record](sessions/02-queue.md)) |
| 3 | Rate limiter | **done** (2026-11-20, [record](sessions/03-ratelimit.md)) |
| 4 | Provider client and the send gate | **done** (2026-11-20, [record](sessions/04-provider.md)) |
| 5 | Dispatcher, breaker, reaper; fault tests | **done** (2026-11-20, [record](sessions/05-dispatcher.md)) |
| 6 | API: notifications, batches, templates, idempotency | next |
| 7 | Events, WebSocket, metrics, tracing, alerts | |
| 8 | Hardening: full independent review, soak, load test | |
| 9 | Walkthrough and final documents | |

## How a session runs

Roles:

- **The candidate** decides scope and trade-offs and reads the record of every session.
- **A coordinating agent** (the conversation that wrote the design) starts each session in a
  new conversation when the previous one has finished, and checks that its result is pushed
  and green before starting the next.
- **The lead** (the main agent of the session; Claude Opus for the build sessions, each in a
  new conversation) builds from the architecture, writes or directs the code, checks every
  claim against the repository, and writes the session record.
- **Builder subagents** (Claude Opus) implement well-specified pieces from the brief when the
  lead delegates them.
- **A reviewer subagent** (Claude Opus, a fresh instance, read-only) reviews the session's
  change with no knowledge of the author's reasoning. It never reviews its own work. It runs
  once per session.

Steps:

1. Read `architecture.md`, this plan and the previous session's record. Read nothing else of
   the earlier sessions unless the brief names it.
2. **Tests of the session's invariants first**, then the code.
3. Run everything in "Definition of done".
4. One independent review per session, run once, after the code is written and green. There
   is no second round and no review of the fixes. The lead fixes the findings as classes, not
   as single reproductions; a finding that shows the design is wrong changes `architecture.md`
   first. Each fix is verified by its own named test and by the guard check (`make guards`),
   not by a second review.
5. Write the session record in `docs/sessions/NN-name.md`: what was built, what was measured,
   what the review found, what changed in the design, what is still open.
6. Update the status table above, commit, push.

## Definition of done (every session)

- [ ] `make test` passes with `-race`; `make lint` passes; CI is green on the pushed commit.
- [ ] Every invariant and assumption the session owns has a named test, and the name is
      written into the table of `architecture.md` §11 or §12.
- [ ] Every guard the session added is in `make guards`, and disabling it turns a named test
      red. A guard that is disabled without any test failing is a failed session.
- [ ] Every bound of `architecture.md` §8 that the session touches has a test at the bound.
- [ ] Numbers written into documents come from a command that is written next to them.
- [ ] The session's one independent review (run once, after the code was green) is recorded in
      `docs/reviews/`, with each finding's resolution and the test or guard that verifies it.
- [ ] The session record exists and the status table is updated.
- [ ] The dispatcher loop still fits on one screen (from session 5 on).

## Session briefs

### 1. Skeleton

**Goal:** `docker compose up` starts Postgres, Redis, a migrated database, an API that answers
`/healthz` and `/readyz`, a worker that starts and stops cleanly, and a mock provider. CI runs
lint and tests on every push.

- Go module, `internal/config` with every setting of architecture §13 and the relation checks
  (a worker with a lease shorter than send timeout + 30 s refuses to start).
- `internal/domain`: channels, priorities, statuses, the allowed status transitions.
- Migrations for the four tables and their indexes (architecture §4); `cmd/migrate`.
- `internal/observability`: JSON logging with the correlation id carried in the context.
- `internal/testkit`: test Postgres and Redis (testcontainers), and the **recording provider
  server**, which stamps every request when it arrives and can inject status codes, latency,
  slow connection setup and closed connections.
- `Makefile` (`test`, `test-unit`, `lint`, `guards`, `up`), GitHub Actions, `make guards`
  runner (empty list for now), the CI script that checks every test named in `docs/` exists.

**Out of scope:** any queue, limiter or delivery logic.

### 2. Queue

**Goal:** the five queue operations of architecture §5, proven on a real Postgres.

Tests that must exist (names may differ; meaning may not):

- Two workers claiming at once never get the same row (I2), under many concurrent claims.
- A claim returns rows in priority order across calls and in `ready_at` order within one;
  rows that are not due are not returned.
- The claim's plan uses the claim index with no sort and no full scan, under a custom plan and
  under a forced generic plan, with many tied and many future rows (A3).
- Finish from a stale attempt changes nothing; Finish on a row that is no longer `processing`
  changes nothing; a repeated Finish adds no second attempt row (I3).
- `attempts` never decreases, across claim, finish, reap and requeue; requeue sets
  `max_attempts = attempts + budget` (I7).
- Reap returns expired rows to `pending`, sends rows at their last attempt to `failed`, and
  two reapers at once do not conflict (I4).
- Cancel succeeds only from `scheduled`, `pending`, `retrying`; a cancelled row cannot be
  revived by a late result.
- A claim, finish or reap in flight is not cut off by a cancelled caller context.

### 3. Rate limiter

**Goal:** `ratelimit.Available` and `ratelimit.Wait` on Redis.

- The Lua sliding-window script, on the Redis clock, window = 1 s + margin (the first
  version's script may be carried over, with its tests; say so in the session record).
- Many concurrent callers on several connections never get more than the limit in any window
  of 1 s + margin (A6).
- `Wait` returns when a permit frees, respects its context, and returns an error (not a
  permit) when Redis fails or the script fails.
- `Available` changes nothing. A test covers "the read works and the script fails".

### 4. Provider client and the send gate

**Goal:** `provider.Send(ctx, message, gate)` with invariant I8, proven at the recording
server through real connections.

- The gate runs once per send, after the connection is ready and before the write.
- A refused send never reaches the server, on new and on reused connections (A1), in a test
  with thousands of refusals.
- A dead reused connection is not replayed: one hook call, at most one request received (A2).
- Redirects are not followed; a 301 is a response with status 301.
- HTTP/1.1 against a TLS server that offers HTTP/2 (build a fresh `Transport` for this test).
- One deadline covers connect, gate, write and response; a gate that waits past the deadline
  takes no permit afterwards.
- The time from permit to arrival, measured at the recording server on new and on reused
  connections, against the 50 ms margin (A4).
- `Result` carries what was observed (response or not, refused or not, why), and nothing else.

### 5. Dispatcher, breaker, reaper

**Goal:** architecture §6 working end to end with two or more workers.

Write first, before the dispatcher:

- The randomised fault test: random claim stalls, failing result writes, provider errors,
  latency, slow dials and closed connections, through the real provider client. After each
  scenario it checks I1 (at the recording server), I5 and "no notification accepted twice".

Then the dispatcher, `Classify`, retry policy, breaker and reaper, with tests for:

- Every row of the verdict table, including "a response always wins" (I6).
- 404 and 401 from the provider open the breaker and do not drain the queue into `failed`.
- A limiter whose script fails while `Available` works: the breaker opens; no row reaches
  `failed` without a send.
- The half-open breaker lets exactly one row through; a claim that finds nothing leaves no
  probe marked; a refused probe leaves the breaker half-open.
- A failed result write is retried and the message is not sent again.
- A worker killed mid-send: the row comes back through the reaper and is delivered (I4).
- Shutdown waits for sends in flight and does not cancel them.
- High priority overtakes a backlog of lower priorities (I9).
- The wait at the gate and the throughput with 1, 3, 6 and 12 dispatchers on one channel (A5).

### 6. API

**Goal:** architecture §8, with OpenAPI.

- Every endpoint in the table, with the idempotency rules (I10) under concurrent identical
  requests.
- A test at every bound of the bounds table, including the size of error responses (I11).
- Templates: compile once per request, length before rendering.
- The list endpoint returns summaries, with cursor pagination.
- Optional API keys.

### 7. Events and observability

**Goal:** architecture §9 and §10.

- Status events over Redis to WebSocket clients, with the connection and slow-client bounds.
- `GET /v1/metrics`, Prometheus metrics, the correlation id on every worker log line of a
  notification, traces that join the API request to the delivery.
- Alert rules, each with a `promtool` test; CI fails when a rule has no test.

### 8. Hardening

**Goal:** an independent review of the whole system, and measurements.

- A full review by a fresh reviewer against the assignment, with fault injection on a running
  stack (paused Postgres and Redis, a killed worker, a slow and failing provider).
- A long run of the randomised fault test; its pass rates in the README, with the command.
- A load test of a burst against the running stack: throughput, the peak per second seen by
  the mock provider, the latency of a high-priority message during the burst.
- `make guards` over the complete list.

### 9. Walkthrough and final documents

**Goal:** a reader can follow one message through the code, and the README states only what
the repository proves.

- `docs/walkthrough.md`: one notification from the HTTP request to the provider and back,
  with file and function names.
- README: setup, architecture overview, API examples, the measurements of session 8, the list
  of what is left out and why.
- The journey document brought up to date.
