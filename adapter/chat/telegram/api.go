package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// maxResponseBytes bounds what the adapter reads of one Bot API answer: a
// batch of 100 updates is the largest.
const maxResponseBytes = 16 << 20

// defaultRateWait is the wait asked for when a flood-control answer states
// none; Telegram states one in parameters.retry_after.
const defaultRateWait = 30 * time.Second

// apiError is a Bot API answer with "ok": false. "In case of an unsuccessful
// request, 'ok' equals False and the error is explained in the
// 'description'. An Integer 'error_code' field is also returned, but its
// contents are subject to change in the future. Some errors may also have an
// optional field 'parameters' of the type ResponseParameters, which can help
// to automatically handle the error"
// (https://core.telegram.org/bots/api#making-requests). The descriptions are
// not documented, so the adapter reads the code first and a description only
// to tell apart what one code covers.
type apiError struct {
	code        int
	description string
	retryAfter  time.Duration
	migratedTo  int64
}

func (e *apiError) Error() string { return "telegram: error " + strconv.Itoa(e.code) }

// statusError is an HTTP answer that is neither a success nor a Bot API
// error object.
type statusError struct{ status int }

func (e *statusError) Error() string { return "telegram: HTTP " + strconv.Itoa(e.status) }

// transportError is a request that failed before an answer: refused,
// dropped or timed out. The error underneath names the request URL, which
// holds the bot token: it is never put in a message.
type transportError struct{ err error }

func (e *transportError) Error() string { return "telegram: transport failure" }
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
// request path.
func (x *Instance) token(ctx context.Context, fresh bool) (string, error) {
	c, err := x.creds.Resolve(ctx, ports.CredentialRequest{
		Channel: x.channel.Ref, Reference: x.channel.CredentialRef, Purpose: CredentialPurposeAPI,
		Accept: []ports.CredentialKind{ports.KindSecret}, Fresh: fresh,
	})
	if err != nil {
		return "", err
	}
	s, ok := c.(ports.Secret)
	if !ok {
		return "", final(model.Unsupported, "credential kind not usable for the Bot API")
	}
	if exp := s.Expiry(); len(s.Bytes) == 0 || (!exp.IsZero() && !x.now().Before(exp)) {
		if !fresh {
			return x.token(ctx, true)
		}
		return "", final(model.Unauthorized, "Telegram credential empty or expired")
	}
	return string(s.Bytes), nil
}

// call runs one Bot API method with a JSON body and decodes its result into
// out (which may be nil). A token Telegram refuses as unauthorized is
// resolved again once, fresh: nothing was applied, so the retry is safe.
func (x *Instance) call(ctx context.Context, method string, args, out any) error {
	tok, err := x.token(ctx, false)
	if err != nil {
		return err
	}
	err = x.post(ctx, method, tok, args, out)
	if ae, ok := errors.AsType[*apiError](err); ok && ae.code == http.StatusUnauthorized {
		if tok, err = x.token(ctx, true); err != nil {
			return err
		}
		return x.post(ctx, method, tok, args, out)
	}
	return err
}

// answer is the envelope of every Bot API answer.
type answer struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
	Parameters  *struct {
		RetryAfter      int   `json:"retry_after"`
		MigrateToChatID int64 `json:"migrate_to_chat_id"`
	} `json:"parameters"`
}

// post sends one request: "https://api.telegram.org/bot<token>/METHOD_NAME",
// with the parameters as an application/json body. The answer is read
// whatever its HTTP status: an error carries its object there too.
func (x *Instance) post(ctx context.Context, method, token string, args, out any) error {
	body, err := json.Marshal(args)
	if err != nil {
		return final(model.Validation, "request is not encodable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, x.cfg.apiURL()+"bot"+token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return final(model.Validation, "invalid api_url")
	}
	req.Header.Set("Content-Type", "application/json")
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
	var a answer
	if json.Unmarshal(raw, &a) != nil {
		if res.StatusCode == http.StatusOK {
			return &transportError{err: errors.New("answer is not JSON")}
		}
		return &statusError{status: res.StatusCode}
	}
	if !a.OK {
		e := &apiError{code: a.ErrorCode, description: a.Description}
		if e.code == 0 {
			e.code = res.StatusCode
		}
		if p := a.Parameters; p != nil {
			e.migratedTo = p.MigrateToChatID
			if p.RetryAfter > 0 {
				e.retryAfter = time.Duration(p.RetryAfter) * time.Second
			}
		}
		return e
	}
	if res.StatusCode != http.StatusOK {
		return &statusError{status: res.StatusCode}
	}
	if out != nil {
		if err := json.Unmarshal(a.Result, out); err != nil {
			return &transportError{err: err}
		}
	}
	return nil
}
