# Mapping provider concepts onto uma

Providers name things differently: a mailbox thread, a Slack channel, a Jira
issue, a WhatsApp number. uma has four shapes that hosts read: the
conversation, the message, the work item and the reply parent. This page says,
per adapter, which provider object becomes which, what references the adapter
reports so an inbox can choose a conversation, and which inbox modes work. It
also defines the inbox modes and the two ways a work item and a conversation
meet.

Each adapter's own page has the operations, limits and configuration; the
[capability matrix](adapters/README.md) says what each declares. This page is
the cross-adapter view.

## The shapes

| Shape | What it is |
| --- | --- |
| Conversation | uma's record that groups messages for one inbox. Which conversation an inbound message joins is decided at ingress by the inbox's selection mode, from what the envelope carries. Its reply route says where a reply goes |
| Message | One provider message: a mail, a chat post, an SMS, an issue description or comment. Its provider message ID is its identity where the provider assigns one (the capability table in [Adapters](adapters/README.md#identity-events-and-exchange-rules) says where) |
| Work item | The binding of one provider issue or ticket to a conversation, with the provider's last reported state (status, subject, assignees, fields). Only issue and ticket adapters create one |
| Reply parent | The earlier message an inbound message explicitly answers: `Message.InReplyTo`. It is set only when the inbox holds that message in the conversation the new message joins |

An adapter reports what the provider sent as an envelope. Beside the message it
fills `Envelope.Sender` (who wrote it) and `Envelope.References`: the room,
thread, work item or context the message belongs to, in the kinds below. The
inbox mode then reads those and nothing else; an adapter never picks the
conversation.

| Reference kind | Names | Read by mode |
| --- | --- | --- |
| `room` | A provider room or channel | `per_room` |
| `thread` | A provider thread; its parent ID is its room | `per_thread` |
| `work_item` | A provider issue or ticket | `per_work_item` |
| `context` | An agent context | `per_agent_context` |
| (the sender) | The identity that wrote the message | `per_contact` |
| `email.message_id`, `email.in_reply_to`, `email.references`, `email.list_id` | Normalized mail threading headers | per-contact email threading |

## Inbox modes

An inbox binds one channel's destinations to a selection (`model.Selection`):
a `mode`, and in `per_contact` mode also `lock_to_single` and `lifecycle_rules`.
The conversation an inbound message joins is chosen among the inbox's
conversations that share the message's scope: the candidate group. Candidates
are the conversations of the group that are not closed, plus the newest closed
one. A group may hold at most 256 candidates; more is a `conflict`, never a
truncated choice.

| Mode | Groups by | Needs from the envelope | Conversation owner | Reply route |
| --- | --- | --- | --- | --- |
| `per_contact` | The sender's identity in the inbox (an identity, not the contact it is linked to) | A sender. No sender is `validation` | The sender | The sender's reference |
| `per_room` | The `room` reference: namespace, parent ID and ID | A `room` reference; the first one scopes if there are several | None; senders join as participants | The room |
| `per_thread` | The `thread` reference, parent room included: thread `t1` of room A and thread `t1` of room B are two groups | A `thread` reference | None; senders join as participants | The thread |
| `per_work_item` | The `work_item` reference | A `work_item` reference. A sender is optional (a work item change has none) | None; senders join as participants | The work item |
| `per_agent_context` | The `context` reference | A sender, and exactly one `context` reference, or an envelope that continues an existing task (which takes the task's conversation). More than one `context` reference is `validation` | The sender who created it | The context |

### What a mode does with the candidates

`per_room`, `per_thread`, `per_work_item` and `per_agent_context` share one
rule: reuse the newest candidate that is not closed, whatever its status
(`open`, `pending` or `resolved`); never reopen one; add a new sender as a
participant; with no such candidate, create one. A closed conversation is
final: the next message in its scope opens a successor. Newest is by creation
time, then by the greater ID, so concurrent first messages converge on one
conversation.

`per_contact` follows the contact lifecycle (`lifecycle_rules` must be
`contact-lifecycle/v1`, the only rule set) over the sender's own conversations,
and `lock_to_single` decides how many a contact gets:

| `lock_to_single` | Reused | A resolved conversation | Closed |
| --- | --- | --- | --- |
| `true` | The sender's open or pending conversation: one live conversation per contact | Reused, and reopened by the sender's own message or interaction. An automated message (an auto-reply) attaches without reopening | Never reused; the next message opens a new one |
| `false` | The sender's open or pending conversation, newest first | Never reused: the next message opens a new conversation | Never reused |

`lock_to_single` has no default: `false` is a decision, not an unset value.
Changing it on a live inbox leaves existing conversations as they are.

An adapter that declares email threading (the mail adapters) adds the email
policy to `per_contact`. Threading headers decide first: `In-Reply-To`, then
`References` from last to first, are looked up among the inbox's stored
messages, sent and received. The conversation of the first msg-id found
decides, by its status: open or pending is reused, resolved is reopened by a
contact's message, closed gets a successor. A reply from someone else who was
copied (reply-all) joins by headers the same way and is added as a participant;
the conversation's reply route does not change. With no header match, `List-Id`
mail and every message of a `lock_to_single=false` inbox open a conversation per
thread, keyed by the thread's root msg-id (the first `References` entry, else
`In-Reply-To`, else its own `Message-ID`); other mail follows the table above.
A message whose msg-id is stored in two conversations is a `conflict`. Subject,
`Resent-*`, forward indicators and `Thread-Index` are never threading evidence.

An outbound start (`Conversations.Start`) goes through the same selection with
the recipient as the sender, and never reopens a conversation. It is supported
in `per_contact` (the recipient is a sender route), `per_room` (a `room`
recipient) and `per_thread` (a `thread` recipient); `per_work_item` and
`per_agent_context` inboxes refuse starts as `unsupported`.

An inbox may name a registered selection `policy`, which replaces the built-in
selector of its mode. The engine still validates what it returns.

### How validation fails

| When | Condition | Result |
| --- | --- | --- |
| Configuring the inbox | Unknown mode; `per_contact` without `lock_to_single` or without `lifecycle_rules`; an unknown rule set; `lock_to_single` or `lifecycle_rules` set in any other mode; a malformed or unregistered `policy` | `validation`; nothing is stored |
| Ingress | The envelope lacks what the mode needs: `envelope.references: per_room selection requires a room reference` (likewise `thread`, `work_item`, `context`); a missing sender in `per_contact` or `per_agent_context` | `validation`; the envelope is refused and leaves no trace |
| Ingress | Work item state, or a `work_item` envelope, in an inbox that is not `per_work_item`; a task binding outside `per_agent_context` | `validation` |
| Ingress | An agent-context inbox given neither a `context` reference nor a task to continue | `validation` |

Configuring an inbox does not check the mode against the channel's adapter. An
inbox in a mode the adapter can never satisfy (a `per_room` inbox on a mail
channel, a `per_contact` inbox on a ticket channel, whose issue events carry
work item state) is accepted, and fails when the first such message arrives.
Pick the mode from
the per-adapter tables below.

## Two ways a work item meets a conversation

A work item can reach a conversation in two unrelated ways. They differ in who
creates them, what the conversation then is, and what they do at the provider.

| | `per_work_item` ingress binding | `Conversations.Link` to a work item |
| --- | --- | --- |
| Made by | The engine at ingress, from a `work_item` reference on the provider's webhook or poll | A host, calling `Conversations.Link` |
| Needs | An issue or ticket adapter and a `per_work_item` inbox | A conversation of any inbox and a work item that already exists |
| What it means | The conversation is the work item: its messages are the issue's description and comments, its reply route is the issue | The conversation is something else (an email thread, a chat) that the desk associates with the work item |
| Held by | The work item record, keyed by its conversation: one work item per conversation, with its binding state (`active`, `merged`, `moved`, `deleted`, `inaccessible`) and history | The linking conversation alone (`Conversation.Links`); the work item's own conversation does not learn of it. A link between two conversations, by contrast, is held by both |
| Effect at the provider | A reply posts a comment; `WorkItems.Act` can change the status | None. A link is association only: it merges no history, changes no reply scope and is never sent |
| Visibility | As the conversation and the work item | Desk-only: `Get` and `List` zero `Links` without `conversation.read_workflow`; a reader sees only the links whose far end it may read |
| Authorization | `channel.receive` for the receiver; `work_item.act` for a status change | `conversation.link` on the conversation, `work_item.read` on the work item |
| Event | `work_item.updated`, for the changed fields | `conversation.linked` for the linking conversation, `conversation.unlinked` on removal |
| Limit | One work item per conversation | At most 64 links per conversation |

In both cases the work item's status and the conversation's status are
independent: closing an issue never closes the conversation, and moving the
conversation never reaches the provider. A work item only ever exists through
the first way, so a link always points at a conversation that is itself a
`per_work_item` conversation.

## Per adapter

Every table has the same rows. "References" are what the adapter reports for the
inbox to scope by; "Modes" are the inbox modes that work with them. A mode not
listed fails at ingress as described above.

### Mail

Mail is one mode: `per_contact`. The adapters report no room, thread or work
item, only the sender's address and the normalized threading headers. A Gmail
`threadId` or a Graph `conversationId` is kept in the message payload as a hint
and never selects a conversation. Cc and Bcc recipients are recorded in the
payload, not as participants.

#### `smtp.mail`, `imap.mail`, `http.mail`

| | |
| --- | --- |
| Conversation | A sender's mail under `lock_to_single=true`; one thread under `false` (see [Inbox modes](#what-a-mode-does-with-the-candidates)) |
| Message | One received mail, identified by its destination and a hash of its bytes: the sender chooses `Message-ID`, so it threads but is not identity |
| Work item | None |
| Reply parent | The first valid `In-Reply-To` msg-id, resolved to a stored message by `Message-ID` |
| References | `email.*` threading headers |
| Sender | The `From` address (`<adapter>.address`, kind `address`) |
| Modes | `per_contact` only |

#### `google.mail`, `microsoft.mail`

| | |
| --- | --- |
| Conversation | As above. The provider thread is provider state only: archive, labels, read and star verbs act on the provider thread of the conversation's messages |
| Message | One mail, identified by the provider's message ID (Gmail message ID, Graph message ID); the RFC 5322 `Message-ID` is also stored as an `email.message_id` reference |
| Work item | None |
| Reply parent | The first valid `In-Reply-To` msg-id |
| References | `email.*` threading headers |
| Sender | The `From` address (`<adapter>.address`, kind `address`). On Gmail the reference also carries the provider message the address wrote, so a reply anchors on it |
| Modes | `per_contact` only |

### SMS, WhatsApp and relay

All four have no thread or room: the number is the sender, and the conversation
is that number's. Use `per_contact` with `lock_to_single=true`.

| Adapter | Conversation | Message | Reply parent | Sender |
| --- | --- | --- | --- | --- |
| `twilio.sms` | One phone number | An SMS or MMS, by Twilio message SID | None | The E.164 number |
| `twilio.whatsapp` | One WhatsApp number or user ID | A WhatsApp message, by Twilio message SID | `OriginalRepliedMessageSid` | The number, or a `CC.` user ID when WhatsApp hides the number |
| `meta.whatsapp` | One WhatsApp user | A WhatsApp message, by `wamid` | `context.id`; a forward has none | The WhatsApp user |
| `relay.messages` | One number (or, on iMessage, an email address) as the gateway reports it | A message, by the gateway's ID (chosen by the gateway, so not identity) | None | The `from` the gateway reports, in `relay.sms`, `relay.whatsapp` or `relay.imessage` |

For all four: work item none; references none beyond the message's own provider
reference; modes `per_contact` only.

### Chat

The chat adapters report a room on every message, a thread where the provider
has one, and the user as sender, so three modes work. What differs is what a
thread is, and what happens to thread messages in a room.

#### `slack.chat`

| | |
| --- | --- |
| Conversation | `per_room`: a Slack conversation (channel, private channel, group DM, DM). `per_thread`: a thread; a message outside a thread roots its own. `per_contact`: one user, replies go to the user's DM |
| Message | A Slack message, `<conversation ID>:<ts>` |
| Work item | None |
| Reply parent | For a thread reply, the thread root; Slack names no other parent. A top-level message or a root has none |
| References | `room` (the conversation ID) on every message; `thread` (the `ts`, parent the conversation) on every message too: the thread it replies in, or the one it roots |
| Sender | The Slack user (`slack.chat.user`) |
| Modes | `per_room`, `per_thread`, `per_contact` |

#### `discord.chat`

| | |
| --- | --- |
| Conversation | `per_room`: a channel or DM; a thread's messages belong to the channel it hangs off. `per_thread`: a thread channel or forum post. `per_contact`: one user, replies go to the user's DM |
| Message | A Discord message, `<channel ID>:<message ID>` |
| Work item | None |
| Reply parent | The referenced message of a reply |
| References | `room` (the channel; for a thread message, its parent channel) and, for a message in a thread, `thread` (the thread channel, parent the room) |
| Sender | The Discord user (`discord.chat.user`) |
| Modes | `per_room`, `per_thread`, `per_contact` |

#### `telegram.messages`

| | |
| --- | --- |
| Conversation | `per_room`: a chat (private, group, supergroup, channel). `per_thread`: a forum topic. `per_contact`: one user, replies go to the private chat |
| Message | A Telegram message, `<chat ID>:<message_id>` |
| Work item | None |
| Reply parent | `reply_to_message`; a forum topic's root message is thread membership, not a reply |
| References | `room` (the chat ID) on every message; `thread` (the topic ID, parent the chat) only for a message in a topic |
| Sender | The Telegram user, or a chat speaking as itself (`telegram.messages.user`) |
| Modes | `per_room`, `per_thread`, `per_contact` |

#### `microsoft.teams`

| | |
| --- | --- |
| Conversation | `per_room`: a Teams conversation (personal chat, group chat, channel). `per_thread`: a channel thread; chats have none. `per_contact`: one user, replies go to the personal chat |
| Message | A Teams activity, `<conversation ID>\|<activity ID>`, the conversation without its thread suffix |
| Work item | None |
| Reply parent | The activity's `replyToId` |
| References | `room` (the base conversation ID) on every message; `thread` (the root post, parent the channel) for a channel message: the thread it replies in, or the one a root post starts |
| Sender | The Teams user (`microsoft.teams.user`) |
| Modes | `per_room`, `per_thread` (channels only), `per_contact` |

#### What roots a thread

| Adapter | A message in a `per_thread` inbox |
| --- | --- |
| `slack.chat` | Every message has a thread: the one it replies in, or the one it roots. That includes top-level messages in channels, DMs and group DMs, so each top-level message opens its own conversation and its replies join it. A DM seldom has threads, so a `per_thread` inbox on DMs yields one conversation per message |
| `discord.chat` | Only a message inside a thread channel. One outside any thread has no thread and is refused |
| `telegram.messages` | Only a message in a forum topic. One outside a topic has no thread and is refused |
| `microsoft.teams` | Only a channel message (a root post starts a thread). A chat message has no thread and is refused |

### Agents and web

| | `a2a.messages` | `native.webwidget` |
| --- | --- | --- |
| Conversation | An A2A context (`contextId`); every request names one, or the adapter allocates a new one | `per_contact`: one visitor or verified user, across sessions. `per_room`: one session of a visitor |
| Message | An A2A message, its ID derived from the agent and the client's `messageId` | An entry of the visitor's log: a visitor message, or one the widget sent |
| Work item | None; an A2A task is a uma task, held in the context's conversation, not a work item | None |
| Reply parent | None | None |
| References | `context`; a task reference when a request continues a task | `room` (ID the session, parent the visitor) |
| Sender | The calling agent principal (`a2a.principal`, kind agent) | The visitor or user (`native.webwidget.visitor`, `native.webwidget.user`) |
| Modes | `per_agent_context` only | `per_contact` (the end-to-end tests); `per_room` through its `room` reference |

### Issues and tickets

All five map an issue or ticket to a work item bound to a conversation. The
inbox must be `per_work_item`: the work item state and the `work_item`
envelopes these adapters send are refused by any other mode. A reply to the
conversation posts a comment on the issue; the reply route is the issue's
`work_item` reference. A conversation is not closed or reopened by the issue's
state ([independent lifecycles](#two-ways-a-work-item-meets-a-conversation)).

| Adapter | Work item reference (ID, parent) | Inbox destination | Messages | Sender |
| --- | --- | --- | --- | --- |
| `github.issues` | Issue number, parent the repository full name | The repository | The description (`issue:<id>`) and each comment (`comment:<id>`) | GitHub user ID |
| `gitlab.issues` | Issue `iid`, parent the numeric project ID | The project path | The description (`issue:<id>`) and each note (`note:<id>`) | GitLab user ID |
| `jira.issues` | The numeric issue ID, no parent (it survives key changes and moves) | The project key | The description (`issue:<id>`) and each comment (`comment:<id>`) | Atlassian `accountId` |
| `linear.issues` | The issue UUID | The team key | The description (`issue:<uuid>`) and each comment (`comment:<uuid>`) | Linear user UUID |
| `zendesk.tickets` | The ticket ID | The brand ID | Each ticket comment, public or an internal note (restricted audience) | Zendesk user |

For all five: reply parent none (the providers' comments name no replied-to
comment); references the one `work_item` reference; modes `per_work_item` only.
Work item statuses map as each page says; only some declare `work_item_act`.

## Behavior to know

### Mail

**One mode.** A mail inbox is `per_contact`. A `per_room`, `per_thread`,
`per_work_item` or `per_agent_context` inbox on a mail channel is accepted when
configured and refuses every inbound mail with `validation` (`envelope.references:
per_room selection requires a room reference`), because mail reports none of
those references. A start fails the same way. Choose the grouping with
`lock_to_single`: `true` for one conversation per correspondent, `false` for one
per thread.

**Importing a conversation reads from its sender, not from its thread.**
`Runtime.Import` with a `Conversation` asks the adapter for mail from the
conversation's reply route, which in mail is the address that started it (Gmail
`from:`, Graph a `from` filter, IMAP `SEARCH FROM`). The reader returns every
message in the window from that address, in every thread, and each goes through
normal ingress. So:

- With `lock_to_single=false`, or for list mail, messages of other threads are
  accepted into their own conversations, which the import creates. The report
  counts them as imported though they are not in the conversation asked about.
  With `lock_to_single=true` and ordinary mail with no header tie to another
  conversation, they land in the one conversation.
- A reply written by someone else, such as a copied colleague, joins the thread
  by its headers when it is received live, but an import of that conversation
  does not fetch it: it is not from the route's address. Nor are replies the
  mailbox sent from another client. Import covers received mail only, from the
  receive mailbox.

To backfill a whole mailbox, import the channel instead of a conversation.

**Copies, forwards, sent mail.**

- Cc and Bcc recipients are recorded on the message payload. They are not
  participants; a Cc'd person who replies joins the thread and is added as a
  participant. No mail adapter declares `membership`: a send carries one route.
- A forward is not a reply. A forward indicator in the subject, `Resent-*`,
  `X-Forwarded-Message-Id` and `Thread-Index` are not threading evidence, and a
  forward sets no reply parent: it is placed like any mail with no header match
  (a new conversation under `lock_to_single=false`).
- Mail the mailbox sent outside uma is not imported: history reads received mail
  only, so a thread can show its received side plus what uma itself sent. uma's
  own sends are stored with their provider IDs and thread normally.

### Chat

**A Slack `per_thread` inbox makes a conversation of every top-level message.**
Slack reports a thread for every message, so there is no message outside a
thread: a top-level post in a channel, in a DM or in a group DM roots its own
conversation, and replies to it join. Pick `per_room` for channel-level
conversation, `per_contact` for one conversation per user. Slack sends
no archive events for DMs and group DMs, so nothing follows their archive state;
in a `per_thread` inbox a channel's archive event updates only the one
conversation holding the channel's newest message.

**In `per_contact` a reply parent resolves only inside the sender's own
conversation, and a reply goes to the user's direct message.** A per-contact
conversation holds one user's messages from every channel. A thread reply names
its root as the reply parent, and uma sets `in_reply_to` only if that root is a
stored message of the same conversation: that holds when the user replied to
their own earlier message, and not when they replied to someone else's, which is
in another conversation or not stored. The reply route is the user, so a
reply to a message written in a channel is posted as a new message in the user's
DM; it is never posted in the channel or its thread. A reply that answers a
message written in the DM itself is threaded on it. The same holds for the other
chat adapters: `per_contact` replies go to the user's private chat.

**Thread messages in a `per_room` inbox, live and imported.** Live, thread
messages carry their room, so a `per_room` inbox keeps them in the room's
conversation (Slack, Discord, Telegram forum topics and Teams channel threads
alike). History does not match: Slack's room read returns top-level messages and
thread broadcasts, Discord's returns the channel's own messages, and a thread's
replies live in the thread. Both adapters report a `gap` for a message that heads
a thread, and `Runtime.Import` counts it in `Gaps`. Importing a `per_room`
conversation therefore has the thread parents but not their replies, and the
conversation's route is the room, so no import of it reads them. Replies are
read by a thread route, which only a `per_thread` conversation carries. Telegram,
Teams and WhatsApp declare no `history`.

| Adapter | Live thread message in `per_room` | Import of the room reads | Thread replies by import |
| --- | --- | --- | --- |
| `slack.chat` | Joins the room's conversation | Top-level messages and thread broadcasts; a `gap` for each thread parent | Only through a `per_thread` conversation (`conversations.replies`) |
| `discord.chat` | Joins the parent channel's conversation | The channel's own messages; a `gap` for each message with a thread | Only through a `per_thread` conversation (the thread channel) |
| `telegram.messages`, `microsoft.teams` | Joins the room's conversation | No `history` | None |
