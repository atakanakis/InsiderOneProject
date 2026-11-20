# Review 0: the design, before any code

- **What was reviewed:** the first draft of [`architecture.md`](../architecture.md) and
  [`lessons.md`](../lessons.md). No code existed.
- **Reviewer:** a subagent on a different model (Claude Opus) from the one that wrote the
  draft (Claude Fable). It started with no knowledge of the author's reasoning. It was given
  the assignment, the two documents, the first version of the project with its review records,
  and the instruction to be impartial and to settle points by experiment where an experiment
  was cheap. It could not edit the repository.
- **What it ran:** a replay experiment and a timeout experiment against Go's HTTP transport;
  the claim query on Postgres 17 with 2 million rows, under custom and generic plans; a
  simulation of the dispatcher loop with 1 to 12 workers.
- **Verdict:** go ahead after changes. Four major findings, four minor.

## Round 1: findings and what was done

| # | Severity | Finding | Reviewer's evidence | Resolution in the design |
|---|---|---|---|---|
| F1 | major | Go's transport replays a request on a new connection when a reused connection dies, and it treats a request with an `Idempotency-Key` header as safe to replay. A replay is a second write inside one attempt. If the gate refuses the replay, the draft classified the attempt as "unsent" although the provider already had the message. | Measured: the server received a request twice while the client saw one 202. With a refusal on the replay: written once, flagged "refused". | The client sets `req.GetBody = nil`, which turns replays off, so one send is one connection and at most one write (measured: one hook call, one request received). The "unsent" verdict no longer exists (see F2). §7, invariant I8. |
| F2 | major | `attempts` was both the fencing token and the retry budget, and the draft gave an attempt back for an unsent row. So a number could be reused and a real attempt's history row silently dropped. A late result could also revive a cancelled row, because Finish checked only the attempt number. Attempts were counted at claim, so "an attempt is counted only when a request may have been written" was false. | Reasoned from the SQL in the draft | The reviewer proposed two counters. The design takes the simpler rule instead: **`attempts` only goes up.** Nothing is given back; a refused request costs an attempt. Requeue raises `max_attempts` and does not reset the counter. Finish also requires `status = 'processing'`, except for a delivered result. Queue writes run on a context that shutdown cannot cancel. The false invariant was removed. §5, I3, I7. |
| F3 | major | Every worker reads the same `Available()` hint, so several workers together claim more rows than there is room for. The extra rows wait at the gate in no priority order, and with 6 or more workers some waited past the gate budget and were refused. | Simulation: 3 workers, worst 1% waited 2.0 s; 6 workers, median 1.05 s and 15 refusals in 25 s | A worker claims at most 10 rows at a time and claims nothing while one of its rows still waits for a permit. The wait at the gate is then bounded by about `workers / 10` seconds. The reviewer proposed a local priority queue at the gate; the batch rule makes the queue too short to need one. To be measured in session 5 (assumption A5). §6, I9. |
| F4 | major | The draft had a "promoter" that moved waiting rows to a ready queue. It ordered by time only, in bounded batches per tick. With a million low-priority rows due, a high-priority retry due one second later was promoted about 17 minutes late. | Measured: 999,500 rows ahead of the high-priority row; 6.6 ms per batch of 500 | The promoter was **removed**. Rows are claimed directly when due, one statement per priority, highest first. There is no ready queue, no tick delay and no extra write per row. This also removes the draft's dependence on housekeeping for all retry traffic. §3, §5. |
| F5 | minor | Timeouts did not add up. `http.Client.Timeout` is invisible to the gate hook, so the gate kept waiting and took a permit for a request that was already dead. Connection setup was missing from the lease relation. The breaker's open time equalled the longest send. | Measured: `Do` returned after 1.0 s with a 300 ms client timeout; the hook saw the deadline 0 of 800 times | One context deadline per send covers connect, gate, write and response; the gate waits on a child of it; no `Client.Timeout`; explicit dial and TLS timeouts; breaker open time longer than the send timeout; a shutdown timeout the worker knows. §7, §13. |
| F6 | minor | The breaker's reaction to "unsent" and to a permanent failure was not defined; a connection failure had no row in the outcome table. | Reasoned | The verdict table now has a breaker column and a row for every case. §6. |
| F7 | minor | The claim's plan was as designed under a custom plan (0.25 ms on 700,000 tied rows) and was a full table scan under a forced generic plan (293 ms on 2 million rows). Dead index entries at the head of the queue slow the claim while a long transaction blocks vacuum. | Measured | `id = ANY(ARRAY(...))` form (0.84 ms under the generic plan); assumption A3 requires both plans to be tested; session and statement timeouts. §5. |
| F8 | minor | Several statements were written as facts and were assumptions: "whether or not anything stalls", "one tick of delay", "under a second", "the worker refuses to start", "HTTP/1.1 only" with no mechanism, "two workers never hold the same row". | Listed by the reviewer | Each was reworded to what is true, moved to the limits in §7, or added to §12. |

No requirement was found missing or misread.

## What this round showed

- Three of the four major findings were in the parts the draft had added to be *simpler* than
  the first version: the promoter, the hint, and giving an attempt back. A new design has the
  same risk as a fix: it is new, and it has only been checked against the problems its author
  thought of.
- Two findings were settled by experiments that took minutes. Without them, F1 and F4 would
  have been found in a running system, as defects.
- Where the reviewer proposed adding a mechanism (a second counter, a local priority queue, a
  priority-aware promoter), the design removed something instead. Fewer parts was the goal of
  the rebuild, and each of those additions would have been a new part to get wrong.
- The reviewer's table of the lessons marked most process lessons as "by discipline". That is
  accurate. The build plan turns the ones that can be checked into checks (`make guards`, the
  test-name check in CI, alert tests) and leaves the rest as rules for each session.

## Round 2: a fresh reviewer checks the revised design

- **Reviewer:** a new Opus instance. It had not seen the first draft, only the revised design
  and the round 1 table above, whose "Resolution" column it was told to treat as claims.
- **What it ran:** the final claim query on Postgres 17 with 2.8 million rows under custom
  and generic plans; a simulation of the claim batch rule; the same rule through Go's real
  HTTP client with slow dials; redirect and idle-connection probes.
- **Verdict:** go ahead after changes; "each fix is a sentence or one line in the design, not
  a new mechanism".

**Round 1 findings re-checked:** F1, F2, F4, F5 and F7 resolved (F4 and F7 by measurement);
F3 resolved in practice, with a wrongly stated bound; F6 partly; F8 mostly.

| # | Severity | Finding | Reviewer's evidence | Resolution in the design |
|---|---|---|---|---|
| 1 | major | The half-open breaker's probe was not defined. If `Allow()` reserved the probe and the claim then found no row, the channel would never claim again on that worker. If it only read, about 15 probes would go out before the first one answered. | Reasoned | `Allow()` only reads; `Started()` marks the probe when a row really starts; `Report()` clears it. §6. |
| 2 | major | "`Available()` fails first" assumed Redis reads and writes fail together. With a read-only replica or Redis out of memory, the hint keeps answering while every permit request fails: all rows are refused, the breaker learns nothing, and thousands of rows per minute reach `failed` without one send. | Reasoned | A refusal caused by a limiter **error** counts as a breaker failure. Only "no permit within the gate budget" leaves the breaker alone. §6, §7. |
| 3 | major | A wrong provider URL (404) or an expired credential (401, 403) was a "permanent failure" of each message, and the breaker saw a healthy provider. The whole backlog would drain into `failed`. | Reasoned | The verdict now separates "this message is wrong" (400, 413, 422: `failed`) from "the channel is wrong" (everything else: retry, breaker failure). §6. |
| 4 | major | Go's client follows 301, 302 and 303 with a body-less `GET` on a second connection: two permits, and the final 200 marks a message `sent` that the provider never accepted. | Measured | Redirects are not followed. §7, property 5. |
| 5 | minor | The stated bound for the wait at the gate ("about W/10 seconds") was wrong: about 1 s at 3 and 6 workers, 2.04 s at 12. The real top speed is about 95 per second, because the window is 1.05 s. | Simulated | Bound, top speed and the worker limit (about 35 per channel) restated. §6, §7, §13. |
| 6 | minor | With replays off, a kept-alive connection that the provider closes while the request waits at the gate costs an attempt. | Measured | Idle connections are dropped after 10 s; the case is counted in a metric and named in §7. |
| 7 | minor | "A delivered result wins whatever the status" could only fire in a narrow window, and when it did, a `cancelled` or `failed` row became `sent`, against "final". | Reasoned | The exception was **removed**. Finish has one rule for every result. The result write is retried until the database answers. §5, I3. |
| 8 | minor | Requeue gave uneven budgets. | Reasoned | `max_attempts = attempts + budget`. §5. |
| 9 | minor | A status event was published even when the result was stale. | Reasoned | Finish returns the new status; nothing is published when nothing changed. §5, §6. |
| 10 | minor | The code sketch did not say whether slots and the breaker are per channel. | Reasoned | One dispatcher per channel. §6. |

Measured and confirmed in round 2: the claim query (assumption A3), the batch rule's
throughput (93 to 96 sends per second, no refusals, 1 to 12 workers), HTTP/1.1 through
`Transport.Protocols`, and the idle pool size (88.8 sends per second with Go's default of 2
idle connections, 95.0 with the pool sized to the slots).

## What the two rounds showed together

- Round 2 found fewer and smaller problems than round 1, and none in the queue. Three of its
  four major findings were about what the system does when something *around* it is
  misconfigured or half-broken: a wrong URL, a redirect, a Redis that reads and cannot write.
  The first draft had only been checked against clean failures.
- Round 2's changes were not reviewed on paper a third time. They are small, and two of them
  remove something. Each one is listed in the [plan](../plan.md) as a test that its build
  session must write, and the review of that session checks it.
- The design stops here and the build starts. More review of documents would cost more than
  it finds; the next reviews have code and tests to run.
