# Record Gmail cassettes

The `google.mail` adapter's recorded test runs against a real Gmail
mailbox through the Gmail API with an OAuth access token. You need a Google
Cloud project with a Desktop OAuth client, one browser consent to get a
refresh token, and a fresh access token for each recording session.

Common steps (record, review, replay, clean up) are in
[Record adapter cassettes](README.md).

## What the recording does

`TestRecordedGmail` opens the channel on the mailbox and:

1. sends one text message to the peer address (`users.messages.send`);
2. searches Sent for it by Message-ID (`users.messages.list` with
   `rfc822msgid:`);
3. stars the sent message and unstars it again: two `users.messages.modify`
   calls on it (the `google.mail.labels` effect, `add` then `remove` of the
   system label `STARRED`), so the mailbox ends as it was and no label is
   created;
4. runs one receiver round: reads the profile, lists the inbox messages
   received in the hour before the test's fixed clock (`recordedNow` in
   `recorded_scrub_test.go`, 2001-01-03 12:00 UTC; the query is
   `after:<recordedNow - 1h> before:<recordedNow>`, both in epoch seconds) and
   fetches each one with `format=raw`; then reads the thread of every message
   it brought in (`users.threads.get`, `format=metadata`), the state the full
   synchronization reports;
5. runs the conversation state verbs on the sent message's thread, each as
   `users.messages.get` (`format=minimal`, to find the thread) and one
   `users.threads` call, and each undone by its pair so the thread ends as a
   send leaves it (in Sent, read, out of the inbox, not starred, not in the
   trash):
   - unarchive, archive: `threads.modify` adding, then removing `INBOX`;
   - unread, read: `threads.modify` adding, then removing `UNREAD`;
   - flag then unflag `star`, and `important`: `threads.modify` with
     `STARRED`, then `IMPORTANT`;
   - move: `threads.modify` adding, then removing the system label
     `CATEGORY_UPDATES` (no label is created);
   - trash, untrash: `threads.trash`, `threads.untrash`;
6. reads the thread back as a reconcile does (`users.threads.get`,
   `format=minimal`, after the untrash) and archives a message ID Gmail does
   not hold (`0000000000000000`): the recorded `404` of
   `users.messages.get`, a final `rejected` with `not_found`;
7. runs a second receiver round: `users.history.list` from the cursor of the
   first round with `historyTypes` `messageAdded`, `labelAdded`,
   `labelRemoved` and `messageDeleted`, scoped to `labelId=SENT` (see below),
   then `users.threads.get` (`format=metadata`) of the sent message's thread.
   The label records of step 5 are the own-change echo, and the test requires
   at least one `conversation.state` envelope naming the sent message;
8. stores a history ID Gmail no longer holds (`1`) and runs a third round:
   `users.history.list` answers the recorded `404`, and the receiver
   recovers with a full synchronization (profile, the window's listing, a
   `format=raw` fetch of each message, the thread reads), one counted resync.

The history read of step 7 is scoped to the `SENT` label by the test
(`historyScope` in `recorded_check_test.go`; the adapter's own request has no
`labelId`). Unscoped, `history.list` answers every change of the whole mailbox
since the cursor, and the receiver fetches each message that arrives in the
inbox meanwhile: mail that came in while the scenario ran would be read and
recorded. The scenario's own message keeps `SENT` throughout, so every label
record of the state verbs is still listed.

The window is fixed, so a replay and a recording made later list the same
messages whatever arrives after it. The test adds the `before:` operator
itself (`syncBound` in `recorded_check_test.go`): the adapter's own query
has the lower bound only. The window lies in 2001, long before any real mail a
mailbox can hold, on a day apart from the history window, so it holds only
messages placed there for the recording (see
[Seed the sync window](#seed-the-sync-window)). Whatever the window holds is
recorded in full. The clock is a constant, never the wall clock, which no
replay could match; the sent message carries its date (2001) in the `Date`
header, but Gmail dates it by delivery, so it is never in the window.

The recorded channel sets a fixed `message_id_domain`
(`msgid.uma.invalid`). Without it the adapter derives the Message-ID domain
from the mailbox address, and the Sent search in step 2 would carry the
mailbox's domain in its `rfc822msgid:` query.

## Record the history cassettes

`TestRecordedGmailHistory` reads the mailbox's past received mail through
`HistoryReader` over the fixed window 2001-01-01 00:00 to 2001-01-02 00:00
UTC (see `recorded_history_test.go`): `users.messages.list` per day slice
(`labelIds=INBOX`, `q=after:<s> before:<s>`), `users.messages.get` with
`format=minimal` for each listed message's date, then `format=raw` for each
envelope. It sends and changes nothing.

The window lies before any real mail a mailbox can hold, so it holds only
messages placed there for the recording. Before recording, insert three
messages from the peer into the mailbox with `users.messages.insert`
(`internalDateSource=dateHeader`, `labelIds` `INBOX` and `UNREAD`), dated
09:00, 10:00 and 11:00 that day, the second with a small attachment; that
needs the `gmail.insert` scope on the token, `gmail.readonly` for the read.
First list the window (`q=after:978307200 before:978393600`,
`includeSpamTrash=true`) and check it is empty. Inserted messages carry the
inserting Cloud project's number in their `Received` header: set
`GMAIL_INSERT_PROJECT` to that number and the scrubber replaces it.

```sh
export GMAIL_ADDRESS=<mailbox> GMAIL_PEER=<peer> GMAIL_INSERT_PROJECT=<project number>
export GMAIL_ACCESS_TOKEN=<token with gmail.readonly>
cd adapter/mail/google
XRR_MODE=record go test -count=1 -run '^TestRecordedGmailHistory$' ./...
```

Cassettes land in `adapter/mail/google/testdata/cassettes/gmail-history`;
the scrub and residue checks are those of the main recording. The recorded
listing answers newest first; the reader sorts each slice and relies on no
order.

## Record the reply draft cassettes

`TestRecordedGmailReplyDraft` records the `google.mail.reply_draft` effect in
a scenario of its own, so the cassettes of the main scenario stay valid. It
needs no seeded window and the scopes of the main recording (`gmail.modify`
covers every draft call). It:

1. sends one message to the peer (`users.messages.send`), the parent of the
   reply;
2. creates a draft on it: `users.messages.get` (`format=metadata`, the
   parent's `Subject`, `Message-ID` and `References`), then
   `users.drafts.create` with `message.raw` and `message.threadId`;
3. lists Drafts for the operation's `Message-ID`
   (`users.drafts.list`, `q=rfc822msgid:<id>`), as `Reconcile` does after a
   lost create answer; found and not yet found (search lag) are both
   accepted;
4. replaces the draft's content: the ownership read (`users.drafts.get`,
   `format=metadata`, `Message-ID` and `To`, then the parent's
   `users.messages.get` again), then `users.drafts.update`;
5. deletes the draft (ownership read, then `users.drafts.delete`), then
   deletes it again: the recorded `404` of the ownership read of a draft that
   is gone, which the effect treats as
   deleted.

The mailbox ends as the main scenario leaves it: the sent message stays (in
Sent, and in Inbox if the peer is the mailbox itself), the draft is gone. If
the run stops between steps 2 and 5, delete the leftover draft by hand: it is
the only draft with the subject `uma recorded reply draft`.

```sh
export GMAIL_ADDRESS=<test mailbox> GMAIL_PEER=<peer address>
export GMAIL_ACCESS_TOKEN="$(go run ./scripts/gmail-oauth token)"
cd adapter/mail/google
XRR_MODE=record go test -count=1 -run '^TestRecordedGmailReplyDraft$' ./...
```

Cassettes land in `adapter/mail/google/testdata/cassettes/gmail-reply-draft`.
The scrub and residue checks are those of the main recording, which this test
runs after a recording; the same checklist applies, plus:

- **The draft answers** (`create`, `update`, `list`) carry the draft ID, the
  message ID, `threadId` and, for `create` and `update`, `labelIds`
  (`DRAFT`); no addresses. The two request bodies carry the reply in `raw`
  (base64url): decode both and check the mailbox and peer addresses were
  replaced, as for the sent message of the main scenario.
- **Check what Gmail did, and write it into the adapter page's known
  limits:** that the `Message-ID` of the created draft is the one the
  request carried (the `list` answer must name the draft; an empty list with
  a populated mailbox means Gmail rewrote it), that the draft ID in the
  `update` answer equals the one `create` answered while the message ID
  changed, and the status of the second `delete`.
## Record the unsubscribe cassettes

`TestRecordedGmailUnsubscribe` reads one message of a fixed 2001 window
(2001-01-04 00:00 to 2001-01-05 00:00 UTC) with the history reader, then runs
the `google.mail.unsubscribe` effect on it with `method: mailto`:
`users.messages.get` with `format=raw`, and `users.messages.send` of the
unsubscribe message. It sends one message, to the peer. The one-click method
is not recorded: it needs a real sender's DKIM-signed message and a request to
that sender's endpoint, and a replay cannot serve the DNS lookups of the DKIM
check; it stays on the local stand-in. Do not unsubscribe a real mailbox from a
real list to record it.

Before recording:

1. List the window (`q=after:978566400 before:978652800`,
   `includeSpamTrash=true`) and check it is empty.
2. Insert one message into the mailbox with `users.messages.insert`
   (`internalDateSource=dateHeader`, `labelIds` `INBOX` and `UNREAD`), dated
   2001-01-04 10:00 UTC, **from the peer address**, carrying the header
   `List-Unsubscribe: <mailto:<peer address>?subject=unsubscribe>`. The
   mailto target is the peer itself, so the send reaches an address you
   control and the organizational-domain rule holds. Needs the `gmail.insert`
   scope. Set `GMAIL_INSERT_PROJECT` as for the other window recordings.
3. List the window again and check it holds exactly that message.

```sh
export GMAIL_ADDRESS=<mailbox> GMAIL_PEER=<peer> GMAIL_INSERT_PROJECT=<project number>
export GMAIL_ACCESS_TOKEN=<token with gmail.readonly, gmail.send>
cd adapter/mail/google
XRR_MODE=record go test -count=1 -run '^TestRecordedGmailUnsubscribe$' ./...
```

Cassettes land in `adapter/mail/google/testdata/cassettes/gmail-unsubscribe`;
the placeholders, the scrub and the residue check are those of the main
recording (the unsubscribe message carries the mailbox address in `From` and
the peer in `To`, both replaced). After recording, delete the inserted message
and the sent one from the mailbox, and the received copy in the peer's.

To also check that DKIM verifies over the copy Gmail returns (not covered by the
recording): take a real, DKIM-signed message of a list you are subscribed to,
save its `format=raw` answer, and run `unsubscribe.CheckDKIM` over it from a
scratch test with the system resolver. A failure there means one-click would be
refused for that sender.

## Seed the sync window

`TestRecordedGmail` reads the inbox window 2001-01-03 11:00 to 12:00 UTC
(epoch `978519600` to `978523200`) in steps 4 and 8. Before recording:

1. List the whole day (`q=after:978480000 before:978566400`,
   `includeSpamTrash=true`) and check it is empty. If it is not, stop: a
   previous recording's messages are in the trash (see the cleanup below) and
   count; use another day (move `recordedNow`) or wait for Gmail to purge the
   trash.
2. Insert two messages from the peer into the mailbox with
   `users.messages.insert` (`internalDateSource=dateHeader`, `labelIds`
   `INBOX` and `UNREAD`), dated 11:20 and 11:40 that day, the second a "Re:"
   of the first. Gmail does not thread inserted mail by its headers: they
   land in two threads. Needs the `gmail.insert` scope.
3. List the day again and check it holds exactly those two.

Set `GMAIL_INSERT_PROJECT` to the inserting Cloud project's number: inserted
messages carry it in their `Received` header and the scrubber replaces it.

```sh
export GMAIL_ADDRESS=<mailbox> GMAIL_PEER=<peer> GMAIL_INSERT_PROJECT=<project number>
export GMAIL_ACCESS_TOKEN=<token with all four scopes>
cd adapter/mail/google
rm -r testdata/cassettes/gmail
XRR_MODE=record go test -count=1 -run '^TestRecordedGmail$' ./...
```

Delete the cassette directory first: a scenario run again sends a new
message, and the old files would answer requests of the new one.

## Choose a test mailbox

- **A personal Google account** created for testing works everywhere. Its
  app must use the External user type, so refresh tokens expire after 7
  days while the app is in Testing status
  ([refresh token expiration](https://developers.google.com/identity/protocols/oauth2#expiration)).
  Run the consent again when that happens.
- **A Google Workspace test user** lets you choose the Internal user type
  (the project must belong to the Workspace's Cloud organization), which is
  not subject to the 7-day expiry. A Workspace admin may block or restrict
  third-party access to Gmail scopes under **Security > Access and data
  control > API controls**; ask them to allow the app
  ([API controls](https://knowledge.workspace.google.com/admin/apps/control-which-apps-access-google-workspace-data)).

Also pick a **peer address** you own, different from the mailbox. It
receives the test message.

## Create the OAuth client

In the [Google Cloud console](https://console.cloud.google.com/):

1. Create a project for recordings.
2. Under **APIs & Services > Library**, enable the **Gmail API**.
3. Under **Google Auth platform**, configure the consent screen
   ([configure OAuth consent](https://developers.google.com/workspace/guides/configure-oauth-consent)):
   - **Branding:** an app name and your support email.
   - **Audience:** External (or Internal, see above). Keep the publishing
     status at **Testing** and add the test mailbox under **Test users**.
   - **Data access:** add the four scopes below.
4. Under **Clients**, create an OAuth client of type **Desktop app**
   ([desktop apps](https://developers.google.com/identity/protocols/oauth2/native-app)).
   Store the client ID and secret in your secret manager.

### Scopes

The adapter declares three scopes (`RequiredPermissions` in `factory.go`);
the recordings need a fourth, `gmail.insert`, to place the messages of the
windows. The state verbs add no scope:

| Scope | Used for | Google's class |
| --- | --- | --- |
| `https://www.googleapis.com/auth/gmail.send` | `messages.send` | Sensitive |
| `https://www.googleapis.com/auth/gmail.insert` | `messages.insert`, seeding the 2001 windows (recording only; the adapter never uses it) | Restricted |
| `https://www.googleapis.com/auth/gmail.readonly` | profile, search, inbox list, `format=raw` fetch | Restricted |
| `https://www.googleapis.com/auth/gmail.modify` | `messages.modify` (the labels effect, step 3), `threads.modify`, `threads.trash` and `threads.untrash` (the state verbs, step 5), `history.list` label records and `threads.get` (steps 4 and 7) | Restricted |

`gmail.modify` alone covers every call the adapter makes
([method reference](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.messages/modify)
lists it for `modify`; it also allows reading and sending), but grant the
three the adapter declares so the recording exercises that scope set, and
`gmail.insert` for the seeding. Do not
add `https://mail.google.com/`; the adapter never deletes anything: the
trash step uses `threads.trash` and restores it with `threads.untrash`. The
labels and state steps change only labels of the message the test itself
sent, and restore them. Restricted scopes need Google's verification only for
published apps: a Testing app used by its own test users does not, and shows
an "unverified app" screen on consent instead
([scopes](https://developers.google.com/workspace/gmail/api/auth/scopes),
[unverified apps](https://support.google.com/cloud/answer/7454865)).

## Get a refresh token once

`scripts/gmail-oauth` runs the consent with the loopback redirect desktop
clients use, with PKCE, and prints the refresh token on standard output. It
uses only the Go standard library and never writes a token to disk.

```sh
export GMAIL_OAUTH_CLIENT_ID=<client ID> GMAIL_OAUTH_CLIENT_SECRET=<client secret>
go run ./scripts/gmail-oauth consent -login-hint <test mailbox>
```

Open the printed URL in a browser signed in as the test mailbox, continue
past the unverified-app screen, and grant all four scopes (the script's
default set lacks `gmail.insert`: pass it with `-scopes`, together with the
other three). The browser lands on
a `127.0.0.1` page and the refresh token appears in the terminal. Store it
in your secret manager.

The consent always asks again (`prompt=consent`), so it returns a new
refresh token each run. `-scopes` overrides the scopes for other uses.

## Record Gmail cassettes

Access tokens last one hour
([token types](https://docs.cloud.google.com/docs/authentication/token-types)).
Mint one at the start of each recording session:

| Variable | Value |
| --- | --- |
| `GMAIL_ACCESS_TOKEN` | Access token for the test mailbox, with all four scopes |
| `GMAIL_INSERT_PROJECT` | Number of the Cloud project that inserted the window messages |
| `GMAIL_ADDRESS` | The test mailbox address |
| `GMAIL_PEER` | The peer address |
| `GMAIL_OAUTH_CLIENT_ID`, `GMAIL_OAUTH_CLIENT_SECRET`, `GMAIL_REFRESH_TOKEN` | Read by `scripts/gmail-oauth` only |

```sh
export GMAIL_OAUTH_CLIENT_ID=<client ID> GMAIL_OAUTH_CLIENT_SECRET=<client secret> GMAIL_REFRESH_TOKEN=<refresh token>
export GMAIL_ACCESS_TOKEN="$(go run ./scripts/gmail-oauth token)"
export GMAIL_ADDRESS=<test mailbox> GMAIL_PEER=<peer address>
cd adapter/mail/google
XRR_MODE=record go test -count=1 -run '^TestRecordedGmail$' ./...
```

One test records the whole scenario, steps 1 to 8, in one run; there is no
separate state recording to run.

Cassettes land in `adapter/mail/google/testdata/cassettes/gmail`. An
`invalid_grant` answer from `token` means the refresh token expired or was
revoked: run `consent` again.

## Keep the traffic safe

- Send only to your own peer address.
- Do not change filters, labels, forwarding or the Gmail API project while
  recording, and do not touch the mailbox's other mail: the state verbs act on
  the sent message's thread only, and every listing the scenario makes is
  narrowed to the messages it created (the 2001 window, the `SENT` history,
  the Message-ID search). Check the 2001 day is empty before seeding, and the
  recorded listings hold nothing else.
- Afterwards, move to the trash (`messages.trash`, with the IDs the run
  and the seeding printed; do not delete) the message the test sent, which
  ends in Sent, outside the inbox, with no star but with a
  `CATEGORY_PERSONAL` label Gmail adds, and the two inserted messages. The
  peer's copy sits in the peer's mailbox: trash it there. Trashed mail
  counts in a `includeSpamTrash` listing until Gmail purges it, 30 days later.
  The three messages of the history window stay: the history cassettes rely
  on them. If a run stopped between a verb and its undo, the thread may be in
  the inbox, unread, starred, marked important, labelled `CATEGORY_UPDATES` or
  in the trash: it is the test message's own thread, so clear it by hand.
- Revoke the refresh token when you are done recording for a while
  ([revoking a token](https://developers.google.com/identity/protocols/oauth2/native-app#tokenrevoke)):

  ```sh
  curl -s -d "token=$GMAIL_REFRESH_TOKEN" https://oauth2.googleapis.com/revoke
  ```

## Check the cassettes

What the test removes:

- the `Authorization` header;
- the mailbox and peer addresses where they appear literally in URLs and
  JSON (recorded as `MAILBOX` and `PEER`);
- the addresses inside base64url `raw` messages, in both directions
  (`messages.send` requests, `format=raw` answers). The scrub hook in
  `recorded_scrub_test.go` decodes `raw`, replaces the mailbox and peer
  addresses with `MAILBOX@mailbox.invalid` and `PEER@peer.invalid` (and
  their domains, unless `gmail.com` or `googlemail.com`, with
  `mailbox.invalid` and `peer.invalid`), matching case-insensitively, and
  encodes it again with the same alphabet and padding. It finds addresses in
  folded headers (a header that holds one is written on one line, so the
  bytes do not depend on where the real address made the writer fold),
  RFC 2047 encoded words, and quoted-printable and base64 bodies, nested
  multipart parts and `message/rfc822` parts (bodies that held one are
  encoded again at the usual line lengths). Replay derives the same request
  bytes from the fake addresses. The hook replaces the same values in every
  other string of a JSON body too.

After a recording the test scans every cassette file for the mailbox and
peer addresses and their non-public domains, in the encodings above and
inside `raw`, and fails naming which one survived (never the value). The
check also runs on committed cassettes when `GMAIL_ADDRESS` is set. A
failure means a recording must not be committed.

What it does not cover:

- **Other people's addresses.** The hook knows the two addresses you give it.
  Senders, recipients and names in the inbox messages of step 4 stay as they
  are, as do `snippet` text, subjects and bodies. The window holds only the
  two messages you inserted, from the peer; read the diff, and decode every
  `raw` body (the command below).
- **Obfuscated forms** such as `&#64;` in an HTML part, or text in a charset
  that is not ASCII-compatible (UTF-16).
- **The `modify` answers** of step 3 return the sent message's `labelIds`
  and `threadId` only, no addresses; nothing to scrub, but check that the
  second answer no longer lists `STARRED`.
- **The thread answers** of steps 4 to 7 carry label IDs and, for the
  metadata reads, the `To` and `Cc` headers of the messages of the threads
  read: the sent message's thread holds the peer address (scrubbed like any
  other value), and the full synchronization's threads hold whatever the
  window holds. Read the `threads` entries of the diff.
- **The history answers** of step 7 list the changes of the `SENT`-labelled
  messages since the first round, as message and label IDs; the scenario's own
  message only. The profile answer carries the mailbox's message and thread
  totals.

If replay with the `GMAIL_*` variables unset misses on `messages/send` or the
`rfc822msgid:` search, a real address is still in those requests; run the
decoding check by hand to see which:

  ```sh
  cd adapter/mail/google
  grep -rhoE '"raw": ?"[^"]+"' testdata/cassettes/gmail | sed -E 's/.*"([^"]+)"$/\1/' |
    python3 -c 'import base64,sys; [print(base64.urlsafe_b64decode(l.strip()+"==").decode(errors="replace")) for l in sys.stdin]' |
    grep -nF -e "$GMAIL_ADDRESS" -e "$GMAIL_PEER"
  grep -rnF -e "$GMAIL_ADDRESS" -e "$GMAIL_PEER" -e "${GMAIL_ADDRESS#*@}" testdata/cassettes/gmail
  ```

Skip the domain pattern for `gmail.com` mailboxes; it matches harmless
public text.
