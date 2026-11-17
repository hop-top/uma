package operations

import (
	"context"
	"encoding/json"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Refetch operations. When two observations of one provider resource carry
// the same source revision with different content, ingress cannot order
// them: it keeps what it holds and records a refetch operation whose work
// asks the adapter's adapter.Refetcher for the resource's current state,
// which then applies as authoritative.

// IntentRefetch names the stored intent of a refetch operation.
const IntentRefetch = "uma.intent.refetch"

// KindRefetch is the operation kind of a refetch, which ingress records as
// the sink's service principal and a superseded edit's settle as the
// edit's caller.
const KindRefetch = "uma.provider.refetch"

// RefetchIntent is the durable intent of a refetch: the channel, the
// resource to read again, and the destination of the observation that
// tied, with that observation's references.
type RefetchIntent struct {
	Channel     model.ChannelRef    `json:"channel"`
	Target      model.ExternalRef   `json:"target"`
	Destination string              `json:"destination"`
	References  []model.ExternalRef `json:"references,omitempty"`
}

// Extension encodes the intent.
func (in RefetchIntent) Extension() (model.Extension, error) {
	data, err := json.Marshal(in)
	if err != nil {
		return model.Extension{}, model.NewError(model.Permanent, "refetch intent cannot be stored")
	}
	return model.Extension{Name: IntentRefetch, Version: 1, Data: data}, nil
}

// DecodeRefetchIntent reads a stored refetch intent.
func DecodeRefetchIntent(e model.Extension) (RefetchIntent, error) {
	var in RefetchIntent
	if e.Name != IntentRefetch || e.Version != 1 {
		return in, &model.Error{Code: model.SchemaMismatch, SafeMessage: "unknown operation intent", Retry: model.NoRetry}
	}
	if err := json.Unmarshal(e.Data, &in); err != nil {
		return in, &model.Error{Code: model.Permanent, SafeMessage: "stored operation intent unreadable", Retry: model.NoRetry}
	}
	return in, nil
}

// RefetchFingerprint is the version 1 fingerprint of a refetch: kind and
// target.
func RefetchFingerprint(resource model.Ref, target model.ExternalRef) []byte {
	f := NewFingerprint()
	f.Set("kind", Field().Str(KindRefetch))
	f.Set("target", Field().Ref(resource))
	f.Set("provider_target", Field().Str(target.Namespace).Str(target.Kind).Str(target.ID).Str(target.ParentID))
	return f.Sum()
}

// RefetchKeyPrefix starts the operation key ID of every refetch; the rest
// names what queued it (the ingress record or the mutation operation),
// so it is queued once. It also starts the event ID of each refetched
// copy.
const RefetchKeyPrefix = "uma.refetch."

// RecordRefetch records in tx a queued refetch operation of in.Target,
// the provider resource stored as resource, claimed by caller under key
// ID key, and its work. The refetch runs as caller, who needs
// model.ActionChannelReceive on the channel: there is no privileged
// runtime principal. A key already claimed records nothing, so it may
// run more than once.
func RecordRefetch(ctx context.Context, tx store.Tx, e *engine.Engine, caller model.ActorID, key model.OperationID, resource model.Ref, in RefetchIntent) error {
	ids := make([]model.ID, 3)
	for i := range ids {
		id, err := NewID(ctx, e)
		if err != nil {
			return err
		}
		ids[i] = id
	}
	intent, err := in.Extension()
	if err != nil {
		return err
	}
	tenant := in.Channel.Tenant
	opRef := model.OperationRef{Tenant: tenant, ID: ids[0]}
	res := resource
	_, created, err := tx.ClaimOperation(ctx, store.OperationRecord{
		Operation: model.Operation{Ref: opRef, State: model.Queued, Result: &res, Revision: model.Revision(ids[1])},
		Key: store.OperationKey{
			Tenant: tenant, Caller: caller, Kind: KindRefetch, Target: resource, ID: key,
		},
		FingerprintVersion: FingerprintVersion1,
		Fingerprint:        RefetchFingerprint(resource, in.Target),
		Command:            intent,
	})
	if err != nil || !created {
		return err
	}
	return tx.EnqueueWork(ctx, store.Work{ID: ids[2], Kind: WorkKindOperation, Resource: opRef.Ref()})
}
