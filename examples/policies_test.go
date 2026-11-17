package examples

import (
	"context"
	"fmt"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/policy"
)

var (
	operator  = model.Principal{Tenant: "acme", Actor: "operator"}
	mailInbox = model.InboxRef{Tenant: "acme", ID: "0190a5c4-0000-7000-8000-000000000001"}
	adaWeb    = model.ContactRef{Tenant: "acme", ID: "0190a5c4-0000-7000-8000-000000000002"}
	adaMail   = model.ContactRef{Tenant: "acme", ID: "0190a5c4-0000-7000-8000-000000000003"}
	mailID    = model.IdentityRef{Inbox: mailInbox, ID: "0190a5c4-0000-7000-8000-000000000004"}
)

// Core builds the request after reading the identity and checks what the
// verifier returns against it before any policy decision. Facts bound to a
// request core did not make are refused.
func ExampleAttestationVerifier() {
	ctx := context.Background()
	v := AttestationVerifier{}
	req := model.LinkRequest{
		Intent: model.LinkClassLink, Identity: mailID, Current: adaMail, ExpectedRevision: "r1",
		Target: &adaWeb, Evidence: model.Evidence{Verifier: model.EvidenceKindAttestation, Reference: "ticket-4711"},
	}
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

	ev, err := v.Verify(ctx, operator, req)
	if err != nil {
		panic(err)
	}
	fmt.Println(ev.Verifier, ev.Assurance, ev.Reference)
	fmt.Println("core accepts it:", ev.ValidateReturn(req, operator.Actor, now) == nil)

	other := req
	other.Target = &model.ContactRef{Tenant: "acme", ID: "0190a5c4-0000-7000-8000-000000000005"}
	fmt.Println("for another target:", model.CodeOf(ev.ValidateReturn(other, operator.Actor, now)))

	_, err = v.Verify(ctx, operator, model.LinkRequest{Intent: "merge", Identity: mailID, Current: adaMail})
	fmt.Println("unknown intent:", model.CodeOf(err))
	// Output:
	// attestation asserted ticket-4711
	// core accepts it: true
	// for another target: validation
	// unknown intent: validation
}

type allowAll struct{}

func (allowAll) Check(context.Context, model.Principal, string, model.Ref, model.Audience, *model.Ref) (bool, error) {
	return true, nil
}

// The three evidence states an unlink decision sees: none supplied, and
// verified at a level below or at the policy's minimum. Supplied evidence
// that fails verification never reaches the authorizer.
func ExampleAccessPolicy() {
	p := AccessPolicy{ACL: allowAll{}, UnlinkAssurance: model.LinkAssuranceAsserted}
	unlink := policy.Authorization{
		Principal: operator, Action: model.ActionIdentityUnlink, Resource: mailID.Ref(), Revision: "r2",
		Assurance: model.LinkAssuranceNone,
	}
	show := func(label string, a policy.Authorization) {
		d, err := p.Authorize(context.Background(), a)
		if err != nil {
			panic(err)
		}
		fmt.Println(label, d.Allow, d.ReasonCode)
	}
	show("no evidence:", unlink)

	unlink.Evidence = &policy.VerifiedEvidence{
		Verifier: model.EvidenceKindAttestation, Reference: "ticket-4712", Assurance: model.LinkAssuranceNone,
	}
	show("verified at none:", unlink)

	unlink.Assurance, unlink.Evidence.Assurance = model.LinkAssuranceAsserted, model.LinkAssuranceAsserted
	show("verified at asserted:", unlink)
	// Output:
	// no evidence: false unlink_evidence_required
	// verified at none: false unlink_evidence_required
	// verified at asserted: true host_acl
}
