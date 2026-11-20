# CLAUDE.md

Rules for AI coding agents (and humans) working in this repository.

## What this is

An event-driven notification service in Go, built in sessions from a reviewed design.

- The design: [`docs/architecture.md`](docs/architecture.md). It is the contract.
- The plan and the brief of each session: [`docs/plan.md`](docs/plan.md).
- Why it is built this way: [`docs/lessons.md`](docs/lessons.md),
  [`docs/journey.md`](docs/journey.md).

## Starting a session

1. Read `docs/architecture.md`, `docs/plan.md` and the record of the previous session in
   `docs/sessions/`.
2. Build only the session marked "next" in the plan. Do not start the one after it.
3. Follow "How a session runs" and "Definition of done" in the plan.

## Rules

1. **The design changes first.** If the code needs to differ from `docs/architecture.md`,
   change the document, with the reason, then the code. Never let them disagree.
2. **Tests of an invariant are written before the code that enforces it.**
3. **Rate-limit permits are taken in one place:** the provider client, when the connection is
   ready. No other code takes, holds, ages or returns a permit.
4. **Nothing is reserved ahead and given back.** No permit, no attempt, no breaker
   reservation. `attempts` only goes up. If a change seems to need a refund path, the design
   is wrong; stop and say so.
5. **Every claimed row ends in `handle`, and `handle` ends in `Finish`.** Do not add a second
   exit.
6. **The verdict is decided in `Classify` from what was observed.** A response always wins.
7. **Queue times come from Postgres (`now()`), rate-limit times from Redis.** Go passes
   durations, never timestamps, to the queue.
8. **Queue writes are not cancelled by shutdown.**
9. **Delivery tests use the real provider client and the recording server.** Do not add a
   fake provider. Guarantees are measured where their effect happens: at the provider's side,
   stamped on arrival.
10. **Every input and output has a bound** in `docs/architecture.md` §8, and a test at the
    bound. A new endpoint or error path states its bound first.
11. **Fix the class, not the reproduction.** When a defect is found, name its class, look for
    the other instances (error paths, the batch next to the item), and prefer removing a part
    to adding a guard.
12. **A claim needs its evidence next to it.** A number has its command. A guarantee has its
    test name. An assumption that is not measured is listed in §12 as an assumption. Commit
    messages follow the same rule.
13. **A guard is real only if disabling it turns a named test red.** Add each guard to
    `make guards`. A mutation that does not compile proves nothing.
14. **Migrations are append-only.**
15. **The dispatcher loop fits on one screen.** If it does not, something belongs elsewhere.

## Reviews

- One independent review per build session, run once, after the code is written and green.
  There is no second round. Fixes are verified by their own named tests and by `make guards`,
  not by another review.
- The reviewer is a fresh instance with no knowledge of the author's reasoning. Give it the
  requirements and the repository, never your conclusions.
- The reviewer is impartial and read-only, and reproduces what it reports.
- The commits that fixed the previous session's review are the next session's review's first
  suspects.
- Review records in `docs/reviews/` are claims too.

## Conventions

- Errors: wrap with `%w`; map to HTTP only in `internal/api`.
- Logs: `log/slog` with the context, so the correlation id is attached.
- Tests: named after the behaviour (`TestHighPriorityOvertakesBacklog`); real Postgres and
  Redis for anything that touches SQL or Lua; `-short` skips those.
- Commits: one finished step per commit, made when the step is finished; the body says why.
- Documents: plain English, short sentences, no claim without evidence.
