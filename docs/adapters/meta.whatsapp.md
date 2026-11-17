# meta.whatsapp

`meta.whatsapp` is the WhatsApp Cloud API adapter (Meta). One channel is one
business phone number. Webhooks arrive signed; sends and reactions go through
the Graph API on the host's HTTP client.

| | |
| --- | --- |
| Key | `meta.whatsapp` |
| Package | `hop.top/uma/adapter/messaging/meta/whatsapp`, in module `hop.top/uma/adapter/messaging/meta` |
| Capabilities | `receive_http`, `send`, `react`, `start_conversation` |
| Events | `message.received`, `message.updated`, `delivery.updated` |
| Identity namespaces | `meta.whatsapp` |
| Sender kinds | person |
| Provider permissions | `whatsapp_business_messaging` |
| Message IDs and threading | assigned by Meta (the `wamid`); no threading; one reaction per user per message |
| Reply parent | reported: the message `context.id`; a forward carries none |
| Interactions | none ([interactions](../interactions.md)) |
| Maturity | experimental |

The matrix of all adapters is in [Adapters](README.md).

## Capabilities and operations

| Capability | Behavior |
| --- | --- |
| `receive_http` | `Runtime.Webhook` answers Meta's subscription verification (GET) and takes event notifications (POST) |
| `send` | `Messages.Reply` posts one text message or one template message to the stored route |
| `react` | `Messages.React` sends a reaction message naming a provider message (inbound or outbound) and an emoji; a removal sends an empty emoji |
| `start_conversation` | `Conversations.Start` sends to any user, one who never wrote in included. A user with no open window takes only a template part; see [Sending](#sending) |

## Not declared

Reason classes are defined in [Adapters](README.md#why-a-capability-is-not-declared).

| Capability | Reason | Why |
| --- | --- | --- |
| `reply_recipients` | `provider` | A WhatsApp message goes to one user |
| `receive` | `provider` | The Cloud API has no call to list or poll messages; webhooks are the only ingress |
| `reconcile` | `provider` | No idempotency key and no call to look up or list sent messages: a lost answer stays `outcome_unknown`, never resent, and settles only through status webhooks |
| `edit` | `provider` | The Cloud API has no call to change a sent message |
| `delete` | `provider` | The Cloud API has no call to take back a sent message |
| `receipt` | `not built` | The Cloud API marks a message read (`status: read` on `/messages`); nothing sends it yet |
| `membership` | `not built` | The Cloud API Groups API (Official Business Accounts, invite-only, 8 participants) is being looked at |
| `history` | `provider` | No call lists past messages |
| `refetch` | `provider` | A message never changes |
| `work_item_act` | `provider` | WhatsApp has no work items |
| `internal_note` | `provider` | WhatsApp has no private messages; notes stay in uma |
| `extension_execute` | `not built` | Template management through the Business Management API could be effects |
| `extension_query` | `not built` | Template listing could be a read |
| `probe_permissions` | `not built` | `debug_token` could probe the token's scopes |
| `stream` | `provider` | WhatsApp has no streamed messages |
| `send_ephemeral` | `provider` | WhatsApp has no message only one reader sees |
| `signal` | `not built` | The Cloud API typing indicator (sent with mark-as-read) is not sent |
| `respond` | `by design` | A button press or list reply is a message in the chat with its own `wamid`, reported as one, not an interaction |
| `respond_follow_up` | `by design` | As `respond` |
| `archive` | `provider` | A phone thread has no archive, folder, flag or trash |
| `move` | `provider` | A phone thread has no archive, folder, flag or trash |
| `read_state` | `provider` | A phone thread has no archive, folder, flag or trash |
| `flag` | `provider` | A phone thread has no archive, folder, flag or trash |
| `trash` | `provider` | A phone thread has no archive, folder, flag or trash |
| `raw_read` | `not built` | Raw read is built for mail only; the provider's whole copy of a message is not read |

## Permissions

- **Provider.** Three credential purposes, resolved through `ports.Credentials`:
  `meta.graph` (the access token, OAuth 2 or a secret), `meta.app_secret` (the
  signing secret for `X-Hub-Signature-256`) and `meta.webhook.verify_token` (the
  subscription verify token). The token reaches only the Graph origin and the
  media host.
- **uma.** The receiving principal needs `channel.receive`; replies are
  authorized as `message.reply` and reactions as `message.react`.

## Configuration

`meta.whatsapp.config` version 1, as JSON in the channel configuration.

| Field | Required | Meaning |
| --- | --- | --- |
| `phone_number_id` | yes | The business phone number's Graph ID |
| `api_version` | no | Graph API version; default `v24.0` |

Host options (`whatsapp.Options`): `BaseURL`, the Graph origin (a host setting,
so a channel cannot redirect its token). Calls go through `uma.Config.HTTP`; an
instance opened without it fails every send without a call.

## Receiving

The handler verifies `X-Hub-Signature-256` over the raw body before anything
reaches the sink, and takes only changes whose verified
`metadata.phone_number_id` is the channel's own.

- Inbound messages become message envelopes, with the sender's profile name and
  `wa_id` as unverified claims. Media is looked up and downloaded with the
  access token, only from the Graph origin and the media host, and staged as an
  attachment when the sink stages attachments. A message type without a text
  form (buttons, interactive replies) is kept as a structured part with its
  type and payload IDs.
- Status notifications (`sent`, `delivered`, `read`, `played`, `failed`) become
  status envelopes carrying delivery evidence. `read` and `played` map to
  `delivered`. The engine weighs the evidence by rank, not arrival: a send
  already `provider_accepted` moves to `delivered` or `failed` when its status
  arrives, a late `sent` never lowers `delivered`, and a delivery outranks a
  failure. Meta's notification carries no read time the adapter reports, so a
  read leaves the operation `delivered` with no `ReadConfirmedAt`.
- A user's reaction is a `reaction.added` envelope; a removal, which WhatsApp
  reports without an emoji, is a `reaction.removed` resolved against the
  sender's one reaction on the message.

## Sending

A free-form text message is only allowed inside the 24-hour customer service
window a user's message opens. Outside it a template message is required: a
structured part of schema `meta.whatsapp.template` holding the Cloud API
template object (`name`, `language.code`, `components`). Meta enforces the
window and its refusal maps to `rejected`. When the host gives the instance
protocol state, the adapter records the window from verified inbound messages
and refuses a free-form message after a recorded window closed, before any
provider call. A reaction follows the window like a free-form message.

Each send carries its attempt and operation in `biz_opaque_callback_data`, which
Meta echoes in status notifications; the status envelope then names the attempt
next to the `wamid`, and the engine settles the operation from it.

## Limits

| What | Limit |
| --- | --- |
| Customer service window | 24 hours |
| Webhook body | 4 MiB |
| Graph response body read | 1 MiB |
| Reactions per user per message | 1 (a new emoji replaces the old one) |
| Content of one send | exactly one text part or one template part |

## How it is tested

The shared adapter suite and package tests run against a scripted Graph API
and signed webhook fixtures. The recorded tests (`TestRecordedTemplateSend`,
`TestRecordedUnknownTemplateRejected`) replay committed cassettes, and **none
are recorded yet**, so they skip. Behavior against the real Cloud API is
unverified. To record, follow
[Record WhatsApp Cloud API cassettes](../recording/meta-whatsapp.md).

## Known limits

- No read confirmation time (`read` and `played` map to `delivered`, and
  `Operation.ReadConfirmedAt` stays empty) and no mention form.
- No `reconcile`: an unknown outcome waits for a status webhook.
- No edit or delete.
- A free-form start to a user who never wrote in is Meta's to refuse; the
  adapter has no window to check.
- Outbound media is not sent; text, template and reaction messages only.
- Experimental: configuration may change ([Compatibility](../compatibility.md)).
