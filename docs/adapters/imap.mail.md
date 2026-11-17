# imap.mail

`imap.mail` is a mailbox adapter, one channel per account: it retrieves mail
over IMAP, sends by SMTP submission, and changes and follows the account's own
view of a conversation (read, star, archive, trash, folder), all with the
channel's one credential.
Use it for providers that offer standard IMAP and SMTP; for Gmail and Microsoft
365 prefer [`google.mail`](google.mail.md) and
[`microsoft.mail`](microsoft.mail.md).

| | |
| --- | --- |
| Key | `imap.mail` |
| Package | `hop.top/uma/adapter/mail/imap`, in module `hop.top/uma/adapter/mail` |
| Capabilities | `receive`, `history`, `send`, `reconcile`, `start_conversation`, `reply_recipients`, `raw_read`, `extension_execute`, `extension_query`, `imap.mail.mailboxes`, `imap.mail.mailbox_create`, `archive`, `move`, `read_state`, `flag`, `trash` |
| Events | none |
| Identity namespaces | `imap.mail.address` |
| Sender kinds | person |
| Provider permissions | `imap`, `smtp.submission` (credential purposes) |
| Message IDs and threading | chosen by the sender; email header threading |
| Reply parent | reported: the first valid `In-Reply-To` msg-id; resolved to a stored message by its `Message-ID` |
| Interactions | none ([interactions](../interactions.md)) |
| Maturity | experimental |

The matrix of all adapters is in [Adapters](README.md).

## Capabilities and operations

| Capability | Behavior |
| --- | --- |
| `receive` | `Instance.Run` retrieves the mailbox read-only with `BODY.PEEK`, waiting in IDLE or polling |
| `history` | `Runtime.Import` searches and fetches the retrieved mailbox read-only, in UID order, by `INTERNALDATE`; received mail only, as the receiver delivers it; a conversation reads the sender's mail ([History](#history)) |
| `send` | `Messages.Reply` composes a `text/plain` message and submits it over SMTP |
| `start_conversation` | `Conversations.Start` sends to an address that never wrote in |
| `reply_recipients` | `Messages.Reply` with `Recipients` also addresses Cc, Bcc and, replying to all, the answered message's other recipients; see [Reply recipients](#reply-recipients). Available when `send` is |
| `raw_read` | `Messages.Raw` reads the message on demand, found by its Message-ID or location as a state change finds it, in a read-only selection with `UID FETCH BODY.PEEK[]`: the whole text, `\Seen` untouched. The size is read first (`RFC822SIZE`) against `max_message_bytes`: a message over it is `permanent` with reason `too_large` and its body is never fetched. Nothing is kept |
| `extension_query` | `Extensions.Query` runs `imap.mail.mailboxes`: `LIST "" "*"`, the account's mailboxes as `model.ProviderMailbox` ([Provider mailboxes](#provider-mailboxes)) |
| `extension_execute` | `Extensions.Execute` runs `imap.mail.mailbox_create`: `CREATE` ([Provider mailboxes](#provider-mailboxes)) |
| `imap.mail.mailboxes` | Gates the list: declared, with `extension_query` |
| `imap.mail.mailbox_create` | Gates the create: declared, with `extension_execute` |
| `reconcile` | Searches the sent-copies mailbox for the operation's `Message-ID`, and reads the mailbox for a provider state change whose answer was lost ([Provider state](#provider-state)). Reported unsupported when `sent.mailbox` is empty |
| `read_state` | `Conversations.SetRead`: `STORE` of `\Seen` on every message of the conversation |
| `flag` | `Conversations.Flag`: `STORE` of `\Flagged`, kind `star` only |
| `archive` | `Conversations.Archive`: move to the mailbox with the `\Archive` special-use attribute (`\All` where there is none, as on Gmail); undone, back to the retrieved mailbox |
| `trash` | `Conversations.Trash`: move to the mailbox with the `\Trash` attribute; undone, back to the retrieved mailbox. Never expunges |
| `move` | `Conversations.Move`: move to a mailbox by name or by role (`inbox`, `archive`, `trash`, `junk`); `folder` shape, so label changes are refused |

Every capability reports unavailable when the host supplies no credential port.

## Not declared

Reason classes are defined in [Adapters](README.md#why-a-capability-is-not-declared).

| Capability | Reason | Why |
| --- | --- | --- |
| `receive_http` | `provider` | IMAP has no webhooks; IDLE and polling are the receiver |
| `edit` | `provider` | A delivered mail cannot be changed at the recipient |
| `delete` | `provider` | A delivered mail cannot be recalled; deleting the mailbox's own copy is not a delete at the peer |
| `react` | `provider` | Mail has no reactions |
| `receipt` | `provider` | Read receipts (RFC 8098 disposition notifications) are optional and unverifiable; none is sent |
| `membership` | `engine` | Mail membership means extra `To`, `Cc` or `Bcc` recipients kept on the conversation for every later send. A reply names its own extra recipients (`reply_recipients`); nothing keeps them on the conversation |
| `refetch` | `provider` | A mail never changes after delivery |
| `work_item_act` | `provider` | Mail has no work items |
| `internal_note` | `provider` | Mail has no private comments; notes stay in uma |
| `probe_permissions` | `not built` | `CAPABILITY`, `LIST` and the submission server's `AUTH` could be probed |
| `stream` | `provider` | Mail has no streamed messages |
| `send_ephemeral` | `provider` | Mail has no message only one reader sees |
| `signal` | `provider` | Mail has no typing or presence |
| `respond` | `provider` | Mail has no interactions to answer |
| `respond_follow_up` | `provider` | As `respond` |

## Permissions

- **Provider.** The channel's credential reference is resolved on every
  connection through the shared mail auth package: purpose `imap` for retrieval
  and `smtp.submission` for submission. Accepted kinds are OAuth 2, password and
  client certificate. TLS comes before AUTH: implicit TLS, then STARTTLS;
  plaintext only to a loopback address the channel opted into.
- **uma.** The receiving principal needs `channel.receive`; replies are
  authorized as `message.reply`, started conversations as `conversation.start`.
  The state verbs report scope `account` and are authorized as
  `conversation.state_account`: they change only this account's view.
- **Mailbox.** The verbs select a mailbox read-write, so the account needs
  write access to the mailboxes it moves from and to; a server that refuses
  ends the operation as rejected, `unauthorized` for `NOPERM`.

## Configuration

`imap.Config{...}.Extension()` builds the channel configuration (schema
`imap.mail.config`, version 1). Unknown fields are refused.

| Field | Required | Meaning |
| --- | --- | --- |
| `address` | yes | The mailbox address, normalized; the `From` of outbound mail |
| `destination` | no | Inbox destination for inbound mail; default `address` |
| `mailbox` | no | Mailbox retrieved; default `INBOX` |
| `imap`, `submission` | yes | Servers: `host`, and at least one of `tls_port`, `starttls_port`, `plain_port` (the last needs `allow_plaintext_loopback`), optional `command_timeout_ms` and `data_timeout_ms` |
| `sent.mailbox`, `sent.saved_by_provider` | no | Where sent copies live. With `saved_by_provider` the server files a copy itself (Gmail, Microsoft 365); otherwise the adapter appends one. Empty keeps no copy and disables reconciliation |
| `from_name`, `subject`, `message_id_domain` | no | Outbound display name, subject and `Message-ID` domain |
| `reconnect_margin_ms` | no | How long before credential expiry a session is replaced; default 5 minutes |
| `idle_restart_ms` | no | Longest IDLE; default 25 minutes, at most 29 (RFC 2177); each IDLE is spread by +-10% so channels do not restart in phase, and never past 29 minutes |
| `poll_interval_ms` | no | Wait on a server without IDLE; default 1 minute, each wait spread by +-10% so channels do not poll in phase |
| `watch_mailboxes` | no | Further mailboxes whose messages the receiver follows ([Provider state](#provider-state)); nothing in them is delivered as new mail |
| `history_mailboxes` | no | Mailboxes a history import walks, in this order, each to its end; empty is `mailbox`. Names like sent, drafts, junk or trash (any case, any parent), `sent.mailbox`, repeats and wildcards are refused at load; a mailbox the server marks `\Sent`, `\Drafts`, `\Junk` or `\Trash` is refused when read |
| `state_sync_interval_ms` | no | On a server without `CONDSTORE`, how often the receiver compares the flags it follows with the server's; default 1 minute |
| `initial_sync_window_ms` | no | How far back the first synchronization reaches; default 1 hour |
| `max_message_bytes` | no | Bytes fetched per message; a larger message is delivered from its header alone. Default 25 MiB |

Host options (`imap.Options`): `RootCAs`, `ReconnectBackoff` (first wait after a
failed connection, doubling to a minute) and `Waiter` (paces timed waits, for
tests) and `Jitter` (source of the +-10% spread on the IDLE restart and the poll
interval, uniform in [0, 1]; nil is the standard library's generator, tests set it
to make the waits deterministic).

## Receiving

The cursor is the mailbox's `UIDVALIDITY` and the highest delivered UID, kept in
the engine's protocol store. Messages above it are delivered in UID order and
the cursor advances after each. Retrieval never changes flags, and flags are
not read evidence.

The instance implements `adapter.ReceiverReporter`: it reports connected while
an authenticated session stands, in IDLE or polling, and not connected when the
session ends, for a planned reconnect before the credential expires too, while
a failed connection is backed off and retried, and when `Run` returns. IMAP has
no session of its own to lose between commands, so a session that died
silently stays reported until the next command, or the IDLE restart, finds it
closed. The report is advisory.

A `UIDVALIDITY` change voids every stored UID: the receiver reports an
`imap.mail.uidvalidity_gap` observation, counts the gap and resynchronizes.
Event IDs never contain a UID, so ingress drops what it already holds. Five
consecutive failed connections end `Run` with the last failure, and the
engine's receiver supervision takes over.

A message can be expunged between the search, the metadata fetch and the body
fetch. Every fetch addresses messages by `UID`, never by sequence number, which
names another message once the mailbox shrinks. A server answers a `UID FETCH`
of an expunged `UID` with `OK` and no data, and may send an `EXPUNGE` during any
`UID` command (RFC 3501 sections 6.4.8 and 7.4.1; RFC 9051 sections 6.4.9 and
7.5.1). An `EXPUNGE` carries a sequence number, not a `UID`, so it is not the
signal. The receiver takes a message for gone when the server has no bytes for
it and a `UID SEARCH` for it finds nothing. Three outcomes follow:

| Outcome | What the receiver does | Operator sees |
| --- | --- | --- |
| Gone: expunged after the search | Skips it, moves the cursor on | `imap.mail.message_vanished` observation |
| There, undeliverable: bytes that do not parse, no bytes | Skips it, moves the cursor on: the bytes of a message never change, so a fetch again fails alike | `imap.mail.message_skipped` observation |
| There, `From` that is not a bare address (`"john doe"@example.org`, a group, unparseable) or absent | As above. Nothing is stored; the sender is never keyed under another identity. See [Mail that cannot be keyed to a sender](#mail-that-cannot-be-keyed-to-a-sender) | `imap.mail.message_skipped` observation |
| Retryable: connection lost, server answers nothing for a message it still holds, sink refusal | Leaves the cursor before the message; the next round or receiver fetches it again | `Run` returns the failure (see below) |

Observations go to the host's observer, carry no message identity and only
count; the receiver does not log. A server that answers nothing for a message
it still holds is asked up to three times in a round, then the round fails with
an unavailable error: `Run` backs off, reconnects, and ends after five
consecutive failed connections for the engine's receiver supervision to restart.
A message the server never answers for stops the mailbox behind it, in the open,
until it leaves the mailbox.

What the receiver cannot know: a message expunged before the search never
existed for it and leaves no trace, whether it arrived and left while the
receiver was stopped or between two rounds. A message expunged after its body
arrived is delivered. A server that frames a truncated body as a complete
message is not told from a short message: it is delivered as fetched, under its
own digest.

A message is identified by its destination and a SHA-256 of the bytes fetched,
never by its `Message-ID`, which the sender chooses and anyone who can mail the
mailbox can repeat. Two mails that share a `Message-ID` but not bytes are two
messages, so a forged mail cannot suppress a genuine one that reuses its id;
the `Message-ID` still threads. A mailbox does not change the text of a message
it holds, so a re-fetch reproduces the bytes and lands once: after a
`UIDVALIDITY` change, after a crash before the cursor advanced, across a
reconnect, or for a copy in the same mailbox. Two cases are second messages:
the same mail reaching the mailbox by two routes, which differ in the
`Received` headers each server adds, and a server migration that rewrites
message bytes. Mail delivered before this key and fetched again after an
upgrade lands once more.

A message over `max_message_bytes` is delivered from its header block alone,
so its identity is a SHA-256 of that block and the size the server reports.
It never equals a message whose whole text is the block, and two with one block
but another size differ. Two with one block and one size but other bodies are
not told apart without fetching the body; the block holds the `Received`
headers the receiving server added, which a sender cannot predict.

Each message is parsed into a text part plus a structured part for other MIME
parts. Threading headers become envelope references and `Auto-Submitted` marks
automated mail. A message whose `From` does not give a bare address, or that
cannot be parsed, is skipped and reported as `imap.mail.message_skipped`. A message past the
MIME structure limits in [Limits](#limits) is not: it is delivered with the header,
the text and the parts read before the limit, and the message payload carries
`parts_omitted: true`. The descriptors of the attachments share the structured
part limit (256 KiB): past what fits, the part's `attachments_omitted` counts
the others.

The bytes of the attachments are staged through the engine and each staged
attachment is named in an attachment part after the descriptors; a descriptor
carries `"staged": true` when its bytes are staged. An attachment over the
bounds in [Limits](#limits), or refused by the engine's attachment checks (size
limit, media types), is described only, as is every attachment when the host
gave the engine no blob store; the message is delivered all the same. A staging
failure a retry can fix (the blob store is down) ends `Run` like a refusal by
the sink: the cursor stays and the next receiver fetches the message again.
Staging is idempotent per message: a redelivery, or a resynchronization after a
`UIDVALIDITY` change, restages the same attachments.

An attachment's name is the sender's suggestion, so it is reduced to a display
name before it is recorded, in the descriptor and on the staged attachment: only
the last path component is kept (`../../etc/passwd` becomes `passwd`), control
and invisible formatting characters, right-to-left overrides included, are
dropped, the text is normalized to Unicode NFC, a Windows device name such as
`CON` gets a leading `_`, and a name over 255 bytes is cut with its extension
kept. A name with nothing left is recorded as `attachment-N` on the staged
attachment, N being its position, and left out of the descriptor. The staged
bytes sit under a key the engine chooses, never one derived from the name; a
host that writes the name to a file still applies the naming rules of its own
filesystem.

A message over `max_message_bytes` is fetched by its header alone and marked
`body_omitted`: its parts are neither described nor staged.

## Provider mailboxes

An IMAP mailbox is a folder (`model.ProviderMailbox`, kind `folder`). Two
extensions list and create them, each gated by the capability of its own name,
so a host reads per channel from `Configuration().Capabilities` whether it can
list (`extension_query` and `imap.mail.mailboxes`) or create (`extension_execute`
and `imap.mail.mailbox_create`).

| | List | Create |
| --- | --- | --- |
| Extension | `imap.mail.mailboxes` (`Extensions.Query`) | `imap.mail.mailbox_create` (`Extensions.Execute`, target the channel) |
| Server commands | `LIST "" "*"` (with `RETURN (SPECIAL-USE)` when offered) | `LIST "" ""` for the delimiter, `LIST "" "*"`, then `CREATE` per missing level |
| Authorized as | `extension.query` | `extension.execute`; a plugin actor's create can be held, like any write |
| Data | none | `{"name": "Clients/Acme"}`; `kind`, when given, is `folder` |
| Result | every mailbox: ID, name, role | the mailbox's reference in the operation's provider references (its name as the server spells it) |

- The ID is the mailbox name as the server spells it; the name is the same with
  the server's hierarchy delimiter replaced by "/", so `Clients.Acme` under a
  "." delimiter reads `Clients/Acme`. A mailbox whose name holds "/" under
  another delimiter cannot be told from a level and is not listed; one that
  cannot exist (`\NonExistent`) is not listed either.
- Roles: `INBOX` (and the retrieved mailbox) `inbox`, `\Archive`, `\Trash`,
  `\Junk`, `\Sent` (`sent`) and `\Drafts` (`drafts`), when the server reports
  special use.
- Create makes each missing parent in turn, since a server need not: a name part
  holding the server's delimiter is refused, and a server without hierarchy
  refuses a path. It is idempotent by name: a mailbox that exists (`INBOX` in
  any case, others byte for byte), or that the server reports `ALREADYEXISTS`
  when created, is the result. The mailbox is not subscribed. A server whose
  personal namespace has a prefix refuses a name outside it: a rejected create,
  with the server's refusal as `permanent`, `unauthorized` or `not_found`.
- A host registers `Factory.Extensions()` with the factory: the adapter
  declares the two capabilities, which need their definitions.

## Provider state

The mailbox is the provider's state of a conversation, kept apart from uma's
workflow state: an archived message is not `resolved`, a folder is not a tenant
label, and the provider's read flag never touches a per-actor read marker.
Reports arrive as `conversation.state` envelopes on `Envelope.State`; the verbs
are the five capabilities above.

**Retrieval stays read-only.** The receiver examines the mailbox and fetches
with `BODY.PEEK`; no retrieval command changes a flag, a mailbox or a message.
Only the state verbs write, and each runs on a session of its own, opened for
the call and selected read-write. The receiver never does.

### Reporting

| Verb | Limits reported | Needs |
| --- | --- | --- |
| `read_state`, `flag` | scope `account`; `flag` kinds `[star]` | nothing beyond `STORE` |
| `move` | scope `account`, shape `folder`, roles the server resolves | `MOVE` (RFC 6851) or `UIDPLUS` (RFC 4315) |
| `archive`, `trash` | scope `account` | as `move`, and a mailbox with `\Archive` (or `\All`) or `\Trash` (RFC 6154) |

A verb is reported unavailable (`unsupported`) when the server cannot do it
safely, and every state capability `unavailable` while the server cannot be
reached. Roles come from the special-use attributes of `LIST`: `inbox` is the
retrieved mailbox, `archive`, `trash` and `junk` the mailboxes with `\Archive`
(else `\All`), `\Trash` and `\Junk`. What a session learned stands for ten
minutes. A destination is a mailbox name or a role; a role the server does not
resolve is refused before any call. No irreversible verb is declared: nothing
expunges for the host, and a trash that is not a move is not offered. Pin is
not a flag kind.

### Fan-out and references

A request names one provider message reference for each message of the
conversation, and the adapter changes each it finds, one command per message.
A reference is a `Message-ID`, which the message starts with, or a location,
which a move gives it:

```
imap:///<mailbox>;UIDVALIDITY=<n>/;UID=<n>;MID=<message-id>
```

A `Message-ID` is looked up in the retrieved mailbox, the sent-copies mailbox,
the watched mailboxes and the `\Archive`, `\Trash` and `\Junk` mailboxes, and
nowhere else. A location is read where it says, while its `UIDVALIDITY` holds,
else it names nothing. A message not found is skipped; none found is
`not_found`, final.

### Move

With `MOVE` one `UID MOVE` moves a message. Without it the adapter copies, and
only when the server confirmed the copy does it set `\Deleted` on that UID and
`UID EXPUNGE` that UID: a plain `EXPUNGE` would remove every message another
client marked `\Deleted`, so a server with neither `MOVE` nor `UIDPLUS` is not
moved on, and a refused `COPY` deletes nothing. (The client library's own
fallback sends the three at once and is not used.)

`COPYUID` gives the message its UID in the destination. The adapter reports the
location as the new reference (`Outcome.ReferenceChanges`); the engine puts it
ahead of the `Message-ID` on the stored message, so the next verb names it and
finds the message where it is. Without a `COPYUID` the adapter looks the
`Message-ID` up in the destination. A move of a message already in the
destination is applied and changes no reference.

### Outcomes and reconcile

A reply that refuses the command before anything changed is final: `NO` with
`TRYCREATE` or `NONEXISTENT` is `not_found`, `NOPERM` `unauthorized`, a
temporary condition retryable, anything else `permanent`. A lost connection
after a command was sent, or a refusal after some messages of the fan-out
changed, is `outcome_unknown`, never resent. `STORE` of a flag is idempotent;
a move is not, since repeating one finds nothing at the source.

`Reconcile` therefore reads the mailbox: a flag change is applied when the
flags are as asked; a move is applied when the messages are in the destination
(with the locations it gave them), not applied when they are still at the
source, and unknown when it is mixed. This is derived from what the commands do:
a message already in the destination before the attempt reads as applied.
Reconcile is available only with `sent.mailbox`. Without it, a change whose
answer was lost stays unknown until the receiver reports the change from the
mailbox, which settles the operation without sending it again.

### Following the mailbox

The receiver follows the messages it delivered: the mailbox each sits in and
its `\Seen` and `\Flagged`, kept in the engine's protocol store (at most 5000
per mailbox, the highest UIDs). Every round it compares them with the server and
reports a change as a `conversation.state` envelope naming the conversation by
the message's `Message-ID`:

| Seen | How | Report |
| --- | --- | --- |
| Flag change | `CONDSTORE` (RFC 7162): `FETCH ... (CHANGEDSINCE n)` returns only what changed since the stored mod-sequence. A server without it is compared in full at most every `state_sync_interval_ms`. IDLE wakes the receiver on another client's flag change | read, star |
| Message left the mailbox | `UID SEARCH` of the UIDs held, then a lookup of its `Message-ID` in the `\Archive`, `\Trash` and `\Junk` mailboxes, the retrieved mailbox and `watch_mailboxes` | found: moved, reported with its mailbox and role (`archived`, `trashed` follow the role); found nowhere: expunged, an `imap.mail.message_expunged` observation and no report |
| Copy of a known message arrives (moved back) | The receiver delivers it as a duplicate | its mailbox, and so unarchive and untrash |

A move to a mailbox the adapter does not look in cannot be told from an
expunge. List such a mailbox in `watch_mailboxes`: it is then searched, and a
message in it is followed from there on (flags, and where it goes next). A
message in `Archive` or `Trash` that is not listed is reported once, when it
arrives, and not followed.

Mail a history read imported is followed the same way. The reader records, for
each envelope it returns, the mailbox, `UIDVALIDITY`, UID, `Message-ID` and flags
in a second ledger (protocol store key `mailstate.import`, or the factory without
a store); the receiver adopts it at the start of every round, so a flag, move or
deletion of imported mail is reported as it is for delivered mail, by the same
`Message-ID`. A message imported and delivered is followed once. UIDs of another
`UIDVALIDITY` than the one followed are dropped. The bound is the same 5000
highest UIDs per mailbox, so a large import does not make the sync rescan the
mailbox: the newest 5000 are followed. It needs a running receiver: the reader
reports nothing itself, and until the receiver runs what it recorded waits in
the store. Each message is recorded under the mailbox it was read from (see
`history_mailboxes`); the receiver reports state only for the retrieved mailbox
and `watch_mailboxes`, so list a mailbox of `history_mailboxes` in
`watch_mailboxes` to have changes to its imported mail reported.

A report covers the one message it observed, so it is never `Complete`: the
adapter cannot know the other messages of the conversation, and the last
message observed decides the conversation's flags. The report is not an
operation: a verb of uma is settled by its own outcome, and a report that shows
what an unsettled operation asked is its echo (it settles it, once).

## Mail that cannot be keyed to a sender

A sender is keyed in `imap.mail.address` by its `From` address, and only by a
bare address that `model.NormalizeEmail` accepts. Valid RFC 5322 mail can have a
`From` that is not one: a quoted local part with a space or an at sign
(`"john doe"@example.org`), a group (`Undisclosed:;`), an address that does not
parse, or no `From` at all. The adapter does not deliver such a message under
another identity: a sender it did not read from the message would be one anyone
can forge or collide with, and the identity is what a reply goes to.

The receiver skips the message, moves the cursor on and reports one
`imap.mail.message_skipped` observation. Nothing of it is stored in uma. The
other mail adapters tell the sender: `smtp.mail` refuses it at SMTP time
(`550 5.1.7`) and `http.mail` answers 400; a receiver that pulls has nobody to
tell. The observation names no message: an observation never carries a provider
identity, so no UID, address or subject is added to it. The reference
OpenTelemetry observer counts the kind as unrecognized, without its name.

**What an operator can do.** The message is still in the mailbox: retrieval
never changes flags, moves or expunges.

| Wish | Possible | How |
| --- | --- | --- |
| Tell that mail was skipped | yes | Count `imap.mail.message_skipped` in the host's observer |
| Find which message | yes, as candidates | Every delivered message records `mailbox`, `uid_validity` and `uid` in its `imap.mail.message` metadata. In one `UIDVALIDITY`, a UID of the mailbox that no delivered message carries, received around the time of the observation, is a candidate. Expunged messages, mail older than the initial synchronization window and copies dropped as redeliveries leave the same gap. Read the candidate in a mail client |
| Re-deliver it by resetting the cursor, or by widening `initial_sync_window_ms` | no | The receiver fetches it again, the bytes are the same, and it is skipped again, each time. The same holds after a `UIDVALIDITY` change |
| Have uma record it | by hand | An operator who vouches for the sender appends a copy to the mailbox with a bare `From` address. That is a new message with its own digest, delivered under the address the operator wrote |
| Be told at the time | no | There is no sender to answer on a pull; a host that must not lose such mail reads the mailbox itself |

The tests pin this behavior end to end: the message is never stored, one
skipped observation, the cursor passes it, mail behind it is delivered, and a
restarted receiver neither delivers nor re-examines it.

## Identities and start recipients

A sender is keyed in `imap.mail.address` by its `From` address with the domain
lowercased and the local part exactly as written. RFC 5321 treats the domain as
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

The `Message-ID` derives from the operation. Submission answers are
definitive: 2xx is `provider_accepted`, 4xx transient, 5xx rejected, and no
answer after the end of `DATA` is `outcome_unknown`, never resent. After
acceptance the adapter appends a copy to `sent.mailbox` unless the provider
saves one. `Reconcile` searches that mailbox for the `Message-ID`; without a
match it reports a safe retry only when the provider saves every submitted
message, and otherwise leaves the outcome unknown.

## Reply recipients

A reply carrying `Recipients` (`model.ReplyRecipients`, `Reply.Recipients`)
addresses more than the route, as on [`smtp.mail`](smtp.mail.md#reply-recipients):
`cc` joins the `Cc` header and the envelope, `bcc` the envelope only, and
`reply_all` adds the answered message's `To` and `Cc` recipients, minus the
channel's `address` and repeats. They are read from the parent's
`imap.mail.message` metadata (`headers.to`, `headers.cc`); a reply answering no
stored message adds nothing. At most 50 addresses in all; the submission server
takes one transaction, and a recipient it refuses rejects the whole send. The
sent copy appended to `sent.mailbox` is the message as sent, without a `Bcc`
header. The capability reports `{"reply_all": true, "cc": true, "bcc": true,
"max_recipients": 50}` as its limits.

## History

`Runtime.Import` reads past mail through `HistoryReader`. It opens a session of
its own, selects the mailbox read-only, fetches with `BODY.PEEK` and closes; it
sets no flag and touches no receiver state; it records what it imported for the receiver to follow ([Following the mailbox](#following-the-mailbox)).

| | |
| --- | --- |
| Scope | the retrieved mailbox (`mailbox`, default `INBOX`) only, unless `history_mailboxes` lists others (`mailbox` itself only if listed): never the sent-copies mailbox or an unlisted one. A conversation import reads the sender's mail: `SEARCH FROM` matches substrings, so the exact sender is checked again on the parsed message |
| Window | `Since <= t < Until` on `INTERNALDATE`, never the `Date` header. `SEARCH SINCE` and `BEFORE` compare dates only, so the search is widened by a day and the exact bounds are judged on the fetched `INTERNALDATE` |
| Order | UID order, which is arrival order in the mailbox; a message copied in from another mailbox arrives when it was copied. With a list, mailbox by mailbox in the listed order, each read to its end: oldest first within a mailbox, not across them (list the mailbox holding parents first). A copy in two listed mailboxes has one event ID, so it is stored once |
| Cursor | opaque. It holds the mailbox, `UIDVALIDITY`, the last UID handled and that message's date; its date must lie in the window and its mailbox is a configured one (a cursor naming another is `Validation`, and that mailbox is never selected); its scope hash covers the mailbox list; a cursor without `UIDVALIDITY` is the start of its mailbox. All checked before any connection |
| Gap | a mail that cannot be delivered (malformed, no usable `From`) is a gap and is never stored; a `UIDVALIDITY` change between two calls is a gap too: the window restarts at the cursor's date under the new value and the engine's event claim absorbs what is read twice |
| Errors | a mailbox that cannot be selected is `Unavailable`; a server with no `UIDVALIDITY` is `Unsupported`; a cursor not issued for the read is `Validation` |
| Attachments | staged through the request's stager (the engine's, set on every import) exactly as the receiver stages them: the same keys and bounds (10 attachments, 25 MiB each), the bytes taken from the fetched message with no further command, descriptors marked `staged`. A message imported and then received holds its bytes once. Without a stager, or without a blob store, they are described only |
| Rate limits | IMAP has none; a failed connection is `Unavailable`, returned for the host to pace |

Envelopes are the receiver's: the same event ID (a digest of the bytes) and
destination. A mail's `Message-ID` is chosen by its sender, so the engine never
skips an imported mail for its ID; a mail already stored, whichever path brought
it first, is skipped by its event ID. A mail with no `Message-ID` imports.
Mail over `max_message_bytes` is read from its header alone, as inbound.

Cassette status: none; IMAP has no record path ([IMAP and SMTP](../recording/imap-smtp.md)).
The in-process server covers the reader; there is no live history test yet, so
behavior against Dovecot or a real provider is unverified.

## Limits

| What | Limit |
| --- | --- |
| Message bytes fetched | `max_message_bytes`, default 25 MiB; a larger message is fetched by its header alone |
| Attachments staged per message | 10, in message order; the others are described only |
| Size of a staged attachment | 25 MiB decoded, and within `max_message_bytes` (see below); a larger one is described only and uses none of the 10 |
| MIME structure of a message | 256 parts (containers included) and 16 levels of nested multipart; past either the message is delivered with what was read before the limit and `parts_omitted` set in the payload. A message inside a `message/rfc822` part is one part, never walked |
| IDLE | restarted at most every 29 minutes |
| Addresses recorded per message | 50 |
| A raw header field kept in metadata | cut at 16 KiB |
| Fetch batch | 50 messages |
| History page | `Limit` envelopes, fetched in batches of 50 |
| Messages followed per mailbox | 5000, the highest UIDs; imported mail shares the bound |
| State verbs | one command per message of the conversation; one session per call |
| Fetches of one body in a round when the server answers nothing for a message it holds | 3 |
| Consecutive failed connections | 5 before `Run` returns |

The message bound counts the raw message, headers and transfer encoding
included, so it dominates the attachment bound at the defaults. A base64 part
(76-character lines, CRLF) is 78 bytes for every 57 decoded: with the default
25 MiB message, the largest attachment is about 18 MiB decoded, and the staged
attachments of one message together stay under that. Only an unencoded part
(`8bit`, `binary`) can reach 25 MiB decoded, and a 25 MiB base64 attachment
needs `max_message_bytes` of about 34.2 MiB or more. A message over the bound is
delivered from its header alone, with no attachments described or staged.

## How it is tested

The shared adapter suite and package tests run against in-process IMAP and SMTP
servers built on the same libraries the adapter uses. There are no recorded
cassettes and no record path: see [IMAP and SMTP](../recording/imap-smtp.md).
The history reader runs against the same servers; a `UIDVALIDITY` change between
two calls is tested where the in-process server can force one.

An opt-in live run (`UMA_LIVE_MAIL=1`, never part of `go test ./...` or
`make check`) runs the `TestLive*` tests against Dovecot over implicit TLS and
STARTTLS and against Mailpit for submission: retrieval and `IDLE`, the cursor,
a forced `UIDVALIDITY` change, header-only delivery, deduplication by bytes,
submission with AUTH, the sent copy and `Reconcile`, and the state verbs and the
flag, move and expunge reports (Dovecot has `MOVE`, `UIDPLUS`, `SPECIAL-USE`
and `CONDSTORE`; the in-process server has no `CONDSTORE`, so the live run is
what exercises `CHANGEDSINCE`). The page
[IMAP and SMTP](../recording/imap-smtp.md#parity-evidence) says what a green
run proves and what it leaves open: real providers (Gmail, Microsoft 365 and
others), their `IDLE` behavior and OAuth 2 against an identity provider are
unverified.

## Known limits

- Attachments of a message over `max_message_bytes` are neither described nor
  staged: the adapter fetches the whole message or its header, never single
  parts (`BODYSTRUCTURE` and `BODY[<part>]` are not used).
- Attachments stage only through an engine with a blob store; without one they
  are described only.
- Text only outbound: no attachments or HTML are sent.
- Reconciliation needs `sent.mailbox`.
- Mail whose `From` is not a bare address is skipped, never stored and not
  recoverable through uma ([details](#mail-that-cannot-be-keyed-to-a-sender)).
- One retrieved mailbox per channel; `watch_mailboxes` only follows.
- Provider state is account-scoped and per message: reports are never
  `Complete`, a move to a mailbox nobody looks in reads as an expunge, and
  `\Flagged` is the only flag (no `important`, no pin). No `QRESYNC`, no
  `X-GM-LABELS`: Gmail's IMAP is read as folders, with `\All` standing for
  archive.
- A state change whose answer was lost is reconciled only with `sent.mailbox`.
- The permission to change a mailbox is the server's; the adapter cannot
  withdraw or probe it, and a refused change is a final rejection.
- Experimental: configuration may change ([Compatibility](../compatibility.md)).
