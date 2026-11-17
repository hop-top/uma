package google

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// PurposeGmail is the credential purpose of the Gmail API access token:
// OAuth2, for the mailbox user (or a service account acting for it through
// domain-wide delegation).
const PurposeGmail = "gmail"

// Response size limits. Gmail receives messages of up to 70 MB after
// encoding (Enterprise Plus; 50 MB on Enterprise Standard), read here as
// MiB, the larger. Message.raw is that message base64url encoded again
// inside the JSON answer, 4/3 as long, so the answer bound is the encoded
// length plus headroom for the other fields. A message over it is not
// fetched whole: see fetchHeaderOnly.
const (
	maxResponseBytes        = 8 << 20
	maxReceivedMessageBytes = 70 << 20
	maxRawMessageBytes      = (maxReceivedMessageBytes*4+2)/3 + 1<<20
	maxErrorDetailBytes     = 64 << 10
)

// client is one channel's Gmail API access: the HTTP port, the user's API
// root and the credential reference. It keeps no credential; see session.
type client struct {
	http    ports.HTTPClient
	base    string // <endpoint>/users/<userId>
	creds   ports.Credentials
	channel model.ChannelRef
	credRef string
	now     func() time.Time
	// rawLimit overrides maxRawMessageBytes; zero keeps it.
	rawLimit int64
}

// session is one unit of Gmail work (a send, a reconcile, a history round,
// a push request): it resolves the access token on first use, never past
// its expiry, and once more with Fresh after a 401.
type session struct {
	c     *client
	token string
	exp   time.Time
	fresh bool // a Fresh token was already used
}

func (c *client) session() *session { return &session{c: c} }

// response is a Gmail answer.
type response struct {
	status int
	header http.Header
	body   []byte
}

// request is one Gmail call.
type request struct {
	method string
	url    string
	body   []byte // JSON
	limit  int64  // response body limit; zero is maxResponseBytes
}

// errTransport marks a call whose answer never arrived: the request may or
// may not have reached Gmail. errUnsent marks the narrower case where the
// HTTP client reported a DNS, connect or TLS failure and never wrote the
// request: Gmail cannot have seen it.
var (
	errTransport = errors.New("gmail: no answer")
	errUnsent    = errors.New("gmail: request never sent")
)

// do sends r with a bearer token. A 401 resolves a Fresh token and resends
// once; a second 401 is model.Unauthorized. Other non-2xx answers map to
// model errors (see statusError) and come back with the response. A call
// without an answer returns an error wrapping errTransport.
func (s *session) do(ctx context.Context, r request) (response, error) {
	for {
		if err := s.ensureToken(ctx); err != nil {
			return response{}, err
		}
		resp, err := s.c.send(ctx, r, s.token)
		if err != nil {
			return response{}, err
		}
		if resp.status != http.StatusUnauthorized {
			if resp.status/100 == 2 {
				return resp, nil
			}
			return resp, statusError(resp)
		}
		if s.fresh {
			return resp, &model.Error{Code: model.Unauthorized, SafeMessage: "gmail rejected a fresh access token", Retry: model.NoRetry}
		}
		s.token, s.fresh = "", true
	}
}

// send makes one HTTP exchange. token is sent as a bearer token when set.
func (c *client) send(ctx context.Context, r request, token string) (response, error) {
	var body io.Reader
	if r.body != nil {
		body = bytes.NewReader(r.body)
	}
	var mu sync.Mutex
	var wrote, dialFailed bool
	trace := &httptrace.ClientTrace{
		DNSDone: func(i httptrace.DNSDoneInfo) {
			mu.Lock()
			dialFailed = dialFailed || i.Err != nil
			mu.Unlock()
		},
		ConnectDone: func(_, _ string, err error) {
			mu.Lock()
			dialFailed = dialFailed || err != nil
			mu.Unlock()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			mu.Lock()
			dialFailed = dialFailed || err != nil
			mu.Unlock()
		},
		WroteHeaders: func() {
			mu.Lock()
			wrote = true
			mu.Unlock()
		},
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), r.method, r.url, body)
	if err != nil {
		return response{}, &model.Error{Code: model.Permanent, SafeMessage: "gmail request could not be built", Retry: model.NoRetry}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		mu.Lock()
		unsent := dialFailed && !wrote
		mu.Unlock()
		switch {
		case ctx.Err() != nil:
			return response{}, errors.Join(errTransport, ctx.Err())
		case unsent:
			// Only a client that reports its connection steps can prove
			// this; any other failure stays "no answer".
			return response{}, errors.Join(errTransport, errUnsent)
		}
		return response{}, errTransport
	}
	defer func() { _ = res.Body.Close() }()
	limit := r.limit
	if limit <= 0 {
		limit = maxResponseBytes
	}
	if res.StatusCode/100 != 2 {
		limit = maxErrorDetailBytes
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return response{}, errTransport
	}
	if int64(len(b)) > limit {
		if res.StatusCode/100 != 2 {
			b = b[:limit] // error detail is only scanned for a reason
		} else {
			return response{}, &errTooLarge{&model.Error{Code: model.Permanent, SafeMessage: "gmail response exceeds the size limit", Retry: model.NoRetry}}
		}
	}
	return response{status: res.StatusCode, header: res.Header, body: b}, nil
}

// errTooLarge is a successful answer longer than the request's bound. It is
// a permanent model error; a caller that can do without the body tells it
// with tooLarge.
type errTooLarge struct{ err *model.Error }

func (e *errTooLarge) Error() string { return e.err.Error() }
func (e *errTooLarge) Unwrap() error { return e.err }

// tooLarge reports an answer over its bound.
func tooLarge(err error) bool {
	var e *errTooLarge
	return errors.As(err, &e)
}

// ensureToken resolves a token when none is held or the held one expired.
// An expired token from the host gets one Fresh resolve.
func (s *session) ensureToken(ctx context.Context) error {
	if s.token != "" && (s.exp.IsZero() || s.c.now().Before(s.exp)) {
		return nil
	}
	for {
		tok, exp, err := s.c.resolve(ctx, s.fresh)
		if err != nil {
			return err
		}
		if exp.IsZero() || s.c.now().Before(exp) {
			s.token, s.exp = tok, exp
			return nil
		}
		if s.fresh {
			return &model.Error{Code: model.Unauthorized, SafeMessage: "access token expired", Retry: model.NoRetry}
		}
		s.fresh = true
	}
}

// resolve asks the host for the channel's Gmail access token.
func (c *client) resolve(ctx context.Context, fresh bool) (string, time.Time, error) {
	if c.creds == nil {
		return "", time.Time{}, &model.Error{Code: model.Unauthorized, SafeMessage: "no credentials port", Retry: model.NoRetry}
	}
	cred, err := c.creds.Resolve(ctx, ports.CredentialRequest{
		Channel: c.channel, Reference: c.credRef, Purpose: PurposeGmail,
		Accept: []ports.CredentialKind{ports.KindOAuth2}, Fresh: fresh,
	})
	if err != nil {
		return "", time.Time{}, credentialError(ctx, err)
	}
	var o ports.OAuth2
	switch v := cred.(type) {
	case ports.OAuth2:
		o = v
	case *ports.OAuth2:
		if v != nil {
			o = *v
		}
	default:
		return "", time.Time{}, &model.Error{Code: model.Unsupported, SafeMessage: "gmail needs an OAuth2 credential", Retry: model.NoRetry}
	}
	if o.AccessToken == "" {
		return "", time.Time{}, &model.Error{Code: model.Unauthorized, SafeMessage: "empty access token", Retry: model.NoRetry}
	}
	return o.AccessToken, o.ExpiresAt, nil
}

// credentialError keeps typed host errors and context errors; anything else
// is an unreachable credential store.
func credentialError(ctx context.Context, err error) error {
	var me *model.Error
	switch {
	case errors.As(err, &me):
		return me
	case ctx.Err() != nil:
		return ctx.Err()
	}
	return model.NewError(model.Unavailable, "credential store unavailable")
}

// Rate-limit reasons of a 403 answer (Gmail "Resolve errors" guide):
// project and per-user rate limits and the daily project quota. They are
// refusals: nothing was applied.
var rateLimitReasons = map[string]bool{
	"rateLimitExceeded":     true,
	"userRateLimitExceeded": true,
	"dailyLimitExceeded":    true,
	"RATE_LIMIT_EXCEEDED":   true, // google.rpc.ErrorInfo form
}

// gError is the Google API error body.
type gError struct {
	Error struct {
		Errors []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
		Details []struct {
			Reason string `json:"reason"`
		} `json:"details"`
	} `json:"error"`
}

// rateLimited reports a 429, or a 403 whose error reason is a rate limit.
func rateLimited(r response) bool {
	if r.status == http.StatusTooManyRequests {
		return true
	}
	if r.status != http.StatusForbidden {
		return false
	}
	var e gError
	if json.Unmarshal(r.body, &e) != nil {
		return false
	}
	for _, x := range e.Error.Errors {
		if rateLimitReasons[x.Reason] {
			return true
		}
	}
	for _, x := range e.Error.Details {
		if rateLimitReasons[x.Reason] {
			return true
		}
	}
	return false
}

// statusError maps a non-2xx Gmail answer to a safe model error: rate
// limits (429, 403 with a rate-limit reason) are rate_limited with the
// Retry-After wait, when stated; 5xx are transient; other 403 are a missing
// scope or a domain policy; 404 not found; other 4xx permanent. Provider
// error text never reaches the message. Send applies its own mapping.
func statusError(r response) *model.Error {
	msg := "gmail answered HTTP " + strconv.Itoa(r.status)
	switch {
	case rateLimited(r):
		return &model.Error{Code: model.RateLimited, SafeMessage: msg, Retry: model.RetrySameOperation, RetryAfter: retryAfter(r.header)}
	case r.status >= 500:
		return &model.Error{Code: model.Transient, SafeMessage: msg, Retry: model.RetrySameOperation, RetryAfter: retryAfter(r.header)}
	case r.status == http.StatusForbidden:
		return &model.Error{Code: model.Unauthorized, SafeMessage: msg, Retry: model.NoRetry}
	case r.status == http.StatusNotFound:
		return &model.Error{Code: model.NotFound, SafeMessage: msg, Retry: model.NoRetry}
	case r.status == http.StatusConflict:
		return &model.Error{Code: model.Conflict, SafeMessage: msg, Retry: model.NoRetry}
	}
	return &model.Error{Code: model.Permanent, SafeMessage: msg, Retry: model.NoRetry}
}

// retryAfter parses Retry-After as delay seconds or an HTTP date.
func retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 || n > int64(24*time.Hour/time.Second) {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// api returns the absolute URL of path under the user's root, with query.
func (c *client) api(path string, query url.Values) string {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

func decode(b []byte, v any) error {
	if err := json.Unmarshal(b, v); err != nil {
		return &model.Error{Code: model.Permanent, SafeMessage: "gmail answered an unexpected body", Retry: model.NoRetry}
	}
	return nil
}

// decodeAnswer decodes a read's answer: a history or messages.list page, the
// profile. A body that is not the JSON asked for (a proxy's error page, a
// truncated or empty body) is a fault of the response, which a repeat may
// not meet, and nothing was applied: errUnexpectedAnswer, transient.
func decodeAnswer(b []byte, v any) error {
	if json.Unmarshal(b, v) != nil {
		return errUnexpectedAnswer()
	}
	return nil
}

func encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("google: encode request: " + err.Error())
	}
	return b
}

// transient turns a call without an answer into a retryable failure, for
// calls whose repetition is harmless.
func transient(err error) error {
	if errors.Is(err, errTransport) && !isContext(err) {
		return model.NewError(model.Transient, "gmail did not answer")
	}
	return err
}

// rawMax is the bound of a raw message response.
func (c *client) rawMax() int64 {
	if c.rawLimit > 0 {
		return c.rawLimit
	}
	return maxRawMessageBytes
}
