# Review of session 9: the walkthrough and the final documents

- **Date:** 2026-11-22.
- **Reviewer:** a fresh Claude Opus instance, read-only, with the rules of
  [`.claude/agents/reviewer.md`](../../.claude/agents/reviewer.md). It was given the assignment
  text, the session's brief from [`plan.md`](../plan.md) ("9. Walkthrough and final documents")
  with the further items the session was started with, and the repository at `2b04f36`. It was
  not given the author's reasoning or conclusions. It had no time limit; it ran for about 43
  minutes.
- **Scope:** the repository read as an evaluator who sees it for the first time: every claim and
  command of the README, run from a fresh clone; every step of the walkthrough against the code;
  every requirement of the assignment and its place in the README's map; contradictions and
  overclaims across the documents; the session's one piece of code, `tools/docrefs`.
- **Runs:** once, after the documents, `make check-docs` and the session's three clean-clone
  rehearsals had passed. There was no second round. Each fix below is verified by its own test
  or command and by `make guards`, not by another review.

## What the reviewer ran

- From a fresh clone of GitHub: `make up`; every command of "Try the API" in order, with
  `curl -i`; `make e2e N=1`; `/openapi.yaml`, `/docs`, `/healthz`, `/metrics`;
  `docker compose run --rm --no-deps -e LEASE=40s worker`; the failing provider and its reset;
  `make load` (30,000); `API_KEYS=... make up` with and without keys, `make e2e` with and without
  `KEY=`, a short key; `make up-observability` (Prometheus targets and rules, a Jaeger trace with
  the API's span and the worker's `deliver` span); `make alerts`; `make down`.
- In a copy: a plain `docker compose up -d` with no image present, and `docker build --no-cache`.
- In the repository: `go build ./...`, `go vet ./...`, `make test` (1 min 30 s), `make test-unit`,
  `make lint`, `make check-docs`; `go run ./tools/guards -only docrefs`; A5 at 48 dispatchers for
  3 s; mutations of copies of `tools/docrefs`; `gh run view` of CI on `2b04f36`.
- It could not check a full local `make guards` (CI ran it: `343 guards, 343 real, 0 failed`),
  a real webhook.site provider (it publishes data on an outside service), a first build with an
  empty Docker layer cache, or the history of the first version's reviews.

## Verdict, as reported

The README works as written: every command gave what it says, and the walkthrough's 20 steps
and six branches match the code, in content and in order. No critical or major defect. The
findings are places where the wording says more than the evidence: the tests of
`tools/docrefs` did not check two properties its documentation promises, several kinds of
reference slipped past it, and a few rows of the README's map and a few figures said more than
their tests or sources.

## Findings and resolutions

| # | Severity | Finding | Reproduced | Resolution | Verified by |
|---|---|---|---|---|---|
| F1 | minor | The test of `tools/docrefs` held no name declared only in another file than the one its step links, and none declared only in another package than its qualifier; a copy whose `declaredIn` searched every file, and one that took a qualified name from any package, both passed it. The two guards were real only because their mutation removed the whole check | Yes, two mutated copies | Class: a check whose test cannot tell its property from a weaker one. The fixture has one case of each (`ac3dfe0`'s fixture extended in `f68403c`, red), and two guards make exactly the reviewer's two mutations | TestDocRefsReportsWhatDoesNotResolve; guards "docrefs: a name of a step is looked for only in the files the step links", "docrefs: a qualified name is looked for only in its own package" |
| F2 | minor | References that slipped past `tools/docrefs`: a qualifier that is no package (a typo, a renamed package); a link with a title; a reference-style link; an HTML link; a path written with `./` or with `:line`; `(*T).m` and `(T).m` not told apart; a code span split across two lines (the walkthrough's retry branch had one); a link inside a `~~~` fence was read. The README, the Makefile and CI said "the documents" for a check of paths and names that reads two of them; the journey said the walkthrough "cannot drift from the code unnoticed" | Yes, a test repository of its own | The same class. Each kind is read and has a case and a guard (`411d32c`): a qualifier must be a package of the repository or one its code imports; `(T).m` needs a value receiver; a line with an unclosed span is reported; `~~~` is a fence. The split span in the walkthrough is mended. The descriptions say exactly what is checked, and that names are checked, not what is said about them | TestDocRefsReportsWhatDoesNotResolve; `go run ./tools/guards -only docrefs`: 18 guards, 18 real |
| F3 | minor | The README's map, the walkthrough and §15 cited TestAScheduledNotificationWaitsUntilItsTime for the scheduled bonus; it checks only the create's answer | Yes (read) | Each now cites what each test shows: stored `scheduled` (that test), not claimed before its time (TestClaimReturnsOnlyDueRows), sent by a real worker once due (TestAWebSocketClientReceivesTheSentEventOfARealDelivery). The test keeps its name, which the records of sessions 2 and 6 cite | `make check-docs`; the three documents |
| F4 | minor | `make test` runs with `-race`, under which the tests of the limit at the provider only log their peak; the README's map cited them under "Tests with one command" without saying so | Yes (read) | The README says so and gives the command that asserts the limit, which CI's test job runs | README, under the map |
| F5 | minor | webhook.site, the assignment's provider, was never used; the README listed it under "Not measured" only, and the journey said the system "meets the assignment" | Yes (read; the runtime image has a CA bundle, so an `https` URL can be reached) | "Not done, and why" says it was not used and why, the one change that would use it, and what its default answer would do (every row `sent`, no provider message id); the journey names the substitution. Trying it stays the owner's decision | README, journey |
| F6 | minor | The slow batches were "one after 29.6 s" in the README's account (two, 15.8 s and 29.6 s, in session 8's record); the burst's drain mixed the author's runs (93.5 to 93.7 per second) with the reviewer's (93.9) without naming it | Yes | Each figure now says whose run it is, from session 8's record and review record | `docs/measurements.md`, README |
| F7 | nit | The order of the list example is not reproducible (two items of one batch share their time); "`curl -i` prints it" read as if the commands used it | Yes | The README says both | README |
| F8 | nit | The README's A5 command named 48 dispatchers, which its result does not report | Yes (48 ran 3 s with `too many clients already` logged and no refusal) | 48 left out of the command; architecture §12 already says 48 could not be measured | README |
| F9 | nit | §14 said `events` and `observability` depend only on `domain`; `events`, `store` and `provider` also import `observability` | Yes (`go list`) | §14 corrected, with the command | §14 |
| F10 | nit | Step 20 said "so no change is missed" with tests that do not check the order of `follow` before reading the current state, and an event queued between them can arrive after a newer state; TestARedisThatRefusesWritesClaimsNothing was listed under the breaker; "four processes" (five); "a worker has at most one claim batch" and "per worker" are per dispatcher | Reasoned (the order); yes (the rest) | Step 20 and the README say what a client can see and that the order has no test (an open item since session 8); the test moved to step 10; the counts and I9's wording (§11) say per channel | the walkthrough, README, §11 |
| F11 | nit | The README named fewer commands that need Go than there are; `make e2e` needs `KEY=` when `API_KEYS` is set; "343 guards" had no command; the record said "the brief asks" for items plan.md's brief does not have | Yes | All four said (the guard count is now 354, with its command) | README, the record |

## Claims and reality, as reported

| Claim | The reviewer's measurement |
|---|---|
| README API examples | Reproduced, ids apart, except the list's order (F7) |
| "pull access denied", then the build | Reproduced with no image present: three lines, then `Built` |
| `make e2e N=1` | `scheduled, processing, sent`; `all 5 notifications sent in 3.68s` |
| `LEASE=40s` exits 2 | Reproduced, the relation named |
| API keys | 401 without, 200 with either header; a short key keeps the API restarting and names it |
| The observability profile | 3 targets up, 10 rules; one trace with the API's span and the worker's `deliver` span |
| The burst of 30,000 | Accepted in 0.257 s; 93.6 per second per channel; peak 100; 30,165 received, accepted and created; none twice; high priority median 0.37 to 0.41 s, a little below the README's 0.42 to 0.46 s; memory peak 42 MiB |
| 282 test functions | 282; all pass in 1 min 30 s |
| 343 guards, all real | 343; CI run 44963849689 on `2b04f36` |
| 606 scenarios, 181,800 notifications, 42 accepted twice | Consistent across the README, `measurements.md` and §11 |
| Two binaries in the history, 47 MB | `api` 23,443,986 B and `worker` 23,607,586 B |

## Hard questions, and the answers

1. *Which test fails if `declaredIn` searched every file?* Now TestDocRefsReportsWhatDoesNotResolve,
   and the guard that makes exactly that mutation (F1).
2. *If a package is renamed, which check reports the old `ratelimit.X` references?* `tools/docrefs`:
   a qualifier that is neither a package of the repository nor one its code imports is reported
   (F2).
3. *Was the client ever pointed at a real HTTPS provider? With webhook.site's default answer
   (200, plain text) rows end `sent` with no provider message id; is that what an evaluator should
   see?* No; the README now says so and how to configure the answer (F5). Trying it is the
   owner's decision.
4. *"About 40 dispatchers before refusals" is derived; at 48, a 3 s run had no refusal and a
   longest wait of 4.27 s. Which run would show the boundary?* A run long enough to fill the
   window many times, at 40 to 60 dispatchers, with Postgres's `max_connections` raised or each
   worker's pool lowered (`pool_max_conns`), so that all of them connect; the reviewer's 48
   logged `too many clients already`, so not all 48 were claiming (reasoned). Not run; §13
   derives the 40 from the relation that the runs up to 36 dispatchers fitted, and says so.
5. *Is "the last WebSocket message is the current status" a promise of the API?* No. Events are
   best-effort and unordered across processes (§9); `sent`, `failed` and `cancelled` are final,
   and the status endpoint is the truth. The README and step 20 now say so (F10).
6. *Should the README's rate-limit row name the command that asserts the limit?* Yes; it does
   now (F4).
