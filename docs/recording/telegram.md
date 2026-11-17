# Record Telegram cassettes

The `telegram.messages` adapter's recorded test writes to one throwaway group
with a bot of your own: it sends messages as plain text, as HTML and as
MarkdownV2 with user text escaped, with an inline keyboard and as a reply, edits
one, sets and clears a reaction, and deletes it, then sees Telegram refuse the
edit and the deletion of a message that is gone and markup it cannot parse. You
need a Telegram account, a bot and a group the bot is in.

Common steps (record, review, replay, clean up) are in
[Record adapter cassettes](README.md).

## What the recording does

`TestRecordedBot`, in the group you name:

1. sends a text full of characters that mean something in a parse mode
   (`sendMessage`, no `parse_mode`) and keeps the message;
2. sends the same text beside markup in HTML mode and in MarkdownV2 mode, where
   the adapter escapes it, so Telegram's parser is the judge of the escaping;
3. sends a message with an inline keyboard, and a reply to the first one
   (`reply_parameters`);
4. edits the first message (`editMessageText`), and edits it to the same text, which
   Telegram answers "message is not modified" and the adapter takes as applied;
5. reacts with 👍, replaces it with 🔥 and clears it (`setMessageReaction`);
6. deletes the first message (`deleteMessage`), then deletes it again and edits
   it, which Telegram refuses, and sends HTML with a tag it does not take, which
   it refuses to parse.

The refusals matter most: the Bot API reference documents no error description, and
the adapter reads a few phrases within a 400. The recording is what shows whether
they are still what Telegram says.

Updates are not recorded: Telegram sends them to a webhook, and a callback query
needs a person to press a button. Long polling is not recorded either.

## Get a bot

1. In Telegram, open [@BotFather](https://t.me/BotFather) and send `/newbot`. Name
   it and choose a username ending in `bot`. BotFather answers with the bot token,
   such as `123456:ABC-DEF...` ([creating a bot](https://core.telegram.org/bots/tutorial)).
   The number before the colon is the bot's ID.
2. Create a group for the test (a plain group is enough) and add the bot to it.
   The bot needs no administrator rights: it deletes its own messages and reacts
   to any. A bot sets reactions the chat allows, so leave the group's reactions
   on.
3. Find the group's chat ID: in the group, send `/start@<username>` (a bot
   with privacy mode on, as BotFather makes it, sees only commands meant for
   it, mentions and replies to it), then ask Telegram what it has queued for
   the bot, with the bot token exported in your shell
   ([`getUpdates`](https://core.telegram.org/bots/api#getupdates)):

   ```sh
   curl -s "https://api.telegram.org/bot$TELEGRAM_BOT_TOKEN/getUpdates" | grep -o '"chat":{"id":-\?[0-9]*'
   ```

   A group's ID is negative (`-100...` after it becomes a supergroup). Do this
   before the recording and with no webhook set: the two exclude each other.
   Use a bot made for the test: on a bot that another process already
   consumes, `getUpdates` takes updates from that process. Read the ID from
   the Telegram web client's address bar (`#-100...`) instead.

## Record Telegram cassettes

| Variable | Value |
| --- | --- |
| `TELEGRAM_BOT_TOKEN` | The bot token |
| `TELEGRAM_BOT_USERNAME` | The bot's username, without `@` |
| `TELEGRAM_CHAT_ID` | The throwaway group's chat ID |

The test fails in record mode when one is missing.

```sh
cd adapter/chat/telegram
export TELEGRAM_BOT_TOKEN=<token> TELEGRAM_BOT_USERNAME=<username> TELEGRAM_CHAT_ID=<-100...>
XRR_MODE=record go test -count=1 -run TestRecordedBot
```

Cassettes land in `adapter/chat/telegram/testdata/cassettes/telegram`.

## Keep the traffic safe

- Write only to the throwaway group, and keep it to yourself: the bot's messages
  go to everyone in it.
- Telegram takes the bot token in the request path
  (`https://api.telegram.org/bot<token>/<method>`), so a cassette's request URL
  would hold it: the test records it as the placeholder `TELEGRAM_BOT_TOKEN`.
  Do not run a proxy or a logging HTTP client on the recording that writes URLs
  to disk.
- Do not set a webhook, change the bot's privacy mode or its commands while
  recording.
- Afterwards, delete the group, and revoke the token with BotFather (`/revoke`)
  or delete the bot.

## Check the cassettes

What the test removes: the `Authorization` header (the bot sends none), and the
token, the bot's ID, the bot's username and the chat ID wherever they appear
literally (recorded as `TELEGRAM_BOT_TOKEN`, `TELEGRAM_BOT_ID`,
`TELEGRAM_BOT_USERNAME` and `TELEGRAM_CHAT_ID`).

What it does not cover:

- **The Message objects** Telegram returns: the bot's name (`first_name`), the
  group's `title`, and the `chat` object. A `from` names the bot, whose name is
  yours. Replace any you do not want published by hand in the response.
- **Names and IDs of other people** in the group; there should be none.
- **Dates** (`date`, `edit_date`) are the messages' own and stay.

```sh
cd adapter/chat/telegram
grep -rnE '"(first_name|last_name|username|title|language_code)"' testdata/cassettes/telegram
grep -rnF -e "$TELEGRAM_BOT_TOKEN" -e "${TELEGRAM_BOT_TOKEN%%:*}" -e "$TELEGRAM_BOT_USERNAME" -e "$TELEGRAM_CHAT_ID" testdata/cassettes/telegram
```

The bot token is the one thing in a cassette that must never be: a leaked one lets
anyone act as the bot. If the second command finds it, delete the cassettes,
revoke the token and record again.
