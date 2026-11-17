package widget_test

import (
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
)

var interactionRef = model.InteractionRef{
	Conversation: model.ConversationRef{Inbox: inboxA, ID: "00000000-0000-7000-8000-0000000000d1"},
	ID:           "00000000-0000-7000-8000-0000000000e1",
}

const providerInteraction = "v0123456789abcdef0123456789abcdef"

// respondTo builds the request the engine hands the widget for an
// interaction of s: no token, as the widget's interactions have none.
func respondTo(r *rig, s widget.SessionResponse, mode string, audience model.Visibility, mod ...func(*adapter.RespondRequest)) adapter.RespondRequest {
	ref := widget.MessageRef(chanA, sourceMessage)
	req := adapter.RespondRequest{
		Effect: adapter.Effect{
			Operation: model.OperationRef{Tenant: tenant, ID: "00000000-0000-7000-8000-0000000000f1"},
			Route:     route(chanA, s), Audience: model.Audience{Visibility: audience},
			Interaction: &adapter.InteractionContext{Ref: interactionRef, ProviderID: providerInteraction, Kind: model.InteractionAction, Message: &ref},
		},
		Mode: mode, Content: text("done"),
	}
	if mode == model.ResponseModeUpdate {
		req.Target = &ref
	}
	for _, m := range mod {
		m(&req)
	}
	return req
}

func respond(t *testing.T, r *rig, req adapter.RespondRequest) (adapter.Outcome, error) {
	t.Helper()
	return r.inst.(adapter.InteractionResponder).Respond(t.Context(), req)
}

func TestRespondEventsInLog(t *testing.T) {
	log := widget.NewMemoryLog(0)
	r := newRig(t, widget.Options{Log: log})
	s := r.session("alice", widget.AssuranceVerified)
	live := r.connect(s.Token, "")

	msg, err := respond(t, r, respondTo(r, s, model.ResponseModeMessage, model.Public))
	if err != nil || msg.State != model.ProviderAccepted || len(msg.References) != 1 {
		t.Fatalf("message response = %+v, %v; want accepted with the new message", msg, err)
	}
	ev := live.next(t)
	if ev.Name != widget.EventInteractionResponse || ev.Data.Type != widget.EventInteractionResponse || ev.Data.Mode != model.ResponseModeMessage ||
		ev.Data.Interaction != providerInteraction || ev.Data.Message != msg.References[0].ID || onlyText(ev.Data) != "done" ||
		ev.Data.FollowUp || ev.Expires != nil {
		t.Fatalf("event = %+v, want an initial message response to the interaction", ev)
	}
	if want := widget.MessageRef(chanA, ev.Data.Message); msg.References[0] != want {
		t.Fatalf("reference = %+v, want the provider message reference %+v", msg.References[0], want)
	}

	eph, err := respond(t, r, respondTo(r, s, model.ResponseModeEphemeral, model.Restricted))
	if err != nil || eph.State != model.ProviderAccepted {
		t.Fatalf("ephemeral response = %+v, %v", eph, err)
	}
	ev = live.next(t)
	if ev.Data.Mode != model.ResponseModeEphemeral || ev.Data.Interaction != providerInteraction || ev.Expires == nil ||
		!ev.Expires.After(r.clock.Now()) || ev.Data.Message == "" {
		t.Fatalf("event = %+v, want an ephemeral response that expires", ev)
	}

	upd, err := respond(t, r, respondTo(r, s, model.ResponseModeUpdate, model.Public))
	if err != nil || upd.State != model.ProviderAccepted || len(upd.References) != 0 {
		t.Fatalf("update response = %+v, %v; want accepted with no new message", upd, err)
	}
	ev = live.next(t)
	if ev.Data.Mode != model.ResponseModeUpdate || ev.Data.Message != sourceMessage || onlyText(ev.Data) != "done" {
		t.Fatalf("event = %+v, want an update of the message the control sits on", ev)
	}

	follow, err := respond(t, r, respondTo(r, s, model.ResponseModeMessage, model.Public, func(q *adapter.RespondRequest) { q.FollowUp = true }))
	if err != nil || follow.State != model.ProviderAccepted {
		t.Fatalf("follow-up = %+v, %v", follow, err)
	}
	if ev = live.next(t); !ev.Data.FollowUp || ev.Data.Mode != model.ResponseModeMessage {
		t.Fatalf("event = %+v, want a follow-up", ev)
	}

	// The log keeps the engine interaction beside the event: the browser
	// never learns it.
	entries, err := log.Read(t.Context(), widget.LogKey{Channel: chanA, Namespace: widget.NamespaceUser, Visitor: "alice"}, 0, 10)
	if err != nil || len(entries) != 4 {
		t.Fatalf("log = %+v, %v", entries, err)
	}
	for _, e := range entries {
		if e.Interaction != interactionRef || e.Session != "" || e.Operation.ID == "" {
			t.Errorf("entry = %+v, want the interaction and operation of the response", e)
		}
	}
}

func TestRespondScopesToTheSessionOfARoomRoute(t *testing.T) {
	log := widget.NewMemoryLog(0)
	r := newRig(t, widget.Options{Log: log})
	s := r.session("alice", widget.AssuranceVerified)
	room := model.ExternalRef{Channel: chanA, Namespace: widget.NamespaceUser, Kind: model.RefKindRoom, ID: s.Session, ParentID: "alice"}
	req := respondTo(r, s, model.ResponseModeMessage, model.Public, func(q *adapter.RespondRequest) { q.Route = room })
	if out, err := respond(t, r, req); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Respond = %+v, %v", out, err)
	}
	entries, err := log.Read(t.Context(), widget.LogKey{Channel: chanA, Namespace: widget.NamespaceUser, Visitor: "alice"}, 0, 10)
	if err != nil || len(entries) != 1 || entries[0].Session != s.Session {
		t.Fatalf("log = %+v, %v; want the response limited to the session", entries, err)
	}
}

func TestRespondRefusals(t *testing.T) {
	log := widget.NewMemoryLog(0)
	r := newRig(t, widget.Options{Log: log})
	s := r.session("alice", widget.AssuranceVerified)
	for name, req := range map[string]adapter.RespondRequest{
		"public ephemeral":       respondTo(r, s, model.ResponseModeEphemeral, model.Public),
		"restricted message":     respondTo(r, s, model.ResponseModeMessage, model.Restricted),
		"internal update":        respondTo(r, s, model.ResponseModeUpdate, model.Internal),
		"update without target":  respondTo(r, s, model.ResponseModeUpdate, model.Public, func(q *adapter.RespondRequest) { q.Target = nil }),
		"update of a foreign ID": respondTo(r, s, model.ResponseModeUpdate, model.Public, func(q *adapter.RespondRequest) { q.Target = &model.ExternalRef{ID: "x"} }),
		"unknown mode":           respondTo(r, s, "broadcast", model.Public),
		"no interaction":         respondTo(r, s, model.ResponseModeMessage, model.Public, func(q *adapter.RespondRequest) { q.Interaction = nil }),
		"foreign route":          respondTo(r, s, model.ResponseModeMessage, model.Public, func(q *adapter.RespondRequest) { q.Route.Channel = chanB }),
	} {
		if _, err := respond(t, r, req); model.CodeOf(err) != model.Validation {
			t.Errorf("%s: Respond = %v, want validation", name, err)
		}
	}
	entries, _ := log.Read(t.Context(), widget.LogKey{Channel: chanA, Namespace: widget.NamespaceUser, Visitor: "alice"}, 0, 10)
	if len(entries) != 0 {
		t.Fatalf("log = %+v, want nothing delivered for a refused response", entries)
	}

	r.clock.Advance(time.Hour)
	_ = r.inst.Close(t.Context())
	if _, err := respond(t, r, respondTo(r, s, model.ResponseModeMessage, model.Public)); model.CodeOf(err) != model.Unavailable {
		t.Errorf("closed instance: Respond = %v, want unavailable", err)
	}
}
