# jira.issues

`jira.issues` connects Jira Cloud issues: one channel per Jira Cloud site, one
work item per issue and one message per issue description or comment. An
issue's workflow status is a work item status that uma reads; replies, edits and
deletions of comments go back to Jira.

| | |
| --- | --- |
| Key | `jira.issues` |
| Package | `hop.top/uma/adapter/ticket/jira`, own module `hop.top/uma/adapter/ticket/jira` |
| Capabilities | `receive_http`, `send`, `edit`, `delete`, `reconcile`, `refetch` |
| Events | `message.received`, `message.updated`, `work_item.updated`, `delivery.updated` |
| Identity namespaces | `jira.issues` |
| Sender kinds | person, bot |
| Provider permissions | `read:jira-work`, `write:jira-work` |
| Message IDs and threading | assigned by Jira; per-work-item inboxes; no reactions |
| Reply parent | none: Jira comments name no replied-to comment |
| Interactions | none ([interactions](../interactions.md)) |
| Maturity | experimental |

The matrix of all adapters is in [Adapters](README.md).

## Capabilities and operations

| Capability | On Jira | Needs |
| --- | --- | --- |
| `receive_http` | Admin webhooks: `jira:issue_created`, `jira:issue_updated`, `jira:issue_deleted`, `comment_created`, `comment_updated`, `comment_deleted` | the webhook secret |
| `send` | `Messages.Reply` adds a comment (`POST /rest/api/3/issue/{id}/comment`) | `replies`, `account_id` |
| `edit` | `Messages.Edit` updates a comment (`PUT .../comment/{id}`) | `replies`, `account_id` |
| `delete` | `Messages.Delete` deletes a comment (`DELETE .../comment/{id}`) | `replies`, `account_id` |
| `reconcile` | Settles a lost answer by reading the issue's comments and their echo property | `replies` |
| `refetch` | Re-reads an issue or comment when two changes share one source revision | read access |

Until `replies` is enabled, `send`, `edit`, `delete` and `reconcile` report
unavailable with reason `disabled`.

## Not declared

Reason classes are defined in [Adapters](README.md#why-a-capability-is-not-declared).
Asking for one of these is `unsupported`.

| Capability | Reason | Why |
| --- | --- | --- |
| `reply_recipients` | `provider` | A comment has no recipient list; Jira notifies the issue's watchers |
| `receive` | `not built` | Issues and comments could be polled with JQL (`GET /rest/api/3/search/jql`, `updated >= ...`); webhooks are the ingress today |
| `start_conversation` | `engine` | A start on a per-work-item inbox opens an issue; core has no path for a start that creates a work item |
| `react` | `provider` | Jira comments have no reactions in the REST API |
| `receipt` | `provider` | Jira has no delivery or read receipts |
| `membership` | `by design` | Everyone who comments on an issue is a participant already; Jira keeps no list to change (watchers are not participants) |
| `history` | `not built` | Comments could be read with `GET .../issue/{id}/comment` (`startAt`, `maxResults`, `orderBy`) and the description with `GET .../issue/{id}`, as a webhook reports them |
| `archive` | `by design` | State goes through the workflow, which uma does not drive; issue lifecycle is not conversation state |
| `move` | `by design` | State goes through the workflow, which uma does not drive; issue lifecycle is not conversation state |
| `read_state` | `by design` | State goes through the workflow, which uma does not drive; issue lifecycle is not conversation state |
| `flag` | `by design` | State goes through the workflow, which uma does not drive; issue lifecycle is not conversation state |
| `trash` | `by design` | State goes through the workflow, which uma does not drive; issue lifecycle is not conversation state |
| `work_item_act` | `not built` | A status change is a workflow transition: `GET .../issue/{id}/transitions` lists the transition IDs of that issue's workflow and `POST` performs one, so a transition from a requested status needs a per-issue lookup and a mapping of workflow-specific names |
| `internal_note` | `not built` | A restricted comment (`visibility` of a role or group) needs a role or group name of the site, which the channel does not carry yet |
| `extension_execute` | `not built` | Assignee, labels and fields could be set with `PUT .../issue/{id}`; no effect is defined yet |
| `extension_query` | `not built` | Project, user and label lookups could be reads |
| `probe_permissions` | `not built` | The credential's permissions could be probed with `GET /rest/api/3/mypermissions` |
| `stream` | `provider` | Jira has no streamed comments |
| `send_ephemeral` | `provider` | A comment is visible to every reader of the issue |
| `signal` | `provider` | Jira has no typing or presence |
| `respond` | `provider` | Issues have no interactions |
| `respond_follow_up` | `provider` | As `respond` |
| `raw_read` | `not built` | Raw read is built for mail only; the provider's whole copy of a message is not read |

## Permissions

- **Provider.** The credential reference is resolved on every provider call and
  webhook request. Purpose `jira.issues.api` accepts:
  - a password credential (the Atlassian account email as user, an
    [API token](https://developer.atlassian.com/cloud/jira/platform/basic-auth-for-rest-apis/)
    as secret): basic authentication, calls go to the site URL;
  - a secret (an API token) with the channel's `email`: the same;
  - an OAuth 2 token (an
    [OAuth 2.0 (3LO)](https://developer.atlassian.com/cloud/jira/platform/oauth-2-3lo-apps/)
    access token the host mints and refreshes), sent as a bearer token with the
    channel's `cloud_id`: calls go through the API gateway,
    `https://api.atlassian.com/ex/jira/{cloudid}`.

  A 401 re-resolves once with a fresh credential. Purpose
  `jira.issues.webhook` is the admin webhook's secret. The account needs the
  Browse projects permission, Add comments to reply, Edit all comments (or Edit
  own comments) to edit and Delete all comments (or Delete own comments) to
  delete; an OAuth 2 token needs the `read:jira-work` and `write:jira-work`
  scopes.
- **uma.** The receiving principal needs `channel.receive`; replies are
  authorized as `message.reply` and mutations as `message.edit` and
  `message.delete`.

## Configuration

`jira.issues.config` version 1, as JSON in the channel configuration.

| Field | Required | Meaning |
| --- | --- | --- |
| `site_url` | yes | The Jira Cloud site, `https://<site>.atlassian.net` (`https`, no path, user info, query or fragment). API-token calls go to it; issue links are `<site_url>/browse/<key>` |
| `cloud_id` | with an OAuth 2 credential | The site's cloud ID; routes calls through the API gateway |
| `email` | with a bare API-token secret | The account email the token belongs to; unused with a password credential |
| `replies` | no | Enables `send`, `edit`, `delete` and `reconcile`: replies become issue comments |
| `account_id` | with `replies` | The Atlassian `accountId` the credential acts as; only its comments are matched as uma's own |
| `local_fields` | no | Work item fields uma owns for this channel; empty keeps every field ingest-only |

Inboxes bind projects by key (`SBX`) and use per-work-item selection. The work
item reference is the numeric issue ID with no project part: an issue keeps its
ID when its key changes or it moves, and the reference is the reply route.
`Options` are `WithBaseURL` (a proxy or test server) and `WithHTTPClient`
(used when the engine injects none). Calls go through `uma.Config.HTTP`;
without any client they fail as unavailable.

## Receiving

Webhooks come from an **admin webhook**: registered by a Jira administrator in
System, WebHooks, or with `POST /rest/webhooks/1.0/webhook`, over HTTPS, with a
secret, for the issue and comment events above. That is the mechanism an
API-token integration can use; the others need an app. Connect apps sign with a
JWT keyed by the app's shared secret, OAuth 2.0 (3LO) apps register dynamic
webhooks and receive a bearer-token JWT, and Forge apps use triggers. The
handler's URL is the one the host mounts through `Runtime.Webhook`.

The handler verifies `X-Hub-Signature` (`method=signature`, the hex HMAC of the
raw body) in constant time before decoding anything. Only `sha256` is accepted;
the documentation warns Jira might start using another method, and an unknown
one is refused, not guessed. Jira retries a delivery up to five times, on 408,
409, 425, 429 and 5xx, so a 401, 400 or 422 is final and a 503 is retried.

A delivery is identified by a SHA-256 of its raw body, never by
`X-Atlassian-Webhook-Identifier`: the signature covers the body only, so a
captured signed body could be replayed under a new identifier and pass for a new
event. A retry resends the body, so it is still one event; two different bodies
are two events under any header. The identifier stays as the correlation. The
event is named by the signed body's `webhookEvent`: an issue event carrying a
`comment`, or a comment event without one, is refused as malformed (400), and
so is a comment event without an `issue`.

| Delivery | Result |
| --- | --- |
| `jira:issue_created` | The description is the first message (`issue:<id>`), with the issue state; an empty description reports the state only |
| `jira:issue_updated` | The state, and, when the changelog names the description, an edit of the description message |
| `jira:issue_deleted` | The binding deleted |
| `comment_created`, `comment_updated`, `comment_deleted` | A message (`comment:<id>`), its edit and its deletion. An edit names its editor (`updateAuthor`) |
| Any other event | Acknowledged and dropped |

App accounts (`accountType` `app`) are `bot` senders and author automated
messages; Atlassian and customer accounts are people. A comment restricted to a
group or role, or a Service Management internal note (`jsdPublic` false), has an
internal audience. Bodies are Atlassian Document Format documents; a plain
string is read as it is, mentions, emoji and cards keep their visible text, and
media is dropped.

The state maps the status category: `new` (To Do) and `indeterminate` (In
Progress) are `open`, `done` is `closed`; the raw status is the status name
(`In Progress`). A moved issue keeps its work item and reports its new project
key as destination. Labels are the field `jira.issues.labels`.

## Sending, edits and deletes

A reply is an ADF document: blank-line-separated blocks become paragraphs and
newlines hard breaks. Only public text is sent, at most 32767 characters (Jira
Cloud's limit for comment text). The request carries a comment property
`uma.echo` naming the operation and attempt, set in the same request that
creates or edits the comment, so there is no visible marker in the text. A sent
comment's ID is its provider message reference. `Reconcile` reads the issue's
comments newest first (`orderBy=-created`), within 7 days, and the property of
those written by `account_id`; a complete listing without the attempt is failed
with `not_found`, so a retry is safe. A webhook for a comment by `account_id`
that carries the property reports an echo. Whether Jira's webhook payload carries
comment properties is not documented; without them the message ID settles the
send after the answer.

Edits and deletes work on `comment:<id>` targets. An issue description cannot be
edited or deleted: `unsupported`. The adapter checks no authorship: any comment
the credential may change is changed.

Provider answers map to outcomes: 201 and 200 are `provider_accepted`; 400 and
413 are rejected (a 400 is also Jira's answer to a user who may not edit or
delete a comment; 413 is the per-issue comment limit); 401 and 403 are
`unauthorized` (a rejected outcome for an edit or delete); 404 is rejected
`not_found`, with binding `inaccessible` for a send (Jira answers 404 for a
missing issue and for one the credential may not browse); 429 is `rate_limited`
with the stated wait (`Retry-After`, else `X-RateLimit-Reset`, else one minute).
Comment creation has no idempotency key, so a 5xx or lost answer after the
request was written is `outcome_unknown`.

## Limits

| What | Limit |
| --- | --- |
| Comment text | 32767 characters (counted as runes) |
| Reconcile look-back | 7 days; 20 pages of 100 comments; 50 property reads, past which the result is inconclusive |
| Webhook retries | 5, by Jira |
| Rate limits | Per tenant and endpoint per second, per-issue writes (20 per 2 seconds, 100 per 30 seconds) and hourly quotas; a 429 carries `Retry-After` |
| Revision precision | one millisecond (`updated`); `refetch` resolves ties |

## How it is tested

The shared adapter suite and package tests run against a stand-in Jira, written
to Atlassian's documentation, with signed webhook payloads in the documented
shapes and the signature vector Atlassian publishes. `TestRecordedComments`
replays committed cassettes recorded against a sandbox site (see
[Live verification](#live-verification-2026-11-17)); `TestRecordedWebhooks`
skips until webhook deliveries are captured, so webhook payload shapes (the
`issue` object and `properties` on comment events) remain the stand-in's
assumptions. To record, follow [Record Jira cassettes](../recording/jira.md).

## Known limits

- Replies are opt-in; without `replies` the channel only receives.
- No status changes: Jira workflow transitions are not declared (`work_item_act`),
  so an issue's status is read, never written.
- No history import, no reactions, no internal notes.
- The description is received and edit-observed but cannot be edited or deleted
  by uma.
- Jira Cloud only: Jira Server and Data Center use another REST version, another
  user identity (no `accountId`) and another webhook signature.
- Experimental: configuration may change ([Compatibility](../compatibility.md)).

## Live verification (2026-11-17)

Recorded against a sandbox Jira Cloud site with a classic API token (basic
authentication), one issue, no other activity. Statuses as Jira answered, in
the order `TestRecordedComments` makes them:

- Reply: `POST /rest/api/3/issue/{id}/comment` with an ADF body and the
  `uma.echo` property (201); reconcile by `GET .../comment?orderBy=-created&maxResults=100&startAt=0`
  (200) and `GET /rest/api/3/comment/{id}/properties/uma.echo` (200).
- Edit: `PUT .../comment/{id}` with the new body and property (200); reconcile
  by the same list and property reads (200); `GET .../comment/{id}` for refetch
  (200); `GET /rest/api/3/issue/{id}?fields=...` for the issue refetch (200).
- Delete: `DELETE .../comment/{id}` (204); reconcile `GET` (404); second
  `DELETE` (404).
- Premises settled: `POST` and `PUT` honour the `properties` field and the
  property is readable at once; an edit replaces the property's value;
  `orderBy=-created` lists newest first; timestamps carry a numeric offset
  (`-0500` form, milliseconds); a gone comment answers `404` on `GET` and
  `DELETE`.
- Recording findings: xrr redacts JSON fields named `key` unless
  `XRR_REDACT_ALLOW=key` is set (the test sets it); the account ID appears URL-encoded in user
  `self` URLs; the site's template names its project (the recording rewrites
  it); a comment ID equal to the issue ID breaks the recording through the
  issue ID placeholder (see [Record Jira cassettes](../recording/jira.md)).

Limits: webhook deliveries were not captured (they need a public endpoint), so
`TestRecordedWebhooks` still skips and webhook parsing rests on the stand-in.
Rate limiting, server errors, retries and OAuth 2.0 (3LO) are not recordable
here. The sandbox issue had no description, so the description is not
exercised by the cassettes.
