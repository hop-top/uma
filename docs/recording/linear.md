# Record Linear cassettes

The `linear.issues` adapter has one recorded test, `TestRecordedSandbox`, on
one issue of a sandbox Linear workspace. It skips until its cassettes exist;
until then the same scenario runs against a stand-in endpoint
(`TestSandboxScenarioAgainstStandIn`). You need a workspace that holds nothing
but test data and a personal API key limited to it.

Common steps (record, review, replay, clean up) are in
[Record adapter cassettes](README.md).

## What the recording does

| Test | Cassettes | Traffic |
| --- | --- | --- |
| `TestRecordedSandbox` | `testdata/cassettes/linear/sandbox` | All requests are `POST https://api.linear.app/graphql`, told apart by their bodies. Sends one reply (`commentCreate` with the derived comment `id`), reads it back (`comment(id:)`), reads a comment that was never created (Linear's answer for an unknown comment), edits the comment (`commentUpdate`) and reads it to find the edit's marker, deletes it (`commentDelete`) and reads it to see it gone or archived, edits the deleted comment (Linear's refusal), reads the issue (`issue(id:)`, the refetch), and sends a reply to an issue UUID that does not exist (Linear's refusal). |

It makes three writes that change data (create, edit, delete) and two refused
writes that change nothing, and ends with the issue holding the comments it
started with. It expects no other change of the issue while it records.

What the recording must show, and the test asserts: that Linear accepts the
comment `id` the adapter chooses (`CommentCreateInput.id`, "the identifier in
UUID v4 format") and returns it; that a comment that does not exist, and an
issue that does not exist, are answered with an error the adapter classifies as
`not_found` (an error message containing "not found"); that a deleted comment
reads as absent or archived; and that an edit of a deleted comment is refused.
If an assertion fails, the adapter's assumption about Linear is wrong: do not
edit the cassette or the test to pass; report the recorded answer.

Also check by hand after recording, from the Linear UI or API, what the
recording cannot assert (see [Checks only you can make](#checks-only-you-can-make)).

## Placeholders

Cassettes never hold the sandbox's real identifiers, and replay needs neither
credentials nor a particular workspace. During recording the test passes the
real values to the adapter and `xrrtest` rewrites them to placeholders in what
it saves (requests, answers and headers):

| Real value (variable) | Placeholder in cassettes |
| --- | --- |
| Issue UUID (`LINEAR_SANDBOX_ISSUE`) | `00000000-0000-4000-8000-0000000042a2` |
| Acting user UUID (`LINEAR_SANDBOX_ACCOUNT_ID`) | `00000000-0000-4000-8000-000000910001` |
| Issue identifier (`LINEAR_SANDBOX_IDENTIFIER`) | `SBX-1` |
| Team key (`LINEAR_SANDBOX_TEAM`), as a JSON string | `"SBX"` |
| Workspace URL key (`LINEAR_SANDBOX_WORKSPACE`) | `uma-sandbox` |
| Acting user's display name (`LINEAR_SANDBOX_ACCOUNT_NAME`) | `uma-sandbox-account` |

The comment IDs in the cassettes are derived from fixed operation and attempt
IDs, not issued at random, so they are the same in every recording. A residue
check runs on every body about to be saved and fails the test when a real value
or the API key is still in it.

## Get a sandbox workspace and key

1. Create a Linear workspace for recordings, free plan is enough
   ([API and webhooks](https://linear.app/docs/api-and-webhooks)). Create one
   team in it and one issue in that team, **with the account the key belongs
   to**. Title and describe it with text you would publish: both are recorded
   as written.
2. Create a personal API key: *Settings > Account > Security & Access*. Admins
   and permitted members can; a member needs an admin to allow member API keys
   (*Settings > Administration > API > Member API keys*). Give it the
   permissions **Read**, **Write** and **Create comments** only, limit it to the
   sandbox team, and label it `uma-recording`. It is shown once.
3. With the key exported as `LINEAR_SANDBOX_TOKEN`, read the acting user and the
   issue (the key is sent as is, without `Bearer`):

   ```sh
   curl -s https://api.linear.app/graphql -H "Content-Type: application/json" \
     -H "Authorization: $LINEAR_SANDBOX_TOKEN" \
     --data '{"query":"{ viewer { id name } }"}'
   curl -s https://api.linear.app/graphql -H "Content-Type: application/json" \
     -H "Authorization: $LINEAR_SANDBOX_TOKEN" \
     --data '{"query":"{ issue(id: \"<IDENTIFIER, e.g. SBX-1>\") { id identifier team { key } } }"}'
   ```

   `viewer.id` is the account ID and `viewer.name` its display name;
   `issue.id` is the issue UUID.
4. The workspace URL key is the segment in your Linear URLs
   (`https://linear.app/<workspace>/issue/...`).

## Record Linear cassettes

| Variable | Value |
| --- | --- |
| `LINEAR_SANDBOX_TOKEN` | The personal API key |
| `LINEAR_SANDBOX_ISSUE` | The sandbox issue's UUID |
| `LINEAR_SANDBOX_IDENTIFIER` | The issue's identifier, such as `ENG-12` |
| `LINEAR_SANDBOX_TEAM` | The team key, such as `ENG` |
| `LINEAR_SANDBOX_WORKSPACE` | The workspace URL key |
| `LINEAR_SANDBOX_ACCOUNT_ID` | The user UUID the key acts as |
| `LINEAR_SANDBOX_ACCOUNT_NAME` | That user's display name |

The test skips in record mode when one is missing. Export them in the recording
shell only, then:

```sh
cd adapter/ticket/linear
XRR_MODE=record go test -count=1 -run '^TestRecordedSandbox$' ./...
```

Replay with the variables unset, in a fresh shell:

```sh
go test -count=1 -run '^TestRecordedSandbox$' ./...
```

A cassette miss means a real value reached a recorded request: fix the
placeholders and record again.

Until the cassette directory exists, `go test` skips `TestRecordedSandbox`.

## Keep the traffic safe

- Write only to the sandbox issue; limit the key to the sandbox team.
- Do not change the workspace, the team or the issue while recording.
- Afterwards, check the issue holds no comment the test left (a comment whose
  text is `Recorded reply.` or `Recorded edit.`; the test deletes its own, so a
  leftover means it stopped early; delete it by hand), and revoke the API key
  (*Settings > Account > Security & Access*).

## Check the cassettes

What the recordings remove: the `Authorization` header, and the identifiers
listed under [Placeholders](#placeholders). Issue title, description and comment
text are recorded as written. Check that no real value is left, without
printing them:

```sh
cd adapter/ticket/linear
for v in "$LINEAR_SANDBOX_TOKEN" "$LINEAR_SANDBOX_ISSUE" "$LINEAR_SANDBOX_IDENTIFIER" "$LINEAR_SANDBOX_WORKSPACE" "$LINEAR_SANDBOX_ACCOUNT_ID" "$LINEAR_SANDBOX_ACCOUNT_NAME"; do
  [ -n "$v" ] && grep -rIFc -- "$v" testdata/cassettes/linear | awk -F: '{n+=$2} END {print n+0}'
done
```

Every count must be `0`. Also check, by reading the answers (do not print
secrets):

- [ ] No user e-mail address, avatar URL or profile URL of a real person (the
      adapter's queries ask for `id`, `name` and `app` only; a hand-edited query
      could add more).
- [ ] No organization or team UUID: the adapter asks for the team `key` only. If
      one appears, replace it in the answer with a neutral value.
- [ ] Rate limit headers (`X-RateLimit-*`) hold counters only, no identifiers.
- [ ] The two refusals (unknown comment, unknown issue) hold Linear's own
      error JSON; read them and note, in the pull request, the HTTP status and
      `extensions` of each: the adapter classifies by those and by the message
      "not found".

## Checks only you can make

These cannot be asserted from a cassette; each decides whether part of the
adapter works as designed. Report what you see.

1. **Hidden echo marker.** Post, with the recorded scenario's body, a comment
   through the adapter and open it in Linear: the `<!-- uma:... -->` line must
   not be visible, and `comment(id:){ body }` must return it unchanged. If
   Linear shows or strips it, echo correlation by marker does not work.
2. **Real webhook deliveries.** The webhook tests use stand-in payloads built
   from the documentation. To replace them, create a webhook (*Settings >
   Administration > API*, admin only) for the resource types **Issue** and
   **Comment**, pointing at a tunnel to a local listener that saves each request
   (headers and raw body). With the sandbox: create an issue with a
   description, edit the description, comment, edit the comment, delete the
   comment, delete the issue; also comment through the API key as the adapter
   would. Keep the signing secret out of the files. Check in the saved
   deliveries: the exact `webhookTimestamp` units (milliseconds), the header
   `Linear-Event`, `actor.type` values for a person and for an OAuth
   application, whether `data.botActor` is present for application comments,
   whether `updatedFrom` lists `description`, and whether a retried delivery
   carries a fresh `webhookTimestamp` (the adapter refuses a stale one).
   Scrub names, e-mail addresses, URLs and IDs, then commit the bodies as
   fixtures with signatures recomputed under a fixture secret.
3. **Application tokens.** If the adapter will run under an OAuth `actor=app`
   token, repeat the scenario with one: `viewer.id` is then the application's
   user, and the comment's `user` or `botActor` must match `account_id`.
4. **Rate limit shape.** Linear's documentation says a rate-limited request is
   HTTP 429 with error code `RATELIMITED` and a `Retry-After`; other sources
   report HTTP 400. The adapter treats both alike. Note which Linear answers,
   if you ever see one.
