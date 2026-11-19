# API coverage

Which public methods of `hop.top/uma` work today, and on which stores they
are tested. A method signature in `uma.go` is not a promise that it works:
check its row here before you build on it.

## How to read the tables

**Status** says what the engine does when you call the method:

- `implemented`: the method has its documented behaviour.
- `partial`: the method works, except for the inputs the note names, which
  return a `*model.Error` with code `model.Unsupported`.
- `unsupported`: every call returns `model.Unsupported`, whatever the
  adapter declares. The method's godoc says so too.

**memory** and **postgres** say how the method is tested on that store:

- `conformance`: the shared suites the store runs (`RunServiceSuite`,
  `RunAttachmentSuite`, `RunDurableLogicalSuite`) call the method on it.
  PostgreSQL also runs `RunDurableSuite`, which repeats the durable cases
  across a restart.
- `engine tests`: engine, service or adapter tests call the method on this
  store; the shared suites do not.
- `unverified`: the method runs on this store through the same store
  contract the store suite checks, but no test calls it there yet.
- `no store`: the method reads no stored state; it behaves the same on
  every store.
- `–`: the method is unsupported.

Each table has one row per method. A change that implements a method
updates that row only; `go test` fails when a row disagrees with what the
engine does.

What each adapter can do is declared in its descriptor: see
`Engine.Describe` and `Configuration.Capabilities`.

## Engine

| Method | Status | memory | postgres | Notes |
| --- | --- | --- | --- | --- |
| `Engine.For` | implemented | no store | no store | |
| `Engine.Runtime` | implemented | no store | no store | |
| `Engine.Describe` | implemented | no store | no store | Adapter descriptors as registered; not what the engine executes. |
| `Engine.ValidateChannel` | implemented | no store | no store | |
| `Engine.Close` | implemented | no store | no store | |

## Configuration

| Method | Status | memory | postgres | Notes |
| --- | --- | --- | --- | --- |
| `Configuration.ConfigureChannel` | implemented | conformance | conformance | |
| `Configuration.ConfigureInbox` | implemented | conformance | conformance | |
| `Configuration.Channels` | implemented | conformance | conformance | |
| `Configuration.Inboxes` | implemented | conformance | conformance | |
| `Configuration.ChannelByKey` | implemented | engine tests | unverified | |
| `Configuration.InboxByKey` | implemented | engine tests | unverified | |
| `Configuration.Capabilities` | implemented | engine tests | unverified | Declaration, intersected with engine support and the instance's current status. Every builtin capability is executed, `edit`, `delete`, `react`, `work_item_act`, `respond` and `respond_follow_up` included. Evidence: the discovery tests (`work_item_act` listed available, withdrawn, and absent when undeclared) and the adapter suite's `DiscoveryReportsEngineSupport` group, which runs `Messages.Edit`, `Messages.Delete`, `Messages.React` and `WorkItems.Act` rows against every adapter declaring them. |

## Contacts

| Method | Status | memory | postgres | Notes |
| --- | --- | --- | --- | --- |
| `Contacts.List` | implemented | conformance | conformance | |
| `Contacts.Identities` | implemented | conformance | conformance | |
| `Contacts.Get` | implemented | conformance | conformance | Admitted as `Contacts.List` admits; carries the revision `Contacts.Update` expects. |
| `Contacts.Create` | implemented | conformance | conformance | Authorized as `contact.write`, apart from channel configuration. Links no identity. Emits `contact.created` for the internal audience. |
| `Contacts.Update` | implemented | conformance | conformance | Replaces display name and attributes at `ExpectedRevision`; a stale revision is `conflict`. Emits `contact.updated` for the internal audience; a no-change update emits nothing. |
| `Contacts.Link` | implemented | conformance | conformance | Evidence verified by the registered `Config.IdentityVerifiers`. |
| `Contacts.Unlink` | implemented | conformance | conformance | |
| `Contacts.PreviewMerge` | implemented | conformance | conformance | Scans the conversations of the identities' inboxes for the ones they own, so a very large inbox gets a flagged partial list. |
| `Contacts.Merge` | implemented | conformance | conformance | Atomic over the contact and all its identities, at most `model.MaxMergeIdentities`; two merges touching one contact conflict cleanly on both stores. |
| `Contacts.History` | implemented | conformance | conformance | |

Walkthrough: [Contacts and identity links](../README.md#contacts-and-identity-links).

## Conversations

| Method | Status | memory | postgres | Notes |
| --- | --- | --- | --- | --- |
| `Conversations.Get` | implemented | conformance | conformance | |
| `Conversations.List` | implemented | conformance | conformance | |
| `Conversations.Transition` | implemented | conformance | conformance | |
| `Conversations.Start` | partial | conformance | conformance | Opens or reuses the recipient's conversation and queues the first message; needs an adapter declaring `start_conversation`. Inboxes in `per_work_item` and `per_agent_context` selection are unsupported. |
| `Conversations.AddLabels` | implemented | conformance | conformance | |
| `Conversations.RemoveLabels` | implemented | conformance | conformance | |
| `Conversations.Assign` | implemented | conformance | conformance | Local desk workflow state; the assignee must be eligible (the authorizer is asked as the assignee). Readable only with `conversation.read_workflow`. |
| `Conversations.Unassign` | implemented | conformance | conformance | |
| `Conversations.Snooze` | implemented | conformance | conformance | Query-time snooze: reads as awake at `Until` or on a live inbound message recorded after it was set, across restarts; imported history never wakes it. A running worker then clears the stored snooze and emits `conversation.snooze_ended` once (`Filter.SnoozeEnded` lists those still waiting). |
| `Conversations.Unsnooze` | implemented | conformance | conformance | |
| `Conversations.Link` | implemented | conformance | conformance | Conversation to conversation (held by both, one unit) or to a work item (held by the conversation); authorized on both ends; readers see only links whose far end they may read. |
| `Conversations.Unlink` | implemented | conformance | conformance | |
| `Conversations.MarkRead` | implemented | conformance | conformance | The caller's own read marker, a record of its own: no conversation revision, no event, no operation. Imported history never counts as unread. |
| `Conversations.MarkUnread` | implemented | conformance | conformance | Without a message, marks from the newest live inbound message that was not imported. |
| `Conversations.ReadMarker` | implemented | conformance | conformance | The caller's own marker only, behind `conversation.read_workflow`. |
| `Conversations.Capabilities` | implemented | conformance | conformance | The conversation's channel statuses as `Configuration.Capabilities` reports them, authorized as `conversation.read` under the internal audience instead of `channel.read`. |
| `Conversations.AddParticipant` | implemented | conformance | conformance | Queued under `membership`, executed by `Runtime.Run`; the identity joins `Participants` (role `participant`) with `conversation.changed` only once the provider applied it. A lost answer is reconciled, not repeated. No inbound membership envelope exists, so a provider-side change is not echoed back; a later message from the participant converges with it. `discord.chat` declares `membership` for thread members (its adapter suite runs the `Membership` cases per thread); the service suite's `Membership` cases over a scripted adapter and the fake provider's host test cover the engine side. |
| `Conversations.RemoveParticipant` | implemented | conformance | conformance | As `AddParticipant`; the owner of a conversation and an identity that is no participant cannot be removed (`conflict`). |
| `Conversations.Archive` | implemented | engine tests | engine tests | Queued under `archive`, executed by `Runtime.Run`; `Undo` unarchives. The adapter's reported scope (`account` or `shared`) picks `conversation.state_account` or `conversation.state_shared`; a capability that is undeclared, unavailable or reported without readable limits is `unsupported` before anything is stored. `Conversation.ProviderState` changes only once the provider applied it, never `Status` or `Labels`, and not when the same part advanced inbound since the call. Shown as `Operation.PendingState` until settled. `Messages` limits the change to the named messages of the conversation (each must be a live message of it with a provider reference on its channel, else `validation` and nothing is queued): the set is stored at submission, reaches the adapter as `Items` with `Targeted` set, and an adapter that changes state per thread (`google.mail`) then calls each message, never the thread. A lost answer is reconciled, not repeated. A `conversation.state` envelope from the adapter reports a provider-side change, and the echo of a change made here, into `ProviderState`. Evidence: the adapter suite's `State` group, which runs every state verb, its refusals and the inbound reports against each adapter declaring the capability (`google.mail`, `microsoft.mail`, `imap.mail`, `slack.chat`, `discord.chat` and the fake provider's state variants). See [Provider conversation state](../README.md#provider-conversation-state). |
| `Conversations.Move` | implemented | engine tests | engine tests | As `Archive` under `move`. The reported `shape` decides the request: `folder` takes a destination only, `labels` a destination or `Add` and `Remove`; a role the adapter does not report is `unsupported`. A destination named by role records no folder until the provider reports one. A move that cannot be repeated reconciles by looking in the destination. |
| `Conversations.SetRead` | implemented | engine tests | engine tests | As `Archive` under `read_state`. Provider read state only; the per-actor read marker (`MarkRead`, `MarkUnread`) is never changed or propagated. |
| `Conversations.Flag` | implemented | engine tests | engine tests | As `Archive` under `flag`. The kind must be one the adapter reported (`star`, `important`). |
| `Conversations.Trash` | implemented | engine tests | engine tests | As `Archive` under `trash`; `Undo` restores. Moves the account's own copy to the provider's trash and is not `Messages.Delete`. |

## Labels

Labels are canonical and local for every channel; provider labels such as
`github.issues.labels` are provider data with no implicit mapping. See
[Conversation labels](../README.md#conversation-labels). Provider-side
labelling is an extension effect of the adapter that offers it
(`google.mail.labels`, `microsoft.mail.categories`, through
`Extensions.Execute`, authorized as `conversation.label`) and touches no uma
label. A reply kept in the mailbox's Drafts for a person to finish is the
`google.mail.reply_draft` and `microsoft.mail.reply_draft` effects (authorized
as `message.reply`). `google.mail.unsubscribe` and `microsoft.mail.unsubscribe` run through
the same method, authorized as `extension.execute`, and change no label.

| Method | Status | memory | postgres | Notes |
| --- | --- | --- | --- | --- |
| `Labels.Configure` | implemented | conformance | conformance | |
| `Labels.Get` | implemented | conformance | conformance | |
| `Labels.List` | implemented | conformance | conformance | |
| `Labels.ByName` | implemented | conformance | conformance | |
| `Labels.Delete` | implemented | conformance | conformance | |

## Messages

| Method | Status | memory | postgres | Notes |
| --- | --- | --- | --- | --- |
| `Messages.Get` | implemented | conformance | conformance | |
| `Messages.Raw` | implemented | engine tests | unverified | The provider's whole copy of a message, read on demand and never stored; needs `raw_read` (`google.mail`, `microsoft.mail`, `imap.mail`), authorized as `message.read` under the message's audience. A redacted or deleted message is `conflict`; over the adapter's size bound is `permanent` with reason `too_large`. Evidence: the engine tests of `Messages.Raw` and the adapter suite's `RawRead` group, run by each mail adapter |
| `Messages.List` | implemented | conformance | conformance | Filters by inbox and conversation, by task (the task message history), and searches by `Text`, `Since`, `Before` and `Attachments` (`Has`, `MediaTypes`). Admission is per message; `MediaTypes` also requires `attachment.read` on a matching attachment. A search page that completes the query carries no `Next`, and one call scans at most 500 stored matches (see [Searching messages](../README.md#searching-messages), which also states the existence limit beyond that bound). `Filter.Labels` selects messages by the labels their conversation holds when the page is read, and also requires `conversation.read` on the message's conversation: a message whose conversation the caller cannot read is skipped. |
| `Messages.Reply` | implemented | conformance | conformance | Replies in an existing conversation; `Conversations.Start` opens one first. `InResponseTo` correlates the reply with an interaction of that conversation the caller can read (engine tests, memory); it never claims the interaction's response. `Recipients` (reply to all, Cc, Bcc) needs an adapter declaring `reply_recipients`; its adapter reports what it supports as limits (`model.RecipientLimits`). |
| `Messages.AddInternalNote` | implemented | conformance | conformance | |
| `Messages.Edit` | implemented | conformance | conformance | Queued, then executed by `Runtime.Run`; the content changes only once the provider applied the edit, and an edit answered after a newer inbound edit settles `edit_superseded` without overwriting it. Adapters declaring `edit`: the native web widget, `github.issues` (comments and issue descriptions), `gitlab.issues` (notes and issue descriptions), `jira.issues` (comments), `linear.issues` (comments), `slack.chat` (`chat.update`, the bot's own messages), `telegram.messages` (`editMessageText`, the bot's own messages), `microsoft.teams` (the Connector's update activity, the bot's own messages) and `discord.chat` (Edit Message, the bot's own messages); `meta.whatsapp` has no edit call. |
| `Messages.Delete` | implemented | conformance | conformance | Queued, then executed by `Runtime.Run`; once the provider applied it the message becomes a tombstone (`deleted`, content and reactions cleared). Adapters declaring `delete`: the native web widget, `github.issues` (comments; an issue description cannot be deleted), `gitlab.issues` (notes; an issue description cannot be deleted), `jira.issues` (comments), `linear.issues` (comments), `slack.chat` (`chat.delete`, the bot's own messages), `telegram.messages` (`deleteMessage`, within 48 hours), `microsoft.teams` (the Connector's delete activity, the bot's own messages) and `discord.chat` (Delete Message; another author's with `MANAGE_MESSAGES`); `meta.whatsapp` has no delete call. |
| `Messages.React` | implemented | conformance | conformance | Queued, then executed by `Runtime.Run`; once the provider applied it the caller's reaction is added to or removed from the stored set. Adapters declaring `react`: the native web widget, `github.issues` (eight GitHub reactions, several per user), `slack.chat` (26 emoji by Slack name, several per user), `meta.whatsapp` (one reaction per user and message; a removal sends an empty emoji), `telegram.messages` (73 listed emoji, one reaction per reactor; a removal clears it), `microsoft.teams` (the six Teams reaction types) and `discord.chat` (any Unicode emoji Discord knows, several per user). GitHub sends no reaction webhooks: its reactions arrive only as counts in comment payloads. |
| `Messages.Receipt` | implemented | conformance | conformance | A delivery or read receipt for a message received from the provider, queued under `receipt` and executed by `Runtime.Run`; the operation is the record that it was sent, nothing else changes. A retry replays without sending again, and a lost answer is never resent blindly. Refused `unsupported` at the call when the adapter does not declare `receipt`. Adapters declaring `receipt`: the native web widget (both kinds, as a `receipt` event to the visitor of the message's conversation). Evidence: the service suite's `Receipts` cases over a scripted adapter and the adapter suite's `Receipts` group. |

## Work items

| Method | Status | memory | postgres | Notes |
| --- | --- | --- | --- | --- |
| `WorkItems.Get` | implemented | conformance | conformance | Read back through the service suite's `WorkItemActs` group and the durable suite's crash cases. |
| `WorkItems.List` | implemented | engine tests | unverified | |
| `WorkItems.Act` | implemented | conformance | conformance | Local fields commit locally. A provider-owned status is queued and sent to the provider along a transition the channel adapter declares (`work_item_act`); an undeclared transition or reason is unsupported. The work item changes from the provider's answer only; the conversation status never changes, by command or by the provider's webhook. Other provider-owned fields are ingest-only (unsupported). Mixing a provider-owned status with a local field, or a status reason with a local status, is a validation error. See [Work item status changes](../README.md#work-item-status-changes). Evidence: the service suite (declared and undeclared transitions, lifecycle independence), the adapter suite's `WorkItemActs` group (`github.issues` and `gitlab.issues`, a fake tracker) and the durable suite's crash cases. |

## Tasks

| Method | Status | memory | postgres | Notes |
| --- | --- | --- | --- | --- |
| `Tasks.Get` | implemented | conformance | conformance | |
| `Tasks.List` | implemented | conformance | conformance | Most recent status first; filters by inbox, conversation and task state. |
| `Tasks.Act` | implemented | conformance | conformance | `uma.task.open` (per-agent-context conversations only) and `uma.task.update`. Status content becomes one uma-authored message with origin `task_status`, committed with the operation and never sent. |

## Operations

| Method | Status | memory | postgres | Notes |
| --- | --- | --- | --- | --- |
| `Operations.Get` | implemented | conformance | conformance | `Operation.UpdatedAt` shows the time of the latest write, set by the store from the ledger's write time on `Get`, `Lookup`, `List` and `Reconcile`; see [Listing operations](../README.md#listing-operations). |
| `Operations.Replay` | implemented | engine tests | unverified | |
| `Operations.Approve` | implemented | conformance | conformance | Decides a held operation: held to queued under the proposal's original key; a second approve returns it replayed. Authorized for `operation.read` and the target's read right, then `operation.approve`; revision compare-and-swap, stale-target refusal unless forced. See the [decision record](decisions/held-operations.md). An optional `Batch` makes approvals that share it count as one clean approval of their trust key; distinct or empty batches count separately, an invalid one is refused, a replay counts nothing. Evidence: the service suite's `Held` group (lifecycle, key replay, authorization, visibility, stale reasons) and the durable suite's held decision cases, also across a restart; batches in `Held/ApprovalBatch`. |
| `Operations.Reject` | implemented | conformance | conformance | Held to rejected: nothing applied, note recorded, counts as a correction. Authorized as `Approve`, with `operation.reject`. Evidence: as `Approve`. |
| `Operations.Edit` | implemented | conformance | conformance | Applies edited arguments under the same key, recording before and after; counts as a correction. Authorized as `Approve`, with `operation.edit`. Evidence: as `Approve`, with the edit lineage and replay cases. |
| `Operations.Trust` | implemented | conformance | conformance | Reads the counters of one trust key on the channel the request names: consecutive clean approvals and totals. Authorized for `trust.read` on the channel; an absent key is `model.NotFound`. See the [decision record](decisions/held-operations.md). Evidence: the service suite's `Held` events and trust case; the durable suite's batch-once, reset and concurrent-approval cases. |
| `Operations.Trusts` | implemented | conformance | conformance | Lists the channel's trust entries by label, ordered by key, paged by key cursor. Authorized for `trust.read` on the channel. See the [decision record](decisions/held-operations.md). Evidence: as `Trust`. |
| `Operations.ResetTrust` | implemented | conformance | conformance | Zeroes the clean count of one key and keeps totals; a key never counted returns an empty entry. Authorized for `trust.reset` on the channel. See the [decision record](decisions/held-operations.md). Evidence: as `Trust`. |
| `Operations.ImportTrust` | implemented | conformance | conformance | Seeds one key's counters from an earlier approval history, idempotently: totals take the larger of stored and imported, `LastCorrectionAt` the later; the consecutive count is raised only while no stored correction is later than the imported one. Authorized for `trust.write` on the channel. See the [decision record](decisions/held-operations.md). Evidence: the service suite's `Held/ImportTrust` case (import into an empty key, idempotent re-import, never lowering live counts, the stale consecutive-count rule, `trust.write` denial). |
| `Operations.ResetTrustSet` | implemented | conformance | conformance | Resets several keys, all or none in one unit; returns the entries that exist. Authorized for `trust.reset` on the channel. See the [decision record](decisions/held-operations.md). Evidence: as `Trust`; the durable suite's all-or-none reset. |
| `Operations.Reconcile` | implemented | conformance | conformance | Asks the adapter now about an `outcome_unknown` operation's unresolved attempt: applied settles it, "not applied" makes it `retry_pending` (nothing is sent), anything inconclusive leaves it unchanged; idempotent. Authorized for `operation.read`, then `operation.reconcile`. Evidence: the durable suite's Reconcile cases (fault provider, also across a store restart) and the fake provider's host tests. See [Reconciling an operation](../README.md#reconciling-an-operation). |
| `Operations.Lookup` | implemented | conformance | conformance | Recovery by operation key after a lost response; contract in its godoc. |
| `Operations.List` | implemented | conformance | conformance | Operations the principal may read, most recently written first, by inbox, conversation, state, write-time window, kind and sending actor (`Kinds` and `Actor` matched in the engine on the operation key); each carries `Operation.Key` (caller, kind, target, operation ID); each shown as `Get` shows it (`UpdatedAt` is the write time the order and window use), those it may not read left out. A new `Reader.Operations` read on every store, with scope columns and indexes on PostgreSQL (migration 19). Evidence: the store suite's `OperationList`, filter and cursor groups, the service suite's `OperationList` cases, and PostgreSQL's backfill, planner and key-race tests. See [Listing operations](../README.md#listing-operations). |

## Events

| Method | Status | memory | postgres | Notes |
| --- | --- | --- | --- | --- |
| `Events.Open` | implemented | conformance | conformance | |
| `Events.Poll` | implemented | conformance | conformance | |
| `Events.Ack` | implemented | conformance | conformance | |
| `Events.Close` | implemented | engine tests | unverified | |
| `Events.List` | implemented | conformance | conformance | Read-only event log page without a subscription; hands off to `Events.Open` at its `Next`. Filters by inbox, conversation, kind and `RecordedAt` since. See [Reading the event log](../README.md#reading-the-event-log). |
| `Events.Get` | implemented | conformance | conformance | Reads one event by ID without a subscription; authorized as `Events.List`. An unreadable, missing, purged or foreign event is the same `not_found`. See [Reading one event](../README.md#reading-one-event). |

## Attachments

| Method | Status | memory | postgres | Notes |
| --- | --- | --- | --- | --- |
| `Attachments.Prepare` | implemented | conformance | conformance | |
| `Attachments.Upload` | implemented | conformance | conformance | |
| `Attachments.Open` | implemented | conformance | conformance | |

## Extensions

| Method | Status | memory | postgres | Notes |
| --- | --- | --- | --- | --- |
| `Extensions.Query` | implemented | engine tests | unverified | Adapters shipping reads: `google.mail`, `microsoft.mail` and `imap.mail` (the provider mailbox list, `<type>.mailboxes`, gated by the capability of that name). Evidence: the adapters' tests and `Extensions` engine tests |
| `Extensions.Execute` | implemented | engine tests | unverified | Adapters shipping effects: the mail adapters' provider mailbox create (`<type>.mailbox_create`, targets the channel, idempotent by name, holdable like any write), `zendesk.tickets` (`internal_note`, `update`), `github.issues` (`update`: assignees, labels, milestone) and `slack.chat` (`views.open`, `views.push` and `views.update`: modals, opened with an interaction's `trigger_id` within its three seconds), each run through the adapter suite's `Extensions` group. A lost answer is reconciled with the attempt's action (`adapter.ReconcileRequest.Action`). |

## Interactions

| Method | Status | memory | postgres | Notes |
| --- | --- | --- | --- | --- |
| `Interactions.Get` | implemented | conformance | conformance | Never returns the response token. Evidence: the service suite's `Interactions` group. |
| `Interactions.List` | implemented | conformance | conformance | Filters by inbox, conversation and actor identity. Evidence: the service suite's `Interactions` group. |
| `Interactions.Respond` | implemented | conformance | conformance | Modes `message`, `ephemeral`, `update`; one atomic initial response, follow-ups when the adapter declares `respond_follow_up`. Adapters declaring `respond`: the native web widget, `slack.chat` (a post to the interaction's `response_url`) `telegram.messages` (a message, the callback query's own answer, or an edit of the button's message) and `discord.chat` (the interaction's webhook, after the deferral that acknowledged it). Evidence: the service suite's `Interactions` group (single winner under contention, follow-ups racing the initial response, expiry at submit and attempt, replay) and the adapter suite's `Interactions`, `InteractionIngress` and `Unsupported` groups. |
| `Interactions.Ephemeral` | implemented | conformance | conformance | Naming an `Interaction` is a follow-up response to its actor. Evidence: the service suite's `Interactions` group (follow-up counting, token at the attempt, `respond_follow_up` required) and the adapter suite's ephemeral cases. |
| `Interactions.Signal` | implemented | engine tests | unverified | |

## Streams

| Method | Status | memory | postgres | Notes |
| --- | --- | --- | --- | --- |
| `Streams.Get` | implemented | engine tests | unverified | |
| `Streams.Start` | implemented | engine tests | unverified | |
| `Streams.Update` | implemented | engine tests | unverified | |
| `Streams.End` | implemented | engine tests | unverified | |

## Privacy

| Method | Status | memory | postgres | Notes |
| --- | --- | --- | --- | --- |
| `Privacy.Redact` | partial | conformance | conformance | `PropagateToProvider` is unsupported. Messages, interactions and attachments are redacted; cascades over contacts, identities and conversations cover their interactions too and are implemented, with per-inbox thresholds from `Config.CascadePolicy`. |
| `Privacy.Export` | implemented | conformance | conformance | Holds interaction records without their response tokens. |
| `Privacy.Open` | implemented | conformance | conformance | |

## Runtime

| Method | Status | memory | postgres | Notes |
| --- | --- | --- | --- | --- |
| `Runtime.Webhook` | implemented | conformance | conformance | |
| `Runtime.Receive` | implemented | conformance | conformance | `ReceiveStartSpread` delays the first connection by a stable per-channel offset; lease renewals are spread per channel. Adapters declaring `receive` include `telegram.messages` (long polling with an update-ID cursor) `slack.chat` (Socket Mode, experimental: a WebSocket opened with an app-level token, acknowledged per envelope, refreshed inside the call) and `discord.chat` (the gateway: heartbeats, resume inside the call and from the receiver cursor). |
| `Runtime.ReceiverStatus` | implemented | engine tests | unverified | The lease's owner and expiry from the store; from the process running the `Receive`, also `Phase`, `Since`, `Failures`, `Reason`, `RetryAt` and the adapter-reported `Link` (`adapter.ReceiverReporter`; `degraded` with a `Detail` code from `adapter.ReceiverDegradationReporter`, as `google.mail` and `microsoft.mail` report a failing push registration), read from memory and empty for another process's call. |
| `Runtime.ReceiverState` | implemented | engine tests | unverified | The `Receive` this process runs, from memory alone: no store read, so a host can poll it per channel. `false` when this process runs none for the channel; authorized by the call's tenant like `ReceiverStatus`; `Until` is zero. |
| `Runtime.Run` | implemented | conformance | conformance | |
| `Runtime.Step` | implemented | engine tests | unverified | One bounded claim round in the caller's goroutine: tests and recovery drive work without a background `Runtime.Run`. |
| `Runtime.Drain` | implemented | engine tests | unverified | |
| `Runtime.Purge` | implemented | conformance | conformance | Includes orphaned contacts past `RetentionPolicy.Contacts`. |
| `Runtime.Import` | implemented | conformance | conformance | Bounded history import through `adapter.HistoryReader` (capability `history`): a channel's or one conversation's past messages in the window `Since <= t < Until` (`Until` optional, zero is open, passed to the adapter and not re-checked; at or before `Since` is `Validation`), accepted through the channel's sink as the service principal (`channel.receive`), after the requester's `channel.import`. Idempotent by provider message ID for adapters whose IDs the provider assigns: a message the inbox holds is skipped, so a second run imports nothing; for sender-chosen IDs (mail) no message is skipped for its ID, one is skipped only when it has neither message ID nor event ID, and the sink's event claim dedups. A reader error is returned as is (`RetryAfter` included) with `Next` set to the failed page's cursor; no retry. Reports imported, skipped and gap counts and a resume cursor. Each imported message carries `Message.ImportedBy` (the requester) and neither wakes a snooze nor makes a conversation unread; it still emits `message.received`. `github.issues`, `gitlab.issues` (one issue; no channel-wide feed), `twilio.sms`, `slack.chat`, `discord.chat`, `google.mail`, `microsoft.mail` and `imap.mail` declare `history` (adapter suites and an engine import test over a scripted provider service each; Slack lists newest first, so its reader widens a time range from the cursor; the mail readers are described in the next table); the service suite's `HistoryImport` cases over a scripted adapter and the fake provider's host test cover the engine side. |

## Capabilities

Behaviour that spans several methods, or that no method offers yet.

| Capability | Status | Notes |
| --- | --- | --- |
| Unified contacts and identity links | implemented | See [Contacts](#contacts). |
| Inbound edits, deletes and reactions | implemented | Applied from provider events to stored messages: an edit records its editor (`LastEdit`) and a newer one wins over an in-flight outbound edit; a delete tombstones (`deleted`); reactions are stored per identity, or as counts when the provider names none, under the adapter's reaction cardinality (on `meta.whatsapp` a user's second emoji replaces the first, and a removal naming no emoji removes the one). A marked echo of an outbound edit, delete or reaction is evidence for its operation, never a second change. |
| Outbound edits, deletes and reactions | implemented | Through the operation ledger, with the pending change on `Operations.Get` until the operation settles; executed by `Runtime.Run` on adapters declaring `edit`, `delete` or `react`, and applied to the stored message only once the provider applied them. See [Messages](#messages). |
| Delivery status from providers | implemented | A status envelope with a delivery report (`Envelope.Delivery`) is evidence for the attempt its echo reference names; the operation's worker weighs the evidence by rank (provider_accepted < failed < delivered < delivered and read) and moves a reply or a conversation start up, never down: `provider_accepted` to `delivered` or `failed`, `failed` to `delivered`, a read recorded as `Operation.ReadConfirmedAt`. One `delivery.updated` event per real change, state in the store so it holds across restarts and replicas. `IngressReceipt.Operation` names the operation a report reached, nil when it reached none. Adapters reporting statuses: `twilio.sms`, `twilio.whatsapp`, `meta.whatsapp`, `relay.messages` (the only one reporting a read time). See [Delivery status from providers](../README.md#delivery-status-from-providers). Evidence: the durable suite's `Status*` cases on both stores (memory and PostgreSQL, restart included), engine tests of the ranking and the in-flight race, and the `relay.messages` and `meta.whatsapp` engine tests. |
| Proactive outbound conversations | implemented | `Conversations.Start`; walkthrough: [Outbound-first conversations](../README.md#outbound-first-conversations). Adapters declaring `start_conversation`: `smtp.mail`, `imap.mail`, `google.mail`, `microsoft.mail`, `twilio.sms`, `twilio.whatsapp`, `meta.whatsapp`, `relay.messages`, `native.webwidget`, `slack.chat`, `telegram.messages` (to a user only once the user started the bot), `microsoft.teams` (to a user only where the app is installed for the user), `discord.chat` (a user's direct message through Create DM); not the ticket adapters or `a2a.messages`. |
| Mail history import | implemented | `google.mail`, `microsoft.mail` and `imap.mail` implement `HistoryReader` over received mail only (sent mail and drafts are never read), oldest first, in the window `Since <= t < Until` judged on the provider's received time (`internalDate`, `receivedDateTime`, `INTERNALDATE`), never on the `Date` header; `ImportRequest.Until` reaches each reader as the exclusive upper bound. A message imported and then received, or the reverse, stores once: the envelope is the receiver's, and for mail, whose IDs the sender chooses, the sink's event claim dedups. A conversation import reads the sender's mail. Cursors are opaque and forgeable by the caller, so each reader checks the scope again on every message it fetches; a cursor issued for another read is `Validation`. A message that cannot be read is a gap in the report. Attachment bytes are staged through the optional `HistoryRequest.Stager` that `Runtime.Import` sets (the channel sink's `AttachmentSink`), under the receiver's keys, bounds and refusals, so an imported and then received message holds its bytes once; a reader without bytes to stage ignores it and describes only. Evidence: the shared `History` adapter cases and the `MailHistoryProvider` cases over each adapter's fake, including `HistoryMailStagesAttachmentBytes`, and the service suite's `ReaderIsOfferedTheChannelsStager`; no real provider is recorded yet. See the [decision record](decisions/mail-history.md). |
| Discovery of what the engine executes | implemented | `Configuration.Capabilities` reports each declared capability with the instance's status, masked by engine support; see [Configuration](#configuration). |
| Structured interaction ingress | implemented | An `interaction` envelope (command, action or submission) is recorded as an interaction, never a message, with the provider's response token held beside the record and out of the raw ingress copy; it emits `interaction.received`. The actor is claimed like a message sender, and the conversation is the source message's when the actor belongs to it, else selected as for a message. Reading and answering the records: [Interactions](#interactions). |
| Interaction records and responses | implemented | `Interactions.Get`, `List` and `Respond` (see [Interactions](#interactions)): a response is a queued operation executed by `Runtime.Run` on adapters declaring `respond`, and `respond_follow_up` for follow-ups. The initial response is claimed once; the response token is read at the attempt and never stored in the operation or its events. |
| Per-agent-context selection | implemented | One conversation per context reference the adapter supplies, whatever the sender; the sender who creates it owns it. A request without a context reference, or a continuation of another context, is rejected. The ingress receipt carries the conversation, context, message and task references, so a retried request recovers the first. |
| Agent tasks | implemented | Hosts open and update tasks through [Tasks](#tasks), and an ingress task binding opens a task for the message that carries it or continues an open one. The engine sink serves `adapter.TaskSink` to an agent protocol adapter: reads scoped to the principal that owns the context, and a cancel request the host answers. The `a2a.messages` adapter (`adapter/agent/a2a`, [reference](adapters/a2a.messages.md)) serves the A2A JSON-RPC methods `SendMessage`, `GetTask`, `ListTasks` and `CancelTask` over that sink; streaming, subscribe, push notifications and the extended card are refused (the card declares them off). Evidence: the service suite's `AgentTasks` group on both stores, the adapter's own tests (the adapter suite included, on the memory store) and the runnable host in `examples/a2a`. |
| Bot and agent senders from ingress | implemented | Adapters declare `Descriptor.SenderKinds` and report `Envelope.SenderKind`; ingress records it on the identity and contact it first claims, and `message.received` carries `author_kind`. `github.issues` maps Bot accounts; `gitlab.issues` maps the bot users of access tokens; `jira.issues` maps app accounts (`accountType` `app`); `linear.issues` maps application authors (a `botActor`, an OAuth client or an integration actor). |
| Reply parent on inbound messages | implemented | An adapter whose provider reports the message a new message replies to sets `Envelope.InReplyTo` to that message's provider ID; ingress resolves it to `Message.InReplyTo` (and `message.received` `in_reply_to`) when the inbox holds the message in the conversation the new message joins, found by provider message ID or, for mail, `Message-ID`. An unknown, other-conversation or ambiguous parent leaves it unset; the provider payload keeps the raw value. A thread root is a parent only where the adapter says so; per-adapter coverage: [Adapters](adapters/README.md#reply-parent). `Replies` returns inbound replies too; `adapter.Responses.Replies` stays uma-authored. |
| Custom event kinds from adapters | implemented | `extension.event` envelopes of registered, declared event kinds, about the inbox or a message under provider-assigned IDs; delivered to subscribers that also hold the definition's `AuthorizeAs`. An `Inboxes` filter selects both the events about the inbox and the ones about its messages. A notice about a redacted or provider-deleted message is accepted and appends nothing. |
| Provider-side redaction | unsupported | `Privacy.Redact` with `PropagateToProvider`. |
| Provider-owned work item writes | implemented | `WorkItems.Act` routes a provider-owned status to adapters declaring `work_item_act` (declared transitions and status reasons) through the operation ledger; the work item changes once the provider answered. Other provider-owned fields stay ingest-only. |
| Message search | implemented | `Messages.List` with `Text`, `Since`, `Before`, `Attachments` and `Labels`: whole-word text matching, half-open time range, attachment predicates, per-message admission, sealed paging. Limitation: a search over many messages the caller cannot read can reveal that they exist beyond the 500-match scan bound. See [Searching messages](../README.md#searching-messages). |
| Conversation labels | implemented | See [Labels](#labels); `Filter.Labels` is honoured by `Conversations.List` and `Messages.List` and rejected by every other query. |
| Conversation workflow state | implemented | Assignee, snooze and links (`Conversations.Assign`, `Unassign`, `Snooze`, `Unsnooze`, `Link`, `Unlink`), desk-only behind `conversation.read_workflow`, and per-actor read markers (`MarkRead`, `MarkUnread`, `ReadMarker`); `Filter.Assignee`, `Snoozed`, `Linked` and `Unread` are honoured by `Conversations.List` in storage and rejected by every other query. Evidence: the store suite's `ConversationWorkflow` and `ReadMarkers` groups and `Filters` rows, the service suite's `ConversationWorkflow` group, and the durable `SnoozeAcrossRestart` case across a PostgreSQL restart. See [Conversation workflow state](../README.md#conversation-workflow-state). |
