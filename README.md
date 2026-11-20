# Event-Driven Notification System

A service that accepts notification requests (SMS, email, push) over an HTTP API and delivers
them to an external provider, with a rate limit of 100 messages per second per channel,
priorities, retries and live status tracking. Written in Go, with PostgreSQL and Redis.

> **Status: design complete, build in progress.** Nothing runs yet. This repository is being
> built in sessions, design first, and this README says only what exists. The table below is
> updated at the end of every session.

| # | Session | Status |
|---|---|---|
| 0 | Lessons, architecture, design review, plan | done |
| 1 | Skeleton: module, config, migrations, Compose, CI, health | next |
| 2 | Queue | |
| 3 | Rate limiter | |
| 4 | Provider client and the send gate | |
| 5 | Dispatcher, breaker, reaper; fault tests | |
| 6 | API | |
| 7 | Events, WebSocket, metrics, tracing, alerts | |
| 8 | Hardening: independent review, soak, load test | |
| 9 | Walkthrough and final documents | |

## Why this is a second build

The first version of this project is public at
[InsiderOneProjectDeneme](https://github.com/atakanakis/InsiderOneProjectDeneme). It works, and
five rounds of independent review each found real defects in it, several of them inside the
fixes of the round before. The cause was the same each time: the design had never been written
down and checked as a whole.

This repository starts from what that taught. The order of work is reversed: the design was
written and reviewed before any code, the invariants are written before the tests, and the
tests before the code. The full account is in [the journey](docs/journey.md).

## Read in this order

| Document | What it answers |
|---|---|
| [docs/journey.md](docs/journey.md) | What happened in the first version, why it was rebuilt, how the work is divided between me and the agents |
| [docs/lessons.md](docs/lessons.md) | The classes of mistakes in the first version, and what each one changed here |
| [docs/architecture.md](docs/architecture.md) | The design: parts, queue, dispatcher, rate limit, invariants, assumptions, timing |
| [docs/reviews/00-design-review.md](docs/reviews/00-design-review.md) | Two independent reviews of the design before any code, and what they changed |
| [docs/experiments/01-gate-refusal.md](docs/experiments/01-gate-refusal.md) | A measurement the design depends on |
| [docs/plan.md](docs/plan.md) | The sessions, each with its brief and its definition of done |

## The design in five sentences

1. The API validates a request and writes rows to Postgres; it never sends anything itself.
2. Postgres is both the record and the queue: a worker claims due rows with
   `FOR UPDATE SKIP LOCKED`, one priority at a time, highest first.
3. The worker takes a rate-limit permit from Redis at the last moment, when the HTTP
   connection is ready, so that nothing slow can happen between the permit and the request.
4. Every claimed row ends in exactly one place, where the result is classified from what was
   observed and written back; a worker that dies loses nothing, because its rows return when
   their lease expires.
5. Delivery is at-least-once, and the cases in which a message can reach the provider twice
   are listed in the architecture.

## How it is built

- **Sessions.** One session builds one part from a written brief, with a fresh context.
- **Agents.** The design was written by a lead agent on Claude Fable and reviewed by Claude
  Opus. Each build session is led by an agent on Claude Opus, with Opus builder and reviewer
  subagents. A reviewer is always a fresh instance that has
  not seen the author's reasoning, cannot edit, and has to reproduce what it reports.
- **Evidence.** A session is done when each of its guards has been disabled on purpose and a
  named test failed. Numbers in documents have the command next to them. Assumptions are
  listed as assumptions until measured.

Definitions: [`CLAUDE.md`](CLAUDE.md), [`.claude/agents/`](.claude/agents/).
