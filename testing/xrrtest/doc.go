// Package xrrtest records and replays provider HTTP exchanges for adapter
// tests with hop.top/xrr.
//
// NewClient returns an *http.Client whose transport, depending on the mode,
// sends each request to the real provider and writes the exchange to a
// cassette directory (record), answers it from that directory without
// touching the network (replay), or sends it without reading or writing
// cassettes (passthrough). The mode comes from the XRR_MODE environment
// variable and defaults to replay, so a plain `go test` never reaches a
// provider.
//
// Before anything reaches disk the transport:
//
//   - keeps only allow-listed request and response headers (Content-Type,
//     Location, Retry-After, X-RateLimit-* and any WithHeaders additions)
//     and always drops credential headers such as Authorization;
//   - normalizes the request: sorted query, dropped volatile parameters,
//     redacted credential parameters, canonical form bodies, caller
//     Normalize hooks, and placeholders for account or owner identifiers;
//   - scrubs secrets from JSON and form bodies field by field, then runs
//     caller Scrub hooks.
//
// The cassette fingerprint covers the method, host, path, query and body of
// the normalized request, plus an occurrence number for repeated identical
// requests on one transport. See README.md for cassette layout and the
// re-record procedure.
package xrrtest
