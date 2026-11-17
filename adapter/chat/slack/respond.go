package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// maxHookReply bounds what the adapter reads of a response_url's answer.
const maxHookReply = 4096

// hookPayload is the message payload a response_url takes: "text, blocks,
// response_type, replace_original, delete_original, thread_ts"
// (https://docs.slack.dev/interactivity/handling-user-interaction/).
type hookPayload struct {
	ResponseType    string          `json:"response_type,omitempty"`
	ReplaceOriginal bool            `json:"replace_original"`
	Text            string          `json:"text,omitempty"`
	Blocks          json.RawMessage `json:"blocks,omitempty"`
	ThreadTS        string          `json:"thread_ts,omitempty"`
	Mrkdwn          bool            `json:"mrkdwn"`
}

// Respond answers an interaction by posting to its response_url, which the
// engine hands over as the interaction's token: the path of the URL, on
// Options.ResponseBaseURL. The bot token is never sent there, and a
// response needs no credential.
//
// A response is shown as the mode asks, and never as another:
//
//   - message: response_type in_channel, replacing nothing, in the thread
//     when the route is one; public audience;
//   - ephemeral: response_type ephemeral, which only the actor sees; a
//     restricted audience;
//   - update: replace_original, the message the control sits on (the
//     effect's target, which must be it); public. A slash command has no
//     message of its own to replace: "replace_original cannot modify a
//     user-posted slash command message", so it is rejected unsupported.
//
// The window is Slack's: a token past the interaction's window is rejected
// with deadline_expired before any call; Slack takes five responses in
// thirty minutes, and its refusal of a sixth, or of an expired or used
// URL, is final. response_url answers 200 "ok" and names no message, so a
// message response has no provider message ID, and a response whose answer
// was lost cannot be reconciled: it stays outcome_unknown and is never
// resent. Slack documents no error body for response_url; the adapter maps
// the statuses of the incoming webhooks page (400 a refused payload, 403 a
// prohibited action, 404 a webhook that is gone). Text goes literally, as
// in Send; mrkdwn is sent off, which the response_url page does not
// document.
func (x *Instance) Respond(ctx context.Context, r adapter.RespondRequest) (adapter.Outcome, error) {
	switch {
	case x.isClosed():
		return adapter.Outcome{}, model.NewError(model.Unavailable, "instance closed")
	case x.http == nil:
		return adapter.Outcome{}, model.NewError(model.Unavailable, "no HTTP client injected")
	case r.Interaction == nil:
		return adapter.Outcome{}, final(model.Validation, "a response names the interaction it answers")
	}
	ic := r.Interaction
	var p hookPayload
	switch r.Mode {
	case model.ResponseModeMessage:
		p.ResponseType = "in_channel"
	case model.ResponseModeEphemeral:
		p.ResponseType = "ephemeral"
	case model.ResponseModeUpdate:
		p.ReplaceOriginal = true
	default:
		return rejected(model.Unsupported, "Slack has no such response mode"), nil
	}
	switch {
	case ic.Token == "":
		return rejected(model.Unsupported, "Slack gave this interaction no response URL"), nil
	case !validToken(ic.Token):
		return rejected(model.Validation, "the response token is not a response URL path"), nil
	case !ic.ExpiresAt.IsZero() && !x.now().Before(ic.ExpiresAt):
		return rejected(model.DeadlineExpired, "the interaction's response window has passed"), nil
	}
	if want := audienceOf(r.Mode); r.Audience.Visibility != want {
		return rejected(model.Unsupported, "Slack shows a "+r.Mode+" response to a "+string(want)+" audience only"), nil
	}
	if r.Mode == model.ResponseModeUpdate {
		switch {
		case r.Target == nil:
			return adapter.Outcome{}, final(model.Validation, "an update names the message it replaces")
		case ic.Kind == model.InteractionCommand:
			return rejected(model.Unsupported, "a slash command has no message to replace"), nil
		case ic.Message == nil || *r.Target != *ic.Message:
			return rejected(model.Validation, "an update replaces the message the control sits on"), nil
		}
	}
	msg, rerr := render(r.Content)
	if rerr != nil {
		return adapter.Outcome{State: model.Rejected, Failure: rerr}, nil
	}
	p.Text, p.Blocks = msg.text, msg.blocks
	if r.Mode == model.ResponseModeMessage {
		t, err := x.parseRoute(r.Route)
		if err != nil {
			return adapter.Outcome{}, err
		}
		p.ThreadTS = t.thread
	}
	return x.postHook(ctx, ic.Token, &p)
}

func audienceOf(mode string) model.Visibility {
	if mode == model.ResponseModeEphemeral {
		return model.Restricted
	}
	return model.Public
}

// postHook posts payload to the response URL token names.
func (x *Instance) postHook(ctx context.Context, token string, payload *hookPayload) (adapter.Outcome, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return adapter.Outcome{}, final(model.Validation, "response is not encodable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, x.responseBase()+"/"+token, bytes.NewReader(body))
	if err != nil {
		return adapter.Outcome{}, final(model.Validation, "invalid response origin")
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := x.http.Do(req)
	if err != nil {
		return failure(ctx, &transportError{err: err}, writeCall)
	}
	defer func() { _ = res.Body.Close() }()
	reply, _ := io.ReadAll(io.LimitReader(res.Body, maxHookReply))
	if res.StatusCode == http.StatusOK {
		return adapter.Outcome{State: model.ProviderAccepted}, nil
	}
	return hookFailure(res.StatusCode, strings.TrimSpace(string(reply)), retryAfter(res.Header))
}

// hookFailure maps a response_url's refusal. The code its body names joins
// the message only when it is a plain token.
func hookFailure(status int, code string, wait time.Duration) (adapter.Outcome, error) {
	detail := ""
	if codeToken.MatchString(code) {
		detail = ": " + code
	}
	switch {
	case status == http.StatusBadRequest:
		return rejected(model.Validation, "Slack refused the response"+detail), nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return rejected(model.Unauthorized, "Slack denied the response"+detail), nil
	case status == http.StatusNotFound || status == http.StatusGone:
		// A URL that is gone, used five times or past its thirty minutes.
		return rejected(model.NotFound, "the response URL is gone"+detail), nil
	case status == http.StatusTooManyRequests:
		return adapter.Outcome{}, rateLimited(wait)
	case status >= 500:
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Slack server error; the response may have been posted")
	}
	return adapter.Outcome{}, final(model.Permanent, "unexpected Slack answer")
}

var _ adapter.InteractionResponder = (*Instance)(nil)
