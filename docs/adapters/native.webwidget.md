# native.webwidget

`native.webwidget` is a browser chat channel the host serves over its own HTTP
mux. There is no remote provider: uma is the provider. Visitors post messages
over JSON and read replies over a Server-Sent Events stream. It uses the
standard library and public uma packages only.

| | |
| --- | --- |
| Key | `native.webwidget` |
| Package | `hop.top/uma/adapter/web/widget` (root module, standard library only); durable log in `hop.top/uma/adapter/web/widget/storelog` |
| Capabilities | `receive_http`, `send`, `start_conversation`, `stream`, `send_ephemeral`, `signal`, `edit`, `delete`, `react`, `receipt`, `respond`, `respond_follow_up` |
| Events | `message.received`, `delivery.updated`, `interaction.received` |
| Identity namespaces | `native.webwidget.visitor` (anonymous), `native.webwidget.user` (host-verified) |
| Sender kinds | person |
| Provider permissions | none; the session key is a secret of at least 32 bytes |
| Message IDs and threading | assigned by uma; no threading |
| Reply parent | none |
| Interactions | reports `action`, `command` and `submission`; answers with `respond` and `respond_follow_up` ([interactions](../interactions.md)) |
| Maturity | experimental |

The matrix of all adapters is in [Adapters](README.md).

## Capabilities and operations

| Capability | Behavior |
| --- | --- |
| `receive_http` | `Runtime.Webhook` serves the receiver; the route is the last path segment, so the host mounts it under any prefix |
| `send` | `Messages.Reply` appends the message to the visitor's log and pushes it to open streams |
| `start_conversation` | `Conversations.Start` appends the first message to the log of a visitor or user who never wrote in; they read it when their stream next opens |
| `stream` | Native streams: `Streams.Start`, `Update`, `End` |
| `send_ephemeral` | `Interactions.Ephemeral`: shown to one visitor until it expires |
| `signal` | `Interactions.Signal`: a transient signal such as typing |
| `edit`, `delete`, `react` | Any message of a visitor's log can be changed through the engine, public audience only |
| `receipt` | `Messages.Receipt` appends a `receipt` event naming a visitor's message and the kind, `delivered` or `read`; see [Receipts](#receipts) |
| `respond`, `respond_follow_up` | `Interactions.Respond` appends an `interaction.response` event |

An applied delivery is a log append: the operation settles `provider_accepted`
once the log stored it, and a lost answer settles `outcome_unknown` and is never
resent.

## Not declared

Reason classes are defined in [Adapters](README.md#why-a-capability-is-not-declared).

| Capability | Reason | Why |
| --- | --- | --- |
| `reply_recipients` | `provider` | A widget conversation is with one visitor or user |
| `receive` | `by design` | uma is the provider; the HTTP receiver is the only ingress |
| `reconcile` | `by design` | Delivery is a log append; a lost answer settles `outcome_unknown` and is never resent |
| `membership` | `by design` | A widget conversation has exactly one visitor |
| `history` | `by design` | uma already holds every message; there is nothing to import |
| `refetch` | `by design` | The log has no provider revisions to re-read |
| `work_item_act` | `provider` | The widget has no work items |
| `internal_note` | `by design` | Visitors never see notes; they stay in uma |
| `extension_execute` | `by design` | Nothing lives outside uma to act on |
| `extension_query` | `by design` | Nothing lives outside uma to read |
| `probe_permissions` | `by design` | There is no provider credential; the session key is checked on every request |
| `archive` | `by design` | uma is the store; there is no provider-side state to mirror |
| `move` | `by design` | uma is the store; there is no provider-side state to mirror |
| `read_state` | `by design` | uma is the store; there is no provider-side state to mirror |
| `flag` | `by design` | uma is the store; there is no provider-side state to mirror |
| `trash` | `by design` | uma is the store; there is no provider-side state to mirror |
| `raw_read` | `not built` | Raw read is built for mail only; the provider's whole copy of a message is not read |

## Surfaces

| Surface | Route | Behavior |
| --- | --- | --- |
| Receiver | `POST .../session` | Issues a session token for the visitor the host verifies |
| | `POST .../messages` | Hands a visitor message to the engine |
| | `POST .../uploads` | Stages a visitor file through the engine's attachment sink |
| | `POST .../actions`, `.../submissions` | Hand a button press, command or form to the engine as an interaction; 202 once the engine holds it |
| | `POST .../interactions` | Evaluates a form synchronously through the host's validator; records nothing |
| Event stream | `Factory.Events` (GET) | SSE to one visitor, from the adapter's own log only; resumes from `Last-Event-ID` |

Receiver bodies are short JSON POSTs of at most 64 KiB, and a request without an
`application/json` content type is refused (an upload is the exception).

## Permissions

- **Sessions.** The host decides who a visitor is. `SessionVerifier` inspects the
  session request and returns a visitor with an explicit assurance:
  `AssuranceVerified` for a host-authenticated user, `AssuranceAnonymous`
  otherwise. There is no default; an empty assurance is refused. Anonymous
  visitors and users live in separate identity namespaces, so an anonymous
  visitor can never assume a verified user's identity by choosing the same ID.
- **Token.** A session token is signed with HMAC-SHA256 under the channel's
  session key, resolved through `ports.Credentials` for purpose
  `native.webwidget.session` on every request and never cached (a secret of at
  least 32 bytes). MACs are compared in constant time, then the channel, expiry
  and site are checked. Rotating the key ends every session; removing a site
  ends its sessions. The token travels as a bearer token; the stream also
  accepts it as a `token` query parameter because `EventSource` cannot set
  headers, so keep the TTL short and the stream URL out of access logs.
- **uma.** The receiving principal needs `channel.receive`; sends are authorized
  as `message.reply`, edits, deletes and reactions as `message.edit`,
  `message.delete` and `message.react`, receipts as `message.receipt`, streams
  as `stream.write`, ephemeral messages as `message.ephemeral`, signals as
  `conversation.signal` and responses as `interaction.respond`.

## Configuration

`native.webwidget.config` version 1: `widget.Configuration(sites...)` builds it.

| Field | Required | Meaning |
| --- | --- | --- |
| `sites` | yes | The sites (inbox destinations) the channel serves. A session names one of them; with exactly one, the name may be omitted |

Host options (`widget.Options`):

| Option | Meaning |
| --- | --- |
| `Verifier` | Required. Verifies a session request and returns the visitor |
| `Log` | The visitor log. Default an in-memory log keeping 256 entries per visitor |
| `SessionTTL` | Session token lifetime; default 1 hour |
| `Heartbeat` | Idle interval after which a stream sends a comment; default 15 seconds |
| `InteractionTimeout` | Bounds synchronous evaluation of a submission, and is the acknowledgement deadline of an accepted interaction; default 10 seconds |

## Receipts

`Messages.Receipt` tells a visitor that a message they sent was delivered or
read. Both kinds are expressible; any other kind is refused `unsupported`
without a log append. The target is a message the visitor sent (an ID starting
with `v`); a message the widget sent is refused, as a receipt answers a
received message. The route is the conversation's visitor or session, and the
audience must be public.

The receipt is one entry in the visitor's log, delivered like a message, so the
operation settles `provider_accepted` once the log stored it. The stream sends
it as an event named `receipt`:

```
id: 7
event: receipt
data: {"type":"receipt","message":"v3f2a...","kind":"read"}
```

| Field | Meaning |
| --- | --- |
| `type` | `receipt` |
| `message` | The ID of the visitor's message, as the receiver returned it |
| `kind` | `delivered` or `read` |

- **Scope.** The receipt reaches the visitor of the conversation, and only the
  session of a per-room conversation, like every other entry of the log.
- **Once.** A retried submission with the same operation ID is replayed by the
  engine and appends nothing more; a lost log answer settles `outcome_unknown`
  and is never resent. A client that sees the same message and kind again keeps
  the same state.
- **Old clients.** A client ignores event types it does not know, and an
  `EventSource` dispatches a named event to its own listener only, so a page
  that does not listen for `receipt` shows nothing.

## Delivery and durability

Outbound effects are appended to the visitor's log and pushed to every open
stream of that visitor. Server history is authoritative: the client only resumes
from a cursor, each event carries its log sequence as the SSE `id`, and when the
log no longer retains the events after the cursor the stream first sends a
`reset` event. Ephemeral replies and signals are not replayed after they expire.

The default log is in memory and per process: run the receiver, workers and
streams in one process, or supply a shared `Log`. With the default, a restart
loses every entry no stream read yet. A host with a durable store should use
`storelog`, which keeps each visitor log in the uma store as a recipient log, so
privacy erasure covers it; a log outside the uma store is also outside erasure.

## Controls

An outbound message can carry a structured part `native.webwidget.controls`
(version 1) holding buttons, selects and one form. The adapter checks the shape
when the part is sent and delivers it unchanged; the page renders it and sends a
press as an action and a form as a submission. A structured part of any other
schema is delivered unchanged and never turned into text.

## Limits

| What | Limit |
| --- | --- |
| Receiver body | 64 KiB, and the engine's webhook limit |
| Session key | at least 32 bytes |
| Controls per message | 25; options per select 100; form fields 50; each identifier or label 256 bytes |
| Attachments per visitor message | 10 |
| Upload file name | 255 bytes |
| Ephemeral message lifetime | 15 minutes after delivery |
| In-memory log | 256 entries per visitor unless configured |

## How it is tested

The shared adapter suite, including the interaction ingress and response cases,
runs against the adapter itself, with end-to-end tests that drive the receiver
and the event stream through the engine. The shared `widgettest` suite checks
every `Log` implementation, `storelog` included. There is no remote provider, so
there are no cassettes. Browser behavior is not tested.

## Known limits

- Visitors cannot edit, delete or react: the receiver has no frames for them.
- Visitor typing is not forwarded.
- Receipts go from the host to the visitor only; the visitor sends none.
- Outbound attachments are passed as attachment IDs for the host to serve.
- No `reconcile`, `membership` or `history`; see [Not declared](#not-declared).
- A host with several processes needs a shared `Log` and its own notification
  between processes.
- Experimental: configuration and event names may change
  ([Compatibility](../compatibility.md)).
