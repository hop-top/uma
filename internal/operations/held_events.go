package operations

import (
	"encoding/json"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// heldEvent returns the operation.held, operation.decided or
// operation.expired event of op, the held operation rec stores. Kind picks
// the schema. The payload holds references and names only: never the
// preview, the reason or any argument, so the event reveals no draft
// content. The audience is internal and the resource the operation; the
// event's inbox is the target's, empty for a target no inbox holds; the
// event gate additionally requires the read right of the operation's
// target (the payload's key).
func heldEvent(id model.ID, kind string, key store.OperationKey, op model.Operation,
	actor model.ActorID, correlation model.CorrelationID, at time.Time,
) (model.Event, error) {
	h := op.Held
	p := model.HeldOperationPayload{
		Operation: op.Ref,
		Verb:      key.Kind, Target: key.Target,
		Key:      model.OperationKey{Caller: key.Caller, Kind: key.Kind, Target: key.Target, ID: key.ID},
		Proposer: h.Proposer, TrustKey: h.TrustKey,
	}
	schema := model.SchemaOperationHeld
	switch kind {
	case model.EventOperationDecided:
		schema, p.Decision, p.DecidedBy = model.SchemaOperationDecided, h.Decision, h.DecidedBy
	case model.EventOperationExpired:
		schema, p.Decision = model.SchemaOperationExpired, model.HeldExpired
	}
	data, err := json.Marshal(p)
	if err != nil {
		return model.Event{}, model.NewError(model.Permanent, "held operation event cannot be encoded")
	}
	return model.Event{
		Version: model.EventEnvelopeVersion, ID: id, Kind: kind,
		Tenant: op.Ref.Tenant, Inbox: key.Target.Inbox, Resource: op.Ref.Ref(), Actor: actor,
		OccurredAt: at, RecordedAt: at, Correlation: correlation,
		Audience: model.Audience{Visibility: model.Internal}, Revision: op.Revision,
		Payload: model.Extension{Name: schema, Version: model.PayloadVersion1, Data: data},
	}, nil
}

// ExpiredEvent returns the operation.expired event of rec, the held
// operation whose deadline passed, now stored as model.Expired. The unit
// that writes the transition appends it, so state and event commit
// together. id is a fresh event ID.
func ExpiredEvent(id model.ID, rec store.OperationRecord, at time.Time) (model.Event, error) {
	return heldEvent(id, model.EventOperationExpired, rec.Key, rec.Operation, rec.Key.Caller, "", at.UTC())
}
