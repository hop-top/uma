# Held operations: lifecycle, trust counters, authorization

Status: accepted and implemented. The user-facing description is the
README's "Held operations" section.

An operation proposed by a plugin actor can be held for a human decision before
it is applied. uma owns the held state and the trust counters. The host owns the
policy (when to hold, thresholds, always-held rules) and the approve, reject and
edit verbs it exposes.

Fixed inputs:

- Only `plugin:` actors can be held. A person's own writes are never held.
- Approve applies the original idempotency key.
- Edit applies the edited arguments and counts as a correction.
- Reject applies nothing and counts as a correction.
- Visibility follows the target's read rights.

Every change below is additive under [compatibility.md](../compatibility.md).

## 1. Lifecycle

`held` is a `model.DeliveryState` that precedes `queued`.

```
held -> queued     approve, or edit then approve
held -> rejected   reject
held -> expired    deadline passed with no decision
```

- `held` and `expired` are new values of a closed set: additive, announced in the
  release notes; callers keep a default branch.
- `rejected` is the existing final state, reused. A held rejection carries the
  decision in the held block (section 2), not a new `ErrorCode`.
- `expired` is final. Expiry is neither a clean approval nor a correction; no
  counter moves.
- Expiry is a sweep (`operations.ExpireHeld`, run by `Runtime.Run` every
  `WorkerOptions.SweepEvery` and callable once as `Runtime.SweepHeld`). Each
  operation is its own unit and a compare-and-swap on the revision the sweep
  read, so sweeps and decisions racing move an operation once. "Now" is the
  engine clock, the one a decision judges the deadline by. An operation still
  held a full sweep window after it was held gets `HeldBlock.CarriedOverAt`,
  once; its state and the duplicate guard are unchanged.
- A held operation has no work item, no attempt and no provider effect. Approval
  enqueues work in the same unit as the `held -> queued` write.
- Alternatives rejected:
  - A separate "proposal" record beside the operation. Doubles lookup, keying and
    retention, and loses the one ledger entry a caller already polls.
  - A held flag on `queued`. Every consumer of `queued` would have to filter it.

## 2. Idempotency

The key scope `(caller, kind, target, id)` is unchanged.

- A held operation claims its key at hold time. A replay of the proposal returns
  the held operation, never a second one.
- Approve re-submits under that same key. The first approval wins; a second
  returns the operation as it stands.
- Edit keeps the lineage: same key, new `Revision`, recomputed fingerprint. The
  held block records the proposed fingerprint and each edit's before and after
  arguments, with who and when.
- A replay compares against the proposed fingerprint, not the edited one. A
  replay of the original proposal after an edit therefore returns the held
  operation instead of failing as a fingerprint mismatch.
- Fingerprint v1 layout is untouched. The held block is not part of it.
- Rejected alternative: a fresh key at approval. A replay of the proposal would
  then create a second operation.

### Deciding

Approve, reject and edit are transitions of the proposal's own ledger entry, not
new operations: they claim no key.

- Authorization: the read check of section 4 first, then the decision's action
  as the principal. Decided-by is the principal's actor.
- Every decision is compare-and-swap on the operation revision (required) and is
  refused as stale when the target, a conversation, advanced since the hold (the
  held block records its revision), unless the caller forces it. Force never
  skips the revision compare or an authorization.
- Both refusals are `model.Conflict`; `Error.Reason` tells them apart:
  `model.StaleTarget` (target advanced) and `model.StaleRevision` (revision
  compare-and-swap). Test with `model.IsReason`, never message text.
- Edit is edit and approve in one step, decision `edited`. It replaces the
  stored arguments in the held block (`EditedArgs`) and appends the edit with
  before and after; a worker executes the edited arguments. The stored intent
  and fingerprint are untouched, so a replay of the proposal matches the stored
  fingerprint and returns the operation.
- A decision already made returns the operation with `Replayed` set and writes
  nothing. Approve after approve or edit, reject after reject, edit repeated with
  the same arguments. Any other decision of a decided operation is `Conflict`.
- Approve and edit apply the claimed action, so the decider also needs the
  operation's kind (for example `message.reply`) on its target; reject applies
  nothing and does not.
- An edit runs the intent's registered validator on the edited arguments: every
  target-bearing field must equal the claimed target and the audience cannot
  change. An intent with no validator cannot be edited; approve and reject do
  not need one.
- Approve and edit past `ExpiresAt` are refused; reject is not.
- The local mutation of an approved operation is written by an `Applier`
  registered for its intent name, in the unit of the transition, under the
  `Result` references named at hold time. The hold records whether the
  operation writes a local mutation (`HeldBlock.Applies`) and whether it
  commits locally rather than queues (`HeldBlock.Local`). A write that applies
  locally is held only if its intent has an `Applier` (else `Unsupported`, nothing
  claimed), and its approval is refused when the `Applier` is gone, never
  approved with nothing written. A provider-work intent needs none: its approval
  queues its work. `HeldBlock.Intent` is required of every held operation.
- Label configure (create or update), label delete and conversation transition
  commit locally with no approval step, so a hold on them is `Unsupported`
  unless `Config.Held` opts them in (`LabelConfigure`, `LabelDelete`,
  `ConversationTransition`; all off by default). Opted in, they are held like a
  label change: checked as the command is at hold time, written only by their
  `Applier` on approval, nothing left by a reject or an expiry. A label create
  reserves its ID when held. An edit may change a label's name, description and
  color or a transition's state, never the target; a label delete has nothing
  to edit.
- A start is always holdable and has two modes (`Config.Held.StartCreateAtHold`).
  Default, create on approve: the hold stores the intent (inbox, recipient,
  content, and the ID the message will be written under, allocated when held)
  and creates nothing, so a rejected or expired proposal leaves no
  conversation, identity or event. The operation names no `Result` or
  `ResultMessage` while held; the approval runs the inbox's selection policy
  against the conversations as they are then (it may reuse one), writes the
  message under the reserved ID in the unit of the transition, and sets
  `Result` and `ResultMessage`. This is the one case where the references are
  not named at hold: the intent registers an `OpeningApplier`, which returns the
  message it wrote, rather than an `Applier`. The stored intent has no
  conversation until then; `Effective` fills it from the result message for
  the worker. Create at hold: the conversation is selected or created in the
  unit that holds the start, which names it and its message; approval only
  writes the message, and a rejected or expired start leaves the conversation
  it opened, empty. The preview is the proposer's in both modes; the stored
  intent carries what a host needs to build one.
- The decision result carries decision, trust key, labels and whether it is a
  correction; the counters of section 5 are written from the in-unit hook
  (`recordDecision`), never for a replay.

## 3. Duplicate guard

A second proposal for the same target and kind, under a different key, while one
is held returns the held operation marked "already held". Two kinds narrow the
scope (`HeldBlock.Scope`; a proposal is a duplicate only when the scopes are
equal): a held start is scoped by its recipient, so a second start to another
recipient in the inbox is a new proposal, and a held label create by the
label's name, so another name is a distinct proposal. A label update and every
other kind have no scope: kind and target decide. It is found with
`Reader.Operations` filtered to the held status and the target; no new Reader
method. Both proposals run in one unit, so a concurrent pair serializes and the
loser sees the winner.

The held operation is shown to the second proposer only if it passes the read
check of section 4 (`operation.read` and the target's read right). Otherwise the
proposal is refused with `model.Conflict` "operation already held", no record and
nothing stored: the held block is draft content the proposer cannot read at its
source. The refusal tells only that a proposal is pending, which the guard
cannot answer without. A replay under the proposer's own key is no duplicate and
is not subject to it.

Rejected alternatives: queue both (a reviewer would approve the same change
twice); `model.NotFound` for the refused proposer (a write that passed its own
authorization is not "not found", and it hides nothing the conflict does not).

## 4. Authorization and visibility

New builtin actions (additive): `operation.approve`, `operation.reject`,
`operation.edit`, plus `trust.read`, `trust.reset` and `trust.write` for the counters.

- The decider needs the matching action on the operation. Proposing never grants
  it: a plugin cannot approve its own proposal.
- A held operation is visible to a principal who holds `operation.read` and the
  read action of its target (for example `conversation.read`). Without the target
  read right the operation is `model.NotFound`, never a redacted stub, because
  the held arguments are the draft content.
- Message redaction clears the held block's draft content with the message's:
  preview, edited arguments and each edit's before and after lose their data;
  names, decision, decider and times stay.
- Rejected alternative: a separate held-queue permission. It would let a reader
  of the queue see content they cannot read at its source.

## 4a. Events

Three builtin event kinds, additive, payload major stays 1 (schema
`model.HeldOperationPayload`, optional fields only):

| Kind | Emitted | Decision field |
| --- | --- | --- |
| `operation.held` | once, in the unit that stores the proposal; never for a replay or a duplicate-guard hit | empty |
| `operation.decided` | once per decision, in the decision's unit; never for a replay | `approved`, `rejected` or `edited` |
| `operation.expired` | in the unit that writes `held -> expired` (`operations.ExpiredEvent`) | `expired` |

- Payload: operation reference, key (caller, kind, target, id), proposer, trust
  key, decision, decided-by. Never the preview, the reason or any argument.
- Resource the operation, audience internal. A subscriber needs `operation.read`
  and the read right of the key's target, as for the operation itself.

## 5. Trust counters: storage

Decision (owner): counters live in uma's existing record storage. No method is
added to `store.Store`, `Reader` or `Tx`. Options weighed:

| Option | Verdict |
| --- | --- |
| (a) new Store methods, announced minor-bump breaking change | not needed; held back as fallback, needs owner sign-off |
| (b) existing record storage, no new method | chosen |
| (c) optional store interface by type assertion | rejected: contradicts "no optional parts" |

### Mechanism

The existing channel-scoped protocol record: `Reader.Protocol(channel, key)` and
`Tx.PutProtocol(state, expected)`, store/store.go. It is a keyed upsert with a
compare-and-set revision, backed by `uma_protocol_states` in PostgreSQL
(primary key `(tenant_id, channel_id, key)`) and a revision-checked table in the
memory store. The engine already reserves the `uma.` key prefix for itself and
refuses it to adapters, and `adapter/web/webhookauth/storereplay` already keeps
counted state this way.

### Why it is atomic

An increment or reset is a read-modify-write of one record in one
`Store.Transact`: `Protocol` read, mutate, `PutProtocol` with the revision just
read.

- PostgreSQL runs `Transact` at serializable isolation; a stale revision is
  `model.Conflict` and a serialization failure `model.Transient`.
- The memory store serializes writers and applies the same compare.
- The loser stores nothing and reruns the whole unit on the winner's state, the
  retry loop `storereplay` uses. Concurrent clean approvals therefore never lose
  an increment, and a reset never resurrects a count.
- The counter write sits in the same unit as the `held -> queued` or
  `held -> rejected` write. A decision and its counter move commit together or
  not at all.

### Layout

- Counters sit in a fixed set of 64 bucket records, keys `uma.trust.00` to
  `uma.trust.3f`, extension name `uma.trust`, version 1. The bucket of a trust
  key is the first byte of its SHA-256 modulo 64. Bucket count and hash are a
  persisted format and never change.
- Buckets, not one record per key, because the store has no list-by-prefix read:
  a fixed set is the only way to enumerate keys with no new method. Listing reads
  the 64 records. Keys in one bucket contend; approval rates make that cheap, and
  the retry loop absorbs it.
- Each entry holds the trust key, its labels, `clean` (consecutive), `approvals`
  and `corrections` totals, `updated_at` and the last few batch ids.
- A bucket is capped by a configured entry limit. Past it the write is refused
  with `model.Validation`; entries are never dropped silently.
- Records anchor to the channel of the operation's target inbox. Trust is
  therefore per channel. A host that wants one trust across channels pins it to
  one channel of its choosing.
- Probed by reading the code: both backends, the schema, the isolation level and
  the precedent were read; no new store behaviour exists, so the store suites
  gain no group of their own; the held and trust cases run in the service and
  durable suites on every backend.

## 6. Trust key and primitives

The key is an opaque string to uma. The host computes it; uma stores and counts
it. Structure belongs to the host's policy, and a structured key would freeze a
dimension set into uma's persisted format.

- The key is 1 to 256 bytes, no PII (same bound as a protocol key).
- Labels are an optional map of short strings set by the host (for example
  actor, kind, class). They exist for listing and filtering only; uma never
  derives the key from them or interprets them. A later write with different
  labels replaces them.
- Rejected alternative: a structured key `(actor, kind, target class)`. It makes
  uma decide what trust is scoped to.

Counting rules:

- Approve unchanged: `clean` +1, `approvals` +1.
- Edit or reject: `clean` = 0, `corrections` +1.
- Expire: no change.

Primitives (host-facing):

- `ResetTrust(key)`: `clean` to 0; totals kept.
- `ResetTrustSet(keys)`: the same for a set of keys, all or none in one unit.
- `RecordCleanApproval(key, batch)`: the engine's counting step, not a public
  method. It counts one clean approval for `batch` at most once per key. A
  batch approving ten operations under one key moves `clean` by 1, and a
  replay of the batch moves nothing. The entry keeps the last 8 batch ids to
  answer a replay. A host names the batch with the optional `DecideHeld.Batch`
  (1 to 128 bytes, else `Validation` before anything is written); empty means
  the operation is its own batch. Reject and edit are corrections, which no
  batch merges.
- `Trust(key)` and `Trusts(filter)`: read an entry; list entries by label.

Recording in the decision unit (never for a replay, never for an expiry):

- approve as proposed: `RecordCleanApproval` of the operation's trust key, the
  decision's `Batch` or, when empty, the operation id as its batch;
- reject: correction of the operation's trust key;
- edit: correction of the keys the edit names (`EditHeld.CorrectedKeys`), by
  default the operation's own; keys not named do not move.
- An operation held with no trust key, or whose target belongs to no channel,
  counts nowhere.
- `trust.reset` authorizes every write (reset, record clean approval, record
  correction); `trust.read` authorizes `Trust` and `Trusts`. Both are checked
  on the channel the counters are scoped to.
- `ImportTrust` seeds a key from a host's earlier history under `trust.write`.
  Per counter: max(stored, imported); `LastCorrectionAt`: the later; labels
  replaced when given. `Clean` is raised only while the stored
  `LastCorrectionAt` is not later than the imported one, because a later
  correction ended the imported streak. A repeat or lower import writes
  nothing.

Decision on thresholds: uma exposes counts only. Whether `clean` of 20 means
auto-approve is the host's policy.

## 7. Consequences

- No Store, Reader or Tx change: `uma.New` compiles against every existing
  backend. The held and trust behaviour is pinned through the existing
  suites, whose service and durable suites carry held and trust groups
  (batch and import included).
- Two new `DeliveryState` values, a held block on `Operation`, six builtin
  action names, three event kinds and the trust methods on a service
  interface uma implements: all additive.
- The held block and the `uma.trust` record are new stored bodies; both are
  `omitempty` or new keys, so existing goldens are unchanged.
- Counter listing costs 64 reads. If that becomes a problem, option (a) is the
  fallback and needs owner sign-off.
