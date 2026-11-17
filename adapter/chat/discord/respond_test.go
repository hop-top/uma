package discord

import (
	"net/http"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

const respondTok = "aW50ZXJhY3Rpb24-respond_token"

// interactionEffect answers an interaction of kind, on the message the
// control sits on when onMessage.
func (r *rig) interactionEffect(kind string, onMessage bool, vis model.Visibility) adapter.Effect {
	e := r.effect(r.x.roomRef(testRoom))
	e.Audience.Visibility = vis
	ic := &adapter.InteractionContext{ProviderID: "interaction:1500000000000000002", Kind: kind, Token: respondTok, ExpiresAt: testNow.Add(10 * time.Minute)}
	if onMessage {
		ic.Message = msgRef(testRoom, "1200000000000000001")
	}
	e.Interaction = ic
	return e
}

func TestRespondByDeferral(t *testing.T) {
	type call struct{ method, suffix string }
	hook := "webhooks/" + testApp + "/" + respondTok
	cases := []struct {
		name      string
		kind      string
		onMessage bool
		mode      string
		followUp  bool
		want      *call
		code      model.ErrorCode
		ephemeral bool
	}{
		{"command message edits the loading response", model.InteractionCommand, false, model.ResponseModeMessage, false, &call{http.MethodPatch, "/messages/@original"}, "", false},
		{"command ephemeral is fixed public by the deferral", model.InteractionCommand, false, model.ResponseModeEphemeral, false, nil, model.Unsupported, false},
		{"command update has no message", model.InteractionCommand, false, model.ResponseModeUpdate, false, nil, model.Unsupported, false},
		{"component update edits its message", model.InteractionAction, true, model.ResponseModeUpdate, false, &call{http.MethodPatch, "/messages/@original"}, "", false},
		{"component message is a followup", model.InteractionAction, true, model.ResponseModeMessage, false, &call{http.MethodPost, ""}, "", false},
		{"component ephemeral is an ephemeral followup", model.InteractionAction, true, model.ResponseModeEphemeral, false, &call{http.MethodPost, ""}, "", true},
		{"follow-up ephemeral", model.InteractionAction, true, model.ResponseModeEphemeral, true, &call{http.MethodPost, ""}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			vis := audienceOf(c.mode)
			e := r.interactionEffect(c.kind, c.onMessage, vis)
			if c.mode == model.ResponseModeUpdate {
				e.Target = msgRef(testRoom, "1200000000000000001")
			}
			out, err := r.x.Respond(t.Context(), adapter.RespondRequest{Effect: e, Mode: c.mode, Content: text("answer"), FollowUp: c.followUp})
			if c.want == nil {
				if err != nil || out.State != model.Rejected || out.Failure.Code != c.code {
					t.Fatalf("Respond = %+v, %v, want rejected %s", out, err, c.code)
				}
				if n := len(r.f.calls("acct1")); n != 0 {
					t.Fatalf("a rejected response made %d calls", n)
				}
				return
			}
			if err != nil || out.State != model.ProviderAccepted {
				t.Fatalf("Respond = %+v, %v", out, err)
			}
			calls := r.callsOf(c.want.method, hook+c.want.suffix)
			if len(calls) != 1 || calls[0].Auth != "" {
				t.Fatalf("calls = %+v, want one %s %s without the bot token", r.f.calls("acct1"), c.want.method, hook+c.want.suffix)
			}
			args := calls[0].args(t)
			if (args["flags"] == float64(flagEphemeral)) != c.ephemeral {
				t.Fatalf("flags = %v, ephemeral %v", args["flags"], c.ephemeral)
			}
			if c.want.method == http.MethodPost && calls[0].Query.Get("wait") != "true" {
				t.Fatal("followup without wait=true")
			}
			if (c.mode == model.ResponseModeMessage) != (len(out.References) == 1) {
				t.Fatalf("references %+v: only a public message names one", out.References)
			}
		})
	}
}

// A follow-up while a deferred command still shows loading would replace
// the loading message, so it is refused until the response is given.
func TestFollowUpWaitsForTheResponse(t *testing.T) {
	r := newRig(t)
	r.f.acknowledge("acct1", respondTok, callbackDeferredMessage)
	e := r.interactionEffect(model.InteractionCommand, false, model.Restricted)
	out, err := r.x.Respond(t.Context(), adapter.RespondRequest{Effect: e, Mode: model.ResponseModeEphemeral, Content: text("psst"), FollowUp: true})
	if err != nil || out.State != model.Rejected || out.Failure.Code != model.Conflict {
		t.Fatalf("follow-up while loading = %+v, %v", out, err)
	}
	if n := len(r.callsOf(http.MethodPost, "webhooks/")); n != 0 {
		t.Fatal("follow-up posted while loading")
	}
	pub := r.interactionEffect(model.InteractionCommand, false, model.Public)
	if out, err := r.x.Respond(t.Context(), adapter.RespondRequest{Effect: pub, Mode: model.ResponseModeMessage, Content: text("done")}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("initial = %+v, %v", out, err)
	}
	out, err = r.x.Respond(t.Context(), adapter.RespondRequest{Effect: e, Mode: model.ResponseModeEphemeral, Content: text("psst"), FollowUp: true})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("follow-up after the response = %+v, %v", out, err)
	}
	// SendEphemeral is the same followup, and needs an interaction.
	if out, err := r.x.SendEphemeral(t.Context(), adapter.EphemeralRequest{SendRequest: adapter.SendRequest{Effect: e, Content: text("again")}, ExpiresAt: testNow.Add(time.Minute)}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("SendEphemeral = %+v, %v", out, err)
	}
	if posts := r.callsOf(http.MethodPost, "webhooks/"); len(posts) != 2 || posts[1].args(t)["flags"] != float64(flagEphemeral) {
		t.Fatalf("followups = %+v", posts)
	}
	plain := r.effect(r.x.roomRef(testRoom))
	plain.Audience.Visibility = model.Restricted
	if out, err := r.x.SendEphemeral(t.Context(), adapter.EphemeralRequest{SendRequest: adapter.SendRequest{Effect: plain, Content: text("x")}}); err != nil || out.State != model.Rejected || out.Failure.Code != model.Unsupported {
		t.Fatalf("ephemeral with no interaction = %+v, %v", out, err)
	}
}

func TestRespondWindowAndRefusals(t *testing.T) {
	r := newRig(t)
	e := r.interactionEffect(model.InteractionAction, true, model.Public)
	e.Interaction.ExpiresAt = testNow
	if out, _ := r.x.Respond(t.Context(), adapter.RespondRequest{Effect: e, Mode: model.ResponseModeMessage, Content: text("late")}); out.State != model.Rejected || out.Failure.Code != model.DeadlineExpired {
		t.Fatalf("expired = %+v", out)
	}
	e = r.interactionEffect(model.InteractionAction, true, model.Restricted)
	if out, _ := r.x.Respond(t.Context(), adapter.RespondRequest{Effect: e, Mode: model.ResponseModeMessage, Content: text("x")}); out.State != model.Rejected || out.Failure.Code != model.Unsupported {
		t.Fatalf("audience mismatch = %+v", out)
	}
	e = r.interactionEffect(model.InteractionAction, true, model.Public)
	e.Interaction.Token = "has spaces/../x"
	if out, _ := r.x.Respond(t.Context(), adapter.RespondRequest{Effect: e, Mode: model.ResponseModeMessage, Content: text("x")}); out.State != model.Rejected || out.Failure.Code != model.Validation {
		t.Fatalf("bad token = %+v", out)
	}
	e = r.interactionEffect(model.InteractionAction, true, model.Public)
	e.Target = msgRef(testRoom, "1200000000000000999")
	if out, _ := r.x.Respond(t.Context(), adapter.RespondRequest{Effect: e, Mode: model.ResponseModeUpdate, Content: text("x")}); out.State != model.Rejected || out.Failure.Code != model.Validation {
		t.Fatalf("update of another message = %+v", out)
	}
	if n := len(r.f.calls("acct1")); n != 0 {
		t.Fatalf("refusals made %d calls", n)
	}
	r.f.respond("acct1", refusal(404, codeUnknownWebhook))
	e = r.interactionEffect(model.InteractionAction, true, model.Public)
	if out, err := r.x.Respond(t.Context(), adapter.RespondRequest{Effect: e, Mode: model.ResponseModeMessage, Content: text("x")}); err != nil || out.State != model.Rejected || out.Failure.Code != model.NotFound {
		t.Fatalf("expired at Discord = %+v, %v", out, err)
	}
}
