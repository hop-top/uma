# zendesk.tickets

`zendesk.tickets` connects Zendesk Support: one channel per account, one work
item per ticket and one message per comment. It talks to the Support API over
the host's HTTP client with the standard library only. Public comments are
public messages; internal notes are restricted messages and never public.

| | |
| --- | --- |
| Key | `zendesk.tickets` |
| Package | `hop.top/uma/adapter/ticket/zendesk`, own module `hop.top/uma/adapter/ticket/zendesk` |
| Capabilities | `receive`, `receive_http`, `send`, `reconcile`, `extension_execute`, `internal_note` |
| Events | `message.received`, `message.updated`, `work_item.updated`, `delivery.updated` |
| Identity namespaces | `zendesk.tickets` |
| Sender kinds | person |
| Provider permissions | `agent`, `admin:webhooks`, `admin:incremental_export` |
| Message IDs and threading | assigned by Zendesk; per-work-item inboxes |
| Reply parent | none: ticket comments name no replied-to comment |
| Interactions | none ([interactions](../interactions.md)) |
| Maturity | experimental |

The matrix of all adapters is in [Adapters](README.md).

## Capabilities and operations

| Capability | Behavior |
| --- | --- |
| `receive_http` | `Runtime.Webhook` serves a Zendesk webhook subscribed to ticket events |
| `receive` | `Runtime.Receive` polls the incremental ticket event export, so comments a webhook missed still arrive |
| `send` | `Messages.Reply` adds a public comment |
| `internal_note` | `Messages.AddInternalNote` adds a private comment, in an inbox that opted in (`DeliverNotes`); elsewhere notes stay in uma |
| `extension_execute` | `Extensions.Execute` runs `zendesk.tickets.internal_note` or `zendesk.tickets.update` |
| `reconcile` | Reads the ticket's audits for the attempt's marker |

## Not declared

Reason classes are defined in [Adapters](README.md#why-a-capability-is-not-declared).

| Capability | Reason | Why |
| --- | --- | --- |
| `reply_recipients` | `not built` | A ticket update's `email_ccs` could carry cc recipients on a public comment; nothing sends them |
| `start_conversation` | `engine` | A start on a per-work-item inbox opens a ticket; core has no path for a start that creates a work item |
| `work_item_act` | `by design` | Ticket status changes go through the `zendesk.tickets.update` effect, not `WorkItems.Act` |
| `edit` | `provider` | Comments cannot be edited; Zendesk only redacts them |
| `delete` | `provider` | Comments cannot be deleted; Zendesk only redacts them |
| `react` | `provider` | Tickets have no reactions |
| `receipt` | `provider` | Tickets have no delivery or read receipts |
| `membership` | `not built` | `email_ccs` and `followers` on a ticket update; being looked at |
| `history` | `not built` | A ticket's comments and the incremental export could import past comments |
| `refetch` | `not built` | Audits carry IDs; nothing re-reads a ticket on a revision tie |
| `extension_query` | `not built` | Ticket field, user and group listings could be reads |
| `probe_permissions` | `not built` | `users/me` roles could be probed |
| `stream` | `provider` | Tickets have no streamed comments |
| `send_ephemeral` | `provider` | A comment is public or an internal note; nothing is shown to one reader only |
| `signal` | `provider` | Tickets have no typing or presence |
| `respond` | `provider` | Tickets have no interactions |
| `respond_follow_up` | `provider` | As `respond` |
| `archive` | `by design` | State goes through `work_item_act` and the `zendesk.tickets.update` effect; ticket lifecycle is not conversation state |
| `move` | `by design` | State goes through `work_item_act` and the `zendesk.tickets.update` effect; ticket lifecycle is not conversation state |
| `read_state` | `by design` | State goes through `work_item_act` and the `zendesk.tickets.update` effect; ticket lifecycle is not conversation state |
| `flag` | `by design` | State goes through `work_item_act` and the `zendesk.tickets.update` effect; ticket lifecycle is not conversation state |
| `trash` | `by design` | State goes through `work_item_act` and the `zendesk.tickets.update` effect; ticket lifecycle is not conversation state |
| `raw_read` | `not built` | Raw read is built for mail only; the provider's whole copy of a message is not read |

## Extension effects

Both effects are version 1 and run through `Extensions.Execute`. The factory
refuses invalid fields at the call (`adapter.ActionValidator`), with nothing
stored; `Execute` applies the same rules again at the attempt.

| Effect | Authorized as | Rules |
| --- | --- | --- |
| `zendesk.tickets.internal_note` | `message.note` | Needs a `body` or `html_body`; never a public audience; sets no `public` flag |
| `zendesk.tickets.update` | `work_item.act` | Updates ticket fields; needs at least one; may not set `comment`, `metadata`, `safe_update` or `updated_stamp` |

## Permissions

- **Provider.** One credential reference per channel, resolved per call.
  Purpose `zendesk.tickets.api`: with `auth` set to `api_token`, a secret (with
  `email` in the configuration) or a password (agent address and token), sent as
  HTTP Basic `{email}/token:{token}`; with `oauth`, an OAuth 2 token or a secret
  holding one, sent as a bearer token (the host owns consent and refresh).
  Purpose `zendesk.tickets.webhook` is the webhook's signing secret.
- **uma.** The receiving principal needs `channel.receive`; replies are
  authorized as `message.reply`, notes as `message.note`, ticket updates as
  `work_item.act`.

## Configuration

`zendesk.tickets.config` version 1, as JSON in the channel configuration.

| Field | Required | Meaning |
| --- | --- | --- |
| `subdomain` | yes | `https://{subdomain}.zendesk.com` |
| `auth` | yes | `api_token` or `oauth` |
| `email` | with a bare secret | The agent address an API token belongs to |
| `webhook_tolerance_seconds` | yes | Replay window of signed webhooks, 1 to 3600 |
| `poll_interval_seconds` | no | Wait between polls that found nothing; at least 6, default 30; each wait spread by +-10% so channels do not poll in phase, never below 6 |
| `poll_lookback_seconds` | no | How far back a connecting receiver reads; 60 seconds to 30 days, default 3600. Ingress deduplicates what an earlier connection read |
| `local_fields` | no | Work item fields uma owns for this channel |

Inboxes use per-work-item selection (`model.ModePerWorkItem`); envelope
destinations are brand IDs. Host options: `WithBaseURL` (override, for tests),
`WithHTTPClient` and `WithJitter` (source of the +-10% poll spread, uniform in
[0, 1]; the default is the standard library's generator).

## Receiving

Every webhook request is verified first: `X-Zendesk-Webhook-Signature` must equal
`base64(HMAC-SHA256(secret, timestamp + body))` in constant time, and the
timestamp header must be within `webhook_tolerance_seconds` of now.

Zendesk statuses map to work item statuses: `new` and `open` to open, `pending`
and `hold` to pending, `solved` to resolved, `closed` to closed. Merges bind the
ticket merged into its target; deletions and spam marks bind it deleted. A
comment whose visibility Zendesk does not state is restricted, never public.

## Sending and echoes

Every update carries the effect's operation, attempt, kind and audience in the
audit's custom metadata. A reply's or delivered note's echo is reported so the
engine settles it; an extension effect's echo is dropped. An update Zendesk may
have applied is never retried before reconciliation.

## Limits

| What | Limit |
| --- | --- |
| Poll interval | at least 6 seconds (the incremental export allows 10 requests a minute) |
| Poll look-back | 60 seconds to 30 days |
| Webhook tolerance | 1 second to 1 hour |
| API response body read | 8 MiB |

## How it is tested

The shared adapter suite and package tests run against a scripted Support API
with signed webhook fixtures. The recorded test (`TestRecordedSandbox`) replays
committed cassettes, and **none are recorded yet**, so it skips. Behavior against
a real Zendesk account is unverified. To record, follow
[Record Zendesk cassettes](../recording/zendesk.md).

## Known limits

- No `WorkItems.Act`; status changes use the update effect.
- Replies and notes carry text only; attachments are not sent.
- Experimental: configuration may change ([Compatibility](../compatibility.md)).
