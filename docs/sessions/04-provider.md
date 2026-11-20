# Session 4: provider client and the send gate

- **Date:** 2026-11-20.
- **Lead:** Claude Opus. **Reviewer:** one fresh Claude Opus instance, run once after the
  code was green ([review record](../reviews/04-session-4-review.md)).
- **Brief:** [plan.md, "4. Provider client and the send gate"](../plan.md#4-provider-client-and-the-send-gate).

## What was built

| Part | What it is | Evidence |
|---|---|---|
| `internal/provider` | `New(Options)` and `Send(ctx, message, gate) Result` of architecture §7 | 17 tests in `provider_test.go`, through real connections to the recording server; four of them also use the real limiter on Redis |
| The request | The assignment's: `POST`, body `{"to", "channel", "content"}`, the notification id as `Idempotency-Key`, `X-Correlation-ID` when set | TestTheRequestHasTheAssignmentsFormat |
| The send gate | The gate runs in the `GotConn` hook: once, after the connection is ready, before the write, on a context that is a child of the send's, limited to the gate budget. A refusal closes the connection inside the hook | I8 and A1 tests (§11, §12) |
| The transport | `GetBody = nil` (no replay), no redirects, HTTP/1.1 only, no proxy, one idle connection per slot for the provider's host, idle timeout, TLS handshake timeout | A2 test, TestRedirectsAreNotFollowed, TestTheClientSpeaksHTTP11ToAServerThatOffersHTTP2, TestTheClientKeepsAnIdleConnectionForEverySlot, TestIdleConnectionsAreDroppedAfterTheIdleTimeout, TestTheTLSHandshakeHasItsOwnTimeout |
| One deadline | The send timeout on the caller's context covers the connection, the gate, the write, the response's headers and its body; a 101's raw connection is not read | TestOneDeadlineCoversConnectGateWriteAndResponse, TestAGateThatWaitsPastTheDeadlineTakesNoPermit |
| `Result` | What was observed and nothing else: response or not, status, `Retry-After`, `messageId`, at most 4 KiB of body; the gate's error when it refused; whether the connection was reused; the error; the duration | TestTheResultCarriesWhatWasObserved |
| Bounds (§8) | Response headers 64 KiB; body kept up to 4 KiB | TestResponseHeadersOverTheBoundAreNotAResponse, TestTheResponseBodyIsReadUpToItsBound |
| `make guards` | 21 guards of the provider client (19 before the review) | see "Measured" |

The client is not wired into the worker yet. Its caller is the dispatcher's `handle`, with
`limiter.Wait` as the gate (session 5). This closes session 2's open item: the client bounds
what it reads from a provider's error body.

## How the tests came first

The client's details went into §7 first (`ec1091e`). Before that, the client was written and
measured outside the history, because one design question needed an answer from the real
client: is the cancel after the close needed? With the close alone, 0 of 20,000 refused sends
reached the server; with the cancel and no close, 5, 5 and 8 of 10,000 did. The cancel adds
nothing, so §7 changed before any code was committed (CLAUDE.md, rule 11).

The 17 tests were committed against a stub that compiled and returned an error (`e1fdb73`):
16 failed (`go test -count=1 -v ./internal/provider`). The one that passed,
TestTheTLSHandshakeHasItsOwnTimeout, expects an error without a gate call, which the stub
returns; a guard checks it against the real client. The code followed in `a75c0cc`.

## Two lessons about tests

**A guard must fail for the reason it names.** All 19 guards turned their test red on the
first run. Reading why, two did so by accident. With the gate moved to `WroteRequest`, it runs
on the transport's write loop; the provider answered and `Send` returned while the gate was
still running, so the test read the gate's results before they existed and failed later, on
something else. The test now fails at once when `Send` returns before its gate (`0358d2a`).
The A2 test stopped at a check of the reused connection before it counted the gate calls; the
count comes first now, and the guard shows the real failure: two gate calls in one send, and
a 202 for a request whose first write was lost.

**A test whose guarantee depends on a timing assumption must say so.** CI ran `make test` with
`-race` and the I1 test saw 106 requests within one second (run 44730514192). The race
detector and the other test packages slowed the client past the 50 ms margin, the case that
§7's limits table already names. The A4 test already asserted the margin only without the race
detector; the I1 test, which depends on A4, did not. It now checks the guarantee as §7 states
it (`6062021`).

## What was measured

| What | Result | Command |
|---|---|---|
| Tests | 137 test functions (120 after session 3, 17 new); all pass with the race detector | `make test`; count: `grep -rh '^func Test' --include='*_test.go' . \| grep -v TestMain \| wc -l` |
| Lint | 0 issues | `make lint` |
| Guards | 132 guards (111 of sessions 1 to 3, 21 of the provider client). Locally, before the review: 130 of 130 real, 8 min 44 s; after it, the provider's 21 of 21. The whole list after the review, in CI on Linux: 132 of 132 real, 0 failed (see "CI") | `time make guards`; `go run ./tools/guards -only provider` |
| Test names in the documents | all exist | `make check-docs` |
| A1: refused sends through the real client | 0 of 10,000 (5,000 new, 5,000 after an allowed send, 3,800 to 4,800 of them on a reused connection) reached the recorder; now 2,500 per half | `go test -count=1 -v -run TestARefusedSendNeverReachesTheProvider ./internal/provider` |
| A1 without the close, the cancel still there | 5, 5 and 8 of 10,000 reached it; with neither, all of them | the same, with the close removed by hand |
| A1 with the close and without the cancel | 0 of 20,000 (two runs) | the same, with the cancel removed by hand, before `ec1091e` |
| A2 with `GetBody` left as Go sets it | two gate calls in one send; the client saw a 202 | `go test -count=1 -v -run TestADeadReusedConnectionIsNotReplayed ./internal/provider`, mutation by hand |
| A4 without the race detector, 4 runs | new connections: median 0.15 to 0.42 ms, max 2.3 to 9.9 ms; reused: median 0.06 to 0.16 ms, max 3.0 to 6.6 ms | `go test -count=4 -v -run TestPermitToArrivalStaysWithinTheMargin ./internal/provider` |
| A4 with `-race`, 6 runs | max 14 to 41 ms new, 10 to 29 ms reused; the reviewer, under load: 71.7 ms | the same with `-race -count=6` |
| I1 at the provider, 3 workers, 150 senders, 3.3 s | peak 100 within one second in every local run; 106 once under `-race` in CI, with late requests | `go test -race -count=3 -v -run TestTheProviderReceivesAtMostTheLimitInAnyWindow ./internal/provider` |

## What the review found

Two major findings, three minor and four nits; all fixed or answered. The record, with each
resolution and the test or guard that verifies it, is
[04-session-4-review.md](../reviews/04-session-4-review.md). In short:

- **CI was red** (F1): the I1 test under `-race`, fixed before the review returned.
- **A 101 response escaped the one deadline** (F2). Go hands the raw connection to the caller
  and the context no longer closes it; `Send` blocked until the provider closed it. The
  client reads a body only from a final response; the deadline test now covers the body and
  a 101.
- **The provider's keep-alive can run out while a send waits at the gate** (F3): stated in §7
  and as a relation in §13; how `Classify` counts it is session 5's decision.
- **The A1 test ran out of ports when run twice in a row on macOS** (F4): 2,500 per half.
- **A measurement was stated under the wrong condition** (F5): the 5, 5, 8 leaks were with the
  cancel and no close; corrected.
- Smaller: the transport discards up to 256 KiB of a long body after close; a message that
  cannot be sent looks like a failed connection; test gaps; a dead branch.

## What changed in the design

All in [architecture.md](../architecture.md):

- **§7, before the code:** property 2 is "close the connection", without the cancel, with the
  measurement; properties 1, 3 and 4 cite the `net/http` lines they rely on; "The provider
  client in detail": the request, the one deadline and the gate's child context, `Result`, no
  proxy, the bounds.
- **§8:** the bound on what the worker reads from a provider's response.
- **§11, §12:** the test names of I8 and of I1 at the provider's side; A1 and A2 permanent
  tests; A4 measured.
- **§7, §11, §12, §13, after the review:** a 101's body is not read; time at the gate counts
  as idle time for the provider, and the relation of §13; the A1 figures under their real
  conditions; what the transport discards after close; I1 at the provider holds while A4
  holds.

## What is still open

- I1 and A4 through the dispatcher, with a worker's real load: session 5.
- `Classify` (session 5) must know that "no response" covers three different things: the
  provider did not answer; the client's own pooled connection was closed by the provider while
  the send waited at the gate (review F3); and a message that was never sent because its
  request could not be built (review F7). The Result carries `ConnReused` and the transport's
  error for the first two.
- A gate budget that ends during a script call is a limiter error, which opens the breaker
  (§6). Whether that happens under ordinary contention is measured with A5 in session 5.
- The dial timeout is set but not tested: a dial that hangs needs an address that drops
  packets, which a test on one machine cannot rely on (§7).
- The client does not log and has no metrics yet. Session 7 adds the wait at the gate, the
  refusals and the connections closed during the gate.

## CI

CI on `44e454b`, run 44730514192: the test job failed (review F1). CI on `6062021` (the I1
test's fix), run 44732001124: all four jobs succeeded. CI on the session's last code and
document commit, "docs: session 4 is done, session 5 is next", run 44734084083: lint, test,
guards and compose all succeeded; the guards job printed `132 guards, 132 real, 0 failed`
(`gh run watch 44734084083 --exit-status`, and `--log` of the guards job). The commit that
adds this section changes only this file.
