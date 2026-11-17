package twilio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// DefaultBaseURL is the Twilio REST API origin.
const DefaultBaseURL = "https://api.twilio.com"

// DefaultTimeout bounds one REST request when the caller's context has a
// later or no deadline.
const DefaultTimeout = 30 * time.Second

// maxResponseBytes bounds a response body read.
const maxResponseBytes = 1 << 20

// ClientConfig configures a Client.
type ClientConfig struct {
	// HTTP sends every request. Required.
	HTTP ports.HTTPClient
	// BaseURL is the API origin; empty is DefaultBaseURL.
	BaseURL string
	// Timeout bounds one request; zero is DefaultTimeout.
	Timeout time.Duration
	// Clock resolves an HTTP-date Retry-After; nil ignores that form.
	Clock ports.Clock
}

// Client calls the Programmable Messaging REST API (2010-04-01). It never
// retries: the caller decides from the returned error.
type Client struct {
	http    ports.HTTPClient
	base    *url.URL
	timeout time.Duration
	clock   ports.Clock
}

// NewClient returns a client for cfg.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.HTTP == nil {
		return nil, errors.New("twilio: HTTP client required")
	}
	raw := cfg.BaseURL
	if raw == "" {
		raw = DefaultBaseURL
	}
	base, err := url.Parse(raw)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" ||
		base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("twilio: base URL must be an absolute http(s) origin")
	}
	base.Path = strings.TrimSuffix(base.Path, "/")
	t := cfg.Timeout
	if t <= 0 {
		t = DefaultTimeout
	}
	return &Client{http: cfg.HTTP, base: base, timeout: t, clock: cfg.Clock}, nil
}

// Message is the documented subset of a Twilio Message resource.
type Message struct {
	SID                 string  `json:"sid"`
	AccountSID          string  `json:"account_sid"`
	MessagingServiceSID string  `json:"messaging_service_sid"`
	From                string  `json:"from"`
	To                  string  `json:"to"`
	Body                string  `json:"body"`
	Status              string  `json:"status"`
	Direction           string  `json:"direction"`
	NumSegments         string  `json:"num_segments"`
	ErrorCode           *int    `json:"error_code"`
	ErrorMessage        *string `json:"error_message"`
	DateCreated         string  `json:"date_created"`
	// DateSent is empty while a message is queued.
	DateSent string `json:"date_sent"`
	NumMedia string `json:"num_media"`
	URI      string `json:"uri"`
}

// CreateMessage is the form of a Create Message request. Empty fields are
// not sent. One of From and MessagingServiceSID is required.
type CreateMessage struct {
	To                  string
	From                string
	MessagingServiceSID string
	Body                string
	// ContentSID (HX...) sends a Content Template instead of Body, with
	// ContentVariables its placeholder values as a JSON object string,
	// e.g. {"1":"Jad"}.
	ContentSID       string
	ContentVariables string
	StatusCallback   string
}

func (c CreateMessage) form() url.Values {
	f := url.Values{}
	for k, v := range map[string]string{
		"To": c.To, "From": c.From, "MessagingServiceSid": c.MessagingServiceSID,
		"Body": c.Body, "ContentSid": c.ContentSID, "ContentVariables": c.ContentVariables,
		"StatusCallback": c.StatusCallback,
	} {
		if v != "" {
			f.Set(k, v)
		}
	}
	return f
}

// APIError is a non-2xx answer. Twilio's message text is kept out of
// Error, since it can echo phone numbers.
type APIError struct {
	// Status is the HTTP status.
	Status int
	// Code is Twilio's error code, zero when the body had none.
	Code int
	// MoreInfo is Twilio's documentation link for Code.
	MoreInfo string
	// RetryAfter is the wait a Retry-After header stated, zero without one.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("twilio: HTTP %d, error %d", e.Status, e.Code)
	}
	return fmt.Sprintf("twilio: HTTP %d", e.Status)
}

// RequestError is a request that got no readable answer. Sent reports
// whether any part of the request may have reached Twilio: false only when
// the transport proves the connection failed before the request was
// written.
type RequestError struct {
	Sent bool
	Err  error
}

func (e *RequestError) Error() string {
	if e.Sent {
		return "twilio: request sent, answer lost: " + e.Err.Error()
	}
	return "twilio: request not sent: " + e.Err.Error()
}

func (e *RequestError) Unwrap() error { return e.Err }

// CreateMessage sends one message for accountSID. There is no idempotency
// key on this endpoint: a RequestError with Sent set may mean the message
// exists.
func (c *Client) CreateMessage(ctx context.Context, accountSID string, cred ports.Credential, m CreateMessage) (Message, error) {
	var out Message
	err := c.do(ctx, http.MethodPost, accountSID, "/Messages.json", cred, m.form(), &out)
	return out, err
}

// FetchMessage reads the message sid of accountSID.
func (c *Client) FetchMessage(ctx context.Context, accountSID string, cred ports.Credential, sid string) (Message, error) {
	if !validSID(sid, "SM") && !validSID(sid, "MM") {
		return Message{}, model.NewError(model.Validation, "message SID malformed")
	}
	var out Message
	err := c.do(ctx, http.MethodGet, accountSID, "/Messages/"+sid+".json", cred, nil, &out)
	return out, err
}

// do runs one request. path is account-relative and may carry a query;
// form is the POST body, nil sends none.
func (c *Client) do(ctx context.Context, method, accountSID, path string, cred ports.Credential, form url.Values, out any) error {
	if !validSID(accountSID, "AC") {
		return model.NewError(model.Validation, "account SID malformed")
	}
	user, pass, err := basicAuth(accountSID, cred)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	u := *c.base
	path, u.RawQuery, _ = strings.Cut(path, "?")
	u.Path += "/2010-04-01/Accounts/" + accountSID + path
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	var tr sendTrace
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, tr.hooks()), method, u.String(), body)
	if err != nil {
		return &RequestError{Err: err}
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(user, pass)

	resp, err := c.http.Do(req)
	if err != nil {
		return &RequestError{Sent: tr.mayHaveSent(), Err: scrubURL(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return c.apiError(resp, data)
	}
	if readErr != nil {
		return &RequestError{Sent: true, Err: readErr}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &RequestError{Sent: true, Err: errors.New("unreadable success body")}
	}
	return nil
}

func (c *Client) apiError(resp *http.Response, data []byte) *APIError {
	e := &APIError{Status: resp.StatusCode, RetryAfter: c.retryAfter(resp.Header.Get("Retry-After"))}
	var body struct {
		Code     int    `json:"code"`
		MoreInfo string `json:"more_info"`
	}
	if json.Unmarshal(data, &body) == nil {
		e.Code, e.MoreInfo = body.Code, body.MoreInfo
	}
	return e
}

// retryAfter parses a Retry-After value: delay seconds or an HTTP-date.
func (c *Client) retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if s, err := strconv.ParseInt(v, 10, 64); err == nil {
		if s <= 0 || s > int64((24*time.Hour)/time.Second) {
			return 0
		}
		return time.Duration(s) * time.Second
	}
	if c.clock == nil {
		return 0
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0
	}
	if d := t.Sub(c.clock.Now()); d > 0 && d <= 24*time.Hour {
		return d
	}
	return 0
}

// basicAuth returns the Basic credentials of cred: an auth token as a
// Secret (user accountSID), or as a Password the account SID and auth token
// or an API key SID (SK...) and its secret. Any other Password user would
// only earn a 401, so it is refused before the call.
func basicAuth(accountSID string, cred ports.Credential) (string, string, error) {
	switch c := cred.(type) {
	case ports.Secret:
		if len(c.Bytes) == 0 {
			return "", "", model.NewError(model.Unauthorized, "empty Twilio credential")
		}
		return accountSID, string(c.Bytes), nil
	case ports.Password:
		if len(c.Secret) == 0 || (c.User != accountSID && !validSID(c.User, "SK")) {
			return "", "", &model.Error{Code: model.Unauthorized, SafeMessage: "Twilio password user is neither the account SID nor an API key SID", Retry: model.NoRetry}
		}
		return c.User, string(c.Secret), nil
	case nil:
		return "", "", model.NewError(model.Unauthorized, "no Twilio credential")
	}
	return "", "", &model.Error{Code: model.Unsupported, SafeMessage: "credential kind unsupported by Twilio", Retry: model.NoRetry}
}

// sendTrace records how far a request got. Hooks may run on transport
// goroutines. A transport that reports no trace events proves nothing, so
// the request then counts as possibly sent.
type sendTrace struct {
	dialing atomic.Bool
	wrote   atomic.Bool
}

func (t *sendTrace) hooks() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn:      func(string) { t.dialing.Store(true) },
		WroteHeaders: func() { t.wrote.Store(true) },
	}
}

func (t *sendTrace) mayHaveSent() bool { return !t.dialing.Load() || t.wrote.Load() }

// scrubURL drops the request URL from a transport error.
func scrubURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// validSID reports whether s is a 34-character Twilio SID with prefix.
func validSID(s, prefix string) bool {
	if len(s) != 34 || !strings.HasPrefix(s, prefix) {
		return false
	}
	for _, r := range s[2:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// ValidAccountSID reports whether s is a Twilio account SID.
func ValidAccountSID(s string) bool { return validSID(s, "AC") }

// ValidMessagingServiceSID reports whether s is a Messaging Service SID.
func ValidMessagingServiceSID(s string) bool { return validSID(s, "MG") }

// ValidContentSID reports whether s is a Content Template SID.
func ValidContentSID(s string) bool { return validSID(s, "HX") }
