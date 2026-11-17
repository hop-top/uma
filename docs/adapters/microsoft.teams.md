# microsoft.teams

`microsoft.teams` is the Microsoft Teams adapter. One channel is one bot: a
single-tenant Azure Bot registration (one Microsoft App ID) in one Microsoft
Entra tenant. The Bot Connector service posts the bot's activities to its
messaging endpoint, which the host mounts; uma answers through the Bot
Connector REST API on the host's HTTP client.

| | |
| --- | --- |
| Key | `microsoft.teams` |
| Package | `hop.top/uma/adapter/chat/teams`, in the module of the same path |
| Capabilities | `receive_http`, `send`, `start_conversation`, `edit`, `delete`, `react`, `signal` |
| Events | `message.received`, `message.updated`, `interaction.received` |
| Identity namespaces | `microsoft.teams.user` |
| Sender kinds | person, bot |
| Provider permissions | none declared: the bot's rights are where its Teams app is installed, not scopes on its token (see [Permissions](#permissions)) |
| Message IDs and threading | assigned by Teams (conversation ID and activity ID); no email-style threading; selection per room, per thread (channel threads) or per contact (a personal chat with each user) |
| Reply parent | reported: the activity `replyToId`, in the conversation's message ID form |
| Interactions | reports Adaptive Card `Action.Execute` presses; does not answer them ([interactions](../interactions.md)) |
| Maturity | experimental |

The matrix of all adapters is in [Adapters](README.md).

## Capabilities and operations

| Capability | Behavior |
| --- | --- |
| `receive_http` | `Runtime.Webhook` serves the bot's messaging endpoint: messages, edits, deletions, reactions and `Action.Execute` invokes, each checked against its Bot Connector token first; see [Receiving](#receiving) |
| `send` | `Messages.Reply` posts a message activity to the stored route: a channel (a new post), a channel thread, a group chat, or a user's personal chat |
| `start_conversation` | `Conversations.Start` posts the first message the same way. A user route goes through the personal chat the Connector creates or returns (`POST /v3/conversations`), which needs the app installed for that user; see [Sending](#sending) |
| `edit` | `Messages.Edit` replaces a message with the Connector's update activity (`PUT /v3/conversations/{id}/activities/{activityId}`); only the bot's own messages |
| `delete` | `Messages.Delete` deletes a message with the Connector's delete activity; only the bot's own messages |
| `react` | `Messages.React` adds or removes the bot's reaction, one of the six reaction types Teams names, with `PUT` or `DELETE .../activities/{activityId}/reactions/{type}` |
| `signal` | `Interactions.Signal` sends a typing indicator (an activity of type `typing`); Teams shows it in personal chats and small group chats only |

## Not declared

Reason classes are defined in [Adapters](README.md#why-a-capability-is-not-declared).

| Capability | Reason | Why |
| --- | --- | --- |
| `reply_recipients` | `provider` | A Teams message goes to its conversation; there is no cc or blind recipient |
| `receive` | `provider` | The Bot Connector only posts activities to the messaging endpoint; there is nothing to poll or hold open |
| `receipt` | `provider` | Teams tells a bot when a user read its message in a personal chat; no call lets a bot send a receipt |
| `membership` | `not built` | The Bot Connector cannot change a roster; Microsoft Graph (`POST /chats/{id}/members`) could, with a Graph credential the adapter does not use |
| `history` | `not built` | The Bot Connector has no read of past messages; Microsoft Graph `GET /chats/{id}/messages` could, with the `ChatMessage.Read.Chat` resource-specific permission |
| `reconcile` | `not built` | The Bot Connector takes no idempotency key and has no read of an activity; a lost answer stays `outcome_unknown`. Microsoft Graph could look the message up by text and time |
| `refetch` | `provider` | An edit carries the whole new message; the adapter names each edit by its time and text, so two edits never tie |
| `work_item_act` | `provider` | Teams has no work items |
| `internal_note` | `provider` | Teams has no private message that is part of the conversation; notes stay in uma |
| `extension_execute` | `not built` | An effect such as a new channel thread (`POST /v3/conversations` with `isGroup`) could be shipped |
| `extension_query` | `not built` | No read is shipped; the conversation roster (`GET /v3/conversations/{id}/members`) would be the first |
| `probe_permissions` | `not built` | Reading the bot's own roster entry in a conversation could check that the token and installation are live |
| `stream` | `not built` | Teams streams a bot's answer in personal chats with typing activities carrying a `streaminfo` entity; sends and edits can stand in |
| `send_ephemeral` | `not built` | Targeted messages (`?isTargetedActivity=true`, a public developer preview) show a message to one member |
| `respond` | `not built` | A reply and the update activity could answer as `message` and `update`; the `ephemeral` mode the respond contract also requires needs targeted messages, a developer preview. Teams answers an invoke only in the HTTP response, within five seconds |
| `respond_follow_up` | `not built` | Needs `respond` |
| `archive` | `provider` | `hideForUser` supports only delegated permissions, so no app-only path |
| `move` | `provider` | A chat has no folders or labels |
| `read_state` | `provider` | `markChatReadForUser` supports only delegated permissions, so no app-only path |
| `flag` | `not built` | Flag kinds are `star` and `important`; the nearest call, pinning a message, marks one message, not a chat |
| `trash` | `provider` | A chat has no trash |
| `raw_read` | `not built` | Raw read is built for mail only; the provider's whole copy of a message is not read |

## Permissions

- **Provider.** One credential purpose, resolved through `ports.Credentials`
  as `ports.OAuth2`: `microsoft.teams.connector`, the Bot Connector access
  token. The host mints it and refreshes it before it expires (an hour), or
  lets the adapter's token source do so (see [Minting the token](#minting-the-token)).
  The token is only ever sent to the channel's `service_url`. When the
  Connector refuses it (401) the adapter asks once more with `Fresh` set.
- **Inbound.** Nothing to resolve: the tokens Teams sends are checked against
  the Bot Framework's published keys and the channel's `app_id`.
- **Teams.** The bot can post where its app is installed: a team (its
  channels), a group chat, or a user's personal scope. In channels and group
  chats it receives only messages that @mention it, unless the app's manifest
  asks for the resource-specific permissions `ChannelMessage.Read.Group` or
  `ChatMessage.Read.Chat`
  ([receive all messages](https://learn.microsoft.com/en-us/microsoftteams/platform/agents-in-teams/enable-receive-all-chat-messages)).
  A bot cannot post in a private channel.
- **uma.** The receiving principal needs `channel.receive`; replies are
  authorized as `message.reply`, starts as `conversation.start`, edits,
  deletions and reactions as `message.edit`, `message.delete` and
  `message.react`, and signals as `conversation.signal`.

## Minting the token

A host that has no token minting of its own uses the public package
`hop.top/uma/adapter/chat/teams/botauth` and hands it to the factory:

```go
src, err := botauth.New(botauth.Config{
	AppID: appID, Tenant: tenantID, // Tenant empty: the multi-tenant botframework.com
	Secret: secret,                 // or Certificate + Key
	HTTP:   httpClient,             // ports.HTTPClient; Clock is optional
})
f := teams.NewFactory(teams.Options{Tokens: func(model.Channel) teams.TokenSource { return src }})
```

`Options.Tokens` is optional and chosen per channel: a nil answer, or no
option, keeps the host-supplied `ports.Credentials` path unchanged.

| Setting | Meaning |
| --- | --- |
| `AppID`, `Tenant` | The Entra application and tenant; the token comes from `<authority>/<tenant>/oauth2/v2.0/token`. A single-tenant bot names its tenant ID; a multi-tenant bot uses `botframework.com` |
| `Secret` or `Certificate` + `Key` | Exactly one. A secret goes in the request body. A certificate signs a client assertion (`client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer`): `PS256` by default (`AssertionAlg` `RS256` is the older form, which also adds the `x5t` header), header `x5t#S256` (base64url SHA-256 of the certificate's DER), claims `aud` (the token endpoint), `iss` and `sub` (the app ID), `jti`, `nbf`, `iat`, `exp` ten minutes on |
| `Authority` | Token endpoint origin, default `https://login.microsoftonline.com`. A sovereign cloud names its own host (for example `https://login.microsoftonline.us`). Must be https (http only for a loopback stand-in) |
| `Scope` | Default `https://api.botframework.com/.default`; a sovereign cloud may need its own Bot Framework scope, to be confirmed against that cloud's documentation |
| `HTTP`, `Clock`, `Skew` | Injected HTTP client and clock; a token is replaced `Skew` (default 5 minutes, at most half its life) before it expires |

The source caches the token in memory, shares one mint among concurrent
callers (a caller's cancellation does not abort it), and answers a fresh
request with the cached token when that was minted under 30 seconds ago.
Failures are `*model.Error`: `unauthorized` when the endpoint refuses the
credential (the message names only the OAuth error code and `AADSTS` code),
`rate_limited` with the `Retry-After` wait, `unavailable` or `transient`
otherwise. Nothing is logged and no secret, key, assertion or token appears in
an error. References: [Bot Connector authentication](https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-authentication),
[certificate credentials](https://learn.microsoft.com/en-us/entra/identity-platform/certificate-credentials),
[national clouds](https://learn.microsoft.com/en-us/entra/identity-platform/authentication-national-cloud).

Its tests run against an in-package stand-in of the token endpoint and verify
the certificate assertion by parsing it and checking its signature with a key
generated in the test; no live exchange is recorded (see the
[recording guide](../recording/teams.md#check-the-token-source-live)). It was
run once against Microsoft Entra on 2026-11-16, see
[Live verification](#live-verification-2026-11-16).

## Configuration

`microsoft.teams.config` version 1, as JSON in the channel configuration.
Unknown fields are refused, so a secret put there by mistake is caught.

| Field | Required | Meaning |
| --- | --- | --- |
| `app_id` | yes | The bot's Microsoft App ID, a GUID: the audience every inbound token must name. The bot's own Teams account is `28:<app_id>`, never reported back |
| `tenant_id` | yes | The Microsoft Entra tenant the bot is registered and installed in, a GUID. Activities naming another tenant are acknowledged and dropped. It is the destination of every envelope: bind the inbox to it |
| `service_url` | yes | The Bot Connector service to send to: the `serviceUrl` Teams puts on the bot's activities, such as `https://smba.trafficmanager.net/teams/` (the public cloud's; GCC and the other clouds have their own). https without user info, query or fragment |

Host options (`teams.Options`): `OpenIDMetadataURL`, the metadata document
naming the keys inbound tokens are signed with (default
`https://login.botframework.com/v1/.well-known/openidconfiguration`). A channel
cannot change whose keys it trusts.

## Receiving

The handler checks the request's token before anything reaches the sink, as
[Authenticate requests from the Bot Connector service](https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-authentication)
lists:

- a `Bearer` token in `Authorization`; none is 401;
- an RS256 JWT (no other algorithm) signed by a key of the OpenID metadata's
  `jwks_uri`, issuer `https://api.botframework.com`, audience `app_id`, with
  an expiry, valid within five minutes of clock skew; otherwise 401;
- its `serviceurl` claim equal to the activity's `serviceUrl` (compared
  without a trailing `/` and case-insensitively, as Microsoft's Teams SDK
  does); otherwise 401;
- its key endorsed for the activity's `channelId` when the key lists
  endorsements; otherwise 403, as the page asks.

Signature and claim checks are the `github.com/golang-jwt/jwt/v5` library's.
The metadata and the key set are read through the host's HTTP client, never a
client of the adapter's own, and cached for a day ("refresh ... at least once
every 24 hours"); a token naming a key the cache lacks reads them again at
most once a minute, so invented key IDs cannot become a flood of fetches.
When the keys cannot be read the request gets 503. Tokens issued by Microsoft
Entra (the Bot Framework Emulator, agentic identities) are not accepted.

| Teams sends | uma reports |
| --- | --- |
| `message` in a personal chat, group chat or channel | `message.received`: a message envelope from `microsoft.teams.user`, scoped by the conversation and, in a channel, by the thread: the one its conversation ID names, else the one a root post starts |
| A message by the bot itself (`from` is `28:<app_id>` or the activity's recipient) | Nothing: it would loop, and uma knows what it sent by its provider message IDs |
| A message by another bot (`28:` account or role `bot`) | A `message.automated` envelope from a bot sender |
| `messageUpdate` with `eventType` `editMessage` | `message.updated`: an edit by `from`. An edit of a message uma never saw records the message instead. `undeleteMessage` is not reported |
| `messageDelete` with `eventType` `softDeleteMessage` | `message.updated`: a deletion by `from` |
| `messageReaction` | `message.updated`: one reaction per entry of `reactionsAdded` and `reactionsRemoved`, on the message `replyToId` names; see [Reactions](#reactions) |
| A message with attachments and no text | One structured part (`microsoft.teams.attachments`) with each attachment's content type and name; the files and their URLs are not kept. The `text/html` rendering Teams attaches beside a message's text is not an attachment |
| A message with data and no text (a legacy `Action.Submit`) | Nothing: there is nothing to say; use `Action.Execute` |
| `invoke` named `adaptiveCard/action` | `interaction.received`; see [Interactions](#interactions) |
| Any other `invoke` (`task/fetch`, `task/submit`, message extensions) | 501 Not Implemented, as the Bot Framework SDK answers an invoke with no handler |
| `conversationUpdate`, `installationUpdate`, `typing`, other activities, other tenants, other channels than `msteams` | Acknowledged, nothing reported |

A message ID is `<conversation ID>|<activity ID>`, the conversation without
the `;messageid=<root>` suffix Teams adds for a channel thread: a reply carries
the suffix and a reaction to it may not, and both must name one message. Text is
kept as Teams sent it, mentions included (`<at>Name</at>`). The payload of every
message (`microsoft.teams.message`, version 1) names the tenant, the
conversation as Teams sent it, the activity ID, the conversation type,
`replyToId`, the sender's Entra object ID, the team and Teams channel IDs, and
whether the bot was mentioned (a `mention` entity naming it). It is a claim,
never an identity.

An activity whose sink answer is retryable gets 503; any other refusal gets
422. A redelivered activity is a duplicate to core: a message is named by its
ID, an edit by its time and a digest of its new text (Teams gives an edit no ID
of its own), a reaction by the reaction activity's ID.

### Reactions

Teams names six reaction types; each maps to one emoji, both ways:

| Type | Emoji |
| --- | --- |
| `like` | 👍 |
| `heart` | ❤️ |
| `laugh` | 😆 |
| `surprised` | 😮 |
| `sad` | 😢 |
| `angry` | 😡 |

An inbound reaction of another type is read when its ID starts with the emoji's
code points in hexadecimal, the form the Teams SDK documents (`1f440_eyes`
is 👀); any other is dropped. The bot adds only the six: the name part of any
other ID cannot be derived from the emoji.

## Conversations

Every message and interaction names the conversation it belongs to, so one
channel serves the three selections an inbox may use:

| Selection | A conversation is | Its reply route |
| --- | --- | --- |
| per room | a Teams conversation: a personal chat, a group chat or a channel | the conversation; a send to a channel starts a new post |
| per thread | one thread of a channel: a root post starts one, its replies join it | the thread, posted to as `<channel>;messageid=<root>`. Chats have no threads |
| per contact | one user | the user: replies and starts go to the user's personal chat, whatever conversation the user wrote in |

The inbox is bound to the tenant, not to a conversation: conversations and
threads are what separate conversations inside it.

## Interactions

A user pressing an `Action.Execute` on an Adaptive Card the bot sent makes Teams
post an `invoke` named `adaptiveCard/action`
([Universal Action Model](https://learn.microsoft.com/en-us/adaptive-cards/authoring-cards/universal-action-model)).
The sink records it, and only then does the handler answer 200 with the invoke
response `{"statusCode":200,"type":"application/vnd.microsoft.activity.message","value":""}`:
"a message that the client should display", empty, which leaves the card as it
is. Teams waits five seconds for it and retries twice.

| `Action.Execute` | Interaction |
| --- | --- |
| `value.action.verb` | `Action` (the action's `id` when it has no verb) |
| `value.action.data` | `Arguments`: a string as it is, an object as JSON (the card's inputs are merged into it) |
| `replyToId` | `Message`: the card's message |
| `from` | the actor, from the verified activity, never a field of the data |
| the conversation | `Context`: the conversation and, in a channel, the thread |

The interaction ID is `action:<message ID of the invoke activity>`, so the
invoke Teams retries is a duplicate to core. The deadline is five seconds from
the request. Teams gives no response credential, so the interaction has no
token and no respond window.

A refresh the client triggers on its own (`trigger` `automatic`) asks for a
user-specific card within five seconds, which nothing in uma can give: it is
acknowledged the same way and recorded as nothing.

### Answering

`microsoft.teams` does not declare `respond`, so `Interactions.Respond` is
`unsupported`. The host answers an interaction with the ordinary effects:
`Messages.Reply` naming it (`InResponseTo`), or `Messages.Edit` of the card's
message (the interaction's `Message`, a message of the bot's) with a new card.
Dialogs (task modules) are not offered: their content must come back in the
response to `task/fetch` within five seconds.

## Sending

A send posts one message activity to the route's conversation
(`POST /v3/conversations/{id}/activities`), and the Connector's answer
(`{"id": ...}`) is the message's activity ID. A reply naming a message of the
same conversation carries it as `replyToId`.

| Content | Activity |
| --- | --- |
| Text parts | `text`, joined by blank lines, with `textFormat` `plain`: literal text, never markdown |
| One `microsoft.teams.card` part, version 1 | One attachment of type `application/vnd.microsoft.card.adaptive`; the data is the card's JSON object (`"type": "AdaptiveCard"`) |

A message carries text or one card, never both: Teams splits text and an
attachment into two messages and then answers with no message ID
([conversational capability](https://learn.microsoft.com/en-us/microsoftteams/platform/bots/build-conversational-capability)),
which would leave the send without the message it made. Anything else, a
restricted audience, an empty message and one over 80 KB are refused before any
call (`unsupported` or `validation`).

A user route first creates or returns the personal chat with
`POST /v3/conversations` (`bot`, `members` with the user, `isGroup` false and
the tenant). Teams creates it only when the app is installed for the user or
the user is in a team where it is installed
([proactive messages](https://learn.microsoft.com/en-us/microsoftteams/platform/bots/how-to/conversations/send-proactive-messages));
its 403 is a final rejection, `unauthorized`.

| Connector answer | Outcome |
| --- | --- |
| 2xx with an ID | `provider_accepted` |
| 400, 413 | rejected, `validation` |
| 401 (after one fresh token) | `unauthorized` |
| 403 (`BotNotInConversationRoster`, `ForbiddenOperationException`, ...) | rejected, `unauthorized` |
| 404 (`ConversationNotFound`, `ActivityNotFoundInConversation`) | rejected, `not_found` |
| 429 | `rate_limited`, after `Retry-After` |
| 5xx, or the answer lost after the request left | `outcome_unknown`: never sent again |
| The connection never made | `transient`, retried |

## Editing, deleting and reacting

Edits and deletions act on the target message alone, through
`PUT` and `DELETE /v3/conversations/{conversation}/activities/{activityId}`. A
bot changes only its own messages ("An agent can't update or delete messages
sent by the user"); Teams' refusal is a final rejection, never a new message.
An edit replaces the whole message under the same rules as a send.

Reactions are `PUT` and `DELETE .../activities/{activityId}/reactions/{type}`,
the call the Teams SDK makes for `addReaction` and `deleteReaction`
([message reactions](https://learn.microsoft.com/en-us/microsoftteams/platform/teams-sdk/in-depth-guides/message-reactions));
the REST path is the SDK's and not on Learn, and the Bot Connector accepted
both calls in the recording. Adding a reaction the bot holds is
a no-op at Teams. An emoji outside the [six types](#reactions) is rejected,
`unsupported`, before any call.

## Limits

| What | Limit |
| --- | --- |
| Clock skew of inbound tokens | 5 minutes |
| Key refresh | Daily; an unknown key at most once a minute |
| Invoke answer | Teams waits 5 seconds and retries twice |
| One message | 80 KB of activity JSON (Teams' limit is 100 KB with mentions and reactions) |
| Rate, per bot and conversation | 7 messages a second, 60 in 30 seconds, 1800 an hour; 2 reactions a second ([rate limits](https://learn.microsoft.com/en-us/microsoftteams/platform/bots/how-to/rate-limit)) |
| Reactions | The six named types outbound; inbound also hexadecimal code-point IDs |

## How it is tested

The shared adapter suite (`conformance.RunAdapterSuite`) runs three times over a
stand-in Bot Framework, once for each way a bot's conversations divide: per room
(a group chat for each peer), per thread (one channel, a thread for each peer)
and per contact (a personal chat with each user). The stand-in publishes OpenID
metadata and a key set with endorsements, signs the activities it posts with the
documented claims, and answers the Connector calls in the documented shapes and
error codes. Package tests cover the token checks one by one (issuer, audience,
expiry and skew, algorithm, signature, `serviceurl`, endorsement, size), the
bounded key refresh, the activity rules, echo and tenant filtering, invokes,
every send and mutation outcome, and the credential's per-call resolution.

The suite skips what the stand-in cannot express: a revoked permission (the
bot's rights are its installation), an echo of uma's own edit (no marker rides
on a message), a reaction summary, a message only some see, an interaction with
no card message or whose deadline has passed (it is the adapter's own five
seconds), and response tokens (Teams has none).

The recorded test (`TestRecordedBot`) replays cassettes recorded against the
Bot Connector in a bot's personal chat with one user: the signing keys read,
the chat created, a message sent, edited, given and stripped of a 👍, a typing
indicator, and the message deleted, each accepted. To record again, follow
[Record Teams cassettes](../recording/teams.md).

## Live verification (2026-11-17)

Run through a Teams-managed dev bot (client secret, Teams channel on, no
messaging endpoint) and a test user of its tenant, in the bot's personal chat
with that user only. The cassettes were recorded in this run.

| Check | Live result |
| --- | --- |
| Create the personal chat (`POST /v3/conversations`) | 201 |
| Send a message | 201, activity id returned |
| Edit it | 200 |
| Reaction add and remove | 200 each |
| Typing indicator | 202 |
| Delete the message | 200 |

Two findings:

- **Install.** A tenant admin's install of the Teams app for the user,
  from the admin center, did not reach the user for hours: chat creation kept
  answering 403 `BadArgument` "Bot is not installed in user's personal
  scope". The user adding the app from the Teams Store made chat creation
  succeed at once. Have the test user add the app themselves.
- **Roster id.** `members[0].id` in the chat-creation request must be the
  user's bare Microsoft Entra object ID; the `29:` and `8:orgid:` forms are
  refused. The `29:` ID is then read from the chat's members and used as
  `TEAMS_USER_ID`.

Still unverified: inbound activities, channel threads and group chats (see
below).

## Live verification (2026-11-16)

Superseded by [Live verification (2026-11-17)](#live-verification-2026-11-17): the send path is verified and the cassettes are refreshed. Kept for the 401 finding.

Run against a single-tenant bot application that authenticates with a
certificate, and a test user of its tenant. The cassettes were not refreshed:
the run stopped at the Bot Connector (below), so those still date from the
earlier recording, made with another bot registration.

| Check | Live result |
| --- | --- |
| `botauth` with a certificate, default `PS256` assertion | Accepted by Microsoft Entra: an app-only token for `https://api.botframework.com`, issued by the tenant, 1727 characters, valid for one hour |
| `botauth` with `AssertionAlg` `RS256` | Accepted: the same kind of token, 1728 characters |
| Bot Connector, create the personal chat (`POST /v3/conversations`) with that token, on the `teams`, `amer` and `emea` service URLs | **Refused, 401** `Authorization has been denied for this request`, the answer a token the Connector does not know gets as well |
| Send, thread reply, edit, reaction add and remove, deletion | Not run: they need the chat the previous step creates |

A second run used a Teams-managed dev bot (client secret, Teams channel on,
no messaging endpoint). The Connector accepted its tenant token, which
confirms the 401 above was a missing bot registration, and answered the
personal-chat creation with 403 `BadArgument` "Bot is not installed in user's
personal scope", twice, on the `teams` and `amer` service URLs, a few minutes
apart. The send path is still unverified; the install for the test user must
take effect (or be reinstalled) before the recording can run.

So the token source is verified live and the send path is not. A 401 from
the Connector for a well-formed tenant token means the Connector holds no bot
registration for the application, or none that accepts that tenant's token;
the run had no way to read or change the registration, and left the tenant
alone. Owner steps, in order:

1. Confirm the application has an Azure Bot resource (or a Developer Portal
   bot) of type single tenant, with the Microsoft Teams channel enabled, and
   that its Microsoft App ID is the application's ID.
2. Confirm the Teams app for that bot is installed in the personal scope for
   the test user (a tenant admin can read this with
   `TeamsAppInstallation.ReadForUser.All`; the automation identity's
   permissions do not include it).
3. Re-run the [recording](../recording/teams.md) with the certificate: mint the
   token with `botauth` (one line from the section above) instead of a client
   secret.

Not verifiable from a recording whatever the registration:

- **Inbound activities** (a user's message, an edit, a deletion, a reaction, a
  card action, redelivery after a refused answer): Teams posts them to the
  bot's messaging endpoint, which needs a public HTTPS URL, and a person
  writing in the chat. Owner step: point the bot's messaging endpoint at the
  host, write a message, edit it, react and delete it as the test user, and
  check each arrives once.
- **Channel threads and group chats**: the test user's personal chat is the
  only place the run may write in.

## Known limits

- Edits, deletions and reactions name the base conversation, never the
  `;messageid=` form: whether the Connector accepts that for a reply inside a
  channel thread is undocumented (Microsoft's SDKs use the threaded ID they
  received). A post the bot made to a channel or chat is unaffected.
- The invoke response's empty message is the documented `activity.message`
  answer with an empty value; what the Teams client shows for it is
  unverified.
- Activities from Teams are not recorded: they arrive signed, and only the
  stand-in has produced them. None has been received live either.
- `textFormat` `plain` is documented by the Bot Framework and the Connector
  accepts it; that the Teams client shows such text without markdown is
  unverified.
- Channel threads and group chats are not recorded; only a personal chat is.
- No reconciliation: a send whose answer was lost stays `outcome_unknown`.
- One audience: public. Targeted messages are a developer preview.
- A channel's `service_url` is fixed; Teams may move a tenant to another
  service URL, which then needs a configuration change.
- Experimental: configuration may change ([Compatibility](../compatibility.md)).
