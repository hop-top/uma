package webhookauth

import (
	"fmt"
	"net/textproto"
	"strings"
	"time"
)

// Scheme selects how a request proves it comes from the sender. The scheme
// is always configured, never read from the request, so a request cannot
// pick a weaker one (algorithm confusion).
type Scheme string

const (
	// SchemeBearer is an RFC 6750 bearer token: the header carries
	// "Bearer <token>" and the token equals the shared secret.
	SchemeBearer Scheme = "bearer"
	// SchemeToken is a shared secret sent as the whole value of a header.
	SchemeToken Scheme = "token"
	// SchemeHMAC is a keyed hash (RFC 2104) of the signed payload, sent in
	// a header under the configured encoding.
	SchemeHMAC Scheme = "hmac"
	// SchemeEd25519 is an Ed25519 signature (RFC 8032) of the signed
	// payload; the secrets are public keys.
	SchemeEd25519 Scheme = "ed25519"
)

// Hash selects the HMAC hash function.
type Hash string

const (
	// HashSHA256 is HMAC-SHA-256, the default.
	HashSHA256 Hash = "sha256"
	// HashSHA512 is HMAC-SHA-512.
	HashSHA512 Hash = "sha512"
)

// Encoding names how bytes travel in a header or a secret. There is no
// auto-detection: a value that does not decode under the configured
// encoding is malformed, so hex and base64 never get mistaken for each
// other.
type Encoding string

const (
	// EncodingRaw is the bytes as they are. It is the default for keys.
	EncodingRaw Encoding = "raw"
)

// Config is the wire-level configuration of one Verifier. It holds no
// secret. Defaults are neutral; every header name, encoding and layout can
// be set so that senders already in the field keep working unchanged.
type Config struct {
	// Scheme is required.
	Scheme Scheme

	// Header carries the credential. Default by scheme: Authorization for
	// SchemeBearer, X-Webhook-Token for SchemeToken, X-Webhook-Signature
	// for the signature schemes. Names match without regard to case.
	Header string

	// Hash is the HMAC hash function (SchemeHMAC only). Default HashSHA256.
	Hash Hash

	// Encoding is how the signature travels in Header (signature schemes
	// only): hex, base64, base64url or their unpadded forms. Default
	// EncodingHex. A value that does not decode under it is malformed; no
	// other encoding is tried.
	Encoding Encoding

	// Prefix is text the signature value starts with, such as "sha256=",
	// stripped before decoding. Empty means none, and a value carrying one
	// anyway does not decode. With PrefixOptional the value may also come
	// without it.
	Prefix         string
	PrefixOptional bool

	// SignatureList opts in to a header carrying several signatures, such
	// as a sender's "v1=a,v1=b" while it rotates its secret; see
	// SignatureList. Off by default: the header is then one signature.
	SignatureList SignatureList

	// Payload is the template of the bytes the signature covers (signature
	// schemes only): literal text, {body}, the raw request body, which must
	// appear once, and {timestamp}, the timestamp header exactly as sent.
	// "{{" and "}}" are literal braces. Default "{body}", or
	// "{timestamp}.{body}" when a timestamp header is configured. {id} is
	// the replay id header, exactly as sent, when Replay.Source is
	// ReplayByHeader.
	Payload string

	// Timestamp turns on the freshness check; see Timestamp.
	Timestamp Timestamp

	// Replay turns on replay rejection; see Replay. It needs a
	// Deps.Replay store.
	Replay Replay

	// KeyEncoding is how the secrets the SecretSource returns are encoded:
	// raw bytes (default) or any signature encoding. SchemeBearer and
	// SchemeToken take raw keys only. Secrets that are text files may end
	// in whitespace; it is ignored for the text encodings.
	KeyEncoding Encoding

	// MinSecretBytes is the shortest decoded secret accepted for bearer,
	// token and HMAC. Default 16 (128 bits; NIST SP 800-107 asks for at
	// least 112, RFC 2104 3 discourages keys shorter than the hash output
	// for full strength). An empty secret is refused whatever this says.
	// Ed25519 keys must be exactly 32 bytes.
	MinSecretBytes int

	// MaxBodyBytes bounds the body VerifyHTTP reads. Default 1 MiB.
	MaxBodyBytes int64
}

const (
	defaultMinSecret = 16
	defaultMaxBody   = 1 << 20
)

// plan is a validated Config with every default applied.
type plan struct {
	Config
	header string // canonical
	layout layout
	tsHdr  string // canonical, empty when the check is off
	tsFmt  TimestampFormat
	tol    time.Duration
	rpHdr  string // canonical, empty unless the replay id is a header
	rpTTL  time.Duration

	listMax int // entries a signature list may hold, 0 when off
}

func (c Config) validate() (plan, error) {
	p := plan{Config: c}
	signed := c.Scheme == SchemeHMAC || c.Scheme == SchemeEd25519
	switch c.Scheme {
	case SchemeBearer:
		p.Header = firstNonEmpty(c.Header, "Authorization")
	case SchemeToken:
		p.Header = firstNonEmpty(c.Header, "X-Webhook-Token")
	case SchemeHMAC, SchemeEd25519:
		p.Header = firstNonEmpty(c.Header, "X-Webhook-Signature")
	case "":
		return p, cfgErr("scheme is required")
	default:
		return p, cfgErr("unknown scheme %q", c.Scheme)
	}
	var err error
	if p.header, err = canonicalHeader(p.Header); err != nil {
		return p, err
	}
	if p.KeyEncoding == "" {
		p.KeyEncoding = EncodingRaw
	}
	if p.KeyEncoding != EncodingRaw && !p.KeyEncoding.text() {
		return p, cfgErr("unknown key encoding %q", c.KeyEncoding)
	}
	if !signed {
		if p.KeyEncoding != EncodingRaw {
			return p, cfgErr("scheme %q takes raw keys, not %q", c.Scheme, c.KeyEncoding)
		}
		if c.Hash != "" || c.Encoding != "" || c.Prefix != "" || c.PrefixOptional || c.Payload != "" || c.SignatureList != (SignatureList{}) {
			return p, cfgErr("hash, encoding, prefix, signature list and payload apply to signature schemes, not %q", c.Scheme)
		}
	} else if err := p.validateSignature(); err != nil {
		return p, err
	}
	if !signed && p.Timestamp.Header != "" && !p.Timestamp.AllowUnsigned {
		return p, cfgErr("scheme %q signs nothing, so its timestamp is unsigned: set AllowUnsigned", c.Scheme)
	}
	if err := p.validateTimestamp(); err != nil {
		return p, err
	}
	if err := p.validateReplay(signed); err != nil {
		return p, err
	}
	if signed {
		if err := p.validateLayout(); err != nil {
			return p, err
		}
	}
	if p.MinSecretBytes < 0 {
		return p, cfgErr("negative minimum secret length")
	}
	if p.MinSecretBytes == 0 {
		p.MinSecretBytes = defaultMinSecret
	}
	if p.MaxBodyBytes < 0 {
		return p, cfgErr("negative body limit")
	}
	if p.MaxBodyBytes == 0 {
		p.MaxBodyBytes = defaultMaxBody
	}
	return p, nil
}

func (p *plan) validateSignature() error {
	switch {
	case p.Scheme == SchemeEd25519 && p.Hash != "":
		return cfgErr("hash applies to scheme %q only", SchemeHMAC)
	case p.Scheme == SchemeHMAC && p.Hash == "":
		p.Hash = HashSHA256
	}
	if p.Scheme == SchemeHMAC && p.Hash != HashSHA256 && p.Hash != HashSHA512 {
		return cfgErr("unknown hash %q", p.Hash)
	}
	if p.Encoding == "" {
		p.Encoding = EncodingHex
	}
	if !p.Encoding.text() {
		return cfgErr("signature encoding %q is not one of hex, base64, base64url, base64raw, base64urlraw", p.Encoding)
	}
	if p.PrefixOptional && p.Prefix == "" {
		return cfgErr("optional prefix without a prefix")
	}
	for i := 0; i < len(p.Prefix); i++ {
		if p.Prefix[i] <= ' ' || p.Prefix[i] > '~' {
			return cfgErr("prefix %q has whitespace or non-ASCII characters", p.Prefix)
		}
	}
	var err error
	p.listMax, err = p.SignatureList.validate()
	return err
}

func (p *plan) validateTimestamp() error {
	t := p.Timestamp
	if t.Header == "" {
		if t.Format != "" || t.Tolerance != 0 || t.AllowUnsigned {
			return cfgErr("timestamp format, tolerance and consent need a timestamp header")
		}
		return nil
	}
	var err error
	if p.tsHdr, err = canonicalHeader(t.Header); err != nil {
		return err
	}
	if p.tsHdr == p.header {
		return cfgErr("timestamp and credential share header %q", p.tsHdr)
	}
	if p.tsFmt = t.Format; p.tsFmt == "" {
		p.tsFmt = TimestampUnix
	}
	if !p.tsFmt.valid() {
		return cfgErr("unknown timestamp format %q", t.Format)
	}
	if t.Tolerance < 0 || t.Tolerance > maxTolerance {
		return cfgErr("timestamp tolerance %v is outside 0..%v", t.Tolerance, maxTolerance)
	}
	if p.tol = t.Tolerance; p.tol == 0 {
		p.tol = defaultTolerance
	}
	return nil
}

func (p *plan) validateReplay(signed bool) error {
	r := p.Replay
	switch r.Source {
	case "":
		if r.Header != "" || r.TTL != 0 || r.AllowUnsigned {
			return cfgErr("replay header, ttl and consent need a replay source")
		}
		return nil
	case ReplayBySignature:
		if !signed {
			return cfgErr("scheme %q has no signature to identify a delivery by", p.Scheme)
		}
		if r.Header != "" {
			return cfgErr("replay by signature takes no header")
		}
	case ReplayByHeader:
		var err error
		if p.rpHdr, err = canonicalHeader(r.Header); err != nil {
			return err
		}
		if p.rpHdr == p.header || p.rpHdr == p.tsHdr {
			return cfgErr("replay id shares header %q with the credential or timestamp", p.rpHdr)
		}
		if !signed && !r.AllowUnsigned {
			return cfgErr("scheme %q signs nothing, so the replay id is unsigned: set AllowUnsigned", p.Scheme)
		}
	default:
		return cfgErr("unknown replay source %q", r.Source)
	}
	if r.TTL < 0 {
		return cfgErr("negative replay ttl")
	}
	p.rpTTL = r.TTL
	if p.tsHdr != "" {
		// A request stamped at the future edge stays fresh for two
		// tolerances; remember it at least that long.
		p.rpTTL = max(p.rpTTL, 2*p.tol+time.Second)
	} else if p.rpTTL == 0 {
		p.rpTTL = defaultReplayTTL
	}
	return nil
}

// validateLayout parses the payload template and settles whether the
// signature covers the timestamp.
func (p *plan) validateLayout() error {
	enabled := map[string]field{"body": fieldBody}
	def := "{body}"
	if p.tsHdr != "" {
		enabled["timestamp"] = fieldTimestamp
		def = "{timestamp}.{body}"
	}
	if p.rpHdr != "" {
		enabled["id"] = fieldID
	}
	l, err := parseLayout(firstNonEmpty(p.Payload, def), enabled)
	if err != nil {
		return err
	}
	p.layout = l
	if p.tsHdr != "" && !l.uses(fieldTimestamp) && !p.Timestamp.AllowUnsigned {
		return cfgErr("the payload does not cover the timestamp; add {timestamp} or set AllowUnsigned")
	}
	if p.rpHdr != "" && !l.uses(fieldID) && !p.Replay.AllowUnsigned {
		return cfgErr("the payload does not cover the replay id; add {id} or set AllowUnsigned")
	}
	return nil
}

func cfgErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrConfig, fmt.Sprintf(format, args...))
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// canonicalHeader returns name in canonical MIME form, or an error when it
// is not an HTTP field name (RFC 9110 5.1 token).
func canonicalHeader(name string) (string, error) {
	if name == "" {
		return "", cfgErr("empty header name")
	}
	for i := 0; i < len(name); i++ {
		if !isTchar(name[i]) {
			return "", cfgErr("header name %q is not a field name", name)
		}
	}
	return textproto.CanonicalMIMEHeaderKey(name), nil
}

func isTchar(c byte) bool {
	switch {
	case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}
