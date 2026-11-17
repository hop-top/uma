# Writing a store backend

How to implement `store.Store` for a database of your own and prove it with
the shared suites. You need the godoc of `hop.top/uma/store` open (it is the
contract) and the `hop.top/uma/conformance` package as a black box: run the
suites, read the failure, fix the store. You do not need to read the suite
source. The maintained stores are in-memory (`store/memory`) and PostgreSQL
(`store/postgres`); neither is required reading either.

The interface is one atomic `Store`, a read-only `Reader` (55 methods) and a
`Tx` that embeds `Reader` and adds 46 writes. There are no optional parts.
Every method must work, because the engine uses all of them and a store
cannot declare a family it lacks: no suite tolerates `model.Unsupported`. The
interface is frozen (see
[Compatibility](compatibility.md#store-interface-and-persisted-schema)): a method
change is a breaking change announced with a version bump, so pin the
`hop.top/uma` version you build against.

## The path

1. Pick a representation. [A table of JSON rows](#a-json-row-design) is a
   valid, low-effort design.
2. [Stub every method](#start-with-stubs-for-every-method) and wire the first
   suite. All cases fail, each naming the method it needs.
3. [Implement in suite order](#implement-in-suite-order), one group at a time.
4. Get units of work right early: [they overlap](#units-of-work-overlap),
   and [pages and cursors](#pages-cursors-and-filters) have exact rules.
5. Run the [other suites](#the-suites-in-order).
6. For a durable backend, supply a [`RestartFunc`](#durable-backends).

## The suites, in order

Run them in this order; each assumes the one before it passes.

| Suite | Proves | Run it |
| --- | --- | --- |
| `RunStoreSuite` | Every method, record rule, tenant isolation, atomicity and rollback, snapshots, the filter and cursor matrix, unit overlap, exact `Redact` output | always, first |
| `RunDurableLogicalSuite` | Concurrent claims, publication order, replay, and engine fault cases (a process crashing around an attempt, an ingress or a checkpoint) on one store, without a restart | a store that cannot restart |
| `RunDurableSuite` | Everything the logical suite proves, then the same state after a simulated crash and reopen | a durable store, instead of the logical suite |
| `RunServiceSuite` | The engine's authorized queries composed with your filters, cursors and lookups, end to end | always |
| `RunAttachmentSuite` | Attachment records, staging intents and retention with a blob store (use `portstest.MemoryBlobs`) | always |

Passing the logical suite is evidence of correct concurrent behavior only.
`Properties().Durable` may be `true` only for a store that passes
`RunDurableSuite`; the suite fails a store that reports `false`.

The whole suite runs your factory once per case, so building a fresh store
must be cheap: a new in-process value, a new schema from a template, a
truncate. The fixture's store must hold no data, `Now` is the clock your
store judges lease expiry by, and `Advance` moves it forward (cases that need
lease expiry skip when it is nil). `SubscriptionRows` is optional.

```go
func fixture(t *testing.T) conformance.StoreFixture {
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0))
	return conformance.StoreFixture{
		Store:   mybackend.New(clock),
		Now:     clock.Now,
		Advance: func(d time.Duration) { clock.Advance(d) },
	}
}

func TestStore(t *testing.T)       { conformance.RunStoreSuite(t, fixture) }
func TestService(t *testing.T)     { conformance.RunServiceSuite(t, fixture) }
func TestAttachments(t *testing.T) {
	conformance.RunAttachmentSuite(t, fixture,
		func(*testing.T) ports.BlobStore { return &portstest.MemoryBlobs{} })
}
```

Each suite is a tree of subtests. `RunStoreSuite` groups are `Transactions`,
`Records`, `Resources`, `References`, `Holders`, `Inboxes`, `Keys`, `Labels`,
`Claims`, `Links`, `Ledger`, `OperationList`, `OperationUpdatedAt`, `Events`,
`Retention`, `Staging`, `Redaction`, `Exports`, `Tasks`, `Work`, `Search`,
`RecipientLogs`, `Interactions`, `Filters`, `Cursors`, `Overlap`,
`ConversationWorkflow` and `ReadMarkers`. Select one with
`go test -run 'TestStore/Records'`.

## Start with stubs for every method

Write the stubs before any logic. Every case seeds a channel and an inbox
through `PutChannel` and `PutInbox`, and most go on to store a conversation
and messages, so until those and `View` and `Transact` exist no case can
pass. A store that implements only the Put and Get methods of those four
records still fails nearly every case, because each case then reaches for a
method a few lines later. You will
have written most of the surface before the suite goes mostly green. Plan for
that instead of implementing one family end to end.

1. Generate a method stub for every `Reader` and `Tx` method. No helper
   package is published; a tool that implements an interface does it
   (`github.com/josharian/impl` emits the signatures with the right
   qualified types, including those declared in sibling files of package
   `store`). Embed the reader in your transaction type
   (`type tx struct{ *reader }`) so the 46 write stubs are the only ones
   `Tx` needs.
2. Make each stub return `model.NewError(model.Unsupported, "<Method> not
   implemented")` with zero values, not `panic`. A panic aborts the whole
   test binary at the first case that reaches it, and you see one failure per
   run. With Unsupported errors every case fails on its own, with the missing
   method named:

   ```text
   Transact: unsupported: mybackend: PutChannel not implemented
   ```

   (The text before `unsupported:` varies by case; the rest names your
   missing method.)

3. Wire `RunStoreSuite` as above. At this point zero cases pass. That is the
   baseline, not a problem.

## Implement in suite order

Count the failures by missing method (`go test ... 2>&1 | grep -o 'unsupported: .*' | sort | uniq -c | sort -rn`)
and fill the most-demanded method first. A workable order, which is also the
order of the groups:

1. `View`, `Transact`, and the Put and Get pairs for channel, inbox,
   conversation and message. Compare-and-set revisions here (see the
   `store` godoc, Revisions): an empty expected revision means the record must
   not exist.
2. The paged lists (`Channels`, `Inboxes`, `Contacts`, `Conversations`,
   `Messages`, `Tasks`, `WorkItems`, `Labels`, `Identities`, `Replies`,
   `Operations`) with
   [their filters and cursors](#pages-cursors-and-filters).
3. Contacts, identities, labels and protocol state; attachments, exports and
   staging; `Search` fields on `Messages`.
4. The claims (identity, conversation, ingress, message) and links. These are
   idempotent create-or-return, and concurrent claims of one key must converge
   on a single stored record.
5. The operation ledger, attempt evidence, the event log and subscriptions,
   work and leases.
6. Tasks, streams, recipient logs and interactions.
7. Retention last: `Purge`, then `Redact`. Both have a long per-method godoc
   that is the specification; read it before writing them.

Run `RunStoreSuite` under `-race` before moving on; several cases use
goroutines, and a shared map or connection pool is the usual finding.

## Units of work overlap

This is the rule stores most often break, and the suite reports it from the
store group (`Overlap`), not only from the durable suites.

A callback never waits for another unit's callback to finish. Units in one
tenant or many are inside their callbacks at the same time, and a `View` runs
while a `Transact` is open. Taking a mutex, advisory lock or write lock when
the callback starts and releasing it at commit is therefore not valid, however
simple. It passes nearly every other case, then fails with:

```text
units did not overlap: a unit stayed inside its callback for 3s ...
```

and in the durable suites with `party never reached the gate`, because two
deliveries of one provider event rely on the second unit reaching its claim
while the first is still open.

What the store does when overlapping units conflict:

| Situation | Result |
| --- | --- |
| Overlapping updates of one record (both read revision `r1`, both write `r2`) | one commits; the other returns `model.Conflict` or `model.Transient`, stores nothing, and its other writes in the same unit are rolled back |
| Overlapping creates of one record | same |
| Writes to different records, same tenant | both commit, or one returns `model.Transient` |
| Units in different tenants | both commit, or one returns `model.Transient` |

Use `model.Conflict` for a violated constraint or a stale revision, and
`model.Transient` for a race the unit did not cause: it could not be
serialized, and rerunning the complete unit is safe. Report `Transient` only
for overlapping writes. A unit that does not overlap another must commit; the
suite's sequential cases treat any error, `Transient` included, as a failure.
The callback runs once, the store never retries it, and a failed unit leaves
no trace.

A tenant-wide version check (copy the tenant's data, run the callback,
publish at commit if the version has not moved) is legal and is the shortest
route to a correct store. It also reports `Transient` for overlapping writes
to different records. Who absorbs that is the engine, within a bound:
ingress, the worker running a claimed item and most other write paths rerun
a unit up to 16 times (the worker only while its lease lasts), the worker and
the other write paths after a jittered pause that grows with each loss; event
batches rerun up to 3 times. Past its bound the engine returns `Transient` to
the caller (a worker leaves the item to its lease). Row-level checks
(a PostgreSQL `SERIALIZABLE` unit, per-key versions) lose less under
contention, so move to them once the suites are green and your workload is
contended.

Two rules follow from the same text: handles are valid only during their
callback and on its goroutine (a handle used afterward fails with
`model.Permanent` and changes nothing), and units do not nest.

## Pages, cursors and filters

Every paged read takes a positive limit (the engine defaults zero before the
store sees it; `ListRequest.Validate` accepts zero but your reads must
reject it with `model.Validation`, as the `Cursors` group checks). A page may
hold fewer items than asked, including none, with a `Next` cursor;
`Complete` means nothing more exists within the query's scan boundary.

The `Cursors` group pins, for every paged read:

- A cursor is opaque and valid only for the query that made it. One it
  cannot decode (including a valid cursor with garbage appended) is
  `model.Validation`.
- A cursor resumes exactly after the last item returned, whatever the page
  size, in a new unit, and the same cursor can be used twice. The page size
  may change on resume.
- A commit between two pages neither repeats nor skips an item already
  reached. A cursor holds a position, never an offset.
- A page that is not complete must return a `Next` that advances.
- `Events` cursors are positions in the event log, also usable as
  checkpoints. One beyond the head is `model.Validation`, and one before
  `EventBase` is `model.CursorExpired`.
- A reader that moved past a position never later finds an event at or
  before it, and a unit's events are adjacent. Positions taken before
  commit (a sequence) break this when a lower one commits after a higher
  one; the PostgreSQL store assigns positions after commit instead, one
  placement at a time per tenant, so its appenders share no row.

The `Filters` group pins which of the sixteen store-facing `model.Filter`
fields each read honours: it applies those and rejects every other set field with
`model.Validation`, never silently ignoring one. The list per read is in the
`store` package documentation, under Pages; follow it field by field. A
malformed filter (an empty inbox reference, an empty kind) is
`model.Validation`, and a reference in a filter that another tenant owns is
`model.NotFound`.

`model.Filter.Actor`, the seventeenth field, is the engine's: `Operations.List`
matches it against each record's key, every other engine query refuses it,
and no store read receives it, so the group does not pin it. The operation
list's `Kinds` is matched the same way; `Reader.Operations` keeps rejecting
`Kinds`, and the engine never passes it.

Cost matters even though correctness passes first. The engine calls a read
with whatever it still needs, as small as one item, and scans up to 500
stored items per call; the service and search suites store more than 500
matching messages and page across them. A store that decodes or loads every
row for every page is correct and slow, and `-race` multiplies it. Skip rows
before the cursor using an index on the position (a per-tenant insertion
sequence works for the lists that page in recording order) and decode only
the rows you return.

## Errors

Every failure the store detects is a `*model.Error`; the codes and their use
are in the `store` godoc (Errors). Where a call fails more than one way,
check in this order:

1. `model.Validation`: shape. Malformed references and keys, missing
   required fields, a non-positive limit, a malformed cursor or filter.
2. `model.NotFound`: a missing record or parent, and any reference another
   tenant owns. A foreign record is never confirmed, so it reports the same
   as a missing one.
3. `model.Conflict`: a stale revision, mismatched fingerprint, stale or
   foreign fence, uniqueness violation, or a bounded read whose complete
   result is larger than its limit.

`RecordStaging` and `ClearStaging` are the documented example: a malformed
key is `Validation` before the tenant is looked at, and only a well-formed
key of another tenant is `NotFound`. `Redact` checks everything before its
first write, so a failed call writes nothing. Read each method's godoc for
the checks it names; the input rules that are not in a signature (for
instance that `ClaimConversation` needs a mode equal to the selection's, or
that `LinkIdentity` needs `Supersedes` to name the latest link record) are
there.

A store applies exactly these model validators, and no others:
`model.Label.Validate`, `model.Conversation.ValidateLabels`,
`model.Event.Validate`, `model.IngressReceipt.Validate` and
`model.Filter.Validate`. Record semantics such as `model.Task.Validate`
belong to the engine. If you call them from `PutTask`, you will reject
records the suites store on purpose.

## A JSON row design

The model package states that wire and storage form is `encoding/json`, so
every `model` type round-trips through it unchanged. A store can keep, per
tenant, one table per record kind, each row holding the complete record as
JSON plus the few columns the store needs to constrain or order: the
reference, the revision, an insertion sequence, and the keys it must keep
unique (channel and inbox keys, external references, claim keys). The
PostgreSQL store is built this way. In-process, the same idea is a map of
encoded rows copied at `Transact`.

The PostgreSQL schema is at version 26 (`postgres.SchemaVersion`); migration
19 added the operation scope columns and indexes, migration 20 the
conversation workflow columns (assignee, snooze) and the message recording
time the snooze wake rule compares, migration 21 the conversation link count
and the read marker table, migration 22 the event conversation and
recording time columns that event filters read, migration 23 the event
placeholders that let positions be assigned after commit, and migration 24
moved the foreign key checks of a new message's reference, attachment and
claim rows and of a conversation's claim row to commit, migration 25
added the interaction's provider message columns and the index of
interactions not yet linked to a local message, and migration 26 an index
repeating a conversation's key, so that the planner probes a conversation
by key rather than scanning the tenant's conversations when it has no
statistics yet.

Decoding a row into a fresh value on every read also gives you the snapshot
rule for free: the store never retains caller memory and never returns
memory it retains.

## Records with rules of their own

A few methods carry exact record shapes; follow their godoc and let the suite
confirm.

- `Redact` keeps and clears named fields, and `RunStoreSuite` pins the exact
  output. A redacted message keeps `Deleted`, `Reactions`,
  `ReactionsComplete`, `LastEdit`, `EchoOf` and `InResponseTo`. A store that
  clears them fails `MessageRedactionLeavesExactlyTheDocumentedRecords`.
- `Purge` has per-family rules (the event log is deleted as a prefix only).
- `EraseRecipientLog` deletes the retained entries of one recipient log and
  keeps the log's last sequence, so a later append continues after it. An
  absent log, an erased one and one on a channel that is not stored return
  0 and no error. Privacy erasure of a contact calls it in the erasure's
  unit for every identity's log (channel of the identity's inbox, identity
  namespace, identity source ID); the `RecipientLogs/Erase*` cases pin it.
- `Messages` evaluates label and search filters in storage, by join with
  the conversation: nothing is copied onto a message.
- `Operations` pages the operation ledger most recently written first: the
  time of an operation's latest write (its claim, or any `UpdateOperation`),
  compared to the microsecond, then the greater ID. It honours `Inboxes`,
  `Conversation`, `Statuses` (operation states) and the window `Since`
  (inclusive) and `Before` (exclusive) on that time. An operation's inbox
  and conversation come from `OperationRecord.Scope`, which reads the key's
  target and the record's typed results; call it, do not re-derive the rule.
  It is in the suite: the `OperationList` group, and `Operations` in the
  `Filters` and `Cursors` groups.
  The PostgreSQL store keeps the scope in the `inbox_id` and
  `conversation_id` columns, rewritten by every `UpdateOperation`, with
  indexes ending in `(written_at, id)`.
- Every read of an operation (`Operation`, `OperationByKey`, `Operations`) and
  the record `ClaimOperation` returns sets `Operation.UpdatedAt` to that same
  write time, truncated to the microsecond and in UTC. A write ignores the
  value it is given, and the stored record does not carry it. Truncate once and
  use the result for both the stored time and the time shown, so a record and
  the window edges agree. The suite's `OperationUpdatedAt` group pins it: set
  on every read, moved by every write, never the caller's, and equal to the
  edges of `Since` and `Before`.
- `Conversations` evaluates the workflow filters in storage, so a filtered
  page is full. `Assignee` selects `Conversation.Assignee` equal to `Actor`,
  or unset for `None`; `Self` is the engine's to resolve and a store refuses
  it with `model.Validation`. `Snoozed` keeps the conversations whose
  `Snooze` is in effect (true) or the others (false): `Until` after the
  store clock's now, and no live inbound message of the conversation (as
  `InboundMessages` selects them) that was not imported (empty
  `Message.ImportedBy`) with `RecordedAt` after `At`, compared exactly.
  The predicate runs at read time, so a snooze wakes by expiry or by a live
  inbound message with no write; history that `Runtime.Import` records is
  stamped `ImportedBy` and wakes nothing. `SnoozeEnded` keeps the
  conversations whose stored `Snooze` is present but no longer in effect by
  that rule (true) or the others (false); a worker's sweep selects them,
  clears the snooze and emits one `conversation.snooze_ended`, so they are
  transient.
  `Linked` keeps the conversations holding a link (true) or none (false).
  `Unread` (its `Actor` is required) keeps the conversations holding a
  message that `model.ReadMarker.Unread` counts for the actor's marker on
  it, or any such message when the actor has no marker: recorded after
  `ReadThrough`, no echo, not imported, not the actor's own uma-authored
  message. `InboundMessages` itself still returns imported messages. The
  `ConversationWorkflow` group pins all of it, and `Conversations` in the
  `Filters` matrix. The PostgreSQL store keeps the assignee and the snooze
  instants and the link count in conversation columns and each message's
  recording time in a message column, all rewritten from the record on every
  write.

- `Events` honours `Inboxes`, `Conversation`, `Kinds` and `Since`. An
  event's conversation comes from `store.EventConversation`, which reads its
  resource and its builtin payload; call it at append, do not re-derive the
  rule. An event with none never matches a conversation filter. `Since`
  keeps the events whose `RecordedAt` is at or after it, compared exactly.
  `RecordedAt` does not follow log order (a unit sets it before it
  commits), so never stop at the first event below `Since`; keep, per
  event, the running maximum of `RecordedAt` along the tenant's log
  (continued across units and purges), start after the last position whose
  maximum is still below `Since`, or at the cursor when that is later, and
  compare each event from there. Next and Complete stay the scan's: the
  position after the last scanned event, the head on a complete page. The
  `Events` group (`ConversationOfEachShape`, `SinceOnRecordedAt`,
  `SinceComposesWithCursor`, `FiltersSeeTheUnitsAppends`) and `Events` in
  the `Filters` matrix pin it. The PostgreSQL store keeps the conversation,
  `RecordedAt` and the running maximum in event columns, the tenant's
  maximum in its head row, and indexes for a conversation range and the
  `Since` start. `PutSubscription` still refuses `Conversation` and
  `Since`.
- `UnlinkedInteractions` pages, in recording order, the interactions of an
  inbox whose `MessageExternal` equals the given provider message, field by
  field, and whose `Message` is unset; `LinkInteraction` sets that `Message`
  once. A link to the message already named returns the record unchanged,
  to another message (or on an interaction naming no provider message) is
  `model.Conflict`; redacted interactions are linked too, and the token is
  never touched. The engine calls the pair in the unit that records a sent
  message's provider ID, and ingress looks the same ID up with
  `MessagesByReference` before it records an interaction, so whichever of
  the two units commits second links the interaction. Two such units can
  overlap, each reading what the other writes: your store must then fail
  one of them (`model.Transient`) rather than commit both, as a
  serializable store does. The PostgreSQL store keeps the provider message
  in interaction columns with a partial index of the unlinked rows, so a
  lookup only conflicts with writes of interactions it could link. The
  `Interactions` group (`UnlinkedInteractions*`, `LinkInteraction*`) and
  the `Faults/InteractionBeforeSendAnswerIsLinked` case pin it.
- `ReadMarker` and `PutReadMarker` keep one read marker per conversation and
  actor: a record of its own with its own revision, compare-and-set like any
  `Put`, on a stored conversation only (`model.NotFound` otherwise), and never
  touching the conversation record or its revision. A read by a conversation
  reference whose inbox differs from the stored one is `model.NotFound`. The
  `ReadMarkers` group pins it.

### Held operations and trust counters

Neither adds a method or a schema change; both ride on what a backend already
stores. The formats below are frozen once released.

- **Held operation.** An operation proposed for a human decision is an
  ordinary `OperationRecord` whose `Operation.State` is `held` and whose
  `Operation.Held` (`model.HeldBlock`) holds the proposal. The block is part
  of the operation's JSON body, so a JSON row design stores it unchanged: no
  column, no index beyond the state index the operation list already has.
  `held` and `expired` are plain state values; a decided operation keeps its
  block with `Decision`, `DecidedBy` and `DecidedAt` filled in. A store
  returns the block exactly as written, on `OperationByKey` and in
  `Reader.Operations`, which lists the held queue with `Filter.Statuses` set
  to `held` and, per inbox, `Filter.Inboxes`. The PostgreSQL plan for a held
  listing of one inbox ranges over `uma_operations_state_recent` (a held
  queue is small) and filters the inbox; `TestOperationsUseTheirIndexes`
  pins it.
- **Trust counters.** The host's per-key decision counts live in 64
  channel-scoped protocol records, one per hash bucket, keyed
  `uma.trust.00` to `uma.trust.3f` (two lowercase hex digits). The `uma.`
  prefix is reserved to the engine. A count is a read-modify-write of one
  bucket in one unit: `Reader.Protocol`, then `Tx.PutProtocol` with the
  revision just read. A backend must therefore make `PutProtocol` a true
  compare-and-set, and units that read the same revision must not both
  commit: exactly one wins, the other gets `model.Conflict` or
  `model.Transient` and reruns on the winner's state, so no increment is lost.
  The durable `Concurrency` group (`ProtocolBucketRaceHasOneWinner`,
  `ProtocolBucketIncrementsConverge`) pins it; the `Records` store group pins
  the bucket round trip (`ProtocolTrustBucketsRoundtrip`), and the
  `OperationList` group the held block round trip and listing.

### Migrating a backend to read markers

This adds two methods, so every backend breaks at compile time:
`Reader.ReadMarker` and `Tx.PutReadMarker`. Store `model.ReadMarker` keyed by
(conversation, actor), compare-and-set on its revision, as above; a backend
with a JSON row design adds one table. The `ReadMarkers` store group is new.

### Migrating a backend to unlinked interactions

This adds two methods, so every backend breaks at compile time:
`Reader.UnlinkedInteractions` and `Tx.LinkInteraction`. A backend with a JSON
row design indexes the interaction's `MessageExternal` (or scans the
interactions of the inbox) and selects those with no `Message`; the shared
rules are those above. The `Interactions` store group gains four cases and
the durable `Faults` group `InteractionBeforeSendAnswerIsLinked`. An
existing interaction keeps its body: nothing is rewritten.

### Migrating a backend to the conversation workflow filters

This is a breaking change for a backend that passed the previous release:
`Conversations` must honour `Filter.Assignee`, `Filter.Snoozed`,
`Filter.Linked` and `Filter.Unread` as above (it used to refuse them), and
the `ConversationWorkflow` group and the `assignee`, `snoozed`, `linked` and
`unread` rows of the `Filters` matrix are new. No method
changed. Store `Conversation.Assignee`, `Conversation.Snooze` and
`Conversation.Links` with the record (they are ordinary optional fields of
its JSON), then evaluate the four filters; a backend that indexes nothing can scan each conversation's
messages for the newest live inbound recording time. Both the snooze and the
unread predicate skip imported messages (`Message.ImportedBy` set); it is a
field of the message's JSON, so no column is needed.

### Migrating a backend to the event list filters

This is a breaking change for a backend that passed the previous release:
`Events` must honour `Filter.Conversation` and `Filter.Since` as above (it
used to refuse them); the `Events` store group gains four cases and the
`Filters` matrix the `conversation` and `since` rows of `Events`. No method
changed. Resolve each event's conversation with `store.EventConversation`
and its running `RecordedAt` maximum at append; a backend that indexes
nothing can scan the log in position order and compare both per event, at
the cost of reading every event before the first match.

## Durable backends

A store that survives restarts runs `RunDurableSuite` with a `RestartFunc`:

```go
func TestDurable(t *testing.T) {
	conformance.RunDurableSuite(t, fixture,
		func(t *testing.T, fx conformance.StoreFixture) conformance.StoreFixture {
			next := fx
			next.Store = mybackend.Reopen(t, fx.Store) // same persisted state, new handle
			return next
		})
}
```

The function simulates a crash and restart of the process:

- It abandons `fx.Store` without a graceful close and without waiting for
  units still running on it. A unit still running when the process stopped
  must leave no trace.
- It returns a fixture whose `Store` is a new handle (the same value fails
  the case) opened on the same persisted state, and whose `Store` reports
  `Properties().Durable` as `true`.
- Its clock continues from the old one: leases committed before the restart
  are judged against it.
- It may register `t.Cleanup` to close the abandoned handle once the case
  ends.

Committed units must come back with their claims, ledger, leases, events and
checkpoints intact, and the engine's fault cases run again with the process
crashing on one handle and the next process recovering on the restarted one.

## When a suite fails

| You see | Cause |
| --- | --- |
| Every case fails with `unsupported` | Stubs are in; implement the method named in the message |
| `units did not overlap ...` | The callback holds a lock until commit; see [Units of work overlap](#units-of-work-overlap) |
| A `Filters` case reports a read accepted a filter field | The read ignores a field it does not honour; return `model.Validation` |
| A `Cursors` case reports wrong items after resuming | The cursor is an offset, or the page size changed what was skipped |
| A `Cursors` case reports a page that does not advance | `Next` repeats the `After` that produced it |
| `Redaction/MessageRedactionLeavesExactlyTheDocumentedRecords` | `Redact` cleared or kept a field the godoc lists the other way |
| `RecipientLogs/EraseDeletesEntriesAndKeepsTheSequence` | `EraseRecipientLog` dropped the log's counter, so a later append reuses a sequence a reader already saw |
| `RunDurableSuite needs a store reporting Properties().Durable=true` | Report `Durable` only for a store that can restart; otherwise run `RunDurableLogicalSuite` |
| A slow `RunServiceSuite` | Pages decode rows before the cursor; see [Pages, cursors and filters](#pages-cursors-and-filters) |

## Done

A backend is ready when `RunStoreSuite`, `RunServiceSuite` and
`RunAttachmentSuite` pass, together with either `RunDurableSuite` (set
`Durable` to `true`) or `RunDurableLogicalSuite` (leave it `false`). Treat a
divergence between your store and a maintained one that the suites do not
catch as a suite bug, and report it.
