# Implementer contracts

What core guarantees and what an adapter implementer owns. Each section
states one split; the godoc of the named types is authoritative for detail.

## Extension data: who validates what

Extensions carry data core does not interpret: a channel configuration, the
action of an extension effect (`Extensions.Execute`), the query of an
extension read (`Extensions.Query`) and the payload of an extension event.
Core checks the envelope; the adapter owns the fields.

| Check | Owner | When | Failure |
|-------|-------|------|---------|
| Name is a registered definition of the right kind | core | at the call, before anything is stored | `model.Unsupported` (unregistered), `model.SchemaMismatch` (other version or kind) |
| Version is registered | core | same | `model.SchemaMismatch` |
| Data is valid JSON | core | same | `model.Validation` |
| Data is within the structured payload limit (`model.DefaultContentLimits().MaxStructuredBytes`) | core | same | `model.Validation` |
| Fields: required, types, values, combinations, audience rules | adapter | see below | `model.Validation` |

Core keeps the data opaque past those checks. Fields it does not know reach
the adapter unchanged, within the declared name and version and under the
request's audience. Core never validates the fields against a schema.

### When the adapter validates

| Data | Adapter hook | Before durable acceptance? |
|------|--------------|----------------------------|
| Channel configuration | `Factory.Validate`, without I/O, via `Configuration.ConfigureChannel` and `ValidateChannel` | yes: a configuration it refuses is never stored |
| Extension read | `ExtensionReader.Query` | nothing is stored by a read, so a refusal is a `model.Validation` error to the caller |
| Extension effect, fields | `ActionValidator.ValidateAction` on the factory, without I/O, optional | yes: a refusal is a `model.Validation` error at the call and nothing is stored |
| Extension effect, at the attempt | `ExtensionExecutor.Execute` | no: the stored effect is validated again at the attempt |

An extension effect is accepted as durable intent before any provider call,
as every outbound effect is. A factory that also implements
`adapter.ActionValidator` is asked about the action's fields first, with the
action and the audience it is sent under:
`ValidateAction(audience, action) []model.ValidationIssue`. Core refuses on
the first issue with a `model.Validation` error from `Extensions.Execute`,
before policy and before anything is stored, so the operation ID stays free.
The hook does no I/O and keeps no state; the name and version are registered
by then and select the rules. Core does not repeat it at the attempt.

`Execute` validates regardless, before any provider call, and answers fields
it refuses with a final `model.Rejected` outcome whose failure is
`model.Validation`: it is the guard for an action stored before a rule
tightened, and the only check of a factory without the hook. For that
factory the caller sees a queued operation that settles rejected, not an
error from `Extensions.Execute`. Share one set of rules between the hook and
`Execute`. Implementing the hook is optional and not a contract change: the
contract major stays as it is, and a factory without it keeps working.
Fields that arrive malformed or oversize never get that far: core refuses
them at the call with nothing stored, hook or not.

Rules for an adapter:

- Validate every field `Execute` or `Query` reads. Never pass an unvalidated
  action to the provider.
- Implement `ActionValidator` when the adapter has field rules, so a caller
  learns of a bad action at the call instead of from a settled operation.
- Refuse with `model.Validation`, a safe message and no retry. A refusal made
  before the provider call leaves the provider untouched.
- Ignore fields you do not know rather than refusing them, unless the
  provider would misread them: they round-trip by contract.
- Validate configuration without I/O in `Factory.Validate`; reach the
  provider only when an instance is opened.

### Policy action of a definition

A definition's `AuthorizeAs` is a namespaced action or a builtin one. Core
classifies every builtin action as read-only, effect-eligible or core-only,
by an explicit table, not by the shape of the name:

- A read-only action grants reading only: `message.read`, `event.list`,
  `operation.read`, `extension.query`, `identity.history` and the other
  read actions. An event kind may authorize as one; an effect may not.
- An effect-eligible action writes through a provider resource:
  `message.reply`, `message.note`, `work_item.act`, `task.act`,
  `conversation.transition`, `extension.execute` and similar. An effect may
  authorize as one; an event kind may not.
- A core-only action is bound to a core flow: `contact.write`,
  `contact.merge`, `identity.link` and its siblings, `operation.replay`, `operation.reconcile`,
  `subscription.*`, `channel.configure`, `channel.receive`, `inbox.configure`, attachment
  staging, `stream.write`, `privacy.*` and `label.configure`. Neither kind
  may authorize as one.

`uma.New` refuses a definition that breaks the rule with a `model.Validation`
error naming the definition. A namespaced action is never refused for its
class.

### Conformance

`conformance.RunAdapterSuite` checks the split for every adapter declaring
`extension_execute`:

- `InvalidDataRejectedBeforeCommit`: malformed JSON and an oversize payload
  are refused with `model.Validation` at the call. The same operation ID then
  accepts a valid action as a new operation, neither replayed nor in
  conflict, and the provider sees only that one.
- `InvalidFieldsNeverApplied`: set `AdapterHarness.InvalidAction` to an action
  that is valid JSON the adapter refuses. The provider must never apply it.
  When the factory implements `ActionValidator`, the case requires the
  `model.Validation` error at the call, with nothing stored: the operation ID
  then accepts a valid action as a new operation. Without the hook, the case
  requires the stored operation to settle rejected with `model.Validation`.

## Receivers: failure and reconnect

An adapter declaring `receive` implements `adapter.Receiver`: `Run` reads the
provider under the context until it fails or is stopped. `Runtime.Receive`
supervises it: each connection is a fresh instance, and the supervisor, not
the adapter, owns the reconnect policy.

| `Run` returns | The supervisor |
|---------------|----------------|
| the context's error after a stop | ends the call |
| a retryable `*model.Error` (`transient`, `unavailable`, `rate_limited`), an untyped error or nil | reconnects after the backoff |
| any other `*model.Error` (a denial, `disabled`, `permanent`) | ends the call with it |

The backoff is the engine's `Retry` policy: a full-jitter exponential wait,
paced by the configured `Waiter`. A failure that states a wait, the
`RetryAfter` of a `model.Error` such as a provider's rate limit or session
start budget, replaces the backoff when longer, from the first failure. A
stated wait is capped at 24 hours; one that is not stated, zero or shorter
than the backoff leaves the backoff as it is. The wait ends at once when the
call's context is cancelled, the tenant drains or the engine closes.
`Retry.MaxAttempts` consecutive connections that accept nothing still end
the call with the last failure, so a failure that states a wait counts like
any other, and no wait follows the one that ends the call. A connection that
accepted ingress resets the count.

An adapter that can wait inside `Run`, as the Slack Socket Mode receiver does
for the rate limit of `apps.connections.open`, spends no attempt doing so.
One that ends `Run` with the provider's stated `RetryAfter` gets that wait
from the supervisor.

### What a host can see

`Runtime.ReceiverStatus` names the owner of a channel's receiver lease and,
from the process that holds it, what that `Receive` call is doing. The extra
fields are read from the process's memory: a lease another process holds
shows its `Owner` and `Until` and an empty `Phase`, so ask the owner. They
are empty too for a lease that expired and after `Receive` returned, and a
crash loses them: the next `Receive` starts at `running` with no failures.
Nothing is stored, and none of it changes a retry or lease decision.

`ReceiverStatus` reads the store (the channel and its lease) on every call.
A host that polls the phase of the receivers it runs, per channel on a
timer, should call `Runtime.ReceiverState` instead: it reads memory only,
returns `false` for a channel this process runs no `Receive` for, and takes
the same `RuntimeCall` (a principal of the channel's tenant). `Until` is
zero in it, and it keeps reporting a call whose lease just lapsed until
that call stops.

| Field | Meaning |
|-------|---------|
| `Phase` | `running` while a connection is being opened or is open; `retrying` between a failed connection and the next; `ended` while the call releases its lease, until `Receive` returns with the reason |
| `Since` | when the phase began, by the engine's clock |
| `Failures` | consecutive connections that accepted nothing, the count `Retry.MaxAttempts` bounds; zero once a connection accepted ingress |
| `Reason` | the `model.ErrorCode` of the last such failure, empty when none; never a message |
| `RetryAt` | while `retrying`, when the next connection opens at the earliest: the backoff, or the wait the failure stated, up to 24 hours |
| `Link` | while `running`, what the adapter says of its provider link: `connecting` or `connected`, or `degraded` (below); empty when the adapter reports neither |
| `Detail` | while `Link` is `degraded`, the adapter's stable code for what is failing (`google.mail.push_denied`); empty otherwise; never a message |

`running` is not "connected": `Run` is executing, and an adapter may retry
inside it (the Discord receiver reconnects three times before `Run`
returns). `running` with `Failures` above zero is a call reconnecting after
failures. Only the adapter knows its link, through the optional interface
below, so `Link` stays empty for adapters that do not implement it. A quiet
channel and a stuck one look alike in the envelopes; `Link` and `Phase` tell
them apart.

### Reporting the connection

An adapter whose receiver has a provider link worth showing implements
`adapter.ReceiverReporter` on its `Instance`, found by type assertion:

```go
type ReceiverReporter interface {
	ReportReceiver(report func(connected bool))
}
```

- **Who calls what.** The supervisor calls `ReportReceiver` once per
  instance, on its own goroutine, before `Run`; store `report` and return.
  The receiver calls `report(true)` when its link is up (a gateway session
  established, a socket said hello, a poll answered) and `report(false)`
  when it is lost or being re-established, from any goroutine, as often as
  it likes. `report` is safe for concurrent use, runs no adapter code and
  returns at once, so it may be called while holding the adapter's locks.
- **Ordering with `Run`.** Until the first call the link reads `connecting`.
  The link ends with `Run`: a call made after `Run` returned, or through
  an earlier connection's `report`, has no effect, and reporting `false`
  before returning is not needed.
- **Advisory.** The signal never changes a retry, a wait, an attempt count
  or the lease. What ends or restarts a connection is what `Run` returns. A
  `ReportReceiver` that panics is treated as absent; an adapter that never
  calls `report` reads `connecting` for as long as it runs.

An adapter whose receiver keeps working while something it relies on fails
(a mail receiver that still polls after its push registration failed)
implements `adapter.ReceiverDegradationReporter`, independent of the one
above:

```go
type ReceiverDegradationReporter interface {
	ReportReceiverDegraded(report func(code string))
}
```

The supervisor hands over `report` once per instance, before `Run`, as for
`ReportReceiver`. `report(code)` marks the link `degraded` with `code` as
`Detail`; `report("")` clears it, and the link reads what `ReportReceiver`
last said (`connecting` until then). The code is a short stable token the
adapter documents, never a provider message or a secret. Like the link
report it is connection-scoped (a report through an earlier connection has
no effect), safe for concurrent use, and advisory: no retry, wait, attempt
count or lease decision follows from it. The runtime emits no event for link
changes, so hosts read it through `ReceiverStatus` / `ReceiverState`. The
mail receivers report the codes of their adapter pages.

### Starting many receivers

`Receive` takes `uma.ReceiveStartSpread(d)` (`ReceiveOptions.StartSpread`):
the first connection waits a stable offset in `[0, d)` taken from a hash of
the channel, so a host starting hundreds of receivers at once does not open
all their first connections together. The same channel always gets the same
offset, reconnects are not delayed, zero (the default) starts at once, and
the lease is held and renewed during the wait. Renewals of the lease are
themselves spread: the first at a per-channel offset in `[period/2, period)`,
then every 20 seconds +-10%, never later than 22 seconds after the last, so a
renewal is attempted at least 38 seconds before the 60-second lease expires.

Add the interface only where "connected" means something a host can act on.
The receivers of `discord.chat` (gateway ready or resumed), `slack.chat`
(Socket Mode hello), `telegram.messages` (a long poll answered) and
`imap.mail` (a session authenticated) report it.

## Receipts, membership and history

Three optional interfaces serve these capabilities: `adapter.ReceiptSender`
(`receipt`), `adapter.MembershipChanger` (`membership`) and
`adapter.HistoryReader` (`history`). Core queues a receipt or a membership
change as an operation, runs it once through the worker and reconciles a lost
answer, as it does for an edit. The adapter owns the provider call and its
refusals: a receipt kind the provider has none for is a final
`model.Rejected` outcome with `model.Unsupported`, made before any provider
call. A history reader returns pages oldest first, at most `Limit` envelopes
each, of the messages occurring at or after `Since`. Each envelope is the one
the adapter reports inbound for that message, with the same provider message
ID and destination, so `Runtime.Import` skips a message ingress already
holds. A provider that lists newest first needs the reader to reorder the
window. `HistoryRequest.Stager` is the channel sink's `adapter.AttachmentSink`,
set on every read: a reader whose provider hands over attachment bytes stages
them through it as the inbound path does (same keys and bounds), and names the
staged attachments in the content; one without bytes to stage ignores it.

### Conformance

`conformance.RunAdapterSuite` runs these cases only for an adapter declaring
the capability; for every other adapter they skip.

| Group | Case | What it requires |
| --- | --- | --- |
| `Receipts` | `ReceiptTargetsReceivedProviderMessage` | Each kind the provider sends reaches it once, on the received message's stored provider message and the conversation's route, and changes nothing in uma |
| `Receipts` | `ReceiptRetriedSubmitSentOnce` | A submission retried before and after delivery is replayed; the provider sees one receipt |
| `Receipts` | `ReceiptKindWithoutProviderReceiptRefused` | A kind in `AdapterHarness.UnsupportedReceiptKinds` settles rejected with `model.Unsupported`, and the provider sees no receipt |
| `Receipts` | `ReceiptRejectedIsFinal`, `ReceiptAnswerLostIsNeverResent`, `ReceiptRevokedPermissionMakesNoCall` | A refusal is final; a lost answer is never resent, and is reconciled when the adapter declares `reconcile`; a revoked permission makes no call |
| `Membership` | `MembershipAddThenRemove` | The addition and the removal each reach the provider once, on the conversation's route, for the identity as the adapter reports it inbound; the participant joins and leaves only once the provider applied the change; a retried submission is replayed |
| `Membership` | `MembershipRejectedLeavesConversation`, `MembershipAnswerLostIsNeverResent`, `MembershipRevokedPermissionMakesNoCall` | As for receipts; a refused or lost change leaves the conversation as it was |
| `History` | `HistoryWindowOldestFirstInBoundedPages` | Pages of limit 1, 2 and 3 all return exactly the window's messages, oldest first, each once, with the destination ingress reports; the message at `Since` is in, the one before is out; the first page reads the same twice; nothing is applied at the provider |
| `History` | `HistoryConversationOrChannel` | A route returns that conversation's messages; no route returns the channel's |
| `History` | `HistoryUnknownCursorIsValidation` | A cursor the provider never issued is `model.Validation` |
| `History` | `HistoryImportSkipsInboundCopy` | `Runtime.Import` skips a message received through ingress and records the others once, marked `ImportedBy` |

The harness supplies the provider side:

- Receipt cases need a received message: the adapter declares `receive`, or
  `receive_http` with a `conformance.WebhookProvider`. Otherwise they skip.
  The provider reports a receipt call with `ProviderCall.Kind` set to the
  receipt kind.
- Membership cases use the sender of a second inbound message as the new
  participant, or, for an adapter that does not receive, an identity stored
  for the provider route to a second peer. The provider reports a
  membership call with `ProviderCall.Participant` and `ProviderCall.Remove`.
- History cases need the provider to implement `conformance.HistoryProvider`,
  whose `HoldHistory` places a past message on the account. Without it they
  skip. `HistoryImportSkipsInboundCopy` also needs inbound traffic.

## Raw message reads

`adapter.RawReader` (`raw_read`) serves `Messages.Raw`: one synchronous
provider read of a message's whole copy, from the provider reference core
stored for it (`adapter.RawRequest.Message`), returned as
`adapter.RawMessage` (the bytes and their media type, `message/rfc822` for
mail). Core authorizes the read as `message.read` under the message's audience,
refuses a redacted or deleted message and one without a provider reference,
and stores nothing of the answer, so the reader keeps nothing either. The
adapter owns the bound: read no more than the size limit its inbound reads
already keep, and answer a message over it as `model.Permanent` with
`model.TooLarge`, never a truncated copy. A message the provider no longer
holds is `model.NotFound`; a reference of another channel, namespace or kind
is `model.Validation`, before any request. The read must change nothing at the
provider (IMAP selects read-only and fetches with `BODY.PEEK[]`).

The adapter suite's `RawRead` group runs for an adapter declaring `raw_read`
whose provider implements `conformance.RawProvider`: the bytes are the
provider's copy, a read applies nothing and changes nothing in uma, and an
unknown message is `not_found`; an adapter without the capability gets
`unsupported` with no provider call.

## Delivery status reports

A provider that tells you what became of a send reports it as a status
envelope: `Kind` `model.EnvelopeStatus`, a `Delivery` report, and the attempt it is
about as `model.EchoRef(channel, operation, attempt)` in `References`. Carry
`Effect.Operation` and `Effect.Attempt` in the outbound payload, where the
provider round-trips them (a callback URL parameter, an opaque data field), and
read them back on the status. A report naming no attempt, or one the engine
does not hold, is kept as a status and moves nothing: core never guesses an
attempt from a provider message ID. `adapter.Sink.Accept` answers with a
receipt whose `Operation` names the operation the report reached, and is nil
when it reached none; an adapter that answers a webhook can pass that on.

Map the provider's values to the report, and no further:

| The provider says | `DeliveryReport` |
| --- | --- |
| accepted, queued, sent | `State: ProviderAccepted` |
| delivered | `State: Delivered` |
| read, played | `State: Delivered` and `ReadAt` set to when it was read, if the provider says; otherwise just `Delivered` |
| failed, undelivered, canceled | `State: Failed` with a `DeliveryFailure` (`Permanent`, `NoRetry`, a safe summary that repeats none of the provider's words) |

An adapter reports each provider update as it arrives, in whatever order, and
does not remember what it reported before: the engine decides what a report
changes.

### What a report may move

The engine ranks the evidence it holds, so order, repeats and replicas do not
matter. From lowest to highest: provider_accepted (accepted and queued and
sent), failed, delivered, delivered with a read time. A report can move an
operation up and never down; one it already outranks is recorded and changes
nothing.

| The operation is | A report of | Becomes |
| --- | --- | --- |
| outcome_unknown (the answer was lost) | any | the highest rank among the reports held, in one settle |
| provider_accepted | delivered; read; failed | delivered; delivered and read; failed (final) |
| failed | delivered or read | delivered, the failure cleared |
| delivered | read | delivered, `ReadConfirmedAt` set once |
| delivered or failed | sent, repeats, a lower rank | unchanged |

Only a reply and a conversation start move on after the provider accepted
them. A failure fails the operation only when it is about the latest attempt
and no earlier attempt was applied; delivery of any attempt delivers it. Each
real change is one `delivery.updated` event (trigger `provider`), written by the
unit that changed the operation; the first read of a delivered operation is one
more, with `state` and `previous_state` both `delivered`.

The adapter suite does not exercise this path: it belongs to the engine. The
durable suite's `Faults` and `Restart` groups run it over every store (the
`Status*` cases): a report acted on by the process after the one that took it,
repeats and out-of-order reports, and the ranking of reports that arrive before a
lost answer is settled.

Do not declare `receipt` for any of this. That capability is the outbound
direction, uma telling the sender of a received message that it was delivered
or read (`adapter.ReceiptSender`); a provider's report to uma is part of
receiving.

## Start recipients: one spelling per identity

A start names its recipient before the provider has ever reported it. In a
per-contact inbox core claims the recipient's identity on the bytes it is
given, as it does for an inbound sender, and fingerprints the start on them. An
identifier a provider treats as one but spells two ways, such as an email
address that ignores case, then becomes two identities and two conversations,
and a replay under another spelling a conflict.

A factory that implements the optional `adapter.RecipientNormalizer` closes
that gap: `NormalizeRecipient(recipient) (model.ExternalRef, error)`.
`Conversations.Start` calls it on the request's recipient after the inbox, its
channel and the `start_conversation` capability are known, before the start is
checked, fingerprinted or stored. Identity, conversation route, fingerprint
and the provider call all see the result, so one provider identity is one
identity whatever the caller typed, and the same operation key with another
spelling is the same operation.

| Rule | Why |
|------|-----|
| Return the spelling the adapter's inbound reports for the identity | A reply from the recipient must land on the identity the start created. Core normalizes nothing inbound; the adapter keeps doing that itself |
| No I/O, no state, idempotent | It runs on every start, before any write, and a replay normalizes again |
| Change `ID` only | A result that differs in `Channel`, `Namespace`, `Kind` or `ParentID`, or is not a valid reference, is refused as a `model.Permanent` adapter fault with nothing stored |
| Refuse with an error to refuse the start | It is a `model.Validation` error at the call, before any write, so the operation ID stays free. A `*model.Error`'s safe message is kept; any other error becomes a fixed message |

Only a recipient of the inbox's channel reaches the hook; another channel's is
refused as before. It does not run for a send, a reply or any other command,
which route on what the conversation stored. Identities and conversations
stored before an adapter began normalizing keep their stored spelling; core
looks nothing up under the normalized one and migrates nothing, and an
operator can merge the contacts afterwards. Implementing the hook is optional
and not a contract change: the contract major stays as it is, and a factory
without it keeps the recipient exactly as sent. `relay.messages` implements it
for iMessage email addresses; see
[relay.messages](adapters/relay.messages.md#mediums-and-identities).

The mail adapters `smtp.mail`, `imap.mail`, `google.mail` and
`microsoft.mail` implement it with one rule, `model.NormalizeEmail`: the domain
lowercased, the local part byte-exact, because RFC 5321 makes the domain
case-insensitive and reserves the local part's case to the receiving host.
Their inbound sender key is the same string, so a start to `Bob@EXAMPLE.org`
and a message from `Bob@example.org` are one identity, while `Bob@` and `bob@`
stay two. Local-part folding, punycode conversion, plus-addressing and provider
dot rules are not applied. `smtp.mail`, `imap.mail` and `google.mail` refuse a
recipient that is not a bare address at the call, which a send would have
refused for good. `microsoft.mail` keeps one as given, because its send accepts
any plain token. `http.mail` declares no `start_conversation`, so it has
nothing to normalize. `google.mail` and `microsoft.mail` used to key a sender
on the address byte-for-byte, so a returning sender whose address domain has an
uppercase letter is a new identity after the change; see
[google.mail](adapters/google.mail.md#identities-and-start-recipients) and
[microsoft.mail](adapters/microsoft.mail.md#identities-and-start-recipients).
Adapters whose identifiers are opaque provider IDs (Telegram, Slack, Discord,
Teams) or E.164 numbers (Twilio, Meta WhatsApp) have only one spelling and need
no hook.

### Reusing identities stored under the typed spelling

By default core looks nothing up under another spelling of the recipient. An
adapter whose recipients were stored as typed before it began normalizing
can also implement the optional `adapter.LegacyRecipientReuser`:
`ReuseLegacyRecipient(typed) bool`. It is asked only in a per-contact inbox and
only when `NormalizeRecipient` changed the ID. On `true`, and only when the
normalized spelling has no identity in the inbox, `Conversations.Start` reads
the identity under the typed spelling (`Reader.Identity`, an exact point read,
at most two reads in one read unit before the claiming unit; nothing is read
when the spelling did not change, and the claiming unit gains no read). If one
is stored, the start claims it rather than the normalized one, so it joins that
identity's conversations. The fingerprint stays keyed on the normalized
recipient.

| Rule | Why |
|------|-----|
| Say `true` only if a conversation routed to the typed spelling can still be sent to | A reused conversation keeps its stored route. A conversation the start opens for the legacy identity routes to the normalized spelling |
| Expect it to be partial | It finds the identity only when the caller types the address as before. Any other spelling claims the normalized identity, and inbound keeps claiming it too. There is no case-insensitive lookup, which would need a new store method and index |
| The normalized identity wins | If both spellings have an identity the start uses the normalized one, with no error |
| No I/O, no state | A pure function of the typed recipient |

Contacts that already split are joined with `Contacts.PreviewMerge` and
`Contacts.Merge`. Implementing the hook is optional and not a contract change:
the contract major stays as it is.

