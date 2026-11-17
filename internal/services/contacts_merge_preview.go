package services

import (
	"context"
	"slices"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// maxPreviewScan bounds the conversations one preview scans to find those
// the moved identities own. A scan that stops there reports
// MoreConversations.
const maxPreviewScan = 20 * model.MaxMergePreviewConversations

// PreviewMerge lists what Merge of req.From into req.Into would move, and
// changes nothing. It refuses what Merge would refuse before any decision:
// a contact already merged and a kind mismatch. It decides
// model.ActionContactMerge on the contact merged and model.ActionIdentityRead
// on every identity on it: a principal that may not read what would move
// has not reviewed the merge, so one denial refuses the preview.
// Conversations lists those the identities own that the principal may read
// (model.ActionConversationRead), found by scanning the conversations of
// the identities' inboxes.
func (s Contacts) PreviewMerge(ctx context.Context, req model.PreviewMerge) (model.MergePreview, error) {
	c, e := s.c, s.c.Engine()
	done, err := e.Begin()
	if err != nil {
		return model.MergePreview{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.MergePreview{}, err
	}
	if err := mergeScope(c, req.From, req.Into, nil); err != nil {
		return model.MergePreview{}, err
	}
	mc := &mergeCall{cl: c, req: model.MergeContacts{From: req.From, Into: req.Into}}
	if err := mc.read(ctx, req.From, req.Into); err != nil {
		return model.MergePreview{}, err
	}
	// Preview what the contacts are now: fresh then refuses only a contact
	// already merged and a kind mismatch.
	mc.req.ExpectedRevision, mc.req.ExpectedIntoRevision = mc.from.Revision, mc.into.Revision
	mc.req.Identities = make([]model.IdentityRef, len(mc.idents))
	for i, id := range mc.idents {
		mc.req.Identities[i] = id.Ref
	}
	if err := mc.fresh(); err != nil {
		return model.MergePreview{}, err
	}
	if _, err := mc.allow(ctx, mc.gate()); err != nil {
		return model.MergePreview{}, err
	}
	out := model.MergePreview{
		From: mc.from.Ref, Into: mc.into.Ref, FromRevision: mc.from.Revision, IntoRevision: mc.into.Revision,
		Identities: make([]model.IdentityRef, len(mc.idents)),
	}
	var inboxes []model.InboxRef
	for i, id := range mc.idents {
		if _, err := mc.allow(ctx, identityRead(id)); err != nil {
			return model.MergePreview{}, err
		}
		out.Identities[i] = id.Ref
		if !slices.Contains(inboxes, id.Ref.Inbox) {
			inboxes = append(inboxes, id.Ref.Inbox)
		}
	}
	if len(inboxes) > 0 {
		if out.Conversations, out.MoreConversations, err = ownedConversations(ctx, c, out.Identities, inboxes); err != nil {
			return model.MergePreview{}, err
		}
	}
	return out, nil
}

// ownedConversations scans the conversations of inboxes for those owned by
// one of owners and readable by the principal, up to the preview bounds.
func ownedConversations(ctx context.Context, c *engine.Client, owners []model.IdentityRef, inboxes []model.InboxRef) ([]model.ConversationRef, bool, error) {
	var out []model.ConversationRef
	var after model.Cursor
	for scanned := 0; ; {
		var got model.Page[model.Conversation]
		if err := view(ctx, c, func(r store.Reader) (err error) {
			got, err = r.Conversations(ctx, model.ListRequest{
				Filter: model.Filter{Inboxes: inboxes}, Page: model.PageRequest{Limit: MaxPageLimit, After: after},
			})
			return err
		}); err != nil {
			return nil, false, err
		}
		scanned += max(len(got.Items), 1)
		// Authorization runs outside the store unit.
		for _, cv := range got.Items {
			if cv.Owner == nil || !slices.Contains(owners, *cv.Owner) {
				continue
			}
			a := engine.ConversationRead(cv)
			switch err := c.Authorize(ctx, a, a.Resource); {
			case err == nil:
				if len(out) == model.MaxMergePreviewConversations {
					return out, true, nil
				}
				out = append(out, cv.Ref)
			case !hidden(err):
				return nil, false, err
			}
		}
		switch {
		case got.Complete || len(got.Items) == 0 || got.Next == after:
			return out, false, nil
		case scanned >= maxPreviewScan:
			return out, true, nil
		}
		after = got.Next
	}
}
