# twilio.sms

`twilio.sms` sends and receives SMS and MMS through Twilio Programmable
Messaging. One channel is one sending number, or a Messaging Service. Incoming
messages and status callbacks arrive as signed webhooks; outbound text goes
through the Messages API on the host's HTTP client, and past incoming messages
can be imported from the account's message list.

| | |
| --- | --- |
| Key | `twilio.sms` |
| Package | `hop.top/uma/adapter/messaging/twilio/sms`, in module `hop.top/uma/adapter/messaging/twilio` |
| Capabilities | `send`, `reconcile`, `receive_http`, `start_conversation`, `history` |
| Events | `message.received`, `delivery.updated` |
| Identity namespaces | `twilio.sms` (phone numbers in E.164) |
| Sender kinds | person |
| Provider permissions | none declared; the credential must be able to create and list messages |
| Message IDs and threading | assigned by Twilio; no threading |
| Reply parent | none: SMS names no replied-to message |
| Interactions | none ([interactions](../interactions.md)) |
| Maturity | experimental |

The matrix of all adapters is in [Adapters](README.md).

## Capabilities and operations

| Capability | Behavior |
| --- | --- |
| `receive_http` | `Runtime.Webhook` takes incoming messages and status callbacks (form-encoded or JSON POSTs) |
| `send` | `Messages.Reply` creates a message through the Messages API |
| `start_conversation` | `Conversations.Start` sends to any number, one that never wrote in included |
| `reconcile` | After a lost answer, lists the account's messages for the one the attempt created, by route, sender, body and the attempt's record time. "Not found" counts only after a 5 minute settle window, because the list shows a new message with a lag |
| `history` | `Runtime.Import` reads the messages the channel's number received since a time, for one conversation or the whole channel, oldest first. See [History](#history) |

## Not declared

Reason classes are defined in [Adapters](README.md#why-a-capability-is-not-declared).

| Capability | Reason | Why |
| --- | --- | --- |
| `reply_recipients` | `provider` | An SMS goes to one number |
| `receive` | `by design` | Twilio pushes webhooks; the message list serves `history`, not a receiver loop |
| `edit` | `provider` | An SMS cannot be changed once sent |
| `delete` | `provider` | An SMS cannot be recalled |
| `react` | `provider` | SMS has no reactions |
| `receipt` | `provider` | Delivery receipts flow from Twilio to uma, never from uma to the sender |
| `membership` | `provider` | Programmable Messaging has no groups; Twilio Conversations is a separate product |
| `refetch` | `provider` | A message never changes |
| `work_item_act` | `provider` | SMS has no work items |
| `internal_note` | `provider` | SMS has no private messages; notes stay in uma |
| `extension_execute` | `not built` | Cancelling a scheduled message could be an effect |
| `extension_query` | `not built` | Lookup and message status reads could be queries |
| `probe_permissions` | `not built` | The API key's permissions could be probed |
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

- **Provider.** Two credential purposes, resolved per request and never read
  from the environment.
  - `twilio.rest` authenticates Messages API calls: the auth token as a secret
    (with the configured account SID), or a password of the account SID and
    auth token, or of an API key SID (`SK...`) and its secret.
  - `twilio.webhook` is the account's auth token, which signs webhooks: a
    secret, or a password whose secret is the token.
- **uma.** The receiving principal needs `channel.receive`; replies are
  authorized as `message.reply`, started conversations as
  `conversation.start`.

## Configuration

`twilio.sms.config` version 1, as JSON in the channel configuration.

| Field | Required | Meaning |
| --- | --- | --- |
| `account_sid` | yes | The Twilio account (`AC...`) |
| `from` | one of | The sending number |
| `messaging_service_sid` | one of | A Messaging Service (`MG...`) to send through instead |
| `webhook_url` | for receiving | The public URL configured on the number or service, exactly as entered there. The signature check uses it, not the URL the request reached the host with. Outbound messages ask for status callbacks at the same URL. Without it the channel only sends and its webhook answers 503 |

Host options (`sms.Options`): `BaseURL` (Twilio origin override, for tests; a
host setting so a channel cannot redirect credentials) and `Timeout` (one REST
call; default 30 seconds). Calls go through `uma.Config.HTTP`; without it
`Send` fails as unavailable and webhooks still work.

## Receiving

The handler verifies `X-Twilio-Signature` over the configured URL, the query and
every POST parameter before the sink sees anything. A [JSON body](#json-bodies)
is verified by its URL and body hash instead.

| Answer | When |
| --- | --- |
| 200 with empty TwiML | The sink accepted the event, new or duplicate |
| 400 | Unreadable, malformed or incomplete body; a JSON body whose URL lacks one valid `bodySHA256` |
| 403 | Missing or wrong signature, a JSON body that does not match its `bodySHA256`, or another account's request |
| 405, 413 | Method other than POST; body over the engine's webhook limit |
| 422 | A sink failure that retrying will not fix |
| 503 | No `webhook_url`, the instance is closed, the token cannot be resolved, or a retryable sink failure |

A request carrying `MessageStatus` is a status callback: a status envelope with
the mapped delivery state and, when the callback URL carries them, the operation
and attempt that `Send` tagged it with, so the engine settles that attempt. The
engine weighs these reports by rank, not arrival (see
[Delivery status reports](../implementers.md#delivery-status-reports)): a
message already `provider_accepted` moves to `delivered` or `failed` when its
callback arrives, and a late `sent` never lowers `delivered`. Twilio's `read`
is `delivered`, with no read time.
Any other request is an incoming message. An incoming MMS's media are
downloaded from the account's Media resources with the REST credential, only
from the API origin, and staged through the engine's attachment sink. At most
10 media entries are read from one message.

### Incoming media

Twilio [enforces HTTP Basic authentication on media URLs](https://www.twilio.com/docs/messaging/api/media-resource)
and keeps the media until the Media resource is deleted. What a download does
depends on the answer:

| Twilio answers | The media | The webhook |
| --- | --- | --- |
| 2xx | Staged | 200, as above |
| 301, 302, 303, 307 or 308 | The redirect is followed ([below](#redirects)) and the final answer counts | |
| 404, 410 | Not staged; `unavailable` as `gone` | 200 |
| 401, 403 from the API origin | Not staged; `unavailable` as `unauthorized`, and no other media of that message is tried | 200 |
| Any other 4xx; a 3xx that is no redirect; a refused redirect | Not staged; `unavailable` as `failed`, or `redirect` for a redirect | 200 |
| 408, 429, any 5xx, no answer, a timeout, a dropped connection | The message is not accepted | 503 |
| The REST credential cannot be resolved for good | Not staged; `unavailable` as `unauthorized` | 200 |

A 401 or 403 after a redirect is not `unauthorized`: that request carried no
credential, so it says nothing about the REST credential (it is `failed`).

The message keeps its text and its media summary (`twilio.sms.media`). A media
that failed for good is listed in the summary's `unavailable` field as
`{"index": n, "reason": "..."}`, `index` being the media's position in the
message (`MediaUrl{n}`) and `reason` one of `gone`, `unauthorized`, `redirect`
or `failed`. The field is the only record of the loss: the adapter keeps no
Media URL and does not fetch the media again later, so after an `unauthorized`
is fixed (the `twilio.rest` credential) the media of messages already accepted
stay at Twilio, not in the engine. A media that the engine refuses (size,
type, no blob store) or whose URL is off the account's Media resources is
skipped without an entry.

A 503 asks Twilio to deliver the webhook again, but Twilio does so only as the
webhook's retry policy says. By default it retries only after a TCP or TLS
connection failure (policy `ct`, one retry), not after a 5xx or a timeout; the
[connection overrides](https://www.twilio.com/docs/usage/webhooks/webhooks-connection-overrides)
`rp=5xx` and `rt` add them. Without them a message answered 503 is not
redelivered; only a [history](#history) import brings its text in later, without
its media.

#### Redirects

Twilio's documentation does not describe a redirect on media downloads;
reports of one to a media store on another host are unverified, so the
download handles a redirect without assuming one:

- At most 3 redirects are followed.
- The REST credential goes only to the API origin. A hop to any other origin
  carries none, and a redirect back to the API origin does not get it back.
- A hop to another origin must be `https` on the default port, name no user and
  password, and name a public host: an address that is not loopback, private,
  link-local, shared (100.64.0.0/10), documentation or otherwise reserved, and a
  name that has a dot, an alphabetic last label and is not `localhost`,
  `.local`, `.internal` or the like. The check reads the host as written in
  the URL: a name that resolves to a private address passes it, so a host that
  must stop that gives `uma.Config.HTTP` a dialer that refuses such addresses.
- The redirect target is never kept or reported, since it can carry a
  signature.
- The response body size is bounded by the engine's attachment limit.

An `*http.Client` given as `uma.Config.HTTP` has its own redirect following
switched off for these downloads; a client of another type must not follow
redirects itself, or these rules are out of the adapter's hands.

### JSON bodies

Twilio's default messaging webhooks are form-encoded, but it can also post
`application/json`, and a relay may. For a JSON body
([Webhooks security](https://www.twilio.com/docs/usage/webhooks/webhooks-security))
Twilio signs the URL alone, with no POST parameters, and adds the hex SHA-256 of
the raw body to that URL as the `bodySHA256` query parameter. The handler checks
both, in constant time: the signature of the URL as called, `bodySHA256` included,
and the hash of the bytes received. The signature covers the body only through
that hash, so a JSON request whose URL has no `bodySHA256`, an invalid or a
repeated one is refused (400) rather than accepted on a signature that
authenticates no body.

The object's top-level strings, numbers and booleans are read as the form
parameters of the same names (`MessageSid`, `From`, `To`, `Body`, `AccountSid`,
`NumMedia`, `MediaUrl0`, `MessageStatus`, ...); null, nested objects and arrays
are ignored. Everything else works as for a form, including the other account
check, media staging and the status callback tags, which are read from the
query beside `bodySHA256`. The `webhook_url` stays without a query, exactly as
entered at Twilio.

## Sending

The Messages API has no idempotency key. A lost answer, or a 5xx after the
request reached Twilio, is `outcome_unknown` and the engine does not resend
before `Reconcile`. Text parts only; a non-public audience is `unsupported`
(SMS has no private message).

## History

`History` lists the account's messages
([Message resource](https://www.twilio.com/docs/messaging/api/message-resource))
with `To` set to the channel's `from` number, `From` set to the conversation's
number when one is asked for, and a `DateSent>` lower bound.

- **Inbound only.** Only messages Twilio received (direction `inbound`) are
  history. The channel's own sends are outbound messages, stored already from
  `Send` and `Reconcile` with their message SIDs, so they never come back as
  new incoming messages. Outbound messages that another client of the account
  sent are left out too: an import envelope has no form for a message the
  channel wrote.
- **Same message as the webhook.** Each message is the envelope the webhook
  reports for it: the message SID as the provider message ID, the channel's
  number as the destination, the sender's number as the sender. An import
  skips a message the webhook already delivered. Its time is Twilio's
  `date_sent`, when Twilio received it. An MMS carries its media count only;
  history stages no media.
- **Order and pages.** Twilio lists newest first and documents the `DateSent`
  filter by day only. The reader asks for the UTC day holding the window's
  start, reads pages of 1000 from the newest until one holds an older message
  or the list ends, keeps the window, and returns it oldest first by
  `date_sent`, then message SID within one second. The cursor is that position
  (never a Twilio page URI, which is refused as `validation`). Every page reads
  the window from its lower bound again, so no message appears on two pages
  and a message arriving meanwhile comes after the cursor.
- **Window start.** Twilio dates have one-second resolution: a message in the
  second holding the start is in, one in the second before is out.
- **Gaps.** Past 10 list pages (10,000 messages) from the newest, the older
  part of the window is out of reach: the page reports a gap and holds the
  oldest messages read. A received message without a readable `date_sent`
  is left out and also reported as a gap. Twilio keeps messages for a limited
  time and deleted messages are gone; the reader cannot tell those apart from
  messages that never existed.
- **Messaging Service channels.** A channel with only `messaging_service_sid`
  names no number of its own: `history` reports unavailable (`unsupported`)
  and `History` refuses.
- **Failures.** A 429 (error 20429) is `rate_limited` with Twilio's
  `Retry-After`; refused credentials are `unauthorized`; other 4xx answers are
  permanent, the rest transient. A read changes nothing, so the engine may
  repeat it.

## Limits

| What | Limit |
| --- | --- |
| REST response body read | 1 MiB |
| One REST call | 30 seconds unless the caller's context is shorter |
| Media entries read per incoming message | 10 |
| Redirects followed per media download | 3 |
| Reconcile settle window | 5 minutes |
| History listing | list pages of 1000, at most 10 list pages per history page |
| Content | text parts only |

## How it is tested

The shared adapter suite (including the `history` reader cases, over list
pages of two messages) and package tests run against a scripted Twilio
service, with signed webhook fixtures; an engine test imports a conversation
the webhook already delivered a message of. Recorded cassettes **exist** for
this adapter: send and fetch, reconcile (sent and none), rejected and refused
credentials, and an unknown fetch. They replay under `go test` with no
credentials. To refresh them, follow
[Record Twilio cassettes](../recording/twilio.md).
The scripted service answers an accepted send in the shape of the recorded
create answer, not a documented one.

History is recorded too: `TestRecordedHistory` replays one send and one
history listing of an inbound reply, with no credentials. To refresh it,
follow the
[history recording](../recording/twilio.md#record-twilio-sms-history).

## Known limits

- Text out only; no outbound media or templates.
- A media that failed for good is not fetched again, and a credential fixed
  afterwards does not recover the media of messages already accepted (see
  [Incoming media](#incoming-media)); the summary's `unavailable` list is the
  record.
- Whether Twilio redirects media downloads, and where to, is unverified
  against real traffic: the download follows up to 3 redirects under the rules
  in [Redirects](#redirects) and refuses the rest.
- History holds incoming messages only, and needs the channel's own number.
- SMS length and segmenting are Twilio's: the adapter sets no limit of its own.
- Experimental: configuration may change ([Compatibility](../compatibility.md)).
