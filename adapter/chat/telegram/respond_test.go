package telegram

import (
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// ic is the interaction a response answers, as the effector loads it: the
// callback query ID is the token, valid for AnswerWindow, and the control
// sits on message 77 of the group.
func (r *rig) ic(token string) *adapter.InteractionContext {
	msg := model.ProviderMessageRef(testChannel, messageID(testGroup, 77))
	c := &adapter.InteractionContext{ProviderID: "callback:" + token, Kind: model.InteractionAction, Token: token, ExpiresAt: testNow.Add(AnswerWindow), Message: &msg}
	if token != "" {
		r.f.openCallback("acct1", token, testGroup, 77)
	}
	return c
}

func (r *rig) respond(t *testing.T, mode string, ic *adapter.InteractionContext, c model.Content, mod ...func(*adapter.RespondRequest)) (adapter.Outcome, error) {
	t.Helper()
	e := r.effect(r.room(testGroup))
	e.Interaction = ic
	e.Audience = model.Audience{Visibility: model.Public}
	if mode == model.ResponseModeEphemeral {
		e.Audience = model.Audience{Visibility: model.Restricted}
	}
	req := adapter.RespondRequest{Effect: e, Mode: mode, Content: c}
	for _, m := range mod {
		m(&req)
	}
	return r.x.Respond(t.Context(), req)
}

func methodsOf(r *rig) string {
	var out []string
	for _, c := range r.f.calls("acct1") {
		out = append(out, c.Method)
	}
	return strings.Join(out, ",")
}

func TestRespondWithAMessage(t *testing.T) {
	r := newRig(t)
	ic := r.ic("cbq-9")
	out, err := r.respond(t, model.ResponseModeMessage, ic, text("done"))
	mustAccept(t, out, err)
	// The message goes out first, then the callback query is answered with
	// nothing to show, which clears the client's progress bar.
	if got := methodsOf(r); got != "sendMessage,answerCallbackQuery" {
		t.Fatalf("calls %s", got)
	}
	if a := r.callsOf("answerCallbackQuery")[0].args(); a["callback_query_id"] != "cbq-9" || a["text"] != nil {
		t.Errorf("answer %v", a)
	}
	if len(out.References) != 1 || out.References[0].Kind != model.RefKindMessage {
		t.Errorf("references %+v: Telegram names the message it sent", out.References)
	}
	if !r.f.callback("acct1", "cbq-9").answered {
		t.Error("the query was never answered")
	}
}

// Whether the callback query could still be answered never changes what the
// response did: the message is out.
func TestAnAnswerThatFailsDoesNotUndoTheMessage(t *testing.T) {
	r := newRig(t)
	ic := r.ic("cbq-9")
	r.f.expireCallback("acct1", "cbq-9")
	out, err := r.respond(t, model.ResponseModeMessage, ic, text("done"))
	mustAccept(t, out, err)
	if n := len(r.callsOf("answerCallbackQuery")); n != 1 {
		t.Errorf("%d answers", n)
	}
	// A refused send makes no answer: nothing was done to acknowledge.
	r2 := newRig(t)
	ic2 := r2.ic("cbq-8")
	r2.f.respond("acct1", failWith(403, "Forbidden: bot was kicked from the supergroup chat"))
	out, err = r2.respond(t, model.ResponseModeMessage, ic2, text("done"))
	wantRejected(t, out, err, model.Unauthorized)
	if n := len(r2.callsOf("answerCallbackQuery")); n != 0 {
		t.Errorf("%d answers after a refused message", n)
	}
}

func TestRespondInATopicWithAKeyboard(t *testing.T) {
	r := newRig(t)
	ic := r.ic("cbq-9")
	kb := `{"inline_keyboard":[[{"text":"Again","callback_data":"again"}]]}`
	e := r.effect(r.topic(testGroup, 4))
	e.Interaction, e.Audience = ic, model.Audience{Visibility: model.Public}
	out, err := r.x.Respond(t.Context(), adapter.RespondRequest{Effect: e, Mode: model.ResponseModeMessage, Content: content(textPart("again?"), keyboard(kb))})
	mustAccept(t, out, err)
	a := r.callsOf("sendMessage")[0].args()
	if a["message_thread_id"] != float64(4) || a["reply_markup"] == nil {
		t.Errorf("args %v", a)
	}
}

func TestRespondByUpdatingTheMessage(t *testing.T) {
	r := newRig(t)
	id := r.ownMessage(testGroup, "Approve?")
	msg := model.ProviderMessageRef(testChannel, messageID(testGroup, id))
	ic := r.ic("cbq-9")
	ic.Message = &msg
	out, err := r.respond(t, model.ResponseModeUpdate, ic, text("Approved"), func(q *adapter.RespondRequest) { q.Target = &msg })
	mustAccept(t, out, err)
	if got := methodsOf(r); got != "editMessageText,answerCallbackQuery" {
		t.Fatalf("calls %s", got)
	}
	if m := r.f.find("acct1", testGroup, id); m.text != "Approved" {
		t.Errorf("message %q", m.text)
	}
	// A button's message is the only one an update replaces.
	other := model.ProviderMessageRef(testChannel, messageID(testGroup, id+1))
	before := len(r.f.calls("acct1"))
	out, err = r.respond(t, model.ResponseModeUpdate, ic, text("x"), func(q *adapter.RespondRequest) { q.Target = &other })
	wantRejected(t, out, err, model.Validation)
	_, err = r.respond(t, model.ResponseModeUpdate, ic, text("x"))
	_ = wantErr(t, err, model.Validation)
	if len(r.f.calls("acct1")) != before {
		t.Error("a refused update reached Telegram")
	}
}

func TestRespondEphemerallyIsTheCallbackAnswer(t *testing.T) {
	r := newRig(t)
	ic := r.ic("cbq-9")
	out, err := r.respond(t, model.ResponseModeEphemeral, ic, text("Saved"))
	mustAccept(t, out, err)
	// Only the one call: a notification shown to the person who pressed.
	if got := methodsOf(r); got != "answerCallbackQuery" {
		t.Fatalf("calls %s", got)
	}
	a := r.callsOf("answerCallbackQuery")[0].args()
	if a["callback_query_id"] != "cbq-9" || a["text"] != "Saved" || a["show_alert"] == true {
		t.Errorf("answer %v", a)
	}
	if len(out.References) != 0 {
		t.Errorf("references %+v: a notification is no message", out.References)
	}
	// 200 characters fit, 201 do not; markup and keyboards have no place in one.
	r2 := newRig(t)
	mustAcceptOutcome(t, r2, r2.ic("a"), text(strings.Repeat("é", 200)))
	for name, c := range map[string]model.Content{
		"201 characters": text(strings.Repeat("é", 201)),
		"nothing":        text(""),
		"markup":         content(markup(ParseModeHTML, "<b>x</b>")),
		"keyboard":       content(textPart("x"), keyboard(`{"inline_keyboard":[[{"text":"a","callback_data":"b"}]]}`)),
	} {
		t.Run(name, func(t *testing.T) {
			before := len(r2.f.calls("acct1"))
			out, err := r2.respond(t, model.ResponseModeEphemeral, r2.ic("b"), c)
			if err != nil || out.State != model.Rejected {
				t.Fatalf("outcome %+v error %v, want a rejection", out, err)
			}
			if len(r2.f.calls("acct1")) != before {
				t.Error("a refused response reached Telegram")
			}
		})
	}
}

func mustAcceptOutcome(t *testing.T, r *rig, ic *adapter.InteractionContext, c model.Content) {
	t.Helper()
	out, err := r.respond(t, model.ResponseModeEphemeral, ic, c)
	mustAccept(t, out, err)
}

func TestRespondChecksAudienceAndMode(t *testing.T) {
	r := newRig(t)
	ic := r.ic("cbq-9")
	for name, tc := range map[string]struct {
		mode string
		aud  model.Visibility
		mod  func(*adapter.RespondRequest)
	}{
		"ephemeral to everyone": {model.ResponseModeEphemeral, model.Public, nil},
		"message to one person": {model.ResponseModeMessage, model.Restricted, nil},
		"update to one person":  {model.ResponseModeUpdate, model.Restricted, nil},
		"unknown mode":          {"whisper", model.Public, nil},
		"a follow-up":           {model.ResponseModeMessage, model.Public, func(q *adapter.RespondRequest) { q.FollowUp = true }},
	} {
		t.Run(name, func(t *testing.T) {
			e := r.effect(r.room(testGroup))
			e.Interaction, e.Audience = ic, model.Audience{Visibility: tc.aud}
			req := adapter.RespondRequest{Effect: e, Mode: tc.mode, Content: text("x")}
			if tc.mod != nil {
				tc.mod(&req)
			}
			out, err := r.x.Respond(t.Context(), req)
			wantRejected(t, out, err, model.Unsupported)
			if n := len(r.f.calls("acct1")); n != 0 {
				t.Fatalf("%d calls", n)
			}
		})
	}
	_, err := r.x.Respond(t.Context(), adapter.RespondRequest{Effect: r.effect(r.room(testGroup)), Mode: model.ResponseModeMessage, Content: text("x")})
	_ = wantErr(t, err, model.Validation)
}

func TestRespondAfterTheWindowIsExpired(t *testing.T) {
	r := newRig(t)
	ic := r.ic("cbq-9")
	ic.ExpiresAt = testNow
	for _, mode := range []string{model.ResponseModeMessage, model.ResponseModeEphemeral, model.ResponseModeUpdate} {
		out, err := r.respond(t, mode, ic, text("late"), func(q *adapter.RespondRequest) { q.Target = ic.Message })
		wantRejected(t, out, err, model.DeadlineExpired)
	}
	if n := len(r.f.calls("acct1")); n != 0 {
		t.Fatalf("%d calls after the window", n)
	}
	// Telegram can refuse sooner than the adapter's window: a query that
	// "is too old" is expired too.
	r2 := newRig(t)
	ic2 := r2.ic("cbq-old")
	r2.f.expireCallback("acct1", "cbq-old")
	out, err := r2.respond(t, model.ResponseModeEphemeral, ic2, text("late"))
	wantRejected(t, out, err, model.DeadlineExpired)
	// And a second answer to one query is no answer.
	ic3 := r2.ic("cbq-twice")
	out, err = r2.respond(t, model.ResponseModeEphemeral, ic3, text("one"))
	mustAccept(t, out, err)
	out, err = r2.respond(t, model.ResponseModeEphemeral, ic3, text("two"))
	wantRejected(t, out, err, model.DeadlineExpired)
}

// A command carries no credential. A message answers it; a notification and
// an update have no query to ride on.
func TestRespondToACommand(t *testing.T) {
	r := newRig(t)
	ic := &adapter.InteractionContext{ProviderID: "command:424242:12", Kind: model.InteractionCommand}
	out, err := r.respond(t, model.ResponseModeMessage, ic, text("pong"))
	mustAccept(t, out, err)
	if got := methodsOf(r); got != "sendMessage" {
		t.Errorf("calls %s: with no query there is nothing to answer", got)
	}
	for _, mode := range []string{model.ResponseModeEphemeral, model.ResponseModeUpdate} {
		before := len(r.f.calls("acct1"))
		out, err = r.respond(t, mode, ic, text("x"), func(q *adapter.RespondRequest) { q.Target = ic.Message })
		wantRejected(t, out, err, model.Unsupported)
		if len(r.f.calls("acct1")) != before {
			t.Errorf("%s reached Telegram", mode)
		}
	}
}

func TestRespondOutcomes(t *testing.T) {
	for name, tc := range map[string]struct {
		mode   string
		script scripted
		check  func(t *testing.T, o adapter.Outcome, e error)
	}{
		"answer lost":  {model.ResponseModeEphemeral, answerLost, func(t *testing.T, o adapter.Outcome, e error) { _ = wantErr(t, e, model.OutcomeUnknown) }},
		"message lost": {model.ResponseModeMessage, answerLost, func(t *testing.T, o adapter.Outcome, e error) { _ = wantErr(t, e, model.OutcomeUnknown) }},
		"flood": {model.ResponseModeEphemeral, rateLimitedFor(2 * time.Second), func(t *testing.T, o adapter.Outcome, e error) {
			if me := wantErr(t, e, model.RateLimited); me.RetryAfter != 2*time.Second {
				t.Errorf("retry after %v", me.RetryAfter)
			}
		}},
		"unreachable": {model.ResponseModeMessage, unreachable, func(t *testing.T, o adapter.Outcome, e error) { _ = wantErr(t, e, model.Transient) }},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			ic := r.ic("cbq-9")
			r.f.respond("acct1", tc.script)
			out, err := r.respond(t, tc.mode, ic, text("x"))
			tc.check(t, out, err)
			if strings.Contains(joined(out, err), "cbq-9") || strings.Contains(joined(out, err), "AAtestToken") {
				t.Errorf("%q names a credential", joined(out, err))
			}
		})
	}
}

func TestRespondOnAnUnreadyInstance(t *testing.T) {
	r := newRig(t)
	ic := r.ic("cbq-9")
	_ = r.x.Close(t.Context())
	_, err := r.respond(t, model.ResponseModeMessage, ic, text("x"))
	_ = wantErr(t, err, model.Unavailable)
}
