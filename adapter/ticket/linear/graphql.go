package linear

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
	"strconv"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// defaultRateWait is the wait used when a rate limit answer states none.
const defaultRateWait = time.Minute

// maxAnswerBytes bounds the answer body the adapter reads.
const maxAnswerBytes = 4 << 20

// gqlError is one entry of a GraphQL response's errors array.
type gqlError struct {
	Message    string         `json:"message"`
	Extensions map[string]any `json:"extensions"`
}

func (e gqlError) code() string {
	for _, k := range []string{"code", "type"} {
		if s, ok := e.Extensions[k].(string); ok && s != "" {
			return strings.ToUpper(strings.ReplaceAll(s, " ", "_"))
		}
	}
	return ""
}

// answer is what Linear's endpoint returned for one request.
type answer struct {
	status int
	header http.Header
	errs   []gqlError
}

func (a *answer) has(codes ...string) bool {
	for _, e := range a.errs {
		for _, c := range codes {
			if e.code() == c {
				return true
			}
		}
	}
	return false
}

func (a *answer) notFound() bool {
	for _, e := range a.errs {
		if strings.Contains(strings.ToLower(e.Message), "not found") {
			return true
		}
	}
	return false
}

func (a *answer) rateLimited() bool {
	return a.status == http.StatusTooManyRequests || a.has("RATELIMITED")
}

func (a *answer) unauthorized() bool {
	return a.status == http.StatusUnauthorized || a.has("AUTHENTICATION_ERROR")
}

// ready checks that the instance can call Linear.
func (x *Instance) ready() error {
	switch {
	case x.isClosed():
		return model.NewError(model.Unavailable, "instance closed")
	case x.http == nil:
		return model.NewError(model.Unavailable, "no HTTP client injected")
	case x.clock == nil:
		return model.NewError(model.Unavailable, "no clock injected")
	case x.creds == nil:
		return &model.Error{Code: model.Unavailable, SafeMessage: "no credentials port", Retry: model.NoRetry}
	}
	return nil
}

// authorization returns the Authorization header value for one call: a
// personal API key (ports.Secret) is sent as is, an OAuth access token
// (ports.OAuth2) as a bearer token.
func (x *Instance) authorization(ctx context.Context, fresh bool) (string, error) {
	c, err := x.creds.Resolve(ctx, ports.CredentialRequest{
		Channel: x.channel.Ref, Reference: x.channel.CredentialRef, Purpose: CredentialPurposeAPI,
		Accept: []ports.CredentialKind{ports.KindOAuth2, ports.KindSecret}, Fresh: fresh,
	})
	if err != nil {
		return "", err
	}
	var value string
	switch c := c.(type) {
	case ports.OAuth2:
		if c.AccessToken != "" {
			value = "Bearer " + c.AccessToken
		}
	case ports.Secret:
		value = string(c.Bytes)
	default:
		return "", &model.Error{Code: model.Unsupported, SafeMessage: "credential kind not usable for the Linear API", Retry: model.NoRetry}
	}
	if exp := c.Expiry(); value == "" || (!exp.IsZero() && !x.clock.Now().Before(exp)) {
		if !fresh {
			return x.authorization(ctx, true)
		}
		return "", &model.Error{Code: model.Unauthorized, SafeMessage: "Linear credential empty or expired", Retry: model.NoRetry}
	}
	return value, nil
}

// exec sends one GraphQL operation and decodes the answer's data into out.
// It re-resolves the credential once with Fresh set when Linear refuses it:
// nothing was applied then. A transport failure is returned as is; a Linear
// answer, whatever its status, is returned as the answer.
func (x *Instance) exec(ctx context.Context, query string, vars map[string]any, out any) (*answer, error) {
	auth, err := x.authorization(ctx, false)
	if err != nil {
		return nil, err
	}
	a, err := x.post(ctx, auth, query, vars, out)
	if err != nil || !a.unauthorized() {
		return a, err
	}
	if auth, err = x.authorization(ctx, true); err != nil {
		return nil, err
	}
	return x.post(ctx, auth, query, vars, out)
}

func (x *Instance) post(ctx context.Context, auth, query string, vars map[string]any, out any) (*answer, error) {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, x.cfg.apiURL(), bytes.NewReader(body))
	if err != nil {
		return nil, model.NewError(model.Validation, "invalid api_url")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", auth)
	resp, err := x.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
	if err != nil {
		return nil, err
	}
	a := &answer{status: resp.StatusCode, header: resp.Header}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}
	if json.Unmarshal(raw, &env) == nil {
		a.errs = env.Errors
		if out != nil && len(env.Data) > 0 && string(env.Data) != "null" && len(env.Errors) == 0 && resp.StatusCode == http.StatusOK {
			if err := json.Unmarshal(env.Data, out); err != nil {
				return a, model.NewError(model.Permanent, "unexpected Linear answer")
			}
		}
	}
	return a, nil
}

// failed reports whether the answer is not a clean success.
func (a *answer) failed() bool { return a.status != http.StatusOK || len(a.errs) > 0 }

// callKind is what a provider call does: a read, a write creating a
// comment, a write changing one, or the read a mutation does first.
type callKind int

const (
	readCall callKind = iota
	createCall
	mutateCall
	lookupCall
)

func (k callKind) write() bool { return k == createCall || k == mutateCall }

// failure maps a failed provider call. A write changes provider state, so an
// ambiguous failure is outcome unknown; a read is simply retried. A definite
// refusal of the effect is an Outcome with state rejected; every other
// failure is a *model.Error. a is nil for a transport failure (err).
func (x *Instance) failure(ctx context.Context, a *answer, err error, kind callKind) (adapter.Outcome, error) {
	if me, ok := errors.AsType[*model.Error](err); ok {
		return adapter.Outcome{}, me
	}
	if ctx.Err() != nil {
		return adapter.Outcome{}, ctx.Err()
	}
	if a == nil {
		if kind.write() && !dialFailure(err) {
			return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Linear answer lost; reconcile before retrying")
		}
		return adapter.Outcome{}, model.NewError(model.Transient, "Linear unreachable")
	}
	switch {
	case a.rateLimited():
		return adapter.Outcome{}, rateLimited(x.rateWait(a.header))
	case a.unauthorized():
		return adapter.Outcome{}, final(model.Unauthorized, "Linear refused the credential")
	case a.status >= 500 && kind.write():
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Linear server error; reconcile before retrying")
	case a.status >= 500:
		return adapter.Outcome{}, model.NewError(model.Transient, "Linear server error")
	case a.notFound():
		return lost(final(model.NotFound, "target not found or inaccessible"), kind)
	case a.status == http.StatusForbidden || a.has("FORBIDDEN"):
		if kind.write() || kind == lookupCall {
			return adapter.Outcome{State: model.Rejected, Failure: final(model.Unauthorized, "Linear denied the operation")}, nil
		}
		return adapter.Outcome{}, final(model.Unauthorized, "Linear denied the operation")
	case a.status == http.StatusBadRequest || a.status == http.StatusOK || a.has("INVALID_INPUT"):
		// A GraphQL error answer: the operation did not run.
		f := final(model.Validation, "Linear rejected the operation")
		if kind.write() || kind == lookupCall {
			return adapter.Outcome{State: model.Rejected, Failure: f}, nil
		}
		return adapter.Outcome{}, f
	}
	return adapter.Outcome{}, final(model.Permanent, "unexpected Linear answer")
}

// lost answers a refusal showing the target gone: a rejected write, or the
// read's error. A comment write that Linear cannot find the issue for names
// the work item binding state it shows.
func lost(f *model.Error, kind callKind) (adapter.Outcome, error) {
	switch kind {
	case createCall:
		return adapter.Outcome{State: model.Rejected, Failure: f, Binding: model.WorkItemBindingInaccessible}, nil
	case mutateCall, lookupCall:
		return adapter.Outcome{State: model.Rejected, Failure: f}, nil
	}
	return adapter.Outcome{}, f
}

// readFailure is the error of a failed read.
func (x *Instance) readFailure(ctx context.Context, a *answer, err error) error {
	out, ferr := x.failure(ctx, a, err, readCall)
	if ferr == nil {
		ferr = out.Failure
	}
	return ferr
}

// rateWait is the wait a rate limit answer states: Retry-After seconds, else
// the latest reset (UTC epoch milliseconds) of the request or complexity
// quota that is exhausted, else a minute.
func (x *Instance) rateWait(h http.Header) time.Duration {
	if s, err := strconv.ParseInt(h.Get("Retry-After"), 10, 64); err == nil && s >= 0 {
		return max(time.Duration(s)*time.Second, time.Second)
	}
	var reset int64
	for _, q := range []string{"Requests", "Complexity"} {
		if h.Get("X-RateLimit-"+q+"-Remaining") != "0" {
			continue
		}
		if ms, err := strconv.ParseInt(h.Get("X-RateLimit-"+q+"-Reset"), 10, 64); err == nil && ms > reset {
			reset = ms
		}
	}
	if reset > 0 {
		return max(time.UnixMilli(reset).Sub(x.clock.Now()), time.Second)
	}
	return defaultRateWait
}

func rateLimited(wait time.Duration) *model.Error {
	return &model.Error{Code: model.RateLimited, SafeMessage: "Linear rate limit", Retry: model.RetrySameOperation, RetryAfter: wait}
}

func final(code model.ErrorCode, msg string) *model.Error {
	return &model.Error{Code: code, SafeMessage: msg, Retry: model.NoRetry}
}

// dialFailure reports a connection that was never established, by a failed
// dial or TLS handshake: the request provably never reached Linear.
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
