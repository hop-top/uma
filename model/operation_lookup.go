package model

// OperationLookup finds the operation a command claimed, by the key the
// engine claimed it under: the principal's tenant and actor, Kind, Target
// and ID. It recovers an operation after a lost response. There is no
// tenant or caller field: the principal supplies both, so a caller finds
// only its own operations.
//
// Kind is the operation kind, the policy action the mutating method's
// godoc names in its operation key line; Target is the target that line
// names; ID is the command's Operation.
type OperationLookup struct {
	Call
	Kind   string      `json:"kind"`
	Target Ref         `json:"target"`
	ID     OperationID `json:"id"`
}

// Validate checks the call, the shape of Kind (a two-segment core action
// or a namespaced one, at most MaxNameLength bytes), Target as a complete
// reference and ID as an operation ID token. Whether Kind is a registered
// action is not checked: a lookup of a kind nothing registered finds
// nothing.
func (l OperationLookup) Validate() error {
	return firstError(
		l.Call.Validate(),
		checkActionName("kind", l.Kind),
		l.Target.validate("target", false),
		checkToken("id", string(l.ID)),
	)
}

// checkActionName checks a policy action name's shape: a two-segment core
// action, or a name ValidateExtensionName accepts.
func checkActionName(field, name string) error {
	switch {
	case name == "":
		return invalidField(field, "required")
	case len(name) > MaxNameLength:
		return invalidField(field, "exceeds maximum length")
	case builtinPairRE.MatchString(name):
		return nil
	case ValidateExtensionName(name) != nil:
		return invalidField(field, "must be a two-segment core action or a namespaced action name")
	}
	return nil
}
