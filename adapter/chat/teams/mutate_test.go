package teams

import (
	"net/http"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// sent posts a bot message to the group chat and returns its reference.
func (r *rig) sent(t *testing.T) model.ExternalRef {
	t.Helper()
	r.seed(testGroupChat)
	out, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(r.x.roomRef(testGroupChat)), Content: text("original")})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("send %+v %v", out, err)
	}
	return out.References[0]
}

func (r *rig) targeting(ref model.ExternalRef) adapter.Effect {
	e := r.effect(r.x.roomRef(testGroupChat))
	e.Target = &ref
	return e
}

func TestEditDeleteReactOwnMessage(t *testing.T) {
	r := newRig(t)
	ref := r.sent(t)
	_, id, _ := parseMessageID(ref.ID)
	if out, err := r.x.Edit(t.Context(), adapter.EditRequest{Effect: r.targeting(ref), Content: text("edited")}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("edit %+v %v", out, err)
	}
	if c := r.lastCall(t); c.Op != "update" || c.Conversation != testGroupChat || c.Activity != id || c.args()["id"] != id || c.args()["text"] != "edited" {
		t.Fatalf("edit call %+v %s", c, c.Body)
	}
	if m := r.f.find(testPrefix, testGroupChat, id); m.text != "edited" {
		t.Fatalf("message %q", m.text)
	}
	for _, remove := range []bool{false, true} {
		if out, err := r.x.React(t.Context(), adapter.ReactRequest{Effect: r.targeting(ref), Emoji: "👍", Remove: remove}); err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("react %+v %v", out, err)
		}
		c := r.lastCall(t)
		if c.Reaction != "like" || (c.Op == "unreact") != remove {
			t.Fatalf("react call %+v", c)
		}
	}
	if out, err := r.x.Delete(t.Context(), adapter.DeleteRequest{Effect: r.targeting(ref)}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("delete %+v %v", out, err)
	}
	if r.f.find(testPrefix, testGroupChat, id) != nil {
		t.Fatal("message still there")
	}
}

func TestMutationsOfOthersMessagesRejected(t *testing.T) {
	r := newRig(t)
	r.seed(testGroupChat)
	r.f.put(testPrefix, testGroupChat, testUser, "theirs", "1001")
	ref := model.ProviderMessageRef(testChannel, testGroupChat+"|1001")
	if out, err := r.x.Edit(t.Context(), adapter.EditRequest{Effect: r.targeting(ref), Content: text("mine now")}); err != nil || out.State != model.Rejected || out.Failure.Code != model.Unauthorized {
		t.Fatalf("edit %+v %v, want rejected unauthorized", out, err)
	}
	if out, err := r.x.Delete(t.Context(), adapter.DeleteRequest{Effect: r.targeting(ref)}); err != nil || out.State != model.Rejected || out.Failure.Code != model.Unauthorized {
		t.Fatalf("delete %+v %v, want rejected unauthorized", out, err)
	}
	gone := model.ProviderMessageRef(testChannel, testGroupChat+"|404")
	if out, err := r.x.Edit(t.Context(), adapter.EditRequest{Effect: r.targeting(gone), Content: text("x")}); err != nil || out.State != model.Rejected || out.Failure.Code != model.NotFound {
		t.Fatalf("edit of a missing message %+v %v, want rejected not_found", out, err)
	}
	for _, c := range r.f.calls(testPrefix) {
		if c.Op == "send" {
			t.Fatal("a refused mutation fell back to a send")
		}
	}
}

func TestReactionOutsideTheTypesRejectedBeforeAnyCall(t *testing.T) {
	r := newRig(t)
	ref := r.sent(t)
	n := len(r.f.calls(testPrefix))
	out, err := r.x.React(t.Context(), adapter.ReactRequest{Effect: r.targeting(ref), Emoji: "🦄"})
	if err != nil || out.State != model.Rejected || out.Failure.Code != model.Unsupported {
		t.Fatalf("%+v %v", out, err)
	}
	if len(r.f.calls(testPrefix)) != n {
		t.Fatal("an unsupported reaction reached Teams")
	}
}

func TestMutationTargetChecked(t *testing.T) {
	r := newRig(t)
	for name, target := range map[string]*model.ExternalRef{
		"none":          nil,
		"other channel": new(model.ProviderMessageRef(model.ChannelRef{Tenant: testTenant, ID: "00000ad0-0000-7000-8000-0000000000bb"}, testGroupChat+"|1")),
		"malformed":     new(model.ProviderMessageRef(testChannel, "no-separator")),
		"thread form":   new(model.ProviderMessageRef(testChannel, testTeamChannel+";messageid=5|1")),
	} {
		e := r.effect(r.x.roomRef(testGroupChat))
		e.Target = target
		if _, err := r.x.Delete(t.Context(), adapter.DeleteRequest{Effect: e}); model.CodeOf(err) != model.Validation {
			t.Fatalf("%s: %v, want validation", name, err)
		}
	}
	if n := len(r.f.calls(testPrefix)); n != 0 {
		t.Fatalf("calls %d", n)
	}
}

func TestEditAnswerLostIsOutcomeUnknown(t *testing.T) {
	r := newRig(t)
	ref := r.sent(t)
	r.f.respond(testPrefix, answerLost)
	if _, err := r.x.Edit(t.Context(), adapter.EditRequest{Effect: r.targeting(ref), Content: text("x")}); model.CodeOf(err) != model.OutcomeUnknown {
		t.Fatalf("%v", err)
	}
	r.f.respond(testPrefix, failWith(http.StatusTooManyRequests, "Throttled"))
	if _, err := r.x.React(t.Context(), adapter.ReactRequest{Effect: r.targeting(ref), Emoji: "❤️"}); model.CodeOf(err) != model.RateLimited {
		t.Fatalf("%v", err)
	}
}
