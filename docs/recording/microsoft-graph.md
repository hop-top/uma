# Record Microsoft Graph cassettes

The `microsoft.mail` adapter's recorded test runs against a real Exchange
Online mailbox through Microsoft Graph with an OAuth access token. You need
a Microsoft 365 tenant with a test mailbox, an app registration, and a fresh
access token for each recording session.

Common steps (record, review, replay, clean up) are in
[Record adapter cassettes](README.md).

## What the recording does

`TestRecordedGraph` opens the channel on the mailbox (`/users/<mailbox>`)
and:

1. creates a draft to the peer address and sends it;
2. looks the attempt up in Sent Items (Reconcile);
3. sets the category `uma recorded` on the sent message and clears the list
   again: two `PATCH /users/<mailbox>/messages/<id>` calls with
   `categories` (the `microsoft.mail.categories` effect), so the message
   ends as it was. The name need not exist in the mailbox's master
   categories; if Graph refuses it, note the answer in the adapter page's
   known limits and pick a master category of the test mailbox instead;
4. runs a baseline delta round: the inbox delta (every message received since
   one hour before the test's fixed clock, listed and fetched, body
   included) and one state delta per watched folder (inbox, Archive and
   Deleted Items), each listing the whole folder with the state fields;
5. runs the provider state verbs on the sent message and puts it back
   where it was (`recordedState`), each verb one Graph call:
   `PATCH` of `isRead` (unread, then the original value), `PATCH` of
   `flag` (flagged, then notFlagged), `PATCH` of `importance` (high, then
   normal), `POST .../move` to `archive`, a
   Reconcile of that archive (read back, `GET` of the message and of the
   `archive` folder), a delta round that reports the message in Archive,
   a move back by folder ID (the Sent Items folder ID read from the
   message first), then the same for `deleteditems` and for `junkemail`.
   Every call carries `Prefer: IdType="ImmutableId"`, and the message ID
   must not change across the moves (the test fails if a move reports a
   reference change);
6. runs a last delta round on the same folders.

Steps 4 and 6 record every message in the mailbox's inbox, Archive and
Deleted Items. Use a mailbox that holds no real mail.

The Sent Items copy lags the send by seconds, so a recording waits 15 seconds
before the reconcile of step 2 and the cassette holds the found answer; a
replay does not wait. The sent message's `internetMessageId` is the one
Exchange assigns, which names the mailbox server
(`<...@<server>.PROD.OUTLOOK.COM>`); it is Microsoft's host, not the tenant,
and is not scrubbed.

## Record the history cassettes

`TestRecordedGraphHistory` reads the mailbox's past received mail through
`HistoryReader` over the fixed window 2001-01-01 00:00 to 2001-01-02 00:00
UTC (see `recorded_history_test.go`): the inbox list
`mailFolders/inbox/messages` with
`$filter=receivedDateTime ge <since> and receivedDateTime lt <until>`,
`$orderby=receivedDateTime asc` and `$top`, the receiver's `Prefer` headers,
each `@odata.nextLink` page, and the attachment of the message that has one.
It sends and changes nothing. The same variables and token as above apply,
plus the window, which is part of every recorded request.

The window lies before any real mail a mailbox can hold, so it holds only
messages placed there for the recording. First list it in every folder and
check it is empty. Then create three messages in the inbox from the peer
(`POST /users/<mailbox>/mailFolders/inbox/messages`), dated 09:00, 10:00
and 11:00 that day, the second with a small file attachment, each with the
extended properties that make it received mail rather than a draft:

```json
"singleValueExtendedProperties": [
  {"id": "Integer 0x0E07",    "value": "0"},
  {"id": "SystemTime 0x0E06", "value": "2001-01-01T09:00:00Z"},
  {"id": "SystemTime 0x0039", "value": "2001-01-01T09:00:00Z"}
]
```

`0x0E07` (`PR_MESSAGE_FLAGS`) without the unsent flag gives `isDraft` false,
`0x0E06` (`PR_MESSAGE_DELIVERY_TIME`) sets `receivedDateTime` and `0x0039`
(`PR_CLIENT_SUBMIT_TIME`) `sentDateTime`. No mail is sent.

The recording answered what the reader took on trust: a list of the inbox
accepts `receivedDateTime lt` together with `$orderby receivedDateTime asc`,
and `internetMessageHeaders` can be selected in a list. Graph's own
`@odata.nextLink` names the folder by key (`mailFolders('inbox')`) where the
reader asks for `mailFolders/inbox`; the reader takes both spellings for the
same list.

```sh
export MS_GRAPH_MAILBOX=<test mailbox UPN> MS_GRAPH_PEER=<peer address>
export MS_GRAPH_HISTORY_SINCE=2001-01-01T00:00:00Z MS_GRAPH_HISTORY_UNTIL=2001-01-02T00:00:00Z
cd adapter/mail/microsoft
XRR_MODE=record go test -count=1 -run '^TestRecordedGraphHistory$' ./...
```

Cassettes land in `adapter/mail/microsoft/testdata/cassettes/graph-history`;
the scrub and residue checks are those of the main recording.
`TestRecordedHistoryOverFake` runs the same scenario against the scripted
service.

## Record the inbound cassettes

`TestRecordedGraphInbound` runs the receiver over mail the peer wrote,
in the fixed window 2001-02-01 (the fixed clock is noon that day; the first
delta round starts one hour before it). Like the history window it lies
before any real mail, so it holds only messages the recording creates, by
`POST /users/<mailbox>/mailFolders/inbox/messages` with the extended
properties of the history section: message 1 at 11:10 and message 2 at 11:20
(with a small text attachment) before the first round, message 3 at 11:30
between the first and the second. No mail is sent to create them. The test
does this itself when recording, with the token, outside the recorded client,
and fails first when the inbox already holds a message dated in the window.

The scenario then:

1. runs a delta round: it lists messages 1 and 2 with their bodies and
   downloads the attachment of message 2 (`attachments`, then `$value`);
2. runs a second round from the stored delta link: it lists message 3 only;
3. replies to message 1: `createReply`, the update of the draft, the send
   (one message to the peer) and, after a 15 second wait, the Sent Items
   lookup of the attempt;
4. moves message 1 to Deleted Items and runs a last round, which delivers
   no message: the delta of the inbox (`changeType=created`) answers an empty
   list for a message that left it. The state deltas of the watched folders
   run in each round and are recorded too.

```sh
export MS_GRAPH_MAILBOX=<test mailbox UPN> MS_GRAPH_PEER=<peer address>
cd adapter/mail/microsoft
XRR_MODE=record go test -count=1 -run '^TestRecordedGraphInbound$' ./...
```

Cassettes land in `adapter/mail/microsoft/testdata/cassettes/graph-inbound`.
Before recording again, move the messages of the window (subjects
`uma inbound recording N`, in the inbox, and the `RE:` reply in Sent Items)
to Deleted Items and delete the cassette directory. The mailbox then holds
the three history messages and nothing dated in the window. The recording
confirmed that a created message with those extended properties is received
mail to a delta round, that the attachment list answers a `size` larger than
the content, and that the delta link names the folder by key
(`mailFolders('inbox')`).

## Record the refusal cassettes

`TestRecordedGraphErrors` records what Graph answers to requests it refuses.
It changes nothing: it reads one message of the history window (the three
history messages must be there) and derives from its ID the ID of a
well-formed message that does not exist (one character changed), then:

- sets the read state of the unknown message (`PATCH`, 404
  `ErrorItemNotFound`) and reconciles it (`GET`, 404);
- reconciles a move to the unknown ID as a destination folder (`GET` of the
  folder, 404);
- sets the read state of the ID `not-a-message-id` (400
  `ErrorInvalidIdMalformed`);
- runs a delta round from a link with the token `corrupt` (400 `BadRequest`,
  "Badly formed token"; an expired token is a 410 Graph does not give on
  request);
- runs a round with a token Graph does not accept (401
  `InvalidAuthenticationToken`, twice: the adapter asks for a fresh token
  once);
- runs a round with a change-notification URL set: the subscription request
  is answered 400 `InvalidRequest` and the round goes on with delta tracking
  over an empty window (2001-03-01). The fixed clock is long past, so Graph
  refuses the expiry before it looks at the notification URL; a request that
  Graph could accept would create a subscription, which the recording never
  does.

```sh
export MS_GRAPH_MAILBOX=<test mailbox UPN> MS_GRAPH_PEER=<peer address>
cd adapter/mail/microsoft
XRR_MODE=record go test -count=1 -run '^TestRecordedGraphErrors$' ./...
```

Cassettes land in `adapter/mail/microsoft/testdata/cassettes/graph-errors`.
Graph's error bodies carry a request ID and a timestamp, and for Exchange
errors the names of Exchange's internal exception types; none names the
tenant.

## Record the reply draft cassettes

`TestRecordedGraphReplyDraft` records the `microsoft.mail.reply_draft` effect
in a scenario of its own, so the cassettes of the main scenario stay valid.
It needs no new permission (`Mail.ReadWrite` covers createReply, the update
and the delete) and no seeded mail. It:

1. sends one message to the peer (the draft path of a send: `createReply` or
   a new draft, `PATCH`, `send`), the parent of the reply;
2. creates a draft on it: `POST /messages/{id}/createReply`, then `PATCH` of
   the draft with the text, the peer as only recipient and the attempt
   property;
3. replaces the draft's body: the ownership read (`GET` with `$select` and
   `$expand` of the attempt property, and of the parent's `conversationId`),
   then the `PATCH` of `body` alone;
4. deletes the draft (ownership read, then `DELETE`), then deletes it again:
   the recorded `404` of the ownership read
   of a message that is gone, which the effect treats as deleted.

The mailbox ends as the main scenario leaves it: the sent message stays in
Sent Items, and the deleted draft sits in Deleted Items (a delete of a
message moves it there). If the run stops between steps 2 and 4, delete the
leftover draft by hand: it is the only draft with a subject beginning `RE:`.

```sh
export MS_GRAPH_MAILBOX=<test mailbox UPN> MS_GRAPH_PEER=<peer address>
cd adapter/mail/microsoft
XRR_MODE=record go test -count=1 -run '^TestRecordedGraphReplyDraft$' ./...
```

Cassettes land in `adapter/mail/microsoft/testdata/cassettes/graph-reply-draft`.
The residue check runs after a recording; the answers return whole messages,
so scrub display names and `@odata.context` as above and read the `diff`.
Write into the adapter page's known limits what Graph did: whether
`createReply` on a message the mailbox itself sent answered a draft (if it
refuses, record with a received message as the parent instead and say so),
and the status of the second `delete`.
## Record the unsubscribe cassettes

`TestRecordedGraphUnsubscribe` reads one message of a window with the history
reader, then runs the `microsoft.mail.unsubscribe` effect on it with
`method: mailto`: `GET /users/<mailbox>/messages/<id>/$value` (the MIME form),
and the draft (`POST /messages`) and `POST /messages/<id>/send` of the
unsubscribe message. It sends one message, to the peer. The one-click method is
not recorded: it needs a real sender's DKIM-signed message and a request to
that sender's endpoint, and a replay cannot serve the DNS lookups of the DKIM
check; it stays on the local stand-in. Do not unsubscribe a real mailbox from a
real list to record it.

The message must carry `List-Unsubscribe: <mailto:<peer address>?subject=unsubscribe>`
and be from the peer, so the send reaches an address you control and the
organizational-domain rule holds. Graph sets an Internet header on a message
created through the API only when its name starts with `x-`
([internetMessageHeaders](https://learn.microsoft.com/en-us/graph/api/resources/internetmessageheader)),
so create it from MIME instead: `POST /users/<mailbox>/mailFolders/inbox/messages`
with `Content-Type: text/plain` and the base64 of an RFC 5322 message as the
body ([create a message from MIME](https://learn.microsoft.com/en-us/graph/api/user-post-messages)).
Check that the created message reports the header in `internetMessageHeaders`
and that its `receivedDateTime` falls where you expect; the window follows it.
If Graph dates it today rather than 2001, record with the window around that
time (`MS_GRAPH_UNSUB_SINCE`, `MS_GRAPH_UNSUB_UNTIL`) in a mailbox that holds no
other mail then, and set the test's defaults (`unsubscribeSince`,
`unsubscribeUntil`) to that window before committing the cassettes, since the
window is part of every recorded request. List the window first and check that
it holds exactly that message.

```sh
export MS_GRAPH_MAILBOX=<test mailbox UPN> MS_GRAPH_PEER=<peer address>
export MS_GRAPH_UNSUB_SINCE=<rfc3339> MS_GRAPH_UNSUB_UNTIL=<rfc3339>
cd adapter/mail/microsoft
XRR_MODE=record go test -count=1 -run '^TestRecordedGraphUnsubscribe$' ./...
```

Cassettes land in `adapter/mail/microsoft/testdata/cassettes/graph-unsubscribe`;
the scrub and residue checks are those of the main recording. Afterwards
delete the created message and the sent one from the mailbox, and the copy in
the peer's. Graph documents that `$value` returns a MIME *representation*
("Outlook does not save messages in MIME format"): inspect the recorded `$value`
answer for the `DKIM-Signature` and `Authentication-Results` fields of a real
delivered message to learn whether a one-click check can ever pass on Graph's
copy; the effect refuses one-click when it does not.

## What is not recorded

The change-notification webhook and subscriptions (Graph calls a public
endpoint; a created subscription is tenant state), throttling and server
errors (not triggered on purpose), delta token expiry, the Internet headers
of received mail (a message created through the API carries none), many or
unusual attachments and the cases of the shared adapter suite that need a
harness placing mail on demand stay on the scripted service; see
[how the adapter is tested](../adapters/microsoft.mail.md#how-it-is-tested).

## Get a tenant and a test mailbox

- **A tenant you already administer:** create a dedicated test mailbox.
  Never record against a person's mailbox. A **shared mailbox** needs no
  license (under 50 GB, no archive) and works with application permissions;
  block its account's sign-in and hide it from the address lists. A dedicated
  licensed user works too, and is what the delegated token path needs.
  - With application permissions, restrict the app to the test mailbox: put
    the mailbox alone in a mail-enabled security group and add an Exchange
    Online application access policy
    (`New-ApplicationAccessPolicy -AppId <client id> -PolicyScopeGroupId <group>
    -AccessRight RestrictAccess`), then check it with
    `Test-ApplicationAccessPolicy` against the test mailbox (granted) and
    another mailbox (denied)
    ([limit app access](https://learn.microsoft.com/en-us/graph/auth-limit-mailbox-access)).
- **The Microsoft 365 Developer Program sandbox** is free, but only for
  eligible members (Visual Studio Professional or Enterprise subscribers,
  some partner program tiers and Premier or Unified Support customers), and
  setup links a billing account
  ([program FAQ](https://learn.microsoft.com/en-us/office/developer-program/microsoft-365-developer-program-faq)).

Also pick a **peer address** you own, different from the mailbox. It
receives the test message.

## Register the app

In the Microsoft Entra admin center, under **App registrations > New
registration**, register a single-tenant app
([register an app](https://learn.microsoft.com/en-us/entra/identity-platform/quickstart-register-app)).
Note its **Application (client) ID** and the **Directory (tenant) ID**.

### Permissions

The adapter declares two Microsoft Graph permissions, `Mail.ReadWrite`
(read, delta, drafts, Sent Items lookups, the message update of the
categories effect, step 3, and the state verbs and folder deltas, steps 4 to
6) and `Mail.Send`, as delegated or as application permissions
(`RequiredPermissions` in `factory.go`). The test needs both; the categories
step and the state steps need nothing further
([update message](https://learn.microsoft.com/en-us/graph/api/message-update),
[move message](https://learn.microsoft.com/en-us/graph/api/message-move),
[message delta](https://learn.microsoft.com/en-us/graph/api/message-delta)).
No new OAuth scope or app registration change is needed. Do not add `Mail.ReadWrite.Shared`, `MailboxSettings.*` or
`.All` directory permissions.

Pick one of the two token paths below. Delegated is the least privilege: the
token can reach only the signed-in test user's mailbox.

## Get a delegated token (recommended)

1. In the app registration, under **API permissions**, add the delegated
   Microsoft Graph permissions `Mail.ReadWrite` and `Mail.Send`.
2. Under **Authentication**, turn on **Allow public client flows**, which the
   device code flow needs
   ([public client flows](https://learn.microsoft.com/en-us/entra/identity-platform/msal-client-applications)).
3. Run the device code flow
   ([device code flow](https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-device-code))
   and sign in as the test user when it asks. The shell variables `tenant`
   and `client_id` are local to these commands; the test does not read them.

   ```sh
   tenant=<directory (tenant) ID> client_id=<application (client) ID>
   scope="https://graph.microsoft.com/Mail.ReadWrite https://graph.microsoft.com/Mail.Send"
   dc=$(curl -s -d "client_id=$client_id" --data-urlencode "scope=$scope" \
     "https://login.microsoftonline.com/$tenant/oauth2/v2.0/devicecode")
   echo "$dc" | jq -r .message      # open the URL, enter the code, sign in as the test user
   # after signing in:
   export MS_GRAPH_ACCESS_TOKEN=$(curl -s -d "client_id=$client_id" \
     -d "grant_type=urn:ietf:params:oauth:grant-type:device_code" \
     -d "device_code=$(echo "$dc" | jq -r .device_code)" \
     "https://login.microsoftonline.com/$tenant/oauth2/v2.0/token" | jq -r .access_token)
   ```

   Leaving out `offline_access` means no refresh token is issued; sign in
   again for the next session.

If the tenant does not let users consent to apps, an administrator grants
consent for the two delegated permissions once.

## Get an app-only token

App-only tokens act without a user and, by default, on every mailbox in the
tenant. Use them only when the mailbox cannot sign in interactively.

1. Under **API permissions**, add the application permissions `Mail.ReadWrite`
   and `Mail.Send` and grant admin consent
   ([permissions reference](https://learn.microsoft.com/en-us/graph/permissions-reference)).
2. Restrict the app to the test mailbox with Exchange Online RBAC for
   Applications (a management scope on the test mailbox and the
   `Application Mail.ReadWrite` and `Application Mail.Send` roles). The
   tenant-wide Entra consent and the RBAC grant add up, so remove the
   Entra consent once the scoped roles are in place
   ([RBAC for Applications](https://learn.microsoft.com/en-us/exchange/permissions-exo/application-rbac)).
3. Under **Certificates & secrets**, create a client secret with a short
   expiry and store it in your secret manager.
4. Request a token with the client credentials grant
   ([client credentials flow](https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-client-creds-grant-flow)):

   ```sh
   export MS_GRAPH_ACCESS_TOKEN=$(curl -s -d "client_id=$client_id" \
     --data-urlencode "client_secret=$client_secret" \
     -d "scope=https://graph.microsoft.com/.default" -d "grant_type=client_credentials" \
     "https://login.microsoftonline.com/$tenant/oauth2/v2.0/token" | jq -r .access_token)
   ```

Access tokens last 60 to 90 minutes
([access tokens](https://learn.microsoft.com/en-us/entra/identity-platform/access-tokens)).

## Record Microsoft Graph cassettes

| Variable | Value |
| --- | --- |
| `MS_GRAPH_ACCESS_TOKEN` | Delegated or app-only access token with both permissions |
| `MS_GRAPH_MAILBOX` | The test mailbox's user principal name |
| `MS_GRAPH_PEER` | The peer address |

```sh
export MS_GRAPH_MAILBOX=<test mailbox UPN> MS_GRAPH_PEER=<peer address>
cd adapter/mail/microsoft
XRR_MODE=record go test -count=1 -run TestRecordedGraph ./...
```

Cassettes land in `adapter/mail/microsoft/testdata/cassettes/graph`.

## Keep the traffic safe

- Send only to your own peer address.
- Do not change mailbox rules, forwarding, the app registration or its
  permissions while recording.
- Afterwards, delete the test messages from the mailbox's Sent Items and the
  peer's inbox (the main scenario's message, and the reply of the inbound
  recording), and delete client secrets you no longer need. The state
  steps move the sent message to Archive, Deleted Items and Junk Email and
  back to Sent Items by folder ID; if the run stopped half way, find it in
  one of those folders (search by subject) and move it back or delete it,
  and empty the test mailbox's Deleted Items and Junk Email afterwards.

## Check the cassettes

What the test removes: the `Authorization` header, and the mailbox and peer
addresses (recorded as `MAILBOX` and `PEER`) in every form Graph writes
them: literal, with `@` percent-encoded as `%40` (Graph's `@odata.context`,
for example `users('alice%40example.com')`, and its paging links), and
encoded twice (`%2540`), in either letter case of the hex digits. When you
record, the test fails if the token, the mailbox or the peer survives in any
of these forms in a cassette, in any letter case.

What it does not cover:

- **Display names** of the mailbox user and the peer in `from`, `sender` and
  `toRecipients`. Give the test user a neutral display name.
- **Mailbox content** from steps 4 and 6 above: the state deltas list every
  message of the inbox, Archive and Deleted Items (id, folder, read, flag, importance,
  last modified).
- **Folder IDs and names.** Graph's mail folder IDs and display names of the
  test mailbox appear in the move, folder and state answers; they identify
  your tenant's mailbox and are not placeholder-covered. Review them.
- **The update and move answers** of steps 3 and 5: Graph returns the whole sent message,
  recipients and body included, after each `PATCH` and each move. The placeholders cover
  the addresses in every form above; check the display names.
- **The mailbox's object ID.** Graph may name the user by object ID rather
  than address in `@odata.context` and links; it is an identifier of your
  tenant, not a placeholder-covered address. Look for it and replace it by
  hand in responses.

```sh
cd adapter/mail/microsoft
grep -rnF -e "$MS_GRAPH_MAILBOX" -e "${MS_GRAPH_MAILBOX/@/%40}" -e "$MS_GRAPH_PEER" -e "${MS_GRAPH_PEER/@/%40}" testdata/cassettes
```

Also grep every cassette for the tenant ID, the application ID, the tenant's
domain and for IP addresses, and list every address in them: only
`MAILBOX`, `PEER` and `@recording.invalid` addresses may remain, besides the
sent message's Exchange-assigned `internetMessageId`.
