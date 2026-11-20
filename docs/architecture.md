# Architecture

This document was written before any code. It is the contract every build session works from.
When a session finds that the design is wrong, the design is changed here first, with the
reason, and then the code follows.

This is the third draft. Two rounds of independent review found flaws in the first two drafts
on paper, before any of them became code. What changed and why is in
[the design review record](reviews/00-design-review.md).

Where a statement below is an assumption that has not been measured yet, it says so. The list
is in [section 12](#12-assumptions-and-how-each-is-proven).

## 1. What the system does

A client sends a notification request (SMS, email or push) to the API. The system stores it,
delivers it to an external provider over HTTP, retries when the provider fails, and shows the
status at every step.

The hard requirements that shape the design:

| Requirement | Consequence for the design |
|---|---|
| At most 100 messages per second per channel | One shared counter for all workers, checked at the last moment before a request leaves |
| Reliable delivery | Nothing is lost when a worker dies; a failed send is retried; a message that cannot be delivered ends in a visible failed state |
| Priorities (high, normal, low) | Each priority is claimed in turn, highest first |
| Burst traffic | The API only writes to the database; delivery happens later, at the provider's pace |
| Idempotency | A repeated request with the same key creates nothing new |
| Status tracking in real time | Every status change is stored and published |

## 2. The parts

```
                 ┌────────────┐
   client ─────► │    API     │ ──── writes ────┐
   (HTTP, WS)    └────────────┘                 ▼
                       ▲                 ┌─────────────┐
                       │ status events   │  Postgres   │  notifications, attempts,
                 ┌───────────┐           │ (the truth  │  batches, templates
                 │   Redis   │           │ and the     │
                 │ rate limit│           │   queue)    │
                 │ + events  │           └─────────────┘
                 └───────────┘                 ▲ │
                       ▲                 claim │ │ result
                       │ permit                │ ▼
                 ┌───────────────────────────────────┐        ┌──────────┐
                 │              Worker               │ ─────► │ provider │
                 │  dispatcher + provider client     │  HTTP  │ (webhook)│
                 │  reaper                           │        └──────────┘
                 └───────────────────────────────────┘
```

| Part | Job | Does not do |
|---|---|---|
| **API** | Validates requests, writes rows, answers queries, pushes status events to WebSocket clients | Never sends to the provider |
| **Postgres** | Stores every notification and every delivery attempt. It is also the queue. | |
| **Worker: dispatcher** | Takes due rows from the queue and hands each to the provider client, then records the result | Never takes, holds or returns a rate-limit permit |
| **Worker: provider client** | Sends one HTTP request. Takes the rate-limit permit when the connection is ready. | Never decides about retries |
| **Worker: reaper** | Returns rows whose worker died while holding them | |
| **Redis** | The shared rate-limit counter, and a publish/subscribe channel for status events | Holds nothing that cannot be lost |

Any number of API and worker replicas can run. They share nothing except Postgres and Redis.

**Why Postgres is the queue.** The queue and the notification state are the same rows, so
cancel, schedule, retry and "what is the status?" need no synchronisation between two systems.
At 100 messages per second per channel, `FOR UPDATE SKIP LOCKED` is far inside what Postgres
can do. Past a few thousand messages per second the state would stay in Postgres and dispatch
would move to a log such as Kafka; the `Queue` interface in section 6 is the seam for that.

## 3. The life of a notification

```
 create ──► scheduled (send time in the future) ─┐
   │                                             ├──(due, claimed)──► processing
   └──────► pending (send now) ──────────────────┤                        │
                                                 │                        │ result
            retrying (waiting for next attempt) ─┘                        ▼
                 ▲                                        ┌─ delivered ──► sent
                 └──────────── failed attempt ────────────┤
                                                          └─ permanent failure,
                                                             or attempts used up ──► failed
 cancel: scheduled, pending or retrying ──► cancelled
 failed ──(POST /retry)──► pending
 processing with an expired lease ──(reaper)──► pending, or failed if attempts are used up
```

| Status | Meaning |
|---|---|
| `scheduled` | Waiting for its send time |
| `pending` | Ready to be sent |
| `processing` | A worker holds it, with a lease that expires |
| `retrying` | An attempt failed. Waiting for the next attempt time. |
| `sent` | The provider accepted it. Final. |
| `failed` | Not delivered. Final until someone calls `/retry`. This is the dead-letter state. |
| `cancelled` | Cancelled by the client before it was sent. Final. |

`scheduled`, `pending` and `retrying` behave the same way in the queue: the row may be claimed
once its `ready_at` time has come. The three names exist so that a client can see why a
notification is waiting.

The diagram is `domain.CanTransition`. TestStatusTransitionsFollowTheLifecycle walks all 49
pairs of statuses against the list of arrows above.

## 4. Data model

```
notifications
  id uuid PK                    batch_id uuid NULL → batches
  recipient, channel, content
  priority smallint             0 high, 1 normal, 2 low (strings in the API)
  status text                   one of the seven above (CHECK)
  attempts int                  number of claims so far; only ever goes up
  max_attempts int
  ready_at timestamptz          earliest time the row may be claimed
  locked_until timestamptz      lease end while processing
  idempotency_key text NULL UNIQUE, request_hash bytea
  correlation_id, traceparent   carried from the API request to the worker
  provider_message_id, last_error
  created_at, updated_at, sent_at

delivery_attempts               one row per finished attempt, written once
  (notification_id, attempt_number) PK
  started_at, duration_ms, status_code, outcome, error

batches
  id uuid PK, idempotency_key text NULL UNIQUE, request_hash bytea, total, created_at

templates
  name PK, channel, body, created_at
```

Indexes, each for one query:

| Index | Serves |
|---|---|
| `(channel, priority, ready_at) WHERE status IN ('scheduled', 'pending', 'retrying')` | The claim |
| `(locked_until) WHERE status = 'processing'` | The reaper |
| `(created_at DESC, id)`, `(batch_id)` | List and batch queries. The list is ordered `created_at DESC, id ASC`, the order of the index; its cursor compares `(created_at, id)` in that mixed order. (Noted in session 1's review, for session 6.) |

Migrations are versioned, append-only files run by a one-shot `migrate` command.

The schema is tied to package `domain` by tests, so the two cannot drift apart:

| What | Test |
|---|---|
| The tables and columns above | TestMigrationsCreateTheDataModel |
| The claim index holds exactly `domain.Claimable` | TestClaimIndexCoversExactlyTheClaimableStatuses |
| The other three indexes | TestReapListAndBatchIndexesExist |
| The CHECK constraints accept exactly the statuses, channels and priorities of `domain` | TestColumnChecksAcceptExactlyTheDomainValues |
| One row per idempotency key, separately for notifications and batches | TestIdempotencyKeysAreUniqueInTheDatabase |
| One attempt row per attempt number | TestAnAttemptNumberIsRecordedOnce |
| A migration is never changed after it is recorded in `migrations.sha256` | TestMigrationsAreAppendOnly |

## 5. The queue: five operations

All queue SQL lives in one file. Each operation is one statement, or one short transaction
where noted.

**Claim** takes up to `n` due rows of one channel and one priority:

```sql
UPDATE notifications
SET status = 'processing', attempts = attempts + 1, locked_until = now() + $lease
WHERE id = ANY(ARRAY(
    SELECT id FROM notifications
    WHERE channel = $1 AND priority = $2
      AND status IN ('scheduled', 'pending', 'retrying')
      AND ready_at <= now()
    ORDER BY ready_at
    LIMIT $n
    FOR UPDATE SKIP LOCKED))
RETURNING ...
```

- `SKIP LOCKED`: two workers never take the same row and never wait for each other.
- For a fixed channel and priority the index is ordered by `ready_at`, so the database reads
  due rows in order and stops after `n`. Rows that are not due yet lie past the end of the
  range and are never read, however many there are.
- The claim is called once per priority, highest first, by the dispatcher (section 6). Each
  call is a complete statement. Rows returned by one call are started before the next call is
  made, so there is no combined result that a later failure could drop.
- The `id = ANY(ARRAY(...))` form is deliberate. The design review measured the more common
  `id IN (SELECT ...)` form under a forced generic plan (the plan a cached prepared statement
  can switch to): it scanned the whole table, 293 ms on 2 million rows. The `ANY(ARRAY(...))`
  form stayed under 1 ms.

**The attempt number.** `attempts` after a claim is that claim's attempt number. It does two
jobs with one rule: **it only ever goes up.** It counts attempts against `max_attempts`, and it
is the fencing token: a result carries the attempt number it belongs to and is only accepted
while the row still has that number. Nothing ever gives an attempt back, so a number is never
reused.

**Finish** records the result of attempt `k` in one transaction:

1. Insert the attempt row (`ON CONFLICT DO NOTHING`, so a repeated write is harmless).
2. Update the notification `WHERE id = $1 AND attempts = k AND status = 'processing'`.

If the update matches no row, the result is stale: the lease expired and the reaper has taken
the row back. The attempt row is still kept as history, and nothing else changes. There is no
exception to this rule, for any kind of result. A final status stays final.

Finish returns the row's new status, or "stale". The caller publishes a status event only when
the status really changed.

**Reap** returns `processing` rows whose lease has expired to `pending`, or to `failed` when
`attempts` has reached `max_attempts`. It takes a bounded batch with `SKIP LOCKED`, so every
worker can run it. It does not change `attempts`.

**Cancel** sets `cancelled` only `WHERE status IN ('scheduled', 'pending', 'retrying')`. A row
that is being sent cannot be cancelled; the API answers 409.

**Requeue** (`POST /retry`) moves a `failed` row to `pending` and sets
`max_attempts = attempts + the default budget`. It does not reset `attempts`, which keeps the
attempt number unique.

**Times.** Every time stored in the queue is computed by Postgres (`now()`). Go code passes
durations ("retry in 4 s"), never timestamps. One clock, so worker clock drift cannot reorder
the queue.

**Queue writes are not cancelled by shutdown.** Claim, Finish and Reap run on a context that
ignores the stop signal and has its own timeout. A claim that is interrupted half-way could
otherwise leave rows `processing` with nobody holding them.

**Known behaviours.**
- If the reply to a claim is lost after the statement committed (a dropped connection), the
  rows stay `processing` until the lease ends, and that claim still counts as an attempt.
- Rows that left the queue leave dead entries at the head of the claim index until vacuum
  removes them. The review measured the cost: after 500,000 processed rows under a
  long-running transaction a claim took 12 to 14 ms, where it normally takes under 1 ms. Every
  session the service opens sets `idle_in_transaction_session_timeout` to 10 s (its
  transactions are one statement, or Finish's two, and take milliseconds), and the API's
  sessions also set a statement timeout of 5 s (section 8). So the service's own sessions
  cannot hold vacuum back for long. The settings are made per session by `store.Connect`, so
  they hold on any Postgres, not only the Compose one (TestServiceSessionsCarryTheirTimeouts).
  A session from outside the service (a backup, an analytics query) still can hold vacuum
  back.

## 6. The dispatcher

A worker runs one dispatcher per channel. Each has its own slots, its own breaker and one
loop. This is the whole of it:

```go
func (d *Dispatcher) run(ctx context.Context) {        // d serves one channel
    for ctx.Err() == nil {
        n := d.room()                                  // see the table below
        if n == 0 { d.pause(ctx, busy); continue }
        claimed := 0
        for _, p := range Priorities {                 // high, normal, low
            rows, err := d.queue.Claim(d.channel, p, n-claimed, d.lease)
            if err != nil { d.log(err); break }
            for _, row := range rows { d.start(row) }
            claimed += len(rows)
            if claimed == n { break }
        }
        if claimed == 0 { d.pause(ctx, idle) }
    }
}

func (d *Dispatcher) start(row Notification) {         // runs on the loop's goroutine
    d.slots.Take()
    d.breaker.Started()                  // marks the probe when the breaker is half-open
    wait := d.waiting.Enter()            // "claimed, no permit yet"
    go d.handle(row, wait)
}

func (d *Dispatcher) handle(row Notification, wait *Waiting) {
    defer d.slots.Release()
    defer wait.Leave()                   // safe to call twice
    result := d.provider.Send(sendCtx, messageOf(row), func(ctx context.Context) error {
        err := d.limiter.Wait(ctx, d.channel)          // the send gate, section 7
        wait.Leave()
        return err
    })
    verdict := Classify(result, row)     // what happens to the row, and what the breaker learns
    d.breaker.Report(verdict)
    if status, ok := d.queue.Finish(row, verdict); ok {  // retried until answered
        d.events.Publish(row.ID, status)
    }
}
```

`room` is the smallest of four numbers. Each one only reads; none reserves anything outside
the worker.

| Limit | Question it answers | If it is wrong a moment later |
|---|---|---|
| Free slots (50) | How many sends can this worker run at once? | Cannot be wrong; it is local |
| Claim batch (10), and 0 while any earlier row of this worker still waits for its permit | Would new rows only queue behind rows that are already waiting? | Cannot be wrong; it is local |
| `breaker.Allow()` | Is the provider healthy enough to try? | Some attempts fail that would have failed anyway |
| `limiter.Available()` | Is there rate-limit room right now, and is Redis reachable? | The row waits at the send gate |

**Why the claim batch rule exists.** `Available()` is a hint, and every worker reads the same
hint. Without a cap, many workers would each claim a full set of rows for the same free room,
and most of those rows would wait at the gate, holding leases, in no priority order. With the
rule, a worker never has more than 10 rows that are claimed and not yet permitted. Across `W`
workers that is at most `10 × W` rows waiting at the gate at once. The longest wait is roughly
one window plus `10 × W / 100` seconds: the design review simulated about 1 s for 3 and for 6
workers and 2 s for 12, with nothing refused. This is also how long a high-priority row that
has just become due can wait behind rows claimed before it. The gate budget of 5 s is reached
at about 35 workers per channel; past that, some sends are refused and retried. (To be
measured on the real system in session 5; assumption A5.)

**One exit.** Every claimed row ends in `handle`, and `handle` ends in `Finish`. A claimed row
has no other path, so there is no path on which a row, a slot, a waiting mark or a breaker
probe can be forgotten.

**The verdict** is decided in one function, from what was observed:

| What was observed | Row | Breaker learns |
|---|---|---|
| Response 2xx | `sent` | success |
| Response 400, 413 or 422: the provider rejected this message | `failed` | success: the provider answered |
| Any other response (5xx, 429, 408, 401, 403, 404, a redirect, ...) | retry (`Retry-After` honoured) | failure |
| No response (could not connect, timeout, connection reset) | retry | failure |
| Refused at the send gate because the limiter returned an error | retry in about 1 s | failure |
| Refused at the send gate because no permit came free within the gate budget | retry in about 1 s | nothing |

- A **response always wins.** The table is read top to bottom: if a response arrived, the
  response decides, whatever else happened to the request.
- The second and third rows separate two questions. "Is something wrong with this message?"
  ends the message. "Is something wrong with the channel?" (a provider that is down, a wrong
  URL, an expired credential) is retried and counts against the breaker, so a broken
  configuration stops the channel and does not drain the whole queue into `failed`.
- "Retry" becomes `failed` when the attempt was the last one allowed.
- Every row of the table is one attempt. A refused request costs an attempt too. That is the
  price of never giving attempts back; it is paid rarely, because the hint and the batch rule
  keep rows away from a gate that has no room, and limiter errors open the breaker.

**Retries** use exponential backoff with jitter (2 s, 4 s, 8 s, ... capped at 2 minutes;
6 attempts by default).

**Circuit breaker**, one per dispatcher. It has three states and three calls.

- *Closed:* `Allow()` sets no limit. Five failures in a row open it; a success resets the
  count.
- *Open* (30 s): `Allow()` returns 0, so nothing is claimed.
- *Half-open:* `Allow()` returns 1 while no probe is marked, otherwise 0.

`Allow()` only reads. `Started()` is called when a row really starts, and in the half-open
state it marks that row as the probe. `Report()` clears the mark: a success closes the
breaker, a failure opens it again, and "nothing" leaves it half-open for the next row. If a
claim finds no row, nothing was marked, so nothing can be left hanging. Because every started
row reaches `Report()` (the single exit), the probe cannot be lost, and because the mark is
set on the loop's own goroutine, there cannot be two.

The open time is longer than the longest possible send, so no result from before the breaker
opened can arrive while it is probing. While the breaker is open, an outage uses attempts only
through probes: one row per open period per worker.

**A lost result write.** If writing the result fails after a send, the write is retried with
backoff until the database answers: either the result is stored, or the row is no longer this
worker's (stale). The retries stop after one lease length at the latest. If the result was
never stored, the reaper returns the row and it is sent again.

**Shutdown.** On a stop signal the loop stops claiming and waits for the rows in flight. A send
in flight is not cancelled; it finishes within the send timeout. If the shutdown timeout
passes first, the worker exits and the rows it still held come back through the reaper.

**At-least-once.** A notification can reach the provider twice in three cases:

1. A worker is killed after the provider accepted the message and before the result was stored.
2. The result write keeps failing until the reaper has taken the row back.
3. An attempt ended without a response (timeout, reset) although the provider had processed it.

In all three the provider receives the same `Idempotency-Key` (the notification id) again. A
provider that honours the header drops the repeat. webhook.site does not, so there a repeat is
visible. In cases 1 and 2, if the attempt was the last one allowed, the row ends as `failed`
although the provider accepted the message; the system cannot know. Exactly-once delivery
across a network call is not possible; the design keeps the cases few and names them.

## 7. The rate limit

**The guarantee:** the provider does not receive more than 100 requests for one channel in any
one-second window, however many workers run, and whatever stalls before the permit is taken.
The limits of this guarantee are listed at the end of this section.

**The counter** is a sliding-window log in Redis: a sorted set of the times of recent permits,
updated by one Lua script that uses the Redis server's clock. A permit is granted when fewer
than 100 permits are younger than the window. `Available` reads the same set without changing
it.

**The window is one second plus a margin** (50 ms by default). A permit is taken on the
worker, and the request arrives at the provider slightly later. If that delay varies by at
most the margin, then the requests that arrive within any one second were all permitted
within one second plus the margin, and there are at most 100 of those. The margin has a
price: 100 permits per 1.05 s is a top speed of about 95 messages per second per channel.

**The send gate: one place, at the last moment.** The provider client takes the permit when
the HTTP connection is ready and the request is about to be written (`httptrace.GotConn`).
Everything slow and variable has already happened by then: the claim, the database, DNS, the
TCP and TLS handshakes. Only the request write and the network remain.

```go
GotConn: func(info httptrace.GotConnInfo) {
    if err := gate(gateCtx); err != nil {    // limiter.Wait, bounded by the gate budget
        refused.Store(true)
        info.Conn.Close()                    // nothing can be written on this connection
        cancel()                             // end the request
    }
}
```

Five properties make "one send, one connection, one permit, at most one write" true:

1. **The hook runs before the write.** `GotConn` is called on the caller's goroutine before the
   transport hands the request to its write loop.
2. **A refused request is stopped by closing its connection, then cancelling.** Cancelling
   alone is not enough: the write loop does not look at the context
   ([experiment 01](experiments/01-gate-refusal.md)).
3. **The transport never replays a request.** Go replays a request on a new connection when a
   reused connection turns out to be dead, and it treats a request with an `Idempotency-Key`
   header as safe to replay. A replay would be a second write inside one attempt. The client
   sets `req.GetBody = nil`, which turns replays off; a dead connection then becomes an
   ordinary failed attempt, and the retry is the queue's decision. (Measured in the design
   review: with the default, the server received the request twice while the client saw one
   success.)
4. **HTTP/1.1 only**, set through `Transport.Protocols`, so one connection carries one
   request and closing it cannot hurt another. A test checks the protocol against a TLS server
   that offers HTTP/2.
5. **Redirects are not followed.** Go's client follows a 301, 302 or 303 by sending a `GET`
   without the body on a second connection, and the final 200 would look like a delivery
   (measured in the design review: two permits, and `sent` for a message the provider never
   accepted). `CheckRedirect` returns `http.ErrUseLastResponse`, so a redirect is just "any
   other response".

**One deadline per send.** A send has a single context deadline (15 s) that covers connecting,
waiting at the gate, writing and reading the response. The gate waits on a child of that
context limited to the gate budget (5 s). `http.Client.Timeout` is not used, because the hook
cannot see it. Dial and TLS handshake have their own shorter timeouts (3 s each).

**The connection pool** keeps as many idle connections as there are slots; with Go's default
of 2 the review measured 686 new connections in 10 s and 6% less throughput. Idle connections
are dropped after 10 s. A connection that the provider closes while a request waits on it at
the gate fails that attempt (nothing is replayed, property 3); the short idle time keeps this
rare, and it is counted in a metric.

The limiter **fails closed**: when a permit cannot be taken, nothing is sent. When Redis is
unreachable `Available()` fails too, so nothing is even claimed. When only the permit script
fails (a read-only replica after a failover, Redis out of memory), sends are refused, and
those refusals open the breaker.

There is no other place where a permit is taken, held, checked for age or given back.

**Limits of the guarantee.** In each case the provider's 429 is the backstop: it is retried
and it counts towards the breaker.

| Case | Effect |
|---|---|
| Redis loses its data (restart without persistence, failover to an empty replica) | The counter restarts empty; for one window the provider can receive up to twice the limit |
| A worker process is frozen for longer than the margin between taking a permit and writing (CPU starvation, a paused container), or the network delays a request by more than the margin | Requests permitted in one window can arrive in the next. The excess is at most the number of delayed requests. |
| The Redis server's clock jumps forward | Permits expire early, once |

## 8. The API

| Method and path | Purpose |
|---|---|
| `POST /v1/notifications` | Create one |
| `POST /v1/notifications/batch` | Create up to 1000; all validated before any is stored |
| `GET /v1/notifications/{id}` | Status, with the attempt history |
| `GET /v1/batches/{id}` | Batch summary: counts per status |
| `GET /v1/notifications` | List: filter by status, channel, batch, date range; cursor pagination |
| `POST /v1/notifications/{id}/cancel` | Cancel before sending |
| `POST /v1/notifications/{id}/retry` | Re-queue a failed notification |
| `POST /v1/templates`, `GET /v1/templates`, `GET /v1/templates/{name}` | Templates |
| `GET /v1/metrics` | Queue depth, success and failure rates, latency |
| `GET /v1/ws` | WebSocket: live status events, filtered by notification or batch |
| `GET /healthz`, `GET /readyz` | Liveness; readiness: Postgres answers a ping within 2 s. Redis is pinged and reported too, but does not decide. |
| `GET /metrics` | Prometheus |
| `GET /docs`, `GET /openapi.yaml` | OpenAPI specification and viewer |

**Idempotency.**
- A notification may carry an `idempotencyKey`. The key is unique in the database. A repeat
  with the same key and the same content returns the first notification. The same key with
  different content is a 409. The comparison uses a hash of the request stored with the row.
- A batch may carry an `Idempotency-Key` header. It is stored on the batch row, with a hash of
  the whole request. A repeat returns the same batch. Batch keys and notification keys live in
  different columns, so they cannot collide.

**Scheduling.** `scheduledAt` in the future creates the row as `scheduled` with that
`ready_at`.

**Templates.** A template body contains `{{.name}}` placeholders. Rendering is substitution
only; no template language is executed. A template is compiled once per request, and the
rendered length is calculated before anything is rendered.

**Validation** happens before anything is stored: required fields, the recipient format per
channel (E.164 phone number, email address, device token), content length per channel, and
known enum values. Errors name the field.

**Bounds.** Every input and every output has a stated limit, and each limit has a test.

| What | Limit |
|---|---|
| Request headers | 64 KiB, request line included, arriving within 5 s (TestRequestHeadersOverTheBoundAreRefused, TestSlowRequestHeadersAreCutOff) |
| Request body | 1 MiB (8 MiB for a batch) |
| Batch size | 1000 notifications |
| Content after rendering | Per channel: SMS 1600 characters, push 4096, email 100,000 |
| Template variables per notification | 50; names up to 64 characters |
| Total rendered content per request | 16 MiB |
| Validation errors in one response | 100 fields, 300 characters each, plus a count of the rest |
| List page | 100 items; items are summaries without `content` |
| Metrics window | 1 hour |
| Database time per API request | Statement timeout of 5 s |
| WebSocket | 1000 connections per API instance; a client that cannot keep up is disconnected |
| `X-Correlation-ID` header | 1 to 64 characters, letters, digits, `-`, `_` and `.`; any other value is replaced by a new id, never stored or logged (TestCorrelationIDBound, TestCorrelationIDIsAcceptedOrReplaced) |

**Readiness.** The API needs only Postgres to accept and answer requests: it writes rows, and
status events through Redis are best-effort (section 9). So an API whose Redis is unreachable
stays ready and keeps accepting notifications; `/readyz` reports `"redis": "unreachable"` for
the operator (TestReadyzStaysReadyWhenOnlyRedisIsUnreachable). Until session 1's review the
design made Redis decide too, which would have taken every API instance out of service during
a Redis outage for no reason. Each ping is bounded by 2 s, with the real clients against a
server that accepts connections and never answers
(TestReadyzAnswersWithinItsTimeoutWhenRealClientsHang). Dependency errors are logged, not
shown (TestReadyzDoesNotShowDependencyErrors).

**Every Redis client is built in one place** (`internal/redisclient`), with context deadlines
switched on. The go-redis default ignores a context's deadline on the socket, so a 2 s
deadline waited 5 s against a paused Redis (found in session 1's review). The rate limiter's
wait at the gate (section 7) depends on the same property
(TestRedisClientHonoursContextDeadlines).

**Authentication** is optional and off by default for local use: when `API_KEYS` is set,
every `/v1` request needs a key (`Authorization: Bearer` or `X-API-Key`).

## 9. Status events and WebSocket

Whoever changes a status publishes an event to a Redis channel after the change is committed.
Every API instance subscribes and forwards events to its WebSocket clients that asked for that
notification or batch.

Events are best-effort. An event can be lost (a client was disconnected, Redis restarted), and
a lost event never affects delivery. The database is the truth; a client can always ask for
the current status.

## 10. Observability

- **Logs:** structured JSON (`log/slog`). The API accepts or creates an `X-Correlation-ID`,
  stores it on the notification row, and the worker logs it on every line about that
  notification. One id follows a message from the HTTP request to the provider call.
- **Metrics:** Prometheus counters and histograms for requests, queue depth per channel and
  status, the age of the oldest due row, attempts by verdict, provider latency, wait at the
  send gate, refusals at the gate, breaker state and reaped rows. `GET /v1/metrics` gives the
  main figures as JSON for a recent window.
- **Tracing:** OpenTelemetry. The trace context is stored on the row, so the worker's delivery
  span joins the trace of the API request that created the notification.
- **Alerts:** a small set of Prometheus rules. Every rule has a `promtool` test, and CI fails
  when a rule has none. A rule that cannot fire is not shipped.

## 11. Invariants

Each invariant is enforced in one place and has at least one test that fails when the
enforcing line is disabled (`make guards` runs that check). The test names are filled in by
the session that builds the part.

| # | Invariant | Enforced in | Tests so far |
|---|---|---|---|
| I1 | The provider does not receive more than the limit for a channel in any one-second window (within the limits listed in section 7) | Send gate + limiter window | |
| I2 | A row is never claimed by a second worker while its lease is valid | `SKIP LOCKED` and the status filter in Claim | |
| I3 | A result changes a row only if it carries the row's current attempt number and the row is still `processing` | The `WHERE` of Finish | The database part: TestAnAttemptNumberIsRecordedOnce (a repeated Finish cannot add a second attempt row). The `WHERE` of Finish: session 2. |
| I4 | Every claimed row reaches Finish, or its lease expires and the reaper returns it | Single exit in `handle`; Reap | |
| I5 | An accepted notification is never lost: it ends as `sent`, `failed` or `cancelled` | I4 + retries + the `failed` state | |
| I6 | A provider response always decides the verdict | `Classify` | |
| I7 | `attempts` only goes up. Each claim is one attempt, and no attempt number is used twice. | Claim; Requeue raises `max_attempts` | |
| I8 | One send makes one connection, takes one permit and writes at most one request | Provider client (section 7, properties 1 to 5) | |
| I9 | A due row of a higher priority is claimed before a due row of a lower one. A worker has at most one claim batch that is claimed and not yet permitted. | Dispatcher loop order; the claim batch rule | |
| I10 | The same idempotency key never creates a second notification or batch | Unique indexes + request hash | The database part: TestIdempotencyKeysAreUniqueInTheDatabase. The request hash: session 6. |
| I11 | No request can make the server allocate or return more than its stated bound | Bounds table, section 8 | Request headers and the correlation id (section 8). The rest: session 6. |
| I12 | Queue times come from the Postgres clock; rate-limit times from the Redis clock | Queue SQL; Lua script | |

Tests of I1, I4, I5 and I8 observe the system **from the provider's side**: a recording HTTP
server stamps each request at the moment it arrives. They run through the real provider
client. The test kit does not contain a fake provider that could skip the send gate.

## 12. Assumptions and how each is proven

| # | Assumption | Status |
|---|---|---|
| A1 | Closing the connection in `GotConn`, then cancelling, stops a refused request from reaching the server. Cancelling alone does not. | **Measured** before the design was fixed: cancel alone let 13 of 40,000 refused requests through; close and cancel, 0 of 40,000 ([experiment 01](experiments/01-gate-refusal.md)). Permanent test in session 4. |
| A2 | With `GetBody = nil` the transport never replays a request, so one send is one `GotConn` and at most one write | **Measured** by the design reviewer (one hook call, one request received). Permanent test in session 4. |
| A3 | The claim reads the index in order with no sort and no full scan, under a custom plan and under a generic plan, also with many tied `ready_at` values and many rows that are not due yet | **Measured** by the second design reviewer on the final query: 2.8 million rows, 1 million of them future high-priority rows; 0.15 to 0.59 ms under both plans; a claim that finds nothing due reads 3 index pages. Permanent test in session 2. |
| A4 | The time from permit to arrival at the provider varies by less than the 50 ms margin, on new and on reused connections | To measure in sessions 4 and 5 at the recording server |
| A5 | With the claim batch rule, the wait at the gate stays near one window plus `10 × W / 100` seconds and nothing is refused, for up to 12 workers; throughput stays near the top speed | Simulated in the design review (93 to 96 sends per second, no refusals, worst wait 2.04 s at 12 workers). To measure on the real system in session 5. |
| A6 | The sliding-window script grants at most the limit per window under concurrent callers | Carried over from the first version, where it held in 203 randomised fault scenarios. Re-proven in session 3. |

## 13. Timing budget

These values are configuration. The worker refuses to start when a relation in the last
column does not hold.

| Setting | Environment variable | Default | Must hold | Test of the relation |
|---|---|---|---|---|
| Rate limit | `RATE_LIMIT` | 100 per second per channel | | |
| Window margin | `WINDOW_MARGIN` | 50 ms | | |
| Send timeout (connect + gate + write + response) | `SEND_TIMEOUT` | 15 s | | |
| Dial timeout, TLS handshake timeout | `DIAL_TIMEOUT`, `TLS_HANDSHAKE_TIMEOUT` | 3 s each | each < send timeout | TestDialTimeoutMustBeShorterThanSendTimeout, TestTLSHandshakeTimeoutMustBeShorterThanSendTimeout |
| Gate budget (longest wait for a permit) | `GATE_BUDGET` | 5 s | < send timeout | TestGateBudgetMustBeShorterThanSendTimeout |
| Lease | `LEASE` | 60 s | ≥ send timeout + 30 s for the result write | TestLeaseMustCoverSendTimeoutPlusResultWrite |
| Breaker open time | `BREAKER_OPEN` | 30 s | > send timeout | TestBreakerOpenTimeMustExceedSendTimeout |
| Shutdown timeout | `SHUTDOWN_TIMEOUT` | 25 s | ≥ send timeout + 5 s | TestShutdownTimeoutMustCoverSendTimeoutPlusFiveSeconds |
| Stop grace period: how long the platform waits after SIGTERM before it kills the worker | `STOP_GRACE_PERIOD` | 30 s | > shutdown timeout | TestShutdownTimeoutMustEndBeforeTheStopGracePeriod |
| Result write retries | (the lease) | up to one lease length after the send | | |
| Concurrent sends (slots) per channel per worker | `SLOTS` | 50 | | |
| Idle connections kept per provider host | `IDLE_CONNS` | 50 | = slots | TestIdleConnectionsMustEqualSlots |
| Idle connection timeout | `IDLE_CONN_TIMEOUT` | 10 s | | |
| Claim batch | `CLAIM_BATCH` | 10 | ≤ slots | TestClaimBatchMustNotExceedSlots |
| Pause when there is no room / nothing to claim | `PAUSE_BUSY`, `PAUSE_IDLE` | 20 ms / 200 ms | | |
| Reaper tick | `REAPER_TICK` | 5 s | | |
| Attempts; backoff | `MAX_ATTEMPTS`; `BACKOFF_BASE`, `BACKOFF_CAP` | 6; 2 s doubling to a cap of 2 min, with jitter | | |

Every number in the table must also be positive (TestNonPositiveSettingsAreRefused); a relation
between two settings means nothing if one of them is zero. The defaults are the table
(TestDefaultsAreTheTimingBudget). The worker checks all of this before it connects to anything,
and exits with status 2 naming each broken relation
(TestWorkerRefusesToStartWithALeaseShorterThanSendTimeoutPlus30s).

The stop grace period is set by the platform (Docker, Kubernetes), not by the worker. The
worker is told its value through `STOP_GRACE_PERIOD`, so that it can check the relation.
`docker-compose.yml` takes both the container's `stop_grace_period` and the worker's
`STOP_GRACE_PERIOD` from the same variable, so a `.env` file cannot change one without the
other (TestComposeGivesTheWorkerItsStopGracePeriod). Until session 1's review this relation
was checked only against the Compose file, and a `.env` that raised the shutdown timeout
broke it silently.

Two relations cannot be checked by a single worker. They are stated here and watched by
metrics:

- One worker can run at most `slots ÷ provider latency` sends per second. With 50 slots that
  is the full rate up to a provider latency of about 500 ms; a slower provider needs more
  slots or more workers.
- `window + claim batch × workers ÷ rate` must stay under the gate budget, which holds up to
  about 35 workers per channel. The metric for refusals at the gate shows when it does not.

## 14. Code layout

```
cmd/api, cmd/worker, cmd/migrate, cmd/mockprovider
internal/domain        types, statuses, validation, templates; no I/O
internal/store         all SQL; queue.go holds the five queue operations; migrations/
internal/redisclient   the one place a Redis client is built; context deadlines are on
internal/ratelimit     the Redis sliding window: Available, Wait
internal/provider      the HTTP client with the send gate; Result
internal/dispatch      the loop, Classify, retry policy, breaker, reaper
internal/service       use cases behind the API
internal/api           handlers, middleware, WebSocket
internal/events        publish and subscribe
internal/observability logging, metrics, tracing
internal/config        settings and the checks of section 13
internal/recorder      the recording provider server: stamps each request on arrival,
                       injects responses, latency, slow TLS setup and closed connections
internal/testkit       test database and test Redis (testcontainers); starts the recorder
tools/guards           `make guards`: disables each guard and expects a named test to fail
tools/testnames        checks that every test named in docs/ exists in the code
```

The recording server is its own package, not part of `testkit`, because `cmd/mockprovider`
runs the same code as the local provider of the Compose stack. Inside `testkit` it would pull
the testcontainers dependencies into that binary. (Changed in session 1.)

Dependencies point one way: `api` and `dispatch` depend on `service`, `store`, `ratelimit`
and `provider`; all of them depend on `domain`; `domain` depends on nothing.

## 15. Requirements coverage

| Requirement | Where | Session |
|---|---|---|
| Create, with recipient, channel, content, priority | Section 8 | 6 |
| Batch creation up to 1000 | Section 8 | 6 |
| Status by id or batch id | Section 8 | 6 |
| Cancel pending | Sections 5, 8 | 2, 6 |
| List with filters and pagination | Section 8 | 6 |
| Asynchronous processing by queue workers | Sections 5, 6 | 2, 5 |
| Rate limit of 100 per second per channel | Section 7 | 3, 4, 5 |
| Priority queue | Sections 5, 6 | 2, 5 |
| Content validation | Section 8 | 6 |
| Idempotency | Sections 6, 8 | 5, 6 |
| Delivery and retry logic | Section 6 | 4, 5 |
| Metrics endpoint | Section 10 | 7 |
| Structured logging with correlation ids | Section 10 | 1, 7 |
| Health check | Section 8 | 1 |
| Bonus: failure handling (breaker, dead-letter state, retry endpoint) | Section 6 | 5, 6 |
| Bonus: scheduled notifications | Sections 3, 5 | 2, 6 |
| Bonus: templates | Section 8 | 6 |
| Bonus: WebSocket updates | Section 9 | 7 |
| Bonus: distributed tracing | Section 10 | 7 |
| Bonus: GitHub Actions | [Plan](plan.md) | 1 |
| Docker Compose, OpenAPI, migrations, single-command tests | [Plan](plan.md) | 1, 6 |

## 16. Deliberately left out

| Not done | Why |
|---|---|
| Priority aging (a low-priority row can wait as long as higher ones keep arriving) | Strict priority is what the requirement asks for and is easy to reason about. Aging needs real traffic to tune. |
| Pausing the whole channel on one 429 | A provider that throttles a few requests at random would halve throughput. A run of 429s opens the breaker, which covers the sustained case. |
| Not counting an attempt when nothing was sent | It needs a second counter, or attempts that go down. Both were sources of defects on paper (design review, F2). The rare lost attempt is cheaper. |
| Expiry of idempotency keys | Needs a retention policy; without one, keeping keys is the safe default |
| Exactly-once delivery | Not possible across a network call. See "At-least-once" in section 6. |
| Tenants | The requirements have none. Keys, limits and templates would be scoped per tenant. |
