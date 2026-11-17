// Package policy contains trusted host extension contracts, not
// authentication implementations: the Authorizer that decides every
// operation, conversation selectors, the interaction validator, identity
// verifiers and the contact-cascade threshold policy.
//
// # Identity verification
//
// An IdentityVerifier is registered per evidence kind and verifies the
// evidence of one Contacts.Link or Contacts.Unlink. It never sees the
// caller's request as such: core reads the identity first and hands the
// verifier a model.LinkRequest whose Intent (link, relink or unlink) is
// the class core computed, with the identity, its current contact, the
// expected revision and, for link and relink, the target. The verifier
// returns facts bound to that request; core checks them
// (model.Evidence.ValidateReturn) and refuses any mismatch, so evidence
// verified for one identity, contact or intent cannot be replayed for
// another.
//
// # Verified evidence in decisions
//
// Authorization.Assurance and Authorization.Evidence carry verified facts
// only. Core sets them from the evidence its verifier returned and it
// checked; no request field reaches them, and no caller can declare an
// assurance or waive evidence. An Authorizer can therefore require
// evidence for an action, such as unlinking, by denying when Evidence is
// nil or Assurance is below the level it needs. Of the three evidence
// states, two reach it: none supplied (Evidence nil, Assurance
// model.LinkAssuranceNone) and verified (Evidence set, Assurance the
// verified level). Evidence supplied but not verified ends the call before
// any decision, with model.Validation, or model.Unsupported for a kind no
// verifier is registered for. Every other action leaves both zero.
package policy

import (
	"context"

	"hop.top/uma/model"
)

// Authorization is one policy question: may Principal perform Action on
// Resource, under Audience, toward Destination, at Revision.
//
// Assurance and Evidence carry identity-link evidence for the decisions of
// one Contacts.Link or Contacts.Unlink call (its class action and, when the
// link assurance drops, model.ActionIdentityLinkOverride), the same values
// on each. Core sets them from verifier output it has checked, never from
// the request, so a policy can require evidence, such as a verified one
// to unlink, without trusting the caller. Three states:
//
//   - no evidence supplied: Evidence nil, Assurance model.LinkAssuranceNone;
//   - evidence supplied but not verified: never reaches the authorizer; the
//     call fails first with model.Validation (model.Unsupported for an
//     unregistered verifier);
//   - verified: Evidence set and Assurance the verifier-returned level,
//     which may be model.LinkAssuranceNone.
//
// Every other action leaves both zero: Assurance "" and Evidence nil. An
// authorizer that ignores both decides as before.
type Authorization struct {
	Principal   model.Principal
	Action      string
	Resource    model.Ref
	Audience    model.Audience
	Destination *model.Ref
	Revision    model.Revision
	Assurance   model.LinkAssurance
	Evidence    *VerifiedEvidence
}

// VerifiedEvidence is the verified evidence of an identity-link decision:
// the verifier key (an evidence kind), the evidence reference and the
// assurance the verifier returned. It never holds facts.
type VerifiedEvidence struct {
	Verifier  string
	Reference string
	Assurance model.LinkAssurance
}
type Decision struct {
	Allow         bool
	PolicyVersion string
	ReasonCode    string
}
type Authorizer interface {
	Authorize(context.Context, Authorization) (Decision, error)
}
type AuthorizeFunc func(context.Context, Authorization) (Decision, error)

func (f AuthorizeFunc) Authorize(c context.Context, a Authorization) (Decision, error) {
	return f(c, a)
}

// SelectionInput is the immutable snapshot one selection decides on.
// Candidates holds the sender's candidate group in the inbox plus every
// conversation a header match names. Event classifies the new message;
// Matches lists the stored messages of the inbox that the message's
// winning threading header names (see ReferenceMatch); core loads them
// when the channel's adapter declares adapter.ThreadingEmail.
//
// For EventOutboundStart, Sender is the recipient's identity in a
// per-contact inbox (nil otherwise), References holds the recipient
// reference in a per-room or per-thread inbox, Matches is empty and
// Operation is the operation the start commits as: a selector that gives
// every start its own conversation derives its create key from it. A
// replayed start never selects again. Operation is zero for inbound
// messages.
type SelectionInput struct {
	Inbox      model.Inbox
	Sender     *model.Identity
	References []model.ExternalRef
	Candidates []model.Conversation
	Event      EventClass
	Matches    []ReferenceMatch
	Operation  model.OperationRef
}

// SelectionResult names exactly one existing candidate or a create key.
// Reopen asks core to move a resolved Existing to open; AddParticipant asks
// core to add the sender to Existing's participants.
type SelectionResult struct {
	Existing       *model.ConversationRef
	CreateKey      string
	Reopen         bool
	AddParticipant bool
}
type ConversationSelector interface {
	Select(context.Context, SelectionInput) (SelectionResult, error)
}

// IdentityVerifier verifies identity-link evidence for one evidence kind,
// the key it is registered under. Verify is read-only: it writes nothing,
// and core calls it outside any store unit of work, before any policy
// decision. It returns the evidence it vouches for: Verifier its key,
// Facts the kind's version 1 facts bound to req, Assurance at most the
// kind's maximum. Core checks the return (model.Evidence.ValidateReturn)
// and persists it, never the caller's evidence; a failed check is
// model.Validation and nothing is written.
type IdentityVerifier interface {
	Verify(ctx context.Context, p model.Principal, req model.LinkRequest) (model.Evidence, error)
}

// VersionedVerifier is optionally implemented by an IdentityVerifier to
// report its version, recorded on each link record it verified; a verifier
// without it records an empty version.
type VersionedVerifier interface {
	Version() string
}

// InteractionValidator evaluates one interaction synchronously, before
// the adapter acknowledges it: field errors of a submission or dynamic
// options (model.InteractionOptions). It is read-only and bounded by the
// interaction's deadline; its answer never accepts the interaction.
type InteractionValidator interface {
	Evaluate(context.Context, model.Principal, model.InteractionInput) (model.InteractionDecision, error)
}
