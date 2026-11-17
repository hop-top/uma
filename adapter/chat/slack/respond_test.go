package slack

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

var testControl = model.ProviderMessageRef(testChannel, testRoom+":"+testTS)

func (r *rig) interaction(mod ...func(*adapter.InteractionContext)) *adapter.InteractionContext {
	ic := &adapter.InteractionContext{
		ProviderID: "action:1.2.3", Kind: model.InteractionAction, Token: hookTok,
		ExpiresAt: testNow.Add(ResponseWindow), Message: &testControl,
	}
	for _, m := range mod {
		m(ic)
	}
	return ic
}

func (r *rig) respond(t *testing.T, route model.ExternalRef, ic *adapter.InteractionContext, mode string, c model.Content, mod ...func(*adapter.RespondRequest)) (adapter.Outcome, error) {
	t.Helper()
	e := r.effect(route)
	e.Interaction = ic
	switch mode {
	case model.ResponseModeEphemeral:
		e.Audience.Visibility = model.Restricted
	case model.ResponseModeUpdate:
		e.Target = ic.Message
	}
	req := adapter.RespondRequest{Effect: e, Mode: mode, Content: c}
	for _, m := range mod {
		m(&req)
	}
	return r.x.Respond(t.Context(), req)
}

func (r *rig) hookCalls() []fakeCall { return r.callsOf("response_url") }

func hookBody(t *testing.T, c fakeCall) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.Body, &m); err != nil {
		t.Fatalf("response body %q: %v", c.Body, err)
	}
	return m
}

func TestRespondInEachMode(t *testing.T) {
	room := func(r *rig) model.ExternalRef { return r.x.roomRef(testRoom) }
	for name, tc := range map[string]struct {
		mode string
		want map[string]any
	}{
		"message":   {model.ResponseModeMessage, map[string]any{"response_type": "in_channel", "replace_original": false, "text": "a &lt; b", "mrkdwn": false}},
		"ephemeral": {model.ResponseModeEphemeral, map[string]any{"response_type": "ephemeral", "replace_original": false, "text": "a &lt; b", "mrkdwn": false}},
		"update":    {model.ResponseModeUpdate, map[string]any{"replace_original": true, "text": "a &lt; b", "mrkdwn": false}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			out, err := r.respond(t, room(r), r.interaction(), tc.mode, text("a < b"))
			wantAccepted(t, out, err)
			calls := r.hookCalls()
			if len(calls) != 1 || !calls[0].Applied || calls[0].Token != hookTok {
				t.Fatalf("calls = %+v, want one applied post to the response URL", calls)
			}
			got := hookBody(t, calls[0])
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s = %v, want %v in %s", k, got[k], v, calls[0].Body)
				}
			}
			if _, has := got["thread_ts"]; has {
				t.Errorf("thread_ts in %s", calls[0].Body)
			}
			// The bot token never goes to a response URL.
			if calls[0].Auth != "" || strings.Contains(string(calls[0].Body), testToken) {
				t.Errorf("the request carries credentials: %+v", calls[0])
			}
			if len(r.creds.requests) != 0 {
				t.Errorf("credential requests = %+v: a response needs none", r.creds.requests)
			}
		})
	}
}

func TestRespondInAThreadAndWithBlocks(t *testing.T) {
	r := newRig(t)
	c := model.Content{Parts: []model.Part{
		{Kind: model.PartText, Text: "Done"},
		{Kind: model.PartStructured, Structured: &model.Extension{Name: BlocksSchema, Version: BlocksVersion, Data: []byte(`[{"type":"divider"}]`)}},
	}}
	out, err := r.respond(t, r.x.threadRef(testRoom, "1699999999.000100"), r.interaction(), model.ResponseModeMessage, c)
	wantAccepted(t, out, err)
	got := hookBody(t, r.hookCalls()[0])
	if got["thread_ts"] != "1699999999.000100" || got["replace_original"] != false || got["text"] != "Done" {
		t.Errorf("body = %v, want a reply in the thread that replaces nothing", got)
	}
	if blocks, _ := got["blocks"].([]any); len(blocks) != 1 {
		t.Errorf("blocks = %v", got["blocks"])
	}
}

func TestRespondRefusals(t *testing.T) {
	r := newRig(t)
	room := r.x.roomRef(testRoom)
	with := func(f func(*adapter.InteractionContext)) func() *adapter.InteractionContext {
		return func() *adapter.InteractionContext { return r.interaction(f) }
	}
	plain := func() *adapter.InteractionContext { return r.interaction() }
	audience := func(v model.Visibility) func(*adapter.RespondRequest) {
		return func(q *adapter.RespondRequest) { q.Audience.Visibility = v }
	}
	other := model.ProviderMessageRef(testChannel, testRoom+":1600000000.000001")
	attachment := model.Content{Parts: []model.Part{{Kind: model.PartAttachment, Attachment: &model.AttachmentRef{}}}}
	type refusal struct {
		ic   func() *adapter.InteractionContext
		mode string
		mod  func(*adapter.RespondRequest)
		c    model.Content
		code model.ErrorCode
		// malformed is a request the engine never makes: an error of the call.
		malformed bool
	}
	for name, tc := range map[string]refusal{
		"no interaction":                    {ic: func() *adapter.InteractionContext { return nil }, mode: model.ResponseModeMessage, c: text("x"), code: model.Validation, malformed: true},
		"no token":                          {ic: with(func(c *adapter.InteractionContext) { c.Token = "" }), mode: model.ResponseModeMessage, c: text("x"), code: model.Unsupported},
		"a token that is a URL":             {ic: with(func(c *adapter.InteractionContext) { c.Token = "https://evil.example/x" }), mode: model.ResponseModeMessage, c: text("x"), code: model.Validation},
		"a token that climbs":               {ic: with(func(c *adapter.InteractionContext) { c.Token = "actions/../../x" }), mode: model.ResponseModeMessage, c: text("x"), code: model.Validation},
		"window closed":                     {ic: with(func(c *adapter.InteractionContext) { c.ExpiresAt = testNow }), mode: model.ResponseModeMessage, c: text("x"), code: model.DeadlineExpired},
		"message to a restricted audience":  {ic: plain, mode: model.ResponseModeMessage, mod: audience(model.Restricted), c: text("x"), code: model.Unsupported},
		"ephemeral to a public audience":    {ic: plain, mode: model.ResponseModeEphemeral, mod: audience(model.Public), c: text("x"), code: model.Unsupported},
		"update to an internal audience":    {ic: plain, mode: model.ResponseModeUpdate, mod: audience(model.Internal), c: text("x"), code: model.Unsupported},
		"update with no message to replace": {ic: with(func(c *adapter.InteractionContext) { c.Message = nil }), mode: model.ResponseModeUpdate, mod: func(q *adapter.RespondRequest) { q.Target = nil }, c: text("x"), code: model.Validation, malformed: true},
		"update of another message":         {ic: plain, mode: model.ResponseModeUpdate, mod: func(q *adapter.RespondRequest) { q.Target = &other }, c: text("x"), code: model.Validation},
		"update of a command's message":     {ic: with(func(c *adapter.InteractionContext) { c.Kind = model.InteractionCommand }), mode: model.ResponseModeUpdate, c: text("x"), code: model.Unsupported},
		"an attachment":                     {ic: plain, mode: model.ResponseModeMessage, c: attachment, code: model.Unsupported},
		"no content":                        {ic: plain, mode: model.ResponseModeMessage, c: model.Content{}, code: model.Validation},
		"a mode that is none":               {ic: plain, mode: "reply", c: text("x"), code: model.Unsupported},
		"a route that is not Slack's": {ic: plain, mode: model.ResponseModeMessage, mod: func(q *adapter.RespondRequest) {
			q.Route = model.ExternalRef{Channel: testChannel, Namespace: "x.y", Kind: "z", ID: "1"}
		}, c: text("x"), code: model.Validation, malformed: true},
	} {
		t.Run(name, func(t *testing.T) {
			var mod []func(*adapter.RespondRequest)
			if tc.mod != nil {
				mod = append(mod, tc.mod)
			}
			out, err := r.respond(t, room, tc.ic(), tc.mode, tc.c, mod...)
			if tc.malformed {
				wantError(t, out, err, tc.code)
			} else {
				wantRejected(t, out, err, tc.code)
			}
			if len(r.hookCalls()) != 0 {
				t.Fatalf("a refused response made calls: %+v", r.hookCalls())
			}
		})
	}
}

func TestRespondMapsSlacksAnswers(t *testing.T) {
	for name, tc := range map[string]struct {
		ans  answer
		want func(*testing.T, adapter.Outcome, error)
	}{
		"gone, used or expired": {answer{status: 404, errCode: "no_service"}, func(t *testing.T, o adapter.Outcome, e error) { wantRejected(t, o, e, model.NotFound) }},
		"refused payload":       {answer{status: 400, errCode: "invalid_payload"}, func(t *testing.T, o adapter.Outcome, e error) { wantRejected(t, o, e, model.Validation) }},
		"no text":               {answer{status: 400, errCode: "no_text"}, func(t *testing.T, o adapter.Outcome, e error) { wantRejected(t, o, e, model.Validation) }},
		"prohibited":            {answer{status: 403, errCode: "action_prohibited"}, func(t *testing.T, o adapter.Outcome, e error) { wantRejected(t, o, e, model.Unauthorized) }},
		"archived":              {answer{status: 404, errCode: "channel_is_archived"}, func(t *testing.T, o adapter.Outcome, e error) { wantRejected(t, o, e, model.NotFound) }},
		"rate limited": {rateLimitedFor(12 * time.Second), func(t *testing.T, o adapter.Outcome, e error) {
			if me := errorOf(t, o, e, model.RateLimited); me.RetryAfter != 12*time.Second {
				t.Errorf("wait = %v", me.RetryAfter)
			}
		}},
		"server error":  {answer{status: 503, errCode: "x"}, func(t *testing.T, o adapter.Outcome, e error) { wantError(t, o, e, model.OutcomeUnknown) }},
		"answer lost":   {answerLost, func(t *testing.T, o adapter.Outcome, e error) { wantError(t, o, e, model.OutcomeUnknown) }},
		"request lost":  {requestLost, func(t *testing.T, o adapter.Outcome, e error) { wantError(t, o, e, model.OutcomeUnknown) }},
		"never reached": {unreachable, func(t *testing.T, o adapter.Outcome, e error) { wantError(t, o, e, model.Transient) }},
		"unexpected":    {answer{status: 302, errCode: "x"}, func(t *testing.T, o adapter.Outcome, e error) { wantError(t, o, e, model.Permanent) }},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.f.respond("acct1", tc.ans)
			out, err := r.respond(t, r.x.roomRef(testRoom), r.interaction(), model.ResponseModeMessage, text("x"))
			tc.want(t, out, err)
			// Whatever happened, the token is nowhere in what the engine keeps.
			dump := ""
			if err != nil {
				dump += err.Error()
			}
			if out.Failure != nil {
				dump += out.Failure.Error()
			}
			if strings.Contains(dump, hookTok) || strings.Contains(dump, "T0001/397700885554") {
				t.Errorf("the response token leaked into %q", dump)
			}
		})
	}
}

// A response URL takes five answers: the sixth is Slack's refusal, final.
func TestRespondFollowUpsUpToSlacksLimit(t *testing.T) {
	r := newRig(t)
	ic := r.interaction()
	for i := range 5 {
		out, err := r.respond(t, r.x.roomRef(testRoom), ic, model.ResponseModeEphemeral, text("n"), func(q *adapter.RespondRequest) { q.FollowUp = i > 0 })
		wantAccepted(t, out, err)
	}
	out, err := r.respond(t, r.x.roomRef(testRoom), ic, model.ResponseModeEphemeral, text("n"), func(q *adapter.RespondRequest) { q.FollowUp = true })
	wantRejected(t, out, err, model.NotFound)
}

func TestRespondNeedsAHost(t *testing.T) {
	r := newRig(t)
	_ = r.x.Close(t.Context())
	out, err := r.respond(t, r.x.roomRef(testRoom), r.interaction(), model.ResponseModeMessage, text("x"))
	wantError(t, out, err, model.Unavailable)
	noHTTP := open(t, testConfig())
	e := r.effect(noHTTP.roomRef(testRoom))
	e.Interaction = r.interaction()
	out, err = noHTTP.Respond(t.Context(), adapter.RespondRequest{Effect: e, Mode: model.ResponseModeMessage, Content: text("x")})
	wantError(t, out, err, model.Unavailable)
}

func TestRespondHonoursTheHostsOrigin(t *testing.T) {
	r := newRig(t)
	r.x.opts.ResponseBaseURL = "https://hooks.slack.test/"
	out, err := r.respond(t, r.x.roomRef(testRoom), r.interaction(), model.ResponseModeMessage, text("x"))
	wantAccepted(t, out, err)
}
