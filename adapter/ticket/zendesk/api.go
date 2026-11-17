package zendesk

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Credential purposes. Both resolve the channel's credential reference.
const (
	// CredentialPurposeAPI is the Support API credential, resolved before
	// every API call (see AuthAPIToken and AuthOAuth for its kinds).
	CredentialPurposeAPI = string(model.TypeZendeskTickets) + ".api"
	// CredentialPurposeWebhook is the webhook signing secret, a
	// ports.Secret, resolved for every webhook request.
	CredentialPurposeWebhook = string(model.TypeZendeskTickets) + ".webhook"
)

// maxResponseBytes bounds a Zendesk API response body.
const maxResponseBytes = 8 << 20

// api is a Support API client for one channel. Every call resolves the
// API credential afresh, so a rotated or revoked credential takes effect
// on the next call.
type api struct {
	base    string // scheme://host[/prefix], no trailing slash
	cfg     Config
	channel model.Channel
	creds   ports.Credentials
	http    ports.HTTPClient
	clock   ports.Clock
}

// apiError is a provider answer the adapter maps to a typed failure.
// applied reports whether the provider may have applied the request.
type apiError struct {
	status  int
	applied bool
	err     *model.Error
}

func (e *apiError) Error() string { return e.err.Error() }

// errUnknown marks an answer that says nothing about whether the request
// applied: a transport failure after the request may have reached the
// provider, a 5xx other than 503, or an unreadable 2xx body.
func errUnknown() *apiError {
	return &apiError{applied: true, err: model.NewError(model.OutcomeUnknown, "zendesk outcome unknown; reconcile before retrying")}
}

// do sends one API request with a JSON body (nil for none) and decodes a
// 2xx JSON answer into out (nil to discard it). A typed *model.Error is
// returned for credential refusals; every provider answer and transport
// failure is an *apiError.
func (a *api) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	auth, err := a.authorization(ctx)
	if err != nil {
		return err
	}
	u := a.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return &apiError{err: model.NewError(model.Validation, "request body does not encode")}
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return &apiError{err: model.NewError(model.Permanent, "request does not build")}
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if notSent(err) {
			return &apiError{err: model.NewError(model.Transient, "zendesk unreachable")}
		}
		return errUnknown()
	}
	defer func() { _ = resp.Body.Close() }()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode/100 == 2 {
		if readErr != nil {
			return errUnknown()
		}
		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return errUnknown()
			}
		}
		return nil
	}
	return a.failure(resp)
}

// failure maps a non-2xx answer. Zendesk ticket updates take no
// idempotency key (its Idempotency-Key header covers ticket creation only),
// so a request that reached Zendesk is retried only when the answer proves
// nothing applied: a 429 rate limit. Every 5xx, 503 included, and a 409
// update collision are outcome unknown: the worker reconciles before any
// second write.
func (a *api) failure(resp *http.Response) error {
	status := resp.StatusCode
	switch {
	case status == http.StatusTooManyRequests:
		e := model.NewError(model.RateLimited, "zendesk rate limit")
		e.RetryAfter = retryAfter(resp.Header.Get("Retry-After"), a.now())
		return &apiError{status: status, err: e}
	case status == http.StatusConflict || status >= 500:
		e := errUnknown()
		e.status = status
		return e
	case status == http.StatusUnauthorized:
		return &apiError{status: status, err: &model.Error{Code: model.Unauthorized, SafeMessage: "zendesk refused the credential", Retry: model.NoRetry}}
	case status == http.StatusForbidden:
		return &apiError{status: status, err: &model.Error{Code: model.Unauthorized, SafeMessage: "zendesk permission denied", Retry: model.NoRetry}}
	case status == http.StatusNotFound:
		return &apiError{status: status, err: &model.Error{Code: model.NotFound, SafeMessage: "zendesk ticket deleted or inaccessible", Retry: model.NoRetry}}
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity || status == http.StatusRequestEntityTooLarge:
		return &apiError{status: status, err: &model.Error{Code: model.Permanent, SafeMessage: "zendesk rejected the request", Retry: model.NoRetry}}
	}
	return &apiError{status: status, err: &model.Error{Code: model.Permanent, SafeMessage: "zendesk answered " + strconv.Itoa(status), Retry: model.NoRetry}}
}

// authorization resolves the API credential into an Authorization value.
func (a *api) authorization(ctx context.Context) (string, error) {
	if a.creds == nil {
		return "", &model.Error{Code: model.Unavailable, SafeMessage: "credentials port not configured", Retry: model.NoRetry}
	}
	c, err := a.creds.Resolve(ctx, ports.CredentialRequest{
		Channel: a.channel.Ref, Reference: a.channel.CredentialRef, Purpose: CredentialPurposeAPI,
		Accept: a.accepted(),
	})
	if err != nil {
		return "", err
	}
	switch v := c.(type) {
	case ports.Secret:
		if len(v.Bytes) == 0 {
			break
		}
		if a.cfg.Auth == AuthOAuth {
			return "Bearer " + string(v.Bytes), nil
		}
		if a.cfg.Email == "" {
			return "", &model.Error{Code: model.Validation, SafeMessage: "api_token with a bare secret needs configuration email", Retry: model.NoRetry}
		}
		return basic(a.cfg.Email+"/token", v.Bytes), nil
	case ports.Password:
		if a.cfg.Auth == AuthAPIToken && v.User != "" && len(v.Secret) > 0 {
			return basic(v.User+"/token", v.Secret), nil
		}
	case ports.OAuth2:
		if a.cfg.Auth == AuthOAuth && v.AccessToken != "" {
			return "Bearer " + v.AccessToken, nil
		}
	}
	return "", &model.Error{Code: model.Unsupported, SafeMessage: "credential kind unusable for zendesk " + a.cfg.Auth, Retry: model.NoRetry}
}

func (a *api) accepted() []ports.CredentialKind {
	if a.cfg.Auth == AuthOAuth {
		return []ports.CredentialKind{ports.KindOAuth2, ports.KindSecret}
	}
	return []ports.CredentialKind{ports.KindSecret, ports.KindPassword}
}

func basic(user string, secret []byte) string {
	raw := make([]byte, 0, len(user)+1+len(secret))
	raw = append(append(append(raw, user...), ':'), secret...)
	return "Basic " + base64.StdEncoding.EncodeToString(raw)
}

func (a *api) now() time.Time {
	if a.clock == nil {
		return time.Now()
	}
	return a.clock.Now()
}

// retryAfter reads a Retry-After value: delay seconds or an HTTP date.
func retryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if s, err := strconv.ParseInt(v, 10, 64); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// notSent reports a transport failure that provably happened before any
// byte of the request reached Zendesk: a dial failure or a failed TLS
// handshake. Any other transport failure may follow a written request.
func notSent(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var (
		header tls.RecordHeaderError
		alert  tls.AlertError
		verify *tls.CertificateVerificationError
		auth   x509.UnknownAuthorityError
		host   x509.HostnameError
		cert   x509.CertificateInvalidError
	)
	return errors.As(err, &header) || errors.As(err, &alert) || errors.As(err, &verify) ||
		errors.As(err, &auth) || errors.As(err, &host) || errors.As(err, &cert)
}

// asFailure returns the typed failure of err.
func asFailure(err error) *model.Error {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.err
	}
	var me *model.Error
	if errors.As(err, &me) && me.Code.Valid() {
		return me
	}
	return model.NewError(model.Unavailable, "zendesk call failed")
}

// statusOf returns the HTTP status of a provider answer, or 0.
func statusOf(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.status
	}
	return 0
}

func ticketPath(id string) string { return "/api/v2/tickets/" + url.PathEscape(id) }
