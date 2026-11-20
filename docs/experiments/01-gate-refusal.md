# Experiment 01: can a refused request be stopped for certain?

**Date:** 2026-11-20, before the architecture was fixed.

## Question

The provider client takes its rate-limit permit when the HTTP connection is ready
(`httptrace.GotConn`). The hook cannot return an error. When no permit can be had, how does
the client stop the request, and is it certain that nothing reaches the provider?

The first version of this project cancelled the request context in the hook and stated in a
commit message that "the request is not written". A later review showed that statement was
false. This time the question was measured before the design depended on the answer.

## Method

A local HTTP server counts the requests it receives. A client refuses every request in
`GotConn`, in two ways:

- **cancel only:** cancel the request context.
- **close and cancel:** close the connection the hook was given, then cancel.

Each way was run on new connections ("cold") and on reused ones ("warm"), HTTP/1.1,
32 requests in parallel. The code is in
[`01_gate_refusal_test.go.txt`](01_gate_refusal_test.go.txt).

## Result

| Way | Connection | Refused | Reached the server |
|---|---|---|---|
| cancel only | cold | 20,000 | 3 |
| cancel only | warm | 20,000 | 10 |
| close and cancel | cold | 20,000 | **0** |
| close and cancel | warm | 20,000 | **0** |

An earlier run of 3,000 per row gave 2, 1, 0 and 0.

## Conclusion

- Cancelling alone leaks. The transport hands the request to its write loop before it looks at
  the cancelled context, so now and then the request is written.
- Closing the connection first leaves the write nowhere to go.
- The design therefore closes and cancels, and uses HTTP/1.1 only so that one connection
  carries one request.

## Limits of this experiment

- A local server and one machine. It shows the mechanism; it is not a proof for every network.
- Zero in 40,000 is strong evidence, not certainty. If a refused request did leak, the
  provider would receive one request without a permit, and would receive the same message
  again when the refused attempt is retried (with the same idempotency key). Nothing would be
  lost.
- Session 4 turned this into a permanent test of the real provider client
  (TestARefusedSendNeverReachesTheProvider, architecture §12, A1). Through that client the
  close alone was enough: 0 of 20,000 refused sends arrived, so the cancel was removed.
