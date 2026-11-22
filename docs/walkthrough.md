# Walkthrough: one notification through the code

This page follows one SMS from the HTTP request that creates it to the provider that accepts
it, and back to the client. Then it follows the six branches a notification can take: a
retry, a cancel, a scheduled send, a worker that dies, a refusal by the rate limit, and the
circuit breaker.

Each step names the file and the function that do the work, the invariant the step keeps
(I1 to I13, [architecture §11](architecture.md#11-invariants)), and the tests that prove it.
The design behind each step is in [architecture.md](architecture.md); this page is the map
from the design to the code.

Every file, function and link on this page is checked by `make check-docs`
([`tools/docrefs`](../tools/docrefs/main.go)): on a line that starts with **Code:**, every
name must be declared in the file that line links, outside the tests. Every test name is
checked by [`tools/testnames`](../tools/testnames/main.go).

## The whole path on one screen

```
client ── POST /v1/notifications ──► API (cmd/api)
                                      correlate → authenticate → create
                                      validate, hash, INSERT ... ON CONFLICT DO NOTHING
                                      row: pending, ready_at = now()
client ◄── 201 {"id", "status": "pending", ...} ──┘      │ event "pending" → Redis
                                                         ▼
                                    Postgres: notifications (the queue)
                                                         │
worker (cmd/worker), dispatcher of channel sms           │
  room: slots, claim batch, breaker, Available ──────────┤
  Claim (high, normal, low): pending → processing, attempts + 1, lease
  start → handle (one goroutine per row)
     Send: connect ─► gate: limiter.Wait (Redis, the shared window) ─► write POST ─► provider
                                                                          202 accepted
     Classify(result) → sent
     Finish: attempt row + "sent" WHERE attempts = k AND status = 'processing'
     event "sent" → Redis → every API instance → WebSocket clients that follow the id
client ── GET /v1/notifications/{id} ──► "sent", with its attempt history
```

Four processes run in the Compose stack: the API, two workers, the mock provider, and a
one-shot migration. They share nothing but Postgres and Redis.

| Process | Entry | What it runs |
|---|---|---|
| API | [`cmd/api/main.go`](../cmd/api/main.go) | `internal/api`: the HTTP API, the WebSocket hub, the queue's figures |
| Worker | [`cmd/worker/main.go`](../cmd/worker/main.go) | `internal/dispatch`: one dispatcher per channel and one reaper |
| Mock provider | [`cmd/mockprovider/main.go`](../cmd/mockprovider/main.go) | `internal/recorder`: stamps each request on arrival, answers 202 |
| Migration | [`cmd/migrate/main.go`](../cmd/migrate/main.go) | the files in `internal/store/migrations/`, then exits |

## The happy path

### 1. The API starts

**Code:** [`cmd/api/main.go`](../cmd/api/main.go): `run`.

`run` reads the settings (`config.Load`), opens the Postgres pool with a statement timeout of
5 s on every session (`store.Connect`, `api.DatabaseOptions`), and builds three Redis clients
in the one place that builds them (`redisclient.New`): one for readiness, one for publishing
status events, one for the subscription. It starts the events subscription
(`events.Subscribe`, which hands each event to `api.(*Hub).Deliver`) and serves
`api.NewHandler` through `api.NewServer`.

- **Keeps:** I11 for time: the server's own timeouts bound a slow client.
- **Proven by:** TestAPIServesReadinessAndStopsCleanly, TestTheServerHasTheTimeoutsOfTheDesign,
  TestServiceSessionsCarryTheirTimeouts, TestRedisClientsAreBuiltOnlyHere,
  TestRedisClientHonoursContextDeadlines.

### 2. The request arrives: a correlation id, a span, a log line

**Code:** [`internal/api/health.go`](../internal/api/health.go): `NewServer`, `newServer`, `NewHandler`, `correlate`.

The client sends:

```sh
curl -s localhost:8080/v1/notifications -H 'Content-Type: application/json' -H 'X-Correlation-ID: order-42' \
  -d '{"recipient":"+905551234567","channel":"sms","content":"Your code is 4242","priority":"high","idempotencyKey":"order-42"}'
```

`newServer` bounds the request before any handler runs: headers within 5 s and 64 KiB, the
whole request within 30 s, the answer within 30 s. `correlate` wraps every route. It keeps the
client's `X-Correlation-ID` when it is 1 to 64 letters, digits, `-`, `_` or `.`
(`observability.ValidCorrelationID`), and otherwise makes a new one
(`observability.NewCorrelationID`). It puts the id in the request's context
(`observability.WithCorrelationID`), so every log line of the request carries it, and sends it
back in the response. It starts a span, the child of the request's `traceparent` header when
there is one (`observability.FromHeaders`). When the handler returns, it counts the request
under its route pattern and writes one log line.

- **Keeps:** I11 (request headers, the correlation id's bound).
- **Proven by:** TestCorrelationIDIsAcceptedOrReplaced, TestCorrelationIDBound,
  TestRequestHeadersOverTheBoundAreRefused, TestSlowRequestHeadersAreCutOff.

### 3. Routing and the API key

**Code:** [`internal/api/notifications.go`](../internal/api/notifications.go): `register`, `(*server).routes`, `(*server).authenticate`.

`register` adds every `/v1` route of `(*server).routes` behind `(*server).authenticate`.
With no `API_KEYS` set, every request passes. With keys set, the request needs one of them
in `Authorization: Bearer` or `X-API-Key`; each configured key is compared in constant time.
`POST /v1/notifications` goes to `(*server).create`.

- **Proven by:** TestAPIKeysAreRequiredWhenConfigured, TestOpenAPIMatchesTheServer (every route
  is in the OpenAPI specification, and every path, method and status code of the
  specification is produced by the real server).

### 4. The body is decoded within its bound

**Code:** [`internal/api/notifications.go`](../internal/api/notifications.go): `(*server).create`, `readOne`, `body`, `end`, `decodeFailure`.

**Code:** [`internal/domain/request.go`](../internal/domain/request.go): `Input`, `checkNames`.

`create` first refuses an `Idempotency-Key` header: a single notification carries its key in
the body. `readOne` reads the body through `http.MaxBytesReader` with the bound
`MaxBodyBytes` (1 MiB); one byte over is a 413. The body is decoded into `domain.Input`,
whose `UnmarshalJSON` first checks the field names (`checkNames`): an unknown name, a name in
another case or a name given twice is a 400 that names the field. `end` refuses anything after
the object. A decoding error becomes an answer through `decodeFailure`, which never quotes the
request.

- **Keeps:** I11 (the body's bound; an error never grows with the request).
- **Proven by:** TestRequestBodiesAreBoundedAtTheirLimit, TestFieldNamesAreMatchedExactlyAndOnce,
  TestUnknownFieldsAndWrongTypesAreRefusedByName,
  TestAnIdempotencyKeyHeaderOnASingleCreateIsRefused.

### 5. A deadline, then a rendering slot

**Code:** [`internal/api/notifications.go`](../internal/api/notifications.go): `(*server).database`, `(*server).render`, `DatabaseDeadline`, `MaxRenderingRequests`.

`(*server).database` gives the request's database work a deadline of 5 s
(`DatabaseDeadline`). Within that deadline, `(*server).render` waits for one of 4 rendering
slots per API instance (`MaxRenderingRequests`). A template can make a small request render
a lot, so at most 4 requests render and store at once; the others wait holding only what
they sent. A request that gets no slot before its deadline is a 500.

- **Keeps:** I11 (memory across requests, and time when Postgres stalls).
- **Proven by:** TestARequestWaitingForASlotEndsAtItsDeadline,
  TestConcurrentRequestsRenderInBoundedMemory, TestARequestEndsAtItsDeadlineWhenPostgresStalls.

### 6. Validation, before anything is stored

**Code:** [`internal/api/notifications.go`](../internal/api/notifications.go): `(*server).lookup`, `validationError`, `writeError`.

**Code:** [`internal/domain/request.go`](../internal/domain/request.go): `Prepare`, `checkRecipient`, `scheduledAt`, `ContentLimit`, `CompileTemplate`, `(*Compiled).Measure`, `Echo`.

`(*server).lookup` reads the templates the request names in one query
(`store.(*Notifications).TemplatesNamed`) and compiles each once (`domain.CompileTemplate`).
Our SMS has plain content, so no template is read. `domain.Prepare` validates the item and
names every wrong field:

- the channel is `sms`, `email` or `push`;
- the recipient fits the channel (`checkRecipient`): an E.164 phone number for SMS;
- the priority is `high`, `normal` or `low`, `normal` when absent;
- `scheduledAt`, when given, is an RFC 3339 time before the year 10000 (`scheduledAt`);
- the idempotency key is 1 to 128 printable ASCII characters;
- there is either `content` or a `template` with `variables`, not both. A template's length
  is measured from its compiled parts before anything is rendered (`(*Compiled).Measure`);
- the content is not empty, has no NUL, and is within the channel's bound
  (`ContentLimit`: 1,600 characters for SMS).

The errors are collected, at most 100 kept, and written by `writeError`, the one error
writer. A name taken from the request passes `domain.Echo`, which keeps 64 bytes.

- **Keeps:** I11 (every field's bound; the error body's bound).
- **Proven by:** TestEveryFieldIsValidatedAndNamed, TestRecipientFormatsPerChannel,
  TestContentIsBoundedPerChannelInCharacters, TestContentAndVariablesMustNotContainNUL,
  TestTheRenderedLengthIsMeasuredBeforeRendering, TestATemplateIsCompiledOncePerRequest,
  TestErrorBodiesStayWithinTheirBoundForAnyRequest, TestFieldErrorsAreKeptUpToTheirBound.

### 7. One statement stores the row, or finds the one with the same key

**Code:** [`internal/domain/request.go`](../internal/domain/request.go): `Hash`, `(Prepared).Content`.

**Code:** [`internal/api/notifications.go`](../internal/api/notifications.go): `newRow`.

**Code:** [`internal/store/notifications.go`](../internal/store/notifications.go): `(*Notifications).Create`, `createSQL`, `ErrIdempotencyConflict`.

`domain.Hash` is the request hash: SHA-256 of the validated request in a canonical form
(defaults filled in, `scheduledAt` in UTC, variables sorted), without the key. `newRow` gives
the row a new id and its rendered content. `store.(*Notifications).Create` runs `createSQL`:

- `INSERT ... ON CONFLICT (idempotency_key) DO NOTHING`. Two identical requests at the same
  moment both end with one row: the second insert waits on the unique index, then does nothing.
- Postgres decides the status and the time with its own clock: `scheduled` with
  `ready_at = scheduledAt` when that is in the future, otherwise `pending` with
  `ready_at = now()`. Go passes no timestamp of its own.
- The correlation id and the trace context come from the request's context
  (`observability.CorrelationID`, `observability.Traceparent`) and are stored on the row, so
  the worker can carry them on.

When the insert inserts nothing, the row with the key is read and its stored hash decides: the
same hash returns that row (the API answers 200 with it, as the status endpoint would), another
hash is `store.ErrIdempotencyConflict` (409). Our request is new: the row is `pending`, due now.

The row waits in `notifications_claim_idx`, a partial index on
`(channel, priority, ready_at)` of the rows in `scheduled`, `pending` or `retrying`
([`internal/store/migrations/00002_notifications.sql`](../internal/store/migrations/00002_notifications.sql)).

- **Keeps:** I10 (one key, one notification), I12 (queue times from Postgres).
- **Proven by:** TestTheRequestHashIgnoresFormAndDefaults,
  TestConcurrentIdenticalRequestsCreateOneNotification (20 identical requests at once: one 201,
  19 times 200, one row), TestAKeyReusedWithAnotherRequestIsAConflict,
  TestARepeatedCreateAnswersWithItsAttemptHistory, TestIdempotencyKeysAreUniqueInTheDatabase,
  TestMigrationsCreateTheDataModel, TestClaimIndexCoversExactlyTheClaimableStatuses.

### 8. The answer, and an event that never makes it wait

**Code:** [`internal/api/notifications.go`](../internal/api/notifications.go): `(*server).create`, `(*server).reply`, `notificationJSON`.

**Code:** [`internal/api/health.go`](../internal/api/health.go): `writeJSON`.

**Code:** [`internal/events/redis.go`](../internal/events/redis.go): `(*Redis).Publish`.

After the insert is committed, `create` publishes the event `pending`:
`events.(*Redis).Publish` puts it in a buffer of 4,096 events and returns at once. When the
buffer is full the event is dropped and counted; the answer never waits for Redis.
`(*server).reply` answers through `writeJSON`, which encodes the body before it writes the
status, so an answer that cannot be encoded is a 500, never a 201 without a body. The client
receives 201 with the notification: its `id`, `status: "pending"`, its `correlationId`, and
an empty attempt history.

- **Keeps:** I13 (an event never delays an answer).
- **Proven by:** TestCreateAndReadANotification, TestPublishNeverBlocksWhenRedisHangs,
  TestTheAPIAnswersWhileRedisHangs, TestAnAnswerThatCannotBeEncodedIsA500.

A batch takes the same steps with `(*server).createBatch`: the items are decoded one at a
time and the 1,001st ends the request, every item is validated before any is stored, and
`store.(*Notifications).CreateBatch` writes the batch and its items in one transaction. Item
`i` gets the id `store.BatchItemID` of the batch id and `i`, so a repeated batch answers the
same ids (TestBatchSizeIsBoundedAtAThousand, TestABatchIsValidatedWholeBeforeAnyIsStored,
TestConcurrentIdenticalBatchesCreateOneBatch).

### 9. A worker starts, and refuses a timing budget that does not hold

**Code:** [`cmd/worker/main.go`](../cmd/worker/main.go): `run`.

**Code:** [`internal/config/config.go`](../internal/config/config.go): `Load`, `LoadWorker`, `(Worker).Check`.

`run` reads the settings and checks the relations of the timing budget
(`config.(Worker).Check`, [architecture §13](architecture.md#13-timing-budget)): for example,
a lease shorter than the send timeout plus 30 s could let a message be sent twice, so the
worker exits with status 2 and names the relation. It then connects to Postgres and Redis and
builds the shared pieces: the rate limiter (`ratelimit.New`), the queue (`store.NewQueue`),
a status-event publisher with its own Redis client (`events.NewRedis`), and the metrics. It
starts one dispatcher per channel of `domain.Channels` (`dispatch.New`) and one reaper
(`dispatch.Reaper`).

Each dispatcher builds its own provider client from `provider.Options`; it cannot be given
one. So no dispatcher can use a client without the send gate, and none shares a connection
pool.

- **Proven by:** TestWorkerRefusesToStartWithALeaseShorterThanSendTimeoutPlus30s,
  TestDefaultsSatisfyEveryRelation, TestEveryBrokenRelationIsReported,
  TestTheWorkerDeliversEveryChannel, TestTheWorkerLogsOnlyTheProvidersHost.

### 10. The loop asks how many rows it may claim

**Code:** [`internal/dispatch/dispatch.go`](../internal/dispatch/dispatch.go): `(*Dispatcher).Run`, `(*Dispatcher).room`.

**Code:** [`internal/ratelimit/ratelimit.go`](../internal/ratelimit/ratelimit.go): `(*Limiter).Available`, `readScript`.

`(*Dispatcher).Run` is the loop of [architecture §6](architecture.md#6-the-dispatcher). It
fits on one screen. Each turn starts with `(*Dispatcher).room`, the smallest of four numbers,
read cheapest first:

1. free slots: at most 50 sends in flight per dispatcher;
2. the claim batch, 10, and 0 while any row this dispatcher claimed before still waits for
   its permit;
3. `(*Breaker).Allow`: no limit when the breaker is closed, 0 when it is open, 1 when it is
   half-open and no probe is out;
4. `ratelimit.(*Limiter).Available`: the free permits of the channel's window now, read by
   `readScript`, which changes nothing. An error (Redis unreachable or refusing writes) is 0.

None of these takes or reserves anything. When the room is 0, the loop pauses 20 ms and asks
again.

- **Keeps:** I9's second half (a worker has at most one claim batch waiting for permits);
  CLAUDE.md rule 3 (the loop never takes a permit).
- **Proven by:** TestAWorkerHasAtMostOneClaimBatchWaitingForPermits,
  TestNoRowIsClaimedWhileTheWindowIsFull, TestAnUnavailableRateLimitClaimsNothing,
  TestAvailableChangesNothing, TestAvailableFailsWhenTheTakeScriptWould, TestBreakerAllowOnlyReads.

### 11. The claim: one statement per priority, highest first

**Code:** [`internal/store/queue.go`](../internal/store/queue.go): `(*Queue).Claim`, `claimSQL`, `(*Queue).writeContext`.

For each priority of `domain.Priorities` (high, normal, low), the loop calls
`store.(*Queue).Claim` for the room that is left. `claimSQL` is one statement:

```sql
UPDATE notifications
SET status = 'processing', attempts = attempts + 1, locked_until = now() + <lease>
WHERE id = ANY(ARRAY(
    SELECT id FROM notifications
    WHERE channel = $1 AND priority = $2
      AND status IN ('scheduled', 'pending', 'retrying') AND ready_at <= now()
    ORDER BY ready_at LIMIT $3
    FOR UPDATE SKIP LOCKED))
```

Our SMS is `pending`, high priority, and due, so the first call takes it. It becomes
`processing`, its `attempts` becomes 1 (this attempt's number), and it holds a lease of 60 s
on the database's clock. `FOR UPDATE SKIP LOCKED` keeps two workers off the same row without
making them wait for each other. The rows come back sorted by `ready_at`. The statement runs
on `(*Queue).writeContext`: the caller's values without its cancellation, and a timeout of its
own (10 s), so a worker that is shutting down cannot cut a claim in half.

- **Keeps:** I2 (no row is claimed twice), I7 (`attempts` only goes up), I9 (higher priority
  first), I12 (the lease is on the Postgres clock), CLAUDE.md rule 8.
- **Proven by:** TestConcurrentClaimsNeverTakeTheSameRow (32 claimers, 2,000 rows),
  TestClaimSkipsRowsLockedByAnotherTransaction, TestClaimTakesExactlyTheClaimableStatuses,
  TestClaimsInPriorityTurnTakeHigherPrioritiesFirst, TestHighPriorityOvertakesBacklog,
  TestClaimSetsTheLeaseOnTheDatabaseClock, TestAttemptsNeverGoDown,
  TestClaimPlanReadsTheIndexInOrder (the plan reads the claim index in order, no sort, under a
  generic plan too: A3), TestQueueWritesAreNotCutOffByACancelledCaller.

### 12. Each claimed row gets one goroutine and one exit

**Code:** [`internal/dispatch/dispatch.go`](../internal/dispatch/dispatch.go): `(*Dispatcher).start`, `(*Dispatcher).enterWaiting`, `(*Dispatcher).handle`.

**Code:** [`internal/dispatch/breaker.go`](../internal/dispatch/breaker.go): `(*Breaker).Started`.

`(*Dispatcher).start` runs on the loop's goroutine. It takes a slot, asks
`(*Breaker).Started` whether this row is the breaker's probe (only when the breaker is
half-open), marks the row as waiting for its permit (`enterWaiting`), and starts
`(*Dispatcher).handle` on its own goroutine with `context.WithoutCancel`: a shutdown stops
the loop, not the sends in flight or their result writes. `handle` is the only way out for a
claimed row, and it ends in `Finish` (step 19).

- **Keeps:** I4 (every claimed row reaches Finish, or its lease ends and the reaper returns
  it); CLAUDE.md rule 5.
- **Proven by:** TestShutdownWaitsForSendsInFlightAndDoesNotCancelThem,
  TestTheHalfOpenBreakerLetsExactlyOneRowThrough, TestAClaimThatFindsNothingLeavesNoProbe, and
  TestInvariantsHoldUnderRandomFaults (every claim of every scenario has its attempt row, which
  only Finish writes).

### 13. `handle` carries the correlation id and the trace, and says "processing"

**Code:** [`internal/dispatch/dispatch.go`](../internal/dispatch/dispatch.go): `(*Dispatcher).handle`, `messageOf`.

**Code:** [`internal/observability/tracing.go`](../internal/observability/tracing.go): `WithTraceparent`.

`handle` puts the row's correlation id in its context, so every worker line about the row
carries it. It starts a delivery span as a child of the trace context the API stored
(`observability.WithTraceparent`), so the delivery joins the trace of the request. It
publishes `processing` (which never waits), turns the row into a `provider.Message` with
`messageOf`, and calls `provider.(*Client).Send` with a gate function.

- **Proven by:** TestTheDeliverySpanJoinsTheAPIRequestsTrace,
  TestEveryStatusChangeOfTheWorkerIsPublished,
  TestEveryWorkerLineAboutANotificationCarriesItsCorrelationID.

### 14. The provider client builds one request with one deadline

**Code:** [`internal/provider/provider.go`](../internal/provider/provider.go): `New`, `(*Client).Send`, `(*Client).send`, `body`, `Message`.

`provider.New` built the client once per dispatcher: HTTP/1.1 only, so one connection carries
one request; redirects are not followed (`http.ErrUseLastResponse`), because following one
would send a second request without a permit; at most 50 idle connections, one per slot.
`(*Client).send` puts one deadline on the whole send (`SEND_TIMEOUT`, 15 s): connecting,
waiting at the gate, writing and reading the response. The request is the assignment's:
`POST` to `PROVIDER_URL` with `{"to", "channel", "content"}`, the notification id as
`Idempotency-Key`, and `X-Correlation-ID` when the id is within its bound. It sets
`req.GetBody = nil`, so Go's transport can never replay the request on a second connection.

- **Keeps:** I8 (one send, one connection, one permit, at most one write).
- **Proven by:** TestTheRequestHasTheAssignmentsFormat, TestOneDeadlineCoversConnectGateWriteAndResponse,
  TestADeadReusedConnectionIsNotReplayed, TestRedirectsAreNotFollowed,
  TestTheClientSpeaksHTTP11ToAServerThatOffersHTTP2, TestACorrelationIDOutsideItsBoundIsNotSent,
  TestNewRefusesAURLThatIsNotAbsoluteHTTP, TestTheDialHasItsOwnTimeout.

### 15. The send gate: the permit is taken when the connection is ready

**Code:** [`internal/provider/provider.go`](../internal/provider/provider.go): `(*Client).send`, `Gate`.

**Code:** [`internal/dispatch/dispatch.go`](../internal/dispatch/dispatch.go): `(*Dispatcher).handle`, `(*mark).leave`.

**Code:** [`internal/ratelimit/ratelimit.go`](../internal/ratelimit/ratelimit.go): `(*Limiter).Wait`, `(*Limiter).wait`, `(*Limiter).take`, `takeScript`.

Inside `send`, the gate runs in the `GotConn` hook of an `httptrace.ClientTrace`. Go calls
that hook on the sending goroutine when the connection is ready (DNS, TCP and TLS done) and
before the request is handed to the write loop. Everything slow has happened by then; only
the write and the network are left.

The hook calls the gate function that `handle` passed, with a context limited to the gate
budget (`GATE_BUDGET`, 5 s). That function calls `ratelimit.(*Limiter).Wait`, which runs
`takeScript` in Redis:

- it reads the Redis server's clock (`TIME`) and removes the permits older than one window
  (1 s plus a margin of 50 ms) from the channel's sorted set, `ratelimit:sms`;
- if fewer than 100 permits remain, it adds one and answers "granted";
- otherwise it answers how long until the oldest permit leaves the window, and `Wait` sleeps
  that long and asks again, until its context ends.

Every worker asks the same Redis key, so the limit is shared. Go sends Redis no time; the
only clock is Redis's. When `Wait` returns, the row is no longer waiting for a permit
(`(*mark).leave`), and the loop may claim the next batch. With the permit granted, the hook
returns, and Go writes the request.

- **Keeps:** I1 (at most 100 per channel in any one-second window, at the provider), I8 (one
  permit per send), I12 (rate-limit times from the Redis clock), A6.
- **Proven by:** TestTheGateRunsOnceWhenTheConnectionIsReadyAndBeforeTheWrite,
  TestNoWindowHoldsMoreThanTheLimit, TestAPermitCountsForOneSecondPlusTheMargin,
  TestChannelsHaveSeparateWindows, TestTheLimiterSendsRedisNoClockTime,
  TestTheProviderReceivesAtMostTheLimitInAnyWindow,
  TestTheProviderReceivesAtMostTheLimitThroughTheDispatchers, and
  TestPermitToArrivalStaysWithinTheMargin (A4: the time from permit to arrival stays within
  the 50 ms margin). The limits of I1 (a frozen process, a step of Redis's clock) are in
  [architecture §7](architecture.md#7-the-rate-limit).

### 16. The provider accepts

**Code:** [`cmd/mockprovider/main.go`](../cmd/mockprovider/main.go): `newHandler`, `(*acceptances).add`.

**Code:** [`internal/recorder/recorder.go`](../internal/recorder/recorder.go): `(*Recorder).ServeHTTP`.

In the Compose stack the workers send to `http://mockprovider:9090/send` (`PROVIDER_URL`).
The mock provider is the recording server of the tests: `(*Recorder).ServeHTTP` stamps the
request the moment its handler runs, before reading the body, and answers the assignment's
contract, `202` with `{"messageId", "status": "accepted", "timestamp"}`. `newHandler` counts
what it accepted by `Idempotency-Key`, so a run can see a notification accepted twice
(`GET /stats`). To use webhook.site instead, set `PROVIDER_URL` to your webhook URL and make
it answer 202 with that body; the client sends the same request either way.

- **Proven by:** TestRecorderAnswersWithTheProviderContract, TestRecorderStampsOnArrivalNotOnResponse,
  TestMockProviderAnswersTheContractAndCountsPeaks, TestMockProviderCountsWhatItAcceptedTwice.

### 17. The client records what it saw, and decides nothing

**Code:** [`internal/provider/provider.go`](../internal/provider/provider.go): `Result`, `retryAfter`, `MaxResponseBody`, `MaxResponseHeaderBytes`.

Back in `send`, the response fills a `provider.Result`: a response arrived, its status 202,
its `Retry-After` (`retryAfter`), at most 4 KiB of its body (`MaxResponseBody`), and the
`messageId` of a 2xx JSON body. Response headers over 64 KiB are not a response
(`MaxResponseHeaderBytes`). The result holds no verdict.

- **Keeps:** I11 (what the worker reads from the provider is bounded).
- **Proven by:** TestTheResultCarriesWhatWasObserved, TestTheResponseBodyIsReadUpToItsBound,
  TestResponseHeadersOverTheBoundAreNotAResponse.

### 18. `Classify` decides, from what was observed

**Code:** [`internal/dispatch/classify.go`](../internal/dispatch/classify.go): `Classify`, `rejected`, `Verdict`.

**Code:** [`internal/dispatch/dispatch.go`](../internal/dispatch/dispatch.go): `(*Dispatcher).measure`.

**Code:** [`internal/dispatch/breaker.go`](../internal/dispatch/breaker.go): `(*Breaker).Report`.

`dispatch.Classify` reads the verdict table of [architecture §6](architecture.md#6-the-dispatcher)
top to bottom. A response always wins: a 2xx is `sent`, and the breaker learns "success".
`(*Dispatcher).measure` counts the attempt by outcome and records the wait at the gate and
the provider's latency, each an increment in memory. `(*Breaker).Report` tells the breaker
what the attempt taught.

- **Keeps:** I6 (a response always decides); CLAUDE.md rule 6.
- **Proven by:** TestAResponseAlwaysWins, TestClassifyDecidesEveryRowOfTheVerdictTable,
  TestTheVerdictTableHoldsForRealSends, TestAttemptsAndResultsAreCountedOnce,
  TestTheGateWaitAndProviderLatencyAreMeasured.

### 19. `Finish` stores the result, only if the row is still this attempt's

**Code:** [`internal/dispatch/dispatch.go`](../internal/dispatch/dispatch.go): `(*Dispatcher).finish`, `errorText`.

**Code:** [`internal/store/queue.go`](../internal/store/queue.go): `(*Queue).Finish`, `finishAttemptSQL`, `finishRowSQL`, `bounded`.

`(*Dispatcher).finish` calls `store.(*Queue).Finish` with attempt number 1. In one
transaction:

1. `finishAttemptSQL` inserts the attempt row (`ON CONFLICT DO NOTHING`, so a repeated write
   adds nothing). Its `started_at` is `now()` minus the duration Go measured: Go passes a
   duration, not a time.
2. `finishRowSQL` sets `sent`, `sent_at` and the provider's message id, only
   `WHERE id = $1 AND attempts = 1 AND status = 'processing'`.

If the update matches nothing, the result is stale: the lease ended and the reaper took the
row back, and another worker may hold it now. Then nothing but the history changes. Text from the provider is cut and cleaned
first (`bounded`), so a result write cannot fail on it every time. If the write fails, it is
retried from 100 ms, doubling up to 5 s, for at most one lease after the send. When the
status changed, `finish` publishes `sent`, counts the result, and writes one info line,
`attempt finished`, with the correlation id.

- **Keeps:** I3 (a result changes a row only with its current attempt number, while it is
  `processing`), I4, I12.
- **Proven by:** TestFinishFromAStaleAttemptChangesNothing,
  TestFinishOnARowThatIsNoLongerProcessingChangesNothing, TestRepeatedFinishAddsNoSecondAttemptRow,
  TestAnAttemptNumberIsRecordedOnce, TestFinishStoresEachResultOnTheDatabaseClock,
  TestFinishStoresTextAtItsBound, TestAFailedResultWriteIsRetriedAndNotSentAgain,
  TestAStaleResultIsCountedAsStale.

### 20. Back to the client: the WebSocket, and the status

**Code:** [`internal/events/redis.go`](../internal/events/redis.go): `(*Redis).run`, `(*Redis).send`, `Subscribe`, `subscribeOnce`.

**Code:** [`internal/events/events.go`](../internal/events/events.go): `Channel`, `Event`, `Decode`.

**Code:** [`internal/api/ws.go`](../internal/api/ws.go): `(*server).websocket`, `followed`, `(*Hub).follow`, `(*server).currentState`, `(*Hub).Deliver`, `(*server).stream`.

**Code:** [`internal/store/notifications.go`](../internal/store/notifications.go): `(*Notifications).Get`.

The worker's publisher goroutine (`(*Redis).run`) sends the buffered `sent` event to the
Redis channel `notifications:status` (`events.Channel`) in one pipeline (`(*Redis).send`).
Every API instance has one subscription (`events.Subscribe`); it reads the event, drops one
over 1 KiB or malformed (`events.Decode`), and hands it to its hub, `(*Hub).Deliver`. The hub
puts it on the queue of every WebSocket client that follows this notification or its batch;
it never waits for a client.

A client follows a notification with `GET /v1/ws?notificationId=<id>`. `(*server).websocket`
checks the filters (`followed`), the origin and the upgrade, adds the client to the hub
(`(*Hub).follow`), and only then reads the current state (`(*server).currentState`), so no
change is missed between the two. `(*server).stream` writes the current state first, then
every event. Followed from its creation, a notification scheduled 3 s ahead gave, on the
Compose stack:

```
{"type":"status","id":"97da9252-...","status":"scheduled"}
{"type":"status","id":"97da9252-...","status":"processing"}
{"type":"status","id":"97da9252-...","status":"sent"}
```

A client that asks instead with `GET /v1/notifications/{id}` gets `(*server).get`, which reads
the row and its last 100 attempts, oldest first (`store.(*Notifications).Get`): `sent`, with
attempt 1, `outcome: "sent"`, `statusCode: 202`.

- **Keeps:** I13 (a lost event never affects delivery; the database is the truth).
- **Proven by:** TestAWebSocketClientReceivesTheSentEventOfARealDelivery,
  TestAWebSocketClientGetsTheCurrentStateThenEveryChange, TestEventsReachSubscribersThroughRedis,
  TestAMalformedOrOversizedEventIsDropped, TestLostEventsDoNotAffectDelivery,
  TestCreateAndReadANotification, TestTheAttemptHistoryIsBounded.
- **End to end:** TestOneCorrelationIDFollowsAMessageFromTheRequestToTheProvider runs the API,
  Redis and a worker together: the same correlation id is in the API's answer, on the row, on
  every worker line and in the provider's request. CI's compose job does the same through the
  running stack (`tools/e2e`).

## The branches

### A retry

The provider answers 503. `Classify` gives `retrying` with a backoff, and the breaker learns
"failure" (a 400, 413 or 422 is the message's fault instead: `failed` at once, and the breaker
learns "success").

**Code:** [`internal/dispatch/retry.go`](../internal/dispatch/retry.go): `RetryPolicy`, `(RetryPolicy).DelayAfter`.

**Code:** [`internal/store/queue.go`](../internal/store/queue.go): `finishRowSQL`, `(*Queue).Requeue`, `requeueSQL`.

**Code:** [`internal/api/notifications.go`](../internal/api/notifications.go): `(*server).retry`, `(*server).change`.

After attempt `k`, `(RetryPolicy).DelayAfter` draws the delay between half and all of
`min(2 min, 2 s × 2^(k−1))`, so rows that failed together come back apart; a `Retry-After`
that asks for longer is honoured up to the same cap. `finishRowSQL` stores `retrying` with
`ready_at = now() + delay`, and the row waits in the claim index until then. The next claim
makes it attempt `k + 1`. When the attempt was the last one allowed
(`attempts >= max_attempts`, 6 by default), the same statement stores `failed` instead, from
the row's own counters. `failed` is the dead-letter state: `POST /v1/notifications/{id}/retry`
(`(*server).retry`) calls `store.(*Queue).Requeue`, which makes the row `pending`, due now,
with `max_attempts = attempts + 6`. It never lowers `attempts`, so no attempt number is
used twice.

- **Keeps:** I5 (an accepted notification ends `sent`, `failed` or `cancelled`), I7.
- **Proven by:** TestRetryDelaysDoubleWithJitterUpToTheCap, TestRetryAfterIsHonouredUpToTheCap,
  TestARetriedRowWaitsForItsBackoff, TestARetryOnTheLastAttemptIsStoredAsFailed,
  TestRetryRequeuesOnlyAFailedNotification, TestRequeueRaisesMaxAttemptsByTheBudget,
  TestInvariantsHoldUnderRandomFaults (each scenario: two or three workers, 300 notifications,
  random provider errors, stalls and failed result writes; every notification ends final).

### A cancel

**Code:** [`internal/api/notifications.go`](../internal/api/notifications.go): `(*server).cancel`, `(*server).change`.

**Code:** [`internal/store/queue.go`](../internal/store/queue.go): `(*Queue).Cancel`, `(*Queue).change`, `cancelSQL`, `ErrWrongStatus`.

`POST /v1/notifications/{id}/cancel` calls `store.(*Queue).Cancel`. In one transaction,
`(*Queue).change` locks the row (`SELECT status ... FOR UPDATE`) and runs `cancelSQL`, which
sets `cancelled` only from `scheduled`, `pending` or `retrying`. Any other status returns
`ErrWrongStatus` with the status it found, and the API answers 409 with it; an unknown id is
404. A row being sent cannot be cancelled. A cancelled row stays cancelled: a late result
matches nothing in `finishRowSQL` (its status is not `processing`), and neither the claim nor
the reaper selects it.

- **Keeps:** I3; the lifecycle of [architecture §3](architecture.md#3-the-life-of-a-notification).
- **Proven by:** TestCancelOnlyFromWaitingStatuses, TestCancelOnlyFromWaitingStatusesThroughTheAPI,
  TestACancelledRowCannotBeRevived, TestStatusTransitionsFollowTheLifecycle.

### A scheduled notification

**Code:** [`internal/store/notifications.go`](../internal/store/notifications.go): `createSQL`.

**Code:** [`internal/store/queue.go`](../internal/store/queue.go): `claimSQL`.

With `"scheduledAt": "2030-01-01T09:00:00Z"`, `createSQL` stores the row `scheduled`, with
`ready_at` at that time. The claim only takes rows with `ready_at <= now()`, and the claim
index is ordered by `ready_at`, so future rows are never read, however many there are. When
the time comes, the row is claimed like any other. There is no separate scheduler: a
scheduled row is a row that is not due yet.

- **Keeps:** I12 (Postgres decides "is it due?"), A3.
- **Proven by:** TestAScheduledNotificationWaitsUntilItsTime, TestClaimReturnsOnlyDueRows,
  TestClaimPlanReadsTheIndexInOrder, TestAScheduledAtFromTheYear10000IsRefused.

### A worker dies in the middle of a send

**Code:** [`internal/dispatch/reaper.go`](../internal/dispatch/reaper.go): `(Reaper).Run`, `(Reaper).reap`, `ReapBatch`.

**Code:** [`internal/store/queue.go`](../internal/store/queue.go): `(*Queue).Reap`, `reapSQL`.

The worker is killed after its claim. The row stays `processing` until its lease (60 s) ends.
Every worker runs a reaper, which every 5 s (`REAPER_TICK`) calls `store.(*Queue).Reap` in
batches of 100 (`ReapBatch`) until a batch comes back short. `reapSQL` returns each expired row
to `pending`, or to `failed` when it has used its last attempt. It keeps `attempts` and
`ready_at`, so the row takes its old place in the queue, and it uses `SKIP LOCKED`, so two
reapers pass over each other's rows. The reaper publishes each returned row's status and logs
it with the row's correlation id. The next claim sends it again with the same
`Idempotency-Key`: if the provider had accepted the first send, it receives the message twice.
That is at-least-once case 1 of [architecture §6](architecture.md#6-the-dispatcher).

A worker that is stopped instead (SIGTERM) stops claiming and waits for its sends in flight,
up to its shutdown timeout (25 s), without cancelling them.

- **Keeps:** I4, I5.
- **Proven by:** TestAKilledWorkersRowComesBackThroughTheReaper (the built worker binary is
  killed while the provider holds its request; the test ends the lease in the database instead
  of waiting 60 s; the row is sent at attempt 2, attempt 1 is missing from its history, and
  the provider received it twice with the same `Idempotency-Key`), TestReapReturnsExpiredRowsToPendingOrFailed,
  TestTwoReapersAtOnceReturnEachRowOnce, TestReapSkipsRowsLockedByAnotherTransaction,
  TestTheReaperReturnsExpiredRowsAndPublishesTheirStatus, TestTheReaperCountsTheRowsItReturns,
  TestShutdownWaitsForSendsInFlightAndDoesNotCancelThem, TestWorkerBinaryExitsZeroOnSIGTERM.

### The rate limit says no

**Code:** [`internal/ratelimit/ratelimit.go`](../internal/ratelimit/ratelimit.go): `ErrNoPermitInTime`, `(*Limiter).Wait`.

**Code:** [`internal/provider/provider.go`](../internal/provider/provider.go): `(*Client).send`, `Result`.

**Code:** [`internal/dispatch/classify.go`](../internal/dispatch/classify.go): `Classify`.

Usually the limit stops a row before it is claimed: when the window is full,
`(*Limiter).Available` is 0, the room is 0, and nothing is claimed, so no attempt is spent.
`Available` is a hint, though, and every worker reads the same one, so a claimed row can still
reach a gate with no free permit. Then `Wait` waits up to the gate budget (5 s). If no permit
comes free, it returns `ratelimit.ErrNoPermitInTime`; the hook in `send` stores it in
`Result.Refusal` and **closes the connection**, so the write that follows fails and nothing
reaches the provider. `Classify` gives `retrying` in about 1 s (between 1 and 2 s), and the
breaker learns nothing: the provider was not asked. If the limiter failed instead (Redis
unreachable or stalled), the verdict is the same retry, and the breaker learns "failure". The
attempt is spent either way: nothing is given back (CLAUDE.md, rule 4).

- **Keeps:** I1, I8 (a refused send writes nothing).
- **Proven by:** TestARefusedSendNeverReachesTheProvider (5,000 refused sends, on new and on
  reused connections; none reached the recording server), TestARefusedSendsConnectionIsClosedBeforeTheTransportGoesOn,
  TestWaitStopsWhenItsContextEnds, TestAGateThatWaitsPastTheDeadlineTakesNoPermit,
  TestAHungRedisIsALimiterErrorNotAWaitThatRanOut, TestAnUnreachableRedisIsAnErrorNotAPermit,
  TestNoRowIsClaimedWhileTheWindowIsFull.

### The circuit breaker

**Code:** [`internal/dispatch/breaker.go`](../internal/dispatch/breaker.go): `Breaker`, `FailuresToOpen`, `(*Breaker).Allow`, `(*Breaker).Started`, `(*Breaker).Report`.

Each dispatcher has its own breaker, in one of three states:

- **closed:** `Allow` sets no limit. Five failures in a row (`FailuresToOpen`) open it; a
  success resets the count;
- **open** for `BREAKER_OPEN` (30 s): `Allow` is 0, so the room is 0 and nothing is claimed;
- **half-open:** `Allow` is 1 while no probe is out. `Started` marks the next row that starts
  as the probe, on the loop's goroutine, so there cannot be two. The probe's `Report` closes
  the breaker on "success", opens it again on "failure", and leaves it half-open on "nothing".

`Allow` only reads; nothing is reserved. A claim that finds no row marks no probe, and every
started row reaches `Report` through `handle`'s single exit, so a probe cannot be lost. While
open, an outage costs one attempt per open period per worker. A provider that answers 404 or
401 to everything (a wrong URL, an expired key) opens the breaker instead of draining the queue
into `failed`. One that answers 400 to everything does drain it, because 400 means "this
message is wrong"; why that trade-off was kept is in
[architecture §6](architecture.md#6-the-dispatcher), and the rows come back with
`POST /retry`.

- **Keeps:** no invariant of its own. It is how I6's row "a broken configuration does not
  drain the queue" holds for every answer but 400, 413 and 422, and I4's single exit is why
  its probe is always reported.
- **Proven by:** TestBreakerOpensAfterFiveFailuresInARow, TestBreakerHalfOpenLetsOneProbeThrough,
  TestTheHalfOpenBreakerLetsExactlyOneRowThrough, TestARefusedProbeLeavesTheBreakerHalfOpen,
  TestReportsWhileOpenChangeNothing, TestBreakerAllowOnlyReads, TestAClaimThatFindsNothingLeavesNoProbe,
  TestNotFoundAndUnauthorizedOpenTheBreakerAndDoNotDrainTheQueue, TestARedisThatRefusesWritesClaimsNothing,
  TestTheBreakerStateIsExportedWhenScraped.

## Where each invariant is kept

| # | Invariant, in short | Steps |
|---|---|---|
| I1 | At most the limit per channel in any one-second window, at the provider | 15; the rate-limit branch |
| I2 | A row is never claimed by a second worker while its lease is valid | 11 |
| I3 | A result changes a row only with its current attempt number, while `processing` | 19; the cancel branch |
| I4 | Every claimed row reaches Finish, or the reaper returns it | 12, 19; the worker-dies branch |
| I5 | An accepted notification ends `sent`, `failed` or `cancelled` | the retry and worker-dies branches |
| I6 | A provider response always decides the verdict | 18; the breaker |
| I7 | `attempts` only goes up | 11; the retry branch |
| I8 | One send: one connection, one permit, at most one write | 14, 15; the rate-limit branch |
| I9 | Higher priority is claimed first; at most one claim batch waits for permits | 10, 11 |
| I10 | One idempotency key, one notification or batch | 7, 8 |
| I11 | No request makes the server allocate or return more than its bound | 1, 2, 4, 5, 6, 17 |
| I12 | Queue times from Postgres, rate-limit times from Redis | 7, 11, 15, 19; the scheduled branch |
| I13 | A status event never delays or changes a send, a result write or an answer | 8, 20 |

One test checks I1, I4 and I5 together, under random faults, through the real client and the
recording server: TestInvariantsHoldUnderRandomFaults. Its long runs (606 scenarios) are in
[measurements.md](measurements.md).
