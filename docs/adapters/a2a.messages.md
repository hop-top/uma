# a2a.messages

`a2a.messages` makes uma an A2A (Agent2Agent) server endpoint. Remote agents
send messages and read their tasks over JSON-RPC 2.0; the messages land in an
inbox, your host does the work, and uma keeps the protocol state and the
correlation between messages, contexts and tasks.

| | |
| --- | --- |
| Key | `a2a.messages` |
| Package | `hop.top/uma/adapter/agent/a2a` (root module, standard library only) |
| Protocol | A2A 1.0 (specification v1.0.1) over JSON-RPC 2.0; [specification](https://a2a-protocol.org/v1.0.1/specification/) |
| Capabilities | `receive_http`, `send` |
| Sender kind | `agent` |
| Identity namespace | `a2a.principal` |
| Events | `message.received`, `message.created`, `task.updated` |
| Provider permissions | none; the host authenticates requests |
| Message IDs and threading | derived by uma from the agent and the client's `messageId`; no threading |
| Reply parent | none |
| Interactions | none ([interactions](../interactions.md)) |
| Inbox selection | per agent context (required) |
| Maturity | experimental |

The matrix of all adapters is in [Adapters](README.md).

Runnable host: [`examples/a2a`](../../examples/a2a), covered by an end-to-end
test.

## What it is, and is not

- **A server endpoint.** Remote agents call uma. Calling a remote agent (an
  outbound A2A client) is a separate piece and is not part of this adapter.
- **Not an executor.** uma never runs the work. Your host reads the inbound
  message, decides what it means and answers through `Messages.Reply` and
  `Tasks.Act`. The adapter maps nothing in a message's text, parts or role to
  a service call or control command.
- **Not a pusher.** The agent reads replies, status and artifacts through
  the protocol. Nothing is pushed to it: no streaming, no push notifications.

The card says so explicitly: `streaming`, `pushNotifications` and
`extendedAgentCard` are all `false`.

## Not declared

Reason classes are defined in [Adapters](README.md#why-a-capability-is-not-declared).

| Capability | Reason | Why |
| --- | --- | --- |
| `reply_recipients` | `provider` | An A2A message has one counterpart agent; there is no recipient list |
| `receive` | `by design` | Remote agents call uma; there is nothing to poll |
| `start_conversation` | `by design` | An outbound A2A client is a separate piece; per-agent-context inboxes refuse starts |
| `edit` | `provider` | A2A has no message edit |
| `delete` | `provider` | A2A has no message recall |
| `react` | `provider` | A2A has no reactions |
| `receipt` | `provider` | A2A has no delivery or read receipts; the agent polls |
| `membership` | `provider` | A context has one remote agent |
| `history` | `by design` | uma is the server and already holds the history |
| `reconcile` | `by design` | `Send` has no transport, so no answer can be lost |
| `refetch` | `provider` | A message never changes |
| `work_item_act` | `provider` | A2A tasks are `Tasks.Act`, not work items |
| `internal_note` | `by design` | Restricted content never reaches a remote agent |
| `extension_execute` | `not built` | A2A protocol extensions are not mapped to effects |
| `extension_query` | `not built` | A2A protocol extensions are not mapped to reads |
| `probe_permissions` | `by design` | The host authenticates; there is no provider credential |
| `stream` | `not built` | The protocol has `SendStreamingMessage` and `SubscribeToTask`; the card declares `streaming: false` |
| `send_ephemeral` | `provider` | A2A has no message only one reader sees |
| `signal` | `provider` | A2A has no typing or presence |
| `respond` | `provider` | A2A has no interactions |
| `respond_follow_up` | `provider` | As `respond` |
| `archive` | `by design` | uma is the store; there is no provider-side state to mirror |
| `move` | `by design` | uma is the store; there is no provider-side state to mirror |
| `read_state` | `by design` | uma is the store; there is no provider-side state to mirror |
| `flag` | `by design` | uma is the store; there is no provider-side state to mirror |
| `trash` | `by design` | uma is the store; there is no provider-side state to mirror |
| `raw_read` | `not built` | Raw read is built for mail only; the provider's whole copy of a message is not read |

## Mount it

`Runtime.Webhook` returns the channel's handler. Mount it anywhere on your
mux:

```go
factory, err := a2a.New(a2a.Options{Authenticate: authenticate})
engine, err := uma.New(uma.Config{ /* ... */ Adapters: []adapter.Factory{factory}})
handler, err := engine.Runtime().Webhook(ctx, call, channel)
mux.Handle("/a2a/", handler)
```

| Request | Answer |
| --- | --- |
| `POST` | A JSON-RPC request (below) |
| `GET` or `HEAD` of `agent-card.json` or `.well-known/agent-card.json` under the mount | The Agent Card, without authentication, with an `ETag` and `Cache-Control: public, max-age=300` |

The host owns the domain-root `/.well-known/agent-card.json` route, if it
wants one. HTTP statuses report transport failures only: 401 or 503 for a
refused authentication, 405, 415 (the content type must be `application/json`
or `application/a2a+json`) and 413 (from the engine's webhook body limit).
Every other answer, JSON-RPC errors included, is HTTP 200.

Every request must carry `A2A-Version: 1.0` (or the `A2A-Version` query
parameter); anything else is error -32009.

## Authenticate requests

The host authenticates; uma authorizes. `Options.Authenticate` is required
(`a2a.New` fails without it):

```go
type Authenticator func(*http.Request, model.ChannelRef) (a2a.Principal, error)
```

It sees the request and the channel and returns the verified remote agent as
`a2a.Principal{ID: ...}`. The ID is 1 to 255 bytes of UTF-8 without control
characters, stable for the agent, and never taken from the request body.

| Result | Answer |
| --- | --- |
| A principal | The request goes on |
| An error | HTTP 401 with a `WWW-Authenticate` challenge naming the declared security schemes |
| A `*model.Error` with a retryable code (such as `Unavailable`) | HTTP 503 |

It runs before the body is read. The chain is:

1. The host verifies the credential (a bearer token, an API key, a client
   certificate: whatever the channel declares).
2. The adapter turns the principal into the message's sender (namespace
   `a2a.principal`, kind `agent`) and trusts nothing else about the request.
   No `*http.Request` crosses into core.
3. Core authorizes every access afterwards: the policy decision of the
   ingress, and the reads and the cancel of the engine's task sink.

The adapter holds no token table and verifies nothing itself. The channel's
`security_schemes` and `security_requirements` are declarative: they are what
the card tells clients and what the 401 challenge names, and your
`Authenticate` is what enforces them. The agent's identity is created as a
contact of kind `agent` and is never folded into a person; the A2A message
role is never identity evidence. The example keeps its static bearer-token
map in the example, not in the adapter.

`Options.ErrorDomain` (default `uma.hop.top`) names the domain of the
`ErrorInfo` details on errors uma defines (see [Errors](#errors)). Set it to
a stable namespace you own if you want your own; ordinary self-hosting needs
no override. It never changes errors the A2A specification defines, which
keep the domain `a2a-protocol.org`.

## Configure the channel

`a2a.Configuration(a2a.Config{...})` builds a channel configuration (schema
`a2a.messages.config`, version 1). Everything except the two durations is the
Agent Card's content, which the adapter serves and enforces none of.

| Field | Required | Meaning |
| --- | --- | --- |
| `destination` | yes | The inbox destination every request is delivered to. The channel's inbox must use per-agent-context selection |
| `public_url` | yes | Absolute `http(s)` URL the endpoint is mounted at: the card's only interface (`JSONRPC`, protocol version `1.0`) |
| `name`, `description`, `version` | yes | Card identity |
| `skills` | yes, at least one | Each has `id`, `name`, `description` and at least one tag; optional `examples`, `input_modes`, `output_modes` |
| `provider` | no | `organization` and `url`, both required when set |
| `documentation_url`, `icon_url` | no | Absolute `http(s)` URLs |
| `input_modes`, `output_modes` | no | Default `text/plain` |
| `security_schemes` | no | Map of name to an A2A `SecurityScheme` object (exactly one of `apiKeySecurityScheme`, `httpAuthSecurityScheme`, `oauth2SecurityScheme`, `openIdConnectSecurityScheme`, `mtlsSecurityScheme`) |
| `security_requirements` | no | Alternatives, each mapping declared scheme names to required scopes |
| `blocking_wait` | no | How long a blocking `SendMessage` waits. Go duration, positive, default `30s` |
| `poll_interval` | no | How often a blocking call looks for its answer, each wait spread by +-10% so calls do not read in phase. From `50ms` to `blocking_wait`, default `250ms` |

Validation reports every problem as a `model.ValidationIssue` under
`configuration.data.<field>`. The 401 challenge names the declared schemes:
an HTTP scheme by its own name, OAuth 2 and OpenID Connect as `Bearer`; API
keys and mutual TLS have none; with no scheme declared it is `Bearer`.

## Methods

| Method | Behavior |
| --- | --- |
| `SendMessage` | Delivers a message to the inbox. Blocking by default (below); `configuration.returnImmediately` opens a task with the message and answers with it at once |
| `GetTask` | The task and its status message. `historyLength` above zero adds that many of the newest messages, at most 100 |
| `ListTasks` | The agent's tasks, most recently updated first. Filters: `contextId`, `status`, `statusTimestampAfter`; `pageSize` 1 to 100 (default 50), `pageToken`, `historyLength`, `includeArtifacts`. Needs a `contextId` when the agent has tasks in several inboxes. `totalSize` is the size of the page: uma counts nothing beyond it |
| `CancelTask` | Records a cancel request and returns the task in its current state. Your host decides whether it reaches `canceled`; a repeat is a no-op; a terminal task is -32002 |

`GetTask` and `ListTasks` return the calling agent's tasks only; another
agent's task is simply absent.

Rejected methods:

| Method | Code |
| --- | --- |
| `SendStreamingMessage`, `SubscribeToTask`, `GetExtendedAgentCard` | -32004 |
| `CreateTaskPushNotificationConfig`, `GetTaskPushNotificationConfig`, `ListTaskPushNotificationConfigs`, `DeleteTaskPushNotificationConfig` | -32003 |
| Any other method | -32601 |

## Contexts, tasks and message-only exchanges

A request names no context, a context or a task:

- **Neither.** The adapter allocates a new opaque context ID (random, never
  derived from the agent or the message), and the engine opens the
  conversation under it.
- **A `contextId`.** It must be a context the agent owns. An unknown context
  and another agent's are the same -32602; a context is never created on
  demand.
- **A `taskId`.** It must be a task of the agent (-32001 otherwise), not
  terminal (-32004), and in the given context if both are given (-32602). The
  message continues the task: it is stored bound to it.

**A task is your host's choice.** A message opens no task by itself. The
exchange stays message-only until your host opens a task with `Tasks.Act`, or
the request sets `returnImmediately`, which opens one with the message. There
is no fallback task. `referenceTaskIds` are kept as references on the message
and used for nothing else. One task's completion closes neither the context
nor any other task, and many tasks may live in one context.

Message identity: the engine message ID derives from the agent and the
client's `messageId`, so equal IDs from two agents never collide.

## Blocking SendMessage

Unless `returnImmediately` is set, `SendMessage` waits for your host. It
waits up to `blocking_wait` or the caller's own deadline, whichever is
shorter, polling the store every `poll_interval` (+-10%, from `Options.Jitter`, uniform in [0, 1]; a worker in another process
counts). It answers with the first of:

| Condition | Answer |
| --- | --- |
| The message is bound to no task and your host replied to it (`Messages.Reply` with `InReplyTo` the message) | A `Message` response; no task exists |
| The message's task is terminal or interrupted (`input_required`, `auth_required`), reported after the message or opened by it | A `Task` response. A `failed`, `canceled` or `rejected` task is a task response, never the error below |

When the wait runs out, the answer is JSON-RPC **-32603** with a
`google.rpc.ErrorInfo` of reason `DEADLINE_EXCEEDED`. This is uma's documented
mapping, not a code the A2A specification defines (the specification
allows structured `ErrorInfo` details, section 9.5, and defines no deadline
error). Its `domain` is `Options.ErrorDomain` (default `uma.hop.top`) and its
metadata holds `contextId`, `messageId` and, only when a task exists,
`taskId`.

Expiry is only an expiry. It never returns a non-terminal task as success,
creates no task and cancels nothing: the accepted message stays, work
continues, and a task opened later is readable through `GetTask` and
`ListTasks`. Resend the same `messageId` to get the first context back. A
retry creates nothing.

## Errors

| Code | When |
| --- | --- |
| -32700 | Malformed JSON |
| -32600 | Batch, malformed request, wrong `jsonrpc` |
| -32601 | Unknown method |
| -32602 | Invalid params; unknown or another agent's `contextId` |
| -32603 | Any other uma error, with an `ErrorInfo` whose reason is the uma error code in upper case; the blocking deadline above |
| -32001 | Unknown or another agent's task |
| -32002 | Cancel of a terminal task |
| -32003 | Push notification methods |
| -32004 | Streaming, subscribe, extended card; a message to a terminal task |
| -32009 | `A2A-Version` header missing or not `1.0` |

Every error carries `data` as an array of `@type` objects. A policy denial is
-32603 like any uma error and reveals nothing about what was denied. Consumers
identify uma-defined errors by `domain` plus `reason`.

## Parts

| Inbound part | Becomes |
| --- | --- |
| `text` | Text |
| `data` | A structured part, schema `a2a.messages.data` v1 |
| `url` | A structured part, schema `a2a.messages.file_url` v1. Never fetched |
| `raw` | An attachment, staged through the engine. Needs a blob store in `uma.Config.Blobs`; without one the request is -32603 |

A part holding none or several of `text`, `raw`, `url` and `data` is -32602.
Part metadata is ignored.

Going out, text is text, `a2a.messages.data` is a `data` part,
`a2a.messages.file_url` a `url` part, and any other structured part a `data`
part naming its schema. The adapter has no read path to attachment bytes, so
an attachment goes out as a `data` part naming the attachment ID (media type
`application/vnd.uma.attachment+json`), never as a `raw` part.

## Replies and status

`Sender` has no transport. `Send` records the reply as provider-accepted under
a message reference derived from the attempt; delivery is never confirmed.
Only public audiences are sent: internal notes and restricted content never
reach a remote agent.

Status content is not a message pushed to the agent. When a `Tasks.Act` update
carries `status_message`, uma records exactly one uma-authored message bound
to the task (origin `task_status`), sets it as the task's status message and
never sends it. The agent reads it as `status.message` of the task and in its
history. A state change without content records no message; there is never a
message per transition.

## Limits

| What | Limit |
| --- | --- |
| Request body | The engine's webhook body limit (`Config.WebhookBodyLimit`, default 1 MiB); it applies to the base64 text of `raw` parts. Larger is HTTP 413 |
| `messageId` | 1 to 128 printable characters, no spaces |
| `referenceTaskIds` | At most 64 |
| Parts, text, data | uma's content limits: 64 parts per message, 1 MiB per text part, 256 KiB per `data` part |
| Raw part file name | At most 255 bytes |
| Artifacts | At most 256 per task; names and descriptions at most 8 KiB each |
| `url` parts | Any string; never fetched or validated beyond being non-empty |
| History | The newest 100 messages of a task |
| Page size | 1 to 100 |

## Idempotency limits

A retry of a `SendMessage` with the same `messageId` is a duplicate and
returns the first context, within the engine's ingress horizon
(`RetentionPolicy.Ingress`). Past it, a resend is a new message and opens a
new context. `returnImmediately` and the JSON-RPC `id` are not part of the
key. A request that was claimed but not processed to the end answers
-32603 and never a new context; the retry finds the first acceptance.

## Write your executor

Your host subscribes to events and answers through two services. The example
([`examples/a2a`](../../examples/a2a)) is a complete loop.

**Events** (`Events.Open` with these kinds):

| Event | Payload | Meaning |
| --- | --- | --- |
| `message.received` | `model.MessageReceivedPayload`; `author_kind` is `agent` | A remote agent sent a message. Read it with `Messages.Get`. `Message.Task` is set when the request set `returnImmediately` or continued a task |
| `task.updated` | `model.TaskUpdatedPayload`; `changes` names what changed | A task opened or changed. `cancel_requested` in `changes` is a remote cancel request for you to answer |
| `message.created` | `model.MessageCreatedPayload` | Optional: origin `task_status` marks a status message |

**Answer a message without a task**: `Messages.Reply` with `InReplyTo` the
inbound message. The blocking caller gets it as a `Message`.

**Open and update tasks**: `Tasks.Act` with a `model.TaskAction`:

| Action (`Action.Name`, version 1) | Data | Notes |
| --- | --- | --- |
| `uma.task.open` | `model.TaskOpen{Message, State}` | `Target` names the conversation (task ID empty); no `ExpectedRevision`. `State` defaults to `submitted`. Only per-agent-context conversations hold tasks; a message already bound to a task is a conflict |
| `uma.task.update` | `model.TaskUpdate{State, StatusMessage, Audience, Artifacts}` | `Target` names the task; `ExpectedRevision` required. Artifacts upsert by ID (`append` extends, `complete` marks the last chunk) |

Task state moves only along these edges; a terminal task never changes:

| From \ To | submitted | working | input or auth required | completed, failed, canceled, rejected |
| --- | --- | --- | --- | --- |
| (open) | default | yes | yes | yes |
| submitted | same state only | yes | yes | yes |
| working | no | same state | yes | yes |
| input or auth required | no | yes | yes | yes |
| terminal | no | no | no | no |

Reaching `canceled` clears the pending cancel request. A move the table
refuses, or any update of a terminal task, is a conflict.

Retry with the same operation ID and payload and you get the stored result.
Derive operation IDs from the event ID, and choose each step from the task's
stored state, so an event redelivered after a crash repeats nothing.

## Permissions

- **Provider.** There is no provider credential: the host authenticates each
  request and returns the remote agent's principal ([Authenticate
  requests](#authenticate-requests)). The adapter holds no token table.
- **uma.** The ingress principal needs `channel.receive` on the channel;
  replies are authorized as `message.reply`, task opening and updates as
  `task.act`, and task reads and the cancel of the engine's task sink are
  authorized for the calling agent.

## How it is tested

The shared adapter suite runs against the adapter, and package tests drive the
JSON-RPC handler through a real engine. Golden files pin the message and task
shapes the A2A specification shows (a basic task, a multi-turn
`input_required` exchange) and the Agent Card. [`examples/a2a`](../../examples/a2a)
has an end-to-end test of a host with an executor loop. There is no remote
provider, so there are no cassettes. Interoperability with third-party A2A
clients is not tested.

## Known limits

- Server only: no outbound A2A client.
- No streaming, push notifications or extended Agent Card.
- Sends are never confirmed delivered; the agent reads replies by polling.
- Attachments leave as `data` parts naming the attachment ID, never `raw`.
- A blocking `SendMessage` that outlasts `blocking_wait` answers -32603
  `DEADLINE_EXCEEDED`, a uma mapping the specification does not define.

## Maturity

Experimental. The configuration schema, the card and the host contract may
change before the first stable release ([Compatibility](../compatibility.md)).
