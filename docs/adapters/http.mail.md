# http.mail

`http.mail` takes mail that a relay posts over HTTP: an inbound-parse service, a
script beside an MTA, a bridge in front of a mailbox. One channel is one set of
recipient addresses. It receives only: it reads no mailbox, serves no SMTP and
sends nothing. Replies need a channel of an adapter that sends
([`smtp.mail`](smtp.mail.md), [`imap.mail`](imap.mail.md), and the provider
adapters).

| | |
| --- | --- |
| Key | `http.mail` |
| Package | `hop.top/uma/adapter/mail/httpmail`, in module `hop.top/uma/adapter/mail` |
| Capabilities | `receive_http` |
| Events | none |
| Identity namespaces | `http.mail.address` |
| Sender kinds | person |
| Provider permissions | none declared; the credential purpose `http.mail.webhook` holds the relay's shared secret |
| Message IDs and threading | chosen by the sender; email header threading |
| Reply parent | reported: the first valid `In-Reply-To` msg-id; resolved to a stored message by its `Message-ID` |
| Interactions | none ([interactions](../interactions.md)) |
| Maturity | experimental |

The matrix of all adapters is in [Adapters](README.md).

## Capabilities and operations

| Capability | Behavior |
| --- | --- |
| `receive_http` | `Runtime.Webhook` returns the handler for the channel. Each `POST` is one message. Unavailable until the host supplies a credential port |

## Not declared

Reason classes are defined in [Adapters](README.md#why-a-capability-is-not-declared).

| Capability | Reason | Why |
| --- | --- | --- |
| `reply_recipients` | `not built` | Follows `send`, which the adapter does not declare; the sending mail adapters declare it |
| `receive` | `provider` | The relay pushes; there is no mailbox or listener for uma to read |
| `send` | `not built` | A reply would be a request to a bridge URL the relay serves, or a submission through a relay of its own; neither is built. Reply through a channel of `smtp.mail` or another sending mail adapter |
| `start_conversation` | `not built` | Follows `send`. With no start there is no recipient to normalize: the sender key already has its domain lowercased and its local part exact, as on the other mail adapters |
| `edit` | `provider` | A delivered mail cannot be changed at the recipient |
| `delete` | `provider` | A delivered mail cannot be recalled |
| `react` | `provider` | Mail has no reactions |
| `receipt` | `provider` | Nothing is sent back to the sender |
| `membership` | `provider` | There is no provider conversation to add a participant to |
| `history` | `provider` | There is no mailbox to read back |
| `reconcile` | `provider` | Nothing is sent, so nothing needs its answer found |
| `refetch` | `provider` | A mail never changes after delivery |
| `work_item_act` | `provider` | Mail has no work items |
| `internal_note` | `provider` | Mail has no private comments; notes stay in uma |
| `extension_execute` | `provider` | Nothing is stored at a provider to act on |
| `extension_query` | `provider` | Nothing is stored at a provider to read |
| `probe_permissions` | `provider` | There is no provider permission to probe: a wrong secret shows as a refused request |
| `stream` | `provider` | Mail has no streamed messages |
| `send_ephemeral` | `provider` | Mail has no message only one reader sees |
| `signal` | `provider` | Mail has no typing or presence |
| `respond` | `provider` | Mail has no interactions to answer |
| `respond_follow_up` | `provider` | As `respond` |
| `archive` | `provider` | No mailbox is read through the adapter |
| `move` | `provider` | No mailbox is read through the adapter |
| `read_state` | `provider` | No mailbox is read through the adapter |
| `flag` | `provider` | No mailbox is read through the adapter |
| `trash` | `provider` | No mailbox is read through the adapter |
| `raw_read` | `provider` | The mail arrives through the host's webhook; the adapter holds no mailbox to read a message's bytes back from |

## Mount it

```go
factory := httpmail.NewFactory(httpmail.Options{ReplayStore: store}) // the store only for replay rejection
engine, err := uma.New(uma.Config{ /* ... */ Adapters: []adapter.Factory{factory}})
handler, err := engine.Runtime().Webhook(ctx, call, channel)
mux.Handle("/hooks/mail", handler)
```

Give the relay the URL and the secret. The engine bounds the request body to
its `WebhookBodyLimit` (1 MiB by default) before the adapter reads it; raise
that and `max_body_bytes` together for larger messages.

## Authenticate requests

Every request is authenticated before anything else is read, by
[webhook authentication](../webhook-auth.md). The `auth` block of the channel
configuration is that package's configuration in JSON; it holds no secret. The
secret comes from the host's credential port on every request, for the channel's
credential reference and purpose `http.mail.webhook`: the shared token or HMAC
key, or the Ed25519 public key. A missing or unusable secret refuses the
request (503), whatever it carries.

| Field | Meaning |
| --- | --- |
| `scheme` | Required: `bearer`, `token`, `hmac` or `ed25519`. There is no "none" |
| `header`, `hash`, `encoding`, `prefix`, `prefix_optional`, `payload`, `key_encoding`, `min_secret_bytes` | As in webhook authentication |
| `timestamp` | `header`, `format`, `tolerance_seconds`, `allow_unsigned`: the freshness window |
| `replay` | `source` (`signature` or `header`), `header`, `ttl_seconds`, `allow_unsigned`. Needs `Options.ReplayStore`; the channel is refused without it |

The configuration is refused when webhook authentication would refuse it, for
example an unsigned timestamp without `allow_unsigned`.

Senders already in the field keep their conventions. Hosted services, from their
documentation (not tried against the live services):

| Sender | What its documentation says | Configure |
| --- | --- | --- |
| Postmark inbound | One JSON document, no HMAC signature. Protect the URL with HTTP Basic credentials | `auth: {scheme: "token", header: "Authorization"}`, with the whole value `Basic <base64 user:password>` as the secret. `json_fields`: `from` `From`, `to` `OriginalRecipient`, `subject` `Subject`, `body` `TextBody`; attachments `Attachments` with `name` `Name`, `mime_type` `ContentType`, `size_bytes` `ContentLength`, `content` `Content` (the base64 bytes, staged) |
| Mailgun route to a URL ending in `json` | One JSON document; signature and timestamp in `X-Mailgun-Signature` and `X-Mailgun-Timestamp`, HMAC-SHA-256 hex with the webhook signing key. Its receiving page signs the timestamp followed by the entire body; its signing page describes only the form layout (timestamp and token), so check the JSON layout against your account | `auth: {scheme: "hmac", header: "X-Mailgun-Signature", payload: "{timestamp}{body}", timestamp: {header: "X-Mailgun-Timestamp"}, replay: {source: "signature"}}`. `json_fields`: `from` `from`, `to` `recipient`, `subject` `subject`, `body` `body-plain`; attachments `attachments` with `name` `filename`, `mime_type` `content-type`. Its documentation, as read, does not say whether a JSON route embeds attachment bytes, so the recipe maps no attachment bytes; under the default member name a base64 `content` would still be staged |
| SendGrid Inbound Parse | `multipart/form-data` (or the raw message in an `email` field of one). It documents no signature or authentication | Not accepted: form bodies are not read. Put a small converter in front, or use a mailbox adapter |
| Mailgun route to any other URL | `application/x-www-form-urlencoded` or `multipart/form-data`, with `signature`, `timestamp` and `token` as form fields | Not accepted, for the same reason; point the route at a URL ending in `json` |
| Your own script or MTA hook | Yours | Any scheme; prefer a signed one with a signed timestamp |

## What the relay posts

The content type picks the format among the channel's `formats` (default
`["json"]`). One that is not listed is 415.

**`application/json`.** One object per message. The members read, and their
names, are configuration (`json_fields`); the defaults are neutral.

| Role | Default member | Type | Required |
| --- | --- | --- | --- |
| Sender | `from` | an address, or `Name <address>` | yes |
| Recipients | `to` | an address list as a string, or an array of such strings | yes |
| Subject | `subject` | string | no |
| Text | `body` | string | no |
| Threading | `message_id`, `in_reply_to`, `references` | RFC 5322 msg-ids; another value threads nothing | no |
| Written at | `date` | RFC 3339 or RFC 5322; else the time received | no |
| Attachments | `attachments` | array of descriptions | no |

An attachment description reads `type`, `url`, `mime_type`, `size_bytes`, `name`
and `content`, the base64 of the attachment's bytes (renamable under
`json_fields.attachment_fields`). Postmark's documented sample carries its
`Content` that way. Bytes are staged; a URL is only recorded; see
[Attachments](#attachments). A member of the wrong
type is a 400. Members not named are ignored: `spf`, `dkim`, `assurance`,
`sender_kind` and the like in a payload change nothing.

**`message/rfc822`.** The message itself, parsed by the same MIME parser as
`smtp.mail` and `imap.mail`. It carries what a JSON object cannot: the real
`Message-ID`, `References`, `Auto-Submitted` and `List-Id`, the MIME structure
with attachments staged and described by digest and size, and the header fields sender
evidence comes from. Prefer it when the relay can post the raw message. The
recipients are the `To` and `Cc` fields, or, with `recipient_header`, the
address list of that request header (the SMTP envelope recipients a hook knows).
When `recipient_header` is set and the request lacks it, the message is refused
rather than routed by a field the operator chose not to trust.

Each recipient is matched against the channel's `recipients` and mapped to its
destination; the message reaches the sink once per destination. A recipient no
entry matches is ignored, and a message to none is refused. Addresses compare
with the domain case-insensitive and the local part exact.

## Answers

| Status | Cause |
| --- | --- |
| 200 `{"accepted": n}` | The sink accepted the message for each of `n` destinations, new or duplicate |
| 400 | The body is not a message of its format, or has no valid sender or recipient |
| 401, 409, 413, 503 | Authentication refused: [webhook authentication](../webhook-auth.md#errors) |
| 405, 415 | Method other than `POST`; content type not accepted |
| 403 | The service principal may not receive on the channel |
| 422 | No recipient matches, the destination is bound to no inbox or is disabled, or the sink refused for a reason a retry cannot change |
| 503 | The sink failed for a reason a retry can change, or the instance is closed |

A request that was not accepted is not remembered as seen: when replay rejection
is on, the sender's retry is no replay. Hosted services retry by their own rules,
and take only 200 as success: Postmark retries any other status except 403
(ten times over about a day), Mailgun any other except 406 (for eight hours),
SendGrid only a 5xx (for three days). A 422 is therefore retried by Postmark and
Mailgun until their schedule ends, and refused again each time; no status is
configurable yet.

## Sender evidence

uma verifies nothing a request carries. The `From` address is the sender's
claim. By default every message has source assurance `unverified`, and the
payload records no SPF, DKIM or DMARC result of uma's own. What a message says
about its authentication is recorded as a claim, never as evidence:

- `message/rfc822`: the `Authentication-Results` and `Received-SPF` fields
  as found (at most 8 of each, 4 KiB each), under `evidence.claimed`.
- JSON: nothing, unless `evidence.authentication_results_field` names a member;
  its value is then recorded as a claim.

`evidence.trusted_authserv_id` is the one way to raise the assurance, and it is a
declaration by the operator: *the relay that passes authentication evaluated SPF,
DKIM and DMARC itself, stamps its results in an `Authentication-Results` value
(RFC 8601) under this authserv-id, and removes any stamp under that id a sender
supplied.* Examples: an MTA hook on the MX that posts the message it received, or
a hosted service whose signed webhook you trust to say so. With it:

- The topmost `Authentication-Results` value (header field of an RFC 822 body, or
  the configured JSON member) whose authserv-id equals the declared one, ignoring
  case, is read. A value naming another id, or one that does not parse, is a claim.
- Its results are recorded under `evidence` with `trusted: true` and the
  `authserv_id`: `spf`, `dkim` and `dmarc`. A DMARC result counts only when its
  `header.from` is the domain of the message's `From`.
- The DMARC result sets the assurance with the values `smtp.mail` uses:
  `dmarc_pass`, `dmarc_fail`, `dmarc_none`, `dmarc_temperror`, `dmarc_permerror`.
  Without a DMARC result for the From domain the assurance stays `unverified`.

Evidence grades the sender; it never proves identity and never links contacts.
Header threading (`In-Reply-To`, `References`) is the sender's too: it selects a
conversation of the same inbox and nothing more.

A message is identified by its bytes, never by its `Message-ID`: the id is the
sender's to choose, so a forged message that reuses the id of a genuine one is a
second message, not a duplicate that suppresses it. A redelivery of the same
bytes to the same destination lands once. Two genuine messages with identical
bytes are one.

## Configuration

`httpmail.Config{...}.Extension()` builds the channel configuration (schema
`http.mail.config`, version 1). Unknown fields are refused, and every problem is
reported as a validation issue.

| Field | Required | Meaning |
| --- | --- | --- |
| `recipients` | yes | Accepted addresses, each with the `destination` it maps to. No duplicates |
| `auth` | yes | Request authentication, above |
| `formats` | no | `json`, `rfc822`; default `["json"]` |
| `json_fields` | no | Member names of a JSON body; defaults above. Two roles may not read one member |
| `recipient_header` | no | Request header of envelope recipients, for `rfc822` |
| `evidence.trusted_authserv_id` | no | The trust declaration above |
| `evidence.authentication_results_field` | no | JSON member carrying the stamp; needs the declaration |
| `max_body_bytes` | no | Request body limit; default 1 MiB |

Host options (`httpmail.Options`): `ReplayStore`, the `webhookauth.ReplayStore`
for replay rejection. The bundled in-memory store forgets on restart and is
not shared by replicas; a [`storereplay.Store`](../webhook-auth.md#durable-store)
over the host's durable store survives both. See
[webhook authentication](../webhook-auth.md#replay-and-restarts).

## Attachments

Attachment bytes a request carries are staged through the engine's attachment
sink, the way `smtp.mail`, `imap.mail`, `google.mail` and `microsoft.mail` stage
theirs: each staged attachment is named in an attachment part of the message,
reserved in the inbox of its destination under the engine's own checks (size
limit, accepted media types, declared size and digest, audience). What is not
staged is described.

- `message/rfc822`: the attachments of the parsed message, with the bytes kept
  by the MIME parser within the bounds below.
- JSON: an entry whose `content` member (`json_fields.attachment_fields.content`)
  is standard base64. Its size and digest are taken from the decoded bytes, not
  from the sender's `size_bytes`. A value that is no base64, or decodes to
  nothing or beyond the bound, leaves the entry described only.
- A JSON entry with only a `url` is described only. **The adapter never fetches
  a URL a sender supplies**: it would let any holder of the relay secret, or a
  forged payload that passes it, make uma request internal addresses (SSRF) or
  send its credentials to a host of the sender's choosing. The page defines no
  allowlisted, no-redirect fetch policy, so there is none; deliver the bytes in
  the message, or put a converter that fetches in front.

Every described attachment says whether its bytes are staged (`"staged": true`).
A sender's description is otherwise a claim, marked `attachments_claimed` in the
message metadata: its `kind` (the sender's `type`), `url`, `media_type`, `size`
and `filename`. A `url` is kept only when absolute `http` or `https`, and is
untrusted. Embedded bytes are never part of the description. At most 50
attachments are described per message; the rest are counted in `omitted`.

A message without a sink that can stage, or whose engine has no blob store, is
delivered with its attachments described. An attachment the engine refuses for
good is skipped. A staging failure a retry can fix is a 503 and the delivery is
not counted as seen, so the sender's retry is no replay. Staging is idempotent
per message and destination: its key is derived from the message's event ID,
which is bound to the request body and the destination, so a redelivery restages
the same attachments, and one message to two destinations stages for each.

A filename, from a MIME header or from a JSON description alike, is the sender's
suggestion, so it is reduced to a display name before it is described: only the
last path component is kept (`../../etc/passwd` becomes `passwd`), control and
invisible formatting characters, right-to-left overrides included, are dropped,
the text is normalized to Unicode NFC, a Windows device name such as `CON` gets a
leading `_`, and a name over 255 bytes is cut with its extension kept. A name
with nothing left is described without a filename. A host that writes the name
to a file still applies the naming rules of its own filesystem.

## Limits

| What | Limit |
| --- | --- |
| Request body | `max_body_bytes`, default 1 MiB, and the engine's webhook body limit |
| Recipients per message | 100; 50 recorded in the headers metadata |
| A raw header field kept in metadata | cut at 16 KiB |
| Text | cut at the content limit (1 MiB); `text.truncated` in the metadata |
| Attachments described | 50 |
| Attachments staged | 10 per message, in order, of at most 25 MiB decoded each; the rest are described only. The request body limit applies first |
| Claimed `Authentication-Results`, `Received-SPF` | 8 each, 4 KiB each |
| DKIM results recorded from a trusted stamp | 8 |

## How it is tested

The shared adapter suite runs for `receive_http` against a scripted relay that
posts JSON under the account's bearer token. Package tests cover authentication
(missing, wrong, stale, replayed, signed), both body formats, recipient routing,
the attachment descriptions and their staging (embedded and raw bytes, the
bounds, a sink without staging, refusals, a retryable failure and the replay slot,
a URL never fetched), sender evidence and the trust declaration, and drive the
handler through a real engine with a blob store. The Postmark and Mailgun configurations above
run against payloads shaped as their documentation shows. There are no recorded
cassettes and no record path: nothing here has been tried against a hosted
service or a real MTA.

## Known limits

- Receive only: no reply path.
- Attachment bytes are staged only when the message carries them. A JSON entry
  with only a URL is described, never fetched. Mailgun's JSON attachment form is
  not documented in the sources read, so its bytes are staged only under the
  default `content` member.
- Staged bytes are held in memory while the message is handled: up to 10 times
  25 MiB with a `max_body_bytes` large enough to carry them.
- Form bodies (`multipart/form-data`, `application/x-www-form-urlencoded`) are
  not read, so SendGrid Inbound Parse and the default Mailgun route cannot post
  here directly.
- The JSON body has no real headers: a Postmark `Headers` array is not read, so a
  Postmark reply threads only when the message carries `in_reply_to` or
  `references` members. Postmark's `MessageID` is not an RFC 5322 msg-id and
  threads nothing.
- DKIM and SPF are not evaluated: a raw message's `DKIM-Signature` is not
  checked, and there is no client address. Verified evidence exists only through
  the trust declaration.
- Refusals use one status each; a hosted service's "stop retrying" status (403
  for Postmark, 406 for Mailgun) is not configurable.
- Experimental: configuration may change ([Compatibility](../compatibility.md)).
