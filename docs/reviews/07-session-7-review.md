# Review of session 7: events, WebSocket, metrics, tracing, alerts

- **Date:** 2026-11-21.
- **Reviewer:** a fresh Claude Opus instance, read-only, with the rules of
  [`.claude/agents/reviewer.md`](../../.claude/agents/reviewer.md). It was given the assignment
  text, the session's brief from [`plan.md`](../plan.md) and a checkout of the repository at
  `1d791aa`. It was not given the author's reasoning or conclusions, and it had no time limit
  (it ran for 1 h 51 min).
- **Scope:** commits `381d78e..1d791aa`: `internal/events`, `internal/observability`, the
  WebSocket hub and `/v1/metrics` of `internal/api`, the dispatcher's and the reaper's
  measurements, `internal/store`'s new statements and migration 5, `internal/systemtest`,
  `deploy/prometheus`, `tools/alertcheck`, `tools/e2e`, Compose, CI, the guards and the design.
- **Runs:** once, after the code was green locally and in CI (run 44816376876), as the plan
  requires. There was no second round. Each fix below is verified by its own named test and by
  `make guards`, not by another review.

## What the reviewer ran

- `go build`, `go vet`, `make test` with the race detector three times (two passed, one failed
  on a timing assertion, F5), `make lint` (0 issues), `make check-docs`, `make alerts` (8
  rules, promtool passes), and `make guards`: "318 guards, 318 real, 0 failed", 40:33.
- About two hours of experiments on two Compose stacks: the default one with the
  observability profile, and a copy with `RATE_LIMIT=5`, `GATE_BUDGET=300ms`, a provider of
  its own with a 500 ms keep-alive and failures per channel, and extra workers.
- Many and slow WebSocket clients (1,100 at once, 990 that read nothing, slow readers),
  every kind of malformed frame and handshake it could build, Redis stopped for 10 min 48 s
  and paused for 90 s, Postgres paused and made read-only, the API and workers stopped, the
  metrics counted against the database and the mock provider with and without faults, and
  each alert rule's condition produced on the stack.
- It could not check a real provider, more than one API instance, or a collector other than
  Jaeger.

## Verdict, as reported

The events, WebSocket, metrics and tracing work as designed and held up under attack on a
running stack; every metric matched counts taken independently, with and without faults. The
alerting falls short: the rule on result writes cannot fire in the failure it was written
for, nothing alerts when the API or a worker is down, and the breaker rule pages on a healthy
idle channel. Two compiled binaries (47 MB) were committed and pushed. Three major findings,
four minor, three nits.

## Findings and resolutions

| # | Severity | Finding | Verified by the reviewer | Resolution | Verified by |
|---|---|---|---|---|---|
| F1 | major | The binaries `api` and `worker` (Mach-O, 23.4 and 23.6 MB, with local module paths in their strings) were committed in `8279be1` and `67ef55d` and pushed; `.gitignore` ignored only `/bin/`, and `.dockerignore` sent them into every image build. | Yes | Class: build output in the tree, picked up by `git add -A` after `go build ./cmd/...` in the root. Removed from the tree; the four commands' names ignored in `.gitignore` and `.dockerignore`; CI's lint job fails when any tracked file is an executable. The same session then committed a draft document with `git add -A` once more, unpushed, and rebuilt that commit; commits now name their files. The binaries remain in the pushed history of `main`: rewriting it means a force push to a public branch, which is the owner's decision. | CI's step "No build output is tracked" (`git ls-files -z \| xargs -0 file \| grep -E 'executable\|ELF\|Mach-O'`) |
| F2 | major | The alert set misses the failures it describes. (a) With Postgres made read-only, result writes failed only for the lease of the rows in flight and then claims failed on every loop: `NotificationsResultWritesFailing` went pending and resolved without firing, and the first alert came after 10.5 min; its promtool input (errors for 10 minutes) was one a worker never produces. (b) No rule on `up`: with the API stopped, the queue's figures went with it and the firing `OldestDueRowTooOld` resolved. (c) `NotificationsBreakerOpen` (page) fired on a healthy idle channel whose breakers stayed half-open. | Yes | Class: a rule tested on an input the system cannot produce. The design gained a fourth check (§10): a rule's promtool input has the shape its failure has on the stack. `NotificationsDatabaseFailing` replaces the result-write rule and covers claims, result writes and reaps; `NotificationsBreakerOpen` requires attempts in the channel; `NotificationsAPIDown` (`up{job="api"} == 0`) and `NotificationsNoWorker` (`absent(up{job="worker"} == 1)`) are new; alertcheck checks `up` against the jobs of `prometheus.yml`. Each changed rule was made to fire, or shown quiet, on the stack (below). | `make alerts` (10 rules; the inputs: a read-only Postgres, an idle half-open breaker, a failed scrape, workers ending with staleness markers); TestTheCheckRefusesWhatNoBinaryExports; guards "alertcheck: up is checked against the jobs of the configuration", "alertcheck: the jobs are read from prometheus.yml"; the stack, below |
| F3 | major | The figures from Postgres were read under a lock held during the read, and a failed read was not kept. With Postgres paused, 20 concurrent `/v1/metrics?window=1h` waited for each other's 2 s reads: 500s at 2 to 28 s, 11 without an answer at the server's 30 s; scrapes of the API took 4 to 18 s and failed whole (`up` = 0). §10's "left out of that scrape" and "four queries every 5 s" did not hold. | Yes | One shared read, its value or its error kept for 5 s, with its own timeout; a caller stops waiting at its own deadline (§10). The count of statements is corrected: two for the queue and two per window, ten per 5 s at most. The scrape's own timeout repeated the read's and was removed. | TestAFailingFigureReadIsSharedKeptAndLeftByItsCallers (before: 20 reads, the slowest caller 40 s; after: one read, every caller within it, a caller with 300 ms leaves at 300 ms, a scrape answers without the figures); guards "a failed figure read is kept for 5 s (review F3)", "a figure read is shared by its callers (review F3)", "a caller stops waiting for a figure at its deadline (review F3)" |
| F4 | minor | At `LOG_LEVEL=info` a delivered notification left no line with its correlation id: the API's request line and the worker's line of a sent attempt were debug lines. §10 also named a claim line that does not exist. | Yes | Both are info lines (§10 states the cost: about 300 worker lines per second at the full rate); the claim line is removed from §10. | TestOneCorrelationIDFollowsAMessageFromTheRequestToTheProvider, now with both processes at info (before: no API line, one worker line of two attempts); guards "a request has a line at info (review F4)", "every attempt has a line at info, a delivery included (review F4)" |
| F5 | minor | `make test` failed once in three runs: TestPublishNeverBlocksWhenRedisHangs required every `Publish` to take at most 10 ms, and one took 10.018 ms with every package running at once. | Yes | The slowest call is compared with 100 ms; a `Publish` that waits for Redis takes its context's 2 s, so the guard still turns the test red. | Guard "events: Publish never waits for Redis (§9)" |
| F6 | minor | An HTTP/1.0 upgrade, or one with two `Sec-WebSocket-Key` headers, passed the checks, took a connection slot, read the database and was then refused by the library in plain text (426 and 400). | Yes | `isUpgrade` refuses what the library would, in the error form, before the bound and the database (§9). | TestAnUpgradeTheLibraryWouldRefuseIsRefusedFirst; guards "an upgrade has exactly one key (review F6)", "an upgrade is HTTP/1.1 or later (review F6)" |
| F7 | minor | A client that follows 10 batches receives about 600 events per second at the full rate, so 256 waiting messages is about 0.4 s of slack: with 990 non-reading clients connected, 1 of 10 normal readers was cut off once; not in a second run. Memory stayed bounded (peak 138 MiB). | Once | Accepted, and stated in §9 as a burst allowance: the client reconnects and is sent the current state. The bound is now tested at 256 and 257. | TestAClientQueueHoldsItsBoundAndNoMore |
| F8 | nit | A client that dropped TCP without a close frame was counted as `no_pong` (6 of 10). | Yes | When a ping fails and the client's reader has already returned, the close is the client's. | TestAClientThatHangsUpIsCountedAsTheClients (before: 20 of 20 counted as `no_pong`); guard "a client that hung up under a ping is the client's (review F8)" |
| F9 | nit | The delivery latency counts from `created_at`, so a scheduled notification's wait, or a requeued one's, is in it. | Reasoned | Stated in §10: the time a row was due is not kept apart from `ready_at`, which retries move. | §10 |
| F10 | nit | "Refused unread and uncopied" overstated it: go-redis has read the payload. | Yes | §9 and `Decode` say "before it is decoded". The event's bound is now tested at 1024 and 1025 bytes. | TestAnEventIsReadUpToItsBound |

## The changed rules on the stack

After the fixes, on the Compose stack with the observability profile (`make up-observability`,
Prometheus evaluating every 15 s; the script polled `localhost:9091/api/v1/alerts` every 15 s):

| Condition | What Prometheus showed |
|---|---|
| The mock provider stopped, 20 push notifications, the waiting rows cancelled, the provider back: the push breakers half-open on an idle channel | `NotificationsBreakerOpen{channel="push"}` pending while the failed attempts were inside its 5-minute rate, then gone at 300 s; never firing |
| 30 sms rows in flight to a provider answering in 10 s, then `ALTER SYSTEM SET default_transaction_read_only = on` | `NotificationsDatabaseFailing` pending at 15 s, firing at 135 s, until Postgres was writable again |
| The API stopped | `NotificationsAPIDown` pending at 30 s, firing at 150 s |
| Both workers stopped | `NotificationsNoWorker` pending at 45 s, firing at 165 s; still firing 30 s after they were started again, the time Prometheus takes to look the name up again |

Before the fixes the reviewer measured no alert for 10.5 minutes of a read-only Postgres, no
alert with the API stopped, and a page for the idle breaker.

## Claims and reality

- **§10, the figures from Postgres** ("left out of that scrape", "four queries"): F3.
- **§10, alerts** ("three checks keep a rule from being one that cannot fire"): F2. The checks
  proved that a rule fires on its own input, not on the input its failure produces.
- **§10, logs** ("the claim's row"): F4.
- **§9, the upgrade** ("a 400 in the error form, before the upgrade"): F6.
- **§11, I13** cited "session 7's record", which did not exist at `1d791aa`; it does now. The
  README was stale at that commit; it was updated with the record.
- Reproduced as claimed: the workers' counters, `delivery_attempts`, `/v1/metrics` and the
  mock provider agreed exactly, 3,002 without faults and 9,513 attempts with 30% failures,
  10% throttles and 50 ms latency; events published equalled events received, 34,032; 1000
  WebSockets accepted and 100 refused with 503, about 128 MiB at 1000; every malformed frame
  cut or answered with 1002, a frame claiming 2^63−1 bytes cut at once; another origin 403;
  shutdown closed 50 of 50 with 1001; Redis stopped for 10 min 48 s: 628 creates, all 201,
  the subscription back within 5 s; Redis paused 90 s: 87 creates, all 201; in Jaeger the
  request's span is the parent of the `deliver` span. Alerts that fired on the stack:
  `OldestDueRowTooOld`, `GateRefusals`, `FailedShareHigh`, `ProviderClosesIdleConnections`,
  `RateLimitUnavailable`, `EventsDropped`, `BreakerOpen`.

## The hard questions, answered

1. *Postgres refuses writes for 10 minutes: which alert fires, and when?* Before the fixes,
   none for 10.5 minutes. After, `NotificationsDatabaseFailing`, on claims, result writes
   and reaps: firing 135 s after Postgres turned read-only on the stack (above).
2. *Every API instance is down: what does the on-call person see?* `NotificationsAPIDown`,
   firing 150 s after the API stopped on the stack. The queue's figures come only from the API, so their rules are silent
   while it is gone; that is why the rule on `up` exists.
3. *What does the 2 s read timeout bound with 20 callers and a stalled Postgres?* Now, one read
   per 5 s whatever the number of callers, each caller within its own deadline. A failure is
   kept for 5 s like a value, so a stall costs one statement per 5 s, not one per caller.
4. *How does an operator follow a delivered notification by its correlation id at the level
   that ships?* The API's request line and the worker's line of each attempt are info lines,
   each with the id; the provider receives it in `X-Correlation-ID`.
5. *Why are executables in the public history, and will it be rewritten?* `go build ./cmd/...`
   in the root and `git add -A`. They are out of the tree and CI refuses the next; the history
   is left to the owner.
6. *Why does an idle breaker report half-open forever, and why did that page?* A claim that
   finds nothing marks no probe (§6), so a breaker on a channel with no rows stays half-open
   until one comes; that costs nothing. The rule now requires attempts.
7. *With `RATE_LIMIT=20`, two workers sent 8 to 12.6 per second with no refusal: what holds
   the loop below the limit?* Not answered in this session; it is open for session 8's load
   test (the session record).

## The full guard run and CI

The reviewer's own `make guards` on `1d791aa`: `318 guards, 318 real, 0 failed`, 40:33; CI on
that commit, run 44816376876, green on all five jobs. After the fixes: locally on `bc7ce16`,
`328 guards, 328 real, 0 failed` (40 min 27 s); CI on `850d1a0`, run 44842146255, green on all
five jobs, guards `328 guards, 328 real, 0 failed`, the compose job's three counts of sent
attempts 1502, 1502 and 1502, and alertcheck passing on the live scrapes
(`gh run watch 44842146255 --exit-status`). The commit that adds this paragraph changes only
the two records.
