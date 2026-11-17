# linear.issues

`linear.issues` connects Linear issues: one channel per workspace credential (a
personal API key or an OAuth access token), one work item per issue and one
message per issue description or comment. Replies are comments, and a comment
can be edited or deleted. Issue state is reported, not changed: this adapter
declares no `work_item_act`.

| | |
| --- | --- |
| Key | `linear.issues` |
| Package | `hop.top/uma/adapter/ticket/linear`, own module `hop.top/uma/adapter/ticket/linear` |
| Capabilities | `receive_http`, `send`, `reconcile`, `refetch`, `edit`, `delete` |
| Events | `message.received`, `message.updated`, `work_item.updated`, `delivery.updated` |
| Identity namespaces | `linear.issues` |
| Sender kinds | person, bot |
| Provider permissions | `read`, `comments:create`, `write` |
| Message IDs and threading | assigned by Linear; per-work-item inboxes |
| Reply parent | none: replies are top-level issue comments |
| Interactions | none ([interactions](../interactions.md)) |
| Maturity | experimental |

The matrix of all adapters is in [Adapters](README.md).

## Capabilities and operations

| Capability | On Linear | Needs |
| --- | --- | --- |
| `receive_http` | `Issue` and `Comment` webhooks | the webhook signing secret |
| `send` | `Messages.Reply` runs `commentCreate` | `replies`, `account_id` |
| `edit` | `Messages.Edit` runs `commentUpdate` | `replies`, `account_id` |
| `delete` | `Messages.Delete` runs `commentDelete` | `replies`, `account_id` |
| `reconcile` | Settles a lost answer by reading the comment | `replies` |
| `refetch` | Re-reads an issue or comment when two changes share a revision | read access |

Until `replies` is enabled, `send`, `edit` and `delete` report unavailable with
reason `disabled`.

## Not declared

Reason classes are defined in [Adapters](README.md#why-a-capability-is-not-declared).
Asking for one of these is `unsupported`.

| Capability | Reason | Why |
| --- | --- | --- |
| `reply_recipients` | `provider` | A comment has no recipient list; Linear notifies the issue's subscribers |
| `receive` | `not built` | Issues and comments could be polled with `updatedAt` ordering; webhooks are the ingress today |
| `start_conversation` | `engine` | A start on a per-work-item inbox opens an issue; core has no path for a start that creates a work item |
| `react` | `not built` | Linear has comment reactions (`reactionCreate`, `reactionDelete`); the emoji mapping is not built |
| `receipt` | `provider` | Linear has no delivery or read receipts for comments |
| `membership` | `by design` | Everyone who comments on an issue is a participant already; subscribers are not a participant list uma changes |
| `history` | `not built` | The `comments` and `issue` queries page through past comments (`orderBy: updatedAt`, up to 250 per page) |
| `work_item_act` | `not built` | `issueUpdate` with `stateId` changes the state; a state ID belongs to one team's workflow, so the transitions need a per-team lookup first |
| `internal_note` | `provider` | A Linear comment carries no audience; every comment is visible to whoever can read the issue |
| `extension_execute` | `not built` | `issueUpdate` sets assignee, labels and priority |
| `extension_query` | `not built` | Team, state and label listings could be reads |
| `probe_permissions` | `not built` | The `viewer` query could report the acting user, and the token's scopes come from the OAuth grant |
| `stream` | `provider` | Linear has no streamed comments |
| `send_ephemeral` | `provider` | Every comment is visible to every reader |
| `signal` | `provider` | Linear has no typing or presence for API clients |
| `respond` | `provider` | Issues have no interactions |
| `respond_follow_up` | `provider` | As `respond` |
| `archive` | `by design` | Issue lifecycle is a work item status, not conversation state |
| `move` | `by design` | Issue lifecycle is a work item status, not conversation state |
| `read_state` | `by design` | Issue lifecycle is a work item status, not conversation state |
| `flag` | `by design` | Issue lifecycle is a work item status, not conversation state |
| `trash` | `by design` | Issue lifecycle is a work item status, not conversation state |
| `raw_read` | `not built` | Raw read is built for mail only; the provider's whole copy of a message is not read |

## Permissions

- **Provider.** The credential reference is resolved on every provider call and
  webhook request. Purpose `linear.issues.api` accepts a secret (a personal API
  key, sent as is in `Authorization`, without a prefix) or an OAuth 2 token
  (sent as `Authorization: Bearer <token>`); a 401 re-resolves once with a fresh
  credential. Purpose `linear.issues.webhook` is the webhook signing secret. The
  token needs `read`; `comments:create` suffices to reply, and editing or
  deleting a comment needs `write`. Only the author, or an admin, may edit or
  delete a comment. Creating a webhook needs a workspace admin or the `admin`
  scope; that is done outside uma.
- **uma.** The receiving principal needs `channel.receive`; replies are
  authorized as `message.reply`, mutations as `message.edit` and
  `message.delete`.

## Configuration

`linear.issues.config` version 1, as JSON in the channel configuration.

| Field | Required | Meaning |
| --- | --- | --- |
| `api_url` | no | GraphQL endpoint (`https`, no user info, query or fragment). Default `https://api.linear.app/graphql` |
| `replies` | no | Enables `send` and the message mutations: replies become issue comments |
| `account_id` | with `replies` | UUID of the Linear user the credential acts as (the token owner, or the application's own user); only its comments are matched as uma's own |
| `local_fields` | no | Work item fields uma owns for this channel; empty keeps every field ingest-only |

Inboxes bind teams by key (`ENG`, the envelope destination) and use
per-work-item selection. The work item reference is namespace `linear.issues`,
kind `work_item`, ID the issue UUID (stable across moves between teams and
identifier changes); it is also the reply route. `Options` is empty. Calls go
through `uma.Config.HTTP`; without it they fail as unavailable. The GraphQL is
written by hand over the standard library.

## Receiving

The webhook handler verifies `Linear-Signature` (the hex HMAC-SHA256 of the raw
body under the webhook's signing secret) in constant time before decoding
anything, then checks the signed `webhookTimestamp` (UTC milliseconds): a
delivery more than a minute from the adapter's clock, either way, is refused
(401), as Linear recommends. The unsigned `Linear-Timestamp` header is never
read. A missing `webhookTimestamp` is malformed (400).

A delivery is identified by a SHA-256 of its raw body, never by
`Linear-Delivery`: the signature covers the body only, so a captured signed body
could be replayed under a new delivery ID and pass for a new event. The delivery
ID stays as the correlation. `Linear-Event` is unsigned too: it must name the
signed payload's `type` (400 otherwise). Events other than `Issue` and `Comment`,
and comments on anything but an issue, are acknowledged and dropped.

| Delivery | Result |
| --- | --- |
| `Issue` create | The description is the first message, with the issue state; an empty description reports the state only |
| `Issue` update | The state; with `description` in `updatedFrom`, an edit of the description message (or its deletion when emptied) |
| `Issue` remove | The binding deleted |
| `Comment` create, update, remove | A message, its edit and its deletion. An edit names its editor |

The state maps Linear's workflow state types `triage`, `backlog`, `unstarted`
and `started` to `open`, and `completed`, `canceled` and `duplicate` to
`closed`; the raw status is `<type>:<state name>`. Labels are the sorted field
`linear.issues.labels`. A comment payload carries the issue's title and link but
not its state, so a comment seen first binds the work item without a status.
Authors that are applications (a `botActor`, or an OAuth client or integration
actor) are `bot` senders and their comments automated messages.

## Sending, edits and reconciliation

A reply is one `commentCreate` with the issue UUID, the body and a comment `id`
the adapter derives from the operation and attempt: a version 4 UUID, because
`CommentCreateInput.id` accepts the new comment's ID. An attempt therefore names
its comment before the request is sent, and a lost answer is reconciled by
reading that one comment (`comment(id:)`): found is accepted, not found is
failed (`not_found`), so a retry is safe. Comments are public; non-text content
is `unsupported`.

The body also ends with a hidden marker (an HTML comment) naming the operation
and attempt. A webhook for a comment by `account_id` carrying a marker reports
an echo, so the engine settles the send even when the webhook arrives first, and
an edit by the account of a body ending in a marker reports the marked attempt.
An edit by anyone else names no attempt.

`Edit` runs `commentUpdate` with the text and the attempt's marker; `Delete`
runs `commentDelete` (a root comment's replies go with it). Targets are
`comment:<uuid>` messages; an issue description (`issue:<uuid>`) is rejected as
`unsupported`. A lost edit is reconciled by reading the comment (body ends with
the attempt's marker: applied; anything else: not applied, `not_found`; comment
gone: rejected `not_found`); a lost delete by reading it (gone or archived:
applied).

Provider answers map to outcomes (Linear reports GraphQL errors in an `errors`
array, and the adapter treats any entry as a failure of the operation):

| Answer | Outcome |
| --- | --- |
| Success | `provider_accepted` |
| Error `INVALID_INPUT` or HTTP 400, other than not found | rejected `validation` |
| Error text "not found" | rejected `not_found`; for a reply it names binding `inaccessible` |
| `FORBIDDEN` or HTTP 403 | rejected `unauthorized` |
| HTTP 401 or `AUTHENTICATION_ERROR` | error `unauthorized` after one fresh credential |
| HTTP 429 or error code `RATELIMITED` (in any status) | `rate_limited` with `Retry-After`, else the exhausted quota's reset, else one minute |
| HTTP 5xx or a lost answer after the request was written | `outcome_unknown`, reconciled before any retry |
| Failed dial or TLS handshake | transient |

## Limits

| What | Limit |
| --- | --- |
| Requests | 2,500 per hour per API key, 5,000 per OAuth user |
| Query complexity | 10,000 points per query; 3,000,000 per hour per API key, 2,000,000 per OAuth user |
| Webhook replay window | 60 seconds |
| Webhook response | 200 within 5 seconds; retried after 1 minute, 1 hour and 6 hours |
| Page size | 250 (not used: the adapter reads single objects) |
| Revision precision | milliseconds (`updatedAt`) |

## How it is tested

The shared adapter suite and package tests run against a stand-in GraphQL
endpoint and stand-in webhook deliveries written from the documentation and
Linear's published schema; they are not evidence about the real service.
`TestRecordedSandbox` replays committed cassettes recorded live (see
[Live verification](#live-verification-2026-11-17)); the same scenario also
runs against the stand-in (`TestSandboxScenarioAgainstStandIn`). To record
again, follow [Record Linear cassettes](../recording/linear.md).

## Known limits

- Replies are opt-in; without `replies` the channel only receives.
- Replies are top-level comments; a reply does not join a Linear thread
  (`parentId`).
- Only comments are edited and deleted; an issue description is never changed.
- The webhook deliveries are still stand-ins; see
  [Live verification](#live-verification-2026-11-17). Whether the Linear UI
  hides the echo marker is not checked.
- No history import, reactions, issue state changes or issue field updates.
- Comments on projects, documents and initiatives are dropped.
- Experimental: configuration may change ([Compatibility](../compatibility.md)).

## Live verification (2026-11-17)

Run against a sandbox workspace holding test data only, with a personal API
key limited to one team, on one issue. Every call went to the GraphQL
endpoint and answered HTTP 200; the run left the issue with the comments it
started with.

| Capability | Live result | Evidence |
| --- | --- | --- |
| `send` | `commentCreate` with a client-chosen comment `id` succeeds and the comment reads back under that `id`, body and hidden echo marker unchanged, authored by the key's user (`app` false) | recorded |
| `edit` | `commentUpdate` succeeds; the read-back shows the new body and `editedAt` set | recorded |
| `delete` | `commentDelete` succeeds; `comment(id:)` then answers "Entity not found: Comment" | recorded |
| edit after delete | `commentUpdate` on the deleted comment is refused with "Entity not found: Comment" | recorded |
| unknown comment | `comment(id:)` for a comment never created answers "Entity not found: Comment" | recorded |
| unknown issue | `commentCreate` on a missing issue UUID is refused with "Entity not found: Issue" | recorded |
| `refetch` | `issue(id:)` by UUID returns the issue with team key, state, labels and description | recorded |

Refusals are HTTP 200 with an `errors` array, `extensions.code`
`INPUT_ERROR`, `extensions.statusCode` 400 and the message above; the adapter
classifies them as `not_found` by that message. Rate limit headers carry
counters only.

Limits:

- Webhooks were not recorded (no public endpoint); the webhook tests keep
  stand-in deliveries.
- Not checked: whether the Linear UI hides the `<!-- uma:... -->` marker;
  application (`actor=app`) tokens; the status Linear uses for a rate-limited
  request.
- One recorded answer was edited by hand: the issue read-back, to replace the
  team and workflow state UUIDs and the upload URL IDs in the description with
  neutral values, and the team key, which the recorder had masked, with the
  placeholder.
