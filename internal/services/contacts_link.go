package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// linkIntent is the replay context a Link or Unlink stores with its
// operation intent: the class action that admitted it (identity.read for a
// no-op) and the assurance and verified evidence it was decided with. A
// replay is authorized with these, never by verifying again.
type linkIntent struct {
	Action    string              `json:"action"`
	Assurance model.LinkAssurance `json:"assurance,omitempty"`
	Evidence  *verifiedEvidence   `json:"evidence,omitempty"`
}

type verifiedEvidence struct {
	Verifier  string              `json:"verifier"`
	Reference string              `json:"reference"`
	Assurance model.LinkAssurance `json:"assurance"`
}

// linkCall is one Link or Unlink in flight.
type linkCall struct {
	cl    *engine.Client
	kind  string // operation kind: identity.link or identity.unlink
	cmd   model.Command
	fp    []byte
	class model.LinkClass // "" for a no-op Link
	id    model.Identity  // as read before the unit
	// target is the Link target; zero for Unlink.
	target model.Contact
	req    model.LinkRequest
	// auth is what every decision of the call carries; stored for replay.
	auth linkIntent
	// Set by verify: the verifier-returned evidence (zero when none was
	// supplied), the verifier's version and the anchor the facts name.
	ev      model.Evidence
	version string
	anchor  *model.IdentityRef
	// decision is the allow that admitted the change: the override's when
	// one was needed, else the class action's.
	decision policy.Decision
}

func (lc *linkCall) resource() model.Ref { return lc.id.Ref.Ref() }

// authorization is the question for action, carrying the call's
// destination, expected revision and evidence context. Only the link
// actions name the target; a no-op's identity.read carries no evidence
// context.
func (lc *linkCall) authorization(action string, ctxt linkIntent) policy.Authorization {
	a := policy.Authorization{Action: action, Resource: lc.resource(), Revision: lc.cmd.ExpectedRevision}
	switch action {
	case model.ActionIdentityLink, model.ActionIdentityRelink, model.ActionIdentityLinkOverride:
		a.Destination = new(lc.target.Ref.Ref())
	}
	if action == model.ActionIdentityRead {
		return a
	}
	a.Assurance = ctxt.Assurance
	if e := ctxt.Evidence; e != nil {
		a.Evidence = &policy.VerifiedEvidence{Verifier: e.Verifier, Reference: e.Reference, Assurance: e.Assurance}
	}
	return a
}

func (lc *linkCall) allow(ctx context.Context, action string, ctxt linkIntent) (policy.Decision, error) {
	d, err := lc.cl.Decide(ctx, lc.authorization(action, ctxt), lc.resource())
	if err != nil {
		return policy.Decision{}, err
	}
	if !d.Allow {
		return policy.Decision{}, model.NewError(model.Unauthorized, "denied by policy")
	}
	return d, nil
}

// replay returns the stored result of an operation already claimed under
// the call's key, authorized with its stored action and evidence context
// before anything is revealed; false when none is stored. The key is the
// caller's own, so a different fingerprint is model.Conflict at once.
func (lc *linkCall) replay(ctx context.Context) (model.Acceptance, bool, error) {
	e, p := lc.cl.Engine(), lc.cl.Principal()
	key := store.OperationKey{Tenant: p.Tenant, Caller: p.Actor, Kind: lc.kind, Target: lc.resource(), ID: lc.cmd.Operation}
	var rec store.OperationRecord
	err := e.Store().View(ctx, p.Tenant, func(r store.Reader) (err error) {
		rec, err = r.OperationByKey(ctx, key)
		return err
	})
	switch {
	case model.IsCode(err, model.NotFound):
		return model.Acceptance{}, false, nil
	case err != nil:
		return model.Acceptance{}, false, err
	case rec.FingerprintVersion != operations.FingerprintVersion1 || !bytes.Equal(rec.Fingerprint, lc.fp):
		return model.Acceptance{}, false, model.NewError(model.Conflict, "operation ID already used with a different payload")
	}
	var intent localIntent
	if rec.Command.Name != intentLocal || json.Unmarshal(rec.Command.Data, &intent) != nil || intent.Link == nil {
		return model.Acceptance{}, false, model.NewError(model.Permanent, "stored operation intent is unreadable")
	}
	if _, err := lc.allow(ctx, intent.Link.Action, *intent.Link); err != nil {
		return model.Acceptance{}, false, err
	}
	return model.Acceptance{Operation: operations.Shown(rec), Durable: e.Store().Properties().Durable, Replayed: true}, true, nil
}

// fresh refuses a request whose expected revision is not the identity's
// current one, before any verifier I/O. The commit compares again.
func (lc *linkCall) fresh() error {
	if lc.cmd.ExpectedRevision != lc.id.Revision {
		return staleIdentity()
	}
	return nil
}

// errTargetMerged refuses a Link to a contact merged into another: it holds
// no identity any more, and its redirect names where they went.
func errTargetMerged() error {
	return model.NewError(model.Conflict, "contact was merged into another; link to that one")
}

func staleIdentity() error { return model.NewError(model.Conflict, "identity revision changed") }

// verify asks the verifier registered for the evidence's kind, outside any
// unit, and checks what it returns before any decision. No evidence (an
// Unlink only) is assurance none with no evidence. Unverified evidence never
// reaches the authorizer: an unregistered kind is model.Unsupported and any
// other failure model.Validation.
func (lc *linkCall) verify(ctx context.Context) error {
	if lc.req.Evidence.IsZero() {
		lc.auth.Assurance = model.LinkAssuranceNone
		return nil
	}
	e, p := lc.cl.Engine(), lc.cl.Principal()
	v, err := e.IdentityVerifier(lc.req.Evidence.Verifier)
	if err != nil {
		return err
	}
	ev, err := v.Verify(ctx, p, lc.req.Clone())
	if err != nil {
		return verifierError(ctx, err)
	}
	if err := ev.ValidateReturn(lc.req, p.Actor, e.Clock().Now()); err != nil {
		return err
	}
	facts, err := ev.DecodeFacts()
	if err != nil {
		return err
	}
	if f, ok := facts.(model.ImportFacts); ok && f.IdentityKey != identityKey(lc.id) {
		return model.NewError(model.Validation, "evidence.facts.identity_key: must be the identity's key")
	}
	if anchor := facts.Binding().Anchor; anchor != nil {
		var on model.Identity
		err := view(ctx, lc.cl, func(r store.Reader) error { return loadIdentity(ctx, r, *anchor, &on) })
		if err != nil && !model.IsCode(err, model.NotFound) {
			return err
		}
		if err != nil || on.Contact != lc.target.Ref {
			return model.NewError(model.Validation, "evidence.facts.anchor: must be on the target contact")
		}
		lc.anchor = new(*anchor)
	}
	if vv, ok := v.(policy.VersionedVerifier); ok {
		lc.version = vv.Version()
	}
	lc.ev = ev.Clone()
	lc.auth.Assurance = ev.Assurance
	lc.auth.Evidence = &verifiedEvidence{Verifier: ev.Verifier, Reference: ev.Reference, Assurance: ev.Assurance}
	return nil
}

// verifierError keeps context errors and retryable typed failures; any
// other verifier failure is evidence not verified, without its text.
func verifierError(ctx context.Context, err error) error {
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return err
	}
	switch model.CodeOf(err) {
	case model.Unavailable, model.RateLimited, model.Transient:
		var me *model.Error
		errors.As(err, &me)
		return me
	}
	return model.NewError(model.Validation, "evidence: not verified")
}

func identityKey(id model.Identity) model.IdentityKey {
	return model.IdentityKey{Inbox: id.Ref.Inbox, Namespace: id.Namespace, SourceID: id.SourceID}
}

// decide asks the class action and, for a link or relink whose verified
// assurance is below the identity's current link assurance,
// identity.link_override too, with the same evidence context. It keeps the
// deciding allow for the record.
func (lc *linkCall) decide(ctx context.Context) error {
	d, err := lc.allow(ctx, lc.auth.Action, lc.auth)
	if err != nil {
		return err
	}
	if lc.class != model.LinkClassUnlink && lc.class != "" && lc.auth.Assurance.Below(lc.id.LinkAssurance) {
		if d, err = lc.allow(ctx, model.ActionIdentityLinkOverride, lc.auth); err != nil {
			return err
		}
	}
	lc.decision = d
	return nil
}

// commit decides, then claims the operation and applies the change in one
// unit. IDs are allocated first: operation, its revision, link record,
// identity revision, event, and for Unlink the fresh contact and its
// revision.
func (lc *linkCall) commit(ctx context.Context) (model.Acceptance, error) {
	ids, err := allocate(ctx, lc.cl.Engine(), 7)
	if err != nil {
		return model.Acceptance{}, err
	}
	res := lc.resource()
	return commit(ctx, lc.cl, localCommand{
		kind: lc.kind, target: res, resource: res, owner: res,
		command: lc.cmd, fingerprint: lc.fp, result: res,
		authorize: lc.decide, link: &lc.auth,
		apply: func(ctx context.Context, tx store.Tx, op model.OperationRef) error {
			return lc.apply(ctx, tx, op, ids[2:])
		},
	}, ids[0], model.Revision(ids[1]))
}

// apply re-reads the identity at the expected revision and, unless the
// call is a no-op, the target (gone: not found) and the anchor (moved off
// the target: model.Conflict), then writes the fresh contact for Unlink,
// the link record with the pointer move, and identity.linked.
func (lc *linkCall) apply(ctx context.Context, tx store.Tx, op model.OperationRef, ids []model.ID) error {
	var cur model.Identity
	if err := loadIdentity(ctx, tx, lc.id.Ref, &cur); err != nil {
		return err
	}
	if cur.Revision != lc.cmd.ExpectedRevision {
		return staleIdentity()
	}
	if lc.class == "" {
		return nil
	}
	to := lc.target.Ref
	assurance := lc.ev.Assurance
	if lc.class == model.LinkClassUnlink {
		to, assurance = model.ContactRef{Tenant: op.Tenant, ID: ids[3]}, model.LinkAssuranceNone
		if err := tx.PutContact(ctx, model.Contact{Ref: to, Kind: cur.Kind, Revision: model.Revision(ids[4])}, ""); err != nil {
			return err
		}
	} else if err := lc.recheck(ctx, tx); err != nil {
		return err
	}
	head, err := linkHead(ctx, tx, cur.Ref)
	if err != nil {
		return err
	}
	rec := store.LinkRecord{
		ID: ids[0], Tenant: op.Tenant, Identity: cur.Ref, IdentityKey: identityKey(cur),
		Class: lc.class, From: cur.Contact, To: to, Supersedes: head,
		Evidence: lc.ev.Clone(), VerifierVersion: lc.version, Assurance: assurance, Anchor: lc.anchor,
		Actor: lc.cl.Principal().Actor, Operation: op, Correlation: lc.cmd.Correlation,
		PolicyVersion: lc.decision.PolicyVersion, ReasonCode: lc.decision.ReasonCode,
		RevisionBefore: cur.Revision, RevisionAfter: model.Revision(ids[1]),
		RecordedAt: lc.cl.Engine().Clock().Now().UTC(), Reason: lc.req.Reason,
	}
	if err := tx.LinkIdentity(ctx, rec); err != nil {
		return err
	}
	ev, err := commandEvent(lc.cl, ids[2], model.EventIdentityLinked, model.SchemaIdentityLinked,
		lc.resource(), rec.RevisionAfter, model.Audience{Visibility: model.Internal}, lc.cmd.Correlation,
		model.IdentityLinkedPayload{Link: rec.ID, Class: rec.Class, From: rec.From, To: rec.To, Assurance: rec.Assurance})
	if err != nil {
		return err
	}
	return tx.AppendEvents(ctx, []model.Event{ev})
}

// recheck re-reads a Link's target, which a retention purge may have
// removed, and its anchor, which may have moved since verification.
func (lc *linkCall) recheck(ctx context.Context, tx store.Tx) error {
	var target model.Contact
	if err := loadContact(ctx, tx, lc.target.Ref, &target); err != nil {
		return err
	}
	if target.MergedInto != nil {
		return errTargetMerged()
	}
	if target.Kind != lc.id.Kind {
		return model.NewError(model.Validation, "contact: must be of the identity's kind")
	}
	if lc.anchor == nil {
		return nil
	}
	var anchor model.Identity
	err := loadIdentity(ctx, tx, *lc.anchor, &anchor)
	if err != nil && !model.IsCode(err, model.NotFound) {
		return err
	}
	if err != nil || anchor.Contact != target.Ref {
		return model.NewError(model.Conflict, "anchor identity is no longer on the target contact")
	}
	return nil
}
