# Lessons from the first version

The first version of this project is kept as it was at
[InsiderOneProjectDeneme](https://github.com/atakanakis/InsiderOneProjectDeneme) ("deneme" is
Turkish for "trial"). It works, and it was reviewed five times by independent reviewer agents.
Each review found real defects. This page lists what those defects had in common and what the
rebuild does differently because of them.

The lessons are about classes of mistakes. A list of fixed bugs would only describe the past.

## How the first version went wrong

It was designed and built in one long session, feature by feature. The reviews came after the
code. Each review finding was fixed where it was found. Three patterns followed:

1. **A fix answered one reproduction.** The next review found the same class of defect one
   step to the side: per batch where the fix was per item, on the error path where the fix
   was on the success path.
2. **Fixes broke other guarantees.** Two of the three major findings in the second review were
   inside the commits that fixed the first review.
3. **The core grew by one guard per round.** The dispatcher ended with seven separate paths
   that give back a rate-limit permit or a breaker reservation. It passed its tests, and the
   author could no longer explain it in a few sentences.

## The lessons

### Design

| # | What happened | The class | What the rebuild does |
|---|---|---|---|
| 1 | The rate limit was enforced when a send was *started*. A database stall, then a slow new connection, each let the provider see more than the limit. It took three rounds to close. | A guarantee enforced far from where its effect happens | The permit is taken in one place, at the last moment before the request is written. No other code touches permits. ([Architecture §7](architecture.md#7-the-rate-limit)) |
| 2 | Permits were taken before rows were claimed, so every path that did not send had to give permits back. Seven such paths existed, and reviews kept finding paths where the hand-back was missing. | Reserving ahead, then refunding by hand | Nothing is reserved ahead. The dispatcher only reads a hint, so there is no permit to give back. The same rule is applied to attempts: the counter only goes up. ([§5](architecture.md#5-the-queue-five-operations), [§6](architecture.md#6-the-dispatcher)) |
| 3 | A performance fix split the claim into three statements and lost atomicity. Wrapping them in a transaction then let one failing priority block the others. Savepoints fixed that and made the claim return rows and an error together. | Several statements treated as one result, so a failure in a later one dropped the rows of an earlier one | A claim is one statement for one priority. The loop over priorities is in the dispatcher, and the rows of each claim are started before the next claim is made. No transaction, no savepoint, no combined result. ([§5](architecture.md#5-the-queue-five-operations)) |
| 4 | A claimed row could be left without an owner on several rare paths (partial claim, abandoned wait, shutdown). | Many exits from one piece of work | Every claimed row goes through one function that ends in one call. Shutdown does not cancel sends or queue writes. ([§6](architecture.md#6-the-dispatcher)) |
| 5 | When the permit check refused a request, the code replaced the result without looking at it, so a confirmed delivery could be thrown away and sent again. | Deciding an outcome from intent, not from evidence | The verdict is decided in one function from what was observed. A response always wins. One send writes at most one request, so there is only one thing to observe. ([§6](architecture.md#6-the-dispatcher), [§7](architecture.md#7-the-rate-limit)) |
| 6 | A template with thousands of placeholders, times a batch of 1000, made the server allocate gigabytes. The fix capped one item, then one request, and then the *error message* for the same input turned out to be 68 MB. | Work or output that grows with input, with no stated limit | A bounds table: every input and every output has a limit, written down before the endpoint exists, each with a test. ([§8](architecture.md#8-the-api)) |
| 7 | A result write that failed once after a successful send caused a second send. | Treating the write of a result as something that cannot fail | Result writes are retried until the database answers, and this is part of the first design of the dispatcher. ([§6](architecture.md#6-the-dispatcher)) |

### Evidence

| # | What happened | The class | What the rebuild does |
|---|---|---|---|
| 8 | A commit message said a cancelled request "is not written". A reviewer showed it sometimes is. | A claim written down without a measurement | An assumption is listed as an assumption until it is measured. The one that mattered most was measured before the design was fixed. ([§12](architecture.md#12-assumptions-and-how-each-is-proven), [experiment 01](experiments/01-gate-refusal.md)) |
| 9 | The verification page said "no guard is untested". A reviewer found two guards whose removal no test noticed. | Documentation that drifts ahead of the tests | Each invariant names its tests. A CI script checks that every test named in the documents exists. The guard check (below) is in the repository and anyone can run it. |
| 10 | Most rate-limit tests used a fake provider. A fake has no connection, so those tests could not see the slow-connection defect, and the permit check at the connection did not run in them at all. Another test stamped a request's arrival after a simulated delay. | Measuring a guarantee in the wrong place | Delivery tests use the real provider client against a recording server that stamps each request when it arrives. ([§11](architecture.md#11-invariants)) |
| 11 | Tests passed, and it was unknown which line each test protected. Checking by hand found two "protections" that were only build failures. | Passing tests mistaken for protecting tests | `make guards` disables each guard in turn and expects a named test to fail. A session is not done until its guards are in that list. |
| 12 | Two alert rules could never fire. | Configuration that is never executed | Each alert rule has a `promtool` test. ([§10](architecture.md#10-observability)) |

### Process

| # | What happened | The class | What the rebuild does |
|---|---|---|---|
| 13 | Reviews came after the code, so every finding became a patch. | Finding design errors when they are most expensive | The design is reviewed before any code exists. Each build session is reviewed while its change is small. |
| 14 | One long session held the whole system. Details of early parts were lost by the time later parts were built. | Too much in one context | One session builds one part, from a written brief, with a fresh context and a definition of done. ([Plan](plan.md)) |
| 15 | The invariants were discovered by reviewers, one failure at a time. | Guarantees found by accident | The invariants are written first. The randomised fault test that checks them is written before the dispatcher. |
| 16 | The commits arrived in clusters, which says little about how the work went. | History that does not tell the story | One commit per finished step, made when the step is finished. |
| 17 | The core became hard for its owner to explain without the agent's help. | Code the owner cannot defend | The dispatcher has to fit on one screen. A walkthrough follows one message through the code. The candidate reviews every session's result before the next one starts. |

## What the first version got right, and is kept

- Postgres as the queue, with `FOR UPDATE SKIP LOCKED` and leases.
- The attempt number as a fencing token.
- The sliding-window script in Redis, on the Redis clock, with a window margin.
- At-least-once delivery, stated openly, with an idempotency key sent to the provider.
- The review loop itself: a reviewer with no knowledge of the author's reasoning, on a
  different model, read-only, required to reproduce every finding, and replaced by a fresh
  instance each round.
- Randomised fault testing against invariants: 203 scenarios, 60,900 notifications, none lost,
  none doubled, the limit never exceeded.

Proven pieces of the first version may be carried over, one at a time, each with its tests,
and each is named in the [journey](journey.md). Nothing is copied wholesale.
