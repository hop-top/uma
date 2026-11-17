# uma

Universal mailbox: receive, route and reply across channels.

uma is an embeddable Go messaging engine for applications, bots and unified
inboxes across email, messaging apps, community chat, agent protocols, issue
trackers and support desks. Hosts embed one set of identity, conversation,
authorization and delivery rules without losing provider-specific meaning.
Human participation is optional: a bot with no inbox UI and a web support desk
use the same engine.

> **Status: pre-1.0, API frozen.** `uma.New` validates a composition (store,
> policies, ports, adapter and extension registrations) and returns an engine
> with an actor-bound client and lifecycle. Every application service and
> runtime method has behaviour; an input a method does not support returns a
> `model.Unsupported` error. [API coverage](docs/api-coverage.md) lists each
> method's status and the stores it is tested on. The core API, adapter
> contract, store interface, wire and persisted formats, event envelope and
> conformance suites are frozen: a change to them needs a version bump and a
> release note, even while modules are `v0.x`. Adapters stay experimental.
> [Compatibility](docs/compatibility.md) says what is frozen and how it changes.
> What each adapter does, and what is tested against a real provider, is in
> [Adapters](docs/adapters/README.md).

## Packages

| Package | Primary adopter | Responsibility |
| --- | --- | --- |
| `hop.top/uma` | Application, bot or inbox host | Composition, actor-bound client, lifecycle, typed services |
| `hop.top/uma/adapter` | Provider maintainer | Descriptor/factory, scoped ingress, optional capabilities |
| `hop.top/uma/adapter/web/widget` | Inbox host | `native.webwidget` browser chat channel, standard library only: host-verified visitor sessions with explicit anonymous assurance and HMAC-signed tokens, JSON receiver through `Runtime.Webhook`, SSE event stream with cursor replay; send, edit, delete, react, native streams, ephemeral replies and typing signals; button, command and form interactions (`actions`, `submissions`) answered with `respond` and `respond_follow_up`, and outbound controls (buttons, selects, a form); [reference](docs/adapters/native.webwidget.md), [interactions](docs/interactions.md) |
| `hop.top/uma/adapter/agent/a2a` | Agent host | `a2a.messages` A2A 1.0 server endpoint, standard library only: JSON-RPC over the host's HTTP mux through `Runtime.Webhook`, agent card, host-owned request authentication, per-agent contexts and tasks, blocking `SendMessage` with a documented deadline error, `GetTask`, `ListTasks`, `CancelTask`; no streaming or push; [reference](docs/adapters/a2a.messages.md), runnable [example](examples/a2a) |
| `hop.top/uma/adapter/web/widget/storelog` | Inbox host | Durable widget visitor log in the host's uma store: per-visitor gapless sequences, retention with reset, survives restarts |
| `hop.top/uma/adapter/web/widget/widgettest` | Log implementer | The shared `widget.Log` suite that `MemoryLog`, `storelog` and a host's own log pass |
| `hop.top/uma/adapter/web/webhookauth` | Adapter author | Inbound webhook authentication for `receive_http` adapters whose sender has no provider signature, standard library only: bearer, token, HMAC-SHA-256/512 and Ed25519 schemes, configurable header names, encodings and signed layout, timestamp window both ways, replay rejection over a host-injected store (in memory by default, or restart-safe over a durable `store.Store` with `webhookauth/storereplay`), secrets through `ports.Credentials`; [guide](docs/webhook-auth.md) |
| `hop.top/uma/adapter/messaging/relay` | Messaging host | `relay.messages` generic JSON relay for SMS, WhatsApp and iMessage providers with no adapter, standard library only: webhooks authenticated by `webhookauth` under configured field names, replies posted to one host-allowed callback URL with the credential from the host; reconcile through an optional host-allowed lookup URL, without which a lost answer stays unknown, or settles from an opt-in delivery status update the gateway posts; [reference](docs/adapters/relay.messages.md) |
| `hop.top/uma/companions/bot` | Bot host | Dispatcher over one subscription: direct, watched, mention and pattern routes, interaction routes (command, action, form submission) with replay-safe initial responses and follow-ups, kind handlers, a fallback route for unclaimed messages, `TextContent` and `Turn.ReplyText`, replay-safe replies, ACK after handlers, bounded concurrency; public API only |
| `hop.top/uma/companions/bot/host` | Bot host | Subscription preparation with a replay-based continuity check (`Prepare`, `Bootstrap`, `Resume`, `Marker`), component supervisor with bounded shutdown (`Run`, `Component`), validated route composition (`Mux`, `Binding`, `Route`), `New` for a bot, worker and handler from one `Config`; [guide](docs/bot-host.md); public API only |
| `hop.top/uma/conformance` | Storage maintainer, adapter author | Shared store suite every backend must pass ([writing a store backend](docs/store-backends.md)); service suite running the application services, identity links included, over a backend; durable suite (concurrency, publication, replay, crash/restart) behind a durability claim; adapter suite run per declared capability, with policy-bypass cases and mutation cases (edit, delete, react, lost answers, provider echoes, reaction summaries), interaction ingress and response cases |
| `hop.top/uma/model` | All adopters | Domain values, requests/results, references, events, errors |
| `hop.top/uma/policy` | Host/policy integrator | Authorization, identity evidence, conversation selection, contact-cascade thresholds |
| `hop.top/uma/ports` | Infrastructure integrator | Clock, IDs, credentials, HTTP, blobs, telemetry; opt-in `SystemClock` and `UUIDv7` (standard library only) for production |
| `hop.top/uma/ports/portstest` | Test author | Deterministic doubles for the ports: `FixedClock`, `SequenceIDs`, in-memory blobs |
| `hop.top/uma/store` | Storage maintainer | Transaction-scoped persistence, ledger, event log, checkpoints, adapter recipient logs |
| `hop.top/uma/store/memory` | Application or bot host | In-memory, non-durable store |
| `hop.top/uma/scripts/gmail-oauth` | Adapter maintainer | Command that obtains Gmail tokens for recording cassettes ([guide](docs/recording/gmail.md)); not an API |

`internal/` holds the engine implementation behind `hop.top/uma`, one
package per concern (`engine`, `ingress`, `operations`, `services`,
`events`, `extensions`, `runtime`). It is implementation, not API: nothing outside this
module can import it, and its packages change without notice.

The two adapters above need only the standard library, so they are packages of
the root module. [Adapters](docs/adapters/README.md) has the capability matrix
for every adapter and one page per adapter: key, events, operations, permissions,
limits, configuration, tests and known limits.
[Mapping provider concepts](docs/mapping.md) says, per adapter, what becomes a
conversation, message, work item and reply parent, and defines the inbox modes.

Integrations with substantial dependency trees live in separate modules,
grouped by role, so the core stays standard-library only. Adapters sit under
`adapter/<category>/<provider>`, one module per provider group. Every module
below has an exported API and is a release component:

| Module | Role |
| --- | --- |
| `hop.top/uma/adapter/chat/slack` | Slack adapter (`slack.chat`): signed Events API requests, or Socket Mode (experimental) for an app without a public URL, with bot, mention and duplicate rules, reported as messages, edits, deletions and reactions; replies, conversation starts, edits, deletions and reactions through the Web API; reconciliation by message metadata; history; buttons, slash commands and modal submissions reported as interactions and answered through `response_url`, modals opened, pushed and updated as extension effects; [reference](docs/adapters/slack.chat.md) |
| `hop.top/uma/adapter/chat/teams` | Microsoft Teams adapter (`microsoft.teams`): Bot Framework activities at the messaging endpoint, each checked against its Bot Connector token (RS256, issuer, audience, `serviceurl`, endorsement), reported as messages, edits, deletions and reactions; replies, conversation starts (personal chats through the Connector), edits, deletions, reactions and typing through the Bot Connector API with a host-minted token; Adaptive Card `Action.Execute` presses reported as interactions; [reference](docs/adapters/microsoft.teams.md) |
| `hop.top/uma/adapter/chat/telegram` | Telegram adapter (`telegram.messages`): webhook updates verified by their secret token, or long polling with a cursor, reported as messages, edits and reactions; replies, conversation starts, edits, deletions and reactions through the Bot API; inline keyboard buttons and bot commands reported as interactions and answered; text under no parse mode, with escaping for HTML and MarkdownV2; [reference](docs/adapters/telegram.messages.md) |
| `hop.top/uma/adapter/chat/discord` | Discord adapter (`discord.chat`): a gateway receiver (heartbeats, resume, a receiver cursor; mention-first without the privileged `MESSAGE_CONTENT` intent) reporting messages, edits, deletions and reactions, and Ed25519-verified interactions at the Interactions Endpoint URL; replies, direct messages, edits, deletions, reactions, typing and thread membership through the REST API with a module-local rate limiter; reconciliation by nonce; history; slash commands, components and modal submissions reported as interactions, deferred, and answered through the interaction webhook; [reference](docs/adapters/discord.chat.md) |
| `hop.top/uma/adapter/mail` | `smtp.mail` adapter (`smtp`: MX receiver with SPF/DKIM/DMARC sender evidence, relay submission; [reference](docs/adapters/smtp.mail.md)); `imap.mail` adapter (`imap`: IMAP retrieval with IDLE and a UIDVALIDITY-checked UID cursor, SMTP submission with the same credential, sent-copy reconciliation; [reference](docs/adapters/imap.mail.md)); `http.mail` adapter (`httpmail`: mail posted by an inbound-parse service or MTA hook as JSON or `message/rfc822`, webhook-authenticated, sender evidence unverified unless a trusted authserv-id is declared; [reference](docs/adapters/http.mail.md)); shared mail auth (`auth`: TLS-before-AUTH, SASL negotiation) and MIME (`mime`) |
| `hop.top/uma/adapter/mail/google` | Gmail API mail adapter (`google.mail`): `users.watch` with authenticated Pub/Sub push, history gap filling and full resync, raw MIME replies on the thread, Sent search reconciliation; [reference](docs/adapters/google.mail.md) |
| `hop.top/uma/adapter/mail/microsoft` | Microsoft Graph mail adapter (`microsoft.mail`): change notifications with delta-query gap filling, replies through drafts, Sent Items reconciliation; [reference](docs/adapters/microsoft.mail.md) |
| `hop.top/uma/adapter/messaging/twilio` | Twilio messaging: `sms` (`twilio.sms`: send, signed webhooks and status callbacks; [reference](docs/adapters/twilio.sms.md)); `whatsapp` (`twilio.whatsapp`: free-form sends inside the 24-hour window, Content Template sends outside it, signed webhooks and status callbacks; [reference](docs/adapters/twilio.whatsapp.md)); shared signature check and REST client |
| `hop.top/uma/adapter/messaging/meta` | WhatsApp Cloud API adapter (`whatsapp`, type `meta.whatsapp`: send, reactions, signed webhooks; [reference](docs/adapters/meta.whatsapp.md)); shared Meta webhook verification, `X-Hub-Signature-256` validation and Graph API client |
| `hop.top/uma/adapter/ticket/gitlab` | GitLab issues adapter (`gitlab.issues`): issue and note webhooks checked by secret token, opt-in replies as notes with echo correlation and reconciliation, note edit and delete, issue close and reopen, one issue's history; [reference](docs/adapters/gitlab.issues.md) |
| `hop.top/uma/adapter/ticket/github` | GitHub issues adapter (`github.issues`): signed issues and issue comment webhooks, opt-in replies as comments with echo correlation and reconciliation, comment edit, delete and reactions, issue close and reopen with `state_reason`; [reference](docs/adapters/github.issues.md) |
| `hop.top/uma/adapter/ticket/jira` | Jira Cloud issues adapter (`jira.issues`): admin webhooks verified by `X-Hub-Signature`, opt-in replies as ADF comments with an echo property and reconciliation, comment edit and delete; [reference](docs/adapters/jira.issues.md) |
| `hop.top/uma/adapter/ticket/zendesk` | Zendesk Support tickets adapter (`zendesk.tickets`): signed webhooks, polling receiver, public replies, internal notes and ticket updates as extension effects, audit-based reconciliation; [reference](docs/adapters/zendesk.tickets.md) |
| `hop.top/uma/adapter/ticket/linear` | Linear issues adapter (`linear.issues`): signed webhooks with a replay window, opt-in replies as comments with idempotent comment IDs, echo correlation and reconciliation, hand-written GraphQL; [reference](docs/adapters/linear.issues.md) |
| `hop.top/uma/store/postgres` | Durable PostgreSQL storage backend |
| `hop.top/uma/contrib/otel` | OpenTelemetry observer |
| `hop.top/uma/contrib/kit` | hop.top/kit integration: `secret` (`ports.Credentials` over a host-built kit secret store); `blob` (`ports.BlobStore` over a host-built kit blob store) |
| `hop.top/uma/testing/xrrtest` | Record/replay HTTP transport for adapter tests ([README](testing/xrrtest/README.md), [provider recording guides](docs/recording/README.md)) |

`examples/` is a separate module (`hop.top/uma/examples`) that uses only the
exported API: a quickstart bot over the bot host package and the same bot
composed by hand, an A2A host with its executor loop, a durable inbox composition, a mailbox triage loop on provider state, an inbound-only
external adapter, an alternate storage boundary, custom policies and a
companion helper, including an identity verifier and an authorizer that
requires evidence to unlink. These compile, and their examples run as
tests; `examples/bot`, `examples/botexplicit` and `examples/a2a` run.

`examples/fakeprovider` is its own module (`hop.top/uma/examples/fakeprovider`):
a scripted provider adapter built as a third party would build one, from
public packages only, with no init registration. Ingress-only, send-only and
full variants implement exactly the interfaces their capabilities declare,
and the full variant ships one extension effect (`Factory.Extensions`); the
webhook variant also reports interactions, acknowledging one only once it is
durably accepted, and the interactive variant answers them (`respond`,
`respond_follow_up`);
per-channel scripts queue inbound ingress and outbound results (accepted,
rejected, outcome unknown, transient) for deterministic tests. None of the
example modules is released.

### Run the bot examples

Two runnable bot hosts serve the same chat page: the browser chat widget
(`adapter/web/widget`) as the channel, the bot companion with an echo handler
and a help handler that answers both a `/help` command and a Help button as
[interactions](docs/interactions.md), and the in-memory store, served on one
local address. The guide to the host layer is [Hosting a bot](docs/bot-host.md).

- `examples/bot` is the short quickstart over `companions/bot/host`: `Prepare`
  opens the subscription, `New` composes the bot, the delivery worker and the
  routes, and `Run` supervises them and the server with a bounded shutdown. It
  also shows a generated reply recorded before it is sent.
- `examples/botexplicit` is the explicit reference composition: the same bot
  with the underlying calls (`Configuration`, `Runtime.Webhook`,
  `Events.Open`, `Runtime.Run` and `Runtime.Drain`) and its own supervision.

```sh
cd examples && go run ./bot           # then open http://localhost:8080/
cd examples && go run ./botexplicit   # the same bot, wired by hand
```

Both listen on `localhost:8080` by default (`-addr` changes it), so run one at
a time. Visitors are anonymous and the authorizer allows everything; the memory
store is not durable, so a restart loses every conversation. The page sends
`/help` and the Help button as `actions` requests, which uma records as
interactions; the bot answers each with `Turn.Respond`, and the answer reaches
the page as an `interaction.response` event. The tests of both run one shared
suite that drives the path a browser takes: session token, message, reply over
the event stream, then a button press and a command answered as responses.

`examples/inbox` is its own module too (`hop.top/uma/examples/inbox`), also
unreleased: a maintained, runnable durable inbox host, smoke-tested against
PostgreSQL on every CI run.

`examples/triage` is its own module too (`hop.top/uma/examples/triage`), also
unreleased: a mailbox triage loop on `google.mail` and `imap.mail` that
archives, flags and trashes at the provider, tested offline
([walkthrough](#mailbox-triage-example)).

The reference iMessage gateway for `relay.messages` (Blooio and BlueBubbles
Server backends, standard library only) is in the root module:
`go install hop.top/uma/cmd/imessagegateway@<version>`
([README](examples/imessagegateway/README.md)). Its end-to-end tests live in
`examples/imessagegateway`, an unreleased module.

### Run the A2A example

`examples/a2a` is a runnable host for the `a2a.messages` adapter
([reference](docs/adapters/a2a.messages.md)): remote agents call it over
A2A JSON-RPC, authenticate with a static bearer token the example keeps (the
adapter holds no credentials), and an executor loop in the same process does
the work.

```sh
cd examples && go run ./a2a   # prints the curl commands, then serves localhost:8080
```

What it shows: `a2a.New` with the host's `Authenticate`, a channel and a
per-agent-context inbox, the endpoint mounted from `Runtime.Webhook`, and the
executor reading `message.received` and `task.updated`. It answers `echo` as a
message-only exchange (no task is invented), opens a task explicitly for
`report` (working, an artifact, completed with one status message), keeps a
`hold` task working until the remote agent's cancel request, and takes `late`
longer than the blocking wait so the caller gets the documented
`DEADLINE_EXCEEDED` error while the task still completes. The memory store is
not durable, and the authorizer allows everything. Its test drives every
path over `httptest`, on localhost only.

### Durable inbox example

A website chat (`native.webwidget`) and an email address (`smtp.mail`) feed
one inbox model in PostgreSQL; an operator view lists conversations, reads
messages, replies and adds internal notes. It uses only the public packages
of uma, `store/postgres` and the adapter modules, and shows the host's side of
the contract:

- `postgres.Migrate`, then `postgres.Open`; `uma.New` with `RequireDurable`.
- The host's HTTP mux serves the widget receiver (`Runtime.Webhook`), the
  visitor event stream and the operator view; the host binds the SMTP port.
- `ports.SystemClock{}` and `ports.NewUUIDv7(...)` are the opt-in production
  clock and ID generator; the engine never defaults to either.
- `Runtime.Run` workers deliver queued replies; `Runtime.Receive` runs the MX
  receiver, owned by one host at a time across every host on the database.
- Credentials come from a host credential port, never from configuration.
- Shutdown in order: event streams, HTTP server, `Runtime.Drain`,
  `Engine.Close`. A reply accepted before a restart is delivered once after
  it.

Tests and recovery tools that want work run without a background worker call
`Runtime.Step`: one bounded claim round in the caller's goroutine that returns
how many items it ran, never waits, and honours `Runtime.Drain` and
`Engine.Close`.

Run it with the services in `examples/inbox/compose.yaml` (PostgreSQL, and a
development relay that catches email replies at `http://127.0.0.1:8025`):

```sh
cd examples/inbox
docker compose up -d --wait
DATABASE_URL=postgres://uma:uma@127.0.0.1:5432/inbox \
WIDGET_KEY=$(openssl rand -hex 32) OPERATOR_TOKEN=change-me-operator-token \
go run .
```

A single container works as well when email replies are not needed:
`docker run -d -p 127.0.0.1:5432:5432 -e POSTGRES_USER=uma -e POSTGRES_PASSWORD=uma -e POSTGRES_DB=inbox postgres:18`.

Open `http://127.0.0.1:8080` to chat as a visitor, mail
`support@example.com` through the receiver on `127.0.0.1:2525`, and answer
as the operator:

```sh
OP='Authorization: Bearer change-me-operator-token'
curl -s -H "$OP" http://127.0.0.1:8080/operator/conversations
curl -s -H "$OP" http://127.0.0.1:8080/operator/inboxes/$INBOX/conversations/$CONV/messages
curl -s -H "$OP" -H 'Content-Type: application/json' -d '{"text":"On its way."}' \
  http://127.0.0.1:8080/operator/inboxes/$INBOX/conversations/$CONV/replies
curl -s -H "$OP" -H 'Content-Type: application/json' -d '{"text":"Checked the carrier."}' \
  http://127.0.0.1:8080/operator/inboxes/$INBOX/conversations/$CONV/notes
```

The smoke tests (`go test ./...` in `examples/inbox`, Docker required) start
PostgreSQL in a container and cover a widget message through to the operator
reply on the visitor's stream, an email through the receiver to a relayed
reply, a restart with a reply still queued, receiver takeover between two
hosts and revoked credentials.

### Mailbox triage example

`examples/triage` follows a Gmail mailbox (`google.mail`) and an IMAP mailbox
(`imap.mail`) and, on a timer, applies one rule to each conversation's newest
inbound message: junk goes to the trash, mail that asks for attention is
flagged (`important` where the provider has it, else `star`), bulk mail is
archived. It is the host's side of [provider conversation
state](#provider-conversation-state): `Sweep` reads the inbox and queues
`Trash`, `Flag` and `Archive` only for verbs the channel reports available,
with operation IDs derived from the conversation and the verb so a repeated
sweep replays; `Host.Pass` then runs the queued operations with
`Runtime.Step` and reports how each ended. The triage actor holds
`conversation.state_account` and nothing shared.

Its test runs offline against the fake provider's state variants, a labels
mailbox standing in for Gmail and a folders mailbox for IMAP, and needs no
credentials. To point it at real mailboxes, set the variables listed in
`examples/triage/main.go` and run:

```sh
cd examples/triage
GMAIL_ADDRESS=me@example.com GMAIL_ACCESS_TOKEN=... \
IMAP_ADDRESS=me@example.org IMAP_HOST=imap.example.org IMAP_PASSWORD=... \
go run .
```

Use a test mailbox: the loop trashes and archives real mail.

## Contacts and identity links

Ingress gives every new sender address an identity in its inbox, on a
provisional contact of its own. `Client.Contacts()` reads them, lets a desk
create and rename contacts, and moves an identity between contacts; both
backends (`store/memory`, `store/postgres`) implement it.

| Method | What it does | Authorized as |
| --- | --- | --- |
| `List` | Contacts the principal may read | `contact.read_all`, or `identity.read` on one of the contact's identities |
| `Identities` | Identities, by contact or inbox | `identity.read`, per identity |
| `Get` | One contact, with the revision `Update` expects | as `List` |
| `Create` | A contact no identity points at, with a display name and optional email and phone attributes | `contact.write` |
| `Update` | Replaces a contact's display name and attributes at `ExpectedRevision` | `contact.write` on the contact |
| `Link` | Points an identity at an existing contact of its kind, with verified evidence | `identity.link` off the provisional contact, `identity.relink` otherwise; `identity.link_override` too when the assurance drops |
| `Unlink` | Detaches an identity to a fresh contact, with a reason code and optional evidence | `identity.unlink` |
| `PreviewMerge` | What merging one contact into another would move: identities, owned conversations, both revisions | `contact.merge`, and `identity.read` on every identity listed |
| `Merge` | Moves every identity of one contact to another, once, and leaves a redirect | `contact.merge`, and `identity.relink` on every identity (`identity.link_override` too when the assurance drops) |
| `History` | The link records of an identity or contact, in order | `identity.history`, per record |

`Create` and `Update` are desk actions, authorized as `contact.write`, a
different action from `channel.configure` and `inbox.configure`, so a host
grants it to the staff who manage contacts and to nobody else; an external
participant holds no such grant. Each commits one `contact.created` or
`contact.updated` event for the internal audience, naming the fields that
changed but never their values, which are read with `Get`. A create links
no identity: a contact is unified with identities by `Link` alone, and
until then `List` shows it only under `contact.read_all`. `Update` replaces
the display name and the attributes whole and needs the contact's current
revision from `Get`; a stale one is `conflict`. Attributes are normalized,
unverified lookup hints (`model.Attribute`), at most
`model.MaxContactAttributes`, stored sorted. A privacy cascade on a contact
erases its name and attributes too.

An adapter may report the sender's verified attributes on the envelope
(`Envelope.SenderAttributes`: a phone number, a WhatsApp business-scoped user
ID). Ingress records them on the sender's identity when it first claims it
and never changes them after; the provisional contact gets none, and
`Identities` reads them back. They are lookup hints and the input of a host's
`verified_attribute` verifier: two identities that carry the same verified
attribute can be linked to one contact with that evidence, which is how a
WhatsApp user known by number and the same user known only by user ID join. Core
never links on its own.

A link moves one pointer and records it. It never merges conversations,
changes reply routes or message authors, or grants access to another
inbox.

`Merge` is the explicit way to fold contact A into B. It is never
automatic: you call `PreviewMerge`, then pass its revisions and identities
back, so the merge moves exactly what was reviewed, and a revision or an
identity that changed since is `conflict`. In one commit every identity of A
moves to B with a link record of class `merge` (who, when, the reason
code), A gets a redirect (`Contact.MergedInto`) and an `identity.linked`
event per identity and one `contact.merged` event (references only,
internal audience, resource B) are appended. A stays with no identities;
it is not undone by linking back (a link to a merged contact is
`conflict`), but `Unlink` detaches an identity again. Conversations are not
moved: their owner is an identity, and it now belongs to B. A privacy
export or cascade aimed at A follows the redirect to B. A, its redirect and
its link records' contact refs live until a retention purge past
`RetentionPolicy.Contacts` removes A. A merge moves at most
`model.MaxMergeIdentities` identities.

Evidence is checked by the verifier the host registered for its kind
(`Config.IdentityVerifiers`); there is no default, and an unregistered kind
is `unsupported`. The authorizer sees only verified evidence. Runnable
examples: `ExampleContacts_Link`, `ExampleContacts_Unlink`,
`ExampleContacts_Merge`, `ExampleContacts_History` and `ExampleContacts_List`
([example_contacts_test.go](example_contacts_test.go)),
`ExampleContacts_Create` and `ExampleContacts_Update`
([example_contacts_write_test.go](example_contacts_write_test.go)), and a verifier
written from scratch in [examples/policies.go](examples/policies.go).

A privacy cascade on a contact (`Privacy().Redact` with `Cascade`) redacts
only the identities its actor may redact (`privacy.redact`, per identity).
How many denied identities refuse it is a host choice per tenant and inbox
(`Config.CascadePolicy`): `0` refuses every contact cascade, `N` refuses
once `N` identities are denied, `-1` never refuses. A contact spanning
inboxes takes the most restrictive of its inboxes. With no policy every
inbox is `-1`: denied identities are skipped, left untouched, and the
operation reports `Progress.Partial`. Example host policy:
`CascadeThresholds` in [examples/policies.go](examples/policies.go).

## Outbound-first conversations

`Conversations().Start` sends the first message to someone no inbound
message named yet: a reminder by text, an email to a new lead. It is a
queued outbound operation like a reply, authorized as `conversation.start`
on the inbox, and its result names the message and so the conversation.

| Step | What happens |
| --- | --- |
| Check | The inbox's channel adapter declares `start_conversation` (an `adapter.Sender` that reaches any recipient). Every mail, SMS, WhatsApp and widget adapter does; a WhatsApp start to a user outside the 24-hour window must carry a template part |
| Select | In one unit with the operation: the recipient's identity is claimed as ingress would claim a sender's, and the inbox's selection policy picks the conversation with event class `outbound_start`. Built-in rules reuse the recipient's open or pending conversation, never reopen one, and create one otherwise; an email inbox without `lock_to_single` starts a new thread per start |
| Deliver | A worker sends on the conversation's stored route, rechecking `conversation.start` and the capability first. The message answers no message |
| Answer | The recipient's reply lands in the same conversation: by identity, or by the sent Message-ID in its `In-Reply-To` |

The recipient is the route the adapter reports for that counterpart (its
identity namespace, route kind and canonical ID), or the room or thread
reference in a per-room or per-thread inbox. Per-work-item inboxes are
`unsupported`: opening a ticket is a provider operation of its own. A
retry with the same operation ID replays the stored result without
selecting again. Runnable example: `ExampleConversations_Start`
([example_start_test.go](example_start_test.go)).

## Reply recipients

A mail reply can address more than the conversation's stored route. `Reply`
takes optional `Recipients` (`model.ReplyRecipients`):

```go
held, err := plugin.Messages().Reply(ctx, model.Reply{
	Command: cmd, Conversation: conv, InReplyTo: &parent, Content: text,
	Recipients: &model.ReplyRecipients{ReplyAll: true, Cc: []string{"legal@example.com"}},
})
```

| Field | Does |
| --- | --- |
| `ReplyAll` | Adds the answered message's other `To` and `Cc` recipients, minus the channel's own address and repeats. The adapter reads them from the answered message, so a reply answering none adds nothing |
| `Cc`, `Bcc` | Explicit bare addresses added to the reply's Cc and Bcc; a Bcc recipient never appears in the delivered headers |

The route address stays the first recipient. Addresses are validated and
normalized when the reply is accepted (at most 50 in `Cc` and `Bcc`; the
adapter bounds the total) and part of the request's fingerprint, so a retry
with other recipients conflicts. A held reply keeps its recipients: an approval
sends them, and an edit may change the content but not who receives it.

The channel's adapter must declare `reply_recipients`; otherwise a reply naming
recipients is `unsupported` before anything is stored. A host reads what the
channel supports, per channel, from `Configuration().Capabilities`: the
`reply_recipients` status carries `model.RecipientLimits` (`ParseRecipientLimits`
on its `Limits`): `ReplyAll`, `Cc`, `Bcc` and `MaxRecipients`. A request using
what a channel does not support is refused by its adapter as a final
`rejected`. The mail adapters declare it (`smtp.mail`, `imap.mail`,
`google.mail`, `microsoft.mail`), and so do their reply draft effects
(`google.mail.reply_draft`, `microsoft.mail.reply_draft`), whose data takes
`reply_all`, `cc` and `bcc`. A host that follows a mailbox, such as the [mailbox
triage example](#mailbox-triage-example), and also replies reads these limits the
same way it reads the state limits.

## Raw message read

uma keeps a message's text, headers and staged attachments, not the provider's
whole copy. `Messages.Raw` reads that copy from the provider on demand and
hands it to the caller alone:

```go
raw, err := client.Messages().Raw(ctx, model.Lookup[model.MessageRef]{Call: call, Ref: msg.Ref})
// raw.Bytes is the RFC 5322 message (raw.MediaType "message/rfc822"), raw.Size its length
```

| Rule | Detail |
| --- | --- |
| Capability | The channel's adapter must declare `raw_read` (`google.mail`, `microsoft.mail`, `imap.mail`); otherwise `unsupported`. A host reads which channels have it, per channel, from `Configuration().Capabilities` |
| Authorization | As `Messages.Get`: `message.read` under the message's audience, before anything is read |
| Stored | Nothing: no copy, operation, event or cache. Each call reads the provider again |
| Size | The adapter's existing bound applies (`google.mail`: the receiver's raw read bound; `microsoft.mail`: 25 MiB; `imap.mail`: `max_message_bytes`). A message over it is `permanent` with reason `model.TooLarge`, never a truncated copy |
| Redacted or deleted | `conflict`, with no provider call: the content was scrubbed, and the provider's copy is not served around that |
| No provider copy | A message with no provider reference (an internal note, a message not yet delivered) is `conflict`; one the provider no longer holds is `not_found` |

Provider bytes are the provider's copy: Exchange may have re-encoded a
message since it arrived, and Gmail and IMAP return it as stored.

## Recovering from a lost response

Each mutating call in the key table of `Operations.Lookup`'s godoc claims
an operation key when it is accepted: the principal's tenant and actor, a
kind, a target, and the command's `Operation` ID. When the call's response is lost (a timeout, a dropped
connection, a crash after sending), the host knows the key but not the
operation ref. `Operations().Lookup` finds the operation by that key:

```go
op, err := c.Operations().Lookup(ctx, model.OperationLookup{
	Call: call, Kind: model.ActionConversationStart, Target: inbox.Ref(), ID: sentID,
})
switch {
case model.IsCode(err, model.NotFound):
	// Not claimed under this key: send the command again with sentID.
case err != nil:
	// Validation, authorization or engine error, as Operations().Get reports it.
default:
	// op is what Operations().Get returns; follow op.State from here.
}
```

| Rule | Detail |
| --- | --- |
| Kind and Target | From the mutating method's "Operation key" godoc line; `Operations.Lookup`'s godoc has the full table. Creations target `model.CreationTarget(tenant, kind)` |
| Scope | Own operations only: another actor's key, another tenant's, or a target outside the principal's tenant is `not_found` |
| Authorization | `operation.read` on the found operation, exactly as `Get`; a denial is `Get`'s error |
| Side effects | None: no write, no event |
| Recovery window | `RetentionPolicy.Operations`. A purged key is `not_found` too, which does not mean the command was never accepted; reusing the ID claims the key again only once it is gone |

Resending the same command under the same ID is also safe: it replays the
stored acceptance (`Acceptance.Replayed`). Lookup reads the state without
rebuilding or repeating the request. A resend that differs in payload is a
`conflict`. For `Messages().Edit` the payload includes the message's
audience: a retry after the audience narrowed is a different command and
conflicts rather than replays; `Operations().Lookup` still finds the stored
operation. Runnable example:
`ExampleOperations_Lookup` ([example_lookup_test.go](example_lookup_test.go)).

## Reconciling an operation

A send whose answer was lost leaves its operation `outcome_unknown`: the
provider may or may not have applied it, and the runtime never sends it
again before asking. It asks on its own schedule, which
`Operation.NextAttemptAt` shows, when the channel can reconcile; on a channel
that cannot (its adapter does not declare `reconcile`, or reports it
unavailable for that channel's configuration) the operation stays
`outcome_unknown` with nothing scheduled. `Operations().Reconcile` asks now,
for one operation, and returns the operation as `Get` shows it afterwards:

```go
op, err := c.Operations().Reconcile(ctx, model.Lookup[model.OperationRef]{Call: call, Ref: ref})
switch {
case model.IsCode(err, model.Unsupported):
	// The channel cannot reconcile: not declared, or unavailable for it.
case model.IsCode(err, model.Conflict):
	// A worker holds the operation, or it is not awaiting reconciliation.
case err != nil:
	// Unauthorized, not found, or the provider could not be asked.
case op.State == model.Unknown:
	// The provider's answer proved nothing; the runtime asks again at op.NextAttemptAt.
default:
	// Settled by the provider's record, or retry_pending: not applied.
}
```

| Provider answer | Result |
| --- | --- |
| The effect is applied | The operation settles (`provider_accepted` or `delivered`) with the provider's references |
| Reported failed for good | It settles `failed` or `rejected` |
| No record of it, past the adapter's settle window | It was not applied: the operation becomes `retry_pending`. Nothing is sent by this call; the runtime retries on its schedule (now, when none was scheduled) |
| Proves nothing, such as no record inside the settle window | Returned unchanged, still `outcome_unknown`; nothing is recorded |
| The reconciliation fails | Its error; nothing is recorded |

Reconcile never infers an outcome: only the adapter's answer changes the
operation. It is idempotent, so calling it again, or after the runtime
settled the operation, returns the settled operation without a provider
call.

| Rule | Detail |
| --- | --- |
| Authorization | `operation.read` on the operation, as `Get`, then `operation.reconcile`; either denial is `unauthorized` and nothing is revealed or called. Grant `operation.reconcile` only to callers that may spend a provider call |
| Scope | An operation of another tenant, or one never stored, is `not_found` |
| States | Settled: returned as it is. `outcome_unknown` with an unresolved attempt: reconciled. Any other state (`queued`, `retry_pending`, `dead_lettered`, `held`): `conflict`, since the runtime, `Replay` or a human decision owns it |
| Concurrency | An operation a worker is executing, or another Reconcile is running, is `conflict` (retryable); Reconcile never races a worker |
| Operation key | None: it takes no operation ID and claims nothing. A state change appends the usual `delivery.updated` event |
| After the runtime gave up | An `outcome_unknown` operation whose reconcile budget is spent, with no work scheduled, can still be reconciled by hand |

## Delivery status from providers

A reply or a started conversation is `provider_accepted` once the provider took
it. A provider that reports more (Twilio and Meta status callbacks, a
`relay.messages` gateway's status updates) moves it on, and the move is the
operation's own: read it with `Operations().Get`, or watch `delivery.updated`.

| Reported | The operation becomes |
| --- | --- |
| delivered | `delivered` |
| read | `delivered`, with `ReadConfirmedAt` set to the time the provider reported it (adapters that report one) |
| failed, for the attempt that carried the message | `failed`, final, with a safe summary; it is not sent again |

Reports are ranked, not applied in arrival order: sent < failed < delivered <
delivered and read. An operation only moves up. A late or repeated "sent" never
lowers `delivered`, a failure report never undoes a delivery, and a delivery
that follows a failure outranks it, since it proves the message arrived. Each
real change appends one `delivery.updated` event with trigger `provider`; the
first read of a delivered operation appends one more, with `state` and
`previous_state` both `delivered` and `read_confirmed_at` set. A report the
operation already outranks changes and announces nothing. The state lives in
the store, so it holds across restarts and replicas.

Only a reply or a conversation start follows this lifecycle; an edit, delete,
reaction or note ends when the provider applied it. A report moves an
operation within a worker pass of the runtime, not inside the provider's
request. The adapter-side contract is in
[Delivery status reports](docs/implementers.md#delivery-status-reports).

## Listing operations

`Operations().List` pages the operations the principal may read, most
recently written first, so a host needs no history file of its own:

```go
page, err := c.Operations().List(ctx, model.ListRequest{
	Call: call,
	Filter: model.Filter{
		Inboxes:  []model.InboxRef{inbox},
		Statuses: []string{string(model.RetryPending), string(model.Unknown)},
		Since:    &since,
	},
	Page: model.PageRequest{Limit: 50},
})
```

| Filter | Selects |
| --- | --- |
| `Inboxes` | Operations whose target lies in one of the inboxes (the inbox itself, a conversation, message or task in it) |
| `Conversation` | Operations on that conversation, or that produced a message, stream or task in it: replies, notes, edits, deletes, reactions, transitions, labels, starts, streams and task acts. Operations on a work item, an interaction or an attachment that record no conversation are not selected |
| `Statuses` | `model.DeliveryState` values, any of them; an unknown state is `validation` |
| `Since`, `Before` | The window on the time written, `Since` inclusive and `Before` exclusive, to the microsecond |
| `Kinds` | Operation kinds, any of them: the kind on the method's "Operation key" line, such as `message.reply`, `contact.write` or `contact.merge`. A kind no operation has selects none |
| `Actor` | The operations one actor sent: `Operation.Key.Caller`. Pass the principal's own actor to list only its own commands |

The filters combine; any other filter field is `validation`, and a
reference in another tenant is `not_found`. `Kinds` and `Actor` are matched
by the engine, not the store, so a selective one may return short pages
that still carry `Next`.

| Rule | Detail |
| --- | --- |
| Order | The time of an operation's latest change in the store, newest first, then the greater ID. An operation changes when it is accepted and at each state change: an attempt, a retry scheduled, a settling, a replay. A state change therefore moves it to the front |
| Authorization | Each operation as `Get`: `operation.read` at its revision. One the principal may not read is left out without trace: no total, no placeholder; a page may be short or empty and still carry `Next`. Each page reauthorizes |
| What it shows | Each operation exactly as `Get` returns it, `Pending` and `Key` included |
| Key | `Operation.Key` on every operation uma returns (acceptances, `Get`, `Lookup`, `List`, `Reconcile`): the key its command claimed, without the tenant. `Caller` sent it; `Kind`, `Target` and `ID` (the command's `Operation`) are the fields `Operations.Lookup` takes, so a listed operation shows what recovers it |
| Audience | The list shows every operation the principal may read, whoever sent it: `operation.read` decides, not the sender. A desk that shows each user only their own operations passes `Actor`; the filter narrows and never widens what `operation.read` allows |
| When | `Operation.UpdatedAt`, on `Get`, `Lookup`, `Reconcile` and every listed operation: the time of the operation's latest change, to the microsecond and in UTC. It is the time the list orders by and the window bounds, so a listed operation shows a time inside the window that selected it, and `Since: &op.UpdatedAt` lists it while `Before: &op.UpdatedAt` does not. The store sets it; a value in a command is ignored. Redacting the message an operation produced does not move it |
| Cursors | Bound to the caller, the tenant and the filter. A page names the position of the last operation it returned, so operations accepted after it never shift the next page. An operation written again after a page was read moves to the front and is not returned by that listing again: page once, and start over to see changes |
| Retention | Purged operations (`RetentionPolicy.Operations`) are gone from the list |
| Cost | A key range scan of an index per filter combination on PostgreSQL (migration 19 adds the scope columns and indexes); several inboxes or states sort the matches. The memory store reads every operation once |

Runnable example: `ExampleOperations_List`
([example_operations_list_test.go](example_operations_list_test.go)).

## Held operations

A plugin actor (`plugin:` prefix, `ActorID.IsPlugin`) can ask that a write be
held for a human decision instead of applied. The command carries a
`Hold`; the operation is stored as `held`, with no work, attempt or provider
effect, and a reviewer approves, rejects or edits it:

```go
cmd.Hold = &model.HoldProposal{Reason: "first reply by this plugin", TrustKey: "triage/reply", ExpiresAt: &deadline}
held, err := plugin.Messages().Reply(ctx, model.Reply{Command: cmd, /* ... */})
// held.Operation.State == model.Held; held.Operation.Held is the proposal.

got, err := reviewer.Operations().Approve(ctx, model.DecideHeld{
	Call: call, Operation: held.Operation.Ref, ExpectedRevision: held.Operation.Revision,
})
// got.Operation.State == model.Queued; got.Decision == model.HeldApproved.
```

| State | Meaning |
| --- | --- |
| `held` | Waiting for a decision. Precedes `queued`; `Operation.Held` (`model.HeldBlock`) carries the proposal |
| `queued` | Approved (or edited, then approved): the operation continues as any other |
| `rejected` | Rejected: nothing was applied and no work was queued |
| `expired` | The deadline passed with no decision. Final; nothing applied |

`held` and `expired` are new `model.DeliveryState` values: keep a default
branch when switching on a state. A held proposal is a write the caller did
not make yet, so a person's own command with a `Hold` is `validation`, and a
command that cannot be applied later is `unsupported` with nothing claimed.

| Rule | Detail |
| --- | --- |
| Verbs | `Operations().Approve`, `Reject` and `Edit`, each taking `model.DecideHeld` (`EditHeld` adds the new arguments). They are transitions of the proposal's own operation and claim no key. `Decided.Decision` is `approved`, `rejected` or `edited`; `Correction` is true for the last two |
| Authorization | The principal needs `operation.read` and the target's read right (else `not_found`, never a redacted stub: the held block is the draft content), then `operation.approve`, `operation.reject` or `operation.edit` (else `unauthorized`). Proposing grants none of them. `Held.DecidedBy` is the decider's actor. Approve and edit also need the operation's own action on its target; reject does not |
| Revision | `ExpectedRevision` is required and compared before anything is written: a mismatch is `conflict` with reason `model.StaleRevision`. A target that advanced since the hold is `conflict` with `model.StaleTarget`, unless `Force` is set; `Force` never skips the revision compare or an authorization. Test with `model.IsReason` |
| Key and replay | The proposal claims its operation key when held. A replay of the proposal returns the held operation (`Acceptance.Replayed`), also after an edit: the stored fingerprint is the proposal's. Approve applies under that same key. A decision already made is returned as it stands with `Decided.Replayed` set and writes nothing (a second approve, a second reject, an edit repeated with the same arguments); any other decision of a decided operation is `conflict` |
| Edit | `EditHeld.Args` replaces the stored intent (same extension name and version, new data), validated by the intent's validator: the target and audience cannot change, and an intent with no validator cannot be edited. The edit keeps the operation and its key; `Held.Edits` records who edited, when, before and after |
| Duplicate guard | While one operation is held, another proposal of the same kind on the same target under a different key is not stored: the held operation is returned with `Acceptance.AlreadyHeld`. Two kinds narrow this scope: a start is a duplicate only for the same recipient, a label create only for the same label name. A proposer that may not read the held operation gets `conflict` ("operation already held") instead |
| Deadline | `HoldProposal.ExpiresAt`. Approve and edit at or after it are refused; reject is not. `Runtime.Run` sweeps held operations on `WorkerOptions.SweepEvery` (default one minute, negative turns it off) and `Runtime.SweepHeld` sweeps once. The sweep writes `held -> expired` once per operation, applies nothing and moves no trust counter. An operation still held a full sweep window after it was held gets `Held.CarriedOverAt`; its state does not change |
| Redaction | Redacting the message a held operation concerns clears the draft content of the held block (preview, edited arguments, each edit's before and after); names, decision, decider and times stay |

### What can be held

Message edits, deletes, reactions, replies and notes, conversation label
changes and state verbs, and starts are holdable. Three local writes commit
with no approval step of their own, so a hold on them is `unsupported` until
`Config.Held` opts them in; once in, they are written only on approval, and
a reject or an expiry leaves nothing:

| `Config.Held` field | Holds |
| --- | --- |
| `LabelConfigure` | `Labels().Configure`, a label create or update. A create reserves the label's ID when held |
| `LabelDelete` | `Labels().Delete` |
| `ConversationTransition` | `Conversations().Transition` |
| `StartCreateAtHold` | When a held `Conversations().Start` creates its conversation. False (default): on approval, so a rejected or expired start leaves no conversation and the inbox's selection policy chooses among the conversations as they are then; the operation names its result message only once approved. True: in the unit that holds it, so the operation names the conversation and message up front, and a conversation a rejected or expired start opened stays, empty |

A start is always holdable; `StartCreateAtHold` only chooses when it writes.
All four default to false.

### Trust records

uma counts decisions under a key the host sets (`HoldProposal.TrustKey`, an
opaque string of at most 256 bytes, optional `TrustLabels` for listing). Policy
stays in the host: uma never reads a count as a reason to skip review, and
`Trust` returns numbers only.

| Counter | Moves |
| --- | --- |
| `Clean` | Consecutive approvals as proposed: +1 per clean approval, zero on a correction or `ResetTrust` |
| `Approvals`, `Corrections` | Totals. An approval as proposed is an approval; a reject or an edit is a correction (`LastCorrectionAt` is when) |

An expiry moves nothing. A replayed decision moves nothing. An operation held
with no trust key, or whose target belongs to no channel, counts nowhere.
Counters are per channel (the channel of the operation's target inbox), in
the same unit as the decision. An edit corrects the keys it names
(`EditHeld.CorrectedKeys`), by default the operation's own.

| Method | Does | Action on the channel |
| --- | --- | --- |
| `Trust`, `Trusts` | Read one entry; list entries by label | `trust.read` |
| `ResetTrust`, `ResetTrustSet` | Zero `Clean`, keep totals; several keys, all or none | `trust.reset` |
| `ImportTrust` | Seed a key from an earlier approval history | `trust.write` |

`DecideHeld.Batch` is optional. Approvals of one trust key that share a batch
count as one clean approval, so a host approving ten operations in one action
names one batch; the entry keeps the last 8 batch ids to answer a replay. An
empty batch makes each operation its own. A batch is 1 to 128 bytes of UTF-8
without NUL, and an invalid one is `validation` before anything is written. Reject and
edit are corrections, which no batch merges.

`ImportTrust` merges, never replaces, so it is idempotent and never lowers
what live decisions accumulated:

| Field | Result |
| --- | --- |
| `Approvals`, `Corrections` | The larger of stored and imported (`Approvals` defaults to `Clean` when zero; `Corrections` to 1 when zero and `LastCorrectionAt` is set) |
| `LastCorrectionAt` | The later of the two |
| `Clean` | Raised to the larger value only while the stored `LastCorrectionAt` is not later than the imported one: a later correction ended the imported streak |
| `Labels` | Replace the key's when given |

`Clean` may not exceed a non-zero `Approvals`, and no count may be negative
(`validation`). A repeat, or an import lower than the live counts, writes
nothing.

### Events

Three builtin kinds, internal audience, payload `model.HeldOperationPayload`
(references only: never the preview, the reason or any argument). A subscriber
needs `operation.read` and the read right of the operation's target.

| Kind | Emitted |
| --- | --- |
| `operation.held` | Once, when the proposal is stored; never for a replay or a duplicate-guard hit |
| `operation.decided` | Once per decision; never for a replay. `Decision` is `approved`, `rejected` or `edited` |
| `operation.expired` | When the sweep writes `held -> expired` |

The operation's state changes also append the usual `delivery.updated` event.

### Stores

A held operation is an ordinary operation record whose body carries the held
block, and trust counters are 64 protocol records under the reserved
`uma.trust.` keys. Neither adds a store method or a schema change; see
[Held operations and trust counters](docs/store-backends.md#held-operations-and-trust-counters).
Held operations list with `Operations().List` and `Statuses: held`; a held
operation is visible only to a principal that can read its target.

Runnable example: `ExampleOperations_Approve`
([example_held_test.go](example_held_test.go)). The decision record is
[docs/decisions/held-operations.md](docs/decisions/held-operations.md); the
surface is additive under [docs/compatibility.md](docs/compatibility.md#held-operations).

## Conversation labels

Labels sort conversations: "billing", "needs-legal", "vip". A label is
tenant vocabulary: one definition (`Client.Labels()`) shared by every inbox of
the tenant, and a conversation holds labels by reference.
`Conversations().AddLabels` and `RemoveLabels` change them and are idempotent: a label already held (or
absent) changes nothing, and a call that changes nothing commits without an
event. A call that changes labels moves the conversation's revision once and
emits one `conversation.label_added` or `conversation.label_removed` event per
changed label, readable by anyone who may read the conversation.
`Conversations().List` filters by `Filter.Labels`: `Any` of, `All` of and
`None` of the named labels. `Messages().List` honours the same selector on the
labels a message's conversation holds when the page is read, and needs
`conversation.read` on that conversation as well as `message.read`: a caller
who may read a message but not its conversation never receives a
label-matched message, and so learns nothing of the conversation's labels.

Labels are canonical and local for every channel. A provider's own labels,
such as `github.issues.labels` on a GitHub work item, are provider data: they
stay on the work item as the provider reported them and never become uma
labels or change a conversation's labels. uma does not match them by name
either: a provider label named "bug" and a uma label named "bug" are
unrelated, and name matching never establishes equivalence. Syncing a
provider's labels with uma's is future work, and will go through explicit
mappings scoped to a provider account or repository.

Label names should not carry personal data: they are tenant vocabulary, not
message content, so `Privacy().Redact` and contact cascades leave label
assignments in place.

## Conversation workflow state

Helpdesk-style clients keep local desk state on a conversation: who works it
and whether it is set aside. Both are fields of the conversation record and
part of its revision, changed through `Conversations()` and never sent to a
provider.

| Call | Action | Event (internal audience, references only) |
| --- | --- | --- |
| `Assign` (`Assignee` set) | `conversation.assign` | `conversation.assigned` (assignee, previous) |
| `Unassign` | `conversation.assign` | `conversation.unassigned` (previous) |
| `Snooze` (`Until` set) | `conversation.snooze` | `conversation.snoozed` (until) |
| `Unsnooze` | `conversation.snooze` | `conversation.unsnoozed` |
| a snooze ending by itself (a worker's sweep; no call) | none | `conversation.snooze_ended` (cause `until` or `message`, until), once per snooze |
| `Link` | `conversation.link` (both ends; `work_item.read` on a work item end) | `conversation.linked`, once per conversation end |
| `Unlink` | `conversation.link` (both ends) | `conversation.unlinked`, once per conversation end |

Each is a committed operation with an optional `ExpectedRevision` (a stale one
is `conflict`); a call that changes nothing commits without an event and
leaves the revision alone. The assignee must be eligible: uma asks the
authorizer whether the assignee itself may perform `conversation.assign` on the
conversation. The event log keeps the assignment history.

A snooze hides the conversation from lists that exclude snoozed ones until
`Until`, or until a live inbound message is recorded after the snooze was set,
whichever comes first. It is evaluated when read, so the conversation reads
as awake the moment it ends, and it holds across restarts; it is independent
of `Status` (unlike `pending`, it wakes on inbound). History that
`Runtime.Import` records (`Message.ImportedBy` set) is not new inbound and
never wakes it. A running worker (`Runtime.Run`, on its `SweepEvery` cadence)
then clears the stored snooze and emits one `conversation.snooze_ended`, with
cause `until` or `message`, in one unit with the conversation's new revision;
sweeps racing for it wake it once. `Filter.SnoozeEnded` lists the conversations
still holding an ended snooze, so a list under it is transient once a worker
runs. With no worker the snooze stays stored and no event is emitted.

Workflow state is desk-only. `Get` and `List` zero `Assignee` and `Snooze`
unless the caller also holds `conversation.read_workflow` on the conversation,
the workflow events reach only such callers, and exports never carry it.
`List` filters by `Filter.Assignee` (an actor, `Self`, or `None`) and
`Filter.Snoozed` (true: snooze in effect; false: the others) in storage, so
pages are full; under either filter a conversation whose workflow state the
caller may not read is left out.

A link associates a conversation with another conversation or a work item:
no history merge, no reply-scope change, no provider effect. A link between
two conversations is held by both (`Conversation.Links`, written in one
unit); a link to a work item by the conversation alone. A reader sees only
the links whose far end it may read, and a link event reaches a subscriber
only when it may read the far end it names. `Filter.Linked` selects
conversations holding a readable link, or none.

Read state is per actor: `MarkRead` and `MarkUnread` move the caller's own
read marker (`conversation.mark_read`), a record of its own, so marking never
moves the conversation's revision and takes no operation ID; repeating a
mark changes nothing. Each actual move emits one `conversation.read_marked` or
`conversation.unread_marked` event (conversation, actor, the marker's new revision;
never a message), which reaches that actor alone and only while it holds
`conversation.read_workflow`. A message is unread for an actor when it was
recorded after the marker and is neither an echo, nor imported history
(`Runtime.Import`), nor the actor's own message.
`ReadMarker` returns the caller's marker, and `Filter.Unread` lists the
caller's unread conversations in storage; no call reads another actor's
read state. This is uma's local desk read state, distinct from provider
read confirmations: those mark a delivered operation (`Operation.ReadConfirmedAt`,
see [Delivery status from providers](#delivery-status-from-providers)).

## Provider conversation state

A provider keeps its own state for a conversation: a mailbox archives, files,
reads, stars and trashes it; a chat workspace archives a channel or thread.
uma carries that state apart from its own. `Conversation.Status` and
`Conversation.Labels` are uma's desk vocabulary, the read marker (`MarkRead`)
is the caller's own, and none of them is ever merged with what the provider
holds, in either direction. The provider's view is
`Conversation.ProviderState`: one entry per channel, with `Archived`, a
`Folder` or provider `Labels` (by the provider's move shape), `Read`, `Flags`,
`Trashed`, and `Complete`, whether every provider item of the conversation was
observed.

Five calls on `Conversations()` change it at the provider:

| Call | Capability | Does |
| --- | --- | --- |
| `Archive` | `archive` | Archives the conversation; `Undo` unarchives |
| `Move` | `move` | Files it in a destination (a provider ID or a role: `inbox`, `archive`, `trash`, `junk`), or, on a provider with labels, adds and removes labels |
| `SetRead` | `read_state` | Marks it read or unread at the provider; never changes the read marker |
| `Flag` | `flag` | Sets or, with `Undo`, clears a flag of kind `star` or `important`; the provider reports the kinds it has |
| `Trash` | `trash` | Moves the account's copy to the provider's trash, or restores it; not `Messages.Delete`, and the other party's copy stays |

Each call is queued, returns when it commits, and is executed by
`Runtime.Run`. `ProviderState` changes only once the provider has applied
the change: until then `Operations.Get` shows it as `PendingState`, a refusal
leaves the state as it was, and a lost answer is reconciled, never repeated
blindly. If the provider reported newer state of the same part in the
meantime, that state wins.

Support differs per provider, and the channel says what it supports. A call
the adapter does not declare, or reports unavailable, is `unsupported` before
anything is stored; `Conversations().Capabilities` returns the status of each
verb with its limits: a `scope`, `kinds` for flags, a `shape` and `roles` for
moves. The scope decides who may call: `account` means only the connected
account's own view changes (a mailbox) and is authorized as
`conversation.state_account`; `shared` means everyone sees it (a chat
thread, a ticket) and is authorized as `conversation.state_shared`. A host can
grant a desk the first and withhold the second.

The provider can also report state itself: a `conversation.state` envelope
sets `ProviderState` and `conversation.changed` follows with the field
`provider_state`. Changes made in the mailbox client or the chat app, and the
echo of a change made through uma, arrive this way. An envelope for a
conversation uma does not hold is accepted and recorded as nothing.

[Adapters](docs/adapters/README.md#what-each-capability-gives-the-host) lists
which adapter declares which capability, and the adapter pages give the
limits and the provider calls: [Gmail](docs/adapters/google.mail.md#conversation-state),
[Microsoft Graph](docs/adapters/microsoft.mail.md#provider-conversation-state),
[IMAP](docs/adapters/imap.mail.md#provider-state),
[Slack](docs/adapters/slack.chat.md#archiving) and
[Discord](docs/adapters/discord.chat.md#archiving-threads).

## Provider mailboxes

A mail provider keeps containers that mail sits in or is marked with: Gmail
labels, Exchange folders and categories, IMAP mailboxes. They are not tenant
`Label`s (`Labels()`), and `ProviderLabel` names one only as a conversation holds
it. The mail adapters list and create them as extensions, so a host reads and
writes the provider's own vocabulary, for example to file a message into a
folder that may not exist yet. Both use the common model `model.ProviderMailbox`:

| Field | Meaning |
| --- | --- |
| `ID` | The provider's own: a Gmail label ID, an Exchange folder ID, an IMAP mailbox name as the server spells it; an Exchange category is referenced by display name, which is its ID |
| `Name` | The display name, with `/` as hierarchy separator on every provider (`Clients/Acme`); the adapter translates an IMAP server's delimiter |
| `Kind` | `label` (Gmail), `folder` (Exchange, IMAP) or `category` (Exchange) |
| `Role` | The special use, empty for most: `inbox`, `archive`, `trash`, `junk`, `sent`, `drafts` |

```go
// list: Extensions().Query, the adapter's list extension, a model.ProviderMailboxes result
res, err := client.Extensions().Query(ctx, model.QueryExtension{
	Call: call, Target: channel.Ref(), Audience: aud,
	Query: model.Extension{Name: model.MailboxListName(typ), Version: model.MailboxExtensionVersion},
})
boxes, err := model.DecodeProviderMailboxes(res)

// create: Extensions().Execute, an effect like any write (it can be held)
action, _ := model.CreateProviderMailbox{Name: "Clients/Acme"}.Extension(typ)
acc, err := client.Extensions().Execute(ctx, model.ExecuteExtension{
	Command: cmd, Target: channel.Ref(), Audience: aud, Action: action,
})
// once settled: Operations().Get(...).ProviderReferences holds model.ProviderMailboxRef(channel, id)
```

| Rule | Detail |
| --- | --- |
| Capabilities | Each adapter declares `extension_query` and `extension_execute` and one capability per extension, named as the extension: `model.MailboxListName(type)` (`google.mail.mailboxes`) gates the list, `model.MailboxCreateName(type)` (`google.mail.mailbox_create`) the create. A host gates a `list` or `create` verb per channel on them in `Configuration().Capabilities`; without one the call is `unsupported` |
| Registration | An adapter that declares a capability of its own needs its definitions: the host registers `Factory.Extensions()` in `Config.Extensions`, else `New` refuses the adapter |
| Authorization | List: `extension.query`. Create: `extension.execute`, a write like any other: a plugin actor proposes it with a `Hold` and a person approves it, and an approved create runs once |
| Create | Idempotent by name: a mailbox of that name and kind that exists is the result, never an error and never a second one. The adapter creates the missing parents of a path where the provider has them as objects (Exchange folders, IMAP mailboxes); Gmail keeps `/` in the label's name. `Kind` is optional: the adapter's own (`label` for Gmail, `folder` for Exchange and IMAP; Exchange also takes `category`) |
| Names | UTF-8 without control characters, at most 225 bytes, no empty path segment (`ValidateMailboxName`); refused at the call (`validation`) with nothing stored |
| State reports | `ProviderLabel.Name` is filled from the list where a provider reports only IDs (Gmail); an unreadable list leaves the ID as the name |

The adapters: [`google.mail`](docs/adapters/google.mail.md#provider-mailboxes),
[`microsoft.mail`](docs/adapters/microsoft.mail.md#provider-mailboxes),
[`imap.mail`](docs/adapters/imap.mail.md#provider-mailboxes).

## Searching messages

`Messages().List` searches as it lists: set `Filter.Text`, `Since`, `Before`,
`Attachments` or `Labels` and the page holds only the messages that satisfy
all of them, and the other filters, in recording order. There is no
separate search call and no ranking. Runnable example: `ExampleMessages_List`
([example_search_test.go](example_search_test.go)).

| Field | Matches |
| --- | --- |
| `Text` | Messages whose text parts hold every word of `Text`, in any order. Words are runs of letters, marks and digits, compared case-folded and whole: `appointment` does not match `appoint`, `foo@bar.com` is the words `foo`, `bar` and `com`, `v2.1` is `v2` and `1`. No phrase, prefix or stemmed match |
| `Since`, `Before` | `Message.OccurredAt` in the half-open range `[Since, Before)`, to the nanosecond. `OccurredAt` is the sender's or provider's time (for mail, the `Date` header), not the time uma recorded the message. `Since` must precede `Before` |
| `Attachments.Has` | Messages with at least one attachment part (`true`) or none (`false`) |
| `Attachments.MediaTypes` | Messages with an attachment whose record's media type, parameters stripped, matches `type/subtype` or `type/*` (lowercase). A redacted or missing record never matches |
| `Labels` | Messages whose conversation holds the labels the selector accepts when the page is read: `Any`, `All` and `None`, as for `Conversations().List` |

Only text parts are searched. Structured parts, `Message.Metadata` (a mail
subject lives there), attachment names and work item subjects are not, and
neither is the text of a redacted or provider-deleted message. Text is not
normalized: a precomposed and a decomposed spelling of the same letter are
different words, and scripts written without spaces tokenize as whole runs.

Limits, each refused as `validation`:

| Limit | Value |
| --- | --- |
| `Text` length | `model.MaxSearchTextBytes`, 256 bytes |
| Distinct words in `Text` | 1 to `model.MaxSearchTokens`, 16 |
| One word of `Text` | `model.MaxSearchTokenBytes`, 64 bytes; a longer word in a message is not indexed |
| `MediaTypes` | `model.MaxSearchMediaTypes`, 16 patterns; not with `Has: false`; an empty `Attachments` is refused |
| Other queries | Every query except `Messages().List` refuses the search fields, and so do subscriptions |

Search narrows and never widens: each message is still authorized on its own
(`message.read` under its audience), and a message the caller may not read,
an internal note included, is skipped without trace. Two fields add a check:
`MediaTypes` needs `attachment.read` on a matching attachment, so a list
never confirms the type of an attachment the caller cannot open, and `Labels`
needs `conversation.read` on the message's conversation, so a caller who may
read a message but not its conversation learns nothing of the labels.

Paging works as for every list, with two additions. A call scans at most 500
stored matches (five times the largest page of 100), readable or not, and
returns what it found with a `Next`: a page can be short or empty and still be
incomplete, so loop until `Complete`, never until a page is empty. And a page
that completes the search carries no `Next`: the position where the scan
stopped could lie past a message the caller may not read, so it is not
handed out. To find messages recorded since, run the search again. Pages are
separate reads: a message recorded between two pages appears on a later page
if it matches, never twice, and a cursor is valid only for the filter it was
issued for (an equivalent spelling of the same words is the same filter).

```go
req := model.ListRequest{Call: call, Filter: model.Filter{Text: "invoice refund", Since: &since}}
for {
	page, err := c.Messages().List(ctx, req)
	if err != nil {
		return err
	}
	// ... use page.Items ...
	if page.Complete {
		break
	}
	req.Page.After = page.Next
}
```

**Known limit: existence beyond the scan bound.** A caller cannot ask what
exists, but can sometimes infer it. The store narrows by the search fields
before authorization, so when a call scans its 500 stored matches and none
is readable, the page is empty but incomplete and carries a `Next`. That tells
a caller that at least 500 messages matching its search exist before that
position, and the number of such pages it must skip approximates how many
there are. Sealed paging closes only the end of the walk: the completing
page reveals nothing. A caller whose searches match many messages it cannot
read, for example internal notes, can still observe that they exist. This is
accepted and part of the paging contract, not a defect awaiting a fix. Hosts
that cannot accept it should not give such callers a search over
conversations they only partly read.

The same holds for plain lists, with no search. Every list authorizes each
scanned record on its own and leaves an unreadable one out without trace, with
no total, but the scan bound applies and `Next` is the store position the scan
reached. A page can therefore be short or empty and still carry a `Next`, which
tells a caller that records it cannot read exist before that position. Only the
searching `Messages().List` and `Operations().List` seal the completing page;
every other list returns a `Next` on its completing page too, so an extra empty
page after the last readable record shows that the walk went past hidden ones.
Treat `Complete`, not `Next`, as the end of a walk, and give a caller a list
only over scope it may read in full when the existence of other records is
itself sensitive.

On PostgreSQL the words of each message are stored as an array beside the
message and matched by array containment through a GIN index, computed in Go
at every message write with the same tokenizer the memory store uses; times
are stored as Unix seconds and nanoseconds. Attachment predicates read the
attachment holder index and the attachment record. Search adds no extension
and no text search configuration. The conformance suite runs the same
`MessageSearch` cases on both stores, and `BenchmarkMessageSearch` in
`store/postgres` measures the queries on a fixed 20,000-message fixture.

## Reading the event log

Three calls read committed events. Pick by what you need:

| Need | Call | State |
| --- | --- | --- |
| Catch up, audit or backfill: read history in pages, any number of times | `Events().List` | None. No subscription, lease or acknowledgment; the call writes nothing |
| Receive new events at least once, resuming after a crash | `Events().Open`, `Poll`, `Ack` | A subscription: a checkpoint, a lease and an ACK per batch |
| Show one event whose ID you hold | `Events().Get` | None. Authorized like `List`; an event the principal may not read is `not_found` |

`List` is for reading; use a subscription when the host must act on every
event and record that it did. To read the past and then follow the log, list
up to a page's `Next` and open a subscription there. `Next` is a position of
the same space as `Subscription.Checkpoint`, so the subscription delivers
exactly the events committed after the list, with no gap and no duplicate:

```go
req := model.ListEvents{Call: call, Start: model.StartBeginning}
var next model.Cursor
for {
	page, err := c.Events().List(ctx, req)
	if err != nil {
		return err
	}
	// ... use page.Items ...
	next = page.Next
	if page.Complete {
		break
	}
	req.Start, req.After = model.StartCheckpoint, next
}
sub, err := c.Events().Open(ctx, model.OpenSubscription{
	Command: cmd, Consumer: "auditor", Start: model.StartCheckpoint, After: next,
	PayloadMajor: model.PayloadVersion1,
})
```

Runnable example: `ExampleEvents_List` ([example_events_list_test.go](example_events_list_test.go)).

| Rule | Detail |
| --- | --- |
| Start | `StartBeginning` (the retained beginning), `StartTail` (the head: an empty, complete page whose `Next` is the head) or `StartCheckpoint` with `After` set. There is no default |
| Limit | Counts the events scanned (`DefaultEventListLimit` when zero, capped at `MaxEventListLimit`), not the events returned. Events the principal may not read count against it and are dropped, so a page can be short or empty and still advance: loop until `Complete`, never until a page is empty |
| Complete and Next | `Complete` means the read reached the head. `Next` is the position after the last scanned event, on an empty page too |
| Empty log | On a log never written, `StartTail` and `StartBeginning` both return an empty, complete page whose `Next` is empty. An empty `Next` is not a checkpoint: continue with `StartBeginning` (or open the subscription with `StartBeginning`), never `StartCheckpoint` |
| Filter | `Inboxes`, `Conversation`, `Kinds` and `Since` only; they select before `Limit` applies. Any other set field and an unknown kind are `validation`; an inbox or a conversation of another tenant is `not_found`. An inbox selects the events about it (such as an adapter's inbox-level extension events) as well as those about its conversations and messages. A field added to `Filter` later stays refused until `List` honours it |
| Conversation | Selects the events about one conversation: those whose resource is the conversation (any kind, extension kinds included) and the builtin `message.*`, `conversation.*`, `task.updated`, `work_item.updated` and `interaction.*` events whose payload names it. Events about no conversation (`delivery.updated`, `identity.linked`, `contact.*`, channel and inbox events, extension events about anything else) are never selected while it is set; list them by inbox or kind. Subscriptions refuse it |
| Since | Selects the events whose `RecordedAt` (the time uma recorded the event) is at or after it, not `OccurredAt` (the provider's or host's time, as `Messages.List` compares). The read starts at the first log position `Since` can select, or at `After` when that is later, so paging with the same filter from each `Next` neither repeats nor skips an event, and a `Since` later than every event gives an empty, complete page at the head. Subscriptions refuse it |
| Authorization | `event.list` on the tenant's event log (`model.EventLogRef`) admits the call; a denial is `unauthorized` and nothing is read. Each scanned event is then authorized as a subscription poll authorizes it (including the policy action of an extension event kind) and one the principal may not read is skipped with no placeholder and no count |
| Retention | A position behind retention is `cursor_expired`: restart from `StartBeginning` or `StartTail` and reconcile from current resource state |
| Payloads | Returned as stored: `List` declares no payload major. Each event's `Payload` names its schema and version; skip or reject a version you do not read |

### Reading one event

`Events().Get` reads one event by ID, for the host that holds an event ID (from
a log, a ticket or a support report) and needs the event itself, not a scan of
the log:

```go
ev, err := c.Events().Get(ctx, model.Lookup[model.EventRef]{
	Call: call, Ref: model.EventRef{Tenant: "acme", ID: eventID},
})
if model.IsCode(err, model.NotFound) {
	// No such event, or none this principal may read: the two look the same.
}
```

| Rule | Detail |
| --- | --- |
| Authorization | `event.list` on the tenant's event log admits the call, as for `List`; a denial is `unauthorized` and nothing is read. The event is then authorized as `List` and a poll authorize each event: its inbox, and the resource it is about at the event's audience (including the policy action of an extension event kind) |
| Not found | An event that never existed, one removed by event retention, a ref of another tenant and an event the principal may not read are the same `not_found` error, so a caller cannot probe for events it may not read. An external actor cannot fetch the event of an internal note this way. A failure of the policy itself is not `not_found`: it fails the call |
| State | None. No subscription, lease or acknowledgment; the call writes nothing |
| Payload | Returned as stored, as `List` returns it |

## Work item status changes

`WorkItems().Act` changes a work item with a `model.WorkItemUpdate`. Who owns
the field decides where the change happens:

| Field | Owner | `Act` |
| --- | --- | --- |
| A field the work item lists in `LocalFields` (subject, assignees, fields, or the status itself) | uma | Commits at once as a local operation; the work item changes when `Act` returns |
| The status of a work item whose status the provider owns | The provider | Queued (`Queued`) as an outbound operation; the work item changes only from the provider's answer |
| Any other provider-owned field | The provider | `model.Unsupported`: ingest-only |

A provider-owned status takes an optional `StatusReason` (`completed`,
`not_planned`, `reopened`, ...), which only the provider's vocabulary gives a
meaning: omit it for the adapter's default. The adapter declares the
transitions it performs (`adapter.Descriptor.WorkItemActs`: target status,
source statuses and reasons) beside the `work_item_act` capability.
A transition or reason it does not declare is `model.Unsupported` when
`Act` is called, before anything is queued; uma never coerces one status onto
another, and a closed status reopens only where the adapter declares it. A
provider-owned status mixed with a local field, or a reason with a local
status, is `model.Validation`.

The change is sent even when the stored copy already shows the target: it may
be stale. The provider's own webhook of the change is an ordinary observation;
it never settles the operation, which only the provider's answer or
reconciliation does. A provider success that returns the work item's state
applies it in source order. One that returns none, and a settlement found by
reconciliation (which carries no provider state), leave the work item as it
is until the provider's own webhook arrives: settling queues no refetch,
and `Config` has no service principal to run one.

A work item's status and its conversation's status are independent
lifecycles. Closing or reopening an issue, by `Act` or by someone at the
provider, never changes the conversation, whatever its status; moving the
conversation never reaches the provider. Staff close the conversation with
`Conversations().Transition`.

For GitHub (`github.issues`), `Act` closes an issue as `completed` (the
default) or `not_planned` and reopens it as `reopened`; the other statuses and
reasons are unsupported. The [package documentation](adapter/ticket/github/doc.go)
lists the mapping and the failures.

## Answering interactions

A command, button press or form submission a provider reports is stored as an
interaction, never as a message. `Interactions().Get` and `List` read the
records (filter by inbox, conversation or actor identity); `Respond` answers
one through the adapter that reported it.

```go
acc, err := c.Interactions().Respond(ctx, model.Respond{
	Command:     model.Command{Call: model.Call{Correlation: corr}, Operation: opID},
	Interaction: in.Ref,
	Mode:        model.ResponseModeEphemeral, // message | ephemeral | update
	Audience:    model.Audience{Visibility: model.Restricted},
	Content:     content,
	ExpiresAt:   time.Now().Add(time.Minute), // not past in.RespondUntil
})
```

| Rule | Behavior |
| --- | --- |
| Mode and audience | Both explicit; the engine never implies one from the other or falls back to another mode. `ephemeral` needs a restricted audience and answers the actor; `message` commits a message with the operation (origin `response`, `in_response_to` the interaction); `update` replaces the provider message the control sits on and stores no local message |
| Initial response | One per interaction, claimed atomically with the operation: of two concurrent initial responses one is accepted and the other is `conflict`. The same operation ID and payload replays the accepted response, also after its window |
| Follow-ups | `FollowUp: true` needs the `respond_follow_up` capability (`unsupported` otherwise), counts one and never consumes the initial claim. `Interactions().Ephemeral` naming an `Interaction` is a follow-up to its actor; without one it is an independent ephemeral reply that never touches interaction state |
| Deadlines | `ExpiresAt` may not pass `RespondUntil` (zero: the provider states no window). A passed window or deadline is `deadline_expired`, at submit and again at the attempt, which then makes no provider call |
| Redaction | A redacted interaction refuses a new response with `conflict`; a queued one fails `permanent` at its attempt |
| Token | The provider's response token is read from the interaction's storage at the attempt and handed to the adapter only; it is never in a record, the operation, its attempt evidence, an event or an error. A provider without tokens is answered with an empty one |

Reading needs `interaction.read` under the interaction's audience; responding
needs `interaction.respond` on the interaction. `Messages().Reply` may name the
interaction (`InResponseTo`) to correlate a reply: that is recorded and
announced, nothing more.

[Interactions](docs/interactions.md) covers the inbound kinds, the record's
lifecycle, the deferred acknowledgement, token custody, the response modes,
how Streams differ, and which adapters take part.

The bot companion (`companions/bot`) routes `interaction.received` events to
handlers by command name, action ID or form schema and answers through
`Turn.Respond` and `Turn.FollowUp`. Their operation IDs derive from the event,
so a redelivered event replays its response; a lost initial claim ends the
event as ignored, an expired window drops it.

## Memory store limits

`store/memory` is for tests, demonstrations and small bots. A unit of work
copies each table it writes on the first write, so what a write costs grows
with how much the tenant already holds. Medians on an Apple M1 Pro; the
method, revision and full tables are in
[Pinned benchmarks](docs/evaluation.md#pinned-benchmarks).

| Messages already stored in the tenant | 100 | 1,000 | 10,000 | 100,000 |
| --- | --- | --- | --- | --- |
| Ingest one message into one conversation | 0.09 ms | 0.5 ms | 3.8 ms | not measured |
| Ingest one message from a new contact | 0.18 ms | 0.9 ms | 7.3 ms | not measured |
| Garbage per ingest (existing / new contact) | 0.3 / 0.6 MB | 2.3 / 5.0 MB | 21 / 44 MB | not measured |
| Write one message record (store level) | 0.01 ms | 0.26 ms | 1.8 ms | 17 ms |
| First page of 50, one conversation | 0.01 ms | 0.07 ms | 0.6 ms | 6 ms |
| Page of 50 late in the table | 0.06 ms | 1.0 ms | 14 ms | 148 ms |
| Memory retained per stored message | 3 to 6 KB | | | |

- **Suitable:** a tenant holding up to about a thousand messages, where an
  ingest stays under a millisecond, a few thousand at a few milliseconds,
  and anything short-lived: tests, demos, prototypes. Ten thousand messages
  in one tenant still run, at 4 to 7 ms per ingest (about 140 to 260 messages
  a second at best) and tens of megabytes of garbage per message.
- **Not suitable:** a bot or inbox that keeps growing. Cost per write is linear
  in the stored records, so total cost is quadratic, and memory use is 3 to 6
  KB per message with no ceiling. `Purge` frees events, operations, attempts,
  ingress copies, exports and contacts; messages and conversations are never
  freed. Tens of thousands of messages in one tenant, any host that must
  survive a restart, and a host that runs several workers against one tenant
  belong on `store/postgres`.
- **Workers:** a unit that commits after another unit copied the same tenant
  fails with `transient` and is rerun. With 8 workers on one tenant a claim
  takes 6 to 7 units on average, and nearly half need more than three.
- **Tenants:** each tenant pays only for its own records and tenants never
  contend, so a host with many small tenants stays fast where one big tenant
  would not.
- **Reads:** a list builds and orders the whole table before it cuts a page,
  so even a first page costs time proportional to the table. Walk large
  tables with `Page.Limit` at its maximum, not with small pages.

## Out of scope for v0.1

Two parts of the product requirements are partial and are not in v0.1:

- **Provider read state.** There is no `read` delivery state; a provider's
  read confirmation marks a delivered operation with the time it was
  reported (`Operation.ReadConfirmedAt`), which `relay.messages` reports. A
  Meta WhatsApp `read` or `played` and a Twilio `read` still map to `delivered`
  without a time. Desk actors' own read state is local
  ([Conversation workflow state](#conversation-workflow-state)). Sending one
  is in: `Messages.Receipt` tells the sender of a received message that it
  was delivered or read, on adapters declaring `receipt`.
- **Mentions.** A message has no normalized mention form. The bot companion's
  mention route takes a host-supplied `IsMention` predicate.

Both arrive later as additions under the closed-set rule in
[Compatibility](docs/compatibility.md#go-api): a new value in a closed set
(for example a `DeliveryState`) or a new optional field is announced in the
release notes, and callers keep a default branch.

## Design rules

- Core depends on the standard library only. No provider SDK, database driver,
  HTTP framework, global logger or environment-variable convention.
- Importing uma starts nothing and performs no network or file I/O.
- The host owns credentials, configuration sources, process lifecycle, port
  binding, scheduling, workers and migrations.
- Missing authorization policy denies access. Unknown visibility fails closed.
  Public replies and internal notes are separate operations.
- Ingress is authorized like any write: the service principal a receiver or
  webhook runs as needs `channel.receive` on the channel, checked when it
  binds and again for every accepted ingress into the resolved inbox, so a
  revoked grant stops ingress without rebinding.
- Outbound effects are accepted as durable intent before any provider call.
  Results distinguish queued, provider-accepted, delivered, failed and
  outcome-unknown.
- Third-party adapters and storage backends register through the same public
  contracts as maintained ones, with no fake methods for capabilities they
  don't support.
- Capability discovery (`Configuration.Capabilities`) is the adapter's
  declaration intersected with engine support and current status: a
  capability no core operation executes is unavailable whatever the adapter
  reports. Every builtin capability is executed today (`edit`, `delete`,
  `react`, `work_item_act`, `respond`, `respond_follow_up`, `receipt`,
  `membership`, `history`), so discovery
  lists each one an adapter declares with the status its instance reports.
- Desk clients read the same statuses for one conversation through
  `Conversations.Capabilities`, authorized as `conversation.read` under the
  internal audience instead of `channel.read`: statuses only (name,
  availability, reason such as `disabled`, limits), no channel
  configuration, and nothing for a participant limited to public content.

## Development

Tool versions are pinned in `mise.toml`. `go.work` lists every module for
editors and cross-module work; `make` targets check each module on its own
`go.mod`.

```sh
mise install
make check   # go.work + tidy + vet + golangci-lint + tests, every module
make check-changed   # the same checks, only for what the branch changed
```

`make check-changed` diffs against `origin/next`, maps changed files to Go
packages and runs build, vet and tests for those packages and the packages
that import them in any module. Lint and format cover the changed packages
only. Test files, docs and testdata rerun just their own package. A change to
`Makefile`, `go.mod`, `go.sum`, `go.work`, the toolchain or lint config runs
the full `make check`. `CHECK_DRY=1 make check-changed` prints the plan, and
`CHECK_BASE=<ref>` picks another base.

The pinned toolchain is `go 1.27.3`, the newest floor any module declares.
The root module, `contrib/otel` and the example modules declare `go 1.26.0`;
`contrib/kit` declares `go 1.27.3`, the floor of `hop.top/kit`; the rest
declare `go 1.26.1`. CI builds, vets and tests each floor on its own
release: `mise exec go@1.26.0 -- scripts/min-toolchain.sh 1.26.0`, then the
same with `1.26.1`. The script reads each module's `go` directive and lists the
ones above the given version as skipped.

Adapter tests replay recorded provider exchanges, so `make check` needs no
provider credentials. To record or refresh an adapter's cassettes, follow
[Record adapter cassettes](docs/recording/README.md): one guide per provider
for test accounts, credentials, environment variables and the checks to run
before committing.

Writing an adapter: [Implementer contracts](docs/implementers.md) state what
core guarantees and what the adapter owns, such as who validates extension
data and when.

Release process and module bump order: [RELEASING.md](RELEASING.md).

## License

[Apache-2.0](LICENSE)
