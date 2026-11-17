package discord

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func TestEditDeleteReact(t *testing.T) {
	r := newRig(t)
	r.f.put("acct1", testRoom, testPerson, "x", "")
	sent, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(r.x.roomRef(testRoom)), Content: text("first")})
	if err != nil {
		t.Fatal(err)
	}
	e := r.effect(r.x.roomRef(testRoom))
	e.Target = &sent.References[0]
	_, id, _ := parseMessageID(e.Target.ID)

	if out, err := r.x.Edit(t.Context(), adapter.EditRequest{Effect: e, Content: text("second")}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Edit = %+v, %v", out, err)
	}
	if m := r.f.find("acct1", testRoom, id); m.content != "second" {
		t.Fatalf("content %q", m.content)
	}
	edit := r.callsOf(http.MethodPatch, "channels/"+testRoom+"/messages/"+id)[0].args(t)
	if comps, ok := edit["components"].([]any); !ok || len(comps) != 0 {
		t.Fatalf("edit body %v: components not cleared", edit)
	}

	for _, remove := range []bool{false, true} {
		if out, err := r.x.React(t.Context(), adapter.ReactRequest{Effect: e, Emoji: "👍", Remove: remove}); err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("React(remove %v) = %+v, %v", remove, out, err)
		}
	}
	reacts := r.callsOf(http.MethodPut, "channels/"+testRoom+"/messages/"+id+"/reactions/")
	if len(reacts) != 1 || reacts[0].Path != "channels/"+testRoom+"/messages/"+id+"/reactions/"+url.PathEscape("👍")+"/@me" {
		t.Fatalf("reaction calls %+v", reacts)
	}
	if len(r.callsOf(http.MethodDelete, "channels/"+testRoom+"/messages/"+id+"/reactions/")) != 1 {
		t.Fatal("no Delete Own Reaction")
	}

	if out, err := r.x.Delete(t.Context(), adapter.DeleteRequest{Effect: e}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Delete = %+v, %v", out, err)
	}
	if r.f.find("acct1", testRoom, id) != nil {
		t.Fatal("message not deleted")
	}
}

// Refusals are final rejections, never another effect.
func TestMutationRefusals(t *testing.T) {
	r := newRig(t)
	other := r.f.put("acct1", testRoom, testPerson, "theirs", "")
	e := r.effect(r.x.roomRef(testRoom))
	e.Target = msgRef(testRoom, other.id)
	before := len(r.f.calls("acct1"))
	if out, err := r.x.Edit(t.Context(), adapter.EditRequest{Effect: e, Content: text("mine now")}); err != nil || out.State != model.Rejected || out.Failure.Code != model.Unauthorized {
		t.Fatalf("Edit of another's message = %+v, %v", out, err)
	}
	if out, err := r.x.Delete(t.Context(), adapter.DeleteRequest{Effect: e}); err != nil || out.State != model.Rejected || out.Failure.Code != model.Unauthorized {
		t.Fatalf("Delete of another's message = %+v, %v", out, err)
	}
	calls := r.f.calls("acct1")[before:]
	if len(calls) != 2 || calls[0].Method != http.MethodPatch || calls[1].Method != http.MethodDelete {
		t.Fatalf("calls %+v: a refusal fell back to another effect", calls)
	}
	gone := r.effect(r.x.roomRef(testRoom))
	gone.Target = msgRef(testRoom, "1299999999999999999")
	for name, call := range map[string]func() (adapter.Outcome, error){
		"edit": func() (adapter.Outcome, error) {
			return r.x.Edit(t.Context(), adapter.EditRequest{Effect: gone, Content: text("x")})
		},
		"delete": func() (adapter.Outcome, error) { return r.x.Delete(t.Context(), adapter.DeleteRequest{Effect: gone}) },
		"react": func() (adapter.Outcome, error) {
			return r.x.React(t.Context(), adapter.ReactRequest{Effect: gone, Emoji: "👍"})
		},
	} {
		if out, err := call(); err != nil || out.State != model.Rejected || out.Failure.Code != model.NotFound {
			t.Errorf("%s of a message that is gone = %+v, %v", name, out, err)
		}
	}
	r.f.respond("acct1", refusal(404, codeUnknownEmoji), refusal(403, codeReactionBlocked))
	if out, err := r.x.React(t.Context(), adapter.ReactRequest{Effect: e, Emoji: "🫠"}); err != nil || out.State != model.Rejected || out.Failure.Code != model.Unsupported {
		t.Fatalf("unknown emoji = %+v, %v", out, err)
	}
	if out, err := r.x.React(t.Context(), adapter.ReactRequest{Effect: e, Emoji: "👍"}); err != nil || out.State != model.Rejected || out.Failure.Code != model.Unauthorized {
		t.Fatalf("blocked reaction = %+v, %v", out, err)
	}
	bad := r.effect(r.x.roomRef(testRoom))
	bad.Target = &model.ExternalRef{Channel: testChannel, Namespace: model.ProviderMessageNamespace, Kind: model.RefKindMessage, ID: "not-a-message"}
	if _, err := r.x.Edit(t.Context(), adapter.EditRequest{Effect: bad, Content: text("x")}); !model.IsCode(err, model.Validation) {
		t.Fatalf("malformed target = %v", err)
	}
}

func TestSignal(t *testing.T) {
	r := newRig(t)
	r.f.put("acct1", testRoom, testPerson, "x", "")
	req := adapter.SignalRequest{Route: r.x.roomRef(testRoom), Kind: "typing", Audience: model.Audience{Visibility: model.Public}, ExpiresAt: testNow.Add(5 * time.Second)}
	if err := r.x.Signal(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if len(r.callsOf(http.MethodPost, "channels/"+testRoom+"/typing")) != 1 {
		t.Fatal("no typing call")
	}
	req.Kind = "recording"
	if err := r.x.Signal(t.Context(), req); !model.IsCode(err, model.Unsupported) {
		t.Fatalf("other kind = %v", err)
	}
	req.Kind, req.ExpiresAt = "typing", testNow
	if err := r.x.Signal(t.Context(), req); !model.IsCode(err, model.DeadlineExpired) {
		t.Fatalf("expired = %v", err)
	}
	if n := len(r.callsOf(http.MethodPost, "channels/"+testRoom+"/typing")); n != 1 {
		t.Fatalf("typing calls %d, want only the first", n)
	}
}

func TestMembership(t *testing.T) {
	r := newRig(t)
	r.f.thread("acct1", testRoom, testThread)
	p := model.ExternalRef{Channel: testChannel, Namespace: UserNamespace, Kind: string(model.KindIdentity), ID: testOther}
	e := r.effect(r.x.threadRef(testRoom, testThread))
	for _, remove := range []bool{false, true} {
		out, err := r.x.ChangeMembership(t.Context(), adapter.MembershipRequest{Effect: e, Participant: p, Remove: remove})
		if err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("remove %v: %+v, %v", remove, out, err)
		}
		if r.acct.channels[testThread].members[testOther] == remove {
			t.Fatalf("remove %v: membership not applied", remove)
		}
		rec, err := r.x.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: e, Capability: model.CapabilityMembership, Participant: p, Remove: remove})
		if err != nil || rec.State != model.ProviderAccepted {
			t.Fatalf("reconcile remove %v: %+v, %v", remove, rec, err)
		}
		rec, _ = r.x.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: e, Capability: model.CapabilityMembership, Participant: p, Remove: !remove})
		if rec.State != model.Failed {
			t.Fatalf("reconcile of the opposite change = %+v, want not applied", rec)
		}
	}
	room := r.effect(r.x.roomRef(testRoom))
	if out, err := r.x.ChangeMembership(t.Context(), adapter.MembershipRequest{Effect: room, Participant: p}); err != nil || out.State != model.Rejected || out.Failure.Code != model.Unsupported {
		t.Fatalf("room membership = %+v, %v", out, err)
	}
	r.f.respond("acct1", refusal(403, codeMissingPermissions))
	if out, err := r.x.ChangeMembership(t.Context(), adapter.MembershipRequest{Effect: e, Participant: p, Remove: true}); err != nil || out.State != model.Rejected || out.Failure.Code != model.Unauthorized {
		t.Fatalf("denied removal = %+v, %v", out, err)
	}
}
