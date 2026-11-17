# Record Discord cassettes

The `discord.chat` adapter's recorded test talks to Discord's REST API with a
bot token, in one throwaway channel and one thread of a private test guild: it
sends a message, finds it again, edits it, adds and removes a reaction, shows
the bot typing, adds a member to the thread and removes them, and deletes the
message; and, in a second set of cassettes, archives the thread and unarchives
it. Discord has no sandbox; its guide says to test "in a server that isn't
actively used by others", and a server is free to create
([getting started](https://docs.discord.com/developers/quick-start/getting-started)).

Common steps (record, review, replay, clean up) are in
[Record adapter cassettes](README.md).

## What the recording does

`TestRecordedBot`, in the channel and thread you name:

1. posts a message (Create Message, with a nonce) and finds it again (Get
   Channel Messages `after` a fixed date);
2. edits it (Edit Message) and reads it back (Get Channel Message);
3. adds the 👍 reaction (Create Reaction) and reads it, then removes it
   (Delete Own Reaction) and reads that;
4. shows the bot typing in the channel (Trigger Typing Indicator);
5. adds the member to the thread (Add Thread Member) and reads it (Get
   Thread Member), then removes them (Remove Thread Member) and reads that;
6. deletes the message (Delete Message) and reads that it is gone.

And, as the subtest `thread archive`, with its own cassettes in
`testdata/cassettes/discord-archive`, in the thread:

7. archives it (Modify Channel, `archived` true) and reads it (Get Channel:
   `thread_metadata.archived` true), then unarchives it (Modify Channel,
   `archived` false) and reads that. Archiving needs **Manage Threads**.
   The thread ends as it began, unarchived: nothing to clean up, unless the
   test failed between the two calls (see
   [Keep the traffic safe](#keep-the-traffic-safe)).

Not recorded: the gateway (a WebSocket session, not an HTTP exchange of the
adapter's; `THREAD_UPDATE`, which reports the archive, is a hand-written
fixture in `state_test.go`), interactions and their webhook answers (Discord sends the
interaction, and its token lives fifteen minutes), and history reads, whose
request names the time of the call.

## Get a guild and a bot

1. Create a server in the Discord client (**Add a Server**, **Create My
   Own**). Keep it private: no one but you and a second account of yours.
2. At the [Developer Portal](https://discord.com/developers/applications),
   **New Application**. On **Bot**, **Reset Token** and copy the token; leave
   the privileged intents off (the test reads no gateway).
3. On **OAuth2**, **URL Generator**: scope `bot`, permissions **View
   Channels**, **Send Messages**, **Send Messages in Threads**, **Read Message
   History**, **Add Reactions** and **Manage Threads**. Open the URL and add
   the bot to the server
   ([OAuth2](https://docs.discord.com/developers/topics/oauth2),
   [permissions](https://docs.discord.com/developers/topics/permissions)).
4. In the server, create a text channel for the test, and in it a public
   thread that is not locked; leave the thread unarchived. Its auto-archive
   duration is one hour at the shortest: the test finishes well inside it, but
   do not leave it idle between recordings and expect it unarchived.
5. Have a second account of yours join the server: it is the member the test
   adds to and removes from the thread.
6. Find the IDs (turn on **Developer Mode** under **Advanced** in the client's
   settings, then **Copy ID** on each): the application (the portal's
   **General Information**), the bot user (the bot's profile in the server),
   the channel, the thread, the thread's parent channel (the channel from
   step 4) and the second account.

The application's public key is not used: the test receives nothing.

## Record Discord cassettes

| Variable | Value |
| --- | --- |
| `DISCORD_BOT_TOKEN` | The bot token |
| `DISCORD_APPLICATION_ID` | The application ID |
| `DISCORD_BOT_USER_ID` | The bot user's ID |
| `DISCORD_CHANNEL_ID` | The throwaway channel's ID |
| `DISCORD_THREAD_ID` | The thread's ID |
| `DISCORD_PARENT_ID` | The thread's parent channel |
| `DISCORD_MEMBER_ID` | The second account's user ID |

The test fails in record mode when one is missing.

```sh
cd adapter/chat/discord
export DISCORD_BOT_TOKEN=<token> DISCORD_APPLICATION_ID=<id> DISCORD_BOT_USER_ID=<id> \
  DISCORD_CHANNEL_ID=<id> DISCORD_THREAD_ID=<id> DISCORD_PARENT_ID=<id> DISCORD_MEMBER_ID=<id>
XRR_MODE=record go test -count=1 -run TestRecordedBot
```

Cassettes land in `adapter/chat/discord/testdata/cassettes/discord` and, for
the `thread archive` subtest, `adapter/chat/discord/testdata/cassettes/discord-archive`.
`-run TestRecordedBot` records both sets; to record only the archive, run
`-run 'TestRecordedBot/thread_archive'` with the same variables: the parent
test then replays the first set, so keep that directory as committed. Without
`discord-archive`, the subtest skips.

The test dates the reconciliation reads at 2026-01-01, well before the
recording, so the same requests replay whenever the test runs; a fresh channel
holds only the test's message after that date. The message mentions the
member, but `allowed_mentions` suppresses the notification.

What the recording answers that the stand-in only assumes, worth reading in
the cassette once recorded:

- whether Get Channel Messages with `after` answers the messages nearest
  after the ID, and whether it returns the message's `nonce`;
- the `X-RateLimit-*` headers of each route;
- the answers of Add and Remove Thread Member and Get Thread Member;
- Modify Channel on a thread: that `{"archived": true}` alone is accepted from
  a bot with Manage Threads, the channel object it answers, and the
  `archive_timestamp` Get Channel shows after each change.

## Keep the traffic safe

- Write only to the throwaway channel and thread.
- Do not change the bot's permissions or the application's settings while
  recording.
- The archive steps end with the thread unarchived. If the test failed
  between archive and unarchive, unarchive the thread in the client (or send a
  message in it) before recording again.
- Afterwards, delete the channel (or the server), and reset the bot token in
  the portal if it was exported anywhere it could linger.

## Check the cassettes

What the test removes: the `Authorization` header, and the application, bot
user, channel, thread, parent and member IDs where they appear literally
(recorded as `DISCORD_APPLICATION_ID`, `DISCORD_BOT_USER_ID`,
`DISCORD_CHANNEL_ID`, `DISCORD_THREAD_ID`, `DISCORD_PARENT_ID` and
`DISCORD_MEMBER_ID`).

What it does not cover:

- **User objects** Discord returns with a message: the bot's and, in mentions,
  the member's `username`, `global_name`, `avatar`, `discriminator`,
  `public_flags` and `clan`. Replace the member's by hand in the response.
- **The guild ID** (`guild_id`, in thread member and channel answers) and
  `member` objects with `joined_at` and roles.
- **Message IDs and timestamps** are the message's own and stay: later
  requests name them.
- **Other people's messages** in the channel since the reads' date, such as
  the system message that started the thread, with its author's user object
  and the guild ID: replace them by hand like the member's.
- **A bot that shares its application's ID**, as new applications' bots do:
  the bot's ID is recorded as `DISCORD_APPLICATION_ID`, the first placeholder
  for that value. Replace it with `DISCORD_BOT_USER_ID` in the bot's
  `author` objects; replay gives the two placeholders different IDs.

```sh
cd adapter/chat/discord
grep -rnE '"(username|global_name|avatar|email|guild_id|joined_at|nick)"' testdata/cassettes/discord
grep -rnF -e "$DISCORD_BOT_TOKEN" -e "$DISCORD_APPLICATION_ID" -e "$DISCORD_BOT_USER_ID" -e "$DISCORD_CHANNEL_ID" \
  -e "$DISCORD_THREAD_ID" -e "$DISCORD_PARENT_ID" -e "$DISCORD_MEMBER_ID" testdata/cassettes/discord
```
