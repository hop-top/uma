package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// maxResponseBytes bounds what the adapter reads of one Web API answer: a
// page of history is the largest.
const maxResponseBytes = 8 << 20

// defaultRateWait is the wait asked for when a rate limit answer states
// none.
const defaultRateWait = time.Minute

// apiError is a Web API answer with "ok": false and an error code: "Callers
// should always check the value of the ok parameter in the response"
// (https://docs.slack.dev/reference/methods/chat.postMessage/). The
// references do not state the HTTP status of such an answer; the adapter
// reads the body of a 200 and treats any other status as a failure of its own
// kind.
type apiError struct{ code string }

func (e *apiError) Error() string { return "slack: " + e.code }

// statusError is an HTTP answer other than 200.
type statusError struct {
	status int
	// retryAfter is the Retry-After wait of a 429, zero when unstated.
	retryAfter time.Duration
}

func (e *statusError) Error() string { return "slack: HTTP " + strconv.Itoa(e.status) }

// transportError is a request that failed before an answer: refused,
// dropped or timed out.
type transportError struct{ err error }

func (e *transportError) Error() string { return "slack: transport failure" }
func (e *transportError) Unwrap() error { return e.err }

// ready checks that the instance can call the provider.
func (x *Instance) ready() error {
	switch {
	case x.isClosed():
		return model.NewError(model.Unavailable, "instance closed")
	case x.http == nil:
		return model.NewError(model.Unavailable, "no HTTP client injected")
	case x.creds == nil:
		return &model.Error{Code: model.Unavailable, SafeMessage: "no credentials port", Retry: model.NoRetry}
	}
	return nil
}

// token resolves the bot token for one call. It lives only in that call's
// request header.
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
		return "", final(model.Unsupported, "credential kind not usable for the Slack API")
	}
	if exp := c.Expiry(); tok == "" || (!exp.IsZero() && !x.now().Before(exp)) {
		if !fresh {
			return x.token(ctx, true)
		}
		return "", final(model.Unauthorized, "Slack credential empty or expired")
	}
	return tok, nil
}

// call runs one Web API method with a JSON body and decodes its answer into
// out (which may be nil). A token Slack refuses as invalid or expired is
// resolved again once, fresh: nothing was applied, so the retry is safe.
func (x *Instance) call(ctx context.Context, method string, args, out any) error {
	return x.callWith(ctx, x.token, method, args, out)
}

// callWith is call with the token resolve gives.
func (x *Instance) callWith(ctx context.Context, resolve func(context.Context, bool) (string, error), method string, args, out any) error {
	tok, err := resolve(ctx, false)
	if err != nil {
		return err
	}
	err = x.post(ctx, method, tok, args, out)
	if ae, ok := errors.AsType[*apiError](err); ok && (ae.code == "invalid_auth" || ae.code == "token_expired") {
		if tok, err = resolve(ctx, true); err != nil {
			return err
		}
		return x.post(ctx, method, tok, args, out)
	}
	return err
}

func (x *Instance) post(ctx context.Context, method, token string, args, out any) error {
	body, err := json.Marshal(args)
	if err != nil {
		return final(model.Validation, "request is not encodable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, x.cfg.apiURL()+method, bytes.NewReader(body))
	if err != nil {
		return final(model.Validation, "invalid api_url")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json")
	res, err := x.http.Do(req)
	if err != nil {
		return &transportError{err: err}
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return &transportError{err: err}
	}
	if res.StatusCode != http.StatusOK {
		return &statusError{status: res.StatusCode, retryAfter: retryAfter(res.Header)}
	}
	var head struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return &transportError{err: err}
	}
	if !head.OK {
		return &apiError{code: head.Error}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return &transportError{err: err}
		}
	}
	return nil
}

// retryAfter reads Retry-After as whole seconds: "a Retry-After HTTP header
// containing the number of seconds until you can retry"
// (https://docs.slack.dev/apis/web-api/rate-limits/).
func retryAfter(h http.Header) time.Duration {
	if s, err := strconv.ParseInt(strings.TrimSpace(h.Get("Retry-After")), 10, 64); err == nil && s >= 0 {
		return max(time.Duration(s)*time.Second, time.Second)
	}
	return 0
}
