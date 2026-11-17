package enginetest

import (
	"context"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// WithMembership declares the membership capability.
func WithMembership(_ *engine.Config, f *OutFactory) { f.Membership = true }

// ChangeMembership answers like a send, consuming the same script.
func (i *OutInstance) ChangeMembership(ctx context.Context, r adapter.MembershipRequest) (adapter.Outcome, error) {
	return i.mutate(ctx, MutationCall{Capability: model.CapabilityMembership, Effect: r.Effect, Participant: r.Participant, Remove: r.Remove})
}

// SeedIdentity stores an identity of the rig's inbox with source ID
// source in the route namespace.
func (r *Rig) SeedIdentity(id model.ID, source string) model.Identity {
	r.T.Helper()
	ident := model.Identity{
		Ref:     model.IdentityRef{Inbox: r.Conv.Ref.Inbox, ID: id},
		Contact: model.ContactRef{Tenant: TenantA, ID: id}, SourceID: source, Namespace: RouteNSpace,
		Kind: model.ContactKindPerson, Revision: "id1",
	}
	err := r.E.Store().Transact(r.Ctx, TenantA, func(tx store.Tx) error {
		_, _, err := tx.ClaimIdentity(r.Ctx, model.IdentityKey{Inbox: ident.Ref.Inbox, Namespace: ident.Namespace, SourceID: source}, ident)
		return err
	})
	if err != nil {
		r.T.Fatalf("seed identity: %v", err)
	}
	return ident
}

// ChangeMembership submits adding, or removing, identity id to the rig's
// conversation.
func (r *Rig) ChangeMembership(opID model.OperationID, id model.IdentityRef, remove bool) (model.Acceptance, error) {
	req := model.ParticipantChange{
		Command:      model.Command{Call: model.Call{Correlation: "corr-membership"}, Operation: opID},
		Conversation: r.Conv.Ref, Identity: id,
	}
	if remove {
		return r.C.Conversations().RemoveParticipant(r.Ctx, req)
	}
	return r.C.Conversations().AddParticipant(r.Ctx, req)
}
