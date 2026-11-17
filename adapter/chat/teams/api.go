package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// maxResponseBytes bounds what the adapter reads of one Connector answer.
const maxResponseBytes = 1 << 20

// defaultRateWait is the wait asked for when a 429 states none.
const defaultRateWait = time.Minute

// statusError is a Connector answer other than 2xx, with the error code
// of its ErrorResponse body when it carries one
// (https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-api-reference#errorresponse-object).
type statusError struct {
	status int
	code   string
	// retryAfter is the Retry-After wait of a 429, zero when unstated.
	retryAfter time.Duration
}

func (e *statusError) Error() string { return "teams: HTTP " + strconv.Itoa(e.status) }

// transportError is a request that failed before an answer: refused,
// dropped or timed out.
type transportError struct{ err error }

func (e *transportError) Error() string { return "teams: transport failure" }
func (e *transportError) Unwrap() error { return e.err }

// ready checks that the instance can call the provider.
func (x *Instance) ready() error {
	switch {
	case x.isClosed():
		return model.NewError(model.Unavailable, "instance closed")
	case x.http == nil:
		return model.NewError(model.Unavailable, "no HTTP client injected")
	case x.creds == nil && x.src == nil:
		return &model.Error{Code: model.Unavailable, SafeMessage: "no credentials port", Retry: model.NoRetry}
	}
	return nil
}

// token resolves the Bot Connector access token for one call. It lives
// only in that call's request header. The host mints and refreshes it;
// fresh asks for one that is not cached.
func (x *Instance) token(ctx context.Context, fresh bool) (string, error) {
	if x.src != nil {
		t, err := x.src.Token(ctx, fresh)
		if err != nil {
			return "", err
		}
		if t.Value == "" {
			return "", final(model.Unauthorized, "Bot Connector token empty")
		}
		return t.Value, nil
	}
	c, err := x.creds.Resolve(ctx, ports.CredentialRequest{
		Channel: x.channel.Ref, Reference: x.channel.CredentialRef, Purpose: CredentialPurposeConnector,
		Accept: []ports.CredentialKind{ports.KindOAuth2}, Fresh: fresh,
	})
	if err != nil {
		return "", err
	}
	o, ok := c.(ports.OAuth2)
	if !ok {
		return "", final(model.Unsupported, "credential kind not usable for the Bot Connector")
	}
	if exp := o.Expiry(); o.AccessToken == "" || (!exp.IsZero() && !x.now().Before(exp)) {
		if !fresh {
			return x.token(ctx, true)
		}
		return "", final(model.Unauthorized, "Bot Connector credential empty or expired")
	}
	return o.AccessToken, nil
}

// conversationPath is the Connector path of a conversation's activities,
// or of one activity of it.
func conversationPath(conversation string, activity ...string) string {
	p := "/v3/conversations/" + url.PathEscape(conversation) + "/activities"
	for _, a := range activity {
		p += "/" + url.PathEscape(a)
	}
	return p
}

// call runs one Connector request with a JSON body (nil for none) and
// decodes a JSON answer into out (which may be nil). A token the Connector
// refuses with 401 is resolved again once, fresh: nothing was applied, so
// the retry is safe.
func (x *Instance) call(ctx context.Context, method, path string, body, out any) error {
	tok, err := x.token(ctx, false)
	if err != nil {
		return err
	}
	err = x.do(ctx, method, path, tok, body, out)
	if se, ok := err.(*statusError); ok && se.status == http.StatusUnauthorized {
		if tok, err = x.token(ctx, true); err != nil {
			return err
		}
		return x.do(ctx, method, path, tok, body, out)
	}
	return err
}

func (x *Instance) do(ctx context.Context, method, path, token string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return final(model.Validation, "request is not encodable")
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, x.cfg.serviceBase()+path, rd)
	if err != nil {
		return final(model.Validation, "invalid service_url")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	res, err := x.http.Do(req)
	if err != nil {
		return &transportError{err: err}
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return &transportError{err: err}
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		var e struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return &statusError{status: res.StatusCode, code: e.Error.Code, retryAfter: retryAfter(res.Header)}
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return &transportError{err: err}
		}
	}
	return nil
}

// retryAfter reads Retry-After as whole seconds.
func retryAfter(h http.Header) time.Duration {
	if s, err := strconv.ParseInt(strings.TrimSpace(h.Get("Retry-After")), 10, 64); err == nil && s >= 0 {
		return max(time.Duration(s)*time.Second, time.Second)
	}
	return 0
}
