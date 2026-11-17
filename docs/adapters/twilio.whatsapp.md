# twilio.whatsapp

`twilio.whatsapp` sends and receives WhatsApp messages through Twilio
Programmable Messaging. One channel is one WhatsApp sender on a Twilio account,
or a Messaging Service holding WhatsApp senders. It shares the Messages API and
the `X-Twilio-Signature` webhooks with [`twilio.sms`](twilio.sms.md); Twilio
addresses WhatsApp users as `whatsapp:` plus the number, or plus a
[user ID](#user-ids) when WhatsApp hides the number, and uma routes and
identities carry the address without that prefix.

| | |
| --- | --- |
| Key | `twilio.whatsapp` |
| Package | `hop.top/uma/adapter/messaging/twilio/whatsapp`, in module `hop.top/uma/adapter/messaging/twilio` |
| Capabilities | `send`, `reconcile`, `receive_http`, `start_conversation` |
| Events | `message.received`, `delivery.updated` |
| Identity namespaces | `twilio.whatsapp` (E.164 numbers, or [user IDs](#user-ids) `CC.<id>`) |
| Sender kinds | person |
| Provider permissions | none declared; the credential must be able to create and list messages |
| Message IDs and threading | assigned by Twilio; no threading |
| Reply parent | reported: `OriginalRepliedMessageSid` |
| Interactions | none ([interactions](../interactions.md)) |
| Maturity | experimental |

The matrix of all adapters is in [Adapters](README.md).

## Capabilities and operations

| Capability | Behavior |
| --- | --- |
| `receive_http` | `Runtime.Webhook` takes incoming messages and status callbacks |
| `send` | `Messages.Reply`: free-form text, or one template part outside the service window |
| `reconcile` | After a lost answer, lists the account's messages for the free-form one the attempt created. A template message is not matched. "Not found" counts only after a 5 minute settle window |
| `start_conversation` | `Conversations.Start` sends to any user, one who never wrote in included. A user with no open window takes only a template part; see [The 24-hour window](#the-24-hour-window) |

## Not declared

Reason classes are defined in [Adapters](README.md#why-a-capability-is-not-declared).

| Capability | Reason | Why |
| --- | --- | --- |
| `reply_recipients` | `provider` | A WhatsApp message goes to one user |
| `receive` | `by design` | Twilio pushes webhooks; a receiver loop over the message list would duplicate them |
| `edit` | `provider` | WhatsApp lets a business change nothing it sent |
| `delete` | `provider` | WhatsApp lets a business recall nothing it sent |
| `react` | `provider` | Twilio documents no reaction send and no reaction webhook (checked October 2026) |
| `receipt` | `not built` | WhatsApp has mark-as-read; whether Twilio exposes it is unverified |
| `membership` | `provider` | Programmable Messaging has no groups; Twilio Conversations is a separate product |
| `history` | `not built` | The message list could import past messages, as [`twilio.sms`](twilio.sms.md#history) does |
| `refetch` | `provider` | A message never changes |
| `work_item_act` | `provider` | WhatsApp has no work items |
| `internal_note` | `provider` | WhatsApp has no private messages; notes stay in uma |
| `extension_execute` | `not built` | No provider effect identified yet |
| `extension_query` | `not built` | Content Template listing could be a read |
| `probe_permissions` | `not built` | The API key's permissions could be probed |
| `stream` | `provider` | WhatsApp has no streamed messages |
| `send_ephemeral` | `provider` | WhatsApp has no message only one reader sees |
| `signal` | `provider` | Twilio documents no typing indicator |
| `respond` | `by design` | A quick-reply button press or list reply is a message in the chat with its own ID, reported as one, not an interaction |
| `respond_follow_up` | `by design` | As `respond` |
| `archive` | `provider` | A phone thread has no archive, folder, flag or trash |
| `move` | `provider` | A phone thread has no archive, folder, flag or trash |
| `read_state` | `provider` | A phone thread has no archive, folder, flag or trash |
| `flag` | `provider` | A phone thread has no archive, folder, flag or trash |
| `trash` | `provider` | A phone thread has no archive, folder, flag or trash |
| `raw_read` | `not built` | Raw read is built for mail only; the provider's whole copy of a message is not read |

## Permissions

- **Provider.** Credential purposes `twilio.rest` and `twilio.webhook`, exactly
  as for [`twilio.sms`](twilio.sms.md#permissions); `twilio.rest` also reads
  incoming media.
- **uma.** The receiving principal needs `channel.receive`; replies are
  authorized as `message.reply`.

## Configuration

`twilio.whatsapp.config` version 1, as JSON in the channel configuration.

| Field | Required | Meaning |
| --- | --- | --- |
| `account_sid` | yes | The Twilio account (`AC...`) |
| `from` | one of | The WhatsApp sender's number in E.164, without `whatsapp:` |
| `messaging_service_sid` | one of | A Messaging Service (`MG...`) with WhatsApp senders |
| `webhook_url` | for receiving | The public URL configured on the sender or service, exactly as entered there; the signature check and status callbacks use it. Without it the channel only sends and its webhook answers 503 |

Host options (`whatsapp.Options`): `BaseURL` and `Timeout`, as for
`twilio.sms`.

## The 24-hour window

WhatsApp allows free-form messages only inside the 24-hour customer service
window that a user's message opens. Outside it only an approved template may be
sent. A send carries either text parts or exactly one `twilio.whatsapp.template`
part (`content_sid` of the Content Template, `content_variables`). When the host
gives the instance protocol state, each verified inbound message records the
user's window, and a free-form send after a recorded window closed is rejected
before any call. Without that state, or for a user with no recorded inbound
message, Twilio decides and answers error 63016. A non-public audience is
`unsupported`.

## Receiving

Signature checks, status codes and callbacks work as for `twilio.sms`, including
[JSON-bodied webhooks](twilio.sms.md#json-bodies) verified by `bodySHA256`. Beyond
plain text the adapter reports a quick-reply button press (its hidden
`ButtonPayload`, variables included, and its `ButtonText` label), a shared
location and the count and types of media, each as a typed payload on the
message envelope.

An incoming message's media are downloaded from the account's Media resources
with the `twilio.rest` credential, only from the API origin, and staged through
the engine's attachment sink exactly as for
[`twilio.sms`](twilio.sms.md#receiving): each item becomes an attachment part
of the message, keyed by its media SID so a redelivery restages the same one,
with the content type Twilio reported. At most 10 media entries are read per
message. The Media URL itself is not kept. A media URL off the account's Media
resources, or one the engine refuses (size, type, no blob store), is skipped
and the message keeps its summary. Media gone at Twilio (404, 410), refused for
its credential (401, 403 from the API origin; the media of that message are
then not tried further, and a credential that cannot be resolved for good is
the same), refused for a redirect, or answered otherwise in a way a repeat
would not change are skipped too, but named in the summary's `unavailable`
list with the reason `gone`, `unauthorized`, `redirect` or `failed`, as for
[`twilio.sms`](twilio.sms.md#incoming-media), which also states the redirect
rules (at most 3, the credential only to the API origin, public `https` hosts
only). A failure a redelivery may fix (Twilio unreachable, a timeout, a 408,
a rate limit or server fault, the credential or the staging store failing for
now) answers 503 so that Twilio can redeliver, before the customer service
window moves; by default Twilio redelivers only after a connection failure,
not after a 5xx or a timeout, and a 503 it does not retry loses the message
(see the [retry policy](twilio.sms.md#incoming-media)). Without an HTTP client
for the instance, or a sink that stages attachments, a message arrives with
the summary only. Every send tags its status callback URL with the operation
and attempt, and the status envelope names that attempt. The engine weighs the
reports by rank, not arrival, so a message already `provider_accepted` moves to
`delivered` or `failed` when its callback arrives (see
[Delivery status reports](../implementers.md#delivery-status-reports)); Twilio's
`read` is `delivered`, with no read time.

### User IDs

WhatsApp lets a user adopt a username that hides their phone number. Twilio
then names that user by a business-scoped user ID: a country code, a dot and
up to 128 letters and digits, such as `CA.0000000000000002`, prefixed with
`whatsapp:` like a number. Without a visible number the ID is the message's
`From`, and so the sender's identity. With one, `From` is the number and the
ID arrives beside it in `ExternalUserId`. Either way the adapter reports the
ID, without the prefix, as `external_user_id` in the message's
`twilio.whatsapp.inbound` payload, next to `profile_name`, `wa_id` (absent
without a number), `message_type` and `replied_to`.

Every incoming message also reports the sender's verified attributes on the
envelope (`Envelope.SenderAttributes`): `phone` from `From` when it is a number
(or from `WaId` when `From` is not one), and `whatsapp_user_id` from
`ExternalUserId` (or from `From` itself when it is an ID and Twilio sent no
`ExternalUserId`). A value that is not a valid attribute is left out and the
message is still accepted. The engine records them on the sender's identity when
it first claims it; later messages never change them. The identity key stays
the address: one person who wrote from a number and later from a username has two
identities, and the user ID they share is the verified attribute a host's
`verified_attribute` verifier links them on; two identities that show the same
number share the `phone` attribute as well
([Twilio](https://www.twilio.com/en-us/changelog/whatsapp-usernames--new-business-scoped-user-id--bsuid--field-re),
[WhatsApp](https://developers.facebook.com/documentation/business-messaging/whatsapp/business-scoped-user-ids/)).

WhatsApp issues a new user ID when the user changes number. The identity under
the new number then carries a new phone and a new user ID, shares no
attribute with the old identity, and no verified-attribute link joins them;
only a desk's attestation does. Once that user writes with the number hidden, the
new ID keys a third identity that shares the new user ID with the second. A user
ID is unique to one business portfolio, so a number that serves several
portfolios sees one ID per portfolio.

A send or a start to a user ID, or to a parent ID (`US.ENT.<id>`), goes to
Twilio as `whatsapp:` plus the ID, unchanged. Twilio delivers it as it does a
send to a number. One-tap, zero-tap and copy-code authentication templates
need the number; WhatsApp refuses them to a user ID.

## Limits

| What | Limit |
| --- | --- |
| Customer service window | 24 hours |
| REST response body read | 1 MiB |
| One REST call | 30 seconds unless the caller's context is shorter |
| Media entries read per incoming message | 10 |
| Redirects followed per media download | 3 |
| Reconcile settle window | 5 minutes |

## How it is tested

The shared adapter suite and package tests run against a scripted Twilio
service, with signed webhook fixtures for text, image, button, location,
messages from a user ID with and without a visible number, and status
callbacks, JSON-bodied ones included, and media downloads from it. The
recorded test (`TestRecordedSendAndFetch`) replays committed cassettes of a
send and its fetch. Everything else, media downloads included, is unverified
against real Twilio WhatsApp senders. To record, follow
[Record Twilio WhatsApp cassettes](../recording/twilio.md#record-twilio-whatsapp-cassettes).

## Known limits

- No outbound media, no reactions, edits or deletes.
- Incoming media are staged only when the host gives the instance an HTTP
  client and a blob store; otherwise the message carries the media count and
  types only. A Media URL is read only if it names the message's own Media
  resource on the API origin.
- A media that failed for good (gone, credential refused, redirect refused) is
  named in the media summary's `unavailable` list and not fetched again; see
  [Incoming media](twilio.sms.md#incoming-media) for the reasons, the redirect
  rules and what a 503 does.
- A free-form start to a user who never wrote in is Twilio's to refuse (error
  63016); the adapter has no window to check.
- A template message cannot be reconciled after a lost answer.
- Window state needs the host's protocol state; without it Twilio enforces the
  window.
- One identity per address: a user who hides their number after writing from
  it arrives under a new identity keyed by the user ID, not linked to the
  number's identity; the engine links them only when a host's
  `verified_attribute` verifier runs on the shared `whatsapp_user_id` (see
  [User IDs](#user-ids)). An identity that already exists keeps the
  attributes of its first message: one created before a user ID was reported
  carries none. A user who changes number is a new identity with no attribute
  in common with the old one.
- Experimental: configuration may change ([Compatibility](../compatibility.md)).
