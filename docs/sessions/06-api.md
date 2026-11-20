# Session 6: the API

- **Date:** 2026-11-20.
- **Lead:** Claude Opus. **Builder:** one Claude Opus subagent, for the OpenAPI specification
  and its test, the end-to-end tool and CI's compose job. **Reviewer:** one fresh Claude Opus
  instance, run once after the code was green
  ([review record](../reviews/06-session-6-review.md)).
- **Brief:** [plan.md, "6. API"](../plan.md#6-api).

## What was built

| Part | What it is | Evidence |
|---|---|---|
| Create (`POST /v1/notifications`) | Validates, renders a template if named, inserts with `ON CONFLICT (idempotency_key) DO NOTHING`; the stored row's request hash decides between 200 and 409. Postgres decides `scheduled` or `pending` with `now()` | TestCreateAndReadANotification, TestAScheduledNotificationWaitsUntilItsTime, I10 (§11) |
| Batch (`POST /v1/notifications/batch`) | Decoded one item at a time up to 1000; every item validated before any is stored; the rendered total measured before rendering; one transaction; item `i` has the id `UUIDv5(batchId, i)` | TestABatchIsValidatedWholeBeforeAnyIsStored, TestConcurrentIdenticalBatchesCreateOneBatch, I11 (§11) |
| Status, batch summary, list | The row with the last 100 attempts and their total; a count per status; summaries without content, `ORDER BY created_at DESC, id ASC` with a cursor on `(created_at, id)` | TestTheAttemptHistoryIsBounded, TestTheBatchSummaryCountsEveryStatus, TestTheListIsOrderedAndPagedByCursor |
| Cancel, retry | The queue's Cancel and Requeue (session 2); 409 carries the status found; `/retry` gives `MAX_ATTEMPTS` more attempts | TestCancelOnlyFromWaitingStatusesThroughTheAPI, TestRetryRequeuesOnlyAFailedNotification |
| Templates | Immutable; `{{.name}}` substitution only, any other `{{` refused at creation; compiled once per request, on first use; measured from the compiled parts | TestTemplatesAreCreatedOnceAndRendered, TestTheRenderedLengthIsMeasuredBeforeRendering, TestATemplateIsCompiledOncePerRequest |
| `internal/domain` | `Prepare` (every field, named), `CompileTemplate`, `Measure`, `Render`, `Hash`, `Echo`; `Variables` decodes itself and stops at the 51st | 15 unit tests of `request_test.go` |
| Errors | One form, written in one place; a name from the request passes `domain.Echo` (64 bytes); at most 100 fields and a count | TestErrorBodiesStayWithinTheirBoundForAnyRequest, TestFieldErrorsAreKeptUpToTheirBound |
| API keys | `API_KEYS`; `Authorization: Bearer` or `X-API-Key`; each configured key compared in constant time | TestAPIKeysAreRequiredWhenConfigured |
| OpenAPI | `internal/api/openapi.yaml`, embedded, at `/openapi.yaml` and `/docs`; every path, method and status code produced by the real server, and every route in the specification (built by the builder subagent) | TestOpenAPIMatchesTheServer |
| `tools/e2e`, `make e2e`, CI | Through the API on the Compose stack: one notification and its repeat, a batch of N per channel, waits until all are `sent`; replaces `make enqueue` (built by the builder subagent) | CI's compose job |
| Rendering slots, database deadline | A decoded create or batch waits for one of 4 slots before it renders; the rendered total of a request is 8 MiB; every `/v1` request's database work has a 5 s deadline (after the review) | TestConcurrentRequestsRenderInBoundedMemory, TestARequestEndsAtItsDeadlineWhenPostgresStalls |
| Server timeouts | The whole request, body included, within 30 s; the answer written within 30 s of the request's headers; the headers within 5 s (session 1) | TestSlowRequestBodiesAreCutOff, TestAnswersToAClientThatDoesNotReadAreCutOff, TestTheServerHasTheTimeoutsOfTheDesign |
| `internal/config` | `API_KEYS`; `MAX_ATTEMPTS` moved from the worker's budget to the shared settings, since the API reads it and the worker never did | TestLoadReadsEverySettingFromTheEnvironment, TestMaxAttemptsMustBePositive |

## How the tests came first

1. The design (`28f42a8`): section 8 gained the shape of every endpoint, the request hash and
   the insert that makes concurrent identical requests end in one row, the error form, the
   bounded decoding and the new bounds, before any code.
2. The tests (`bf4666d`), against stubs that compile: every new domain test and every API test
   failed (`go test ./internal/domain ./internal/api`).
3. The code (`1b53a55`). Every API test passed on its first run, which made the guard run the
   real check of the tests: see "Guards that proved nothing".
4. Two gaps the lead found after the code, each with a red test first: a request body and an
   answer had no time bound (`233ea6e` red, `04871f8` the fix), and a hand-made list cursor
   was accepted (`d5b5cf8`, the test red before the change).

## Decisions this session made

- **No service layer.** Section 14 listed `internal/service`. Every use case is "validate, then
  one statement or one transaction", so the layer would only forward calls; it was removed from
  the design instead of being built.
- **Batch items carry no idempotency key.** A batch is retried as a whole under its header. A
  key per item would need a second conflict path inside the batch's transaction.
- **Batch ids are derived, not stored.** `UUIDv5(batchId, i)` gives a repeated batch the same
  ids in the same order with no column for the order.
- **Templates are immutable.** The same name, channel and body again is a 200; anything else a
  409. A notification's content is rendered when it is created, so a template change could not
  reach it anyway; immutability makes that plain.
- **Error texts are fixed; only names from the request are echoed, cut to 64 bytes in one
  function.** The first draft also cleaned every text in the error writer. No request could
  reach that code, so no test could prove it; it was removed before it was committed, and the
  64 KiB bound was measured instead (28,494 bytes in the worst case the form allows).
- **`MAX_ATTEMPTS` is a shared setting.** It was in the worker's timing budget, which the
  worker never read; the API needs it for new rows and `/retry`.

## Measured

| What | Result | Command |
|---|---|---|
| 8 MiB of `{}` items (2.8 million) in a batch | 400 at the 1,001st, under 1 MiB allocated | `go test -count=1 -v -run TestABatchOfManyEmptyItemsIsRefusedWithoutDecodingThemAll ./internal/api` |
| 1 MiB of variables | 400 at the 51st, 4 MiB allocated | `go test -count=1 -v -run TestManyVariablesAreRefusedWhileDecoding ./internal/api` |
| A batch that would render 384 MB | 400 before rendering, 4 MiB allocated | `go test -count=1 -v -run TestARequestOverTheRenderedTotalIsRefusedBeforeRendering ./internal/api` |
| 500 items of a template with 16,000 placeholders (8 MB rendered and stored; 1000 items and 134 MiB before the rendered bound became 8 MiB) | 201, 72 MiB allocated; compiled per item it fails the test | `go test -count=1 -v -run TestATemplateIsCompiledOncePerRequest ./internal/api` |
| Error bodies | every field of 1000 items wrong: 8,690 bytes; 1000 items with a name of control characters, the worst case (the reviewer's): 47,794 bytes; bound 65,536 | `go test -count=1 -v -run TestErrorBodiesStayWithinTheirBoundForAnyRequest ./internal/api` |
| 40 batches at once, each rendering 8 MB from 44 KB | the heap grew by 745 MiB without the slots; 144 to 199 MiB with them, six runs | `go test -count=6 -v -run TestConcurrentRequestsRenderInBoundedMemory ./internal/api` |
| A list page 150,000 rows deep, 200,000 rows | 150,001 rows read and dropped in 14 ms before the review's fix; 1 row in 0.02 ms after | `go test -count=1 -v -run TestAListPageReadsTheIndexFromItsCursor ./internal/store` |
| The Compose stack through the API | 1501 notifications sent in 5.63 s locally and 5.62 s in CI; the mock provider's peak 100 per channel | `make up; make e2e N=500`; CI's compose job |
| 20 identical requests at once | one 201, 19 times 200, one row | `go test -count=1 -run TestConcurrentIdenticalRequestsCreateOneNotification ./internal/api` |

## Guards that proved nothing

56 guards were added before the review (52 in `50fef12`, 3 for the server timeouts, 1 for the
cursor) and 14 after it, 70 in all; the list went from 179 to 249. Each was run with
`go run ./tools/guards -only '<name>'` when it was written. On their first run six proved nothing, each a
fault found and fixed until its named test turned red:

| Guard | Why it proved nothing | Fix |
|---|---|---|
| every `{{` starts a placeholder | No test body had text between `{{` and a name (`{{xa}}`); the name check refused every other case | `{{xa}}` and `{{-a}}` in TestVariableNamesAreBounded |
| the hash separates fields | The test's example (text moved between recipient and content) differed anyway, because the channel lies between them | An example where a variable's value is followed by the priority |
| the same guard, second form | The mutation wrote eight zero bytes in place of the length, which still separates fields | The mutation removes the length |
| a template's channel is the notification's | The mutation did not compile | A mutation that compiles |
| a template is compiled once per request | The mutation did not compile | The cache check is the mutated line |
| a key with another request is a conflict | The mutation did not compile | A mutation that compiles |

During the first run the machine's disk filled (22 GB of Go build cache, from the
compilations of every guard of every session), which stopped Docker and made seven guards fail
to build. The cache was cleared (`go clean -cache`), Docker restarted, and those guards run
again; none of the seven was at fault.

A guard was also kept from being written: the error writer cleaned every text a second time,
and no request could reach that code, so no test could fail without it. It was removed
(`3d85b03`).

Lint found one more gap: the list cursor's time was converted from `uint64` without a bound,
and a hand-made cursor answered an empty page instead of a 400 (`d5b5cf8`).

## The review

One fresh Claude Opus reviewer, run once on `d6d21cb` after CI was green, with no time limit
([review record](../reviews/06-session-6-review.md)). It confirmed the idempotency under
concurrency (200 identical creates, one row) and every bound it probed, and found three major
problems: no bound across requests on what templates render (40 small requests took the API
to 1.1 GiB), the provider URL's secret path in the errors the API shows (a class session 5
had fixed in two instances only), and a list cursor that was a filter, not an index bound.
Six minor findings and seven nits followed. Every finding was answered as a class: the design
first (`0afa23c`), then red tests (`852fc68`), then the fixes (`e585488`, `e89a575`), and a
guard for each. Two parts were removed while fixing: the decoder's `DisallowUnknownFields`
(the types check their names) and the second rule for field names it implied.

Fixing the findings also found a test of session 2 that failed by chance: TestAttemptsNeverGoDown
missed a cancel in 2 of 30 runs; it now runs until every operation has taken effect
(`9261a5e`).

## What is still open

- Status events go to `events.Discard` until session 7 adds the Redis publisher.
- Quotas per client: a template still lets a request of a few KB store up to 8 MiB, and
  nothing bounds what one client stores over time (§16). A gateway is the usual place.
- The list's filters other than the batch have no index: on 2 million rows `status=failed`
  is a sequential scan of 65 ms (the reviewer's measurement). Bounded by the 5 s deadline;
  session 8's load test decides whether they need an index.
- TestOpenAPIMatchesTheServer checks that every code the document lists is produced, not that
  the server produces no other (review F8).
- 4 rendering slots per instance means a large batch can make single creates wait behind it;
  the wait is not measured. Session 8's load test should measure it.
- A request cut off at its database deadline may still have committed its write (§8); only an
  idempotency key makes the retry safe.

## CI

CI_PLACEHOLDER
