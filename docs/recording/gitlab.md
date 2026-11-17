# Record GitLab cassettes

The `gitlab.issues` adapter has four recorded tests: three against one issue of
a sandbox project (cassettes committed), and one over webhook deliveries you
capture from it (not captured yet, so it skips). You need a project
you own, one issue in it and a token limited to it.

Common steps (record, review, replay, clean up) are in
[Record adapter cassettes](README.md).

## What the recordings do

| Test | Cassettes | Traffic |
| --- | --- | --- |
| `TestRecordedExchanges` | `testdata/cassettes/gitlab/note` | Sends two replies (`POST /projects/<id>/issues/<iid>/notes`), the second with a line `/close` that GitLab would run as a quick action; finds the first among the issue's recent notes (`GET .../notes?sort=desc&order_by=created_at&per_page=100`); reads both back and the issue (`GET .../notes/<id>`, `GET .../issues/<iid>`); edits the first (`PUT .../notes/<id>`) and finds the edit by its marker (`GET`); deletes both (`DELETE .../notes/<id>`), finds the deletion (`GET`, `404`) and deletes the first again (`DELETE`, `404`). |
| `TestRecordedTransitions` | `testdata/cassettes/gitlab/act` | Closes the issue (`PUT .../issues/<iid>` with `state_event: close`), reconciles the close (`GET .../issues/<iid>`), reconciles a reopen (not applied), closes the closed issue again, reopens it (`state_event: reopen`) and reconciles that. Then sends one close to issue `999999`, which does not exist, to record GitLab's `404`; nothing changes. |
| `TestRecordedHistory` | `testdata/cassettes/gitlab/history` | Posts three replies (`POST .../notes`), reads the first back (`GET .../notes/<id>`) to take its creation time as the window start, then reads the window of the issue: `GET .../issues/<iid>`, then `GET .../notes?sort=asc&order_by=created_at&per_page=2` paged through `X-Next-Page`. Deletes its three notes (`DELETE`). |
| `TestRecordedWebhookPayloads` | none: `testdata/webhooks/*.json` | Replays deliveries you captured; see [Capture webhook deliveries](#capture-webhook-deliveries). |

`TestRecordedTransitions` makes four updates (the last refused) and three reads and expects the
issue open when it starts; it leaves it open. `TestRecordedExchanges` makes six
writes (two notes, an edit, two deletes, and the refused second delete) and
deletes its own notes. `TestRecordedHistory` makes six writes (three notes,
three deletes) and expects **no other activity on the issue while it records**:
a note written by anyone else during the run falls in the window and fails the
test. Its window starts at its own first note, so earlier notes and the
description stay out of it.

### Premises the recording settles

The adapter was written to GitLab's documentation without a sandbox. These
exchanges decide what the documentation leaves open; if one comes back
differently the test fails at recording, and the adapter, not the test, is what
needs the fix (report it):

- **Quick actions.** `TestRecordedExchanges` expects the issue still open after
  the reply holding `/close`, and the stored note to still read `/close`. The
  adapter escapes such a line with a backslash (`\/close`); this checks that
  GitLab does not run it and shows the stored form.
- **Closing a closed issue.** `TestRecordedTransitions` expects the second close
  to answer 200 with the issue closed, as the adapter relies on (one `PUT` that
  never reads the issue first). The documentation does not say.
- **The marker.** Replies end with an HTML comment naming the operation and
  attempt; the finds above expect GitLab to keep it in the stored body.
- **Pagination.** The history read expects `X-Next-Page` on every page but the
  last, empty on the last.

## Placeholders

Cassettes never hold the sandbox's real identifiers, and replay needs neither
credentials nor a particular project, issue or account. During recording a
translating transport sits between the test and GitLab:

- requests leave the adapter naming host `gitlab.example.com`, project
  `700000001` (path `example/uma-sandbox`) and issue `4242`, and reach GitLab
  with the real host, project and issue;
- answers come back with the real host, project path and ID, issue number and
  ID, the account's user ID, user names and profile and avatar URLs replaced,
  and emails and user creation times removed: project `700000001`, issue
  `4242` (issue ID `3004242`), account ID `9100001`, name `uma-sandbox-account`
  (the account) and `uma-sandbox-user` (anyone else, ID `9100002`). Note IDs
  and timestamps are GitLab's own and stay: the adapter reads them from one
  recorded answer and sends them in the next.

Issue numbers other than the sandbox issue pass through unchanged.

## Get a sandbox project and token

1. Use a project for recordings, private or public, holding nothing but test
   issues. Open one issue in it, with any account.
2. Create a **project access token**
   ([project access tokens](https://docs.gitlab.com/user/project/settings/project_access_tokens/)):
   - **Role:** Reporter or higher; Developer is enough to close and reopen
     issues and to edit and delete the token's own notes.
   - **Scopes:** `api`. The adapter needs nothing else
     ([token scopes](https://docs.gitlab.com/api/rest/authentication/)).
   - **Expiration:** the shortest that covers your session.

   A project access token acts as a bot user, so every note it writes is the
   bot's: that is the `account_id`. Its username has the form
   `project_<project id>_bot_<random>`.
3. With the token exported as `GITLAB_SANDBOX_TOKEN`, get the project's numeric
   ID and path and the bot's user ID
   ([get a single project](https://docs.gitlab.com/api/projects/#get-a-single-project),
   [get the current user](https://docs.gitlab.com/api/users/#get-the-current-user)):

   ```sh
   curl -s -H "Authorization: Bearer $GITLAB_SANDBOX_TOKEN" "https://gitlab.com/api/v4/projects/<group>%2F<project>" | jq '{id, path_with_namespace}'
   curl -s -H "Authorization: Bearer $GITLAB_SANDBOX_TOKEN" "https://gitlab.com/api/v4/user" | jq .id
   ```

   Use your instance's API root instead of `https://gitlab.com/api/v4/` when
   self-managed, and set `GITLAB_SANDBOX_URL` to it below. If the user endpoint
   refuses the token, take the bot's ID from the project's members list.

## Record GitLab cassettes

| Variable | Value |
| --- | --- |
| `GITLAB_SANDBOX_URL` | The REST API root, only when not `https://gitlab.com/api/v4/` |
| `GITLAB_SANDBOX_PROJECT_ID` | The sandbox project's numeric ID |
| `GITLAB_SANDBOX_PROJECT_PATH` | The project's `path_with_namespace`, with at least one `/` |
| `GITLAB_SANDBOX_ISSUE` | The sandbox issue's number within the project (`iid`) |
| `GITLAB_SANDBOX_ACCOUNT_ID` | The numeric user ID the token acts as |
| `GITLAB_SANDBOX_TOKEN` | The token |

The tests skip in record mode when one is missing. Record each test on its own,
with the issue open:

```sh
cd adapter/ticket/gitlab
export GITLAB_SANDBOX_PROJECT_ID=<id> GITLAB_SANDBOX_PROJECT_PATH=<group/project> GITLAB_SANDBOX_ISSUE=<iid> GITLAB_SANDBOX_ACCOUNT_ID=<user ID> GITLAB_SANDBOX_TOKEN=<token>
XRR_MODE=record go test -count=1 -run '^TestRecordedTransitions$'
XRR_MODE=record go test -count=1 -run '^TestRecordedExchanges$'
XRR_MODE=record go test -count=1 -run '^TestRecordedHistory$'
```

Then replay all three without the variables: `go test -count=1 -run 'TestRecorded'`.
Until a test's cassette directory exists, plain `go test` skips that test.

## Capture webhook deliveries

GitLab's webhooks cannot be recorded by the test; capture them by hand from the
sandbox project, so the adapter is judged on real payloads rather than the
documentation's examples.

1. In the project, **Settings > Webhooks**
   ([webhooks](https://docs.gitlab.com/user/project/integrations/webhooks/)), add
   a webhook to any endpoint you control (a request-capturing service you own,
   or `https://example.invalid` and read the payloads from the delivery log
   below). Set a **secret token** and select **Issues events**, **Comments** and
   **Confidential issues events** (and **Confidential comments** where your
   version offers it). Do not select anything else.
2. In the sandbox issue, with the bot or your own account: write a comment,
   edit it, write an **internal** comment (the "Make this an internal note"
   checkbox), change the issue's description, close and reopen the issue, and
   add a label (which writes a system note). Open a merge request in the project
   and comment on it, to capture a delivery the adapter drops.
3. In **Settings > Webhooks > Edit > Recent events**, open each delivery and
   copy its **request body** (the JSON) and its `X-Gitlab-Event` header.
4. Save each body as `testdata/webhooks/<event>__<case>.json`, where `<event>` is
   the header with spaces as dashes (`Issue-Hook`, `Note-Hook`,
   `Confidential-Note-Hook`, `Confidential-Issue-Hook`) and `<case>` names it:
   `note-create`, `note-update`, `note-internal`, `issue-open`,
   `issue-update-description`, `issue-close`, `issue-reopen`, and the dropped
   ones `note-system` and `note-merge-request` (`__system` and `__merge-request`
   in the name tell the test an empty result is expected). Record the exact
   `X-Gitlab-Event` strings you saw in the pull request: the adapter accepts
   `Issue Hook`, `Confidential Issue Hook`, `Note Hook` and
   `Confidential Note Hook`.
5. Scrub every body before saving it (see the checklist below), then run
   `go test -count=1 -run TestRecordedWebhookPayloads`.
6. Delete the webhook in the project when done.

## Keep the traffic safe

- Write only to the sandbox issue; the project access token cannot reach other
  projects.
- Do not change the project's settings, other webhooks or the issue's title and
  description while recording.
- Afterwards, delete what a failed run left
  ([delete a note](https://docs.gitlab.com/api/notes/#delete-an-issue-note)) and
  revoke the token. `TestRecordedExchanges` deletes both of its notes (bodies
  `Recorded reply.` and `Recorded quick action line:`...) and
  `TestRecordedHistory` its three (`Recorded history one.`, `two.`, `three.`);
  if either stops before its deletes, delete those too. If
  `TestRecordedTransitions` stops while the issue is closed, reopen it.

## Check the cassettes

What the recordings remove: the `Authorization` header, and the identifiers
listed under [Placeholders](#placeholders). Note and issue bodies and titles are
recorded as written, so write nothing you would not publish. Check that no real
value is left, without printing them:

```sh
cd adapter/ticket/gitlab
for v in "$GITLAB_SANDBOX_TOKEN" "$GITLAB_SANDBOX_PROJECT_PATH" "${GITLAB_SANDBOX_PROJECT_PATH%%/*}" "$GITLAB_SANDBOX_PROJECT_ID" "$GITLAB_SANDBOX_ACCOUNT_ID" "/issues/$GITLAB_SANDBOX_ISSUE\b"; do
  grep -rIEc -- "$v" testdata | awk -F: '{n+=$2} END {print n+0}'
done
```

Every count must be `0`. Also check, by reading the files:

- the real host name (when self-managed) appears nowhere; every URL names
  `gitlab.example.com`;
- the bot's username (`project_<id>_bot_...`) and your own username appear
  nowhere: every user is `uma-sandbox-account` or `uma-sandbox-user`, and no
  email, `avatar_url` or `web_url` names a real user;
- the issue's real `id` and `iid` appear nowhere, nor the project's `id` in a
  `project_id` field;
- `history`: the window holds only the three recorded notes and no other
  user's note text.

For `testdata/webhooks`, which no transport scrubs, also replace by hand, in
every file:

- the project: `project.id`, `project.name`, `project.path_with_namespace`,
  `project.web_url`, `project.namespace`, `project.homepage`, `project.git_*`,
  `project.http_url`, `project.ssh_url`, `project.url` and every other URL
  naming the project (`repository.*`, `object_attributes.url`, `issue` URLs)
  with `example/uma-sandbox` forms and project ID `700000001`;
- the users: `user.id`, `user.name`, `user.username`, `user.avatar_url`,
  `user.email`, `object_attributes.author_id`, `assignee_ids` and each
  `assignees` and `labels[].project_id` with `9100001` (the account) and
  `9100002` (anyone else) and the names `uma-sandbox-account` and
  `uma-sandbox-user`; keep the bot's username shape
  `project_700000001_bot_fixture` for the account, which the adapter reads;
- the issue: `object_attributes.id`, `iid`, `noteable_id` and `issue.id`, `iid`
  with `3004242` and `4242`, consistently across the files of one scenario.

Keep every timestamp, the `action` and `state` fields and the note and
description texts. Read each file once for anything else you would not publish.
