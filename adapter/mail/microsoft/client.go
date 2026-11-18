package microsoft

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

// Credential purposes this adapter resolves.
const (
	// PurposeGraph is the Graph access token: OAuth2, delegated or
	// app-only.
	PurposeGraph = "graph"
	// PurposeNotifications is the secret the change-notification client
	// state is derived from: a Secret, shared by the receiver that creates
	// subscriptions and the webhook that checks notifications.
	PurposeNotifications = "graph.notifications"
)

// maxResponseBytes bounds every Graph response body read.
const maxResponseBytes = 8 << 20

// Prefer header values. Immutable IDs keep a message ID stable across
// folder moves, so the ID a notification, a delta round and a send report
// for one message is the same.
const (
	preferImmutableID = `IdType="ImmutableId"`
	preferTextBody    = `outlook.body-content-type="text"`
)

// client is one channel's Graph API access: the HTTP port, the endpoint and
// the credential reference. It keeps no credential; see session.
type client struct {
	http     ports.HTTPClient
	endpoint string
	origin   *url.URL
	creds    ports.Credentials
	channel  model.ChannelRef
	credRef  string
	now      func() time.Time
	// tokens, when set, is the access token source instead of creds: the
	// standalone Mailer's. fresh asks for a token other than the one that
	// was rejected or has expired.
	tokens func(ctx context.Context, fresh bool) (string, time.Time, error)
}

// session is one unit of Graph work (a send, a reconcile, a delta round, a
// webhook request): it resolves the access token on first use, never past
// its expiry, and once more with Fresh after a 401.
type session struct {
	c     *client
	token string
	exp   time.Time
	fresh bool // a Fresh token was already used
}

func (c *client) session() *session { return &session{c: c} }

// response is a successful Graph answer.
type response struct {
	status int
	header http.Header
	body   []byte
}

// request is one Graph call.
type request struct {
	method      string
	url         string // absolute; built from the endpoint or a checked link
	body        []byte
	contentType string
	prefer      []string
	accept      string // Accept header; JSON when empty
	limit       int64  // response body limit; zero is maxResponseBytes
}

// errTransport marks a call whose answer never arrived: the request may or
// may not have reached Graph. errUnsent marks the narrower case where the
// HTTP client reported a DNS, connect or TLS failure and never wrote the
// request: Graph cannot have seen it.
var (
	errTransport = errors.New("graph: no answer")
	errUnsent    = errors.New("graph: request never sent")
)

// do sends r with a bearer token. A 401 resolves a Fresh token and resends
// once; a second 401 is model.Unauthorized. Other non-2xx answers map to
// model errors (see statusError). A call without an answer returns an error
// wrapping errTransport.
func (s *session) do(ctx context.Context, r request) (response, error) {
	for {
		if err := s.ensureToken(ctx); err != nil {
			return response{}, err
		}
		resp, err := s.send(ctx, r)
		if err != nil {
			return response{}, err
		}
		if resp.status != http.StatusUnauthorized {
			if resp.status/100 == 2 {
				return resp, nil
			}
			return resp, statusError(resp.status, resp.header)
		}
		if s.fresh {
			return resp, &model.Error{Code: model.Unauthorized, SafeMessage: "graph rejected a fresh access token", Retry: model.NoRetry}
		}
		s.token, s.fresh = "", true
	}
}

func (s *session) send(ctx context.Context, r request) (response, error) {
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
		return response{}, &model.Error{Code: model.Permanent, SafeMessage: "graph request could not be built", Retry: model.NoRetry}
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	accept := r.accept
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	if r.contentType != "" {
		req.Header.Set("Content-Type", r.contentType)
	}
	for _, p := range r.prefer {
		req.Header.Add("Prefer", p)
	}
	res, err := s.c.http.Do(req)
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
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return response{}, errTransport
	}
	if int64(len(b)) > limit {
		return response{}, model.NewReasonError(model.Permanent, model.TooLarge, "graph response exceeds the size limit")
	}
	return response{status: res.StatusCode, header: res.Header, body: b}, nil
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

// resolve asks the host for the channel's Graph access token.
func (c *client) resolve(ctx context.Context, fresh bool) (string, time.Time, error) {
	if c.tokens != nil {
		return c.tokens(ctx, fresh)
	}
	if c.creds == nil {
		return "", time.Time{}, &model.Error{Code: model.Unauthorized, SafeMessage: "no credentials port", Retry: model.NoRetry}
	}
	cred, err := c.creds.Resolve(ctx, ports.CredentialRequest{
		Channel: c.channel, Reference: c.credRef, Purpose: PurposeGraph,
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
		return "", time.Time{}, &model.Error{Code: model.Unsupported, SafeMessage: "graph needs an OAuth2 credential", Retry: model.NoRetry}
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

// statusError maps a non-2xx Graph status to a safe model error. Throttling
// (429) is rate_limited and 503 transient, both with the Retry-After wait;
// other 5xx are transient; 403 is a missing or revoked permission; other
// 4xx are permanent. Provider error text never reaches the message.
func statusError(status int, h http.Header) *model.Error {
	msg := "graph answered HTTP " + strconv.Itoa(status)
	switch {
	case status == http.StatusTooManyRequests:
		return &model.Error{Code: model.RateLimited, SafeMessage: msg, Retry: model.RetrySameOperation, RetryAfter: retryAfter(h)}
	case status == http.StatusServiceUnavailable:
		return &model.Error{Code: model.Transient, SafeMessage: msg, Retry: model.RetrySameOperation, RetryAfter: retryAfter(h)}
	case status >= 500:
		return &model.Error{Code: model.Transient, SafeMessage: msg, Retry: model.RetrySameOperation}
	case status == http.StatusForbidden:
		return &model.Error{Code: model.Unauthorized, SafeMessage: msg, Retry: model.NoRetry}
	case status == http.StatusNotFound:
		return &model.Error{Code: model.NotFound, SafeMessage: msg, Retry: model.NoRetry}
	case status == http.StatusConflict:
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

// api returns the absolute URL of path under the endpoint, with query.
func (c *client) api(path string, query url.Values) string {
	u := c.endpoint + path
	if len(query) > 0 {
		u += "?" + encodeQuery(query)
	}
	return u
}

// encodeQuery encodes q keeping OData's "$" option names readable.
func encodeQuery(q url.Values) string {
	return strings.ReplaceAll(q.Encode(), "%24", "$")
}

// sameOrigin reports whether link (a nextLink or deltaLink Graph returned)
// points at the endpoint's scheme and host, so a bearer token never leaves
// for another origin.
func (c *client) sameOrigin(link string) bool {
	u, err := url.Parse(link)
	return err == nil && u.Scheme == c.origin.Scheme && strings.EqualFold(u.Host, c.origin.Host) && u.User == nil
}

func decode(b []byte, v any) error {
	if err := json.Unmarshal(b, v); err != nil {
		return &model.Error{Code: model.Permanent, SafeMessage: "graph answered an unexpected body", Retry: model.NoRetry}
	}
	return nil
}

func encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("microsoft: encode request: " + err.Error())
	}
	return b
}

// transient turns a call without an answer into a retryable failure, for
// calls whose repetition is harmless.
func transient(err error) error {
	if errors.Is(err, errTransport) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return model.NewError(model.Transient, "graph did not answer")
	}
	return err
}
