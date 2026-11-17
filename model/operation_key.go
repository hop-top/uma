package model

// OperationKey is the key an accepted command claimed in the operation
// ledger, as Operation.Key shows it: Caller, the actor of the principal
// that sent it (never a value from the request), Kind, the operation kind
// (the policy action the mutating method's godoc names on its "Operation
// key" line), Target, the target that line names, and ID, the command's
// Operation. The tenant is the operation's own.
//
// Kind, Target and ID are the fields of an OperationLookup: the caller
// recovers its operation with them. Lookup finds only the principal's own
// operations, so a key whose Caller is another actor recovers nothing.
type OperationKey struct {
	Caller ActorID     `json:"caller"`
	Kind   string      `json:"kind"`
	Target Ref         `json:"target"`
	ID     OperationID `json:"id"`
}
