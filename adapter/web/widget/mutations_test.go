package widget_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"slices"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
)

// visitorMessage is the ID shape of a message a visitor sent.
var visitorMessage = "v" + hex.EncodeToString([]byte("0123456789abcdef"))

func publicEffect(r *rig, s widget.SessionResponse, target string) adapter.Effect {
	ref := widget.MessageRef(chanA, target)
	return adapter.Effect{Route: route(chanA, s), Target: &ref, Audience: model.Audience{Visibility: model.Public}}
}

func TestMessageRefIsTheProviderMessageReference(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	out := r.send(route(chanA, s), "hello")
	if len(out.References) != 1 {
		t.Fatalf("references = %+v", out.References)
	}
	id := out.References[0].ID
	if want := model.ProviderMessageRef(chanA, id); out.References[0] != want {
		t.Fatalf("reference = %+v, want the provider message reference %+v", out.References[0], want)
	}
}

func TestDescribeDeclaresMutations(t *testing.T) {
	f, _ := widget.New(widget.Options{Verifier: headerVerifier})
	d := f.Describe()
	for _, c := range []model.Capability{model.CapabilityEdit, model.CapabilityDelete, model.CapabilityReact} {
		if !slices.Contains(d.Capabilities, c) {
			t.Errorf("capabilities = %v, want %s", d.Capabilities, c)
		}
	}
	if d.Exchange.Reactions != adapter.ReactionsMultiple {
		t.Errorf("reactions = %q, want multiple: the host holds one per emoji", d.Exchange.Reactions)
	}
	r := newRig(t, widget.Options{})
	statuses, err := r.inst.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []model.Capability{model.CapabilityEdit, model.CapabilityDelete, model.CapabilityReact} {
		if !slices.ContainsFunc(statuses, func(s model.CapabilityStatus) bool { return s.Name == c && s.Available }) {
			t.Errorf("statuses = %+v, want %s available", statuses, c)
		}
	}
}

func TestEditAppendsEditEvent(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	sent := r.send(route(chanA, s), "first")
	id := sent.References[0].ID
	out, err := r.inst.(adapter.Editor).Edit(t.Context(), adapter.EditRequest{Effect: publicEffect(r, s, id), Content: text("second")})
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 0 {
		t.Fatalf("Edit = %+v, %v; want accepted with no reference: an edit adds none", out, err)
	}
	live := r.connect(s.Token, "")
	live.next(t)
	ev := live.next(t)
	if ev.Name != widget.EventMessageEdit || ev.Data.Message != id || onlyText(ev.Data) != "second" || ev.ID != "2" {
		t.Fatalf("event = %+v, want %s of %s with the whole new content", ev, widget.EventMessageEdit, id)
	}
}

func TestEditOfVisitorMessageIsAllowed(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	out, err := r.inst.(adapter.Editor).Edit(t.Context(), adapter.EditRequest{Effect: publicEffect(r, s, visitorMessage), Content: text("fixed")})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Edit of a visitor's message = %+v, %v; want accepted: uma is the provider", out, err)
	}
	ev := r.connect(s.Token, "").next(t)
	if ev.Name != widget.EventMessageEdit || ev.Data.Message != visitorMessage {
		t.Fatalf("event = %+v", ev)
	}
}

func TestDeleteAppendsDeleteEvent(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	id := r.send(route(chanA, s), "oops").References[0].ID
	out, err := r.inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: publicEffect(r, s, id)})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Delete = %+v, %v", out, err)
	}
	live := r.connect(s.Token, "")
	live.next(t)
	ev := live.next(t)
	if ev.Name != widget.EventMessageDelete || ev.Data.Message != id || len(ev.Data.Parts) != 0 {
		t.Fatalf("event = %+v, want %s of %s without content", ev, widget.EventMessageDelete, id)
	}
}

func TestReactAppendsReactionAndRemoval(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	id := r.send(route(chanA, s), "nice").References[0].ID
	re := r.inst.(adapter.Reactor)
	for _, remove := range []bool{false, true} {
		out, err := re.React(t.Context(), adapter.ReactRequest{Effect: publicEffect(r, s, id), Emoji: "👍", Remove: remove})
		if err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("React remove=%v = %+v, %v", remove, out, err)
		}
	}
	live := r.connect(s.Token, "")
	live.next(t)
	for _, remove := range []bool{false, true} {
		ev := live.next(t)
		if ev.Name != widget.EventReaction || ev.Data.Message != id || ev.Data.Emoji != "👍" || ev.Data.Removed != remove {
			t.Fatalf("event = %+v, want reaction removed=%v on %s", ev, remove, id)
		}
	}
}

func TestMutationsRequirePublicAudience(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	id := r.send(route(chanA, s), "x").References[0].ID
	eff := publicEffect(r, s, id)
	eff.Audience = model.Audience{Visibility: model.Internal}
	calls := map[string]func() error{
		"edit": func() error {
			_, err := r.inst.(adapter.Editor).Edit(t.Context(), adapter.EditRequest{Effect: eff, Content: text("y")})
			return err
		},
		"delete": func() error {
			_, err := r.inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: eff})
			return err
		},
		"react": func() error {
			_, err := r.inst.(adapter.Reactor).React(t.Context(), adapter.ReactRequest{Effect: eff, Emoji: "👍"})
			return err
		},
	}
	for name, call := range calls {
		if model.CodeOf(call()) != model.Validation {
			t.Errorf("%s of a non-public message: want validation: visitors never see it", name)
		}
	}
	live := r.connect(s.Token, "")
	live.next(t)
	select {
	case ev := <-live.events:
		t.Fatalf("event %+v delivered for a refused mutation", ev)
	default:
	}
}

func TestMutationTargetMustBeThisChannel(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	id := r.send(route(chanA, s), "x").References[0].ID
	good := widget.MessageRef(chanA, id)
	bad := map[string]*model.ExternalRef{
		"none":            nil,
		"other channel":   ptr(model.ProviderMessageRef(chanB, id)),
		"other namespace": ptr(model.ExternalRef{Channel: chanA, Namespace: "com.example.chat", Kind: model.RefKindMessage, ID: id}),
		"other kind":      ptr(model.ExternalRef{Channel: chanA, Namespace: good.Namespace, Kind: model.RefKindRoom, ID: id}),
		"has parent":      ptr(model.ExternalRef{Channel: chanA, Namespace: good.Namespace, Kind: model.RefKindMessage, ID: id, ParentID: "p"}),
		"malformed id":    ptr(model.ProviderMessageRef(chanA, "not-a-message")),
		"empty id":        ptr(model.ProviderMessageRef(chanA, "")),
	}
	for name, target := range bad {
		eff := publicEffect(r, s, id)
		eff.Target = target
		if _, err := r.inst.(adapter.Editor).Edit(t.Context(), adapter.EditRequest{Effect: eff, Content: text("y")}); model.CodeOf(err) != model.Validation {
			t.Errorf("Edit with target %s = %v, want validation", name, err)
		}
		if _, err := r.inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: eff}); model.CodeOf(err) != model.Validation {
			t.Errorf("Delete with target %s = %v, want validation", name, err)
		}
		if _, err := r.inst.(adapter.Reactor).React(t.Context(), adapter.ReactRequest{Effect: eff, Emoji: "👍"}); model.CodeOf(err) != model.Validation {
			t.Errorf("React with target %s = %v, want validation", name, err)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestMutationRoutesFollowTheSendRules(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	id := r.send(route(chanA, s), "x").References[0].ID
	eff := publicEffect(r, s, id)
	eff.Route = model.ExternalRef{Channel: chanB, Namespace: widget.NamespaceUser, Kind: widget.KindUser, ID: "alice"}
	if _, err := r.inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: eff}); model.CodeOf(err) != model.Validation {
		t.Fatalf("Delete on a route of another channel = %v, want validation", err)
	}
	r.keys.revoke()
	if _, err := r.inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: publicEffect(r, s, id)}); model.CodeOf(err) != model.Unauthorized {
		t.Fatalf("Delete with a revoked key = %v, want unauthorized", err)
	}
}

func TestEventStreamReplaysMutations(t *testing.T) {
	r := newRig(t, widget.Options{})
	s := r.session("alice", widget.AssuranceVerified)
	id := r.send(route(chanA, s), "v1").References[0].ID
	eff := publicEffect(r, s, id)
	must := func(_ adapter.Outcome, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(r.inst.(adapter.Editor).Edit(t.Context(), adapter.EditRequest{Effect: eff, Content: text("v2")}))
	must(r.inst.(adapter.Reactor).React(t.Context(), adapter.ReactRequest{Effect: eff, Emoji: "🎉"}))
	must(r.inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: eff}))
	first := r.connect(s.Token, "")
	var want []string
	for range 4 {
		want = append(want, first.next(t).Name)
	}
	first.close()
	if !slices.Equal(want, []string{widget.EventMessage, widget.EventMessageEdit, widget.EventReaction, widget.EventMessageDelete}) {
		t.Fatalf("events = %v", want)
	}
	again := r.connect(s.Token, "1")
	for _, name := range want[1:] {
		if ev := again.next(t); ev.Name != name || ev.Data.Message != id {
			t.Fatalf("replay = %+v, want %s of %s", ev, name, id)
		}
	}
}

func TestMutationEventWireFormat(t *testing.T) {
	for want, ev := range map[string]widget.Event{
		`{"type":"message.edit","message":"m1","parts":[{"kind":"text","text":"hi"}]}`: {Type: widget.EventMessageEdit, Message: "m1", Parts: []widget.Part{{Kind: "text", Text: "hi"}}},
		`{"type":"message.delete","message":"m1"}`:                                     {Type: widget.EventMessageDelete, Message: "m1"},
		`{"type":"reaction","message":"m1","emoji":"👍"}`:                               {Type: widget.EventReaction, Message: "m1", Emoji: "👍"},
		`{"type":"reaction","message":"m1","emoji":"👍","removed":true}`:                {Type: widget.EventReaction, Message: "m1", Emoji: "👍", Removed: true},
	} {
		got, err := json.Marshal(ev)
		if err != nil || string(got) != want {
			t.Errorf("wire = %s, %v; want %s", got, err, want)
		}
	}
}

// refusingLog is a visitor log whose appends fail with err.
type refusingLog struct {
	*widget.MemoryLog
	err error
}

func (l refusingLog) Append(ctx context.Context, k widget.LogKey, e widget.Entry) (uint64, error) {
	return 0, l.err
}

func TestMutationRefusalKeepsItsCode(t *testing.T) {
	for _, code := range []model.ErrorCode{model.Unauthorized, model.NotFound, model.Unsupported} {
		r := newRig(t, widget.Options{Log: refusingLog{MemoryLog: widget.NewMemoryLog(0), err: model.NewError(code, "refused")}})
		s := r.session("alice", widget.AssuranceVerified)
		out, err := r.inst.(adapter.Editor).Edit(t.Context(), adapter.EditRequest{Effect: publicEffect(r, s, visitorMessage), Content: text("y")})
		if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != code || out.Failure.Retry != model.NoRetry {
			t.Errorf("Edit refused with %s = %+v, %v; want a final rejection with that code", code, out, err)
		}
	}
	r := newRig(t, widget.Options{Log: refusingLog{MemoryLog: widget.NewMemoryLog(0), err: model.NewError(model.Validation, "no")}})
	s := r.session("alice", widget.AssuranceVerified)
	out, err := r.inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: publicEffect(r, s, visitorMessage)})
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Permanent {
		t.Errorf("Delete refused otherwise = %+v, %v; want a permanent rejection", out, err)
	}
}

func TestMutationLostAnswerIsUnknown(t *testing.T) {
	r := newRig(t, widget.Options{Log: refusingLog{MemoryLog: widget.NewMemoryLog(0), err: context.DeadlineExceeded}})
	s := r.session("alice", widget.AssuranceVerified)
	out, err := r.inst.(adapter.Reactor).React(t.Context(), adapter.ReactRequest{Effect: publicEffect(r, s, visitorMessage), Emoji: "👍"})
	if err != nil || out.State != model.Unknown || out.Failure == nil || out.Failure.Code != model.OutcomeUnknown {
		t.Fatalf("React with a lost answer = %+v, %v; want outcome_unknown", out, err)
	}
}
