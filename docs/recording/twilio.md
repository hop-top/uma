# Record Twilio cassettes

The `twilio.sms` and `twilio.whatsapp` adapters share one recorded scenario:
send one message through the Programmable Messaging API, then fetch it back
by its SID. Both need live Twilio credentials: Twilio's test credentials
only create messages and answer `403` to the fetch
([test credentials](https://www.twilio.com/docs/iam/test-credentials)).

Common steps (record, review, replay, clean up) are in
[Record adapter cassettes](README.md).

## Get an account and credentials

1. Sign up for a Twilio account. A trial account comes with a Twilio phone
   number and trial credit, and can message only verified recipients; your
   sign-up number is verified already
   ([trial accounts](https://www.twilio.com/docs/usage/tutorials/how-to-use-your-free-trial-account)).
   Trial messages start with a "Sent from a Twilio trial account" prefix.
2. Optional, to keep recordings apart from other work: create a subaccount
   and record with its credentials. Each subaccount has its own Account SID
   and Auth Token
   ([subaccounts](https://www.twilio.com/docs/iam/api/subaccounts)).
3. Note the **Account SID** and **Auth Token** from the Console.

The recorded tests authenticate with the Account SID and its Auth Token.
API keys, which Twilio prefers for production
([API keys](https://www.twilio.com/docs/iam/api-keys)), are accepted by the
adapter but not by these tests as written. Rotate the Auth Token after
recording if it was shared beyond your shell.

## Record Twilio SMS cassettes

| Variable | Value |
| --- | --- |
| `TWILIO_ACCOUNT_SID` | Account (or subaccount) SID, `AC...` |
| `TWILIO_AUTH_TOKEN` | Its Auth Token |
| `TWILIO_FROM` | Your Twilio phone number, E.164 (`+1...`) |
| `TWILIO_TO` | A phone you own, E.164; on a trial account, a verified number |

```sh
cd adapter/messaging/twilio
export TWILIO_ACCOUNT_SID=AC<...> TWILIO_AUTH_TOKEN=<auth token> TWILIO_FROM=+<twilio number> TWILIO_TO=+<your phone>
XRR_MODE=record go test -count=1 ./sms -run 'TestRecordedSendAndFetch$|TestRecordedReconcileFindsSent$'
```

Cassettes land in `adapter/messaging/twilio/sms/testdata/cassettes/twilio`.
`TestRecordedSendAndFetch` sends one SMS and fetches it;
`TestRecordedReconcileFindsSent` sends one more, waits until the account's
message list shows it (seconds to a minute) and reconciles it. Two SMS are
sent. The `TestRecordedAnswers` cases send nothing to a phone: they record
a refused create (a message to the sender itself), an empty listing, an
unknown message and a wrong auth token.

The tests' scripted Twilio builds each accepted create answer from the
recorded `send-and-fetch` create answer, so a re-recording changes what it
answers; `TestFakeAcceptedCreateMatchesRecording` fails if its answer stops
matching the recording.

US carriers require A2P 10DLC registration for application SMS from US
10-digit numbers, and toll-free numbers need verification; both need a paid
account
([A2P 10DLC](https://www.twilio.com/docs/messaging/compliance/a2p-10dlc)).
Check what your trial number may send in your country before recording.

## Record Twilio SMS history

`TestRecordedHistory` records the `history` read. It needs an incoming SMS,
so you reply from your phone while it runs. Use the same variables as above:
`TWILIO_TO` is your phone, `TWILIO_FROM` the Twilio number.

The account's message list may hold other people's messages. The test reads
only the conversation between your two numbers: the listing names both
(`To` the Twilio number, `From` your phone) and a `DateSent` lower bound on
the day the test started. Its transport refuses a list page that holds any
other number before anything is written, and the test then fails.

1. Run, with the variables exported:

   ```sh
   cd adapter/messaging/twilio
   XRR_MODE=record go test -count=1 -v ./sms -run 'TestRecordedHistory$'
   ```

2. The test sends one SMS to `TWILIO_TO` ("recorded test message: please reply
   recorded test reply"), then logs `reply "recorded test reply" from ... now`.
3. From the phone, reply to the Twilio number with exactly
   `recorded test reply` (a capital first letter is fine), once. The test
   polls the message list without recording until the reply shows (seconds to
   a minute; it gives up after 5 minutes), then records one history read and
   checks it holds exactly your reply.
4. The test fails, and you start again, if the window holds any other message
   from your phone after the test started: send nothing else meanwhile.

Cassettes land in
`adapter/messaging/twilio/sms/testdata/cassettes/twilio/history-reply`: the
send, the history listing and `attempted_at`, the test's start time, which
replay reads back as the window's start. If the test fails, delete that
directory before recording again:

```sh
rm -r adapter/messaging/twilio/sms/testdata/cassettes/twilio/history-reply
```

Before committing, review the listing cassette (the `GET` whose URL holds
`DateSent`) by hand, in addition to the grep under
[Check the cassettes](#check-the-cassettes): its `messages` must hold only
your reply, with `from` `TO_NUMBER` and `to` `FROM_NUMBER`, and no other
number, body or name anywhere in the file. The listing covers the whole UTC
day; the test drops every message but its own from it before writing, and
the review confirms that.

Clean up: delete the sent message and your reply from the account's message
log (see [Keep the traffic safe](#keep-the-traffic-safe)), and delete the
reply on the phone if you want. The message SIDs are in the test's cassettes
(`sid`), or list them with the same filters:

```sh
curl -s -u "$TWILIO_ACCOUNT_SID:$TWILIO_AUTH_TOKEN" -G \
  "https://api.twilio.com/2010-04-01/Accounts/$TWILIO_ACCOUNT_SID/Messages.json" \
  --data-urlencode "To=$TWILIO_FROM" --data-urlencode "From=$TWILIO_TO" \
  --data-urlencode "DateSent>=$(date -u +%Y-%m-%d)"
```

## Record Twilio WhatsApp cassettes

The Twilio WhatsApp Sandbox needs no WhatsApp sender of your own
([sandbox](https://www.twilio.com/docs/whatsapp/sandbox)):

1. In the Console, open the WhatsApp Sandbox and note its number
   (`+14155238886`) and your join code.
2. From your own WhatsApp, send `join <code>` to the sandbox number. This
   joins you for 3 days and opens a 24-hour window for free-form messages.
3. Record either a free-form message, inside that window, or a template:
   the sandbox only sends Twilio's pre-approved templates, so set
   `TWILIO_CONTENT_SID` to one of their Content SIDs, not a template of your
   own
   ([try out WhatsApp](https://www.twilio.com/docs/usage/trials/try-out-whatsapp)).

| Variable | Value |
| --- | --- |
| `TWILIO_ACCOUNT_SID`, `TWILIO_AUTH_TOKEN` | As for SMS |
| `TWILIO_FROM` | The sandbox number, `+14155238886` |
| `TWILIO_TO` | The recipient the cassettes replay with: the WhatsApp user ID Twilio gives for you (`CC.<id>`, such as `CA.<digits>`: your country code, a dot and the ID; the `From` of your message in the Console's message log, or its `ExternalUserId`); a phone number in E.164 records too, but then replay misses until the test's recipient is a number again |
| `TWILIO_CONTENT_SID` | Optional: a sandbox template's Content SID; unset sends free-form text |

```sh
cd adapter/messaging/twilio
export TWILIO_FROM=+14155238886 TWILIO_TO=+<your WhatsApp number>
XRR_MODE=record go test -count=1 ./whatsapp -run TestRecordedSendAndFetch
```

A WhatsApp sender of your own records the same way: message it from your
WhatsApp first, which opens the 24-hour window, and set `TWILIO_FROM` to its
number. Twilio names you by a WhatsApp user ID (`CC.<id>`), not your
number, in the webhook and in its message log when WhatsApp hides your
number; the adapter reports it as `external_user_id`
([user IDs](../adapters/twilio.whatsapp.md#user-ids)). Twilio sends to that
ID as it does to a number.

Cassettes land in
`adapter/messaging/twilio/whatsapp/testdata/cassettes/twilio`. A recipient
that has not joined the sandbox fails with error `63015`.

## Keep the traffic safe

- Send only to phones you own; a trial account enforces this for SMS.
- Do not change numbers, messaging services or webhook URLs while
  recording.
- Delete the recorded messages from the account's message log if you do not
  want their bodies kept; deletion cannot be undone
  ([message resource](https://www.twilio.com/docs/messaging/api/message-resource)):

  ```sh
  curl -s -X DELETE -u "$TWILIO_ACCOUNT_SID:$TWILIO_AUTH_TOKEN" \
    "https://api.twilio.com/2010-04-01/Accounts/$TWILIO_ACCOUNT_SID/Messages/<SM sid>.json"
  ```

## Check the cassettes

What the tests remove: the `Authorization` header (basic auth), and the
Account SID and both phone numbers where they appear (recorded as
`ACCOUNT_SID`, `FROM_NUMBER` and `TO_NUMBER`). The send request is
form-encoded, so its body and the list URLs carry the numbers as `%2B1...`
(WhatsApp's as `whatsapp%3A%2B1...`): both tests also replace those encoded
forms (`FROM_NUMBER_QUERY`, `TO_NUMBER_QUERY`). The SMS tests also drop
every message but their own (the test message and the history reply) from a
recorded message list: the list covers the account's whole history to the
recipient, and the history listing a whole day of your phone's messages.

Grep for the digits as well, in case a form slipped through:

```sh
cd adapter/messaging/twilio
grep -rnF -e "$TWILIO_ACCOUNT_SID" -e "${TWILIO_FROM#+}" -e "${TWILIO_TO#+}" -e "$TWILIO_AUTH_TOKEN" sms/testdata whatsapp/testdata
```

A match in a recorded request also makes replay with the variables unset
miss: the test needs a placeholder for that form before its cassettes can
be committed. The sandbox number is public and may stay.
