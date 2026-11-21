# The journey

This repository is the second build of the same assignment. This page says why there are two,
what happened in the first, and how the second is being built. It is updated at the end of
every session.

## 1. The first version

The first version is public and unchanged at
[InsiderOneProjectDeneme](https://github.com/atakanakis/InsiderOneProjectDeneme) ("deneme" is
Turkish for "trial").

It was designed and built with an AI coding agent in one long session, and it meets the
assignment: an API, a Postgres queue, workers, a rate limit shared through Redis, retries, a
circuit breaker, templates, scheduling, WebSocket updates, tracing, CI.

After it worked, I set up a review loop. A reviewer agent on a different model read the
repository with no knowledge of how it was built, had to reproduce everything it reported,
and could not edit anything. Each round used a fresh reviewer, which also re-checked the
previous round's fixes. There were five rounds. The records of the first four are in that
repository under `docs/reviews/`.

What the rounds found, in short:

| Round | The most serious findings |
|---|---|
| 1 | The rate limit could be exceeded after a database stall. A template in a batch could make the server allocate gigabytes. |
| 2 | Two of three major findings were inside the fixes for round 1: the claim had lost atomicity, and the template fix covered one item and not the batch. A failed result write caused a second send. |
| 3 | With no fault at all, slow new connections let the provider see twice the limit. The fix of the fix had a new failure mode (one failing priority blocked the others). A 99 KB request now produced a 68 MB error message. |
| 4 | A commit message claimed a cancelled request "is not written". It sometimes is. |
| 5 | Two fixes had no test that noticed their removal. A confirmed delivery could be discarded. Two alert rules could never fire. |

By the end the system was measurably solid: 203 randomised fault scenarios with 60,900
notifications, none lost, none delivered twice, the limit never exceeded. It was also a system
whose core had grown by one guard per round, where each fix had been the next round's first
suspect, and whose documents had more than once claimed more than its tests proved.

The fifth round's findings were not patched. They became input for this rebuild.

## 2. The decision to rebuild

The defects were different each round, and the cause was the same each time: the design had
never been written down and checked as a whole. Guarantees were added to the code one
reproduction at a time. A sixth round of patches would have produced a seventh.

So the first version was renamed as the trial, its lessons were written down
([lessons.md](lessons.md)), and the build started again with a different order of work:

| First version | This version |
|---|---|
| Code first, review after | Design first; the design is reviewed before code exists |
| One long session | One session per part, each with a brief and a definition of done |
| Reviews found the invariants | The invariants are written first, and the tests for them before the code |
| "The tests pass" | Each guard is disabled on purpose to see that a named test fails |
| Claims in documents | Claims with the measurement or test next to them; assumptions labelled as assumptions |

## 3. How the work is divided

- **I** decide scope and trade-offs and read every session's record. The sessions are started
  one after another by a coordinating agent, each in a new conversation, and each reports back
  when it is done. Being able to explain the core without the agent is a requirement of this
  rebuild, because the first version failed it; the session records and the
  [walkthrough](walkthrough.md) exist for that.
- **The lead agent** of a session holds to the architecture, writes or directs the code, and
  checks claims against the repository. The design session (session 0) was led on Claude
  Fable. The build sessions are led on Claude Opus, each in a new conversation.
- **Builder subagents** (Claude Opus) implement well-specified pieces.
- **Reviewer subagents** (Claude Opus, a fresh instance each time, read-only, no knowledge of
  the author's reasoning) review each session. Agent definitions are in
  [`.claude/agents/`](../.claude/agents/); the rules every session follows are in
  [`CLAUDE.md`](../CLAUDE.md).

A fresh context per session and a fresh reviewer per review are used for the same reason: an
agent that has followed the author's reasoning tends to agree with it.

## 4. Session log

### Session 0: lessons, architecture, design review (2026-11-20)

No code. The products are [lessons.md](lessons.md), [architecture.md](architecture.md),
[plan.md](plan.md) and the [design review record](reviews/00-design-review.md).

- **One assumption was measured before the design depended on it.** The first version had
  claimed that cancelling a request stops it from being written. A short experiment showed
  that cancelling alone let 13 of 40,000 refused requests reach the server, and that closing
  the connection first let none through ([experiment 01](experiments/01-gate-refusal.md)).
- **The first draft of the design was wrong in four places**, and an independent reviewer
  found all four on paper, two of them by experiment. Three were in parts I had added to make
  the design simpler than the first version. One example: a "promoter" that moved waiting
  rows into a ready queue would have delivered a high-priority retry 17 minutes late behind a
  million scheduled rows.
- **The response was to remove parts.** The promoter is gone. The rule that gave an attempt
  back when nothing was sent is gone; the attempt counter now only goes up. Where the reviewer
  proposed a new mechanism, the design looked for a part to delete first.
- **A second, fresh reviewer** checked the revised design. It confirmed the queue by
  measurement (the claim query on 2.8 million rows), and found four further problems, all about
  misconfiguration and half-failures around the system: a wrong provider URL, a redirect, a
  Redis that can read and cannot write, an undefined breaker probe. Each was a change of a
  sentence in the design, and each is now a named test in the plan.

What it cost and what it bought: two design reviews and three drafts, with no code written.
Every one of the eight major findings would have been a defect and then a patch in the first
version's way of working.

### Session 1: skeleton (2026-11-20)

The module, configuration, schema, health endpoints, worker and mock provider, the Compose
stack and CI. Record: [sessions/01-skeleton.md](sessions/01-skeleton.md).

- **Every guard was disabled on purpose.** `make guards` now does it as a command: for each
  of 53 guards it breaks the line, checks that the package still compiles, and expects a
  named test to fail. CI runs it on every push. A rule in a document became a check.
- **The timing budget is enforced.** A worker whose lease is shorter than the send timeout
  plus 30 s could send a message twice; it now refuses to start and names the relation.
- **The one review found a major defect in a place the tests looked at but could not see.**
  The readiness test used a fake dependency that behaved well; the real Redis client ignored
  the deadline, and `/readyz` took 5 s instead of 2 s. The fix was a class fix: one place
  builds every Redis client, and a test forbids any other. The same property guards the
  rate limiter's wait in session 3. The lesson is the first version's again: test the real
  part where its effect happens.
- **The review also changed the design once.** The API had been made unready when Redis was
  down, although it needs only Postgres to accept requests. The design was changed first,
  then the code.
- **One process change.** From this session on, each session has one review, run once after
  the code is green; its fixes are proven by tests and the guard check, not by another review.

### Session 2: queue (2026-11-20)

The five queue operations on Postgres: claim, finish, reap, cancel, requeue. Record:
[sessions/02-queue.md](sessions/02-queue.md).

- **The tests came first, and the history shows it.** The 25 tests were committed against a
  stub that compiled and returned errors; 24 were red. The code came in the next commit.
- **A design decision became a permanent measurement.** The design review had chosen the
  `id = ANY(ARRAY(...))` form of the claim over the common `id IN (SELECT ...)`, from one
  experiment. A test now reads the query plan on every run, under the plan a cached statement
  can switch to: 4 index pages and 0.13 to 0.31 ms on 300,000 rows. With the common form the
  same test sees a full table scan and 32.5 ms, and `make guards` checks that it does.
- **The review found a guard that guarded too much.** The guard for `SKIP LOCKED` removed the
  whole `FOR UPDATE SKIP LOCKED`, which breaks the lock and so failed the right test for the
  wrong half. Plain `FOR UPDATE` passed everything while claims waited behind each other. The
  lesson is about guards themselves: a mutation must disable only the line it names.
- **A bound without a test at its edge had an off-by-one.** An error text of exactly 1,024
  bytes made the result write panic, the very failure the bound was there to prevent.
- **The guard check got faster.** Postgres and Redis now start once per `make guards` run, not
  twice per guard; a database guard takes about 3.5 s instead of 10 s.

### Session 3: rate limiter (2026-11-20)

The shared limit of 100 per second per channel, as a sliding window in Redis: `Available` and
`Wait`. Record: [sessions/03-ratelimit.md](sessions/03-ratelimit.md).

- **The design was checked before it was written down.** Each claim the new §7 makes about
  Redis (a read-only replica, a full memory, a write inside a read-only script, how Lua prints
  a large number) was tried on the real Redis image first. One of them, Lua printing a
  microsecond time with 14 digits, decided how the scripts format numbers.
- **A guard check found a test that could not fail.** Every caller in the concurrency test
  waited for the limiter's own "come back in N ms" hint, so a bug that freed permits 100 ms
  early went unseen: nobody asked during those 100 ms. Real sends arrive at any moment. With
  half the callers arriving at random, the test catches a window that is 2 ms short.
- **The review found problems in how later sessions will use the limiter**, not in the
  limit itself. "No permit in time" and "Redis hangs" looked the same to a caller, though the
  design treats them differently; workers with different settings could break the limit
  together; and waiters are served in no order, so a figure in the design ("about 35
  workers") was optimistic. The first two are fixed in code with tests and guards; the third
  is now an assumption that session 5 measures, instead of a claim.

### Session 4: provider client and the send gate (2026-11-20)

The HTTP client that delivers one notification, with the rate-limit permit taken when the
connection is ready and before the request is written. Record:
[sessions/04-provider.md](sessions/04-provider.md).

- **A design detail was removed by measurement.** The design said: refuse a send by closing
  its connection, then cancelling the request. Through the real client, the close alone let 0
  of 20,000 refused sends through; the cancel added nothing, so it went, before any code was
  committed.
- **A rare event got a second, exact test.** A refused send that only cancels leaks a few
  times in ten thousand, so a test that counts leaks is a weak guard. A second test checks
  the order itself: when the client's hook returns, the connection is already closed.
- **A guard must fail for the reason it names.** Two guards turned their tests red by
  accident: a gate moved after the write let the response arrive while the gate still ran,
  and the test read the gate's results too early. Both tests were fixed to fail for the
  reason the guard names.
- **CI caught a test that trusted a timing assumption.** Under the race detector on the CI
  runner, the provider saw 106 requests in one second. The delay between permit and arrival
  passed the 50 ms margin, a case the design already lists. The test now checks the
  guarantee in the form the design states it.
- **The review found a response that escapes the deadline.** To a `101 Switching Protocols`,
  Go hands over the raw connection and stops applying the request's deadline; the client
  waited as long as the provider kept the connection open. It no longer reads that body.

### Session 5: dispatcher, breaker, reaper; fault tests (2026-11-20)

The loop that joins the queue, the rate limiter and the provider client, with its circuit
breaker and reaper. This is the part of the first version that received the most patches.
Record: [sessions/05-dispatcher.md](sessions/05-dispatcher.md).

- **The test that checks the invariants came first.** The randomised fault test was written
  and committed against a stub, before the dispatcher, as lesson 15 asks. It runs two or three
  workers through the real provider client and checks, at the provider's side, the limit, that
  nothing is lost and nothing is accepted twice.
- **The loop stayed small.** 29 lines, the shape the design drew before any code. Nothing is
  reserved ahead: the breaker's probe is marked when a row really starts and cleared at its
  single exit, so the seven hand-back paths of the first version have no counterpart.
- **A long run found a duplicate, and the design already named it.** One scenario of 53 saw
  four notifications accepted twice. Each had an attempt that ended with no response: the
  test's own fault had closed a connection after the provider read the request. That is the
  third at-least-once case of the design; the test now checks the guarantee as written, and
  still fails on any other duplicate.
- **A measurement replaced two models.** The design review and session 3's reviewer had
  estimated waits of 2 to 3 s at the gate with 12 workers. The real dispatchers waited about
  one window (1.05 s) with no refusal, because the limiter's hint stops all claims while the
  window is full.
- **The margin question was answered with numbers.** Session 4's CI saw 106 in one second
  under the race detector. Through the dispatchers the delay from permit to arrival was at
  most 2.3 ms without the race detector and 28.6 ms with it, against a margin of 50 ms. The
  margin stays; a starved CPU is the case the design lists, and no margin bounds it.
- **Six guards proved nothing on their first run,** each a fault of the guard: mutations that
  did not compile, one that changed nothing, one that hung its test, one that mutated a line
  its test never reaches. Each was fixed until its named test turned red.
- **The review found the breaker's limits.** An address that accepts and closes connections
  looked like an idle connection to Go and drained the queue with the breaker closed; it now
  counts only on a reused connection. A Redis that refused writes let rows end `failed` with no
  send; the fix removed a part: the limiter's read now fails exactly when its write would, so
  nothing is claimed. With little traffic a per-worker breaker cannot save every row from a
  wrong URL, and the design now says so instead of implying it.
- **CI found three tests that trusted the machine.** Under the race detector on a busy runner a
  lateness count, a deadline test and a test that read a pipe while waiting all failed. Each was
  fixed as a class, and the timing guarantees are checked without the race detector, where CI
  also measures them.

### Session 6: the API (2026-11-20)

The front door: create, batch, status, list, cancel, retry, templates, API keys, OpenAPI.
Record: [sessions/06-api.md](sessions/06-api.md).

- **The first version's worst API defect was the starting point.** A 99 KB request there
  produced a 68 MB error, because an error message quoted the value it refused. Here every
  input and every output has a bound in the design before any code, with a test at the bound,
  one under and one over; error texts are fixed and a name from the request passes one
  function that keeps 64 bytes. The largest error the form allows measured 28,494 bytes.
- **Bounds on what the server allocates, not only on what it accepts.** 8 MiB of `{}` is 2.8
  million batch items; the batch is decoded one item at a time and refused at the 1,001st,
  with under 1 MiB allocated. A template's rendered length is calculated from its compiled
  parts before anything is rendered, so a 100 KB batch that would render 384 MB is refused
  with 4 MiB allocated. The tests read the process's allocation counter.
- **Idempotency is one statement, not a check and a write.** `INSERT ... ON CONFLICT DO
  NOTHING`, then the stored request hash decides between "the same request" and a conflict. 20
  identical requests at once made one row; 10 identical batches, one batch with the same ids
  in the same order.
- **A part was removed from the design before it was built.** The design listed a service
  layer; every use case turned out to be "validate, then one statement", so it went.
- **The guards found weak tests, and a full disk.** Every API test passed on its first run.
  The guard run then showed three guards that proved nothing: two tests that did not test the
  line they named, and a mutation that did not compile. A second cleaning step in the error
  writer could not be reached by any request, so no guard could prove it, and it was removed.
  The machine's disk filled during the run (22 GB of Go build cache), which stopped Docker;
  the cache was cleared and the run repeated.
- **The review found what bounds per request do not cover.** The reviewer, with no time
  limit and the stack running, confirmed idempotency under 200 concurrent requests and every
  bound it probed, then found three classes the tests had missed: nothing bounded rendering
  across requests (40 small requests took the API to 1.1 GiB), a send's error still quoted the
  provider URL's secret path and the API now showed it (session 5 had fixed two instances of
  that class, not the class), and the list cursor was a filter rather than an index bound
  (300 ms per page at 1.9 million rows). Each was answered in the design first, then with a
  red test: 4 rendering slots and an 8 MiB rendered bound (745 MiB became 144 to 199 MiB), the
  URL dropped where an error leaves the provider client, and one redundant condition that
  turned 14 ms into 0.02 ms.

### Session 7: events and observability (2026-11-20 to 2026-11-21)

Live status over a WebSocket, Prometheus metrics, OpenTelemetry traces, alert rules.
Record: [sessions/07-events.md](sessions/07-events.md).

- **The first version's two alert rules that could never fire were the starting point.**
  Here every rule has a promtool case in which it fires and one in which it does not, and a
  test builds the real API and worker, reads their `/metrics` and fails when a rule names a
  metric, a label value or a grouping label that neither exports. CI runs the same check on
  the running stack. Stopping Redis on that stack fired the rate-limit alert after 150 s.
- **The review showed what those checks do not cover.** A rule can fire on the input its test
  gives it and never on the input its failure produces: a read-only Postgres fails result
  writes for one lease and then claims, so the rule on result writes went pending and
  resolved without firing for 10.5 minutes; a stopped API took its own figures with it; an
  idle half-open breaker paged. The rules were rewritten on the shapes seen on the stack and
  run there until each fired: the database rule at 135 s, the API rule at 150 s, the worker
  rule at 165 s. Two binaries had also been committed by `git add -A`; CI now refuses tracked
  build output.
- **A lost event must never cost a delivery, and that was tested before it was built.**
  Against a Redis that accepts connections and never answers, a publisher that waited let 30
  of 300 rows through in 15 s. The publisher now puts the event in a bounded buffer and
  returns; one goroutine sends it. The same 300 rows went at the limit. It became invariant
  I13.
- **Every bound of the WebSocket has a test, and building one found a defect in the
  library's use.** A frame that announced 1 GiB and sent 100 bytes held its handler, because
  the close handshake reads the rest of the message first. A data message now closes the
  connection at once. A client that does not read is let go when 256 messages wait, and
  1,000,000 events to it did not grow the heap.
- **The figures were checked against reality.** After 1502 notifications on the Compose
  stack, the workers' counters, `GET /v1/metrics` and the mock provider all said 1502; CI
  checks it on every push. One trace shows an API request and the worker's deliveries
  minutes later.
- **"A line per try" was a class, not one place.** Session 5's open item, 3,200 warnings in
  75 s from a read-only Postgres, became a counter and one line per 10 s. Stopping Redis then
  showed the same class in go-redis's own output and in readiness probes, fixed the same way.
- **The guards found ten that proved nothing.** Nine mutations did not compile; one removed a
  write deadline and no test failed, because the test never made the write block. Each was
  fixed until its named test turned red, and a fast test now finds a guard whose text a
  change left behind.

### Session 8: hardening (2026-11-21)

No new feature: the system was measured under a burst and under faults, every guard was run,
and the whole system was reviewed once more. Record: [sessions/08-hardening.md](sessions/08-hardening.md).

- **The numbers came from the outside.** A load tool posts 30,000 notifications to the running
  stack and counts at the mock provider, which now remembers what it accepted: the peak stayed
  at 100 per channel in one second, nothing was lost, nothing was accepted twice, and the
  backlog drained at 93.5 to 93.9 per second per channel. Two long runs of the randomised fault
  test, 606 scenarios and 181,800 notifications, lost nothing.
- **What did not fit was written down as it was.** One fault scenario in 606 passed the limit
  (47 against 40), with ten requests late after their permits by its own stamps; one burst in
  thirty saw 130 at the provider and never again. The first fits a case the design names; the
  second has three possible causes and none is shown. Both are in the record as open, not as
  passes, and the review found a third possible cause neither the tests nor I had named: the
  Docker VM's clock, which Redis reads, steps forward by tens of milliseconds.
- **CI found a guard that proved nothing on a fast machine.** A test of the event publisher's
  shutdown passed with the code it guarded removed, because the sender had already sent
  everything; the code had the same dependence on timing. Both were fixed as a class.
- **The review found two defects next to earlier fixes**, the first version's pattern. Session
  6 had fixed two problems with two parts: a 5 s deadline for the database, and 4 slots for
  rendering. The slots were waited for outside the deadline, so with Postgres stalled the
  deadline stopped holding: answers came at 5, 10, ... 30 s, then not at all, with rows
  written. Fixed and guarded; on the stack, 40 requests during a 60 s stall now all get a 500
  in 5 s. The second: an address that answers 400 to everything drains the queue into
  `failed`, which the design had claimed could not happen. That one is kept, and the claim
  corrected, because the other choice lets one client's bad messages hold the channel for all;
  the trade-off is written next to it, and requeueing 1,000 such rows was done and measured.
- **Questions left open by earlier sessions were answered with measurements:** the dial
  timeout has a test; the wait at the gate follows the design review's old estimate above 12
  dispatchers, which gives how many dispatchers a channel can take before refusals start (about
  40 at the defaults, 8 at a limit of 20); `GET /v1/metrics` over an hour reaches its deadline
  at about 16 million notifications, because nothing removes old rows.

### Session 9: walkthrough and final documents (2026-11-22)

No new feature. A reader can now follow one notification through the code, and the README
says only what the repository shows. Record: [sessions/09-walkthrough.md](sessions/09-walkthrough.md).

- **The walkthrough was written from the code, then held to it by a check.**
  [walkthrough.md](walkthrough.md) follows one SMS through 20 steps and six branches, each
  with its file, its functions, the invariant it keeps and the tests that prove it. Two of the
  first draft's figures were wrong against their tests: they were taken from the documents
  instead of read from the test. A new check, `tools/docrefs`, now fails `make check-docs`
  when a link, a path or a function name of the documents does not exist, so the walkthrough
  cannot drift from the code unnoticed. Lesson 9 again: a document is a claim, and a claim
  needs a check.
- **The README became a way in.** What it is, one command, where to read, the API examples with
  their real answers, and a map from every requirement of the assignment to the code and the
  tests that meet it. The long account of the measurements moved to its own page, and one of
  its figures turned out to be wrong against its source (7.6 s for 7.5 s).
- **A rehearsal from a clean clone found seven gaps in a README that had been read many
  times.** Answers whose status the command did not print, a repeat that was described but not
  given, a failing provider with no way back, a check that cannot pass against a failing
  provider, a warning on the first start, a status read too soon, an exit code. Every command
  worked; what was missing was what a reader needs to know and the author no longer sees. Each
  was fixed and the rehearsal run again from a new clone.

## 5. Where it ended

The system meets the assignment, its bonuses included; the README maps each requirement to its
code and tests. What held under the measurements: the rate limit at the provider, with two
exceptions, one burst of 30 and one fault scenario of 606, both recorded with their possible
causes; no notification lost; no notification accepted twice except in the at-least-once cases
the design names.

What did not end clean, and is written down as such: three events of session 8 that did not
repeat and are not explained; a provider address that answers 400 to everything drains the
queue into `failed`, kept as a trade-off and measured; tables that grow without a retention
policy; and the parts that were never measured here: a real provider, more than one API
instance, a server whose clock does not step. Two binaries committed in session 7 remain in the
public history.

The process made mistakes too, and they are part of the story: guards that proved nothing
until they were made to fail, a fix that broke an earlier fix (session 8), checks whose reach
did not match their class. Each was found by a test that failed, a guard run, CI, a reviewer
or a rehearsal, not by rereading.

## 6. What is honest to say about time

The assignment suggests about 12 hours. The review rounds on the first version and this
rebuild are work beyond that, which I chose to do. The history of both repositories shows the
work as it happened. I would rather show how it really went, including the version that did
not hold up, than present a clean result with the history removed.
