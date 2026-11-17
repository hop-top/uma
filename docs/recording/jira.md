# Record Jira cassettes

The `jira.issues` adapter has two recorded tests, both on one issue of a
sandbox Jira Cloud site. `TestRecordedComments` is recorded; its cassettes are
committed and the test replays them. `TestRecordedWebhooks` is not: it skips
until someone captures webhook deliveries (see
[Capture the webhooks](#capture-the-webhooks)), and webhook parsing rests on
the stand-in Jira written to Atlassian's documentation. This guide is what you
run to refresh the comment cassettes or to capture the webhooks.

Common steps (record, review, replay, clean up) are in
[Record adapter cassettes](README.md). The adapter stays `experimental` until
this is done.

## What the recordings do

| Test | Cassettes | Traffic |
| --- | --- | --- |
| `TestRecordedComments` | `testdata/cassettes/jira/comments` | On the sandbox issue: adds a comment (`POST /rest/api/3/issue/{id}/comment`, an ADF body and the `uma.echo` property) and finds it again (`GET .../comment?orderBy=-created&maxResults=100&startAt=0`, then `GET /rest/api/3/comment/{id}/properties/uma.echo`); reconciles an attempt that was never made (not found); edits the comment (`PUT .../comment/{id}`) and finds the edit by its property; reads the comment (`GET .../comment/{id}`) and the issue (`GET /rest/api/3/issue/{id}?fields=...`) for refetch; deletes the comment (`DELETE`, `204`), reconciles the delete (`GET`, `404`) and deletes it again, which records Jira's `404` for a comment that is gone |
| `TestRecordedWebhooks` | `testdata/webhooks/<event>.json` | Not HTTP exchanges: raw bodies of six real admin webhook deliveries, captured by the owner and replayed through the handler, signed in the test with its fixture secret |

`TestRecordedComments` makes one comment, one edit and two deletes, and removes
its own comment, so the issue ends with the comments it started with.

## What the recordings answer

Besides proving the adapter works on real answers, the recordings decide these
points, which the stand-in only assumes. If an assertion fails, the adapter or
its documentation is wrong, not the cassette:

- Does `POST .../comment` honour the `properties` field, and does
  `GET /rest/api/3/comment/{id}/properties/uma.echo` return it? (The
  [schema](https://developer.atlassian.com/cloud/jira/platform/rest/v3/api-group-issue-comments/)
  says `properties` is optional on create and update. If it does not, the
  reconcile steps fail: the adapter then needs another round-trip marker.)
- Does `orderBy=-created` list newest first?
  ([ordering](https://developer.atlassian.com/cloud/jira/platform/rest/v3/intro/#ordering))
- Does an edit by `PUT` replace the property's value?
- Which timestamp format and offset does the site answer with, and does the
  site's default time zone change it? (The parser accepts any offset.)
- Do comment webhooks carry the `issue` object, ADF or string bodies, and the
  comment's `properties`? (`TestRecordedWebhooks` fails on a comment event
  without an `issue`.)

## Get a sandbox

1. Create a throwaway Jira Cloud site ([free plan](https://www.atlassian.com/software/jira/free))
   you administer, with one software or business project and **no customer
   data**. Create one issue in it with a non-empty description, for example
   `uma recording issue` / `Description written for the uma recording.`. Keep it
   in a status whose category is To Do or In Progress.
2. Create an API token for the account that will record
   ([manage API tokens](https://id.atlassian.com/manage-profile/security/api-tokens)).
   Tokens expire (one year at most); the account needs the project permissions
   Browse projects, Add comments, Edit all comments and Delete all comments
   (or the *own* variants) on the sandbox project. Use a dedicated account, not
   your own.
3. Read what the test needs, with the token exported as `JIRA_SANDBOX_TOKEN`, the
   email as `JIRA_SANDBOX_EMAIL` and the site as `JIRA_SANDBOX_SITE`
   (`https://<site>.atlassian.net`):

   ```sh
   # accountId the token acts as
   curl -s -u "$JIRA_SANDBOX_EMAIL:$JIRA_SANDBOX_TOKEN" "$JIRA_SANDBOX_SITE/rest/api/3/myself" | jq -r .accountId
   # numeric ID of the sandbox issue
   curl -s -u "$JIRA_SANDBOX_EMAIL:$JIRA_SANDBOX_TOKEN" "$JIRA_SANDBOX_SITE/rest/api/3/issue/<KEY-n>?fields=summary" | jq -r .id
   ```

## Record the comment cassettes

| Variable | Value |
| --- | --- |
| `JIRA_SANDBOX_SITE` | The site, `https://<site>.atlassian.net` |
| `JIRA_SANDBOX_EMAIL` | The recording account's email |
| `JIRA_SANDBOX_TOKEN` | The API token |
| `JIRA_SANDBOX_ACCOUNT_ID` | The recording account's `accountId` |
| `JIRA_SANDBOX_ISSUE_KEY` | The sandbox issue's key, `<KEY-n>` |
| `JIRA_SANDBOX_ISSUE_ID` | The sandbox issue's numeric ID |

Recording stops at once when one is missing. Export them in the recording shell
only, then:

```sh
cd adapter/ticket/jira
export JIRA_SANDBOX_SITE=https://<site>.atlassian.net JIRA_SANDBOX_EMAIL=<email> JIRA_SANDBOX_TOKEN=<token> \
  JIRA_SANDBOX_ACCOUNT_ID=<accountId> JIRA_SANDBOX_ISSUE_KEY=<KEY-n> JIRA_SANDBOX_ISSUE_ID=<id>
XRR_MODE=record go test -count=1 -run '^TestRecordedComments$'
```

The test sets `XRR_REDACT_ALLOW=key` itself, for recording and replay. xrr
redacts every JSON field named `key` as a credential, which would blank the
issue key, the project key, the status category's `key` (`new`,
`indeterminate`, `done`) and the `key` of the `uma.echo` property; without it the
issue refetch finds no status and replay misses the request fingerprint.
Placeholders cover the issue and project keys, and the `uma.echo` property name
is not a secret.

Jira numbers comments, issues and projects from shared sequences, so the new
comment's ID can equal the sandbox issue's ID. The issue ID placeholder then
also rewrites the comment ID, the adapter reads a comment that does not exist
and the property lookup answers `404`. Before recording, add a throwaway comment
to the sandbox issue and delete it, then check that the ID the recording
creates differs from `JIRA_SANDBOX_ISSUE_ID`; if it does not, record again.

Until the cassette directory exists, plain `go test` skips the test; the same
scenario runs against the stand-in (`TestRecordedScenarioRunsAgainstTheDouble`).
Replay with the variables unset:

```sh
go test -count=1 -run '^TestRecordedComments$'
```

## Placeholders

Cassettes never hold the sandbox's real identifiers; replay needs neither
credentials nor a particular site. The test replaces, in recorded requests and
answers and in what the adapter sees:

| Real | Placeholder |
| --- | --- |
| the site host | `sandbox.example.atlassian.net` |
| the account email | `sandbox@example.com` |
| the account's `accountId` | `5b10ac8d82e05b22cc7d4ef5` |
| the issue key | `SBX-1` |
| the issue's numeric ID | `10001` |
| the project key | `SBX` |

It also rewrites every `displayName` to `Sandbox User`, every `emailAddress` to
the placeholder email, every `timeZone` to `UTC` and every `locale` to `en_US`,
rewrites the `name` of a project object to `Sandbox Project` (the site's
template names it), and empties `avatarUrls`. The account ID also appears
URL-encoded (`%3A` for the colon) in the `self` URLs of user objects; the test
replaces that form too, and the check below greps for it. The `Authorization` header is never recorded. A real
sandbox value left in a body fails the recording (`jira recording keeps a real
value`). Placeholders replace text literally: pick a project key that is not a
common word, or the replacement also rewrites it inside other text.

## Capture the webhooks

Webhook deliveries cannot be recorded by the test: Jira calls the receiver.
Capture them by hand.

1. Start any listener you control that prints the raw request body and
   headers, reachable over HTTPS (Jira delivers only to HTTPS URLs, on
   ports such as 443, 8080 and 8443, never 80, following at most five redirects),
   for example a tunnel to a local port. Use a throwaway URL.
2. In the sandbox site, System, WebHooks (administrator), create a webhook to
   that URL, with a secret you make up, for the events *Issue: created,
   updated, deleted* and *Comment: created, updated, deleted*, with an empty JQL
   filter (the site is a sandbox). Do not use a dynamic or app webhook: the
   adapter serves admin webhooks.
3. On a new issue with a description, in this order: create it; edit its
   description; add a comment; edit the comment; delete the comment; delete the
   issue. Each action sends one delivery.
4. Save the raw JSON body of each delivery, unchanged, as
   `testdata/webhooks/issue_created.json`, `issue_updated.json` (the description
   edit; one whose changelog names `description`), `comment_created.json`,
   `comment_updated.json`, `comment_deleted.json` and `issue_deleted.json`.
5. Check one delivery's signature against your secret, to prove the header
   format the handler verifies. Without printing the secret:

   ```sh
   # body.json is the raw body; the header is X-Hub-Signature: sha256=<hex>
   printf '%s' "$(cat body.json)" | openssl dgst -sha256 -hmac "$WEBHOOK_SECRET" | awk '{print "sha256=" $2}'
   ```

   (Use the exact bytes: a trailing newline added by an editor changes the
   signature. The test signs each file with its own fixture secret, so the
   saved file only needs to be the body.)
6. Delete the webhook and the sandbox issue; stop the listener.

Scrub each file before committing (see the checklist). Edit values to neutral
ones the same way the cassettes do; keep the structure: field names, types and
nesting are the evidence.

## Check the recordings

For cassettes, without printing values:

```sh
cd adapter/ticket/jira
for v in "$JIRA_SANDBOX_TOKEN" "$JIRA_SANDBOX_EMAIL" "$JIRA_SANDBOX_ACCOUNT_ID" "$(printf %s "$JIRA_SANDBOX_ACCOUNT_ID" | jq -sRr @uri)" "$JIRA_SANDBOX_ISSUE_KEY" "${JIRA_SANDBOX_SITE#https://}"; do
  grep -rIFc -- "$v" testdata | awk -F: '{n+=$2} END {print n+0}'
done
```

Every count must be `0`.

Checklist for the cassettes and the webhook files:

- [ ] Replay passes with the recording variables unset, in a fresh shell.
- [ ] No token, email, site host, `accountId`, issue key or ID of the sandbox in
      `testdata` (the loop above), nor the project key or the site's name in
      free text (titles, comment text, URLs).
- [ ] No other person: every `displayName`, `emailAddress`, `accountId` of anyone
      but the recording account is replaced; webhook payloads carry
      `user` and `author` objects, `reporter`, `creator` and `assignee`, with
      avatar URLs and (for the site) `self` URLs: replace them all.
- [ ] Webhook files: no `X-Hub-Signature`, secret or listener URL (the body has
      none, but a capture tool may have saved headers next to it); no
      `webhookId` or `matchedWebhookIds` of your site; remove or neutralize
      `timestamp` values only if they identify a real event you want to hide
      (the handler does not need them).
- [ ] Issue summaries, descriptions and comments contain only the sentences the
      scenario wrote.
- [ ] Custom field IDs and project, board or sprint names of the site are
      neutral, or removed (`fields` carries many of them).

## Clean up

- `TestRecordedComments` deletes its own comment. If it stopped early, delete the
  comments with the text `uma recorded reply` or `uma recorded edit` from the
  sandbox issue
  ([delete comment](https://developer.atlassian.com/cloud/jira/platform/rest/v3/api-group-issue-comments/#api-rest-api-3-issue-issueidorkey-comment-id-delete)).
- Delete the admin webhook, the webhook listener and its tunnel.
- Delete the sandbox issues the webhook capture created.
- Revoke the API token and delete the recording account if it was created for
  this; close the shell so the variables go.

## Not recordable here

- Rate limiting (`429`, `Retry-After`) and server errors: no safe way to cause
  them; covered by the stand-in's outcome mapping.
- Retries of a webhook (`X-Atlassian-Webhook-Retry`): covered by the digest
  deduplication tests.
- OAuth 2.0 (3LO) requests: need an OAuth app and a user's consent; covered by
  the stand-in's gateway test. The basic-authentication path is the recorded one.
