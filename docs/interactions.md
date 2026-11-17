# Interactions

A visitor presses a button, types a slash command or sends a form. The
provider calls that an interaction and expects an answer quickly. uma stores
it as an interaction record, never as a message, hands it to your handler and
sends the handler's answer back through the adapter that reported it.

This page covers what arrives, how a record moves from report to answer, who
holds the provider's response token, how to respond, and which adapters and
companions take part. The method reference is in
[API coverage](api-coverage.md#interactions); the bot companion's routing is
in its package documentation.

## What arrives

| Kind | What the provider reported | Names | Recorded |
| --- | --- | --- | --- |
| `command` | A slash or bot command | `Command`, with the raw text after it in `Arguments` | yes |
| `action` | A button press or menu selection | `Action`, the provider's action ID, and `Arguments` when the control carried a value | yes |
| `submission` | A form or modal | `Fields`: the submitted values under a named schema | yes |
| `options` | A request for dynamic options or field validation | `Fields` | no: evaluated and answered at once, never stored |

Every record also carries its actor (resolved like a message sender), the
conversation, the provider message the control sits on when there is one
(`MessageExternal`) and the local message holding it (`Message`), the
audience, the time it was reported and recorded, and the provider's two
limits: the acknowledgement `Deadline` and, when the provider states one,
`RespondUntil`, the end of the window in which a response is still possible.

A schema name in `Fields.Name` is a lowercase dotted registry key such as
`org.example.feedback`; anything else is refused at ingress.

`Interactions().Get` and `List` read the records. Reading needs
`interaction.read` under the interaction's audience.

## From report to answer

1. **Report.** The adapter normalizes what the provider sent into an
   interaction envelope and hands it to the engine (`Sink.Accept`).
2. **Record.** In one transaction the engine claims the actor's identity, finds
   the conversation (the one the source message belongs to when the actor
   takes part in it, otherwise the inbox's selection decides), stores the
   record with the provider's response token beside it, and queues one
   `interaction.received` event. No message is written. The same provider
   interaction ID reported again is a duplicate: it is acknowledged again and
   recorded once. `Message` is the local message holding the control's
   provider message ID, when the adapter assigns provider message IDs and
   one message holds it. A control can be pressed before the send that
   posted it has recorded that ID (the provider shows the message before
   its answer reaches uma): the interaction is then recorded with
   `MessageExternal` alone, and the unit that records the send's answer
   links it, setting `Message` under a new revision. The link emits no
   event, as recording a sent message's provider IDs emits none, and the
   `interaction.received` payload keeps naming no message; a handler that
   needs the control's message reads the interaction, or answers in
   `update` mode, which targets `MessageExternal`.
3. **Acknowledge.** Only now does the adapter acknowledge the provider.
4. **Handle.** A subscriber reads `interaction.received`, fetches the record
   with `Interactions().Get` and decides.
5. **Respond.** `Interactions().Respond` queues an operation. A worker
   (`Runtime.Run`) loads the record and token, rechecks the window and the
   actor's authorization, and calls the adapter.
6. **Settle.** The record's `Response` shows the operation that won the initial
   response (`Initial`) and the number of follow-ups. Redaction clears
   `Arguments` and `Fields` and the stored token and keeps references, actor
   and response state.

### The acknowledgement is deferred

A provider's acknowledgement means "received", not "answered". An adapter
acknowledges after step 2, so a provider that retries an unacknowledged
interaction never produces a second record and one that is acknowledged can
rely on uma holding the interaction durably. The answer comes later, from the
handler, inside the respond window.

A handler therefore never answers inside the provider's request. It returns a
response through `Respond` (or `FollowUp`), and uma delivers it. An
interaction whose acknowledgement deadline has passed is still recorded: the
response may still be possible, and judging a late acknowledgement is the
provider's.

`Messages().Reply` may name the interaction (`InResponseTo`). That correlates
the reply with the interaction and is announced; it is not a response, does
not claim the initial response and consumes no token.

## Token custody

Some providers give each interaction a credential for answering it, such as a
response URL or an interaction token, with an expiry.

- The adapter reports it once, in the envelope. The engine stores it beside
  the interaction record and removes it from the raw ingress copy.
- It is never returned: not by `Get` or `List`, not in an event, an
  operation, attempt evidence or an error.
- It is read at the attempt, held in memory for that one provider call and
  handed to the adapter only. Formatting the adapter's interaction context
  never prints it.
- A provider that has none, such as the browser widget which answers through
  its own event log, is answered with an empty token.
- Redaction deletes it.

## Respond

```go
acc, err := c.Interactions().Respond(ctx, model.Respond{
	Command:     model.Command{Call: model.Call{Correlation: corr}, Operation: opID},
	Interaction: in.Ref,
	Mode:        model.ResponseModeMessage, // message | ephemeral | update
	Audience:    model.Audience{Visibility: model.Public},
	Content:     content,
	ExpiresAt:   in.RespondUntil, // when the provider states a window; never later
})
```

Inside a bot handler, `Turn.Respond` and `Turn.FollowUp` do the same with
operation IDs derived from the event, so a redelivered event replays its
responses instead of answering twice.

| Mode | Shows | Audience | Local message |
| --- | --- | --- | --- |
| `message` | A new message answering the interaction | public | committed with the operation, origin `response`, `in_response_to` the interaction |
| `ephemeral` | Content only the interaction's actor sees | restricted | none |
| `update` | Replaces the provider message the control sits on | public | none |

The mode and the audience are both explicit. The engine never derives one from
the other and never falls back to another mode or a wider audience: a
response the adapter cannot give as asked is a rejected outcome.

**Initial response.** There is one per interaction, claimed atomically with
the operation. Of two concurrent initial responses one is accepted and the
other fails `conflict`; a bot handler sees that as an error wrapping
`bot.ErrResponseClaimed`, which ends the event as ignored. The same operation
ID and payload replays the accepted response, also after its window.

**Follow-ups.** `FollowUp: true` needs the adapter to declare
`respond_follow_up` (otherwise `unsupported`). A follow-up counts one, never
consumes the initial claim and may come before or after the initial response.
`Interactions().Ephemeral` naming an `Interaction` is a follow-up to the actor.

**Window.** `ExpiresAt` may not pass `RespondUntil`. When the provider states
no window the bot companion bounds the response with `Config.RespondWindow`
(15 minutes by default) after the record was recorded. A passed window is
`deadline_expired`, at submit and again at the attempt, which then makes no
provider call.

Responding needs `interaction.respond` on the interaction. A redacted
interaction refuses a new response with `conflict`.

## Streams and Interactions

Both answer a person, and they do different jobs.

| | Streams | Interactions |
| --- | --- | --- |
| Starts with | Your code, on a conversation | The person, with a structured action |
| Direction | Outbound only | Inbound record, then an outbound answer |
| Stored as | A stream record in the conversation | An interaction record; a `message` response commits a message, the others none |
| Capability | `stream` (native), or `send` and `edit` (edit mode), or `send` (buffer) | `respond`, and `respond_follow_up` for follow-ups |
| Limits | Sequences, one final state | One initial response, any number of follow-ups, a provider window |

Use a stream to show a long answer growing. Use an interaction response to
answer a press, a command or a form. A handler may do both: respond to the
interaction, then stream the real answer into the conversation.

## Adapters and companions

Only an adapter that declares `interaction.received` reports interactions, and
only one that declares `respond` answers them. An adapter that does not is
`unsupported` for `Respond`, before any claim or provider call. An account that
only receives, such as the webhook variant of the scripted provider in
`examples/fakeprovider`, reports interactions and cannot answer them.

| Adapter | Type | `interactions` |
| --- | --- | --- |
| Web widget | `native.webwidget` | events and `respond`, `respond_follow_up` |
| Slack | `slack.chat` | events and `respond`, `respond_follow_up` |
| Telegram | `telegram.messages` | events and `respond` |
| Microsoft Teams | `microsoft.teams` | events |
| Discord | `discord.chat` | events and `respond`, `respond_follow_up` |
| SMTP mail | `smtp.mail` | none |
| IMAP mail | `imap.mail` | none |
| HTTP mail | `http.mail` | none |
| Gmail | `google.mail` | none |
| Microsoft Graph mail | `microsoft.mail` | none |
| Twilio SMS | `twilio.sms` | none |
| Twilio WhatsApp | `twilio.whatsapp` | none |
| WhatsApp Cloud API | `meta.whatsapp` | none |
| GitHub issues | `github.issues` | none |
| GitLab issues | `gitlab.issues` | none |
| Zendesk Support | `zendesk.tickets` | none |
| Linear | `linear.issues` | none |
| A2A server | `a2a.messages` | none |

`interactions` is `events` when the descriptor lists `interaction.received`,
plus the response capabilities it declares. Run the adapter suite's
`Interactions`, `InteractionIngress` and `Unsupported` groups to check an
adapter's claim.

### The web widget

The widget has no provider to give it a token. It accepts what the visitor
sends over its receiver, answers through the visitor's own event log, and
shows the answer on the event stream.

| Route (last path segment) | Body | Answer |
| --- | --- | --- |
| `actions` | `id`; `action` and optional `value`, or `kind: "command"` with `command` and optional `arguments`; optional `message` (the control's message) and `fields` | 202 `{"interaction", "duplicate"}` once the engine holds it |
| `submissions` | `id`; `fields` (`name`, `version`, `data`); optional `message` | 202, as for an action |
| `interactions` | `id`, `kind`, `fields` | 200 with field errors and options; evaluated by the host's `InteractionValidator`, never recorded |

The visitor is the actor, taken from the signed session token, never from the
body. The client-chosen `id` makes a retry a duplicate. The `interaction` in
the 202 answer is the ID an `interaction.response` event names. The widget
states no respond window; an ephemeral response stays on screen for 15
minutes.

An outbound message can carry controls, a structured part named
`native.webwidget.controls` at version 1: buttons, selects and one form. The
page renders it, and a press or a submit comes back through the routes above.
A structured part the page does not know is left out, never turned into text.

The runnable [bot quickstart](../examples/bot) shows the whole path: a `/help`
command and a Help button, both answered by one handler. The same bot wired by
hand is [`examples/botexplicit`](../examples/botexplicit); the
[bot host guide](bot-host.md) explains the difference.

### Slack

Slack posts a button press, a select, a slash command and a modal submission to
the app's request URL, signed, and expects an empty 200 inside three seconds.
`slack.chat` records the interaction first and acknowledges after, so the
acknowledgement means "held", not "answered". The provider's response token is
the interaction's `response_url`, stored as its path and valid for thirty minutes
and five posts; `Respond` and `FollowUp` post to it, and a response that is
refused or past its window is final. A modal submission carries its `response_url`
only when its view asked for one, and a submission with none cannot be answered.
A modal is not a response: Slack opens one only with the `trigger_id` of an
interaction, within three seconds, so it is an extension effect the host queues
(`Extensions().Execute`, `slack.TriggerOf`), not `Respond`.
See [slack.chat](adapters/slack.chat.md#interactions) and
[Modals](adapters/slack.chat.md#modals).

### Telegram

Telegram reports the press of an inline keyboard button as a callback query and a bot
command as an ordinary message that starts with a command entity; `telegram.messages`
records either as an interaction and acknowledges the webhook after. A callback query
is answered once, with `answerCallbackQuery`, whatever else a response does: that is
what clears the client's progress bar. The query ID is the response token, and
Telegram states no window for it, so the adapter sets its own, short one (30
seconds). `Respond` posts a message, shows the notification the query carries (the
`ephemeral` mode, up to 200 characters, to the person who pressed) or edits the
message the button sits on; a command has no query, so only a message answers it, and
there are no follow-ups. See [telegram.messages](adapters/telegram.messages.md#interactions).

### Microsoft Teams

Teams posts the press of an Adaptive Card `Action.Execute` as an `invoke` activity
named `adaptiveCard/action`; `microsoft.teams` records it as an action interaction
(the action's verb, its data as the arguments, the card's message) and answers the
invoke after, with an empty message that leaves the card as it is. Teams gives no
response credential and takes an answer to the invoke only in the HTTP response,
within five seconds, so the adapter reports interactions and does not declare
`respond`: answer with a reply naming the interaction, or an edit of the card's
message. See [microsoft.teams](adapters/microsoft.teams.md#interactions).

### Discord

Discord sends a slash command, a button or select and a modal submission over the
gateway, or, once the application sets an Interactions Endpoint URL, to it, signed
with Ed25519, and wants an answer inside three seconds. `discord.chat` records the
interaction first and acknowledges after with a deferral: a loading response for a
command, an update later for a control on a message. The interaction token is the
response token, valid fifteen minutes; `Respond` edits the deferred response or posts
a followup through the interaction's webhook, and `Interactions.Ephemeral` naming the
interaction posts a followup only its user sees. A command's deferral is public, so
its initial response is too. See [discord.chat](adapters/discord.chat.md#interactions).

### The bot companion

`companions/bot` routes `interaction.received` events to `Commands`, `Actions`
and `Submissions` handlers by command name, action ID and form schema name,
then to a `Kinds` fallback. It reads the record, sets `Turn.Interaction` and
answers with `Turn.Respond` and `Turn.FollowUp`. A lost initial claim ends the
event as ignored and an expired window drops it.
