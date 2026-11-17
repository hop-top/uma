# Mail history: window, cursors, identity, scope, limits

Status: accepted. The three mail adapters declare `history`; section 14 records
where the readers refine what sections 2 to 6 sketch.

`Runtime.Import` reads past messages through an adapter's
`adapter.HistoryReader` (`model.CapabilityHistory`) and accepts each through the
channel's sink as `Sink.AcceptImported`. `discord.chat`, `slack.chat`,
`github.issues` and `twilio.sms` implement it. This record fixes what a mail
reader means by the request and the page, so the three readers can be written
against one contract.

Fixed inputs, read from the code:

- A reader returns message envelopes oldest first, "as the adapter reports the
  same messages inbound, each with its provider message ID and destination"
  (`adapter.HistoryPage`). `Next` resumes; `Gap` means the provider could not
  return part of the window; a cursor the provider no longer accepts is
  `model.Validation` (`adapter.HistoryReader`).
- `Import` reads at most `Limit` envelopes per call (100 by default, 500 at
  most), across as many pages, and returns `Next` and `Complete`
  (`internal/runtime/history.go`, `history_import.go`). It runs no retry loop.
- The live path and the import path share one sink. A history envelope is
  accepted exactly as ingress accepts it, so everything below about identity
  is a property of `internal/ingress`, not of `Import`.
- Mail replies are linked by the new `Envelope.InReplyTo`: the first valid
  In-Reply-To msg-id, which ingress resolves to `Message.InReplyTo` only when
  the inbox already holds that message in the conversation the new message
  joins; otherwise no link is recorded (`model.Envelope`,
  `ingress.replyParent`). Order therefore matters for mail (section 8).

Every change below is additive under [compatibility.md](../compatibility.md)
(section 10). Adapters are not frozen; the engine and `adapter` surface are.

## 1. Window: an `Until` bound

Decision: add an optional `Until time.Time` to `adapter.HistoryRequest` and to
`runtime.ImportRequest` (so `uma.ImportRequest`). The window is half-open:
`Since <= t < Until`. Zero means open, today's behaviour. A mail reader bounds
the window by the provider's received time: Gmail `internalDate`, Graph
`receivedDateTime`, IMAP `INTERNALDATE`.

Why the received time, not `Envelope.OccurredAt`: the IMAP envelope's
`OccurredAt` is the `Date` header, which the sender chose (`imap/ingress.go`),
while every IMAP search and the receiver's own catch-up pass work on
`INTERNALDATE` (`imap/receive.go`, `deliverNew`). The envelope keeps the
inbound value so the two paths agree; only the window is judged by arrival.
For Gmail and Graph the two are the same value.

The engine passes `Until` through and does not re-check envelopes against it,
as it does not re-check `Since`. A reader that ignores `Until` reads on past
it; `Limit` still bounds the call.

Rejected: the caller bounds an import by page count (`Limit` and `After` only).

- A count is not a time. A host that wants "everything before the receiver
  started" cannot express it; it would walk into the receiver's territory
  until the provider runs out.
- Gmail has no oldest-first listing (section 2), so the Gmail reader must
  itself walk time in slices; it needs the upper bound the caller already has.
- A count drifts while mail arrives: the same count covers a different range
  on the second call.

## 2. Cursor shape, opaque to the engine

All three cursors follow the Discord reader's shape: base64url JSON
`{v, k, ...}` where `k` is a hash of the scope (route, mailboxes, `Since`,
`Until`). A cursor issued for another read, malformed, or from another version
is `model.Validation` before any provider call. The engine never parses a
cursor, and `Next` never equals the cursor it came from (`Import` fails the
call otherwise).

### Gmail: a time slice, not a pageToken

What the docs say (`users.messages.list`,
<https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.messages/list>,
read for this record): `maxResults` defaults to 100, at most 500; `pageToken`
"retrieve a specific page"; `q` "same query format as the Gmail search box"
and unusable under the `gmail.metadata` scope; `labelIds` match all given;
`includeSpamTrash` opts spam and trash in. The page documents no result
order. The code assumes newest first (`google/receiver.go`, `fullSync`:
"messages.list answers newest first") and reverses the whole list after
paging it; the recorded history cassettes confirm a newest-first answer for
one day slice. The reader sorts regardless, because the list carries only `id` and `threadId`: dates need a
`get`.

Decision: the cursor holds a slice of time, never a raw `pageToken`.

- Cursor: `{v, k, lo, hi, rest}`: the slice `[lo, hi)` in epoch seconds being
  delivered, and `rest`, the IDs of that slice not yet delivered, in delivery
  order. `lo` starts at `Since`; `hi` at the lesser of `Until` and a snapshot
  of "now" taken on the first call and carried in the cursor.
- A call with no `rest` lists a slice with `labelIds=INBOX` and
  `q=after:<lo> before:<hi>` (epoch seconds, the form the receiver's own
  full-sync query already uses), draining every `pageToken` inside the call.
  IDs are fetched `format=raw` through the receiver's `fetchIngress`, sorted
  by `(internalDate, id)`, and the first `Limit` envelopes are returned; the
  remainder becomes `rest`.
- A slice is one day. A day whose listing does not end on the first page
  (500 IDs) is halved and listed again. Bounded slices bound `rest`
  (500 IDs, about 10 KB) and the memory of a call.
- `Next` is empty once the last slice is drained.

Rejected: cursor = Gmail's `pageToken`, each page reversed.

- Pages then arrive newest page first, so the window is not oldest first and a
  reply is accepted before its parent (section 8).
- The docs state no token lifetime, so a stored token could die between two
  `Import` calls.
- The listing has no dates; a page's order would still need a `get` per
  message.

Rejected for `rest`: re-list the slice on every call and skip what was
delivered. It costs a list per call and the skip point needs a date the list
does not give.

### Microsoft Graph: the list's `@odata.nextLink`, not delta

What the docs say:

- `List messages`
  (<https://learn.microsoft.com/en-us/graph/api/user-list-messages>): default
  page 10, `$top` 1 to 1000; "apply the entire URL returned in
  `@odata.nextLink`"; do not extract `$skip` from it; `$orderby` properties
  must also appear in `$filter`, in the same order, before any other filter
  property, else `InefficientFilter`. Listing `/me/messages` includes Deleted
  Items; a folder is listed through `/mailFolders/{id}/messages`.
- `message: delta`
  (<https://learn.microsoft.com/en-us/graph/api/message-delta>): the only
  `$filter` is `receivedDateTime ge|gt {value}`; the only `$orderby` is
  `receivedDateTime desc`, and without it "the return order is not
  guaranteed"; delta can return `@removed` and read-state events that do not
  match the filter.

Decision: list `mailFolders('inbox')/messages` (the receiver's own
`mailboxPath()` plus the inbox) with
`$filter=receivedDateTime ge {Since} and receivedDateTime lt {Until}` and
`$orderby=receivedDateTime asc`, `$top` set to the page size, `$select`
`messageSelect` (as the inbound fetch), and the same headers the receiver
sends: `Prefer: IdType="ImmutableId"` and the text body preference
(`microsoft/client.go`, `microsoft/inbound.go`). Cursor
`{v, k, link}` holding the `@odata.nextLink`; a link to another origin is
refused as the receiver refuses it (`client.sameOrigin`). Ascending order
needs no reversal.

`ImmutableId` is load-bearing for identity: without it the ID a history read
returns differs from the receiver's, and one message stores twice.

Rejected: delta for history. It has no upper bound and no ascending order, its
tokens belong to the receiver's tracking, and its folder-level events add
noise an import has no use for.

Verified by the recorded history cassettes: `lt` on `receivedDateTime`
together with `$orderby asc` on the inbox works, and `internetMessageHeaders`
is selectable in a list. Graph's `@odata.nextLink` names the folder by key
(`mailFolders('inbox')`); the reader compares paths in one spelling.

### IMAP: UIDVALIDITY plus a UID position

Decision: cursor `{v, k, mailbox, uid_validity, last_uid, last_date}`.

- The reader selects the mailbox read-only, as the receiver does, and compares
  the server's UIDVALIDITY to the cursor's (section 5).
- Each call runs `UID SEARCH` with `UID last_uid+1:*` and, for the window,
  `SINCE` a day before `Since` and `BEFORE` a day after `Until`, then refines
  by the fetched `INTERNALDATE` to `[Since, Until)`. `SINCE` and `BEFORE`
  compare dates only (RFC 9051 section 6.4.4); the receiver's catch-up
  already widens by a day for the same reason, and `n:*` always matches the
  highest UID even below n, so UIDs at or below `last_uid` are dropped
  (`deliverNew`).
- UIDs are processed ascending and the first `Limit` found are fetched
  (`BODY.PEEK[]`, header only past `Config.MaxMessageBytes`, as `fetchMessage`),
  so no flag changes. `last_uid` is the last UID handled, delivered or
  skipped, as in the receiver.
- UID order is arrival order in the mailbox (RFC 9051 section 2.3.1.1:
  UIDs are assigned ascending within one UIDVALIDITY). A message copied in from
  another mailbox takes a new UID, so it is delivered when it arrived, not when
  it was sent. Rejected: re-sorting by `INTERNALDATE`; it would diverge from
  the receiver's order for no gain in thread order.

## 3. Identity: the same envelope as the inbound path

Decision: a history envelope is the inbound envelope, produced by the same
function, with the same provider message ID, destination, `EventID` and
payload. The three readers call each adapter's existing mapping
(`google` `fetchIngress` and `normalize`, `microsoft` `fetchIngress` and
`normalize`, `imap` `fetchMessage` and `ingress`) and set no field of their own.
In particular they never set an `EventID` such as the Discord reader's
`"history:" + id`: that choice makes the event claim differ between paths and
leaves only the second layer below.

What "stored once" rests on, verified in `internal/ingress`:

1. Event claim. `ClaimIngress` is keyed per channel by `dedupKey`: kind, event
   ID, message ID, source revision, length-prefixed. The second arrival of the
   same key returns the first's receipt with `Duplicate` set and stores nothing,
   whichever path arrived first. Gmail and Graph envelopes carry no `EventID`
   and the same `MessageID` (the Gmail or Graph immutable ID), so both paths
   produce one key. IMAP's `EventID` is `mailbox:<destination>;sha256:<digest
   of the fetched bytes>` (`eventID`), so both paths produce one key when they
   fetch the same bytes.
2. Message identity. For an adapter declaring `MessageIDsProvider` (Gmail and
   Graph), the provider message ID is the message's identity in its inbox:
   `knownMessage` reads the stored message by `ProviderMessageRef` and
   `putReceived` claims it atomically (`ClaimMessage`); a concurrent loser
   reruns as `Transient`. A second report is that message's update, a no-op for
   equal content. IMAP declares `MessageIDsSender`: a Message-ID is never
   claimed, because anyone can repeat it (`ingress/identity.go`).
3. Import's own check. `importer.held` skips an envelope whose destination
   inbox already holds its provider message ID before the sink runs.

Result: Gmail and Graph, imported then received or received then imported, are
stored once by two independent mechanisms. IMAP is stored once by the event
claim alone, which holds only while both paths fetch identical bytes in the
same mode (full, or header-only past `MaxMessageBytes`), which they do by
construction.

Gaps found, each decided:

- G1, engine. `importer.held` and `importer.one` key on `MessageID` for every
  adapter. For IMAP that has two wrong effects: a mail whose sender-chosen
  Message-ID an inbox already holds is skipped although its bytes differ
  (the case the IMAP event ID exists to keep apart), and a mail with no valid
  Message-ID is skipped as "no provider message ID" although the live path
  delivers it (ingress accepts an `EventID` alone, `check`: "event_id or
  message_id required"). Decision: the `MessageID` pre-check runs only for
  adapters declaring provider-assigned IDs (`engine.ProviderAssigned`), and
  `one` skips an envelope only when it has neither `MessageID` nor `EventID`.
  Behaviour for the four existing readers is unchanged: all declare
  provider-assigned IDs.
- G2, attachments: resolved (section 13, item 3). `HistoryRequest` carries an
  optional `Stager` (`adapter.AttachmentSink`), the engine's channel sink, which
  `Runtime.Import` always sets. The mail readers hand their shared mapping a
  sink built from it (`staging.HistorySink`), so a history read stages
  attachment bytes exactly as the receiver does: same keys (message, position
  or attachment ID), same bounds (10 attachments, 25 MiB each), same refusal
  and retry handling, same descriptors with `Staged` set. Staging is idempotent
  per channel and key, so an imported message and its live delivery, in either
  order, name the same attachments and hold the bytes once. Readers that have
  no bytes to stage (Twilio media, Slack and Discord files) ignore the field and
  keep describing only.
- G3, IMAP state (resolved, section 14.5). The receiver follows each message it
  delivers (mailbox, UID and flags, `follow`) so later flag and folder changes
  are reported as conversation state. A history read now records what it
  imported in a second ledger that the receiver adopts, so imported mail is
  followed as delivered mail is. Gmail and Graph report state from mailbox
  changes, not from a delivered-message ledger, and are unaffected.

Never in an import: `Ingress.ReceiverCursor` is empty, so `saveCursor`
(`internal/ingress/cursor.go`, a no-op without it) writes nothing; an import
cannot move the receiver's cursor (section 9).

## 4. Sent mail: out of this import

Decision: this import covers received mail only. Each reader applies the
inbound path's own exclusions so an imported message is one the receiver
would also have accepted: Gmail skips messages labelled `DRAFT` or `SENT`
(`skipLabels`); Graph skips drafts (`isDraft`); IMAP walks only the receive
mailbox, never `Config.Sent.Mailbox`.

Why it cannot be adapter work: the model has no direction. `model.Envelope`
describes a message received; the Twilio reader says "an envelope has no
form for a message the channel wrote". Messages uma sent already exist as
outbound messages with an `Origin`, and their provider ID may be rewritten by
the provider (`ProviderMessageRef`, "including a provider-rewritten email
Message-ID").

The separate decision (decided with the inbound conversation-state work) must
cover:

- a direction marker on the envelope and the stored message, and what an
  authored-by-the-mailbox message does to author resolution (the mailbox
  owner's identity);
- reconciliation with messages uma itself sent, so a Sent import of one is the
  echo of that operation (`reconcileEcho`, `markedEcho`), never a second message;
- conversation selection when the first message of a thread is the mailbox's
  own, and unread, `message.received` and snooze effects for own mail;
- audience and recipients (To, Cc, Bcc) of a sent message;
- which verb carries it (`Import` or another), and the sent folder of each
  provider (Gmail `SENT`, Graph `sentitems`, IMAP `Config.Sent.Mailbox`);
- sent-to-self parity: Gmail skips `SENT` mail even when it also sits in the
  inbox, while the Graph inbound path skips drafts only (`microsoft/inbound.go`),
  so such a message is received on Graph and not on Gmail today.

## 5. Mailboxes walked

Decision: by default a reader walks exactly the receive scope: Gmail messages
labelled `INBOX`; Graph the `inbox` folder of the configured mailbox path; IMAP
`Config.Mailbox` (`INBOX` by default). Spam, junk, trash, drafts and sent are
never walked. An additive option, `history_mailboxes` on each adapter's
configuration, names the mailboxes to walk instead (Gmail label IDs, Graph
folder IDs or well-known names, IMAP mailbox names); empty is the receive
scope. Section 14 item 5 records how each reader walks it.

Why the receive scope:

- Identity (section 3) and the parity argument hold for messages the
  receiver would also accept. Outside it nothing proves a message stored by
  import matches a later live report.
- Archived mail has no live counterpart: the Gmail receiver takes `messageAdded`
  only for inbox messages (`syncHistory`). Importing it would open conversations
  nobody is waiting on.
- The docs agree: Graph's `/me/messages` includes Deleted Items; Gmail's
  `messages.list` excludes spam and trash unless `includeSpamTrash` is set.

Rejected: all mail except spam and trash by default. It is the likely
long-term want for a backfill, but it brings in archive and the conversation
state questions with it; make it an explicit choice.

Conversation-scoped import (`ImportRequest.Conversation`) passes the
conversation's reply route, an address reference (`Kind: "address"`). A mail
reader filters on that sender: Gmail `from:<address>`, Graph a
`from/emailAddress/address eq` filter (kept first, in `$orderby` order), IMAP
`SEARCH FROM`. Without a route it reads the channel. The conformance case
`HistoryConversationOrChannel` needs both.

## 6. Gap

`Gap` reports that the reader knows part of the window was not returned. It is
never set for what the provider silently lost: a message past a retention
policy or deleted at the provider cannot be seen and is not a gap.

Gap is set when:

- A message listed in the window can never be read: Gmail `errUnreadable` (a raw
  form that is not base64url or does not parse); IMAP a fetched message that
  cannot be delivered (malformed, no usable From). The receiver counts these as
  `ObservationSkipped`; a history page reports them as gap.
- IMAP, UIDVALIDITY changed between two calls of one import (below).

Gap is not set, and the message is simply absent, for: a message gone between
the list and the get (Gmail 404, Graph 404, IMAP expunged: the receiver's
vanished); a message over the size bound (delivered header-only, as inbound,
`body_omitted`).

A whole-read refusal is an error, not a gap: no scope for the call (a Gmail
token under `gmail.metadata` cannot use `q`: `model.Unsupported`); mailbox not
accessible (IMAP `SELECT` answers NO or BAD: `model.Unavailable`, as the
receiver; Graph 403 or 404 for a mailbox the credential cannot reach); a server
that reports no UIDVALIDITY (`model.Unsupported`, as the receiver).

UIDVALIDITY change mid-import: UIDs from the earlier value are void
(RFC 9051 section 2.3.1.1). The reader does not resume by UID. It sets
`Gap`, restarts the window at the cursor's `last_date` (the day-wide margin of
section 2 applies) with the new value, and carries on. Messages read twice are
absorbed by the event claim (the digest does not depend on a UID,
`eventID`). This is what the receiver does (`round`, `ObservationGap`).
Rejected: fail the call with `model.Validation` ("a cursor the provider no
longer accepts"). It is correct but forces the host to restart the import from
nothing and re-read every message to skip it.

## 7. Rate limits and backoff

Decision: a reader never sleeps and never retries a throttled call. It maps the
answer to `model.RateLimited` (or `Transient`) with `RetryAfter` and returns.
Gmail and Graph already do (`google/client.go`, `microsoft/client.go`:
`RateLimited`, `Retry: RetrySameOperation`, `RetryAfter` from `Retry-After`).
IMAP has no rate protocol; a connection failure is `model.Unavailable` as in the
receiver.

What the providers document:

- Gmail (<https://developers.google.com/workspace/gmail/api/guides/handle-errors>):
  `rateLimitExceeded` and `userRateLimitExceeded` are 403, per-user concurrent
  and daily limits 429; retry with exponential backoff starting at one second;
  avoid many parallel requests for one user. Quota
  (<https://developers.google.com/workspace/gmail/api/reference/quota>, read for
  this record): 6,000 units per minute per user per project; `messages.list` 5
  units, `messages.get` 20, or 60 for `raw`. At those figures a mailbox
  sustains about 100 raw reads a minute; a large backfill is hours, and the
  host paces it.
- Graph (<https://learn.microsoft.com/en-us/graph/throttling>): 429 with a
  `Retry-After`; "avoid immediate retries, because all requests accrue against
  your usage limits"; wait the stated seconds, then retry. The page also
  directs bulk extraction to Graph Data Connect; an import is bounded
  by `Limit` and the host.

Engine side, today and proposed:

- `RetryPolicy` governs receivers and queued work. `Runtime.Import` has no
  retry loop and returns on the first reader error, so the engine honours
  `RetryAfter` by returning it, and the host waits.
- Gap in `Import`: on a reader error `run` returns the report without `Next`
  (`history_import.go`), so a host cannot resume at the failed page and must
  re-read from its last `After`, skipping what it already imported.
  Decision (additive behaviour, no signature change): on a reader error the
  report carries `Next` set to the cursor of the page that failed.
- `Limit` per call (at most 500) bounds one call's provider spend, and with it
  the work a throttled call loses.

Rejected: backoff inside the adapter. It blocks the caller's goroutine past its
own deadline, hides the quota from the host that has the budget view, and
stacks on any backoff the host adds.

## 8. Order and replies

Decision: oldest first across the whole window, per section 2. For mail this is
a correctness property, not a convention:

- `Envelope.InReplyTo` is resolved when the reply is accepted, to a message the
  inbox already holds in the conversation the reply joins (`replyParent`);
  otherwise no link is recorded and none is added later.
- Conversation selection by header references matches stored messages
  (`headerMatches`), so a parent accepted after its reply is not merged in.

Each reader therefore delivers a parent before its replies whenever both are in
the window. Across a window's start the parent is simply absent and the reply
links nothing, as for any history.

## 9. Interaction with the live receive cursor

The receivers keep their position in the protocol store: Gmail `historyId`
(`receiverState`), Graph the delta links (`receiverState.DeltaLink`,
`Folders`), IMAP the cursor and ledger (`protocolKey "receiver"`).

Decisions:

- A history reader is read-only against receiver state. It never calls
  `loadState`, `saveState`, `loadCursor`, `saveCursor` or `saveLedger`, and it
  holds none of its own: the position is in the cursor the caller keeps.
- The engine side already cannot regress a cursor: an ingress accepted by
  `AcceptImported` carries no `ReceiverCursor`, and `saveCursor` returns at
  once without one (`ingress/cursor.go`). Only a receiver's own `Accept`
  advances it.
- No ordering between import and receive is guaranteed or needed for
  storage: both are idempotent (section 3). Ordering matters only for
  reply links and threads (section 8): a live reply accepted before its older
  parent is imported stays unlinked, and the parent starts its own conversation.
- Operating rule: a host sets `Until` at the receiver's first synchronization
  start and imports before or beside it. The window then ends where the live path
  begins. The receivers' initial windows overlap it by design (one hour by
  default for Graph and IMAP, `initial_sync_window_ms`); the overlap is
  duplicates and is skipped.
- Mailbox access: a history read opens its own instance
  (`runtime.OpenOutbound`), distinct from the receiver's. For IMAP that is a
  second session on the account; servers cap sessions per account, and the
  reader closes its session at the end of every call.

## 10. Additive after the freeze

The surface touched, under [compatibility.md](../compatibility.md):

| Surface | Change | Class |
| --- | --- | --- |
| `adapter.HistoryRequest` | add optional `Until` (zero is today's behaviour) | additive: a struct field whose zero value keeps today's behaviour |
| `uma.ImportRequest` | add optional `Until` | additive |
| `adapter.HistoryReader` | none: no method added | unchanged |
| `Runtime.Import` behaviour | message-ID pre-check only for provider-assigned adapters; skip only when no `MessageID` and no `EventID`; `Next` set on a reader error | additive: relaxes a skip, adds a value to a field that was empty |
| Capabilities | `google.mail`, `microsoft.mail`, `imap.mail` declare `history` (a name already builtin) | additive; adapters are not frozen |
| Adapter configuration | optional `history_mailboxes` option (section 14 item 6) | additive; adapter schemas are not frozen |
| `store.Store`, `Reader`, `Tx`, persisted formats, event payloads | none: cursors are opaque and held by the caller; no receiver state is read or written | unchanged |
| Conformance suite | new adapter cases (section 12); an adapter that does not declare `history` skips them | additive |

Existing readers (`discord.chat`, `slack.chat`, `github.issues`, `twilio.sms`)
must honour `Until` in the same change that adds it; each provider read has an
upper time bound to confirm per adapter.

After the freeze the surface above gains optional fields and new values only.

## 11. Per-adapter sketch

Common to all three: reject `Limit < 1` and a zero `Since` as `Validation`;
refuse a foreign cursor (section 2); read only; never touch receiver state;
never set `EventID` beyond what the inbound mapping sets; apply the inbound
exclusions (section 4); `Route` (section 5) is an address reference of this
channel, else `Validation`.

`google.mail` (`adapter/mail/google/history.go`)
- Slice walk of section 2. List ids per slice (`labelIds=INBOX`,
  `q=after:<lo> before:<hi>`, `maxResults` 500, drain `pageToken`); fetch each
  with `fetchIngress` and the request's stager as its staging sink (G2); skip
  what `skipLabels` skips; sort by `(internalDate, id)`.
- 429 and 403 rate reasons map through the client's existing `rateLimited`.
  `Gap` for `errUnreadable`.
- Needs: nothing in `receiver.go`; a `Route` adds `from:<address>` to `q`.

`microsoft.mail` (`adapter/mail/microsoft/history.go`)
- First call builds the list URL of section 2; later calls follow the cursor's
  link after `sameOrigin`. Map items through `normalize` (attachment
  descriptors from `attachments`, no staging). Skip `isDraft`. A `404` on an
  attachment list ends it with what was read, as inbound.
- `Prefer` headers identical to the receiver's. Page size from `$top`, at most
  `Limit`.

`imap.mail` (`adapter/mail/imap/history.go`)
- Open a session as the receiver does (`connect`), `SELECT` read-only, apply
  section 2 and section 6; fetch metadata then body as `deliverBatch` and
  `fetchMessage` do, then `ingress`; records the imported messages in the import ledger (section 14.5), never the receiver's ledger or cursor.
  Close the session before returning. `Disabled` if no credentials, as the
  receiver.

Engine (`internal/runtime`): `Until` on both requests; G1 checks; `Next` on a
reader error.

## 12. Test plan

Conformance (`conformance/adapter_history.go`, run by `RunAdapterSuite` for any
adapter declaring `history`):

- The four existing cases apply to the mail adapters unchanged:
  `HistoryWindowOldestFirstInBoundedPages`, `HistoryConversationOrChannel`,
  `HistoryUnknownCursorIsValidation`, `HistoryImportSkipsInboundCopy`. Each mail
  provider implements `HistoryProvider.HoldHistory` over its fake (the Gmail
  fake server, the Graph fake, the IMAP test server).
- New, all adapters: `HistoryUntilIsExclusive` (a message at `Until` is out, one
  just before is in); `HistoryImportedThenReceivedOnce`, the reverse of the existing
  `HistoryImportSkipsInboundCopy` (import first, then deliver the same message
  through the receiver, expect one stored message);
  `HistoryEnvelopeEqualsInbound` (same `MessageID`, `EventID`, `Destination` as
  the receiver reports the same message).
- New, engine (service suite `HistoryImport`): reader error carries `Next`; an
  adapter with sender-chosen IDs imports a mail with no Message-ID and a mail
  whose Message-ID the inbox holds under different bytes; an import never moves
  the receiver's cursor (read the protocol record before and after).
- Mail-specific unit tests per adapter:
  - Gmail: slice halving when a listing has a `nextPageToken`; `rest` resume
    across calls; delivery order of out-of-order `internalDate`; `Gap` on
    `errUnreadable`; header-only message past the size bound.
  - Graph: nextLink resume; refusal of a link to another origin; ImmutableId
    header sent; `Until` in the filter; draft skipped.
  - IMAP: UIDVALIDITY change between calls (Gap, restart at `last_date`, no
    duplicate); `n:*` quirk at the top of the mailbox; header-only past the
    bound; two UIDs with the same bytes store once; mail with no Message-ID.
- Mutation check for each fix to G1 and `Next`-on-error: reintroduce the old
  behaviour, confirm the new case fails, restore it.

Cassettes:

- Gmail: record `users.messages.list` with `labelIds`, `q=after:` and
  `before:` and a two-page answer, and `messages.get` raw, from the existing
  recorded-test guide (`docs/recording/gmail.md`). The recorded test
  currently skips until someone records it. The probe also settles the order
  of `messages.list`, which the docs do not state.
- Graph: record the list with `$filter`/`$orderby asc` and a `nextLink`, plus the
  headers sent (`docs/recording/microsoft-graph.md`). Recorded; `lt` on
  `receivedDateTime` confirmed.
- IMAP: no record path exists (`docs/recording/imap-smtp.md`); cover with
  opt-in `TestLive*` cases against the existing containers, including recreating
  the mailbox to force a UIDVALIDITY change.

## 13. Decisions on the open questions

1. `Until` for the four existing readers: all four update in the same change;
   no marker interface. Rejected: an optional interface the engine checks, so a
   reader that cannot bound by `Until` is `Unsupported` for a request that sets
   it; it costs API for a temporary state.
2. Mailbox scope: the first readers ship with the receive scope only; default
   unchanged. Rejected: shipping the mailbox list now so an archive backfill is
   possible from the start. Follow-up, since done: an optional mailbox list
   (section 14 item 6).
3. Attachment bytes in history (G2): staged through an optional
   `HistoryRequest.Stager`, set by the engine on every import read; a reader
   that does not use it, or finds it nil, describes attachments only. Additive:
   the zero value is the earlier behaviour. Rejected: describing only; a host
   that imports history then lacks the bytes of every attachment, and, with the
   event claim of layer 1, a later live delivery of the message is a duplicate
   and never adds them. Rejected: a separate attachment-fetch verb run after the
   import; it needs a second pass over the provider for bytes the read already
   holds (Gmail and IMAP) or is about to download (Graph).
   Consequences:
   - Rate limits: no new calls for Gmail (`format=raw` holds the bytes) or IMAP
     (the body is fetched whole). Graph adds one `$value` request per staged
     file attachment, at most 10 a message, under the same no-sleep rule: a
     throttled download returns `RateLimited` with `RetryAfter` and the cursor of
     the page being read, so the host paces and the page is read again.
   - Staging runs while a page is read, before the engine decides whether to
     import the envelope. A message the engine then skips (already held, or
     bound for another inbox) leaves its attachments staged and unreferenced;
     the keys are the receiver's, so the same bytes are not staged twice.
   - A message imported before this change, whose bytes were never staged, is
     left as it is: nothing. A live delivery of it is still a duplicate and
     adds no bytes; importing it again skips it. Recovering the bytes of such a
     message is not offered; a host that needs them imports into an empty
     inbox or deletes and re-imports the message.
   - A staging failure a retry may fix (blob store unavailable, context ended)
     fails the read as it fails the receiver's fetch; the import returns it with
     `Next` set to the failed page.
4. G1 engine change: accepted. The message-ID pre-check is scoped to adapters
   whose message IDs are provider-assigned; without it IMAP history drops mail
   the live path stores.
5. Retry inside `Import`: none. `Import` returns `RetryAfter`, sets `Next` on
   error, and the host paces. Rejected: a bounded retry honouring `RetryAfter`
   inside `Import`; it holds the caller's goroutine for the wait.
6. Conversation-scoped import for mail: supported through the sender address
   (section 5), because the suite's `HistoryConversationOrChannel` case needs
   it. Rejected: `Unsupported` for a route, with the suite gaining an opt-out.
7. Gmail slice cursor carries the undelivered IDs (about 10 KB at most):
   accepted. Rejected: re-list and skip; dearer, and the skip point has no
   date.
8. IMAP state following for imported mail (G3): resolved, section 14.5.
9. Sent-to-self parity (Graph receives it, Gmail does not): left alone here;
   settled in the sent-mail decision (section 4).
10. Sent-mail decision owner: the inbound conversation-state work, in its own
    record, accepted before any sent-mail reader is written.

Follow-ups, none built by this record: the sent-mail decision, owned by its
own record.

## 14. Implementation refinements

Accepted refinements to the sketches above, found while building the readers.
Each keeps the decision it refines; none changes the engine contract.

1. Graph cursor. Section 2 sketches `{v, k, link}` holding the
   `@odata.nextLink`. The cursor holds `{v, k, p, t, s, n}` instead: the paging
   parameter (`$skiptoken` or `$skip`) and its token, the `$top` the read opened
   with, and `n`, a count of the page's messages already returned (a later,
   smaller `Limit` takes a page in parts, so the page is read again and its head
   dropped). No URL is stored or followed: the list URL is rebuilt from the
   request on every call, and a `nextLink` that is not the next page of that
   rebuilt list is refused. A cursor therefore cannot point the credential at
   another path, folder or mailbox, and the cross-origin check of the sketch has
   nothing left to guard. A token is set as one query value and cannot add a
   parameter.
2. Gmail dating. The listing carries no dates, so a call dates each listed ID
   with `messages.get` `format=minimal` and judges the window on `internalDate`
   before it fetches `format=raw` for the envelopes; a message outside the
   window costs the cheaper call. `q` is widened by one second on both
   bounds, because Gmail rounds `after:` and `before:`, so the listing is a
   superset and the exact bound is the `internalDate` check. A call that meets
   31 empty slices stops and returns its cursor, so a window far in the past
   does not cost one call its whole budget. The quota figure of section 7
   becomes 80 units a message (20 minimal plus 60 raw): about 75 messages a
   minute.
3. IMAP sender. `SEARCH FROM` matches substrings, so a conversation read checks
   the exact sender again on the parsed message and drops the rest.
4. Cursors are forgeable. The scope hash `k` is computed from values the caller
   knows (route, mailbox, window), so it detects a cursor issued for another
   read and guards nothing against a caller who forges one. All three readers
   treat what a cursor names as untrusted: bounds and dates must lie in the
   request's window (`Validation` otherwise, before any provider call), the
   mailbox must be the configured one, and every fetched item is checked again
   against the read's scope (labels, folder, sender, window) before it is
   returned, so a forged Gmail `rest` list or IMAP position delivers nothing out
   of scope.
5. IMAP state following (G3). The ledger has one writer, the receiver, so the
   reader does not write it. Nothing the adapter can read says what was
   imported (it has the sink and the protocol store, no message store), so the
   reader records `{mailbox, UIDVALIDITY, UID, Message-ID, \Seen, \Flagged}` of
   each envelope it returns in an import ledger of the same shape under its own
   protocol key (in the factory without a store). The receiver adopts it at the
   start of every round: entries it does not follow yet join its ledger, then
   are released from the import ledger; both sides write it by compare-and-swap
   over a fresh read, so neither loses the other's write. The key is the
   mailbox, UIDVALIDITY and UID, so a message imported and delivered, or
   imported twice, is one entry; the reference is the `Message-ID` the envelope
   carries, as in inbound. From adoption the sync is the existing one: flags,
   moves and expunges give the same `conversation.state` report as for a received message.
   - UIDVALIDITY: UIDs of another value than the one the receiver follows are
     dropped, not adopted over it; the sync voids a stale mailbox as before.
   - Cost bound: the import ledger and the ledger each keep the highest 5000
     UIDs per mailbox (`trackedLimit`), so years of imported mail follow only
     the newest 5000 and import cannot displace newer received mail. The sync
     stays one `UID SEARCH` of at most 5000 UIDs per round, flags by CONDSTORE
     `CHANGEDSINCE` or at most every `state_sync_interval_ms`, never a mailbox
     rescan. Adopting new messages makes the next comparison of their mailbox
     one full flag fetch (at most 5000 UIDs), because the reader's flags may be
     older than the stored mod-sequence. A round with nothing to adopt costs
     one extra protocol-store read.
   - Rejected: the reader writing the receiver's ledger (lost updates between
     processes); deriving tracking from stored messages (the adapter has no
     read access to them); rescanning the mailbox for unfollowed mail (cost
     grows with the mailbox).
6. Optional mailbox list. Resolves the follow-up of decision 2. Each adapter
   gains `history_mailboxes`, a list of strings; empty or absent is the receive
   scope, exactly as before. The received-mail rules do not change: the same
   items are skipped as inbound skips them, and a listed mailbox only widens
   where they are looked for. Spam or junk, trash, drafts and sent mail are
   refused even when listed: by name in `Validate` (an issue on
   `history_mailboxes`), and where a name cannot tell, when a folder is read
   (`Validation`: Graph compares the listed ID with the IDs of the refused
   well-known folders; IMAP reads RFC 6154 `\Sent`, `\Drafts`, `\Junk`,
   `\Trash` from LIST; Gmail ships label IDs, whose refused ones are fixed
   names). Sent mail stays its own decision (section 4). The scope hash of
   every cursor covers the list, so a cursor issued under another list is
   `Validation` before any call; what a cursor names is untrusted as in item 4.
   - Gmail (label IDs only; names would need a `labels.list` per read):
     a day slice is listed once per label with `labelIds=<label>`, the IDs are
     united and the slice dated and sorted as a whole. Order stays oldest first
     across labels, and a message under two labels is one ID, read once. A
     slice is halved when any label's listing needs a second page. The cursor
     shape is unchanged and names no label; each fetched message must carry a
     listed label and none of SPAM, TRASH, DRAFT, SENT.
   - Graph: mailbox by mailbox in the listed order, each to its end; the cursor
     adds `m`, the folder being read, which must be listed (the request URL is
     still rebuilt, never taken from a cursor). Graph cannot order one list
     across folders (`$orderby` with a `parentFolderId` disjunction is not an
     efficient filter), and a merge needs a paging state per folder. Order is
     oldest first within a folder, not across them. A message copied into
     several listed folders has one Internet message ID and a distinct Graph ID
     per copy, so a copy found in an earlier listed folder (a lookup by
     `internetMessageId`) is skipped: it is delivered once, in the first folder.
   - IMAP: mailbox by mailbox, the cursor's existing `mailbox` field naming the
     current one, which must be listed; a cursor with no `uid_validity` is the
     start of its mailbox. UID order within a mailbox, not oldest first across
     them. A copy has the same bytes, so the same event ID and the sink stores
     it once; no lookup is made across mailboxes.
   Rejected: merging by received time across mailboxes for Graph and IMAP. It
   costs a metadata read of `Limit` messages per mailbox per call and a
   per-mailbox position in the cursor, to protect reply links across mailboxes
   a host can order by listing the parents' mailbox first. Sections 8 and 9
   still hold within a mailbox, and across Gmail labels.

Status stays accepted.
