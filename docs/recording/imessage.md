# Record iMessage cassettes

`relay.messages` is the operator's own relay, so the adapter has nothing to
record. What can be recorded is the path through the reference gateway
(`gateway/imessage`, tested from `examples/imessagegateway`): uma (in process)
posts a first message to the gateway's callback, the gateway sends it through an iMessage backend, and the
recipient's reply comes back through the backend's webhook into the same uma
conversation. Each backend has its own recorded test:

| Backend | Test | Cassettes | Captured reply webhook |
| --- | --- | --- | --- |
| Blooio (hosted) | `TestRecordedBlooio` | `examples/imessagegateway/testdata/cassettes/blooio` | `examples/imessagegateway/testdata/webhooks/blooio/reply.json` |
| BlueBubbles Server (your Mac) | `TestRecordedBlueBubbles` | `examples/imessagegateway/testdata/cassettes/bluebubbles` | `examples/imessagegateway/testdata/webhooks/bluebubbles/reply.json` |

Both skip until their cassettes exist. You need an iMessage account to send
from (a Blooio line, or an Apple ID signed in to Messages on a Mac) and a
device you hold that receives iMessage on the recipient handle, to reply from.

Common steps (record, review, replay, clean up) are in
[Record adapter cassettes](README.md). The gateway is a reference, not a list
of supported providers: uma states no support for any particular iMessage
provider ([relay.messages](../adapters/relay.messages.md#mediums-and-identities)).

## What the recording does

Each test, against its backend:

1. starts a conversation with `IMESSAGE_TO` (`Conversations.Start`) with the
   text "uma recorded test message: please reply recorded test reply". The
   relay posts it to the gateway's callback, and the gateway sends it through
   the backend: **one real iMessage to `IMESSAGE_TO`**;
2. checks the operation settled `provider_accepted` with the backend's id;
3. posts the same callback again with the same `Idempotency-Key`, which the
   gateway answers with the same id without calling the backend, and asks the
   gateway's lookup, which answers `sent`;
4. logs `reply "recorded test reply" to the message ... now` and waits up to 3
   minutes for the backend's webhook carrying your reply. **Reply from the
   recipient's device with exactly `recorded test reply`, once.** The gateway
   verifies the webhook, forwards it to uma, and the test writes the webhook
   body, scrubbed, to `testdata/webhooks/<backend>/reply.json`;
5. checks the reply is a `message.received` from the recipient's
   `relay.imessage` identity in the conversation step 1 started.

Replay posts the captured webhook to the gateway instead of waiting, so the
same assertions run with no network.

The backend must reach the gateway's webhook while the test runs. The test
listens on `IMESSAGE_GATEWAY_LISTEN` (for example `127.0.0.1:8787`) and serves
the webhook at `/webhook` (Blooio) or `/webhook/<BLUEBUBBLES_WEBHOOK_TOKEN>`
(BlueBubbles). Blooio needs a public `https://` URL: put a tunnel in front of
the listen address (`ngrok http 8787`, `tailscale funnel 8787`, or
`cloudflared tunnel --url http://127.0.0.1:8787`). BlueBubbles posts from your
Mac, so a LAN or tailnet address of the machine running the test is enough;
listen on that address instead of `127.0.0.1`.

## Record Blooio cassettes

### Get an account

1. Sign up at [blooio.com](https://blooio.com). The free trial covers a few
   messages from a trial line, enough for one recording
   ([pricing](https://blooio.com/integrations/api/pricing)); a trial line can
   answer `402` once its limit is reached.
2. In the dashboard ([app.blooio.com](https://app.blooio.com)), create an API
   key. It is a bearer key (`api_...`); keep it in your secret manager.
3. Note your line's number, E.164 (`+1...`): `GET /v4/me/numbers` lists the
   numbers the key may send from.

   ```sh
   curl -s https://api.blooio.com/v4/me/numbers -H "Authorization: Bearer $BLOOIO_API_KEY"
   ```

### Register the webhook

Start the tunnel first, then register its URL with `/webhook` appended
([receive webhooks locally](https://docs.blooio.com/guides/webhooks-local)).
The answer carries the signing secret once:

```sh
curl -s -X POST https://api.blooio.com/v4/webhooks \
  -H "Authorization: Bearer $BLOOIO_API_KEY" -H 'Content-Type: application/json' \
  -d '{"url": "https://<your tunnel host>/webhook"}'
```

Note `id` (`wh_...`) for the cleanup and `signing_secret`
(`whsec_...`): the gateway checks `X-Blooio-Signature` with it
([webhook signatures](https://docs.blooio.com/guides/webhook-signatures)).

### Record

| Variable | Value |
| --- | --- |
| `BLOOIO_API_KEY` | The API key |
| `BLOOIO_FROM` | The line's number, E.164 |
| `BLOOIO_WEBHOOK_SECRET` | The webhook's `signing_secret` |
| `IMESSAGE_TO` | The recipient: a phone number in E.164 or an Apple ID email address, on a device you hold |
| `IMESSAGE_GATEWAY_LISTEN` | The address the test listens on, behind the tunnel, such as `127.0.0.1:8787` |

The test fails in record mode when one is missing. Use the handle exactly as
Blooio reports it in its webhooks: E.164 for a number.

```sh
cd examples/imessagegateway
export BLOOIO_API_KEY=<key> BLOOIO_FROM=+<line number> BLOOIO_WEBHOOK_SECRET=whsec_<...> \
  IMESSAGE_TO=<+number or email> IMESSAGE_GATEWAY_LISTEN=127.0.0.1:8787
XRR_MODE=record go test -count=1 -v -run 'TestRecordedBlooio$' .
```

Reply when the test asks. Replay, in a fresh shell without the variables:

```sh
cd examples/imessagegateway
go test -count=1 -v -run 'TestRecordedBlooio$' .
```

### Clean up

Delete the webhook, and delete the conversation on your device if you want:

```sh
curl -s -X DELETE https://api.blooio.com/v4/webhooks/<wh_id> -H "Authorization: Bearer $BLOOIO_API_KEY"
```

Revoke the API key in the dashboard if it was made for the recording, and
stop the tunnel.

## Record BlueBubbles cassettes

BlueBubbles Server runs on a Mac signed in to Messages with the Apple ID that
sends. The gateway uses its REST API without the Private API: sends go through
AppleScript, which works with System Integrity Protection on.

What works without the Private API, and what does not:

- Text to a one-to-one chat, `iMessage;-;<handle>`: when Messages has no chat
  with the handle yet, the server falls back to a script that addresses the
  handle directly, so a first message works too. Group chats, reactions,
  replies to a message, effects, subjects, edits and unsends need the Private
  API (`POST /api/v1/chat/new` too); the gateway uses none of them.
- The server needs a `tempGuid` for an AppleScript send and refuses a second
  send with the same one while the first is queued; the gateway passes the
  relay's `Idempotency-Key`. It is no lasting deduplication: the gateway's own
  key log is.
- Webhooks are not signed. The gateway accepts them only on a URL holding a
  secret token, and the Mac and the gateway should talk over a private network
  (LAN or tailnet), never the internet.

### Set up the server

1. Use a Mac you can dedicate to this, signed in to Messages with the sending
   Apple ID, and check that Messages itself can send an iMessage (blue bubble)
   to `IMESSAGE_TO`.
2. Download the `.dmg` from the
   [latest GitHub release](https://github.com/BlueBubblesApp/bluebubbles-server/releases/latest)
   and open it with right click > Open: the app is unsigned and not notarized,
   so Gatekeeper blocks a plain double click. The Homebrew cask is disabled
   since 2026-09-01 for failing Gatekeeper checks; do not use it.
3. Walk through the setup
   ([manual setup](https://docs.bluebubbles.app/server/installation-guides/manual-setup)):
   grant **Full Disk Access** (the server reads the Messages database). The
   Notifications step is for BlueBubbles' own phone apps; the gateway does not
   need it.
4. Allow the server to control **Messages** and **System Events** when macOS
   asks (System Settings > Privacy & Security > Automation): AppleScript sends
   need it
   ([can't send messages](https://docs.bluebubbles.app/server/troubleshooting-guides/cant-send-messages-from-bluebubbles)).
5. Set a strong server password in the Connection step and save it.
6. Note the server's local address and port (default `1234`): for example
   `http://<mac LAN or tailnet address>:1234`. A proxy (Cloudflare, ngrok,
   zrok) is not needed for the recording and exposes the server to the
   internet; leave it off or set it to the LAN/dynamic DNS option. The REST
   API takes the password as a query parameter, so keep it on a private
   network.
7. Check the API:

   ```sh
   curl -s "$BLUEBUBBLES_URL/api/v1/ping?password=$BLUEBUBBLES_PASSWORD"
   ```

### Register the webhook

Pick a random token (`openssl rand -hex 16`). In the server, open
**API & Webhooks**, choose **Manage > Add Webhook**, enter
`http://<IMESSAGE_GATEWAY_LISTEN>/webhook/<token>` and subscribe it to
**New Messages**
([webhooks](https://docs.bluebubbles.app/server/developer-guides/simple-web-server-for-webhooks)).
The listen address must be one the Mac reaches: the LAN or tailnet address of
the machine running the test.

### Record

| Variable | Value |
| --- | --- |
| `BLUEBUBBLES_URL` | The server's address, such as `http://192.0.2.10:1234` |
| `BLUEBUBBLES_PASSWORD` | The server password |
| `BLUEBUBBLES_ACCOUNT` | The Apple ID address Messages sends from (the "to" of inbound messages) |
| `BLUEBUBBLES_WEBHOOK_TOKEN` | The webhook's random token |
| `IMESSAGE_TO` | The recipient handle on a device you hold, E.164 or email; not the Mac's own Apple ID |
| `IMESSAGE_GATEWAY_LISTEN` | The address the test listens on, the one in the webhook URL |

```sh
cd examples/imessagegateway
export BLUEBUBBLES_URL=http://<mac>:1234 BLUEBUBBLES_PASSWORD=<password> \
  BLUEBUBBLES_ACCOUNT=<apple id> BLUEBUBBLES_WEBHOOK_TOKEN=<token> \
  IMESSAGE_TO=<+number or email> IMESSAGE_GATEWAY_LISTEN=<this machine's LAN or tailnet address>:8787
XRR_MODE=record go test -count=1 -v -run 'TestRecordedBlueBubbles$' .
```

Reply when the test asks; an AppleScript send can take several seconds. Then
replay in a fresh shell:

```sh
cd examples/imessagegateway
go test -count=1 -v -run 'TestRecordedBlueBubbles$' .
```

### Clean up

Delete the webhook in **API & Webhooks**, delete the test conversation in
Messages on the Mac and on your device if you want, and change the server
password if it was shared beyond your shell.

## Keep the traffic safe

- Send only to a handle you hold. Hosted relays and BlueBubbles send from a
  real Apple ID under Apple's iCloud terms, which restrict automated and
  unsolicited messages
  ([operator note](../adapters/relay.messages.md#mediums-and-identities)): one
  recording is one message and one reply.
- Do not change the webhook, the line or the server settings while recording.
- If a recording fails, delete the backend's cassette directory and its
  `testdata/webhooks/<backend>` directory before recording again.

## Check the cassettes and the reply webhook

What the tests remove:

- **Credentials.** The `Authorization` header (Blooio's API key) is never
  recorded. The BlueBubbles password, which travels in the query, is recorded
  as `BLUEBUBBLES_PASSWORD` or redacted, and the server's host as
  `BLUEBUBBLES_HOST`. Signing secrets and the webhook token never reach a
  file.
- **Identifiers.** `IMESSAGE_TO` is recorded as `RECIPIENT_HANDLE` (its digits
  as `RECIPIENTDIGITS`), `BLOOIO_FROM` as `SENDER_NUMBER` (`SENDERDIGITS`),
  `BLUEBUBBLES_ACCOUNT` as `BLUEBUBBLES_ACCOUNT`.
- **The reply webhook** is rewritten before it is written: the recipient, the
  line or account (any letter case) become the replay values `+15550100001`,
  `+15550100002` and `uma-test@example.com`, `name`, `displayName`,
  `first_name` and `last_name` become empty and `organization_id` becomes
  `org_test`. It is written indented, for review.

What they do not cover, to check by hand:

- **Blooio**: the send answer and the webhook carry `chat_id`, `contact_id`,
  `channel_id` and message ids, which are Blooio's own and stay; a `contact`
  object may name a contact (blanked) and attachments may carry URLs (there
  should be none: reply with text only).
- **BlueBubbles**: the send answer is the whole serialized message
  (`attributedBody`, `dateCreated`, `handle`, `chats` with `chatIdentifier`),
  and the webhook too. Look for your own name, other handles, the Mac's name,
  and a `country` field; replace any you do not want published by hand in the
  response or the fixture, never in a recorded request.

```sh
cd examples/imessagegateway
for v in "$IMESSAGE_TO" "${IMESSAGE_TO#+}" "$BLOOIO_FROM" "${BLOOIO_FROM#+}" "$BLOOIO_API_KEY" "$BLOOIO_WEBHOOK_SECRET" \
         "$BLUEBUBBLES_PASSWORD" "$BLUEBUBBLES_ACCOUNT" "$BLUEBUBBLES_WEBHOOK_TOKEN" "${BLUEBUBBLES_URL#http://}"; do
  [ -n "$v" ] && grep -rniF -- "$v" testdata && echo "found: recorded value above"
done
grep -rnE '"(name|displayName|first_name|last_name|country)"' testdata
```

A match for a credential means: delete the cassettes and the fixture, rotate
the credential, record again.
