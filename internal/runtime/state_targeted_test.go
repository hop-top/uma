package runtime_test

import (
	"slices"
	"testing"
	"time"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
)

// targetedSubmit submits each state verb limited to msgs.
var targetedSubmit = []struct {
	name   string
	cap    model.Capability
	submit func(r *rig, op model.OperationID, msgs []model.ID) (model.Acceptance, error)
}{
	{"archive", model.CapabilityArchive, func(r *rig, op model.OperationID, m []model.ID) (model.Acceptance, error) {
		q := r.ArchiveReq(op, false)
		q.Messages = m
		return r.C.Conversations().Archive(r.Ctx, q)
	}},
	{"move", model.CapabilityMove, func(r *rig, op model.OperationID, m []model.ID) (model.Acceptance, error) {
		return r.C.Conversations().Move(r.Ctx, model.MoveConversation{
			Command:      model.Command{Call: model.Call{Correlation: "corr-state"}, Operation: op},
			Conversation: r.Conv.Ref, Destination: model.MoveDestination{ID: "f-archive"}, Messages: m,
		})
	}},
	{"read", model.CapabilityReadState, func(r *rig, op model.OperationID, m []model.ID) (model.Acceptance, error) {
		return r.C.Conversations().SetRead(r.Ctx, model.SetConversationRead{
			Command:      model.Command{Call: model.Call{Correlation: "corr-state"}, Operation: op},
			Conversation: r.Conv.Ref, Read: true, Messages: m,
		})
	}},
	{"flag", model.CapabilityFlag, func(r *rig, op model.OperationID, m []model.ID) (model.Acceptance, error) {
		return r.C.Conversations().Flag(r.Ctx, model.FlagConversation{
			Command:      model.Command{Call: model.Call{Correlation: "corr-state"}, Operation: op},
			Conversation: r.Conv.Ref, Kind: model.FlagStar, Messages: m,
		})
	}},
	{"trash", model.CapabilityTrash, func(r *rig, op model.OperationID, m []model.ID) (model.Acceptance, error) {
		return r.C.Conversations().Trash(r.Ctx, model.TrashConversation{
			Command:      model.Command{Call: model.Call{Correlation: "corr-state"}, Operation: op},
			Conversation: r.Conv.Ref, Messages: m,
		})
	}},
}

// A state change that names messages reaches the provider with exactly
// those messages as items, marked targeted; the rest of the conversation
// is left out.
func TestStateTargetedItemsAreExactlyTheNamedMessages(t *testing.T) {
	for _, v := range targetedSubmit {
		t.Run(v.name, func(t *testing.T) {
			r := newRig(t, enginetest.WithState(model.ScopeAccount))
			items := r.SeedProviderMessages(3)
			a, err := v.submit(r, "op-state", []model.ID{enginetest.SeededMessageID(2)})
			if err != nil {
				t.Fatal(err)
			}
			r.Step("w")
			requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
			calls := stateCalls(r)
			if len(calls) != 1 || calls[0].Capability != v.cap {
				t.Fatalf("calls = %+v, want one %s", calls, v.cap)
			}
			if got := calls[0].State; !got.Targeted || !slices.Equal(got.Items, items[1:2]) {
				t.Fatalf("items = %v targeted=%v, want only %v targeted", got.Items, got.Targeted, items[1:2])
			}
		})
	}
}

// Without messages the whole conversation is the item set and the request
// is not targeted.
func TestStateUntargetedItemsAreTheWholeConversation(t *testing.T) {
	r := newRig(t, enginetest.WithState(model.ScopeAccount))
	items := r.SeedProviderMessages(3)
	if _, err := r.Archive("op-state", false); err != nil {
		t.Fatal(err)
	}
	r.Step("w")
	calls := stateCalls(r)
	if len(calls) != 1 || calls[0].State.Targeted || !slices.Equal(calls[0].State.Items, items) {
		t.Fatalf("calls = %+v, want all of %v, untargeted", calls, items)
	}
}

// The named set is fixed at submission: a message that arrives before the
// worker runs is not added, and the set is the same on every attempt.
func TestStateTargetedSetIsFixedAtSubmit(t *testing.T) {
	r := newRig(t, enginetest.WithState(model.ScopeAccount))
	items := r.SeedProviderMessages(2)
	if _, err := targetedSubmit[0].submit(r, "op-state", []model.ID{enginetest.SeededMessageID(1)}); err != nil {
		t.Fatal(err)
	}
	r.SeedProviderMessage(3) // message 3 arrives before the worker runs
	r.Step("w")
	calls := stateCalls(r)
	if len(calls) != 1 || !slices.Equal(calls[0].State.Items, items[:1]) {
		t.Fatalf("calls = %+v, want only %v", calls, items[:1])
	}
}

// A message that is unknown, another conversation's or absent is a
// validation error and nothing is queued; so is a repeated one.
func TestStateTargetedRefusesUnknownMessages(t *testing.T) {
	for _, v := range targetedSubmit {
		t.Run(v.name, func(t *testing.T) {
			r := newRig(t, enginetest.WithState(model.ScopeAccount))
			r.SeedProviderMessages(2)
			for name, msgs := range map[string][]model.ID{
				"unknown":     {"00000005-0000-7000-8000-0000000009ff"},
				"one unknown": {enginetest.SeededMessageID(1), "nope"},
				"repeated":    {enginetest.SeededMessageID(1), enginetest.SeededMessageID(1)},
			} {
				_, err := v.submit(r, model.OperationID("op-"+name), msgs)
				wantCode(t, err, model.Validation)
			}
			if n := r.Step("w"); n != 0 {
				t.Fatalf("work items = %d, want nothing queued", n)
			}
			if n := len(stateCalls(r)); n != 0 {
				t.Fatalf("provider calls = %d", n)
			}
		})
	}
}

// A message of another conversation is not a member.
func TestStateTargetedRefusesAnotherConversationsMessage(t *testing.T) {
	r := newRig(t, enginetest.WithState(model.ScopeAccount))
	r.SeedProviderMessages(1)
	q := r.ArchiveReq("op-state", false)
	q.Messages = []model.ID{enginetest.SeededMessageID(1)}
	q.Conversation.ID = "00000004-0000-7000-8000-0000000000ee"
	_, err := r.C.Conversations().Archive(r.Ctx, q)
	if err == nil {
		t.Fatal("archive of a message under another conversation was accepted")
	}
}

// The same operation ID with another message set conflicts.
func TestStateTargetedPayloadConflict(t *testing.T) {
	r := newRig(t, enginetest.WithState(model.ScopeAccount))
	r.SeedProviderMessages(2)
	if _, err := targetedSubmit[0].submit(r, "op-state", []model.ID{enginetest.SeededMessageID(1)}); err != nil {
		t.Fatal(err)
	}
	_, err := targetedSubmit[0].submit(r, "op-state", []model.ID{enginetest.SeededMessageID(2)})
	wantCode(t, err, model.Conflict)
	_, err = r.Archive("op-state", false)
	wantCode(t, err, model.Conflict)
}

// A lost answer is reconciled with the targeted set, not the conversation.
func TestStateTargetedReconcileCarriesTheSet(t *testing.T) {
	r := newRig(t, enginetest.WithState(model.ScopeAccount), withReconciler)
	items := r.SeedProviderMessages(2)
	r.F.Respond(lostAnswer)
	if _, err := targetedSubmit[0].submit(r, "op-state", []model.ID{enginetest.SeededMessageID(2)}); err != nil {
		t.Fatal(err)
	}
	r.Step("w")
	r.Clock.Advance(time.Minute)
	r.Step("w")
	rc := r.F.Reconciled()
	if len(rc) != 1 || !rc[0].Targeted || !slices.Equal(rc[0].Items, items[1:2]) {
		t.Fatalf("reconcile = %+v, want only %v targeted", rc, items[1:2])
	}
}
