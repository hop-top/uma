# gitlab.issues

`gitlab.issues` connects GitLab issues: one channel per GitLab account (a
personal, project or group access token, or an OAuth token), one work item per
issue and one message per issue description or note. An issue's state is a work
item status, and uma can close and reopen it. It is built after
[`github.issues`](github.issues.md) and declares what GitLab's REST API and
webhooks support of that capability set; the rows below name the rest.

| | |
| --- | --- |
| Key | `gitlab.issues` |
| Package | `hop.top/uma/adapter/ticket/gitlab`, own module `hop.top/uma/adapter/ticket/gitlab` |
| Capabilities | `receive_http`, `send`, `reconcile`, `refetch`, `work_item_act`, `edit`, `delete`, `history` |
| Events | `message.received`, `message.updated`, `work_item.updated`, `delivery.updated` |
| Identity namespaces | `gitlab.issues` |
| Sender kinds | person, bot |
| Provider permissions | `api` (a token scope; `read_api` is enough without replies, acts and edits) |
| Message IDs and threading | assigned by GitLab; per-work-item inboxes |
| Reply parent | none: issue notes name no replied-to note |
| Interactions | none ([interactions](../interactions.md)) |
| Maturity | experimental |

The matrix of all adapters is in [Adapters](README.md).

## Capabilities and operations

| Capability | On GitLab | Needs |
| --- | --- | --- |
| `receive_http` | `Issue Hook` and `Note Hook` webhooks, confidential ones included | the webhook secret token |
| `send` | `Messages.Reply` posts an issue note (`POST /projects/:id/issues/:iid/notes`) | `replies`, `account_id` |
| `edit` | `Messages.Edit` replaces a note (`PUT .../notes/:id`) or an issue description (`PUT /projects/:id/issues/:iid`) | `replies`, `account_id` |
| `delete` | `Messages.Delete` deletes a note (`DELETE .../notes/:id`) | `replies`, `account_id` |
| `reconcile` | Settles a lost answer by reading what GitLab holds | `replies` for messages; nothing for work item acts |
| `refetch` | Re-reads an issue or note when two changes share GitLab's `updated_at` | read access |
| `work_item_act` | `WorkItems.Act` closes or reopens an issue (`state_event`) | write access to issues |
| `history` | `Runtime.Import` reads one issue's past description and notes | read access to issues |

Until `replies` is enabled, `send`, `edit` and `delete` report unavailable with
reason `disabled`.

## Not declared

Reason classes are defined in [Adapters](README.md#why-a-capability-is-not-declared).
Asking for one of these is `unsupported`.

| Capability | Reason | Why |
| --- | --- | --- |
| `reply_recipients` | `provider` | A note has no recipient list; GitLab notifies the issue's subscribers |
| `receive` | `not built` | Issues and notes could be polled with `updated_after`; webhooks are the ingress today |
| `start_conversation` | `engine` | A start on a per-work-item inbox opens an issue; core has no path for a start that creates a work item |
| `receipt` | `provider` | GitLab has no delivery or read receipts |
| `membership` | `by design` | Everyone who comments on an issue is a participant already; GitLab keeps no list to change |
| `react` | `not built` | Award emoji (`POST .../issues/:iid/notes/:id/award_emoji`) could carry reactions; GitLab sends an emoji webhook the adapter does not read yet |
| `internal_note` | `not built` | GitLab has internal notes (`internal=true` on note creation); inbound ones already carry an internal audience, but no path sends one |
| `extension_execute` | `not built` | `PUT /projects/:id/issues/:iid` with `labels`, `assignee_ids` and `milestone_id` could be an update effect, as on GitHub |
| `extension_query` | `not built` | Label, milestone and member listings could be reads |
| `probe_permissions` | `not built` | The token's scopes could be probed |
| `stream` | `provider` | GitLab has no streamed notes |
| `send_ephemeral` | `provider` | Every note is visible to every reader of the issue, or to all members when internal |
| `signal` | `provider` | GitLab has no typing or presence |
| `respond` | `provider` | Issues have no interactions |
| `respond_follow_up` | `provider` | As `respond` |
| `archive` | `by design` | State goes through `work_item_act`; issue lifecycle is not conversation state |
| `move` | `by design` | State goes through `work_item_act`; issue lifecycle is not conversation state |
| `read_state` | `by design` | State goes through `work_item_act`; issue lifecycle is not conversation state |
| `flag` | `by design` | State goes through `work_item_act`; issue lifecycle is not conversation state |
| `trash` | `by design` | State goes through `work_item_act`; issue lifecycle is not conversation state |
| `raw_read` | `not built` | Raw read is built for mail only; the provider's whole copy of a message is not read |

## Permissions

- **Provider.** The credential reference is resolved on every provider call and
  webhook request. Purpose `gitlab.issues.api` accepts an OAuth 2 token or a
  secret (a personal, project or group access token), sent as
  `Authorization: Bearer`; a 401 re-resolves once with a fresh credential.
  Purpose `gitlab.issues.webhook` is the webhook's secret token. The token needs
  the `api` scope to reply, edit, delete, close and reopen, and `read_api` to
  read, refetch and import. A project access token's bot user is the account:
  its numeric user ID is `account_id`.
- **uma.** The receiving principal needs `channel.receive`; replies are
  authorized as `message.reply`, mutations as `message.edit` and
  `message.delete`, and closing as `work_item.act`.

## Configuration

`gitlab.issues.config` version 1, as JSON in the channel configuration.

| Field | Required | Meaning |
| --- | --- | --- |
| `api_url` | no | REST root, for a self-managed instance (`https`, no user info, query or fragment). Default `https://gitlab.com/api/v4/` |
| `replies` | no | Enables `send` and the message mutations: replies become issue notes |
| `account_id` | with `replies` | Numeric GitLab user ID the credential acts as; only its notes are matched as uma's own |
| `local_fields` | no | Work item fields uma owns for this channel; empty keeps every field ingest-only |

Inboxes bind projects by their full path (`group/subgroup/project`, the
envelope destination) and use per-work-item selection. The work item reference
names the issue by its project-scoped number (`iid`) under the project's
**numeric ID**, not its path: a renamed or transferred project keeps working,
where GitLab answers non-GET requests to an old path with 405. Senders are
GitLab user IDs. `Options` is empty. Calls go through `uma.Config.HTTP`; without
it they fail as unavailable. The adapter has no GitLab SDK: its module depends
on the standard library and uma only.

## Receiving

The webhook handler compares `X-Gitlab-Token` with the webhook secret in
constant time (both sides hashed first, so lengths do not leak) before decoding
anything. A missing, empty or wrong token is 401. GitLab sends the token as plain
text, which GitLab's own documentation calls weaker than a signing token; the
adapter does not read GitLab's newer HMAC `webhook-signature` yet. Serve the
webhook over https only.

A delivery is identified by a SHA-256 of its raw body. GitLab's retry resends the
body, so it is one event; two different bodies are two events under any header.
`Idempotency-Key` (or `Webhook-Id`) stays as the correlation. `X-Gitlab-Event`
must agree with the payload's `object_kind` (400 otherwise). `Push Hook`, merge
request, pipeline, emoji and other events are acknowledged and dropped, as are
`work_item` events of other work item types.

| Delivery | Result |
| --- | --- |
| `Issue Hook` `open` | The description is the first message, with the issue state |
| `Issue Hook` `update` with a `description` change | The state, and an edit of the description message (a cleared description deletes it) |
| `Issue Hook` `close`, `reopen`, other `update` | The state |
| `Note Hook` on an issue, `create` | A message, with the issue state |
| `Note Hook` on an issue, `update` | An edit of the message. It names its editor, the delivery's user, not the note's author |

System notes ("closed", "changed the label") are activity the issue event
already reports and are dropped; notes on merge requests, commits and snippets
are dropped. An internal note arrives with an `internal` audience. A note by
`account_id` carrying uma's marker is the echo of the attempt that sent it.
GitLab documents no deletion webhook for notes: a deleted note is learned only
by `reconcile` (for uma's own deletes) or a later import. Bot users (the
generated `project_<id>_bot_*` and `group_<id>_bot_*` usernames of access tokens)
are `bot` senders; everyone else is a person.

The state maps `opened` to `open` and `closed` to `closed`; the raw status is
GitLab's. Labels are the field `gitlab.issues.labels` (sorted names) and the
assignees come from `assignee_ids`. GitLab's webhooks name only a milestone's ID,
so the milestone is not a field. Timestamps are read in GitLab's ISO 8601 form
and its older `2013-12-03 17:15:43 UTC` form.

## Work item acts

| Requested | GitLab |
| --- | --- |
| `closed` | `PUT .../issues/:iid` with `state_event: close` |
| `open` | `PUT .../issues/:iid` with `state_event: reopen` |
| any reason, `pending`, `resolved` | `unsupported`, never coerced: GitLab has no close reason |

The act is one PUT that never reads the issue first. A lost answer is reconciled
by reading the issue: it holds the requested state (`closed`, `opened`) →
delivered; otherwise failed with `not_found`, and a new attempt is safe. The
issue webhook GitLab sends for the change is an ordinary state observation and
never settles the act.

## Sending, edits and deletes

A sent note's body ends with a hidden marker (an HTML comment) naming the
operation and attempt. `Reconcile` reads the issue's notes newest first, without
system notes, looking for that marker in a note by `account_id` created within 7
days; a complete listing without it is failed with `not_found`, so a retry is
safe, and a listing cut short is inconclusive.

GitLab runs any note line that starts with `/` and a command (`/close`,
`/assign @user`, `/label ~x`) as a quick action and removes it from the note. A
reply is text, never commands, so the adapter writes a backslash before the
slash of such a line (`\/close`): the note is stored as sent and shown without
the backslash. GitLab also strips `\r` and trailing whitespace, which the marker
survives.

Edits and deletes work on `note:<id>` and `issue:<id>` message targets; an issue
description cannot be deleted. The adapter checks no authorship: any note the
token may change is changed, and GitLab's 403 is a rejected `unauthorized`
outcome. Only public text is sent: another audience, or non-text content, is
`unsupported`.

Provider answers map to outcomes: 201 and 200 are `provider_accepted` (or
`delivered`); 400 and 422 are rejected; 401 is `unauthorized` after one fresh
credential; 403 on a write is a rejected `unauthorized`; 404 is a rejected
`not_found` naming binding `inaccessible` (GitLab answers 404 for an issue the
token cannot see), and for a note edit or delete a rejected `not_found` without
a binding; 405 (a moved project) and redirects are not followed and read as
`not_found`; 429 is `rate_limited` with `Retry-After` seconds, else the
`RateLimit-Reset` epoch, else one minute. Note creation has no idempotency key,
so a 5xx or lost answer after the request was written is `outcome_unknown`,
reconciled before any retry.

## History

`Runtime.Import` reads a window of past messages through `history`, oldest
first, and imports each once: history messages carry the same provider message
IDs (`issue:<id>`, `note:<id>`), destinations (the project path) and work item
references as webhook messages, so a message uma already holds, however it
arrived, is skipped. A note by `account_id` carrying uma's marker is the echo of
its send, never a new message.

| Import of | GitLab reads |
| --- | --- |
| A conversation | The issue (`GET /projects/:id/issues/:iid`, for its description and the project path), then its notes (`GET .../notes?sort=asc&order_by=created_at`) |
| A channel | `unsupported`: GitLab has no feed of notes across issues |

- **Skipped.** System notes, as the webhook drops them; work item state (the
  issue's state now says nothing about the time a past message was written).
  Internal notes are imported with an `internal` audience.
- **Paging.** Pages follow the `X-Next-Page` header, at most 100 notes, sized by
  the first page's limit. The cursor holds the page and the ID of the last note
  consumed, not an ordering of IDs (imported notes keep their original time), so
  a note deleted while the window is read neither repeats nor drops a message;
  when deletions move the last note up to three pages back it is found there, and
  when that note is itself gone the read repeats from the start of its page and
  reports a gap. A cursor the adapter did not issue for that issue and window is
  `validation`, before any call.
- **Failures.** As for the other reads: a rate limit is `rate_limited` with
  GitLab's wait, a missing issue `not_found`.

## Limits

| What | Limit |
| --- | --- |
| Note body | 1,000,000 characters (runes) |
| Issue description | 1,048,576 characters (runes) |
| Reconcile look-back | 7 days |
| Revision precision | the webhook's `updated_at` (one second in the older form); `refetch` resolves ties |
| History page | 100 notes per GitLab page |

## How it is tested

The shared adapter suite and package tests run against a stand-in GitLab API
and stand-in webhook payloads, written to GitLab's documentation
([notes](https://docs.gitlab.com/api/notes/),
[issues](https://docs.gitlab.com/api/issues/),
[webhook events](https://docs.gitlab.com/user/project/integrations/webhook_events/)).
They are not evidence about the real service. The suite's channel-wide history
cases and its deletion and reaction webhook cases are not expressible on GitLab
and skip; one issue's history is covered by the package tests.

Cassettes are committed for `TestRecordedExchanges`, `TestRecordedTransitions`
and `TestRecordedHistory`, recorded against a sandbox project on gitlab.com
(see below). `TestRecordedWebhookPayloads` skips until someone captures
webhook deliveries. To record again, follow
[Record GitLab cassettes](../recording/gitlab.md). The adapter stays
`experimental` until the webhook payloads are captured too.

## Live verification (2026-11-17)

Recorded against a sandbox project with a project access token (Developer,
`api`), one issue, no other activity. Statuses as GitLab answered:

- Notes (`TestRecordedExchanges`): two `POST .../notes` (201); list recent
  notes (200); `GET` of both notes (200) and of the issue (200); `PUT` of the
  first note (200); find by marker (200); `DELETE` of both notes (204); `GET`
  of a deleted note (404); second `DELETE` of a deleted note (404).
- Transitions (`TestRecordedTransitions`): `PUT` close (200), close of a closed
  issue (200), `PUT` reopen (200), three reconciling `GET` of the issue (200),
  and a close of a nonexistent issue (404).
- History (`TestRecordedHistory`): three `POST .../notes` (201), `GET` of the
  first note (200), `GET` of the issue (200), notes read two per page over two
  pages (200, `X-Next-Page` set on the first page and empty on the last), three
  `DELETE` (204).
- Premises settled: the reply holding a `/close` line, escaped as `\/close`,
  left the issue open and was stored as written; closing a closed issue
  answers 200; the marker comment survives in the stored note; pagination
  headers behave as the adapter expects.

Limits: webhook deliveries were not captured (they need a public endpoint), so
`TestRecordedWebhookPayloads` still skips and webhook parsing rests on the
documentation's payloads. The sandbox ran on gitlab.com, so a self-managed
instance's version differences are unchecked.

## Known limits

- Replies are opt-in; without `replies` the channel only receives, acts and
  imports.
- Not built: reactions, internal notes, an issue update effect (labels,
  assignees, milestone) and the HMAC webhook signature. Each is a provider
  capability listed above.
- Notes deleted at GitLab are not reported by webhook.
- Channel-wide history is unsupported.
- Closing with a reason, `pending` and `resolved` are not supported.
- Quick actions in a person's own notes are GitLab's; only text uma sends is
  escaped.
- Issue state and conversation status are independent lifecycles: closing an
  issue never changes the conversation's status, and a conversation transition
  never reaches GitLab.
- Experimental: configuration may change ([Compatibility](../compatibility.md)).
