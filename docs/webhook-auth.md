# Webhook authentication

`hop.top/uma/adapter/web/webhookauth` authenticates inbound deliveries for an
adapter that takes HTTP webhooks from a sender with no provider signature
format of its own: a generic message relay, a mail HTTP ingress, an in-house
service. Provider adapters keep checking their provider's signature. This
package is for senders whose header names, encodings and signed layout are a
matter of configuration, and it is part of the root module: standard library
and uma's public contract packages only.

It is `experimental` like every package under `adapter/`; see
[Compatibility](compatibility.md#what-is-not-frozen).

## Verify a request

```go
v, err := webhookauth.New(
	webhookauth.Config{
		Scheme:    webhookauth.SchemeHMAC,
		Header:    "X-Signature",
		Prefix:    "sha256=",
		Timestamp: webhookauth.Timestamp{Header: "X-Timestamp"},
		Replay:    webhookauth.Replay{Source: webhookauth.ReplayBySignature},
	},
	webhookauth.FromCredentials(creds, ports.CredentialRequest{
		Channel: ch, Reference: ref, Purpose: "relay.webhook",
	}),
	webhookauth.Deps{
		Clock: clock, Replay: store,
		Scope: string(ch.Tenant) + "/" + string(ch.ID),
	},
)

body, res, err := v.VerifyHTTP(r)
if err != nil {
	v.Reject(w, err)
	return
}
if err := accept(body); err != nil {
	_ = res.Release(r.Context()) // the sender retries; do not call that a replay
	w.WriteHeader(http.StatusServiceUnavailable)
	return
}
```

`VerifyHTTP` reads the body once, bounded by `Config.MaxBodyBytes` (1 MiB), and
returns it as received. Signatures cover the wire bytes: parse what it returns,
never re-encode a parsed body to check it. `Verify(ctx, Request{Header, Body})`
takes the bytes directly.

## Schemes

| Scheme | The request carries | Secrets | Default header |
| --- | --- | --- | --- |
| `bearer` | `Bearer <token>` (RFC 6750); scheme word case-insensitive | shared token | `Authorization` |
| `token` | the shared token as the whole header value | shared token | `X-Webhook-Token` |
| `hmac` | HMAC-SHA-256 or -SHA-512 (RFC 2104) of the signed payload | shared key | `X-Webhook-Signature` |
| `ed25519` | Ed25519 (RFC 8032) signature of the signed payload | 32-byte public keys | `X-Webhook-Signature` |

The scheme is configuration, never read from the request, so a request cannot
pick a weaker one. There is no "none": a sender that needs no authentication
gets no `Verifier`.

## Configuration

| Field | Meaning | Default |
| --- | --- | --- |
| `Header` | Header carrying the credential or signature; matched ignoring case | by scheme |
| `Hash` | `sha256` or `sha512` (`hmac` only) | `sha256` |
| `Encoding` | Signature encoding: `hex`, `base64`, `base64url`, `base64raw`, `base64urlraw`. No fallback between them | `hex` |
| `Prefix`, `PrefixOptional` | Text the signature value starts with (`sha256=`); optional lets a value come without it | none |
| `SignatureList.Separator`, `Max` | Opt in to several signatures in one header: `,` or one space between entries, at most `Max` (1 to 32); see [Several signatures in one header](#several-signatures-in-one-header) | off (one signature); 8 |
| `Payload` | Signed bytes: literals plus `{body}`, `{timestamp}`, `{id}`; `{{` and `}}` are braces | `{body}`, or `{timestamp}.{body}` with a timestamp header |
| `KeyEncoding` | How the host stored the secret: `raw`, or a signature encoding | `raw` |
| `MinSecretBytes` | Shortest decoded secret; Ed25519 keys are exactly 32 | 16 |
| `MaxBodyBytes` | Body limit for `VerifyHTTP` | 1 MiB |
| `Timestamp.Header`, `Format`, `Tolerance` | Freshness check, both directions, inclusive. Formats: `unix`, `unix_ms`, `rfc3339`, `unix_or_rfc3339` | off; `unix`; 5 minutes (at most 24 hours) |
| `Replay.Source`, `Header`, `TTL` | Replay rejection by decoded `signature` or by an id `header` | off; 24 hours, or two tolerances with a timestamp |
| `AllowUnsigned` (on `Timestamp` and `Replay`) | Accept a timestamp or id the signature does not cover | refused |

New refuses a configuration that could accept an unauthenticated request:
no scheme, a timestamp or replay id outside the signature without
`AllowUnsigned`, a replay source without a store, a payload with no `{body}`.

## Senders already in the field

A migrating sender keeps its header names, encodings and layout; set them.

| Sender convention | Configuration |
| --- | --- |
| Token in `X-Relay-Token` | `Scheme: token, Header: "X-Relay-Token"` |
| `X-Relay-Signature: sha256=<hex>` over the body, prefix sometimes absent | `Scheme: hmac, Header: "X-Relay-Signature", Prefix: "sha256=", PrefixOptional: true` |
| A timestamp header the signature does not cover, RFC 3339 or Unix seconds | `Payload: "{body}", Timestamp: {Header: "X-Relay-Timestamp", Format: unix_or_rfc3339, AllowUnsigned: true}` |
| Ed25519 over the timestamp header immediately followed by the body, hex key and signature | `Scheme: ed25519, Payload: "{timestamp}{body}", KeyEncoding: hex, Timestamp: {Header: ..., Format: unix_or_rfc3339}` |
| Short shared secrets | `MinSecretBytes: n`, deliberately; an empty secret is never accepted |

Cover the timestamp and id with the signature when the sender can be changed:
an unsigned timestamp only stops honest stale deliveries, because a captured
request can be sent again with a fresh one.

## Secrets and rotation

`Config` holds no secret. `FromCredentials` asks the host's `ports.Credentials`
on every request (a `ports.Secret`, or a `ports.Password`'s secret) and keeps
nothing, so a rotated credential applies at once. Pass the current and the
previous reference to overlap a rotation: any listed key verifies, and all of
them are always compared. A missing, empty, too short or undecodable secret
refuses the request (`ErrSecret`), whatever it carries.

## Several signatures in one header

A sender that rotates its own secret signs each delivery under both the old
and the new one and sends both signatures in one header: comma-delimited
`v1=<a>,v1=<b>`, or the space-delimited `v1,<a> v1,<b>` of the
[Standard Webhooks](https://github.com/standard-webhooks/standard-webhooks)
and Svix convention. Secrets rotated by the receiver already work (see
[Secrets and rotation](#secrets-and-rotation)); this is the sender's side of
it, and it is opt in. With `SignatureList` unset the header is one signature,
exactly as before.

```go
webhookauth.Config{
	Scheme: webhookauth.SchemeHMAC,
	Header: "X-Signature",
	Prefix: "v1=",
	SignatureList: webhookauth.SignatureList{
		Separator: webhookauth.SeparatorComma, // or SeparatorSpace
		Max:       4,                          // default 8, at most 32
	},
}
```

The request verifies when at least one entry does.

| Rule | Detail |
| --- | --- |
| Entries | `Separator` splits the value exactly: `,` or one space. Nothing is trimmed, so `a, b` has the entry ` b`, which does not decode. The transport's outer whitespace is ignored as for any header |
| Same shape for every entry | Each entry takes the configured `Prefix`, `PrefixOptional`, `Encoding` and decoded size, as a lone signature does. There is no auto-detection and no normalization |
| Malformed refuses the whole header | An entry that is empty, lacks the prefix, is not canonical (wrong alphabet, padding or trailing bits, a line break), has the wrong size, or lies beyond `Max` makes the request `ErrMalformed`, whether or not another entry would verify. Nothing is skipped or ignored, so a request cannot park a bad entry behind a good one |
| Cap | Entries are counted by separator before any is decoded or compared, so an over-long header costs nothing. Each entry costs one comparison per secret |
| Compare all | Every entry is compared against every secret, in constant time, without stopping at a match, whichever entry matches and wherever it sits |
| Duplicates | Harmless: a repeated entry counts toward `Max` and is one signature |
| One header | The list is one header line. A header repeated on the request is still `ErrMalformed` |
| Timestamp | Checked once, for the request, after an entry verified. Every entry covers the same signed timestamp, so a stale request is stale whichever entry matched, and a stale or forged request reserves nothing |

An element that is not a signature of the configured shape is not discarded.
A header such as `t=<timestamp>,v1=<signature>,v0=<signature>` therefore does
not parse (`t=` and `v0=` entries lack the `v1=` prefix): its embedded
timestamp is not read either, since the timestamp comes from its own header
(`Timestamp`). Senders that put only signatures in the list, as Standard
Webhooks does, work; so does `v1=<a>,v1=<b>`. A host that must read a header
with embedded elements needs them extracted before the verifier.

### Replay with a list

`ReplayBySignature` identifies a delivery by the decoded signatures that
verified. The list is not signed, so anyone holding a captured request can
drop entries and resend it. To keep that from slipping past, every distinct
signature that verified is reserved, in sorted key order, and the delivery is
a replay (`ErrReplay`, 409) when any of them was accepted before:

- `[old, new]` accepted, then `[new]`, `[old]`, `[new, old]` or either with a
  forged entry added: all replays.
- An entry that does not verify is never reserved, so a forged entry cannot use
  up a delivery's slot, and a duplicate is one reservation.
- `Result.Release` frees every key the delivery reserved. If the store fails
  while reserving, the keys already taken are released and the request is
  `ErrUnavailable`, so the sender's retry is not mistaken for a replay.
- A replay found midway leaves the keys reserved before it in place: they
  belong to a signature that verified for this payload.
- Identity stays the signature, as it already was with two configured
  secrets: the same message accepted with only the old entry, then sent again
  with only the new entry, is a new delivery. For identity of the message,
  sign a timestamp or an id into the payload and use `ReplayByHeader`, which
  reserves one key whatever the list holds.

Reserving `n` signatures is `n` store calls, so it costs more than a single
signature on a remote store.

## Replay and restarts

A delivery is recorded only after its credential and window verified, so a
forged request cannot use up an id. With a [signature list](#replay-with-a-list)
every verifying signature is recorded. The key is a digest of `Deps.Scope`, the
source and the identity; the signature is identified by its decoded bytes, so
re-encoding a captured request does not dodge it. Entries live at least as
long as the request would still pass the window: two tolerances, since a
request stamped at the future edge stays fresh that long.

`ReplayStore` (`Reserve` atomic, `Release`) is the host's. Two stores come with
the package, and a host can supply its own.

| Store | Survives a restart | Shared by replicas | Use |
| --- | --- | --- | --- |
| `webhookauth.MemoryReplayStore` | no | no | The default; tests; one process that accepts the window reopening on restart |
| `storereplay.Store` | with a durable `store.Store` | with a store the replicas share | Hosts that already run the PostgreSQL store |

`MemoryReplayStore` is bounded, expires entries by time and, when full of live
entries, refuses (`ErrReplayStoreFull`, 503) instead of evicting one. It is
process-local: after a restart, a request captured inside the timestamp window
can be accepted again, and each replica keeps its own memory.

### Durable store

`hop.top/uma/adapter/web/webhookauth/storereplay` keeps the same state in the
root `store.Store` port, so any backend that implements it works and the
PostgreSQL store makes it restart-safe and shared with no further service:

```go
replay, err := storereplay.New(storereplay.Options{
	Store:   st,    // the host's store.Store; postgres.Open(...) for durability
	Channel: ch,    // a channel the store holds; it carries the state
	Clock:   clock, // the clock the verifier uses
})
v, err := webhookauth.New(cfg, secrets, webhookauth.Deps{
	Clock: clock, Replay: replay, Scope: string(ch.Tenant) + "/" + string(ch.ID),
})
```

The state lives in adapter-owned protocol records of `Channel`
(`store.Tx.PutProtocol`), spread over `Buckets` records (default 256) by a
digest of the key. A record holds the truncated digest of each delivery's key
and its expiry, nothing else: no signature, id, scope, secret or body. One
`Channel` can serve every receiver of a tenant, since keys already include
`Deps.Scope`; keep that channel stored as long as replay protection is needed.

| Property | How |
| --- | --- |
| Atomic `Reserve` | A record changes by compare-and-set on its revision inside one unit. Of concurrent reservations of one key, on one process or on replicas sharing the database, one commits as fresh. A reservation that loses a race reads the winner's record and tries again (up to 16 times) |
| Expiry | By `Options.Clock` against the stored expiry; an entry expires at its TTL exactly. Replicas must agree on the time to well within the TTL, which is at least two tolerances |
| Bounded growth | At most `Buckets` records. Every write drops its record's expired entries, and a record holds at most `BucketCapacity` live entries (default 256); a full record refuses with `ErrReplayStoreFull` (503) rather than forget a live delivery. Size `Buckets` × `BucketCapacity` above deliveries per TTL; a write rewrites its whole record, so raise `Buckets`, not the capacity |
| Fails closed | A store error, a missing `Channel`, unreadable state and contention that does not settle all return an error: the verifier answers 503 and records nothing |
| `Release` | Forgets the key, so the sender's retry is accepted |

Keep `Buckets` fixed for the life of the data: records are named by it, so
changing it leaves the old entries unreachable and reopens their windows. With
the PostgreSQL store each `Reserve` is one serializable transaction on one row;
a busy receiver with a large TTL (the 24-hour default when there is no
timestamp window) wants more buckets. If a commit's outcome is unknown, the
call fails and the sender's retry may then find its own delivery recorded: a
409 for a delivery the host never accepted, the safe side of that failure.

Two genuine messages with identical bodies and no signed timestamp or id are
one delivery under `ReplayBySignature`.

## Errors

Every error wraps one sentinel and carries no request content.

| Sentinel | Cause | HTTP status |
| --- | --- | --- |
| `ErrMissing` | No credential, or an empty one | 401 |
| `ErrInvalid` | Credential or signature does not match | 401 |
| `ErrStale` | Timestamp outside the tolerance | 401 |
| `ErrMalformed` | Repeated header, undecodable value, wrong signature size, bad timestamp | 400 |
| `ErrReplay` | Delivery already accepted | 409 |
| `ErrTooLarge` | Body over the limit | 413 |
| `ErrSecret`, `ErrUnavailable` | No usable secret; credential port or replay store failed | 503 |

`Status` maps an error and `Reject` answers with it, adding the RFC 6750
`WWW-Authenticate` challenge for bearer.

## Standards notes

- Comparisons are constant time and hide length: tokens are compared as SHA-256
  digests, MACs at their fixed size, every key every time.
- The bearer scheme word is case-insensitive and may be followed by several
  spaces (RFC 9110). The token is not restricted to `token68` characters, only
  to one run of non-space, non-control characters, so existing senders keep
  working.
- A repeated header is refused: a proxy and this verifier could otherwise read
  different values.
- Ed25519 verification is the RFC 8032 check as implemented by Go's
  `crypto/ed25519`: signatures with `S` at or above the group order are
  refused. HMAC secrets under 16 bytes are refused by default (NIST SP 800-107
  asks for 112 bits; RFC 2104 discourages keys shorter than the hash output).
- Query-string and form-body bearer tokens (RFC 6750 2.2, 2.3) are not
  supported: they leak into logs.

## Tests

The package carries the RFC 4231 HMAC known answers, the RFC 8032 Ed25519
vectors, skew and replay tables, and constant-time and fail-closed cases. The
signature list adds Svix's published worked example (secret, id, timestamp,
payload and signature as printed in its documentation, recomputed
independently), and tables of constructed vectors for comma and space
delimiters, mixed, duplicate, empty, over-cap and non-canonical entries, the
timestamp window and replay. The comma-delimited cases are constructed, and
the one mixed-element header only exercises the parser:

```sh
GOWORK=off go test -race ./adapter/web/webhookauth/...
```

`storereplay` adds restart, concurrent-reserve, expiry, purge, scope and
fail-closed cases over the memory store. The same behaviours run against a
real PostgreSQL server in `store/postgres` (`TestReplayStore`, which starts
and removes its own container and skips without a container runtime; set
`UMA_REPLAY_TEST_IMAGE` to use an image already present).
