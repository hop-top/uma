package jira

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

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Credential purposes. Both resolve the channel's credential reference.
const (
	// CredentialPurposeAPI is the REST API credential, resolved before
	// every call: a ports.Password (Atlassian account email and API
	// token), a ports.Secret (an API token, with Config.Email) or a
	// ports.OAuth2 (an OAuth 2.0 (3LO) access token, with Config.CloudID).
	CredentialPurposeAPI = string(model.TypeJiraIssues) + ".api"
	// CredentialPurposeWebhook is the webhook secret, a ports.Secret,
	// resolved for every webhook request.
	CredentialPurposeWebhook = string(model.TypeJiraIssues) + ".webhook" // #nosec G101 -- purpose name, not a credential
)

// maxResponseBytes bounds a Jira API response body.
const maxResponseBytes = 8 << 20

// defaultRateWait is the wait asked for when a rate limit answer states
// none.
const defaultRateWait = time.Minute

// answer is a Jira response the adapter reads.
type answer struct {
	status int
	header http.Header
	body   []byte
}

// callKind is what a provider call does: a read, a comment creation, a
// message mutation (editing or deleting a comment) or the read a lost
// mutation is settled with.
type callKind int

const (
	readCall callKind = iota
	commentCall
	mutateCall
)

func (k callKind) write() bool { return k == commentCall || k == mutateCall }

// base resolves the API root for the credential in use.
func (x *Instance) base(oauth bool) (string, error) {
	switch {
	case x.baseOverride != "":
		return x.baseOverride, nil
	case oauth && x.cfg.CloudID == "":
		return "", &model.Error{Code: model.Validation, SafeMessage: "an OAuth 2.0 credential needs configuration cloud_id", Retry: model.NoRetry}
	case oauth:
		return GatewayURL + x.cfg.CloudID, nil
	}
	return x.cfg.site(), nil
}

// authorization resolves the API credential into an Authorization value and
// whether it is an OAuth 2.0 token (routed through the gateway).
func (x *Instance) authorization(ctx context.Context, fresh bool) (string, bool, error) {
	c, err := x.creds.Resolve(ctx, ports.CredentialRequest{
		Channel: x.channel.Ref, Reference: x.channel.CredentialRef, Purpose: CredentialPurposeAPI,
		Accept: []ports.CredentialKind{ports.KindPassword, ports.KindSecret, ports.KindOAuth2}, Fresh: fresh,
	})
	if err != nil {
		return "", false, err
	}
	if exp := c.Expiry(); !exp.IsZero() && !x.clock.Now().Before(exp) {
		if !fresh {
			return x.authorization(ctx, true)
		}
		return "", false, &model.Error{Code: model.Unauthorized, SafeMessage: "Jira credential expired", Retry: model.NoRetry}
	}
	switch v := c.(type) {
	case ports.Password:
		if v.User != "" && len(v.Secret) > 0 {
			return basic(v.User, v.Secret), false, nil
		}
	case ports.Secret:
		if len(v.Bytes) > 0 {
			if x.cfg.Email == "" {
				return "", false, &model.Error{Code: model.Validation, SafeMessage: "an API token as a bare secret needs configuration email", Retry: model.NoRetry}
			}
			return basic(x.cfg.Email, v.Bytes), false, nil
		}
	case ports.OAuth2:
		if v.AccessToken != "" {
			return "Bearer " + v.AccessToken, true, nil
		}
	default:
		return "", false, &model.Error{Code: model.Unsupported, SafeMessage: "credential kind not usable for the Jira API", Retry: model.NoRetry}
	}
	return "", false, &model.Error{Code: model.Unauthorized, SafeMessage: "Jira credential is empty", Retry: model.NoRetry}
}

func basic(user string, secret []byte) string {
	raw := make([]byte, 0, len(user)+1+len(secret))
	raw = append(append(append(raw, user...), ':'), secret...)
	return "Basic " + base64.StdEncoding.EncodeToString(raw)
}

// send runs one request, re-resolving the credential once with Fresh set
// when Jira answers 401: nothing was applied, so the retry is safe. A
// transport failure is returned as is; a status is for the caller to map.
func (x *Instance) send(ctx context.Context, method, path string, query url.Values, body any) (answer, error) {
	a, err := x.attempt(ctx, false, method, path, query, body)
	if err == nil && a.status == http.StatusUnauthorized {
		return x.attempt(ctx, true, method, path, query, body)
	}
	return a, err
}

func (x *Instance) attempt(ctx context.Context, fresh bool, method, path string, query url.Values, body any) (answer, error) {
	auth, oauth, err := x.authorization(ctx, fresh)
	if err != nil {
		return answer{}, err
	}
	root, err := x.base(oauth)
	if err != nil {
		return answer{}, err
	}
	u := root + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return answer{}, final(model.Validation, "request body does not encode")
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return answer{}, final(model.Permanent, "request does not build")
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := x.http.Do(req)
	if err != nil {
		return answer{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, rerr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if rerr != nil && resp.StatusCode/100 == 2 {
		return answer{}, rerr
	}
	return answer{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

// failure maps a failed call: err is a transport failure or a typed error,
// or a (non-2xx) status. A definite refusal of the effect is an Outcome in
// state rejected; every other failure is a *model.Error. A write changes
// provider state, so an ambiguous failure is outcome unknown; a read is
// simply retried.
func (x *Instance) failure(ctx context.Context, a answer, err error, kind callKind) (adapter.Outcome, error) {
	if err != nil {
		if me, ok := errors.AsType[*model.Error](err); ok {
			return adapter.Outcome{}, me
		}
		if ctx.Err() != nil {
			return adapter.Outcome{}, ctx.Err()
		}
		if kind.write() && !dialFailure(err) {
			return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Jira answer lost; reconcile before retrying")
		}
		return adapter.Outcome{}, model.NewError(model.Transient, "Jira unreachable")
	}
	write := kind.write()
	switch c := a.status; {
	case c == http.StatusBadRequest || c == http.StatusRequestEntityTooLarge:
		// 400 is also Jira's answer to a user who may not edit or delete
		// the comment; 413 is the per-issue comment limit.
		return rejected(model.Validation, "Jira rejected the change"), nil
	case c == http.StatusUnauthorized:
		return adapter.Outcome{}, final(model.Unauthorized, "Jira refused the credential")
	case c == http.StatusTooManyRequests:
		return adapter.Outcome{}, x.rateLimited(a.header)
	case c == http.StatusForbidden && kind == mutateCall:
		return rejected(model.Unauthorized, "Jira denied the operation"), nil
	case c == http.StatusForbidden:
		return adapter.Outcome{}, final(model.Unauthorized, "Jira denied the operation")
	case c == http.StatusMethodNotAllowed && kind == mutateCall:
		// Documented as the answer to an anonymous call.
		return rejected(model.Unauthorized, "Jira refused an anonymous call"), nil
	case c == http.StatusNotFound && kind == mutateCall:
		// The comment may be gone while the issue exists: no binding.
		return rejected(model.NotFound, "target not found or inaccessible"), nil
	case c == http.StatusNotFound && write:
		// Jira answers 404 for an issue that is gone and for one the
		// credential may not browse.
		return adapter.Outcome{State: model.Rejected, Failure: final(model.NotFound, "issue not found or inaccessible"), Binding: model.WorkItemBindingInaccessible}, nil
	case c == http.StatusNotFound:
		return adapter.Outcome{}, final(model.NotFound, "issue or comment not found or inaccessible")
	case c >= 500 && write:
		// Comment creation has no idempotency key: a server error after
		// the body was sent may still have created the comment.
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Jira server error; reconcile before retrying")
	case c >= 500:
		return adapter.Outcome{}, model.NewError(model.Transient, "Jira server error")
	}
	return adapter.Outcome{}, final(model.Permanent, "unexpected Jira answer")
}

func rejected(code model.ErrorCode, msg string) adapter.Outcome {
	return adapter.Outcome{State: model.Rejected, Failure: final(code, msg)}
}

// rateLimited maps a 429: Retry-After seconds, else the X-RateLimit-Reset
// ISO 8601 time, else one minute.
func (x *Instance) rateLimited(h http.Header) *model.Error {
	wait := defaultRateWait
	if s, err := strconv.ParseInt(strings.TrimSpace(h.Get("Retry-After")), 10, 64); err == nil && s >= 0 {
		wait = max(time.Duration(s)*time.Second, time.Second)
	} else if t, err := time.Parse(time.RFC3339, strings.TrimSpace(h.Get("X-RateLimit-Reset"))); err == nil {
		wait = max(t.Sub(x.clock.Now()), time.Second)
	}
	return &model.Error{Code: model.RateLimited, SafeMessage: "Jira rate limit", Retry: model.RetrySameOperation, RetryAfter: wait}
}

func final(code model.ErrorCode, msg string) *model.Error {
	return &model.Error{Code: code, SafeMessage: msg, Retry: model.NoRetry}
}

// readError is the error of a failed read.
func (x *Instance) readError(ctx context.Context, a answer, err error) error {
	out, ferr := x.failure(ctx, a, err, readCall)
	if ferr == nil {
		ferr = out.Failure
	}
	return ferr
}

// dialFailure reports a connection that was never established, by a
// failed dial or TLS handshake: the request provably never reached Jira.
func dialFailure(err error) bool {
	if op, ok := errors.AsType[*net.OpError](err); ok && op.Op == "dial" {
		return true
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return true
	}
	if _, ok := errors.AsType[tls.RecordHeaderError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.HostnameError](err); ok {
		return true
	}
	_, ok := errors.AsType[x509.CertificateInvalidError](err)
	return ok
}
