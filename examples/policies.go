package examples

import (
	"context"

	"hop.top/uma/model"
	"hop.top/uma/policy"
)

// ACL is host-owned policy data, not provider credentials or caller-supplied role strings.
type ACL interface {
	Check(context.Context, model.Principal, string, model.Ref, model.Audience, *model.Ref) (bool, error)
}

// AccessPolicy decides through the host ACL. It also requires verified
// evidence to unlink an identity: core sets Authorization.Evidence and
// Assurance from evidence its verifier vouched for, never from the
// request, so a nil Evidence means the caller supplied none. Unverified
// evidence never reaches the authorizer; the call fails before.
type AccessPolicy struct {
	ACL ACL
	// UnlinkAssurance is the lowest verified assurance an unlink needs,
	// such as model.LinkAssuranceAsserted. Empty requires no unlink
	// evidence.
	UnlinkAssurance model.LinkAssurance
}

func (p AccessPolicy) Authorize(ctx context.Context, r policy.Authorization) (policy.Decision, error) {
	if p.ACL == nil || r.Principal.Tenant == "" || r.Principal.Tenant != r.Resource.Tenant {
		return policy.Decision{}, nil
	}
	if r.Action == model.ActionIdentityUnlink && p.UnlinkAssurance != "" &&
		(r.Evidence == nil || r.Assurance.Below(p.UnlinkAssurance)) {
		return policy.Decision{ReasonCode: "unlink_evidence_required"}, nil
	}
	allowed, err := p.ACL.Check(ctx, r.Principal, r.Action, r.Resource, r.Audience, r.Destination)
	return policy.Decision{Allow: allowed, ReasonCode: "host_acl"}, err
}

// AttestationVerifier verifies attestation evidence: the requesting actor
// vouches, under a reason code, that an identity belongs to a contact, or
// detaches it. Register it under model.EvidenceKindAttestation in
// Config.IdentityVerifiers; uma.New rejects a key that is not an evidence
// kind, and evidence naming a kind with no registered verifier fails with
// model.Unsupported. Attestation is the lowest grade of evidence
// (model.LinkAssuranceAsserted): register it only when operators may link
// on their word alone.
//
// Verify reads nothing and writes nothing; core calls it outside any store
// unit and before any policy decision. It binds the facts to the request
// core computed after reading the identity: the intent, the identity, its
// current contact and, for link and relink, the target. Core checks the
// returned evidence against that request (model.Evidence.ValidateReturn)
// and refuses any mismatch with model.Validation, so facts bound to
// another identity, contact or intent are never accepted.
//
// Evidence.Reference is single-use. Core stores the (verifier, reference)
// pair with the link record and refuses a later link reusing it with
// model.Conflict, even after the record's facts are redacted. Use an
// opaque proof ID, such as a ticket or approval number, and never personal
// data. An empty reference is not tracked.
type AttestationVerifier struct {
	// ReasonCode is the reason attested for link and relink, one lowercase
	// name segment; empty means "same_person". An unlink attests the
	// request's own reason.
	ReasonCode string
}

// Version is recorded on every link record this verifier vouched for.
func (AttestationVerifier) Version() string { return "attestation-1" }

// Verify returns attestation evidence bound to req. It declines an intent
// it does not know with model.Validation.
func (v AttestationVerifier) Verify(_ context.Context, p model.Principal, req model.LinkRequest) (model.Evidence, error) {
	binding := model.EvidenceBinding{Intent: req.Intent, Identity: req.Identity, Current: req.Current}
	reason := v.ReasonCode
	switch req.Intent {
	case model.LinkClassLink, model.LinkClassRelink:
		if req.Target == nil {
			return model.Evidence{}, &model.Error{Code: model.Validation, SafeMessage: "attestation: target required"}
		}
		binding.Target = req.Target
		if reason == "" {
			reason = "same_person"
		}
	case model.LinkClassUnlink:
		reason = req.Reason
	default:
		return model.Evidence{}, &model.Error{Code: model.Validation, SafeMessage: "attestation: intent not supported"}
	}
	facts, err := model.NewEvidenceFacts(model.AttestationFacts{EvidenceBinding: binding, Actor: p.Actor, ReasonCode: reason})
	if err != nil {
		return model.Evidence{}, err
	}
	return model.Evidence{
		Verifier: model.EvidenceKindAttestation, Reference: req.Evidence.Reference,
		Facts: facts, Assurance: model.LinkAssuranceAsserted,
	}, nil
}

// ScopedSelector handles a prefiltered candidate set; core must still validate the result.
type ScopedSelector struct{}

func (ScopedSelector) Select(_ context.Context, in policy.SelectionInput) (policy.SelectionResult, error) {
	if len(in.Candidates) == 1 {
		return policy.SelectionResult{Existing: &in.Candidates[0].Ref}, nil
	}
	if len(in.Candidates) > 1 {
		return policy.SelectionResult{}, &model.Error{Code: model.Conflict, SafeMessage: "ambiguous conversation"}
	}
	return policy.SelectionResult{}, &model.Error{Code: model.NotFound, SafeMessage: "no eligible conversation"}
}

// CascadeThresholds answers contact-cascade thresholds from host-owned
// settings: an inbox's own setting, else its tenant's, else
// policy.CascadeNeverRefuse, the engine's default too. Register it as
// Config.CascadePolicy. A cascade over a contact whose identities sit in
// several inboxes takes the most restrictive of their thresholds, so a
// strict inbox protects every contact it touches.
type CascadeThresholds struct {
	Inboxes map[model.InboxRef]int
	Tenants map[model.TenantID]int
}

func (c CascadeThresholds) ContactCascadeThreshold(_ context.Context, p model.Principal, in model.InboxRef) (int, error) {
	if n, ok := c.Inboxes[in]; ok {
		return n, nil
	}
	if n, ok := c.Tenants[p.Tenant]; ok {
		return n, nil
	}
	return policy.CascadeNeverRefuse, nil
}

var _ policy.Authorizer = AccessPolicy{}
var _ policy.CascadePolicy = CascadeThresholds{}
var _ policy.ConversationSelector = ScopedSelector{}
var _ policy.IdentityVerifier = AttestationVerifier{}
var _ policy.VersionedVerifier = AttestationVerifier{}
