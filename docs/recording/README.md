# Record adapter cassettes

Adapter tests replay provider HTTP exchanges from committed
[xrr](../../testing/xrrtest/README.md) cassettes, so `go test` never reaches
a provider and needs no credentials. To add or refresh a cassette you run
the recorded test once against a real test account. This page covers the
steps every provider shares; each provider guide covers the account,
credentials and environment variables that provider needs.

## Pick your provider

| Provider | Adapter | Recorded test | Guide |
| --- | --- | --- | --- |
| WhatsApp Cloud API (Meta) | `adapter/messaging/meta/whatsapp` | `TestRecordedTemplateSend`, `TestRecordedUnknownTemplateRejected` | [Record WhatsApp Cloud API cassettes](meta-whatsapp.md) |
| Gmail API | `adapter/mail/google` | `TestRecordedGmail`, `TestRecordedGmailHistory` | [Record Gmail cassettes](gmail.md) |
| Twilio SMS | `adapter/messaging/twilio/sms` | `TestRecordedSendAndFetch`, `TestRecordedReconcileFindsSent`, `TestRecordedAnswers`, `TestRecordedHistory` | [Record Twilio cassettes](twilio.md) |
| Twilio WhatsApp | `adapter/messaging/twilio/whatsapp` | `TestRecordedSendAndFetch` | [Record Twilio cassettes](twilio.md#record-twilio-whatsapp-cassettes) |
| Microsoft Graph mail | `adapter/mail/microsoft` | `TestRecordedGraph`, `TestRecordedGraphHistory`, `TestRecordedGraphInbound`, `TestRecordedGraphErrors` | [Record Microsoft Graph cassettes](microsoft-graph.md) |
| GitHub issues | `adapter/ticket/github` | `TestRecordedExchanges`, `TestRecordedTransitions`, `TestRecordedMutations`, `TestRecordedHistory`, `TestRecordedUpdates` | [Record GitHub cassettes](github.md) |
| GitLab issues | `adapter/ticket/gitlab` | `TestRecordedExchanges`, `TestRecordedTransitions`, `TestRecordedHistory`, `TestRecordedWebhookPayloads` | [Record GitLab cassettes](gitlab.md) |
| Zendesk Support | `adapter/ticket/zendesk` | `TestRecordedSandbox` | [Record Zendesk cassettes](zendesk.md) |
| Jira Cloud issues | `adapter/ticket/jira` | `TestRecordedComments`, `TestRecordedWebhooks` | [Record Jira cassettes](jira.md) |
| Linear | `adapter/ticket/linear` | `TestRecordedSandbox` | [Record Linear cassettes](linear.md) |
| Slack | `adapter/chat/slack` | `TestRecordedWorkspace`, `TestRecordedArchiveEvents` | [Record Slack cassettes](slack.md) |
| Telegram | `adapter/chat/telegram` | `TestRecordedBot` | [Record Telegram cassettes](telegram.md) |
| Microsoft Teams | `adapter/chat/teams` | `TestRecordedBot` | [Record Teams cassettes](teams.md) |
| Discord | `adapter/chat/discord` | `TestRecordedBot`, `TestRecordedBot/thread_archive` | [Record Discord cassettes](discord.md) |
| iMessage through the reference relay gateway (Blooio, BlueBubbles Server) | `examples/imessagegateway` | `TestRecordedBlooio`, `TestRecordedBlueBubbles` | [Record iMessage cassettes](imessage.md) |
| IMAP and SMTP | `adapter/mail/imap`, `adapter/mail/smtp` | none; opt-in `TestLiveStateVerbs`, `TestLiveStateFollowsTheMailbox` and the other `TestLive*` against containers | [IMAP and SMTP have no record path](imap-smtp.md) |

Cassettes are committed for GitHub issues, GitLab issues (the webhook payloads excepted), Twilio SMS, Twilio WhatsApp,
Gmail, Jira issues (the webhook payloads excepted), Linear, Microsoft Teams, Microsoft Graph mail, Discord (`TestRecordedBot`,
the `thread_archive` subtest included) and the Blooio iMessage
path. The recorded tests of the other providers and the Slack archive events skip until someone
records them; see
[how the adapters are tested](../adapters/README.md#how-the-adapters-are-tested).

The web widget (`adapter/web/widget`), the bot companion, `contrib/kit` and
`contrib/otel` talk to no remote provider, and `store/postgres` tests use a
local container: none of them need provider credentials.

## Record, review, replay

1. **Get test credentials** as the provider guide describes. Use a test
   account, never a production one.
2. **Export them in the recording shell only.** Keep secrets in your secret
   manager and export them for the one shell that records; do not write them
   to `.env`, shell profiles or files in the repository. Close the shell
   when you are done.
3. **Record** from the adapter's module directory, scoped to the recorded
   test with `-run`, and with `-count=1` so Go does not answer from its test
   cache:

   ```sh
   XRR_MODE=record go test -count=1 -run TestRecordedGmail ./...
   ```

   Never run `XRR_MODE=record` without `-run`: every recorded test in the
   module would try to reach its provider.
4. **Review the diff** of `testdata/cassettes/<provider>` with the
   [scrubbing checklist](#check-cassettes-before-committing) below.
5. **Replay with the recording variables unset**, in a fresh shell:

   ```sh
   go test -count=1 -run TestRecordedGmail ./...
   ```

   A cassette miss here means a real value reached a recorded request: the
   test's placeholders did not cover it. Fix the placeholders and record
   again rather than committing the cassette.
6. **Clean up** what the recording created at the provider (messages,
   comments, drafts) as the provider guide describes, and revoke short-lived
   tokens you no longer need.

When requests were removed from a test, delete the provider's cassette
directory before recording so stale files do not linger.

## Keep recording traffic safe

- Send only to recipients you own or that the provider marks as test
  recipients. Every guide names the test recipient variable.
- Record the smallest scenario the test runs; do not loop the recording to
  "get a clean one" against a paid sender.
- Do not change account, app or webhook configuration while recording; a
  cassette should capture the provider's answers, not a configuration in
  flux.
- Use credentials with the narrowest scopes the guide lists, on an account
  or sandbox that holds no customer data.

## Check cassettes before committing

`xrrtest` keeps credentials out of cassettes on its own: it drops
`Authorization`, cookie and API-key headers, redacts credential-named JSON,
form and query fields, and replaces the identifiers each test passes as
placeholders (account IDs, addresses, phone numbers, hosts). See
[what is recorded](../../testing/xrrtest/README.md#what-is-recorded). It
cannot know about identifiers a test does not declare, so check every
recording:

- [ ] Replay passes with the recording variables unset (step 5 above).
- [ ] No exported secret or identifier appears in the cassettes. From the
      adapter's module directory, pass every value you exported:

  ```sh
  for v in "$VALUE_1" "$VALUE_2"; do
    [ -n "$v" ] && grep -rnF -- "$v" testdata/cassettes && echo "found: recorded value above"
  done
  ```

  Phone numbers also appear URL-encoded (`%2B1555...`) or without the `+`;
  grep for the digits alone (`"${NUMBER#+}"`) to catch every form.
- [ ] No personal data from the test account: names, other addresses,
      message bodies you did not send, IP addresses or locations. The
      provider guides list where each provider puts them.
- [ ] Each provider guide's own checklist.

A response body may be edited by hand to replace a stray identifier with a
neutral value (for example `192.0.2.1` for an IP address): xrr fingerprints
requests only. Do not edit recorded requests; record again instead.
