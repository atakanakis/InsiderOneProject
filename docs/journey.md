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

- **I** decide scope and trade-offs, read every session's result, and start the next session
  only when I can explain the last one. Being able to explain the core without the agent is a
  requirement of this rebuild, because the first version failed it.
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

### Session 1 and later

Recorded here as each session finishes.

## 5. What is honest to say about time

The assignment suggests about 12 hours. The review rounds on the first version and this
rebuild are work beyond that, which I chose to do. The history of both repositories shows the
work as it happened. I would rather show how it really went, including the version that did
not hold up, than present a clean result with the history removed.
