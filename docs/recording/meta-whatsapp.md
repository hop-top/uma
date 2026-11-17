# Record WhatsApp Cloud API cassettes

The `meta.whatsapp` adapter's recorded tests send through Meta's WhatsApp
Cloud API from a test business number. A Meta developer app gives you that
number for free; you need no business verification and no payment method.

Common steps (record, review, replay, clean up) are in
[Record adapter cassettes](README.md).

## What the recording does

`TestRecordedTemplateSend` sends the `hello_world` template to your test
recipient: one real WhatsApp message arrives on that phone.
`TestRecordedUnknownTemplateRejected` sends a template that does not exist
(`no_such_template_uma`); Meta rejects it with error `132001` and nothing is
delivered. Both call `POST /v24.0/<phone number ID>/messages`.

## Get a test number and token

1. Create an app in the [Meta App Dashboard](https://developers.facebook.com/apps/)
   with the WhatsApp use case ("Connect with customers through WhatsApp"),
   then choose to start using the API. Meta creates a test WhatsApp Business
   account and a test business phone number for the app
   ([get started](https://developers.facebook.com/documentation/business-messaging/whatsapp/get-started),
   [test accounts](https://developers.facebook.com/documentation/business-messaging/whatsapp/about-the-platform)).
2. On the app's **API Setup** page, note the test number's **Phone number
   ID** (not the phone number itself).
3. In the **To** field of the same page, add the WhatsApp number that will
   receive the test message, and confirm it as the page asks. Use a phone
   you own. Test numbers only deliver to recipients added here.
4. Get an access token:
   - **One recording session:** select **Generate access token** on API
     Setup. The token is short-lived (it "expires quickly"), which is what
     you want for a recording.
   - **Repeated recordings:** create a system user in Business settings,
     assign it the app and the test WhatsApp Business account, and generate
     a token for it
     ([access tokens](https://developers.facebook.com/documentation/business-messaging/whatsapp/access-tokens)).
     Pick the shortest expiry the dialog offers that fits your schedule.

### Permissions

The adapter declares one permission, `whatsapp_business_messaging`: it only
sends messages. Grant `whatsapp_business_management` (or
`business_management`) only if the token dialog requires it, and never give a
recording token access to a production WhatsApp Business account.

## Record WhatsApp Cloud API cassettes

| Variable | Value |
| --- | --- |
| `WHATSAPP_ACCESS_TOKEN` | The access token from step 4 |
| `WHATSAPP_PHONE_NUMBER_ID` | The test number's Phone number ID from step 2 |
| `WHATSAPP_RECIPIENT` | The recipient from step 3, with country code, in the form Meta recommends: a leading `+` |

The test reads them only when `XRR_MODE` is `record` or `passthrough`, and
fails if one is missing.

```sh
cd adapter/messaging/meta
export WHATSAPP_ACCESS_TOKEN=<token> WHATSAPP_PHONE_NUMBER_ID=<phone number ID> WHATSAPP_RECIPIENT=+<country code><number>
XRR_MODE=record go test -count=1 ./whatsapp -run TestRecorded
git diff --stat testdata/cassettes
```

Cassettes land in `adapter/messaging/meta/whatsapp/testdata/cassettes/meta`.
The adapter calls Graph API `v24.0` by default, which Meta supports until
February 2028 ([Graph API changelog](https://developers.facebook.com/docs/graph-api/changelog)).

## Keep the traffic safe

- Send only to the recipient you added in step 3; never set
  `WHATSAPP_RECIPIENT` to someone else's number.
- Do not create, edit or delete templates, webhooks or phone numbers on the
  app while recording.
- WhatsApp messages cannot be recalled through the API; delete the test
  message on the recipient phone if you want it gone.
- Revoke a system user token you no longer need.

## Check the cassettes

What the test removes: the `Authorization` header (the token is sent as a
bearer token), the Phone number ID (recorded as `PHONE_NUMBER_ID`) and the
recipient in every form Meta writes it (recorded as `RECIPIENT`): as you set
it, as digits without the `+`, and percent-encoded (`%2B...`, in either
letter case).

The send answer's `contacts` entry carries the recipient twice, as `input`
(your value) and as `wa_id`, the number Meta resolved, without the `+`
([send messages](https://developers.facebook.com/documentation/business-messaging/whatsapp/messages/send-messages)).
Meta can resolve a `wa_id` that differs from the number you sent, so the
test also replaces the phone-bearing fields of every JSON body by name,
whatever their value: `input`, `wa_id`, `recipient_id` and `from` become
`RECIPIENT`, and `display_phone_number` (the test business number) becomes
`BUSINESS_PHONE`. These are the fields of the send answer and of the webhook
payloads the adapter parses.

When you record, the test then fails if the access token, the Phone number
ID or the recipient survives in any form in a cassette. Grep as a second
check; a `wa_id` that differs from your number is covered by the field
replacement and no grep can find it:

```sh
cd adapter/messaging/meta/whatsapp
grep -rnF -e "$WHATSAPP_PHONE_NUMBER_ID" -e "${WHATSAPP_RECIPIENT#+}" -e "$WHATSAPP_ACCESS_TOKEN" testdata/cassettes/meta
```

The `wamid.` message IDs in responses are not personal data and stay.
