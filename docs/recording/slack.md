# Record Slack cassettes

The `slack.chat` adapter's recorded test writes to one throwaway channel of a
Slack workspace with a bot token: it sends a message, finds it again, edits it,
adds and removes a reaction, deletes it, and archives and unarchives the
channel. You need a workspace you can
install an app in, and a channel the app's bot is a member of.

Common steps (record, review, replay, clean up) are in
[Record adapter cassettes](README.md).

## What the recording does

`TestRecordedWorkspace`, in the channel you name:

1. posts a message (`chat.postMessage`) and finds it again by its attempt
   (`conversations.history` with metadata);
2. edits it (`chat.update`) and reads it back (`conversations.replies`
   bounded to the message);
3. adds the 👍 reaction (`reactions.add`) and reads it (`reactions.get`),
   then removes it (`reactions.remove`) and reads that;
4. deletes the message (`chat.delete`) and reads that it is gone;
5. archives the channel (`conversations.archive`) and reads that it is
   archived (`conversations.info`), then unarchives it
   (`conversations.unarchive`) and reads that it is open again.

Reading history to import it is not recorded: its request carries the time of
the call, which a replay cannot match. Interactions, slash commands and events
arrive from Slack and are not recorded either; the two archive events are
captured by hand ([Capture the archive events](#capture-the-archive-events)).

## Get a workspace

- **Developer sandbox:** free through the Slack Developer Program. A basic
  sandbox lives one month; others six
  ([developer sandboxes](https://docs.slack.dev/tools/developer-sandboxes/)).
- **A workspace you own** that holds no customer data.

Never record in a workspace people work in: the test posts, edits and deletes.

## Get a bot token

1. At [api.slack.com/apps](https://api.slack.com/apps), choose **Create an
   app**, then **Blank app**, name it and pick the workspace
   ([app settings quickstart](https://docs.slack.dev/app-management/quickstart-app-settings/)).
2. Under **OAuth & Permissions**, add these **Bot Token Scopes**: `chat:write`,
   `reactions:read`, `reactions:write`, `channels:history` (`groups:history`
   for a private channel), `channels:manage` (`groups:write` for a private
   channel) to archive, and `channels:read` (`groups:read` for a private channel) to read the channel's
   state back. Under **Event Subscriptions**, subscribe the bot to
   `channel_archive` and `channel_unarchive` (`group_archive` and
   `group_unarchive` for a private channel).
3. **Install to Workspace** and copy the token under **OAuth Tokens**; it
   starts with `xoxb-`.
4. Create a channel for the test and invite the app to it: the history reads
   need the bot to be a member
   ([`conversations.history`](https://docs.slack.dev/reference/methods/conversations.history/)).
5. Find the IDs the test needs: the workspace (`T...`) and the bot user
   (`U...`) with `auth.test` run on the token
   ([`auth.test`](https://docs.slack.dev/reference/methods/auth.test/)), which
   also names the bot (`B...`), and the ID of the channel from step 4 (`C...`).

The signing secret (the app's **Basic Information** page) is not used by
`TestRecordedWorkspace`: it sends and reads, it receives nothing. It is needed
only to capture the events below.

## Record Slack cassettes

| Variable | Value |
| --- | --- |
| `SLACK_BOT_TOKEN` | The bot token |
| `SLACK_TEAM_ID` | The workspace ID |
| `SLACK_BOT_USER_ID` | The bot user's ID |
| `SLACK_CHANNEL_ID` | The throwaway channel's ID |
| `SLACK_BOT_ID` | The bot's ID; optional |

The test fails in record mode when one of the first four is missing.

```sh
cd adapter/chat/slack
export SLACK_BOT_TOKEN=<token> SLACK_TEAM_ID=<T...> SLACK_BOT_USER_ID=<U...> SLACK_CHANNEL_ID=<C...> SLACK_BOT_ID=<B...>
XRR_MODE=record go test -count=1 -run TestRecordedWorkspace
```

Cassettes land in `adapter/chat/slack/testdata/cassettes/slack`. The test needs
the bot able to archive the channel; a channel the bot did not create, or a
workspace that limits who may archive, answers `restricted_action`, and the
cassette then shows that answer in place of the archive. Slack removes members
from an archived channel; if the unarchive is refused (`not_in_channel` or
`channel_not_found`), unarchive the channel by hand and note the answer, as the
adapter's page says it is unverified that a bot token can.

The test dates the reconciliation reads well before the recording, so the same
requests replay whenever the test runs. The message carries metadata of type
`uma_attempt`; if the app has not registered it, Slack documents that the
metadata is ignored or refused
([message metadata](https://docs.slack.dev/messaging/message-metadata/)). The
adapter then sends without it and reconciles by text, and the cassette shows
which happened: a send answered with an error, then a second one without
`metadata`.

## Capture the archive events

Slack sends `channel_archive` and `channel_unarchive` (`group_archive` and
`group_unarchive` for a private channel, same payload shape) to the app while the
archive steps run, so they are not in the cassettes. With the app's request URL
(or Socket Mode) reachable, run the test above and copy the two
`event_callback` bodies Slack delivers into
`adapter/chat/slack/testdata/events/slack/channel_archive.json` and
`channel_unarchive.json`. Before committing, replace literally the workspace ID
with `SLACK_TEAM_ID`, the app ID with `SLACK_APP_ID`, the channel ID with
`SLACK_CHANNEL_ID` and the user ID with `SLACK_USER_ID`, and drop `token`,
`authorizations` and any other field naming a person. `TestRecordedArchiveEvents`
replays them and skips until they exist.

## Keep the traffic safe

- Write only to the throwaway channel.
- Do not change the app's scopes or event subscriptions while recording.
- The test leaves the channel unarchived. If a run stops between the archive and
  the unarchive, unarchive the channel by hand.
- Afterwards, archive the channel and revoke the token or uninstall the app.

## Check the cassettes

What the test removes: the `Authorization` header, and the workspace, bot user,
channel and bot IDs where they appear literally (recorded as `SLACK_TEAM_ID`,
`SLACK_BOT_USER_ID`, `SLACK_CHANNEL_ID` and `SLACK_BOT_ID`).

What it does not cover:

- **Message objects** Slack returns: `bot_profile` (the app's ID, name and team),
  `team`, and `blocks` with generated IDs. Replace any you do not want
  published by hand in the response.
- **Names and user IDs** of other people in the channel; there should be none.
- **Timestamps** (`ts`) are the message's own and stay: they are what later
  requests name.

```sh
cd adapter/chat/slack
grep -rnE '"(app_id|team_id|display_name|real_name|username|email)"' testdata/cassettes/slack
grep -rnF -e "$SLACK_BOT_TOKEN" -e "$SLACK_TEAM_ID" -e "$SLACK_BOT_USER_ID" -e "$SLACK_CHANNEL_ID" testdata/cassettes/slack
```
