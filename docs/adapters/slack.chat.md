# slack.chat

`slack.chat` is the Slack adapter. One channel is one workspace install of one
Slack app. Slack calls the app's request URL with signed requests, or, for an
app behind NAT or without a public URL, sends the same payloads over a Socket
Mode connection uma opens (experimental); uma reads them, and answers through
the Web API on the host's HTTP client.

| | |
| --- | --- |
| Key | `slack.chat` |
| Package | `hop.top/uma/adapter/chat/slack`, in the module of the same path |
| Capabilities | `receive`, `receive_http`, `send`, `start_conversation`, `edit`, `delete`, `react`, `history`, `reconcile`, `respond`, `respond_follow_up`, `extension_execute`, `archive` |
| Events | `message.received`, `message.updated`, `interaction.received` |
| Identity namespaces | `slack.chat.user` |
| Sender kinds | person, bot |
| Provider permissions | `app_mentions:read`, `channels:history`, `channels:manage`, `channels:read`, `chat:write`, `groups:history`, `groups:read`, `groups:write`, `im:history`, `im:write`, `mpim:history`, `reactions:read`, `reactions:write` |
| Message IDs and threading | assigned by Slack (conversation ID and `ts`); no email-style threading; selection per room, per thread or per contact (a direct message with each user) |
| Reply parent | reported for a thread reply: the thread root (Slack names no other parent); a root or top-level message has none |
| Interactions | reports and answers buttons, selects, slash commands and modal submissions ([interactions](../interactions.md)); opens and changes modals as extension effects ([Modals](#modals)) |
| Maturity | experimental |

The matrix of all adapters is in [Adapters](README.md).

## Capabilities and operations

| Capability | Behavior |
| --- | --- |
| `receive` | Experimental. `Runtime.Receive` runs the Socket Mode receiver: it opens a WebSocket with `apps.connections.open` and the app-level token and takes the same events, interactions and slash commands over it; see [Socket Mode](#socket-mode) |
| `receive_http` | `Runtime.Webhook` takes Slack's signed requests: `url_verification` and Events API deliveries (messages, mentions, edits, deletions, reactions, channel archives) |
| `send` | `Messages.Reply` posts a message with `chat.postMessage` to the stored route: a room, a thread, or a user's direct message |
| `start_conversation` | `Conversations.Start` posts the first message to a room, a thread or a user the same way; a user's direct message is opened with `conversations.open` |
| `edit` | `Messages.Edit` replaces the text and blocks of a message with `chat.update` |
| `delete` | `Messages.Delete` deletes a message with `chat.delete` |
| `react` | `Messages.React` adds or removes the bot's reaction with `reactions.add` and `reactions.remove` |
| `history` | `Runtime.Import` reads a window of past messages of a room, a direct message, a thread, or the configured conversations; see [History](#history) |
| `reconcile` | `Operations.Reconcile` and the worker read Slack for what a lost answer applied, never writing; see [Reconciling](#reconciling) |
| `respond` | `Interactions.Respond` answers an interaction by posting to its `response_url`, as a message, an ephemeral message or an update of the message the control sits on |
| `respond_follow_up` | Further responses to the same `response_url`, up to Slack's five in thirty minutes |
| `archive` | `Conversations.Archive` archives the channel of a room conversation with `conversations.archive`, or unarchives it with `conversations.unarchive`; scope `shared`, see [Archiving](#archiving) |
| `extension_execute` | `Extensions.Execute` runs `slack.chat.views.open`, `slack.chat.views.push` and `slack.chat.views.update`: a modal opened, stacked or changed with the `trigger_id` of an interaction; see [Modals](#modals) |

## Not declared

Reason classes are defined in [Adapters](README.md#why-a-capability-is-not-declared).

| Capability | Reason | Why |
| --- | --- | --- |
| `reply_recipients` | `provider` | A Slack message goes to its room or thread; there is no cc or blind recipient |
| `receipt` | `provider` | Slack documents no call that tells the author of a message it was delivered or read |
| `membership` | `not built` | `conversations.invite` and `conversations.kick` could add and remove participants |
| `refetch` | `provider` | A message edit carries the whole new message; two changes never tie on a revision |
| `work_item_act` | `provider` | Slack has no work items |
| `internal_note` | `provider` | Slack has no private message that is part of the conversation; notes stay in uma |
| `extension_query` | `not built` | No read is shipped; `auth.test` (token and workspace identity) would be the first |
| `probe_permissions` | `not built` | `auth.test` could check that the bot token is live |
| `stream` | `not built` | `chat.startStream`, `chat.appendStream` and `chat.stopStream` could carry a native stream; sends and edits can stand in |
| `send_ephemeral` | `not built` | `chat.postEphemeral` |
| `signal` | `not built` | `assistant.threads.setStatus` shows a status in an AI assistant thread; nothing sends it |
| `move` | `provider` | A room has no folders or labels |
| `read_state` | `provider` | `conversations.mark` needs a user token; a bot token cannot set a read cursor |
| `flag` | `by design` | Flag kinds are `star` and `important`; Slack's nearest call, `pins.add`, marks one message, not a conversation, so pins are left out of conversation state |
| `trash` | `provider` | A room has no trash; deleting one is not undoable |
| `raw_read` | `not built` | Raw read is built for mail only; the provider's whole copy of a message is not read |

## Modals

A modal is opened with `views.open`, and "a `trigger_id` will expire 3 seconds
after it's sent to your app"
([modals](https://docs.slack.dev/surfaces/modals/)); the id is single-use
("The trigger_id was already exchanged in a previous call",
[views.open](https://docs.slack.dev/reference/methods/views.open/)). Nothing
else opens one: a `response_url` posts messages, and the request's own answer
carries a view only for a modal submission. The adapter ships three effects,
registered from `Factory.Extensions` and authorized as `extension.execute` on
the effect's target (a conversation, an inbox or the channel):

| Effect | Slack method | Data |
| --- | --- | --- |
| `slack.chat.views.open` | `views.open` | `trigger_id`, `expires_at`, `view` |
| `slack.chat.views.push` | `views.push`: a view on the stack of the modal the `trigger_id` came from | `trigger_id`, `expires_at`, `view` |
| `slack.chat.views.update` | `views.update` | `view`, and `view_id` or `external_id` (one of them), optionally `hash` |

All three are version 1, and every effect's data is a JSON object whose unknown
fields are ignored and never sent. A modal is shown to the person who pressed or
typed, is no message, and uma records nothing of it: the effect's audience is
policy's to judge, not the adapter's.

### From the interaction to the call

```go
in, _ := c.Interactions().Get(ctx, model.Lookup[model.InteractionRef]{Ref: ref}) // after interaction.received
trig, ok := slack.TriggerOf(in)              // the trigger_id and when it stops working
data, _ := json.Marshal(map[string]any{
	"trigger_id": trig.ID,
	"expires_at": trig.ExpiresAt.Format(time.RFC3339Nano),
	"view":       modal,                     // a Slack modal view object
})
_, err := c.Extensions().Execute(ctx, model.ExecuteExtension{
	Command:  model.Command{Call: model.Call{Correlation: corr}, Operation: opID},
	Target:   in.Ref.Conversation.Ref(),
	Audience: in.Audience,
	Action:   model.Extension{Name: slack.ExtensionViewsOpen, Version: slack.ExtensionVersion, Data: data},
})
// then run the worker: Runtime.Step, or Runtime.Run with a short Poll
```

The trigger_id is not a field of the record: it is part of the interaction ID
the adapter reports (`action:<trigger_id>`, `command:<trigger_id>`,
`view:<view id>:<trigger_id>`, see [Interactions](#interactions)) and so of the
record's provider reference, which survives redaction. `slack.TriggerOf` reads
it back, for an interaction of this adapter whose payload carried one (a
payload without one is named by a digest and has none), and returns the
record's acknowledgement deadline as `ExpiresAt`: three seconds from the
request that carried it, the same instant. Slack's modals guide names slash
commands, block actions and shortcuts as the sources of a `trigger_id`; it does
not say that a modal submission's can open a view, so for a submission use
`response_action`, which this adapter does not offer (see
[Answering inside the request](#answering-inside-the-request)).

The three seconds are spent in uma too: the effect is stored as an operation and
a worker executes it, so the time from Slack's request to the call is the
record, the `interaction.received` event reaching the handler, `Execute`, and
the worker's claim. A worker's default wait between empty rounds is a second
(`WorkerOptions.Poll`), so a host that opens modals runs a worker with a short
poll, or calls `Runtime.Step` right after `Execute`, which claims at once. An
attempt that comes too late costs nothing: it is refused before any call.

| The attempt | Outcome |
| --- | --- |
| `expires_at` has passed (at it or after) | `rejected`, `deadline_expired`, no call |
| `expires_at` is more than 8 seconds past the host's clock (3 seconds of life and 5 of clock skew) | `rejected`, `validation`, no call: no `trigger_id` lives that long, so the guard would be off |
| Slack: `expired_trigger_id` | `rejected`, `deadline_expired` |
| Slack: `exchanged_trigger_id` | `rejected`, `conflict` |
| Slack: `invalid_trigger_id`, `view_too_large` | `rejected`, `validation` |
| Slack: `push_limit_reached`, `duplicate_external_id`, `hash_conflict` | `rejected`, `conflict` |
| Slack: `not_found` (an update's view) | `rejected`, `not_found` |
| Another code Slack lists as refusing the request, or any other | as for a send: `invalid_arguments` and the block codes `validation`, a denial `unauthorized`, anything else `permanent`; all final |
| 429 | `rate_limited` with `Retry-After`, retryable: nothing was applied, and a retry is refused locally once `expires_at` passed |
| Connection never made | `transient`, retryable |
| A 5xx, `internal_error`, an answer lost after the request was written | `outcome_unknown`, **never sent again**: the `trigger_id` may be spent |

A refused credential is resolved again once, fresh, as for a send. A `trigger_id`
is used by one attempt: an unknown outcome is not reconciled (`Reconcile` says
`unsupported` for this capability, so the operation settles `outcome_unknown`),
because a spent `trigger_id` cannot be asked what it opened. A modal that did
open is at worst opened once. Errors and failure messages carry Slack's error
code and nothing of the action: never the `trigger_id`, the view or its text.

### The view

`view` is a modal view object, sent as the JSON object it is (the modals guide's
example) and otherwise unchanged, so Block Kit Slack adds later passes. Before
any call, and at the call when the factory's `ValidateAction` is asked
(`model.Validation`, nothing stored), the adapter checks what
[modal views](https://docs.slack.dev/reference/views/modal-views/) states:

| Field | Rule |
| --- | --- |
| `type` | `modal`, required |
| `title` | `plain_text`, 1 to 24 characters, required |
| `close`, `submit` | `plain_text`, 1 to 24 characters; `submit` required when a block is an `input` |
| `blocks` | an array of at most 100, required; Slack judges each |
| `callback_id`, `external_id` | at most 255 characters |
| `private_metadata` | at most 3000 characters |
| `clear_on_close`, `notify_on_close`, `submit_disabled` | booleans |
| whole view | at most 250 KiB encoded (Slack: `view_too_large`, "View exceeds 250kb"; read as 250 KiB); the action as a whole is also bounded by the structured payload limit, 256 KiB |

`trigger_id` has Slack's documented shape (`132456.7890123.abcdef`: two numbers
and a token, dot separated), `expires_at` is RFC 3339, `view_id` a Slack view ID
(`V...`), and `hash` the string Slack gave the view. A refusal message names the
field and the rule, never a value.

### Changing a modal later

An opened or pushed view's Slack ID is on the settled operation, as a provider
reference (`slack.chat`, kind `view`, ID `V...`), for a later `views.update`.
The interaction record of a button inside a modal names no view, so the other
way is to open the modal with an `external_id` of the host's own, unique per
team ("must be unique for all views on a per-team basis"), and update by it.
`hash` is Slack's guard against a stale update; the adapter passes one the host
holds and a mismatch is `conflict`. A push adds to the stack ("a modal can hold
up to 3 views at a time"): the third push is `conflict`.

### What the contract gives, and what it does not

uma's extension effect has no link to an interaction: it is aimed at a
conversation, an inbox or the channel, and the adapter never receives the
interaction record, its recorded times or its token. So the `trigger_id` and
its end travel in the action, put there by the host, and four things follow:

- Authorization is `extension.execute` on the target, not `interaction.respond`
  on the interaction. A policy that should limit who may open modals grants that
  action on the conversations concerned.
- The engine does not recheck the interaction before the attempt (redaction, the
  respond window), and does not bind the call to the interaction's actor.
  Slack binds a `trigger_id` to the user it was sent for, and refuses what is
  expired, spent or invalid.
- `expires_at` is as honest as the host that sets it. The adapter refuses past
  it and Slack remains the authority.
- Slack's `interactivity_pointer`, the other way to exchange an interaction, is
  not offered.

The smallest contract change that would remove the first two is an optional
interaction reference on `model.ExecuteExtension`, which the extension effector
would load into `adapter.Effect.Interaction` as it does for a response, with the
interaction's window and authorization rechecked at the attempt. It is not made:
the effects above work without it.

## Permissions

- **Provider.** Credential purposes, resolved through `ports.Credentials`
  and always as a secret: `slack.chat.api` (the bot token, for the Web API),
  `slack.chat.signing` (the app's signing secret, for request verification)
  and, for Socket Mode only, `slack.chat.socket` (an app-level token, `xapp-`,
  with the `connections:write` scope, for `apps.connections.open`; the
  descriptor's permissions are the bot's scopes and do not list it). Both
  tokens are only sent to the channel's `api_url`. Scopes the descriptor
  lists: `app_mentions:read`, `channels:history`, `channels:manage`,
  `channels:read`, `chat:write`, `groups:history`, `groups:read`,
  `groups:write`, `im:history`, `im:write`, `mpim:history`, `reactions:read`
  and `reactions:write`, with the event subscriptions `message.channels`,
  `message.groups`, `message.im`, `message.mpim`, `app_mention`,
  `reaction_added`, `reaction_removed`, `channel_archive`,
  `channel_unarchive`, `group_archive` and `group_unarchive`.
  `channels:manage` and `groups:write` archive and unarchive a public and a
  private channel; `channels:read` and `groups:read` are what
  `conversations.info` (reconciliation) needs for a public and a private
  channel. The archive events need no scope of their own.
- **uma.** The receiving principal needs `channel.receive`; replies are authorized as `message.reply`, starts as `conversation.start`, and edits, deletions and reactions as `message.edit`, `message.delete` and `message.react`; the view effects as `extension.execute`; an archive or unarchive as `conversation.state_shared`, the action for the scope the adapter reports.

The view methods list no scope ("No scopes required") and need the app's interactivity turned on, which the interactions above need too. They are Tier 4, 100 or more a minute.

## Configuration

`slack.chat.config` version 1, as JSON in the channel configuration. Unknown
fields are refused, so a secret put there by mistake is caught.

| Field | Required | Meaning |
| --- | --- | --- |
| `team_id` | yes | The workspace ID, such as `T0123`. Requests naming another team are acknowledged and dropped. It is the destination of every envelope: bind the inbox to it |
| `bot_user_id` | yes | The app's bot user, such as `U0123`. What the adapter never reports back and what a mention is |
| `bot_id` | no | The app's bot, such as `B0123`, for legacy `bot_message` events that name a bot and no user |
| `app_id` | no | Restricts requests to this app (`api_app_id`) |
| `api_url` | no | The Web API root; default `https://slack.com/api/`. https without user info or query |
| `mentions_only` | no | Report a channel or group message only when it mentions the bot; direct messages always |
| `history_channels` | no | Up to 10 conversation IDs a channel-wide history read covers |
| `evaluate_interactions` | no | Put a modal submission and an external select's option request to the host's interaction validator inside Slack's three seconds; see [Answering inside the request](#answering-inside-the-request). Default off |
| `webhook_tolerance_seconds` | no | Replay window of signed requests, 1 to 300; default 300 |
| `receive` | no | How the app takes its payloads: `events_api` (the request URL, `receive_http`) or `socket_mode` (`receive`); empty offers both. See [Choosing how to receive](#choosing-how-to-receive) |

Host options (`slack.Options`): `ResponseBaseURL`, the origin interaction
answers go to (default `https://hooks.slack.com`), and `DebugReconnects`, which
appends `debug_reconnects=true` to every Socket Mode connection URL so Slack
ends each connection after 360 seconds: a way to watch the receiver refresh,
not for production.

## Receiving

The handler reads the body and verifies `X-Slack-Signature` before anything
else: `v0=` and the hex HMAC-SHA256, keyed with the signing secret, of
`v0:`, the `X-Slack-Request-Timestamp` and the raw body
([Verifying requests from Slack](https://docs.slack.dev/authentication/verifying-requests-from-slack/)).
The comparison is constant-time and the timestamp must be within the replay
window either way. A request that fails either check gets 401, and a
`url_verification` request that fails them gets no challenge back; a verified
one is answered with the challenge as plain text.

One request URL takes everything Slack sends the app. The rules for events:

| Slack sends | uma reports |
| --- | --- |
| `message` in a channel, group, group direct message or direct message | `message.received`: a message envelope from `slack.chat.user`, scoped by the room and by the thread: the one a reply is in, else the one the message roots |
| `app_mention` | The same message. Slack sends it and the `message` event of one message under two event IDs; the adapter derives one event ID from the message, so core sees one |
| A message by the app's own bot | Nothing: it would loop, and uma knows what it sent by its provider message IDs |
| A message by another bot | A `message.automated` envelope from a bot sender |
| `message_changed` carrying `edited` | `message.updated`: an edit by the editor. One without `edited` is an unfurl or thread bookkeeping and is dropped. An edit of a message uma never saw records the message instead |
| `message_deleted` | `message.updated`: a deletion by a system actor, because Slack does not name the deleter |
| `reaction_added`, `reaction_removed` on a message | `message.updated`: a reaction. The bot's own is dropped; so is one by a name with no Unicode emoji |
| `channel_archive`, `channel_unarchive`, `group_archive`, `group_unarchive` | A `conversation.state` envelope by a system actor: the channel's room as the conversation, `archived` true or false, complete, observed at the event's `event_ts`. It creates no message or contact; see [Archiving](#archiving) |
| A message with files and no text | One structured part (`slack.chat.files`) with file IDs, names, types and sizes; the files are not fetched |
| A button or select (`block_actions`), a slash command, a modal submission (`view_submission`) | `interaction.received`; see [Interactions](#interactions) |
| The options request of an external select (`block_suggestion`) | Nothing recorded: answered with options or an empty list; see [Answering inside the request](#answering-inside-the-request) |
| Other subtypes, other events, other teams or apps | Acknowledged, nothing reported |

A message ID is `<conversation ID>:<ts>`. Slack's `ts` is unique only per
conversation, so the conversation is part of the identity. Text keeps Slack's
own encoding: `&`, `<` and `>` as entities, mentions and links as `<...>`.
The payload of every message (`slack.chat.message`, version 1) names the team,
conversation, `ts`, `thread_ts` for a reply, the event type, subtype, bot ID
and whether the bot was mentioned. It is a claim, never an identity.

An event whose sink answer is retryable gets 503, so Slack retries it (up to
three times, with `X-Slack-Retry-Num`); any other refusal gets 422 and
`X-Slack-No-Retry: 1`. A retried delivery is a duplicate to core and is
acknowledged.

### Choosing how to receive

"When you toggle Socket Mode on, you'll only receive events and interactive
payloads over your WebSocket connections—not over HTTP"
([Using Socket Mode](https://docs.slack.dev/apis/events-api/using-socket-mode/)),
and the switch is in the app's settings, so the channel's `receive` says which
way a host takes:

| Configuration `receive` | Capability `receive` (`Runtime.Receive`) | Capability `receive_http` (`Runtime.Webhook`) |
| --- | --- | --- |
| `socket_mode` | available | unavailable, `disabled` |
| `events_api` | unavailable, `disabled` | available |
| empty | available | available: the host picks one |

The descriptor declares both for every channel, as it is the adapter type's; the
channel's choice is in the capability statuses its instance reports, and in
`Configuration.Capabilities`. The engine checks them first: `Runtime.Receive` on
an `events_api` channel and `Runtime.Webhook` on a `socket_mode` channel fail
`unsupported` before the adapter makes any call. Called directly, the instance
refuses too: `Instance.Run` returns `disabled` without a call, and the handler of
a `socket_mode` channel answers every request 404 and reads nothing. The
adapter never turns Socket Mode on or off; that is the app owner's setting.

### Socket Mode

Experimental. The receiver follows [Using Socket Mode](https://docs.slack.dev/apis/events-api/using-socket-mode/)
and [`apps.connections.open`](https://docs.slack.dev/reference/methods/apps.connections.open/):

1. `apps.connections.open` with the app-level token "in the Authorization
   header, not as a parameter" answers `{"ok": true, "url": "wss://..."}`. The
   URL "refreshes regularly", so every connection asks for a new one. Only a
   `wss` URL with a host and no user info is dialled, through the host's HTTP
   client, following no redirect. The client must return the body of the `101`
   answer writable, as `net/http` does.
2. Slack says `hello` (with `num_connections` and `debug_info`). A connection
   that says nothing within ten seconds is given up.
3. Each envelope (`envelope_id`, `type`, `payload`, `accepts_response_payload`)
   goes through the parsing of the request URL: an `events_api` payload is the
   Events API delivery, "identical to the typical Events API payloads"; an
   `interactive` one is the interaction payload; a `slash_commands` one holds
   the command's form fields as a JSON object. The bot, mention, team, app and
   duplicate rules above apply unchanged. There is no signature: Slack says
   "there's no need to verify or validate inbound events, because you're
   receiving the events over a pre-authenticated WebSocket", and the signing
   secret is never resolved.
4. The acknowledgement `{"envelope_id": ..., "payload": ...}` is sent once the
   sink recorded the envelope. Its payload is what the request URL would have
   answered in its body, when the envelope accepts one: the options of an
   options request, the field errors of a modal submission. Nothing else gets a
   payload, so a slash command shows nothing.

| The request URL would answer | Over the socket |
| --- | --- |
| 2xx | acknowledged |
| 503: a retryable sink refusal | not acknowledged; the connection ends `unavailable` and the engine reconnects after its backoff. Slack delivers the envelope again |
| 422: a denied ingress (`unauthorized`) | not acknowledged; the receiver ends with it and is not reconnected |
| 422: any other sink refusal | not acknowledged; the connection stays up, so one envelope cannot stop the channel. Slack cannot be told not to retry over the socket: it delivers the envelope again a few times, and each is refused alike |
| 400, or a failed evaluation (403, 503, 504, 422) | not acknowledged: Slack retries an event, and shows its user an error for an interaction, as for a non-2xx answer |

| Slack sends | The receiver |
| --- | --- |
| `disconnect` `warning` ("about 10 seconds before the disconnect") or `refresh_requested` | opens the next connection while the old one still delivers, and closes the old one once the new one said `hello`. This happens inside `Runtime.Receive`; the engine sees no failure, so a refresh every few hours never counts against the retry budget |
| `disconnect` `link_disabled` (Socket Mode turned off) | ends with `disabled`, not reconnected |
| `disconnect` of another reason | ends `unavailable` naming the reason; the engine reconnects after its backoff |
| a connection closed unasked, no `hello`, no pong | ends `unavailable`; the engine reconnects |

The receiver pings every 30 seconds and gives a connection ten seconds to
answer: "Upon receipt of a Ping frame, an endpoint MUST send a Pong frame in
response" ([RFC 6455](https://www.rfc-editor.org/rfc/rfc6455#section-5.5.2)),
so a connection a NAT dropped is noticed. A rate-limited `apps.connections.open`
(429 with `Retry-After`, Tier 3) is waited out; `invalid_auth` or
`token_expired` resolves the token again once, fresh, then ends `unauthorized`,
as do the other refusals of the token (`not_authed`, `account_inactive`,
`not_allowed_token_type`, ...); `internal_error` and server errors are
retryable. One message is at most 1 MiB, the engine's default webhook body
limit.

Envelopes are handled in lanes, one per conversation: the Slack conversation
an event names (`channel`, or the `item.channel` of a reaction), an
interaction names (`channel.id`, `container.channel_id`) or a slash command
names (`channel_id`); a modal submission with none goes in a lane of its view,
and anything else in one lane of its own. A lane's envelopes are recorded one
at a time, in the order they were read, so an edit, a deletion or a reaction
is never recorded ahead of the message it changes (a deletion or reaction of a
message the sink does not hold yet is dropped). Up to 8 lanes are handled at
once, and each acknowledgement is written as soon as its record completes, so
a slow record holds back only its own conversation's acknowledgements, not
the others' past Slack's three seconds. At most 64 envelopes are held, waiting
or being recorded; past that the receiver stops reading until one is
answered. Acknowledgements are whole WebSocket messages, one at a time. A
connection a refresh replaced is closed once the envelopes it delivered are
answered. A stop cancels what is being recorded, acknowledges none of it, and
returns once every handler has ended.

The request URL keeps no order: its requests are served concurrently, and
Slack documents no order for them, so the lanes keep more than it does. Across
connections nothing is ordered either, since "each payload may be sent to any
of the connections".

**The link.** The instance implements `adapter.ReceiverReporter`: it reports
connected at each `hello`, not connected when the connection that said the last
one closes (a replacement's `hello` reports connected again) and when `Run`
returns. A refresh whose new connection says `hello` before the old one closes
leaves it connected throughout. So
`Runtime.ReceiverStatus` reads `connecting` from the first dial until the
first `hello`, and `connected` while a connection stands. The report is
advisory.

Every `hello` reports two observations to the host's observer, neither naming
the app or a connection:

| Observation | Kind | Meaning |
| --- | --- | --- |
| `slack.chat.socket_connections` | gauge, `Value` = `num_connections` | The app's open Socket Mode connections as Slack counts them. 1 for uma alone; 2 at the `hello` of a refresh, the replaced connection still open |
| `slack.chat.socket_shared` | count | The `hello` counted more connections than the receiver holds (the new one and the one it replaces): another consumer of the same app-level token, such as another deployment or a Bolt app, holds a connection |

Slack spreads payloads over every open connection ("each payload may be sent
to any of the connections"), so a shared token means uma never sees a share of
the envelopes: alert on `slack.chat.socket_shared`. It does not change the
channel's capabilities or stop the receiver: Slack allows an app 10
connections on purpose, the other consumer may be intended, and the receiver
still records everything it is sent.

Slack keeps no cursor for the receiver: what was not acknowledged is Slack's to
deliver again, and core's duplicate check absorbs what arrives twice. A stop
between the record and the acknowledgement is such a case. `Runtime.Receive`
holds the channel's receiver lease, so one process per store receives a channel.

## Conversations

Every message and interaction names the conversation it belongs to, so one channel serves
the three selections an inbox may use:

| Selection | A conversation is | Its reply route |
| --- | --- | --- |
| per room | a Slack conversation (a channel, a group, a direct message) | the room |
| per thread | one thread of a room: a message outside a thread roots one, and its replies join it | the thread, which a send posts into with `thread_ts` |
| per contact | one user | the user: replies and starts go to the user's direct message, whatever conversation the user wrote in |

The inbox is bound to the workspace's `team_id`, not to a conversation: rooms and
threads are what separate conversations inside it.

## Interactions

Slack sends an interaction to the same request URL, as a form with a `payload`
field, and a slash command as a form with a `command` field. Both are signed and
verified like events. The sink records the interaction first, and only then does
the handler answer 200 with an empty body, inside Slack's three seconds: a body
would be shown to the user as a message, and a modal takes an empty 200 to close.
The interaction is never a message. A modal submission and an option request can
instead be put to the host first; see
[Answering inside the request](#answering-inside-the-request).

| Slack sends | Interaction |
| --- | --- |
| `block_actions`: a button or select | kind `action`; `Action` is the `action_id`, `Arguments` the button's `value` or the select's `selected_option.value`; `Message` is the message the control sits on (none on an ephemeral message, which has no `ts` to name); `Context` the room and the thread (the one the message is in, else the one it roots) |
| A slash command | kind `command`; `Command` is the command without its slash, `Arguments` the text after it; `Context` the room, and no thread: a per-thread inbox refuses a command |
| `view_submission`: a modal | kind `submission`; `Fields` named `slack.chat.view.<callback_id>` (the callback lowercased, other characters `_`) version 1, data `callback_id`, `view_id`, `private_metadata` and `values` (the view's `state.values` as Slack sent it). Context is the room of the view's `response_urls` when it has one; a view with none has no room, which a per-room or per-thread inbox refuses (`422`) |
| `block_suggestion`: an external select asking for options | not an interaction record: evaluated and answered in the request; see [Answering inside the request](#answering-inside-the-request) |
| `view_closed`, shortcuts, message actions | acknowledged, not consumed |

The trigger_id is also what opens a modal: see [Modals](#modals).

The actor is the signed payload's user, never a field the body claims; the bot is the
app's bot user. The interaction ID is the `trigger_id` (`action:<id>`,
`command:<id>`), or for a modal the view and the submission's `trigger_id`
(`view:<view id>:<trigger id>`). Each submit is an interaction with a trigger
of its own ("Triggers may only be used once"), so a modal submitted again after
field errors is another interaction, while the same submission delivered
again (a retried request, or an envelope Slack sends again) has the same ID and
is a duplicate to core. The reference's `view_submission` example shows no
`trigger_id`; Slack's Bolt SDK types it as a required field of the payload. A
payload without a `trigger_id` (a button's, a command's or a submission's) is
named by a digest of its bytes instead. The deadline is 3 seconds and the
respond window 30 minutes from the request's timestamp.

The `response_url` is kept as its path alone, as the interaction's response token,
and only when it is `https` on the host setting `ResponseBaseURL` (default
`hooks.slack.com`) without user info, query or traversal: a payload can never point
an answer anywhere else. It is stored beside the interaction and never read back.

### Answering inside the request

Slack takes the answer to a modal submission or to an options request from the
HTTP response to the request that carried it
([modals](https://docs.slack.dev/surfaces/modals/),
[external select menus](https://docs.slack.dev/reference/block-kit/block-elements/select-menu-element/));
`response_url` cannot carry it later. uma's own answer path is asynchronous
(`Respond`, after the interaction is recorded), so the one synchronous slot the
contract has is the host's interaction validator: the engine's
`adapter.Sink.EvaluateInteraction`, read-only, bounded by the interaction's
deadline. With `evaluate_interactions` the adapter asks it before it answers.

| Slack sends | The validator is asked | Slack is answered |
| --- | --- | --- |
| `view_submission` | kind `submission`, the same input the record would hold but without the response token | no field errors: the submission is recorded, then an empty `200`, which closes the view. Field errors: `200` `{"response_action":"errors","errors":{...}}`, the decision's map keyed by the input block's `block_id`; the modal stays open and **nothing is recorded**, because the submission did not happen. A resubmission is a new request |
| `block_suggestion` | kind `options`; `Action` the `action_id`, `Arguments` the text typed so far, `Context` the room of a message's menu; `Fields` named `slack.chat.suggestion` version 1, data `action_id`, `block_id`, `value` and, for a menu in a modal, `view_id`, `callback_id`, `private_metadata` and `values` | the decision's `Options` as the body, `application/json`; none, or the setting off: `{"options":[]}` |

The decision's `Options` is an extension named `slack.chat.options` version 1
whose data is the object of Slack's answer: `{"options":[...]}` or
`{"option_groups":[...]}`, never both. An option is `text` (`plain_text`, 1 to 75
characters), `value` (1 to 150) and optionally `description` (`plain_text`, up
to 75); a group is a `label` (as `text`) and its options. At most 100 options,
or 100 groups of at most 100 options each. A list outside those limits, a field
beyond those, or another schema is refused before Slack sees it, as `422`. A
decision's field errors on an options request, and its options on a submission,
are ignored.

The adapter fails closed. A submission is never recorded without its
evaluation, so no validator configured (the engine denies) is `403`, an
unavailable validator `503`, a deadline passed `504` and any other refusal, or a
field-error map with an empty or over-255-byte key or a blank message, `422`;
Slack then tells its user that the app had trouble and the modal stays open.
With the setting off nothing is asked and a submission is recorded and
acknowledged as before, which is why a host with no validator must leave it off.

The validator has until the request's deadline, three seconds after the
timestamp Slack signed. Slack states the three seconds for a submission, not
for an options request; the adapter bounds both the same.

What stays out, and why:

- **`response_action` `update`, `push` and `clear`.** They answer with a view
  (or close the whole stack), and `model.InteractionDecision` has only field
  errors and options: there is nothing to turn into one, and the answer cannot be
  sent later. An empty `200` closes the submitted view only, which is not
  `clear`. The smallest contract change is a provider-defined answer on the
  decision (an extension the adapter validates, such as a Slack view and its
  action), not a new kind.
- **`view_closed`** (sent only for a view opened with `notify_on_close`): the
  interaction kinds are closed (`command`, `action`, `submission`, `options`) and
  a dismissal is none of them; calling it an action would invent a control the
  user pressed. Acknowledged, not reported.
- **A decision from the host after the acknowledgement.** Slack accepts none:
  field errors and options have to be in the response. A host that cannot decide
  in three seconds can only record the submission and correct it afterwards with
  `Respond`.
- **Checking an error key against the view.** Slack documents no behavior for a
  key that names no input block, and no limit for a message; the adapter checks
  neither.

### Answering

`Interactions.Respond` posts JSON to the interaction's `response_url`: the origin is the host setting, the path the stored token. The bot token is never sent there and a response resolves no credential.

| Mode | Audience | Posted as |
| --- | --- | --- |
| `message` | public | `response_type: in_channel`, `replace_original: false`, in the thread when the conversation's route is one |
| `ephemeral` | restricted | `response_type: ephemeral`: only the actor sees it |
| `update` | public | `replace_original: true`, on the message the control sits on, which must be the effect's target. Not for a slash command, which has no message of its own to replace: `unsupported` |

Content is as for a send: literal text (entities, `mrkdwn: false`) and optionally one `slack.chat.blocks` part. A response is `rejected` before any call when the interaction has no response URL (a modal without `response_urls`), the token is not a URL path, the window has passed (`deadline_expired`), the audience does not fit the mode, or the content cannot be carried. Slack's refusal after the call is final: `400` `validation`, `403` `unauthorized`, `404` or `410` `not_found` (a URL that is gone, used five times, or past its thirty minutes), `429` `rate_limited` with `Retry-After`, a 5xx or a lost answer `outcome_unknown`.

Slack documents no error body or status list for `response_url`; the mapping follows the incoming webhooks page. A `response_url` answers `ok` and names no message, so a message response has no provider message ID, and a response whose answer was lost cannot be reconciled: it stays `outcome_unknown` and is never sent again. Whether `mrkdwn: false` is honoured by a `response_url` is not documented.

## Sending

A route is where a message lands, and what the adapter stores as a conversation's
reply route:

| Route | Reference | Lands |
| --- | --- | --- |
| Room | namespace `slack.chat`, kind `room`, ID the conversation (`C...`, `G...`, `D...`) | a top-level message |
| Thread | kind `thread`, ID the parent `ts`, parent ID the conversation | a reply in that thread |
| User | namespace `slack.chat.user`, kind `user`, ID the user (`U...`) | the user's direct message: `conversations.open` opens or resumes it, then the message is posted there |

A reply that names the message it answers lands in that message's thread: the
thread of the answered message when it is itself a reply (Slack: "Avoid using a
reply's `ts` value; use its parent instead"), else the message.

Content is text parts, joined by a blank line, and at most one structured part
`slack.chat.blocks` version 1 whose data is the JSON array of Block Kit blocks;
the text then travels as the blocks' fallback. Text is literal: `&`, `<` and `>`
are sent as entities and `mrkdwn` is off, so a message cannot mention or format
through text. Put that in blocks. Attachments and other structured parts are
rejected `unsupported` before any call, as are text over 40000 characters
(Slack would truncate it) and any audience but public.

The operation succeeds with the provider message `<conversation>:<ts>` the
answer names. Each send carries its operation and attempt as message metadata
(`event_type` `uma_attempt`). Slack documents that an app registers metadata
schemas in its manifest before it sends metadata; a message whose metadata
Slack refuses (`invalid_metadata_schema`, `metadata_must_be_sent_from_app`) is
sent again without it, and later sends skip it.

Slack reports a refusal in the body, `"ok": false` and an `error` code (its reference says to check `ok` on every answer; it states no HTTP status for a refusal, so the adapter reads the body of a `200` and treats any other status as a failure of the call). A code of the effect being
refused (a missing conversation, no right to post, text or blocks Slack does not
accept) settles the operation `rejected`; a rate limit is `rate_limited` with the
`Retry-After` wait; `internal_error`, `fatal_error`, a 5xx and a lost answer
are `outcome_unknown`; a connection that was never made is `transient`. A
refused token is resolved again once, fresh.

## Editing, deleting and reacting

The target of each is a message of the channel, `<conversation>:<ts>`, sent through
uma or not; Slack answers for what the bot may change. A bot token updates and
deletes only the messages its bot posted, so another author's message is
rejected `unauthorized` (`cant_update_message`, `cant_delete_message`), a message
that is gone `not_found`, and neither ever becomes a new message. `chat.update`
takes the content as the whole message: text without blocks drops the old blocks.
The reference for `chat.update` tells a bot token to pass `as_user` when it edits
its own message; that is a classic app's rule (a workspace app gets
`as_user_not_supported`), so the adapter does not send it. That is unverified
against a real workspace.

Slack names a reaction, not an emoji (`thumbsup`, not 👍). The adapter has a table
of 26 common emoji; any other emoji is rejected `unsupported` before a call.
Adding a reaction the bot holds, and removing one it does not, is applied, as
`already_reacted` and `no_reaction` say that is what was asked. A user may hold
several reactions on a message, so nothing is replaced.

## Archiving

`Conversations.Archive` on a room conversation calls `conversations.archive`
(or `conversations.unarchive` with `Undo`) on the channel of the conversation's
route. Archiving is channel-level: it changes the channel for everyone, so the
adapter reports scope `shared` (`uma.state.limits`) and the engine authorizes it
as `conversation.state_shared`. A host can grant mailbox tidying elsewhere and
withhold this. A channel is one object, so the request's list of messages is not
used. The bot token needs `channels:manage` for a public and `groups:write` for
a private channel ([`conversations.archive`](https://docs.slack.dev/reference/methods/conversations.archive/)).

| Case | Outcome |
| --- | --- |
| Archive an archived channel (`already_archived`), unarchive an open one (`not_archived`) | applied: that is what was asked |
| A thread or a direct message as the conversation | `rejected`, `unsupported`, no call: only a channel is archived, and archiving a thread's channel would archive every other conversation in it |
| `cant_archive_general`, `method_not_supported_for_channel_type` | `rejected`, `unsupported` |
| `restricted_action`, `missing_scope`, `not_in_channel` | `rejected`, `unauthorized` |
| `channel_not_found` | `rejected`, `not_found` |
| A lost answer | `outcome_unknown`; reconciled by reading `is_archived` ([Reconciling](#reconciling)) |

An archived channel refuses new messages (`is_archived`, a rejected
`unauthorized` send), and archiving never changes the conversation's status:
resolving and archiving are separate.

The adapter only watches for incoming events, and follows archive state only for
the kinds of conversation it already receives messages from. It receives public
and private channel messages (`message.channels`, `message.groups`), so it
follows both: `channel_archive` and `channel_unarchive` for a public channel,
`group_archive` and `group_unarchive` for a private one. Group direct messages
and direct messages have no archive events, so nothing is followed for them.

The four events report the same state
whoever changed it, including the bot, as a `conversation.state` envelope with
the channel's room as the conversation: in a per-room inbox that selects the
conversation by its room. In a per-thread or per-contact inbox a channel is not
the conversation's scope, so the report resolves through the room reference
stored on messages: the one conversation holding the channel's newest stored
message takes the state, not every thread or user of the channel, and a channel
with no stored message records nothing. The echo of uma's own archive settles the operation as evidence and
applies the state once. Unverified against a real
workspace: that a bot token can unarchive (an archived channel removes its
members, the bot included).

## Reconciling

A send, edit, delete or reaction whose answer was lost is `outcome_unknown` and is
never resent before the adapter has read Slack.

| Attempt | Slack is read with | Applied when |
| --- | --- | --- |
| Send | `conversations.history` (a room or direct message) or `conversations.replies` (a thread, or the thread of the message answered), from the attempt on, with `include_all_metadata` | a message of the bot carries the attempt's metadata; or, with no metadata, a message of the bot posted after the attempt has the text and, if the attempt had blocks, has blocks and carries no other attempt's marker. The earliest such message |
| Edit | `conversations.replies` bounded to the message's `ts` | the message carries the attempt's metadata, or its text is the replacement's |
| Delete | the same | the message is gone |
| Reaction | `reactions.get` with `full` | the bot holds the reaction (a removal: does not) |
| Archive | `conversations.info`, the channel's `is_archived` | it is what the attempt asked for: archived, or for an unarchive not archived |

Not applied is `failed` with `not_found`, so a retry is safe. A listing that is
not read to its end (10 pages of 200) is inconclusive: the operation stays
`outcome_unknown`. A message that is gone is a rejected edit or reaction,
`not_found`. A read Slack refuses is the call's error, never "not applied": that
would invite a resend.

The attempt metadata is the exact signal, and Slack documents that an app registers
its metadata schemas in its manifest before it sends them. Where it does not,
Slack ignores or refuses the metadata and the text match stands in; two identical
messages by the bot after one attempt cannot be told apart, and the earliest is
taken. `conversations.history` also needs the bot in the conversation, and
Slack limits it to one request per minute for apps not in the Marketplace
installed from 29 May 2025 on, with 15 messages per page; a rate limit is
waited out, not read as "not found".

## History

`Runtime.Import` reads past messages oldest first, as the webhook reports them,
each once: a message the engine holds already, which includes every one uma
sent, is skipped.

| Route | Read with |
| --- | --- |
| A room | `conversations.history`: top-level messages and thread broadcasts |
| A user | `conversations.open` with `prevent_creation` to find the direct message, then as a room. A user with no direct message has an empty window, and none is created |
| A thread | `conversations.replies`: the parent, then the replies, with Slack's own cursor |
| None (channel-wide) | the conversations in `history_channels`, merged by time and then conversation; without any it is `disabled` |

`conversations.history` lists newest first, so a page does not read through the
window. It asks for the range after the cursor, a few hours wide at first,
doubling it while it holds fewer messages than the page needs and halving it
while Slack holds more than 1000 in it, and keeps the oldest. A cursor names its
read: one issued for another route, channels or window start is `validation`.

The page reports a gap when a second holds more than a read takes, and when a
room's message heads a thread (a `thread_ts` equal to its `ts`, or a reply
count): the replies are not in the room's history, so read the thread route for
them. A thread's answer first holds its parent, whatever the range says of it;
the adapter drops a parent older than the window. Slack documents that apps outside the Marketplace
installed from 29 May 2025 get one request per minute and 15 messages per page
from these methods; a rate limit is waited out as `rate_limited`, so such an
app reads slowly. The bot must be in the conversation; the scopes are
`channels:history`, `groups:history`, `im:history` and `mpim:history`.

## Limits

| What | Limit |
| --- | --- |
| Replay window | 5 minutes either way (configurable down) |
| Acknowledgement | Slack wants a 2xx within 3 seconds; an evaluation, when configured, runs inside it. Socket Mode states no deadline; the adapter holds an acknowledgement to the same 3 seconds |
| Socket Mode | One message 1 MiB; a connection pinged every 30 seconds, 10 seconds for a pong or a `hello`; 8 conversations recorded at once, 64 envelopes held; Slack allows an app 10 connections |
| Options of an external select | 100 options, or 100 groups of 100; text and label 75 characters, value 150 |
| A `trigger_id` | 3 seconds, one use |
| A modal | 3 views on its stack; a view 250 KiB, 100 blocks, a title of 24 characters |
| Text of one message | 40000 characters |
| Rate | Slack allows about one message per second per conversation |
| Reaction names | A built-in table of 26 common emoji; other names, custom emoji and skin tones are dropped |

## How it is tested

The shared adapter suite (`conformance.RunAdapterSuite`) runs three times over a
scripted Slack workspace, once for each way a workspace is divided into
conversations: per room, per thread and per contact (a direct message with each
user). The scripted workspace answers the documented shapes of the Web API
methods and the `response_url`, and signs the events and forms it posts the way
Slack does. The view effects pass the suite's extension cases, with an action of
`slack.chat.views.open` whose `trigger_id` the scripted workspace accepts once
and ends three seconds after it was sent, and an `InvalidAction` the factory's
`ValidateAction` refuses at the call. Package tests cover the signature (with the worked example of
[Verifying requests from Slack](https://docs.slack.dev/authentication/verifying-requests-from-slack/)
as a vector, replay window included), the Events API rules, interactions,
reconciliation, history and the response modes.

The suite runs once per transport: as the request URL alone (the descriptor it
is given leaves out `receive`) and as the Socket Mode receiver, over a scripted
WebSocket server (`httptest` with TLS) that answers `apps.connections.open`,
says `hello` and sends the suite's events as envelopes, each counted
acknowledged when its acknowledgement arrives. Both transports pass the same
cases and skip the same ones. Package tests drive the receiver against that
server directly: the acknowledgement only after the record (a sink held open
gets none until it returns), within the deadline, with the options and field
errors as payloads; a slow record holding back only its own conversation, the
order of one conversation kept (a reaction waits for its message), at most 8
records at once, a stop waiting for the records in progress, and a replaced
connection kept open until it answered what it delivered; the observations of
`num_connections`, across a refresh and for a shared token; refusals left
unacknowledged, a retryable one and a denial
ending the connection; `refresh_requested` and `warning` refreshing inside
`Run`, with the old connection still delivering until the new one's `hello`
and closing without failure if Slack closes it first; `link_disabled`, other
reasons, an unasked close, a missing `hello` and a missing pong; credential
failures, a refused token resolved fresh once, a rate limit waited out, a URL
that is not `wss`; `debug_reconnects`; and the stop with the context. A modal
submission delivered again has one ID at the request URL and over the socket,
and a second submit of the same view another. An engine
test checks that the capability statuses follow `receive` and that
`Runtime.Receive` connects a `socket_mode` channel and refuses an `events_api`
one.

The suite skips what the scripted workspace cannot express: a revoked
permission (Slack's scopes show only in its answers), an echo of uma's own edit (the adapter reads none of Slack's `message_metadata_*` events), a reaction summary, a message only
some see, an interaction whose deadline has passed (it is always three seconds
after the request), a modal submission (its answer is covered by package tests with a scripted validator), and, per thread, history (a thread's
window starts with a parent message the suite does not hold) and an interaction
with no message to name a thread.

The suite's `State` group runs for `archive` per room only: a thread or a direct
message is not archivable, so the per-thread and per-contact runs omit it, and
`TestArchiveRefusesThreadAndDirect` covers the refusal. Its inbound cases skip:
the suite seeds the conversation and sends into it, and the engine selects a room
report only through the room's inbound candidates, so the scripted workspace
cannot make a report find that conversation. Package tests (`archive_test.go`)
cover the calls, the refusals, reconciliation, the declared scope and the events
and their shape.

The recorded tests (`TestRecordedWorkspace`, with `TestRecordedArchiveEvents` for
the two events) replay committed cassettes and captured event bodies, and
**none are recorded yet**, so they skip. Behavior against a real workspace is
unverified. To record, follow [Record Slack cassettes](../recording/slack.md).

## Known limits

- A message cannot mention or format through text parts; use blocks.
- `prevent_creation` is documented without its answer for a user who has no direct message; the adapter reads any not-found answer as an empty window. Unverified against a real workspace.
- Reactions cover 26 emoji; custom emoji and skin tones have no mapping.
- Reconciliation does not read a message edited or deleted after the attempt by someone else: it reports what Slack shows now.
- A reconciliation of a reply reads `conversations.replies` with the reply's `ts`, which the reference allows ("either a thread's parent message or a message in the thread") without saying what comes back; the adapter keeps the message of that `ts`. Unverified against a real workspace.
- One audience: public. A message only one member sees is `send_ephemeral`, not built.
- Edits of messages by bots are reported only when Slack marks them `edited`.
- The view effects need the `trigger_id` within three seconds of Slack's request, through the host's own handler and a worker; uma bounds nothing of that path, and an attempt that is late is refused locally. Slack documents `views.open` with a `view` that is "a JSON-encoded string" and its modals guide with a JSON object in a JSON body; the adapter sends the object. Unverified against a real workspace, as is the 250 KiB reading of `view_too_large` and whether Slack counts the characters of a title as code points.
- Modal answers other than field errors (`response_action` `update`, `push`, `clear`) and `view_closed` are not offered; see [Answering inside the request](#answering-inside-the-request). With `evaluate_interactions` off a submission is recorded and closed with an empty `200`, and an options request gets an empty list.
- An answer from the host's validator is an answer of the moment: it is not recorded with the submission, and a modal answered with field errors leaves no record of the attempt.
- Socket Mode is experimental and **not recorded against Slack**: no cassette
  covers it (recording one is up to the app owner), so it is verified only
  against the scripted server built from the documentation.
- Socket Mode's page states no acknowledgement deadline; the adapter assumes
  the three seconds of the request URL. A record slower than that still delays
  the acknowledgements of its own conversation behind it, and Slack may
  deliver them again; duplicates are absorbed. Eight slow conversations at
  once hold back the rest the same way.
- Slack documents `num_connections` only by example, not whether the `hello`
  of a refresh counts the connection it replaces; the receiver assumes it
  does, as that connection is still open and still delivers. Were it not
  counted, a shared token would show in `slack.chat.socket_shared` at every
  `hello` but a refresh's. A shared token is reported, never prevented: uma
  cannot see the envelopes Slack sends the other consumer.
- The undocumented envelope fields `retry_attempt` and `retry_reason` (seen in
  Slack's SDKs) are not read; a redelivery is recognized by its event or
  interaction ID. Whether Slack redelivers an unacknowledged modal submission
  is not documented; if it does, the submission's `trigger_id` makes it a
  duplicate. That `view_submission` carries a `trigger_id` is Slack's SDK's
  word, not the reference's; a payload without one falls back to its digest,
  so two submits whose payloads are byte for byte the same would be one.
- Experimental: configuration may change ([Compatibility](../compatibility.md)).
