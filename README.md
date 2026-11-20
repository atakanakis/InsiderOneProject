# Event-Driven Notification System

A service that accepts notification requests (SMS, email, push) over an HTTP API and delivers
them to an external provider, with a rate limit of 100 messages per second per channel,
priorities, retries and live status tracking. Written in Go, with PostgreSQL and Redis.

> **Status: build in progress.** The skeleton runs: the stack starts, the API answers its
> health endpoints, the worker starts and stops, and the mock provider records what it
> receives. The queue's five operations exist and are tested on Postgres; nothing is
> delivered yet, because the dispatcher that uses them comes in session 5. This README says only what exists; the table
> below is updated at the end of every session.

| # | Session | Status |
|---|---|---|
| 0 | Lessons, architecture, design review, plan | done |
| 1 | Skeleton: module, config, migrations, Compose, CI, health | done ([record](docs/sessions/01-skeleton.md)) |
| 2 | Queue: the five operations | done ([record](docs/sessions/02-queue.md)) |
| 3 | Rate limiter | next |
| 4 | Provider client and the send gate | |
| 5 | Dispatcher, breaker, reaper; fault tests | |
| 6 | API | |
| 7 | Events, WebSocket, metrics, tracing, alerts | |
| 8 | Hardening: independent review, soak, load test | |
| 9 | Walkthrough and final documents | |

## Running what exists

Needs Docker and Go 1.27.

```sh
make up          # Postgres, Redis, migrations, API, worker, mock provider
curl localhost:8080/healthz    # {"status":"ok"}
curl localhost:8080/readyz     # {"postgres":"ok","redis":"ok","status":"ready"}
curl localhost:9090/stats      # what the mock provider has received
make down        # stop and delete the data
```

| Command | What it does |
|---|---|
| `make test` | Every test with the race detector; integration tests start Postgres and Redis in Docker |
| `make test-unit` | Only the tests that need no Docker (`-short`) |
| `make lint` | golangci-lint v2.15.0, in Docker |
| `make guards` | Disables each guard in turn and checks that its named test fails ([`tools/guards`](tools/guards/list.go)) |
| `make check-docs` | Checks that every test named in `docs/` and this README exists |

Settings are environment variables, listed with their defaults and relations in
[architecture §13](docs/architecture.md#13-timing-budget). The worker refuses to start when a
relation does not hold, for example
`docker compose run --rm --no-deps -e LEASE=40s worker` exits with status 2.

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
| [docs/sessions/](docs/sessions/) | What each build session built, measured and changed |
| [docs/reviews/](docs/reviews/) | Every independent review, with each finding's resolution |

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
  subagents. Each session has one review, run once after its code is green, by a fresh
  instance that has not seen the author's reasoning, cannot edit, and has to reproduce what it
  reports. Its fixes are verified by tests and by `make guards`.
- **Evidence.** A session is done when each of its guards has been disabled on purpose and a
  named test failed. Numbers in documents have the command next to them. Assumptions are
  listed as assumptions until measured.

Definitions: [`CLAUDE.md`](CLAUDE.md), [`.claude/agents/`](.claude/agents/).
