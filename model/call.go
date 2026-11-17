package model

// RevisionRule states how a command uses Command.ExpectedRevision. Every
// command request documents its rule through a RevisionRule method; an empty
// revision is never an undocumented overwrite.
type RevisionRule uint8

const (
	// RevisionRequired: edits, deletes, explicit state or configuration changes
	// and link replacement. The zero value, so an unset rule fails closed.
	RevisionRequired RevisionRule = iota
	// RevisionOptional: append-only commands. When set, the engine compares it
	// with the target's current revision.
	RevisionOptional
	// RevisionAbsent: creation and token-guarded commands; a revision has
	// nothing to compare with and is rejected.
	RevisionAbsent
)

// Validate requires a correlation token. Correlation is diagnostic only.
func (c Call) Validate() error { return c.Correlation.Validate() }

// Validate checks the call, the operation ID token and the expected revision
// against rule.
func (c Command) Validate(rule RevisionRule) error {
	if err := firstError(c.Call.Validate(), c.Operation.Validate()); err != nil {
		return err
	}
	const field = "expected_revision"
	rev := string(c.ExpectedRevision)
	switch rule {
	case RevisionRequired:
		if rev == "" {
			return invalidField(field, "required by this operation")
		}
	case RevisionOptional:
	case RevisionAbsent:
		if rev != "" {
			return invalidField(field, "not accepted by this operation")
		}
	default:
		return invalidField(field, "unknown revision rule")
	}
	return checkOptionalToken(field, rev)
}
