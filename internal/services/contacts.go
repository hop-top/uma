package services

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Link points an identity at an existing contact of the identity's kind,
// as a committed operation keyed identity.link on the identity whatever its
// class. Per call: validate; scope (another tenant is not found); read the
// identity and the target; classify: no-op when the target is the current
// contact, link while the identity has no link record (its provisional
// contact), relink otherwise; replay a stored operation under the same key,
// authorized with its stored action and verified evidence, without
// verifying again; refuse a stale revision (model.Conflict) and a kind
// mismatch (model.Validation, no override); verify the evidence outside any
// unit; decide the class action, plus identity.link_override when the
// verified assurance is below the current one; then, in one unit, claim
// the operation, re-read the identity, target and anchor, append the link
// record, move the pointer and append identity.linked.
//
// A no-op is not verified: it is decided as identity.read with no evidence
// context and commits a ledger row only, with no record, event or revision
// change.
func (s Contacts) Link(ctx context.Context, req model.LinkIdentity) (model.Acceptance, error) {
	c, e := s.c, s.c.Engine()
	done, err := e.Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	tenant := c.Principal().Tenant
	if req.Identity.Inbox.Tenant != tenant || req.Contact.Tenant != tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	lc := &linkCall{cl: c, kind: model.ActionIdentityLink, cmd: req.Command, fp: linkFingerprint(req)}
	var head model.ID
	if err := view(ctx, c, func(r store.Reader) (err error) {
		if err = loadIdentity(ctx, r, req.Identity, &lc.id); err != nil {
			return err
		}
		if err = loadContact(ctx, r, req.Contact, &lc.target); err != nil {
			return err
		}
		head, err = linkHead(ctx, r, req.Identity)
		return err
	}); err != nil {
		return model.Acceptance{}, err
	}
	switch {
	case lc.target.Ref == lc.id.Contact:
		lc.auth.Action = model.ActionIdentityRead
	case head == "":
		lc.class, lc.auth.Action = model.LinkClassLink, model.ActionIdentityLink
	default:
		lc.class, lc.auth.Action = model.LinkClassRelink, model.ActionIdentityRelink
	}
	ev := req.Evidence.Clone()
	ev.Assurance = ""
	lc.req = model.LinkRequest{
		Intent: lc.class, Identity: lc.id.Ref, Current: lc.id.Contact,
		ExpectedRevision: req.ExpectedRevision, Target: &lc.target.Ref, Evidence: ev,
	}
	if acc, ok, err := lc.replay(ctx); ok || err != nil {
		return acc, err
	}
	if err := lc.fresh(); err != nil {
		return model.Acceptance{}, err
	}
	if lc.class == "" {
		return lc.commit(ctx)
	}
	if lc.target.MergedInto != nil {
		return model.Acceptance{}, errTargetMerged()
	}
	if lc.target.Kind != lc.id.Kind {
		return model.Acceptance{}, model.NewError(model.Validation, "contact: must be of the identity's kind")
	}
	if err := lc.req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if err := lc.verify(ctx); err != nil {
		return model.Acceptance{}, err
	}
	return lc.commit(ctx)
}

// Unlink detaches an identity to a fresh contact of the identity's kind,
// allocated in the commit, as a committed operation keyed identity.unlink
// on the identity. The flow is Link's with the unlink class: no target and
// no kind or assurance rule; the reason is required; evidence is optional
// and, when supplied, verified for the unlink intent. The record's
// assurance is none.
func (s Contacts) Unlink(ctx context.Context, req model.UnlinkIdentity) (model.Acceptance, error) {
	c, e := s.c, s.c.Engine()
	done, err := e.Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if req.Identity.Inbox.Tenant != c.Principal().Tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	lc := &linkCall{
		cl: c, kind: model.ActionIdentityUnlink, cmd: req.Command, fp: unlinkFingerprint(req),
		class: model.LinkClassUnlink, auth: linkIntent{Action: model.ActionIdentityUnlink},
	}
	if err := view(ctx, c, func(r store.Reader) error {
		return loadIdentity(ctx, r, req.Identity, &lc.id)
	}); err != nil {
		return model.Acceptance{}, err
	}
	ev := req.Evidence.Clone()
	ev.Assurance = ""
	lc.req = model.LinkRequest{
		Intent: model.LinkClassUnlink, Identity: lc.id.Ref, Current: lc.id.Contact,
		ExpectedRevision: req.ExpectedRevision, Reason: req.Reason, Evidence: ev,
	}
	if err := lc.req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if acc, ok, err := lc.replay(ctx); ok || err != nil {
		return acc, err
	}
	if err := lc.fresh(); err != nil {
		return model.Acceptance{}, err
	}
	if err := lc.verify(ctx); err != nil {
		return model.Acceptance{}, err
	}
	return lc.commit(ctx)
}

// linkFingerprint covers a Link's identity, target, evidence (verifier,
// reference, facts) and expected revision.
func linkFingerprint(req model.LinkIdentity) []byte {
	f := evidenceFingerprint(model.ActionIdentityLink, req.Identity, req.Evidence, req.ExpectedRevision)
	f.Set("contact", operations.Field().Ref(req.Contact.Ref()))
	return f.Sum()
}

// unlinkFingerprint covers an Unlink's identity, reason, evidence and
// expected revision.
func unlinkFingerprint(req model.UnlinkIdentity) []byte {
	f := evidenceFingerprint(model.ActionIdentityUnlink, req.Identity, req.Evidence, req.ExpectedRevision)
	f.Set("reason", operations.Field().Str(req.Reason))
	return f.Sum()
}

func evidenceFingerprint(kind string, id model.IdentityRef, ev model.Evidence, rev model.Revision) *operations.Fingerprint {
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(kind))
	f.Set("identity", operations.Field().Ref(id.Ref()))
	f.Set("verifier", operations.Field().Str(ev.Verifier))
	f.Set("reference", operations.Field().Str(ev.Reference))
	f.Set("facts", operations.Field().Extension(ev.Facts))
	f.Set("expected_revision", operations.Field().Str(string(rev)))
	return f
}

// loadIdentity reads ref and requires the stored record to carry exactly
// that reference. A mismatch is not found.
func loadIdentity(ctx context.Context, r store.Reader, ref model.IdentityRef, out *model.Identity) error {
	id, err := r.GetIdentity(ctx, ref)
	if err != nil {
		return err
	}
	if id.Ref != ref {
		return engine.ErrNotFound()
	}
	*out = id
	return nil
}

// loadContact reads ref and requires the stored record to carry exactly
// that reference. A mismatch is not found.
func loadContact(ctx context.Context, r store.Reader, ref model.ContactRef, out *model.Contact) error {
	ct, err := r.GetContact(ctx, ref)
	if err != nil {
		return err
	}
	if ct.Ref != ref {
		return engine.ErrNotFound()
	}
	*out = ct
	return nil
}

// linkHead returns the ID of the identity's latest link record, or "" when
// it has none: the identity is still on its provisional contact.
func linkHead(ctx context.Context, r store.Reader, ref model.IdentityRef) (model.ID, error) {
	var head model.ID
	var after model.Cursor
	for {
		p, err := r.IdentityLinks(ctx, store.LinkFilter{Identity: &ref}, model.PageRequest{Limit: MaxPageLimit, After: after})
		if err != nil {
			return "", err
		}
		if n := len(p.Items); n > 0 {
			head = p.Items[n-1].ID
		}
		if p.Complete || len(p.Items) == 0 || p.Next == after {
			return head, nil
		}
		after = p.Next
	}
}
