package gitlab

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// defaultRateWait is the wait used when a rate limit answer states none.
const defaultRateWait = time.Minute

// maxAnswer bounds the bytes read from one provider answer.
const maxAnswer = 16 << 20

// answer is one provider answer.
type answer struct {
	status int
	header http.Header
	body   []byte
}

func (a *answer) ok() bool { return a != nil && a.status >= 200 && a.status < 300 }

// ready checks that the instance can call the provider.
func (x *Instance) ready() error {
	switch {
	case x.isClosed():
		return model.NewError(model.Unavailable, "instance closed")
	case x.client == nil:
		return model.NewError(model.Unavailable, "no HTTP client injected")
	case x.clock == nil:
		return model.NewError(model.Unavailable, "no clock injected")
	case x.creds == nil:
		return &model.Error{Code: model.Unavailable, SafeMessage: "no credentials port", Retry: model.NoRetry}
	}
	return nil
}

// token resolves the API token for one call.
func (x *Instance) token(ctx context.Context, fresh bool) (string, error) {
	c, err := x.creds.Resolve(ctx, ports.CredentialRequest{
		Channel: x.channel.Ref, Reference: x.channel.CredentialRef, Purpose: CredentialPurposeAPI,
		Accept: []ports.CredentialKind{ports.KindOAuth2, ports.KindSecret}, Fresh: fresh,
	})
	if err != nil {
		return "", err
	}
	var tok string
	switch c := c.(type) {
	case ports.OAuth2:
		tok = c.AccessToken
	case ports.Secret:
		tok = string(c.Bytes)
	default:
		return "", &model.Error{Code: model.Unsupported, SafeMessage: "credential kind not usable for the GitLab API", Retry: model.NoRetry}
	}
	if exp := c.Expiry(); tok == "" || (!exp.IsZero() && !x.clock.Now().Before(exp)) {
		if !fresh {
			return x.token(ctx, true)
		}
		return "", &model.Error{Code: model.Unauthorized, SafeMessage: "GitLab credential empty or expired", Retry: model.NoRetry}
	}
	return tok, nil
}

// spec is one provider request: path under the API root, query and an
// optional JSON body.
type spec struct {
	method string
	path   string
	query  url.Values
	body   any
}

// call runs s with a resolved token, re-resolving once with Fresh set when
// GitLab answers 401: nothing was applied, so the retry is safe. A
// transport failure is the error; any answer, even a failed status, is the
// answer.
func (x *Instance) call(ctx context.Context, s spec) (*answer, error) {
	tok, err := x.token(ctx, false)
	if err != nil {
		return nil, err
	}
	a, err := x.do(ctx, tok, s)
	if err != nil || a.status != http.StatusUnauthorized {
		return a, err
	}
	if tok, err = x.token(ctx, true); err != nil {
		return nil, err
	}
	return x.do(ctx, tok, s)
}

func (x *Instance) do(ctx context.Context, tok string, s spec) (*answer, error) {
	u := x.cfg.apiURL() + s.path
	if len(s.query) > 0 {
		u += "?" + s.query.Encode()
	}
	var body io.Reader
	if s.body != nil {
		b, err := json.Marshal(s.body)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, s.method, u, body)
	if err != nil {
		return nil, final(model.Validation, "invalid request")
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := x.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return nil, err
	}
	return &answer{status: resp.StatusCode, header: resp.Header, body: b}, nil
}

// callKind is what a provider call does: a read, a write creating a note
// or updating an issue, a write changing a note (editing or deleting), or
// the read a mutation does first.
type callKind int

const (
	readCall callKind = iota
	noteCall
	issueCall
	mutateCall
	lookupCall
)

func (k callKind) write() bool { return k == noteCall || k == issueCall || k == mutateCall }

// mutation reports a call made for a message mutation: GitLab's refusal of
// it (403, or 404 for a target that is gone) is a final rejected outcome.
func (k callKind) mutation() bool { return k == mutateCall || k == lookupCall }

// failure maps a failed provider call: a transport error (err) or a
// non-2xx answer. A write changes provider state, so an ambiguous failure
// is outcome unknown; a read is simply retried. A definite refusal of the
// effect is an Outcome with state rejected; every other failure is a
// *model.Error.
func (x *Instance) failure(ctx context.Context, a *answer, err error, kind callKind) (adapter.Outcome, error) {
	if err != nil {
		if me, ok := errors.AsType[*model.Error](err); ok {
			return adapter.Outcome{}, me
		}
		if ctx.Err() != nil {
			return adapter.Outcome{}, ctx.Err()
		}
		if kind.write() && !dialFailure(err) {
			return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "GitLab answer lost; reconcile before retrying")
		}
		return adapter.Outcome{}, model.NewError(model.Transient, "GitLab unreachable")
	}
	write := kind.write()
	switch c := a.status; {
	case c == http.StatusBadRequest || c == http.StatusUnprocessableEntity:
		f := final(model.Validation, "GitLab rejected the note")
		if kind == issueCall {
			f = final(model.Validation, "GitLab rejected the issue update")
		}
		if kind.mutation() {
			f = final(model.Validation, "GitLab rejected the change")
		}
		return adapter.Outcome{State: model.Rejected, Failure: f}, nil
	case c == http.StatusUnauthorized:
		return adapter.Outcome{}, final(model.Unauthorized, "GitLab refused the credential")
	case c == http.StatusTooManyRequests:
		return adapter.Outcome{}, rateLimited(x.rateWait(a.header))
	case c == http.StatusForbidden && (kind == issueCall || kind.mutation()):
		return adapter.Outcome{State: model.Rejected, Failure: final(model.Unauthorized, "GitLab denied the operation")}, nil
	case c == http.StatusForbidden:
		return adapter.Outcome{}, final(model.Unauthorized, "GitLab denied the operation")
	case c == http.StatusNotFound && kind.mutation():
		return adapter.Outcome{State: model.Rejected, Failure: final(model.NotFound, "target not found or inaccessible")}, nil
	case c == http.StatusNotFound:
		// GitLab answers 404 for an issue the token cannot see as well.
		return lost(final(model.NotFound, "issue not found or inaccessible"), model.WorkItemBindingInaccessible, write)
	case c == http.StatusMethodNotAllowed || (c >= 300 && c < 400):
		// A renamed or transferred project: GET follows a redirect the
		// adapter does not, other methods answer 405. The route names the
		// project by ID, so this is a stale or foreign route.
		return lost(final(model.NotFound, "project moved"), model.WorkItemBindingInaccessible, write)
	case c >= 500 && write:
		// No idempotency key on note creation: a server error after the
		// body was sent may still have created the note, or applied the
		// issue update.
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "GitLab server error; reconcile before retrying")
	case c >= 500:
		return adapter.Outcome{}, model.NewError(model.Transient, "GitLab server error")
	}
	return adapter.Outcome{}, final(model.Permanent, "unexpected GitLab answer")
}

// readFailure is the error of a failed read.
func (x *Instance) readFailure(ctx context.Context, a *answer, err error) error {
	out, ferr := x.failure(ctx, a, err, readCall)
	if ferr == nil {
		ferr = out.Failure
	}
	return ferr
}

// rateWait is the wait a rate limit answer states: Retry-After seconds,
// else the RateLimit-Reset epoch, else one minute.
func (x *Instance) rateWait(h http.Header) time.Duration {
	if s, err := strconv.ParseInt(h.Get("Retry-After"), 10, 64); err == nil && s >= 0 {
		return max(time.Duration(s)*time.Second, time.Second)
	}
	if s, err := strconv.ParseInt(h.Get("RateLimit-Reset"), 10, 64); err == nil && s > 0 {
		return max(time.Unix(s, 0).Sub(x.clock.Now()), time.Second)
	}
	return defaultRateWait
}

// lost answers a refusal showing the issue gone: a rejected write that
// names the work item binding state it shows, or the read's error.
func lost(f *model.Error, binding string, write bool) (adapter.Outcome, error) {
	if !write {
		return adapter.Outcome{}, f
	}
	return adapter.Outcome{State: model.Rejected, Failure: f, Binding: binding}, nil
}

func rateLimited(wait time.Duration) *model.Error {
	return &model.Error{Code: model.RateLimited, SafeMessage: "GitLab rate limit", Retry: model.RetrySameOperation, RetryAfter: wait}
}

func final(code model.ErrorCode, msg string) *model.Error {
	return &model.Error{Code: code, SafeMessage: msg, Retry: model.NoRetry}
}

// dialFailure reports a connection that was never established, by a failed
// dial or TLS handshake: the request provably never reached GitLab.
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
