# Session 9: walkthrough and final documents

- **Date:** 2026-11-22.
- **Lead:** Claude Opus. **Builders:** none; the lead wrote the documents and the one tool.
  **Reviewer:** one fresh Claude Opus instance, run once after the documents, `make check-docs`
  and the clean-clone rehearsal had passed ([review record](../reviews/09-session-9-review.md)).
- **Brief:** [plan.md, "9. Walkthrough and final documents"](../plan.md#9-walkthrough-and-final-documents).
  No new feature: a reader can follow one message through the code, and the README states only
  what the repository proves. The measure of the documents is the assignment: for every
  requirement, a reader can find where it is met. The session was started with a longer brief
  than plan.md's: it also asked that every path and function name of the walkthrough be
  verified by a command, for a map from each requirement to its file and test, a short list of
  what was not done, the open issues, reading paths through the architecture, a sweep of every
  number, and a rehearsal from a clean clone. "The brief" below is that one.

## What was built

| Part | What it is | Evidence |
|---|---|---|
| [walkthrough.md](../walkthrough.md) | One SMS from `POST /v1/notifications` to the mock provider and back to the client, in 20 steps, then six branches: a retry, a cancel, a scheduled send, a worker that dies, a refusal by the rate limit, the circuit breaker. Each step links its file, names its functions, the invariant it keeps and the tests that prove it; a table at the end gives the steps of each invariant. Written from the code of `cmd/` and `internal/`, then each figure checked against its test (below) | `make check-docs` |
| [measurements.md](../measurements.md) | The README's long account of session 8's runs, moved, with a heading for each part and pointers to the other measurements | the commands on the page |
| README | A way in: what it is, the one command, where to read (a 10-minute and a 1-hour path), the API examples with what each answered on a fresh clone, the design in brief, a map from every requirement of the assignment to its code and its tests, session 8's figures in short with their commands, what was left out and why, the known open issues, the commands | the rehearsal (below); `make check-docs` |
| [architecture.md](../architecture.md), "How to read this document" | The contract is in the present tense; the record names a session or a review and can be skipped on a first reading; sections 2 to 7 are the core; the long cells of §11 and §12 hold the evidence. Section 14 lists the new tool | — |
| `tools/docrefs` | Every relative link of a Markdown file under `docs/`, of the README and of `CLAUDE.md` names a file that exists, and a heading of it when it names one (anchors made as GitHub makes them). In the README and the walkthrough, every path written in backticks exists, and every Go name is declared outside the tests: on a step's "**Code:**" line, in the file that line links; elsewhere, on its type or in its package. `make check-docs` and CI's check-docs step run it after `tools/testnames` | TestDocRefsReportsWhatDoesNotResolve (a repository of its own with one reference of each kind that does not resolve), TestEveryLinkPathAndNameInTheDocsResolves (this repository); seven guards, one per check |

The tool was written test first (`ac3dfe0` red against a stub, `18545de` the check), after
section 14 named it (`8fb07d6`, rule 1). The repository passed it on its first run, with every
one of its anchored links.

## Decisions

- **A tool, not a command in this record.** The brief asks that every path and function name of
  the walkthrough be verified by a command. A command kept only here could not be run again by
  the reviewer or by CI after the next change; a check in `make check-docs` can. It is the one
  piece of code this session added, so the full `make guards` ran (below).
- **The record stays inside architecture.md.** The review of session 8 found the document heavy
  because it is a contract and a history at once. The history sentences sit next to the rule
  they explain and carry its evidence: test names, runs, numbers. Moving them to another file
  would part claims from their evidence (rule 12) and break the references of eight session
  records. Instead the document now begins with how to read it, and the README gives two
  reading paths.
- **The README is longer than before, and its first screen is shorter.** The brief asks for a
  requirement map, the API examples with their answers, the measurements with their commands,
  what was left out and the open issues. Each is short and links its full form; the first
  screen says what the system is, the one command, and where to read.
- **webhook.site was not tried.** The assignment's provider is a webhook.site URL that answers
  202. Creating one and sending notifications to it publishes data on an outside service, which
  is the owner's decision. The mock provider answers the same contract, the client sends the
  assignment's request (TestTheRequestHasTheAssignmentsFormat), and `PROVIDER_URL` is all that
  changes. The README says it was not measured.
- **The first `make up` prints "pull access denied".** Compose looks for the image of the
  services that share it before it builds it. The README says so instead of changing
  `docker-compose.yml`, which this session does not change.

## How the walkthrough's names were checked

`make check-docs` runs `go run ./tools/testnames` and `go run ./tools/docrefs`; both pass on
every commit of this session from `18545de` on. To see that the check reads the names, four
were broken by hand in a copy of the walkthrough (a method, a qualified name, a name on the
wrong step's file, a method in prose); docrefs reported each with its line, and the file was
put back.

Two figures of the first draft were wrong against their tests and were corrected before the
first commit: the refusal test sends 5,000 refused requests, 2,500 on new and 2,500 on reused
connections (`const perMode, parallel = 2500, 32` in `internal/provider/provider_test.go`), not
10,000; and the test of the killed worker checks that the provider received the row twice with
the same `Idempotency-Key` and that attempt 1 is missing from its history (the end of
TestAKilledWorkersRowComesBackThroughTheReaper).

One figure of the README moved to measurements.md was wrong against its source: the reviewer's
slow batch of session 8 was answered after 7.5 s (7.524 s in the reviewer's report; 7.5 s in
session 8's record and review record), not 7.6 s, which was the acceptance time of its whole
run.

## The clean-clone rehearsal

Each rehearsal cloned the repository from GitHub into a new directory, followed the README in
order, and used nothing the README does not say. Before each, the running stack was stopped and
its data deleted (`make down`) and the image `insider-notifications:local` removed, so that the
stack started empty and the image was built from the clone. Docker's build cache was not
cleared.

| Rehearsal | Commit | What was run | What it found |
|---|---|---|---|
| 1 | `cf9566e` | `git clone ... && cd InsiderOneProject`; `make up` (12.6 s, cached layers); `curl -s localhost:8080/readyz`; every API example in order; `make e2e N=1` (`websocket: notification ...: scheduled, processing, sent`, then `all 5 notifications sent in 3.64s`); `make status`; `/openapi.yaml` and `/docs`; `docker compose run --rm --no-deps -e LEASE=40s worker` (exit 2, the relation named); `API_KEYS=... make up` (401 without a key, 200 with `X-API-Key` and with `Authorization: Bearer`) and `make up` again (200 without); the mock provider made to fail with the README's command, then `make e2e N=100`; `make load` (30,000: accepted in 260 ms, drained in 1 min 46.8 s, 93.6 to 93.7 per second per channel, peak 100 per channel, received and accepted 30,165 of 30,165, none twice, high priority median 421 to 441 ms); `make down`; `docker compose up --build` in the foreground (ready; a notification sent; two workers), then `make down` | Four gaps: the comments gave each answer's HTTP status, which `curl -s` does not print; the idempotent repeat and the conflict were described, not given as commands; nothing said how to make the mock provider well again; `make e2e` against the failing provider exits 1 (its single notification was retried), though the 309 notifications of that run were all sent, 80 after an injected 503 or 429, none twice. Fixed in `2a72f77` |
| 2 | `2a72f77` | The same, in order, with the README's new commands: the repeat (200, the same id) and the conflict (409); the failing provider with ten creates, `make status`, `/stats`, then `make load LOAD_N=3000` (it reported `created 3054, the provider received 3955 and accepted 3057` and failed, as the README says; every row ended `sent`, none accepted twice, peak 100), and `docker compose up -d mockprovider` (its `MOCK_` variables 0 again); `make down` | Three gaps: the first `make up` printed "pull access denied" for the image before building it; a status read made at once after a create found it still `pending` (an idle worker looks every 200 ms; it was `sent` 0.19 s after creation); `make` exits 2, not 1, when the tool it runs fails. Fixed in the next commit |
| 3 | `0383347` | The same, in order, from a new clone: `make up` (the three "pull access denied" lines, then `Built` and every container `Healthy`), every API example (the create answered `pending`; read 1 s later, `sent` with attempt 1, status code 202; the repeat 200 with the same id; the conflict 409; the batch, its summary, both pages of the list, the cancel, the retry's 409, `/v1/metrics`, `/stats`), `make e2e N=1` (`scheduled, processing, sent`; `all 5 notifications sent in 3.59s`), `/docs` and `/openapi.yaml`, `LEASE=40s` (exit 2), `API_KEYS` (401, 200, 200) and back (200), the failing provider (ten creates: `retrying` 4 then all `sent`, 17 received for 10 accepted, none twice) and back (its `MOCK_` variables 0), `make down`, `docker compose up --build` in the foreground (ready, two workers), `make down` | Nothing new |

The commands and their answers are those of the README's "Try the API"; its answers are
rehearsal 1's. Rehearsal 3 ran the same commands from a new clone and got the same answers,
ids and times apart. In the list, two items of one batch share their creation time and come in
the order of their ids, so which comes first differs from run to run.

## Measured

| What | Result | Command |
|---|---|---|
| Tests | 282 test functions; all pass with the race detector in 1 min 33 s | `time make test`; count: `grep -rh '^func Test' --include='*_test.go' . \| grep -v TestMain \| wc -l` |
| Guards | 343 in the list before the review, 354 after its fixes | `grep -cE '^\s*Name: "' tools/guards/list.go` |
| The new guards | `7 guards, 7 real, 0 failed` before the review; `18 guards, 18 real, 0 failed` after its fixes | `go run ./tools/guards -only docrefs` |
| Every guard, locally | `343 guards, 343 real, 0 failed` in 40 min, on `2a72f77` with only the README changed; the tree unchanged after the mutations | `time make guards` |
| Every guard, in CI | `343 guards, 343 real, 0 failed` on `2a72f77` (run 44959944863), and lint, test (check-docs included), alerts and compose green | `gh run view 44959944863 --log` |
| Lint, documents | 0 issues; every test, link, path and Go name the documents name exists | `make lint`, `make check-docs` |
| No Turkish in the repository | 0 lines | `LC_ALL=en_US.UTF-8 git grep -nIP '[\x{11E}\x{11F}\x{15E}\x{15F}\x{130}\x{131}\x{C7}\x{E7}\x{D6}\x{F6}\x{DC}\x{FC}]' -- .`: the twelve letters of Turkish that ASCII lacks, written as code points, so that the command does not find itself. With a bracket of the letters themselves and no UTF-8 locale, git grep matches single bytes, and every `§` matches |

## The review

One fresh Claude Opus reviewer, run once at `2b04f36` after the documents, `make check-docs` and
three rehearsals had passed, with no time limit (about 43 minutes;
[review record](../reviews/09-session-9-review.md)). It ran the README from a fresh clone,
every command in order, the observability profile and a plain `docker compose up` without an
image included; read every step of the walkthrough against the code; looked for every
requirement of the assignment; and mutated copies of `tools/docrefs`. Its verdict: the README
works as written and the walkthrough matches the code; no critical or major defect; six minor
findings and five nits, each a place where the wording said more than the evidence.

| # | Severity | What | Resolution |
|---|---|---|---|
| F1 | minor | `tools/docrefs`'s test could not tell "declared in the linked file" or "in its own package" from "declared somewhere" | Fixed: red `f68403c`, fix `411d32c`; two guards with exactly the reviewer's mutations |
| F2 | minor | References that slipped past `tools/docrefs` (unknown qualifiers, titled, reference-style and HTML links, `./` and `:line` paths, value and pointer receivers, split code spans, `~~~` fences); descriptions wider than the check | Fixed in `411d32c`, a case and a guard each; descriptions narrowed |
| F3 | minor | The scheduled bonus cited a test that does not wait | The README, the walkthrough and §15 cite what each test shows (`69b7b74`) |
| F4 | minor | `make test` (`-race`) only logs the peak at the provider | The README gives the command that asserts it (`69b7b74`) |
| F5 | minor | webhook.site never used, listed only as "not measured" | Under "Not done", with the one change and its default answer's effect (`69b7b74`); open below |
| F6 | minor | Two figures shortened or mixed their sources | Corrected from session 8's records (`69b7b74`) |
| F7 to F11 | nits | The list's order; `curl -i`; the A5 command's 48; §14's dependencies; step 20's order, a test under the wrong step, process counts, per worker or per dispatcher; commands that need Go, `KEY=`, the guard count's command, "the brief" | All done (`69b7b74`) |

`go run ./tools/guards -only docrefs` after the fixes: `18 guards, 18 real, 0 failed`; with
them the list holds 354 guards.

**Found by the lead after the review.** The command this record gave for "no Turkish in the
repository" held the Turkish letters it looks for, so in the commits from `0383347` to
`69b7b74` the check found this record itself, the one line in the repository with those
letters. The command now writes them as code points, and finds nothing. The same class as
session 8's CI check for binaries: a check whose reach included its own text.

## What is still open

- From session 8, unchanged: an address that answers 400 to every message drains the queue
  into `failed` (kept; §6); the peak of 130, 121 and 125 at the mock provider; the two slow
  batches (29.6 s and 7.5 s); where seed 2268's process was held; `Get` not in one snapshot;
  how many requests may be in flight with their bodies; `GET /v1/metrics` without retention;
  the two relations no single worker can check; session 7's reviewer's figures at
  `RATE_LIMIT=20`; the two binaries in the history of `main`; the order of events from two
  processes; the WebSocket's "follow before reading the current state" has no test. Each is in
  [session 8's record](08-hardening.md#what-is-still-open).
- **Not measured against webhook.site** (decisions, above; the review's F5): the README says
  what to change and what webhook.site's default answer would do. Trying it is the owner's
  decision, because it sends the test data to an outside service.
- **The order of a WebSocket's first message and the events queued before it** has no test
  (from session 8); the README and the walkthrough now say what a client can see (the review's
  F10).
- **"About 40 dispatchers per channel before refusals"** is derived from runs up to 36; no run
  reached the boundary (the review's hard question 4).
- **The "pull access denied" lines of the first `make up`** are documented, not removed.

## CI

To be written when CI has run on the session's last commit.
