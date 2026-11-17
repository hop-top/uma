package services

import (
	"bytes"
	"context"
	"slices"
	"strings"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Merge merges one contact into another as a committed operation keyed
// contact.merge on the contact merged. Per call: validate; scope (another
// tenant is not found); read both contacts and the identities on the one
// merged; replay a stored operation under the same key, authorized again;
// refuse stale revisions, a contact already merged, a kind mismatch and
// reviewed identities that are not the ones on the contact (see
// mergeCall.fresh); decide model.ActionContactMerge on the contact merged
// and, for every identity it holds, model.ActionIdentityRelink at the
// asserted level plus model.ActionIdentityLinkOverride when that is below
// the identity's link assurance; then, in one unit, claim the operation,
// re-read both contacts and the identities, append one link record of
// class merge and one identity.linked event per identity, set the
// redirect on the contact merged, bump both contacts' revisions and append
// contact.merged. A race with another merge, a Link or an Unlink touching
// either contact or any of its identities fails the unit's revision checks
// and is model.Conflict.
func (s Contacts) Merge(ctx context.Context, req model.MergeContacts) (model.Acceptance, error) {
	c, e := s.c, s.c.Engine()
	done, err := e.Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if err := mergeScope(c, req.From, req.Into, req.Identities); err != nil {
		return model.Acceptance{}, err
	}
	mc := &mergeCall{cl: c, req: req}
	if err := mc.read(ctx, req.From, req.Into); err != nil {
		return model.Acceptance{}, err
	}
	if acc, ok, err := mc.replay(ctx); ok || err != nil {
		return acc, err
	}
	if err := mc.fresh(); err != nil {
		return model.Acceptance{}, err
	}
	return mc.commit(ctx)
}

// mergeScope requires every ref in the principal's tenant; another tenant's
// is not found.
func mergeScope(c *engine.Client, from, into model.ContactRef, ids []model.IdentityRef) error {
	tenant := c.Principal().Tenant
	if from.Tenant != tenant || into.Tenant != tenant {
		return engine.ErrNotFound()
	}
	for _, id := range ids {
		if id.Inbox.Tenant != tenant {
			return engine.ErrNotFound()
		}
	}
	return nil
}

// mergeCall is one Merge or PreviewMerge in flight.
type mergeCall struct {
	cl  *engine.Client
	req model.MergeContacts
	// Read before the unit: both contacts and the identities on from, in
	// store order, at the revisions the decisions are made for.
	from, into model.Contact
	idents     []model.Identity
	// decisions are the allows that admitted each identity, in idents
	// order: the override's when one was needed, else the relink's.
	decisions []policy.Decision
}

func (mc *mergeCall) resource() model.Ref { return mc.req.From.Ref() }

// read loads both contacts and the identities on from. A merge holds at
// most model.MaxMergeIdentities.
func (mc *mergeCall) read(ctx context.Context, from, into model.ContactRef) error {
	return view(ctx, mc.cl, func(r store.Reader) (err error) {
		if err = loadContact(ctx, r, from, &mc.from); err != nil {
			return err
		}
		if err = loadContact(ctx, r, into, &mc.into); err != nil {
			return err
		}
		mc.idents, err = identitiesOn(ctx, r, from)
		return err
	})
}

// identitiesOn returns the identities on contact in store order, or
// model.Validation when there are more than model.MaxMergeIdentities.
func identitiesOn(ctx context.Context, r store.Reader, contact model.ContactRef) ([]model.Identity, error) {
	var out []model.Identity
	var after model.Cursor
	for {
		p, err := r.Identities(ctx, model.ListRequest{
			Filter: model.Filter{Contact: &contact}, Page: model.PageRequest{Limit: MaxPageLimit, After: after},
		})
		if err != nil {
			return nil, err
		}
		for _, id := range p.Items {
			if id.Contact != contact {
				continue
			}
			if out = append(out, id); len(out) > model.MaxMergeIdentities {
				return nil, model.NewError(model.Validation, "contact: holds too many identities to merge; relink some first")
			}
		}
		if p.Complete || len(p.Items) == 0 || p.Next == after {
			return out, nil
		}
		after = p.Next
	}
}

func staleContact() error {
	return model.NewError(model.Conflict, "contact revision or identities changed; preview the merge again")
}

// fresh refuses a merge the contacts as read cannot take, before any
// decision: a revision other than the one expected on either contact, a
// contact already merged (the one merged or the one merged into), a kind
// mismatch, or reviewed identities other than the ones on the contact.
// The commit compares again.
func (mc *mergeCall) fresh() error {
	switch {
	case mc.from.Revision != mc.req.ExpectedRevision || mc.into.Revision != mc.req.ExpectedIntoRevision:
		return staleContact()
	case mc.from.MergedInto != nil:
		return model.NewError(model.Conflict, "contact already merged into another")
	case mc.into.MergedInto != nil:
		return model.NewError(model.Conflict, "contact into was merged into another; merge into that one")
	case mc.from.Kind != mc.into.Kind:
		return model.NewError(model.Validation, "into: must be of the kind of the contact merged")
	}
	for _, id := range mc.idents {
		if id.Kind != mc.into.Kind {
			return model.NewError(model.Validation, "into: must be of the kind of every identity merged")
		}
	}
	if !sameIdentitySet(mc.req.Identities, mc.idents) {
		return staleContact()
	}
	return nil
}

// sameIdentitySet reports whether reviewed names exactly the identities in
// held, in any order.
func sameIdentitySet(reviewed []model.IdentityRef, held []model.Identity) bool {
	if len(reviewed) != len(held) {
		return false
	}
	for _, id := range held {
		if !slices.Contains(reviewed, id.Ref) {
			return false
		}
	}
	return true
}

// mergeFingerprint covers the contacts, both expected revisions, the
// reason and the reviewed identities, which are a set.
func mergeFingerprint(req model.MergeContacts) []byte {
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionContactMerge))
	f.Set("from", operations.Field().Ref(req.From.Ref()))
	f.Set("into", operations.Field().Ref(req.Into.Ref()))
	f.Set("expected_revision", operations.Field().Str(string(req.ExpectedRevision)))
	f.Set("expected_into_revision", operations.Field().Str(string(req.ExpectedIntoRevision)))
	f.Set("reason", operations.Field().Str(req.Reason))
	keys := make([]string, len(req.Identities))
	for i, id := range req.Identities {
		keys[i] = string(id.Inbox.ID) + "/" + string(id.ID)
	}
	slices.Sort(keys)
	f.Set("identities", operations.Field().Str(strings.Join(keys, "\n")))
	return f.Sum()
}

// gate is the merge question: contact.merge on the contact merged, with
// the contact merged into as destination, at the contact's revision.
func (mc *mergeCall) gate() policy.Authorization {
	into := mc.req.Into.Ref()
	return policy.Authorization{
		Action: model.ActionContactMerge, Resource: mc.resource(), Destination: &into, Revision: mc.req.ExpectedRevision,
	}
}

// replay returns the stored result of an operation already claimed under
// the call's key, authorized again as contact.merge before anything is
// revealed; false when none is stored. The key is the caller's own, so a
// different fingerprint is model.Conflict at once.
func (mc *mergeCall) replay(ctx context.Context) (model.Acceptance, bool, error) {
	e, p := mc.cl.Engine(), mc.cl.Principal()
	key := store.OperationKey{Tenant: p.Tenant, Caller: p.Actor, Kind: model.ActionContactMerge, Target: mc.resource(), ID: mc.req.Operation}
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
	case rec.FingerprintVersion != operations.FingerprintVersion1 || !bytes.Equal(rec.Fingerprint, mergeFingerprint(mc.req)):
		return model.Acceptance{}, false, model.NewError(model.Conflict, "operation ID already used with a different payload")
	}
	if _, err := mc.allow(ctx, mc.gate()); err != nil {
		return model.Acceptance{}, false, err
	}
	return model.Acceptance{Operation: operations.Shown(rec), Durable: e.Store().Properties().Durable, Replayed: true}, true, nil
}

// allow asks the policy about a and requires an explicit allow. A resource
// that vanished is a denial: a merge never confirms to a caller what it may
// not relink.
func (mc *mergeCall) allow(ctx context.Context, a policy.Authorization) (policy.Decision, error) {
	d, err := mc.cl.Decide(ctx, a, a.Resource)
	switch {
	case model.IsCode(err, model.NotFound):
		return policy.Decision{}, model.NewError(model.Unauthorized, "denied by policy")
	case err != nil:
		return policy.Decision{}, err
	case !d.Allow:
		return policy.Decision{}, model.NewError(model.Unauthorized, "denied by policy")
	}
	return d, nil
}

// decide asks the merge question, then for every identity relink at the
// asserted level and, when that is below the identity's link assurance,
// override too. Any denial refuses the whole merge: a merge is no more
// permissive than relinking each identity. Decisions are asked in the
// order the identities were read.
func (mc *mergeCall) decide(ctx context.Context) error {
	if _, err := mc.allow(ctx, mc.gate()); err != nil {
		return err
	}
	into := mc.req.Into.Ref()
	mc.decisions = make([]policy.Decision, 0, len(mc.idents))
	for _, id := range mc.idents {
		a := policy.Authorization{
			Action: model.ActionIdentityRelink, Resource: id.Ref.Ref(), Destination: &into, Revision: id.Revision,
			Assurance: model.LinkAssuranceAsserted,
		}
		d, err := mc.allow(ctx, a)
		if err != nil {
			return err
		}
		if model.LinkAssuranceAsserted.Below(id.LinkAssurance) {
			a.Action = model.ActionIdentityLinkOverride
			if d, err = mc.allow(ctx, a); err != nil {
				return err
			}
		}
		mc.decisions = append(mc.decisions, d)
	}
	return nil
}

// commit decides, then claims the operation and applies the merge in one
// unit. IDs are allocated first: operation, its revision, the contact.merged
// event, the revisions of both contacts, then a link record, an identity
// revision and an identity.linked event per identity.
func (mc *mergeCall) commit(ctx context.Context) (model.Acceptance, error) {
	const fixed, per = 5, 3
	ids, err := allocate(ctx, mc.cl.Engine(), fixed+per*len(mc.idents))
	if err != nil {
		return model.Acceptance{}, err
	}
	res := mc.resource()
	return commit(ctx, mc.cl, localCommand{
		kind: model.ActionContactMerge, target: res, resource: res, owner: res,
		command: mc.req.Command, fingerprint: mergeFingerprint(mc.req), result: mc.req.Into.Ref(),
		authorize: mc.decide,
		apply: func(ctx context.Context, tx store.Tx, op model.OperationRef) error {
			return mc.apply(ctx, tx, op, ids[2:])
		},
	}, ids[0], model.Revision(ids[1]))
}

// apply re-reads both contacts and the identities in the unit and refuses
// anything that moved since the decisions: a contact revision, a contact
// merged, the identities or any identity's revision. It then moves every
// identity, writes the redirect and appends the events.
func (mc *mergeCall) apply(ctx context.Context, tx store.Tx, op model.OperationRef, ids []model.ID) error {
	var from, into model.Contact
	if err := loadContact(ctx, tx, mc.from.Ref, &from); err != nil {
		return err
	}
	if err := loadContact(ctx, tx, mc.into.Ref, &into); err != nil {
		return err
	}
	if from.Revision != mc.req.ExpectedRevision || into.Revision != mc.req.ExpectedIntoRevision ||
		from.MergedInto != nil || into.MergedInto != nil {
		return staleContact()
	}
	held, err := identitiesOn(ctx, tx, from.Ref)
	if err != nil {
		return err
	}
	if !slices.EqualFunc(held, mc.idents, func(a, b model.Identity) bool { return a.Ref == b.Ref && a.Revision == b.Revision }) {
		return staleContact()
	}
	cl := mc.cl
	now := cl.Engine().Clock().Now().UTC()
	mergeEvent, fromRev, intoRev := ids[0], model.Revision(ids[1]), model.Revision(ids[2])
	moved := make([]model.IdentityRef, len(held))
	links := make([]model.ID, len(held))
	events := make([]model.Event, 0, len(held)+1)
	for i, id := range held {
		base := ids[3+3*i:]
		head, err := linkHead(ctx, tx, id.Ref)
		if err != nil {
			return err
		}
		d := mc.decisions[i]
		rec := store.LinkRecord{
			ID: base[0], Tenant: op.Tenant, Identity: id.Ref, IdentityKey: identityKey(id),
			Class: model.LinkClassMerge, From: from.Ref, To: into.Ref, Supersedes: head,
			Assurance: model.LinkAssuranceAsserted, Actor: cl.Principal().Actor, Operation: op,
			Correlation: mc.req.Correlation, PolicyVersion: d.PolicyVersion, ReasonCode: d.ReasonCode,
			RevisionBefore: id.Revision, RevisionAfter: model.Revision(base[1]), RecordedAt: now, Reason: mc.req.Reason,
		}
		if err := tx.LinkIdentity(ctx, rec); err != nil {
			return err
		}
		ev, err := commandEvent(cl, base[2], model.EventIdentityLinked, model.SchemaIdentityLinked,
			id.Ref.Ref(), rec.RevisionAfter, model.Audience{Visibility: model.Internal}, mc.req.Correlation,
			model.IdentityLinkedPayload{Link: rec.ID, Class: rec.Class, From: rec.From, To: rec.To, Assurance: rec.Assurance})
		if err != nil {
			return err
		}
		moved[i], links[i] = id.Ref, rec.ID
		events = append(events, ev)
	}
	from.MergedInto, from.Revision = new(into.Ref), fromRev
	if err := tx.PutContact(ctx, from, mc.req.ExpectedRevision); err != nil {
		return err
	}
	into.Revision = intoRev
	if err := tx.PutContact(ctx, into, mc.req.ExpectedIntoRevision); err != nil {
		return err
	}
	ev, err := commandEvent(cl, mergeEvent, model.EventContactMerged, model.SchemaContactMerged,
		into.Ref.Ref(), intoRev, model.Audience{Visibility: model.Internal}, mc.req.Correlation,
		model.ContactMergedPayload{From: from.Ref, Into: into.Ref, Identities: moved, Links: links, Operation: op})
	if err != nil {
		return err
	}
	return tx.AppendEvents(ctx, append(events, ev))
}
