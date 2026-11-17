package discord

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func TestSendRoutes(t *testing.T) {
	r := newRig(t)
	r.f.put("acct1", testRoom, testPerson, "question", "1200000000000000001")
	r.f.thread("acct1", testRoom, testThread)
	cases := []struct {
		name    string
		route   model.ExternalRef
		target  *model.ExternalRef
		channel string
		replyTo string
	}{
		{"room", r.x.roomRef(testRoom), nil, testRoom, ""},
		{"room reply", r.x.roomRef(testRoom), msgRef(testRoom, "1200000000000000001"), testRoom, "1200000000000000001"},
		{"reply to another channel's message is no reply", r.x.roomRef(testThread), msgRef(testRoom, "1200000000000000001"), testThread, ""},
		{"thread", r.x.threadRef(testRoom, testThread), nil, testThread, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := r.effect(c.route)
			e.Target = c.target
			out, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: e, Content: text("hello <@1> @everyone")})
			if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
				t.Fatalf("Send = %+v, %v", out, err)
			}
			calls := r.callsOf(http.MethodPost, "channels/"+c.channel+"/messages")
			last := calls[len(calls)-1]
			args := last.args(t)
			if out.References[0] != *msgRef(c.channel, last.Message) {
				t.Fatalf("reference %+v, want the created message", out.References[0])
			}
			if args["content"] != "hello <@1> @everyone" || args["enforce_nonce"] != true || args["nonce"] != operationNonce(e) || len(args["nonce"].(string)) > 25 {
				t.Fatalf("body = %+v", args)
			}
			if am, _ := args["allowed_mentions"].(map[string]any); am == nil || len(am["parse"].([]any)) != 0 {
				t.Fatalf("allowed_mentions = %v, want none parsed", args["allowed_mentions"])
			}
			ref, _ := args["message_reference"].(map[string]any)
			if (c.replyTo == "") != (ref == nil) || (ref != nil && (ref["message_id"] != c.replyTo || ref["fail_if_not_exists"] != false)) {
				t.Fatalf("message_reference = %v, want %q", ref, c.replyTo)
			}
			if last.Auth != "Bot "+testToken || last.Agent != UserAgent {
				t.Fatalf("headers auth %q agent %q", last.Auth, last.Agent)
			}
		})
	}
	t.Run("user opens a direct message", func(t *testing.T) {
		out, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(r.x.userRef(testPerson)), Content: text("hi")})
		if err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("Send = %+v, %v", out, err)
		}
		opens := r.callsOf(http.MethodPost, "users/@me/channels")
		if len(opens) != 1 || opens[0].args(t)["recipient_id"] != testPerson {
			t.Fatalf("Create DM calls = %+v", opens)
		}
		dm := r.acct.dms[testPerson]
		if len(r.callsOf(http.MethodPost, "channels/"+dm+"/messages")) != 1 {
			t.Fatal("not posted in the direct message channel")
		}
	})
}

// A second attempt of an operation carries its nonce: Discord answers the
// first attempt's message and creates none.
func TestSendNonceDedupes(t *testing.T) {
	r := newRig(t)
	r.f.put("acct1", testRoom, testPerson, "x", "")
	r.f.respond("acct1", answerLost)
	e := r.effect(r.x.roomRef(testRoom))
	if _, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: e, Content: text("once")}); !model.IsCode(err, model.OutcomeUnknown) {
		t.Fatalf("lost answer = %v, want outcome unknown", err)
	}
	e.Attempt = "00000ad0-0000-7000-8000-0000000000f3"
	out, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: e, Content: text("once")})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("second attempt = %+v, %v", out, err)
	}
	bot := 0
	for _, m := range r.acct.channels[testRoom].msgs {
		if m.author == testBotUser {
			bot++
		}
	}
	if bot != 1 {
		t.Fatalf("bot messages = %d, want one", bot)
	}
}

func TestSendRefusals(t *testing.T) {
	r := newRig(t)
	r.f.put("acct1", testRoom, testPerson, "x", "")
	room := r.x.roomRef(testRoom)
	long := strings.Repeat("é", MaxTextRunes+1)
	for name, c := range map[string]struct {
		content  model.Content
		audience model.Visibility
		code     model.ErrorCode
	}{
		"too long":     {text(long), model.Public, model.Validation},
		"empty":        {text("  "), model.Public, model.Validation},
		"restricted":   {text("x"), model.Restricted, model.Unsupported},
		"attachment":   {model.Content{Parts: []model.Part{{Kind: model.PartAttachment}}}, model.Public, model.Unsupported},
		"other schema": {model.Content{Parts: []model.Part{{Kind: model.PartStructured, Structured: &model.Extension{Name: "x.y", Version: 1, Data: []byte(`[]`)}}}}, model.Public, model.Unsupported},
	} {
		e := r.effect(room)
		e.Audience.Visibility = c.audience
		out, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: e, Content: c.content})
		if err != nil || out.State != model.Rejected || out.Failure.Code != c.code {
			t.Errorf("%s: %+v, %v", name, out, err)
		}
	}
	if n := len(r.callsOf(http.MethodPost, "channels/")); n != 0 {
		t.Fatalf("refused sends reached Discord %d times", n)
	}
	comps := model.Content{Parts: []model.Part{{Kind: model.PartStructured, Structured: &model.Extension{Name: ComponentsSchema, Version: ComponentsVersion,
		Data: []byte(`[{"type":1,"components":[{"type":2,"style":1,"label":"OK","custom_id":"ok"}]}]`)}}}}
	if out, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(room), Content: comps}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("components = %+v, %v", out, err)
	}
	for name, c := range map[string]struct {
		a    answer
		code model.ErrorCode
		ok   bool
	}{
		"denied":       {refusal(403, codeMissingPermissions), model.Unauthorized, true},
		"cannot DM":    {refusal(403, codeCannotDMUser), model.Unauthorized, true},
		"gone":         {refusal(404, codeUnknownChannel), model.NotFound, true},
		"invalid":      {refusal(400, codeInvalidFormBody), model.Validation, true},
		"server":       {answer{status: 502}, model.OutcomeUnknown, false},
		"lost":         {answerLost, model.OutcomeUnknown, false},
		"unreachable":  {unreachable, model.Transient, false},
		"rate limited": {rateLimitedFor(1500*time.Millisecond, false), model.RateLimited, false},
	} {
		x := newRig(t)
		x.f.put("acct1", testRoom, testPerson, "x", "")
		x.f.respond("acct1", c.a)
		out, err := x.x.Send(t.Context(), adapter.SendRequest{Effect: x.effect(x.x.roomRef(testRoom)), Content: text("x")})
		switch {
		case c.ok && (err != nil || out.State != model.Rejected || out.Failure.Code != c.code):
			t.Errorf("%s: %+v, %v, want rejected %s", name, out, err, c.code)
		case !c.ok && model.CodeOf(err) != c.code:
			t.Errorf("%s: %+v, %v, want %s", name, out, err, c.code)
		}
		if name == "rate limited" {
			if me, _ := err.(*model.Error); me == nil || me.RetryAfter != 1500*time.Millisecond {
				t.Errorf("rate limit wait = %v, want retry_after", err)
			}
		}
	}
}

// A bot token Discord refuses is resolved again, fresh, once.
func TestRefusedTokenResolvedFresh(t *testing.T) {
	r := newRig(t)
	r.f.put("acct1", testRoom, testPerson, "x", "")
	r.creds.token = "stale"
	fresh := 0
	r.x.creds = credentialFunc(func(req portsRequest) (portsCredential, error) {
		if req.Fresh {
			fresh++
			return secretOf(testToken), nil
		}
		return secretOf("stale"), nil
	})
	out, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(r.x.roomRef(testRoom)), Content: text("x")})
	if err != nil || out.State != model.ProviderAccepted || fresh != 1 {
		t.Fatalf("Send = %+v, %v after %d fresh resolutions", out, err, fresh)
	}
	r.x.creds = credentialFunc(func(portsRequest) (portsCredential, error) { return secretOf("stale"), nil })
	if _, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(r.x.roomRef(testRoom)), Content: text("x")}); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("Send with a refused token = %v, want unauthorized", err)
	}
}
