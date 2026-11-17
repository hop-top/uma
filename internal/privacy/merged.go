package privacy

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// maxRedirects bounds the redirects followed from one contact. Merging
// never forms a cycle (a merged contact is neither merged again nor a
// target), so a longer chain is a corrupt store.
const maxRedirects = 64

// redirects follows the redirects Contacts.Merge left from ref to the
// contact holding the identities that were merged away, and returns every
// contact on the way, ref's first and the survivor last. A contact that was
// never merged is its own survivor. A redirect whose target a retention
// purge removed ends the chain at the last contact that still exists.
func redirects(ctx context.Context, r store.Reader, ref model.ContactRef) ([]model.Contact, error) {
	cur, err := r.GetContact(ctx, ref)
	if err != nil {
		return nil, err
	}
	if cur.Ref != ref {
		return nil, engine.ErrNotFound()
	}
	chain := []model.Contact{cur}
	for range maxRedirects {
		if cur.MergedInto == nil {
			return chain, nil
		}
		next, err := r.GetContact(ctx, *cur.MergedInto)
		switch {
		case model.IsCode(err, model.NotFound):
			return chain, nil
		case err != nil:
			return nil, err
		case next.Ref != *cur.MergedInto:
			return nil, engine.ErrNotFound()
		}
		chain = append(chain, next)
		cur = next
	}
	return nil, model.NewError(model.Permanent, "contact redirects do not end")
}

// surviving is the last contact of redirects.
func surviving(ctx context.Context, r store.Reader, ref model.ContactRef) (model.Contact, error) {
	chain, err := redirects(ctx, r, ref)
	if err != nil {
		return model.Contact{}, err
	}
	return chain[len(chain)-1], nil
}
