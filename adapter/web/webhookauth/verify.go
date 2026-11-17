package webhookauth

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"hop.top/uma/ports"
)

// Deps are the host-owned pieces a Verifier needs.
type Deps struct {
	// Clock is the time source. Nil reads the system clock.
	Clock ports.Clock

	// Replay remembers accepted deliveries. Required when Config.Replay is
	// on, ignored otherwise; see ReplayStore for what the host must provide.
	Replay ReplayStore

	// Scope namespaces replay keys, so channels sharing one store never
	// collide. Use the channel's identity.
	Scope string
}

// Request is what Verify checks. Body must be the bytes as received:
// signatures cover the wire bytes, so a body parsed and re-encoded never
// verifies.
type Request struct {
	Header http.Header
	Body   []byte
}

// Result describes an accepted request.
type Result struct {
	Scheme Scheme
	// Timestamp is the request's timestamp; zero when no timestamp header
	// is configured.
	Timestamp time.Time

	sigs    [][]byte // distinct decoded signatures that verified, the replay identity for ReplayBySignature
	store   ReplayStore
	keys    []string // store keys this delivery reserved
	release sync.Once
}

// Release forgets the delivery's replay reservation. Call it when the host
// accepted the request but could not process it and answers with a retryable
// status: the sender's retry is then a new attempt, not a replay. It is
// safe to call more than once, on a nil Result and on one that reserved
// nothing.
func (r *Result) Release(ctx context.Context) error {
	if r == nil || r.store == nil {
		return nil
	}
	var errs []error
	r.release.Do(func() {
		for _, k := range r.keys {
			errs = append(errs, r.store.Release(ctx, k))
		}
	})
	return errors.Join(errs...)
}

// Verifier checks requests under one Config. It is immutable after New and
// safe for concurrent use.
type Verifier struct {
	plan    plan
	secrets SecretSource
	deps    Deps
}

// New validates cfg and returns a Verifier that reads key material from
// secrets on every request. It refuses a configuration that could accept an
// unauthenticated request.
func New(cfg Config, secrets SecretSource, deps Deps) (*Verifier, error) {
	p, err := cfg.validate()
	if err != nil {
		return nil, err
	}
	if secrets == nil {
		return nil, cfgErr("no secret source")
	}
	if p.Replay.Source != "" && deps.Replay == nil {
		return nil, cfgErr("replay rejection needs a replay store")
	}
	return &Verifier{plan: p, secrets: secrets, deps: deps}, nil
}

// Verify checks the request against the configured scheme. It parses and
// checks the shape of every header first, resolves the secrets only after
// that, compares against every secret without stopping at the first match,
// and checks the timestamp window only for a request whose credential
// verified. It returns an error wrapping one of the package's sentinels
// unless the request is authentic and fresh.
func (v *Verifier) Verify(ctx context.Context, req Request) (*Result, error) {
	var in inputs
	var err error
	if v.plan.tsHdr != "" {
		if in.ts, err = v.readStamp(req.Header); err != nil {
			return nil, err
		}
	}
	if v.plan.rpHdr != "" {
		if in.id, err = v.readID(req.Header); err != nil {
			return nil, err
		}
	}
	var res *Result
	switch v.plan.Scheme {
	case SchemeBearer, SchemeToken:
		res, err = v.verifySecret(ctx, req)
	case SchemeHMAC, SchemeEd25519:
		res, err = v.verifySignature(ctx, req, in)
	default:
		return nil, fmt.Errorf("%w: scheme %q", ErrConfig, v.plan.Scheme)
	}
	if err != nil {
		return nil, err
	}
	if v.plan.tsHdr != "" {
		if d := v.now().Sub(in.ts.at); d > v.plan.tol || d < -v.plan.tol {
			return nil, fmt.Errorf("%w: %s", ErrStale, v.plan.tsHdr)
		}
		res.Timestamp = in.ts.at
	}
	if err := v.reserve(ctx, res, in); err != nil {
		return nil, err
	}
	return res, nil
}

// inputs are the per-request values read before the credential is checked.
type inputs struct {
	ts stamp
	id string
}

// maxIDLen bounds a replay id header.
const maxIDLen = 256

func (v *Verifier) readID(h http.Header) (string, error) {
	id, err := headerValue(h, v.plan.rpHdr)
	if err != nil {
		return "", err
	}
	if len(id) > maxIDLen {
		return "", fmt.Errorf("%w: %s longer than %d bytes", ErrMalformed, v.plan.rpHdr, maxIDLen)
	}
	for i := 0; i < len(id); i++ {
		if id[i] < ' ' || id[i] == 0x7f {
			return "", fmt.Errorf("%w: %s has control characters", ErrMalformed, v.plan.rpHdr)
		}
	}
	return id, nil
}

// reserve records the delivery once everything else verified. The key is a
// digest of the scope, the source and the identity, so it is fixed-size and
// reveals neither a signature nor an id.
//
// With a signature list, more than one signature can verify; each is an
// identity of the delivery, and the delivery is fresh only when none was
// accepted before. The list is not signed, so a replayed request could
// otherwise drop the entry that was recorded and keep another. Keys are
// reserved in sorted order, so two concurrent copies of one delivery
// contend on the same key first and one of them loses.
func (v *Verifier) reserve(ctx context.Context, res *Result, in inputs) error {
	var idents [][]byte
	switch v.plan.Replay.Source {
	case "":
		return nil
	case ReplayBySignature:
		idents = res.sigs
	case ReplayByHeader:
		idents = [][]byte{[]byte(in.id)}
	}
	res.sigs = nil
	keys := make([]string, len(idents))
	for i, ident := range idents {
		keys[i] = v.replayKey(ident)
	}
	slices.Sort(keys)
	var held []string
	for _, key := range keys {
		fresh, err := v.deps.Replay.Reserve(ctx, key, v.plan.rpTTL)
		if err != nil {
			// The sender retries on 503: its retry must not meet the
			// keys this attempt took.
			v.forget(ctx, held)
			return fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		if !fresh {
			return fmt.Errorf("%w: duplicate delivery", ErrReplay)
		}
		held = append(held, key)
	}
	res.store, res.keys = v.deps.Replay, held
	return nil
}

func (v *Verifier) forget(ctx context.Context, keys []string) {
	for _, k := range keys {
		_ = v.deps.Replay.Release(ctx, k)
	}
}

func (v *Verifier) replayKey(ident []byte) string {
	h := sha256.New()
	for _, part := range [][]byte{[]byte(v.deps.Scope), []byte(v.plan.Replay.Source), ident} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// stamp is a parsed timestamp header: the instant, and the text the sender
// signed.
type stamp struct {
	raw string
	at  time.Time
}

func (v *Verifier) readStamp(h http.Header) (stamp, error) {
	raw, err := headerValue(h, v.plan.tsHdr)
	if err != nil {
		return stamp{}, err
	}
	at, err := v.plan.tsFmt.parse(raw)
	if err != nil {
		return stamp{}, fmt.Errorf("%w: %s is not %s", ErrMalformed, v.plan.tsHdr, v.plan.tsFmt)
	}
	return stamp{raw: raw, at: at}, nil
}

func (v *Verifier) now() time.Time {
	if v.deps.Clock != nil {
		return v.deps.Clock.Now()
	}
	return time.Now()
}

func (v *Verifier) verifySecret(ctx context.Context, req Request) (*Result, error) {
	raw, err := headerValue(req.Header, v.plan.header)
	if err != nil {
		return nil, err
	}
	presented := raw
	if v.plan.Scheme == SchemeBearer {
		if presented, err = bearerToken(raw); err != nil {
			return nil, err
		}
	}
	keys, err := v.keys(ctx)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(presented))
	ok := false
	for _, k := range keys {
		kd := sha256.Sum256(k)
		if equalFixed(digest[:], kd[:]) == 1 {
			ok = true
		}
	}
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrInvalid, v.plan.header)
	}
	return &Result{Scheme: v.plan.Scheme}, nil
}

// keys resolves and decodes the secrets, refusing any that is unusable.
func (v *Verifier) keys(ctx context.Context) ([][]byte, error) {
	raw, err := v.secrets.Secrets(ctx)
	if err != nil {
		if errors.Is(err, ErrSecret) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: none resolved", ErrSecret)
	}
	out := make([][]byte, len(raw))
	for i, k := range raw {
		if len(k) == 0 {
			return nil, fmt.Errorf("%w: empty", ErrSecret)
		}
		dk, err := v.plan.KeyEncoding.decodeKey(k)
		if err != nil {
			return nil, fmt.Errorf("%w: does not decode as %s", ErrSecret, v.plan.KeyEncoding)
		}
		switch {
		case v.plan.Scheme == SchemeEd25519 && len(dk) != ed25519.PublicKeySize:
			return nil, fmt.Errorf("%w: not a %d-byte public key", ErrSecret, ed25519.PublicKeySize)
		case len(dk) == 0, len(dk) < v.plan.MinSecretBytes && v.plan.Scheme != SchemeEd25519:
			return nil, fmt.Errorf("%w: shorter than %d bytes", ErrSecret, v.plan.MinSecretBytes)
		}
		out[i] = dk
	}
	return out, nil
}

// equalFixed compares two equal-length digests in constant time. It is a
// variable only so a test can observe what is compared.
var equalFixed = subtle.ConstantTimeCompare

// headerValue returns the single value of header name (canonical form),
// matched without regard to the case of the stored key. A header that is
// absent or empty is ErrMissing; one repeated, under any spelling, is
// ErrMalformed: with two values a proxy and this verifier may read
// different ones.
func headerValue(h http.Header, name string) (string, error) {
	var vals []string
	for k, vs := range h {
		if strings.EqualFold(k, name) {
			vals = append(vals, vs...)
		}
	}
	switch len(vals) {
	case 0:
		return "", fmt.Errorf("%w: %s", ErrMissing, name)
	case 1:
		v := strings.Trim(vals[0], " \t")
		if v == "" {
			return "", fmt.Errorf("%w: %s", ErrMissing, name)
		}
		return v, nil
	}
	return "", fmt.Errorf("%w: %s repeated", ErrMalformed, name)
}

// bearerToken extracts the token of an RFC 6750 Authorization value. The
// scheme word is case-insensitive and separated by one or more spaces
// (RFC 9110 11.1, 11.6.2). A value of another scheme carries no bearer
// credential. The token itself is taken as sent: anything but whitespace
// and control characters is left to the comparison.
func bearerToken(v string) (string, error) {
	scheme, rest, _ := strings.Cut(v, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("%w: not a bearer credential", ErrMissing)
	}
	tok := strings.TrimLeft(rest, " ")
	if tok == "" {
		return "", fmt.Errorf("%w: empty bearer token", ErrMalformed)
	}
	for i := 0; i < len(tok); i++ {
		if tok[i] <= ' ' || tok[i] == 0x7f {
			return "", fmt.Errorf("%w: bearer token has whitespace or control characters", ErrMalformed)
		}
	}
	return tok, nil
}
