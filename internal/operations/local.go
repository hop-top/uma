package operations

import (
	"context"
	"encoding/json"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// LocalCommand is one local domain mutation entering the operation ledger:
// the operation claim, the mutation and its events commit in one unit with
// state model.Committed, and no provider work is queued. The caller
// authorizes before CommitLocal.
//
// The operation key is (tenant, caller, kind, target, operation ID), the
// caller being the client's principal.
type LocalCommand struct {
	Kind        string
	Target      model.Ref
	Command     model.Command
	Fingerprint []byte
	// Result is the resource the operation produces or changes, and
	// ResultTask the task it concerns, when it does.
	Result     model.Ref
	ResultTask *model.TaskRef
	// Apply checks mutable preconditions and writes the mutation inside the
	// claiming unit. It runs only for a new claim, never for a replay, and
	// must be free of effects outside the unit.
	Apply func(ctx context.Context, tx store.Tx, op model.OperationRef) error
}

// intentLocal names the stored intent of a local command, as the local
// services store it.
const intentLocal = "uma.intent.local"

type localIntent struct {
	Kind   string    `json:"kind"`
	Result model.Ref `json:"result"`
}

// CommitLocal claims c's operation and applies it in one unit under opID
// and rev. The same key with the same fingerprint returns the stored
// operation with Replayed set; with a different fingerprint it is
// model.Conflict.
func CommitLocal(ctx context.Context, cl *engine.Client, c LocalCommand, opID model.ID, rev model.Revision) (model.Acceptance, error) {
	if err := RefuseHold(c.Command); err != nil {
		return model.Acceptance{}, err
	}
	tenant := cl.Principal().Tenant
	opRef := model.OperationRef{Tenant: tenant, ID: opID}
	result := c.Result
	intent, err := json.Marshal(localIntent{Kind: c.Kind, Result: result})
	if err != nil {
		return model.Acceptance{}, model.NewError(model.Permanent, "operation intent cannot be stored")
	}
	rec := store.OperationRecord{
		Operation: model.Operation{Ref: opRef, State: model.Committed, Result: &result, ResultTask: c.ResultTask, Revision: rev},
		Key: store.OperationKey{
			Tenant: tenant, Caller: cl.Principal().Actor, Kind: c.Kind, Target: c.Target, ID: c.Command.Operation,
		},
		FingerprintVersion: FingerprintVersion1,
		Fingerprint:        c.Fingerprint,
		Command:            model.Extension{Name: intentLocal, Version: 1, Data: intent},
	}
	var got store.OperationRecord
	var created bool
	err = Transact(ctx, cl.Engine(), tenant, func(tx store.Tx) error {
		var err error
		got, created, err = tx.ClaimOperation(ctx, rec)
		if err != nil || !created {
			return err
		}
		return c.Apply(ctx, tx, opRef)
	})
	if err != nil {
		if model.IsCode(err, model.Conflict) && !created {
			return model.Acceptance{}, model.NewError(model.Conflict,
				"operation ID already used with a different payload")
		}
		return model.Acceptance{}, err
	}
	return model.Acceptance{
		Operation: Shown(got),
		Durable:   cl.Engine().Store().Properties().Durable,
		Replayed:  !created,
	}, nil
}
