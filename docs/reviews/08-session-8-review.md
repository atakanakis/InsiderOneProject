# Review of session 8: the whole system

- **Date:** 2026-11-21.
- **Reviewer:** a fresh Claude Opus instance, read-only, with the rules of
  [`.claude/agents/reviewer.md`](../../.claude/agents/reviewer.md). It was given the assignment
  text, the session's brief from [`plan.md`](../plan.md) ("8. Hardening") and the repository at
  `d54036a`, with CI green on it (run 44931660697). It was not given the author's reasoning or
  conclusions. It had no time limit; it ran for about 1 h 30 min.
- **Scope:** the whole system, not only the session's change: every requirement of the
  assignment on the running stack, faults injected into it and counted at the provider, the
  fixes of the seven earlier reviews, every claim and number of the README and `docs/`, and
  whether a reader can follow the repository in half an hour. It was asked to examine the two
  events the session record could not explain, without taking the record's reading as given.
- **Runs:** once, after the measurements, the local guards and CI were green. There was no
  second round. Each fix below is verified by its own named test and by `make guards`, not by
  another review.

## What the reviewer ran

- `make test` (1 min 34 s), `make lint`, `go vet`, `make check-docs`; CI's full guard run on
  the commit (`334 guards, 334 real, 0 failed`) and 10 guards locally, all real.
- Every requirement of the assignment by hand on the Compose stack (create, the idempotent
  repeat and the conflicting key, status, templates, a batch under a key, the batch summary,
  the list with filters and its cursor, cancel and retry with their 409s, `/v1/metrics`,
  health, the WebSocket follow), `make e2e`, `make load`.
- 13 faults on the stack, each counted at the mock provider: a worker killed with a fast and
  with a slow provider; Postgres paused, stopped and made read-only; Redis paused, restarted
  and made read-only; a worker paused ten times and one stopped; a failing provider; a 2 s
  provider; the VM's clock stepped forward by 300 ms.
- 9 bursts with a watcher on the Docker VM's clock (one of 30,000; eight of 15,000 with 8 MB
  batches posted 8 at once); seed 2268 three times quietly and five times with the CPUs
  oversubscribed twice; 103 new fault scenarios (seeds 5000 to 5099 and the three fixed);
  A5 at 24 dispatchers and at a limit of 20; 32 concurrent 8 MB batches; workers pointed at
  an address that answers 400; Postgres paused under 40 concurrent creates.
- It could not check a full local `make guards` (CI ran it), the alert rules on the stack,
  more than one API instance, a real provider, `RATE_LIMIT=20` on the stack, or the 16-million-row
  reads.

## Verdict, as reported

The core holds: the mock provider never saw more than 100 per channel in one second except
when the reviewer stepped the clock on purpose, in 13 fault runs and 9 bursts; nothing was
lost; every duplicate was at-least-once case 1 or 3; 103 new fault scenarios passed. Two major
defects, both next to an earlier fix: session 6's rendering slots sat outside session 6's 5 s
deadline, and an address that answers 400 to everything drained the queue into `failed` with
the breaker closed. Of the two events the record could not explain, seed 2268 fits a process
stall whose place is not shown, and the peak of 130 has a cause the record did not name: the
Docker VM's wall clock, which Redis reads, is stepped forward by tens of milliseconds.

## Findings and resolutions

| # | Severity | Finding | Reproduced | Resolution | Verified by |
|---|---|---|---|---|---|
| F1 | major | The wait for one of the 4 rendering slots ran on the request's own context, before the 5 s database deadline was made. With Postgres paused, 40 concurrent creates were answered four at a time at 5, 10, ... 30 s, and 16 got no answer within the server's 30 s although their rows were written. Session 6's F1 fix (the slots) had brought back the symptom of its F5 fix (the deadline); the deadline's test sent one request at a time. | Yes, on the stack (Postgres paused 60 s and 33 s) | Class: a wait placed before the deadline that should bound it. The design first (`d9f079e`: the deadline covers the slot), the test red (`830cfd6`: 20 creates and batches at once with Postgres stalled were answered at 0.5 to 2.5 s with a 500 ms deadline), then the fix (`b9cb688`): the deadline is made first and the slot is waited for within it; a request with no slot in time is a 500. No other handler waits before its deadline: cancel and retry are queue writes with their own timeout, and the body is read under the server's 30 s bound. On the stack after the fix: Postgres paused 33 s and 60 s, 40 concurrent creates, all 40 answered 500 in 5.00 to 5.03 s, no row written | TestARequestWaitingForASlotEndsAtItsDeadline (the last of 20 answered after 505 ms); guards "api: a create's deadline covers its wait for a slot", "api: a batch's deadline covers its wait for a slot" |
| F2 | major | A provider address that answers 400 to every message drains the queue into `failed` with the breaker closed: 400, 413 and 422 end the row and teach the breaker a success. Section 6 claimed a broken configuration never drains the queue. | Yes, on the stack (1,000 rows `failed` with one attempt each within 15 s) | Not changed in the code; the claim corrected and the trade-off stated (`8f85b3a`). The reviewer's change, a rejection teaching the breaker a failure, would stop this drain at about five rows per worker, but any client's run of messages the provider refuses on their content would then hold the channel, for every client, to one probe per open period per worker. A misconfiguration is an operator's, seen at a deployment, and its rows are requeued by `POST /retry`; a run of bad messages is any client's, at any time. Section 6 says what the drain costs and how the alert shows it; section 16 lists the other breaker as left out. Reproduced by the lead: 1,000 `failed` in 10.5 s; then, with the right address, all 1,000 found through the list (`status=failed`, by cursor) and requeued, 1,000 sent, the provider received 1,000, none twice | Section 6 and 16; the run in session 8's record. Open: see the record |
| F3 | minor | The take script ages permits by Redis's `TIME`, the VM's wall clock; the mock provider stamps arrivals on a monotonic clock. A forward step of the wall clock shortens one window as the provider counts it. The watcher saw steps of 28.4 and 34.7 ms during loads; a 300 ms step made on purpose gave a peak of 200 per channel on all three channels at once, the signature of the unexplained 130/121/125. The record's capture used the same wall clock. | Yes | Documented (`7cc9acb`): section 7's limits table gives the measured steps and their effect; section 11's I1 row names the step as a third cause of the unexplained excess; the record's open list names it | Section 7, 11; the record |
| F4 | minor | The load tool's "writing the body" ended when the host's socket took the bytes, which on Docker Desktop is the port forwarder; it cannot tell a slow upload into Docker from a slow API. One of the reviewer's batches took 7.5 s with a body write of 251 ms, and at least 2.5 s of it were spent before the database work began. | Yes | The tool's label and comment say what the figure is (`fbf194e`); the record no longer reads it as a test of the upload hypothesis; the slow batches stay open | The tool's output; the record |
| F5 | minor | The README's and Compose's example `API_KEYS=key1,key2` puts the API in a restart loop: a key needs at least 16 characters. | Yes | The examples use keys of 20 and 21 characters (`7cc9acb`). Checked on the stack: 401 without a key, 200 with `X-API-Key` and with `Authorization: Bearer` | The run in session 8's record |
| F6 | minor | Nothing bounds how many requests are in flight, each holding its decoded body: 32 concurrent 8 MiB batches were all answered 201 within 1.32 s, the API peaking at 336 MiB, about 10 MiB per batch, linear. | Yes | Documented (`7cc9acb`): section 8 states it, and that since F1 a request waits for its slot at most its deadline; section 16's quota row says how many requests a client may have in flight is the gateway's | Section 8, 16 |
| N1 | nit | Section 9's "after Close only Close's context sends" was slightly too strong: a pipeline already in flight finishes on its own 1 s timeout. | Reasoned | Worded so (`7cc9acb`) | Section 9 |
| N2 | nit | The record said 278 test functions (279 at the review); it linked this review record before it existed; the status tables said "next". | Yes | Updated at the end of the session: 280 test functions | The record; plan and README tables |
| N3 | nit | Experiment 03's scripts were not in the repository. | Yes | Added as `docs/experiments/03-part1.sh` and `03-part2.sh` (`fbf194e`) | The experiment's page |
| N4 | nit | `Get` reads the row and its history in three statements outside one snapshot, so the history can be one attempt ahead of the status. | Reasoned, not reproduced | Not changed. Each statement's answer is true when it is read; the history being one attempt ahead of the status is a race with a result write in flight, and the status endpoint makes no promise of one snapshot. Open in the record | — |

## The two events the record could not explain

- **Seed 2268.** Three quiet runs and five under CPU load passed (the latest arrival 0 to 3 ms
  and 6 to 30 ms after its permit), and 103 new scenarios passed (34,335 requests, peak 40,
  none late). The excess fits the frozen-process row of section 7, but lateness is measured
  inside one process, from a permit's return to the server's stamp: a stall in the recording
  server looks the same as one in the worker, and a stall before the permit returns is not
  counted at all. The record and the README now say that the place of the stall is not shown.
- **The peak of 130.** All 9 of the reviewer's bursts peaked at 100. It stays unexplained,
  with three causes named: a stall of the Docker VM, a stall of the mock provider, or a step of
  the VM's clock. Only the first and the last would hit all three channels at once.

## Earlier fixes, as reported

| Review | Holds? | Defect next to the fix |
|---|---|---|
| 01 | Yes: `/readyz` answered 503 in 2.004 s with Postgres paused; `LEASE=40s` exits 2 | none |
| 02 | Yes: no unexplained second claim over more than 100,000 rows | none |
| 03 | Yes: a read-only Redis made nothing be claimed (two limiter errors, then recovery); A5 at 24 dispatchers 94.8 per second, longest wait 3.117 s; at a limit of 20, 28 refused | the window rests on Redis's wall clock (F3) |
| 04 | Yes: CI shows permit to arrival of at most 3.78 ms | clock steps and CPU load (30 ms) take part of the margin |
| 05 | Yes: Postgres paused, 43 rows held for one lease and delivered once; a read-only Redis claims nothing; a worker killed with a slow provider, 50 accepted twice (case 1) | F2: section 6's broader claim fails for 400 |
| 06 | F2 to F9 hold; F1's fix broke F5 | F1 |
| 07 | Yes where checked (info lines with correlation ids; no tracked binaries; the WebSocket follow); the alerts and the shared figure read were not checked again | none found |
| 08 (`5cd31ed`, the guard CI found) | Yes; both guards real | N1 |

## Claims and reality, as reported, and what changed

| Claim | The reviewer's measurement | Now |
|---|---|---|
| The slots held a single create back at most 63 ms | up to 91 ms in its runs | The README gives both |
| Section 6: a broken configuration does not drain the queue | false for 400, 413, 422 | Corrected (F2) |
| README: "nothing slow can happen between the permit and the request" | 144 ms recorded (seed 2268); 30 ms under CPU load | Reworded: a stall there, or a clock step, is what the margin may not absorb |
| 8 MB runs: at most 245 MiB, accepted in 0.43 to 0.51 s in 19 runs | 123 to 268 MiB; 0.444 to 0.495 s in 7 runs and 7.63 s in one | The README gives the ranges of all 30 runs |
| 30,000: high priority p95 1.14 to 1.19 s, longest 5.1 to 5.2 s | 1.35 to 1.36 s and 5.35 to 5.36 s | The README's ranges include the reviewer's run |
| 30,000: accepted, drained, peak, counts | reproduced: 0.232 s, 1 min 46.5 s, 93.9 per second, peak 100, 30,164 received = accepted = created, none twice | — |
| "Captured by kernel time" | realtime timestamps, which the VM steps | Section 11 says so (F3) |
| The tool now measures the upload | it does not | Corrected (F4) |
| Seed 2268 held "between permits and writes" | the place is not shown | Corrected |
| `API_KEYS=key1,key2` | the API does not start | Corrected (F5) |
| A5 at 24 dispatchers; at a limit of 20 | reproduced (28 refused against 50; the same conclusion) | — |
| The fault test's pass rates | reproduced: 103 of 103 | — |
| 278 tests; experiment 03's scripts | 279; not in the repository | N2, N3 |

## Half-hour readability, as reported

A reader can run the stack and follow the API examples in about five minutes (once F5 was
fixed). The README's reading order began with the process documents; it now begins with
sections 2 to 7 of the architecture and the dispatcher. `architecture.md` is the contract and a
history at once, and some table cells run to a page (the I1 row); the code is the easy part (a
29-line loop, small commented files). Missing: the walkthrough, a map from each requirement
to its file, and a short "not done" list in the README; these are session 9's (plan, "9.
Walkthrough and final documents").

## Hard questions, and the answers

1. *A 40 s stall with 40 clients posting: who gets a 500, who nothing, which rows exist?*
   After F1, every one of the 40 gets a 500 within its 5 s deadline; on the stack, with
   Postgres paused 33 s and 60 s, all 40 were answered 500 in 5.00 to 5.03 s and no row was
   written. A write cut off at its deadline may still commit when Postgres resumes (section 8);
   a client that retries with an idempotency key gets the stored row or a new one, never two.
2. *Why does a 400 teach the breaker "healthy"? How would an operator requeue 100,000 rows?*
   Because the verdict table reads 400, 413 and 422 as the message's fault, and the other
   reading lets one client's bad messages hold the channel for all (section 6, F2). The rows
   are found with `GET /v1/notifications?status=failed` and its cursor and requeued with
   `POST /retry`, one by one; done for 1,000 rows on the stack. A bulk requeue would be a new
   endpoint; it is not built.
3. *How large a forward step does the margin absorb? How often did the clock step during the
   runs behind the README's numbers?* A step of `s` shortens one window by `s`; the margin of
   50 ms absorbs the delay from permit to arrival plus any step within one window, so steps of
   28 to 35 ms, as measured, take most of it. How often the clock stepped during the author's
   runs was not recorded; the reviewer's watcher saw a step about every 30 s under load.
4. *What in `tools/load` separates a slow upload into Docker from a slow API?* Nothing; the
   tool now says so (F4). The two slow batches stay open.
5. *What puts seed 2268's stall between the permit and the write?* Nothing; the record now
   says the place is not shown.
6. *Why may a request hold its decoded body as long as its client waits for a slot?* Since F1
   it may not: the wait ends at the request's 5 s deadline. How many such requests may be in
   flight is not bounded (F6), which is the gateway's quota (section 16).
7. *If a 30% excess recurs on a real deployment, what shows the cause?* The provider's 429s
   (counted as `error_response` attempts) are the backstop of section 7; the attempts' stored
   start times and the provider's own arrival log, compared on one clock, would show late
   requests; a clock step shows in the host's time-keeping log. On a server whose clock is slewed
   rather than stepped, the third cause does not apply.
