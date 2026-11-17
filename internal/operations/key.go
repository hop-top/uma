package operations

import (
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Shown returns a copy of rec's operation with model.Operation.Key set from
// the key it was claimed under, the tenant left out. Every path that
// returns an operation to a caller (an acceptance, Get, Lookup, List,
// Reconcile) builds it here; the key is never stored in the operation.
func Shown(rec store.OperationRecord) model.Operation {
	op := rec.Operation.Clone()
	k := rec.Key
	op.Key = &model.OperationKey{Caller: k.Caller, Kind: k.Kind, Target: k.Target, ID: k.ID}
	return op
}
