# IMAP and SMTP have no record path

The `imap.mail` and `smtp.mail` adapters (`adapter/mail/imap`,
`adapter/mail/smtp`) have no recorded tests and read no credentials from the
environment. Their tests run against in-process servers instead: an IMAP
server built on `go-imap`'s `imapserver` and SMTP servers built on
`go-smtp`, on loopback listeners, with the TLS, SASL, Kerberos and NTLM
cases served from `adapter/mail/auth` test fixtures. They need no account
and no network.

xrr cannot record them as they stand: it records HTTP, gRPC, Redis, SQL and
subprocess calls, and IMAP and SMTP are stateful line protocols on their own
connections. The substitute for cassettes is an opt-in live run against real
mail servers in containers, described below. It is evidence of a different
kind: it is run again, not replayed, and what it covers is stated in
[Parity evidence](#parity-evidence).

- [Run the live tests](#run-the-live-tests)
- [What the harness starts](#what-the-harness-starts)
- [Use your own servers](#use-your-own-servers)
- [What the live tests cover](#what-the-live-tests-cover)
- [History has no recording either](#history-has-no-recording-either)
- [Parity evidence](#parity-evidence)

## Run the live tests

You need Docker (any engine that publishes ports on `127.0.0.1`), `openssl`
and a shell. From the repository root:

```sh
eval "$(adapter/mail/testdata/live/up.sh)"       # start containers, export UMA_LIVE_*
cd adapter/mail
go test -count=1 -run TestLive -v ./imap ./smtp  # the live tests
cd ../..
adapter/mail/testdata/live/down.sh               # remove everything up.sh made
```

`up.sh` builds two small images, starts three containers, waits until each
greets and prints the `export` lines the tests read; the same lines are saved
in `$UMA_LIVE_STATE/env`. A full run takes about a minute once the images are
built, most of it deliberate waits that show nothing unwanted arrives.

Without `UMA_LIVE_MAIL=1` every live test skips with a message that names
the variable, so `go test ./...` and `make check` never need Docker or a mail
server. With it set, a group of endpoints that is half configured fails the
run; a group that is absent skips its tests. In CI, `UMA_REQUIRE_LIVE_MAIL=1`
turns an absent group into a failure too.

Tear down with `down.sh`, which removes only containers whose name starts
with `uma-live-evidence-` and that carry the label `uma.live-evidence=1`
(`UMA_LIVE_RUN` narrows it to one run), plus the run's certificate directory.
`down.sh --images` also removes the two images `up.sh` built. It touches no
other container, volume, network or image, and nothing in the harness prunes.

## What the harness starts

Every container publishes to `127.0.0.1` only, on ports Docker picks. The
mail never leaves the machine: Mailpit relays nothing, and Postfix is
configured with one transport that points at the test's own listener until
the test sets it, and with DNS lookups off.

| Container | Image | Role | Account |
| --- | --- | --- | --- |
| `uma-live-evidence-<run>-dovecot` | built from `testdata/live/dovecot`, Dovecot 2.3.19 on `debian:bookworm-slim@sha256:7c7b2c96...c587` | IMAP, implicit TLS on 993 and STARTTLS on 143, `PLAIN` after TLS; `MOVE`, `UIDPLUS`, `CONDSTORE` and `SPECIAL-USE`, with `Archive`, `Trash` and `Junk` mailboxes carrying the special-use attributes | `desk@example.test`, password `desk-live-pw` |
| `uma-live-evidence-<run>-mailpit` | `axllent/mailpit:v1.31.2@sha256:74d609a4...863d` | submission server and catcher: STARTTLS, AUTH, HTTP API to read back what it caught | the same account |
| `uma-live-evidence-<run>-postfix` | built from `testdata/live/postfix`, Postfix 3.7.11 on the same Debian base | the sending MTA of the `smtp.mail` receiver tests, with 5-second retry timers | none |

Every certificate is made by `up.sh` for that run: a two-day CA and one
server certificate for `localhost` and `127.0.0.1`, written under
`$UMA_LIVE_STATE/certs`. The tests trust that CA only, through `UMA_LIVE_CA`.
Passwords are throwaway values in the repository; the domains are the
reserved `example.test`, `example.com` and `example.org`.

Dovecot and Postfix install their packages from the Debian base's current
repositories when the image is built; the versions above are those of the
recorded run. Pin them in the Dockerfiles when a run must be exactly
reproduced.

The tests read these variables. `up.sh` sets all of them.

| Variable | Meaning |
| --- | --- |
| `UMA_LIVE_MAIL` | `1` opts in |
| `UMA_REQUIRE_LIVE_MAIL` | `1` makes an absent endpoint group a failure instead of a skip |
| `UMA_LIVE_CA` | PEM file of the CA that signed the servers' certificates; empty uses the system roots |
| `UMA_LIVE_IMAP_HOST`, `_TLS_PORT`, `_STARTTLS_PORT` | the IMAP server; at least one port |
| `UMA_LIVE_IMAP_USER`, `UMA_LIVE_IMAP_PASS` | the mailbox account; each test creates mailboxes of its own in it and deletes them |
| `UMA_LIVE_SMTP_HOST`, `_STARTTLS_PORT` or `_PLAIN_PORT` | the submission server; a plain port is used on loopback only |
| `UMA_LIVE_SMTP_USER`, `UMA_LIVE_SMTP_PASS` | its account; for `imap.mail` the same as the IMAP account, as the adapter has one credential |
| `UMA_LIVE_SMTP_API` | base URL of the catcher's HTTP API (Mailpit's `/api/v1`) |
| `UMA_LIVE_MTA_CONTAINER`, `UMA_LIVE_MTA_PORT` | the Postfix container (the tests run `docker exec` only in a container named `uma-live-evidence-*`) and its published SMTP port |
| `UMA_LIVE_MTA_RELAY_HOST` | how the MTA container reaches the test host; default `host.docker.internal` |

The Postfix container reaches the test's listener, which is bound to
`127.0.0.1`, as `host.docker.internal`; OrbStack forwards that name to the
host's loopback, and it was run there only. On an engine that does not (plain
Docker on Linux), the receiver tests that use Postfix need
`UMA_LIVE_MTA_RELAY_HOST` set to an address of the host the container can
reach, and a listener bound to it; the other tests do not use Postfix.

## Use your own servers

The tests need nothing from `up.sh` but endpoints. Point the variables at
servers you run and the matching tests use them:

- **A mail catcher you already run.** Mailpit's SMTP port and API are
  enough for the submission tests (`imap.mail` sending, `smtp.mail` sending).
  Set `UMA_LIVE_SMTP_HOST`, `UMA_LIVE_SMTP_PLAIN_PORT` (or `_STARTTLS_PORT`),
  `UMA_LIVE_SMTP_USER`, `UMA_LIVE_SMTP_PASS` and `UMA_LIVE_SMTP_API`. A catcher
  that accepts any credential over plain SMTP, as Mailpit does with
  `MP_SMTP_AUTH_ACCEPT_ANY=1` and `MP_SMTP_AUTH_ALLOW_INSECURE=1`, works; with
  only a plain port the test of a wrong password skips, because such a
  catcher takes any. The tests
  read back only messages that carry their own token, and delete nothing.
- **An IMAP server.** Mailpit has no IMAP, only POP3, so it cannot be the
  `imap.mail` receiver; use Dovecot, GreenMail or any server with `IDLE`,
  `UIDPLUS` and mailbox delete and create. The account must be one the tests
  may create and delete mailboxes in. The state tests also need `MOVE`,
  `UIDPLUS` and `SPECIAL-USE` and mailboxes with the `\Archive` and `\Trash`
  attributes named `Archive` and `Trash`; without those capabilities they skip.
  They move test mail into those mailboxes and remove it again.
- **A sending MTA.** Only Postfix from `up.sh` is supported: the retry test
  sets its transport with `postconf`.

Never point the variables at a mailbox or relay that carries real mail: the
receiver tests read whole mailboxes and the submission tests send.

## What the live tests cover

Each test works in a mailbox or a Message-ID space of its own, so runs and
tests never see each other's mail. They live in `adapter/mail/imap/live_test.go`
and `adapter/mail/smtp/live_test.go`, on the helpers in
`adapter/mail/internal/livemail`.

| Test | Adapter | What it drives |
| --- | --- | --- |
| `TestLiveReceive` | `imap.mail` | mail appended by another client, over implicit TLS and STARTTLS; event identity equals the SHA-256 of the bytes as appended; no flag but `\Recent` set afterwards |
| `TestLiveIdleDelivers` | `imap.mail` | a message appended during `IDLE` arrives in under ten seconds with a 60-second restart |
| `TestLiveCursorResumes` | `imap.mail` | a second receiver with the stored cursor delivers only new mail; one without it delivers all again under the same identities |
| `TestLiveUIDValidityChange` | `imap.mail` | the mailbox deleted and created again (a new `UIDVALIDITY`), a copy and a new message: one gap observed, the copy keeps its identity, the new mail is delivered |
| `TestLiveOversizeIsHeaderOnly` | `imap.mail` | a message over `max_message_bytes` arrives from its header block, identified by that block and the size the server reports |
| `TestLiveDedupThroughTheEngine` | `imap.mail` | identical bytes at two UIDs, forged `Message-ID`, and the whole mailbox delivered again after a new `UIDVALIDITY`, through the engine's store |
| `TestLiveSendSubmits` | `imap.mail` | reply over STARTTLS and AUTH to the catcher: authenticated user, `Message-ID`, threading, stable across attempts, appended sent copy with `\Seen`, `Reconcile` found and unknown |
| `TestLiveSendRefusesWrongPassword` | both | the submission server refuses a wrong password; nothing is caught or filed |
| `TestLiveEngineRoundTrip` | `imap.mail` | mail in, stored under the sender's identity, reply out and threaded on it |
| `TestLiveStateVerbs` | `imap.mail` | through the engine: read, star, `MOVE` into a mailbox by name with the follow-up reaching the message by the `COPYUID` location, archive and trash by their special-use mailboxes, each undone, and flags kept across moves |
| `TestLiveStateFollowsTheMailbox` | `imap.mail` | another client's flag change (`CONDSTORE` `CHANGEDSINCE`), a move into `Archive` and an expunge: the first two reach the conversation's provider state, the expunge is told from the move and only observed |
| `TestLiveReceiveSurface` | `smtp.mail` | EHLO offers STARTTLS only when configured and never AUTH; `AUTH` is refused; mail is taken with and without TLS |
| `TestLiveReceiveLimits` | `smtp.mail` | 552 for an oversize message and for a declared size, 452 beyond `max_recipients` |
| `TestLiveTemporaryFailureIs4xx` | `smtp.mail` | 451 when the sink fails; the retry of the same bytes keeps its event identity |
| `TestLiveFanOutAndDedup` | `smtp.mail` | one `DATA` to two destinations lands once in each; 550 5.1.1 for an unknown recipient; the same bytes again add nothing; a forged `Message-ID` adds a message |
| `TestLiveSendThroughSubmission` | `smtp.mail` | reply over STARTTLS and AUTH to the catcher: authenticated user, `Message-ID`, threading, stable across attempts |
| `TestLiveMTARetriesA4xx` | `smtp.mail` | Postfix over STARTTLS: 451 is deferred and retried, the retry delivers once per destination in one `DATA`, 550 is bounced |
| `TestLiveMTADelivery` | `smtp.mail` | two Postfix submissions sharing one `Message-ID` are two messages per destination through the engine |

The clients of the receiver are Go's `net/smtp`, a different implementation
from the `go-smtp` the receiver is built on, and Postfix. The IMAP account is
read back by a second `go-imap` session, which is the same library as the
adapter's; the server is Dovecot.

## History has no recording either

`imap.mail` declares `history`, and its reader has no record path, as the rest
of the adapter has none: nothing is recorded, replayed or scrubbed. Its tests
run against the in-process IMAP server built on the library the adapter uses,
including the cursor, the window by `INTERNALDATE`, the exact sender check
after `SEARCH FROM`, and a `UIDVALIDITY` change between two calls where that
server can force one.

No live history test exists yet. When one is written it belongs in the opt-in
`TestLive*` run above, against the Dovecot container, recreating the mailbox to
force a `UIDVALIDITY` change; until then the reader is unverified against a
real server, and the [parity evidence](#parity-evidence) says nothing about it.

## Parity evidence

Cassettes cannot be recorded for these two adapters, so a green run of the
live tests is the evidence in their place. It proves the following about the
commit that ran, against the server versions recorded above.

**It proves**, with real servers and real clients on the wire:

- `imap.mail` retrieves over implicit TLS and over STARTTLS, authenticates
  with `PLAIN` after the certificate was verified, waits in `IDLE` and is
  woken by new mail, and reads without changing a flag.
- The cursor survives a restart; a changed `UIDVALIDITY`, forced by deleting
  and creating the mailbox, is reported once and resynchronized without
  storing anything twice; an oversize message arrives from its header block
  under an identity built from the size the server reports.
- A message is one message by its bytes, on both adapters: the same bytes
  by two routes or two attempts are one, a forged `Message-ID` is another
  message and hides nothing. For `smtp.mail` this includes a real MTA's
  retry after a 4xx.
- `smtp.mail` as a receiver behaves as its page says to a client: STARTTLS
  when configured, no AUTH, size and recipient limits with codes that real
  clients and Postfix act on (retry on 4xx, bounce on 5xx), one `DATA` for
  several recipients.
- `imap.mail` changes the account's mailbox only through its state verbs, on a
  read-write session of their own: `\Seen` and `\Flagged`, `MOVE` with the
  `COPYUID` re-resolved into the next verb's reference, archive and trash by
  special-use mailbox. It follows what another client does: flag changes by
  `CONDSTORE`, and a message that left the mailbox is classified as moved or
  expunged by looking it up in the other mailboxes.
- Both adapters submit through a real server over STARTTLS with AUTH, under
  a stable `Message-ID` and with threading headers, and refuse a wrong
  password; `imap.mail` files a sent copy and finds it again in `Reconcile`.

**It does not prove**:

- The state verbs against a real provider's mailbox model: Gmail's folder view
  of labels (`\All` as archive), Microsoft 365's and Fastmail's special-use
  mailboxes and their `MOVE` behavior, a server without `MOVE` (the `COPY`,
  `\Deleted` and `UID EXPUNGE` path runs against the in-process server only),
  `QRESYNC`, or a server that refuses a write.

- Behavior of any real provider. Dovecot, Postfix and Mailpit are
  conforming servers, not Gmail, Microsoft 365, Fastmail, Exchange or Cyrus:
  their `IDLE` timeouts, rate limits, `UID` reuse, label or folder rules and
  authentication policies are unverified. OAuth 2 (`XOAUTH2`, `OAUTHBEARER`),
  Kerberos and NTLM are not exercised here: OAuth 2 is covered by in-process
  servers, Kerberos by the `adapter/mail/auth` container test against Dovecot
  and a KDC, NTLM by recorded exchanges. None of them ran against an identity
  provider or a domain controller.
- The internet-facing receiver. SPF, DKIM and DMARC evidence uses a stub
  resolver: no real DNS, no real sender's signatures, no port 25 exposure,
  reverse DNS, greylisting or backup MX.
- Outbound deliverability. The catcher keeps what it is given; nothing is
  delivered to a real mailbox, so spam scoring, SPF alignment of the sending
  relay, DKIM signing at the relay and bounces from remote servers are
  unverified.
- Scale and time: no soak, no concurrent receivers, no IDLE session of the
  25 to 29 minutes the adapter restarts at, no server that drops the
  connection on its own schedule.
- That a run is reproducible to the byte: the Debian packages are not
  pinned, and a green run is a result, not an artifact that replays.

**What counts as parity evidence for retiring an email relay.** A green run
of every live test on the commit being relied on, with the output and the
image versions kept with the retirement record, covers the protocol rows of
a parity checklist for `imap.mail` and `smtp.mail`. It does not cover the
rows that depend on the operator's own mail path: the real MX records and
senders, the real relay or provider account, and its deliverability. Those
need one run on the operator's own mailbox and relay (the variables above
accept them) or a shadow period beside the old relay, with the mail that
differs listed. The live run supports retiring a relay; it does not by
itself change an adapter's maturity from `experimental`.
