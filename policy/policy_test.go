package policy_test

import (
	"context"
	"fmt"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/policy"
)

// An authorizer written before link evidence reached Authorization, one
// that never reads Assurance or Evidence, still compiles and decides.
func TestAuthorizerIgnoringAssuranceBuilds(t *testing.T) {
	var a policy.Authorizer = policy.AuthorizeFunc(func(_ context.Context, az policy.Authorization) (policy.Decision, error) {
		return policy.Decision{Allow: az.Action == model.ActionIdentityUnlink}, nil
	})
	d, err := a.Authorize(context.Background(), policy.Authorization{
		Action: model.ActionIdentityUnlink, Assurance: model.LinkAssuranceNone,
	})
	if err != nil || !d.Allow {
		t.Fatalf("Authorize = %+v, %v", d, err)
	}
}

// A verifier with no version still satisfies IdentityVerifier; a version
// is offered through the optional VersionedVerifier.
type stubVerifier struct{}

func (stubVerifier) Verify(context.Context, model.Principal, model.LinkRequest) (model.Evidence, error) {
	return model.Evidence{}, nil
}

type versionedVerifier struct{ stubVerifier }

func (versionedVerifier) Version() string { return "2026-10" }

func TestVersionedVerifierIsOptional(t *testing.T) {
	var plain policy.IdentityVerifier = stubVerifier{}
	if _, ok := plain.(policy.VersionedVerifier); ok {
		t.Fatal("plain verifier reports a version")
	}
	var v policy.IdentityVerifier = versionedVerifier{}
	vv, ok := v.(policy.VersionedVerifier)
	if !ok || vv.Version() != "2026-10" {
		t.Fatal("versioned verifier not recognised")
	}
}

// A host that requires verified evidence to unlink decides on the verified
// facts core passes in, never on anything the caller declared.
func ExampleAuthorization_requireUnlinkEvidence() {
	auth := policy.AuthorizeFunc(func(_ context.Context, a policy.Authorization) (policy.Decision, error) {
		if a.Action == model.ActionIdentityUnlink && a.Evidence == nil {
			return policy.Decision{ReasonCode: "unlink_evidence_required"}, nil
		}
		return policy.Decision{Allow: true}, nil
	})
	ctx := context.Background()
	missing, _ := auth.Authorize(ctx, policy.Authorization{
		Action: model.ActionIdentityUnlink, Assurance: model.LinkAssuranceNone,
	})
	verified, _ := auth.Authorize(ctx, policy.Authorization{
		Action: model.ActionIdentityUnlink, Assurance: model.LinkAssuranceAsserted,
		Evidence: &policy.VerifiedEvidence{
			Verifier: model.EvidenceKindAttestation, Reference: "ticket-9", Assurance: model.LinkAssuranceAsserted,
		},
	})
	fmt.Println(missing.Allow, missing.ReasonCode)
	fmt.Println(verified.Allow)
	// Output:
	// false unlink_evidence_required
	// true
}

// A function serves as a CascadePolicy.
func TestCascadePolicyFunc(t *testing.T) {
	var p policy.CascadePolicy = policy.CascadePolicyFunc(func(_ context.Context, _ model.Principal, in model.InboxRef) (int, error) {
		if in.ID == "strict" {
			return policy.CascadeDisabled, nil
		}
		return policy.CascadeNeverRefuse, nil
	})
	pr := model.Principal{Tenant: "t", Actor: "a"}
	if n, err := p.ContactCascadeThreshold(context.Background(), pr, model.InboxRef{Tenant: "t", ID: "strict"}); err != nil || n != 0 {
		t.Fatalf("strict inbox = %d, %v", n, err)
	}
	if n, err := p.ContactCascadeThreshold(context.Background(), pr, model.InboxRef{Tenant: "t", ID: "open"}); err != nil || n != -1 {
		t.Fatalf("open inbox = %d, %v", n, err)
	}
}
