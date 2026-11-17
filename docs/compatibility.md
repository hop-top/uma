# Compatibility

The uma API is frozen. Modules are still `v0.x` and nothing is tagged yet; this
page says what may change inside `v0.x` now, in place of "anything may
change". Four things version independently: the Go API, the adapter contract,
the store interface with its stored schema, and the wire and event formats.

## What is frozen

| Surface | Contents |
| --- | --- |
| Core API | `hop.top/uma`: `New`, `Config`, `RetentionPolicy`, `WorkPolicy`, `PurgeReport`, `RetryPolicy`, `Engine`, `Client`, the 14 service interfaces, `Runtime` and its option types. Packages `model`, `policy`, `ports` and `ports/portstest` |
| Adapter contract | package `adapter`: `Descriptor`, `Factory`, `Instance`, `Sink`, the capability interfaces, `ActionValidator`, `RecipientNormalizer`, `LegacyRecipientReuser`, `ReceiverReporter` and `ReceiverDegradationReporter`; `adapter.ContractMajor` is 3 |
| Store interface | `store.Store` (3 methods), `Reader` (55) and `Tx` (embeds `Reader`, adds 46): 101 methods, plus `Properties` and the unit-of-work rules in the `store` godoc |
| Conformance suites | `RunStoreSuite`, `RunServiceSuite`, `RunDurableLogicalSuite`, `RunDurableSuite`, `RunAttachmentSuite`, `RunBlobStoreSuite` and `RunAdapterSuite`, with their harness types. Where the godoc is silent, the suites define what a backend or adapter must do |
| Wire formats | the JSON of the `model` types: snake_case keys, `omitempty` rules, millisecond durations. 168 golden files pin them |
| Persisted formats | stored record bodies, and the PostgreSQL schema (version 26, forward-only migrations) |
| Event envelope | `model.Event` and the builtin event payloads at payload major 1 |
| Operation identity | the operation key scope, the fingerprint v1 layout and the extension intent encoding |

## What is not frozen

- Adapters (`adapter/*`, including the in-module `adapter/web/widget` and
  `adapter/agent/a2a`). Every descriptor declares `experimental`, and adapter
  configuration schemas and extension names may change. Adapters stay
  experimental until their provider traffic is recorded and committed as test
  cassettes. A change to what an adapter keys an identity on is announced in
  the release notes and on the adapter's page; the domain case of a
  `google.mail` and `microsoft.mail` sender is one
  ([google.mail](adapters/google.mail.md#identities-and-start-recipients)).
- `companions/bot`, `companions/bot/host` and `contrib/*`.
- The Go API of `store/postgres` beyond `store.Store`. Its schema is frozen, its
  constructor is not.
- `testing/xrrtest`, and everything under `examples/`.

Each of these versions under its own release component. Features documented but
not shipped, such as `PropagateToProvider`, read confirmations from Meta WhatsApp and Twilio, mentions and
channel types without an adapter, are outside the frozen surface and may be
added later; see [Out of scope for v0.1](../README.md#out-of-scope-for-v01).

## Go API

| Change | Class |
| --- | --- |
| Remove or rename an exported identifier; change a signature, a field type or a constant's value | breaking |
| Add a method to an interface hosts or third parties implement: `store.Store`, `Reader`, `Tx`; the `adapter`, `policy` and `ports` interfaces; the conformance harness types | breaking |
| Make an optional `Descriptor` field, `Envelope` field or capability interface required | breaking |
| Add a method to a service interface that uma implements (`Engine`, `Client`, the 14 services, `Runtime`) | additive; a host that wraps or mocks one embeds it |
| Add an exported identifier, a struct field whose zero value keeps today's behaviour, a new optional adapter interface found by type assertion (as `ActionValidator` and `RecipientNormalizer` are), or a new builtin capability, action or event name | additive |
| Add a value to a closed set (`model.ErrorCode`, `model.DeliveryState`, interaction kinds, capability and action names) | additive, announced in the release notes; callers keep a default branch. Removing or renaming a value is breaking |
| Tighten validation so an input that worked is refused | breaking, unless it closes a privacy or authorization defect; then it is a fix, announced in the release notes |

A breaking change needs a recorded decision, a `!` or `BREAKING CHANGE:` commit
so release-please raises the version, and a release-notes entry. While the
modules are `v0.x` it ships in a minor release. From `v1` it needs a major
release.

## Read marker events

The read-marker events ([record](decisions/desk-state-events.md)) touch the
surface in these places, and in no other:

| Surface | Change | Class |
| --- | --- | --- |
| Events | new kinds `conversation.read_marked` and `conversation.unread_marked` (schemas `uma.conversation.read_marked`, `uma.conversation.unread_marked`), payload major 1: conversation, actor, the marker's new revision. Resource is the conversation, so `Event.Inbox` is its inbox. Delivered to the marker's own actor alone, who must also hold `conversation.read_workflow` | additive, announced in the release notes |
| `model.ReadMarkerPayload`, `model.IsReadMarkerEvent`, `model.EventConversationRead`, `model.EventConversationUnread`, `model.SchemaConversationRead`, `model.SchemaConversationUnread` | new identifiers | additive |
| `Conversations.MarkRead`, `MarkUnread` | a call that moves the marker appends one event in the same unit; one that leaves it unchanged emits none. Signatures and results unchanged | additive |
| `store.Store`, `Reader`, `Tx`, persisted formats, PostgreSQL schema | none | unchanged |

## Held operations

The held-operation decision ([record](decisions/held-operations.md)) touches the
surface in these places, and in no other:

| Surface | Change | Class |
| --- | --- | --- |
| `model.DeliveryState` | add `held` and `expired`; `rejected` is reused | additive, announced in the release notes |
| `model.Operation` | add an optional held block (`omitempty`): reason, deadline, proposed fingerprint, stored intent, edit history | additive; goldens updated in the same commit |
| Builtin actions | add `operation.approve`, `operation.reject`, `operation.edit`, `trust.read`, `trust.reset`, `trust.write` | additive |
| Service interfaces uma implements | add approve, reject, edit and trust methods (`Operations.Approve`, `Reject`, `Edit`, `Trust`, `Trusts`, `ResetTrust`, `ResetTrustSet`, `ImportTrust`, with `model.DecideHeld`, `EditHeld`, `Decided`, `ImportTrust`) | additive; a host that wraps one embeds it |
| `model.Command` | add optional `Hold *model.HoldProposal` (`omitempty`), carried by every write request; not part of any fingerprint. Only commands whose approval can be applied can be held (message edit, delete and react, reply, notes, conversation labels and state verbs, start); others with a hold are `Unsupported`, and so are label configure, label delete and conversation transition until `Config.Held` opts them in | additive |
| `uma.Config` | add `Held HeldPolicy` (`LabelConfigure`, `LabelDelete`, `ConversationTransition`, `StartCreateAtHold`), zero value opts none in and creates a held start's conversation on approval | additive |
| `model.HoldProposal`, `HeldBlock`, `TrustEntry`, `TrustLookup`, `TrustFilter`, `ResetTrust`, `ResetTrustSet`, `ImportTrust`, `HeldOperationPayload`, `MaxTrustKeyBytes`, `MaxTrustBatchBytes` | new types and constants | additive |
| `model.DecideHeld.Batch` | optional field; empty keeps each approval its own batch | additive |
| Events | the operation state event carries the new state values; new kinds `operation.held`, `operation.decided`, `operation.expired` (schemas `uma.operation.held`, `uma.operation.decided`, `uma.operation.expired`), payload major 1, with optional `verb` and `target` fields; the event's `inbox` is its target's, and `Event.Validate` accepts an inbox on an event whose resource is an operation | additive, announced in the release notes |
| `store.Store`, `Reader`, `Tx` | none: counters are protocol records under the reserved `uma.trust.` keys | unchanged |
| Persisted formats | add the `uma.trust` record body, version 1, with 64 fixed bucket keys | additive; the bucket count and hash are frozen once shipped |
| Operation identity | key scope and fingerprint v1 untouched; the proposed fingerprint is kept for replay | unchanged |
| After the freeze | the held surface above (states, held block, actions, event kinds and payload, `Config.Held` fields, trust methods and the `uma.trust` record) gains optional fields and new values only. Removing or renaming a state, decision, action, event kind or field, or changing the bucket count or hash, is breaking | additive-only |
| PostgreSQL schema | none | unchanged |

Announcement for the release notes (generated from the commits, no
hand-written notes file exists): new `model.DeliveryState` values `held` and
`expired`, new event kinds `operation.held`, `operation.decided` and
`operation.expired`, new builtin actions `operation.approve`,
`operation.reject`, `operation.edit`, `trust.read`, `trust.reset` and
`trust.write`. Callers that switch on a state or an event kind keep a default
branch.

## Sender attributes at ingress

An adapter reports the verified attributes of a message's sender, and the
engine records them on the sender's identity at its first claim, so a host's
`verified_attribute` verifier can link identities of one person. The identity
key stays `IdentityKey`; nothing keys on an attribute.

| Surface | Change | Class |
| --- | --- | --- |
| `model.AttributeKind` | add `whatsapp_user_id` (a WhatsApp business-scoped user ID); `model.Attribute.Validate` accepts it, and `model.NormalizeWhatsAppUserID` checks it | additive, announced in the release notes; callers keep a default branch |
| `model.Envelope` | add optional `SenderAttributes` (`sender_attributes`, `omitempty`): at most `model.MaxContactAttributes`, valid and normalized, no kind and value twice, only on envelope kinds that claim a sender; a violation is `validation` and stores nothing. An adapter that sets none behaves as before | additive; goldens updated in the same commit |
| `model.Identity` | none: `Attributes` already existed and is persisted in the record body | unchanged |
| `store.Store`, `Reader`, `Tx` | none: `ClaimIdentity` already takes the whole identity | unchanged |
| Conformance suites | add the service case `IngressSenderAttributesRecordedAtClaim` (identity attributes stored sorted, first claim wins, linking through a verifier on shared attributes); a backend that persists the identity body whole passes it | additive; a new case, no new method |
| `twilio.whatsapp` | reports the phone number and the user ID of the sender | adapter, not frozen |
| PostgreSQL schema | none: attributes live in the identity's `body`; the ingress record body golden gains the optional `sender_attributes` | unchanged |

Where the contact write path treats attributes as unverified (`Contacts.Create`
and `Update` refuse `verified: true`), ingress accepts the adapter's assertion,
as the `Attribute` type documents. A contact write may now also carry
`whatsapp_user_id`, unverified.

## Adapter contract major

The engine registers a descriptor only if its `ContractMajor` equals
`adapter.ContractMajor`, which is 3. The check protects against an adapter built
for a contract whose behaviour differs from the engine's, which the compiler
cannot see.

- **Bump** the integer, in one commit with a migration note in the `adapter`
  godoc, when a breaking change from the table above touches the adapter
  contract, or when behaviour an adapter relies on changes without a signature
  change (for example the meaning of a field in `Envelope`, `Effect` or
  `SendRequest`).
- **Do not bump** for additive changes: new optional interfaces, optional
  fields, new capability names, new `ActionValidator`- and `RecipientNormalizer`-style hooks. A factory
  without the new hook keeps working.
- **Support window.** The engine accepts the current major only; there is no
  multi-major dispatch. Adapters release in step with the major. A bump lands in
  a minor release of `v0.x`, never in a patch.
- The same rule covers the fake provider and any third-party adapter. The adapter
  conformance suite is the test that an adapter meets the major it declares.

## Store interface and persisted schema

- Adding, removing or changing a method on `Store`, `Reader` or `Tx` is breaking
  for every backend. It bumps the minor version and comes with a migration note
  in [Writing a store backend](store-backends.md). The compiler catches it for a
  Go backend and the conformance suites catch a change in behaviour. There is no
  separate store contract number: `uma.New` does not compile against a backend
  with a different method set, and the suites pin behaviour.
- A new conformance case that a backend passing the previous release fails is
  breaking and ships with a release note. New groups for new methods are not.
- The backend versions its storage schema (`Properties.SchemaVersion`; 26 for
  PostgreSQL). Migrations are forward-only and numbered, and a release applies
  every migration since the previous release. A migration that cannot run on
  existing data, or that drops a column the previous release reads, is breaking.
  A database newer than the binary is refused, and so is one that was not
  migrated. There is no downgrade path.
- Stored record bodies follow the wire rules below. The PostgreSQL body goldens
  are the signal.

## Wire, events and operation identity

- A change to a golden file is the signal. Adding an optional (`omitempty`) field
  is additive and updates the golden in the same commit. Removing or renaming a
  key, changing a type or a unit, or giving an existing key a new meaning is
  breaking.
- Builtin event payloads carry a major (`PayloadMajor`, today 1). A new optional
  field keeps the major. Removing or retyping a field, or changing what a field
  means, increments it, and a subscription opens at the major its consumer
  understands.
- Fingerprint v1 is frozen. A change to what a command's fingerprint covers is a
  new layout version; operations stored under v1 keep comparing as v1 until they
  leave the retention window.
- A consumer must ignore event fields it does not know, and must not treat an
  unknown event kind as an error.

## Conformance suites

The suites are part of the contract. Changing an exported suite function, harness
type or group name is breaking. Adding a case is governed by the store rule
above and, for adapters, by the contract major. Case names inside a group are
not API; [API coverage](api-coverage.md) names groups, not cases.

## Making a change to a frozen surface

Every such change carries four things: a recorded decision or a dated note in the
pull request, the version bump above, the golden or suite update, and a
release-notes line. Reviewers check the list. Nothing is tagged without explicit
authorization.

## Enforcing the Go surface

When the first tag is cut, CI starts comparing each released module's exported
API against the previous tag (for example with `gorelease` or `apidiff`), so the
freeze is enforced by a tool and not only by review. The comparison cannot run
before a tag exists.

## `v0.x` and `v1`

Until `v1`, the modules stay on `v0.x` through the release-please prerelease
channel, and the policy above governs what may change in them. Pin the version
you build against. A breaking change ships in a minor release with a release
note. No date is set for `v1`. From `v1`, a breaking change
needs a major release.
