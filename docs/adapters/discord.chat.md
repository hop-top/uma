# discord.chat

`discord.chat` is the Discord adapter. One channel is one bot application: the
bot in every guild it was added to, and its direct messages. uma holds a
gateway connection for messages, edits, deletions, reactions and interactions,
or takes interactions at the application's Interactions Endpoint URL, and
answers through the REST API on the host's HTTP client.

| | |
| --- | --- |
| Key | `discord.chat` |
| Package | `hop.top/uma/adapter/chat/discord`, in the module of the same path |
| Capabilities | `receive`, `receive_http`, `send`, `start_conversation`, `edit`, `delete`, `react`, `membership`, `history`, `reconcile`, `refetch`, `send_ephemeral`, `signal`, `respond`, `respond_follow_up`, `archive` |
| Events | `message.received`, `message.updated`, `interaction.received` |
| Identity namespaces | `discord.chat.user` |
| Sender kinds | person, bot |
| Provider permissions | `VIEW_CHANNEL`, `SEND_MESSAGES`, `SEND_MESSAGES_IN_THREADS`, `READ_MESSAGE_HISTORY`, `ADD_REACTIONS`, `MANAGE_THREADS` |
| Message IDs and threading | assigned by Discord (channel ID and message snowflake); no email-style threading; selection per room (a channel), per thread (a thread channel) or per contact (a direct message with each user) |
| Reply parent | reported for a reply (message type 19): the referenced message; a forward or crosspost carries none |
| Interactions | reports and answers slash commands, buttons, selects and modal submissions ([interactions](../interactions.md)) |
| Maturity | experimental |

The matrix of all adapters is in [Adapters](README.md).

## The headline limit: message content is privileged

`MESSAGE_CONTENT` is a privileged gateway intent. Without it, Discord sends a
guild message's `content`, `embeds`, `attachments` and `components` empty,
except for messages that mention the bot, direct messages with the bot, the
bot's own messages and the target of a message command
([privileged intents](https://docs.discord.com/developers/events/gateway#privileged-intents)).
An application seen by fewer than 10,000 users turns it on in the Developer
Portal; past that it has 90 days to apply, must reapply every year, and a use
case that "can be accomplished through interactions or other means without
privileged intents" is denied: Discord's advice is slash commands, components
and modals
([privileged intent review](https://docs.discord.com/developers/gateway/getting-started-with-privileged-intent-review)).
Asking for an intent the application was not granted closes the gateway with
`4014`.

So the adapter is built mention-first: `mentions_only` defaults to on. The
receiver then identifies **without** `MESSAGE_CONTENT` and reports a guild
message only when it mentions the bot, every direct message, and every
interaction, which is exactly what Discord fills without the intent. Turn
`mentions_only` off only for an application granted the intent: the receiver
asks for it, and every guild message the bot can see is reported. A guild
message whose content Discord still withholds (an edit of a message that did
not mention the bot, say) has nothing to report and is dropped. A support desk
that reads free text in guild channels should plan on mentions, direct
messages or slash commands.

## Capabilities and operations

| Capability | Behavior |
| --- | --- |
| `receive` | `Runtime.Receive` runs the gateway receiver: messages, edits, deletions, reactions and interactions over a WebSocket; see [The gateway](#the-gateway) |
| `receive_http` | `Runtime.Webhook` takes Discord's signed requests at the Interactions Endpoint URL: slash commands, components and modal submissions. Plain messages never arrive there |
| `send` | `Messages.Reply` posts a message (Create Message) to the stored route: a channel, a thread, or a user's direct message |
| `start_conversation` | `Conversations.Start` posts the first message the same way; a user's direct message channel is opened with Create DM |
| `edit` | `Messages.Edit` replaces the content of a message the bot wrote (Edit Message) |
| `delete` | `Messages.Delete` deletes a message (Delete Message) |
| `react` | `Messages.React` adds or removes the bot's reaction (Create Reaction, Delete Own Reaction) |
| `membership` | `Conversations.AddParticipant` and `RemoveParticipant` add a user to the thread of a per-thread conversation or remove one (Add and Remove Thread Member) |
| `history` | `Runtime.Import` reads a window of past messages of a channel, a thread, a direct message, or the configured channels; see [History](#history) |
| `reconcile` | `Operations.Reconcile` and the worker read Discord for what a lost answer applied, never writing; see [Reconciling](#reconciling) |
| `refetch` | The engine reads a message again (Get Channel Message) when two observations tie on a revision |
| `send_ephemeral` | `Interactions.Ephemeral` naming an interaction posts a followup only the interaction's user sees |
| `signal` | `Interactions.Signal` of kind `typing` shows the bot typing for ten seconds (Trigger Typing Indicator) |
| `respond` | `Interactions.Respond` answers an interaction through its webhook; see [Answering](#answering) |
| `respond_follow_up` | Further responses as followup messages, for the token's fifteen minutes |
| `archive` | `Conversations.Archive` archives the thread of a per-thread conversation, or unarchives it (Modify Channel, `archived`); see [Archiving threads](#archiving-threads) |

## Not declared

Reason classes are defined in [Adapters](README.md#why-a-capability-is-not-declared).

| Capability | Reason | Why |
| --- | --- | --- |
| `reply_recipients` | `provider` | A Discord message goes to its channel or thread; there is no cc or blind recipient |
| `receipt` | `provider` | Discord has no read or delivery receipt for a message, and no call to send one; channels know only their `last_message_id` |
| `stream` | `engine` | Discord has no streaming primitive; core streams on this adapter by posting and editing (edit mode), which it declares |
| `work_item_act` | `provider` | Discord has no work items |
| `internal_note` | `provider` | Discord has no message that is part of a channel and private to its staff; notes stay in uma |
| `extension_execute` | `not built` | An effect such as Create Message with a modal response, or Start Thread from Message, could be shipped |
| `extension_query` | `not built` | No read is shipped; Get Current User (the bot behind the token) would be the first |
| `probe_permissions` | `not built` | Get Current User could check that the bot token is live; a channel's permissions need its guild member and overwrites |
| `move` | `provider` | A channel has no folders or labels |
| `read_state` | `provider` | Bots have no read state |
| `flag` | `not built` | Flag kinds are `star` and `important`; the nearest call, Pin Message, marks one message, not a conversation |
| `trash` | `provider` | A channel has no trash; deleting one is not undoable |
| `raw_read` | `not built` | Raw read is built for mail only; the provider's whole copy of a message is not read |

## Permissions

- **Provider.** Credential purposes, resolved through `ports.Credentials`
  and always as a secret: `discord.chat.api` (the bot token, sent as
  `Authorization: Bot <token>` to the channel's `api_url` and in the gateway's
  Identify and Resume) and, for the Interactions Endpoint URL only,
  `discord.chat.interactions` (the application's public key from the
  Developer Portal, hex). The public key is not a secret; it is a credential
  purpose rather than configuration so it rotates with the host's other
  credentials and a configuration never carries key material. The bot
  needs, in the channels it serves, `VIEW_CHANNEL`, `SEND_MESSAGES`,
  `SEND_MESSAGES_IN_THREADS`, `READ_MESSAGE_HISTORY` (replies, reactions,
  history and reconciliation all read history) and `ADD_REACTIONS`, and
  `MANAGE_THREADS` to remove thread members and to archive a thread (or to
  unarchive a locked one); deleting another author's message
  needs `MANAGE_MESSAGES`, which the descriptor does not list. Install it with
  the `bot` scope, which includes `applications.commands`.
- **uma.** The receiving principal needs `channel.receive`; replies are
  authorized as `message.reply`, starts as `conversation.start`, edits,
  deletions and reactions as `message.edit`, `message.delete` and
  `message.react`, membership as `conversation.membership`, ephemeral
  messages as `message.ephemeral`, signals as `conversation.signal`, responses
  as `interaction.respond`.

Every request carries `User-Agent: DiscordBot (https://github.com/hop-top/uma, 1)`,
the form Discord requires
([API reference](https://docs.discord.com/developers/reference)).

## Configuration

`discord.chat.config` version 1, as JSON in the channel configuration. Unknown
fields are refused, so a secret put there by mistake is caught.

| Field | Required | Meaning |
| --- | --- | --- |
| `application_id` | yes | The application, a snowflake. It is the destination of every envelope (bind the inbox to it) and the application interaction answers are sent as; an interaction of another application is refused |
| `bot_user_id` | yes | The bot user, a snowflake (Get Current User with the token). What the adapter never reports back and what a mention is. READY naming another user ends the receiver: the token is another bot's |
| `api_url` | no | The REST root; default `https://discord.com/api/v10/`. https without user info or query |
| `gateway_url` | no | The gateway when Get Gateway Bot names none; default `wss://gateway.discord.gg`. It also bounds which hosts a gateway URL may name: its own, or one of its parent domain |
| `mentions_only` | no | Default true: report a guild message only when it mentions the bot, and identify without `MESSAGE_CONTENT`. False asks for the intent and reports every guild message; see [the headline limit](#the-headline-limit-message-content-is-privileged) |
| `history_channels` | no | Up to 10 channel IDs a channel-wide history read covers |
| `webhook_tolerance_seconds` | no | Replay window of signed interaction requests, 1 to 300; default 300 |
| `receive` | no | `gateway` (`receive` only), `interactions` (`receive_http` only), or empty for both; see [Choosing how to receive](#choosing-how-to-receive) |

## Choosing how to receive

An application takes interactions "in two mutually-exclusive ways": over the
gateway by default, or at an Interactions Endpoint URL once one is set
([overview](https://docs.discord.com/developers/interactions/overview)).
Messages only ever arrive over the gateway: the separate Webhook Events URL
carries application and entitlement events, no `MESSAGE_CREATE`
([webhook events](https://docs.discord.com/developers/events/webhook-events)).

| Configuration `receive` | `receive` (`Runtime.Receive`) | `receive_http` (`Runtime.Webhook`) | Use when |
| --- | --- | --- | --- |
| `gateway` | available | unavailable, `disabled` | the application has no Interactions Endpoint URL: everything over the gateway |
| `interactions` | unavailable, `disabled` | available | only slash commands, buttons and modals matter, and no process may hold a WebSocket |
| empty | available | available | both run: the gateway for messages, the endpoint for interactions when the URL is set |

The descriptor declares both; the channel's choice is in the capability
statuses its instance reports. Called directly, the instance refuses too:
`Run` returns `disabled` without a call under `interactions`, and the handler
answers 404 without reading under `gateway`. The adapter never sets the
endpoint URL; that is the application owner's setting.

## The gateway

The receiver follows [Gateway](https://docs.discord.com/developers/events/gateway),
[Gateway Events](https://docs.discord.com/developers/events/gateway-events) and
the close codes of
[Opcodes and Status Codes](https://docs.discord.com/developers/topics/opcodes-and-status-codes):

1. Before each new session it calls Get Gateway Bot for the URL and the
   session start limit. Discord allows 1000 Identify calls a day, and past
   them "all active sessions for the app will be terminated, the bot token
   will be reset": with none remaining the receiver ends `rate_limited` until
   the reset and never identifies.
2. It dials the URL with `v=10&encoding=json` through the host's HTTP client,
   following no redirect, and waits ten seconds for Hello.
3. Identify: the token, the intents and `properties` (`os`, `browser`,
   `device`). The intents are `GUILD_MESSAGES`, `DIRECT_MESSAGES`,
   `GUILD_MESSAGE_REACTIONS` and `DIRECT_MESSAGE_REACTIONS`, and `GUILDS`
   (not privileged; it carries `THREAD_UPDATE`, which reports a thread's
   archive state), plus
   `MESSAGE_CONTENT` when `mentions_only` is off. READY gives the session ID
   and `resume_gateway_url`; a READY naming another user than `bot_user_id`
   ends the receiver `permanent`.
4. Heartbeats: the first after `heartbeat_interval` times a random fraction,
   then every interval, carrying the last sequence number (null before any).
   A beat whose predecessor got no Heartbeat ACK finds the connection zombied:
   the receiver drops it without a close frame and resumes. Discord asking
   for a beat (opcode 1) gets one at once, at most once a second, which keeps
   the connection far under Discord's 120 sends a minute.

| Discord sends | The receiver |
| --- | --- |
| Reconnect (op 7), Invalid Session with `d` true, a close with 4000-4002, 4005, 4008 or without a code, a missing ACK | resumes: dials `resume_gateway_url` and sends Resume with the session ID and last sequence number; Discord replays what was missed, then RESUMED |
| Invalid Session with `d` false, close 4003, 4007, 4009 | identifies again after a short random wait (one to five seconds) |
| close 4014 (disallowed intent) | ends `disabled`, never retried: grant `MESSAGE_CONTENT` or set `mentions_only` |
| close 4004 (authentication failed) | ends `unauthorized`, never retried |
| close 4010-4013 | ends `permanent`, never retried |
| three connections in a row failing before READY or RESUMED | ends `unavailable`; the engine reconnects after its backoff |

The receiver never closes with 1000 or 1001, which would invalidate the
session: a stop drops the connection, so the next `Runtime.Receive` resumes.

**The link.** The instance implements `adapter.ReceiverReporter`: it reports
connected when READY or RESUMED arrives and not connected when a connection
ends, whether it resumes next or `Run` returns, so `Runtime.ReceiverStatus`
reads `connecting` while the gateway is dialled, waiting for Hello or between
connections, and `connected` once the session is up. The three connections the
receiver may fail in a row inside `Run` show as `connecting`, not as
`retrying`. The report is advisory.

**The cursor.** The gateway has no acknowledgement; what a session dispatched
can be replayed by resuming it. Each ingress carries the receiver cursor: the
session ID, its resume URL and the highest sequence number below which every
dispatch was handled, the ingress's own included once all before it are. A
restarted receiver resumes from there, so a stop, a crash or a refused record
loses nothing Discord still holds for the session; what it replays twice is a
duplicate to core. A cursor naming a resume host outside the configured
gateway's domain is ignored and the receiver identifies afresh.

**Lanes.** `MESSAGE_CREATE`, `MESSAGE_UPDATE`, `MESSAGE_DELETE`,
`MESSAGE_REACTION_ADD`, `MESSAGE_REACTION_REMOVE`, `THREAD_UPDATE` and
`INTERACTION_CREATE` are handled in lanes, one per channel (a thread's own
dispatches in the thread's lane, with its messages): a channel's dispatches are recorded one at
a time, in order, so an edit, a deletion or a reaction is never recorded ahead
of its message. Up to 8 lanes run at once and 64 dispatches are held; past
that the receiver stops reading. An interaction has a lane of its own, so its
three seconds are never spent behind a channel's records. A refusal that is
retryable, or a denial, ends the receiver with it (the engine reconnects after
a retryable one and the cursor replays the dispatch); any other refusal drops
the dispatch.

| Discord dispatches | uma reports |
| --- | --- |
| `MESSAGE_CREATE` of type `DEFAULT` or `REPLY` | `message.received`: a message envelope from `discord.chat.user`, scoped by the room and, in a thread, the thread |
| A message by the bot, or by the application's interaction webhook | Nothing: it would loop, and uma knows what it sent by its provider message IDs |
| A message by another bot, a system user or a webhook (`webhook_id`) | A `message.automated` envelope from a bot sender |
| A guild message not mentioning the bot, under `mentions_only` | Nothing |
| A message with attachments and no text | One structured part (`discord.chat.attachments`) with attachment IDs, names, types and sizes; the files are not fetched |
| A message whose content Discord withheld | Nothing |
| `MESSAGE_UPDATE` carrying `edited_timestamp` | `message.updated`: an edit by the author (only the author edits content), revision `edited_timestamp`. One without it is an unfurl and is dropped. An edit of a message uma never saw records the message instead |
| `MESSAGE_DELETE` | `message.updated`: a deletion by a system actor; Discord names no deleter |
| `MESSAGE_REACTION_ADD`, `MESSAGE_REACTION_REMOVE` with a Unicode emoji | `message.updated`: a reaction. The bot's own is dropped, as is a custom emoji (it has an ID and no Unicode form) |
| `THREAD_UPDATE` of a thread with `thread_metadata` | `conversation.state`: the thread's `archived`, complete, observed at `archive_timestamp`, for the thread's reference; see [Archiving threads](#archiving-threads) |
| `INTERACTION_CREATE` | `interaction.received`; see [Interactions](#interactions) |
| System messages, other dispatches | Nothing |

A message ID is `<channel ID>:<message ID>`: snowflakes are unique across
Discord, but every route that reads or changes a message names its channel.
Text is reported as Discord sent it, markdown and `<@id>` mentions included.
The payload of every message (`discord.chat.message`, version 1) names the
application, channel, message, guild, thread parent, the message it replies
to, a webhook and whether the bot was mentioned. It is a claim, never an
identity. A gateway message names its channel, not the channel's type: the
first message of an unknown channel reads it once (Get Channel) to learn
whether it is a thread and of which channel; interactions carry their channel
and need no read.

## The Interactions Endpoint URL

The handler reads the body and verifies `X-Signature-Ed25519`, the hex Ed25519
signature over the `X-Signature-Timestamp` value immediately followed by the
raw body, against the application's public key, before anything else; the
timestamp (Unix seconds) must be within the replay window either way. The
shared [webhook authentication](../webhook-auth.md) package does the check
with the standard library's `crypto/ed25519`. Every failure is `401`, a
malformed signature included: "If the signature fails, your app should
respond with a 401", and Discord sends such requests itself and removes a URL
that accepts one ([overview](https://docs.discord.com/developers/interactions/overview)).
A public key that cannot be resolved is `503`.

| Discord sends | The handler answers |
| --- | --- |
| `PING` (type 1) | `200` `{"type":1}` |
| An application command, a component, a modal submission | records the interaction, then `200` with the deferral: `{"type":5}` (a loading response) or `{"type":6}` for a control on a message (an update later); see [Answering](#answering) |
| An autocomplete request (type 4) | `200` `{"type":8,"data":{"choices":[]}}`, nothing recorded |
| A retryable sink refusal | `503`; any other refusal `422`. Discord does not retry an interaction; its user sees it fail |
| Another application's interaction, an unreadable payload | `400` |

## Conversations

| Selection | A conversation is | Its reply route |
| --- | --- | --- |
| per room | a channel: a guild text channel or a direct message. A thread's messages belong to the channel it hangs off | the room |
| per thread | a thread channel (a thread or a forum post). A message outside a thread has no thread and is refused by a per-thread inbox | the thread, which a send posts into |
| per contact | one user | the user: replies and starts go to the user's direct message, whatever channel the user wrote in |

The inbox is bound to the application ID, not to a guild: channels and threads
separate conversations inside it.

## Interactions

| Discord sends | Interaction |
| --- | --- |
| An application command (type 2) | kind `command`; `Command` is the name, with its subcommand group and subcommand as further words (`ticket close`); `Arguments` the leaf options as a JSON object of name to value, or a context menu command's target ID |
| A component (type 3): a button or select | kind `action`; `Action` is the `custom_id`, `Arguments` the selected values, one per line; `Message` the message the control sits on (none on an ephemeral message) |
| A modal submission (type 5) | kind `submission`; `Fields` named `discord.chat.modal.<custom_id>` (lowercased, other characters `_`) version 1, data `custom_id` and `values` (every submitted component's value or values by its `custom_id`) |

The actor is the member's user in a guild, the user in a direct message: the
signed payload's, never a field the content claims. The interaction ID is
`interaction:<id>`, so an interaction redelivered is a duplicate to core. The
deadline is three seconds and the respond window fifteen minutes from the
request's signed timestamp (the receiver's clock over the gateway); the token
is stored beside the interaction as its response token. `Context` is the room
and, in a thread, the thread.

### Answering

Discord takes an interaction's first answer within three seconds, so the
adapter acknowledges each one as soon as it is recorded, with a deferral, and
the host answers later through the interaction's webhook
(`/webhooks/<application>/<token>`, no bot token). Over the endpoint the
deferral is the HTTP answer; over the gateway the receiver posts it to the
callback endpoint ("If you are receiving Interactions over the gateway, you
have to respond via HTTP",
[receiving and responding](https://docs.discord.com/developers/interactions/receiving-and-responding)).

| Interaction | Deferred with | `message` | `ephemeral` | `update` |
| --- | --- | --- | --- | --- |
| A command, or a modal not opened from a message | `DEFERRED_CHANNEL_MESSAGE_WITH_SOURCE`: everyone sees it loading | edits the loading message (Edit Original Interaction Response) | rejected `unsupported`: the deferral fixed the response's visibility ("The ephemeral flag will be ignored, and the value you provided in the initial defer response will be preserved") | rejected `unsupported`: no message to update |
| A component, or a modal opened from one | `DEFERRED_UPDATE_MESSAGE` | a followup message | a followup with the `EPHEMERAL` flag | edits the control's message, which must be the effect's target |

A follow-up (`FollowUp`, or `Interactions.Ephemeral` naming the interaction) is
a followup message, ephemeral in mode `ephemeral`. A followup sent right after
a loading deferral "will function as Edit Original Interaction Response" and
drops the flag, so after a command's deferral the adapter reads the original
response first and rejects a follow-up with `conflict` while it still shows
loading: give the initial response first. A response is rejected before any
call when the token is missing or not a token, the window has passed
(`deadline_expired`), the audience does not fit the mode or the content cannot
be carried. After the call: `404` (an unknown webhook or interaction, a token
past fifteen minutes) is `not_found`, `400` `validation`, `403` `unauthorized`,
`429` `rate_limited`, a 5xx or a lost answer `outcome_unknown`. A response
whose answer was lost is not reconciled.

`Interactions.Ephemeral` naming no interaction is rejected `unsupported`:
Discord has ephemeral messages for interaction answers only. Core holds the
recipient to the interaction's actor, the one user Discord shows it to.

## Sending

| Route | Reference | Lands |
| --- | --- | --- |
| Room | namespace `discord.chat`, kind `room`, ID the channel | a message in the channel |
| Thread | kind `thread`, ID the thread channel, parent ID its channel | a message in the thread |
| User | namespace `discord.chat.user`, kind `user`, ID the user | the user's direct message channel: Create DM opens it or answers the existing one, then the message is posted there |

A send that answers a message of the same channel replies to it
(`message_reference`, `fail_if_not_exists: false`, so a deleted original does
not fail the send). Content is text parts, joined by a blank line, sent as
written (Discord's markdown applies) with `allowed_mentions: {"parse": []}`, so
no text pings anyone; and at most one structured part `discord.chat.components`
version 1 whose data is the JSON array of components (action rows of buttons
and selects, which come back as interactions). Attachments and other parts are
rejected `unsupported`, text over 2000 characters `validation`, any audience
but public `unsupported`, all before a call.

Every message carries a nonce of its operation (25 characters) with
`enforce_nonce`: "If another message was created by the same author with
the same nonce, that message will be returned and no new message will be
created" within Discord's "past few minutes", so a retried attempt of an
operation never posts twice inside that window.

Discord says direct messages "should generally be initiated by a user action",
not to "DM everyone in a server", and that opening many quickly gets a bot
rate limited or blocked ([Create DM](https://docs.discord.com/developers/resources/user)):
use `start_conversation` to a user for answers the user asked for. A user
Discord does not let the bot message ("Cannot send messages to this user",
`50007`) is `unauthorized`.

## Editing, deleting and reacting

The target of each is a message of the channel, `<channel>:<message>`, sent
through uma or not; Discord answers for what the bot may change. Only an
author edits content: another author's message is rejected `unauthorized`
(`50005`). Deleting another author's guild message needs `MANAGE_MESSAGES`,
else `unauthorized`. A message that is gone is `not_found`, and none of them
ever becomes a new message. An edit is the whole message: components it does
not carry are removed.

Reactions take the Unicode emoji, URL-encoded; an emoji Discord does not know
(`10014`) is rejected `unsupported`, a blocked reaction (`90001`)
`unauthorized`. Adding a reaction the bot holds and removing one it does not
are applied. A reactor holds several reactions on a message, one per emoji.

## Signals and membership

`signal` of kind `typing` calls Trigger Typing Indicator on the route's
channel (opening a user's direct message channel first); Discord shows it ten
seconds. Discord says "Generally bots should not use this route", except while
a reply takes a few seconds
([channel](https://docs.discord.com/developers/resources/channel)). Other
kinds are `unsupported`.

`membership` adds and removes users of a thread: the route of a per-thread
conversation. Adding needs the bot able to send in the thread; removing needs
`MANAGE_THREADS` (or a private thread the bot created); an archived thread
refuses both (`50083`, `unauthorized`). Both answer 204 whether or not the user
was a member, so adding a member and removing a non-member are applied. A room
or user route is rejected `unsupported`: guild membership needs a user's
OAuth token (`guilds.join`) and group direct messages are legacy.

## Archiving threads

`archive` is the only provider conversation state Discord offers a bot, and it
is the thread's: the route of a per-thread conversation. A room, a direct
message and a channel have no archive; their route is rejected `unsupported`
with no call. It is reported with the limit `scope` = `shared`: archiving
changes the thread for everyone in the guild, so the engine authorizes it as
`conversation.state_shared`.

`Conversations.Archive` calls Modify Channel on the thread with
`{"archived": true}`, and with `Undo` `{"archived": false}`. The request names
the conversation's provider messages (`Items`); one call changes the whole
thread, so they are not sent. Archiving needs `MANAGE_THREADS`; unarchiving a
thread that is not locked needs only `SEND_MESSAGES`, a locked one
`MANAGE_THREADS`
([channel](https://docs.discord.com/developers/resources/channel)). A refusal
is final: `unauthorized` (50013, 50001), `not_found` for a thread that is
gone. Applying the state the thread holds already is applied. Reconciliation
reads the thread (Get Channel) and compares its `archived` with the request.

The state also changes without uma, and every change arrives as
`THREAD_UPDATE`:

| Change | Cause | Reported as |
| --- | --- | --- |
| archived | uma's own request (the echo that settles its operation); a person with `MANAGE_THREADS` or the thread's creator; auto-archive, when the thread has had no activity for its `auto_archive_duration` (1 hour, 1 day, 3 days or 7 days) | `conversation.state`, `archived` true |
| unarchived | uma's request; a person; a new message in the archived thread, which unarchives it | `conversation.state`, `archived` false |

The report names the thread (`discord.chat` thread reference under its parent
channel), is complete (the thread is the whole conversation) and is observed
at `thread_metadata.archive_timestamp`, which Discord sets on every change of
the flag; the engine orders reports by that time. A change of the thread's
name or lock carries the same state and time, so it is the same event again
and changes nothing. A `THREAD_UPDATE` of a channel that is not a thread, or
without `thread_metadata`, is dropped, as is one of a conversation uma does
not hold. Reports resolve the conversation by its thread reference, so a
per-room inbox (where a thread's messages belong to the room's conversation)
shows a thread's archive on that conversation: archive a room's thread only
under per-thread selection.

`move`, `read_state`, `flag` and `trash` are not declared; see
[Not declared](#not-declared). Pin, which would be `flag`, is deferred.

## Reconciling

| Attempt | Discord is read with | Applied when |
| --- | --- | --- |
| Send | Get Channel Messages `after` the attempt (minus five seconds of clock skew), 100 a page, at most 10 pages | a message of the bot carries the operation's nonce; or, carrying no nonce, has the text and components the attempt sent. A message carrying another nonce is another operation's |
| Edit | Get Channel Message | its content is the replacement's |
| Delete | the same | the message is gone |
| Reaction | the same | its reaction of the emoji is the bot's (`me`); a removal: is not |
| Membership | Get Thread Member | the user is a member; a removal: is not |
| Archive | Get Channel | the thread's `archived` is true; an unarchive: is false. A thread that is gone is rejected `not_found` |

Not applied is `failed` with `not_found`, so a retry is safe. A listing not
read to its end is inconclusive: the operation stays `outcome_unknown`. A
message that is gone is a rejected edit or reaction, `not_found`. A read
Discord refuses is the call's error, never "not applied". Whether a read
returns a message's nonce is not documented (the reference names it only in
`MESSAGE_CREATE`); without it the bot-and-content match stands in, and two
identical messages of the bot after one attempt cannot be told apart.

## History

`Runtime.Import` reads past messages oldest first, as the gateway reports
them, each once: a message the engine holds already, which includes every one
uma sent, is skipped.

| Route | Read with |
| --- | --- |
| A room or a thread | Get Channel Messages of that channel |
| A user | Create DM for the direct message channel (it answers the existing one, and opening one sends nothing), then as a room |
| None (channel-wide) | the channels in `history_channels`, merged; without any it is `disabled` |

Snowflakes order messages in time across channels, so a page asks each
channel for the messages `after` one ID (100 at most, which Discord answers
newest first), merges and keeps the oldest; the cursor is the last ID
returned and the window starts at the snowflake of its time. A cursor issued
for another route, channels or window start is `validation` before any call. A
message that heads a thread (`HAS_THREAD`) reports a gap: the thread's messages
are in the thread; read its route. The bot needs `VIEW_CHANNEL` and
`READ_MESSAGE_HISTORY`, without which Discord answers no messages.

## Rate limits

REST calls go through a limiter of the adapter's own, after
[Rate Limits](https://docs.discord.com/developers/topics/rate-limits):

- at most 50 requests a second across routes (the global limit); interaction
  callbacks and webhooks are exempt;
- per-route buckets from `X-RateLimit-Bucket`, `X-RateLimit-Remaining` and
  `X-RateLimit-Reset-After`, kept per top-level resource (channel, guild,
  webhook); nothing is hard-coded;
- a request that would wait up to a second waits; a longer wait is refused
  before the request as `rate_limited` with the wait, nothing applied;
- a `429` is recorded and returned as `rate_limited` with its `retry_after`
  (a global one blocks every route), never retried inline;
- `401`, `403` and `429` answers (a `shared` 429 excepted) count as invalid
  requests, of which 10,000 in ten minutes get the IP address banned: at
  9,000 the adapter refuses every request until they age out.

## Limits

| What | Limit |
| --- | --- |
| Message text | 2000 characters |
| Interaction | acknowledged within 3 seconds; token valid 15 minutes |
| Replay window | 5 minutes either way (configurable down) |
| Gateway | one message 8 MiB; 8 channels recorded at once, 64 dispatches held; 1000 identifies a day (Discord's) |
| REST | 50 requests a second; buckets from headers |
| Reactions | Unicode emoji only; custom emoji are dropped inbound and refused outbound by Discord |

## How it is tested

The shared adapter suite (`conformance.RunAdapterSuite`) runs six times: as the
gateway receiver and as the Interactions Endpoint URL alone, each per room,
per thread (each peer a thread of one channel) and per contact (each peer a
direct message), against a stand-in Discord written to the references above:
a REST API and a gateway (`httptest` with TLS) that says Hello, ACKs
heartbeats, answers Identify with READY and Resume with the missed dispatches,
and sends the suite's events as dispatches. An event counts acknowledged when
the receiver handled its dispatch without a refusal. Over the endpoint only
interactions are expressible, so the message cases skip there. The descriptor
the suite is given leaves out `send_ephemeral` (the suite's ephemeral case
names no interaction, which Discord cannot show) and, outside per thread,
`membership` and `archive`; package tests cover both. The `State` group runs
per thread against the stand-in's thread (archived, `thread_metadata`); its
inbound cases skip, because a `THREAD_UPDATE` names the thread and the
suite's conversation is stored outside any thread selection, so nothing
Discord sends resolves it. Package tests hold the report: archive,
unarchive and auto-archive as `THREAD_UPDATE` fixtures, an event ID per
change, a non-thread and a thread without metadata dropped, a conversation
uma does not hold dropped, and the thread's lane.

Package tests drive the receiver against the stand-in gateway: Identify with
the configured token and intents, heartbeats carrying the sequence number and
acknowledged, a beat on request, dispatches recorded with the cursor; resume on
the resume URL with the session and sequence after a resumable close, a
Reconnect and a resumable Invalid Session, with the missed dispatch replayed;
identify again after an Invalid Session and closes 4007 and 4009; 4014, 4004,
4011 and 4013 ending the receiver without retry; a zombie connection resumed;
a restart resuming from the cursor, which never passes a refused dispatch; a
refusal retrying cannot cure leaving the receiver up; no identify left, a token
of another bot, connections failing in a row, a cursor naming a foreign host;
and lanes keeping one channel's order while another's goes on, the cursor held
below a dispatch still recording. The endpoint's tests sign with a test key:
wrong key, stale and future timestamps, a missing or malformed signature or
timestamp, a timestamp other than the signed one, a tampered body, all `401`;
PING, deferrals by kind, autocomplete, redelivery, refusals. Send, edit,
delete, react, signal, membership, the response modes and deferrals, the
follow-up guard, reconciliation (with and without nonces in reads, bounded),
history paging and cursors, refetch and the limiter (buckets, 429s, the global
second, the invalid ceiling) each have their own.

The recorded test (`TestRecordedBot`) replays committed cassettes, and its
subtest `thread archive` (the thread archived and unarchived, each read back)
replays its own. The REST calls the cassettes hold were checked live on
2026-11-16, see [Live verification](#live-verification-2026-11-16). To
record again, follow [Record Discord cassettes](../recording/discord.md). The
gateway and the endpoint are not recorded: a WebSocket session and requests
Discord sends are no HTTP exchange of the adapter's.

## Live verification (2026-11-16)

Run against a private test guild with a bot application installed for it, in
one text channel and one public thread. Every call below was accepted by
Discord and the state it read back matched; the messages the run created were
deleted at its end.

| Capability | Live result | Evidence |
| --- | --- | --- |
| `send` | A message with a nonce posts to the channel; `allowed_mentions` suppresses the mention it contains | recorded: Create Message |
| `reconcile` | Get Channel Messages `after` a snowflake dated before the send returns the message, so a send whose answer is lost is found again; the list does not carry the message's `nonce`, so the match is by author and content | recorded: Get Channel Messages |
| `edit` | Edit Message answers 200; the message reads back with the new content and `edited_timestamp` | recorded: Edit and Get Channel Message |
| `react` | Create Reaction and Delete Own Reaction answer 204; the read-back shows the 👍 with `me` true, then no reactions | recorded: PUT, DELETE and Get Channel Message |
| `signal` | Trigger Typing Indicator answers 204 | recorded |
| `membership` | Add Thread Member and Remove Thread Member answer 204; Get Thread Member answers 200, then 404 `Unknown Member` (code 10007) after the removal, which the reconcile reads as applied | recorded |
| `delete` | Delete Message answers 204; Get Channel Message then answers 404 `Unknown Message` (code 10008) | recorded |
| `archive` | `PATCH` `{"archived": true}` alone is accepted from a bot with Manage Threads and answers the thread with `thread_metadata.archived` true; Get Channel shows it; `{"archived": false}` unarchives it the same way | recorded in `discord-archive` |
| thread reply | A send on the thread's route, with the first message as target, posts into the thread; the reply carries its parent, which the adapter reports as `in_reply_to` | live run, not recorded |
| `history` | A thread's route read from a window start returns the thread's messages oldest first as automated messages from the bot, without a gap; the reply's envelope names its parent | live run, not recorded |
| `refetch` | A message read back as a `message.edited` envelope | live run, not recorded |

What the recording answered that the stand-in only assumed:

- Get Channel Messages with `after` answers newest first, and its messages
  carry no `nonce`: the Create Message answer does, a later read of the same
  message does not.
- The `X-RateLimit-*` headers carry a bucket, limit, remaining, reset and
  reset-after on every route; reaction routes allow one call per 250 ms.
- Get Channel on a thread reports `archive_timestamp` with microseconds
  truncated to milliseconds, where Modify Channel reports all six digits.
- Adding a member who is already in the thread answers 204 and keeps the
  original join time.

Not verified live:

- **Inbound over the gateway** (message, edit, deletion, reaction events,
  `THREAD_UPDATE` for an archive, redelivery after a resume): the gateway needs
  a person posting in the guild, or a bot that holds the privileged intent, and
  a long-lived process. Owner step: run the receiver against the test guild
  with `GUILDS`, `GUILD_MESSAGES` and `GUILD_MESSAGE_REACTIONS` and post a
  message mentioning the bot, a reply, a reaction and an edit from a second
  account, then archive the thread in the client; each should arrive once.
- **Interactions** (slash commands, buttons, modals): Discord sends them to the
  gateway or to the Interactions Endpoint URL, which needs a public HTTPS
  endpoint.
- **Redelivery:** the adapter has no REST path for it; a resumed gateway session
  replays the missed dispatches, which is covered by the gateway item above.
- **A direct message** with a user (needs a person to open it).
- **History with a channel-wide window** and paging past one page: the run read
  one thread, two messages.

Side effect to know before running the recording again: removing the member
from the thread removes that account from the thread's followers until it
rejoins.

## Known limits

- Guild messages need a mention or `MESSAGE_CONTENT`; see
  [the headline limit](#the-headline-limit-message-content-is-privileged).
- The gateway is verified only against the stand-in written from the
  documentation; nothing about it is recorded or run live. Sharding is not built: one
  connection serves the bot; a bot Discord requires to shard is refused with
  close 4011 (`permanent`).
- `THREAD_UPDATE` needs the `GUILDS` intent; the stand-in sends it, and
  the `archive_timestamp` of an archive and an unarchive is recorded for the
  REST answers only. That a new message unarchives an archived thread, and
  that auto-archive is dispatched as `THREAD_UPDATE`, is the documented
  behavior, not observed.
- The gateway's MESSAGE_UPDATE is read as a whole message; the reference says
  only "a message object with the same extra fields as MESSAGE_CREATE".
- Whether Discord accepts a Resume with a sequence number older than the last
  one received (the cursor's, after a restart) is not documented; if it does
  not, the receiver gets an Invalid Session and identifies afresh, and what
  the session held is lost to it (history can import it).
- That Get Channel Messages with `after` answers the messages nearest after
  the ID (rather than the newest) is relied on by history and reconciliation;
  the reference says only that the answer is newest first. The test channel
  held two messages, fewer than a page, so the live run cannot tell the two
  apart: a channel with more than a page after the ID is not checked.
- A read does not return a message's nonce (the recording shows none on Get
  Channel Messages or Get Channel Message), so reconciliation always matches
  by author and content.
- An interaction is acknowledged with a deferral before the host decides, so
  the host cannot open a modal (`MODAL`) or answer in the acknowledgement
  itself; a command's answer is public. Autocomplete gets no choices.
- A command's follow-up waits for its initial response (`conflict` until then).
- Reconciliation does not read a message edited or deleted after the attempt
  by someone else: it reports what Discord shows now.
- The 1000-identifies limit and `max_concurrency` are respected for one
  receiver; several processes receiving one bot are not coordinated (the
  engine's receiver lease keeps one per store).
- Experimental: configuration may change ([Compatibility](../compatibility.md)).
