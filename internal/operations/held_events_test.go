package operations_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

func heldEvents(r *enginetest.Rig, kind string) []model.Event {
	var out []model.Event
	for _, ev := range r.Events() {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func heldPayload(t *testing.T, ev model.Event) model.HeldOperationPayload {
	t.Helper()
	var p model.HeldOperationPayload
	if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHeldEmitsOnceNotOnReplayOrDuplicate(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	held := propose(t, r, c, proposal(r, "p-1", "fp-1"))
	evs := heldEvents(r, model.EventOperationHeld)
	if len(evs) != 1 {
		t.Fatalf("operation.held events = %d, want 1", len(evs))
	}
	ev := evs[0]
	p := heldPayload(t, ev)
	if ev.Resource != held.Operation.Ref.Ref() || ev.Payload.Name != model.SchemaOperationHeld ||
		ev.Payload.Version != model.PayloadVersion1 || ev.Audience.Visibility != model.Internal ||
		ev.Revision != held.Operation.Revision || ev.Actor != plugin {
		t.Fatalf("event = %+v", ev)
	}
	if p.Operation != held.Operation.Ref || p.Key != *held.Operation.Key || p.Proposer != plugin ||
		p.TrustKey != "triage/reply" || p.Decision != "" || p.DecidedBy != "" {
		t.Fatalf("payload = %+v", p)
	}
	for _, secret := range []string{"would", "send hi", "first contact", `"text"`} {
		if strings.Contains(string(ev.Payload.Data), secret) {
			t.Fatalf("payload %s leaks draft content %q", ev.Payload.Data, secret)
		}
	}
	// A replay and a duplicate-guard hit write no second event.
	propose(t, r, c, proposal(r, "p-1", "fp-1"))
	propose(t, r, c, proposal(r, "p-2", "fp-2"))
	if n := len(heldEvents(r, model.EventOperationHeld)); n != 1 {
		t.Fatalf("operation.held events after replay and duplicate = %d, want 1", n)
	}
}

func TestDecidedEmitsPerDecisionNotOnReplay(t *testing.T) {
	operations.RegisterValidator("test.intent", func(context.Context, store.Reader, store.OperationKey, model.Extension, model.Extension) error {
		return nil
	})
	t.Cleanup(func() { operations.UnregisterValidator("test.intent") })
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	approve := propose(t, r, c, proposal(r, "p-1", "fp-1")).Operation
	reject := propose(t, r, c, withTarget(r, proposal(r, "p-2", "fp-2"), model.ActionMessageNote)).Operation
	edit := propose(t, r, c, withTarget(r, proposal(r, "p-3", "fp-3"), model.ActionConversationTransition)).Operation

	if _, err := r.C.Operations().Approve(r.Ctx, decide(approve)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.C.Operations().Reject(r.Ctx, decide(reject)); err != nil {
		t.Fatal(err)
	}
	args := model.Extension{Name: "test.intent", Version: 1, Data: []byte(`{"text":"rewritten body"}`)}
	if _, err := r.C.Operations().Edit(r.Ctx, model.EditHeld{DecideHeld: decide(edit), Args: args}); err != nil {
		t.Fatal(err)
	}
	// Replays of each decision write nothing.
	if got, err := r.C.Operations().Approve(r.Ctx, decide(approve)); err != nil || !got.Replayed {
		t.Fatalf("replay = %+v, %v", got, err)
	}
	if got, err := r.C.Operations().Reject(r.Ctx, decide(reject)); err != nil || !got.Replayed {
		t.Fatalf("replay = %+v, %v", got, err)
	}
	evs := heldEvents(r, model.EventOperationDecided)
	if len(evs) != 3 {
		t.Fatalf("operation.decided events = %d, want 3", len(evs))
	}
	want := []model.HeldDecision{model.HeldApproved, model.HeldRejected, model.HeldEdited}
	for i, ev := range evs {
		p := heldPayload(t, ev)
		if p.Decision != want[i] || p.DecidedBy != enginetest.Actor || p.Proposer != plugin ||
			p.TrustKey != "triage/reply" || ev.Actor != enginetest.Actor || ev.Payload.Name != model.SchemaOperationDecided {
			t.Fatalf("event %d = %+v payload %+v", i, ev, p)
		}
		if strings.Contains(string(ev.Payload.Data), "rewritten") || strings.Contains(string(ev.Payload.Data), "first contact") {
			t.Fatalf("payload %s leaks content", ev.Payload.Data)
		}
	}
	if evs[1].Revision == evs[0].Revision {
		t.Fatal("decided events share a revision")
	}
}

// A decision that is refused writes no event.
func TestRefusedDecisionEmitsNothing(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	held := propose(t, r, c, proposal(r, "p-1", "fp-1")).Operation
	stale := decide(held)
	stale.ExpectedRevision = "old"
	if _, err := r.C.Operations().Approve(r.Ctx, stale); err == nil {
		t.Fatal("stale approve succeeded")
	}
	if n := len(heldEvents(r, model.EventOperationDecided)); n != 0 {
		t.Fatalf("operation.decided events = %d, want 0", n)
	}
}

func TestExpiredEventCarriesNoDecider(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	held := propose(t, r, c, proposal(r, "p-1", "fp-1")).Operation
	var rec store.OperationRecord
	err := r.E.Store().View(r.Ctx, enginetest.TenantA, func(rd store.Reader) error {
		var err error
		rec, err = rd.Operation(r.Ctx, held.Ref)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	rec.Operation.State = model.Expired
	ev, err := operations.ExpiredEvent("00000000-0000-7000-8000-0000000000e1", rec, r.Clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	p := heldPayload(t, ev)
	if ev.Kind != model.EventOperationExpired || ev.Payload.Name != model.SchemaOperationExpired ||
		p.Decision != model.HeldExpired || p.DecidedBy != "" || p.Operation != held.Ref ||
		p.Key != *held.Key || p.Proposer != plugin || p.TrustKey != "triage/reply" ||
		ev.Resource != held.Ref.Ref() || ev.Audience.Visibility != model.Internal {
		t.Fatalf("event = %+v payload %+v", ev, p)
	}
	if strings.Contains(string(ev.Payload.Data), "would") {
		t.Fatalf("payload %s leaks preview", ev.Payload.Data)
	}
}

// A reader without the target's read right receives no held event of any
// kind, however it may read operations.
func TestHeldEventsFollowTargetRead(t *testing.T) {
	r := newRig(t, func(c *engine.Config, _ *enginetest.OutFactory) {
		c.Authorizer = denyReads{actor: "blind", actions: []string{model.ActionConversationRead}}
	})
	prop := pluginClient(t, r, plugin)
	held := propose(t, r, prop, proposal(r, "p-1", "fp-1")).Operation
	if _, err := r.C.Operations().Approve(r.Ctx, decide(held)); err != nil {
		t.Fatal(err)
	}
	kinds := []string{model.EventOperationHeld, model.EventOperationDecided, model.EventOperationExpired}
	list := func(c enginetest.Client) []model.Event {
		p, err := c.Events().List(r.Ctx, model.ListEvents{
			Call: model.Call{Correlation: "l"}, Filter: model.Filter{Kinds: kinds}, Start: model.StartBeginning,
		})
		if err != nil {
			t.Fatal(err)
		}
		return p.Items
	}
	if got := list(pluginClient(t, r, "sighted")); len(got) != 2 {
		t.Fatalf("a reader of the target sees %d held events, want 2", len(got))
	}
	if got := list(pluginClient(t, r, "blind")); len(got) != 0 {
		t.Fatalf("a reader without the target read right sees %d held events: %+v", len(got), got)
	}
}

// withTarget returns s keyed on another operation kind, so a proposal for
// the same conversation is no duplicate of a held one.
func withTarget(_ *enginetest.Rig, s operations.Submission, kind string) operations.Submission {
	s.Kind = kind
	return s
}
