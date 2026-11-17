# Record Teams cassettes

The `microsoft.teams` adapter's recorded test talks to the Bot Framework as a
bot installed for one test user: it reads the Bot Framework signing keys,
creates (or reopens) the bot's personal chat with the user, sends a message,
edits it, adds and removes a 👍 reaction, sends a typing indicator and deletes
the message. You need a Microsoft 365 tenant with Teams that holds no customer
data, a bot registration (an Azure Bot resource, or a Developer Portal bot
when the tenant has no Azure subscription), and a user of that tenant.

Common steps (record, review, replay, clean up) are in
[Record adapter cassettes](README.md).

## What the recording does

`TestRecordedBot`, for the user you name:

1. reads the OpenID metadata at
   `https://login.botframework.com/v1/.well-known/openidconfiguration` and the
   key set it names, and checks that a key is endorsed for `msteams`;
2. creates or returns the personal chat (`POST /v3/conversations`) and posts a
   message to it (`POST /v3/conversations/{id}/activities`);
3. edits it (`PUT /v3/conversations/{id}/activities/{activityId}`);
4. adds the `like` reaction and removes it
   (`PUT` and `DELETE .../activities/{activityId}/reactions/like`);
5. sends a typing indicator (an activity of type `typing`);
6. deletes the message (`DELETE .../activities/{activityId}`).

Activities Teams posts to the bot (messages, edits, reactions, card actions)
are not recorded: they arrive signed by the Bot Connector, and a replay cannot
produce such a token. The token request to Microsoft Entra is not recorded
either: you mint the token before the run (below), as a host does.

## Get a tenant

- **Microsoft 365 Developer Program sandbox.** No longer open to everyone:
  "Eligible Microsoft 365 Developer Program members can request" one, which
  today means Visual Studio Professional or Enterprise subscribers, some
  Microsoft AI Cloud Partner Program and ISV Success members, and Premier or
  Unified Support customers; not in GCC, GCC High or DoD
  ([Microsoft 365 Developer Program](https://learn.microsoft.com/en-us/office/developer-program/microsoft-365-developer-program)).
- **A tenant you own** with Teams licences and no customer data, such as a
  trial.

Never record in a tenant people work in: the test posts, edits and deletes.

## Register the bot

Without an Azure subscription, register the bot in the
[Developer Portal](https://dev.teams.microsoft.com/) instead of steps 1 to 3:
**Tools**, **Bot management**, **New bot**. It creates the Microsoft Entra app
registration (the bot ID is its App ID) and enables the Teams channel; add a
client secret under **Client secrets** or on the app registration. Its tokens
come from the tenant's endpoint, as below. The bot needs no messaging endpoint
for the recording.

1. In the Azure portal, create an **Azure Bot** resource. Under **Type of
   App** choose **Single Tenant**: "Multi-tenant bot creation will be
   deprecated after July 31, 2025"
   ([create an Azure Bot](https://learn.microsoft.com/en-us/azure/bot-service/abs-quickstart)).
   Let it create a Microsoft Entra app registration in your tenant.
2. From the bot's **Configuration** page note the **Microsoft App ID** and the
   **App Tenant ID**. Under **Manage Password** (the app registration's
   **Certificates & secrets**), create a client secret and keep its value in
   your secret manager.
3. Under **Channels**, add **Microsoft Teams**.
4. Build a Teams app for the bot in the
   [Developer Portal](https://dev.teams.microsoft.com/) (or with the Microsoft
   365 Agents Toolkit): **Apps**, **New app**, **App features**, **Bot**, enter
   the App ID as the existing bot, and allow the **Personal** scope. Download
   the app package (a zip). The manifest's `bots[].botId` must be the App ID.
5. Allow custom app upload for your user (Teams admin center, **Setup
   policies**), then in Teams: **Apps**, **Manage your apps**, **Upload an
   app**, **Upload a custom app**, pick the zip and **Add**
   ([upload your app](https://learn.microsoft.com/en-us/microsoftteams/platform/concepts/deploy-and-publish/apps-upload)).
   The bot can now open a personal chat with you
   ([proactive messages](https://learn.microsoft.com/en-us/microsoftteams/platform/bots/how-to/conversations/send-proactive-messages)).
   A tenant admin can instead install it for the test user with Microsoft
   Graph: publish the zip to the tenant's catalog
   (`POST /appCatalogs/teamsApps?requiresReview=false`, `Content-Type:
   application/zip`), then `POST /users/{user}/teamwork/installedApps` with
   `teamsApp@odata.bind` naming it; the delegated permissions are
   `AppCatalog.ReadWrite.All` and `TeamsAppInstallation.ReadWriteForUser`.
6. Find your Teams user ID (`29:...`, specific to this bot). With the token
   minted [below](#mint-the-bot-connector-token), open the personal chat naming
   the user by their Microsoft Entra object ID, then list its members:

   ```sh
   SU=https://smba.trafficmanager.net/teams/   # the public cloud's service URL
   CONV="$(curl -fsS -X POST "${SU}v3/conversations" \
     -H "Authorization: Bearer $TEAMS_CONNECTOR_TOKEN" -H 'Content-Type: application/json' \
     -d '{"bot":{"id":"28:'"$TEAMS_APP_ID"'"},"members":[{"id":"<Entra object ID>"}],
          "channelData":{"tenant":{"id":"'"$TEAMS_TENANT_ID"'"}},"isGroup":false}' | jq -r .id)"
   curl -fsS "${SU}v3/conversations/$CONV/members" \
     -H "Authorization: Bearer $TEAMS_CONNECTOR_TOKEN" | jq -r '.[0].id'
   ```

   `$CONV` (`a:...`) is the personal chat's ID; keep it for the recording.
   Alternatively, both IDs and the service URL are on any activity Teams posts
   to the bot's messaging endpoint (`from.id`, `conversation.id`,
   `serviceUrl`).

## Mint the Bot Connector token

The adapter does not mint a token itself: a host (or `botauth`) mints it from the app's client
credentials and hands it over as `ports.OAuth2`. For the recording, mint one
in the recording shell; it lasts an hour
([Bot Connector authentication](https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-authentication)).
Export the client secret as `TEAMS_APP_SECRET` from your secret manager, in
this shell only. The secret is read from standard input so it never shows in
the process list:

```sh
export TEAMS_APP_ID=<app ID> TEAMS_TENANT_ID=<tenant ID>
TEAMS_CONNECTOR_TOKEN="$(printf %s "$TEAMS_APP_SECRET" | curl -fsS \
  "https://login.microsoftonline.com/$TEAMS_TENANT_ID/oauth2/v2.0/token" \
  -d grant_type=client_credentials -d client_id="$TEAMS_APP_ID" \
  --data-urlencode client_secret@- \
  --data-urlencode scope=https://api.botframework.com/.default | jq -r .access_token)"
export TEAMS_CONNECTOR_TOKEN
```

A production host does the same with
[MSAL Go](https://github.com/AzureAD/microsoft-authentication-library-for-go)
(`confidential.NewCredFromSecret` and `AcquireTokenByCredential` with the scope
`https://api.botframework.com/.default`).

## Check the token source live

`botauth` is tested against a stand-in token endpoint only. The owner can
check it against Microsoft Entra once, outside the test suite, with a
throwaway program (not committed) that builds a `botauth.Source` from the
variables below and calls `Token(ctx, false)`:

- `TEAMS_APP_ID`, `TEAMS_TENANT_ID`, and either `TEAMS_APP_SECRET` or a
  certificate uploaded to the app registration (**Certificates & secrets**)
  with its PEM key read from a file path, never printed.
- Run once with a secret, once with the certificate and the default `PS256`
  assertion, and once with `AssertionAlg: botauth.AlgRS256`; note which the
  endpoint accepts. Then use the token for the send in step 6.
- Print only the token's length and expiry. Remove the certificate from the
  app registration afterwards if it was added for the check.
- A sovereign cloud needs its `Authority` and, if different, `Scope`.

## Record Teams cassettes

| Variable | Value |
| --- | --- |
| `TEAMS_CONNECTOR_TOKEN` | The token minted above |
| `TEAMS_APP_ID` | The bot's Microsoft App ID |
| `TEAMS_TENANT_ID` | The tenant ID |
| `TEAMS_SERVICE_URL` | The service URL from step 6 |
| `TEAMS_USER_ID` | Your Teams user ID (`29:...`) from step 6 |
| `TEAMS_CONVERSATION_ID` | Optional: the personal chat's ID (`a:...`) from step 6 |

The test fails in record mode when one of the first five is missing. Without
`TEAMS_CONVERSATION_ID` the chat's ID is recorded as is.

```sh
cd adapter/chat/teams
export TEAMS_SERVICE_URL=<service URL> TEAMS_USER_ID=<29:...> TEAMS_CONVERSATION_ID=<a:...>
XRR_MODE=record go test -count=1 -run TestRecordedBot
```

Cassettes land in `adapter/chat/teams/testdata/cassettes/teams`, one per
exchange: the two key reads, two conversation creations (the send and the
typing indicator each open the chat), the send, the edit, the two reaction
calls, the typing activity and the deletion. The key set is large (about
900 KB, some 260 public keys) and is committed as recorded.

While recording, watch the personal chat: the message appears, changes,
gets and loses a 👍 and disappears. Note what Teams shows; the adapter page
lists what is unverified (`textFormat` `plain`, the reaction call's REST path).

## Keep the traffic safe

- Record only in the personal chat with your own test user.
- If the test stops before the deletion, the message stays: a user cannot
  delete a bot's message, so run the test again or uninstall the app.
- Afterwards, delete the client secret, uninstall the app and, when you are
  done with it, delete the Azure Bot resource and its app registration.

## Check the cassettes

What the test removes: the `Authorization` header, and the app ID, tenant ID,
service URL (host and path), user ID and, when set, conversation ID where they
appear literally (recorded as `TEAMS_APP_ID`, `TEAMS_TENANT_ID`,
`TEAMS_SERVICE_HOST_AND_PATH`, `TEAMS_USER_ID` and `a:TEAMS_CONVERSATION_ID`).

What it does not cover:

- **The personal chat's conversation ID** (`a:...`) when
  `TEAMS_CONVERSATION_ID` was not set. It is opaque but specific to the bot and
  the user. Do not replace it by hand: replay matches requests by a fingerprint
  of the URL, so an edited URL no longer finds its cassette. Record again with
  the variable set; the chat's ID stays the same for the bot and the user.
- **Activity IDs** are the messages' own and stay: they are what later
  requests name.
- **The key set** is public; nothing to remove.
- **Response headers and bodies** the Connector adds, such as correlation IDs
  in error bodies; there should be no names or emails.

```sh
cd adapter/chat/teams
grep -rnE '"(name|aadObjectId|userPrincipalName|email)"' testdata/cassettes/teams
grep -rnF -e "$TEAMS_CONNECTOR_TOKEN" -e "$TEAMS_APP_ID" -e "$TEAMS_TENANT_ID" -e "$TEAMS_USER_ID" -e "$TEAMS_CONVERSATION_ID" testdata/cassettes/teams
```
