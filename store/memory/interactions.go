package memory

import (
	"context"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/internal/interactions"
	"hop.top/uma/store/internal/tombstone"
)

// Interaction records live in one table keyed by reference. The record
// holds the response token beside the interaction; no read except
// InteractionToken copies it out.

func (r reader) Interaction(ctx context.Context, ref model.InteractionRef) (model.Interaction, error) {
	rec, err := lookup(ctx, r.h, &r.h.st.interactions, ref, ref, visible)
	return rec.Interaction, err
}

func (r reader) InteractionToken(ctx context.Context, ref model.InteractionRef) (store.ResponseToken, error) {
	rec, err := lookup(ctx, r.h, &r.h.st.interactions, ref, ref, same[store.InteractionRecord])
	return store.ResponseToken{Token: rec.ResponseToken, ExpiresAt: rec.TokenExpiresAt}, err
}

func (r reader) Interactions(ctx context.Context, req model.ListRequest) (model.Page[model.Interaction], error) {
	page, err := listTable(ctx, r.h, &r.h.st.interactions, req, filterSupport{inboxes: true, conversation: true, identity: true},
		func(f model.Filter, v store.InteractionRecord) bool {
			in := v.Interaction
			return inInboxes(f, in.Ref.Conversation.Inbox) && inConversation(f, in.Ref.Conversation) &&
				(f.Identity == nil || *f.Identity == in.Actor.Identity)
		}, visible)
	out := model.Page[model.Interaction]{Complete: page.Complete, Next: page.Next}
	for _, rec := range page.Items {
		out.Items = append(out.Items, rec.Interaction)
	}
	return out, err
}

// UnlinkedInteractions scans the interactions in recording order for those
// of inbox on provider message message that name no local message. The
// memory store keeps no secondary index.
func (r reader) UnlinkedInteractions(ctx context.Context, inbox model.InboxRef, message model.ExternalRef, pr model.PageRequest) (model.Page[model.Interaction], error) {
	if err := r.h.scoped(inbox, message); err != nil {
		return model.Page[model.Interaction]{}, err
	}
	page, err := listTable(ctx, r.h, &r.h.st.interactions, model.ListRequest{Page: pr}, filterSupport{},
		func(_ model.Filter, v store.InteractionRecord) bool {
			return v.Interaction.Ref.Conversation.Inbox == inbox && interactions.Unlinked(v.Interaction, message)
		}, visible)
	out := model.Page[model.Interaction]{Complete: page.Complete, Next: page.Next}
	for _, rec := range page.Items {
		out.Items = append(out.Items, rec.Interaction)
	}
	return out, err
}

// visible is the part of a record a read may return: a copy of the
// interaction and nothing else.
func visible(rec store.InteractionRecord) store.InteractionRecord {
	return store.InteractionRecord{Interaction: rec.Interaction.Clone()}
}

func (t tx) PutInteraction(ctx context.Context, rec store.InteractionRecord) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	ref := rec.Interaction.Ref
	if err := firstErr(interactions.CheckPut(h.tenant, rec), h.requireConversation(ref.Conversation)); err != nil {
		return err
	}
	if _, found := h.st.interactions.get(ref); found {
		return conflict("interaction already exists")
	}
	h.st.interactions.set(ref, entry[store.InteractionRecord]{seq: h.st.next(), v: rec.Clone()})
	return nil
}

func (t tx) RecordInteractionResponse(ctx context.Context, claim store.ResponseClaim) (model.Interaction, error) {
	h, err := t.write(ctx)
	if err != nil {
		return model.Interaction{}, err
	}
	defer h.mu.Unlock()
	if err := interactions.CheckClaim(h.tenant, claim); err != nil {
		return model.Interaction{}, err
	}
	cur, found := h.st.interactions.get(claim.Interaction)
	if !found {
		return model.Interaction{}, notFound()
	}
	next, err := interactions.Claim(cur.v.Interaction, claim)
	if err != nil {
		return model.Interaction{}, err
	}
	rec := cur.v
	rec.Interaction = next
	h.st.interactions.set(claim.Interaction, entry[store.InteractionRecord]{seq: cur.seq, at: cur.at, v: rec})
	return next.Clone(), nil
}

func (t tx) LinkInteraction(ctx context.Context, link store.InteractionLink) (model.Interaction, error) {
	h, err := t.write(ctx)
	if err != nil {
		return model.Interaction{}, err
	}
	defer h.mu.Unlock()
	if err := interactions.CheckLink(h.tenant, link); err != nil {
		return model.Interaction{}, err
	}
	cur, found := h.st.interactions.get(link.Interaction)
	if !found {
		return model.Interaction{}, notFound()
	}
	next, changed, err := interactions.Link(cur.v.Interaction, link)
	if err != nil || !changed {
		return next, err
	}
	rec := cur.v
	rec.Interaction = next
	h.st.interactions.set(link.Interaction, entry[store.InteractionRecord]{seq: cur.seq, at: cur.at, v: rec})
	return next.Clone(), nil
}

func (h *handle) redactInteraction(ref model.InteractionRef, expected, rev model.Revision) (store.RedactResult, error) {
	if err := h.scoped(ref); err != nil {
		return store.RedactResult{}, err
	}
	cur, ok := h.st.interactions.get(ref)
	if !ok {
		return store.RedactResult{}, notFound()
	}
	if err := cas(true, cur.v.Interaction.Revision, expected); err != nil {
		return store.RedactResult{}, err
	}
	h.st.interactions.set(ref, entry[store.InteractionRecord]{seq: cur.seq, at: cur.at, v: tombstone.Interaction(cur.v, rev)})
	// The raw copy the interaction was created from, when still retained.
	if raw, found := h.st.ingress.get(cur.v.Ingress); found {
		h.st.ingress.set(raw.v.ID, entry[store.IngressRecord]{seq: raw.seq, at: raw.at, v: tombstone.Ingress(raw.v)})
	}
	return store.RedactResult{}, nil
}

// purgeInteractionTokens clears up to limit response tokens that expired
// before before. The records stay as they are: same revision, same
// response state.
func (h *handle) purgeInteractionTokens(before time.Time, limit int) store.PurgeResult {
	r := store.PurgeResult{Complete: true}
	for _, e := range h.st.interactions.sorted(nil) {
		if e.v.ResponseToken == "" || !e.v.TokenExpiresAt.Before(before) {
			continue
		}
		if r.Purged == limit {
			r.Complete = false
			break
		}
		rec := e.v
		rec.ResponseToken = ""
		h.st.interactions.set(rec.Interaction.Ref, entry[store.InteractionRecord]{seq: e.seq, at: e.at, v: rec})
		r.Purged++
	}
	return r
}
