# xrrtest

Record and replay provider HTTP exchanges in adapter tests, on
[hop.top/xrr](https://pkg.go.dev/hop.top/xrr) cassettes.

```go
import "hop.top/uma/testing/xrrtest"

func TestSendMessage(t *testing.T) {
	sid := os.Getenv("TWILIO_ACCOUNT_SID") // empty in replay
	if sid == "" {
		sid = "ACtest"
	}
	client := xrrtest.NewClient(t, "testdata/cassettes/twilio",
		xrrtest.WithPlaceholder(sid, "ACCOUNT_SID"),
	)
	// Hand client to the adapter under test.
}
```

`hop.top/xrr` is pinned to exactly `v0.1.0-alpha.5`. Its HTTP fingerprint
changes after that release, so bumping the pin means re-recording every
cassette.

## Modes

`XRR_MODE` selects the mode; `WithMode` overrides it.

| `XRR_MODE` | Network | Cassettes |
| --- | --- | --- |
| unset, `replay` | never | read; a missing cassette fails the test with the fingerprint and file it looked for |
| `record` | yes | written (directory created), existing files overwritten |
| `passthrough` | yes | untouched |

## Conventions

- One cassette directory per package and provider, committed with the tests:
  `testdata/cassettes/<provider>`. Go tooling ignores `testdata`.
- Record against a sandbox account. Pass sandbox account, owner or tenant
  identifiers through `WithPlaceholder` so they never reach a cassette; in
  replay pass any fake value for the same placeholder. Responses carry the
  value you passed back to the code under test.
- Responses handed back are the recorded form in every mode: filtered
  headers, placeholders restored, secrets scrubbed. Record and replay runs
  therefore assert against the same data.
- Repeated identical requests on one client are numbered: the second and
  later get their own cassette, replayed in the same order. Numbering
  restarts per client, so create one client per subtest. Concurrent
  identical requests get numbers in arrival order, which replay cannot
  reproduce: keep them on separate clients or make them sequential.
- Replay never reaches the network, so tests must not assert on
  OS-allocated state (ports, PIDs) or wall-clock time from the provider.

## What is recorded

**Fingerprint.** Method, host, path, query and body of the normalized
request, plus the occurrence number. xrr's own HTTP fingerprint ignores the
host; xrrtest folds it into the path. To replay against a server whose host
changes (a local test server), add `WithPlaceholder(host, "PROVIDER_HOST")`.

**Headers.** Request and response headers pass an allow-list:
`Content-Type`, `Location`, `Retry-After`, `X-RateLimit-*`. Extend it with
`WithHeaders` (a trailing `*` is a prefix match). `Authorization`,
`Proxy-Authorization`, `Cookie`, `Set-Cookie` and any header whose name
reads as a credential (`X-Api-Key`, `X-Auth-Token`, ...) are dropped even
when listed. Repeated values are joined with `, `.

**Normalization** (recorded copy only; the provider sees the original
request), in order:

1. query sorted by key; `WithDropQuery` parameters removed; values of
   credential-named parameters (`access_token`, `api_key`, ...) redacted;
2. `application/x-www-form-urlencoded` bodies sorted by key;
3. `WithNormalize` hooks;
4. `WithPlaceholder` substitutions on the URL and body.

**Scrubbing.** `ScrubSecrets` redacts JSON and form fields one at a time,
using xrr's classification: credential-like field names and known token
shapes (GitHub, Slack, Stripe, AWS, JWT, bearer) become
`<redacted:FIELD>`. A scrubbed JSON body is re-encoded compact with sorted
keys. `WithScrub` hooks run after it, for provider-specific secrets and for
other content types. xrr's cassette redactor still runs at write time as a
backstop, but it replaces a whole body when it finds a token shape anywhere
in it, so scrub field by field before that happens.

Request bodies are scrubbed before fingerprinting, so replay with a fake
secret matches a recording made with a real one.

**Errors.** A transport error during recording is saved as its message.
Replay returns `errors.New(message)`: the message survives, the error type
and `errors.Is` chain do not.

## Re-recording

[Record adapter cassettes](../../docs/recording/README.md) has one guide
per provider: how to get sandbox credentials, the environment variables each
recorded test reads, and the checks to run before committing.

From the module or package directory, with sandbox credentials in the
environment the test reads:

```sh
XRR_MODE=record go test ./... -run TestSendMessage
git diff testdata/cassettes   # review: no secrets, no real identifiers
go test ./... -run TestSendMessage   # replay
```

Delete a provider's cassette directory first when requests were removed, so
stale files do not linger. `XRR_REDACT_ALLOW`, `XRR_REDACT_DENY` and
`XRR_REDACT_DISABLE` tune xrr's body and query classification; they never
re-enable credential headers.
