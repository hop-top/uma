// Package webhookauth authenticates inbound webhook requests for adapters
// that take HTTP deliveries (capability receive_http) from a sender that is
// not a provider with its own signature format: a generic message relay, a
// mail ingress, an in-house service. Provider adapters keep verifying their
// provider's own signature; this package is for senders whose convention is
// configuration.
//
// # Schemes
//
// A Config names one scheme, never read from the request:
//
//   - SchemeBearer: RFC 6750 bearer token in Authorization (or another
//     header), equal to the shared secret.
//   - SchemeToken: the shared secret as the whole value of a header.
//   - SchemeHMAC: HMAC (RFC 2104) with SHA-256 or SHA-512 over a signed
//     payload.
//   - SchemeEd25519: Ed25519 (RFC 8032) over a signed payload; the secrets
//     are public keys.
//
// Header names, signature encodings (hex, base64, base64url, padded or not),
// an optional prefix such as "sha256=", the timestamp format and the layout
// of the signed payload ("{timestamp}.{body}", "v0:{timestamp}:{body}",
// "{timestamp}{body}") are all configuration, so a sender already in the
// field keeps working unchanged. Defaults are neutral.
//
// A sender that rotates its own secret may send several signatures in one
// header ("v1=a,v1=b", "v1,a v1,b"); SignatureList opts in to accepting the
// request when any entry verifies, with the same strict decoding per entry,
// a cap on the list and a comparison of every entry against every secret.
//
// # Freshness and replay
//
// Timestamp checks the distance from the clock in both directions.
// Replay rejects a delivery seen before, identified by its decoded
// signature or by a signed id header; with a signature list, every entry that
// verified is part of the identity. Both refuse to be configured loosely
// by accident: a timestamp or id the signature does not cover needs an
// explicit AllowUnsigned, because anyone holding a captured request can
// change it. A delivery is recorded only after everything else verified,
// and Result.Release frees it when the host asks the sender to retry.
//
// # What the host injects
//
// Secrets come through SecretSource, normally FromCredentials over
// ports.Credentials, resolved on every request and never kept; Config holds
// no secret. The replay record is a ReplayStore the host supplies:
// MemoryReplayStore is bounded and process-local, so it forgets on restart
// and is not shared across replicas; use a durable shared store when a
// replay inside the timestamp window after a restart matters. The clock is
// a ports.Clock.
//
// # Usage
//
//	v, err := webhookauth.New(cfg, webhookauth.FromCredentials(creds, req), webhookauth.Deps{
//		Clock: clock, Replay: store, Scope: channelID,
//	})
//	...
//	body, res, err := v.VerifyHTTP(r)
//	if err != nil {
//		v.Reject(w, err) // 401, 400, 409, 413 or 503; no body
//		return
//	}
//	if err := hand(body); err != nil {
//		_ = res.Release(r.Context()) // let the sender's retry through
//		http.Error(w, "", http.StatusServiceUnavailable)
//		return
//	}
//
// Verify takes the raw bytes instead of an *http.Request. Nothing here
// reads the environment, keeps global state or registers at init, and the
// package imports the standard library and the public uma contract
// packages only.
package webhookauth
