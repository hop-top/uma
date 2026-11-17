# microsoft.mail

`microsoft.mail` is the Microsoft Graph v1.0 mail adapter, one channel per
mailbox, for tenants and mailboxes that IMAP and SMTP cannot reach. It speaks
plain JSON over the host's HTTP client (no Graph SDK).

| | |
| --- | --- |
| Key | `microsoft.mail` |
| Package | `hop.top/uma/adapter/mail/microsoft`, own module `hop.top/uma/adapter/mail/microsoft` |
| Capabilities | `receive`, `receive_http`, `history`, `send`, `reconcile`, `start_conversation`, `reply_recipients`, `raw_read`, `extension_execute`, `extension_query`, `microsoft.mail.mailboxes`, `microsoft.mail.mailbox_create`, `archive`, `move`, `read_state`, `flag`, `trash` |
| Events | `message.received`, `delivery.updated` |
| Identity namespaces | `microsoft.mail.address` |
| Sender kinds | person |
| Provider permissions | Graph `Mail.ReadWrite`, `Mail.Send` (delegated or application) |
| Message IDs and threading | assigned by Graph; email header threading |
| Reply parent | reported: the first valid `In-Reply-To` msg-id; resolved to a stored message by its `Message-ID` |
| Interactions | none ([interactions](../interactions.md)) |
| Maturity | experimental |

The matrix of all adapters is in [Adapters](README.md).

## Capabilities and operations

| Capability | Behavior |
| --- | --- |
| `receive` | `Runtime.Receive` keeps a change-notification subscription on the inbox and runs a delta query every round from the stored delta link |
| `receive_http` | `Runtime.Webhook` answers Graph's `validationToken` check and takes change and lifecycle notifications |
| `history` | `Runtime.Import` lists the inbox oldest first by `receivedDateTime` through `@odata.nextLink` paging; received mail only, as the receiver accepts it; a conversation reads the sender's mail ([History](#history)) |
| `send` | `Messages.Reply`: a reply to a stored message is `createReply`, an update and `send`; other routes get a new draft |
| `reconcile` | After a lost answer, looks up the draft's attempt property in Sent Items. Settles after a 5 minute window |
| `start_conversation` | `Conversations.Start` sends to an address that never wrote in, as a new draft |
| `reply_recipients` | `Messages.Reply` with `Recipients` also addresses Cc, Bcc and, replying to all, the parent's other recipients; see [Reply recipients](#reply-recipients) |
| `raw_read` | `Messages.Raw` reads the message on demand: `GET /messages/{id}/$value`, the MIME form Exchange serves for the stored message, with the immutable ID the adapter reports. Exchange may have re-encoded the message, so the bytes are Graph's copy of it. Bounded at 25 MiB, the largest single read the adapter makes; a message over the bound is `permanent` with reason `too_large`, never truncated. Nothing is kept. `Mail.ReadWrite` covers the read |
| `extension_execute` | `Extensions.Execute` runs `microsoft.mail.categories`: a message update (`PATCH`) replacing one message's `categories`; and `microsoft.mail.reply_draft`: a reply kept as an Outlook draft (`createReply` then `PATCH`, `PATCH`, `DELETE`), never sent; and `microsoft.mail.unsubscribe`: reads one message's MIME form (`$value`) and unsubscribes by RFC 8058 one-click or by a mailto in the sender's organization |
| `extension_query` | `Extensions.Query` runs `microsoft.mail.mailboxes`: the mail folders and master categories as `model.ProviderMailbox` ([Provider mailboxes](#provider-mailboxes)) |
| `microsoft.mail.mailboxes` | Gates the list: declared, with `extension_query`, when the adapter can list folders and categories |
| `microsoft.mail.mailbox_create` | Gates the create: declared, with `extension_execute`, when the adapter can create a folder or category |
| `archive` | `Conversations.Archive`: each message of the conversation moves to the well-known `archive` folder; unarchive moves it to the undo folder (`undo_folder`, default the inbox) |
| `move` | `Conversations.Move`: each message moves to a destination, a mail folder ID or a role (`inbox`, `archive`, `trash`, `junk`); `folder` shape, label changes are refused |
| `read_state` | `Conversations.SetRead`: `PATCH` of `isRead` on each message |
| `flag` | `Conversations.Flag`: `PATCH` of the follow-up `flag` (`flagged` / `notFlagged`), kind `star`; `PATCH` of `importance` (`high` / `normal`), kind `important` |
| `trash` | `Conversations.Trash`: each message moves to Deleted Items; untrash moves it to the undo folder. Never a permanent delete |

## Not declared

Reason classes are defined in [Adapters](README.md#why-a-capability-is-not-declared).

| Capability | Reason | Why |
| --- | --- | --- |
| `edit` | `provider` | A sent mail cannot be changed at the recipient; `PATCH /messages/{id}` changes the mailbox's own copy only |
| `delete` | `provider` | A sent mail cannot be recalled; deleting the mailbox's own copy is not a delete at the peer |
| `react` | `provider` | Outlook reactions have no Graph API |
| `receipt` | `provider` | Read receipts are optional and unverifiable; none is sent |
| `membership` | `engine` | Mail membership means extra `To`, `Cc` or `Bcc` recipients kept on the conversation for every later send. A reply names its own extra recipients (`reply_recipients`); nothing keeps them on the conversation |
| `refetch` | `provider` | A mail never changes after delivery |
| `work_item_act` | `provider` | Mail has no work items |
| `internal_note` | `provider` | Mail has no private comments; notes stay in uma |
| `probe_permissions` | `not built` | The token's roles and scopes could be probed |
| `stream` | `provider` | Mail has no streamed messages |
| `send_ephemeral` | `provider` | Mail has no message only one reader sees |
| `signal` | `provider` | Mail has no typing or presence |
| `respond` | `provider` | Mail has no interactions to answer |
| `respond_follow_up` | `provider` | As `respond` |

## Provider conversation state

Every state verb changes the mailbox's own view: scope `account`, authorized
as `conversation.state_account`. The conversation is the messages of it uma
holds on the channel, so a verb is one Graph call per message, named by its
immutable ID.

| Verb | Request | Limits reported |
| --- | --- | --- |
| archive | `POST /messages/{id}/move` `{"destinationId": "archive"}` | scope `account` |
| unarchive | the same to the undo folder: `undo_folder`, default `inbox` (Graph keeps no record of the folder a message came from) | |
| trash, untrash | the same to `deleteditems`, or the undo folder | |
| move | the same to a folder ID, or the well-known name of a role | shape `folder`; roles `inbox`, `archive`, `trash`, `junk` |
| read, unread | `PATCH /messages/{id}` `{"isRead": bool}` | scope `account` |
| flag, unflag | `PATCH /messages/{id}` `{"flag": {"flagStatus": "flagged" \| "notFlagged"}}` | kinds `star`, `important` |
| flag, unflag (`important`) | `PATCH /messages/{id}` `{"importance": "high" \| "normal"}` | |

- Every call sends `Prefer: IdType="ImmutableId"`: an immutable ID does not
  change when a message moves between folders of the mailbox, so no move
  reports a reference change. A move to an archive mailbox or an export and
  re-import does change it and is out of scope. If Graph ever answered a move
  with another ID, the outcome reports the change.
- The `important` kind is the message importance: flag sets `high`, unflag
  sets `normal` (an unflag also clears importance a sender set `high`). There
  is no pin.
- A lost answer, a 5xx other than 503, or a failure after some messages were
  changed leaves the outcome unknown. Reconcile reads each message back
  (`GET /messages/{id}`, `parentFolderId`, `isRead`, `flag`, `importance`): a move is applied
  when the message is in the destination, a read or flag when it shows the
  value. A message that is gone is a final `rejected` with `not_found`.
- Inbound: next to the inbox delta, the receiver runs a delta query on each
  watched folder: the inbox, Archive and Deleted Items, plus the
  configuration's `watched_folders` (mail folder IDs or well-known names; one
  the mailbox lacks is skipped). Folders are resolved once to their ID, name and
  role and kept, with their delta links, in the receiver state. Each changed
  message is reported as a partial `conversation.state` envelope: its folder
  (with the `archive` or `trash` role), `archived`, `trashed`, read, the
  `star` flag (follow-up flagged) and the `important` flag (importance
  `high`). A message that is `@removed` from one folder is read back by
  ID: found, it is reported where it is now, so a move is never a delete (the
  folder it arrived in reports the same state, once); a 404, a permanent
  delete, reports nothing. A message moved to an unwatched folder is reported
  there but its later changes are not seen. New inbox mail the inbox delta
  delivers in the same round is not reported again while it is in its default
  state (inbox, unread, unflagged, normal importance).
- A folder's first delta round lists the whole folder and reports the
  messages last modified within the initial synchronization window; later
  changes are all seen.

## Extension effects

Two effects, version 1, run through `Extensions.Execute`. The factory refuses
invalid fields at the call (`adapter.ActionValidator`), with nothing stored;
`Execute` applies the same rules again at the attempt.

| Effect | Authorized as | Rules |
| --- | --- | --- |
| `microsoft.mail.categories` | `conversation.label` | `set` is the whole new list of Outlook category names (the mailbox's master categories, by display name), each once, none empty; `[]` clears the message's categories. `set` is required. Other fields are ignored and never sent |
| `microsoft.mail.reply_draft` | `message.reply` | `text`, `draft` and `delete`: see [Reply drafts](#reply-drafts). Other fields are ignored |
| `microsoft.mail.unsubscribe` | `extension.execute` | Optional `method`: `auto` (the default: one-click when the message is eligible, else mailto), `one_click` or `mailto`. See [Unsubscribe](#unsubscribe) |

`set` replaces the list because Graph's message update replaces `categories`
([update message](https://learn.microsoft.com/en-us/graph/api/message-update)):
an `add`/`remove` form would need a read before the write and could lose a
change made in between. A caller that wants to add reads the current list
first (Graph `GET /messages/{id}?$select=categories`) and sets the union.

The effect names no message itself: it applies to the effect's target when
the engine sets one naming a provider message of the channel, else to the
conversation's reply route's parent message (the Graph message the peer
wrote, which `Messages.Reply` also answers). A conversation whose route has
no parent message (nothing received from the peer yet) gets a final
`rejected` outcome with `validation`. `Extensions.Execute` sets no target
today, so the parent message is the one updated.

uma's own labels (`Conversations.AddLabels`, `Labels.*`) stay local and
canonical: the effect reads or writes none of them, and no inbound category
change maps back to one.

Outcomes: the update is idempotent (the list replaces whatever the message
held), so a lost answer or a 5xx is `transient` and the attempt is repeated;
it is never `outcome_unknown` and never reconciled by reading the message
back. A message Graph no longer holds (404) is a final `rejected` with
`not_found`; a token without `Mail.ReadWrite` on the mailbox (403)
`unauthorized`, never retried; 429 and 503 `rate_limited` and `transient`
with the stated `Retry-After`.

## Provider mailboxes

An Exchange mailbox is a mail folder (`model.ProviderMailbox`, kind `folder`) or
a master category (kind `category`). Two extensions list and create them, each
gated by the capability of its own name, so a host reads per channel from
`Configuration().Capabilities` whether it can list (`extension_query` and
`microsoft.mail.mailboxes`) or create (`extension_execute` and
`microsoft.mail.mailbox_create`).

| | List | Create |
| --- | --- | --- |
| Extension | `microsoft.mail.mailboxes` (`Extensions.Query`) | `microsoft.mail.mailbox_create` (`Extensions.Execute`, target the channel) |
| Provider calls | `GET .../mailFolders`, `.../childFolders` per folder with children, `.../outlook/masterCategories`, and one `GET .../mailFolders/{well-known}` per well-known folder for its role | `POST .../mailFolders` or `.../childFolders` (a folder), `POST .../outlook/masterCategories` (a category) |
| Authorized as | `extension.query` | `extension.execute`; a plugin actor's create can be held, like any write |
| Permissions | `Mail.Read`, `MailboxSettings.Read` for categories | `Mail.ReadWrite`, `MailboxSettings.ReadWrite` for categories |
| Data | none | `{"name": "Clients/Acme", "kind": "folder"}`; `kind` is `folder` (the default) or `category` |
| Result | every folder (ID, path, role) and category (display name as ID) | the object's reference in the operation's provider references: the folder's immutable ID, or the category's display name |

- A folder's name is its path from the mailbox root, with "/"; the ID is the
  immutable folder ID, the one a move's `destinationId` takes. The roles are
  `inbox`, `archive`, `trash` (Deleted Items), `junk`, `sent` and `drafts`,
  read from the well-known folder names. A category is referenced by display
  name, which is its ID; it has no nesting, and its name is taken whole, "/"
  included. A new category takes the first color preset.
- A folder create makes each missing parent, under the root or under a
  well-known folder (`Inbox/Clients`). It is idempotent by name at every
  level, compared without regard to case as Exchange does: what exists is
  reused, and a create that stops midway leaves the parents it made and
  finishes when repeated. A level that loses a race (409) reads the winner
  back.
- The list reads at most 8 levels and 2000 folders; a larger tree is a
  `permanent` error, never a cut list.
- A host registers `Factory.Extensions()` with the factory: the adapter
  declares the two capabilities, which need their definitions.

## Reply drafts

`microsoft.mail.reply_draft` leaves a reply in the mailbox's Drafts folder for
a person to finish and send in their mail client. It sends nothing. It
exposes, over the adapter's existing draft path, the same data as
`google.mail.reply_draft`:

| Call | Data | Graph calls |
| --- | --- | --- |
| create | `{"text": "..."}` | `POST .../messages/{id}/createReply`, then `PATCH` of the draft with the text, the route's address as the only recipient and the attempt property |
| update | `{"text": "...", "draft": "<draft ID>"}` | `PATCH` of that draft's `body` alone |
| delete | `{"draft": "<draft ID>", "delete": true}` | `DELETE` of that message |

A create also takes `reply_all` (boolean), `cc` and `bcc` (lists of bare
addresses), which address the draft as a send's [reply
recipients](#reply-recipients) do; they are invalid on a deletion, and an update
leaves the draft's recipients alone.

`text` is required unless `delete` is set, and must be absent with it; a
deletion needs `draft`; blank text, a non-string field and a padded draft ID
are refused at the call, with nothing stored.

A create is the first half of a send: `createReply` on the peer's message
(Graph sets the reply subject, `In-Reply-To`, `References` and conversation)
and the update of the new draft, then no send call. A route without a parent
message, or one whose message is gone, becomes a new draft (`POST /messages`).
An update changes the body only, so a recipient or subject a person changed
since stays; the body is replaced wholesale. A delete moves the draft to
Deleted Items (`DELETE` is not permanent for a mailbox message).

The outcome's provider references name the draft: kind `draft` under the
provider message namespace (`microsoft.DraftRef`), carrying the draft's
immutable Graph ID. The operation keeps them (`Operation.ProviderReferences`);
a host passes that ID as `draft` in a later call to update or delete the same
draft. Every call is its own operation: the same operation ID with the same
payload replays the stored result, and with another payload conflicts, as for
every effect, so a host uses a new operation ID for each update.

Ownership. The draft ID is the caller's, so an update or a deletion first
reads the message (`$select=isDraft,conversationId,toRecipients`, with the
attempt property expanded) and acts only on a draft this adapter made for the
conversation: still a draft, carrying the attempt property of a reply draft
of the effect's tenant, and in the conversation of the route's parent message
(`conversationId`; a route whose parent is gone binds by the draft's only
recipient being the route's address). Any other message, a draft a person
wrote and a delivered or sent message included, is a final `rejected` with
`validation`, and nothing is changed. The adapter keeps no record of the
drafts it made, so this is a check of the draft itself, not of the operation
that created it.

Outcomes: update and delete are idempotent, so a lost answer or a 5xx is
`transient` and the attempt is repeated. A create has no idempotency key
(`createReply` and `POST /messages` each make a draft): a lost answer is
`transient` too and the repeat can leave an extra unsent draft in Drafts; the
effect is never `outcome_unknown` and `Reconcile` answers `failed` with a
transient failure without a read. A draft Graph no longer holds (404) is a
final `rejected` with `not_found` on update, and a success on delete (the
ownership read answers 404). A message that is no draft any more (the person
sent it) is refused by the ownership check before any change; Graph also limits `body` to messages with
`isDraft` true ([update message](https://learn.microsoft.com/en-us/graph/api/message-update)). A token without `Mail.ReadWrite` (403) is
`unauthorized`, never retried; 429 and 503 are `rate_limited` and `transient`
with the stated `Retry-After`.

Sources: [createReply](https://learn.microsoft.com/en-us/graph/api/message-createreply)
(`201 Created`, `Mail.ReadWrite`),
[update message](https://learn.microsoft.com/en-us/graph/api/message-update)
(`PATCH`, `200 OK`), [delete message](https://learn.microsoft.com/en-us/graph/api/message-delete)
(`204 No Content`).
### Unsubscribe

`microsoft.mail.unsubscribe` unsubscribes the mailbox from the sender of one
message, by the one method the message's own headers allow, and by no other.
It applies to the effect's target or the reply route's parent message, as the
categories effect does. The adapter reads that message again from Graph (its
MIME form, `GET /users/{mailbox}/messages/{id}/$value`,
[Get MIME content of a message](https://learn.microsoft.com/en-us/graph/outlook-get-mime-message))
at the attempt and decides from its headers; the caller supplies no header,
URL or address.

| Method | Only when | What the adapter does |
| --- | --- | --- |
| One-click ([RFC 8058](https://www.rfc-editor.org/rfc/rfc8058)) | the message has exactly one `List-Unsubscribe` field naming an `https` URI (default port, no credentials, a public host name or address), exactly one `List-Unsubscribe-Post` field whose value is `List-Unsubscribe=One-Click`, and a valid DKIM signature by the sender's organization that covers both fields | One `POST` of the form body `List-Unsubscribe=One-Click` (`application/x-www-form-urlencoded`) to that URI through a client of the adapter's own (not the host's), guarded at connect time (see below): no cookies, no authorization, no other data, 15 second bound for the whole exchange |
| Mailto ([RFC 2369](https://www.rfc-editor.org/rfc/rfc2369), [RFC 6068](https://www.rfc-editor.org/rfc/rfc6068)) | the first `mailto` URI of the header names one address, no `to`, `cc` or `bcc` field, a single-line subject, and the address is in the sender's organizational domain | A text message from the mailbox to that address, with the URI's `subject` and `body` (default `unsubscribe`), through the adapter's own draft and send, so it is reconciled by its attempt property like any send |

Rules that hold for both:

- Every other URI in the header is never followed: `http`, `ftp`, `tel` and
  any other scheme, and an `https` URI when the message does not qualify for
  one-click. The adapter makes no `GET`.
- A redirect from the one-click endpoint is never followed. RFC 8058 section
  3.1 has senders not redirect; the client's redirect following and cookie jar
  are off, and a `3xx` answer is a final refusal.
- The target URI is chosen by whoever sent the message, so the request is
  guarded against reaching the host's own network. Before any request the URI's
  text is checked (`https`, default port, no credentials, a public-looking
  host: no loopback, private, link-local, shared or reserved literal address,
  no single-label or `.local`/`.internal` name): a failure is the usual
  `rejected` with `validation`. At connect time the adapter resolves the host
  itself, refuses the whole name when any answer is loopback, private,
  link-local (the cloud metadata address included), shared (100.64/10),
  unspecified, multicast, documentation or reserved, an IPv4-mapped or
  IPv4-compatible form of one, or NAT64 or 6to4, and dials only an address it
  vetted, so a name that changes its answer between check and dial reaches
  nothing it was not vetted for (`rejected`, `validation`, no request made).
  The client uses no proxy, not even one the environment names, and keeps no
  connection. The host's HTTP client is never used for this request; a host
  that must send it through an egress proxy has no way to say so yet.
- The **organizational domain** is the public suffix plus one label, from
  `golang.org/x/net/publicsuffix` (already a dependency of the module). It
  includes the privately operated suffixes, so `a.github.io` and `b.github.io`
  are not one organization. The sender is the message's single `From` address;
  a message with none or several has no sender to compare with. The `From`
  header is the sender's claim: the mailto rule limits where the mail goes
  (the sender's own domain), it does not authenticate who sent it.
- DKIM is checked with `github.com/emersion/go-msgauth`: a signature must
  verify (the key is looked up in DNS), list both fields in `h=`, and have an
  `d=` whose organizational domain is the `From` domain's. RFC 8058 section 4
  has a receiver not offer one-click for a message whose signature does not
  cover the two fields. A message with two `List-Unsubscribe` or two
  `List-Unsubscribe-Post` fields is refused (a signature covers one instance).
  The mailto method needs no signature.
- Graph documents `$value` as a MIME *representation* of a message Outlook
  does not store as MIME. A signature that no longer verifies on that copy
  refuses one-click; the mailto method stays available. Whether Graph's copy
  ever verifies is not checked against a real signed message.
- A message whose answer is over the response bound (8 MiB) cannot be verified
  and is a final `rejected` with `permanent`.

Every refusal is a final `rejected` outcome with a `validation` failure whose
safe message names the rule (no header, an `http` URI, no `https` URI, a
missing or different `List-Unsubscribe-Post`, a mailto outside the sender's
organization, a mailto adding recipients, an unsigned or uncovered message), and
is decided from the message's headers before any request to the unsubscribe
target. The only requests made earlier are the read of the message from Graph
and the DNS lookups of the DKIM keys. A DNS failure while looking up a key is
`transient`, so the attempt is repeated. A message Graph no longer holds is a
final `rejected` with `not_found`.

The one-click `POST` is idempotent: a conforming sender treats a repeat as a
no-op, so a lost answer, a `408`, `429` or `5xx` is `transient` and the
attempt is repeated; any other `4xx` is a final `rejected` with `permanent`; a
`2xx` is `provider_accepted`. A mailto send is answered as every send is:
`outcome_unknown` after an ambiguous answer, reconciled by the attempt property.

The effect is authorized as `extension.execute`: it sends a request to a third
party, so a host grants it on its own and not with `conversation.label`. A host
that wants a confirmation before the request, as RFC 8058 section 3.2
requires ("MUST NOT perform a POST on the HTTPS URI without user consent"),
asks its user before it calls the effect: the call is the consent. Reading the
message needs `Mail.ReadWrite` (or `Mail.Read`), a mailto `Mail.Send`.

The headers are kept on the stored message: the `microsoft.mail.message`
payload carries `list_unsubscribe` and `list_unsubscribe_post` as Graph reports
them in `internetMessageHeaders` (a repeated field joined by a space), so a host
can tell that an unsubscribe is possible before it calls the effect. They
decide nothing; the effect reads the message again from Graph.

## Permissions

- **Provider.** Every unit of work resolves an access token through
  `ports.Credentials` for purpose `graph`, accepting OAuth 2 only, delegated or
  app-only; the host acquires and refreshes it. A 401 resolves once more with a
  fresh token; a second 401 is `unauthorized` and never retried. The webhook's
  client state derives from a secret resolved for purpose `graph.notifications`.
  App-only tokens need `mailbox` in the configuration, since `/me` requires a
  signed-in user. `Mail.ReadWrite` covers the message update behind the
  categories effect, the draft calls behind the reply draft effect, the state verbs and the folder delta queries; no further
  permission is needed.
- **uma.** The receiving principal needs `channel.receive`; replies are
  authorized as `message.reply`, category changes as `conversation.label` on
  the conversation, state verbs as `conversation.state_account`.

## Configuration

`microsoft.mail.config` version 1, as JSON in the channel configuration. It
holds no secret.

| Field | Required | Meaning |
| --- | --- | --- |
| `mailbox` | with app-only tokens | User ID or principal name (`/users/{mailbox}`). Empty addresses the signed-in user (`/me`) and needs a delegated token |
| `addresses` | no | Recipient addresses the mailbox answers to, primary first; the first found in `To` or `Cc` is an inbound message's destination |
| `cloud` | no | `global` (default), `usgov`, `usgov_dod` or `china` |
| `notification_url` | no | Public HTTPS URL where the host mounts the channel's webhook. Empty disables notifications: the receiver polls delta only |
| `lifecycle_notification_url` | no | Receives lifecycle notifications; default `notification_url` |
| `subscription_lifetime_ms` | no | Requested lifetime of each subscription; default 72 hours, from 45 minutes to just under 7 days; renewal falls a third of it (15 minutes to a day) before expiry, +-10% |
| `poll_interval_ms` | no | Wait between delta rounds; default 5 minutes, each wait spread by +-10% so channels do not poll in phase |
| `initial_sync_window_ms` | no | How far back the first delta round reaches; default 1 hour |
| `undo_folder` | no | Mail folder ID or well-known name (for example `junkemail`) that unarchive and untrash move messages to; default `inbox`. Rejected at load when it has whitespace, quotes or path separators |
| `history_mailboxes` | no | Mail folder IDs or well-known names (`inbox`, `archive`, a folder ID) a history import walks, in this order; empty is `inbox`. `junkemail`, `deleteditems`, `drafts`, `sentitems` and `outbox` are refused at load by name, and by ID when read; repeats are refused |
| `watched_folders` | no | Mail folder IDs or well-known names watched for provider state besides the inbox, Archive and Deleted Items |

Host options (`microsoft.Options`): `Endpoint` (override, for tests) and
`PageSize` (delta page size; default 50). Calls go through `uma.Config.HTTP`;
`Open` fails without it.

## Receiving

Notifications are the fast path and the delta query fills every gap. Both use
immutable IDs, so one message has one ID whichever path sees it and ingress
deduplicates on it. The webhook compares each notification's client state in
constant time and fetches the notified message from the channel's own mailbox.
The subscription is renewed before expiry, recreated when Graph removes it and
reauthorized on request.

**Receiver status.** The instance implements `adapter.ReceiverReporter` and
`adapter.ReceiverDegradationReporter`. It reports connected once a round
completed, with or without push. A failed subscription create or renewal
does not stop the receiver, since delta tracking covers the mailbox, but it
is no longer silent: `Runtime.ReceiverStatus` and `Runtime.ReceiverState` read
`Link` `degraded` with `Detail` one of

| `Detail` | Cause | When reported |
| --- | --- | --- |
| `microsoft.mail.subscription_rejected` | Graph refused the create or renewal (other 4xx): a notification URL it cannot validate, a missing permission | at the first failed attempt |
| `microsoft.mail.client_state_missing` | the notification secret the client state derives from is missing, empty or not a secret | at the first failed attempt |
| `microsoft.mail.subscription_unavailable` | transient failures (5xx, throttling, no answer) | after 3 failed rounds in a row, or at once when the live subscription would expire before the next attempt |

The next successful create or renewal clears it and the link reads connected
again. Polling, retries and the lease are unchanged; the signal is advisory.

An inbound message becomes a text part (the body requested as text), one
structured descriptor part per attachment (metadata) and a payload with the
subject, `conversationId`, recipients and `Authentication-Results`. Threading
uses the RFC 5322 headers only; `conversationId` is a hint and never selects a
conversation.

The bytes of file attachments are downloaded one at a time from the
attachment's raw content (`.../attachments/{id}/$value`, which carries no base64
overhead) and staged through the engine, in the inbox of the message's
destination. Each staged attachment is named in an attachment part after the
descriptors, and its descriptor carries `"staged": true`. Only file attachments
are staged. An item attachment (an Outlook message, event or contact) does have
a raw form, its MIME form at `$value`, but the adapter does not download it: it
is described only, as a policy and not a Graph limit. A reference attachment (a
link) has no raw form, and its `$value` is HTTP 405; it is described only too.
So is a file attachment over the bounds in [Limits](#limits) (never downloaded), one that is gone or
refused by the engine's attachment checks (size limit, media types), and every
attachment when the host gave the engine no blob store, which is found out on
the first one. The message is delivered all the same. The attachment list
is read through every page Graph names with `@odata.nextLink` (its page size is
not documented for this list), within the bound in [Limits](#limits); a link to
another origin is not followed. A page, download or staging failure a repeat
can fix (Graph or the blob store is unavailable, throttling) fails the fetch like a refusal by the sink: the notification is answered 503 and
the delta round meets the message again. Staging is idempotent per message:
a redelivery restages the same attachments. It needs `Mail.Read`, which
`Mail.ReadWrite` includes.

An attachment's name is the sender's suggestion, so it is reduced to a display
name before it is recorded, in the descriptor and on the staged attachment: only
the last path component is kept (`../../etc/passwd` becomes `passwd`), control
and invisible formatting characters, right-to-left overrides included, are
dropped, the text is normalized to Unicode NFC, a Windows device name such as
`CON` gets a leading `_`, and a name over 255 bytes is cut with its extension
kept. A name with nothing left is recorded as `attachment` on the staged attachment and left out of the descriptor. The staged bytes sit under a key the engine
chooses, never one derived from the name; a host that writes the name to a file
still applies the naming rules of its own filesystem.

## Identities and start recipients

A sender is keyed in `microsoft.mail.address` by its `From` address with the domain
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

An address that is not a bare address is kept as given, never refused by the
normalization: a send decides on it as before, and it keys the same way an
inbound sender of that spelling does (an Exchange legacy distinguished name is
one).

**Identities from before this rule.** Senders were keyed on the `From` address
byte-for-byte. A sender whose address has an uppercase letter in the domain,
such as `Peer@Example.org`, now keys as `Peer@example.org`: uma finds no
identity under the new key for a returning sender, so the next message from
them creates a new identity and contact, and starts a conversation on its own
unless header threading ties it to an existing one. The old identity and its
conversations are untouched, and uma does not look them up under the new key,
migrate or relabel them. An operator can join the two contacts afterwards with
`Contacts.PreviewMerge` and `Contacts.Merge`; the conversations stay with the
identities that own them. Mail from an address with a lowercase domain
keys as before. A start replayed under its operation key across the upgrade with
a mixed-case domain fingerprints differently and is refused as a conflict;
start again under a new key.

## Sending

Graph assigns the `Message-ID` at draft creation and the adapter cannot set it;
the outcome reports it with the Graph ID. Each draft carries a named extended
property holding the operation attempt, which `Reconcile` looks up in Sent Items
after a lost answer. `Retry-After` is reported to the operation ledger and never
waited on.

## Reply recipients

`Messages.Reply` with `Recipients` (`model.ReplyRecipients`) addresses the
draft with more than the route's address:

| Field | Effect |
| --- | --- |
| `cc` | Added to the draft's `ccRecipients` |
| `bcc` | Added to its `bccRecipients` |
| `reply_all` | `createReplyAll` instead of `createReply`: the draft starts with the parent's recipients, and the update sets its `To` to the route address and the parent's other `To` recipients and its `Cc` to the parent's `Cc` recipients, each minus the mailbox's own addresses (`addresses`, and `mailbox` when it is an address) and any address already named. A reply whose parent Graph no longer holds becomes a new draft addressed to the route and the explicit recipients |

The route address is always the first `To`. Addresses are compared without
case, and at most 50 in all are sent; more is a final `validation`, found
before any draft is made for explicit addresses and, for a reply to all that
outgrows the bound, after the draft is made and then deleted. The reply draft
effect takes the same fields on a create. The capability reports `{"reply_all":
true, "cc": true, "bcc": true, "max_recipients": 50}` as its limits
(`model.RecipientLimits`).

## History

`Runtime.Import` reads past mail through `HistoryReader`; nothing is sent or
changed at Graph.

| | |
| --- | --- |
| Scope | the inbox of the configured mailbox, the receive scope, unless `history_mailboxes` lists other folders (the inbox only if listed); no other folder is read, and a draft is skipped as inbound skips it. A message copied into several listed folders is read in the first of them (a lookup by Internet message ID in the folders walked before). A conversation import reads the sender's mail (`from/emailAddress/address`) |
| Window | `Since <= t < Until` on `receivedDateTime`; Graph times carry whole seconds, so the bounds are rounded up to the second |
| Order | oldest first (`$orderby=receivedDateTime asc`) within a folder; with a list, folder by folder in the listed order, each read to its end (Graph cannot order one list across folders), so list the folder holding parents before the one holding replies; no delta, which has no upper bound and no ascending order |
| Cursor | opaque. It holds the paging parameter (`$skiptoken` or `$skip`) and token of the last `@odata.nextLink`, the `$top` the read opened with, and a count of that page's messages already returned, so a smaller `Limit` takes a page in parts. With a list it names the folder being read, which must be listed. It never holds a URL: the request is rebuilt from the call each time, and a `nextLink` that is not the next page of that list is refused |
| Gap | an item of the list without an ID is a gap; a message or an attachment list gone between pages is simply absent |
| Errors | a mailbox the credential cannot reach (403, 404 on the first page) is `Unavailable`. A cursor not issued for the read, or one that names another parameter, a page size past the maximum or a position past the page, is `Validation` before any call |
| Rate limits | 429 and 5xx are returned with `Retry-After`, never waited on; the host paces. Graph directs bulk extraction to Graph Data Connect; an import is bounded by `Limit` and the host |

Requests carry the receiver's headers (`Prefer: IdType="ImmutableId"` and the
text body preference), so an imported message and the received one are one
message. File attachments are staged through the request's stager as the
receiver stages them (same keys, bounds and refusals, so a message imported and
received holds its bytes once): one `$value` request per staged attachment, a
throttled one returned with its `Retry-After` and the cursor kept. Without a
stager they are described only.

Cassette status: `TestRecordedGraphHistory` replays cassettes recorded against
a real mailbox over a fixed window holding only messages created for it; see
[Record Microsoft Graph cassettes](../recording/microsoft-graph.md#record-the-history-cassettes).
They confirm `lt` on `receivedDateTime` with `$orderby asc` on the inbox and
`internetMessageHeaders` in the `$select` of a list.

## Limits

| What | Limit |
| --- | --- |
| API response body read | 8 MiB; 25 MiB for one attachment's raw content |
| Attachments staged per message | 10, in message order; the others are described only |
| Size of a staged attachment | 25 MiB; a larger one is described only, never downloaded, and uses none of the 10 |
| Attachments described per message | 53; the payload's `attachments_omitted` counts the others |
| Pages of the attachment list read | 20, following `@odata.nextLink`; a list longer than that is cut there and `attachments_omitted` counts only the attachments read |
| Subscription lifetime | 45 minutes to just under 7 days |
| Delta page | `PageSize`, default 50 |
| History page | `Limit` envelopes, listed with `$top` of at most 1000 |
| Watched folders | inbox, Archive, Deleted Items and `watched_folders`; one delta link each |
| Initial synchronization | the last hour by default |

## How it is tested

The shared adapter suite and package tests run against a scripted Graph
service. Five recorded tests replay cassettes recorded against a real Exchange
Online test mailbox:

| Test | Records |
| --- | --- |
| `TestRecordedGraph` | send, Sent Items reconcile, the categories effect, every state verb with its reconcile, and the delta rounds |
| `TestRecordedGraphHistory` | the history reader over a fixed past window: paging, the attachment of one message |
| `TestRecordedGraphInbound` | received mail from the peer: the first delta round with bodies and a staged attachment, a later round that follows the delta link, a reply to a received message (`createReply`) and its Sent Items lookup, a trash that the next round does not deliver again |
| `TestRecordedGraphReplyDraft` | the reply draft effect: a send, `createReply` and the draft `PATCH`, a body update, a delete and the `404` of a second delete; skips until recorded |
| `TestRecordedGraphUnsubscribe` | the mailto unsubscribe over a window: the history read, the MIME read (`$value`), the draft and send of the unsubscribe message |
| `TestRecordedGraphErrors` | Graph's refusals: a message or folder that does not exist (404), a malformed ID (400), a delta token Graph cannot read (400), a rejected token (401), a subscription Graph refuses (400) |

`TestRecordedScenarioOverFake`, `TestRecordedInboundOverFake` and
`TestRecordedHistoryOverFake` run the first three scenarios against the
scripted service, as `TestRecordedUnsubscribeOverFake` does the unsubscribe one.
The one-click request, the DKIM checks and every unsubscribe refusal stay on a
local stand-in for the sender's endpoint and a locally generated signing key. Not recorded, and kept on the scripted service only:

- the change-notification webhook (validation token, client state,
  lifecycle events) and subscription creation, renewal and deletion: Graph
  calls a public HTTPS endpoint the recording has none of, and a created
  subscription is tenant state;
- throttling (`429`, `Retry-After`), server errors and a lost send answer:
  the test mailbox cannot be made to fail on request, and a repeated send is
  not safe to provoke;
- delta token expiry (`410`): a token expires only after days; a corrupt one
  answers `400`, which is recorded;
- threading from `In-Reply-To`, `References` and `Auto-Submitted`, and
  `Authentication-Results`: a message created through the API carries no
  Internet headers of its own, and none can be set on it;
- a mailbox holding many attachments, an item or reference attachment, an
  attachment over the staging bound, and attachment list paging;
- shared adapter suite cases that drive the provider (deliveries of many
  senders, visibility, revoked permissions): they need a harness that
  places mail and state in a real mailbox on demand, which the recorded
  scenarios do not.

To record, follow
[Record Microsoft Graph cassettes](../recording/microsoft-graph.md).

## Known limits

- The `size` Graph lists for a file attachment is larger than its content
  (197 for 33 bytes recorded): it counts the attachment object. A staged
  attachment reports the downloaded length; the `size` of an attachment part
  and the bound of 25 MiB that decides whether to download use Graph's.
- A delta link whose token Graph cannot read is a `400` error of the round,
  not a restart: only an expired token (`410`) restarts it.
- Attachments stage only through an engine with a blob store; without one they
  are described only, after one wasted download.
- Item attachments (embedded messages, events, contacts) are described only:
  their MIME form (`$value`) exists but is not staged, by choice.
- Text only outbound, no attachments or HTML.
- The `Message-ID` of a sent message is Graph's, known only after the send.
- The reply draft effect is unverified against the real service: that
  `createReply` on a message the mailbox itself sent answers a draft, the
  error code Graph gives a body update of a message that is no draft, and
  that the expanded attempt property and `conversationId` of a `createReply` draft read back as the ownership check expects, and that a deleted draft answers `404` on a read. A lost create answer
  can leave an extra unsent draft.
- The categories effect replaces the whole list (no add or remove) and
  reaches only the conversation's parent message (or the target the engine
  names); it creates no master category. Whether Graph refuses a name that
  is no master category, or keeps it as an uncoloured category, is not
  verified against the real service.
- Unarchive and untrash restore to the configured `undo_folder` (default
  the inbox), not to the folder the message was in. Folders are resolved by ID or well-known name; the folder tree is
  not walked. State reports are per message (partial); permanent deletes and
  moves to an archive mailbox are not state.
- Experimental: configuration may change ([Compatibility](../compatibility.md)).
