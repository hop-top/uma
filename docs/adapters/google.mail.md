# google.mail

`google.mail` is the Gmail API mail adapter, one channel per mailbox. It speaks
plain JSON over the host's HTTP client (no Google client library) and, unlike
IMAP and SMTP against Gmail, runs with separable OAuth scopes: `gmail.send` for
sending, `gmail.readonly` for receiving and `gmail.modify` for the labels
effect, the reply draft effect and the conversation state verbs (that one alone also covers reading and
sending).

| | |
| --- | --- |
| Key | `google.mail` |
| Package | `hop.top/uma/adapter/mail/google`, own module `hop.top/uma/adapter/mail/google` |
| Capabilities | `receive`, `receive_http`, `history`, `send`, `reconcile`, `start_conversation`, `reply_recipients`, `raw_read`, `extension_execute`, `extension_query`, `google.mail.mailboxes`, `google.mail.mailbox_create`, `archive`, `move`, `read_state`, `flag`, `trash` |
| Events | `message.received`, `delivery.updated` |
| Identity namespaces | `google.mail.address` |
| Sender kinds | person |
| Provider permissions | OAuth scopes `https://www.googleapis.com/auth/gmail.readonly`, `https://www.googleapis.com/auth/gmail.send`, `https://www.googleapis.com/auth/gmail.modify` |
| Message IDs and threading | assigned by Gmail; email header threading |
| Reply parent | reported: the first valid `In-Reply-To` msg-id; resolved to a stored message by its `Message-ID` |
| Interactions | none ([interactions](../interactions.md)) |
| Maturity | experimental |

The matrix of all adapters is in [Adapters](README.md).

## Capabilities and operations

| Capability | Behavior |
| --- | --- |
| `receive` | `Runtime.Receive` keeps a `users.watch` and reads `users.history.list` every round, so dropped notifications are filled in |
| `receive_http` | `Runtime.Webhook` takes the authenticated Pub/Sub push subscription's requests (the fast path) |
| `history` | `Runtime.Import` reads past inbox mail (or the labels of `history_mailboxes`) oldest first in day slices of `users.messages.list` (`labelIds=<label>`, `q=after: before:`), dated by `internalDate`; received mail only, as the receiver accepts it; a conversation reads the sender's mail (`from:`) |
| `send` | `Messages.Reply` calls `users.messages.send` with a raw message; a reply sets the parent's thread |
| `reconcile` | After a lost answer, searches Sent for the operation's `Message-ID` with `rfc822msgid:`. Settles after a 2 minute window, because search indexes new mail with a lag. For a state verb it reads the thread's labels back (see [Conversation state](#conversation-state)) |
| `start_conversation` | `Conversations.Start` sends to an address that never wrote in; the message opens a new thread under the configured `subject` |
| `reply_recipients` | `Messages.Reply` with `Recipients` also addresses Cc, Bcc and, replying to all, the parent's other recipients; see [Reply recipients](#reply-recipients) |
| `raw_read` | `Messages.Raw` reads the message on demand: `users.messages.get` with `format=raw`, the whole RFC 5322 message with every header and part, as Gmail holds it. Bounded like the receiver's own raw read (about 94 MiB of response, a 70 MiB message); a message over the bound is `permanent` with reason `too_large`, never truncated. Nothing is kept. Needs the readonly scope |
| `extension_execute` | `Extensions.Execute` runs `google.mail.labels`: `users.messages.modify` on one message; and `google.mail.reply_draft`: a reply kept as a Gmail draft (`users.drafts.create`, `update`, `delete`); and `google.mail.unsubscribe`: reads one message (`format=raw`) and unsubscribes by RFC 8058 one-click or by a mailto in the sender's organization; and `google.mail.forward`: reads one message (`format=raw`) and sends it to another address with the original attached as `message/rfc822` |
| `extension_query` | `Extensions.Query` runs `google.mail.mailboxes`: `users.labels.list`, the account's labels as `model.ProviderMailbox` ([Provider mailboxes](#provider-mailboxes)) |
| `google.mail.mailboxes` | Gates the list: declared, with `extension_query`, when the adapter can list labels |
| `google.mail.mailbox_create` | Gates the create: declared, with `extension_execute`, when the adapter can create a label |
| `archive` | `Conversations.Archive`: `users.threads.modify` removing `INBOX`, adding it back to undo |
| `move` | `Conversations.Move`, `labels` shape: `users.threads.modify` adding and removing label IDs; `users.threads.trash` for the `trash` role |
| `read_state` | `Conversations.SetRead`: `users.threads.modify` removing `UNREAD`, adding it for unread |
| `flag` | `Conversations.Flag`, kinds `star` and `important`: `users.threads.modify` adding or removing `STARRED` or `IMPORTANT` |
| `trash` | `Conversations.Trash`: `users.threads.trash`, `users.threads.untrash` to undo |

## Not declared

Reason classes are defined in [Adapters](README.md#why-a-capability-is-not-declared).

| Capability | Reason | Why |
| --- | --- | --- |
| `edit` | `provider` | A sent mail cannot be changed at the recipient; `users.messages.modify` changes labels only |
| `delete` | `provider` | A sent mail cannot be recalled; trashing the mailbox's own copy is not a delete at the peer |
| `react` | `provider` | Gmail reactions have no API |
| `receipt` | `provider` | Read receipts are optional and unverifiable; none is sent |
| `membership` | `engine` | Mail membership means extra `To`, `Cc` or `Bcc` recipients kept on the conversation for every later send. A reply names its own extra recipients (`reply_recipients`); nothing keeps them on the conversation |
| `refetch` | `provider` | A mail never changes after delivery |
| `work_item_act` | `provider` | Mail has no work items |
| `internal_note` | `provider` | Mail has no private comments; notes stay in uma |
| `probe_permissions` | `not built` | The token's granted scopes could be probed (`tokeninfo`) |
| `stream` | `provider` | Mail has no streamed messages |
| `send_ephemeral` | `provider` | Mail has no message only one reader sees |
| `signal` | `provider` | Mail has no typing or presence |
| `respond` | `provider` | Mail has no interactions to answer |
| `respond_follow_up` | `provider` | As `respond` |

## Conversation state

The five state capabilities carry what Gmail holds about a conversation apart
from uma's own status and labels (nothing here changes `Conversation.Status`
or `Conversation.Labels`). Every verb is **account scope**, so a host
authorizes it as `conversation.state_account`, and needs `gmail.modify`.

| Capability | Limits it reports |
| --- | --- |
| `archive`, `read_state`, `trash` | `scope: account` |
| `flag` | `scope: account`, `kinds: [star, important]` |
| `move` | `scope: account`, `shape: labels`, `roles: [inbox, archive, trash, junk]` |

**Requests.** A conversation is a set of Gmail messages, and Gmail changes
state per thread, so each verb is one `users.threads` call on the thread of
each of the conversation's messages, which Gmail applies to every message of
the thread. The adapter first reads the thread of each item
(`users.messages.get`, `format=minimal`); items in one thread are one call.
It never modifies a single message, so a thread with later messages is not
left half archived.

| Verb | Call | Labels |
| --- | --- | --- |
| archive, unarchive | `threads.modify` | remove, add `INBOX` |
| read, unread | `threads.modify` | remove, add `UNREAD` |
| flag, unflag `star` | `threads.modify` | add, remove `STARRED` |
| flag, unflag `important` | `threads.modify` | add, remove `IMPORTANT` |
| move, `labels` shape | `threads.modify` | the request's `add` and `remove` lists, or a destination label ID added; at most 100 per list |
| move, role `inbox` | `threads.modify` | add `INBOX` |
| move, role `archive` | `threads.modify` | remove `INBOX` |
| move, role `junk` | `threads.modify` | add `SPAM`, remove `INBOX` |
| move, role `trash` | `threads.trash` | |
| trash, untrash | `threads.trash`, `threads.untrash` | |

A flag kind other than `star` and `important`, a role Gmail does not resolve,
and a move naming neither a destination nor labels are a final `rejected` with
`unsupported` or `validation`, before any call. Gmail IDs are stable, so no
verb reports a reference change. An item Gmail no longer holds is skipped; a
conversation with none left is a final `rejected` with `not_found`.

**Outcomes.** `threads.modify` is idempotent, so a state the thread holds
already is applied. A 5xx, a 408, or a call that may have reached Gmail
without an answer is `outcome_unknown`; a failure before the request was
written is `transient`. A failure after the first thread of a conversation
that spans several threads is `outcome_unknown` too: the conversation may be
partly changed. 400 and 404 are final `rejected`; 403 (no `gmail.modify`) is
`unauthorized`; rate limits are `rate_limited`.

**Reconcile.** Reads every thread of the conversation back
(`users.threads.get`, `format=minimal`) and compares the label IDs of all its
messages with the verb: applied when every message shows the change, else
failed with a `transient` failure, which allows a new attempt. A conversation
Gmail no longer holds is a final `rejected` with `not_found`. A move to a
label is applied when every message has the added labels and none the removed
ones.

**Inbound.** The receiver reads `users.history.list` with `messageAdded`,
`labelAdded`, `labelRemoved` and `messageDeleted`, without a `labelId` filter
(a flag on a message outside the inbox is a change too; a message added
outside the inbox is not inbound mail and is skipped). For every thread a
label or delete record names, it reads the thread (`users.threads.get`,
`format=metadata`) and hands the sink a `conversation.state` envelope with
the thread's state as Gmail holds it at that read, complete, one per thread
per history page. The first full synchronization reports the thread of every
message it brings in. The envelope's event ID is `state:<thread>:<history ID>`.
Its conversation is the thread's newest message that is not a draft, which the
engine resolves to the conversation holding that message: a thread of
messages uma never stored records nothing. A thread Gmail no longer holds
reports nothing.

| State field | From the thread's messages (drafts left out) |
| --- | --- |
| `archived` | no message has `INBOX` (a trashed or spam thread has none either, so it is archived as well) |
| `trashed` | every message has `TRASH` |
| `read` | no message has `UNREAD` |
| `flags` | `star` if any has `STARRED`; `important` if any has `IMPORTANT` |
| `labels` | every other label of any message (no `INBOX`, `UNREAD`, `STARRED`, `IMPORTANT`, `TRASH`, `SPAM`, `SENT`), sorted, named by ID |

Gmail dates no label change, so `observed_at` is when the receiver read the
thread. The adapter's own change comes back through history as the same
report, which the engine takes as the echo that settles it.

## Extension effects

Two effects, version 1, run through `Extensions.Execute`. The factory refuses
invalid fields at the call (`adapter.ActionValidator`), with nothing stored;
`Execute` applies the same rules again at the attempt.

| Effect | Authorized as | Rules |
| --- | --- | --- |
| `google.mail.labels` | `conversation.label` | `add` and `remove` are lists of Gmail label IDs (`INBOX`, `STARRED`, `Label_7`, ...): at least one ID overall, each ID once across both lists, at most 100 per list. Other fields are ignored and never sent |
| `google.mail.reply_draft` | `message.reply` | `text`, `draft` and `delete`: see [Reply drafts](#reply-drafts). Other fields are ignored |
| `google.mail.unsubscribe` | `extension.execute` | Optional `method`: `auto` (the default: one-click when the message is eligible, else mailto), `one_click` or `mailto`. See [Unsubscribe](#unsubscribe) |
| `google.mail.forward` | `extension.execute` | `to` (one bare address, required) and `text` (an optional note, at most 16 KiB). See [Forward](#forward) |

The effect names no message itself: it applies to the effect's target when
the engine sets one naming a provider message of the channel, else to the
conversation's reply route's parent message (the Gmail message the peer
wrote, which `Messages.Reply` also answers). A conversation whose route has
no parent message (nothing received from the peer yet) gets a final
`rejected` outcome with `validation`. `Extensions.Execute` sets no target
today, so the parent message is the one labelled.

uma's own labels (`Conversations.AddLabels`, `Labels.*`) stay local and
canonical: the effect reads or writes none of them, and no inbound label
change maps back to one. The label IDs a message holds arrive in the
`google.mail.message` payload (`label_ids`) of every received message.

Outcomes: `users.messages.modify` is idempotent (a label already present or
already absent is left as it is), so a lost answer or a 5xx is `transient` and
the attempt is repeated; it is never `outcome_unknown` and never reconciled by
reading the message back. A message Gmail no longer holds (404) is a final
`rejected` with `not_found`; an unknown label ID (400) a final `rejected`; a
token without `gmail.modify` (403) `unauthorized`, never retried.

## Provider mailboxes

A Gmail mailbox is a label (`model.ProviderMailbox`, kind `label`). Two
extensions list and create them, each gated by the capability of its own name,
so a host reads per channel from `Configuration().Capabilities` whether it can
list (`extension_query` and `google.mail.mailboxes`) or create
(`extension_execute` and `google.mail.mailbox_create`).

| | List | Create |
| --- | --- | --- |
| Extension | `google.mail.mailboxes` (`Extensions.Query`) | `google.mail.mailbox_create` (`Extensions.Execute`, target the channel) |
| Provider call | `users.labels.list` | `users.labels.create` (visible in the label and message lists) |
| Authorized as | `extension.query` | `extension.execute`; a plugin actor's create can be held, like any write |
| Scope | `gmail.readonly` | `gmail.modify` |
| Data | none | `{"name": "Clients/Acme"}`; `kind`, when given, is `label` |
| Result | every label: ID, name, role | the label's reference in the operation's provider references (`ProviderMailboxNamespace`, `RefKindMailbox`, the label ID) |

- IDs are Gmail's label IDs; system labels are their own names (`INBOX`,
  `TRASH`), and `INBOX`, `TRASH`, `SPAM`, `SENT` and `DRAFT` carry the roles
  `inbox`, `trash`, `junk`, `sent` and `drafts`.
- Nested names keep "/" in the name: Gmail nests a label under another by name
  alone, and does not create the parent.
- Create is idempotent by name, compared without regard to case as Gmail does:
  a label of that name (a system label included) is the result, with no second
  create; a create that loses a race (409) reads the winner back. A name Gmail
  refuses is rejected as `permanent`.
- A conversation's provider state names its labels from the list: a report's
  `ProviderLabel.Name` is the label's name, read once for several reports (kept
  five minutes, a label made since looked up again after thirty seconds). A
  list the adapter cannot read leaves the ID as the name.
- A host registers `Factory.Extensions()` with the factory: the adapter
  declares the two capabilities, which need their definitions.

## Reply drafts

`google.mail.reply_draft` leaves a reply in the mailbox's Drafts for a person
to finish and send in their mail client. It sends nothing. Its data:

| Call | Data | Gmail call |
| --- | --- | --- |
| create | `{"text": "..."}` | `users.drafts.create`: `message.raw` and `message.threadId` |
| update | `{"text": "...", "draft": "<draft ID>"}` | `users.drafts.update` on that draft: replaces its content |
| delete | `{"draft": "<draft ID>", "delete": true}` | `users.drafts.delete`: permanent, the draft does not go to Trash |

A create or an update also takes `reply_all` (boolean), `cc` and `bcc` (lists of
bare addresses), which address the draft as a send's [reply
recipients](#reply-recipients) do; they are invalid on a deletion. An update
recomposes the whole draft, so it names them again to keep them.

`text` is required unless `delete` is set, and must be absent with it; a
deletion needs `draft`; blank text, a non-string field and a padded draft ID
are refused at the call, with nothing stored.

The reply is composed as a send composes it: From the mailbox, To the
conversation's reply route address, the subject of the peer's message with
`Re: ` once, `In-Reply-To` and `References` of the peer's message, its
`threadId` (the conditions Gmail documents for a message to join a thread),
and uma's own `Message-ID` derived from the operation. The peer's message is
the route's parent; when it is gone the draft starts a new thread.

The outcome's provider references name the draft: kind `draft` under the
provider message namespace (`google.DraftRef`). The operation keeps them
(`Operation.ProviderReferences`); a host passes that ID as `draft` in a later
call to update or delete the same draft. Gmail keeps a draft's own ID stable
while the ID of the message it holds changes on every update, so the draft
ID, not a message ID, is the handle. Every call is its own operation: the
same operation ID with the same payload replays the stored result, and with
another payload conflicts, as for every effect, so a host uses a new
operation ID for each update and names the draft it means.

Ownership. The draft ID is the caller's, so an update or a deletion first
reads the draft (`users.drafts.get`, `format=metadata`) and acts only on a
draft this adapter made for the conversation: its `Message-ID` has uma's
derived form on the adapter's Message-ID domain, and its `threadId` is the
route's parent message's (a route whose parent is gone binds by the `To`
header: one parsed address equal to the route's, compared whole and without
case; a lookalike, a display-name trick, a second recipient or an unparsable
header is refused). Any other draft of the mailbox, a draft a
person wrote included, is a final `rejected` with `validation`, and nothing
is changed. The adapter keeps no record of the drafts it made, so this is a
check of the draft itself, not of the operation that created it.

Outcomes. An update replaces the content wholesale, so an edit a person made
since is overwritten. A draft Gmail no longer holds (the person sent or
deleted it) is a final `rejected` with `not_found` on update, and a success on
delete (the ownership read answers 404): the draft is gone, which is the state asked for. A create has no
idempotency key: a 5xx, a lost answer or an answer naming no draft is
`outcome_unknown`, and `Reconcile` lists Drafts for the operation's
`Message-ID` (`users.drafts.list`, `q=rfc822msgid:`): found, it is
`provider_accepted` with the draft; not found, `failed` with a transient
failure, which allows a new attempt once the settle window has passed. An
update or a deletion is idempotent, so a lost answer or a 5xx is `transient`
and repeated, never reconciled. A token without a draft scope (403) is
`unauthorized`, never retried.

Scopes: `users.drafts.*` accept `gmail.compose`, `gmail.modify` and full
access ([create](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.drafts/create),
[update](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.drafts/update),
[delete](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.drafts/delete),
[list](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.drafts/list)),
so `gmail.modify`, which the adapter already declares, covers it; `gmail.send`
and `gmail.readonly` do not.
### Forward

`google.mail.forward` sends one message to another address with the original
carried whole. It applies to the effect's target or the reply route's parent
message, as the labels effect does. The adapter reads that message again from
Gmail (`users.messages.get`, `format=raw`, bounded like the raw read) at the
attempt and attaches its bytes unchanged as a `message/rfc822` part
([RFC 2046](https://www.rfc-editor.org/rfc/rfc2046) section 5.2.1) of a new
`multipart/mixed` message: the original's headers, body and attachments all
survive, and the receiver's client opens it as an attached message. A message
that is gone is `rejected` with `not_found`; one over the read bound is
`rejected` with `validation`. The send is the adapter's own
(`users.messages.send`), with the outcomes of [Sending](#sending), and needs
`gmail.send` and `gmail.readonly`.

| Header | Value |
| --- | --- |
| From, To | the mailbox; the `to` address |
| Subject | `Fwd: ` and the original's subject (not repeated when it already starts `Fwd:` or `Fw:`) |
| Message-ID | uma's, derived from the operation as a send's is; the outcome reports it and Reconcile searches Sent for it |
| References | the original's Message-ID alone, so a receiver that stores threading headers finds the copy by the original's Message-ID |
| In-Reply-To, threadId | none: a forward is not a reply and starts its own Gmail thread |

The text part is the note, or "Forwarded message attached." when there is none.
`microsoft.mail` and `imap.mail` ship no such effect: a host checks the shipped
effects before it offers a forward.

### Unsubscribe

`google.mail.unsubscribe` unsubscribes the mailbox from the sender of one
message, by the one method the message's own headers allow, and by no other.
It applies to the effect's target or the reply route's parent message, as the
labels effect does. The adapter reads that message again from Gmail
(`users.messages.get`, `format=raw`) at the attempt and decides from its
headers; the caller supplies no header, URL or address. The method is chosen
from what the headers permit:

| Method | Only when | What the adapter does |
| --- | --- | --- |
| One-click ([RFC 8058](https://www.rfc-editor.org/rfc/rfc8058)) | the message has exactly one `List-Unsubscribe` field naming an `https` URI (default port, no credentials, a public host name or address), exactly one `List-Unsubscribe-Post` field whose value is `List-Unsubscribe=One-Click`, and a valid DKIM signature by the sender's organization that covers both fields | One `POST` of the form body `List-Unsubscribe=One-Click` (`application/x-www-form-urlencoded`) to that URI through a client of the adapter's own (not the host's), guarded at connect time (see below): no cookies, no authorization, no other data, 15 second bound for the whole exchange |
| Mailto ([RFC 2369](https://www.rfc-editor.org/rfc/rfc2369), [RFC 6068](https://www.rfc-editor.org/rfc/rfc6068)) | the first `mailto` URI of the header names one address, no `to`, `cc` or `bcc` field, a single-line subject, and the address is in the sender's organizational domain | A text message from the mailbox to that address, with the URI's `subject` and `body` (default `unsubscribe`), through the adapter's own send (`users.messages.send`), so it is reconciled by its Message-ID like any send |

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
- A message over the raw size bound cannot be verified and is refused.

Every refusal is a final `rejected` outcome with a `validation` failure whose
safe message names the rule (no header, an `http` URI, no `https` URI, a
missing or different `List-Unsubscribe-Post`, a mailto outside the sender's
organization, a mailto adding recipients, an unsigned or uncovered message), and
is decided from the message's headers before any request to the unsubscribe
target. The only requests made earlier are the read of the message from Gmail
and the DNS lookups of the DKIM keys. A DNS failure while looking up a key is
`transient`, so the attempt is repeated. A message Gmail no longer holds is a
final `rejected` with `not_found`.

The one-click `POST` is idempotent: a conforming sender treats a repeat as a
no-op, so a lost answer, a `408`, `429` or `5xx` is `transient` and the
attempt is repeated; any other `4xx` is a final `rejected` with `permanent`; a
`2xx` is `provider_accepted`. A mailto send is answered as every send is:
`outcome_unknown` after an ambiguous answer, reconciled by Message-ID.

The effect is authorized as `extension.execute`: it sends a request to a third
party, so a host grants it on its own and not with `conversation.label`. A host
that wants a confirmation before the request, as RFC 8058 section 3.2
requires ("MUST NOT perform a POST on the HTTPS URI without user consent"),
asks its user before it calls the effect: the call is the consent. Reading the
message needs `gmail.readonly` (or `gmail.modify`), a mailto `gmail.send`.

The headers are kept on the stored message: the `google.mail.message` payload
carries `list_unsubscribe` and `list_unsubscribe_post` as found (the first of a
repeated field, at most 16 KiB), so a host can tell that an unsubscribe is
possible before it calls the effect. They decide nothing; the effect reads the
headers again from Gmail. A message too large to fetch raw, delivered from its
headers alone, carries neither.

## Permissions

- **Provider.** Every unit of work resolves an access token through
  `ports.Credentials` for purpose `gmail`, accepting OAuth 2 only. The host
  acquires and refreshes it (user consent, or a service account with
  domain-wide delegation). A 401 resolves once more with a fresh token; a second
  401 is `unauthorized` and never retried. `gmail.readonly` and `gmail.modify`
  are restricted scopes, `gmail.send` a sensitive one. `gmail.modify` is
  required by `users.messages.modify` and `users.drafts.*` only
  ([method reference](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.messages/modify));
  since it also covers every read and `users.messages.send`, a host may grant
  it alone. Gmail reveals a missing scope only on the call: a host that grants
  `gmail.readonly` and `gmail.send` but not `gmail.modify` keeps receiving and
  sending, and every labels and reply draft attempt settles `unauthorized`.
- **Push.** Each push request carries an OIDC token the adapter verifies:
  RS256, signed by a current Google key, issued by `accounts.google.com`, for
  `push_audience`, naming `push_service_account` with a verified email, and
  unexpired. The audience alone does not identify the subscription, so the
  service account is required with a topic.
- **uma.** The receiving principal needs `channel.receive`; replies and reply drafts
  are authorized as `message.reply`, label changes as `conversation.label` on the
  conversation.

## Configuration

`google.mail.config` version 1, as JSON in the channel configuration. It holds
no secret.

| Field | Required | Meaning |
| --- | --- | --- |
| `user` | yes | Gmail `userId`: an address, or empty for `me`, the token's own user |
| `address` | no | The mailbox's own address, the `From` of outbound mail; read once from the profile when empty |
| `addresses` | no | Recipient addresses the mailbox answers to, primary first; the first found in `To` or `Cc` is an inbound message's destination |
| `subject`, `message_id_domain` | no | Subject of a message that starts a thread; `Message-ID` domain |
| `topic` | no | Pub/Sub topic `projects/<project>/topics/<topic>`. Empty disables push: the receiver polls history only |
| `push_audience`, `push_service_account` | with `topic` | The push subscription's OIDC audience and service account |
| `poll_interval_ms` | no | Wait between history rounds; default 5 minutes, each wait spread by +-10% so channels do not poll in phase |
| `initial_sync_window_ms` | no | How far back a full synchronization reaches; default 1 hour |
| `watch_renew_interval_ms` | no | How often the watch is renewed; default 1 day, from 1 hour to 6 days; each renewal time is spread by +-10% |
| `history_mailboxes` | no | Label IDs a history import walks (`INBOX`, `STARRED`, `Label_12`); empty is `INBOX`. `SPAM`, `TRASH`, `DRAFT` and `SENT` are refused |

Host options (`google.Options`): `Endpoint` and `CertsURL` (overrides, for
tests) and `PageSize` (history and list pages; default 100). Calls go through
`uma.Config.HTTP`; `Open` fails without it.

## Receiving

Push is the fast path; polling `users.history.list` from the stored `historyId`
is the guarantee. The watch is renewed daily and always before its 7 day
expiry. When no `historyId` is stored, or Gmail no longer holds the stored one,
the receiver runs a full synchronization of the inbox messages received within
the initial window and counts it (`FullResyncs`, `LastFullResync`); changes in
the gap outside the window are not received.

**Receiver status.** The instance implements `adapter.ReceiverReporter` and
`adapter.ReceiverDegradationReporter`. It reports connected once a round
completed, with or without push. A failed `users.watch` does not stop the
receiver, since history polling covers the mailbox, but it is no longer
silent: `Runtime.ReceiverStatus` and `Runtime.ReceiverState` read
`Link` `degraded` with `Detail` one of

| `Detail` | Cause | When reported |
| --- | --- | --- |
| `google.mail.push_denied` | `users.watch` refused for permission (403/401), such as a Pub/Sub topic that does not grant Gmail publish rights | at the first failed attempt |
| `google.mail.push_rejected` | refused for another permanent reason (other 4xx), such as a topic name Gmail does not accept | at the first failed attempt |
| `google.mail.push_unavailable` | transient failures (5xx, throttling, no answer) | after 3 failed rounds in a row, or at once when the live watch would expire before the next attempt |

The next successful `users.watch` clears it and the link reads connected
again. Polling, retries and the lease are unchanged; the signal is advisory.

Every message is fetched raw and parsed: the text body (or rendered HTML) is a
text part and every other MIME part one structured descriptor part carrying
metadata and a digest. Drafts and messages labelled `SENT` are skipped. A
message that cannot be read is told apart from an answer that cannot be read,
and a whole page of history from one message on it:

| Outcome | What the receiver does | Operator sees |
| --- | --- | --- |
| Unreadable message: Gmail answers it whole (JSON naming the message), and its `raw` is not base64url or does not parse as a message | Skips it, moves the cursor on: Gmail never changes a message's bytes, so a fetch again fails alike | `google.mail.message_skipped` observation |
| Unreadable answer: not JSON (a proxy's error page), truncated, empty, naming no message or another one, without `raw` (or, for a metadata read, without `payload`) | Fails the round as `transient` and leaves the cursor before the message, so the next round reads it again; it is never skipped for this | `Runtime.Receive` backs off and reconnects; a push request answers 503, so Pub/Sub redelivers |
| Unreadable page: a `history.list` or `messages.list` page, or the profile, that is not JSON (a proxy's error page), truncated or empty, or a profile without `historyId` | Fails the round as `transient` before any record of the page is handed over; the cursor stays after the last whole page, so the next round reads the page again | As for an unreadable answer |
| Out-of-date cursor: `history.list` answers 404 | Runs a full synchronization (see above) | `FullResyncs`, `LastFullResync` in the receiver state |

A round that fails part way through a history page, on an unreadable answer or
a refusal by the sink, still stores the cursor past the history records it
handed over whole. The next connection starts after them, so it does not hand
the same messages over again: a connection that meets only the same failure
accepts nothing, and counts toward `Retry.MaxAttempts`.

The observation goes to the host's observer (`uma.Config.Observer`), names no
message and only counts. The receiver round that moves the cursor past the
message reports it, once per pass: a push request reads history without moving
the cursor, so it skips the message without reporting it and leaves the count to
that round. A message past the MIME structure limits in
[Limits](#limits) is not skipped: it is delivered with the header, the text and the
parts read before the limit, and the payload carries `parts_omitted: true`.
Threading uses the RFC 5322 headers only;
Gmail's `threadId` is a hint in the payload and never selects a conversation.

The raw form (`format=raw`) is the whole RFC 2822 message, attachments
included, so their bytes need no `users.messages.attachments.get` call: they
are staged through the engine from the parts already read, in the inbox of the
message's destination, and each staged attachment is named in an attachment
part after the descriptors. A descriptor carries `"staged": true` when its
bytes are staged. An attachment over the bounds in [Limits](#limits), or
refused by the engine's attachment checks (size limit, media types), is
described only, as is every attachment when the host gave the engine no blob
store; the message is delivered all the same. A staging failure a retry can fix
(the blob store is down) fails the fetch like a refusal by the sink, so the
message is read again. Staging is idempotent per message: a redelivery or a
resynchronization restages the same attachments. The staging needs no scope
beyond the one receiving needs.

A message too large for the answer bound in [Limits](#limits) is not fetched
whole and does not stop the mailbox. The adapter reads its headers alone
(`format=metadata`), delivers it with no text part and no attachment descriptor
or staging, and sets `body_omitted` and `size_estimate` (Gmail's estimate in
bytes) in the `google.mail.message` payload; the sender, subject, recipients and
threading headers are those of any message. The history cursor moves past it
once the sink accepted it, so the messages after it arrive. Nothing fetches its
body later. A failure a repeat can fix, in reading its headers (Gmail answers
5xx or does not answer), holds the cursor like any other failed fetch.

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

## Identities and start recipients

A sender is keyed in `google.mail.address` by its `From` address with the domain
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
operation ID stays free; a send would have refused it for good.

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

`users.messages.send` has no idempotency key. Any 5xx, and any failure after the
request may have been written, is `outcome_unknown`; only a failure before the
request was written is transient. Rate limits (429, and 403 with
`rateLimitExceeded`, `userRateLimitExceeded` or `dailyLimitExceeded`) are
`rate_limited`; `Retry-After` is reported to the operation ledger and never
waited on.

## Reply recipients

`Messages.Reply` with `Recipients` (`model.ReplyRecipients`) composes the
message with more than the route's address:

| Field | Effect |
| --- | --- |
| `cc` | Added to the `Cc` header |
| `bcc` | Added as a `Bcc` header, which Gmail takes as the blind recipients and removes before delivery |
| `reply_all` | The parent's `To` recipients join `To` and its `Cc` recipients `Cc`, each minus the mailbox's own address and any address already named. The parent's recipients are read with the `users.messages.get` (`format=metadata`) the send makes already; a reply whose parent Gmail no longer holds adds nothing |

The route address is always the first `To`. Addresses are compared without
case, and at most 50 in all are sent; more is a final `validation` before any
call to Gmail. The reply draft effect takes the same fields. The capability
reports `{"reply_all": true, "cc": true, "bcc": true, "max_recipients": 50}` as
its limits (`model.RecipientLimits`).

## History

`Runtime.Import` reads past mail through `HistoryReader`; nothing is sent or
changed at Gmail.

| | |
| --- | --- |
| Scope | the INBOX label by default, received mail only: sent, drafts, spam and trash are never read, and a message that carries SENT, DRAFT, SPAM or TRASH is skipped even when a walked label is set too. `history_mailboxes` replaces INBOX with the labels it lists (INBOX only if listed); a message under any of them is in scope, and one under several is read once. A conversation import reads the sender's mail (`from:`). Mail under none of the walked labels is not read |
| Window | `Since <= t < Until`, judged on `internalDate`, never on the `Date` header |
| Order | oldest first by `(internalDate, ID)` |
| Cursor | opaque; never a Gmail `pageToken`. It holds the slice being read and the IDs of it not yet delivered, so a later, smaller `Limit` takes a slice in parts and a cursor stays valid between calls. Its scope hash covers the label list, so a cursor issued under another list is `Validation` |
| Gap | a listed message whose raw form can never be read is a gap, not a silent skip; a message gone between the list and the get is simply absent |
| Errors | a token under `gmail.metadata` cannot use `q`: `Unsupported`. A cursor not issued for the read, or with bounds outside the window, is `Validation` before any call. A forged cursor is judged again per message against the read's scope |
| Attachments | staged through the request's stager (the engine's, set on every import) exactly as the receiver stages them: the same keys and bounds (10 attachments, 25 MiB each), the bytes taken from the `format=raw` answer with no further call, descriptors marked `staged`. A message imported and then received holds its bytes once. Without a stager, or without a blob store, they are described only |
| Rate limits | `RateLimited` or `Transient` with `Retry-After`, returned and never waited on; the host paces |

How a call reads: it lists one day of each walked label with `q=after: before:` (widened
by one second on each side, because Gmail rounds those operators), drains
every `pageToken`, unites the labels' IDs (a message under two labels is one ID, and the day is sorted as a whole, so order stays oldest first across labels), then dates each ID with `messages.get` `format=minimal` and
judges the exact bounds on `internalDate` before it fetches `format=raw` for
the envelopes. A day whose listing does not end on the first page is halved
and listed again. A call that meets 31 slices with nothing in them stops
there and returns a cursor, so a window far in the past does not cost one call
its whole budget.

Cost: a message takes a `format=minimal` get (20 quota units) and a raw get
(60), and each listing 5, against 6,000 units per minute per user: about 75
messages a minute. A large backfill is hours of host-paced calls.

Cassette status: `TestRecordedGmailHistory` replays committed cassettes; to
record again, see
[Record Gmail cassettes](../recording/gmail.md#record-the-history-cassettes).

## Limits

| What | Limit |
| --- | --- |
| Message size Gmail receives | 50 MB (Enterprise Standard) or 70 MB (Enterprise Plus), after encoding ([Google](https://knowledge.workspace.google.com/admin/gmail/gmail-receiving-limits-in-google-workspace)) |
| Raw message answer | about 94 MiB: a 70 MiB message base64url encoded (`Message.raw`) plus 1 MiB for the other fields; a larger message is delivered by its headers alone (see [Receiving](#receiving)) |
| Attachments staged per message | 10, in message order; the others are described only |
| Size of a staged attachment | 25 MiB decoded; a larger one is described only and uses none of the 10 |
| Attachments described per message | 53; the payload's `attachments_omitted` counts the others |
| MIME structure of a message | 256 parts (containers included) and 16 levels of nested multipart; past either the message is delivered with what was read before the limit and `parts_omitted` set in the payload. A message inside a `message/rfc822` part is one part, never walked |
| API response body read | 8 MiB |
| Gmail notifications | at most one per second per user (history polling fills the gap) |
| Push token | 8 KiB |
| Signing keys | cached by their `max-age`, at most 24 hours |
| Addresses recorded per message | 50 |
| Watch renewal | 1 hour to 6 days |
| History page | `Limit` envelopes; a slice's undelivered IDs (at most 500) ride in the cursor |
| Labels added, and removed, per `google.mail.labels` effect, and per state move | 100 each (Gmail's limit per `modify`) |
| Drafts listed by a reply draft reconcile | 10 |

## How it is tested

The shared adapter suite, its conversation state group included, and package
tests run against a scripted Gmail service. The recorded test
(`TestRecordedGmail`) replays committed cassettes of one scenario against a
real mailbox: a send, the Sent search, the labels effect, two receiver rounds
over a fixed 2001 window that holds only messages inserted for the recording
(a first synchronization with `format=raw` fetches and thread reads, then a
history read scoped to the sent message), every conversation state verb and
its undo, a thread read back by a reconcile, the `404` of a message Gmail does
not hold, and the recovery from a history ID Gmail no longer holds. The
history reader has its own recorded test (`TestRecordedGmailHistory`) over
another 2001 window. The reply draft effect has another (`TestRecordedGmailReplyDraft`):
a send, a draft created on it, found by `Message-ID`, updated and deleted twice
(the second the recorded `404`); it skips until recorded. The rest of the suite stays on the scripted service: the
another 2001 window, and the mailto unsubscribe has one
(`TestRecordedGmailUnsubscribe`) over a third. The one-click request, the DKIM
checks and every unsubscribe refusal run against a local stand-in for the
sender's endpoint and a locally generated signing key. The rest of the suite stays on the scripted service: the
cases that need provider faults no safe account can trigger (rate limits,
5xx, truncated or malformed pages, a page that fails part way), Pub/Sub push
and its OIDC check, oversize and poison messages, the durable and concurrency
groups and every inbound or send variant beyond the scenario's. To record,
follow [Record Gmail cassettes](../recording/gmail.md).

## Known limits

- Attachments stage only through an engine with a blob store; without one they
  are described only.
- A message over the raw answer bound is delivered without body or attachments
  (`body_omitted`); the bound is read before that is known, so the receiver
  downloads up to the bound once for it.
- Gmail's answer for messages between 25 and 70 MB is unverified against the
  real service: the bound follows the documented receive limit.
- A message whose `raw` is not base64url or does not parse is skipped for good
  (`google.mail.message_skipped`); nothing fetches it later. The receiver
  takes Gmail's whole JSON answer as the message's own bytes: a fault that left
  the JSON whole and naming the message, yet changed `raw`, would be taken for
  an unreadable message.
- An answer that is never what was asked for (a proxy that always answers an
  error page for one message, or for a history page) holds the cursor before
  it, and the messages after it wait until the answer changes:
  `Runtime.Receive` reconnects with backoff, and returns the `transient`
  failure once `Retry.MaxAttempts` connections in a row accepted nothing.
  Messages handed over before it on the same page are not handed over again,
  so they do not count as accepted.
- Two cases still hand messages over again on each connection, so a failure
  that never changes keeps `Runtime.Receive` reconnecting with backoff instead
  of returning: a full synchronization, which keeps no position among its
  messages and runs again from its first one (the repeats are deduplicated,
  but count as accepted), and a single history record that adds several
  messages, which the cursor passes whole or not at all. A full
  synchronization that runs again also reports an unreadable message again.
- Text only outbound, no attachments or HTML.
- A forced full synchronization misses changes outside the initial window.
- `gmail.readonly` and `gmail.modify` are restricted Gmail scopes; receiving
  needs one of them, the labels effect the second.
- The state verbs are unverified against the real Gmail API: that
  `threads.untrash` restores no `INBOX` label, that `threads.modify` of a label
  ID the mailbox does not hold is refused (400), and the history records a
  `threads.modify` leaves are the ones read here.
- A state report names one message of the thread. A thread none of whose
  messages uma holds is recorded as nothing.
- Every new inbound message brings Gmail's own label records (category,
  important), so each costs a thread read and a state report.
- The reply draft effect is unverified against the real Gmail API: that Gmail
  keeps the `Message-ID` header of a draft created from `raw` (the reconcile
  depends on it; if it does not, a lost create answer can leave a duplicate
  draft), that the draft ID is stable across `users.drafts.update`, and the
  answer for an update of a draft the person already sent. It carries text
  only and never sends.
- The labels effect reaches only the conversation's parent message (or the
  target the engine names); it creates no label, and a label ID Gmail does not
  know is a final rejection.
- The unsubscribe effect's DKIM check assumes `format=raw` returns the message
  as it arrived, signature and header bytes intact; that is not verified against
  a real signed message (the recording does not cover it, see
  [Record Gmail cassettes](../recording/gmail.md#record-the-unsubscribe-cassettes)).
  The one-click method and the
  DKIM lookups are covered by local stand-ins only.
- Experimental: configuration may change ([Compatibility](../compatibility.md)).
