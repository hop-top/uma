# Record Zendesk cassettes

The `zendesk.tickets` adapter's recorded test writes to one throwaway
ticket of a Zendesk Support account, authenticated with an agent's API token
or with an OAuth access token. You need an account you can administer and a
ticket whose requester is you.

Common steps (record, review, replay, clean up) are in
[Record adapter cassettes](README.md).

## What the recording does

`TestRecordedSandbox`, on the ticket you name:

1. adds a public reply (`PUT /api/v2/tickets/<id>`), which Zendesk may email
   to the ticket's requester;
2. finds that reply in the ticket's audits (`GET
   /api/v2/tickets/<id>/audits`);
3. adds an internal note (`PUT /api/v2/tickets/<id>`).

## Get an account

- **Free trial:** 14 days, enough for one recording session.
- **Sponsored account:** for Zendesk Marketplace app developers; it does not
  expire. Request it with a `d3v-` subdomain
  ([trial or sponsored account](https://developer.zendesk.com/documentation/api-basics/getting-started/getting-a-trial-or-sponsored-account-for-development/)).
- **Sandbox of a paid account:** included with Enterprise plans and sold as
  an add-on on others
  ([sandboxes](https://support.zendesk.com/hc/en-us/articles/4408822049818-Creating-a-sandbox-environment-to-test-changes)).
  Never record against the production instance.

Choose a distinctive subdomain: the test replaces it as a literal string
everywhere in recorded bodies.

## Choose a credential

Zendesk is retiring API tokens: unused tokens are deleted after 30 days, and
all API tokens stop working by April 30, 2027. Record with an OAuth access
token; the API token variant stays until then. Both variants record the same
exchanges and their cassettes are interchangeable: replay does not depend on
which one recorded them.

### OAuth access token (recommended)

The adapter's `auth: oauth` mode sends `Authorization: Bearer <access
token>`.

1. Register an OAuth client in Admin Center, under the APIs settings: a name,
   an identifier, a redirect URL and the scopes below
   ([OAuth authentication](https://developer.zendesk.com/documentation/api-basics/authentication/using-oauth-to-authenticate-zendesk-api-requests-in-a-web-app/)).
2. Get an access token for it with the scopes `tickets:read` (reading the
   ticket's audits) and `tickets:write` (replying and noting), or the broad
   `read write`
   ([grant types](https://developer.zendesk.com/api-reference/ticketing/oauth/grant_type_tokens/)).
   The grant type is yours to choose (authorization code, or client
   credentials). Access tokens are short lived (30 minutes by default for
   clients created after April 30, 2026): get one right before you record.
3. Act as an agent with access to the sandbox only; the token carries that
   agent's role.

### API token

1. In Admin Center, under **Apps and integrations > APIs > API
   configuration**, turn on **Allow API token access**
   ([API access](https://support.zendesk.com/hc/en-us/articles/8662004958746-Turning-on-and-off-API-access)).
2. Under **API tokens**, add a token and store it in your secret manager
   ([API tokens](https://support.zendesk.com/hc/en-us/articles/4408889192858-Managing-API-token-access-to-the-Zendesk-API)).
3. Note the email of the agent the recording acts as. The adapter
   authenticates as `<email>/token` with basic auth
   ([security and authentication](https://developer.zendesk.com/api-reference/introduction/security-and-auth/)).
   An API token acts with that agent's full role; use an agent with access
   to the sandbox only.

## Create the throwaway ticket

Create a ticket whose requester is an address you own (the agent itself
works), with a neutral subject and description. Its ID is the
`ZENDESK_TICKET_ID`.

The test records the ticket ID as the stand-in `424242`, only where it is a
ticket ID (see [what the test removes](#check-the-cassettes)), so a short ID
such as `4` does not disturb timestamps and other IDs that contain the same
digit. Use the throwaway ticket's own ID; `424242` is refused. Replay uses
`424242` as well, unless `ZENDESK_TICKET_ID` is set.

## Record Zendesk cassettes

| Variable | Value |
| --- | --- |
| `ZENDESK_AUTH` | `api_token` (default) or `oauth` |
| `ZENDESK_SUBDOMAIN` | The account's subdomain (`<subdomain>.zendesk.com`) |
| `ZENDESK_TICKET_ID` | The throwaway ticket's ID |
| `ZENDESK_EMAIL` | The agent's email (`api_token` only) |
| `ZENDESK_API_TOKEN` | The API token (`api_token` only) |
| `ZENDESK_ACCESS_TOKEN` | The OAuth access token (`oauth` only) |

The test fails in record mode when a variable its `ZENDESK_AUTH` needs is
missing, and refuses `ZENDESK_AUTH` values other than the two above.

```sh
cd adapter/ticket/zendesk
export ZENDESK_AUTH=oauth ZENDESK_SUBDOMAIN=<subdomain> ZENDESK_ACCESS_TOKEN=<access token> ZENDESK_TICKET_ID=<ticket ID>
XRR_MODE=record go test -count=1 -run TestRecordedSandbox
```

For an API token, leave `ZENDESK_AUTH` unset and export `ZENDESK_EMAIL` and
`ZENDESK_API_TOKEN` in place of `ZENDESK_ACCESS_TOKEN`.

Cassettes land in `adapter/ticket/zendesk/testdata/cassettes/zendesk`. The
test also records Zendesk's per-endpoint rate limit headers
([rate limits](https://developer.zendesk.com/api-reference/introduction/rate-limits/)).

## Keep the traffic safe

- Write only to the throwaway ticket, whose requester is you.
- Do not change triggers, automations, webhooks or users while recording;
  a trigger firing on the reply changes what the audits record.
- Afterwards, delete the ticket, and revoke the access token or delete the
  API token.

## Check the cassettes

What the test removes:

- the `Authorization` header;
- the subdomain and the agent email wherever they appear literally
  (recorded as `ZENDESK_SUBDOMAIN` and `ZENDESK_EMAIL`);
- the ticket ID, replaced by `424242` only where it is a ticket ID: a URL
  path segment after `tickets` or a `ticket_id` query parameter; the JSON
  fields `ticket_id`, `problem_id`, `followup_ids` and `ticket_ids`; the `id`
  of a `ticket` object or a `tickets` element; and the same URL shapes inside
  any string (a ticket's `url`, a pagination link). The ID's digits inside
  timestamps and other IDs stay as Zendesk sent them;
- the recorder's details in each audit's `metadata.system` (and in a
  comment's): `client` becomes `uma-recorded-client`, `ip_address`
  `192.0.2.1`, `location` empty, `latitude` and `longitude` `0`, and any other
  field there a `<redacted:system.FIELD>` marker
  ([ticket audits](https://developer.zendesk.com/api-reference/ticketing/tickets/ticket_audits/)).

The test checks every body before it saves it and fails the recording,
naming the field, when:

- a `ticket_id`-like value or any string holding the ticket ID as a whole
  token is left (a timestamp is not searched). An unrelated ID that equals
  the ticket's ID, or a subject such as `Ticket 4`, fails too: re-record
  with another ticket, or neutral content;
- `ip_address`, `location`, `client`, `latitude` or `longitude` holds
  anything but the neutral values, in `metadata.system` or anywhere else in
  a body;
- the credential, the agent email or the subdomain appears literally.

The message never prints the value. A failure leaves the cassette written,
so delete `testdata/cassettes/zendesk` and record again after the test is
fixed.

What it does not cover, so check by hand:

- **Names and user IDs** of the agent and the requester (`author_id`,
  `requester_id`, `submitter_id`, a `via.source` sender's name or address).
- **Content you did not write**: the ticket's subject and description, and
  what a trigger added to the audits.

```sh
cd adapter/ticket/zendesk
grep -rnE '"(ip_address|location|latitude|longitude|client)"' testdata/cassettes/zendesk
grep -rnF -e "$ZENDESK_EMAIL" -e "$ZENDESK_API_TOKEN" -e "$ZENDESK_ACCESS_TOKEN" -e "$ZENDESK_SUBDOMAIN" testdata/cassettes/zendesk
```

The first command shows only the neutral values listed above; the second
prints nothing (skip an unset variable, which would match everything).
