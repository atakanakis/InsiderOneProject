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
  rebuild, because the first version failed it; the session records and the walkthrough of
  session 9 exist for that.
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

### Session 4 and later

Recorded here as each session finishes.

## 5. What is honest to say about time

The assignment suggests about 12 hours. The review rounds on the first version and this
rebuild are work beyond that, which I chose to do. The history of both repositories shows the
work as it happened. I would rather show how it really went, including the version that did
not hold up, than present a clean result with the history removed.
