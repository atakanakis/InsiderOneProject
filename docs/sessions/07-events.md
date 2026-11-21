# Session 7: status events, WebSocket, metrics, tracing, alerts

- **Date:** 2026-11-20 to 2026-11-21.
- **Lead:** Claude Opus. **Builders:** none; the lead wrote the code. **Reviewer:** one fresh
  Claude Opus instance, run once after the code was green
  ([review record](../reviews/07-session-7-review.md)).
- **Brief:** [plan.md, "7. Events and observability"](../plan.md#7-events-and-observability).

## What was built

| Part | What it is | Evidence |
|---|---|---|
| `internal/events` | The event `{id, batchId, status}` on the Redis channel `notifications:status`. `Redis.Publish` puts it in a buffer of 4,096 and returns; one goroutine sends up to 256 at once in a pipeline bounded by 1 s; a full buffer or a Redis that does not take them drops and counts them. `Subscribe` pings after a silent 10 s, subscribes again after a second silent 10 s or any error (100 ms doubling to 5 s), and closes its connection at once when its context ends. `Decode` refuses over 1 KiB, no id, an unknown status | I13 (§11); TestEventsReachSubscribersThroughRedis, TestTheSubscriptionComesBackAfterRedisDropsIt, TestTheSubscriptionComesBackAfterAnOutage, TestAMalformedOrOversizedEventIsDropped, TestThePublisherSendsWhatIsBufferedWhenClosed |
| Who publishes | The API's create, batch, cancel and retry; the worker's `handle` (`processing`) and result; the reaper. Claim, Reap, Cancel and Requeue return the batch id from their statement | TestEveryStatusChangeOfTheWorkerIsPublished, TestStatusChangesReturnTheBatchAndTheCorrelationID |
| WebSocket (`GET /v1/ws`) | Up to 100 notification ids and 10 batch ids; the current state first, then every change. The hub is a map per id and a queue of 256 per client that is never waited on; 1000 connections per instance; a write within 5 s; a ping every 30 s answered within 10 s; no data message from the client; 1001 on shutdown. Every refusal is before the upgrade, in the error form | The table of §9, a test per row |
| Prometheus metrics | `notifications_*` on the API's `/metrics` and each worker's `METRICS_ADDR` (`:9100`): requests, queue depth and the oldest due row (from Postgres, once per 5 s), attempts by outcome, what became of each attempt, the wait at the gate, the provider's latency, the breaker, sends in flight, reaped rows, dependency failures, events, WebSockets. Labels from fixed sets, every series created at zero | The table of §10; TestMetricLabelsHaveBoundedValues, TestAttemptsAndResultsAreCountedOnce, TestTheWorkerServesItsMetrics |
| `GET /v1/metrics` | Per channel, for a window of 1m, 5m, 15m or 1h: the queue, the attempts by outcome, per second, the share delivered, percentiles of the attempt's duration and of the delivery latency; computed once per 5 s per window; a BRIN index on `delivery_attempts.started_at` (migration 5) | TestTheMetricsEndpointReportsTheRecentWindow, TestTheMetricsEndpointIsComputedOncePerFiveSeconds, TestTheWindowsAttemptsAreReadThroughTheirIndex |
| Logs | The correlation id on every worker line about a notification, the reaper's included. A failing dependency (claim, rate-limit read, result write, reap, Redis's own dial lines, readiness checks) is counted and logged at most once per 10 s with the number held back (`observability.Every`) | TestEveryWorkerLineAboutANotificationCarriesItsCorrelationID, TestOneCorrelationIDFollowsAMessageFromTheRequestToTheProvider, TestResultWriteFailuresAreCountedAndLoggedSparsely, TestFailingDependenciesAreCountedAndLoggedSparsely |
| Tracing | OpenTelemetry. A server span per API request, the child of its `traceparent`; the span's context stored on each row; the worker's `deliver` span a child of it. Off without `OTEL_EXPORTER_OTLP_ENDPOINT`: the no-op provider | TestTheDeliverySpanJoinsTheAPIRequestsTrace, TestTracingIsOffWithoutAnEndpoint |
| Alerts | Eight rules in `deploy/prometheus/alerts.yml` before the review, ten after it (one replaced, two added), each with a promtool case that fires and one that does not; `tools/alertcheck` finds a rule without a firing case, and a metric, label value or grouping label that the binaries do not export; `make alerts`, a CI job | TestEveryAlertRuleHasAFiringTest, TestTheCheckRefusesWhatNoBinaryExports, TestEveryAlertRuleUsesMetricsTheBinariesExport |
| Compose, CI | The `observability` profile (Prometheus on 9091 with the rules, Jaeger on 16686, tracing on); `tools/e2e` follows one notification over the WebSocket; the compose job checks that the workers' counters, `/v1/metrics` and the provider agree, and runs alertcheck on the live scrapes | CI's compose job |

The dispatcher's loop is unchanged: `Run` is 29 lines (`sed -n '/^func (d \*Dispatcher) Run/,/^}/p' internal/dispatch/dispatch.go | wc -l`).
The measurements are made in `handle`: the gate records two times on its own goroutine, and
everything else is observed after `Classify` and after `Finish`.

## How the tests came first

Each part was a red commit against stubs that compile, then the code:

| Red | Green | What was red |
|---|---|---|
| `e80d723` | `8279be1` | A publisher that waited: against a Redis that never answers, 30 of 300 rows sent in 15 s; no subscription; no metric; a warning per refused result write; the reaper's line without its correlation id |
| `c371459` | `67ef55d` | Every bound of the WebSocket, `/v1/metrics`, `/metrics`, the labels (stubs answered 501) |
| `4e6c87b` | `ecf83f6` | No span, no stored traceparent |
| `22914d6` | `fecde84` | The check found neither a rule without a firing case nor a metric nobody exports |
| `5abf074` | `69e0a42` | The Redis client's own lines and readiness warnings, found on the stack (below) |

`TestTheAPIAnswersWhileRedisHangs`, `TestOneCorrelationIDFollowsAMessageFromTheRequestToTheProvider`
and `TestAWebSocketClientReceivesTheSentEventOfARealDelivery` passed when they were written:
the code they check was already built in the step before.

## Decisions this session made

- **Publishing never waits; I13.** The narrow interface of session 5 was synchronous in
  practice: a slow Redis would have held every send's slot. The buffer, the single sender with
  its own Redis client and the 1 s pipeline make a lost event cost nothing.
- **The current state first.** A client cannot subscribe before a batch exists, and a change
  can happen before the connection opens; without it the WebSocket would miss exactly the
  events a client wants.
- **No "every event" stream, no key in the query string, no `traceparent` to the provider**
  (§16).
- **A data message closes the connection at once**, without the close handshake (below).
- **The figures from Postgres are read by the API, once per 5 s.** Every API instance exports
  them; rules use `max`.
- **Refusals at the gate and connections closed at the gate are outcomes**, not metrics of
  their own: one counter by outcome answers both.
- **Rules on a dependency use a 1-minute rate held for 2 minutes**, so one blip does not page.

## Found while building

- **The close handshake read a 1 GiB message.** A client's frame that announced 1 GiB and
  sent 100 bytes held the handler: the WebSocket library's `Close` first reads the rest of the
  current message, without a deadline. A data message now closes the connection at once.
  Found because the test's counter of closed connections did not move.
- **A line per try, three more places.** On the Compose stack with Redis stopped for 4
  minutes, the alert `NotificationsRateLimitUnavailable` went pending at 30 s and fired at
  150 s, the API stayed ready and created rows, and after Redis came back the subscription
  came back and a WebSocket client saw `scheduled, processing, sent`. Meanwhile one worker
  wrote 307 lines, most of them go-redis's own plain-text "failed to dial", and the API a
  warning per readiness probe. Both now go through `observability.Every`
  (`redisclient.LogTo`).
- **Guards that proved nothing.** On their first run 59 of 69 new guards were real. Nine
  mutations did not compile (a variable or an import left unused); one compiled and no test
  failed: removing the write deadline, because the slow-client test overflowed the queue
  before the TCP buffers were full and the write never blocked. A new test makes the write
  block (TestAWriteToAClientThatStopsReadingEndsAtItsDeadline). Six more guards needed tests
  that were missing: a stale result, the counts of a failing claim, rate-limit read and Reap,
  an event without an id, the queue's exact four statuses, a channel with only a future row,
  the latency of delivering attempts only, the BRIN index, a batch's trace.
- **Two guards of sessions 5 and 6 no longer matched their text** after the event's form
  changed. `make guards` would have reported them after an hour;
  TestEveryGuardsTextOccursOnceInItsFile now finds such drift in the fast suite.
- **A second size check removed.** The subscription and `Decode` both refused an event over
  1 KiB; no test could tell one from the other, so the subscription's went.

## Measured

| What | Result | Command |
|---|---|---|
| Tests | 274 test functions (223 at the start of the session, 269 before the review); all pass with the race detector in 1 min 37 s | `time make test`; count: `grep -rh '^func Test' --include='*_test.go' . \| grep -v TestMain \| wc -l` |
| Lint | 0 issues | `make lint` |
| Guards | 69 new before the review, 10 after it, 328 in all; before the review 318 of 318 real locally (39 min 52 s) and in CI (run 44816376876) | `go run ./tools/guards -from 'events: Publish never waits'`; `time make guards` |
| Publish against a Redis that never answers | 12,288 events in under 1 s, none waiting more than 10 ms; at least 7,936 dropped as `buffer_full`; the first pipeline given up within 2 s | `go test -count=1 -v -run TestPublishNeverBlocksWhenRedisHangs ./internal/events` |
| Delivery with every event going to that Redis | 300 of 300 sent at the limit, no slot held after the last result; with the synchronous stub, 30 of 300 in 15 s | `go test -count=1 -run TestLostEventsDoNotAffectDelivery ./internal/dispatch` |
| A client that does not read, 1,000,000 events | delivered in 13 ms; the client let go; the heap did not grow (a change of -18 KiB) | `go test -count=1 -v -run TestASlowWebSocketClientIsDisconnectedAndMemoryStaysBounded ./internal/api` |
| A client that stops reading, the write blocked | closed 0.94 s after the first event with a write timeout of 500 ms | `go test -count=1 -v -run TestAWriteToAClientThatStopsReadingEndsAtItsDeadline ./internal/api` |
| The Compose stack, two workers | 1502 sent in 8.81 s; the mock provider's peak 100 per channel; the workers' sent counters 1502, `/v1/metrics` 1502, the provider 1502; alertcheck on the three scrapes passed | `make up; go run ./tools/e2e -n 500`, then the compose job's steps |
| The observability profile | 3 targets up, 8 rules healthy; one trace in Jaeger with the API's batch request and its 50 `deliver` spans | `make up-observability; go run ./tools/e2e -n 50`; `curl localhost:9091/api/v1/targets`, `localhost:16686/api/traces?service=notifications-worker` |
| Redis stopped for 4 minutes | the rate-limit alert pending at 30 s, firing at 150 s; `/readyz` 200 with `"redis":"unreachable"`; a create 201; after the restart 2 subscriptions in all and the WebSocket run passed | the script in "Found while building" (`docker compose stop redis`, polling `localhost:9091/api/v1/alerts`) |
| The figures from Postgres at scale | depth 121 ms, oldest due row 0.1 ms, a 5-minute window 105 to 163 ms, a 1-hour window 0.31 s and 0.91 to 1.03 s | [experiment 02](../experiments/02-metrics-at-scale.md) |

## The review

One fresh Claude Opus reviewer, run once on `1d791aa` after CI was green, with no time limit
(1 h 51 min; [review record](../reviews/07-session-7-review.md)). It reproduced the metrics
against independent counts with and without faults (3,002 and 9,513 attempts, every side
equal), 1000 WebSockets and the 1,001st refused, every malformed frame it could build, Redis
stopped for almost 11 minutes and paused, shutdown with 1001, and a trace from the request to
the delivery. It found three major problems: two binaries committed and pushed; alert rules
tested on inputs the system never produces (a read-only Postgres raised no alert for 10.5
minutes, a stopped API none, and an idle half-open breaker paged); and the figures from
Postgres read under one lock, so a stalled Postgres made 20 callers wait for each other's
reads and scrapes fail whole. Four minor findings and three nits followed.

Every finding was answered as a class: the design first (`7bb4fc9`), then red tests
(`b6e1f25`), then the fixes (`b94ae7b` for the binaries, `e35ecf5` for the rest, `bc7ce16` the guards), a guard
for each, and the changed rules run on the stack until each fired or stayed quiet as it
should. While fixing F1 the session repeated its class once more: `git add -A` took a draft
document into a commit, which was caught before the push and rebuilt; commits now name their
files. TestEveryGuardsTextOccursOnceInItsFile, added during the session, found the two
guards the fixes moved, in the fast suite.

## What is still open

- The two binaries of F1 remain in the pushed history of `main` (47 MB); removing them means
  rewriting a public branch, which is the owner's decision.
- With `RATE_LIMIT=20`, two workers sent 8 to 12.6 per second with no refusal at the gate
  (the reviewer's measurement, hard question 7). Why the loop stays below a low limit is for
  session 8's load test.
- `GET /v1/metrics` over 1 hour joins every notification row; nothing removes old rows, so
  its cost grows with the table and at some size reaches the 5 s deadline (§10, §16).
  Session 8's load test measures where.
- Events of one notification from two processes have no order (§9). A client that needs the
  truth asks `GET /v1/notifications/{id}`.
- The WebSocket's "follow before reading the current state" order has no test: the gap it
  closes is a race that a test cannot hold open without a hook in the handler.
- The provider latency histogram is checked for presence, count and a lower bound; that it
  starts at the permit and not at the claim is by construction, not by a test.

## CI

CI on `1d791aa`, the commit the review was given, run 44816376876: lint, test, alerts, guards
and compose all succeeded; guards `318 guards, 318 real, 0 failed`; the compose job's
WebSocket client saw `scheduled, scheduled, processing, sent`, 1502 notifications were sent
in 8.64 s, and the workers' counters, `/v1/metrics` and the provider all said 1502.

After the review, `make guards` locally on `bc7ce16`: `328 guards, 328 real, 0 failed` in
40 min 27 s. CI on `850d1a0`, the session's last code and document commit, run 44842146255:
lint (with the new check that no build output is tracked), test, alerts (10 rules), guards and
compose all succeeded; guards `328 guards, 328 real, 0 failed`; the fault test's scenarios at
peak 40, the limit, none accepted twice; the compose job: `all 1502 notifications sent in
8.65s`, `sent attempts by the workers' counters 1502, by /v1/metrics 1502, received by the
provider 1502`, and alertcheck passed on the three scrapes
(`gh run watch 44842146255 --exit-status`, and the logs of the guards, test and compose
jobs). The commit that adds these paragraphs changes only the two records.
