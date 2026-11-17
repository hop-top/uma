package meta

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/ports"
)

// DefaultBaseURL is the Graph API origin.
const DefaultBaseURL = "https://graph.facebook.com"

// maxResponseBytes bounds a Graph API response body read into memory.
const maxResponseBytes = 1 << 20

var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+$`)

// ValidVersion reports whether v is a Graph API version path segment such
// as "v24.0".
func ValidVersion(v string) bool { return versionPattern.MatchString(v) }

// Client calls the Graph API over an injected HTTP client. The zero value
// is not usable: HTTP and Version are required. Every request goes to
// BaseURL; the access token is sent as a bearer token to that origin only.
type Client struct {
	HTTP ports.HTTPClient
	// BaseURL is the API origin; empty means DefaultBaseURL.
	BaseURL string
	// Version is the API version path segment, such as "v24.0".
	Version string
	// Now reads the clock for HTTP-date Retry-After values; nil means
	// time.Now.
	Now func() time.Time
}

// Response is a successful (2xx) Graph API answer.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// TransportError is a request that got no HTTP answer. Sent is false only
// when the request provably never left the host (the connection was never
// established), so nothing was applied; otherwise the provider may have
// applied it. The wrapped error is the transport's and may name the host.
type TransportError struct {
	Sent bool
	Err  error
}

func (e *TransportError) Error() string {
	if e.Sent {
		return "meta: graph request sent, no answer"
	}
	return "meta: graph request not sent"
}

func (e *TransportError) Unwrap() error { return e.Err }

// Error is a non-2xx Graph API answer. Code is the Graph error code (0
// when the body carried none); Status the HTTP status. Message, Details and
// TraceID are provider text: keep them out of anything shown to users.
type Error struct {
	Status  int
	Code    int
	Subcode int
	Type    string
	Message string
	Details string
	TraceID string
	// Parsed reports whether the body was a Graph error object, so the
	// provider itself answered with Code.
	Parsed bool
	// RetryAfter is the wait the answer states, from Retry-After or the
	// X-Business-Use-Case-Usage estimate; zero when it states none.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("meta: graph error %d (http %d)", e.Code, e.Status)
}

type errorBody struct {
	Error *struct {
		Message   string `json:"message"`
		Type      string `json:"type"`
		Code      int    `json:"code"`
		Subcode   int    `json:"error_subcode"`
		ErrorData struct {
			Details string `json:"details"`
		} `json:"error_data"`
		TraceID string `json:"fbtrace_id"`
	} `json:"error"`
}

// Post sends body as JSON to path under the version, authorized by token,
// and returns the 2xx answer, an *Error for any other status, or a
// *TransportError when no answer arrived. A cancelled or expired ctx is
// returned as the context error.
func (c Client) Post(ctx context.Context, path, token string, body any) (Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}
	return c.do(ctx, http.MethodPost, path, token, data)
}

// Get fetches path under the version, authorized by token.
func (c Client) Get(ctx context.Context, path, token string) (Response, error) {
	return c.do(ctx, http.MethodGet, path, token, nil)
}

func (c Client) do(ctx context.Context, method, path, token string, body []byte) (Response, error) {
	if c.HTTP == nil {
		return Response{}, errors.New("meta: no HTTP client")
	}
	if !ValidVersion(c.Version) {
		return Response{}, errors.New("meta: invalid graph API version")
	}
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(strings.TrimRight(base, "/") + "/" + c.Version + "/" + strings.TrimLeft(path, "/"))
	if err != nil {
		return Response{}, err
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Response{}, ctxErr
		}
		return Response{}, &TransportError{Sent: !notSent(err), Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Response{}, ctxErr
		}
		return Response{}, &TransportError{Sent: true, Err: err}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return Response{Status: resp.StatusCode, Header: resp.Header, Body: data}, nil
	}
	return Response{}, c.graphError(resp, data)
}

func (c Client) graphError(resp *http.Response, data []byte) *Error {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	e := &Error{Status: resp.StatusCode, RetryAfter: RetryAfter(resp.Header, now())}
	var b errorBody
	if json.Unmarshal(data, &b) == nil && b.Error != nil {
		e.Parsed = true
		e.Code, e.Subcode, e.Type = b.Error.Code, b.Error.Subcode, b.Error.Type
		e.Message, e.Details, e.TraceID = b.Error.Message, b.Error.ErrorData.Details, b.Error.TraceID
	}
	return e
}

// notSent reports a transport error raised before any request byte was
// written: a refused or unreachable dial, a failed DNS lookup or a failed
// TLS handshake.
func notSent(err error) bool {
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	if errors.As(err, &dns) || errors.As(err, &cert) || errors.As(err, &record) || errors.As(err, &alert) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// UsageHeader is the Graph API business use case usage header.
const UsageHeader = "X-Business-Use-Case-Usage"

// RetryAfter returns the wait an answer states: the standard Retry-After
// header (seconds or HTTP date, relative to now) when present, else the
// largest estimated_time_to_regain_access (minutes) in the
// X-Business-Use-Case-Usage header. Zero means no stated wait.
func RetryAfter(h http.Header, now time.Time) time.Duration {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if s, err := strconv.Atoi(v); err == nil && s > 0 {
			return time.Duration(s) * time.Second
		}
		if t, err := http.ParseTime(v); err == nil && t.After(now) {
			return t.Sub(now)
		}
	}
	var usage map[string][]struct {
		Minutes float64 `json:"estimated_time_to_regain_access"`
	}
	if v := h.Get(UsageHeader); v == "" || json.Unmarshal([]byte(v), &usage) != nil {
		return 0
	}
	var most float64
	for _, entries := range usage {
		for _, u := range entries {
			most = max(most, u.Minutes)
		}
	}
	return time.Duration(most * float64(time.Minute))
}

// MediaHost is the host serving media download URLs.
const MediaHost = "lookaside.fbsbx.com"

// ErrForeignURL refuses a download URL the access token must not reach.
var ErrForeignURL = errors.New("meta: download URL outside the Graph origin and media host")

// Download GETs rawURL, a media URL the Graph API returned, authorized by
// token, and returns the 2xx body for the caller to close; an *Error for
// any other status, a *TransportError when no answer arrived. The token is
// only ever sent to https://MediaHost or to the client's own BaseURL
// origin: any other URL is ErrForeignURL, refused before a request.
func (c Client) Download(ctx context.Context, rawURL, token string) (io.ReadCloser, error) {
	if c.HTTP == nil {
		return nil, errors.New("meta: no HTTP client")
	}
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	b, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.User != nil {
		return nil, ErrForeignURL
	}
	switch {
	case u.Scheme == "https" && u.Host == MediaHost:
	case u.Scheme == b.Scheme && u.Host == b.Host:
	default:
		return nil, ErrForeignURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &TransportError{Sent: !notSent(err), Err: err}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.Body, nil
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	return nil, c.graphError(resp, data)
}
