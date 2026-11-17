# relay.messages

`relay.messages` relays SMS, WhatsApp and iMessage messages for a provider that
has no adapter of its own, through a gateway the operator runs. The gateway posts each
incoming message to uma as authenticated JSON; uma posts each reply to a
callback URL the gateway serves. One channel is one gateway and one medium.

| | |
| --- | --- |
| Key | `relay.messages` |
| Package | `hop.top/uma/adapter/messaging/relay`, in the root module |
| Capabilities | `receive_http`, `send`, `reconcile`, `start_conversation` |
| Events | `message.received`, `delivery.updated` |
| Identity namespaces | `relay.sms`, `relay.whatsapp`, `relay.imessage` (phone numbers exactly as the gateway reports them; on iMessage also email addresses, lowercased). See [Mediums and identities](#mediums-and-identities) |
| Sender kinds | person |
| Provider permissions | none declared; the gateway decides what its callback credential may do |
| Message IDs and threading | chosen by the gateway; no threading |
| Reply parent | none: the gateway field mapping has no reply field |
| Interactions | none ([interactions](../interactions.md)) |
| Maturity | experimental |

The matrix of all adapters is in [Adapters](README.md).

## What it is, and is not

- **A relay, not a provider.** The operator runs a gateway in front of an SMS,
  WhatsApp or iMessage provider that has no uma adapter. uma never talks to that
  provider. When the provider has an adapter, use it: [`twilio.sms`](twilio.sms.md),
  [`twilio.whatsapp`](twilio.whatsapp.md), [`meta.whatsapp`](meta.whatsapp.md).
- **One contract, three mediums.** The wire contract is the same for SMS,
  WhatsApp and iMessage, so there is one type key and the channel's `medium`
  picks the identity namespace: `relay.sms`, `relay.whatsapp` or
  `relay.imessage`. As with the Twilio pair, one number on two mediums is two
  identities until the host links them.
- **Not a sync endpoint.** The webhook answer carries no reply. See the next
  section.

## Mediums and identities

| `medium` | Identity namespace | Address the gateway reports | uma keeps it |
| --- | --- | --- | --- |
| `sms` | `relay.sms` | A phone number | Exactly as sent |
| `whatsapp` | `relay.whatsapp` | A phone number | Exactly as sent |
| `imessage` | `relay.imessage` | A phone number, or the email address of an Apple ID | A number (any address without `@`) exactly as sent; an email address with its ASCII letters lowercased, since Apple IDs ignore case |

uma checks no address format on any medium: E.164 and a well-formed email
address are the gateway's to report. What uma normalizes is only the case of an
iMessage email address, so `Alice@Example.com` and `alice@example.com` are one
sender, one delivery and one inbox binding. It applies to `from` and `to` alike,
so a reply to an iMessage sender goes to the lowercased address.

A start gets the same treatment. `Conversations.Start` lowercases an iMessage
email recipient before it keys the recipient's identity, opens the conversation
and fingerprints the operation: a start to `Alice@Example.com` claims the
`alice@example.com` identity that Alice's own messages arrive on, the same
conversation answers her, the reply goes out to the lowercased address, and
replaying a start under its operation key with the address in another case is
the same operation, not a conflict. A phone number, and every address on `sms`
and `whatsapp`, is used exactly as given.

**Identities from earlier starts.** A start made before the adapter normalized
recipients keyed its identity on the address as typed. An identity created that
way, such as `Alice@Example.com`, is a separate identity from the lowercased
one inbound claims, and uma migrates nothing. A start can still find it, in one
case: when the caller types the address exactly as that earlier start did and no
identity exists for the lowercased spelling, the start reuses the identity under
the typed spelling and its conversations, instead of claiming a second one. It
costs one exact read of each spelling, made before the start is recorded, and
only when the typed address is not already lowercase.

This is deliberately partial.

- **Exact spelling only.** A start typed `alice@example.com`, `ALICE@EXAMPLE.COM`
  or any case other than the legacy identity's finds nothing and claims the
  lowercased identity, as it would with no legacy identity at all. There is no
  case-insensitive search.
- **The lowercased identity wins.** If both spellings have an identity, the
  start uses the lowercased one and leaves the other alone; there is no
  ambiguity error.
- **Starts only.** Inbound is unchanged: a reply from Alice, in any case,
  claims the lowercased identity, so it does not land on the legacy identity.
- **Routes keep their spelling.** A start that joins a legacy conversation sends
  to the route that conversation stores, the address as typed then, as every
  send on it did before. A conversation the start has to open for a legacy
  identity routes to the lowercased address.
- **Operations.** The start's fingerprint is keyed on the lowercased address, so
  replaying a start under its operation key in any case is the same operation. A
  start replayed across the upgrade with the mixed-case address it first carried
  now fingerprints differently and is refused as a conflict; start again under
  a new key.

Where both identities already exist, or a person types the address in another
case than the legacy one, an operator joins them afterwards with
`Contacts.PreviewMerge` and `Contacts.Merge`. The factory opts in with
`adapter.LegacyRecipientReuser`, for iMessage only.

A person's number and email address on iMessage are two identities in
`relay.imessage`, and the same number on `relay.sms` a third, until the host
links them.

**Operator note on iMessage.** Apple offers no API for iMessage outside Apple
Messages for Business. Hosted iMessage relays send from real Apple IDs on Apple
hardware the provider runs, under Apple's iCloud terms (section V.B), which
restrict automated access and unsolicited or bulk messages, and Apple may
suspend an account that breaks them. That exposure is the operator's: choose the
provider, the Apple ID and the traffic accordingly. uma states no support for
any particular provider. A provider that cannot answer by `Idempotency-Key` or
serve a lookup needs the operator's gateway in front of it to meet the
[callback](#reply-route) and [lookup](#lookup) contracts; without a lookup,
`reconcile` is reported `disabled`.

## Reply route

A reply leaves uma by **callback**: `Messages.Reply` posts one JSON message to
a URL the channel configures, and the gateway relays it. A reply is not written
into the webhook answer.

Why not the answer: uma replies after it accepted the inbound message, from
whatever process runs the worker, and the host's bot may take longer than the
gateway waits. An answer is also gone once sent, so it could carry neither
the provider message ID `Exchange` needs nor an attempt `reconcile` could look
up. A callback is a request uma makes on its own schedule, once per operation,
under the engine's retry and outcome rules. An optional second endpoint, the
[lookup](#lookup), lets uma ask what became of a callback whose answer was lost.
A sender that today reads the reply from the webhook answer must serve a
callback endpoint instead; the inbound side keeps its field names (see [Migrating a sender](#migrating-a-sender-already-in-the-field)).

A synchronous answer cannot be built on the adapter contract as it stands: the
sink serves reply reads (`adapter.TaskSink`) to per-agent-context inboxes only,
and per-contact inboxes have no read the handler could wait on.

Safety of the callback:

| Concern | Rule |
| --- | --- |
| Where it goes | One configured URL per channel, and its host (as `host` or `host:port`) must be in the host's `Options.CallbackHosts`. With none listed no channel can send. The check runs at `Validate` and `Open`, so a refused URL is never stored. The optional `callback.lookup_url` is held to the same allowlist |
| URL shape | `https` (`http` only if the host sets `AllowHTTP`), no credentials, query or fragment: a secret has no business in a URL. The same for `callback.lookup_url` |
| Credential | Resolved from the host's credential port on every call (purpose `relay.callback`), sent as `Authorization: Bearer` or as the whole value of one configured header. It never appears in configuration, errors or outcomes |
| Redirects | Never followed: a 3xx answer is a final rejection. A host HTTP client that follows redirects itself cannot be stopped from sending, but an answer from any URL other than the configured one settles nothing (`outcome_unknown`). Give the adapter a client that does not follow redirects |
| Bounds | Each call, callback or lookup, is bounded by `Options.Timeout` (15 seconds by default) and the caller's context; at most 64 KiB of the answer is read; a reply is at most 64 KiB of text |
| Idempotency | Every call carries `Idempotency-Key: <operation ID>`, the same across the attempts of one reply. A gateway that remembers keys can answer a repeat without sending twice |

## Capabilities and operations

| Capability | Behavior |
| --- | --- |
| `receive_http` | `Runtime.Webhook` takes JSON messages from the gateway and, on a channel that enables them, [delivery status updates](#delivery-status). See [Receiving](#receiving) |
| `send` | `Messages.Reply` posts one reply to the callback. Available only when the channel configures a callback and the host gives the adapter an HTTP client; otherwise reported `disabled` or `unavailable` |
| `reconcile` | `Operations.Reconcile` and the worker ask the [lookup](#lookup) about a reply or first message whose answer was lost. Available only when the channel configures `callback.lookup_url` and the host gives the adapter an HTTP client; otherwise reported `disabled` or `unavailable` |
| `start_conversation` | `Conversations.Start` posts a first message to any number the host names, one that never wrote in included. Whether the gateway accepts a first message (a WhatsApp template, say) is its answer to give |

A call that may have reached the gateway without an answer (connection lost,
timeout, 408, 409, any 5xx) is `outcome_unknown`. What happens next depends on
the lookup:

- **With `callback.lookup_url`**, the engine asks the gateway before any retry.
  `sent` settles the operation `provider_accepted`; `not_found` after the settle
  window sends the reply again under the same `Idempotency-Key`; anything else
  leaves it unknown and asks again later. See [Lookup](#lookup).
- **Without it**, `reconcile` is reported `disabled` and uma never sends that
  reply again: the operation stays unknown (`Operations.Reconcile` answers
  `unsupported`, with no call). The host decides whether to send a new reply,
  which is a new operation with a new key; the operator can check the gateway's
  own log for the old key. This is the same position as `meta.whatsapp`.

## Not declared

Reason classes are defined in [Adapters](README.md#why-a-capability-is-not-declared).

| Capability | Reason | Why |
| --- | --- | --- |
| `reply_recipients` | `provider` | A gateway message goes to one number |
| `receive` | `by design` | The gateway pushes; uma runs no receiver loop against it |
| `edit` | `provider` | The relay contract has no message to change after it was handed over |
| `delete` | `provider` | As `edit`: nothing to take back |
| `react` | `provider` | SMS has no reactions and the contract carries none |
| `receipt` | `provider` | `receipt` is uma telling the sender of a message that it was delivered or read, and the contract has no such call. The other direction, the gateway telling uma what became of a reply, is no capability: it is part of `receive_http`, a [channel setting](#delivery-status) |
| `membership` | `provider` | The contract has no groups |
| `history` | `provider` | The contract has no list call to read past messages from |
| `refetch` | `provider` | A message never changes |
| `work_item_act` | `provider` | A relay has no work items |
| `internal_note` | `provider` | SMS has no private messages; notes stay in uma |
| `extension_execute` | `provider` | The contract defines no provider effects |
| `extension_query` | `provider` | The contract defines no provider reads |
| `probe_permissions` | `provider` | The gateway has no permission model to probe |
| `stream` | `provider` | SMS has no streamed messages |
| `send_ephemeral` | `provider` | SMS has no message only one reader sees |
| `signal` | `provider` | SMS has no typing or presence |
| `respond` | `provider` | SMS has no interactions to answer |
| `respond_follow_up` | `provider` | As `respond` |
| `archive` | `provider` | A phone thread has no archive, folder, flag or trash |
| `move` | `provider` | A phone thread has no archive, folder, flag or trash |
| `read_state` | `provider` | A phone thread has no archive, folder, flag or trash |
| `flag` | `provider` | A phone thread has no archive, folder, flag or trash |
| `trash` | `provider` | A phone thread has no archive, folder, flag or trash |
| `raw_read` | `not built` | Raw read is built for mail only; the provider's whole copy of a message is not read |

## Permissions

- **Gateway.** Two credential purposes, resolved per request and never read from
  the environment.
  - `relay.webhook` is the key that authenticates the gateway's webhooks: a
    secret, or a password whose secret is the key. With `ed25519` it is the
    public key. While `auth.previous_credential_ref` is set, the key behind that
    reference also verifies, so a rotation overlaps.
  - `relay.callback` is what uma presents to the callback: a secret, or a
    password whose secret is the value. It must be printable ASCII without
    spaces, since it travels in a header.
- **uma.** The receiving principal needs `channel.receive`; replies are
  authorized as `message.reply`, started conversations as `conversation.start`.

## Configuration

`relay.messages.config` version 1, as JSON in the channel configuration. Unknown
fields are refused, so a secret pasted into the configuration is an error, not
a stored value.

| Field | Required | Meaning |
| --- | --- | --- |
| `medium` | yes | `sms`, `whatsapp` or `imessage`; see [Mediums and identities](#mediums-and-identities) |
| `auth` | yes | How the gateway proves its webhooks: [`webhookauth.Config`](../webhook-auth.md) as JSON. `scheme` (`bearer`, `token`, `hmac`, `ed25519`) is required; `header`, `hash`, `encoding`, `prefix`, `prefix_optional`, `payload`, `key_encoding`, `min_secret_bytes`, `timestamp` (`header`, `format`, `tolerance`, `allow_unsigned`) and `replay` (`source`, `header`, `ttl`, `allow_unsigned`) are as documented there, durations as strings such as `5m`. There is no unauthenticated mode, and a configuration that could accept one is refused |
| `auth.previous_credential_ref` | no | Credential reference of the key being retired |
| `inbound.fields.from`, `to`, `body`, `message_id`, `media` | no | Top-level JSON keys read for each role: a list tried in order, the first key present wins. Defaults `from`, `to`, `body`, `message_id`, `media`. At most 8 names of up to 64 characters each; a name serves one role |
| `inbound.generate_missing_id` | no | Give a message with no id a random one instead of refusing it. Such a message cannot be recognized when the gateway sends it again |
| `inbound.max_body_bytes` | no | Body limit, up to 1 MiB; default 64 KiB; it bounds status updates too |
| `inbound.status.enabled` | no | Take [delivery status updates](#delivery-status) on the same webhook. Off by default: every request is then a message |
| `inbound.status.fields.status`, `from`, `operation_id`, `attempt_id`, `message_id`, `code` | no | Top-level JSON keys read for each role of a status update, as `inbound.fields`. Defaults the role names. `status` is the key that makes a request an update: no message role may use it. The other roles may share a name with one. At most 8 names of up to 64 characters each; a name serves one role. Checked whether or not the route is on |
| `callback.url` | for sending | Where replies go; see [Reply route](#reply-route). Without it the channel only receives |
| `callback.auth`, `callback.header` | no | `bearer` (default), or `header` with the header's name |
| `callback.message_id` | no | JSON keys of the answer that carry the gateway's id for the message, tried in order; default `message_id`. A lookup answer names the id under the same keys |
| `callback.lookup_url` | no | The gateway's [lookup](#lookup) endpoint, under the rules of `callback.url`; it may be the same URL. Without it `reconcile` is disabled and a lost answer stays `outcome_unknown` |

Host options (`relay.Options`): `CallbackHosts`, `AllowHTTP`, `Timeout` and
`ReplayStore`. They are host settings, not channel configuration, so a channel
cannot redirect credentials. Replay rejection needs a store that survives what
the host needs it to survive: without `ReplayStore` the adapter keeps one per
factory in memory, which a restart forgets and replicas do not share. Pass a
[`storereplay.Store`](../webhook-auth.md#durable-store) over the host's durable
store to survive restarts and share across replicas
([Replay and restarts](../webhook-auth.md#replay-and-restarts)).

Inboxes: use per-contact selection locked to one conversation, as for the SMS
adapters; an inbox binds the numbers the gateway reports as `to`.

## Receiving

The gateway POSTs one JSON object per message:

```json
{"from": "+15550100001", "to": "+15550100002", "body": "hello", "message_id": "m-1",
 "media": ["https://cdn.example.com/a.jpg", {"url": "https://cdn.example.com/b", "content_type": "image/png"}]}
```

`from` and `to` are phone numbers exactly as the gateway sends them (uma neither
normalizes nor checks their format, only that they are 1 to 128 printable
characters; on iMessage they may be email addresses, which uma lowercases, see
[Mediums and identities](#mediums-and-identities)), `body` is text, `message_id` is the gateway's id for the message (a
string up to 255 characters). A message needs `from`, `to`, `message_id` and
`body` or `media`. Other keys are ignored.

Authentication comes first: `webhookauth` reads at most the body limit and
verifies the request under the channel's `auth` before anything is parsed, so an
unauthenticated request never reaches the JSON decoder or the engine.

| Answer | When |
| --- | --- |
| 200 `{"status":"accepted"}` | The sink accepted the message, new or duplicate; for a [status update](#delivery-status), one that reached an attempt (`{"status":"accepted","matched":false}` when it reached none) |
| 400 | An authenticated body that is not a usable message, or a malformed header, signature or timestamp |
| 401 | Missing or wrong credential, or a stale timestamp (`WWW-Authenticate` for `bearer`) |
| 405, 413 | Method other than POST (`Allow: POST`); body over the limit |
| 409 | A delivery already accepted, when replay rejection is on |
| 422 | A sink failure that retrying will not fix |
| 503 | The instance is closed, no credential or replay store can be reached, or a retryable sink failure |

A delivery that was not accepted gives its replay reservation back, so the
gateway's retry is a new attempt, not a replay.

Each message becomes a `message.received` event from a person, `verified` and
public. Its event ID is derived from the sender and the message ID: the same
delivery sent again is deduplicated, and two senders using one message ID never
collide. Message IDs are the gateway's, so core gives them no identity or echo
meaning (`Exchange` declares `sender`). Media URLs are checked (http or https, a
host, no credentials, at most 2048 characters, at most 10 per message) and kept
as a `relay.messages.media` part (`{"items":[{"url","content_type"}]}`); the
adapter never fetches them. A URL is data the gateway chose: a host that fetches
one must treat it as untrusted.

## Delivery status

A callback answered 2xx makes a reply `provider_accepted`: the gateway took it. What happens
after is the gateway's to report. A **status update** is how the gateway tells
uma what became of a reply: it moves the operation on to `delivered` (and records
a read) or to `failed`, and it settles a reply whose answer was lost, which stays
`outcome_unknown` on a channel with no [lookup](#lookup), without sending it again.

Status updates are off until the channel sets `inbound.status.enabled`. They
arrive on the same webhook as messages, authenticated the same way (see
[Receiving](#receiving)); a request is an update when it holds the status key
and a message otherwise, so a sender that posts messages with a `status` key of
its own is not misread while the route is off. The gateway echoes back what the
[callback](#sending) carried:

```json
{"status": "delivered", "from": "+15550100002",
 "operation_id": "0190f3a2-7c4e-7d00-8000-0000000000b1", "attempt_id": "0190f3a2-7c4e-7d00-8000-0000000000a1",
 "message_id": "m-77"}
```

| Role (default key) | Required | Meaning |
| --- | --- | --- |
| `status` | yes | `accepted`, `sent`, `delivered`, `read` or `failed`, in any case. Any other value is refused |
| `from` | yes | The line the reply left from: the address an inbox binds, the `to` of the messages it receives. An iMessage email address is lowercased, as in [Mediums and identities](#mediums-and-identities) |
| `operation_id`, `attempt_id` | with each other | The `operation_id` and `attempt_id` of the callback reported on. Both or neither; each a valid ID. They are what lets the engine apply the report to an attempt |
| `message_id` | one of the two | The gateway's id for the message, as the callback answer named it (up to 255 characters) |
| `code` | no | The gateway's own reason code of a `failed` update: a string or number of up to 64 letters, digits and `. _ : -`. Read for `failed` only; anything else refuses the update |

An update needs `status`, `from`, and an `operation_id` with its `attempt_id`, a
`message_id`, or both. Other keys are ignored. The keys are configuration, with
ordered aliases per role as for a message (`inbound.status.fields`).

**Auth and bounds.** As for a message: webhookauth verifies the request under
the channel's `auth` before anything is parsed, replay rejection (`auth.replay`)
covers updates like messages, and the body limit (`inbound.max_body_bytes`)
bounds them. The same answers apply (see the table under
[Receiving](#receiving)), with 400 for an authenticated body that is not a
usable update. The answer is 200 for an update the engine took, whether it
changed anything or not, so the gateway does not retry what cannot succeed:
`{"status":"accepted"}` when the update reached an attempt the engine holds (even one
whose state it already outranks), and `{"status":"accepted","matched":false}` when
it reached none: it named no operation, or an operation or attempt the engine
does not hold. A gateway that sees `matched:false` for an update that carried
ids has a wrong id; one that sent a `message_id` alone learns that such an update
only records itself. A repeat of an update gets the answer the first got.

**Identity.** An update is a status event, deduplicated by an event ID bound
to its content: the line, the operation, the attempt, the message id, the status
value and, for `failed`, the code. The same update sent again, in any key order
and with any other keys, is one event. A different status, id or code is another
event, so a gateway that reuses its ids cannot make a later update pass for a
redelivery of an earlier one. The time of the update is not part of it: a retry
is the same event whenever it is sent.

**What the engine does with it.** The update is a status envelope with a delivery
report, the path `twilio.sms` and `meta.whatsapp` use. The report is recorded as
evidence of the attempt the update names, and of that attempt only: an update
naming no operation, one naming an operation the engine does not hold, and one
naming an attempt it never made are taken and change nothing. A line no inbox
binds is refused (422). The operation's worker then weighs the evidence it
holds and moves the operation (below), so a report acts within a worker pass,
not inside the request.

| Status | Reported as | Notes |
| --- | --- | --- |
| `accepted`, `sent` | `provider_accepted` | The gateway applied the reply. uma has one state for both |
| `delivered` | `delivered` | The message reached the recipient |
| `read` | `delivered`, read | The operation stays `delivered` and records when the read was reported: `Operation.ReadConfirmedAt`. The payload keeps `read` |
| `failed` | `failed`, permanent, never retried | The summary is `relay reported the delivery failed`, with the code when there is one, never the gateway's own words |

The status envelope carries a `relay.messages.status` payload (version 1:
`status`, `state`, `read`, `operation`, `attempt`, `message_id`, `code`,
`failure`) with what the gateway said.

**Order and repeats.** The engine ranks what it holds instead of applying
updates in the order they arrive. From lowest to highest: `accepted` and `sent`
(one rank), `failed`, `delivered`, `read`. An operation only moves up. Evidence
lives in the store, not in a process, so every replica and a restarted process
reach the same state from the same updates.

| The operation is | An update | Result |
| --- | --- | --- |
| `provider_accepted` | `delivered` | `delivered`, one `delivery.updated` event |
| `provider_accepted` | `read` | `delivered` with `ReadConfirmedAt` set, in one event |
| `provider_accepted` | `failed` | `failed`, final, with the safe summary; the reply is never sent again |
| `delivered` | `read` | still `delivered`, `ReadConfirmedAt` set; one event with `previous_state` and `state` both `delivered`. The first read is kept; a later one changes nothing |
| `delivered` or read | `sent`, `accepted`, `failed` or a repeat | Recorded as evidence; nothing changes and nothing is announced. A late `sent` never lowers `delivered`, a late `failed` never undoes it |
| `failed` | `delivered`, `read` | `delivered`, the failure cleared. A delivery proves the message arrived; a failure report can be a gateway that gave up before a late receipt |
| `failed` | `sent`, `accepted` or a repeat | stays `failed`: failed is final for an attempt that never delivered |
| `outcome_unknown` (the callback answer was lost) | the first update the worker sees, or several before it runs | the highest rank among them, in one settle: `delivered` then `sent` is `delivered` (not `provider_accepted`), `failed` then `sent` is `failed`, `failed` with `delivered` is `delivered` in either order. The reply is never sent again |
| `queued`, the send still in flight | an update that beats the answer | the highest of the two, in one event: the answer does not hide a delivery that arrived first |
| any | the same update again | Taken (200), recorded once, nothing else |

An update reaches the operation of a reply or a conversation start. A `failed`
update fails the operation only when its attempt is the latest one and no
earlier attempt was applied: a reply that was retried and accepted is not
failed by a late report about the first attempt. Each real change is announced
once, by one `delivery.updated` event with trigger `provider`; an update the
operation already outranks announces nothing.

## Sending

`Messages.Reply` and `Conversations.Start` POST this to `callback.url`:

```json
{"medium": "sms", "to": "+15550100001", "body": "hi", "in_reply_to": "m-1",
 "operation_id": "...", "attempt_id": "..."}
```

`in_reply_to` is the gateway's own id of the message answered, when the reply
names one. Text parts only, joined by newlines; a non-public audience is
`unsupported` (a relay has no private message). The outcome follows the answer:

| Answer | Outcome |
| --- | --- |
| 2xx | `provider_accepted`. The reference is the id the answer names under `callback.message_id`, else the attempt's |
| 3xx | `rejected`, final: redirects are never followed |
| 401, 403 | `unauthorized`: the callback credential was refused |
| 429 | `rate_limited` with `Retry-After` (seconds or an HTTP date) |
| 408, 409, 5xx | `outcome_unknown`: the gateway may have applied it |
| other 4xx | `rejected`, final |
| no answer: connection lost or timeout | `outcome_unknown` |
| dial, DNS or TLS failure | `transient`: nothing left the host |

Errors carry the status and no answer text, URL or credential.

## Lookup

The lookup is an optional second endpoint the gateway serves, so uma can ask
what became of a callback whose answer it never got. It is configured as
`callback.lookup_url` and carries the callback's credential (purpose
`relay.callback`, resolved per call, sent as the callback sends it). Without it
nothing below happens.

The request:

```http
GET /uma/lookup?operation_id=0190f3a2-7c4e-7d00-8000-0000000000b1 HTTP/1.1
Host: gateway.example.com
Accept: application/json
Authorization: Bearer <relay.callback secret>
```

`operation_id` is the operation ID: the `Idempotency-Key` header and the
`operation_id` field of the callback being asked about, the same across the
attempts of one reply. The query is the only one the URL carries (the
configured URL has none). There is no body. The call is bounded like a callback
(`Options.Timeout`, the caller's context), redirects are never followed, and at
most 64 KiB of the answer is read.

The gateway answers with a JSON object whose `status` says what it holds for
that key:

```json
{"status": "sent", "message_id": "m-77"}
```

```json
{"status": "not_found"}
```

| Answer | Meaning to uma |
| --- | --- |
| 2xx `{"status":"sent"}` | The gateway took a callback with this key. The operation settles `provider_accepted`, referenced by the id under `callback.message_id` (default `message_id`), else by the attempt's id, as for a callback answer |
| 2xx or 404 `{"status":"not_found"}` | The gateway holds no callback with this key and will act on none. After the settle window uma sends the reply again, same key; inside it, nothing happens yet |
| any other 2xx or 404 answer | Inconclusive: no JSON, no `status`, another value (`pending`, say) or more than 64 KiB. The operation stays unknown and is asked about again |
| 429 | Rate limited; asked again later |
| 408, 5xx, connection lost, timeout | Transient: a lookup changes nothing, so it is asked again later |
| 401, 403 | The credential was refused |
| 3xx, other 4xx, an answer from another URL | Refused |

Refused and unauthorized answers say nothing about the reply: the worker leaves
the operation `outcome_unknown` and never sends it again, and
`Operations.Reconcile` returns the error, so a host can ask again once the
gateway is fixed. The worker asks at most as many times as its retry budget
allows; past it the operation stays unknown. Errors carry the status only,
never the answer text, URL or credential.

**The settle window.** A callback can still be in flight, or inside the
gateway, when uma asks. `not_found` counts only once the attempt is
`ReconcileSettle` old (2 minutes, or twice `Options.Timeout` when that is
longer); before, it is inconclusive and uma asks again after the window.

**What the gateway must guarantee.** `not_found` is what lets uma send a reply
again, so it is a promise, not a guess:

- Record the `Idempotency-Key` of a callback before acting on it, or in the
  same step, and keep it as long as uma may ask: the worker asks within its
  retry budget (minutes under the default policy), and a host may call
  `Operations.Reconcile` on an unknown operation at any later time. Keep days
  to be safe; a key forgotten too early turns into a duplicate reply.
- Answer `not_found` only for a key it has no record of. A key it is still
  processing, or cannot check right now, is anything else (a 503, or
  `{"status":"pending"}`): uma asks again.
- Treat a second callback with a key it already holds as the same reply,
  never a new message. A reply uma sends again after `not_found` carries the
  same key.

## Set up an iMessage channel

An iMessage channel is a `relay.messages` channel with `medium: imessage` and a
gateway in front of an iMessage backend. The steps: pick a backend, run a
gateway, register the adapter in the host, configure the channel, test it.

**1. Pick a backend.** Apple has no iMessage API outside Apple Messages for
Business, so every route sends from a real Apple ID (see the
[operator note](#mediums-and-identities)). Two kinds fit the relay: a hosted
relay with an HTTP API and signed webhooks (such as Blooio), or BlueBubbles
Server on a Mac you run, signed in to Messages. uma names no supported provider;
the gateway below covers these two as a reference.

**2. Run a gateway.** It serves the [callback](#sending) and the
[lookup](#lookup) and keeps the promises under
[what the gateway must guarantee](#lookup). The reference gateway in
[`cmd/imessagegateway`](../../examples/imessagegateway/README.md)
(standard library only) records every `Idempotency-Key` on disk before it
calls the backend, answers a repeated key from that record, answers the lookup
from it, verifies the backend's webhook and posts each inbound message to uma,
signed. Install it with
`go install hop.top/uma/cmd/imessagegateway@<version>`:

```sh
export IMESSAGE_GATEWAY_CALLBACK_SECRET=<relay.callback secret> \
       IMESSAGE_GATEWAY_WEBHOOK_SECRET=<relay.webhook secret> \
       BLOOIO_API_KEY=<key> BLOOIO_WEBHOOK_SECRET=whsec_<...>
imessagegateway -backend blooio -blooio-from +15550100002 \
  -uma-webhook-url https://uma.example.com/uma/imessage -addr 127.0.0.1:8787
```

Its README covers the BlueBubbles backend, the flags and the key log's
retention. Put TLS in front of it unless uma reaches it over a private network.

**3. Register the adapter in the host.** The host gives the adapter the two
credentials (purpose `relay.webhook`, the key the gateway signs with, and
`relay.callback`, the secret uma presents to the gateway), names the gateway's
host in `Options.CallbackHosts`, configures the channel and its inbox, and
mounts the webhook handler from `Runtime.Webhook` where the gateway posts. The
end-to-end tests of the gateway run this code (in
`examples/imessagegateway/host_test.go`); replace the memory store and the
allow-all authorizer with the host's own, and drop `AllowHTTP` for an `https`
gateway:

```go
// gatewaySecrets is the host's credential port for the channel:
// relay.webhook verifies what the gateway posts, relay.callback is what uma
// presents to the gateway's callback and lookup.
type gatewaySecrets struct{ webhook, callback []byte }

func (s gatewaySecrets) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	switch r.Purpose {
	case relay.PurposeWebhook:
		return ports.Secret{Bytes: s.webhook}, nil
	case relay.PurposeCallback:
		return ports.Secret{Bytes: s.callback}, nil
	}
	return nil, model.NewError(model.Unauthorized, "no credential for this purpose")
}

// newEngine registers the relay adapter. CallbackHosts names the gateway's
// host exactly as its callback URL does; AllowHTTP is for a gateway on a
// private network or on localhost only. Timeout covers a backend that
// answers only once Messages sent the message (BlueBubbles).
func newEngine(secrets ports.Credentials, gatewayHost string, ids ports.IDs) (uma.Engine, error) {
	factory := relay.NewFactory(relay.Options{
		CallbackHosts: []string{gatewayHost},
		AllowHTTP:     true,
		Timeout:       time.Minute,
	})
	return uma.New(uma.Config{
		Store: memory.New(memory.Options{Clock: ports.SystemClock{}}),
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{factory},
		Credentials: secrets,
		// A client that follows no redirect: the callback credential goes
		// to the configured URL only.
		HTTP: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		Clock: ports.SystemClock{},
		IDs:   ids,
	})
}

// imessageChannel is the channel configuration: medium imessage, the
// gateway's HMAC signature with a signed timestamp, replays refused, and
// the gateway's callback and lookup.
func imessageChannel(gatewayURL string) relay.Config {
	return relay.Config{
		Medium: relay.MediumIMessage,
		Auth: relay.Auth{
			Scheme:    "hmac",
			Header:    "X-Webhook-Signature",
			Timestamp: relay.AuthTimestamp{Header: "X-Webhook-Timestamp"},
			Replay:    relay.AuthReplay{Source: "signature"},
		},
		Callback: &relay.Callback{
			URL:       gatewayURL + "/uma/reply",
			LookupURL: gatewayURL + "/uma/lookup",
		},
	}
}

// configureIMessage stores the channel and a per-contact inbox bound to the
// address the gateway reports as "to" (the line's number, or the Apple ID
// the Mac sends from), and returns the handler to mount where the gateway
// posts (its uma webhook URL).
func configureIMessage(ctx context.Context, engine uma.Engine, host model.Principal, gatewayURL, address string) (model.ChannelRef, model.InboxRef, http.Handler, error) {
	cmd := func(op model.OperationID) model.Command {
		return model.Command{Call: model.Call{Correlation: model.CorrelationID(op)}, Operation: op}
	}
	admin, err := engine.For(host)
	if err != nil {
		return model.ChannelRef{}, model.InboxRef{}, nil, err
	}
	tenant := host.Tenant
	ch, err := admin.Configuration().ConfigureChannel(ctx, model.ConfigureChannel{
		Command: cmd("imessage-channel"),
		Channel: model.Channel{
			Ref: model.ChannelRef{Tenant: tenant}, Type: model.TypeRelayMessages,
			CredentialRef: "imessage-gateway", Configuration: imessageChannel(gatewayURL).Extension(),
		},
	})
	if err != nil {
		return model.ChannelRef{}, model.InboxRef{}, nil, err
	}
	channel, err := ch.Operation.Result.AsChannel()
	if err != nil {
		return model.ChannelRef{}, model.InboxRef{}, nil, err
	}
	lock := true
	ib, err := admin.Configuration().ConfigureInbox(ctx, model.ConfigureInbox{
		Command: cmd("imessage-inbox"),
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: tenant}, Channel: channel, Name: "imessage",
			Destinations: []string{address},
			Selection: model.Selection{
				Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1,
			},
		},
	})
	if err != nil {
		return model.ChannelRef{}, model.InboxRef{}, nil, err
	}
	inbox, err := ib.Operation.Result.AsInbox()
	if err != nil {
		return model.ChannelRef{}, model.InboxRef{}, nil, err
	}
	webhook, err := engine.Runtime().Webhook(ctx, uma.RuntimeCall{
		Principal: host, Call: model.Call{Correlation: "imessage-webhook"},
	}, channel)
	if err != nil {
		return model.ChannelRef{}, model.InboxRef{}, nil, err
	}
	return channel, inbox, webhook, nil
}
```

Mount the handler at the path the gateway's `-uma-webhook-url` names:

```go
mux := http.NewServeMux()
mux.Handle("/uma/imessage", webhook)
```

**4. The channel configuration** that `imessageChannel` builds, as JSON:

```json
{
  "medium": "imessage",
  "auth": {"scheme": "hmac", "header": "X-Webhook-Signature",
           "timestamp": {"header": "X-Webhook-Timestamp"},
           "replay": {"source": "signature"}},
  "callback": {"url": "https://gateway.example.com/uma/reply",
               "lookup_url": "https://gateway.example.com/uma/lookup"}
}
```

The gateway signs `<timestamp>.<body>` with HMAC-SHA-256 under the
`relay.webhook` secret, hex, the timestamp in Unix seconds, which is the
default signed layout once a timestamp header is set
([webhook authentication](../webhook-auth.md)). Bind the inbox to the address
the gateway reports as `to`: the line's number for a hosted relay, the Apple ID
the Mac sends from for BlueBubbles.

What crosses the wire. An inbound message from an Apple ID email address:

```json
{"from": "Alice@Example.com", "to": "+15550100002", "body": "Is my order shipped?", "message_id": "msg_in_0001"}
```

uma keeps the sender as `alice@example.com` in `relay.imessage`. A reply to
them reaches the gateway's callback as:

```json
{"medium": "imessage", "to": "alice@example.com", "body": "It ships today.", "in_reply_to": "msg_in_0001",
 "operation_id": "0190f3a2-7c4e-7d00-8000-0000000000b1", "attempt_id": "0190f3a2-7c4e-7d00-8000-0000000000a1"}
```

and the gateway answers `{"message_id": "<backend id>"}`.

**5. Test it.** Start a conversation with a handle you hold
(`Conversations.Start` with a `relay.imessage` recipient), check the operation
settles `provider_accepted` with the backend's id, ask the gateway's lookup
for its operation ID, and reply from the device: the reply lands in the same
conversation as `message.received`. The gateway module's recorded test does
exactly this per backend ([Record iMessage cassettes](../recording/imessage.md)).

What it does not give: reactions, typing indicators, read receipts, edits,
unsends, group chats and replies quoting a message (the relay contract carries
none of them, and the BlueBubbles route would need the Private API). The
reference gateway posts no [status updates](#delivery-status). Blooio has
delivered, read and failed events and the gateway drops them, and BlueBubbles'
payload for them is nowhere in the repository. The gateway also keeps no
attempt ID (its key log holds the operation's key and the backend's id), and
there is no recorded backend payload to map, so a reply stays `provider_accepted`
and a gateway of your own is where status updates come from.
Inbound media is forwarded as URLs by the hosted route only.

## Migrating a sender already in the field

Keep the sender's field names and authentication in configuration. A gateway
that posts Twilio-style names with an HMAC in a legacy header:

```json
{
  "medium": "sms",
  "auth": {"scheme": "hmac", "header": "X-Relay-Signature", "prefix": "sha256=", "prefix_optional": true,
           "payload": "{body}",
           "timestamp": {"header": "X-Relay-Timestamp", "format": "unix_or_rfc3339", "allow_unsigned": true}},
  "inbound": {"fields": {"from": ["From", "from"], "to": ["To", "to"], "body": ["Body", "body", "text"],
                         "message_id": ["MessageSid", "message_id"]},
              "generate_missing_id": true},
  "callback": {"url": "https://gateway.example.com/uma/reply",
               "lookup_url": "https://gateway.example.com/uma/lookup"}
}
```

What does not carry over: the reply in the webhook answer, form-encoded bodies,
flat media keys such as `NumMedia` and `MediaUrl0`. The gateway serves the
callback and sends JSON.

## Limits

| What | Limit |
| --- | --- |
| Inbound body | 64 KiB by default, at most 1 MiB; over it, 413 before any secret is read |
| Address and message ID | 128 and 255 bytes |
| Media entries | 10 per message, 2048 characters per URL |
| Reply text | 64 KiB |
| Callback and lookup answer read | 64 KiB |
| One callback or lookup call | 15 seconds by default (`Options.Timeout`), or the caller's context if shorter |
| Reconcile settle window | 2 minutes, or twice `Options.Timeout` when that is longer |
| Content | text parts only |

## How it is tested

The shared adapter suite (`receive_http`, `send`, `reconcile` and
`start_conversation` cases, the lost-answer, reconcile-before-retry and
policy-bypass cases included) runs against a scripted gateway on `httptest` that
serves the lookup, once per medium (on iMessage with email-address peers). Engine tests drive a channel without a lookup (a lost answer
is never resent nor looked up) and with one (settled from `sent`, resent only
after `not_found` outlasts the settle window), and a channel with the status
route, where each status value settles a stranded reply and, for a reply the
gateway accepted, moves it up the order (delivered, read, failed) once and
only up, whichever order the updates come in. Package tests cover
authentication in each scheme including a legacy HMAC convention, replay and
release, the field naming, media checks, each callback and lookup status
mapping, redirects, timeouts, and credential and answer disclosure, and for
status updates: the route off by default, the value mapping, the event ID
bound to the content, malformed updates, aliases, authentication, replay and the
body limit, and that no `receipt` capability is declared. The
relay is the operator's own service, not a provider with a fixed API, so the adapter
has nothing to record: what a gateway does is the operator's to test. The
package imports only the standard library, uma's public contracts and
`webhookauth` (a test pins that).

The iMessage path has an end-to-end test through the reference gateway in
`examples/imessagegateway`: uma in process, the gateway's callback, lookup and
webhook, and the backend's HTTP traffic recorded with xrr, once per backend
(Blooio, BlueBubbles Server). It runs against stand-ins of both backends on
every test run, and as recorded tests that skip until the owner records them
([Record iMessage cassettes](../recording/imessage.md)); none is recorded yet.
The gateway is a reference, not a list of supported providers.

## Known limits

- No synchronous reply and no form-encoded input.
- A status update acts within a worker pass, not inside the request: the
  answer says the engine took it, `Operations.Get` shows the new state once the
  worker ran. `accepted` and `sent` are one state, and `read` is not a state
  but a time on a delivered operation. See [Delivery status](#delivery-status).
- An update that carries only a `message_id` is kept and moves nothing: the id
  is the gateway's, and the engine never guesses an attempt from it. The
  answer says `matched:false`.
- No history, no outbound media or templates (text only).
- Callback credentials travel as a bearer token or one header; the adapter does
  not sign callback bodies.
- Experimental: configuration may change ([Compatibility](../compatibility.md)).
