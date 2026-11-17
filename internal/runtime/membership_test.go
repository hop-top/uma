package runtime_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

const memberID model.ID = "00000006-0000-7000-8000-000000000001"

func storedConversation(t *testing.T, r *rig) model.Conversation {
	t.Helper()
	var c model.Conversation
	err := r.E.Store().View(r.Ctx, tenantA, func(rd store.Reader) (err error) {
		c, err = rd.GetConversation(r.Ctx, r.Conv.Ref)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func participates(c model.Conversation, id model.IdentityRef) bool {
	return slices.ContainsFunc(c.Participants, func(p model.Participant) bool { return p.Identity == id })
}

// membershipChanges returns the conversation.changed payloads that report
// participants.
func membershipChanges(t *testing.T, r *rig) []model.ConversationChangedPayload {
	t.Helper()
	var out []model.ConversationChangedPayload
	for _, ev := range r.Events() {
		if ev.Kind != model.EventConversationChanged {
			continue
		}
		var p model.ConversationChangedPayload
		if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
			t.Fatal(err)
		}
		if slices.Equal(p.Changes, []string{model.ConversationFieldParticipants}) {
			out = append(out, p)
		}
	}
	return out
}

// A membership change is authorized as conversation.membership at the call
// and again at execution, asks the provider once with the identity as the
// adapter reports it and no target, and changes the participants only once
// applied.
func TestMembershipAppliedOnceProviderAccepted(t *testing.T) {
	r := newRig(t, enginetest.WithMembership)
	id := r.SeedIdentity(memberID, "user-7")
	before := storedConversation(t, r)
	a, err := r.ChangeMembership("op-add", id.Ref, false)
	if err != nil {
		t.Fatalf("AddParticipant: %v", err)
	}
	if participates(storedConversation(t, r), id.Ref) {
		t.Fatal("participant added at the call")
	}
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	calls := r.F.MutationCalls()
	want := model.ExternalRef{Channel: r.Ch.Ref, Namespace: routeNSpace, Kind: string(model.KindIdentity), ID: "user-7"}
	if len(calls) != 1 || calls[0].Capability != model.CapabilityMembership || calls[0].Participant != want ||
		calls[0].Remove || calls[0].Effect.Target != nil {
		t.Fatalf("provider calls = %+v, want one add of %+v", calls, want)
	}
	if got := r.Policy.Actions(); countOf(got, model.ActionConversationMembership) != 2 {
		t.Fatalf("policy checks = %v, want conversation.membership at the call and at execution", got)
	}
	after := storedConversation(t, r)
	if !participates(after, id.Ref) || after.Revision == before.Revision {
		t.Fatalf("conversation = %+v, want the identity a participant at a new revision", after)
	}
	if evs := membershipChanges(t, r); len(evs) != 1 || evs[0].Operation == nil || *evs[0].Operation != a.Operation.Ref {
		t.Fatalf("participant events = %+v, want one under the operation", evs)
	}

	rm, err := r.ChangeMembership("op-remove", id.Ref, true)
	if err != nil {
		t.Fatalf("RemoveParticipant: %v", err)
	}
	r.Step("w")
	requireState(t, r.Op(rm.Operation.Ref), model.ProviderAccepted, "")
	if calls := r.F.MutationCalls(); len(calls) != 2 || !calls[1].Remove || calls[1].Participant != want {
		t.Fatalf("provider calls = %+v, want the add then the removal", calls)
	}
	if participates(storedConversation(t, r), id.Ref) {
		t.Fatal("participant still there after an applied removal")
	}
}

// A lost answer is reconciled with the participant and the direction before
// anything is asked again; the reconciled change then applies.
func TestMembershipLostAnswerIsReconciled(t *testing.T) {
	r := newRig(t, enginetest.WithMembership, withReconciler)
	id := r.SeedIdentity(memberID, "user-7")
	r.F.Respond(lostAnswer)
	a, err := r.ChangeMembership("op-add", id.Ref, false)
	if err != nil {
		t.Fatalf("AddParticipant: %v", err)
	}
	r.Step("w")
	if participates(storedConversation(t, r), id.Ref) {
		t.Fatal("participant added without an applied answer")
	}
	r.Clock.Advance(time.Minute)
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	if got := r.F.Calls(); !slices.Equal(got, []string{"membership", "reconcile"}) {
		t.Fatalf("calls = %v, want the change once, then reconciled", got)
	}
	rc := r.F.Reconciled()
	if len(rc) != 1 || rc[0].Capability != model.CapabilityMembership || rc[0].Participant.ID != "user-7" || rc[0].Remove {
		t.Fatalf("reconcile = %+v, want the membership capability and participant", rc)
	}
	if !participates(storedConversation(t, r), id.Ref) {
		t.Fatal("reconciled change not applied")
	}
}

// A permission revoked after the call makes no provider call and leaves the
// conversation as it is.
func TestMembershipRevokedPermissionMakesNoCall(t *testing.T) {
	r := newRig(t, enginetest.WithMembership)
	id := r.SeedIdentity(memberID, "user-7")
	a, err := r.ChangeMembership("op-add", id.Ref, false)
	if err != nil {
		t.Fatalf("AddParticipant: %v", err)
	}
	r.Policy.Set(true)
	r.Step("w")
	r.Policy.Set(false)
	requireState(t, r.Op(a.Operation.Ref), model.Failed, model.Unauthorized)
	if n := len(r.F.MutationCalls()); n != 0 {
		t.Fatalf("provider calls = %d, want none", n)
	}
	if participates(storedConversation(t, r), id.Ref) {
		t.Fatal("participant added without a provider call")
	}
}
