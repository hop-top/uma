# Adapters

An adapter connects one provider to uma: it reports what the provider sent as
envelopes and carries out the operations uma queues. This page compares the
adapters in this repository; each has its own page with its key, events,
operations, permissions, limits, configuration, tests and known limits.
[Mapping](../mapping.md) is the cross-adapter view: what a provider object
becomes as a conversation, message, work item and reply parent, and what the
inbox modes group by.

Every adapter is `experimental`. The adapter contract (package `adapter`) is
frozen, the adapters are not: configuration schemas, extension names and
behavior may change before an adapter is declared supported. See
[Compatibility](../compatibility.md#what-is-not-frozen).

## Capability matrix

A `yes` means the adapter's descriptor declares the capability. Whether an
opened channel can use it depends on its configuration and credentials; each
page says when.

| Adapter | `receive` | `receive_http` | `send` | `start_conversation` | `reply_recipients` | `edit` | `delete` | `react` | `receipt` | `membership` | `history` | `raw_read` | `archive` | `move` | `read_state` | `flag` | `trash` | `reconcile` | `refetch` | `work_item_act` | `internal_note` | `extension_execute` | `extension_query` | `stream` | `send_ephemeral` | `signal` | `respond` | `respond_follow_up` |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| [`smtp.mail`](smtp.mail.md) | yes |  | yes | yes | yes |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |
| [`imap.mail`](imap.mail.md) | yes |  | yes | yes | yes |  |  |  |  |  | yes | yes | yes | yes | yes | yes | yes | yes |  |  |  | yes | yes |  |  |  |  |  |
| [`google.mail`](google.mail.md) | yes | yes | yes | yes | yes |  |  |  |  |  | yes | yes | yes | yes | yes | yes | yes | yes |  |  |  | yes | yes |  |  |  |  |  |
| [`microsoft.mail`](microsoft.mail.md) | yes | yes | yes | yes | yes |  |  |  |  |  | yes | yes | yes | yes | yes | yes | yes | yes |  |  |  | yes | yes |  |  |  |  |  |
| [`http.mail`](http.mail.md) |  | yes |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |
| [`twilio.sms`](twilio.sms.md) |  | yes | yes | yes |  |  |  |  |  |  | yes |  |  |  |  |  |  | yes |  |  |  |  |  |  |  |  |  |  |
| [`twilio.whatsapp`](twilio.whatsapp.md) |  | yes | yes | yes |  |  |  |  |  |  |  |  |  |  |  |  |  | yes |  |  |  |  |  |  |  |  |  |  |
| [`meta.whatsapp`](meta.whatsapp.md) |  | yes | yes | yes |  |  |  | yes |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |
| [`github.issues`](github.issues.md) |  | yes | yes |  |  | yes | yes | yes |  |  | yes |  |  |  |  |  |  | yes | yes | yes |  | yes |  |  |  |  |  |  |
| [`gitlab.issues`](gitlab.issues.md) |  | yes | yes |  |  | yes | yes |  |  |  | yes |  |  |  |  |  |  | yes | yes | yes |  |  |  |  |  |  |  |  |
| [`jira.issues`](jira.issues.md) |  | yes | yes |  |  | yes | yes |  |  |  |  |  |  |  |  |  |  | yes | yes |  |  |  |  |  |  |  |  |  |
| [`zendesk.tickets`](zendesk.tickets.md) | yes | yes | yes |  |  |  |  |  |  |  |  |  |  |  |  |  |  | yes |  |  | yes | yes |  |  |  |  |  |  |
| [`native.webwidget`](native.webwidget.md) |  | yes | yes | yes |  | yes | yes | yes | yes |  |  |  |  |  |  |  |  |  |  |  |  |  |  | yes | yes | yes | yes | yes |
| [`a2a.messages`](a2a.messages.md) |  | yes | yes |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |
| [`relay.messages`](relay.messages.md) |  | yes | yes | yes |  |  |  |  |  |  |  |  |  |  |  |  |  | yes |  |  |  |  |  |  |  |  |  |  |
| [`slack.chat`](slack.chat.md) | yes | yes | yes | yes |  | yes | yes | yes |  |  | yes |  | yes |  |  |  |  | yes |  |  |  | yes |  |  |  |  | yes | yes |
| [`telegram.messages`](telegram.messages.md) | yes | yes | yes | yes |  | yes | yes | yes |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  | yes |  |
| [`microsoft.teams`](microsoft.teams.md) |  | yes | yes | yes |  | yes | yes | yes |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  |  | yes |  |  |
| [`discord.chat`](discord.chat.md) | yes | yes | yes | yes |  | yes | yes | yes |  | yes | yes |  | yes |  |  |  |  | yes | yes |  |  |  |  |  | yes | yes | yes | yes |
| [`linear.issues`](linear.issues.md) |  | yes | yes |  |  | yes | yes |  |  |  |  |  |  |  |  |  |  | yes | yes |  |  |  |  |  |  |  |  |  |

One builtin capability is declared by no adapter yet: `probe_permissions`.
The matrix lists builtin capabilities only: an adapter that also declares
capabilities of its own, one per extension it ships (the mail adapters'
`<type>.mailboxes` and `<type>.mailbox_create`), lists them on its page.

## Why a capability is not declared

An empty cell is a decision, not an oversight. Each adapter page has a
"Not declared" table naming every builtin capability its descriptor omits, with
one of four reason classes and the sentence behind it. A test fails when a page
omits a capability or names one the adapter declares.

| Reason | Meaning |
| --- | --- |
| `provider` | The provider has nothing to carry it: no such operation, no such notion |
| `not built` | The provider can; nobody has built it in the adapter yet. The sentence names the provider call it would use |
| `engine` | The provider can, but core has no path for it on this adapter's shape (its selection mode, its send effect) |
| `by design` | A deliberate choice, with the reason; it stays unless the reason changes |

`not built` rows are the backlog. `engine` rows need a core change before
any adapter work.

## What each capability gives the host

The capability decides which core operation reaches the provider. An operation
on a channel whose adapter lacks the capability fails with `unsupported`, never
with a different capability's behavior. Each operation is authorized as the
policy action in the last column, on top of `channel.receive`, which the
service principal of a receiver or webhook needs for every accepted ingress.

| Capability | What it does | Operation | Policy action |
| --- | --- | --- | --- |
| `receive` | A receiver loop the host runs reads the provider (polling, IDLE, a listener, a WebSocket) | `Runtime.Receive` | `channel.receive` |
| `receive_http` | An HTTP handler the host mounts takes the provider's webhooks | `Runtime.Webhook` | `channel.receive` |
| `send` | Delivers a reply on a route ingress stored | `Messages.Reply` | `message.reply` |
| `start_conversation` | The same send also reaches a recipient no inbound message named | `Conversations.Start` | `conversation.start` |
| `reply_recipients` | The same send also addresses a reply's Cc and Bcc recipients and, replying to all, the answered message's other recipients | `Messages.Reply` with `Recipients` | `message.reply` |
| `edit` | Changes a sent or received message at the provider | `Messages.Edit` | `message.edit` |
| `delete` | Deletes a message at the provider | `Messages.Delete` | `message.delete` |
| `react` | Adds or removes the account's reaction | `Messages.React` | `message.react` |
| `receipt` | Tells the sender of a received message that it was delivered or read | `Messages.Receipt` | `message.receipt` |
| `membership` | Adds a participant to the provider conversation or removes one | `Conversations.AddParticipant`, `RemoveParticipant` | `conversation.membership` |
| `history` | Reads a window of past messages so they can be imported once | `Runtime.Import` | `channel.import` for the requester, `channel.receive` for the service principal |
| `raw_read` | Reads the provider's whole copy of one message on demand (for mail the RFC 5322 bytes), stored nowhere; an oversize message is `permanent` with reason `too_large` | `Messages.Raw` | `message.read` under the message's audience |
| `archive` | Archives a conversation at the provider, or unarchives it | `Conversations.Archive` | `conversation.state_account` or `conversation.state_shared`, by the scope the adapter reports |
| `move` | Moves a conversation to a folder or changes its labels at the provider | `Conversations.Move` | as `archive` |
| `read_state` | Marks a conversation read or unread at the provider; separate from the per-actor read marker | `Conversations.SetRead` | as `archive` |
| `flag` | Stars a conversation or marks it important at the provider, or clears it | `Conversations.Flag` | as `archive` |
| `trash` | Moves the account's copy to the provider's trash, or restores it; not a delete at the peer | `Conversations.Trash` | as `archive` |
| `reconcile` | Looks up an attempt whose answer was lost, so the engine never resends it blindly | `Operations.Reconcile` and the worker | none (idempotent) |
| `refetch` | Reads a resource again when two changes share one source revision | engine, at ingress | none |
| `work_item_act` | Moves a work item between statuses at the provider | `WorkItems.Act` | `work_item.act` |
| `internal_note` | Delivers an internal note to the provider as a private comment | `Messages.AddInternalNote` | `message.note` |
| `extension_query` | Runs a read the adapter ships: for the mail adapters the account's mailboxes, with the capability `<type>.mailboxes` | `Extensions.Query` | the definition's `AuthorizeAs` (`extension.query` for the mailbox list) |
| `extension_execute` | Runs an effect the adapter ships | `Extensions.Execute` | the definition's `AuthorizeAs` |
| `stream` | Native streamed responses | `Streams.Start`, `Update`, `End` | `stream.write` |
| `send_ephemeral` | A message only one visitor sees | `Interactions.Ephemeral` | `message.ephemeral` |
| `signal` | A transient signal such as typing | `Interactions.Signal` | `conversation.signal` |
| `respond` | Answers an interaction the adapter reported | `Interactions.Respond` | `interaction.respond` |
| `respond_follow_up` | Further responses after the first | `Interactions.Respond` with `FollowUp` | `interaction.respond` |

A stream on a channel without `stream` falls back to `send` and `edit`, or to
`send` alone; see [Interactions](../interactions.md#streams-and-interactions).

## Identity, events and exchange rules

What each descriptor declares besides capabilities. A message ID that the
provider assigns is the message's identity: a second report of it is the same
message, and an inbound ID held by a message uma sent is that message's echo.
An ID the sender chooses (an RFC 5322 `Message-ID`) has neither meaning.

| Adapter | Identity namespaces | Events declared | Sender kinds | Message IDs | Threading | Interactions |
| --- | --- | --- | --- | --- | --- | --- |
| `smtp.mail` | `smtp.mail.address` | none | person | sender | email | none |
| `imap.mail` | `imap.mail.address` | none | person | sender | email | none |
| `google.mail` | `google.mail.address` | `message.received`, `delivery.updated` | person | provider | email | none |
| `microsoft.mail` | `microsoft.mail.address` | `message.received`, `delivery.updated` | person | provider | email | none |
| `http.mail` | `http.mail.address` | none | person | sender | email | none |
| `twilio.sms` | `twilio.sms` | `message.received`, `delivery.updated` | person | provider | none | none |
| `twilio.whatsapp` | `twilio.whatsapp` | `message.received`, `delivery.updated` | person | provider | none | none |
| `meta.whatsapp` | `meta.whatsapp` | `message.received`, `message.updated`, `delivery.updated` | person | provider | none | none |
| `github.issues` | `github.issues` | `message.received`, `message.updated`, `work_item.updated`, `delivery.updated` | person, bot | provider | none | none |
| `gitlab.issues` | `gitlab.issues` | `message.received`, `message.updated`, `work_item.updated`, `delivery.updated` | person, bot | provider | none | none |
| `jira.issues` | `jira.issues` | `message.received`, `message.updated`, `work_item.updated`, `delivery.updated` | person, bot | provider | none | none |
| `zendesk.tickets` | `zendesk.tickets` | `message.received`, `message.updated`, `work_item.updated`, `delivery.updated` | person | provider | none | none |
| `native.webwidget` | `native.webwidget.visitor`, `native.webwidget.user` | `message.received`, `delivery.updated`, `interaction.received` | person | provider | none | reports and answers |
| `a2a.messages` | `a2a.principal` | `message.received`, `message.created`, `task.updated` | agent | provider | none | none |
| `relay.messages` | `relay.sms`, `relay.whatsapp`, `relay.imessage` | `message.received`, `delivery.updated` | person | sender | none | none |
| `slack.chat` | `slack.chat.user` | `message.received`, `message.updated`, `interaction.received` | person, bot | provider | none | reports and answers |
| `telegram.messages` | `telegram.messages.user` | `message.received`, `message.updated`, `interaction.received` | person, bot | provider | none | reports and answers |
| `microsoft.teams` | `microsoft.teams.user` | `message.received`, `message.updated`, `interaction.received` | person, bot | provider | none | reports |
| `discord.chat` | `discord.chat.user` | `message.received`, `message.updated`, `interaction.received` | person, bot | provider | none | reports and answers |
| `linear.issues` | `linear.issues` | `message.received`, `message.updated`, `work_item.updated`, `delivery.updated` | person, bot | provider | none | none |

`smtp.mail` and `imap.mail` declare no events; messages they receive still
reach subscribers as the engine's `message.received` event.

## Reply parent

An adapter whose provider reports the message a new inbound message replies
to sets `Envelope.InReplyTo` to that message's provider ID. Ingress resolves
it to `Message.InReplyTo` (and `in_reply_to` on `message.received`) when the
inbox holds the message in the conversation the new message joins; an unknown,
other-conversation or ambiguous parent leaves it unset, and the provider
payload keeps the raw value. Only an explicit reply to one message counts:
thread membership, forwards and quotes of no message do not.

| Adapter | Reports | Parent |
| --- | --- | --- |
| `smtp.mail`, `imap.mail`, `google.mail`, `microsoft.mail`, `http.mail` | yes | first valid `In-Reply-To` msg-id |
| `slack.chat` | thread replies | the thread root |
| `discord.chat` | replies | the referenced message |
| `microsoft.teams` | yes | `replyToId` |
| `telegram.messages` | yes | `reply_to_message`, except a topic's root |
| `meta.whatsapp` | yes | `context.id` |
| `twilio.whatsapp` | yes | `OriginalRepliedMessageSid` |
| `twilio.sms`, `github.issues`, `gitlab.issues`, `zendesk.tickets`, `native.webwidget`, `a2a.messages`, `relay.messages` | no | none |
| `jira.issues` | no | none |
| `linear.issues` | no | none |

## Provider permissions and credentials

`Required permissions` is the descriptor's list of what the credential the host
resolves must be allowed to do at the provider. uma never obtains or refreshes
a credential: the host's `ports.Credentials` does, per call and per purpose.

| Adapter | Required permissions | Credential purposes |
| --- | --- | --- |
| `smtp.mail` | `smtp.relay` | `smtp.relay` |
| `imap.mail` | `imap`, `smtp.submission` | `imap`, `smtp.submission` (one credential reference) |
| `google.mail` | OAuth scopes `https://www.googleapis.com/auth/gmail.readonly`, `https://www.googleapis.com/auth/gmail.send` and `https://www.googleapis.com/auth/gmail.modify` (the last for the labels effect and the state verbs; it alone covers all three) | `gmail` |
| `microsoft.mail` | `Mail.ReadWrite`, `Mail.Send` (`Mail.Send` alone in direct send mode) | `graph`, `graph.notifications` |
| `http.mail` | none declared | `http.mail.webhook` (the relay's shared secret, HMAC key or Ed25519 public key) |
| `twilio.sms` | none declared | `twilio.rest`, `twilio.webhook` |
| `twilio.whatsapp` | none declared | `twilio.rest`, `twilio.webhook` |
| `meta.whatsapp` | `whatsapp_business_messaging` | `meta.graph`, `meta.app_secret`, `meta.webhook.verify_token` |
| `github.issues` | `issues:read`, `issues:write`, `metadata:read` | `github.issues.api`, `github.issues.webhook` |
| `gitlab.issues` | `api` | `gitlab.issues.api`, `gitlab.issues.webhook` (the webhook's secret token) |
| `jira.issues` | `read:jira-work`, `write:jira-work` (OAuth 2.0 scopes; or the Browse projects, Add comments, edit and delete comments project permissions of an API-token account) | `jira.issues.api`, `jira.issues.webhook` |
| `zendesk.tickets` | `agent`, `admin:webhooks`, `admin:incremental_export` | `zendesk.tickets.api`, `zendesk.tickets.webhook` |
| `native.webwidget` | none | `native.webwidget.session` (the session signing key) |
| `a2a.messages` | none | none: the host authenticates requests itself |
| `relay.messages` | none | `relay.webhook`, `relay.callback` |
| `slack.chat` | `app_mentions:read`, `channels:history`, `channels:manage`, `channels:read`, `chat:write`, `groups:history`, `groups:read`, `groups:write`, `im:history`, `im:write`, `mpim:history`, `reactions:read`, `reactions:write` | `slack.chat.api`, `slack.chat.signing` |
| `telegram.messages` | none declared | `telegram.messages.api` (the bot token), `telegram.messages.webhook` (the webhook secret token) |
| `microsoft.teams` | none declared | `microsoft.teams.connector` (the Bot Connector access token, an OAuth2 token the host mints with the bot's client credentials and refreshes) |
| `discord.chat` | `VIEW_CHANNEL`, `SEND_MESSAGES`, `SEND_MESSAGES_IN_THREADS`, `READ_MESSAGE_HISTORY`, `ADD_REACTIONS`, `MANAGE_THREADS`; the privileged `MESSAGE_CONTENT` intent only with `mentions_only` off | `discord.chat.api` (the bot token), `discord.chat.interactions` (the application's public key, hex) |
| `linear.issues` | `read`, `comments:create`, `write` | `linear.issues.api`, `linear.issues.webhook` |

## How the adapters are tested

Every adapter passes the shared adapter suite (`conformance.RunAdapterSuite`)
over a scripted provider, plus its own tests for webhooks, outcome mapping and
limits. Scripted providers are written to the provider's documentation. They
are not evidence about the real service.

Recorded cassettes (committed exchanges with a real test account, replayed by
`go test`) exist for some adapters only; the table says which.

| Adapter | Suite over a scripted provider | Recorded cassettes | Recording guide |
| --- | --- | --- | --- |
| `smtp.mail` | yes, in-process SMTP | no record path; opt-in live run against Postfix and Mailpit | [IMAP and SMTP](../recording/imap-smtp.md) |
| `imap.mail` | yes, in-process IMAP and SMTP servers | no record path; opt-in live run against Dovecot and Mailpit | [IMAP and SMTP](../recording/imap-smtp.md) |
| `google.mail` | yes | recorded (the scenario and the history read; the rest of the suite stays on the scripted service) | [Gmail](../recording/gmail.md) |
| `microsoft.mail` | yes | recorded: send, reply, Sent Items lookup, categories, state verbs, delta rounds, inbound mail with attachments, history, refusals; the change-notification webhook, subscriptions, throttling, server errors and delta expiry not recordable | [Microsoft Graph](../recording/microsoft-graph.md) |
| `http.mail` | yes, with a scripted relay | no record path | none |
| `twilio.sms` | yes | recorded | [Twilio](../recording/twilio.md) |
| `twilio.whatsapp` | yes | recorded: send and fetch | [Twilio](../recording/twilio.md#record-twilio-whatsapp-cassettes) |
| `meta.whatsapp` | yes | not recorded yet | [WhatsApp Cloud API](../recording/meta-whatsapp.md) |
| `github.issues` | yes | recorded, except the send-and-find exchange and the update effect | [GitHub](../recording/github.md) |
| `gitlab.issues` | yes, over a stand-in GitLab REST API and stand-in webhook payloads | recorded over REST: notes, edits, deletions, close and reopen, history paging; webhook payloads not captured | [GitLab](../recording/gitlab.md) |
| `jira.issues` | yes | recorded: comment send, edit, delete, refusals, issue refetch; webhooks not captured | [Jira](../recording/jira.md) |
| `zendesk.tickets` | yes | not recorded yet | [Zendesk](../recording/zendesk.md) |
| `native.webwidget` | yes, plus end-to-end tests | not applicable: no remote provider | none |
| `a2a.messages` | yes, plus protocol golden files | not applicable: no remote provider | none |
| `relay.messages` | yes, over a scripted relay | not applicable: the relay is the operator's own service, no fixed provider; the iMessage path has a recorded end-to-end test through the reference gateway, recorded with Blooio; BlueBubbles not recorded yet | [iMessage](../recording/imessage.md) |
| `slack.chat` | yes, as request URL and as Socket Mode, per room, per thread and per contact | not recorded yet | [Slack](../recording/slack.md) |
| `telegram.messages` | yes, as webhook and as long polling, per room, per thread and per contact | not recorded yet | [Telegram](../recording/telegram.md) |
| `microsoft.teams` | yes, over a stand-in Bot Framework, per room, per thread and per contact | recorded in a personal chat: send, edit, reactions, typing, deletion; inbound activities not recordable | [Teams](../recording/teams.md) |
| `discord.chat` | yes, over a stand-in gateway and REST API, as gateway receiver and as Interactions Endpoint URL, per room, per thread and per contact | recorded over REST: send, find, edit, reactions, typing, thread members, deletion, thread archive and unarchive; the gateway not recorded | [Discord](../recording/discord.md) |
| `linear.issues` | yes, over a stand-in GraphQL endpoint and stand-in webhook deliveries | recorded: comment send, edit, delete, refusals, issue refetch; webhooks not recorded | [Linear](../recording/linear.md) |

The recorded tests of an adapter without cassettes, and recorded subtests
without theirs, skip until someone records them. An adapter stays `experimental` until its provider traffic is recorded
and committed.

## Modules

Adapters live under `adapter/<category>/<provider>`. The adapters that need
only the standard library are packages of the root module; every other adapter
group is its own module, so importing uma never pulls a provider SDK.

| Adapter | Package | Module |
| --- | --- | --- |
| `smtp.mail` | `hop.top/uma/adapter/mail/smtp` | `hop.top/uma/adapter/mail` |
| `imap.mail` | `hop.top/uma/adapter/mail/imap` | `hop.top/uma/adapter/mail` |
| `google.mail` | `hop.top/uma/adapter/mail/google` | `hop.top/uma/adapter/mail/google` |
| `microsoft.mail` | `hop.top/uma/adapter/mail/microsoft` | `hop.top/uma/adapter/mail/microsoft` |
| `http.mail` | `hop.top/uma/adapter/mail/httpmail` | `hop.top/uma/adapter/mail` |
| `twilio.sms` | `hop.top/uma/adapter/messaging/twilio/sms` | `hop.top/uma/adapter/messaging/twilio` |
| `twilio.whatsapp` | `hop.top/uma/adapter/messaging/twilio/whatsapp` | `hop.top/uma/adapter/messaging/twilio` |
| `meta.whatsapp` | `hop.top/uma/adapter/messaging/meta/whatsapp` | `hop.top/uma/adapter/messaging/meta` |
| `github.issues` | `hop.top/uma/adapter/ticket/github` | `hop.top/uma/adapter/ticket/github` |
| `gitlab.issues` | `hop.top/uma/adapter/ticket/gitlab` | `hop.top/uma/adapter/ticket/gitlab` |
| `jira.issues` | `hop.top/uma/adapter/ticket/jira` | `hop.top/uma/adapter/ticket/jira` |
| `zendesk.tickets` | `hop.top/uma/adapter/ticket/zendesk` | `hop.top/uma/adapter/ticket/zendesk` |
| `native.webwidget` | `hop.top/uma/adapter/web/widget` | `hop.top/uma` |
| `a2a.messages` | `hop.top/uma/adapter/agent/a2a` | `hop.top/uma` |
| `relay.messages` | `hop.top/uma/adapter/messaging/relay` | `hop.top/uma` |
| `slack.chat` | `hop.top/uma/adapter/chat/slack` | `hop.top/uma/adapter/chat/slack` |
| `telegram.messages` | `hop.top/uma/adapter/chat/telegram` | `hop.top/uma/adapter/chat/telegram` |
| `microsoft.teams` | `hop.top/uma/adapter/chat/teams` | `hop.top/uma/adapter/chat/teams` |
| `discord.chat` | `hop.top/uma/adapter/chat/discord` | `hop.top/uma/adapter/chat/discord` |
| `linear.issues` | `hop.top/uma/adapter/ticket/linear` | `hop.top/uma/adapter/ticket/linear` |

Shared packages are not adapters and have no page: `adapter/mail/auth`
(TLS-before-AUTH and SASL), `adapter/mail/mime`, `adapter/mail/stage` (inbound
mail attachment staging), `adapter/messaging/twilio` (signature check, REST
client and inbound media staging), `adapter/messaging/meta` (webhook
verification and Graph client), `adapter/staging` (in the root module: what
every adapter that stages inbound bytes through the attachment sink shares,
its failure classification, opaque staging keys and the mail bounds; each
adapter keeps its own caps, origin checks and credentials on top),
`adapter/web/widget/storelog` (durable widget log) and
`adapter/web/webhookauth` (bearer, token, HMAC and Ed25519 authentication of
webhooks from senders with no provider signature, with timestamp and replay
checks; see [Webhook authentication](../webhook-auth.md)).

## Writing another adapter

[Implementer contracts](../implementers.md) state what core guarantees and what
an adapter owns. The adapter suite in `conformance` is the acceptance test, and
`examples/fakeprovider` is a complete third-party adapter built from public
packages only.

## Keeping these pages true

A test in the repository (`adapter_docs_test.go`) reads every adapter
descriptor from the source and fails when an adapter has no page here, a page
names an adapter that does not exist, or a page or the matrix above disagrees
with the descriptor on capabilities, events or maturity.
