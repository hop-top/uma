package conformance

import (
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// The membership cases add and remove a participant through the engine
// (Conversations.AddParticipant, RemoveParticipant). The participant is an
// identity of the conversation's inbox that is not yet one of its
// participants: the sender of a second inbound message when the adapter
// receives, else an identity stored for the provider route to a second
// peer.

var memberID = model.ID("00000ad0-0000-7000-8000-0000000000e2")

// memberTarget is a conversation and an identity of its inbox that is no
// participant of it.
type memberTarget struct {
	conv model.Conversation
	id   model.Identity
}

// participantRef is the participant a membership call carries for the
// target's identity.
func (m memberTarget) participantRef() model.ExternalRef {
	return model.ExternalRef{Channel: adapterChanA, Namespace: m.id.Namespace, Kind: string(model.KindIdentity), ID: m.id.SourceID}
}

// memberTarget returns a membership target, with the worker started and
// idle.
func (e *adapterEnv) memberTarget(t *testing.T) memberTarget {
	t.Helper()
	if !e.canReceive() {
		conv := e.outbound(t)
		route := e.route(adapterChanA, "peer-2")
		id := model.Identity{
			Ref:     model.IdentityRef{Inbox: adapterInboxA, ID: memberID},
			Contact: model.ContactRef{Tenant: adapterTenant, ID: memberID}, SourceID: route.ID,
			Namespace: route.Namespace, Kind: model.ContactKindPerson, Revision: "id1",
		}
		err := e.store.Transact(e.ctx, adapterTenant, func(tx store.Tx) error {
			_, _, err := tx.ClaimIdentity(e.ctx, model.IdentityKey{Inbox: adapterInboxA, Namespace: id.Namespace, SourceID: id.SourceID}, id)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		e.start(t)
		return memberTarget{conv: e.storedConversation(t, conv.Ref), id: id}
	}
	g := e.inboundTarget(t, "")
	ev := e.message(adapterChanA, "member", "peer-2", "support")
	wantAcked(t, e.receiveAll(t, adapterChanA, ev))
	for _, id := range e.identities(t, adapterInboxA) {
		if id.SourceID == ev.Sender {
			return memberTarget{conv: e.storedConversation(t, g.conv.Ref), id: id}
		}
	}
	t.Skip("adapter stored no identity for the second sender")
	return memberTarget{}
}

func (e *adapterEnv) storedConversation(t *testing.T, ref model.ConversationRef) model.Conversation {
	t.Helper()
	c, err := e.client.Conversations().Get(e.ctx, model.Lookup[model.ConversationRef]{Call: model.Call{Correlation: "conformance-get"}, Ref: ref})
	if err != nil {
		t.Fatalf("Conversations.Get: %v", err)
	}
	return c
}

func (e *adapterEnv) changeMembership(m memberTarget, id model.OperationID, remove bool) (model.Acceptance, error) {
	req := model.ParticipantChange{Command: command(id), Conversation: m.conv.Ref, Identity: m.id.Ref}
	if remove {
		return e.client.Conversations().RemoveParticipant(e.ctx, req)
	}
	return e.client.Conversations().AddParticipant(e.ctx, req)
}

// mustChangeMembership submits a change that must be queued with the
// conversation untouched.
func (e *adapterEnv) mustChangeMembership(t *testing.T, m memberTarget, id model.OperationID, remove bool) model.OperationRef {
	t.Helper()
	before := e.storedConversation(t, m.conv.Ref)
	a, err := e.changeMembership(m, id, remove)
	if err != nil {
		t.Fatalf("membership change: %v", err)
	}
	if a.Operation.State != model.Queued {
		t.Fatalf("membership change state = %q, want queued", a.Operation.State)
	}
	if got := e.storedConversation(t, m.conv.Ref); got.Revision != before.Revision {
		t.Fatalf("conversation changed at submission: %+v", got)
	}
	return a.Operation.Ref
}

// wantMember requires the stored conversation to hold the target identity
// as a participant, or not.
func (e *adapterEnv) wantMember(t *testing.T, m memberTarget, member bool) model.Conversation {
	t.Helper()
	got := e.storedConversation(t, m.conv.Ref)
	if hasParticipant(got, m.id.Ref) != member {
		t.Fatalf("conversation participants = %+v, want %s a participant: %v", got.Participants, m.id.Ref.ID, member)
	}
	return got
}

// membershipEffect adds the target identity.
func (e *adapterEnv) membershipEffect(m memberTarget) queuedEffect {
	return queuedEffect{
		capability: model.CapabilityMembership,
		submit: func(t *testing.T, id model.OperationID) model.OperationRef {
			t.Helper()
			return e.mustChangeMembership(t, m, id, false)
		},
		untouched: func(t *testing.T) {
			t.Helper()
			if got := e.wantMember(t, m, false); got.Revision != m.conv.Revision {
				t.Fatalf("conversation revision %q, want it untouched at %q", got.Revision, m.conv.Revision)
			}
		},
		applied: func(t *testing.T) { t.Helper(); e.wantMember(t, m, true) },
	}
}

// wantMembershipCall requires call to be the applied change of ref on the
// conversation's route, carrying the target identity.
func wantMembershipCall(t *testing.T, call ProviderCall, ref model.OperationRef, m memberTarget, remove bool) {
	t.Helper()
	if !call.Applied || call.Operation != ref || call.Remove != remove {
		t.Fatalf("provider membership call = %+v, want one applied change (remove %v) of %v", call, remove, ref)
	}
	if call.Route != m.conv.ReplyRoute || call.Participant != m.participantRef() {
		t.Fatalf("membership change of %+v on route %+v, want %+v on %+v", call.Participant, call.Route, m.participantRef(), m.conv.ReplyRoute)
	}
}

var membershipCap = []model.Capability{model.CapabilityMembership}

var adapterMembershipCases = []adapterCase{
	{name: "MembershipAddThenRemove", needs: membershipCap, run: func(t *testing.T, e *adapterEnv) {
		m := e.memberTarget(t)
		added := e.mustChangeMembership(t, m, "op-add", false)
		a, err := e.changeMembership(m, "op-add", false)
		wantReplayed(t, a, err, added)
		e.wantMember(t, m, false)
		e.later(t, 0)
		wantActApplied(t, e.op(t, added))
		calls := e.calls(adapterChanA, model.CapabilityMembership)
		if len(calls) != 1 {
			t.Fatalf("provider membership calls = %+v, want one", calls)
		}
		wantMembershipCall(t, calls[0], added, m, false)
		after := e.wantMember(t, m, true)
		if after.Revision == m.conv.Revision {
			t.Fatal("an applied membership change kept the conversation revision")
		}
		a, err = e.changeMembership(m, "op-add", false)
		wantReplayed(t, a, err, added)
		wantActApplied(t, a.Operation)

		removed := e.mustChangeMembership(t, m, "op-remove", true)
		e.wantMember(t, m, true)
		e.later(t, 0)
		wantActApplied(t, e.op(t, removed))
		e.wantMember(t, m, false)
		e.later(t, time.Hour)
		calls = e.calls(adapterChanA, model.CapabilityMembership)
		if len(calls) != 2 {
			t.Fatalf("provider membership calls = %+v, want the addition then the removal, each once", calls)
		}
		wantMembershipCall(t, calls[1], removed, m, true)
		e.stop(t)
	}},
	{name: "MembershipRejectedLeavesConversation", needs: membershipCap, run: func(t *testing.T, e *adapterEnv) {
		e.rejectedIsFinal(t, e.membershipEffect(e.memberTarget(t)))
	}},
	{name: "MembershipAnswerLostIsNeverResent", needs: membershipCap, run: func(t *testing.T, e *adapterEnv) {
		e.answerLostNeverResent(t, e.membershipEffect(e.memberTarget(t)))
	}},
	{name: "MembershipRevokedPermissionMakesNoCall", needs: membershipCap, run: func(t *testing.T, e *adapterEnv) {
		e.revokedMakesNoCall(t, e.membershipEffect(e.memberTarget(t)))
	}},
}
