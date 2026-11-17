package slack

import (
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// own stores a message of the bot in the fake workspace and returns the
// reference an effect names it by.
func (r *rig) own(conversation, text string) (model.ExternalRef, *fakeMsg) {
	m := r.f.put("acct1", conversation, testBotUser, text, "", "")
	m.botID = testBotID
	return model.ProviderMessageRef(testChannel, messageID(conversation, m.ts)), m
}

func (r *rig) target(conversation, user, text string) model.ExternalRef {
	m := r.f.put("acct1", conversation, user, text, "", "")
	return model.ProviderMessageRef(testChannel, messageID(conversation, m.ts))
}

func (r *rig) edit(t *testing.T, route model.ExternalRef, target *model.ExternalRef, c model.Content, mod ...func(*adapter.EditRequest)) (adapter.Outcome, error) {
	t.Helper()
	e := r.effect(route)
	e.Target = target
	req := adapter.EditRequest{Effect: e, Content: c}
	for _, m := range mod {
		m(&req)
	}
	return r.x.Edit(t.Context(), req)
}

func (r *rig) del(t *testing.T, route model.ExternalRef, target *model.ExternalRef) (adapter.Outcome, error) {
	t.Helper()
	e := r.effect(route)
	e.Target = target
	return r.x.Delete(t.Context(), adapter.DeleteRequest{Effect: e})
}

func (r *rig) react(t *testing.T, route model.ExternalRef, target *model.ExternalRef, emoji string, remove bool) (adapter.Outcome, error) {
	t.Helper()
	e := r.effect(route)
	e.Target = target
	return r.x.React(t.Context(), adapter.ReactRequest{Effect: e, Emoji: emoji, Remove: remove})
}

func TestEditReplacesTheMessage(t *testing.T) {
	r := newRig(t)
	ref, m := r.own(testRoom, "before")
	m.blocks = []byte(`[{"type":"divider"}]`)
	out, err := r.edit(t, r.x.roomRef(testRoom), &ref, text("after < before"))
	wantAccepted(t, out, err)
	calls := r.callsOf("chat.update")
	if len(calls) != 1 || !calls[0].Applied {
		t.Fatalf("calls = %+v, want one applied chat.update", calls)
	}
	a := calls[0].args(t)
	if a["channel"] != testRoom || a["ts"] != m.ts || a["text"] != "after &lt; before" {
		t.Errorf("arguments = %+v, want the message's conversation and ts and the escaped text", a)
	}
	got := r.f.find("acct1", testRoom, m.ts)
	if got.text != "after &lt; before" || got.blocks != nil {
		t.Errorf("message = %+v, want the text replaced and the old blocks gone: an edit is the whole replacement", got)
	}
	if got.meta == nil || got.meta.EventPayload["attempt"] != string(testAttempt) {
		t.Errorf("metadata = %+v, want the edit's attempt stamped", got.meta)
	}
	if len(out.References) != 0 {
		t.Errorf("references = %+v, want none", out.References)
	}
}

func TestEditWithBlocks(t *testing.T) {
	r := newRig(t)
	ref, m := r.own(testRoom, "before")
	c := model.Content{Parts: []model.Part{
		{Kind: model.PartText, Text: "fallback"},
		{Kind: model.PartStructured, Structured: &model.Extension{Name: BlocksSchema, Version: BlocksVersion, Data: []byte(`[{"type":"divider"}]`)}},
	}}
	out, err := r.edit(t, r.x.roomRef(testRoom), &ref, c)
	wantAccepted(t, out, err)
	if got := r.f.find("acct1", testRoom, m.ts); got.blocks == nil || got.text != "fallback" {
		t.Fatalf("message = %+v, want blocks and fallback text", got)
	}
}

func TestEditRefusals(t *testing.T) {
	r := newRig(t)
	ref, _ := r.own(testRoom, "mine")
	others := r.target(testRoom, testPerson, "theirs")
	missing := model.ProviderMessageRef(testChannel, testRoom+":1600000000.000001")
	room := r.x.roomRef(testRoom)

	out, err := r.edit(t, room, &others, text("x"))
	wantRejected(t, out, err, model.Unauthorized)
	out, err = r.edit(t, room, &missing, text("x"))
	wantRejected(t, out, err, model.NotFound)

	// Before any call: the audience, the content, the target.
	before := len(r.f.calls("acct1"))
	out, err = r.edit(t, room, &ref, text("x"), func(q *adapter.EditRequest) { q.Audience.Visibility = model.Restricted })
	wantRejected(t, out, err, model.Unsupported)
	out, err = r.edit(t, room, &ref, model.Content{Parts: []model.Part{{Kind: model.PartAttachment, Attachment: &model.AttachmentRef{}}}})
	wantRejected(t, out, err, model.Unsupported)
	for name, bad := range map[string]*model.ExternalRef{
		"no target":         nil,
		"another namespace": {Channel: testChannel, Namespace: Namespace, Kind: model.RefKindMessage, ID: testRoom + ":" + testTS},
		"a parent":          {Channel: testChannel, Namespace: model.ProviderMessageNamespace, Kind: model.RefKindMessage, ID: testRoom + ":" + testTS, ParentID: "x"},
		"a thread ref":      {Channel: testChannel, Namespace: Namespace, Kind: model.RefKindThread, ID: testTS, ParentID: testRoom},
		"another channel":   {Channel: model.ChannelRef{Tenant: testTenant, ID: "00000ad0-0000-7000-8000-00000000000b"}, Namespace: model.ProviderMessageNamespace, Kind: model.RefKindMessage, ID: testRoom + ":" + testTS},
		"a bare ts":         {Channel: testChannel, Namespace: model.ProviderMessageNamespace, Kind: model.RefKindMessage, ID: testTS},
		"no conversation":   {Channel: testChannel, Namespace: model.ProviderMessageNamespace, Kind: model.RefKindMessage, ID: "general:" + testTS},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := r.edit(t, room, bad, text("x"))
			wantError(t, out, err, model.Validation)
		})
	}
	if got := len(r.f.calls("acct1")); got != before {
		t.Fatalf("calls grew from %d to %d: refusals made calls", before, got)
	}
}

func TestDelete(t *testing.T) {
	r := newRig(t)
	ref, m := r.own(testRoom, "bye")
	out, err := r.del(t, r.x.roomRef(testRoom), &ref)
	wantAccepted(t, out, err)
	if r.f.find("acct1", testRoom, m.ts) != nil {
		t.Fatal("the message is still there")
	}
	if a := r.callsOf("chat.delete")[0].args(t); a["channel"] != testRoom || a["ts"] != m.ts {
		t.Errorf("arguments = %+v", a)
	}
	// Gone is gone: the target is no longer there for the account.
	out, err = r.del(t, r.x.roomRef(testRoom), &ref)
	wantRejected(t, out, err, model.NotFound)
	// A bot token deletes only what the bot posted.
	others := r.target(testRoom, testPerson, "theirs")
	out, err = r.del(t, r.x.roomRef(testRoom), &others)
	wantRejected(t, out, err, model.Unauthorized)
	out, err = r.del(t, r.x.roomRef(testRoom), nil)
	wantError(t, out, err, model.Validation)
}

func TestReact(t *testing.T) {
	r := newRig(t)
	ref, m := r.own(testRoom, "react to me")
	room := r.x.roomRef(testRoom)

	out, err := r.react(t, room, &ref, "👍", false)
	wantAccepted(t, out, err)
	if a := r.callsOf("reactions.add")[0].args(t); a["channel"] != testRoom || a["timestamp"] != m.ts || a["name"] != "thumbsup" {
		t.Errorf("arguments = %+v, want the message and the name thumbsup", a)
	}
	if got := r.f.find("acct1", testRoom, m.ts).reactions["thumbsup"]; len(got) != 1 || got[0] != testBotUser {
		t.Fatalf("reactions = %+v, want the bot's", got)
	}
	// Adding what the account already holds is what was asked.
	out, err = r.react(t, room, &ref, "👍", false)
	wantAccepted(t, out, err)
	// Another emoji is another reaction: a user may hold several.
	out, err = r.react(t, room, &ref, "🎉", false)
	wantAccepted(t, out, err)
	if got := r.f.find("acct1", testRoom, m.ts).reactions; len(got) != 2 {
		t.Fatalf("reactions = %+v, want two", got)
	}
	out, err = r.react(t, room, &ref, "👍", true)
	wantAccepted(t, out, err)
	if _, still := r.f.find("acct1", testRoom, m.ts).reactions["thumbsup"]; still {
		t.Fatal("the reaction is still there")
	}
	// Removing what is not there is what was asked.
	out, err = r.react(t, room, &ref, "👍", true)
	wantAccepted(t, out, err)
	if a := r.callsOf("reactions.remove")[0].args(t); a["name"] != "thumbsup" {
		t.Errorf("remove arguments = %+v", a)
	}
}

func TestReactRefusals(t *testing.T) {
	r := newRig(t)
	ref, _ := r.own(testRoom, "x")
	room := r.x.roomRef(testRoom)
	before := len(r.f.calls("acct1"))
	for _, emoji := range []string{"🦄", "👍🏽", "thumbsup", ":thumbsup:"} {
		out, err := r.react(t, room, &ref, emoji, false)
		wantRejected(t, out, err, model.Unsupported)
	}
	if got := len(r.f.calls("acct1")); got != before {
		t.Fatalf("an emoji Slack has no name for made %d calls", got-before)
	}
	missing := model.ProviderMessageRef(testChannel, testRoom+":1600000000.000001")
	out, err := r.react(t, room, &missing, "👍", false)
	wantRejected(t, out, err, model.NotFound)
	out, err = r.react(t, room, nil, "👍", false)
	wantError(t, out, err, model.Validation)
}

// A mutation Slack answers ambiguously is outcome unknown, never resent
// blindly; one it refuses is final.
func TestMutationsMapSlacksAnswers(t *testing.T) {
	mutations := map[string]func(*testing.T, *rig, model.ExternalRef) (adapter.Outcome, error){
		"edit": func(t *testing.T, r *rig, ref model.ExternalRef) (adapter.Outcome, error) {
			return r.edit(t, r.x.roomRef(testRoom), &ref, text("x"))
		},
		"delete": func(t *testing.T, r *rig, ref model.ExternalRef) (adapter.Outcome, error) {
			return r.del(t, r.x.roomRef(testRoom), &ref)
		},
		"react": func(t *testing.T, r *rig, ref model.ExternalRef) (adapter.Outcome, error) {
			return r.react(t, r.x.roomRef(testRoom), &ref, "👍", false)
		},
	}
	for name, do := range mutations {
		for what, tc := range map[string]struct {
			ans  answer
			want func(*testing.T, adapter.Outcome, error)
		}{
			"answer lost":  {answerLost, func(t *testing.T, o adapter.Outcome, e error) { wantError(t, o, e, model.OutcomeUnknown) }},
			"request lost": {requestLost, func(t *testing.T, o adapter.Outcome, e error) { wantError(t, o, e, model.OutcomeUnknown) }},
			"internal":     {failWith("internal_error"), func(t *testing.T, o adapter.Outcome, e error) { wantError(t, o, e, model.OutcomeUnknown) }},
			"unreachable":  {unreachable, func(t *testing.T, o adapter.Outcome, e error) { wantError(t, o, e, model.Transient) }},
			"rate limit":   {rateLimitedFor(7e9), func(t *testing.T, o adapter.Outcome, e error) { wantError(t, o, e, model.RateLimited) }},
			"refused":      {failWith("restricted_action"), func(t *testing.T, o adapter.Outcome, e error) { wantRejected(t, o, e, model.Unauthorized) }},
			"read-only":    {failWith("is_archived"), func(t *testing.T, o adapter.Outcome, e error) { wantRejected(t, o, e, model.Unauthorized) }},
			"gone":         {failWith("message_not_found"), func(t *testing.T, o adapter.Outcome, e error) { wantRejected(t, o, e, model.NotFound) }},
		} {
			t.Run(name+"/"+what, func(t *testing.T) {
				r := newRig(t)
				ref, _ := r.own(testRoom, "x")
				r.f.respond("acct1", tc.ans)
				out, err := do(t, r, ref)
				tc.want(t, out, err)
			})
		}
	}
}

func TestMutationsNeedAHost(t *testing.T) {
	r := newRig(t)
	ref, _ := r.own(testRoom, "x")
	_ = r.x.Close(t.Context())
	out, err := r.edit(t, r.x.roomRef(testRoom), &ref, text("x"))
	wantError(t, out, err, model.Unavailable)
	out, err = r.del(t, r.x.roomRef(testRoom), &ref)
	wantError(t, out, err, model.Unavailable)
	out, err = r.react(t, r.x.roomRef(testRoom), &ref, "👍", false)
	wantError(t, out, err, model.Unavailable)
}
