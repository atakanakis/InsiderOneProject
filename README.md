# Event-Driven Notification System

A service that accepts notification requests (SMS, email, push) over an HTTP API and delivers
them to an external provider, with a rate limit of 100 messages per second per channel,
priorities, retries and live status tracking. Written in Go, with PostgreSQL and Redis.

> **Status: build in progress.** The stack starts with an API and two workers, and a
> notification sent to the API is delivered: the API (validation, idempotency, templates,
> bounds on every input and output), the queue (Postgres), the rate limiter (Redis), the
> provider client with its send gate, and the dispatcher with its circuit breaker and reaper
> are built and tested, the last under random faults. Status changes reach WebSocket clients
> through Redis; Prometheus metrics, OpenTelemetry traces and ten alert rules, each with a
> promtool test, watch it. Session 8 hardens and measures it under load. This README says only
> what exists; the table below is updated at the end of every session.

| # | Session | Status |
|---|---|---|
| 0 | Lessons, architecture, design review, plan | done |
| 1 | Skeleton: module, config, migrations, Compose, CI, health | done ([record](docs/sessions/01-skeleton.md)) |
| 2 | Queue: the five operations | done ([record](docs/sessions/02-queue.md)) |
| 3 | Rate limiter | done ([record](docs/sessions/03-ratelimit.md)) |
| 4 | Provider client and the send gate | done ([record](docs/sessions/04-provider.md)) |
| 5 | Dispatcher, breaker, reaper; fault tests | done ([record](docs/sessions/05-dispatcher.md)) |
| 6 | API | done ([record](docs/sessions/06-api.md)) |
| 7 | Events, WebSocket, metrics, tracing, alerts | done ([record](docs/sessions/07-events.md)) |
| 8 | Hardening: independent review, soak, load test | next |
| 9 | Walkthrough and final documents | |

## Running what exists

Needs Docker and Go 1.27.

```sh
make up          # Postgres, Redis, migrations, API, two workers, mock provider
curl localhost:8080/readyz     # {"postgres":"ok","redis":"ok","status":"ready"}

# One notification. 201 with the notification; the same request with the same key again is a
# 200 with the same one, and the key with another request is a 409.
curl -s localhost:8080/v1/notifications -d '{"recipient":"+905551234567","channel":"sms",
  "content":"Your code is 4242","priority":"high","idempotencyKey":"order-42"}'
curl -s localhost:8080/v1/notifications/<id>     # status, with the attempt history

# A template, and a batch of up to 1000 that uses it, retried safely under one key.
curl -s localhost:8080/v1/templates -d '{"name":"welcome","channel":"email","body":"Hi {{.name}}!"}'
curl -s localhost:8080/v1/notifications/batch -H 'Idempotency-Key: signup-2026-11-20' -d '{"notifications":[
  {"recipient":"ada@example.com","channel":"email","template":"welcome","variables":{"name":"Ada"}},
  {"recipient":"+905551234567","channel":"sms","content":"later","scheduledAt":"2030-01-01T09:00:00Z"}]}'
curl -s localhost:8080/v1/batches/<batchId>      # a count per status
curl -s 'localhost:8080/v1/notifications?status=sent&channel=email&limit=100'   # pages: &cursor=<nextCursor>
curl -s -X POST localhost:8080/v1/notifications/<id>/cancel   # only scheduled, pending, retrying
curl -s -X POST localhost:8080/v1/notifications/<id>/retry    # only failed; MAX_ATTEMPTS more attempts

# Live status over a WebSocket: the current state first, then every change. Up to 100
# notificationId and 10 batchId; the client sends nothing. Any WebSocket client works, e.g.
# websocat 'ws://localhost:8080/v1/ws?notificationId=<id>&batchId=<batchId>'
#   {"type":"status","id":"...","status":"scheduled"}
#   {"type":"batch","batchId":"...","total":2,"counts":{"pending":1,"scheduled":1,...}}
#   {"type":"status","id":"...","batchId":"...","status":"processing"}
#   {"type":"status","id":"...","batchId":"...","status":"sent"}

curl -s 'localhost:8080/v1/metrics?window=15m'  # per channel: queue, attempts by outcome, share delivered, p50/p95/p99
curl -s localhost:8080/metrics | grep '^notifications_'   # Prometheus; each worker serves its own on :9100

make e2e N=1000                # follows one over the WebSocket, then 1000 per channel through the API
make status                    # notifications by status, straight from Postgres
curl localhost:9090/stats      # what the mock provider received; at most 100 per channel in any second
make down        # stop and delete the data
```

The specification is at <http://localhost:8080/openapi.yaml>, with a viewer at
<http://localhost:8080/docs>. Started with `API_KEYS=key1,key2 make up` (or the variable in a
`.env` file), the API asks every `/v1` request for one of the keys, in
`Authorization: Bearer <key>` or `X-API-Key: <key>`.

`make up-observability` adds Prometheus (<http://localhost:9091>, scraping the API and every
worker, with the alert rules of [`deploy/prometheus`](deploy/prometheus/alerts.yml)) and
Jaeger (<http://localhost:16686>), and turns tracing on: one trace shows an API request and
the worker's deliveries of the notifications it created. Without it tracing is off and costs
nothing.

The mock provider can fail on purpose, to watch retries and the circuit breaker:

```sh
MOCK_FAIL_RATE=0.2 MOCK_THROTTLE_RATE=0.05 MOCK_LATENCY=30ms docker compose up -d mockprovider
```

| Command | What it does |
|---|---|
| `make test` | Every test with the race detector; integration tests start Postgres and Redis in Docker |
| `make test-unit` | Only the tests that need no Docker (`-short`) |
| `make lint` | golangci-lint v2.15.0, in Docker |
| `make guards` | Disables each guard in turn and checks that its named test fails ([`tools/guards`](tools/guards/list.go)) |
| `make check-docs` | Checks that every test named in `docs/` and this README exists |
| `make alerts` | `promtool check rules` and `promtool test rules` on the alert rules, and a firing test for every rule ([`tools/alertcheck`](tools/alertcheck/main.go)) |

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
