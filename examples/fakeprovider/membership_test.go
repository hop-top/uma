package fakeprovider_test

import (
	"slices"
	"testing"

	"hop.top/uma/examples/fakeprovider"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// A participant added and removed through the worker: the provider gets
// each change once with the identity as the adapter reports it, the
// conversation's participants follow only applied changes, and a refused
// change leaves them as they were.
func TestRuntimeMembershipLifecycle(t *testing.T) {
	h := newHost(t, fakeprovider.NewFull())
	ada := model.Identity{
		Ref:     model.IdentityRef{Inbox: inboxRef, ID: "00000000-0000-7000-8000-0000000000f1"},
		Contact: model.ContactRef{Tenant: tenant, ID: "00000000-0000-7000-8000-0000000000f2"}, SourceID: "ada",
		Namespace: fakeprovider.Namespace, Kind: model.ContactKindPerson, Revision: "id1",
	}
	err := h.store.Transact(h.ctx, tenant, func(tx store.Tx) error {
		_, _, err := tx.ClaimIdentity(h.ctx, model.IdentityKey{Inbox: inboxRef, Namespace: ada.Namespace, SourceID: ada.SourceID}, ada)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	change := func(op model.OperationID, remove bool) model.Acceptance {
		t.Helper()
		req := model.ParticipantChange{
			Command: model.Command{Call: model.Call{Correlation: "c-membership"}, Operation: op}, Conversation: convRef, Identity: ada.Ref,
		}
		a, err := h.client.Conversations().AddParticipant(h.ctx, req)
		if remove {
			a, err = h.client.Conversations().RemoveParticipant(h.ctx, req)
		}
		if err != nil {
			t.Fatalf("membership change %s: %v", op, err)
		}
		return a
	}
	participant := func() bool {
		t.Helper()
		c, err := h.client.Conversations().Get(h.ctx, model.Lookup[model.ConversationRef]{Call: model.Call{Correlation: "get"}, Ref: convRef})
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(c.Participants, func(p model.Participant) bool { return p.Identity == ada.Ref })
	}
	changes := func() []fakeprovider.Call {
		var out []fakeprovider.Call
		for _, c := range h.factory.Script(chanA).Calls() {
			if c.Method == fakeprovider.MethodMembership {
				out = append(out, c)
			}
		}
		return out
	}

	h.factory.Script(chanA).Respond(fakeprovider.Accepted, fakeprovider.Rejected, fakeprovider.Accepted)
	add := change("op-add", false)
	h.start("worker")
	if op := h.op(add.Operation.Ref); op.State != model.ProviderAccepted || !participant() {
		t.Fatalf("add = %+v, participant %v; want it applied", op, participant())
	}
	refused := change("op-remove-refused", true)
	h.later()
	if op := h.op(refused.Operation.Ref); op.State != model.Rejected || !participant() {
		t.Fatalf("refused removal = %+v, participant %v; want rejected and the participant kept", op, participant())
	}
	removed := change("op-remove", true)
	h.later()
	h.stop()
	if op := h.op(removed.Operation.Ref); op.State != model.ProviderAccepted || participant() {
		t.Fatalf("removal = %+v, participant %v; want it applied", op, participant())
	}
	want := model.ExternalRef{Channel: chanA, Namespace: fakeprovider.Namespace, Kind: string(model.KindIdentity), ID: "ada"}
	got := changes()
	if len(got) != 3 || got[0].Participant != want || got[0].Remove || !got[1].Remove || !got[2].Remove || got[0].Effect.Target != nil {
		t.Fatalf("provider membership calls = %+v, want add, refused removal, removal of %+v", got, want)
	}
}
