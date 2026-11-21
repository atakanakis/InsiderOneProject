# Session 8: hardening

- **Date:** 2026-11-21.
- **Lead:** Claude Opus. **Builders:** none; the lead wrote the code. **Reviewer:** one fresh
  Claude Opus instance, run once on the whole system after the measurements and CI were green
  ([review record](../reviews/08-session-8-review.md)).
- **Brief:** [plan.md, "8. Hardening"](../plan.md#8-hardening). No new feature: measure,
  review, fix what is found.
- The session was interrupted once (the machine had to be shut down) and resumed from a
  handover note; nothing was lost, and the only half-done change, a CI step, was committed after
  its tool had been tried.

## What was built

Only what the measurements needed.

| Part | What it is | Evidence |
|---|---|---|
| The dial timeout's test | A send to an address that never answers a connection attempt ends at the dial timeout, not the send's deadline, and the gate never runs. The address is a listener whose accept queue of one is full and never accepted from; Linux and the BSDs drop the SYN then. Sessions 4 and 5 had left the dial timeout untested because such an address seemed impossible on one machine | TestTheDialHasItsOwnTimeout; guard "provider: the dial has its own timeout" |
| The mock provider counts what it accepted twice | `GET /stats` has `accepted`, `acceptedTwice` (keys accepted more than once, each once), `keysTracked` (at most 1,000,000) and `acceptedUntracked`; a Lean recorder still hands `Respond` the whole request | TestMockProviderCountsWhatItAcceptedTwice, TestMockProviderTracksABoundedNumberOfKeys, TestALeanRecorderStillShowsRespondTheWholeRequest; four guards |
| `tools/load`, `make load` | A burst against the Compose stack, measured from outside: acceptance and the time to upload each batch, the API's memory from its `/metrics`, single creates during the burst (the rendering slots), high-priority notifications during the drain, the drain, the provider's peak and duplicates. CI's compose job runs it with 6,000 | the compose job |
| A5's knobs | `A5_COUNTS`, `A5_LATENCY`, `A5_LIMIT`, `A5_RUN` on TestGateWaitAndThroughputWithManyDispatchers; the defaults unchanged | the runs below |
| [Experiment 03](../experiments/03-tables-without-retention.md) | The API's reads as the tables grow without retention: `GET /v1/metrics` and the list's filters up to 16,000,000 notifications | the experiment's commands |

## Measured

All on the author's machine (Apple silicon, 16 GB, 8 CPUs and 8 GB for Docker), one heavy run
at a time. The outputs of the runs are not in the repository; each line has its command.

| What | Result | Command |
|---|---|---|
| A burst of 30,000, three runs | accepted in 0.24 to 0.34 s; the API's resident memory at most 50 MiB; drained in 1 min 46.7 to 46.9 s, 93.5 to 93.7 per second per channel; the provider's peak 100 per channel; received and accepted exactly what was created, none twice; a high-priority notification created during the drain sent in a median of 0.42 to 0.46 s, p95 1.14 to 1.19 s, the longest 5.1 to 5.2 s, behind the burst's own 500 high-priority rows per channel (500 / 95 per second) | `make up; go run ./tools/load -n 30000` |
| The rendering slots, contents of 1,500 bytes | single creates every 20 ms during the burst answered within 27 ms with 4 batches at once and 54 ms with 8 | `go run ./tools/load -n 30000 -concurrency 4 (then 8) -content-bytes 1500 -probe-every 20ms` |
| 8 MB batches, 8 at once, 20 runs | accepted in 0.43 to 0.51 s in 19 runs; the API's resident memory at most 245 MiB; single creates answered within 63 ms in the five runs that printed them. One run: the mock provider stamped 130, 121 and 125 in one second on the three channels at once (below). One other run: one batch answered after 29.6 s (below). Twelve runs with the POSTs captured on the provider's interface: at most 100 per channel there and at the stamps | `go run ./tools/load -n 15000 -concurrency 8 -content-bytes 8000 -probe-every 20ms`; the capture: `tcpdump -i any -tt -nn -l -A -s 700 'tcp dst port 9090 and tcp[((tcp[12:1] & 0xf0) >> 2):4] = 0x504f5354'` in a container sharing the mock provider's network namespace (`docker run --net container:insider-notifications-mockprovider-1 --cap-add NET_RAW alpine:3.22`), counted per channel from each request's body |
| The long fault run | 303 scenarios, 303 passed, 27 min 20 s: 90,900 notifications all final, 100,725 requests received, peak 40 (the limit), none late, no claim without its attempt row, no row reaped; 20 notifications in 8 scenarios accepted twice, each after an attempt without a response (at-least-once case 3) | `FAULT_RUNS=300 FAULT_SEED=1000 go test -count=1 -timeout 0 -run TestInvariantsHoldUnderRandomFaults -v ./internal/dispatch` |
| A second long fault run | 303 scenarios, 302 passed, 27 min 39 s: 101,411 requests received, all final; 22 notifications in 14 scenarios accepted twice, each after an attempt without a response. Seed 2268 failed I1: 47 email requests within one second, limit 40, with 10 requests later than the margin after their permits (the latest 144 ms); the same seed alone passed five times out of five, no request later than 10 ms (below) | `FAULT_RUNS=300 FAULT_SEED=2000 ...` |
| A5 above 12 dispatchers | 12, 24 and 36: 94.9, 94.8 and 94.4 per second; longest wait 1.07, 3.13 and 4.21 s; no refusal. 48: the test's Postgres refused the 48 workers' connections | `A5_COUNTS=12,24,36,48 A5_RUN=12s go test -count=1 -timeout 0 -run TestGateWaitAndThroughputWithManyDispatchers -v ./internal/dispatch` |
| A5 with a 200 ms provider | 1, 6 and 12: 93.5, 94.7 and 94.9 per second; longest wait 8 ms, 1.03 and 1.06 s; no refusal | `A5_COUNTS=1,6,12 A5_LATENCY=200ms A5_RUN=12s ...` |
| A5 at a limit of 20 | 2 dispatchers: 18.9 per second, longest wait 1.02 s, no refusal. 12: 18.9 per second, median wait 2.1 s, longest 5.0 s, 50 refused for no permit in time | `A5_LIMIT=20 A5_COUNTS=2,12 A5_RUN=12s ...` |
| `RATE_LIMIT=20` on the Compose stack | nine runs of 600 to 3,220 rows: 20 per second in almost every second, 18.6 to 19.1 per second over each run (top speed 19.05); session 7's reviewer's 8.1 and 12.6 not reproduced (below) | `.env` with `RATE_LIMIT=20` (and the variations below), `make up`, a batch through the API, then `docker compose exec -T postgres psql -U notifications -At -c "select to_char(date_trunc('second',started_at),'MI:SS'), count(*) from delivery_attempts group by 1 order by 1"` |
| `GET /v1/metrics` and the list at scale | at 16,000,000 notifications: the 1-hour window 3.8 to 5.3 s, 15 minutes 2.4 to 3.6 s, 5 minutes 0.37 to 0.51 s; without the join, at most 0.93 s for the hour; a list filter matching no row 1.6 to 3.2 s | [experiment 03](../experiments/03-tables-without-retention.md) |
| The dial timeout | a send to an address that does not answer ends after 301 to 303 ms with a dial timeout of 300 ms (send timeout 5 s); without the timeout, 5.002 s and the test fails | `go test -count=3 -v -run TestTheDialHasItsOwnTimeout ./internal/provider` |
| CI on `6b69277` | lint, test, alerts, guards (`333 guards, 333 real, 0 failed`) and compose succeeded; the compose job's burst of 6,000: drained in 21.5 s, received and accepted 6,035, none twice, peak 100 per channel | `gh run view 44900529899 --log` |
| Guards, locally, before the review | `333 guards, 333 real, 0 failed` in 40 min 47 s, on `ed5fd2a` with only documents changed after it; the tree unchanged after the mutations | `time make guards` |
| Tests, lint | 278 test functions, all pass with the race detector in 1 min 44 s; lint 0 issues; every test named in the documents exists | `time make test`; count: `grep -rh '^func Test' --include='*_test.go' . \| grep -v TestMain \| wc -l`; `make lint`, `make check-docs` |

### The questions left to this session

- **`RATE_LIMIT=20`, below the limit (session 7).** Not reproduced. The nine runs covered the
  defaults on one channel; three channels with mixed priorities; the reviewer's settings
  (`GATE_BUDGET=300ms`, a provider of the reviewer's own with a keep-alive of 200 ms and of
  500 ms, 80% failures on email, the observability profile); 16 busy processes in a container;
  and the guards of the dispatcher and of the queue running beside it. The reviewer's
  transcript shows its figures were a Prometheus `rate(...[1m])` read 1.5 and about 3 minutes
  after the start, while its own full `make guards` ran. Section 6 gives a reading, marked as
  not shown. A5 at a limit of 20 found a related, real limit: with 12 dispatchers the batch
  rule lets 120 rows wait for 20 permits per window, and sends are refused (section 13).
- **`GET /v1/metrics` over an hour.** Measured: at about 16,000,000 notifications. Kept, with
  the limit stated and the measured alternative named (experiment 03, sections 10 and 16).
- **The list's filters.** No index: a filter that matches nothing reaches the deadline later
  than the metrics do, and an index per filter would be written on every status change
  (section 8).
- **The 4 rendering slots.** A single create waited at most 63 ms behind 8 MB batches posted 8
  at once.
- **A5 above 12 dispatchers and with a slow provider.** Measured; the relation it gives is in
  section 13.
- **The dial timeout.** Tested (TestTheDialHasItsOwnTimeout) and guarded.

### Three events that did not repeat

Two of them are not explained; they are open below.

- **A red fault scenario, seed 2268.** In the second long run, one scenario of 303 failed the
  rate limit at the provider: 47 email requests within one second, the limit being 40. The
  scenario's own measurement of the time from each permit to its request's arrival counted 10
  email requests later than the 50 ms margin, the latest 144 ms, and 4 sms requests, the
  latest just over 50 ms; the excess, 7, is within the 10 late requests. Section 7's limits
  table names this case: a worker held for longer than the margin between taking a permit and
  writing its request. The test runs the workers and the recording server in one process, so
  a stall of that process shows exactly so; no other heavy run of session 8 ran at that
  moment, and what held the process is not known. Run alone five more times
  (`FAULT_RUNS=1 FAULT_SEED=2268 go test -count=1 -run 'TestInvariantsHoldUnderRandomFaults/seed-2268' -v ./internal/dispatch`),
  the seed passed five times, the latest arrival 10 ms after its permit. The test keeps its
  strict check (session 5's reasons, §11 I1); this is the first measured case of A4 not
  holding without the race detector, and §12 records it.

- **A peak of 130 at the mock provider.** In the first run of the 8 MB burst with 8 batches at
  once, the mock provider's stamps showed 130, 121 and 125 requests within one second on sms,
  email and push. The following 19 runs of the same burst stayed at 100, twelve of them with
  every request captured on the provider's network interface by kernel time, also at 100. The
  run with the excess had no capture, so it is not shown which happened: requests that arrived
  later than the margin after their permits (section 7's frozen-process row; all three channels
  at once would fit a stall of the Docker VM, which runs the workers, Redis and the provider),
  or a stall of the mock provider alone, which stamps a request when its handler runs. It is
  recorded as unexplained in section 11 (I1), not as a pass.
- **A batch answered after 29.6 s.** In one run, two of the fifteen batches were answered after
  15.8 and 29.6 s, the others within 0.5 s. The API's own request lines give the same
  durations. Single creates, which need a rendering slot too, were answered every 20 ms
  meanwhile, so no slot was held; Postgres's log shows nothing. The remaining place is the
  upload of the body from the host into Docker. The tool now reports the time to write each
  body and the time from the written body to the answer; in the twelve runs after that the
  writes took at most 353 ms and the answers at most 369 ms after them. Not shown.


## What is still open

- **The excess of 130, 121 and 125 at the mock provider** (above): not explained. The
  hypothesis that fits, a stall of the Docker VM after permits were taken, is not shown; a
  capture on the provider's interface during a run with the excess would separate it from a
  stall of the mock provider alone. Nineteen later runs did not repeat it.
- **The batch answered after 29.6 s** (above): not explained; the upload into Docker is the
  hypothesis, and the tool now measures it.
- **What held the fault test's process in seed 2268** (above): the excess itself is explained
  by the scenario's own stamps (requests late after their permits, the named case of section
  7); the cause of the stall is not known. A4 is now known not to hold always without the race
  detector on this machine; the margin stays 50 ms (§12).
- **`GET /v1/metrics` without retention:** the 1-hour window reaches its 5 s deadline at about
  16,000,000 notifications, the 15-minute window later; a list filter that matches nothing
  reads the whole list index. Retention is left out (§16); the change that would bound the
  metrics without it is measured and named there.
- **Two relations no single worker can check** (§13): the dispatchers per channel before
  refusals at the gate (about 40 at the defaults, 8 at a limit of 20), and the connection
  pools of all workers and API instances against Postgres's `max_connections`. Alerts and
  counters show when either is crossed.
- **Session 7's reviewer's 8.1 and 12.6 per second at `RATE_LIMIT=20`:** not reproduced in
  nine runs; section 6 gives a reading, marked as not shown.
- **The two binaries of session 7's F1** remain in the pushed history of `main` (47 MB);
  removing them means rewriting a public branch, which is the owner's decision.
- From session 7, unchanged: events of one notification from two processes have no order
  (§9); the WebSocket's "follow before reading the current state" order has no test; that the
  provider latency histogram starts at the permit is by construction.
