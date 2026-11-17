package services

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Get returns one contact the principal may read, admitted as List admits
// it: model.ActionContactReadAll on the contact, else model.ActionIdentityRead
// on one of its current identities. A contact of another tenant, or one the
// principal may not read, is not found or unauthorized as for List's single
// item; the revision it carries is the one Update expects.
func (s Contacts) Get(ctx context.Context, req model.Lookup[model.ContactRef]) (model.Contact, error) {
	c := s.c
	done, err := c.Engine().Begin()
	if err != nil {
		return model.Contact{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Contact{}, err
	}
	if req.Ref.Tenant != c.Principal().Tenant {
		return model.Contact{}, engine.ErrNotFound()
	}
	var ct model.Contact
	if err := view(ctx, c, func(r store.Reader) error { return loadContact(ctx, r, req.Ref, &ct) }); err != nil {
		return model.Contact{}, err
	}
	if err := admitContact(ctx, c, ct); err != nil {
		return model.Contact{}, err
	}
	return ct.Clone(), nil
}
