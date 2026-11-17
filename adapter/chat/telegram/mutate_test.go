package telegram

import (
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func (r *rig) target(chat, id int64) *model.ExternalRef {
	ref := model.ProviderMessageRef(testChannel, messageID(chat, id))
	return &ref
}

func (r *rig) eff(chat, id int64) adapter.Effect {
	e := r.effect(r.room(chat))
	e.Target = r.target(chat, id)
	return e
}

// ownMessage is a message the bot sent to chat.
func (r *rig) ownMessage(chat int64, body string) int64 {
	return r.f.put("acct1", chat, testBotID, body, 0, 0).id
}

func TestEdit(t *testing.T) {
	r := newRig(t)
	id := r.ownMessage(testGroup, "first")
	out, err := r.x.Edit(t.Context(), adapter.EditRequest{Effect: r.eff(testGroup, id), Content: text("second <b>&</b>")})
	mustAccept(t, out, err)
	a := r.callsOf("editMessageText")[0].args()
	if a["chat_id"] != float64(testGroup) || a["message_id"] != float64(id) || a["text"] != "second <b>&</b>" {
		t.Errorf("args %v", a)
	}
	for _, k := range []string{"parse_mode", "reply_markup", "entities"} {
		if _, ok := a[k]; ok {
			t.Errorf("a plain edit carries %s", k)
		}
	}
	if m := r.f.find("acct1", testGroup, id); m.text != "second <b>&</b>" || !m.edited {
		t.Errorf("message %+v", m)
	}
	// An edit may carry markup and a keyboard, as a send does.
	kb := `{"inline_keyboard":[[{"text":"Ok","callback_data":"ok"}]]}`
	out, err = r.x.Edit(t.Context(), adapter.EditRequest{Effect: r.eff(testGroup, id), Content: content(markup(ParseModeHTML, "<i>third</i>"), keyboard(kb))})
	mustAccept(t, out, err)
	a = r.callsOf("editMessageText")[1].args()
	if a["parse_mode"] != "HTML" || a["text"] != "<i>third</i>" || a["reply_markup"] == nil {
		t.Errorf("args %v", a)
	}
}

// Editing a message to what it already says is what was asked: Telegram
// answers that the message is not modified, and the edit is applied.
func TestEditToTheSameTextIsApplied(t *testing.T) {
	r := newRig(t)
	id := r.ownMessage(testGroup, "same")
	out, err := r.x.Edit(t.Context(), adapter.EditRequest{Effect: r.eff(testGroup, id), Content: text("same")})
	mustAccept(t, out, err)
	if n := len(r.callsOf("editMessageText")); n != 1 {
		t.Errorf("%d calls", n)
	}
}

func TestEditRefusals(t *testing.T) {
	r := newRig(t)
	theirs := r.f.put("acct1", testGroup, 55, "theirs", 0, 0).id
	for name, tc := range map[string]struct {
		eff  adapter.Effect
		c    model.Content
		code model.ErrorCode
		call bool
	}{
		// A bot edits only its own messages.
		"another's message": {r.eff(testGroup, theirs), text("x"), model.Unauthorized, true},
		"gone":              {r.eff(testGroup, 99999), text("x"), model.NotFound, true},
		"no such chat":      {r.eff(-5, 1), text("x"), model.NotFound, true},
		"nothing to say":    {r.eff(testGroup, theirs), text(""), model.Validation, false},
		"attachment":        {r.eff(testGroup, theirs), model.Content{Parts: []model.Part{{Kind: model.PartAttachment}}}, model.Unsupported, false},
		"restricted": {func() adapter.Effect {
			e := r.eff(testGroup, theirs)
			e.Audience = model.Audience{Visibility: model.Restricted}
			return e
		}(), text("x"), model.Unsupported, false},
	} {
		t.Run(name, func(t *testing.T) {
			before := len(r.f.calls("acct1"))
			out, err := r.x.Edit(t.Context(), adapter.EditRequest{Effect: tc.eff, Content: tc.c})
			wantRejected(t, out, err, tc.code)
			if got := len(r.f.calls("acct1")) - before; tc.call != (got == 1) {
				t.Errorf("%d calls, want a call: %v", got, tc.call)
			}
		})
	}
	if m := r.f.find("acct1", testGroup, theirs); m.text != "theirs" {
		t.Errorf("a refused edit changed the message: %q", m.text)
	}
	// An edit never becomes a new message.
	if n := len(r.callsOf("sendMessage")); n != 0 {
		t.Errorf("%d sendMessage calls", n)
	}
}

func TestMutationTargetMustBeAMessageOfTheChannel(t *testing.T) {
	r := newRig(t)
	other := model.ProviderMessageRef(model.ChannelRef{Tenant: testTenant, ID: "00000ad0-0000-7000-8000-0000000000ff"}, "5:5")
	for name, e := range map[string]adapter.Effect{
		"no target": r.effect(r.room(testGroup)),
		"another channel": func() adapter.Effect {
			e := r.effect(r.room(testGroup))
			e.Target = &other
			return e
		}(),
		"not a message ref": func() adapter.Effect {
			e := r.effect(r.room(testGroup))
			ref := r.room(testGroup)
			e.Target = &ref
			return e
		}(),
		"with a parent": func() adapter.Effect {
			e := r.eff(testGroup, 5)
			e.Target.ParentID = "-100"
			return e
		}(),
		"no chat":      effWithTarget(r, "42"),
		"no message":   effWithTarget(r, "-100:"),
		"message zero": effWithTarget(r, "-100:0"),
		"padded":       effWithTarget(r, "-100:007"),
		"bad route": func() adapter.Effect {
			e := r.eff(testGroup, 5)
			e.Route = model.ExternalRef{Channel: testChannel, Namespace: "slack.chat", Kind: model.RefKindRoom, ID: "C1"}
			return e
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := r.x.Edit(t.Context(), adapter.EditRequest{Effect: e, Content: text("x")})
			_ = wantErr(t, err, model.Validation)
			_, err = r.x.Delete(t.Context(), adapter.DeleteRequest{Effect: e})
			_ = wantErr(t, err, model.Validation)
			_, err = r.x.React(t.Context(), adapter.ReactRequest{Effect: e, Emoji: "👍"})
			_ = wantErr(t, err, model.Validation)
			if n := len(r.f.calls("acct1")); n != 0 {
				t.Fatalf("%d calls reached Telegram", n)
			}
		})
	}
}

func effWithTarget(r *rig, id string) adapter.Effect {
	e := r.effect(r.room(testGroup))
	ref := model.ProviderMessageRef(testChannel, id)
	e.Target = &ref
	return e
}

func TestDelete(t *testing.T) {
	r := newRig(t)
	id := r.ownMessage(testGroup, "bye")
	out, err := r.x.Delete(t.Context(), adapter.DeleteRequest{Effect: r.eff(testGroup, id)})
	mustAccept(t, out, err)
	if r.f.find("acct1", testGroup, id) != nil {
		t.Error("message not deleted")
	}
	a := r.callsOf("deleteMessage")[0].args()
	if a["chat_id"] != float64(testGroup) || a["message_id"] != float64(id) {
		t.Errorf("args %v", a)
	}
	// Gone already: a rejection, not a success: nothing was deleted by this call.
	out, err = r.x.Delete(t.Context(), adapter.DeleteRequest{Effect: r.eff(testGroup, id)})
	wantRejected(t, out, err, model.NotFound)
}

// A message can only be deleted if it was sent less than 48 hours ago, and
// a bot deletes another's only with the right to.
func TestDeleteRefusals(t *testing.T) {
	r := newRig(t)
	theirs := r.f.put("acct1", testGroup, 55, "theirs", 0, 0).id
	out, err := r.x.Delete(t.Context(), adapter.DeleteRequest{Effect: r.eff(testGroup, theirs)})
	wantRejected(t, out, err, model.Unauthorized)
	r.acct.lenient = true
	old := r.f.put("acct1", testGroup, testBotID, "old", 0, 0)
	old.date = testNow.Add(-49 * time.Hour)
	out, err = r.x.Delete(t.Context(), adapter.DeleteRequest{Effect: r.eff(testGroup, old.id)})
	wantRejected(t, out, err, model.Unauthorized)
	if r.f.find("acct1", testGroup, old.id) == nil || r.f.find("acct1", testGroup, theirs) == nil {
		t.Error("a refused deletion removed a message")
	}
}

func TestReact(t *testing.T) {
	r := newRig(t)
	id := r.ownMessage(testGroup, "nice")
	react := func(emoji string, remove bool) (adapter.Outcome, error) {
		return r.x.React(t.Context(), adapter.ReactRequest{Effect: r.eff(testGroup, id), Emoji: emoji, Remove: remove})
	}
	out, err := react("👍", false)
	mustAccept(t, out, err)
	a := r.callsOf("setMessageReaction")[0].args()
	rs, _ := a["reaction"].([]any)
	if a["chat_id"] != float64(testGroup) || a["message_id"] != float64(id) || len(rs) != 1 {
		t.Fatalf("args %v", a)
	}
	if e, _ := rs[0].(map[string]any); e["type"] != "emoji" || e["emoji"] != "👍" {
		t.Errorf("reaction %v", rs[0])
	}
	if got := r.f.find("acct1", testGroup, id).reaction; len(got) != 1 || got[0] != "👍" {
		t.Errorf("the bot's reaction %v", got)
	}
	// The heart as most keyboards type it has a variation selector; Telegram
	// lists it without, and is sent so.
	out, err = react("\u2764\ufe0f", false)
	mustAccept(t, out, err)
	if rs := r.callsOf("setMessageReaction")[1].args()["reaction"].([]any); rs[0].(map[string]any)["emoji"] != "❤" {
		t.Errorf("reaction %v, want the listed form of the heart", rs)
	}
	// A removal sets no reaction: an empty list, sent explicitly.
	out, err = react("👍", true)
	mustAccept(t, out, err)
	a = r.callsOf("setMessageReaction")[2].args()
	if rs, ok := a["reaction"].([]any); !ok || len(rs) != 0 {
		t.Errorf("a removal's args %v, want an empty reaction list", a)
	}
	if got := r.f.find("acct1", testGroup, id).reaction; len(got) != 0 {
		t.Errorf("the bot's reaction %v after removal", got)
	}
}

func TestReactRefusals(t *testing.T) {
	r := newRig(t)
	id := r.ownMessage(testGroup, "x")
	// Telegram lists 73 emoji a bot may set: 😂 is not one of them, and no
	// call is made for it.
	out, err := r.x.React(t.Context(), adapter.ReactRequest{Effect: r.eff(testGroup, id), Emoji: "😂"})
	wantRejected(t, out, err, model.Unsupported)
	if n := len(r.f.calls("acct1")); n != 0 {
		t.Fatalf("%d calls", n)
	}
	// A chat may allow fewer than the full list: Telegram's refusal is final.
	r.f.respond("acct1", failWith(400, "Bad Request: REACTION_INVALID"))
	out, err = r.x.React(t.Context(), adapter.ReactRequest{Effect: r.eff(testGroup, id), Emoji: "👍"})
	wantRejected(t, out, err, model.Validation)
	out, err = r.x.React(t.Context(), adapter.ReactRequest{Effect: r.eff(testGroup, 424242), Emoji: "👍"})
	wantRejected(t, out, err, model.NotFound)
}

// An answer lost is not repeated: Telegram has no lookup, so the outcome
// stays unknown; a flood limit waits and a refused call changes nothing.
func TestMutationOutcomes(t *testing.T) {
	for name, s := range map[string]scripted{"answer lost": answerLost, "request lost": requestLost, "bad gateway": failWith(502, "Bad Gateway")} {
		r := newRig(t)
		id := r.ownMessage(testGroup, "x")
		for what, call := range map[string]func() error{
			"edit": func() error {
				_, e := r.x.Edit(t.Context(), adapter.EditRequest{Effect: r.eff(testGroup, id), Content: text("y")})
				return e
			},
			"delete": func() error {
				_, e := r.x.Delete(t.Context(), adapter.DeleteRequest{Effect: r.eff(testGroup, id)})
				return e
			},
			"react": func() error {
				_, e := r.x.React(t.Context(), adapter.ReactRequest{Effect: r.eff(testGroup, id), Emoji: "👍"})
				return e
			},
		} {
			r.f.respond("acct1", s)
			if me := wantErr(t, call(), model.OutcomeUnknown); strings.Contains(me.SafeMessage, "AAtestToken") {
				t.Errorf("%s %s: %q names the token", name, what, me.SafeMessage)
			}
		}
	}
	r := newRig(t)
	id := r.ownMessage(testGroup, "x")
	r.f.respond("acct1", rateLimitedFor(3*time.Second))
	_, err := r.x.Edit(t.Context(), adapter.EditRequest{Effect: r.eff(testGroup, id), Content: text("y")})
	if me := wantErr(t, err, model.RateLimited); me.RetryAfter != 3*time.Second {
		t.Errorf("retry after %v", me.RetryAfter)
	}
	if m := r.f.find("acct1", testGroup, id); m.text != "x" {
		t.Errorf("a rate-limited edit changed the message: %q", m.text)
	}
}

func TestSingleReactionPerReactor(t *testing.T) {
	// A bot "can set up to one reaction per message": a new reaction replaces
	// the earlier one, and a removal takes the one it holds whatever its
	// emoji, which core applies when the adapter declares it.
	if got := NewFactory(Options{}).Describe().Exchange.Reactions; got != adapter.ReactionsSingle {
		t.Errorf("Exchange.Reactions = %q, want single", got)
	}
}
