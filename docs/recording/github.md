# Record GitHub cassettes

The `github.issues` adapter has five recorded tests, all on one issue of a
sandbox repository. You need a repository you own and a token limited to
it.

Common steps (record, review, replay, clean up) are in
[Record adapter cassettes](README.md).

## What the recordings do

| Test | Cassettes | Traffic |
| --- | --- | --- |
| `TestRecordedExchanges` | `testdata/cassettes/github/comment` | Sends one reply (`POST /repos/<owner>/<repo>/issues/<number>/comments`), then finds it again among the issue's recent comments by the account that wrote it (`GET .../issues/<number>/comments?since=...`). Records the `Link` header for paging and drops the volatile `since` parameter. |
| `TestRecordedTransitions` | `testdata/cassettes/github/act` | Closes the issue (default reason, sent as `completed`), reconciles the close, reopens it, closes it as `not_planned`, reconciles a `completed` close (not applied), and reopens it (`PATCH .../issues/<number>`, `GET .../issues/<number>`). Then sends one close to issue `999999`, which does not exist, to record GitHub's `404`; nothing changes. |
| `TestRecordedHistory` | `testdata/cassettes/github/history` | Posts three comments (`POST .../issues/<number>/comments`), reads the first back (`GET .../issues/comments/<id>`) to take its creation time as the window start, then reads the window: of the issue (`GET .../issues/<number>`, then `GET .../issues/<number>/comments?since=...&per_page=2`, paged through the `Link` header) and channel-wide over the sandbox repository (`GET .../issues?state=all&sort=created&direction=asc&since=...` and `GET .../issues/comments?sort=created&direction=asc&since=...`). Then reads the window of issue `999999`, which does not exist, to record GitHub's `404`. Deletes its three comments (`DELETE .../issues/comments/<id>`). Records the `Link` header. |
| `TestRecordedMutations` | `testdata/cassettes/github/mutate` | Posts one comment, edits it (`PATCH .../issues/comments/<id>`) and finds the edit by its marker (`GET`), adds a 👍 reaction (`POST .../comments/<id>/reactions`), finds it among the comment's reactions, removes it (list, then `DELETE .../reactions/<id>`), removes it again (a list, no write), deletes the comment (`DELETE .../issues/comments/<id>`) and reconciles the delete (`GET`, `404`). Then edits, deletes and unreacts the deleted comment, which records GitHub's `404` for each; nothing changes. |
| `TestRecordedUpdates` | `testdata/cassettes/github/update` | Sets the issue's labels to `uma-recorded` (`PATCH .../issues/<number>` with `{"labels":["uma-recorded"]}`), reconciles the update by reading the issue (`GET .../issues/<number>`, applied), reconciles an update to `uma-other` (`GET`, not applied), clears the labels (`PATCH` with `{"labels":[]}`) and reconciles that (`GET`, applied). Then updates issue `999999`, which does not exist, to record GitHub's `404`; nothing changes. |

`TestRecordedTransitions` makes five issue updates and two reads. It
expects the issue open when it starts and leaves it open, with state reason
`reopened`.

`TestRecordedUpdates` makes two issue updates and three reads. It expects the
issue **without labels** when it starts and leaves it without labels. The
first recording creates the label `uma-recorded` in the repository when it
is absent (GitHub creates a label named in an issue update); delete it
afterwards. The sandbox names no login and no milestone, so assignees and
the milestone are not recorded: they are covered against the local server
shaped like GitHub's answers. To record them too, add an update naming the
account's login and an existing milestone number to the test and extend the
sandbox transport to map the login in request bodies.

`TestRecordedHistory` makes six writes (three comments, three deletes) and
deletes what it posted, so the issue ends with the comments it started with.
It expects no other activity in the sandbox repository while it records: a
comment or issue written by anyone else during the run falls in the
channel-wide window and fails the test. Its window starts at its own first
comment, so earlier comments and the issue's description stay out of it; the
`since` parameter is that comment's creation time, the same in recording and
replay, so it is kept in the cassettes.

`TestRecordedMutations` makes seven writes in all (the comment, its edit, the
reaction and its removal, the delete, and the two refused calls on the
deleted comment) and deletes its own comment, so the issue ends with the
comments it started with. It needs no second user: the sandbox holds only the
recording account's comments, so the edit of "another user's comment" is the
same call as the edit of the account's own (the adapter checks no
authorship), and GitHub's `403` for a comment the token may not edit is
covered by the local server's tests, not recorded.

## Placeholders

Cassettes never hold the sandbox's real identifiers, and replay needs
neither credentials nor a particular issue or account. During recording a
translating transport sits between the test and GitHub:

- requests leave the adapter naming repository `example/uma-sandbox` and
  issue `4242`, and reach GitHub with the real repository and issue;
- answers come back with the real repository name, issue number, the
  account's user ID, user logins, user profile and avatar URLs and node IDs
  replaced: repository `example/uma-sandbox`, issue `4242` (issue ID
  `3000004242`), account ID `9100001`, logins `uma-sandbox-account` (the
  account) and `uma-sandbox-user` (anyone else), node IDs `NODE_ID`; the
  account-specific `u=` hash of avatar URLs is dropped. GitHub's `Link`
  header names the repository by its numeric ID
  (`/repositories/<id>/...`); the ID becomes `700000001`.

Comment and reaction IDs are GitHub's own and stay in the cassettes: the
adapter reads them from one recorded answer and sends them in the next.

So any sandbox issue number and any account replay against the same
fixture values. Issue numbers other than the sandbox issue pass through
unchanged.

## Get a sandbox repository and token

1. Use a repository for recordings, private or public, holding nothing but
   test issues. Open one issue **with the account the token acts as**:
   `TestRecordedTransitions` checks the issue's requester is that account.
2. Create a fine-grained personal access token
   ([personal access tokens](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens)):
   - **Repository access:** only the sandbox repository.
   - **Repository permissions:** Issues, read and write. Fine-grained tokens
     also carry Metadata, read-only.
   - **Expiration:** the shortest that covers your session.

   The adapter declares `issues:read`, `issues:write` and `metadata:read`;
   nothing else is needed
   ([permissions for fine-grained tokens](https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens)).
3. With the token exported as `GITHUB_SANDBOX_TOKEN`, get the numeric user ID
   it acts as
   ([get the authenticated user](https://docs.github.com/en/rest/users/users#get-the-authenticated-user)):

   ```sh
   curl -s -H "Authorization: Bearer $GITHUB_SANDBOX_TOKEN" https://api.github.com/user | jq .id
   ```

## Record GitHub cassettes

| Variable | Value |
| --- | --- |
| `GITHUB_SANDBOX_REPO` | The sandbox repository, `owner/repo` |
| `GITHUB_SANDBOX_ISSUE` | The sandbox issue number |
| `GITHUB_SANDBOX_ACCOUNT_ID` | The numeric user ID the token acts as |
| `GITHUB_SANDBOX_TOKEN` | The token |

The tests skip in record mode when one is missing. Record each test on its
own, with the issue open:

```sh
cd adapter/ticket/github
export GITHUB_SANDBOX_REPO=<owner>/<repo> GITHUB_SANDBOX_ISSUE=<number> GITHUB_SANDBOX_ACCOUNT_ID=<user ID> GITHUB_SANDBOX_TOKEN=<token>
XRR_MODE=record go test -count=1 -run '^TestRecordedTransitions$'
XRR_MODE=record go test -count=1 -run '^TestRecordedExchanges$'
XRR_MODE=record go test -count=1 -run '^TestRecordedMutations$'
XRR_MODE=record go test -count=1 -run '^TestRecordedHistory$'
XRR_MODE=record go test -count=1 -run '^TestRecordedUpdates$'
```

The token needs nothing more for `TestRecordedHistory`: reading issues and
comments is covered by Issues, read; posting and deleting its comments by
Issues, write. A recorded history read changes nothing at GitHub.
`TestRecordedUpdates` needs Issues, write as well (`PATCH .../issues/<number>`
is a write, `GET .../issues/<number>` a read under
[permissions for fine-grained tokens](https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens));
a fine-grained token on a repository you own has the push access GitHub
requires to change labels, without which it drops the change silently.

Until a test's cassette directory exists, plain `go test` runs that test
against a local server shaped like GitHub's answers instead of replaying.

## Keep the traffic safe

- Write only to the sandbox issue; the token cannot reach other
  repositories.
- Do not change the repository's settings, webhooks or the issue while
  recording.
- Afterwards, delete the comment `TestRecordedExchanges` posted
  ([delete an issue comment](https://docs.github.com/en/rest/issues/comments#delete-an-issue-comment))
  and revoke the token. `TestRecordedMutations` deletes its own comment and
  `TestRecordedHistory` its three (bodies `Recorded history one.`, `two.`,
  `three.`); if either stops before its deletes, delete those too.
  `TestRecordedUpdates` clears the labels it set; if it stops before that,
  remove the label `uma-recorded` from the issue. Delete the label
  `uma-recorded` from the repository
  ([delete a label](https://docs.github.com/en/rest/issues/labels#delete-a-label)).

## Check the cassettes

What the recordings remove: the `Authorization` header, and the
identifiers listed under [Placeholders](#placeholders). Comment and issue
bodies and titles are recorded as written, so write nothing you would not
publish. Check that no real value is left, without printing them:

```sh
cd adapter/ticket/github
for v in "$GITHUB_SANDBOX_TOKEN" "$GITHUB_SANDBOX_REPO" "${GITHUB_SANDBOX_REPO%%/*}" "$GITHUB_SANDBOX_ACCOUNT_ID" "/issues/$GITHUB_SANDBOX_ISSUE\b"; do
  grep -rIEc -- "$v" testdata/cassettes/github | awk -F: '{n+=$2} END {print n+0}'
done
```

Every count must be `0`. Also check the login of the account the token acts
as, when it differs from the repository owner.

For `testdata/cassettes/github/update`, also check that the issue answers
hold no label but `uma-recorded` (the first two reads) or none (the last),
and no assignee or milestone: the test expects the issue bare, and a value
another person set would be recorded as written.

For `testdata/cassettes/github/history`, also check, without printing values:

- the repository's numeric ID (`curl -s -H "Authorization: Bearer $GITHUB_SANDBOX_TOKEN" https://api.github.com/repos/$GITHUB_SANDBOX_REPO | jq .id`)
  appears nowhere: `Link` headers must name `/repositories/700000001/`;
- the channel-wide listings hold only the sandbox issue and the three
  recorded comments: no other issue's title or body, no other user's login
  (every other user is `uma-sandbox-user`), no pull request.
