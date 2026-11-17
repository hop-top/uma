package xrrtest

import (
	"net/http"
	"strings"

	xrr "hop.top/xrr"
)

// Mode selects record, replay or passthrough.
type Mode = xrr.Mode

// Modes accepted by WithMode and the XRR_MODE environment variable.
const (
	ModeRecord      = xrr.ModeRecord
	ModeReplay      = xrr.ModeReplay
	ModePassthrough = xrr.ModePassthrough
)

// EnvMode names the environment variable that selects the mode when
// WithMode is not given. Unset or empty means replay.
const EnvMode = xrr.EnvMode

// DefaultHeaders are the header names recorded for requests and responses.
// A trailing "*" matches any header with that prefix. Matching ignores case.
var DefaultHeaders = []string{"Content-Type", "Location", "Retry-After", "X-RateLimit-*"}

// NormalizeFunc rewrites the recorded form of a request before it is
// fingerprinted and saved. It runs after the built-in normalization and
// before placeholders and scrubbing. It never changes the request sent to
// the provider.
type NormalizeFunc func(r *Request)

// ScrubFunc returns body with secrets removed. contentType is the original
// Content-Type of the request or response. It runs after ScrubSecrets on
// request bodies (before fingerprinting) and response bodies (before
// saving).
type ScrubFunc func(contentType string, body []byte) []byte

// Option configures a Transport.
type Option func(*config)

type config struct {
	mode      Mode
	modeSet   bool
	base      http.RoundTripper
	headers   []string
	dropQuery map[string]bool
	normalize []NormalizeFunc
	scrub     []ScrubFunc
	rewrites  []rewrite
}

type rewrite struct{ value, placeholder string }

func newConfig(opts []Option) *config {
	c := &config{
		base:      http.DefaultTransport,
		headers:   append([]string(nil), DefaultHeaders...),
		dropQuery: map[string]bool{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// WithMode sets the mode, overriding XRR_MODE.
func WithMode(m Mode) Option {
	return func(c *config) { c.mode, c.modeSet = m, true }
}

// WithTransport sets the transport used to reach the provider in record and
// passthrough modes. Default http.DefaultTransport.
func WithTransport(rt http.RoundTripper) Option {
	return func(c *config) {
		if rt != nil {
			c.base = rt
		}
	}
}

// WithHeaders adds header names to the recorded allow-list. A trailing "*"
// is a prefix match. Credential headers (Authorization, Cookie, API keys,
// tokens) are dropped even when listed.
func WithHeaders(names ...string) Option {
	return func(c *config) { c.headers = append(c.headers, names...) }
}

// WithDropQuery removes volatile query parameters (timestamps, nonces,
// cache busters) from the recorded URL and the fingerprint.
func WithDropQuery(names ...string) Option {
	return func(c *config) {
		for _, n := range names {
			c.dropQuery[n] = true
		}
	}
}

// WithPlaceholder replaces value with placeholder in the recorded request
// URL and body and in the recorded response body and header values, so
// account, owner or host identifiers never reach a cassette and the
// fingerprint does not depend on them. Responses handed back to the caller
// carry value again, in every mode. An empty value is ignored, so a replay
// run can pass a fake or unset identifier for the same placeholder.
//
// Matching is literal on the encoded URL and raw body bytes. Use
// placeholders that cannot occur in real payloads, e.g. "ACCOUNT_SID".
func WithPlaceholder(value, placeholder string) Option {
	return func(c *config) {
		if value != "" && placeholder != "" && value != placeholder {
			c.rewrites = append(c.rewrites, rewrite{value, placeholder})
		}
	}
}

// WithNormalize adds a request normalization hook.
func WithNormalize(fn NormalizeFunc) Option {
	return func(c *config) {
		if fn != nil {
			c.normalize = append(c.normalize, fn)
		}
	}
}

// WithScrub adds a body scrub hook. ScrubSecrets always runs first.
func WithScrub(fn ScrubFunc) Option {
	return func(c *config) {
		if fn != nil {
			c.scrub = append(c.scrub, fn)
		}
	}
}

// allowHeader reports whether a header is recorded.
func (c *config) allowHeader(name string) bool {
	if isCredentialHeader(name) {
		return false
	}
	for _, h := range c.headers {
		if p, ok := strings.CutSuffix(h, "*"); ok {
			if len(name) >= len(p) && strings.EqualFold(name[:len(p)], p) {
				return true
			}
			continue
		}
		if strings.EqualFold(name, h) {
			return true
		}
	}
	return false
}

func (c *config) toPlaceholders(s string) string {
	for _, r := range c.rewrites {
		s = strings.ReplaceAll(s, r.value, r.placeholder)
	}
	return s
}

func (c *config) fromPlaceholders(s string) string {
	for _, r := range c.rewrites {
		s = strings.ReplaceAll(s, r.placeholder, r.value)
	}
	return s
}
