package conformance

import (
	"context"
	"errors"
	"sync"

	"hop.top/uma/model"
	"hop.top/uma/policy"
)

// AttestationVerifier is an honest attestation verifier: it vouches for
// exactly the request core hands it. Its facts bind the intent core
// computed, the identity, its current contact and, for link and relink,
// the target; the attesting actor is the requesting principal's and the
// reason code the unlink reason, or ReasonCode for link and relink
// ("same_person" when empty). It returns the request's reference at
// model.LinkAssuranceAsserted, the kind's maximum.
//
// Register it under model.EvidenceKindAttestation, or call Evidence from a
// ScriptedVerifier answer to start from honest evidence and alter it.
type AttestationVerifier struct {
	ReasonCode string
}

// Verify returns Evidence for req.
func (v AttestationVerifier) Verify(_ context.Context, p model.Principal, req model.LinkRequest) (model.Evidence, error) {
	return v.Evidence(p, req)
}

// Evidence is the evidence Verify returns.
func (v AttestationVerifier) Evidence(p model.Principal, req model.LinkRequest) (model.Evidence, error) {
	code := req.Reason
	if code == "" {
		code = v.ReasonCode
	}
	if code == "" {
		code = "same_person"
	}
	f := model.AttestationFacts{EvidenceBinding: Binding(req), Actor: p.Actor, ReasonCode: code}
	facts, err := model.NewEvidenceFacts(f)
	if err != nil {
		return model.Evidence{}, err
	}
	return model.Evidence{
		Verifier: model.EvidenceKindAttestation, Reference: req.Evidence.Reference,
		Facts: facts, Assurance: model.LinkAssuranceAsserted,
	}, nil
}

// Binding is the evidence binding an honest verifier returns for req: the
// request's intent, identity, current contact and target, without anchor
// or expiry.
func Binding(req model.LinkRequest) model.EvidenceBinding {
	b := model.EvidenceBinding{Intent: req.Intent, Identity: req.Identity, Current: req.Current}
	if req.Target != nil && req.Intent != model.LinkClassUnlink {
		b.Target = new(*req.Target)
	}
	return b
}

// ErrNotVerified is what a ScriptedVerifier without an answer fails with.
var ErrNotVerified = errors.New("conformance: evidence not verified")

// ScriptedVerifier is an identity verifier that answers whatever the case
// scripts, honest or not, and records every request it receives. Without
// a script it fails every request with ErrNotVerified. It is safe for
// concurrent use.
type ScriptedVerifier struct {
	mu     sync.Mutex
	answer func(model.Principal, model.LinkRequest) (model.Evidence, error)
	reqs   []model.LinkRequest
}

// Script replaces the answer; nil fails every later request.
func (v *ScriptedVerifier) Script(answer func(model.Principal, model.LinkRequest) (model.Evidence, error)) {
	v.mu.Lock()
	v.answer = answer
	v.mu.Unlock()
}

// Verify records req and returns the scripted answer.
func (v *ScriptedVerifier) Verify(_ context.Context, p model.Principal, req model.LinkRequest) (model.Evidence, error) {
	v.mu.Lock()
	v.reqs = append(v.reqs, req.Clone())
	answer := v.answer
	v.mu.Unlock()
	if answer == nil {
		return model.Evidence{}, ErrNotVerified
	}
	return answer(p, req.Clone())
}

// Requests returns every request Verify received, in call order.
func (v *ScriptedVerifier) Requests() []model.LinkRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]model.LinkRequest, len(v.reqs))
	for i, r := range v.reqs {
		out[i] = r.Clone()
	}
	return out
}

// Calls counts Verify calls.
func (v *ScriptedVerifier) Calls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.reqs)
}

var (
	_ policy.IdentityVerifier = AttestationVerifier{}
	_ policy.IdentityVerifier = (*ScriptedVerifier)(nil)
)
