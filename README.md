# Event-Driven Notification System

[![CI](https://github.com/atakanakis/InsiderOneProject/actions/workflows/ci.yml/badge.svg)](https://github.com/atakanakis/InsiderOneProject/actions/workflows/ci.yml)

A Go service that accepts notifications (SMS, email, push) over an HTTP API and delivers them
to an external provider: at most 100 messages per second per channel, high, normal and low
priority, retries, scheduling, templates, and live status over a WebSocket. PostgreSQL holds the
notifications and is the queue; Redis holds the shared rate limit and carries status events.

```sh
git clone https://github.com/atakanakis/InsiderOneProject.git && cd InsiderOneProject
make up      # Postgres, Redis, migrations, the API, two workers and a mock provider
```

**Where to start reading:** [the walkthrough](docs/walkthrough.md) follows one notification
through the code, file by file. The design is [docs/architecture.md](docs/architecture.md).
[Reading paths](#reading-paths) below has a 10-minute and a 1-hour route.

**Status:** complete. All nine build sessions are done (the table is at the end); the last one
wrote the walkthrough and these documents and added no feature.

## Run it

Needs Docker with the Compose plugin, `make` and `curl`. Go 1.27.4 (the version in `go.mod`) is
needed only for the tests and the tools: `make test`, `make check-docs`, `make guards`,
`make alerts`, `make e2e`, `make load`.

```sh
make up                          # builds the image once, starts the stack, waits until healthy
curl -s localhost:8080/readyz    # {"postgres":"ok","redis":"ok","status":"ready"}
```

`docker compose up --build` does the same in the foreground. The API listens on port 8080, the
mock provider on 9090. `make down` stops the stack and deletes its data. On the first start,
Compose looks for the image `insider-notifications:local` in a registry before it builds it,
and prints "pull access denied" for it; that is expected, and the build follows.

## Try the API

Every command below was run on a fresh clone, in this order. Under each is what it answered, cut
where it says `...`, with the HTTP status first; add `-i` to a command to see the status. Ids
are random: use the ones your own answers give.

```sh
# Create one notification. The idempotency key makes a repeat safe. A single notification takes
# it in the body, as here; a batch takes it in the Idempotency-Key header (the header on a
# single create is answered 400, with a message that says so).
curl -s localhost:8080/v1/notifications -H 'Content-Type: application/json' \
  -d '{"recipient":"+905551234567","channel":"sms","content":"Your code is 4242","priority":"high","idempotencyKey":"order-42"}'
# 201 {"id":"86538422-9238-46e2-8e2b-d7c0a228ca8d","recipient":"+905551234567","channel":"sms",
#      "content":"Your code is 4242","priority":"high","status":"pending","attempts":0,"maxAttempts":6,...,
#      "correlationId":"b3267ee3-...","attemptHistory":[],"attemptHistoryTotal":0}

# Its status, with the attempt history. An idle worker looks for work every 200 ms, so it is
# delivered within about 0.2 s; asked sooner, it is still pending.
curl -s localhost:8080/v1/notifications/<id>
# 200 {"id":"86538422-...","status":"sent","attempts":1,...,"sentAt":"...","providerMessageId":"4226e5ea-...",
#      "attemptHistory":[{"number":1,"startedAt":"...","durationMs":2,"statusCode":202,"outcome":"sent"}],...}

# The same request again: the same notification, created once. The key with another request: 409.
curl -s localhost:8080/v1/notifications -H 'Content-Type: application/json' \
  -d '{"recipient":"+905551234567","channel":"sms","content":"Your code is 4242","priority":"high","idempotencyKey":"order-42"}'
# 200 {"id":"86538422-...","status":"sent",...}
curl -s localhost:8080/v1/notifications -H 'Content-Type: application/json' \
  -d '{"recipient":"+905551234567","channel":"sms","content":"Another text","idempotencyKey":"order-42"}'
# 409 {"error":{"code":"idempotency_conflict","message":"the idempotency key was used with another request"}}

# A template, and a batch (up to 1000) that uses it, under one Idempotency-Key for the whole batch.
curl -s localhost:8080/v1/templates -H 'Content-Type: application/json' \
  -d '{"name":"welcome","channel":"email","body":"Hi {{.name}}, welcome!"}'
# 201 {"body":"Hi {{.name}}, welcome!","channel":"email","createdAt":"...","name":"welcome","variables":["name"]}
curl -s localhost:8080/v1/notifications/batch -H 'Content-Type: application/json' -H 'Idempotency-Key: signup-2026-11-22' \
  -d '{"notifications":[
        {"recipient":"ada@example.com","channel":"email","template":"welcome","variables":{"name":"Ada"}},
        {"recipient":"+905551234567","channel":"sms","content":"See you in 2030","scheduledAt":"2030-01-01T09:00:00Z","priority":"low"}]}'
# 201 {"batchId":"ef222110-8d01-498f-a43f-a1321a767dde","ids":["2bd37c46-...","73a09fb6-..."],"total":2}
curl -s localhost:8080/v1/batches/<batchId>
# 200 {"counts":{"cancelled":0,"failed":0,"pending":0,"processing":0,"retrying":0,"scheduled":1,"sent":1},...,"total":2}

# List: filters status, channel, batchId, createdFrom, createdTo; limit 1 to 100; newest first.
# The two items of one batch share their creation time and come in the order of their ids, so
# which one is on the first page differs from batch to batch.
curl -s 'localhost:8080/v1/notifications?batchId=<batchId>&limit=1'
# 200 {"items":[{"id":"2bd37c46-...","channel":"email","status":"sent",...}],"nextCursor":"AAZg0VJAGNwr03xG..."}
curl -s 'localhost:8080/v1/notifications?batchId=<batchId>&limit=1&cursor=<nextCursor>'
# 200 {"items":[{"id":"73a09fb6-...","channel":"sms","status":"scheduled",...}],"nextCursor":"..."}

# Cancel works from scheduled, pending or retrying; otherwise 409 with the status found.
curl -s -X POST localhost:8080/v1/notifications/<id of the scheduled SMS>/cancel
# 200 {"id":"73a09fb6-...","status":"cancelled"}
curl -s -X POST localhost:8080/v1/notifications/<id of the email>/retry      # retry is for failed ones
# 409 {"error":{"code":"wrong_status","message":"the notification is not in a status that allows this","status":"sent"}}

# Figures for a recent window (1m, 5m, 15m, 1h), per channel; Prometheus at /metrics.
curl -s 'localhost:8080/v1/metrics?window=15m'
# 200 {"channels":{"email":{"queue":{"pending":0,...},"oldestDueAgeSeconds":0,"attempts":{"total":1,...,
#      "byOutcome":{...,"sent":1}},"deliveredShare":1,"attemptDurationMs":{"p50":2,...},"deliveryLatencyMs":{...}},...}}
curl -s localhost:9090/stats     # what the mock provider received: at most 100 per channel in any second
# 200 {"accepted":2,"acceptedTwice":0,...,"maxInAnyOneSecondWindow":{"all":1,"email":1,"push":0,"sms":1},"received":2}
```

**Live status.** `GET /v1/ws?notificationId=<id>&batchId=<batchId>` is a WebSocket: up to 100
notification ids and 10 batch ids; the client sends nothing. It receives the current state
first, then every change. Events are best-effort and come from more than one process, so one
can arrive late, after a state that already includes it; `sent`, `failed` and `cancelled` say
the notification is done, and `GET /v1/notifications/{id}` is the truth. Followed from
creation, a notification scheduled 3 s ahead gave:

```
{"type":"status","id":"97da9252-...","status":"scheduled"}
{"type":"status","id":"97da9252-...","status":"processing"}
{"type":"status","id":"97da9252-...","status":"sent"}
```

Any WebSocket client works (for example `websocat`). Without one, `make e2e N=1` does it and
prints `websocket: notification <id>: scheduled, processing, sent`, then sends one notification
per channel through the API and waits until all are sent.

**The specification** is served at <http://localhost:8080/openapi.yaml>, with a viewer at
<http://localhost:8080/docs> ([`internal/api/openapi.yaml`](internal/api/openapi.yaml)).
**API keys** are off by default; with `API_KEYS=first-key-0123456789,second-key-0123456789 make up`
every `/v1` request needs one, in `Authorization: Bearer <key>` or `X-API-Key: <key>` (a key
has at least 16 characters, or the API does not start). `make e2e` and `make load` then need
it too: `make e2e KEY=first-key-0123456789`.

## Reading paths

**10 minutes.** The [design in brief](#the-design-in-brief) below; then the walkthrough's
[whole path on one screen](docs/walkthrough.md#the-whole-path-on-one-screen) and its
[invariant table](docs/walkthrough.md#where-each-invariant-is-kept); then the dispatcher's loop,
`(*Dispatcher).Run` and `(*Dispatcher).handle` in
[`internal/dispatch/dispatch.go`](internal/dispatch/dispatch.go).

**1 hour.**

1. [docs/walkthrough.md](docs/walkthrough.md), with the code open beside it (20 min).
2. [docs/architecture.md](docs/architecture.md), sections 2 to 7: the parts, the life of a
   notification, the queue, the dispatcher, the rate limit. Its first section says how to read
   it (20 min).
3. Architecture [§11, the invariants](docs/architecture.md#11-invariants) and
   [§16, what was left out](docs/architecture.md#16-deliberately-left-out) (10 min).
4. [docs/journey.md](docs/journey.md) and [docs/lessons.md](docs/lessons.md): why this is a
   second build and what the first one taught (10 min).

The records of each session are in [docs/sessions/](docs/sessions/), each independent review in
[docs/reviews/](docs/reviews/), and the plan in [docs/plan.md](docs/plan.md). They are the
process log, written by each session's lead agent and its reviewer; none of them is needed to
evaluate the system.

## The design in brief

```
   client ──HTTP, WebSocket──► API ──writes──► Postgres (notifications = the queue)
                                ▲                    ▲  │
                                │ status events      │  │ claim, result
                              Redis ◄── permit ── Worker: dispatcher per channel,
                     (rate limit, events)              provider client, reaper ──HTTP──► provider
```

1. The API validates a request and writes rows to Postgres; it never sends anything itself.
2. Postgres is both the record and the queue: a worker claims due rows with
   `FOR UPDATE SKIP LOCKED`, one priority at a time, highest first.
3. The worker takes a rate-limit permit from Redis at the last moment, when the HTTP connection
   is ready, so that nothing slow is left between the permit and the request but writing it. A
   process or machine that stalls there, or a step of Redis's clock, is what the window's margin
   may not absorb (architecture §7).
4. Every claimed row ends in exactly one place, where the result is classified from what was
   observed and written back; a worker that dies loses nothing, because its rows return when
   their lease expires.
5. Delivery is at-least-once, and the three cases in which a message can reach the provider
   twice are listed in [architecture §6](docs/architecture.md#6-the-dispatcher).

## Delivery and retry design

The assignment leaves this part to the candidate. The whole of it is in
[architecture §6](docs/architecture.md#6-the-dispatcher); this is the short form.

**One place decides what an attempt means:** `dispatch.Classify`
([`internal/dispatch/classify.go`](internal/dispatch/classify.go)), read top to bottom. A
response always wins over anything else that happened to the request.

| What was observed | The notification | The circuit breaker learns |
|---|---|---|
| A 2xx response | `sent` | success |
| 400, 413 or 422: the provider rejected this message | `failed`, no retry | success: the provider answered |
| Any other response (5xx, 429, 408, 401, 403, 404, ...) | retry with backoff; `Retry-After` honoured | failure |
| The rate limiter returned an error | retry in 1 to 2 s | failure |
| No permit came free in time | retry in 1 to 2 s | nothing |
| The provider closed an idle connection before the request was written | retry in 1 to 2 s | nothing |
| No response (could not connect, timeout, reset) | retry with backoff | failure |

- **Why 401, 403 and 404 are retried and 400 is not.** "Something is wrong with this message"
  ends the message; "something is wrong with the channel" (a wrong URL, an expired credential,
  a provider that is down) must not, or one bad setting would end every notification in the
  queue. The price: an address that answers 400 to everything does end them `failed`
  ([Not done](#not-done-and-why)); they come back with `POST /v1/notifications/{id}/retry`.
- **Backoff:** 6 attempts by default. After attempt k the delay is drawn between half of and
  the whole of min(2 min, 2 s × 2^(k−1)), so rows that failed together do not come back
  together. A `Retry-After` that asks for longer is honoured, up to the same 2 minutes.
- **Every attempt counts, also one that was refused before it was sent.** Nothing is ever
  given back. The first version handed permits and breaker reservations back on seven separate
  paths; this one has none, and its loop has a single exit.
- **Circuit breaker, one per channel per worker:** five failures in a row open it for 30 s, in
  which that dispatcher claims nothing; then exactly one notification is let through as a
  probe, and its result closes the breaker or opens it again.
- **A worker that dies loses nothing.** A claimed row has a lease of 60 s. The reaper, every
  5 s, returns rows whose lease has expired to the queue, or ends them `failed` if that was
  their last attempt. A result that arrives from a worker that no longer holds the row changes
  nothing.
- **At-least-once.** A notification can reach the provider twice in three named cases: a
  worker killed between the provider's answer and the stored result; a result write that
  fails until the reaper has taken the row back; an attempt that got no response although the
  provider processed it. Each repeat carries the same `Idempotency-Key`, the notification id.

Proven by, among others: TestClassifyDecidesEveryRowOfTheVerdictTable,
TestRetryDelaysDoubleWithJitterUpToTheCap, TestRetryAfterIsHonouredUpToTheCap,
TestTheHalfOpenBreakerLetsExactlyOneRowThrough, TestAKilledWorkersRowComesBackThroughTheReaper,
TestAFailedResultWriteIsRetriedAndNotSentAgain.

## Requirements and where they are met

Each line names the code that does it and tests that prove it. The walkthrough shows the same
code in the order a notification meets it; architecture §15 maps the requirements to the design.

| Requirement | Code | Proven by |
|---|---|---|
| Create with recipient, channel, content, priority | [`internal/api/notifications.go`](internal/api/notifications.go) `(*server).create`; [`internal/domain/request.go`](internal/domain/request.go) `domain.Prepare` | TestCreateAndReadANotification, TestEveryFieldIsValidatedAndNamed |
| Batch of up to 1000 | `(*server).createBatch`; `store.(*Notifications).CreateBatch` | TestBatchSizeIsBoundedAtAThousand, TestABatchIsValidatedWholeBeforeAnyIsStored |
| Status by id or batch id | `(*server).get`, `(*server).batch`; [`internal/store/notifications.go`](internal/store/notifications.go) | TestCreateAndReadANotification, TestTheBatchSummaryCountsEveryStatus |
| Cancel pending notifications | `(*server).cancel`; `store.(*Queue).Cancel` in [`internal/store/queue.go`](internal/store/queue.go) | TestCancelOnlyFromWaitingStatusesThroughTheAPI, TestACancelledRowCannotBeRevived |
| List with filters and pagination | `(*server).list`; `store.(*Notifications).List` | TestTheListIsOrderedAndPagedByCursor, TestAListPageReadsTheIndexFromItsCursor |
| Asynchronous processing by queue workers | [`cmd/worker/main.go`](cmd/worker/main.go); [`internal/dispatch/dispatch.go`](internal/dispatch/dispatch.go); `store.(*Queue).Claim` | TestTheWorkerDeliversEveryChannel, TestConcurrentClaimsNeverTakeTheSameRow |
| At most 100 per second per channel | [`internal/ratelimit/ratelimit.go`](internal/ratelimit/ratelimit.go); the send gate in [`internal/provider/provider.go`](internal/provider/provider.go) | TestNoWindowHoldsMoreThanTheLimit, TestTheProviderReceivesAtMostTheLimitThroughTheDispatchers |
| Priority queue (high, normal, low) | `store.claimSQL`; `(*Dispatcher).Run` | TestClaimsInPriorityTurnTakeHigherPrioritiesFirst, TestHighPriorityOvertakesBacklog |
| Content validation | `domain.Prepare`, `domain.ContentLimit` | TestContentIsBoundedPerChannelInCharacters, TestRecipientFormatsPerChannel |
| Idempotency | `domain.Hash`; `store.(*Notifications).Create`; the `Idempotency-Key` sent to the provider | TestConcurrentIdenticalRequestsCreateOneNotification, TestConcurrentIdenticalBatchesCreateOneBatch, TestTheRequestHasTheAssignmentsFormat |
| Delivery and retry logic | [`internal/dispatch/classify.go`](internal/dispatch/classify.go), [`internal/dispatch/retry.go`](internal/dispatch/retry.go), [`internal/dispatch/reaper.go`](internal/dispatch/reaper.go) | TestClassifyDecidesEveryRowOfTheVerdictTable, TestRetryDelaysDoubleWithJitterUpToTheCap, TestInvariantsHoldUnderRandomFaults |
| Metrics: queue depth, success and failure rates, latency | [`internal/api/metrics.go`](internal/api/metrics.go) (`GET /v1/metrics`); [`internal/observability/metrics.go`](internal/observability/metrics.go) (`GET /metrics`) | TestTheMetricsEndpointReportsTheRecentWindow, TestQueueGaugesComeFromTheDatabase |
| Structured logs with correlation ids | [`internal/observability/logging.go`](internal/observability/logging.go); `api.correlate` | TestOneCorrelationIDFollowsAMessageFromTheRequestToTheProvider, TestLogLinesCarryTheCorrelationIDFromTheContext |
| Health check | [`internal/api/health.go`](internal/api/health.go) (`/healthz`, `/readyz`) | TestReadyzIsReadyWhenBothDependenciesAnswer, TestReadyzStaysReadyWhenOnlyRedisIsUnreachable |
| The provider's request and its 202 answer (the mock provider stands in for webhook.site in the tests and the default stack; webhook.site itself was tried once, see [Not done](#not-done-and-why)) | `provider.(*Client).send`; the mock provider, [`cmd/mockprovider/main.go`](cmd/mockprovider/main.go) | TestTheRequestHasTheAssignmentsFormat, TestRecorderAnswersWithTheProviderContract |
| Bonus: failure handling | [`internal/dispatch/breaker.go`](internal/dispatch/breaker.go); `failed` as the dead-letter state; `(*server).retry` | TestNotFoundAndUnauthorizedOpenTheBreakerAndDoNotDrainTheQueue, TestRetryRequeuesOnlyAFailedNotification |
| Bonus: scheduled notifications | `store.createSQL`, `store.claimSQL` | TestAScheduledNotificationWaitsUntilItsTime (stored `scheduled` with its time), TestClaimReturnsOnlyDueRows (not claimed before it), TestAWebSocketClientReceivesTheSentEventOfARealDelivery (scheduled 2 s ahead, then sent by a real worker) |
| Bonus: templates | `domain.CompileTemplate`, `(*server).createTemplate` | TestTemplatesAreCreatedOnceAndRendered, TestTheRenderedLengthIsMeasuredBeforeRendering |
| Bonus: WebSocket updates | [`internal/api/ws.go`](internal/api/ws.go); [`internal/events/redis.go`](internal/events/redis.go) | TestAWebSocketClientGetsTheCurrentStateThenEveryChange, TestAWebSocketClientReceivesTheSentEventOfARealDelivery |
| Bonus: distributed tracing | [`internal/observability/tracing.go`](internal/observability/tracing.go) | TestTheDeliverySpanJoinsTheAPIRequestsTrace, TestTracingIsOffWithoutAnEndpoint |
| Bonus: GitHub Actions | [`.github/workflows/ci.yml`](.github/workflows/ci.yml): lint, tests, guards, alert rules, the stack end to end | the runs on GitHub |
| Docker Compose, one command | [`docker-compose.yml`](docker-compose.yml) | CI's compose job; TestComposeGivesTheWorkerItsStopGracePeriod |
| OpenAPI specification | [`internal/api/openapi.yaml`](internal/api/openapi.yaml) | TestOpenAPIMatchesTheServer |
| Versioned migrations | [`internal/store/migrations/`](internal/store/migrations/), [`cmd/migrate/main.go`](cmd/migrate/main.go) | TestMigrationsAreAppendOnly, TestMigrateBuildsAnEmptyDatabaseAndIsRepeatable |
| Tests with one command | `make test` ([`Makefile`](Makefile)) | CI's test job |

`make test` runs with the race detector, which slows the client and the recording server on the
same CPUs; there the tests of the limit at the provider only log their peak (architecture §11,
I1). They assert it without the race detector, as CI's test job and `make guards` run them:
`A5_FULL=1 go test -count=1 -v -run 'TestTheProviderReceivesAtMostTheLimitThroughTheDispatchers|TestGateWaitAndThroughputWithManyDispatchers|TestInvariantsHoldUnderRandomFaults' ./internal/dispatch`.

## Measured

On the author's machine (Apple silicon, 8 CPUs and 8 GB for Docker), the Compose stack with two
workers and the mock provider, in session 8. Every figure, its range and its command are in
[docs/measurements.md](docs/measurements.md).

| What | Result | Command |
|---|---|---|
| A burst of 30,000 in batches of 1000 | accepted in 0.23 to 0.34 s; drained at 93.5 to 93.7 per second per channel in the author's three runs and 93.9 in the reviewer's one (top speed 95.2); the provider's peak 100 per channel in one second; nothing lost, nothing accepted twice; a high-priority notification created during the drain sent in a median of 0.42 to 0.46 s | `make load` |
| 15,000 in 8 MB batches, 8 at once, 30 runs | accepted in 0.43 to 0.57 s in 28 runs; the API's memory at most 268 MiB; peak 100 in 29 runs, and 130, 121, 125 once (open, below) | `go run ./tools/load -n 15000 -concurrency 8 -content-bytes 8000 -probe-every 20ms` |
| The randomised fault test, 606 scenarios | 181,800 notifications, every one final; 605 passed, one exceeded the limit (47 against 40) with 10 requests late after their permits; 42 accepted twice, each after an attempt that got no response (at-least-once case 3) | `FAULT_RUNS=300 FAULT_SEED=1000 go test -count=1 -timeout 0 -run TestInvariantsHoldUnderRandomFaults -v ./internal/dispatch`, and `FAULT_SEED=2000` |
| The wait at the gate, 1 to 36 dispatchers on one channel (A5) | 94.1 to 94.9 sends per second; longest wait 6 ms with 1 dispatcher, 0.98 to 1.07 s with 3 to 12, 3.13 s with 24, 4.21 s with 36; no refusal | `A5_FULL=1 go test -count=1 -v -run TestGateWaitAndThroughputWithManyDispatchers ./internal/dispatch` (1, 3, 6, 12); `A5_COUNTS=12,24,36 A5_RUN=12s` with the same test (12 to 36) |

Not measured: a real provider under load (webhook.site was tried once with nine notifications,
[below](#not-done-and-why); every other figure is against the mock provider, which answers the
same contract), more than one API instance, and a server whose clock does not step.

## Not done, and why

Each item, with what it costs, is in [architecture §16](docs/architecture.md#16-deliberately-left-out).

- **Removing old rows:** needs a retention policy the requirements do not give. Without it,
  `GET /v1/metrics` over 1 hour reaches its 5 s deadline at about 16 million notifications.
- **Exactly-once delivery:** not possible across a network call. Delivery is at-least-once, and
  the provider receives the notification id as `Idempotency-Key`.
- **A breaker shared by all workers, or one that learns from a 400:** each worker has its own; a
  provider that answers 400 to everything ends rows `failed` (they come back with `POST /retry`),
  because the other choice lets one client's bad messages stop a channel for everyone.
- **webhook.site is not the default provider, and no load was sent to it.** The assignment's
  provider is a webhook.site URL that answers 202; sending to it publishes the test data on an
  outside service, so the tests and the default stack use the mock provider, which answers the
  same contract. `PROVIDER_URL=https://webhook.site/<uuid> make up` is the only change. It was
  tried once, after session 9, with a URL set to answer 202 with
  `{"messageId", "status", "timestamp"}`: nine notifications (three single and a batch of six,
  over sms, email and push) each ended `sent` on the first attempt with the URL's message id,
  in 416 to 443 ms per attempt, and webhook.site's own list showed nine requests in the
  assignment's format, each with its notification id as `Idempotency-Key`. With webhook.site's
  default answer, a 200 in plain text, every notification ends `sent` with no provider message
  id; that case was not tried.
- **Priority aging, tenants, per-client quotas, expiry of idempotency keys, durable status
  events, a bulk requeue endpoint:** not asked for, and each needs a policy or a second source
  of truth.

## Known open issues

From [session 8's record](docs/sessions/08-hardening.md#what-is-still-open) and
[session 9's](docs/sessions/09-walkthrough.md#what-is-still-open):

- **Three events that did not repeat, unexplained.** Once in 30 heavy bursts the mock provider
  counted 130, 121 and 125 in one second on the three channels at once; once in 606 fault
  scenarios requests arrived up to 144 ms after their permits; in one of the author's 30 heavy
  runs two batches were answered after 15.8 s and 29.6 s, and in one of the reviewer's one batch
  after 7.5 s. Each has its candidate causes, its commands and what would
  separate them in the record. For the first, the leading candidate is the clock step of the
  next item: the session's reviewer stepped the clock forward by 300 ms on purpose and the
  provider counted 200 per channel on all three channels at once, the same signature
  ([review 8](docs/reviews/08-session-8-review.md), F3).
- **Docker's clock.** Redis reads the wall clock of Docker's VM, which stepped forward by 28 to
  35 ms under load on the author's machine; that is inside the 50 ms margin, and a larger step
  would let more than the limit through for one window (architecture §7).
- **Two relations no worker can check alone:** how many dispatchers a channel can have before
  refusals at the gate (about 40 at the defaults), and the sum of the connection pools against
  Postgres's `max_connections`. Counters and alerts show when either is crossed.
- **Two binaries in the history of `main`** (47 MB, from session 7): removing them means
  rewriting a public branch, and every commit id the records cite would change. I left the
  history as it happened.

## Commands and settings

| Command | What it does |
|---|---|
| `make up`, `make down` | Start the stack and wait until it is healthy; stop it and delete its data |
| `make up-observability` | The stack with Prometheus on <http://localhost:9091> (scraping the API and every worker, with the [alert rules](deploy/prometheus/alerts.yml)) and Jaeger on <http://localhost:16686>, tracing on |
| `make e2e N=1000` | Follows one notification over the WebSocket, then sends N per channel through the API and waits until all are sent |
| `make load` | A burst of 30,000 against the running stack, measured from outside ([`tools/load`](tools/load/main.go)) |
| `make status` | Notifications by status, straight from Postgres |
| `make test` | Every test with the race detector; integration tests start Postgres and Redis in Docker |
| `make test-unit` | Only the tests that need no Docker (`-short`) |
| `make lint` | golangci-lint v2.15.0, in Docker |
| `make guards` | Disables each guard in turn and checks that its named test fails ([`tools/guards`](tools/guards/list.go)) |
| `make check-docs` | Every test, link, path and Go name the documents point at exists |
| `make alerts` | promtool checks and tests the alert rules; every rule must have a case in which it fires |

Settings are environment variables, read from an optional `.env` file too; they are listed
with their defaults and relations in [architecture §13](docs/architecture.md#13-timing-budget).
The worker refuses to start when a relation does not hold: for example,
`docker compose run --rm --no-deps -e LEASE=40s worker` exits with status 2. The workers send to
`PROVIDER_URL`, by default the mock provider; set it to a webhook.site URL that answers 202 to
use that instead (see [Not done](#not-done-and-why)). The mock provider can fail on purpose, to watch retries and the breaker:

```sh
MOCK_FAIL_RATE=0.2 MOCK_THROTTLE_RATE=0.05 MOCK_LATENCY=30ms docker compose up -d mockprovider
# create notifications as above, then watch them being retried until they are sent:
make status
curl -s localhost:9090/stats        # "injected": the 503 and 429 answers it gave
docker compose up -d mockprovider   # without the variables: a provider that does not fail again
```

`make e2e` and `make load` check a provider that does not fail; against this one they report the
retries as a failure, and `make` stops with an error.

## How it was built

This is the second build of the assignment; the first is public and unchanged at
[InsiderOneProjectDeneme](https://github.com/atakanakis/InsiderOneProjectDeneme). Five rounds of
independent review (four recorded in that repository, the fifth summarised in the journey)
found real defects in it, several inside the fixes of the round before,
because its design had never been written down and checked as a whole. Here the design was
written and reviewed first, then built in sessions, one part each, by Claude Opus agents with a
fresh context. Each session had one review, run once after its code was green, by a fresh
reviewer agent that had not seen the author's reasoning, could not edit, and had to reproduce
what it reported. `make guards` disables every guard on purpose and expects a named test to
fail: 354 guards, all real (`make guards`; counted with
`grep -cE '^\s*Name: "' tools/guards/list.go`). [docs/journey.md](docs/journey.md) tells the story,
[docs/lessons.md](docs/lessons.md) lists what the first version taught, and
[`CLAUDE.md`](CLAUDE.md) holds the rules every session followed.

| # | Session | Status |
|---|---|---|
| 0 | Lessons, architecture, design review, plan | done |
| 1 | Skeleton: module, config, migrations, Compose, CI, health | done ([record](docs/sessions/01-skeleton.md)) |
| 2 | Queue: the five operations | done ([record](docs/sessions/02-queue.md)) |
| 3 | Rate limiter | done ([record](docs/sessions/03-ratelimit.md)) |
| 4 | Provider client and the send gate | done ([record](docs/sessions/04-provider.md)) |
| 5 | Dispatcher, breaker, reaper; fault tests | done ([record](docs/sessions/05-dispatcher.md)) |
| 6 | API | done ([record](docs/sessions/06-api.md)) |
| 7 | Events, WebSocket, metrics, tracing, alerts | done ([record](docs/sessions/07-events.md)) |
| 8 | Hardening: independent review, soak, load test | done ([record](docs/sessions/08-hardening.md)) |
| 9 | Walkthrough and final documents | done ([record](docs/sessions/09-walkthrough.md)) |

The agents' definitions are in [`.claude/agents/`](.claude/agents/).
