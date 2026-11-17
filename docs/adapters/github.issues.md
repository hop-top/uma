# github.issues

`github.issues` connects GitHub issues: one channel per GitHub account (a GitHub
App installation or a personal access token), one work item per issue and one
message per issue description or comment. It is the adapter for work items: an
issue's state is a work item status, uma can close and reopen it, and an
extension effect sets its assignees, labels and milestone.

| | |
| --- | --- |
| Key | `github.issues` |
| Package | `hop.top/uma/adapter/ticket/github`, own module `hop.top/uma/adapter/ticket/github` |
| Capabilities | `receive_http`, `send`, `reconcile`, `refetch`, `work_item_act`, `edit`, `delete`, `react`, `history`, `extension_execute` |
| Events | `message.received`, `message.updated`, `work_item.updated`, `delivery.updated` |
| Identity namespaces | `github.issues` |
| Sender kinds | person, bot |
| Provider permissions | `issues:read`, `issues:write`, `metadata:read` |
| Message IDs and threading | assigned by GitHub; per-work-item inboxes; several reactions per user |
| Reply parent | none: issue comments name no replied-to comment |
| Interactions | none ([interactions](../interactions.md)) |
| Maturity | experimental |

The matrix of all adapters is in [Adapters](README.md).

## Capabilities and operations

| Capability | On GitHub | Needs |
| --- | --- | --- |
| `receive_http` | `issues` and `issue_comment` webhooks | the webhook secret |
| `send` | `Messages.Reply` posts an issue comment | `replies`, `account_id` |
| `edit` | `Messages.Edit` patches a comment or an issue description | `replies`, `account_id` |
| `delete` | `Messages.Delete` deletes a comment | `replies`, `account_id` |
| `react` | `Messages.React` adds or removes a reaction on a comment or description | `replies`, `account_id` |
| `reconcile` | Settles a lost answer by reading what GitHub holds | `replies` for messages; nothing for work item acts and update effects |
| `refetch` | Re-reads an issue or comment when two changes share GitHub's one-second `updated_at` | read access |
| `work_item_act` | `WorkItems.Act` closes or reopens an issue | write access to issues |
| `extension_execute` | `Extensions.Execute` runs `github.issues.update`: assignees, labels and milestone | write access to issues |
| `history` | `Runtime.Import` reads an issue's past description and comments, or channel-wide those of listed repositories | read access to issues; `history_repositories` for a channel-wide import |

Until `replies` is enabled, `send`, `edit`, `delete` and `react` report
unavailable with reason `disabled`.

## Not declared

Reason classes are defined in [Adapters](README.md#why-a-capability-is-not-declared).
Asking for one of these is `unsupported`.

| Capability | Reason | Why |
| --- | --- | --- |
| `reply_recipients` | `provider` | An issue comment has no recipient list; GitHub notifies the issue's subscribers |
| `receive` | `not built` | Issues and comments could be polled with `since`; webhooks are the ingress today |
| `start_conversation` | `engine` | A start on a per-work-item inbox opens an issue; core has no path for a start that creates a work item |
| `receipt` | `provider` | GitHub has no delivery or read receipts |
| `membership` | `by design` | Everyone who comments or reacts on an issue is a participant already; GitHub keeps no list to change |
| `internal_note` | `provider` | GitHub has no private comments |
| `extension_query` | `not built` | Label, milestone and assignee listings could be reads |
| `probe_permissions` | `not built` | Token scopes (`X-OAuth-Scopes`) and installation permissions could be probed |
| `stream` | `provider` | GitHub has no streamed comments |
| `send_ephemeral` | `provider` | Every comment is visible to every reader |
| `signal` | `provider` | GitHub has no typing or presence |
| `respond` | `provider` | Issues have no interactions |
| `respond_follow_up` | `provider` | As `respond` |
| `archive` | `by design` | State goes through `work_item_act` and the update extensions; issue lifecycle is not conversation state |
| `move` | `by design` | State goes through `work_item_act` and the update extensions; issue lifecycle is not conversation state |
| `read_state` | `by design` | State goes through `work_item_act` and the update extensions; issue lifecycle is not conversation state |
| `flag` | `by design` | State goes through `work_item_act` and the update extensions; issue lifecycle is not conversation state |
| `trash` | `by design` | State goes through `work_item_act` and the update extensions; issue lifecycle is not conversation state |
| `raw_read` | `not built` | Raw read is built for mail only; the provider's whole copy of a message is not read |

## Extension effects

One effect, version 1, run through `Extensions.Execute`. The factory refuses
invalid fields at the call (`adapter.ActionValidator`), with nothing stored;
`Execute` applies the same rules again at the attempt.

| Effect | Authorized as | Rules |
| --- | --- | --- |
| `github.issues.update` | `work_item.act` | Sets any of `assignees`, `labels`, `milestone`; needs at least one; may not set `title`, `body`, `state`, `state_reason`, `duplicate_issue_id` or `assignee` |

The data is a JSON object. Each field replaces the issue's whole set with one
`PATCH /repos/{owner}/{repo}/issues/{number}`, as GitHub applies it; a field
left out is untouched.

| Field | Value | Clears with |
| --- | --- | --- |
| `assignees` | Array of distinct GitHub logins (letters, digits, `-`, `_`; at most 39 characters) | `[]` |
| `labels` | Array of distinct, non-empty label names; GitHub creates a name the repository lacks | `[]` |
| `milestone` | A milestone number (positive integer) | `null` |

Fields the adapter does not know pass through to GitHub unchanged. The issue
state is never a field of the effect: closing and reopening go through
`WorkItems.Act`, and the title and body through `Messages.Edit` of the
description. A field uma owns for the channel (`local_fields`) is refused at
the attempt with `validation`, before any call: `assignees` when the channel
lists `assignees`, `labels` and `milestone` when it lists `fields`.

The answer maps as an issue update's: 200 is `provider_accepted`; 400 and 422
are rejected; a 403 without rate limit headers is rejected `unauthorized`; 404
and 410 are rejected `not_found` and name the work item binding they show
(`inaccessible`, `deleted`); 429 and a rate-limited 403 are `rate_limited` with
the stated wait; a 5xx or a lost answer after the request was written is
`outcome_unknown`, reconciled before any retry. A lost answer is reconciled by
reading the issue and comparing the requested fields: every one held is
applied, anything else is not, and a retry is safe because each PATCH replaces
whole sets. The comparison cannot tell who set the fields: values a person set
to the same meanwhile read as applied, and a later change by someone else reads
as not applied, so the retry puts the requested values back. The webhook GitHub
sends for the change (`labeled`, `assigned`, `milestoned` and their reverses)
is an ordinary state observation and never settles the effect. GitHub drops
assignee, label and milestone changes silently for a token without push access:
the PATCH answers 200 and the reconciliation then reads the fields as not
applied.

## Permissions

- **Provider.** The credential reference is resolved on every provider call and
  webhook request. Purpose `github.issues.api` accepts an OAuth 2 token (a
  GitHub App installation token, which the host mints and refreshes) or a
  secret (a classic or fine-grained personal access token), sent as a bearer
  token; a 401 re-resolves once with a fresh credential. Purpose
  `github.issues.webhook` is the webhook secret. The token needs read access to
  issues and metadata, and write access to issues to reply, to close and
  reopen, and to update assignees, labels and the milestone (push access to
  the repository, or GitHub drops those changes silently).
- **uma.** The receiving principal needs `channel.receive`; replies are
  authorized as `message.reply`, mutations as `message.edit`, `message.delete`
  and `message.react`, and closing and the update effect as `work_item.act`.

## Configuration

`github.issues.config` version 1, as JSON in the channel configuration.

| Field | Required | Meaning |
| --- | --- | --- |
| `api_url` | no | REST root, for GitHub Enterprise Server (`https`, no user info, query or fragment). Default `https://api.github.com/` |
| `replies` | no | Enables `send` and the message mutations: replies become issue comments |
| `account_id` | with `replies` | Numeric GitHub user ID the credential acts as; only its comments are matched as uma's own |
| `local_fields` | no | Work item fields uma owns for this channel; empty keeps every field ingest-only. The update effect refuses a field uma owns |
| `history_repositories` | for channel-wide history | Repositories (`owner/repo`, at most 10) a channel-wide import reads; an issue's import needs none |

Inboxes bind repositories by full name (`owner/repo`) and use per-work-item
selection. `Options` is empty. Calls go through `uma.Config.HTTP`; without it
they fail as unavailable.

## Receiving

The webhook handler verifies `X-Hub-Signature-256` (HMAC-SHA256 of the raw body)
in constant time before decoding anything. The legacy SHA-1 signature is never
accepted. Comments on pull requests and other events are acknowledged and
dropped.

A delivery is identified by a SHA-256 of its raw body, never by
`X-GitHub-Delivery`: the signature covers the body only, so a captured signed
body could be replayed under a new GUID and pass for a new event. GitHub's
redelivery resends the body under the same GUID, so it is still one event; two
different bodies are two events under any header. The GUID stays as the
correlation. `X-GitHub-Event` is unsigned too: an `issues` delivery carrying a
`comment`, or an `issue_comment` one without, is refused as malformed (400).
Byte-identical bodies are one event, and one event sent through two webhooks is
one only when the bodies are byte-identical. A delivery received before an
upgrade and redelivered after it is applied once more, as an ordinary delivery.

| Delivery | Result |
| --- | --- |
| `issues.opened` | The description is the first message, with the issue state |
| `issues.edited` | The state, and an edit of the description message |
| `issues.deleted`, `issues.transferred` | The binding deleted, or moved with the new issue as successor |
| Other `issues` actions | The state |
| `issue_comment` created, edited, deleted | A message, its edit and its deletion. An edit names its editor |

GitHub sends no reaction webhooks, so reactions never arrive as events; a comment
payload's counts become a count-only snapshot. Bot accounts are `bot` senders;
users and organizations are people.

The state maps `open` and `closed` to the work item statuses, and the raw status
keeps GitHub's `state_reason` (`open:reopened`, `closed:completed`,
`closed:not_planned`, `closed:duplicate`). Labels and the milestone are fields.

## Work item acts

| Requested | GitHub |
| --- | --- |
| `closed` (reason `completed`, the default, or `not_planned`) | `state: closed` with that `state_reason` |
| `open` (reason `reopened`, the default) | `state: open`, `state_reason: reopened` |
| `closed` with `duplicate`, `pending`, `resolved` | `unsupported`, never coerced |

The act is one PATCH that never reads the issue first. A lost answer is
reconciled by reading the issue. The webhook GitHub sends for the change is an
ordinary state observation and never settles the act.

## Sending, edits and reactions

A sent comment's body ends with a hidden marker (an HTML comment) naming the
operation and attempt; a webhook for a comment by `account_id` carrying a marker
reports an echo, so the engine settles the send even when the webhook arrives
first. `Reconcile` looks for the marker among that account's comments updated
within 7 days. Edits and reactions work on `comment:<id>` and `issue:<id>` message
targets; an issue description cannot be deleted. The adapter checks no
authorship: any comment the token may change is changed.

Emoji map byte for byte to GitHub's contents: 👍 `+1`, 👎 `-1`, 😄 `laugh`,
😕 `confused`, ❤️ `heart`, 🎉 `hooray`, 🚀 `rocket`, 👀 `eyes`. Any other emoji is
`unsupported` before any call.

Provider answers map to outcomes: 201 is `provider_accepted`; 400 and 422 are
rejected; 401 and a 403 without rate limit headers are `unauthorized`; 404 and
410 are `not_found`; 429 and rate-limited 403 are `rate_limited` with the stated
wait. Comment creation has no idempotency key, so a 5xx or lost answer after the
request was written is `outcome_unknown`.

## History

`Runtime.Import` reads a window of past messages through `history`, oldest
first, and imports each once: history messages carry the same provider
message IDs (`issue:<id>`, `comment:<id>`), destinations (the repository) and
work item references as webhook messages, so a message uma already holds,
however it arrived, is skipped. A comment by `account_id` carrying uma's marker
is the echo of its send, never a new message.

| Import of | GitHub reads |
| --- | --- |
| A conversation | The issue (`GET .../issues/<number>`, for its description), then its comments (`GET .../issues/<number>/comments`) |
| A channel | For each of `history_repositories`: its issues (`GET .../issues?state=all&sort=created&direction=asc`) and its comments (`GET .../issues/comments?sort=created&direction=asc`), merged by creation time |

- **Window.** GitHub's `since` filters on update time, so the adapter asks for
  what was updated since the window start and keeps what was created in it. A
  description is imported when it is not empty and was written in the window.
- **Skipped.** Pull requests and their comments, as the webhook drops them;
  reactions; work item state (the issue's state now says nothing about the time
  a past message was written).
- **Paging.** Pages follow the `Link` header, at most 100 items, sized by the
  first page's limit. The cursor holds each listing's page and last item ID, so
  a comment edited into the window or deleted while it is read neither repeats
  nor drops a message; deletions that move unread comments more than three
  pages back are reported as a gap. A cursor the adapter did not issue for that
  issue or those repositories and window start is `validation`, before any
  call.
- **Failures.** As for the other reads: a rate limit is `rate_limited` with
  GitHub's wait, a missing issue or repository `not_found`. Without
  `history_repositories` a channel-wide import is `disabled`.

## Limits

| What | Limit |
| --- | --- |
| Comment body | 65536 characters (runes) |
| Reconcile look-back | 7 days |
| Update effect | one PATCH; logins at most 39 characters; one milestone |
| Revision precision | one second (`updated_at`); `refetch` resolves ties |
| Reactions | the eight contents above |
| History repositories | 10 per channel |
| History page | 100 items per GitHub page |

## How it is tested

The shared adapter suite and package tests run against a scripted GitHub API
with webhook fixtures for every delivery above. Recorded cassettes **exist** for
this adapter and replay under `go test` with no credentials: work item acts,
message mutations and history (`TestRecordedTransitions`, `TestRecordedMutations`,
`TestRecordedHistory`). The send-and-find exchange (`TestRecordedExchanges`) and
the update effect (`TestRecordedUpdates`) are not recorded yet and run against
the scripted API, whose issue update follows the documented shapes. To record
or refresh cassettes, follow [Record GitHub cassettes](../recording/github.md).

## Known limits

- Replies are opt-in; without `replies` the channel only receives, acts and
  updates issue fields.
- The update effect sets assignees, labels and the milestone only, by whole
  sets; a reconciled update cannot tell its own change from another writer's.
  Issue types and projects are not fields of the effect.
- Pull request comments are dropped; reactions are not received as events.
- History imports messages only, not reactions or work item state; a
  channel-wide import reads only the repositories listed in
  `history_repositories`.
- Closing as a duplicate, `pending` and `resolved` are not supported.
- Issue state and conversation status are independent lifecycles: closing an
  issue never changes the conversation's status, and a conversation transition
  never reaches GitHub.
- No internal notes: GitHub has no private comments.
- Experimental: configuration may change ([Compatibility](../compatibility.md)).
