package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"hop.top/uma/model"
)

// maxResponseBytes bounds what the adapter reads of one REST answer: a
// page of 100 messages is the largest.
const maxResponseBytes = 8 << 20

// UserAgent is sent with every request: "User-Agent: DiscordBot ($url,
// $versionNumber)"; "Client requests that do not have a valid User Agent
// specified may be blocked" (https://docs.discord.com/developers/reference).
const UserAgent = "DiscordBot (https://github.com/hop-top/uma, 1)"

// apiError is an answer other than 2xx: its status, the JSON error code
// of the body ("code", such as 10008 Unknown Message), and for a 429 the
// wait.
type apiError struct {
	status int
	code   int
}

func (e *apiError) Error() string {
	return "discord: HTTP " + strconv.Itoa(e.status) + " code " + strconv.Itoa(e.code)
}

// transportError is a request that failed before an answer: refused,
// dropped or timed out.
type transportError struct{ err error }

func (e *transportError) Error() string { return "discord: transport failure" }
func (e *transportError) Unwrap() error { return e.err }

// request is one REST call: method, path under the API root, an optional
// query and JSON body. bare sends no bot token: interaction callbacks and
// interaction webhooks authenticate by the token in their path.
type request struct {
	method string
	path   string
	query  url.Values
	body   any
	bare   bool
}

// call runs one authenticated JSON call and decodes its answer into out
// (which may be nil).
func (x *Instance) call(ctx context.Context, method, path string, body, out any) error {
	return x.do(ctx, request{method: method, path: path, body: body}, out)
}

// do runs r. A bot token Discord refuses (401) is resolved again once,
// fresh, and the call repeated: a 401 applied nothing.
func (x *Instance) do(ctx context.Context, r request, out any) error {
	err := x.attempt(ctx, r, out, false)
	if ae, ok := errors.AsType[*apiError](err); ok && ae.status == http.StatusUnauthorized && !r.bare {
		return x.attempt(ctx, r, out, true)
	}
	return err
}

func (x *Instance) attempt(ctx context.Context, r request, out any, fresh bool) error {
	var payload []byte
	if r.body != nil {
		b, err := json.Marshal(r.body)
		if err != nil {
			return final(model.Validation, "request is not encodable")
		}
		payload = b
	}
	u := x.cfg.apiURL() + r.path
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u, bytes.NewReader(payload))
	if err != nil {
		return final(model.Validation, "invalid api_url")
	}
	if !r.bare {
		tok, err := x.token(ctx, fresh)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bot "+tok)
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	} else {
		req.Body, req.ContentLength = http.NoBody, 0
	}
	rt := routeOf(r.method, r.path)
	if err := x.limits.acquire(ctx, rt); err != nil {
		return err
	}
	res, err := x.http.Do(req)
	if err != nil {
		return &transportError{err: err}
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	x.limits.update(rt, res.Header, res.StatusCode, raw)
	if err != nil {
		return &transportError{err: err}
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		var head struct {
			Code int `json:"code"`
		}
		_ = json.Unmarshal(raw, &head)
		ae := &apiError{status: res.StatusCode, code: head.Code}
		if res.StatusCode == http.StatusTooManyRequests {
			wait, _ := retryAfter(res.Header, raw)
			return rateLimited(wait)
		}
		return ae
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return &transportError{err: err}
		}
	}
	return nil
}
