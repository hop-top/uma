package attachments

import (
	"context"
	"encoding/json"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// command is one local attachment mutation entering the operation ledger:
// the operation claim and the mutation commit in one unit with state
// model.Committed, and no provider work is queued. The caller authorizes
// before it, since Upload must authorize before reading any byte.
//
// The operation key is (tenant, caller, kind, target, operation ID), the
// caller being the client's principal.
type command struct {
	kind        string
	target      model.Ref
	result      model.Ref
	command     model.Command
	fingerprint []byte
	// apply checks mutable preconditions and writes the mutation inside
	// the claiming unit. It runs only for a new claim, never for a replay,
	// and must be free of effects outside the unit.
	apply func(ctx context.Context, tx store.Tx, op model.OperationRef) error
}

// commit claims c's operation and applies it in one unit, reporting whether
// this call created the claim. The same key with the same fingerprint
// returns the stored operation with Replayed set; with a different
// fingerprint it is model.Conflict.
func commit(ctx context.Context, cl *engine.Client, c command, opID model.ID, rev model.Revision) (model.Acceptance, bool, error) {
	if err := operations.RefuseHold(c.command); err != nil {
		return model.Acceptance{}, false, err
	}
	tenant := cl.Principal().Tenant
	opRef := model.OperationRef{Tenant: tenant, ID: opID}
	result := c.result
	intent, err := json.Marshal(localIntent{Kind: c.kind, Result: result})
	if err != nil {
		return model.Acceptance{}, false, model.NewError(model.Permanent, "operation intent cannot be stored")
	}
	rec := store.OperationRecord{
		Operation: model.Operation{Ref: opRef, State: model.Committed, Result: &result, Revision: rev},
		Key: store.OperationKey{
			Tenant: tenant, Caller: cl.Principal().Actor, Kind: c.kind, Target: c.target, ID: c.command.Operation,
		},
		FingerprintVersion: operations.FingerprintVersion1,
		Fingerprint:        c.fingerprint,
		Command:            model.Extension{Name: intentLocal, Version: 1, Data: intent},
	}
	var got store.OperationRecord
	var created bool
	err = operations.Transact(ctx, cl.Engine(), tenant, func(tx store.Tx) error {
		var err error
		got, created, err = tx.ClaimOperation(ctx, rec)
		if err != nil || !created {
			return err
		}
		return c.apply(ctx, tx, opRef)
	})
	if err != nil {
		if model.IsCode(err, model.Conflict) && !created {
			return model.Acceptance{}, false, model.NewError(model.Conflict,
				"operation ID already used with a different payload")
		}
		return model.Acceptance{}, false, err
	}
	return model.Acceptance{
		Operation: operations.Shown(got),
		Durable:   cl.Engine().Store().Properties().Durable,
		Replayed:  !created,
	}, created, nil
}

// intentLocal names the stored intent of a local command, as the other
// local services store it.
const intentLocal = "uma.intent.local"

type localIntent struct {
	Kind   string    `json:"kind"`
	Result model.Ref `json:"result"`
}
