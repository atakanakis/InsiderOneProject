# Measurements

What the system did under a burst and under faults, measured in session 8. The README gives the
short form; this page keeps every figure with its command and its range. The record of
[session 8](sessions/08-hardening.md) has every run, the three events that did not repeat, and
what the review measured; the [review record](reviews/08-session-8-review.md) has the
reviewer's own runs.

Other measurements live next to the design they support: the claim's plan (A3), the time from
permit to arrival (A4) and the wait at the gate with 1 to 36 dispatchers (A5) in
[architecture §12](architecture.md#12-assumptions-and-how-each-is-proven); the API's reads as
the tables grow in [experiment 03](experiments/03-tables-without-retention.md); the metrics at
scale in [experiment 02](experiments/02-metrics-at-scale.md); the gate's refusal in
[experiment 01](experiments/01-gate-refusal.md).

All on the author's machine (Apple silicon, 8 CPUs and 8 GB for Docker), against the Compose
stack with two workers and the mock provider, except the fault test, which runs in one process.

## A burst

`make load`, which runs `go run ./tools/load -n 30000`: 30,000 notifications in
30 batches of 1000, 4 posted at once, the channels in turn and the priorities mixed (5% high,
45% normal, 50% low). Three runs by the author, and one by session 8's reviewer where it is
named. A fourth run by the author after the review's fixes stayed inside every range below.

| What | Result |
|---|---|
| Accepted by the API | in 0.23 to 0.34 s (the reviewer's run 0.232 s), 87,000 to 124,000 per second in the author's runs; a batch answered in 29 to 41 ms (median), 97 ms at most |
| The API's memory | resident 37 to 50 MiB, Go heap 16 to 17 MiB at the peak (the author's runs) |
| Drained | 30,000 in 1 min 46.7 s to 1 min 46.9 s, 93.5 to 93.7 per second per channel in the author's runs; 1 min 46.5 s and 93.9 per second in the reviewer's (the top speed is 100 per 1.05 s, 95.2) |
| The mock provider's peak | 100 per channel within one second in every run |
| Lost, sent twice | none: the provider received and accepted exactly what was created (30,164 to 30,165 with the probes; 30,164 in the reviewer's run), none twice |
| A high-priority notification created during the drain | created to sent: median 0.42 to 0.46 s in the author's runs (54 per channel per run), p95 1.14 to 1.36 s (1.35 to 1.36 s in the reviewer's run); the longest 5.1 to 5.4 s (5.35 to 5.36 s in the reviewer's), the first ones, which queue behind the burst's own 500 high-priority rows per channel (500 / 95 per second = 5.3 s) |

Larger batches, 15,000 notifications with content of up to 8,000 bytes (8 MB batches for
email), posted 8 at once (`go run ./tools/load -n 15000 -concurrency 8 -content-bytes 8000
-probe-every 20ms`), 22 runs by the author (two of them after the review's fixes) and 8 by
the reviewer: accepted in 0.43 to 0.57 s in 28 runs; in one of the author's runs two batches were
answered after 15.8 s and 29.6 s, and in one of the reviewer's one after 7.5 s, and what held them is not shown
(session 8's record). The API's resident memory peaked between 120 and 268 MiB. A single
create made every 20 ms during the burst was answered within 63 ms in the seven of the
author's runs that recorded it and within 91 ms in the reviewer's, which bounds how long the 4
rendering slots held it back. In one of the author's runs the mock provider stamped 130, 121
and 125 requests within one second on the three channels at once, above the limit of 100; the
other 29 runs stayed at 100, twelve of them with the requests also captured on the provider's
network interface. The excess is not
explained; three causes fit it, none is shown (architecture §11, I1).

## The randomised fault test, long

`FAULT_RUNS=300 FAULT_SEED=1000 go test -count=1 -timeout 0 -run
TestInvariantsHoldUnderRandomFaults -v ./internal/dispatch`: 303 scenarios (the
three fixed ones and seeds 1000 to 1299), 303 passed, in 27 min 20 s. Each scenario runs two or
three workers over 300 notifications on two channels with a limit of 40 per second, through
random faults: claims returned late, result writes that fail or whose answer is lost, 503, 429
and 422 answers, latency, connections closed without an answer, the provider closing kept-alive
connections, slow TLS handshakes. In all: 90,900 notifications, every one final; 100,725
requests received by the provider; the peak 40 per channel within one second, the limit, with
no request later than the margin after its permit; no claim without its attempt row and no row
reaped. 20 notifications in 8 scenarios were accepted twice, each after an attempt that ended
without a response (`[no response sent]`): the provider had processed a request whose answer
the test's own fault cut off, at-least-once case 3 of architecture §6, which the test allows
and logs. Count: `grep -c 'FAULT_RESULT.*passed=true'` on the output.

A second run of 300 (`FAULT_SEED=2000`, the same command otherwise; 27 min 39 s): 302 of 303
passed, 101,411 requests received, 22 notifications in 14 scenarios accepted twice, each after an
attempt without a response. The one red scenario, seed 2268, failed on the rate limit: the
provider received 47 email requests within one second against a limit of 40. Its own counters
show that 10 email requests arrived more than the 50 ms margin after their permits (the latest
144 ms); the excess, 7, is within those 10. That fits the frozen-process case of architecture
§7's limits table, but where the process was held is not shown: the test runs the workers and
the recording server in one process, and "late" is measured from a permit's return to the
server's stamp, so a stall in the worker before its write and a stall in the server before it
stamped look the same, and one before the permit returned is not counted at all (session 8's
review). The same seed run alone five more times passed five times, with no request later than
10 ms (`FAULT_RUNS=1 FAULT_SEED=2268 go test -count=1 -run
'TestInvariantsHoldUnderRandomFaults/seed-2268' -v ./internal/dispatch`); session 8's reviewer
ran it eight more times, three quietly and five with the CPUs oversubscribed twice, and 103 new
scenarios, all passing. In the author's two runs together: 606 scenarios, 605 passed, every
notification final, and no notification accepted twice except after an attempt without a
response.
