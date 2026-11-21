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
| `delivery_attempts (started_at)`, BRIN (migration 5, session 7) | The attempts of a recent window, for `GET /v1/metrics` (section 10). Attempt rows are only inserted, in time order, so a BRIN index is a few pages. |
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

- The row lock (`FOR UPDATE`) and the status filter keep two workers from taking the same
  row: a row another claim has just taken is locked, and once that claim commits it is no
  longer waiting (TestConcurrentClaimsNeverTakeTheSameRow). `SKIP LOCKED` makes a claim pass
  over a locked row instead of waiting for it, so claims never wait for each other or for a
  Finish or Cancel in progress (TestClaimSkipsRowsLockedByAnotherTransaction).
- For a fixed channel and priority the index is ordered by `ready_at`, so the database reads
  due rows in order and stops after `n`. Rows that are not due yet lie past the end of the
  range and are never read, however many there are.
- The claim is called once per priority, highest first, by the dispatcher (section 6). Each
  call is a complete statement. Rows returned by one call are started before the next call is
  made, so there is no combined result that a later failure could drop.
- The rows come back from `RETURNING` in no particular order; `Claim` sorts the at most `n`
  rows by `ready_at` in Go, so the statement keeps the plan above, with no sort in the
  database. The order is also in the SQL, not only in the index the planner happens to
  choose: with index scans switched off the claim still takes the oldest rows
  (TestClaimReturnsTheEarliestRowsInReadyAtOrder, TestClaimTakesTheEarliestDueRowsWithoutTheIndex).
- The `id = ANY(ARRAY(...))` form is deliberate. The design review measured the more common
  `id IN (SELECT ...)` form under a forced generic plan (the plan a cached prepared statement
  can switch to): it scanned the whole table, 293 ms on 2 million rows. The `ANY(ARRAY(...))`
  form stayed under 1 ms. The permanent test is TestClaimPlanReadsTheIndexInOrder (A3), and
  `make guards` checks that it fails on the `IN` form.

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

A result is `sent`, `failed` or "retry in d". A retry on the last allowed attempt
(`attempts >= max_attempts`) is stored as `failed`. The update decides this from the row's
own counters, in the same statement, so no caller can get it wrong
(TestARetryOnTheLastAttemptIsStoredAsFailed). The attempt row's `started_at` is `now()` minus
the duration the caller measured: Go passes the duration, not the start time
(TestFinishStoresEachResultOnTheDatabaseClock).

The text a result stores is bounded (section 8): the error and the outcome 1 KiB, the
provider's message id 256 bytes, cut at a character boundary, with invalid UTF-8 replaced and
NUL removed. Postgres refuses both in `text`, and a result write that fails every time would
get the row sent again after its lease (TestFinishStoresAnyErrorTextWithinItsBound,
TestFinishStoresTextAtItsBound). A long input is cut before it is cleaned, so its size costs
nothing.

Finish returns the row's new status, or "stale". The caller publishes a status event only when
the status really changed. If the reply to a Finish is lost and the caller writes the same
result again, the second write finds the row no longer `processing` and returns "stale"; the
status is stored, and only its event is missing (events are best-effort, section 9).

**Reap** returns `processing` rows whose lease has expired to `pending`, or to `failed` when
`attempts` has reached `max_attempts`. It takes a bounded batch with `SKIP LOCKED`, in the same
`id = ANY(ARRAY(...))` form as the claim, so every worker can run it, and reapers pass over
each other's rows (TestReapSkipsRowsLockedByAnotherTransaction). It does not change
`attempts`, and it keeps `ready_at`, so a returned row takes its old place in the queue. It
sets `last_error` to "lease expired". It writes no attempt row: only Finish writes one, so an
attempt lost with its worker shows in the history as a missing attempt number. It returns the
ids, new statuses, batch ids and correlation ids, for the status events and the log
(section 9, section 10). Claim, Cancel and Requeue return the batch id too, from the same
statement (session 7).

**Cancel** sets `cancelled` only `WHERE status IN ('scheduled', 'pending', 'retrying')`. A row
that is being sent cannot be cancelled; the API answers 409. Cancel tells "no such row" apart
from "not in a cancellable status", and returns the status it found
(TestCancelOnlyFromWaitingStatuses). It locks the row before the update, in one transaction,
so the status it reports is the one the update refused; Requeue does the same. A cancelled row is final: a late result, the reaper, a
claim and a requeue all leave it alone (TestACancelledRowCannotBeRevived).

**Requeue** (`POST /retry`) moves a `failed` row to `pending`, due now, and sets
`max_attempts = attempts + the default budget`. It does not reset `attempts`, which keeps the
attempt number unique.

**Times.** Every time stored in the queue is computed by Postgres (`now()`). Go code passes
durations ("retry in 4 s"), never timestamps. One clock, so worker clock drift cannot reorder
the queue.

**Queue writes are not cancelled by shutdown.** All five operations run on a context that
ignores the caller's cancellation (`context.WithoutCancel`, so the correlation id stays) and
has its own timeout (10 s). A claim that is interrupted half-way could otherwise leave rows
`processing` with nobody holding them. Cancel and Requeue follow the same rule: one rule for
every queue write is simpler than a list of exceptions, and both are one short statement.
(TestQueueWritesAreNotCutOffByACancelledCaller, TestQueueWritesHaveTheirOwnTimeout.)

**Known behaviours.**
- If the reply to a claim is lost after the statement committed (a dropped connection), the
  rows stay `processing` until the lease ends, and that claim still counts as an attempt. The
  same happens when Postgres stalls for longer than the queue's own timeout (10 s): the worker
  stops waiting, and the statement commits when Postgres resumes. Session 5's reviewer paused
  Postgres for 20 s: 40 rows stayed `processing` for their 60 s lease, each with one attempt
  spent, and were then reaped and delivered (session 5's review, F7). Nothing is lost; the
  delay is one lease.
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
    probe := d.breaker.Started()         // marks the probe when the breaker is half-open
    wait := d.waiting.Enter()            // "claimed, no permit yet"
    go d.handle(row, probe, wait)
}

func (d *Dispatcher) handle(row Notification, probe bool, wait *Waiting) {
    defer d.slots.Release()
    defer wait.Leave()                   // safe to call twice
    d.events.Publish(row.ID, processing) // never waits (section 9)
    result := d.provider.Send(sendCtx, messageOf(row), func(ctx context.Context) error {
        err := d.limiter.Wait(ctx, d.channel)          // the send gate, section 7
        wait.Leave()
        return err
    })
    verdict := Classify(result)          // what happens to the row, and what the breaker learns
    d.breaker.Report(probe, verdict.Lesson)
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
workers that is at most `10 × W` rows waiting at the gate at once. The design review's
simulation served the waiting rows in order and estimated the longest wait as one window plus
`10 × W / 100` seconds: about 1 s for 3 and for 6 workers and 2 s for 12, with nothing
refused, and the gate budget of 5 s reached at about 35 workers per channel. The limiter
does not serve waiters in order: every waiter refused against the same oldest permit wakes
at once, and the first script to reach Redis wins. Session 3's reviewer modelled this with
the real limiter, every worker always keeping 10 rows waiting (the most the rule allows):
longest waits of 3.16 s and 2.12 s at 12 workers, 4.3 s at 24 with 3 and 4 refusals in 20 s,
76 refusals of about 2,100 at 35 (review F3). So the formula and the "35 workers" are not
supported; the real figures are assumption A5, measured in session 5 on the real dispatcher.
That measurement (section 12, A5) found the wait shorter than both models: with 1, 3, 6 and
12 dispatchers on one channel the longest wait was 6 ms, 0.98 s, 1.03 s and 1.05 s, with no
refusal and 94 to 95 sends per second. The models kept every worker's 10 rows waiting all
the time. The real loop does not: `Available` reports no room while the window is full, so
no dispatcher claims; when the window's permits come free, every dispatcher reads the same
free count and claims up to its batch, the claims add up to more than the free permits, and
the rest wait for the next window. The longest wait is therefore about one window, whatever
the number of dispatchers up to 12. Session 8 measured past that (section 12, A5): the wait
grows with the dispatchers from there, 3.1 s at 24 and 4.2 s at 36, still with no refusal,
close to the design review's estimate of one window plus `10 × W / 100` seconds (3.4 s and
4.6 s), which the gate budget of 5 s reaches at about 40 dispatchers per channel. The same
estimate at a low limit: with `RATE_LIMIT=20`, one window plus `10 × 12 / 20` is 7 s for 12
dispatchers, and the measurement refused 50 sends for no permit in time; 2 dispatchers waited
at most 1.02 s. So the number of dispatchers a channel can have before refusals start is
about `(GATE_BUDGET − window) × RATE_LIMIT ÷ CLAIM_BATCH`: 40 at the defaults, 8 at a limit
of 20 (section 13). A slower provider changes none of this: at 200 ms the waits were those
of 20 ms.
The same wait is how long a high-priority row that has just become due can wait behind rows
claimed before it: the gate has no priority order, and I9 holds at claim time. The batch
rule is what keeps that wait short.

**A low limit.** Session 7's reviewer ran two workers with `RATE_LIMIT=20` and read 8.1 and
then 12.6 attempts per second from Prometheus (`rate(notifications_attempts_total[1m])`),
about 1.5 and 3 minutes after the stack started, while a full `make guards` ran on the same
machine (session 7's review, hard question 7). Session 8 did not reproduce it. In nine runs
of 600 to 3,220 rows on the Compose stack with two workers and `RATE_LIMIT=20`, the attempts
per second, counted from `delivery_attempts` by second, were 20 in almost every second and
18.6 to 19.1 over each run, the top speed being 20 per 1.05 s, 19.05. The runs covered the
defaults on one channel; three channels with mixed priorities; the reviewer's settings
(`GATE_BUDGET=300ms`, a provider of the reviewer's own with a keep-alive of 200 ms and of
500 ms, 80% failures on another channel, the observability profile); 16 busy processes in
Docker; and the guards of the dispatcher and of the queue running beside it (session 8's
record has the commands). What held the reviewer's run below the limit is not known. A
possible reading, not shown: a sliding window frees each permit one window after it was
taken, so the dispatchers can at best send `limit` per `window + d`, where `d` is the time
from a permit's leaving the window to the next row's reaching the gate (the pause, the
`Available` read, the claim). The measured runs fit a `d` of a few to 30 ms; the reviewer's
figures would need 0.5 to 1.3 s, which a machine starved of CPU and disk could give.

**One exit.** Every claimed row ends in `handle`, and `handle` ends in `Finish`. A claimed row
has no other path, so there is no path on which a row, a slot, a waiting mark or a breaker
probe can be forgotten.

**The verdict** is decided in one function, from what was observed:

| What was observed | Row | Breaker learns |
|---|---|---|
| Response 2xx | `sent` | success |
| Response 400, 413 or 422: the provider rejected this message | `failed` | success: the provider answered |
| Any other response (5xx, 429, 408, 401, 403, 404, a redirect, ...) | retry (`Retry-After` honoured) | failure |
| Refused at the send gate because the limiter returned an error | retry in about 1 s | failure |
| Refused at the send gate because no permit came free within the gate budget | retry in about 1 s | nothing |
| No response, because the provider closed the kept-alive connection before the request was written | retry in about 1 s | nothing |
| No response otherwise (could not connect, timeout, connection reset) | retry | failure |

- A **response always wins.** The table is read top to bottom: if a response arrived, the
  response decides, whatever else happened to the request. A refusal comes before "no
  response", because a refused send also has no response: its connection was closed by the
  gate.
- **"No response" covers three things** (session 4's review, F3 and F7), and each has its
  answer:
  1. *The provider did not answer:* it refused the connection, did not answer within the
     send timeout, or reset the connection after the request was written. The channel may be
     broken: retry, and the breaker counts a failure.
  2. *The provider closed a kept-alive connection on which nothing had been written yet.*
     The send took the connection from the pool and waited at the gate; meanwhile the
     provider's keep-alive ran out (section 7, the connection pool). Go reports this case
     with its own error, "http: server closed idle connection" (`transport.go` line 1090,
     set when the read loop sees the connection end while no request is on it, line 2660).
     The provider client reports it as `Result.ClosedIdle`, **only on a reused connection**.
     Nothing reached the provider, so nothing is learned about its health: retry soon, and
     the breaker learns nothing. A provider that is really down also fails new connections,
     and those count. Go gives the same error when a *new* connection is closed before the
     request is written: an address that accepts connections and closes them (a published
     port with nothing behind it, a load balancer with no backend, a provider at its
     connection limit). That is a broken channel, and it is case 1. Until session 5's review
     the client set `ClosedIdle` on new connections too, and such an address drained the
     whole queue into `failed` with the breaker closed (review F1;
     TestAConnectionClosedByTheProviderAtTheGateIsReportedAsClosedIdle). The attempt
     is spent, as for every refusal (rule 4); how often this happens is bounded by the
     relation of section 13 and counted in `notifications_attempts_total{outcome="closed_idle"}`
     (section 10).
  3. *The request was never made,* because it could not be built. This case is removed
     rather than classified. The URL is the same for every message, so `provider.New` refuses
     a URL that is not an absolute `http` or `https` URL, and a message has no other part
     that can make a request invalid: the body is JSON, the `Idempotency-Key` is a UUID, and
     `X-Correlation-ID` is sent only when it is within its bound (section 8). Without this,
     a broken URL would fail every message and a newline in a correlation id would be
     retried as a provider failure.
- The second and third rows separate two questions. "Is something wrong with this message?"
  ends the message. "Is something wrong with the channel?" (a provider that is down, a wrong
  URL, an expired credential) is retried and counts against the breaker, so a broken
  configuration stops the channel and does not drain the whole queue into `failed`.
- "Retry" becomes `failed` when the attempt was the last one allowed.
- Every row of the table is one attempt. A refused request costs an attempt too. That is the
  price of never giving attempts back; it is paid rarely, because the hint and the batch rule
  keep rows away from a gate that has no room, and limiter errors open the breaker.

**Retries** use exponential backoff with jitter (2 s, 4 s, 8 s, ... capped at 2 minutes;
6 attempts by default). After attempt `k` failed, the ceiling is `min(cap, base × 2^(k−1))`
and the delay is drawn between half the ceiling and the ceiling, so retries of rows that
failed together spread out and a delay never exceeds the cap. A response's `Retry-After`
is honoured when it asks for longer, up to the same cap: a provider cannot park a message
for a day. "Retry in about 1 s" is a delay drawn between 1 s and 2 s: about one window, so
the permit that was missing has come free (TestRetryDelaysDoubleWithJitterUpToTheCap,
TestRetryAfterIsHonouredUpToTheCap, TestARetriedRowWaitsForItsBackoff).

**Circuit breaker**, one per dispatcher. It has three states and three calls.

- *Closed:* `Allow()` sets no limit. Five failures in a row open it; a success resets the
  count.
- *Open* (30 s): `Allow()` returns 0, so nothing is claimed.
- *Half-open:* `Allow()` returns 1 while no probe is marked, otherwise 0.

`Allow()` only reads. `Started()` is called when a row really starts, and in the half-open
state it marks that row as the probe and says so; `handle` passes that answer to `Report()`.
`Report()` clears the mark: a success closes the breaker, a failure opens it again, and
"nothing" leaves it half-open for the next row. If a claim finds no row, nothing was marked,
so nothing can be left hanging. Because every started row reaches `Report()` (the single
exit), the probe cannot be lost, and because the mark is set on the loop's own goroutine,
there cannot be two. In the closed state every report counts. In the open and half-open
states only the probe's report counts: a row that started before the breaker opened and
reports while it is open teaches nothing that the opening did not already say
(TestTheHalfOpenBreakerLetsExactlyOneRowThrough, TestAClaimThatFindsNothingLeavesNoProbe,
TestARefusedProbeLeavesTheBreakerHalfOpen).

**The pieces of the loop** (`internal/dispatch`, session 5).

- `room` reads the four limits cheapest first and stops at the first 0, so an open breaker
  or a full worker does not ask Redis. An error from `Available` (Redis unreachable, a worker
  with other settings, a Redis that refuses writes) is room 0, logged. The read uses the
  loop's context with no shorter deadline: against an unreachable Redis it takes about 3.5 s
  (session 3), against a Redis that accepts connections and does not answer (paused) about
  20 s, go-redis's read timeout and its retries (session 5's review, F4). The loop claims
  nothing meanwhile, and the rows in flight run on their own goroutines, so nothing else
  waits for it (TestNoRowIsClaimedWhileTheWindowIsFull, TestAnUnavailableRateLimitClaimsNothing).
- Slots and the waiting mark are the dispatcher's own: a channel's dispatcher has at most
  `SLOTS` sends in flight and at most one claim batch claimed and not yet permitted.
- `handle` runs on a context that keeps the loop's values and ignores its cancellation
  (`context.WithoutCancel`), so a shutdown cancels neither the send nor the result write. The
  context carries the correlation id, so every log line about the row has it.
- The dispatcher builds its own provider client from `provider.Options`; it is not given one.
  So no dispatcher can be given a provider that skips the send gate (rule 9), and no two
  dispatchers share a connection pool: section 7 keeps one idle connection per slot, and the
  first worker shared one client of 50 idle connections among three dispatchers of 50 slots
  each, which made 993 connections for 2,362 requests to a slow provider (session 5's review,
  F5). The queue and the limiter are interfaces, so that tests can wrap the real ones to
  inject stalls and failures and to stamp permits.
- Status events go through a narrow `events.Publisher` interface whose `Publish` never waits
  and never fails (section 9). `handle` publishes `processing` when it starts, and the new
  status when `Finish` changed the row. Session 5 used one that dropped them; session 7 the
  Redis one.
- The measurements (section 10) are made in `handle`, in the gate and after `Finish`, where
  the values are known; each is an increment in memory. The loop has none.

The open time is longer than the longest possible send, so no result from before the breaker
opened can arrive while it is probing. While the breaker is open, an outage uses attempts only
through probes: one row per open period per worker.

**What the breaker protects, and what it does not** (session 5's review, F2 and F6). The
breaker is each worker's, and it counts failures in a row. So a broken channel (a wrong URL, an
expired credential) costs, per worker, the five failures that open the breaker plus the rows
already in flight, and then one attempt per open period. With a backlog, that is a small part
of it: TestNotFoundAndUnauthorizedOpenTheBreakerAndDoNotDrainTheQueue runs two workers through
two open periods and no row fails. With little traffic it is not: a row has 6 attempts, and
with two workers the breakers need ten failures between them, so a few rows that trickle in
can use all their attempts and end `failed` before both breakers are open (the reviewer: 6 of 6
rows over 255 s). They are not lost: `failed` is the dead-letter state, and `POST /retry`
(session 6) requeues them once the configuration is fixed. A breaker shared by all workers
would cut the cost to five failures in all; it is left out (section 16). A provider that fails
about half of its requests opens a breaker often, because five failures in a row are common
among 50 sends at once; the reviewer measured 749 of 900 sent after 400 s with 50% 503s and
10% 429s, and strict priority kept the low-priority rows waiting meanwhile. That is the
intended reading: such a provider is unhealthy.

**A lost result write.** If writing the result fails after a send, the write is retried with
backoff until the database answers: either the result is stored, or the row is no longer this
worker's (stale). The delays start at 100 ms and double up to 5 s. The retries stop after one
lease length at the latest, counted from the end of the send. If the result was never stored,
the reaper returns the row and it is sent again (TestAFailedResultWriteIsRetriedAndNotSentAgain).

**Shutdown.** On a stop signal the loop stops claiming and waits for the rows in flight. A send
in flight is not cancelled; it finishes within the send timeout. If the shutdown timeout
passes first, the worker exits and the rows it still held come back through the reaper
(TestShutdownWaitsForSendsInFlightAndDoesNotCancelThem).

**The reaper** runs once per worker, every `REAPER_TICK`. It calls Reap (section 5) in
batches of 100 until a batch comes back short, and publishes a status event for each row it
returned. Every worker runs one; `SKIP LOCKED` keeps them off each other's rows
(TestAKilledWorkersRowComesBackThroughTheReaper).

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
        r.Refusal = err
        info.Conn.Close()                    // the write that follows fails: nothing is sent
    }
}
```

Five properties make "one send, one connection, one permit, at most one write" true:

1. **The hook runs before the write.** `GotConn` is called on the caller's goroutine before the
   transport hands the request to its write loop. (Go 1.27.4, `net/http/transport.go`:
   `getConn` calls `trace.GotConn` at line 1632, after the connection is ready; `roundTrip`
   calls `getConn` at line 714 and only then `pconn.roundTrip`, which hands the request to
   the write loop at line 3063.)
2. **A refused request is stopped by closing its connection.** Cancelling instead is not
   enough: the write loop does not look at the context
   ([experiment 01](experiments/01-gate-refusal.md)). The first draft closed the connection
   and then cancelled. Session 4 measured the cancel as adding nothing: through the real
   client, with the close alone, 0 of 20,000 refused sends reached the server (two runs of
   the A1 test of section 12, 10,000 each); with the cancel and no close, 5, 5 and 8 of
   10,000 did, and with neither, every one of them (session 4's review, F5). The cancel was
   removed (CLAUDE.md, rule 11): a closed connection makes the transport's write fail, and the
   send returns that error.
3. **The transport never replays a request.** Go replays a request on a new connection when a
   reused connection turns out to be dead, and it treats a request with an `Idempotency-Key`
   header as safe to replay. A replay would be a second write inside one attempt. The client
   sets `req.GetBody = nil`, which turns replays off; a dead connection then becomes an
   ordinary failed attempt, and the retry is the queue's decision. (Measured in the design
   review: with the default, the server received the request twice while the client saw one
   success. In Go 1.27.4 a request with a body is replayable only when `GetBody` is set,
   `net/http/request.go` line 1562, and the "nothing was written" retry needs it too,
   `transport.go` line 871.)
4. **HTTP/1.1 only**, set through `Transport.Protocols`, so one connection carries one
   request and closing it cannot hurt another. A test checks the protocol against a TLS server
   that offers HTTP/2. (Go would also leave HTTP/2 off for a transport with its own dialer,
   `transport.go` line 489; the explicit setting does not depend on that default.)
5. **Redirects are not followed.** Go's client follows a 301, 302 or 303 by sending a `GET`
   without the body on a second connection, and the final 200 would look like a delivery
   (measured in the design review: two permits, and `sent` for a message the provider never
   accepted). `CheckRedirect` returns `http.ErrUseLastResponse`, so a redirect is just "any
   other response".

**One deadline per send.** A send has a single context deadline (15 s) that covers connecting,
waiting at the gate, writing and reading the response. The gate waits on a child of that
context limited to the gate budget (5 s). `http.Client.Timeout` is not used, because the hook
cannot see it. Dial and TLS handshake have their own shorter timeouts (3 s each).
Each part in turn outlasting the send timeout ends the send at the deadline: the connection,
the gate, the write, the response's headers and its body
(TestOneDeadlineCoversConnectGateWriteAndResponse). One response would escape it: to a
`101 Switching Protocols`, Go's transport hands the raw connection to the caller as the body
(`transport.go` line 2732), and the request's context no longer closes it, so reading that
body waited as long as the provider kept the connection open (session 4's review, F2). The
client reads a body only from a final response (status 200 or more); a 101 is closed unread
and is "any other response". The TLS handshake timeout ends a slow handshake first (TestTheTLSHandshakeHasItsOwnTimeout),
and the dial timeout a connection attempt that is never answered (TestTheDialHasItsOwnTimeout:
300 ms, not the send's 5 s). Such a dial needs an address that drops packets; the test makes
one on the machine itself, a listener whose accept queue of one is full and never accepted
from, which Linux and the BSDs answer by dropping the SYN. Until session 8 the dial timeout
had no test.

**The provider client in detail** (`internal/provider`, session 4).

- `Send(ctx, message, gate) Result`. The request is the assignment's: `POST` to
  `PROVIDER_URL` with the JSON body `{"to", "channel", "content"}`, `Content-Type:
  application/json`, the notification id as `Idempotency-Key`, and `X-Correlation-ID` when the
  notification has one (TestTheRequestHasTheAssignmentsFormat).
- `Send` puts the send timeout on the caller's context; that is the one deadline. The gate's
  context is a child of it, limited to the gate budget, so a gate still waiting when the send's
  deadline passes stops and takes no permit afterwards
  (TestAGateThatWaitsPastTheDeadlineTakesNoPermit).
- The hook's state needs no lock: it runs on the goroutine that called `Do` (property 1), and
  `Send` reads it after `Do` returns.
- **`Result` carries what was observed, and nothing else:** whether a response arrived, its
  status, its `Retry-After` (seconds or an HTTP date, read on arrival; absent or malformed is
  0; above one day is one day, so the duration cannot overflow), the `messageId` of a 2xx JSON
  body, and the start of the body; the gate's error when it refused (`ErrNoPermitInTime` or a
  limiter error, which `Classify` tells apart); whether the connection was reused; whether
  the provider closed the kept-alive connection before the request was written
  (`ClosedIdle`, Go's "server closed idle connection", added in session 5); the error when no
  response arrived; the duration. It holds no verdict.
- `New` refuses a URL that is not an absolute `http` or `https` URL with a host, and `Send`
  sets `X-Correlation-ID` only when the id is within its bound (section 8), so no message can
  make a request that cannot be built (section 6, "no response" case 3; session 5).
- The provider URL can hold a secret (a webhook.site path, credentials). It is never logged:
  `New`'s error does not quote it, and the worker logs only its scheme and host (session 5's
  review, a nit; TestTheWorkerLogsOnlyTheProvidersHost). The error of a send does not quote
  it either: Go's `*url.Error` does, and that text was stored as `last_error`, logged, and,
  from session 6, shown by the API. `Send` keeps the operation and the cause and drops the
  URL (session 6's review, F2; TestASendErrorDoesNotQuoteTheURL). What it means for the row is
  decided by `Classify` (section 6), in session 5 (TestTheResultCarriesWhatWasObserved).
- No proxy: a send goes straight to the provider, as it does in every measurement here.
- **Bounds.** The client reads at most 64 KiB of response headers (Go's default is 10 MiB,
  `transport.go` line 340) and keeps at most 4 KiB of a response body. When the client
  closes a longer body, the transport itself discards up to 256 KiB more of it, for at most
  50 ms, in its own goroutine, and keeps the connection only if that reaches the end
  (`transport.go` lines 2420 to 2448); otherwise the connection is closed. So the client's
  memory is bounded by 4 KiB and its time by the send's deadline; the provider can make the
  transport read at most 256 KiB more (session 4's review, F6)
  (TestResponseHeadersOverTheBoundAreNotAResponse, TestTheResponseBodyIsReadUpToItsBound).

**The connection pool** keeps as many idle connections as there are slots; with Go's default
of 2 the review measured 686 new connections in 10 s and 6% less throughput. Idle connections
are dropped after 10 s (TestTheClientKeepsAnIdleConnectionForEverySlot,
TestIdleConnectionsAreDroppedAfterTheIdleTimeout). A connection that the provider closes while a request waits on it at
the gate fails that attempt (nothing is replayed, property 3), and the permit is spent. The
time at the gate is idle time for the provider too: a connection idle for up to 10 s, then
held up to the gate budget (5 s), reaches the provider's keep-alive timeout if that is under
15 s (Node's default is 5 s). Session 4's reviewer reproduced it: a provider with a 2 s
keep-alive, a connection idle 1.5 s, a gate wait of 1 s, no response (review F3). This is the
client's own doing, not the provider's failure; the third relation below states it, session 5
decides how `Classify` counts it, and `notifications_attempts_total{outcome="closed_idle"}`
counts it (section 10).

The limiter **fails closed**: when a permit cannot be taken, nothing is sent. When Redis is
unreachable `Available()` fails too, so nothing is even claimed. When Redis refuses writes (a
read-only replica after a failover, Redis out of memory), `Available()` fails as well, so again
nothing is claimed and no attempt is spent. Until session 5's review `Available` was declared
read-only so that it kept working in that case, and the breaker was meant to see the refusals
at the gate; but each refusal spent an attempt, and the reviewer saw rows end `failed` with no
send (review F2). The half-failure was removed instead of handled.

**The limiter in detail** (`internal/ratelimit`, session 3). One key per channel,
`ratelimit:<channel>`: a sorted set whose members are permits and whose scores are the Redis
times at which they were granted, in microseconds. Next to it, `ratelimit:<channel>:settings`
holds the limit and the window the set was filled with. The scripts use `#!lua` shebangs,
so the limiter needs Redis 7.0 or later; on an older Redis every call fails, which fails
closed. Compose and the tests run `redis:7-alpine`.

- **`Wait(ctx, channel)`** runs the take script. The script reads the clock with `TIME`,
  removes the permits that are one window old or older, and grants one permit if fewer than
  the limit remain: it adds a member (a random token from the caller, so two permits in one
  microsecond are two members) and sets the key to expire one window later. If it refuses, it
  returns the time until the oldest permit leaves the window, and `Wait` sleeps that long, or
  until its context ends, and asks again. A permit granted at Redis time `t` counts for every
  `now < t + window`, so any `limit + 1` permits span at least one window, and no half-open
  window `[a, a + 1 s + margin)` holds more than the limit (TestNoWindowHoldsMoreThanTheLimit,
  TestWaitReturnsWhenTheOldestPermitLeavesTheWindow, TestAPermitCountsForOneSecondPlusTheMargin).
- **Go passes no time to Redis.** The script is given the limit, the window as a duration and
  the token; the clock is Redis's (I12). Workers' clocks do not matter
  (TestTheLimiterSendsRedisNoClockTime).
- **Microseconds, not milliseconds.** The first version stamped permits in milliseconds; the
  truncation let two permits that were one window apart in milliseconds be up to 1 ms less
  than a window apart in real time. Lua's `..` formats a number with 14 significant digits,
  so the scripts build every score and score bound with `string.format('%d', ...)`. This is a
  convention without a guard: with `..` in `Available`'s bound, the count is off by about
  50 µs of the window and every test passes (session 3's review, F5); the take script's
  bounds are covered by the A6 test, which fails when the bound moves 2 ms.
- **`Available(ctx, channel)`** returns the number of free permits now. It is a separate
  script: it reads `TIME` and counts the permits younger than the window, and it has no write
  command, so it changes nothing (TestAvailableChangesNothing). It is declared `#!lua` with no
  flags, like the take script, so Redis treats it as a script that may write: on a read-only
  replica it is refused (`READONLY`) and when Redis is out of memory too (`OOM`, no
  `allow-oom`). So `Available` fails exactly when the take script would, and nothing is
  claimed (TestAvailableFailsWhenTheTakeScriptWould). Sessions 3 and 4 declared it
  `flags=no-writes` and ran it with `EVAL_RO`, so that it kept working when only writes failed;
  session 5's review showed that this half-failure only spent attempts (section 7, "fails
  closed").
- **Errors are never permits, and the two kinds of refusal are told apart.** The verdict
  table of section 6 treats "the limiter returned an error" as a failure of the channel and
  "no permit came free within the gate budget" as nothing. When the context ends before a
  script call or while `Wait` sleeps, `Wait` returns `ErrNoPermitInTime` and has taken nothing
  (TestWaitStopsWhenItsContextEnds, TestWaitWithAnEndedContextTakesNoPermit). Every other
  error is the limiter's: a Redis error, a script error, a malformed reply, and a context that
  ends during a script call, because a healthy Redis answers in well under a millisecond and a
  call that outlives the budget means Redis is stalled (TestAnUnreachableRedisIsAnErrorNotAPermit,
  TestAHungRedisIsALimiterErrorNotAWaitThatRanOut). Until session 3's review both kinds wrapped
  `context.DeadlineExceeded` and could not be told apart (review F1).
- **A permit lost to a cut-off call.** When the context ends during the script call, Redis may
  have granted a permit whose reply never arrived; it counts against the limit and nothing is
  sent on it. go-redis may retry a command whose connection failed after the write; the retry
  carries the same token, so a second grant only moves that member's score later. Either way
  this costs throughput, never a request; nothing relies on it for safety.
- **One setting per window.** The limit and the margin are each worker's configuration, but
  the window is shared. A worker whose limit or margin differs from the settings stored with
  the window gets an error from both scripts, so it claims nothing and its breaker opens;
  without this check, workers with limits 100 and 150 put 150 permits in one window, and a
  worker with no margin removed the margin's protection for all (session 3's review, F2;
  TestWorkersWithDifferentSettingsAreRefused). The stored settings expire with the window, so
  a change of `RATE_LIMIT` or `WINDOW_MARGIN` takes effect one window after the last worker
  with the old settings granted its last permit. A rolling update that changes them therefore
  stops sends on the new workers until the old ones are gone. `New` refuses a limit below 1 and
  a margin that is not positive, the same rule as the configuration
  (TestNewRefusesALimitBelowOneAndAMarginThatIsNotPositive).
- **The bound.** The set of a channel holds at most `limit` members, because a refused call
  adds nothing and a worker with another limit is refused; both keys expire one window after
  the last permit (TestTheWindowHoldsAtMostTheLimitAndExpires).
- **No permit is given back** (CLAUDE.md, rule 4). The first version's `Refund`, and its
  batch `Take`, are not carried over.

There is no other place where a permit is taken, held, checked for age or given back.

**Limits of the guarantee.** In each case the provider's 429 is the backstop: it is retried
and it counts towards the breaker.

| Case | Effect |
|---|---|
| Redis loses its data (restart without persistence, failover to an empty replica) | The counter restarts empty; for one window the provider can receive up to twice the limit |
| A worker process is frozen for longer than the margin between taking a permit and writing (CPU starvation, a paused container), or the network delays a request by more than the margin | Requests permitted in one window can arrive in the next. The excess is at most the number of delayed requests. |
| The Redis server's clock jumps forward | Permits expire early, once |
| The Redis server's clock jumps backward | Permits stamped before the jump look like future permits and count until the clock catches up: the channel sends less, or nothing, for up to the length of the jump. This fails closed; `Available` reports no room, so nothing is claimed and no attempt is spent. |

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

**Requests.** Bodies and answers are JSON; times are RFC 3339 in UTC.

- **Create** (`POST /v1/notifications`): `recipient`, `channel` (`sms`, `email`, `push`),
  either `content` or `template` with `variables` (an object of strings), `priority` (`high`,
  `normal`, `low`; default `normal`), `scheduledAt`, `idempotencyKey`. 201 with the
  notification; 200 with the first one for a repeated key, answered as the status endpoint
  answers it, with its attempt history (session 6's review, F6). The key is a field of the
  body; an `Idempotency-Key` header on this endpoint is a 400, not ignored (review, F9).
- **Field names** are matched exactly, each once, in every object the API decodes: an
  unknown name, a name in another case (`Content`) and a name given twice are 400s naming it.
  Go's decoder matches names in any case and keeps the last of two; the API checks the names
  of each object before decoding it (session 6's review, F7).
- **Batch** (`POST /v1/notifications/batch`): `{"notifications": [...]}`, items as above
  without `idempotencyKey` (a batch is retried as a whole, under its `Idempotency-Key`
  header). 201 with `batchId`, `total` and the ids in the order of the items. Item `i` gets
  the id `UUIDv5(batchId, i)`, so a repeated batch answers the same ids in the same order
  without storing the order.
- **Status** (`GET /v1/notifications/{id}`): every field of the row and the attempt history,
  oldest first.
- **Batch summary** (`GET /v1/batches/{id}`): `total` and a count for each of the seven
  statuses.
- **List** (`GET /v1/notifications`): filters `status`, `channel`, `batchId`, `createdFrom`
  (inclusive), `createdTo` (exclusive); `limit` (1 to 100, default 50); `cursor`. Items are
  summaries without `content`, newest first: `ORDER BY created_at DESC, id ASC`, the order of
  the list index. The cursor is the `(created_at, id)` of the last item, opaque to the
  client; the next page is `created_at <= c AND (created_at < c OR (created_at = c AND
  id > i))`. The first term is redundant for the result and is what makes the cursor a bound
  of the index scan: without it Postgres read the index from its start and filtered, and a
  page 1.9 million rows deep took 300 ms instead of under 1 ms (session 6's review, F3;
  TestAListPageReadsTheIndexFromItsCursor). `nextCursor` is
  present only when the page is full. A cursor the API could not have given (malformed, or a
  time after the year 9999) is a 400 (TestACursorOutsideTheTimeRangeIsRefused).
  The filters other than the batch have no index of their own (session 8,
  [experiment 03](experiments/03-tables-without-retention.md)). The list reads the list index
  in its order and filters, so a filter that most rows match fills its page at once
  (`status=sent`: 0.04 ms at 16,000,000 rows), and one that few rows match reads further:
  `status=failed` with 1,000 such rows took 6 to 7 ms at 2,000,000 rows and 38 to 112 ms at
  16,000,000. A filter that no row matches reads the whole index: `status=cancelled` took 50
  to 55 ms at 2,000,000 rows and 1.6 to 3.2 s at 16,000,000. The request's 5 s deadline bounds
  it; past it the answer is a 500. No index was added for these filters: each would be
  written on every status change of every row, on the queue's path, to speed a read that the
  deadline already bounds, and a page that matches nothing reaches the deadline at about 25 to
  50 million rows (extrapolated from the two sizes, not measured), past the size at which the
  1-hour window of `GET /v1/metrics` does (section 10). Both costs come from the missing
  retention (section 16).
- **Cancel** and **retry** answer 200 with the new status, 404 for an unknown id, and 409
  with the status found when the row is not in a status that allows it. Cancel is allowed
  from `scheduled`, `pending` and `retrying`; retry from `failed` only, and it calls Requeue
  with `MAX_ATTEMPTS` as the budget (section 5).
- **Templates**: `POST /v1/templates` with `name`, `channel`, `body`. A template is never
  changed: the same name with the same channel and body answers 200, with anything else
  409. `GET /v1/templates` lists names and channels in name order, 100 per page, with a
  cursor; `GET /v1/templates/{name}` returns one with its body.

An id that is not a UUID is a 400. Every status change the API makes (create, batch, cancel,
retry) is handed to the `events.Publisher` of section 9 after it is committed; publishing
never makes an answer wait (TestTheAPIAnswersWhileRedisHangs).

- **Metrics** (`GET /v1/metrics?window=5m`) and the **WebSocket** (`GET /v1/ws`) are
  described in sections 10 and 9.

**There is no service layer.** The handlers call `domain` for validation and rendering and
`store` for SQL. Section 14 listed an `internal/service` package for use cases; every use case
here is "validate, then one statement or one transaction", so the layer would only forward
calls. It was removed in session 6.

**Idempotency** (I10).
- A notification may carry an `idempotencyKey` (1 to 128 printable ASCII characters). The key
  is unique in the database. The request hash is SHA-256 of the validated request in a
  canonical form (fields in a fixed order, defaults filled in, `scheduledAt` in UTC, the
  variables sorted by name), without the key itself. The insert is
  `INSERT ... ON CONFLICT (idempotency_key) DO NOTHING`; when it inserts nothing, the row with
  the key is read, and its hash decides: the same hash answers 200 with that row, another hash
  is a 409. Two identical requests at the same moment both end with the same row: the second
  insert waits on the unique index until the first commits, then does nothing.
- A batch may carry an `Idempotency-Key` header (the same format). The batch row is inserted
  the same way, first in the batch's transaction, with the hash of the whole validated batch.
  A repeat with the same hash answers 200 with the same batch and the same ids; another hash is
  a 409. Batch keys and notification keys live in different columns, so they cannot collide.
- Keys never expire (section 16).

**Scheduling.** `scheduledAt` in the future creates the row as `scheduled` with that
`ready_at`. A `scheduledAt` that is absent or not in the future creates it `pending`, due now.
Postgres decides which, with `now()`, in the insert (rule 7). `scheduledAt` is cut to
microseconds, as Postgres stores it, before it is hashed, and must lie before the year 10000
in UTC: Go cannot write a later time as RFC 3339, and such a row answered every read with an
empty 200 (session 6's review, F4; TestAScheduledAtFromTheYear10000IsRefused). An answer is
encoded before its status is written, so one that cannot be encoded is a logged 500, never a
2xx without a body (TestAnAnswerThatCannotBeEncodedIsA500).

**Templates.** A template body contains `{{.name}}` placeholders. Rendering is substitution
only; no template language is executed. Any other `{{` in a body is refused when the
template is created, so a stored body always compiles. A variable name is a letter or `_`
followed by letters, digits or `_`, up to 64 characters; a body has at most 50 distinct
variables, and its length is bounded like content of its channel. In a request, each
template is read and compiled once, however many items use it. The rendered length of an
item is calculated from the compiled parts and the variables (literal length plus each
value's length times its uses) before anything is rendered, both in characters (the channel
bound) and in bytes (the request's total bound). A variable the template does not use, or a
placeholder without a variable, is an error naming it. The template's channel must be the
notification's channel.

**Validation** happens before anything is stored: required fields, the recipient format per
channel (E.164 phone number, an email address of at most 254 bytes, a device token of 16 to
256 characters of letters, digits, `_`, `-`, `:` and `.`), content length per channel, and
known enum values. Content and variable values must not contain NUL, which Postgres refuses
in `text`. Errors name the field (`notifications[3].recipient`).

**Database time.** Every `/v1` request runs with a deadline of 5 s for its database work,
set in one place, on top of the statement timeout of each statement (also 5 s). A statement
timeout is enforced by Postgres itself, so it did not help when Postgres stalled: the
reviewer paused it for 40 s and the client got no answer at all after 40 s, though the row
was written (session 6's review, F5). With the deadline the client gets a 500 within the
deadline (TestARequestEndsAtItsDeadlineWhenPostgresStalls). Whether a write that was cut off
committed is then unknown to the API as well as to the client; a client that retries with an
idempotency key gets the stored row or a new one, never two. Cancel and retry are queue
writes with their own 10 s timeout (section 5, rule 8), so they can take up to 10 s.
The deadline starts before a create or a batch waits for its rendering slot (below), and the
wait ends with it: a request that has not got a slot within its deadline is a 500 too
(TestARequestWaitingForASlotEndsAtItsDeadline). Until session 8's review the slot was waited
for on the request's own context, before the deadline was made: with Postgres paused, 40
concurrent creates were answered four at a time, at 5, 10, ... 30 s, and the last 16 got no
answer at all within the server's 30 s although their rows were written (review F1). It was
the case of session 6's review F5 again, brought back by the fix of its F1.

**Errors** of the handlers have one form, written in one place:
`{"error": {"code", "message", "fields": [{"field", "message"}], "moreFields"}}`. A message
is a fixed text and never quotes a value from the request; a name that comes from the request
(an unknown field, a variable name) passes through one function, `domain.Echo`, which keeps
its first 64 bytes. With at most 100 fields the size of an error body does not depend on the
request: 1000 items with every field wrong answer 8,690 bytes, and 1000 items each with a
64-byte variable name of control characters, which JSON escapes to six bytes each, the
largest the form allows, 47,794 bytes (TestErrorBodiesStayWithinTheirBoundForAnyRequest,
`go test -count=1 -v -run TestErrorBodiesStayWithinTheirBoundForAnyRequest ./internal/api`).
The first version echoed the value of a too-long field into its message: a 99 KB request
produced a 68 MB error. A path or method the API does not have is answered by Go's router
with a plain-text 404 or 405.

**Decoding is bounded too.** The body is read through a limit (`http.MaxBytesReader`). The
batch array is decoded one item at a time and the 1,001st item ends the request; the
variables of an item are counted while they are decoded and the 51st ends it. So 8 MiB of
`{}` items, or 1 MiB of variables, cost the server a bounded amount, not millions of decoded
items.

**Rendering is bounded across requests.** A template makes a small request render far more
than it sent: session 6's reviewer made 5 KB requests render 16 MiB each, and 40 of them at
once took the API from 59 MiB to 1.1 GiB. Two rules bound it. The rendered content of one
request is at most 8 MiB, the batch's body bound, so it is never more than a client could
have sent as plain content. And rendering and storing run in one of 4 slots per API instance
(`api.MaxRenderingRequests`): a request that has been decoded and validated waits for a slot,
with its deadline, before it renders anything. What waits is what the client sent; what is
rendered at once is at most 4 × 8 MiB (TestConcurrentRequestsRenderInBoundedMemory). A
template still lets a small request store up to 8 MiB; the total a client may store over
time is a quota, left out (section 16).

**Bounds.** Every input and every output has a stated limit, and each limit has a test.

| What | Limit |
|---|---|
| Request headers | 64 KiB, request line included, arriving within 5 s (TestRequestHeadersOverTheBoundAreRefused, TestSlowRequestHeadersAreCutOff) |
| Request body | 1 MiB (8 MiB for a batch); one byte over is a 413. The whole request, body included, must arrive within 30 s, and the answer must be written within 30 s of the request's headers, so a client that sends or reads slowly cannot hold a connection and a handler (TestSlowRequestBodiesAreCutOff, TestAnswersToAClientThatDoesNotReadAreCutOff, TestTheServerHasTheTimeoutsOfTheDesign) |
| Batch size | 1000 notifications; the 1,001st ends the decoding |
| Content after rendering | Per channel: SMS 1600 characters, push 4096, email 100,000 |
| Template body | The content bound of its channel; at most 50 distinct variables |
| Template variables per notification | 50; names up to 64 characters |
| Total rendered content per request | 8 MiB, the batch's body bound, calculated before rendering; at most 4 requests per API instance render at once (TestTheRenderedTotalOfARequestIsBounded, TestConcurrentRequestsRenderInBoundedMemory) |
| `scheduledAt` | Before the year 10000 in UTC; cut to microseconds (TestAScheduledAtFromTheYear10000IsRefused) |
| Template name, idempotency key | 64 and 128 characters; a template name starts with a letter or digit |
| API key | At least 16 characters; the API refuses to start with a shorter one (TestAPIKeysMustBeAtLeast16Characters) |
| Error response | 100 fields, plus a count of the rest; a name from the request 64 bytes; the whole body at most 64 KiB (TestErrorBodiesStayWithinTheirBoundForAnyRequest, TestFieldErrorsAreKeptUpToTheirBound) |
| List page | 100 items; items are summaries without `content` |
| Template list page | 100 names, without bodies |
| Attempt history in a status answer | The last 100 attempts, with the total count (`/retry` can add attempts without end) |
| Metrics window | `1m`, `5m`, `15m` or `1h`; any other value is a 400; computed at most once per 5 s per window (TestTheMetricsEndpointReportsTheRecentWindow, TestTheMetricsEndpointIsComputedOncePerFiveSeconds) |
| Database time per API request | A deadline of 5 s on the request, and a statement timeout of 5 s; cancel and retry 10 s (TestARequestEndsAtItsDeadlineWhenPostgresStalls, TestAPIDatabaseSessionsHaveAFiveSecondStatementTimeout) |
| Rate-limit window in Redis (section 7) | At most `limit` permits per channel, also with workers of different settings; both keys expire one window after the last permit (TestTheWindowHoldsAtMostTheLimitAndExpires, TestWorkersWithDifferentSettingsAreRefused) |
| Provider response read by the worker (section 7) | Headers 64 KiB; body 4 KiB, the rest not read (TestResponseHeadersOverTheBoundAreNotAResponse, TestTheResponseBodyIsReadUpToItsBound) |
| Text stored from a delivery result (section 5) | Error and outcome 1 KiB, provider message id 256 bytes, valid UTF-8 (TestFinishStoresTextAtItsBound, TestFinishStoresAnyErrorTextWithinItsBound) |
| WebSocket | 1000 connections per API instance; 256 messages waiting per client, then it is disconnected; 5 s to write a message; no data message from the client; a ping every 30 s, answered within 10 s; up to 100 notification ids and 10 batch ids, at least one. The table and tests are in section 9 |
| Status event read from Redis | 1 KiB; a larger or malformed event is dropped (TestAMalformedOrOversizedEventIsDropped) |
| Status events waiting to be published | 4,096 per process; more are dropped and counted (TestPublishNeverBlocksWhenRedisHangs) |
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

**Authentication** is optional and off by default for local use: when `API_KEYS` is set (keys
separated by commas), every `/v1` request needs one of them, in `Authorization: Bearer` or
`X-API-Key`; otherwise 401 (TestAPIKeysAreRequiredWhenConfigured). The scheme `Bearer` is
matched in any case, as RFC 9110 says. The keys are compared in
constant time, each against every configured key. `/healthz`, `/readyz`, `/docs` and `/openapi.yaml` stay open.
So does `/metrics`, which Prometheus scrapes: its labels hold no data from requests (section
10). `/v1/metrics` and `/v1/ws` need a key like every `/v1` path.

**OpenAPI.** `GET /openapi.yaml` serves the specification, embedded in the binary; `GET /docs`
a viewer. Every path, method and status code in it is produced by the real server in a test,
and every route of the server is in it (TestOpenAPIMatchesTheServer).

## 9. Status events and WebSocket

Whoever changes a status publishes an event to a Redis channel after the change is committed.
Every API instance subscribes and forwards events to its WebSocket clients that asked for that
notification or batch.

Events are best-effort. An event can be lost (a client was disconnected, Redis restarted), and
a lost event never affects delivery. The database is the truth; a client can always ask for
the current status.

**The event** is `{"id", "batchId", "status"}`, JSON, on the Redis channel
`notifications:status`. `batchId` is absent for a notification created alone. Every change
publishes one: the API's create, batch, cancel and retry; the worker's claim (`processing`,
when `handle` starts), its result (`Finish` returned "changed") and the reaper's returns. So
the statements that change a status return the row's batch id with its new status (Claim,
Finish's caller has it from the claim, Reap, Cancel, Requeue; section 5).

**Publishing never waits** (session 7). `Publish` puts the event in a buffer of 4,096 events
and returns; it never blocks and never fails (`events.Buffer`). One goroutine per process takes
events from the buffer and sends up to 256 at once to Redis in one pipeline, with a timeout of
1 s (`events.PublishTimeout`). When the buffer is full the event is dropped; when Redis does not
answer, the events of that pipeline are dropped. Both are counted
(`notifications_events_dropped_total{reason}`) and logged at most once per 10 s. So a Redis
that is down, slow or paused costs the sender nothing: no send, no result write, no API
answer waits for it (TestPublishNeverBlocksWhenRedisHangs,
TestLostEventsDoNotAffectDelivery, TestTheAPIAnswersWhileRedisHangs). The worker publishes
through its own Redis client, so a publish never holds a connection the rate limiter needs.
On shutdown the worker gives the publisher 2 s to send what is buffered, after the sends in
flight have finished. Once `Close` is called, the sender sends only through `Close`'s context:
what is still buffered when that context ends is dropped and counted, never sent later
(TestThePublisherSendsWhatIsBufferedWhenClosed, TestCloseSendsNothingAfterItsContextEnds).
Until session 8 the sender chose at random between the buffer and the stop signal, so events
could go out after `Close` on the sender's own context, and the test of the drain passed in one
CI run with the drain removed: the sender had sent everything before `Close` was called.

**Order.** One process publishes in the order the events were given, and Redis delivers one
connection's messages in order, so the events that one process publishes about one
notification arrive in order. Events from two processes have no order: the API's `pending`
can arrive after the worker's `processing`, when the API's publisher is behind. A client that
needs the truth asks `GET /v1/notifications/{id}`.

**Subscribing.** Each API instance runs one subscription and hands each event to its hub. The
subscription waits at most 10 s for a message; after a silent 10 s it sends a `PING` on the
subscription, and if 10 s more pass with nothing, not even the answer, it closes the
connection and subscribes again. Any error does the same, after a pause that starts at
100 ms and doubles up to 5 s. So a Redis that restarts, drops the connection or is paused is
subscribed to again without anyone's help; the events published meanwhile are lost
(TestTheSubscriptionComesBackAfterRedisDropsIt, TestTheSubscriptionComesBackAfterAnOutage).
An event larger than 1 KiB, or one that is not valid JSON with a UUID and a known status, is
dropped before it is decoded and counted (TestAMalformedOrOversizedEventIsDropped,
TestAnEventIsReadUpToItsBound). The Redis client has already read it; Redis is the service's
own, so the bound is on what the API decodes and hands on.

**The WebSocket endpoint** is `GET /v1/ws?notificationId=...&batchId=...`, behind the API key
like every `/v1` path (TestWebSocketNeedsAnAPIKeyWhenConfigured). A client names what it
wants to follow: up to 100 notification ids and up to 10 batch ids, each parameter repeated,
at least one id; no id, too many, a value that is not a UUID, another parameter, or a request
that is not a WebSocket upgrade the library will take (HTTP/1.1 or later, `Connection:
upgrade`, `Upgrade: websocket`, version 13, exactly one key of 16 bytes) is a 400 in the error
form, before the connection bound and the database (TestWebSocketFiltersAreBounded). Until
session 7's review an HTTP/1.0 upgrade or one with two keys passed these checks, read the
database, and was then refused by the library in plain text (review F6). A browser page of another origin (an `Origin` header whose
host is not the request's) is a 403, so a page cannot use a browser's network position to
read statuses. There is no "every event" stream: each client receives only
what it named.

When the connection opens, the server sends the current state first and then every change:

1. It adds the client to the hub, so no change from then on is missed.
2. It reads the current status of each named notification (one query) and the summary of each
   named batch, and sends them: `{"type": "status", "id", "batchId", "status"}` for a
   notification and `{"type": "batch", "batchId", "total", "counts"}` for a batch. An id that
   does not exist is not answered.
3. Every event of a named notification, or of a notification of a named batch, follows as
   `{"type": "status", ...}`.

Without step 2 a client could not follow a notification it has just created: the change to
`sent` can happen before its connection opens, and a batch's id is known only after it was
created. A change that happens between steps 1 and 2 can arrive twice, and an event that a
publisher sent late can arrive after a state that already includes it (the order rule above).
`sent` and `cancelled` are final, so a client that waits for one of them is not misled
(TestAWebSocketClientGetsTheCurrentStateThenEveryChange).

**The hub** keeps, for each id, the clients that named it. Handing an event to the hub is a map
lookup and, for each client, a send on that client's queue that never waits. The client's
own goroutine writes its queue to the connection. Nothing that one client does can make the
hub, the subscription or another client wait.

**Bounds of a WebSocket connection** (each with its test):

| What | Bound | When it is crossed | Test |
|---|---|---|---|
| Connections per API instance | 1000 (`api.MaxWebSockets`) | The next upgrade is a 503 in the error form, before the upgrade | TestWebSocketConnectionsAreBoundedPerInstance |
| Messages waiting for one client | 256 (`api.WebSocketQueue`) | The client is disconnected (close code 1008, "too slow") and removed from the hub; the API's memory does not grow with what a client does not read. 256 is a burst allowance: a client that follows 10 batches at the full rate receives about 600 events per second, so it may fall about 0.4 s behind; the reviewer saw one such reader of 10 cut off during a burst with 990 other clients connected, and none in a second run (review F7). It reconnects and is sent the current state | TestAClientQueueHoldsItsBoundAndNoMore (256 kept, the 257th lets the client go), TestASlowWebSocketClientIsDisconnectedAndMemoryStaysBounded |
| Time to write one message | 5 s | The connection is closed: a client that stops reading holds no goroutine or connection slot past it | TestAWriteToAClientThatStopsReadingEndsAtItsDeadline |
| Messages from the client | none: the client sends nothing but control frames | The first data message closes the connection at once, its payload unread and without the close handshake: the library's handshake first reads the rest of the message, and a frame that announced 1 GiB and sent 100 bytes held the connection and its handler for as long as the client kept it open (found while building this test). A malformed frame (reserved bits, a control frame over 125 bytes) closes it with 1002, and an unmasked frame at once (RFC 6455, 5.1, requires the close, not a code). A control frame is at most 125 bytes | TestAWebSocketClientMustNotSendDataMessages |
| Messages to the client | one event or one batch summary, under 1 KiB each | | the form above |
| A peer that is gone | a ping every 30 s; no answer within 10 s closes the connection | | TestAWebSocketClientThatDoesNotAnswerPingsIsClosed |
| A quiet connection | no limit: a client may wait hours for a scheduled notification, as long as it answers pings. The server's request timeouts (section 8) end at the upgrade | | TestAWebSocketOutlivesTheServersRequestTimeouts |

On shutdown the API closes every WebSocket with code 1001 ("going away"); a client reconnects
to another instance and receives the current state again. A connection's end is counted by
reason; a client that closes its TCP connection without a close frame is `client`, also when
a ping of the server was out at that moment (review F8; TestAClientThatHangsUpIsCountedAsTheClients).

## 10. Observability

### Logs

Structured JSON (`log/slog`). The API accepts or creates an `X-Correlation-ID`, stores it on
the notification row, and the worker logs it on every line about that notification: the
attempt and its result, a result write that failed, the reaper's return. The provider
receives the same id in `X-Correlation-ID` (section 7). So one id follows a message from the
HTTP request to the provider call
(TestOneCorrelationIDFollowsAMessageFromTheRequestToTheProvider,
TestEveryWorkerLineAboutANotificationCarriesItsCorrelationID).

At the level that ships (`LOG_LEVEL=info`) the API writes one line per request and the worker
one per attempt, whatever its outcome, each with the correlation id; at the full rate of three
channels that is about 300 worker lines per second. Until session 7's review both were debug
lines, so a notification that was delivered left no line at all at info, and only failures
could be followed by their id (review F4). The test runs both processes at info.

**A failing dependency is counted, not logged per try.** Session 5's reviewer made Postgres
read-only: the worker wrote a warning for every retry of every result write, 3,200 lines in
75 s. The same shape existed for a Redis that refuses the rate-limit read: one line per loop,
every 20 ms per channel. Such failures now increment
`notifications_worker_errors_total{operation}` (`claim`, `rate_limit`, `result_write`, `reap`)
and write at most one line per 10 s per operation and dispatcher, with the number of failures
since the last line and the last error (`observability.Every`). The result that was not stored
is still counted in `notifications_finished_total{status="not_stored"}`, and the row's own line
says so at debug level, with its correlation id
(TestResultWriteFailuresAreCountedAndLoggedSparsely).

### Metrics

Prometheus, at `GET /metrics` on the API (port 8080) and on each worker (`METRICS_ADDR`,
default `:9100`). Every metric is named `notifications_...`. Labels take values from a fixed
set only: a channel, a status, an outcome of the verdict table, an operation, an HTTP method,
a route pattern of the API (`unmatched` for any other path) and a status code. A notification
id, a recipient, a batch id or any text from a request is never a label
(TestMetricLabelsHaveBoundedValues). Every series of a fixed set exists from the start with the
value 0, so `rate()` and `increase()` see the first event, and a rule can name a label value
before it has happened.

| Figure (as in the requirements) | Metric | Where it is measured |
|---|---|---|
| Requests | `notifications_http_requests_total{method, route, code}`, `notifications_http_request_duration_seconds{method, route}` | API, around the router |
| Queue depth per channel and status | `notifications_queue_depth{channel, status}`, for `scheduled`, `pending`, `retrying`, `processing` | API, from Postgres (below) |
| Age of the oldest due row | `notifications_queue_oldest_due_age_seconds{channel}`; 0 when nothing is due | API, from Postgres |
| Attempts by verdict | `notifications_attempts_total{channel, outcome}`: `sent`, `rejected`, `error_response`, `limiter_error`, `no_permit_in_time`, `closed_idle`, `no_response` | Worker, after `Classify` |
| Refusals at the gate | the same counter, `outcome` `no_permit_in_time` or `limiter_error` | |
| Connections the provider closed at the gate (sections 7, 13) | the same counter, `outcome="closed_idle"` | |
| What became of each attempt | `notifications_finished_total{channel, status}`: `sent`, `failed`, `retrying`, `stale`, `not_stored`. Every attempt is counted once here and once in the attempts | Worker, after `Finish` |
| Provider latency | `notifications_provider_latency_seconds{channel}`: from the permit to the end of the send, for every send that got a permit | Worker, around the gate |
| Wait at the send gate | `notifications_gate_wait_seconds{channel}`: the time in `limiter.Wait`, for every send that reached the gate | Worker, in the gate |
| Breaker state | `notifications_breaker_state{channel}`: 0 closed, 1 open, 2 half-open, read when scraped | Worker |
| Sends in flight | `notifications_sends_in_flight{channel}`, read when scraped (section 13, slots ÷ latency) | Worker |
| Reaped rows | `notifications_reaped_total{status}`: `pending`, `failed` | Worker, the reaper |
| A failing dependency | `notifications_worker_errors_total{operation}` | Worker |
| Status events | `notifications_events_published_total`, `notifications_events_dropped_total{reason}` (`buffer_full`, `redis_error`); on the API `notifications_events_received_total`, `notifications_events_malformed_total`, `notifications_events_subscriptions_total` (each subscription made, the first included) | Both |
| WebSocket | `notifications_websocket_connections`, `notifications_websocket_refused_total`, `notifications_websocket_closed_total{reason}` (`client`, `slow`, `no_pong`, `data_message`, `write_error`, `shutdown`) | API |

The breaker and sends in flight are read when Prometheus scrapes, because the breaker turns
half-open by the clock, without a call.

The measurements in the worker add no step that can wait or fail to the delivery path: each
is an increment or an observation in memory, made where the value is already known (after
`Classify`, in the gate, after `Finish`). The dispatcher's loop is unchanged.

**The figures from Postgres** (queue depth and the oldest due row) are read by the API, once
per 5 s at most, whatever the number of scrapes and requests: one read at a time, which every
caller in that time shares, and whose result is kept for 5 s whether it is a value or an
error. A caller stops waiting at its own deadline: a scrape after 2 s, answered without these
figures and with every other metric; a `/v1` request at its 5 s. So a stalled Postgres costs
one read per 5 s, and no caller waits for another's turn. Until session 7's review a lock was
held during the read and a failure was not kept: with Postgres paused, 20 callers of
`/v1/metrics` each waited for the read before them and did their own, 11 got no answer
within the server's 30 s, and scrapes of the API failed whole (review F3;
TestAFailingFigureReadIsSharedKeptAndLeftByItsCallers). The depth is one count over the rows in the four statuses, grouped
by channel and status, through the claim index and the reaper's index; the oldest due row is
read from the claim index, one probe per channel and priority. The time is Postgres's
(`now() - ready_at`, rule 7). The read has its own timeout of 2 s; when it fails, the figures
are left out of that scrape and the failure is logged at most once per 10 s. Every API
instance exports the same figures, so rules combine them with `max`, never `sum`. Measured on
1,000,000 waiting rows and 1,080,000 sent rows: the depth 121 to 122 ms, the oldest due row
0.09 to 0.15 ms (nine index probes) ([experiment 02](experiments/02-metrics-at-scale.md)).

**`GET /v1/metrics`** answers the main figures as JSON for a recent window: `window` is one of
`1m`, `5m` (the default), `15m` and `1h` (the bound of section 8); any other value is a 400.
Per channel: the queue depth by status and the age of the oldest due row (as above), and from
the attempts that started within the window, their number by outcome, the number of attempts
per second, the share that delivered, and the 50th, 95th and 99th percentile of the attempt's
duration and of the delivery latency (from the notification's creation to the start of the
attempt that delivered it plus its duration). The latency counts every wait since creation:
a scheduled notification's wait for its time, and the wait of one requeued by `/retry`
(review F9); the time it was due is not stored apart from `ready_at`, which retries move. The attempts are found by a BRIN index on
`delivery_attempts.started_at` (migration 5): attempt rows are only inserted, in time order,
so the index is a few pages and needs no maintenance on the queue's path. An answer is
computed at most once per 5 s per window and API instance, in the same way as the queue's
figures. The queue's figures are two statements and a window's two more, so any number of
clients costs at most ten statements per 5 s per instance (TestTheMetricsEndpointReportsTheRecentWindow,
TestTheMetricsEndpointIsComputedOncePerFiveSeconds,
TestTheWindowsAttemptsAreReadThroughTheirIndex). Measured with 2,080,000 notifications and an
hour of attempts at the full rate (1,080,000): the counts and the percentiles of a 5-minute
window take 105 to 110 ms and 143 to 163 ms, of a 1-hour window 0.31 s and 0.91 to 1.03 s.
The attempts are found through the index, but Postgres joins them to their notifications by
reading the whole notifications table, so the cost grows with the table, not only with the
window; a join by primary key, bounded by the window, measured slower (0.22 to 0.27 s and 1.6
to 2.2 s). Nothing removes old rows (section 16), so at some size the 1-hour window reaches the
5 s deadline and answers 500; nothing else is affected ([experiment 02](experiments/02-metrics-at-scale.md)).
Session 8 measured where ([experiment 03](experiments/03-tables-without-retention.md)): at
16,000,000 notifications the two statements of the 1-hour window take 3.8 to 5.3 s together,
at the deadline, those of the 15-minute window 2.4 to 3.6 s, and those of the default 5-minute
window 0.37 to 0.51 s. At the full rate of three channels, 16,000,000 rows are about 15 hours
of traffic. The same figures read from attempt rows that carry their notification's channel
and creation time, with no join, cost what the window holds whatever the table's size (at most
0.93 s for the hour); that change was measured and not made, because the class is the missing
retention, which also slows the list's filters (section 8) and grows the disk (section 16).

### Tracing

OpenTelemetry. Tracing is on when `OTEL_EXPORTER_OTLP_ENDPOINT` is set (OTLP over HTTP); it is
off by default. Off, the process has a tracer that records nothing, starts no goroutine,
opens no connection and logs nothing (TestTracingIsOffWithoutAnEndpoint).

- The API starts a span for every request (`GET /v1/notifications/{id}`, the route pattern),
  as a child of the request's `traceparent` header when there is one.
- Create and batch store the span's context on each row, as a W3C `traceparent` string. With
  tracing off and no `traceparent` header, the string is empty.
- The worker starts a delivery span for each attempt as a child of the stored context, so it
  joins the trace of the request that created the notification
  (TestTheDeliverySpanJoinsTheAPIRequestsTrace). The span carries the channel, the attempt
  number, the outcome and the status code, and an event when the permit is taken. A row with
  no stored context starts a new trace.
- The worker does not send `traceparent` to the provider; the provider receives the
  correlation id, as before.
- When the exporter cannot reach its collector, spans are dropped by the exporter's own
  bounded queue, and its errors go to the process's log at most once per 10 s.

### Alerts

A small set of Prometheus rules in `deploy/prometheus/alerts.yml`, loaded by the Prometheus of
the Compose `observability` profile. Each rule says what an operator does about it.

| Alert | Fires when | Means |
|---|---|---|
| `NotificationsBreakerOpen` | some worker's breaker of a channel has not been closed for 5 min, while the channel has attempts | The provider fails for that channel (section 6). A breaker stays half-open on an idle channel until a row comes, because a claim that finds nothing marks no probe; with no attempt it is not an outage, and until session 7's review it paged (review F2) |
| `NotificationsOldestDueRowTooOld` | the oldest due row of a channel has waited more than 5 min, for 5 min | Delivery is stalled or far behind |
| `NotificationsGateRefusals` | more than 0.1 refusals per second at a channel's gate, for 10 min | More dispatchers per channel than the gate serves within its budget, or Redis is slow (section 13) |
| `NotificationsFailedShareHigh` | more than 10% of a channel's finished attempts end `failed`, for 15 min | Messages are rejected or run out of attempts |
| `NotificationsDatabaseFailing` | the workers' claims, result writes or reaps fail, for 2 min | Postgres refuses writes or does not answer; rows whose result was not stored are sent again after their lease (section 6). Made read-only, Postgres failed result writes only for the lease of the rows in flight, about a minute, and then claims on every loop; a rule on result writes alone went pending and resolved without firing (review F2) |
| `NotificationsRateLimitUnavailable` | the rate-limit read fails, for 2 min | Redis is down or refuses writes; nothing is claimed (section 7) |
| `NotificationsProviderClosesIdleConnections` | more than 0.05 attempts per second end `closed_idle`, for 15 min | The provider's keep-alive is shorter than `IDLE_CONN_TIMEOUT + GATE_BUDGET` (section 13) |
| `NotificationsEventsDropped` | status events are dropped, for 10 min | Redis is down or slow for the publishers; delivery is not affected (section 9) |
| `NotificationsAPIDown` | Prometheus cannot scrape the API, for 2 min | The API is down or stalled; the queue's figures, which only the API exports, are gone with it, so the rules on them cannot fire (review F2) |
| `NotificationsNoWorker` | no worker answers its scrape, for 2 min | Nothing is delivered. Workers are found by DNS, so with none running there is no target at all: the rule asks for the absence of a worker that is up |

Four checks keep a rule from being one that cannot fire (lesson 12):

1. Every rule has a `promtool` test (`deploy/prometheus/alerts_test.yml`) with a case where it
   fires. `make alerts` runs `promtool check rules` and `promtool test rules`; CI runs them.
   A rule without a firing case fails `go test ./tools/alertcheck`
   (TestEveryAlertRuleHasAFiringTest).
2. Every metric a rule names, and every label value it matches, is exported by the real
   binaries: the test builds `cmd/api` and `cmd/worker`, runs them against Postgres, Redis and
   the recording provider, and reads their `/metrics`
   (TestEveryAlertRuleUsesMetricsTheBinariesExport). CI's compose job runs the same check
   against the running stack. `up` is Prometheus's own series, one per target; a rule's `job`
   must be a job of `prometheus.yml`.
3. Every metric a rule depends on is moved by a named test of the code path that produces it
   (the tests in the metrics table above, and TestAttemptsAndResultsAreCountedOnce,
   TestTheBreakerStateIsExportedWhenScraped, TestTheReaperCountsTheRowsItReturns).
4. A rule's promtool input has the shape the failure produces on the running stack, not the
   shape the rule expects. Checks 1 to 3 showed that a rule could fire on its own input; the
   review showed that the result-write rule's input (errors for 10 minutes) was one a worker
   never produces (review F2). Each rule was made to fire, or shown to stay quiet, on the
   Compose stack, and its test's input follows what was seen there (session 7's review
   record).

## 11. Invariants

Each invariant is enforced in one place and has at least one test that fails when the
enforcing line is disabled (`make guards` runs that check). The test names are filled in by
the session that builds the part.

| # | Invariant | Enforced in | Tests so far |
|---|---|---|---|
| I1 | The provider does not receive more than the limit for a channel in any one-second window (within the limits listed in section 7) | Send gate + limiter window | The limiter's part (A6): TestNoWindowHoldsMoreThanTheLimit, TestAPermitIsRefusedAtTheLimit, TestChannelsHaveSeparateWindows. At the provider's side, through the real client and limiter: TestTheProviderReceivesAtMostTheLimitInAnyWindow (3 workers with their own Redis client, limiter and provider client, 150 senders, limit 100, 3.3 s: peak 100 within one second by the provider's arrival stamps in 8 local runs, 6 of them from `go test -race -count=6 -v -run TestTheProviderReceivesAtMostTheLimitInAnyWindow ./internal/provider`). I1 at the provider holds only while A4 holds: under `-race` on the CI runner the provider saw 106 within one second (CI run 44730514192), because requests arrived more than the margin after their permit, the case of section 7's limits table. Session 4 made the test check the guarantee as that table states it: the peak exceeds the limit by at most the number of requests that arrived later than the margin (session 4's review, F1). Session 5's CI showed that form cannot be checked under the race detector either: "late" is measured from when `Wait` returned on a worker that is itself starved, so a request late by its grant can be on time by its stamp (CI run 44750111098: peak 47, limit 40, 6 counted late, in the fault test). So every I1 test checks the peak against the limit without the race detector, which is how `make guards` and CI's measurement step run them, and only logs it with the detector. Through the dispatchers (session 5): TestTheProviderReceivesAtMostTheLimitThroughTheDispatchers, three workers each with a dispatcher per channel, all three channels at the limit at once: peak 100 per channel, none later than the margin, without and with `-race` (A4); and TestInvariantsHoldUnderRandomFaults, under random faults (I5 row). The Compose stack with two workers: peak 100 per channel at the mock provider (CI's compose job). Under a burst (session 8, `tools/load`): 30,000 notifications, three runs, and 15,000 with 8 MB batches posted 8 at once, 20 runs: peak 100 per channel in every run but one, in which the mock provider stamped 130, 121 and 125 within one second on the three channels at once. Twelve later runs of the same burst captured the requests on the provider's network interface (tcpdump, kernel time): at most 100 per channel there and at the stamps. The one excess was not captured on the wire, so it is not shown whether the requests arrived late after their permits (the frozen-process row of section 7's limits, which all three channels at once would fit: a stall of the Docker VM that runs workers, Redis and provider) or only the mock provider's own stamps were late. It is recorded as unexplained, with its commands, in session 8's record. In the long runs of the fault test, one scenario of 606 exceeded the limit, 47 against 40, with 10 requests late by their own stamps, up to 144 ms (A4). |
| I2 | A row is never claimed by a second worker while its lease is valid | The row lock and the status filter in Claim; `SKIP LOCKED` so that claims do not wait | TestConcurrentClaimsNeverTakeTheSameRow (32 claimers, 2,000 rows), TestClaimTakesExactlyTheClaimableStatuses, TestClaimSkipsRowsLockedByAnotherTransaction |
| I3 | A result changes a row only if it carries the row's current attempt number and the row is still `processing` | The `WHERE` of Finish | The database part: TestAnAttemptNumberIsRecordedOnce. The `WHERE` of Finish: TestFinishFromAStaleAttemptChangesNothing, TestFinishOnARowThatIsNoLongerProcessingChangesNothing, TestRepeatedFinishAddsNoSecondAttemptRow |
| I4 | Every claimed row reaches Finish, or its lease expires and the reaper returns it | Single exit in `handle`; Reap | Reap: TestReapReturnsExpiredRowsToPendingOrFailed, TestTwoReapersAtOnceReturnEachRowOnce, TestReapSkipsRowsLockedByAnotherTransaction. The single exit (session 5): every claim of every scenario of TestInvariantsHoldUnderRandomFaults has its attempt row, which only Finish writes, and no lease expires; TestShutdownWaitsForSendsInFlightAndDoesNotCancelThem; a worker killed mid-send: TestAKilledWorkersRowComesBackThroughTheReaper; the reaper's batches: TestTheReaperReturnsExpiredRowsAndPublishesTheirStatus. |
| I5 | An accepted notification is never lost: it ends as `sent`, `failed` or `cancelled` | I4 + retries + the `failed` state | TestInvariantsHoldUnderRandomFaults (each scenario: two or three workers, 300 rows on two channels, random faults; every row final, none accepted twice, every `sent` row accepted by the provider). Long runs (session 8, `FAULT_RUNS=300 FAULT_SEED=1000` and `FAULT_SEED=2000`): 606 scenarios, 181,800 notifications, every one final in every scenario; 605 passed, the other failed I1 only (the I1 row, A4); 42 notifications accepted twice in 22 scenarios, each after an attempt without a response (at-least-once case 3); TestAFailedResultWriteIsRetriedAndNotSentAgain; TestAKilledWorkersRowComesBackThroughTheReaper |
| I6 | A provider response always decides the verdict | `Classify` | TestAResponseAlwaysWins; every row of the table: TestClassifyDecidesEveryRowOfTheVerdictTable, and from real sends through the client, the limiter and the recorder: TestTheVerdictTableHoldsForRealSends; a broken configuration does not drain the queue: TestNotFoundAndUnauthorizedOpenTheBreakerAndDoNotDrainTheQueue, and a Redis that refuses writes claims nothing: TestARedisThatRefusesWritesClaimsNothing |
| I7 | `attempts` only goes up. Each claim is one attempt, and no attempt number is used twice. | Claim; Requeue raises `max_attempts` | TestAttemptsNeverGoDown (at least 600 random steps of all five operations on six rows, and on, up to 3,000, until every operation has taken effect: 600 alone missed a cancel in 2 of 30 runs, found in session 6; a row that becomes final is replaced, the seed changes per run and is logged, and the test fails if any operation never took effect), TestRequeueRaisesMaxAttemptsByTheBudget |
| I8 | One send makes one connection, takes one permit and writes at most one request | Provider client (section 7, properties 1 to 5) | TestTheGateRunsOnceWhenTheConnectionIsReadyAndBeforeTheWrite (property 1), TestARefusedSendNeverReachesTheProvider and TestARefusedSendsConnectionIsClosedBeforeTheTransportGoesOn (property 2), TestADeadReusedConnectionIsNotReplayed (property 3), TestTheClientSpeaksHTTP11ToAServerThatOffersHTTP2 (property 4), TestRedirectsAreNotFollowed (property 5); the one deadline: TestOneDeadlineCoversConnectGateWriteAndResponse, TestAGateThatWaitsPastTheDeadlineTakesNoPermit |
| I9 | A due row of a higher priority is claimed before a due row of a lower one. A worker has at most one claim batch that is claimed and not yet permitted. | Dispatcher loop order; the claim batch rule | The queue's part: TestClaimsInPriorityTurnTakeHigherPrioritiesFirst, TestClaimTakesOnlyItsChannelAndPriority. The loop (session 5): TestHighPriorityOvertakesBacklog; the claim batch rule: TestAWorkerHasAtMostOneClaimBatchWaitingForPermits. |
| I10 | The same idempotency key never creates a second notification or batch | Unique indexes + request hash; `ON CONFLICT DO NOTHING` then the stored hash decides | The database part: TestIdempotencyKeysAreUniqueInTheDatabase. The hash: TestTheRequestHashIgnoresFormAndDefaults. Through the API (session 6): TestConcurrentIdenticalRequestsCreateOneNotification (20 identical requests at once: one 201, 19 times 200, one row), TestAKeyReusedWithAnotherRequestIsAConflict, TestConcurrentIdenticalBatchesCreateOneBatch (10 identical batches of 100 at once: one batch, the same ids in the same order; another batch under the key is a 409). |
| I11 | No request can make the server allocate or return more than its stated bound | Bounds table, section 8; decoding that stops at the bound; the rendered length measured before rendering; one error writer | Request headers and the correlation id (section 8). Session 6, each at the bound, one under and one over: TestRequestBodiesAreBoundedAtTheirLimit, TestBatchSizeIsBoundedAtAThousand, TestContentIsBoundedPerChannelInCharacters, TestTemplateBodyBounds, TestVariablesAreCountedWhileDecoded, TestVariableNamesAreBounded, TestTheRenderedTotalOfARequestIsBounded, TestIdempotencyKeyAndTemplateNameBounds, TestRecipientFormatsPerChannel, TestFieldErrorsAreKeptUpToTheirBound, TestTheListIsOrderedAndPagedByCursor, TestTheTemplateListIsPagedWithoutBodies, TestTheAttemptHistoryIsBounded. What the server allocates, from the process's allocation counter: TestABatchOfManyEmptyItemsIsRefusedWithoutDecodingThemAll (8 MiB of `{}`, 2.8 million items: under 1 MiB), TestManyVariablesAreRefusedWhileDecoding (1 MiB of variables: 4 MiB), TestARequestOverTheRenderedTotalIsRefusedBeforeRendering (a batch that would render 384 MB: 4 MiB), TestATemplateIsCompiledOncePerRequest (500 items of a template with 16,000 placeholders, 8 MB rendered and stored: 72 MiB) (`go test -count=1 -v -run 'Decoding|BeforeRendering|CompiledOnce' ./internal/api`). Across requests (session 6's review, F1): TestConcurrentRequestsRenderInBoundedMemory (40 batches at once, each rendering 8 MB from 44 KB: the heap grows by 144 to 199 MiB, 745 MiB without the slots; `go test -count=6 -v -run TestConcurrentRequestsRenderInBoundedMemory ./internal/api`). Time: TestSlowRequestBodiesAreCutOff, TestAnswersToAClientThatDoesNotReadAreCutOff, TestARequestEndsAtItsDeadlineWhenPostgresStalls, TestAListPageReadsTheIndexFromItsCursor. Inputs that cannot be answered: TestAScheduledAtFromTheYear10000IsRefused, TestAnAnswerThatCannotBeEncodedIsA500, TestACursorOutsideTheTimeRangeIsRefused. What it returns: TestErrorBodiesStayWithinTheirBoundForAnyRequest. Session 7, the WebSocket (section 9): TestWebSocketConnectionsAreBoundedPerInstance, TestASlowWebSocketClientIsDisconnectedAndMemoryStaysBounded (1,000,000 events to a client that does not read: the heap did not grow), TestAWriteToAClientThatStopsReadingEndsAtItsDeadline, TestAWebSocketClientMustNotSendDataMessages (a frame that claims 1 GiB holds nothing), TestAWebSocketClientThatDoesNotAnswerPingsIsClosed, TestWebSocketFiltersAreBounded; events read from Redis: TestAMalformedOrOversizedEventIsDropped; the metrics: TestTheMetricsEndpointReportsTheRecentWindow (four windows), TestTheMetricsEndpointIsComputedOncePerFiveSeconds, TestMetricLabelsHaveBoundedValues. |
| I12 | Queue times come from the Postgres clock; rate-limit times from the Redis clock | Queue SQL; Lua script | Queue: TestClaimSetsTheLeaseOnTheDatabaseClock, TestFinishStoresEachResultOnTheDatabaseClock. Redis: TestTheLimiterSendsRedisNoClockTime checks that no argument the limiter sends is a time, so the only clock a script can read is `TIME`; a script that read no clock would fail TestNoWindowHoldsMoreThanTheLimit. |
| I13 | A status event is best-effort: publishing it never waits and never fails, so a lost or slow event never delays or changes a send, a result write or an answer of the API (session 7) | `events.Redis.Publish`: a bounded buffer and one sender with its own Redis client and a 1 s pipeline | TestPublishNeverBlocksWhenRedisHangs (12,288 events at once against a Redis that never answers: none waits, what does not fit is dropped and counted, the first pipeline is given up at 1 s); TestLostEventsDoNotAffectDelivery (300 rows sent at the limit with every event going to that Redis, and no slot held after the last result; with a publisher that waited, 30 of 300 were sent in 15 s); TestTheAPIAnswersWhileRedisHangs (a create, a batch of 1000 and a cancel each answered within 1 s); on the Compose stack with Redis stopped for 4 minutes, rows were created and the API stayed ready (session 7's record) |

Tests of I1, I4, I5 and I8 observe the system **from the provider's side**: a recording HTTP
server stamps each request at the moment it arrives. They run through the real provider
client. The test kit does not contain a fake provider that could skip the send gate.

## 12. Assumptions and how each is proven

| # | Assumption | Status |
|---|---|---|
| A1 | Closing the connection in `GotConn` stops a refused request from reaching the server. Cancelling alone does not. | **Measured** before the design was fixed: cancel alone let 13 of 40,000 refused requests through; close and cancel, 0 of 40,000 ([experiment 01](experiments/01-gate-refusal.md)). **Permanent test** (session 4): TestARefusedSendNeverReachesTheProvider, through the real client: 5,000 refused sends on new connections and 5,000 after an allowed send, of which 3,800 to 4,800 per run ran on a reused connection; 0 reached the recording server. With the cancel and no close, 5, 5 and 8 of 10,000 reached it; with neither, all of them (review F5). The cancel was removed after the close alone gave 0 of 20,000 (section 7, property 2; `go test -count=1 -v -run TestARefusedSendNeverReachesTheProvider ./internal/provider`). These runs had 5,000 per half; the test now has 2,500, because each refused send leaves a client port in TIME_WAIT for 30 s on macOS, which has about 16,000: 20,000 ran out of ports, and two runs of 10,000 within 30 s did too (review F4). Because a leak is rare, a second test checks the order directly: when the client's hook returns, the connection is already closed (TestARefusedSendsConnectionIsClosedBeforeTheTransportGoesOn, on HTTP and TLS, new and reused). Both count every request that reached the recorder, not only those whose body was readable. |
| A2 | With `GetBody = nil` the transport never replays a request, so one send is one `GotConn` and at most one write | **Measured** by the design reviewer (one hook call, one request received). **Permanent test** (session 4): TestADeadReusedConnectionIsNotReplayed. The provider closes the kept-alive connection while the send waits at the gate: one gate call, nothing received, an error. With `GetBody` left as Go sets it, the gate ran twice in one send (two permits) and the client saw a 202 for a request whose first write was lost (`go test -count=1 -v -run TestADeadReusedConnectionIsNotReplayed ./internal/provider`, mutation by hand; `make guards` checks it). |
| A3 | The claim reads the index in order with no sort and no full scan, under a custom plan and under a generic plan, also with many tied `ready_at` values and many rows that are not due yet | **Measured** by the second design reviewer on the final query: 2.8 million rows, 1 million of them future high-priority rows; 0.15 to 0.59 ms under both plans; a claim that finds nothing due reads 3 index pages. **Permanent test** (session 2): TestClaimPlanReadsTheIndexInOrder, on 300,000 rows (50,000 due with one `ready_at`, 200,000 not due, 50,000 finished). Under both plans: one ordered index scan, no sort, no sequential or bitmap scan of the claim index; a claim of 10 reads 4 index pages in 0.13 to 0.31 ms, a claim that finds nothing reads 3 pages in 0.03 ms (`go test -count=1 -v -run TestClaimPlanReadsTheIndexInOrder ./internal/store`). With the rejected `IN (SELECT ...)` form the generic plan is a sequential scan with a hash join, 32.5 ms (the same test, mutation applied by `make guards`). |
| A4 | The time from permit to arrival at the provider varies by less than the 50 ms margin, on new and on reused connections | **Measured** (session 4) at the recording server on one machine, with the real limiter: TestPermitToArrivalStaysWithinTheMargin. 40 senders for 2 s at the limit of 100 per second over TLS; half use a fresh client per send, whose handshake the server holds back 100 ms, half share one client. The time is from `Wait` returning to the arrival stamp, both on the worker's monotonic clock; Redis's reply to the take script is not inside it. Four runs without the race detector: new connections median 0.15 to 0.42 ms, maximum 2.3 to 9.9 ms; reused, median 0.06 to 0.16 ms, maximum 3.0 to 6.6 ms (`go test -count=4 -v -run TestPermitToArrivalStaysWithinTheMargin ./internal/provider`). With `-race`, six runs: maxima 14 to 41 ms on new and 10 to 29 ms on reused connections (the same with `-race -count=6`): the race detector slows the client and the server on the same CPUs. So the test asserts the margin only without the race detector, which is how `make guards` runs it, in CI too. Session 4's reviewer, with 14 busy processes on 8 CPUs and `-race`, measured 71.7 ms on reused connections: under CPU starvation A4 does not hold, which is the "frozen worker" case of section 7's limits table. Not measured: the network between a worker and a real provider. **Through the dispatchers** (session 5), with a worker's real load (three workers, nine loops, three channels at the limit at once, 1,200 requests): TestTheProviderReceivesAtMostTheLimitThroughTheDispatchers. Without the race detector: median 60 to 70 µs, p99 under 1.1 ms, maximum 0.97 to 2.27 ms (`go test -count=1 -v -run TestTheProviderReceivesAtMostTheLimitThroughTheDispatchers ./internal/dispatch`). With `-race`, three runs: maximum 10.5 to 28.6 ms, none past the margin (the same with `-race -count=3`). CI runs the measurement without the race detector on every push (the test job's last step). **The margin stays 50 ms**: through the dispatchers the program as it ships uses under 5% of it on this machine. The cases that exceed it are a starved CPU (the race detector with other packages' tests on the same CPUs in CI, the reviewer's 71.7 ms), which a larger margin would not bound either; they are the "frozen worker" row of section 7's limits table, and every I1 test checks the guarantee as that row states it. A margin of 100 ms would cost 4.5% of the throughput for no case that was measured. **Session 8** measured the first case without the race detector: in 606 scenarios of the fault test, one (seed 2268) had 10 requests arrive later than the margin after their permits, the latest 144 ms, and the provider received 47 within one second against a limit of 40; the same seed passed five times alone, the latest 10 ms. A 100 ms margin would not have covered it either. It is the frozen-process row of section 7's limits; the margin stays 50 ms. |
| A5 | With the claim batch rule, the wait at the gate stays near one window plus `10 × W / 100` seconds and nothing is refused, for up to 12 workers; throughput stays near the top speed | Simulated in the design review (93 to 96 sends per second, no refusals, worst wait 2.04 s at 12 workers), with waiters served in order. Session 3's reviewer modelled it with the real limiter, which serves waiters in no order, and every worker keeping 10 rows waiting: worst waits of 3.16 s and 2.12 s at 12 workers, no refusals; refusals from 24 workers (section 6). **Measured** (session 5) on the real dispatchers: TestGateWaitAndThroughputWithManyDispatchers, 1, 3, 6 and 12 dispatchers on one channel, each in its own worker, a provider answering in 20 ms, 12 s each. The provider client has the defaults of section 13 (send timeout 15 s, gate budget 5 s); the dispatcher has the tests' shorter pauses and open time (`testOptions`: idle pause 50 ms, lease 20 s, open time 3.5 s), which do not change the wait at the gate or the rate (session 5's review, a nit). Without the race detector: 94.1, 94.9, 94.9 and 94.9 sends per second (top speed 95.2); longest wait at the gate 6 ms, 0.98 s, 1.03 s and 1.05 s; median 2 to 10 ms; no refusal (`A5_FULL=1 go test -count=1 -v -run TestGateWaitAndThroughputWithManyDispatchers ./internal/dispatch`). With `-race`: 94.1 to 95.0 sends per second, longest wait 1.09 s, no refusal. The wait is about one window, not `10 × W / 100` seconds, because `Available` stops the claims while the window is full (section 6). By default the test runs 1 and 12 dispatchers for 6 s; CI runs all four without the race detector. **Past these settings** (session 8, with the test's knobs, one run each, without the race detector): with 12, 24 and 36 dispatchers, 94.9, 94.8 and 94.4 sends per second, longest wait 1.07 s, 3.13 s and 4.21 s, median 11 ms, 1.01 s and 1.05 s, no refusal (`A5_COUNTS=12,24,36,48 A5_RUN=12s go test -count=1 -timeout 0 -run TestGateWaitAndThroughputWithManyDispatchers -v ./internal/dispatch`); 48 could not be measured, because 48 workers' connection pools passed the test Postgres's 100 connections (section 13). With a provider that answers in 200 ms, 1, 6 and 12 dispatchers: 93.5, 94.7 and 94.9 per second, longest wait 8 ms, 1.03 s and 1.06 s, no refusal (`A5_COUNTS=1,6,12 A5_LATENCY=200ms A5_RUN=12s ...`). At a limit of 20: 2 dispatchers 18.9 per second (top speed 19.0), longest wait 1.02 s, no refusal; 12 dispatchers 18.9 per second, median wait 2.1 s, longest 5.0 s (the gate budget), and 50 sends refused for no permit in time (`A5_LIMIT=20 A5_COUNTS=2,12 A5_RUN=12s ...`). Above about 12 dispatchers the wait follows the design review's estimate, one window plus `CLAIM_BATCH × W ÷ RATE_LIMIT`, and the refusals begin where that passes the gate budget (section 6, section 13). |
| A6 | The sliding-window script grants at most the limit per window under concurrent callers | **Permanent test** (session 3): TestNoWindowHoldsMoreThanTheLimit. 200 callers on 4 Redis clients, half waiting in `Wait`, half asking once at random moments, limit 100, margin 50 ms, for 3.3 s: 400 permits, at most 100 in any window of 1,050,000 µs on the permits' own Redis times (`go test -count=1 -v -run TestNoWindowHoldsMoreThanTheLimit ./internal/ratelimit`). With the window's lower bound moved 2 ms early, it fails in 3 of 3 runs. |

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
| Attempts (read by the API: a new row's `max_attempts` and the budget of `/retry`); backoff | `MAX_ATTEMPTS`; `BACKOFF_BASE`, `BACKOFF_CAP` | 6; 2 s doubling to a cap of 2 min, with jitter | | |

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

These relations cannot be checked by a single worker. They are stated here and watched by
metrics:

- One worker can run at most `slots ÷ provider latency` sends per second. With 50 slots that
  is the full rate up to a provider latency of about 500 ms; a slower provider needs more
  slots or more workers.
- The wait at the gate must stay under the gate budget. The design review estimated
  `window + claim batch × workers ÷ rate`; session 3's review measured refusals from 24
  workers with a model that kept every worker's batch waiting (section 6). Session 5 measured
  about one window (1.05 s) up to 12 dispatchers per channel, and session 8 found the
  estimate close above that: 3.1 s at 24 and 4.2 s at 36 dispatchers with no refusal, and at
  `RATE_LIMIT=20`, 12 dispatchers reached the budget and had refusals (A5). So, per channel,
  `dispatchers ≤ (GATE_BUDGET − window) × RATE_LIMIT ÷ CLAIM_BATCH`: about 40 at the
  defaults, 8 at a limit of 20. A worker cannot check it, because it does not know how many
  others there are; the refusals at the gate
  (`notifications_attempts_total{outcome="no_permit_in_time"}`, alert
  `NotificationsGateRefusals`) show when it is crossed. Each refusal costs an attempt
  (section 6), so a channel that stays past the relation can use up rows' attempts.
- Every worker and API instance has its own Postgres connection pool, of pgx's default size
  (the larger of 4 and the number of CPUs), and the total must stay under Postgres's
  `max_connections` (100 by default). Session 8 found it when 48 workers of the A5 test could
  not connect (`FATAL: sorry, too many clients already`); 36 could. A deployment with many
  workers sets `pool_max_conns` in `DATABASE_URL` or raises `max_connections`. A worker whose
  claims fail counts them (`notifications_worker_errors_total{operation="claim"}`, alert
  `NotificationsDatabaseFailing`).
- `IDLE_CONN_TIMEOUT + GATE_BUDGET` should stay under the provider's keep-alive timeout;
  otherwise some sends find their connection closed by the provider while they wait at the
  gate (section 7, the connection pool). The worker does not know the provider's timeout.

## 14. Code layout

```
cmd/api, cmd/worker, cmd/migrate, cmd/mockprovider
internal/domain        types, statuses, validation, templates; no I/O
internal/store         all SQL; queue.go holds the five queue operations; migrations/
internal/redisclient   the one place a Redis client is built; context deadlines are on
internal/ratelimit     the Redis sliding window: Available, Wait
internal/provider      the HTTP client with the send gate; Result
internal/dispatch      the loop, Classify, retry policy, breaker, reaper
internal/api           handlers, middleware, the WebSocket hub, /v1/metrics
internal/events        the event; the Redis publisher that never waits; the subscription
internal/observability logging (and Every, a log line per period), metrics, tracing
internal/config        settings and the checks of section 13
internal/recorder      the recording provider server: stamps each request on arrival,
                       injects responses, latency, slow TLS setup and closed connections
internal/testkit       test database and test Redis (testcontainers, or servers that
                       make guards starts once); starts the recorder
tools/guards           `make guards`: disables each guard and expects a named test to fail
tools/testnames        checks that every test named in docs/ exists in the code
tools/alertcheck       every alert rule has a firing promtool test, and names only metrics
                       and label values that the binaries export (section 10)
tools/e2e              notifications through the API of the Compose stack, and a WebSocket
                       client that follows one of them
tools/load             a burst against the Compose stack, measured from outside: acceptance,
                       the API's memory, the drain, high-priority latency, the provider's peak
                       and duplicates (`make load`, session 8)
internal/systemtest    tests across the API, Redis and the worker: one correlation id,
                       one trace, a WebSocket event from a real delivery
deploy/prometheus      alert rules, their promtool tests, the scrape configuration
```

The recording server is its own package, not part of `testkit`, because `cmd/mockprovider`
runs the same code as the local provider of the Compose stack. Inside `testkit` it would pull
the testcontainers dependencies into that binary. (Changed in session 1.)

Dependencies point one way: `api` depends on `store` and `domain`; `dispatch` on `store`,
`ratelimit` and `provider`; all of them depend on `domain`; `domain` depends on nothing.
`api` and `dispatch` also depend on `events` and `observability`, which depend only on
`domain`.
(Session 6 removed `internal/service`; section 8 says why.)

## 15. Requirements coverage

| Requirement | Where | Session |
|---|---|---|
| Create, with recipient, channel, content, priority | Section 8: TestCreateAndReadANotification, TestEveryFieldIsValidatedAndNamed | 6 |
| Batch creation up to 1000 | Section 8: TestBatchSizeIsBoundedAtAThousand, TestABatchIsValidatedWholeBeforeAnyIsStored | 6 |
| Status by id or batch id | Section 8: TestCreateAndReadANotification, TestTheBatchSummaryCountsEveryStatus | 6 |
| Cancel pending | Sections 5, 8: TestCancelOnlyFromWaitingStatusesThroughTheAPI | 2, 6 |
| List with filters and pagination | Section 8: TestTheListIsOrderedAndPagedByCursor | 6 |
| Asynchronous processing by queue workers | Sections 5, 6 | 2, 5 |
| Rate limit of 100 per second per channel | Section 7 | 3, 4, 5 |
| Priority queue | Sections 5, 6 | 2, 5 |
| Content validation | Section 8: TestEveryFieldIsValidatedAndNamed, TestRecipientFormatsPerChannel, TestContentIsBoundedPerChannelInCharacters, TestContentAndVariablesMustNotContainNUL | 6 |
| Idempotency | Sections 6, 8: I10 | 5, 6 |
| Delivery and retry logic | Section 6 | 4, 5 |
| Metrics endpoint | Section 10: `GET /v1/metrics` (TestTheMetricsEndpointReportsTheRecentWindow) and `GET /metrics` (TestQueueGaugesComeFromTheDatabase, TestAttemptsAndResultsAreCountedOnce, TestMetricLabelsHaveBoundedValues); alerts (TestEveryAlertRuleHasAFiringTest, TestEveryAlertRuleUsesMetricsTheBinariesExport) | 7 |
| Structured logging with correlation ids | Section 10: TestOneCorrelationIDFollowsAMessageFromTheRequestToTheProvider, TestEveryWorkerLineAboutANotificationCarriesItsCorrelationID | 1, 7 |
| Health check | Section 8 | 1 |
| Bonus: failure handling (breaker, dead-letter state, retry endpoint) | Section 6; `/retry`: TestRetryRequeuesOnlyAFailedNotification | 5, 6 |
| Bonus: scheduled notifications | Sections 3, 5: TestAScheduledNotificationWaitsUntilItsTime | 2, 6 |
| Bonus: templates | Section 8: TestTemplatesAreCreatedOnceAndRendered, TestTheRenderedLengthIsMeasuredBeforeRendering | 6 |
| Bonus: WebSocket updates | Section 9: TestAWebSocketClientGetsTheCurrentStateThenEveryChange, TestAWebSocketClientReceivesTheSentEventOfARealDelivery, the bounds of section 9; CI's compose job (`tools/e2e`) | 7 |
| Bonus: distributed tracing | Section 10: TestTheDeliverySpanJoinsTheAPIRequestsTrace, TestTracingIsOffWithoutAnEndpoint | 7 |
| Bonus: GitHub Actions | [Plan](plan.md) | 1 |
| Docker Compose, OpenAPI, migrations, single-command tests | [Plan](plan.md); OpenAPI: TestOpenAPIMatchesTheServer; the stack end to end through the API: CI's compose job (`tools/e2e`) | 1, 6 |

## 16. Deliberately left out

| Not done | Why |
|---|---|
| Priority aging (a low-priority row can wait as long as higher ones keep arriving) | Strict priority is what the requirement asks for and is easy to reason about. Aging needs real traffic to tune. |
| A circuit breaker shared by all workers | It needs state in Redis and a second protocol between workers. The per-worker breaker bounds what a broken channel costs per worker; with little traffic rows can still reach `failed`, the dead-letter state, and `/retry` requeues them (section 6, "What the breaker protects"). |
| A breaker that counts a failure rate | Five failures in a row is simple to reason about; a rate needs a window and a threshold tuned on real traffic. A provider that fails half its requests opens it often, which is the intended reading (section 6). |
| Pausing the whole channel on one 429 | A provider that throttles a few requests at random would halve throughput. A run of 429s opens the breaker, which covers the sustained case. |
| Not counting an attempt when nothing was sent | It needs a second counter, or attempts that go down. Both were sources of defects on paper (design review, F2). The rare lost attempt is cheaper. |
| Expiry of idempotency keys | Needs a retention policy; without one, keeping keys is the safe default |
| Exactly-once delivery | Not possible across a network call. See "At-least-once" in section 6. |
| Tenants | The requirements have none. Keys, limits and templates would be scoped per tenant. |
| Quotas and rate limits per client at the API | Each request is bounded, and rendering is bounded across requests (section 8), but how much one client may store over time is a policy per client, which needs tenants. A gateway in front of the API is the usual place. |
| Durable status events, or a replay of missed ones | Events are best-effort (section 9): a client that reconnects is sent the current state, which is what a replay would end in. A durable stream (Redis Streams, Kafka) is a second source of truth to keep consistent with Postgres. |
| A WebSocket stream of every event | Each client names what it follows (section 9). A stream of everything would send 300 events per second to each client, and is a job for a consumer of the Redis channel, not for a public endpoint. |
| An API key in the WebSocket's query string | Browsers cannot set headers on a WebSocket, but a key in a URL is written to proxy and access logs. The key is a header, as on every `/v1` path; a browser client needs a server of its own in between. |
| `traceparent` sent to the provider | The provider is outside; the trace ends at the worker's delivery span. The provider receives the correlation id (section 7). |
| Removing old notifications and attempts | Needs a retention policy (how long a client may ask for a status) that the requirements do not give. Without it the tables grow without end, and two reads grow with them (session 8, [experiment 03](experiments/03-tables-without-retention.md)): `GET /v1/metrics` reaches its 5 s deadline over 1 hour at about 16,000,000 notifications, about 15 hours at the full rate, and over 15 minutes later; a list filter that matches no row reads the whole list index, 1.6 to 3.2 s at 16,000,000. A deployment at that rate needs a retention job. If it must run without one, storing each attempt's channel and creation time on its row (a migration and a change to Finish's insert) bounds the metrics by the window, measured at 0.93 s at most for the hour. |
