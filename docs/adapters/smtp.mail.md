# smtp.mail

`smtp.mail` receives mail as an MX server and sends it through a relay you
name, on one channel per inbox. It has no mailbox: nothing is read back and
nothing is stored at a provider.

| | |
| --- | --- |
| Key | `smtp.mail` |
| Package | `hop.top/uma/adapter/mail/smtp`, in module `hop.top/uma/adapter/mail` |
| Capabilities | `receive`, `send`, `start_conversation`, `reply_recipients` |
| Events | none |
| Identity namespaces | `smtp.mail.address` |
| Sender kinds | person |
| Provider permissions | `smtp.relay` (credential purpose) |
| Message IDs and threading | chosen by the sender; email header threading |
| Reply parent | reported: the first valid `In-Reply-To` msg-id; resolved to a stored message by its `Message-ID` |
| Interactions | none ([interactions](../interactions.md)) |
| Maturity | experimental |

The matrix of all adapters is in [Adapters](README.md).

## Capabilities and operations

| Capability | Behavior |
| --- | --- |
| `receive` | `Instance.Run` serves SMTP on the listener your `Options.Listen` returns. Unavailable until the host supplies `Listen` |
| `send` | `Messages.Reply` composes a `text/plain` message to the route address and submits it to the relay. Unavailable until the host supplies a credential port |
| `start_conversation` | `Conversations.Start` sends to an address that never wrote in |
| `reply_recipients` | `Messages.Reply` with `Recipients` also addresses Cc, Bcc and, replying to all, the answered message's other recipients; see [Reply recipients](#reply-recipients). Available when `send` is |

## Not declared

Reason classes are defined in [Adapters](README.md#why-a-capability-is-not-declared).

| Capability | Reason | Why |
| --- | --- | --- |
| `receive_http` | `provider` | Mail arrives over SMTP, not HTTP; the MX listener is the receiver |
| `reconcile` | `provider` | The adapter keeps no sent copy, so a lost relay answer after the end of `DATA` stays `outcome_unknown` and is never resent. Use [`imap.mail`](imap.mail.md) for a sent-copy lookup |
| `edit` | `provider` | A delivered mail cannot be changed at the recipient |
| `delete` | `provider` | A delivered mail cannot be recalled |
| `react` | `provider` | Mail has no reactions |
| `receipt` | `provider` | Read receipts (RFC 8098 disposition notifications) are optional and unverifiable; none is sent |
| `membership` | `engine` | Mail membership means extra `To`, `Cc` or `Bcc` recipients kept on the conversation for every later send. A reply names its own extra recipients (`reply_recipients`); nothing keeps them on the conversation |
| `history` | `provider` | There is no mailbox to read back |
| `refetch` | `provider` | A mail never changes after delivery |
| `work_item_act` | `provider` | Mail has no work items |
| `internal_note` | `provider` | Mail has no private comments; notes stay in uma |
| `extension_execute` | `provider` | Nothing is stored at a provider to act on; the adapter keeps no copy of a received message to read an unsubscribe header from |
| `extension_query` | `provider` | Nothing is stored at a provider to read |
| `probe_permissions` | `not built` | The relay's `AUTH` and the listener could be probed; nothing does yet |
| `stream` | `provider` | Mail has no streamed messages |
| `send_ephemeral` | `provider` | Mail has no message only one reader sees |
| `signal` | `provider` | Mail has no typing or presence |
| `respond` | `provider` | Mail has no interactions to answer |
| `respond_follow_up` | `provider` | As `respond` |
| `archive` | `provider` | No mailbox is read through the adapter |
| `move` | `provider` | No mailbox is read through the adapter |
| `read_state` | `provider` | No mailbox is read through the adapter |
| `flag` | `provider` | No mailbox is read through the adapter |
| `trash` | `provider` | No mailbox is read through the adapter |
| `raw_read` | `provider` | SMTP submission only sends: it has no access to a mailbox to read a message back from |

## Permissions

- **Provider.** One credential per channel, resolved through `ports.Credentials`
  for purpose `smtp.relay`. The adapter negotiates SASL after TLS (TLS before
  AUTH) through the shared mail auth package; the host owns the credential and
  its refresh.
- **uma.** The receiving principal needs `channel.receive`; replies are
  authorized as `message.reply` and started conversations as
  `conversation.start`.
- **Network.** The host owns the listener, port 25 and process privileges.

## Configuration

`smtp.Config{...}.Extension()` builds the channel configuration (schema
`smtp.mail.config`, version 1). Unknown fields are refused, and every problem is
reported as a validation issue.

| Field | Required | Meaning |
| --- | --- | --- |
| `hostname` | yes | The receiver's greeting and EHLO name, and the `Authentication-Results` authserv-id |
| `recipients` | yes | Accepted addresses, each with the `destination` it maps to. No duplicates; others are refused at `RCPT` with 550 5.1.1 |
| `from` | yes | Bare sender address of outbound mail |
| `from_name`, `subject` | no | Display name and subject of outbound mail, one line |
| `message_id_domain` | no | Right-hand side of outbound `Message-ID`s; default the domain of `from` |
| `max_message_bytes`, `max_recipients` | no | Bounds on one inbound message; defaults 25 MiB and 50 |
| `relay.host` | yes | Relay name or IP address |
| `relay.tls_port`, `relay.starttls_port`, `relay.plain_port` | one required | Implicit TLS is tried first, then STARTTLS. `plain_port` needs `allow_plaintext_loopback` and reaches loopback addresses only |
| `relay.command_timeout_ms`, `relay.data_timeout_ms` | no | Reply timeouts; defaults 5 and 10 minutes |

Host options (`smtp.Options`): `Listen` (the receiver's listener), `TLS`
(STARTTLS for the receiver), `Resolver` (DNS for sender evidence) and
`RootCAs` (to verify the relay).

## Receiving

- Each recipient is checked at `RCPT` against the configured list. The server
  enforces the size and recipient limits.
- Each message is evaluated for sender evidence: SPF, DKIM and DMARC. The
  results are recorded as an `Authentication-Results` value and as structured
  metadata; the DMARC result sets the source assurance. Evidence grades the
  sender and never links contacts. ARC headers are recorded as found,
  unverified.
- The message becomes a text part plus a structured part describing other MIME
  parts. `In-Reply-To` then `References` thread it. An `Auto-Submitted` value
  other than `no`, or a null reverse path, marks an automated message.
- The bytes of the attachments are staged through the engine, in the inbox of
  each destination, and each staged attachment is named in an attachment part
  after the descriptors. A descriptor carries `"staged": true` when its bytes
  are staged. An attachment over the bounds in [Limits](#limits), or refused by
  the engine's attachment checks (size limit, media types), is described only,
  as is every attachment when the host gave the engine no blob store; the mail
  is delivered all the same. A staging failure a retry can fix (the blob store
  is down) answers 451 instead, so the sending server retries and the message
  is never delivered without an attachment the engine would have taken.
  Staging is idempotent per message and destination: a redelivery restages the
  same attachments.
- An attachment's name is the sender's suggestion, so it is reduced to a display
  name before it is recorded, in the descriptor and on the staged attachment:
  only the last path component is kept (`../../etc/passwd` becomes `passwd`),
  control and invisible formatting characters, right-to-left overrides included,
  are dropped, the text is normalized to Unicode NFC, a Windows device name such
  as `CON` gets a leading `_`, and a name over 255 bytes is cut with its
  extension kept. A name with nothing left is recorded as `attachment-N` on the
  staged attachment, N being its position, and left out of the descriptor. The
  staged bytes sit under a key the engine chooses, never one derived from the
  name; a host that writes the name to a file still applies the naming rules of
  its own filesystem.
- A message is identified by its destination and a SHA-256 of the `DATA` bytes
  as sent, never by its `Message-ID`, which the sender chooses and anyone can
  repeat. An MTA retry resends the bytes it queued, so it is one message in
  each destination; two mails that share a `Message-ID` but not bytes are two
  messages, so a forged mail cannot suppress a genuine one. The `Message-ID`
  still threads. A client that composes the message afresh on each attempt, or
  the same mail arriving by a second route that adds a header (a backup MX),
  is a second message.
- The sending server gets 250 only after the sink accepted the message for
  every destination. A destination bound to no inbox is a permanent refusal
  (550); any other sink failure is temporary (451), so the sender retries.

## Identities and start recipients

A sender is keyed in `smtp.mail.address` by its `From` address with the
domain lowercased and the local part exactly as written. RFC 5321 treats the domain as
case-insensitive and reserves the local part's case to the receiving host, so
`Bob@Example.org` and `bob@example.org` are two senders, and `bob@Example.org`
and `bob@example.org` are one. Nothing else is folded: no plus-addressing, no
provider dot rules, no punycode conversion of internationalized domains.

A start follows the same rule. `Conversations.Start` lowercases the domain of a
recipient address before it keys the recipient's identity, opens the
conversation and fingerprints the operation: a start to `Bob@EXAMPLE.org`
claims the `Bob@example.org` identity that Bob's own mail arrives on, the reply
goes to that address, and replaying the start under its operation key with the
domain in another case is the same operation, not a conflict. A start to
`bob@EXAMPLE.org` is a different mailbox and a different identity.

A recipient that is not a bare address (a display name, angle brackets, a list,
surrounding space) is refused at the call, before anything is stored, so the
operation ID stays free. Before the adapter normalized recipients, a start to a
mixed-case domain was accepted and stored, then failed at send because the route
was not in its normalized form. Such a start replayed under its operation key now
fingerprints differently and is refused as a conflict; start again under a new
key.

## Sending

The `Message-ID` derives from the operation and is stable across attempts. A
reply threads with `In-Reply-To`, `References` and a `Re:` subject. Relay
answers map to outcomes: 2xx after `DATA` is `provider_accepted` with the
`Message-ID` as reference; 4xx is transient and retried; 5xx is rejected; a lost
or timed-out answer after the end of `DATA` is `outcome_unknown`.

## Reply recipients

A reply carrying `Recipients` (`model.ReplyRecipients`, `Reply.Recipients`)
addresses more than the route:

| Field | Effect |
| --- | --- |
| `cc` | Added to the `Cc` header and the envelope |
| `bcc` | Added to the envelope only; the message carries no `Bcc` header |
| `reply_all` | The answered message's `To` and `Cc` are added: its `To` recipients join `To`, its `Cc` recipients `Cc`, each minus the channel's `from` address and any address already named. They are read from the parent's `smtp.mail.message` metadata (`headers.to`, `headers.cc`); a reply answering no stored message, or one whose payload holds none, adds nothing |

The route address is always the first `To`. Addresses are compared without
case, and at most 50 in all (`To`, `Cc` and `Bcc` together) are sent; more is a
final `validation` before anything is submitted. The relay takes one
transaction for all of them: a recipient it refuses (5xx at `RCPT`) rejects the
whole send, and nothing is delivered. The capability reports
`{"reply_all": true, "cc": true, "bcc": true, "max_recipients": 50}` as its
limits (`model.RecipientLimits`).

## Limits

| What | Limit |
| --- | --- |
| Recipients per reply | 50 (`To`, `Cc` and `Bcc` together) |
| Inbound message size | `max_message_bytes`, default 25 MiB |
| Recipients per inbound message | `max_recipients`, default 50 |
| A raw header field kept in metadata | cut at 16 KiB |
| ARC header fields recorded | 30 |
| DKIM signatures evaluated | 8; evidence lookups time out after 30 seconds |
| Attachments staged per inbound message | 10, in message order; the others are described only |
| Size of a staged attachment | 25 MiB decoded, and within `max_message_bytes` (see below); a larger one is described only and uses none of the 10 |
| MIME structure of an inbound message | 256 parts (containers included) and 16 levels of nested multipart; past either the message is refused as malformed (550), so the sender is told. A message inside a `message/rfc822` part is one part, never walked |

The message bound counts the raw message, headers and transfer encoding
included, so it dominates the attachment bound at the defaults. A base64 part
(76-character lines, CRLF) is 78 bytes for every 57 decoded: with the default
25 MiB message, the largest attachment is about 18 MiB decoded, and the staged
attachments of one message together stay under that. Only an unencoded part
(`8bit`, `binary`) can reach 25 MiB decoded, and a 25 MiB base64 attachment
needs `max_message_bytes` of about 34.2 MiB or more. A message over the bound is
refused (552), so its attachments never arrive.

## How it is tested

The shared adapter suite runs against an in-process SMTP receiver and relay,
and package tests cover configuration, sender evidence, threading and outcome
mapping. There are no recorded cassettes, and no record path: see
[IMAP and SMTP](../recording/imap-smtp.md).

An opt-in live run (`UMA_LIVE_MAIL=1`, never part of `go test ./...` or
`make check`) runs the `TestLive*` tests with Go's `net/smtp` and a Postfix
container as the sending clients of the receiver, and with Mailpit as the
relay: EHLO and STARTTLS, no AUTH, size and recipient limits, 451 and 550 as
a real MTA retries and bounces them, fan-out, deduplication by bytes, and
submission with AUTH. The page
[IMAP and SMTP](../recording/imap-smtp.md#parity-evidence) says what a green
run proves and what it leaves open: sender evidence against real DNS, an
internet-facing port 25 and the deliverability of outbound mail are
unverified.

## Known limits

- Attachments stage only through an engine with a blob store; without one they
  are described only. The staged bytes are the decoded part; a part inside a
  nested `message/rfc822` is not walked.
- No reconciliation: a lost relay answer is never resolved.
- Text only outbound: no attachments or HTML are sent.
- No `AUTH` on the receiver; STARTTLS only when `Options.TLS` is set.
- ARC is recorded, not validated.
- Experimental: configuration may change ([Compatibility](../compatibility.md)).
