# Review of session 6: the API

- **Date:** 2026-11-20.
- **Reviewer:** a fresh Claude Opus instance, read-only, with the rules of
  [`.claude/agents/reviewer.md`](../../.claude/agents/reviewer.md). It was given the assignment
  text, the session's brief from [`plan.md`](../plan.md) and the repository at `d6d21cb`, with
  a Compose stack of it running. It was not given the author's reasoning or conclusions, and
  it had no time limit.
- **Scope:** commits `f28875c..d6d21cb`: `internal/api`, `internal/domain`'s requests and
  templates, `internal/store`'s API statements, `internal/config`, `tools/e2e`, the OpenAPI
  document, Compose, CI, the guards and the design.
- **Runs:** once, after the code was green locally and in CI (run 44781178603), as the plan
  requires. There was no second round. Each fix below is verified by its own named test and by
  `make guards`, not by another review.

## What the reviewer ran

- `go build`, `go vet`, `make test` with the race detector (pass, 1:19), `make lint` (0
  issues), `make check-docs` (pass), and `make guards` in a scratch clone: "235 guards, 235
  real, 0 failed", 18:24.
- Live requests against the Compose stack, with and without `API_KEYS=k1,key-two make up`:
  every body bound at, under and over it; batches at and over 1000 items; bodies built to be
  expensive to decode or to render; boundary values of every field; up to 200 concurrent
  identical creates and 50 concurrent identical batches of 1000; malformed, truncated and
  mistyped JSON, unknown and duplicate fields; wrong, missing and malformed keys; the list
  with 2 million rows and hand-made cursors; cancel and retry from every status; slow clients;
  Postgres paused for 20 and 40 s; a worker with a provider URL holding a secret path.
- It could not check a real provider, and did not push the API's memory past 40 concurrent
  requests.

## Verdict, as reported

Every endpoint of §8 is there and behaves as the brief asks. Idempotency held under
concurrency (200 identical creates: one 201, one id; 50 identical batches of 1000: one batch),
every bound probed sits where §8 puts it, and decoding stays cheap for every hostile body. Two
major problems: each request is bounded but nothing bounds them together, so a 5 KB request
cost about 45 MB of API memory; and the provider URL's secret path reached API clients. The
list cursor cost time in proportion to its depth. Three major findings, six minor, seven nits.

## Findings and resolutions

| # | Severity | Finding | Verified by the reviewer | Resolution | Verified by |
|---|---|---|---|---|---|
| F1 | major | Each request is bounded; nothing bounds them together. A template of 16,000 placeholders and batches of 1000 items of 131 KB each: 40 at once took the API from 58.8 MiB to 1.094 GiB. A template of 99,990 random four-byte characters and batches of 41 items, 5,431 bytes each: 20 at once, 984.5 MiB peak and 238 MB more in the database. | Yes | Class: an amplification bounded per request and not across requests. Two rules (§8, "Rendering is bounded across requests"): the rendered content of one request is at most 8 MiB, the batch's body bound, so it is never more than a client could send as plain content; and rendering and storing run in 4 slots per API instance, so what waits is what clients sent. How much one client may store over time is a quota per client, left out (§16). | TestConcurrentRequestsRenderInBoundedMemory (40 batches at once, each rendering 8 MB from 44 KB: the heap grew by 745 MiB before, 144 to 199 MiB in six runs after); TestTheRenderedTotalOfARequestIsBounded; guards "at most 4 requests render at once (review F1)", "the rendered total is the batch's body bound (review F1)" |
| F2 | major | The error of a send quoted the provider URL: Go's `*url.Error` masks a password but not the path. It was stored as `last_error` and in the attempt history, logged, and from session 6 shown by `GET /v1/notifications/{id}`; on webhook.site the path is the credential. The same class as session 5's N4, which covered the start-up log line and `provider.New`. | Yes | In the provider client, outside this session's scope, so the design changed first (§7) and the change is the narrowest: `Send` keeps the `*url.Error`'s operation and cause and drops the URL. `Classify` does not read the error. | TestASendErrorDoesNotQuoteTheURL (a closed port and a server that closes the connection; failed before on the path, the query and the user); guard "a send's error does not quote the URL (session 6's review, F2)" |
| F3 | major | The list cursor was a filter, not an index bound: on 2 million rows a page 1.5 million deep read and dropped 1,500,001 rows (57 ms); through the API a page took 4 ms at depth 2 and 299 to 339 ms at 1.9 million, and 20 clients paging deep slowed a create from 2 ms to 576 to 893 ms. Filters other than the batch have no index either (`status=failed` on 2 million rows: a parallel sequential scan, 65 ms). | Yes | The cursor's condition starts with the redundant `created_at <= c`, which Postgres uses as the bound of the index scan (§8, "List"). The filters without an index are bounded by the request's deadline; whether to index them is open for session 8's load test. | TestAListPageReadsTheIndexFromItsCursor (200,000 rows, a page 150,000 deep: 150,001 rows read and dropped in 14 ms before, 1 row in 0.02 ms after); guards "the list cursor bounds the index scan (review F3)", "the list cursor excludes the last item" |
| F4 | minor | A `scheduledAt` from the year 10000 in UTC (`9999-12-31T23:59:59-23:59`) was stored, and every answer about the row was a 200 or 201 with no body: `writeJSON` wrote the status and then failed to encode the time. | Yes | Class: an answer that cannot be encoded is a 2xx without a body. `writeJSON` encodes before it writes the status, and a failure is a logged 500; `scheduledAt` must lie before the year 10000 in UTC (§8, "Scheduling"). | TestAScheduledAtFromTheYear10000IsRefused, TestAnAnswerThatCannotBeEncodedIsA500; guards "scheduledAt is before the year 10000 (review F4)", "an answer is encoded before its status (review F4)" |
| F5 | minor | "Database time per API request: 5 s" did not hold when Postgres stalled: the statement timeout is enforced by Postgres. Paused 20 s: three creates answered 201 after 22.07 s. Paused 40 s: the client got no answer after 40 s, and the row existed. | Yes | Every `/v1` request's database work has a deadline of 5 s, set in one place (§8, "Database time"); a request past it answers 500. A write cut off may still commit: a client that retries with an idempotency key gets the stored row, never two. | TestARequestEndsAtItsDeadlineWhenPostgresStalls (through a proxy that stops forwarding: no answer within 10 s before, a 500 at the deadline after); guard "a request's database work has a deadline (review F5)" |
| F6 | minor | The repeat of a create answered `"attemptHistory": [], "attemptHistoryTotal": 0` for a row that had attempts. | Yes | A repeat answers the stored row through `Get`, as the status endpoint does. | TestARepeatedCreateAnswersWithItsAttemptHistory (the repeat's body equals the status answer); guard "a repeat answers the stored row with its history (review F6)" |
| F7 | minor | "Unknown fields are refused", but `{"Content": ...}`, `{"CONTENT": ...}` and a field given twice were accepted, while a duplicate variable and a duplicate `notifications` key were refused. | Yes | Class: two rules for field names. One rule in one place: every object the API decodes checks its names exactly, each once, before it is decoded (`domain.Input`, `domain.TemplateInput`; the variables and the batch already did). The decoder's `DisallowUnknownFields` is removed: the types do it. | TestFieldNamesAreMatchedExactlyAndOnce, TestUnknownFieldsAndWrongTypesAreRefusedByName; guards "an unknown field is refused", "a field name is given once (review F7)", "an unknown field's name passes Echo" |
| F8 | minor | The OpenAPI document did not match the server in four places: no operation listed its 500 and `internal` was not a code; the idempotency key's pattern allowed a space; unknown `/v1` paths and methods answer the router's plain-text 404 and 405; the template name had no pattern. | Yes | Every `/v1` operation lists its 500, which TestOpenAPIMatchesTheServer now produces through a closed database pool; the key's pattern has no space; the template name has its pattern; §8 and the document state that the router answers 404 and 405 in plain text. That the test checks only the codes the document lists, not that the server produces no other, remains. | TestOpenAPIMatchesTheServer |
| F9 | minor | An `Idempotency-Key` header on a single create was ignored: two requests with the header made two notifications. | Yes | It is a 400: a single notification takes its key in its body, and the header is for a batch (§8). | TestAnIdempotencyKeyHeaderOnASingleCreateIsRefused; guard "an Idempotency-Key header on a single create is refused (review F9)" |
| N1 | nit | `Authorization: bearer k1` was a 401; the scheme is case-insensitive. | Yes | Matched in any case. | TestAPIKeysAreRequiredWhenConfigured; guard "the bearer scheme in any case" |
| N2 | nit | Keys of any length were accepted, `API_KEYS=k1` included. | Yes | The API refuses to start with a key shorter than 16 characters; the error does not quote the key. | TestAPIKeysMustBeAtLeast16Characters; guard "API keys of at least 16 characters" |
| N3 | nit | `make e2e` could not pass `-key`. | Yes | `make e2e KEY=...`. | Makefile |
| N4 | nit | A type error in a batch item named the field `notifications[0].`, and `[]` as a body named the field `""`. | Yes | The trailing dot is cut; a body that is not an object is an `invalid_json` error with no field. | TestUnknownFieldsAndWrongTypesAreRefusedByName |
| N5 | nit | Templates named `.` and `..` could be created; `GET /v1/templates/..` redirects. | Yes | A template name starts with a letter or digit. | TestIdempotencyKeyAndTemplateNameBounds; guard "a template name starts with a letter or digit" |
| N6 | nit | A `scheduledAt` with nanoseconds hashed differently from the microseconds Postgres stores, so an equivalent repeat was a 409. | Yes | Times are cut to microseconds before they are hashed and stored. | TestTheRequestHashIgnoresFormAndDefaults; guard "scheduledAt is cut to microseconds (review F4)" |
| N7 | nit | The session record said 57 guards were added; the diff added 56. | Yes | The record is corrected. | The record |

## Claims and reality

- **The largest error body.** The record and §8 said 28,494 bytes; the test's names were not
  the worst case. Names of control characters, which JSON escapes to six bytes each, answer
  47,794 bytes. The test now uses them and §8 states the number; the bound of 64 KiB holds.
- **"Every status change the API makes is published."** The binary passes `events.Discard`
  until session 7; §8 now says so.
- Reproduced as claimed: 8 MiB of `{}` items refused with under 1 MiB allocated; 1 MiB of
  variables, 4 MiB; a batch that would render 384 MB refused with 4 MiB; a template compiled
  once per request; 8,690 bytes for every field of 1000 items wrong; one 201 for 20
  concurrent creates; the body bounds exact to the byte; a slow body cut at 30 s and slow
  headers at 5 s; `make e2e` with a key, 901 sent, peak 100 per channel.

## The hard questions, answered

1. *What bounds the memory and storage the API commits to at once?* Rendering is bounded by
   4 slots of at most 8 MiB each; what waits for a slot is what clients sent. Storage per
   request is at most 8 MiB; storage per client over time is a quota, left out (§16).
2. *Why did the error path keep the URL?* Session 5 fixed the two instances it was shown, not
   the class. The class is "a text that carries the URL": the provider client now drops it at
   the one place an error leaves the client.
3. *What does a list client see at 30 million rows?* With the cursor as an index bound, a page
   reads its 50 rows wherever it starts. A filter without an index reads the table until it
   fills a page, bounded by the 5 s deadline; session 8's load test decides whether those
   filters need an index.
4. *Which inputs produce a value `encoding/json` refuses?* A time after the year 9999, through
   `scheduledAt`; the input is now refused, and any other case is a logged 500.
5. *When Postgres stalls, how does a client avoid a duplicate?* It gets a 500 at the deadline
   and retries with an idempotency key, which returns the stored row if the cut-off write
   committed.
6. *Why is idempotency a body field on one endpoint and a header on the other?* A single
   notification's key is part of what it is; a batch's key belongs to the request as a whole.
   The header on a single create is now refused instead of ignored.

## The full guard run and CI

GUARDS_CI_PLACEHOLDER
