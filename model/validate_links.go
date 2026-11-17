package model

import "regexp"

// reasonCodeRE is one lowercase name segment: the reason code grammar.
var reasonCodeRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_-]{0,61}[a-z0-9])?$`)

// checkReasonCode requires a reason code: one lowercase name segment of
// letters, digits, underscores and hyphens, at most 63 bytes, starting and
// ending with a letter or digit. Free text is refused, so a code that
// outlives redaction cannot carry personal data.
func checkReasonCode(field, s string) error {
	switch {
	case s == "":
		return invalidField(field, "required")
	case !reasonCodeRE.MatchString(s):
		return invalidField(field, "must be a reason code: one lowercase name segment")
	}
	return nil
}

// RevisionRule is RevisionRequired: compared with the identity's revision.
func (LinkIdentity) RevisionRule() RevisionRule { return RevisionRequired }

// Validate checks the identity, the full target contact ref and the
// evidence shape. Evidence is required and declares no assurance.
func (r LinkIdentity) Validate() error {
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Identity.validate("identity", false),
		r.Contact.validate("contact", false),
		r.Evidence.validateRequest("evidence", true),
	)
}

// RevisionRule is RevisionRequired: compared with the identity's revision.
func (UnlinkIdentity) RevisionRule() RevisionRule { return RevisionRequired }

// Validate checks the identity, the reason code and, when any field is set,
// the evidence shape. Zero evidence is valid; evidence declares no
// assurance.
func (r UnlinkIdentity) Validate() error {
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Identity.validate("identity", false),
		checkReasonCode("reason", r.Reason),
		r.Evidence.validateRequest("evidence", false),
	)
}

// Validate checks a verification request: a declared intent, full identity
// and current contact refs and the expected revision; for link and relink
// a target other than the current contact, no reason and required
// evidence; for unlink no target, a reason code and optional evidence.
// Evidence never declares an assurance.
func (r LinkRequest) Validate() error {
	if err := r.Intent.validate("intent"); err != nil {
		return err
	}
	if r.Intent == LinkClassMerge {
		return invalidField("intent", "must be link, relink or unlink: a merge is verified by no verifier")
	}
	unlink := r.Intent == LinkClassUnlink
	var target, reason error
	switch {
	case unlink && r.Target != nil:
		target = invalidField("target", "must be absent for unlink")
	case unlink:
	case r.Target == nil:
		target = invalidField("target", "required for link and relink")
	default:
		target = r.Target.validate("target", false)
		if target == nil && *r.Target == r.Current {
			target = invalidField("target", "must differ from the current contact")
		}
	}
	switch {
	case unlink:
		reason = checkReasonCode("reason", r.Reason)
	case r.Reason != "":
		reason = invalidField("reason", "must be empty for link and relink")
	}
	return firstError(
		r.Identity.validate("identity", false),
		r.Current.validate("current", false),
		checkToken("expected_revision", string(r.ExpectedRevision)),
		target,
		reason,
		r.Evidence.validateRequest("evidence", !unlink),
	)
}

// validateRequest checks caller-supplied evidence. Assurance must be empty:
// it comes from the verifier only. Required or not zero, the evidence needs
// a verifier key and a well-formed reference and facts.
func (e Evidence) validateRequest(path string, required bool) error {
	if e.Assurance != "" {
		return invalidField(fieldPath(path, "assurance"), "must be empty; assurance comes from the verifier")
	}
	if !required && e.IsZero() {
		return nil
	}
	return e.validateShape(path)
}

func (e Evidence) validateShape(path string) error {
	return firstError(
		checkToken(fieldPath(path, "verifier"), e.Verifier),
		checkOptionalToken(fieldPath(path, "reference"), e.Reference),
		e.Facts.validateOptional(fieldPath(path, "facts")),
	)
}
