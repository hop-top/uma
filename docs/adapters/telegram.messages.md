# telegram.messages

`telegram.messages` is the Telegram adapter. One channel is one bot. Telegram
calls the bot's webhook with updates, or uma reads them by long polling; uma
answers through the Bot API on the host's HTTP client. It is written against
Bot API 10.3 (August 2026).

| | |
| --- | --- |
| Key | `telegram.messages` |
| Package | `hop.top/uma/adapter/chat/telegram`, in the module of the same path |
| Capabilities | `receive`, `receive_http`, `send`, `start_conversation`, `edit`, `delete`, `react`, `respond` |
| Events | `message.received`, `message.updated`, `interaction.received` |
| Identity namespaces | `telegram.messages.user` |
| Sender kinds | person, bot |
| Provider permissions | none declared: a bot token carries no scopes; what the bot may do in a chat depends on its rights there (see [Permissions](#permissions)) |
| Message IDs and threading | assigned by Telegram (chat ID and `message_id`); no email-style threading; selection per room, per thread (forum topics) or per contact (a private chat with each user) |
| Reply parent | reported: `reply_to_message`; a forum topic's root message is thread membership and is not reported |
| Interactions | reports inline keyboard button presses and bot commands, answers them ([interactions](../interactions.md)) |
| Maturity | experimental |

The matrix of all adapters is in [Adapters](README.md).

## Capabilities and operations

| Capability | Behavior |
| --- | --- |
| `receive` | `Runtime.Receive` reads updates by long polling (`getUpdates`) with a cursor; see [Long polling](#long-polling). It excludes a webhook. Disabled on a channel whose `receive` is `webhook` |
| `receive_http` | `Runtime.Webhook` takes Telegram's webhook updates: messages, channel posts, their edits and reactions; see [Receiving](#receiving). It excludes `receive`. Disabled on a channel whose `receive` is `poll` |
| `send` | `Messages.Reply` posts a message with `sendMessage` to the stored route: a chat, a forum topic, or a user's private chat |
| `start_conversation` | `Conversations.Start` posts the first message to a chat, a topic or a user the same way. Telegram lets a bot message a user only after the user started the bot, so a start to anyone else is refused; see [Sending](#sending) |
| `edit` | `Messages.Edit` replaces the text, markup and keyboard of a message with `editMessageText` |
| `delete` | `Messages.Delete` deletes a message with `deleteMessage` |
| `react` | `Messages.React` sets or clears the bot's reaction with `setMessageReaction` |
| `respond` | `Interactions.Respond` answers an interaction as a message, as the notification a callback query shows, or as the update of the message the button sits on; see [Answering](#answering) |

## Not declared

Reason classes are defined in [Adapters](README.md#why-a-capability-is-not-declared).

| Capability | Reason | Why |
| --- | --- | --- |
| `reply_recipients` | `provider` | A Telegram message goes to its chat; there is no cc or blind recipient |
| `receipt` | `provider` | The Bot API has no delivery or read receipt, to or from a user |
| `membership` | `provider` | A bot cannot add a user to a chat, only hand out invite links the user opens, so the pair of adding and removing has no add |
| `history` | `provider` | The Bot API has no call that reads past messages; `getUpdates` is a queue that forgets what is confirmed and keeps nothing past 24 hours |
| `reconcile` | `provider` | The Bot API has no call that finds a message by its text, by a marker or by anything but an ID the bot already holds, so a lost answer cannot be looked up |
| `refetch` | `provider` | An edited message arrives whole; the Bot API has no call that reads a message again |
| `work_item_act` | `provider` | Telegram has no work items |
| `internal_note` | `provider` | Telegram has no private message that is part of the conversation; notes stay in uma |
| `extension_execute` | `not built` | An effect such as `sendPoll` or `pinChatMessage` could be shipped |
| `extension_query` | `not built` | No read is shipped; `getMe` (the bot's identity) would be the first |
| `probe_permissions` | `not built` | `getMe` could check that the bot token is live, and `getChatMember` what the bot may do in a chat |
| `stream` | `not built` | `sendMessageDraft` could carry a native stream; sends and edits can stand in |
| `send_ephemeral` | `not built` | `sendMessage` with `ephemeral_message_parameters` (a group message only one member sees) |
| `signal` | `not built` | `sendChatAction` shows "typing"; nothing sends it |
| `respond_follow_up` | `by design` | A callback query takes one answer, and a command none; a further response is an ordinary message, which `Messages.Reply` sends |
| `archive` | `provider` | The Bot API has no archive call for a bot |
| `move` | `provider` | A chat has no folders or labels |
| `read_state` | `provider` | The Bot API has no call that marks a chat read; `readBusinessMessage` needs a business account |
| `flag` | `not built` | Flag kinds are `star` and `important`; the nearest call, `pinChatMessage`, marks one message, not a chat |
| `trash` | `provider` | A chat has no trash; `deleteMessage` is per message and not undoable |
| `raw_read` | `not built` | Raw read is built for mail only; the provider's whole copy of a message is not read |

## Permissions

- **Provider.** Two credential purposes, resolved through `ports.Credentials`
  and always as a secret: `telegram.messages.api` (the bot token BotFather
  issued) and `telegram.messages.webhook` (the webhook's secret token). Telegram
  takes the bot token in the request path, `https://api.telegram.org/bot<token>/<method>`:
  an HTTP client that logs request URLs logs it. The token is only sent to the
  channel's `api_url`.
- **In the chat.** A bot token has no scopes; the bot's rights in each chat decide.
  Reaction updates and deleting another's message need the bot to be an administrator
  (with `can_delete_messages`), posting to a channel needs `can_post_messages`, and a
  group's privacy mode, which BotFather turns on, limits what the bot is sent to commands,
  mentions and replies unless the bot is an administrator or has it off.
- **uma.** The receiving principal needs `channel.receive`; replies are authorized as `message.reply`, starts as `conversation.start`, and edits, deletions and reactions as `message.edit`, `message.delete` and `message.react`.

## Configuration

`telegram.messages.config` version 1, as JSON in the channel configuration.
Unknown fields are refused, so a secret put there by mistake is caught.

| Field | Required | Meaning |
| --- | --- | --- |
| `bot_id` | yes | The bot's user ID: the number before the colon of its token, and the `id` `getMe` answers. Messages it authored are never reported back. It is the destination of every envelope: bind the inbox to it, as the decimal string |
| `bot_username` | yes | The bot's username without `@`, such as `uma_bot`: 5 to 32 letters, digits or `_`, ending in `bot`. What a mention names |
| `api_url` | no | The Bot API root; default `https://api.telegram.org/`. https without user info or query |
| `mentions_only` | no | Report a message of a group, supergroup or channel only when it addresses the bot; private chats always |
| `poll_timeout_seconds` | no | The long-polling timeout of `receive`, 1 to 50; default 30 |
| `receive` | no | How the bot takes updates: `poll` (long polling, `receive`) or `webhook` (`receive_http`). Empty offers both and leaves the choice to the host; see [Choosing how to receive](#choosing-how-to-receive) |

Host options (`telegram.Options`): `PollInterval`, the shortest time between two long
polls that returned nothing (default one second): a long poll that times out empty has
already waited `poll_timeout_seconds`, so it only paces a server that answers at once.
Each pause is spread by +-10% so channels do not read in phase; `Jitter` is the source of
that spread (uniform in [0, 1]; nil is the standard library's generator, tests set it to
make the pauses deterministic).

## Receiving

Set the webhook once, outside uma, with the bot token and a secret token of
your own (at least 16 characters of `A-Z`, `a-z`, `0-9`, `_` and `-`), and ask
for the update types the adapter reads
([`setWebhook`](https://core.telegram.org/bots/api#setwebhook)):

```sh
curl -sS "https://api.telegram.org/bot$TOKEN/setWebhook" \
  -d url="https://example.com/hooks/telegram" \
  -d secret_token="$SECRET" \
  -d 'allowed_updates=["message","edited_message","channel_post","edited_channel_post","message_reaction","callback_query"]'
```

The list is `telegram.AllowedUpdates`. Telegram only sends `message_reaction` when
it is named in `allowed_updates` and the bot is an administrator of the chat;
every other type the adapter reads is sent by default, and naming a list sends
only it, so `callback_query` is in it too. A webhook and `getUpdates` exclude each other; see [Long polling](#long-polling).

The handler reads the body and checks `X-Telegram-Bot-Api-Secret-Token` before
anything else, through [webhook authentication](../webhook-auth.md): the header
is the whole secret, compared in constant time with the secret the host
resolves on every request, and a secret shorter than 16 bytes is refused
whatever the request carries. Telegram sends no timestamp and no signature, so
there is no freshness window: the token proves the request was made by someone
holding it, and TLS is what keeps it secret.

A request that fails gets 401 (503 when the secret cannot be resolved) and
nothing reaches the sink. The rules for updates:

| Telegram sends | uma reports |
| --- | --- |
| `message` in a private chat, group or supergroup | `message.received`: a message envelope from `telegram.messages.user`, scoped by the chat and, in a forum topic, by the topic |
| `channel_post` | The same, from the channel as a `chat` identity: a post names no user |
| A message sent on behalf of a chat (an anonymous administrator, a linked channel) | The same, from the chat the message names (`sender_chat`) as a `chat` identity |
| A message by the bot itself | Nothing: it would loop, and uma knows what it sent by its provider message IDs |
| A message by another bot | A `message.automated` envelope from a bot sender |
| `edited_message`, `edited_channel_post` | `message.updated`: an edit by the author, revision `edit_date`. An edit of a message uma never saw records the message instead |
| `message_reaction` | `message.updated`: one reaction per emoji the user's reactions gained or lost, removals first. The bot's own is dropped; so are custom emoji and paid reactions |
| A message with a caption, photo, file, voice, sticker or other media | The text, or the caption, and one structured part (`telegram.messages.media`) naming each file by kind, IDs, name, type, size and dimensions; the files are not fetched |
| A message that starts with a bot command for this bot (`/start`, `/status@uma_bot`) | `interaction.received`, kind `command`; never a message. A command for another bot is dropped |
| `callback_query` on a message | `interaction.received`, kind `action` |
| A service message, a business message, an ephemeral message | Acknowledged, nothing reported |
| Any other update | Acknowledged, nothing reported |

A message ID is `<chat ID>:<message_id>`. Telegram's `message_id` is unique only
inside a chat, so the chat is part of the identity; a group's chat ID is
negative. Text is Telegram's own, as sent: entities are not applied and nothing
is escaped. The payload of every message (`telegram.messages.message`, version 1)
names the bot, the chat and its type, the message and topic IDs, the message it
replies to, the update type and whether the bot was addressed. It is a claim,
never an identity.

An update's `update_id` is what makes a redelivery a duplicate: an edit's event
ID is `update:<update_id>`, a reaction's adds the change's place in the update,
and a new message's is `message:<ID>`, so the same message reaching uma twice by
two routes is one. `update_id`s grow by one per update, and Telegram "allows you
to ignore repeated updates or to restore the correct update sequence"; the
adapter keeps no state of its own, core's duplicate check is the whole of it.

An update whose sink answer is retryable gets 503 and any other refusal gets 422.
Telegram repeats a request answered otherwise than 2XX and "give[s] up after a
reasonable amount of attempts"; it has no way to say a failure is final, so a
refusal retrying cannot cure is repeated a few times too and then lost. It shows
in `getWebhookInfo` as `last_error_message`.

### Choosing how to receive

Telegram serves a bot one way at a time, and which one is set at Telegram, by
whether the bot has a webhook. The channel's `receive` says which, so a host
reads it from the channel instead of starting both:

| Configuration `receive` | Capability `receive` (`Runtime.Receive`) | Capability `receive_http` (`Runtime.Webhook`) |
| --- | --- | --- |
| `poll` | available | unavailable, `disabled` |
| `webhook` | unavailable, `disabled` | available |
| empty | available | available: the host picks one |

The descriptor declares both for every channel, as it is the adapter type's; the
channel's choice is in the capability statuses its instance reports, and in
`Configuration.Capabilities`. The engine checks them first: `Runtime.Receive` on a
`webhook` channel and `Runtime.Webhook` on a `poll` channel fail `unsupported`
("receive unavailable", "receive_http unavailable") before the adapter makes any
call, and `Runtime.Receive` does not retry it. Called directly, the instance
refuses too: `Instance.Run` returns `disabled` without reading, and the handler of
a `poll` channel answers every request 404 and reads nothing.

The adapter never calls `setWebhook` or `deleteWebhook`, whatever `receive` says:
setting one for a `webhook` channel and removing it before a channel turns to
`poll` are the bot owner's to do.

### Long polling

`receive` is the other way to take updates: `Runtime.Receive` runs a receiver that
reads `getUpdates`, 100 updates at a time with `poll_timeout_seconds` as the long-poll
timeout and the same update types as the webhook example above. "There are two mutually
exclusive ways of receiving updates", and `getUpdates` "will not work if an outgoing
webhook is set up": while a webhook is set Telegram answers every read with `error_code`
409 (seen with the description "Conflict: can't use getUpdates method while webhook is
active; use deleteWebhook to delete the webhook first", which the reference does not
document), and the receiver ends with `conflict`. A `poll` channel whose bot still has a
webhook meets it on its first read; the adapter never removes a webhook itself. A host
picks one way for a bot, stated in the channel's `receive` (see
[Choosing how to receive](#choosing-how-to-receive)), and runs `Runtime.Receive` or
mounts `Runtime.Webhook`, not both. The host's HTTP client must allow a request longer
than the long-poll timeout.

An update is "confirmed as soon as getUpdates is called with an offset higher than its
update_id", and Telegram keeps an unconfirmed one for 24 hours at most. The receiver reads
with the update after the last it handled, resumes after a restart from the cursor the engine
keeps (the `update_id` of the last envelope an update made, saved with it, so a stop between
an update's envelopes resumes at the update), and hands an update the sink refused over again
on the next connection. An update that made no envelope (a type the adapter does not read)
is confirmed by the next read, never saved as a cursor. A flood limit is waited out; a bad
token (`unauthorized`), a webhook (`conflict`), and a sink's refusal end the connection, and
the engine reconnects after a retryable one. An update the adapter cannot read is skipped.

**The link.** The instance implements `adapter.ReceiverReporter`: it reports
connected once a `getUpdates` is answered, an empty answer included, before the
updates it carried are handed over, and not connected after a failed read (a flood
limit too) and when `Run` returns. Long polling has no session, so connected means
only that the last poll got an answer. `Runtime.ReceiverStatus` reads `connecting`
until the first one. The report is advisory.

## Identities and conversations

Identities are in one namespace, `telegram.messages.user`, as `slack.chat.user` is Slack's:
a user or a bot (kind `user`, the user ID, positive) and a group, supergroup or channel
speaking as itself (kind `chat`, the chat ID, negative: an anonymous administrator, a
channel post). Telegram's IDs of the two never meet, so one namespace holds both. A
user's route, the private chat with them, is the same reference, because a private
chat's ID is the user's.

Every message names the conversation it belongs to, so one channel serves the
three selections an inbox may use:

| Selection | A conversation is | Its reply route |
| --- | --- | --- |
| per room | a Telegram chat (a private chat, group, supergroup or channel) | the chat |
| per thread | one forum topic of a chat (`message_thread_id`) | the topic |
| per contact | one user | the user: the private chat with them, whatever chat they wrote in |

The inbox is bound to the bot's `bot_id`, not to a chat: chats and topics are
what separate conversations inside it. Telegram names a thread only for forum
topics of supergroups and of private chats with topics; a message outside a
topic has no thread, so a per-thread inbox cannot place it.

## Interactions

The press of a button of an inline keyboard and a bot command are
[interactions](../interactions.md), never messages. The sink records the
interaction first, and only then does the handler answer 200, with an empty body.

| Telegram sends | Interaction |
| --- | --- |
| `callback_query` | kind `action`; `Action` is the button's `callback_data`, as it is (the host structures it, Telegram caps it at 64 bytes), `Arguments` is empty; `Message` is the message the button sits on, even when Telegram can no longer show it (`InaccessibleMessage`); `Context` the chat and the topic of that message. The ID is `callback:<update_id>`; the query ID is the response token |
| A message whose first entity is a `bot_command` at offset 0 | kind `command`; `Command` is the name without the slash and without the `@bot` suffix, `Arguments` the text after it, trimmed; `Context` the chat and the topic. The ID is `command:<chat ID>:<message_id>`, so one message is one command whichever update delivers it. No token |
| A callback query on an inline-mode message (`inline_message_id`, no chat) or with no data (a game) | acknowledged, not consumed |

The actor is the update's own `from` (or the chat a command speaks for), never a
field of the button's data. A command meant for another bot (`/status@other_bot`) is
dropped whatever `mentions_only` says. A command with no suffix is the bot's:
Telegram delivers one to a bot in privacy mode only when it means it, so
`mentions_only` does not drop it either. A command typed without its entity, one
that is not first in the text, and a command in a caption are text. An edit of a
command is not reported.

Telegram states no deadline for a callback query: the client "will display a progress
bar until you call answerCallbackQuery", and an answer to a query that has become too
old is refused without a time given. The adapter sets its own, short bound, 30 seconds
from when it read the update, for the acknowledgement and for the response; a response
after it is the host's to send as an ordinary message. The webhook's own answer cannot
clear the progress bar, which only `answerCallbackQuery` does, so a host that answers
none leaves it showing until the client gives up.

### Answering

`Interactions.Respond` makes ordinary Bot API calls: Telegram has no response URL.

| Mode | Audience | Done as |
| --- | --- | --- |
| `message` | public | `sendMessage` to the route, in its topic when it is one; then, for a callback query, `answerCallbackQuery` with nothing to show |
| `ephemeral` | restricted | `answerCallbackQuery` with text: a notification at the top of the chat screen that only the person who pressed the button sees; up to 200 characters, text only |
| `update` | public | `editMessageText` on the message the button sits on, which must be the effect's target; then `answerCallbackQuery` with nothing to show |

A response is `rejected` before any call when it is a follow-up, the audience does not fit
the mode, the content cannot be carried (a notification takes text only), or the window
has passed (`deadline_expired`). A command has no query: only `message` answers it, and
`ephemeral` and `update` are `unsupported`. Telegram's refusal of a query it finds too
old, or already answered, is `deadline_expired` too. The answer that clears the progress
bar comes after the message or the edit and is not part of its outcome: when it fails,
the response has still happened. Other outcomes are as for a [send](#sending).

Bot API 10.3 added ephemeral messages, group messages only one member sees, sent by a bot
within 15 seconds of a callback query or to a member when the bot is an administrator. The
adapter does not use them: `ephemeral` here is the callback notification, and
the group message is `send_ephemeral`, not built.

## Sending

A route is where a message lands, and what the adapter stores as a conversation's
reply route:

| Route | Reference | Lands |
| --- | --- | --- |
| Chat | namespace `telegram.messages`, kind `room`, ID the chat ID (negative for a group, supergroup or channel) | a message in the chat |
| Topic | kind `thread`, ID the `message_thread_id`, parent ID the chat ID | a message in that forum topic (`message_thread_id`) |
| User | namespace `telegram.messages.user`, kind `user`, ID the user ID | the private chat with the user, whose chat ID is the user's |

A reply that names the message it answers is sent with `reply_parameters` naming
it, when that message is in the same chat, and "even if it is gone"
(`allow_sending_without_reply`). On a route with no topic it lands in the topic
of the message answered when the stored message says one.

Content is text parts, joined by a blank line, plus at most one markup part and
at most one inline keyboard:

- **Text** is literal. Sent alone it carries no `parse_mode`, so no character
  in it means anything to Telegram: `*`, `_`, `<` or a backslash are only text.
  A message cannot mention or format through text parts.
- **Markup** (`telegram.messages.markup` version 1) is `{"parse_mode": "HTML" | "MarkdownV2",
  "text": "..."}`: the host's own markup, sent as written. Every markup part of a
  message names one mode; the legacy `Markdown` mode is not offered. Text parts
  beside a markup part are escaped by the adapter for its mode and keep their
  place among the parts. The host composes user text into markup itself with
  `EscapeHTML`, `EscapeMarkdownV2`, `EscapeMarkdownV2Code` (inside code and pre)
  and `EscapeMarkdownV2URL` (inside the parentheses of a link), which follow the
  escaping rules of [Formatting options](https://core.telegram.org/bots/api#formatting-options).
- **Keyboard** (`telegram.messages.keyboard` version 1) is an
  [`InlineKeyboardMarkup`](https://core.telegram.org/bots/api#inlinekeyboardmarkup)
  as JSON, sent as `reply_markup`. Each button has a text and exactly one action
  field, and a callback button's data is 1 to 64 bytes. A keyboard is not
  content: with no text there is nothing to send.

Attachments and other structured parts are rejected `unsupported` before any call,
as are any audience but public, plain text over 4096 characters (Telegram's
limit is "after entities parsing", so under a parse mode it is Telegram's to
judge) and a message with no text.

The operation succeeds with the provider message `<chat ID>:<message_id>` the
answer names; the chat ID is the answer's own. An answer that names no message
(`message_id` 0, a message Telegram scheduled) leaves the outcome unknown.

Telegram reports a refusal as `{"ok": false, "error_code": ..., "description": ...}`
and its reference documents no description, so the adapter reads the code and,
within a 400, a few known phrases; one it does not know is a plain validation
refusal and is never retried. A description is never echoed, but for a 403 whose
words are plain.

| Telegram answers | The operation |
| --- | --- |
| 400 for markup it cannot parse, text too long, or a refused reaction | `rejected`, `validation` |
| 400 for a chat, user or message that is not there, or a group that became a supergroup | `rejected`, `not_found` |
| 400 for a message the bot may not change, 403 | `rejected`, `unauthorized`, with Telegram's words for a 403: "bot was blocked by the user", "bot can't initiate conversation with a user", "bot was kicked from the group chat" |
| 401 or 404 | resolved again once with a fresh token, then `unauthorized` |
| 429 | `rate_limited`, waiting `retry_after` |
| 5xx, or an answer lost after the request was sent | `outcome_unknown` |
| A connection that was never made | `transient` |

A lost answer is never repeated: Telegram has no idempotency key and no call that
finds a message again (see [Not declared](#not-declared): `reconcile`), so the
operation stays `outcome_unknown` for an operator to settle.

A start to a user is refused, `rejected` and `unauthorized`, when the user has not
started the bot or has blocked it. Telegram offers no way round it: a bot learns
a user's ID only from what the user sent. A start to a user ID known from an earlier
conversation, or to a chat the bot is in, works. A chat is named by its numeric ID;
a public channel's `@username`, which `chat_id` also takes, is not a route.

## Editing, deleting and reacting

The target of each is a message of the channel, `<chat ID>:<message_id>`, sent
through uma or not; Telegram answers for what the bot may change.

- **Edit.** `editMessageText` takes the content as the whole message: text,
  parse mode and keyboard, as for a send. A bot edits only its own messages, so
  another author's is rejected `unauthorized` ("message can't be edited"), a
  message that is gone `not_found`, and neither ever becomes a new message. An
  edit to the text the message already has is answered "message is not modified";
  the adapter takes that as applied, as it is what was asked. A message with
  media has no text to edit (`editMessageCaption` is not built). The reference does
  not say whether an edit that names no keyboard drops the message's own; the
  adapter always states the keyboard it means.
- **Delete.** `deleteMessage` works "if it was sent less than 48 hours ago". A bot
  deletes its own messages in private chats, groups and supergroups; another's
  only as an administrator, with `can_delete_messages` in a supergroup or channel
  (a channel needs `can_post_messages` for the bot's own). The adapter cannot know a
  message's age, so a refusal for it is Telegram's: `rejected`, `unauthorized`.
- **React.** `setMessageReaction` sets the whole list of the bot's reactions, and
  "as non-premium users, bots can set up to one reaction per message": a new
  reaction replaces the earlier one, and a removal clears it. The adapter declares
  one reaction per reactor, so core applies it the same way, and a removal names
  no emoji it must match. Telegram lists 73 emoji a bot may set
  ([`ReactionTypeEmoji`](https://core.telegram.org/bots/api#reactiontypeemoji)): any
  other, `😂` among them, is rejected `unsupported` before a call, and custom emoji
  and paid reactions are not used. The list is byte for byte as Telegram prints
  it, several without the variation selector (the red heart is `U+2764` alone);
  the heart with `U+FE0F` is sent as the listed one, and nothing else is
  normalized. A chat may allow fewer (`available_reactions` of the chat), which
  only Telegram's refusal shows: `rejected`, `validation`.

## Limits

| What | Limit |
| --- | --- |
| Webhook ports | 443, 80, 88 and 8443 |
| Text of one message | 4096 characters |
| Inline keyboard callback data | 64 bytes |
| Rate | about one message per second in one chat, 20 per minute in one group, about 30 per second overall ([bot FAQ](https://core.telegram.org/bots/faq#my-bot-is-hitting-limits-how-do-i-avoid-this)); a 429 states the wait, which the operation honours |

## How it is tested

The shared adapter suite (`conformance.RunAdapterSuite`) runs six times over a
scripted bot: as the webhook alone and as the long-polling receiver (the suite drives a
receiving adapter through `Runtime.Receive` only, and Telegram's two ways exclude each
other), each once for each way a bot's chats divide into conversations: per
room (a group chat for each peer), per thread (one forum supergroup, a topic for
each peer) and per contact (a private chat with each user). The scripted bot
answers the documented shapes of the Bot API methods the adapter calls, with the
errors the Bot API is known to give (the reference documents none), and the webhook
calls are Updates carrying the secret token header. Package tests cover the secret
token, the update rules, mentions and commands, escaping for both parse modes
(read back by independent decoders of the documented rules), interactions, every
outcome, and the 403 a bot gets for a user who never started it.

The suite skips what the scripted bot cannot express: a revoked permission (a bot
token has no scopes), an echo of uma's own edit (no marker rides on a Telegram
message, so the scripted bot is told which operation made each call), a deletion
(Telegram tells a bot of none), a reaction summary, a message only some see, a
callback query with no message, an interaction whose deadline has passed (the
adapter sets its own from the moment it reads the update) and a submission.

The recorded test (`TestRecordedBot`) replays committed cassettes, and **none are
recorded yet**, so it skips. Behavior against the real Bot API, the phrases read in
its refusals most of all, is unverified, see
[Live verification](#live-verification-2026-11-16). To record, follow
[Record Telegram cassettes](../recording/telegram.md).

## Live verification (2026-11-16)

**Nothing was verified live, and no cassette was recorded.** The only bot
available is in service for another purpose: a process holds its update
stream, so `getUpdates`, `setWebhook`, `deleteWebhook`, `logOut` and `close` are
off limits (each takes the stream from that process or stops it), and the bot
has no group of its own for a test. A recording needs a group where the bot's
messages reach nobody else, and the run could not create one.

| Capability | Status |
| --- | --- |
| `send` (plain, HTML, MarkdownV2, inline keyboard, reply), `edit`, `react`, `delete` | Not run. Owner step 1 |
| The refusal phrases read in a 400 | Not run. Owner step 1 |
| `receive` by webhook | Not verifiable with this bot: a webhook cannot be set without displacing its current consumer. Owner step 2 |
| `receive` by long polling | Same reason, and polling needs the bot's update stream to itself |
| Redelivery (Telegram re-sends an update the webhook answered with an error, and keeps unconfirmed updates for 24 hours) | Needs the webhook or the polling above |
| Archive, history | Telegram has neither for a bot; not declared |

Owner steps:

1. Create a group, add the bot, and give the group's chat ID (a number
   starting `-100`). With the group's ID, the [recording](../recording/telegram.md)
   needs no `getUpdates` call: read the ID from the Telegram web client's
   address bar (`#-100...`) rather than from the bot. The recording then
   covers send, edit, reaction, deletion and the refusals.
2. For inbound, use a second bot made for the test (a bot's webhook and its
   long poll exclude each other, and so does any second consumer), set its
   webhook to the host, and from a second account write in the group: a
   message mentioning the bot, a reply, a command, an edit, and a button
   press on the inline keyboard.

## Known limits

- Two edits within one second of a message cannot be told apart: `edit_date` is
  in whole seconds.
- Telegram reports no deletion of a message to a bot, so none is reported.
- Reactions by users reach uma only when the bot is an administrator and
  `message_reaction` is in `allowed_updates`. With Telegram Premium a user may hold
  several reactions on a message, while the adapter declares one per reactor: the
  last one reported is the one uma keeps.
- `chat_id` takes a public channel's `@username` at Telegram; the adapter takes
  numeric IDs only.
- The phrases the adapter reads in a 400 ("message is not modified", "message can't be
  deleted", "chat not found" and the like) are what the Bot API answers in practice;
  its reference documents none. A phrase it changes degrades to a plain `validation`
  refusal.
- An inline keyboard on a message sent through inline mode, a game button and a callback
  query with no message are not consumed.
- Experimental: configuration may change ([Compatibility](../compatibility.md)).
