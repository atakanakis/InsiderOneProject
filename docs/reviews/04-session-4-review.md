# Review of session 4: provider client and the send gate

- **Date:** 2026-11-20.
- **Reviewer:** a fresh Claude Opus instance, read-only, with the rules of
  [`.claude/agents/reviewer.md`](../../.claude/agents/reviewer.md). It was given the assignment
  text, the session's brief and definition of done in [`plan.md`](../plan.md), and a clean
  clone of the repository. It was not given the author's reasoning or conclusions.
- **Scope:** commits `ec1091e..44e454b` (everything after `0621c19`, the last commit of
  session 3): `internal/provider`, its guards in `tools/guards/list.go`, and the document
  changes.
- **Runs:** once, after the code was green, as the plan requires. There was no second round.
  Each fix below is verified by its own named test and by `make guards`, not by another
  review.

## What the reviewer ran

- `go build ./...`, `go vet ./...`, `make test` with `-race` (pass), `make lint` (0 issues),
  `make check-docs` (pass), `make guards` once: "130 guards, 130 real, 0 failed", with a clean
  tree afterwards.
- CI run 44730514192 on `44e454b`: lint, guards and compose passed; the test job failed (F1).
- In a throwaway copy: a provider that answers `101 Switching Protocols` and then holds the
  connection; a body that arrives slowly; a provider with a 2 s keep-alive; the A1 test
  twice in a row; the close removed from the current code; a 200 KiB chunked error body;
  a correlation id with a newline; A4 and I1 under `-race` with 14 busy processes on 8 CPUs;
  and every `net/http` line number the documents cite.
- It could not check a Linux host locally, the dial timeout, or a real provider over a real
  network.

## Verdict, as reported

The send gate does what the brief asks: the gate runs once, after the connection is ready and
before the write; no refused send reached the server; a dead reused connection is not
replayed; redirects are not followed; HTTP/1.1 holds; all guards are real. The session was
not done: CI was red (F1), one kind of response escaped the one deadline (F2), and some
numbers in the documents did not match the code. Two major findings, three minor, four nits.

## Findings and resolutions

| # | Severity | Finding | Verified by the reviewer | Resolution | Verified by |
|---|---|---|---|---|---|
| F1 | major | CI was red: under `-race` on the runner, TestTheProviderReceivesAtMostTheLimitInAnyWindow saw 106 requests within one second (CI run 44730514192). Under `-race` and CPU contention, permit to arrival passes the 50 ms margin (71.7 ms measured); A4 already gave up its assertion under `-race`, the I1 test, which depends on A4, did not. | Yes | The lead had found the same CI failure before the review returned and fixed it in `6062021`. Class: a test whose guarantee depends on A4. The test records every permit and counts the requests that arrived more than the margin after it; it always checks peak ≤ limit + late, which is how section 7's limits table states the guarantee, and without the race detector, as `make guards` runs it, peak ≤ limit. §11 says I1 at the provider holds only while A4 holds; §12 adds the reviewer's 71.7 ms. CI on `6062021` was green (run 44732001124). | TestTheProviderReceivesAtMostTheLimitInAnyWindow; guard "every send passes the gate (I1)" |
| F2 | major | A `101 Switching Protocols` response escaped the one deadline. Go's transport hands the raw connection to the caller as the body (`transport.go` line 2732) and the request's context no longer closes it, so reading the body blocked until the provider closed the connection; with a 500 ms send timeout, `Send` was still running after 5 s. A slot would be held that long, and the lease and shutdown relations of §13 assume the send ends by its timeout. The deadline test only delayed the headers. | Yes | Class: a part of a send that the deadline does not cover. The client reads a body only from a final response (status 200 or more); a 101 is closed unread and is "any other response" for `Classify`. §7 states it. The deadline test gains the two parts it lacked, a body that never finishes and a 101, through a provider written by hand. | TestOneDeadlineCoversConnectGateWriteAndResponse (subtests "response body" and "switching protocols"); guard "a 101's connection is not read (review F2)" |
| F3 | minor | Time at the gate is idle time for the provider. A connection idle for a while, then held at the gate, can pass the provider's keep-alive timeout; the provider closes it, the permit is spent and the attempt fails with no response. With a 2 s keep-alive: idle 1.5 s, gate 1 s, no response. §7's "the short idle time keeps this rare" ignored the gate. | Yes | No code change: the client cannot know the provider's keep-alive, and redialling after a long gate wait would add a part. §7 states the cause; §13 adds the relation `IDLE_CONN_TIMEOUT + GATE_BUDGET` under the provider's keep-alive to the relations a worker cannot check; session 5 decides how `Classify` counts it, session 7 counts it in a metric. | Documents only |
| F4 | minor | The A1 test failed when it ran twice within about 30 s on macOS: each run left about 10,000 client ports in TIME_WAIT, out of about 16,000. | Yes | 2,500 refusals per half; the order test covers the mechanism deterministically. §12 says why. Two runs in a row pass (`go test -count=2 -run 'TestARefusedSendNeverReachesTheProvider$' ./internal/provider`). | TestARefusedSendNeverReachesTheProvider |
| F5 | minor | §7, §12 and the experiment note said that "without the close (the cancel already gone)" 5, 5 and 8 of 10,000 refused sends reached the server. With the close removed from the current code, all 10,000 did. The 5, 5, 8 came from a run with the cancel still there. | Yes (by hand) | The documents now say: with the cancel and no close, 5, 5 and 8 of 10,000; with neither, all. The conclusion, that the close alone is enough, stands. Commit `ec1091e`'s message keeps the wrong condition; this record corrects it. | Documents only |
| F6 | nit | "Body 4 KiB, the rest not read" was not what Go does: when the client closes a long body, the transport discards up to 256 KiB more, for at most 50 ms, and keeps the connection if that reaches the end (`transport.go` lines 2420 to 2448). A 200 KiB error body was read to the end and the connection reused. | Yes | Text corrected in §7 and §8: the client keeps at most 4 KiB; the transport may discard up to 256 KiB more within 50 ms. Memory and time stay bounded. | TestTheResponseBodyIsReadUpToItsBound (unchanged) |
| F7 | nit | A message that cannot be sent at all (a correlation id with a newline) gives the same Result as "could not connect": no response, no refusal, an error. `Classify` would retry it and count it against the breaker. Reachable only past the API's correlation id bound (§8). | Yes | Not changed. Listed under "What is still open" in the session record for session 5: "no response" includes "never attempted". | — |
| F8 | nit | Test gaps that the guards would not catch: no Retry-After date in the past; no test of the deadline during the body (F2); the A1 and A2 tests counted leaks by the channel read from the body, which misses a request whose body was cut. | Reasoned | A Retry-After date 30 s ago is tested (0) and guarded; the deadline test covers the body (F2); the A1, A2 and close-order tests count every request the recorder received. | TestTheResultCarriesWhatWasObserved, TestARefusedSendNeverReachesTheProvider, TestADeadReusedConnectionIsNotReplayed, TestARefusedSendsConnectionIsClosedBeforeTheTransportGoesOn; guard "a Retry-After date in the past is 0" |
| F9 | nit | The error branch of `json.Marshal` of three strings could not run. | Yes | Removed. | — |

## Claims the reviewer reproduced

- A1 through the real client: 0 of 10,000 refused sends reached the recorder; 4,797 of the
  "reused" half ran on a reused connection.
- A2: one gate call, nothing received, an error.
- A4 without the race detector: maxima 5.4 to 8.3 ms on new and 5.9 to 6.5 ms on reused
  connections, inside the documented ranges.
- The 19 guards of commit `570d16e` are real; 130 of 130 in the whole list.
- Every `net/http` line number cited in §7 matches Go 1.27.4.

## Questions the reviewer asked, and the answers

1. *Is 50 ms the right margin, given 71.7 ms under `-race` and load?* Under CPU starvation A4
   does not hold; that is the "frozen worker" row of section 7's limits table, and the I1 test
   now checks the guarantee in that form. The figure for a real worker with 50 slots is
   measured through the dispatcher in session 5; if it is near the margin, the margin is the
   setting to change, at the price of throughput (§7).
2. *A4 is measured from `Wait` returning, not from the Redis time of the grant. Who bounds
   the reply leg?* Nothing measures it separately. §12 says the reply is outside the
   measurement; the I1 test, which uses only the provider's stamps, covers the whole chain.
3. *Which code path guarantees that `Send` ends by the send timeout for every response?* The
   context covers the connection, the gate, the write and the response; the transport stops
   honouring it only for a protocol switch, whose body the client no longer reads (F2). The
   deadline test covers each part, the body and the 101 included.
4. *Could a gate budget that ends during a script call, a limiter error, open the breaker
   under ordinary contention?* Open; it is measured with A5 in session 5.
5. *How will `Classify` tell "the connection died at the gate" (F3) from "the provider is
   down"?* Open for session 5. The Result carries `ConnReused` and the transport's error.
6. *Why was the I1 test exempt from the race-detector rule that A4 used?* It should not have
   been (F1).
