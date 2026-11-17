package github

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	gh "github.com/google/go-github/v92/github"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// defaultRateWait is the wait GitHub asks for when a rate limit answer
// states none.
const defaultRateWait = time.Minute

type tokenKey struct{}

// tokenTransport sends a request through the injected client with the
// bearer token of its call, carried in the request context. The token never
// lives on the instance.
type tokenTransport struct{ next ports.HTTPClient }

func (t tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	if tok, ok := req.Context().Value(tokenKey{}).(string); ok && tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	return t.next.Do(r)
}

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

// authorized returns ctx carrying the API token, resolved for this call.
func (x *Instance) authorized(ctx context.Context, fresh bool) (context.Context, error) {
	c, err := x.creds.Resolve(ctx, ports.CredentialRequest{
		Channel: x.channel.Ref, Reference: x.channel.CredentialRef, Purpose: CredentialPurposeAPI,
		Accept: []ports.CredentialKind{ports.KindOAuth2, ports.KindSecret}, Fresh: fresh,
	})
	if err != nil {
		return nil, err
	}
	var tok string
	switch c := c.(type) {
	case ports.OAuth2:
		tok = c.AccessToken
	case ports.Secret:
		tok = string(c.Bytes)
	default:
		return nil, &model.Error{Code: model.Unsupported, SafeMessage: "credential kind not usable for the GitHub API", Retry: model.NoRetry}
	}
	if exp := c.Expiry(); tok == "" || (!exp.IsZero() && !x.clock.Now().Before(exp)) {
		if !fresh {
			return x.authorized(ctx, true)
		}
		return nil, &model.Error{Code: model.Unauthorized, SafeMessage: "GitHub credential empty or expired", Retry: model.NoRetry}
	}
	return context.WithValue(ctx, tokenKey{}, tok), nil
}

// call runs fn with a resolved token, re-resolving once with Fresh set when
// GitHub answers 401: nothing was applied, so the retry is safe.
func call[T any](ctx context.Context, x *Instance, fn func(context.Context) (T, *gh.Response, error)) (T, *gh.Response, error) {
	var zero T
	actx, err := x.authorized(ctx, false)
	if err != nil {
		return zero, nil, err
	}
	v, resp, err := fn(actx)
	if status(resp) != http.StatusUnauthorized {
		return v, resp, err
	}
	if actx, err = x.authorized(ctx, true); err != nil {
		return zero, nil, err
	}
	return fn(actx)
}

func status(resp *gh.Response) int {
	if resp == nil || resp.Response == nil {
		return 0
	}
	return resp.StatusCode
}

// callKind is what a provider call does: a read, a write creating a
// comment or updating an issue, a write changing a message (editing or
// deleting a comment, adding or removing a reaction), or the read a
// mutation does first.
type callKind int

const (
	readCall callKind = iota
	commentCall
	issueCall
	mutateCall
	lookupCall
)

func (k callKind) write() bool { return k == commentCall || k == issueCall || k == mutateCall }

// mutation reports a call made for a message mutation: GitHub's refusal of
// it (403, or 404 and 410 for a target that is gone) is a final rejected
// outcome.
func (k callKind) mutation() bool { return k == mutateCall || k == lookupCall }

// failure maps a failed provider call. A write changes provider state, so
// an ambiguous failure is outcome unknown; a read is simply retried. A
// definite refusal of the effect is an Outcome with state rejected; every
// other failure is a *model.Error.
func (x *Instance) failure(ctx context.Context, resp *gh.Response, err error, kind callKind) (adapter.Outcome, error) {
	if me, ok := errors.AsType[*model.Error](err); ok {
		return adapter.Outcome{}, me
	}
	if ctx.Err() != nil {
		return adapter.Outcome{}, ctx.Err()
	}
	if rl, ok := errors.AsType[*gh.RateLimitError](err); ok {
		// Also go-github's client-side short-circuit: nothing was sent.
		wait := x.waitUntil(rl.Rate.Reset.Time)
		if rl.Response != nil {
			wait = x.rateWait(rl.Response.Header, wait)
		}
		return adapter.Outcome{}, rateLimited(wait)
	}
	if ab, ok := errors.AsType[*gh.AbuseRateLimitError](err); ok {
		var h http.Header
		if ab.Response != nil {
			h = ab.Response.Header
		}
		return adapter.Outcome{}, rateLimited(x.rateWait(h, defaultRateWait))
	}
	if _, ok := errors.AsType[*gh.RedirectionError](err); ok {
		return adapter.Outcome{}, final(model.NotFound, "issue moved")
	}
	if er, ok := errors.AsType[*gh.ErrorResponse](err); ok && er.Response != nil {
		return x.statusFailure(er.Response, kind)
	}
	if kind.write() && !dialFailure(err) {
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "GitHub answer lost; reconcile before retrying")
	}
	return adapter.Outcome{}, model.NewError(model.Transient, "GitHub unreachable")
}

// statusFailure maps a GitHub error answer. A 403 without rate limit
// headers refuses an issue update or a message mutation as a rejected
// outcome, and any other call with an unauthorized error. A 404 or 410 on
// a message mutation rejects it as not_found, without a work item binding:
// the issue may well exist.
func (x *Instance) statusFailure(r *http.Response, kind callKind) (adapter.Outcome, error) {
	write := kind.write()
	switch c := r.StatusCode; {
	case c == http.StatusBadRequest || c == http.StatusUnprocessableEntity:
		f := final(model.Validation, "GitHub rejected the comment")
		if kind == issueCall {
			f = final(model.Validation, "GitHub rejected the issue update")
		}
		if kind.mutation() {
			f = final(model.Validation, "GitHub rejected the change")
		}
		return adapter.Outcome{State: model.Rejected, Failure: f}, nil
	case c == http.StatusUnauthorized:
		return adapter.Outcome{}, final(model.Unauthorized, "GitHub refused the credential")
	case c == http.StatusTooManyRequests,
		c == http.StatusForbidden && (r.Header.Get("Retry-After") != "" || r.Header.Get("X-RateLimit-Remaining") == "0"):
		return adapter.Outcome{}, rateLimited(x.rateWait(r.Header, defaultRateWait))
	case c == http.StatusForbidden && (kind == issueCall || kind.mutation()):
		return adapter.Outcome{State: model.Rejected, Failure: final(model.Unauthorized, "GitHub denied the operation")}, nil
	case (c == http.StatusNotFound || c == http.StatusGone) && kind.mutation():
		return adapter.Outcome{State: model.Rejected, Failure: final(model.NotFound, "target not found or inaccessible")}, nil
	case c == http.StatusForbidden:
		return adapter.Outcome{}, final(model.Unauthorized, "GitHub denied the operation")
	case c == http.StatusNotFound:
		return lost(final(model.NotFound, "issue not found or inaccessible"), model.WorkItemBindingInaccessible, write)
	case c == http.StatusGone:
		return lost(final(model.NotFound, "issue deleted"), model.WorkItemBindingDeleted, write)
	case c >= 500 && write:
		// No idempotency key on comment creation: a server error after
		// the body was sent may still have created the comment, or
		// applied the issue update.
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "GitHub server error; reconcile before retrying")
	case c >= 500:
		return adapter.Outcome{}, model.NewError(model.Transient, "GitHub server error")
	}
	return adapter.Outcome{}, final(model.Permanent, "unexpected GitHub answer")
}

// rateWait is the wait a rate limit answer states: Retry-After seconds,
// else the x-ratelimit-reset epoch when no requests remain, else fallback.
func (x *Instance) rateWait(h http.Header, fallback time.Duration) time.Duration {
	if s, err := strconv.ParseInt(h.Get("Retry-After"), 10, 64); err == nil && s >= 0 {
		return max(time.Duration(s)*time.Second, time.Second)
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if s, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil && s > 0 {
			return x.waitUntil(time.Unix(s, 0))
		}
	}
	return fallback
}

func (x *Instance) waitUntil(t time.Time) time.Duration {
	if t.IsZero() {
		return defaultRateWait
	}
	return max(t.Sub(x.clock.Now()), time.Second)
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
	return &model.Error{Code: model.RateLimited, SafeMessage: "GitHub rate limit", Retry: model.RetrySameOperation, RetryAfter: wait}
}

func final(code model.ErrorCode, msg string) *model.Error {
	return &model.Error{Code: code, SafeMessage: msg, Retry: model.NoRetry}
}

// dialFailure reports a connection that was never established, by a
// failed dial or TLS handshake: the request provably never reached GitHub.
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
