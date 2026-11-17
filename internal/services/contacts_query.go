package services

import (
	"context"
	"slices"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Contact queries. A link grants no access: each query authorizes per
// identity, so identities, and the history of identities, in inboxes the
// principal may not read stay hidden whatever contact they share.

// List pages the contacts the principal may read, in recording order,
// optionally limited to Filter.Kinds. A contact is listed when
// contact.read_all on it allows, else when identity.read allows on at
// least one of its current identities; a contact without identities is
// listed only under contact.read_all. Admission through identities reads
// at most maxScan of the contact's identities and otherwise fails closed.
func (s Contacts) List(ctx context.Context, req model.ListRequest) (model.Page[model.Contact], error) {
	return list(ctx, s.c, listQuery[model.Contact]{
		name: "contacts", fields: filterFields{kinds: true}, read: store.Reader.Contacts,
		within: func(f model.Filter, ct model.Contact) bool {
			return len(f.Kinds) == 0 || slices.Contains(f.Kinds, ct.Kind)
		},
		admit: admitContact,
		clone: model.Contact.Clone,
	}, req)
}

// Identities pages the identities the principal may read (identity.read),
// in recording order, optionally limited to Filter.Inboxes and
// Filter.Contact. Identities in inboxes the principal may not read are
// skipped without trace.
func (s Contacts) Identities(ctx context.Context, req model.ListRequest) (model.Page[model.Identity], error) {
	return list(ctx, s.c, identitiesQuery, req)
}

var identitiesQuery = listQuery[model.Identity]{
	name: "identities", fields: filterFields{inboxes: true, contact: true}, read: store.Reader.Identities,
	within: func(f model.Filter, id model.Identity) bool {
		return inInboxes(f, id.Ref.Inbox) && (f.Contact == nil || *f.Contact == id.Contact)
	},
	admit: func(ctx context.Context, c *engine.Client, id model.Identity) error {
		return c.Authorize(ctx, identityRead(id), id.Ref.Ref())
	},
	clone: model.Identity.Clone,
}

func identityRead(id model.Identity) policy.Authorization {
	return policy.Authorization{Action: model.ActionIdentityRead, Resource: id.Ref.Ref(), Revision: id.Revision}
}

// History pages link history records in append order, selected by
// Filter.Identity, Filter.Contact (as From or To), or both; one of them is
// required. Each record is authorized with identity.history on its
// identity at the record's resulting revision, so records of identities in
// inboxes the principal may not read are skipped without trace.
func (s Contacts) History(ctx context.Context, req model.ListRequest) (model.Page[model.IdentityLink], error) {
	return list(ctx, s.c, listQuery[model.IdentityLink]{
		name: "identity_links", fields: filterFields{contact: true, identity: true},
		require: func(f model.Filter) error {
			if f.Identity == nil && f.Contact == nil {
				return model.NewError(model.Validation, "filter.identity or filter.contact: one is required")
			}
			return nil
		},
		read: func(r store.Reader, ctx context.Context, req model.ListRequest) (model.Page[model.IdentityLink], error) {
			return r.IdentityLinks(ctx, store.LinkFilter{Identity: req.Filter.Identity, Contact: req.Filter.Contact}, req.Page)
		},
		within: func(f model.Filter, l model.IdentityLink) bool {
			return (f.Identity == nil || *f.Identity == l.Identity) &&
				(f.Contact == nil || *f.Contact == l.From || *f.Contact == l.To)
		},
		admit: func(ctx context.Context, c *engine.Client, l model.IdentityLink) error {
			a := policy.Authorization{Action: model.ActionIdentityHistory, Resource: l.Identity.Ref(), Revision: l.RevisionAfter}
			return c.Authorize(ctx, a, l.Identity.Ref())
		},
		clone: model.IdentityLink.Clone,
	}, req)
}

// admitContact authorizes one contact for List: contact.read_all on the
// contact, else identity.read on one of its current identities.
func admitContact(ctx context.Context, c *engine.Client, ct model.Contact) error {
	ref := ct.Ref.Ref()
	err := c.Authorize(ctx, policy.Authorization{Action: model.ActionContactReadAll, Resource: ref, Revision: ct.Revision}, ref)
	if err == nil || !hidden(err) {
		return err
	}
	f := model.Filter{Contact: &ct.Ref}
	var after model.Cursor
	for scanned := 0; scanned < maxScan; {
		var got model.Page[model.Identity]
		if err := view(ctx, c, func(r store.Reader) (err error) {
			got, err = r.Identities(ctx, model.ListRequest{Filter: f, Page: model.PageRequest{Limit: min(MaxPageLimit, maxScan-scanned), After: after}})
			return err
		}); err != nil {
			return err
		}
		scanned += max(len(got.Items), 1)
		// Authorization runs outside the store unit.
		for _, id := range got.Items {
			if !identitiesQuery.within(f, id) {
				continue
			}
			switch err := identitiesQuery.admit(ctx, c, id); {
			case err == nil:
				return nil
			case !hidden(err):
				return err
			}
		}
		if got.Complete || len(got.Items) == 0 || got.Next == after {
			break
		}
		after = got.Next
	}
	return model.NewError(model.Unauthorized, "denied by policy")
}

// hidden reports a denial that skips an item without trace.
func hidden(err error) bool {
	return model.IsCode(err, model.Unauthorized) || model.IsCode(err, model.NotFound)
}
