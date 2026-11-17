# imessagegateway

A reference gateway for the [`relay.messages`](../../docs/adapters/relay.messages.md)
adapter on the `imessage` medium: the service the operator runs between uma
and an iMessage backend. It is part of the root module: the library is
`hop.top/uma/gateway/imessage`, the command `hop.top/uma/cmd/imessagegateway`,
standard library only. uma states no support for any particular iMessage
provider, and this gateway is a starting point, not a supported provider
list. This directory is an unreleased module holding its end-to-end and
recorded tests, which need `hop.top/uma/testing/xrrtest`.

```
uma (relay.messages, medium imessage)
  │  POST /uma/reply   callback, Idempotency-Key, Bearer relay.callback
  │  GET  /uma/lookup  ?operation_id=<key>
  ▼
imessagegateway ── key log (fsync'd, survives restarts)
  │  send                         ▲ webhook (signed, or secret path token)
  ▼                               │
Blooio REST v4  or  BlueBubbles Server on a Mac
```

## What it guarantees uma

The [callback and lookup contracts](../../docs/adapters/relay.messages.md#lookup):

- **The key is recorded before acting.** A callback's `Idempotency-Key` is
  written and synced to disk before the backend is called. A second callback
  with a key the gateway holds is answered from its record and never sent
  again: `200 {"message_id"}` for a sent message, `422` for one the backend
  refused, `502` for one whose outcome is unknown, `409` while the first is
  still in flight.
- **Lookup.** `GET /uma/lookup?operation_id=<key>` answers
  `{"status":"sent","message_id":...}`, `404 {"status":"not_found"}` only for a
  key it holds no record of, and `{"status":"pending"}` (or `"rejected"`) for
  anything else.
- **Released keys.** A key whose send the backend provably never took (the
  backend was unreachable, refused the gateway's credential, had no sender, or
  rate limited it) is released: the callback is answered `503` (or `429` with
  `Retry-After`), and a lookup then says `not_found`, so uma sends again.
- **Unknown stays unknown.** A send whose answer was lost (a timeout, a 5xx)
  is never retried by the gateway. Its lookup answers `pending`, and the uma
  operation stays `outcome_unknown` until the operator settles it from the
  backend's own log.
- **Restarts.** A key that was in flight when the process stopped comes back
  `unknown`, never free to send.
- **Retention.** Keys are kept 30 days after their last change (`-retention`),
  and dropped when the gateway starts after that. uma asks within minutes, but
  a host may call `Operations.Reconcile` later; a key dropped too early turns
  a late `Reconcile` into a second message.

Inbound, the gateway verifies the backend's webhook before parsing it, drops
everything that is no inbound one-to-one message (delivery updates,
reactions, group messages, the account's own outbound echoes), and posts each
message to uma's webhook as `{"from","to","body","message_id","media"}`,
signed with `X-Webhook-Timestamp` (Unix seconds) and `X-Webhook-Signature`
(hex HMAC-SHA-256 of `<timestamp>.<body>` under the relay.webhook secret).

## Backends

| | Blooio | BlueBubbles Server |
| --- | --- | --- |
| Runs | hosted | on a Mac you run, signed in to Messages |
| Send | `POST /v4/messages` `{from, to, text}` with Blooio's own `Idempotency-Key` (24 hours) | `POST /api/v1/message/text` `{chatGuid: "iMessage;-;<handle>", tempGuid, message, method: "apple-script"}` |
| Webhook auth | `X-Blooio-Signature: t=<unix>,v1=<hex HMAC-SHA-256 of "t.body">`, 5 minute window | none: a secret path token, on a private network |
| Recipient | phone number or email | phone number or email, one-to-one only |
| Inbound media | forwarded as URLs | not forwarded (served only behind the server password) |
| Needs | an API key and a line | Full Disk Access, Messages automation; no Private API, SIP stays on |

Not given on either: reactions, typing, read receipts, edits, unsends, group
chats, replies to a specific message. See
[Set up an iMessage channel](../../docs/adapters/relay.messages.md#set-up-an-imessage-channel).

## Install

```sh
go install hop.top/uma/cmd/imessagegateway@<version>
```

`<version>` is a tag of the root module (`v0.1.0-alpha.0` and later), or
`latest`. From a checkout, `go run ./cmd/imessagegateway` at the repository
root runs the same command.

## Run it

```sh
export IMESSAGE_GATEWAY_CALLBACK_SECRET=<relay.callback secret> \
       IMESSAGE_GATEWAY_WEBHOOK_SECRET=<relay.webhook secret> \
       BLOOIO_API_KEY=<key> BLOOIO_WEBHOOK_SECRET=whsec_<...>
imessagegateway -backend blooio -blooio-from +15550100002 \
  -uma-webhook-url https://uma.example.com/uma/imessage -addr 127.0.0.1:8787 -data /var/lib/imessagegateway
```

```sh
export IMESSAGE_GATEWAY_CALLBACK_SECRET=<...> IMESSAGE_GATEWAY_WEBHOOK_SECRET=<...> \
       BLUEBUBBLES_PASSWORD=<server password> IMESSAGE_GATEWAY_WEBHOOK_TOKEN=$(openssl rand -hex 16)
imessagegateway -backend bluebubbles -bluebubbles-url http://192.0.2.10:1234 \
  -bluebubbles-account uma-test@example.com -uma-webhook-url http://192.0.2.20:8080/uma/imessage
```

| Flag | Environment | Meaning |
| --- | --- | --- |
| `-addr` | `IMESSAGE_GATEWAY_ADDR` | Listen address (default `127.0.0.1:8787`) |
| `-backend` | `IMESSAGE_GATEWAY_BACKEND` | `blooio` or `bluebubbles` |
| `-data` | `IMESSAGE_GATEWAY_DATA` | Directory of the key log; one process per directory |
| `-retention` | | How long a key is kept (default `720h`) |
| `-uma-webhook-url` | `IMESSAGE_GATEWAY_UMA_WEBHOOK_URL` | uma's webhook for the channel |
| `-callback-header` | `IMESSAGE_GATEWAY_CALLBACK_HEADER` | Header carrying the callback secret, for `callback.auth: "header"`; empty: `Authorization: Bearer` |
| `-blooio-from`, `-blooio-base-url` | `BLOOIO_FROM`, `BLOOIO_BASE_URL` | Blooio line (E.164) and API base |
| `-bluebubbles-url`, `-bluebubbles-account` | `BLUEBUBBLES_URL`, `BLUEBUBBLES_ACCOUNT` | Server address; the Apple ID the Mac sends from |

Secrets come from the environment or from a file, never from a flag:
`IMESSAGE_GATEWAY_CALLBACK_SECRET`, `IMESSAGE_GATEWAY_WEBHOOK_SECRET`,
`IMESSAGE_GATEWAY_WEBHOOK_TOKEN`, `BLOOIO_API_KEY`, `BLOOIO_WEBHOOK_SECRET`,
`BLUEBUBBLES_PASSWORD`, each also as `-<name>-file` (`-callback-secret-file`,
`-blooio-api-key-file`, ...). None is logged; log lines carry keys and
statuses only.

Routes: `POST /uma/reply`, `GET /uma/lookup`, and the backend's webhook at
`/webhook` (Blooio) or `/webhook/<token>` (BlueBubbles, required token of at
least 16 characters). Blooio only posts to `https://` URLs: put TLS in front
(a reverse proxy, or a tunnel). Bind a BlueBubbles gateway to a private
network (LAN or tailnet): the server's webhooks are unsigned and its API takes
the password in the URL.

## Tests

```sh
go test -race ./gateway/... ./cmd/...   # at the repository root
(cd examples/imessagegateway && go test -race ./...)
```

Unit tests cover the callback and lookup contract, the key log (restart,
retention, a torn last line), the signature uma verifies, and each backend
against a stand-in written to its documentation. The end-to-end tests run uma
in process with the gateway in front of each backend: against the stand-ins
(recorded to a temporary directory, then replayed), and as recorded tests
(`TestRecordedBlooio`, `TestRecordedBlueBubbles`) that skip until someone
records them; see [Record iMessage cassettes](../../docs/recording/imessage.md).
