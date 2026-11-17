package model

import (
	"bytes"
	"encoding/json"
	"time"
)

// Evidence kinds. A verifier is registered under its kind
// (Config.IdentityVerifiers); no other key is accepted. Each kind has a
// core facts schema, CoreNamespace + ".evidence." + kind at
// EvidenceFactsVersion1, and a maximum link assurance (MaxLinkAssurance).
//
// Never evidence, at any level: a display name, message text, an unverified
// email From or phone string, a nickname, a provider webhook signature or
// an agent message role.
const (
	// EvidenceKindAttestation: an authorized actor attests, with a reason
	// code. At most LinkAssuranceAsserted.
	EvidenceKindAttestation = "attestation"
	// EvidenceKindImport: a trusted system-of-record import names the
	// association. At most LinkAssuranceImported.
	EvidenceKindImport = "import"
	// EvidenceKindVerifiedAttribute: the identity and an anchor identity on
	// the target carry the same provider-verified attribute. At most
	// LinkAssuranceAttributeMatch.
	EvidenceKindVerifiedAttribute = "verified_attribute"
	// EvidenceKindHostSession: one host-authenticated session was observed
	// on the identity and on an anchor identity. At most
	// LinkAssuranceHostAuthenticated.
	EvidenceKindHostSession = "host_session"
	// EvidenceKindChallenge: a challenge issued through an anchor identity
	// was completed through this identity before it expired. At most
	// LinkAssuranceProven.
	EvidenceKindChallenge = "challenge"
)

// Evidence facts schema names (Evidence.Facts.Name), one per kind.
const (
	SchemaEvidenceAttestation       = "uma.evidence.attestation"
	SchemaEvidenceImport            = "uma.evidence.import"
	SchemaEvidenceVerifiedAttribute = "uma.evidence.verified_attribute"
	SchemaEvidenceHostSession       = "uma.evidence.host_session"
	SchemaEvidenceChallenge         = "uma.evidence.challenge"
)

// EvidenceFactsVersion1 is the Extension.Version of every evidence facts
// schema in this version.
const EvidenceFactsVersion1 uint32 = 1

type evidenceKind struct {
	name, schema string
	max          LinkAssurance
	anchored     bool
}

var evidenceKinds = [...]evidenceKind{
	{EvidenceKindAttestation, SchemaEvidenceAttestation, LinkAssuranceAsserted, false},
	{EvidenceKindImport, SchemaEvidenceImport, LinkAssuranceImported, false},
	{EvidenceKindVerifiedAttribute, SchemaEvidenceVerifiedAttribute, LinkAssuranceAttributeMatch, true},
	{EvidenceKindHostSession, SchemaEvidenceHostSession, LinkAssuranceHostAuthenticated, true},
	{EvidenceKindChallenge, SchemaEvidenceChallenge, LinkAssuranceProven, true},
}

func lookupKind(kind string) (evidenceKind, bool) {
	for _, k := range evidenceKinds {
		if k.name == kind {
			return k, true
		}
	}
	return evidenceKind{}, false
}

// EvidenceKinds returns the evidence kinds, lowest cap first.
func EvidenceKinds() []string {
	out := make([]string, len(evidenceKinds))
	for i, k := range evidenceKinds {
		out[i] = k.name
	}
	return out
}

// IsEvidenceKind reports whether kind is an evidence kind.
func IsEvidenceKind(kind string) bool {
	_, ok := lookupKind(kind)
	return ok
}

// MaxLinkAssurance returns the highest link assurance evidence of kind may
// carry; false for anything but an evidence kind.
func MaxLinkAssurance(kind string) (LinkAssurance, bool) {
	k, ok := lookupKind(kind)
	return k.max, ok
}

// EvidenceSchema returns the facts schema name of kind; false for anything
// but an evidence kind.
func EvidenceSchema(kind string) (string, bool) {
	k, ok := lookupKind(kind)
	return k.schema, ok
}

// IsZero reports whether no field is set: an UnlinkIdentity without
// evidence.
func (e Evidence) IsZero() bool {
	return e.Verifier == "" && e.Reference == "" && e.Assurance == "" &&
		e.Facts.Name == "" && e.Facts.Version == 0 && len(e.Facts.Data) == 0
}

// EvidenceBinding is what every kind's facts bind: the intent core computed
// and the request it verified. Target is the request target for link and
// relink and absent for unlink. Anchor is the identity already on the
// target that the anchored kinds (verified_attribute, host_session,
// challenge) prove against, for link and relink only; core re-checks inside
// the commit that it is still on the target. ExpiresAt is when the facts
// stop being acceptable, required for challenge.
type EvidenceBinding struct {
	Intent    LinkClass    `json:"intent"`
	Identity  IdentityRef  `json:"identity"`
	Current   ContactRef   `json:"current"`
	Target    *ContactRef  `json:"target,omitempty"`
	Anchor    *IdentityRef `json:"anchor,omitempty"`
	ExpiresAt *time.Time   `json:"expires_at,omitempty"`
}

// Binding returns the binding itself; facts types embed it.
func (b EvidenceBinding) Binding() EvidenceBinding { return b }

// EvidenceFacts is the typed facts of one evidence kind: AttestationFacts,
// ImportFacts, VerifiedAttributeFacts, HostSessionFacts or ChallengeFacts.
// The set is closed.
type EvidenceFacts interface {
	EvidenceKind() string
	Binding() EvidenceBinding
	checkKind(path string, req LinkRequest, actor ActorID) error
}

// AttestationFacts are uma.evidence.attestation version 1 facts: Actor is
// the attesting actor, the requesting principal's, and ReasonCode a reason
// code, the unlink reason for an unlink.
type AttestationFacts struct {
	EvidenceBinding
	Actor      ActorID `json:"actor"`
	ReasonCode string  `json:"reason_code"`
}

// ImportFacts are uma.evidence.import version 1 facts: the source system,
// its record ID and the identity key the record names.
type ImportFacts struct {
	EvidenceBinding
	SourceSystem string      `json:"source_system"`
	SourceRecord string      `json:"source_record"`
	IdentityKey  IdentityKey `json:"identity_key"`
}

// VerifiedAttributeFacts are uma.evidence.verified_attribute version 1
// facts: the attribute type and normalized value both sides carry
// verified. Value is personal data; redaction removes it.
type VerifiedAttributeFacts struct {
	EvidenceBinding
	Attribute string `json:"attribute"`
	Value     string `json:"value"`
}

// HostSessionFacts are uma.evidence.host_session version 1 facts: the host
// session subject observed on both identities.
type HostSessionFacts struct {
	EvidenceBinding
	SessionSubject string `json:"session_subject"`
}

// ChallengeFacts are uma.evidence.challenge version 1 facts: the completed
// challenge's reference. ExpiresAt and Evidence.Reference are required.
type ChallengeFacts struct {
	EvidenceBinding
	ChallengeRef string `json:"challenge_ref"`
}

// EvidenceKind returns EvidenceKindAttestation.
func (AttestationFacts) EvidenceKind() string { return EvidenceKindAttestation }

// EvidenceKind returns EvidenceKindImport.
func (ImportFacts) EvidenceKind() string { return EvidenceKindImport }

// EvidenceKind returns EvidenceKindVerifiedAttribute.
func (VerifiedAttributeFacts) EvidenceKind() string { return EvidenceKindVerifiedAttribute }

// EvidenceKind returns EvidenceKindHostSession.
func (HostSessionFacts) EvidenceKind() string { return EvidenceKindHostSession }

// EvidenceKind returns EvidenceKindChallenge.
func (ChallengeFacts) EvidenceKind() string { return EvidenceKindChallenge }

func (f AttestationFacts) checkKind(path string, req LinkRequest, actor ActorID) error {
	if f.Actor != actor {
		return invalidField(fieldPath(path, "actor"), "must be the requesting actor")
	}
	if err := checkReasonCode(fieldPath(path, "reason_code"), f.ReasonCode); err != nil {
		return err
	}
	if req.Intent == LinkClassUnlink && f.ReasonCode != req.Reason {
		return invalidField(fieldPath(path, "reason_code"), "must be the unlink reason")
	}
	return nil
}

func (f ImportFacts) checkKind(path string, req LinkRequest, _ ActorID) error {
	key := fieldPath(path, "identity_key")
	err := firstError(
		checkToken(fieldPath(path, "source_system"), f.SourceSystem),
		checkToken(fieldPath(path, "source_record"), f.SourceRecord),
		f.IdentityKey.Inbox.validate(fieldPath(key, "inbox"), false),
		checkNamespace(fieldPath(key, "namespace"), f.IdentityKey.Namespace),
		checkRaw(fieldPath(key, "source_id"), f.IdentityKey.SourceID, maxTokenBytes),
	)
	if err == nil && f.IdentityKey.Inbox != req.Identity.Inbox {
		err = invalidField(fieldPath(key, "inbox"), "must be the identity's inbox")
	}
	return err
}

func (f VerifiedAttributeFacts) checkKind(path string, _ LinkRequest, _ ActorID) error {
	return firstError(
		checkToken(fieldPath(path, "attribute"), f.Attribute),
		checkToken(fieldPath(path, "value"), f.Value),
	)
}

func (f HostSessionFacts) checkKind(path string, _ LinkRequest, _ ActorID) error {
	return checkToken(fieldPath(path, "session_subject"), f.SessionSubject)
}

func (f ChallengeFacts) checkKind(path string, _ LinkRequest, _ ActorID) error {
	return checkToken(fieldPath(path, "challenge_ref"), f.ChallengeRef)
}

// NewEvidenceFacts encodes f as its kind's facts extension, for a verifier
// to return in Evidence.Facts.
func NewEvidenceFacts(f EvidenceFacts) (Extension, error) {
	if f == nil {
		return Extension{}, invalidField("facts", "required")
	}
	schema, _ := EvidenceSchema(f.EvidenceKind())
	data, err := json.Marshal(f)
	if err != nil {
		return Extension{}, invalidField("facts", "must encode as JSON")
	}
	return Extension{Name: schema, Version: EvidenceFactsVersion1, Data: data}, nil
}

// DecodeFacts decodes Facts as the facts of the kind Verifier names. Facts
// must carry that kind's schema name and version and decode with no
// unknown field.
func (e Evidence) DecodeFacts() (EvidenceFacts, error) { return e.decodeFacts("") }

func (e Evidence) decodeFacts(path string) (EvidenceFacts, error) {
	k, ok := lookupKind(e.Verifier)
	if !ok {
		return nil, invalidField(fieldPath(path, "verifier"), "must be an evidence kind")
	}
	fp := fieldPath(path, "facts")
	switch {
	case e.Facts.Name == "":
		return nil, invalidField(fieldPath(fp, "name"), "required")
	case e.Facts.Name != k.schema:
		return nil, invalidField(fieldPath(fp, "name"), "must be "+k.schema)
	case e.Facts.Version != EvidenceFactsVersion1:
		return nil, invalidField(fieldPath(fp, "version"), "must be 1")
	}
	if err := e.Facts.validate(fp, DefaultContentLimits().MaxStructuredBytes); err != nil {
		return nil, err
	}
	var f EvidenceFacts
	var err error
	switch k.name {
	case EvidenceKindAttestation:
		f, err = decodeStrict[AttestationFacts](e.Facts.Data)
	case EvidenceKindImport:
		f, err = decodeStrict[ImportFacts](e.Facts.Data)
	case EvidenceKindVerifiedAttribute:
		f, err = decodeStrict[VerifiedAttributeFacts](e.Facts.Data)
	case EvidenceKindHostSession:
		f, err = decodeStrict[HostSessionFacts](e.Facts.Data)
	default:
		f, err = decodeStrict[ChallengeFacts](e.Facts.Data)
	}
	if err != nil {
		return nil, invalidField(fieldPath(fp, "data"), "must decode as "+k.schema+" version 1")
	}
	return f, nil
}

// decodeStrict decodes data, already checked to be one JSON value, into a T
// with no unknown field.
func decodeStrict[T any](data []byte) (T, error) {
	var v T
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	err := dec.Decode(&v)
	return v, err
}

// ValidateReturn checks evidence a verifier returned for req, before any
// policy decision: Verifier is the requested key and an evidence kind;
// Reference is well formed, and present for challenge; Facts are the
// kind's version 1 facts and bind req (intent, identity, current contact,
// target, the anchor rule); attestation facts name actor; the facts have
// not expired at now; Assurance is declared, at most the kind's maximum,
// and above none for link and relink. Any failure is Validation.
func (e Evidence) ValidateReturn(req LinkRequest, actor ActorID, now time.Time) error {
	const path = "evidence"
	if e.Verifier != req.Evidence.Verifier {
		return invalidField(fieldPath(path, "verifier"), "must be the requested verifier")
	}
	k, ok := lookupKind(e.Verifier)
	if !ok {
		return invalidField(fieldPath(path, "verifier"), "must be an evidence kind")
	}
	if err := checkOptionalToken(fieldPath(path, "reference"), e.Reference); err != nil {
		return err
	}
	if k.name == EvidenceKindChallenge && e.Reference == "" {
		return invalidField(fieldPath(path, "reference"), "required for challenge evidence")
	}
	facts, err := e.decodeFacts(path)
	if err != nil {
		return err
	}
	fp := fieldPath(path, "facts")
	if err := facts.Binding().check(fp, req, k, now); err != nil {
		return err
	}
	if err := facts.checkKind(fp, req, actor); err != nil {
		return err
	}
	ap := fieldPath(path, "assurance")
	switch {
	case e.Assurance.validate(ap) != nil:
		return e.Assurance.validate(ap)
	case k.max.Below(e.Assurance):
		return invalidField(ap, "exceeds the maximum for "+k.name+" evidence")
	case req.Intent != LinkClassUnlink && e.Assurance == LinkAssuranceNone:
		return invalidField(ap, "must be above none to link")
	}
	return nil
}

func (b EvidenceBinding) check(path string, req LinkRequest, k evidenceKind, now time.Time) error {
	unlink := req.Intent == LinkClassUnlink
	switch {
	case b.Intent != req.Intent:
		return invalidField(fieldPath(path, "intent"), "must be the request intent")
	case b.Identity != req.Identity:
		return invalidField(fieldPath(path, "identity"), "must be the request identity")
	case b.Current != req.Current:
		return invalidField(fieldPath(path, "current"), "must be the identity's current contact")
	case unlink && b.Target != nil:
		return invalidField(fieldPath(path, "target"), "must be absent for unlink")
	case !unlink && (b.Target == nil || req.Target == nil || *b.Target != *req.Target):
		return invalidField(fieldPath(path, "target"), "must be the request target")
	}
	if err := b.checkAnchor(fieldPath(path, "anchor"), req, k.anchored && !unlink); err != nil {
		return err
	}
	switch {
	case b.ExpiresAt == nil && k.name == EvidenceKindChallenge:
		return invalidField(fieldPath(path, "expires_at"), "required for challenge evidence")
	case b.ExpiresAt != nil && !b.ExpiresAt.After(now):
		return invalidField(fieldPath(path, "expires_at"), "expired")
	}
	return nil
}

func (b EvidenceBinding) checkAnchor(path string, req LinkRequest, required bool) error {
	switch {
	case !required && b.Anchor != nil:
		return invalidField(path, "not used by this kind and intent")
	case !required:
		return nil
	case b.Anchor == nil:
		return invalidField(path, "required by this kind")
	}
	if err := b.Anchor.validate(path, false); err != nil {
		return err
	}
	switch {
	case *b.Anchor == req.Identity:
		return invalidField(path, "must be another identity")
	case b.Anchor.Scope() != req.Identity.Scope():
		return invalidField(path, "must be in the identity's tenant")
	}
	return nil
}
