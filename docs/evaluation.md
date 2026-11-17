# Integration exercise and baseline benchmarks

What it takes to build six bounded integrations on uma's public API, and
baseline timings for the engine's hot paths on the memory and PostgreSQL
stores. These are first measurements. They set no performance target and
claim nothing against other libraries.

## Environment

| | |
| --- | --- |
| Machine | Apple M1 Pro, 10 cores, 16 GiB, macOS 15.6.1 |
| Go | go1.26.1 darwin/arm64 |
| uma | `main` at `cd233d3`, plus the benchmark files |
| PostgreSQL | `postgres:18.4-alpine@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15` under OrbStack 2.2.3 (Docker 29.4.0, linux/arm64), via the `store/postgres` test container |
| Load | Shared with other concurrent test runs. The 1-minute load average stayed between 6 and 18 for the baseline runs and peaked above 100 during the isolation runs. Treat absolute numbers as upper bounds. The comparisons within one table were run back to back or interleaved. |

Every benchmark ran with `-count=5`; the tables report the median. Each
iteration count is fixed with `-benchtime=Nx` because the memory store's
cost per operation grows with the number of records it holds (see
[Memory store growth](#memory-store-growth)).

## Integration exercise

Each task was built as a third party would build it: a separate Go module
that imports only exported packages (`hop.top/uma`, `model`, `adapter`,
`policy`, `ports`, `store`, `store/memory`, `companions/bot`). It runs on the
memory store against a scripted chat provider over HTTP. Code written once
and reused by every task:

- a receive-and-send adapter for the provider (webhook receiver, `Send`, `Reconcile`), 189 lines;
- the host composition (clock, ID generator, credential port, authorizer, `uma.New`, channel, inbox and webhook setup, a delivery worker and shutdown), 132 lines.

Lines are non-blank, non-comment Go lines. "Host" counts the shared host
code plus the code the task adds to it.

| Task | Scope reached | Setup steps | Handwritten glue (adapter / host / test) | Time to first passing scenario / total | Recovery correctness |
| --- | --- | --- | --- | --- | --- |
| Text bot | Full: inbound text → bot handler → reply delivered by the worker | 5 | shared / 132+38 / 185 | 63 s / 63 s | Pass: a crashed handler replays its reply once; a stopped dispatcher's batch is redelivered to its successor and posted once; a reply accepted before a restart is delivered once and not again after a second restart |
| Second account | Full: two channels of one adapter type, two credentials, overlapping provider IDs | 3 | shared / 132+0 / 131 | one red run / 58 s | Pass: the same provider event ID is deduplicated per account only; messages and replies stay on their own account |
| Identity link | Partial: linking only through the store, not the public API | 6 | – / 132+78 / 193 | 31 s / 91 s | Pass at store level: a crash before commit leaves nothing and the retry links once; a lost answer replays; consumed evidence and stale revisions are rejected, but only because the host enforces it |
| Custom adapter | Full: external adapter registered with no core change; capabilities match what it implements | 4 | 189 / 132+0 / 197 | 122 s (includes writing the shared adapter and host) / ~5 min | Pass: an outcome-unknown send is settled by `Reconcile` with one provider call; a transient failure is retried 3 times and posted once; a redelivered webhook is deduplicated |
| Private reply | Full: internal note stored, readable only by authorized actors, never sent to the provider | 4 | shared / 132+24 / 148 | 44 s / 44 s | Pass: replaying the note command returns the stored result; the same operation ID with another payload is a conflict |
| Failed-processing replay | Full: dead-lettered reply, `Operations.Replay`, delivery once the provider recovers | 5 | shared / 132+17 / 165 | second run / 300 s | Pass: after two unfinished claims the reply is dead-lettered; a replay posts it once; a replay of an applied-but-unanswered send reconciles with one provider call; replaying a queued or settled operation is a conflict |

Times are the implementer's wall clock from starting the task to the first
green `go test`. The implementer already knew the engine's internals, so
the times are a lower bound for a newcomer, not a measure of one.

### What every host has to supply

- A `ports.Clock` and a `ports.IDs`. There are no defaults, so every host
  writes about 20 lines for a system clock and a UUID generator.
- One `Runtime.Run` worker goroutine to deliver replies, and the HTTP mux
  that mounts the handler from `Runtime.Webhook`. The bot also needs the bot
  companion's dispatch loop.
- A `Call.Correlation` on every read. The first runs failed with
  `correlation: required`.
- No custom persistence, except for identity linking.

### Gaps the exercise hit

- `Contacts.List`, `Contacts.Identities` and `Contacts.Link` returned
  `unsupported` at the time. The identity-link task
  had to write link records through the store from host code, deriving an
  idempotent record ID from the operation ID. That path skips the
  authorizer, the operation ledger and domain events: no identity-linked
  event is emitted. The second-account task checked isolation through
  message authors because it could not list identities.
  **Since then: implemented**, together with `Contacts.Unlink` and
  `Contacts.History`; see [API coverage](api-coverage.md#contacts) for
  status and store coverage, and
  [Contacts and identity links](../README.md#contacts-and-identity-links).
- Identity verifiers registered in `Config.IdentityVerifiers` were
  validated at composition but never called. **Since then:** `Link` and
  `Unlink` call the verifier registered for the evidence kind before any
  policy decision.
- `Messages.Edit`, `Delete` and `React` return `unsupported` whatever the
  adapter declares, so they cannot show a capability-based rejection.
  `Interactions.Signal`, `Streams.Start` and `Runtime.Receive` do: on an
  adapter that does not declare them they reject with `unsupported`.
- There is no single-step runtime call. Recovery tests run `Runtime.Run` in a
  goroutine, poll for state, and advance a fixed clock to expire leases.
- Restarts were simulated by building a new engine on the same memory store
  in the same process. Durable restart behaviour was not exercised here; the
  `store/postgres` crash tests and the inbox example's smoke tests cover it.

### Not covered

The same six tasks have not yet been built against a second library. This
table describes uma alone and supports no claim that integration is easier
or faster than with an alternative.

## Benchmarks

Benchmarks live next to the code they measure and run only with `-bench`:

| Benchmark | Package | Measures |
| --- | --- | --- |
| `BenchmarkIngestCommit` | `internal/ingress`, `store/postgres` | One verified inbound message from `Sink.Accept` to its committed unit. `first_contact`: a new sender each time (contact, identity and conversation created). `existing_conversation`: one sender, one locked conversation. |
| `BenchmarkReplyDispatch` | `internal/runtime`, `store/postgres` | `accept`: `Messages.Reply` committed as durable intent. `dispatch`: one reply through a `Runtime.Run` worker to the provider call (`send-µs`) and back to an idle worker (ms/op). |
| `BenchmarkEventDelivery` | `internal/events`, `store/postgres` | `ready`: `Poll` and `Ack` of an already committed event (`poll-µs`). `wakeup`: commit while a long poll waits; commit-to-delivery latency percentiles. |
| `BenchmarkWriteGrowth` | `store/memory` | The first write of a unit against a store of a pinned size, rolled back so the size holds. `put_message/messages=N`: a new message into N messages in N/10 conversations. `put_conversation/conversations=N`: a new conversation into N conversations. `heap-B/record`: live heap of the built fixture per message. |
| `BenchmarkReadGrowth` | `store/memory` | `Messages` list over N messages in N/10 conversations. `first_page`: 50 messages of one conversation. `last_page`: 50 messages after the cursor of the final message. |
| `BenchmarkIngestGrowth` | `internal/ingress` | `Sink.Accept` on the memory store after filling it with `stored=N` messages through `Accept` (untimed). `first_contact` and `existing_conversation` as in `BenchmarkIngestCommit`. The measured messages are committed, so run it with a small `-benchtime=Nx`. |
| `BenchmarkClaimWork` / `BenchmarkClaimWorkContention` | `store/memory` / `store/postgres` | 1, 4, 8 and 16 concurrent claimers draining one queue at the store level (on PostgreSQL each claimer has its own pool): time per item, claim units per claim, share of claims needing more than three units. |
| `BenchmarkRunWorkers` | `store/postgres` | 1, 4, 8 and 16 `Runtime.Run` workers, each an engine on its own pool, draining queued replies with the default one-second poll. |
| `BenchmarkMessageSearch` | `store/postgres` | `Messages.List` with search fields over a pinned fixture (20,000 messages in 200 conversations, written by position, nothing hidden from the caller). `first_page/*`: one page of 50 for rare and common text, two words, text with a date window, `Has`, a media type, a label, a label with text and no search at all. `walk/*`: the whole result for rare and common text (`hits` is the messages listed). |

Reproduce:

```sh
GOWORK=off go test -run='^$' -bench=IngestCommit -benchtime=1000x -count=5 ./internal/ingress
GOWORK=off go test -run='^$' -bench=ReplyDispatch -benchtime=1000x -count=5 ./internal/runtime
GOWORK=off go test -run='^$' -bench=EventDelivery/ready -benchtime=1000x -count=5 ./internal/events
GOWORK=off go test -run='^$' -bench=EventDelivery/wakeup -benchtime=20x -count=5 ./internal/events
GOWORK=off go test -run='^$' -bench=ClaimWork -benchtime=500x -count=5 ./store/memory
GOWORK=off go test -run='^$' -bench='WriteGrowth|ReadGrowth' -benchtime=20x -count=5 ./store/memory
GOWORK=off go test -run='^$' -bench=IngestGrowth -benchtime=20x -count=5 -timeout=60m ./internal/ingress
cd store/postgres   # Docker required; UMA_REQUIRE_DOCKER=1 fails instead of skipping
GOWORK=off go test -run='^$' -bench='IngestCommit|ReplyDispatch|EventDelivery/ready|ClaimWorkContention' -benchtime=300x -count=5 .
GOWORK=off go test -run='^$' -bench=EventDelivery/wakeup -benchtime=20x -count=5 .
GOWORK=off go test -run='^$' -bench=RunWorkers -benchtime=200x -count=5 -timeout=60m .
GOWORK=off go test -run='^$' -bench=MessageSearch -benchtime=20x -count=5 .
```

### Ingest, reply and event delivery

Memory: 1000 iterations. PostgreSQL: 300 iterations.

| Path | Memory | PostgreSQL |
| --- | --- | --- |
| Ingest, first contact | 0.44 ms (2146 allocs) | 6.0 ms (658 allocs) |
| Ingest, existing conversation | 0.26 ms (1103 allocs) | 4.4 ms (545 allocs) |
| Reply accepted (durable intent) | 0.25 ms | 3.4 ms |
| Reply → provider call | 0.35 ms | 11 ms |
| Reply → provider call → idle worker | 0.63 ms | 18 ms (5 runs ranged 12.7–31) |
| Poll + Ack, event already committed | 0.11 ms | 5.2 ms |
| Commit → long-poll delivery, p50 / p95 / max | 528 / 1000 / 1000 ms | 532 / 1000 / 1000 ms |

- Each PostgreSQL row is a few SERIALIZABLE units, each a commit to a
  containerized server. A dispatch is about five of them: reply, claim,
  attempt record, provider answer, empty claim.
- **Event delivery waits for a rescan, not for the store.** A waiting
  `Poll` rescans the log once per one-second step and is never woken by a
  commit. Latency is spread evenly over 0–1 s on both stores: a median of
  about half a second, and a full second at worst. Hosts that need faster
  delivery today can poll more often without waiting, at about 5 ms per
  empty poll on PostgreSQL. A commit notification (in-process for one
  engine, `LISTEN/NOTIFY` across PostgreSQL processes) would bring delivery
  down to the `ready` cost.

### Memory store growth

The memory store copies a table on the first write in each unit, so the
cost per write grows linearly with the records stored and ingesting N
messages costs O(N²) in total. The marginal cost at pinned sizes, which
replaces the earlier means taken over a store that grew during the run, is
in [Memory store scaling](#memory-store-scaling). That fits the store's
stated scope (tests, demos, small bots that need no durability); a
long-running bot on the memory store slows down steadily and should move to
a durable store.

### ClaimWork contention

Store-level claim-and-complete loops, one item per claim unit. Every unit
that fails with `transient` is rerun. "Units per claim" is 1 when nothing
conflicts. "> 3 units" is the share of claims that needed more than three
units: one engine claim round retries a unit at most three times, then
gives up and waits for its next poll.

PostgreSQL, 300 items, SERIALIZABLE (as shipped):

| Claimers | In-flight cap | ms per item | Units per claim | > 3 units | Complete retries per item |
| --- | --- | --- | --- | --- | --- |
| 1 | none | 2.2 | 1.0 | 0 % | 0 |
| 4 | none | 3.5 | 5.1 | 16 % | 0.08 |
| 8 | none | 7.1 | 9.4 | 37 % | 0.18 |
| 16 | none | 8.3 | 16.5 | 57 % | 0.26 |
| 1 | 16 | 2.3 | 1.0 | 0 % | 0 |
| 4 | 16 | 5.2 | 4.8 | 9 % | 0.02 |
| 8 | 16 | 6.3 | 9.8 | 28 % | 0.05 |
| 16 | 16 | 7.6 | 18.0 | 51 % | 0.13 |

Memory store, 500 items: 1.0 / 4.2 / 6.8–7.1 / 7.7–9.0 units per claim at
1 / 4 / 8 / 16 claimers, with the same pattern. Its optimistic commit fails
any concurrent writer to the tenant.

With SERIALIZABLE, adding claimers makes the queue drain more slowly per
item, not faster. With 8 claimers a claim takes about 9–10 units, and about
a third of claims would exhaust one engine round's retries.

### Engine workers

`BenchmarkRunWorkers`, 200 queued replies, PostgreSQL. Median ms per reply
from the workers' start to the last provider call. "Idle waits" counts
one-second poll waits per reply.

| Workers | Shipped | Idle waits | rc-guarded claim (below) | Worst of 5 runs, shipped |
| --- | --- | --- | --- | --- |
| 1 | 11.3 | 0 | 10.7 | 11.7 |
| 4 | 10.7 | 0.035 | 7.9 | 23.4 |
| 8 | 10.1 | 0.07 | 8.7 | **300.6** |
| 16 | 14.4 | 0.23 | 16.0 | **299.4** |

- No reply was sent twice in any run.
- More workers do not raise throughput: one worker and sixteen both deliver
  a reply every 10–15 ms. Contention in the claim unit is not the whole
  cause, because the guarded READ COMMITTED claim barely moves the
  numbers. Where the remaining serialization comes from (the units after
  the claim: attempt record, provider answer, event append) was not
  isolated here.
- **Tail stall.** With 8 or more workers, some runs take about 60 s for 200
  replies (≈300 ms per reply). That matches the 1-minute default lease:
  after a claim, a unit that loses its serialization race leaves the item
  leased to a worker that no longer runs it, and nothing reclaims it until
  the lease expires. The same stall appears with the guarded claim, whose
  post-claim units are still SERIALIZABLE. The mechanism is inferred from
  the timing, not traced.

### READ COMMITTED with SKIP LOCKED for ClaimWork only

This was tested on a throwaway copy of `store/postgres`, not committed. A
unit whose first store call is `ClaimWork` runs at READ COMMITTED; every
other unit stays SERIALIZABLE. `ClaimWork` already selects pending rows
`FOR UPDATE … SKIP LOCKED`. Three variants:

- **ser**: the copy's mechanism (isolation set by `SET TRANSACTION` at the
  first call), every unit SERIALIZABLE. This is the baseline for the
  mechanism's extra statement.
- **rc**: READ COMMITTED, queries unchanged.
- **rc-guarded**: READ COMMITTED with two additions:
  - the lease upsert only takes a lease whose current holder has expired
    (`ON CONFLICT … DO UPDATE … WHERE until <= now`); a row it does not
    take is skipped;
  - when an in-flight cap is set, a transaction-scoped advisory lock per
    tenant and work kind serializes the cap count.

Correctness, on the copy:

| Check | ser | rc | rc-guarded |
| --- | --- | --- | --- |
| Full `store/postgres` suite (store and durable conformance, cross-pool claims, crash tests) | pass | **fail**: the cross-pool claims test sees one item leased twice while its lease was live | pass |
| Two queued items per resource, 8 claimers, 5 rounds: resources leased twice at once | 0 of 40 | **39–40 of 40**, and 98–109 grants for 80 items | 0 of 40 |
| In-flight cap 4, 8 claimers, nothing completed: items claimed | 4 | **8–10** | 4 |

Plain READ COMMITTED is unsafe. Each statement takes a fresh snapshot, so
the lease check (`NOT EXISTS` over live leases) can miss a lease that
another claimer committed after the check's snapshot was taken but before
this claimer locked the row. The unconditional upsert then takes the lease
over, and the in-flight count goes stale between reading and claiming.
SERIALIZABLE hides all of this today by aborting one of the conflicting
units.

Throughput, interleaved runs (one count per variant per round, five rounds;
PostgreSQL, 300 items):

Median ms per item, with claim units per claim in parentheses:

| Claimers | Cap | Shipped | ser | rc-guarded | rc (unsafe): lost completes of 300 |
| --- | --- | --- | --- | --- | --- |
| 1 | none | 2.47 (1.0) | 2.61 (1.0) | 2.40 (1.0) | 0 |
| 4 | none | 3.52 (5.0) | 4.11 (4.8) | **1.07 (1.0)** | 53 |
| 8 | none | 4.62 (9.6) | 4.63 (9.2) | **0.86 (1.0)** | 103 |
| 16 | none | 6.53 (16.5) | 7.11 (15.8) | **0.72 (1.0)** | 136 |
| 1 | 16 | 2.67 (1.0) | 3.18 (1.0) | 2.99 (1.0) | 0 |
| 4 | 16 | 3.92 (4.9) | 4.38 (5.0) | **1.72 (1.0)** | 48 |
| 8 | 16 | 5.38 (10.1) | 5.38 (10.0) | **1.73 (1.0)** | 93 |
| 16 | 16 | 7.50 (17.2) | 8.03 (17.4) | **1.87 (1.0)** | 138 |

- The guarded variant never needs a second claim unit and lost no
  completion.
- With 8 claimers it drains the queue 5.4× faster than shipped without a
  cap and 3.1× faster with the engine's default cap.
- It scales with claimers where SERIALIZABLE slows down.
- The advisory lock that keeps the cap exact costs about 2× against the
  uncapped guarded variant. It is still well ahead of SERIALIZABLE.
- At one claimer all variants cost the same within noise. The extra
  `SET TRANSACTION` statement of the ser mechanism is within noise of
  shipped.
- The naive variant looks fast only because it hands one item to several
  claimers. Its "lost completes" are completions refused because another
  claim had taken the item over.

### Recommendations

1. **ClaimWork isolation.**
   - Do not switch the claim unit to READ COMMITTED as it stands. Without
     guards it breaks claim exclusivity and the in-flight cap. The existing
     cross-pool claims test catches it.
   - Switch only together with a guarded lease upsert (take a lease only
     from an expired holder; skip the row otherwise) and a per-tenant,
     per-kind lock around the in-flight count. That combination passed the
     full PostgreSQL suite and the exclusivity and cap checks, and cut claim
     cost at 8 claimers from about 4.6–5.4 ms to 0.9–1.7 ms per item,
     always in one unit.
   - Before adopting it, add the two checks above (two items per resource;
     a cap under concurrent claimers) to the durable conformance suite, so
     every backend is held to them.
2. **Claim-round retry bound.** Under SERIALIZABLE, a third of claims at 8
   claimers and half at 16 need more than the three units one engine round
   allows; such a round waits a full poll interval. Whatever the
   isolation, a claim round that lost only to serialization failures could
   claim again at once instead of waiting.
3. **Post-claim stalls.** Look into the ~60 s tail with 8 or more workers.
   A unit lost after the claim should release or renew its lease, not
   leave it to expire.
4. **Event delivery.** Wake waiting polls on commit. The one-second rescan
   step puts median delivery at about 0.5 s and the worst case at 1 s,
   against a 0.1 ms (memory) to 5 ms (PostgreSQL) cost to deliver a ready
   event.
5. **Identity APIs.** Implement `Contacts.Link`, `Contacts.Identities` and
   `Contacts.List`, and call the registered identity verifiers. Until then,
   identity linking is host code that bypasses authorization, the
   operation ledger and events. **Done:** see
   [API coverage](api-coverage.md#contacts).
6. **Memory store.** Document the linear cost per write next to its
   non-durability, so bot hosts know when to move to PostgreSQL.
7. **Targets.** Set throughput and latency targets from these baselines on
   an unloaded machine with a pinned database, and rerun after changes to
   any of the items above.

## After the contention fixes

The same benchmarks, rerun after the changes that followed recommendations
1 to 4 and 6: claim units on PostgreSQL run at guarded READ COMMITTED, a
worker reruns its own units while it holds an item's lease, a claim round
lost to contention is claimed again at once, a waiting `Poll` wakes when
its engine commits events, and the memory store documents its per-write
cost.

| | |
| --- | --- |
| Machine, Go, PostgreSQL | As above |
| uma | Before: `main` at `79b5898`. After: the same base plus the fixes. Both with the benchmark files. |
| Load | Shared with other concurrent test runs; the 1-minute load average stayed between 6 and 13. Before and after were run in the same session. |

Medians of five runs (`-count=5`), PostgreSQL unless noted; ms per item or
per reply. Stalls were counted over 15 runs per worker count.

| Benchmark | Before | After |
| --- | --- | --- |
| ClaimWork, 8 claimers, no cap | 4.73 (9.8 claim units per claim) | 0.71 (1.0) |
| ClaimWork, 8 claimers, cap 16 | 5.27 (9.7) | 1.68 (1.0) |
| ClaimWork, 16 claimers, no cap | 6.14 (17.0) | 0.58 (1.0) |
| ClaimWork, 16 claimers, cap 16 | 7.76 (17.8) | 1.80 (1.0) |
| Run workers, 1 / 4 / 8 / 16 | 8.9 / 8.4 / 10.7 / 8.3 | 9.5 / 6.0 / 6.3 / 7.0 |
| Runs stalled ~60 s, 8 / 16 workers | 1 / 0 of 15 | 0 / 0 of 15 |
| Commit → long-poll delivery, p50 / max, memory | 528 / 1002 ms | 0.10 / 0.36 ms |
| Commit → long-poll delivery, p50 / max, PostgreSQL | 530 / 1024 ms | 7.7 / 11.7 ms |

- No reply was sent twice and no completion was lost in any run.
- The two conformance checks that caught plain READ COMMITTED (two queued
  items per resource under 8 claimers; an in-flight cap of 4 under 8
  claimers) are now part of the durable conformance suite and pass on both
  stores.

### What stalled the workers

A worker's units after its claim (recording the attempt, recording
evidence, settling, dropping, deferring or quarantining the item) were
retried at most three times, like any other unit. Under contention a unit
could lose all three to serialization failures. The worker then dropped
the item while still holding its lease, and nothing could reclaim it until
the lease expired a minute later. That is the ~60 s tail. Moving claims to
READ COMMITTED alone made it more frequent (6 and 4 of 15 runs stalled at 8
and 16 workers), because settles then lost to each other more often. The
worker now reruns its own units while its lease lasts, so an item stays
with its owner instead of waiting out the lease.

### Event delivery scope

The wake-up is in-process: a `Poll` wakes when a unit run by the same
engine commits events. Events committed by other processes sharing the
store are still found by the one-second rescan, so cross-process delivery
keeps the latency measured in the baseline.

### Known limitation

Throughput flattens after about four workers. Every settle appends a
delivery event, and every append updates the tenant's single event-head
row. Under SERIALIZABLE, concurrent updates of that row abort all but one
unit, so settles of one tenant run one at a time and are retried; at 8
workers some settles needed 13 runs before they committed.

## Pinned benchmarks

Every figure below comes from one session at one revision, replacing the
figures in the earlier sections. The benchmarks are named as in the
[Benchmarks](#benchmarks) table and the names are stable: a rename is a
change to this section.

| | |
| --- | --- |
| uma | `27cd7a0` (benchmark files only on top of `main` at `0a4ec5b`) |
| Machine | Apple M1 Pro, 10 cores, 16 GiB, macOS 15.6.1 |
| Go | go1.26.1 darwin/arm64 (`GOTOOLCHAIN=local`) |
| PostgreSQL | `postgres:18.4-alpine@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15`, Docker 29.4.0 under OrbStack (linux/arm64), via the `store/postgres` test container |
| Load | Not a quiet machine. Steps ran one after another with nothing of mine alongside; the 1-minute load average at the start of each step was 2.8 to 5.3 (other processes: a browser, the container runtime). Treat absolute numbers as upper bounds. Within a table the runs are comparable; the spread of five runs is given where it matters. |
| Method | `-run='^$' -count=5` with a fixed `-benchtime=Nx` per benchmark (the commands under [Benchmarks](#benchmarks)); medians of the five runs. `benchstat` was not available; the median and the range are computed from the raw output. |

All 12 steps passed. No reply was sent twice (`dup-sends` 0 in every run).

### Memory store scaling

Marginal cost of one operation against a tenant that already holds the
stated records, memory store. This is the cost the earlier "mean ms per
ingest" tables averaged over a growing store.

Engine ingest (`BenchmarkIngestGrowth`; messages already stored, then one
more through `Sink.Accept`):

| Stored messages | Existing conversation | New contact each time | Garbage per ingest (existing / new contact) |
| --- | --- | --- | --- |
| 100 | 0.09 ms | 0.18 ms | 0.28 / 0.57 MB |
| 1,000 | 0.51 ms | 0.89 ms | 2.3 / 5.0 MB |
| 10,000 | 3.8 ms | 7.3 ms | 21 / 44 MB |

Ranges over five runs: 10,000 stored, 3.7 to 4.2 ms and 6.9 to 9.3 ms. Cost
is linear in the stored messages: 0.38 µs and 0.73 µs per stored message at
10,000, 0.5 and 0.9 µs at 1,000. The earlier means (0.17, 0.44 and 1.59 ms
at 250, 1,000 and 4,000 first-contact messages) were averages over the
growth, about half the marginal cost at the end.

Memory retained, measured once after 3,000 ingests (live heap after a
collection, not part of the committed benchmarks): 3.1 KB per message in one
conversation and 5.9 KB per message with a new contact each. At those rates 10,000
messages hold 31 to 59 MB and 100,000 hold 0.3 to 0.6 GB.

Store level (`BenchmarkWriteGrowth`, `BenchmarkReadGrowth`; messages in
N/10 conversations, the write rolled back so the size holds):

| Messages | Write one message | Garbage per write | Live heap per message | First page of 50 (one conversation) | Page of 50 after the last cursor |
| --- | --- | --- | --- | --- | --- |
| 100 | 9.5 µs | 0.07 MB | 1,072 B | 12.9 µs | 59 µs |
| 1,000 | 0.26 ms | 0.74 MB | 1,101 B | 68 µs | 1.0 ms |
| 10,000 | 1.8 ms | 7.1 MB | 1,084 B | 0.56 ms | 13.8 ms |
| 100,000 | 16.5 ms | 68 MB | 1,056 B | 6.1 ms | 148 ms (319 MB) |

| Conversations (one message each) | Write one conversation | Garbage per write |
| --- | --- | --- |
| 100 | 6.6 µs | 0.04 MB |
| 1,000 | 0.16 ms | 0.48 MB |
| 10,000 | 1.2 ms | 4.6 MB |
| 100,000 | 10.6 ms | 44 MB |

- **Writes are linear.** From 1,000 records up, ten times the records cost
  7 to 9 times as much (1,000 to 10,000: 7 times; 10,000 to 100,000: 9
  times). From 100 to 1,000 the step is 27 times, as the copy outgrows the
  cache. The cost is the table copy: about 165 ns and
  680 bytes per message record at 100,000, and 106 ns and 440 bytes per
  conversation. The engine path copies several tables per ingest, hence
  3 to 7 times the store-level write.
- **Reads are linear in the table, not in the page.** A list builds and
  sorts every entry that matches before it cuts the page. The first page of
  one conversation costs the same whether the page is 1 or 50 entries, and a
  page late in the table costs 24 times a page early in it at 100,000.
  Walking all of a 100,000-message table in pages of 50 would take 2,000
  reads of 6 to 148 ms; the page limit is capped at 1,000.
- **Suitable size.** Up to about 1,000 messages in a tenant an ingest stays
  under 1 ms, and a few thousand stay within a few milliseconds. At 10,000 it is 4 to 7 ms and a tenant sustains 140 to 260
  messages a second at best, with 20 to 44 MB of garbage per message. At
  100,000 it would be about 40 to 70 ms per message (extrapolated, not
  measured; the store-level write is measured to 100,000) with 0.2 to 0.4
  GB of garbage each.
- **Retention does not bound it.** `Purge` has no family for messages or
  conversations, so those tables only grow. Events, operations, attempts,
  ingress copies, exports and contacts can be purged.
- **Tenants.** A unit copies only its own tenant's tables, so cost depends
  on the tenant's own records: 1,000 messages split over ten tenants cost
  what 100 do in one. (By construction; not measured separately.)

Contention, memory (`BenchmarkClaimWork`, 500 items; claim units per claim
and the share of claims needing more than three units):

| Claimers | No cap: ms per item | units / > 3 units | Cap 16: ms per item | units / > 3 units |
| --- | --- | --- | --- | --- |
| 1 | 0.12 | 1.0 / 0 % | 0.12 | 1.0 / 0 % |
| 4 | 0.49 | 4.2 / 23 % | 0.51 | 4.1 / 23 % |
| 8 | 0.85 | 6.5 / 45 % | 0.80 | 7.2 / 45 % |
| 16 | 0.91 | 9.1 / 59 % | 0.95 | 10.2 / 61 % |

No completion was lost in any run. Every writer to a tenant excludes the
others, so more claimers cost more per item.

### Ingest, reply and event delivery

Memory: 1,000 iterations. PostgreSQL: 300 iterations.

| Path | Memory | PostgreSQL |
| --- | --- | --- |
| Ingest, first contact | 0.45 ms (2159 allocs) | 6.1 ms (735 allocs) |
| Ingest, existing conversation | 0.26 ms (1115 allocs) | 4.3 ms (612 allocs) |
| Reply accepted (durable intent) | 3.8 ms | 7.3 ms |
| Reply → provider call | 3.9 ms | 11.6 ms |
| Reply → provider call → idle worker | 4.1 ms | 17.1 ms |
| Poll + Ack, event already committed | 0.12 ms | 5.0 ms |
| Commit → long-poll delivery, p50 / p95 | 0.16 / 0.56 ms | 9.5 / 11.8 ms |

- Ingest and event delivery agree with the earlier runs within noise
  (memory 0.44 and 0.26 ms; PostgreSQL 6.0 and 4.4 ms).
- **Reply accepted is 15 times slower on the memory store and twice as slow
  on PostgreSQL than in the earlier runs (0.25 ms and 3.4 ms).** The
  benchmark is unchanged. A memory profile of `accept` attributes 84 % of
  the allocation to `defaultReplyParent`: a `Messages.Reply` that names no
  `InReplyTo` lists the conversation's entire message history, page by
  page, to find the latest readable inbound message. Its cost therefore
  grows with the conversation's length, and the benchmark replies into one
  conversation, so the figure depends on the iteration count (96 µs at 100
  iterations, 0.53 ms at 300, 3.8 ms at 1,000, memory). On the memory store
  the list also sorts the whole message table for each page. A conversation
  of a few hundred messages is barely affected; a long one pays on every
  reply. The PostgreSQL slowdown runs the same code path and was not
  profiled.
- **Fixed after the pinned session.** `Reader.InboundMessages` lists a
  conversation's live inbound messages newest first, from an index on
  PostgreSQL, and the default parent is the first one the caller may read,
  so the read no longer follows the conversation's length. The figures in
  the table above are those of the pinned revision and are not re-pinned.
  Measured in one session on a busy machine (1-minute load 15 to 28, so
  only the allocations are exact), `BenchmarkReplyDispatch/accept` before
  and after, the same iterations as above:

  | | Memory before | Memory after | PostgreSQL before | PostgreSQL after |
  | --- | --- | --- | --- | --- |
  | Garbage per reply | 7.9 MB | 1.25 MB | 661 KB | 45 KB |
  | Allocations per reply | 2,777 | 1,628 | 4,535 | 438 |
  | Time (three runs each, ms) | 4.4 to 5.4 | 0.44 to 0.53 | 8.9 to 29 | 5.5 to 7.3 |

  The memory store still copies its message table when a unit first writes
  to it, which is linear in the stored messages (see
  [Memory store scaling](#memory-store-scaling)); the profile now
  attributes 0.5 % of the allocation to the default parent, from 84 %.
- Commit → delivery on PostgreSQL has a median of 9.5 ms, the in-process
  wake-up plus a commit to a containerized server; the memory store takes
  0.16 ms.

### ClaimWork and workers, PostgreSQL

`BenchmarkClaimWorkContention`, 300 items, guarded READ COMMITTED claim:

| Claimers | No cap: ms per item | Cap 16: ms per item | Units per claim | Complete retries per item |
| --- | --- | --- | --- | --- |
| 1 | 2.06 | 2.38 | 1.0 | 0 |
| 4 | 0.95 | 1.53 | 1.0 | 0 to 0.01 |
| 8 | 0.69 | 1.58 | 1.0 | 0.007 |
| 16 | 0.56 | 1.73 | 1.0 | 0.02 |

`BenchmarkRunWorkers`, 200 queued replies, median ms per reply:

| Workers | ms per reply | Range over five runs | Duplicate sends | Idle waits per reply |
| --- | --- | --- | --- | --- |
| 1 | 9.6 | 9.5 to 10.4 | 0 | 0 |
| 4 | 6.4 | 5.9 to 8.8 | 0 | 0 |
| 8 | 6.3 | 6.1 to 6.4 | 0 | 0 |
| 16 | 6.8 | 6.5 to 7.1 | 0 | 0 |

These match the "After the contention fixes" figures (0.71 and 1.68 ms at
8 claimers; 9.5 / 6.0 / 6.3 / 7.0 ms for 1 / 4 / 8 / 16 workers). No run
stalled for a lease.

### Message search, PostgreSQL

`BenchmarkMessageSearch`, 20,000 messages in 200 conversations; one page of
50 unless noted.

| Case | Median |
| --- | --- |
| No search | 1.1 ms |
| Has attachment | 1.8 ms |
| Rare text | 2.1 ms |
| Two tokens | 4.0 ms |
| Common text | 4.1 ms |
| Text with a date window | 3.7 ms |
| Label with text | 11 ms |
| Media type | 22.6 ms |
| Label only | 24.1 ms |
| Walk, rare text (20 hits) | 2.1 ms |
| Walk, common text (2,000 hits) | 128 ms |

### Reproducing

Run on an otherwise idle machine, one command after another, from a clean
tree at the pinned revision. Check `uptime` first and record it. The
PostgreSQL steps need Docker; `UMA_REQUIRE_DOCKER=1` fails instead of
skipping. The whole set takes about 20 minutes, of which the
`BenchmarkIngestGrowth` fixtures are 10.
