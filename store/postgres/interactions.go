package postgres

import (
	"context"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/internal/interactions"
	"hop.top/uma/store/internal/tombstone"
	"hop.top/uma/store/postgres/internal/db"
)

// An interaction row keeps the model.Interaction in its body and the
// response token in its own column. Only GetInteractionToken selects the
// column; the other reads here never see it.

// interactionKey is the primary key of ref's row in this tenant.
func (h *handle) interactionKey(ref model.InteractionRef) db.GetInteractionParams {
	c := ref.Conversation
	return db.GetInteractionParams{TenantID: h.tenantID, InboxID: string(c.Inbox.ID), ConversationID: string(c.ID), ID: string(ref.ID)}
}

func (r reader) Interaction(ctx context.Context, ref model.InteractionRef) (model.Interaction, error) {
	return getBody[model.Interaction](ctx, r.h, ref, func() ([]byte, error) {
		row, err := r.h.q.GetInteraction(ctx, r.h.interactionKey(ref))
		return row.Body, err
	})
}

func (r reader) InteractionToken(ctx context.Context, ref model.InteractionRef) (store.ResponseToken, error) {
	var out store.ResponseToken
	err := r.h.call(ctx, func() error {
		if err := r.h.scoped(ref); err != nil {
			return err
		}
		k := r.h.interactionKey(ref)
		row, err := r.h.q.GetInteractionToken(ctx, db.GetInteractionTokenParams(k))
		if err != nil {
			return r.h.lookupErr(ctx, err)
		}
		out = store.ResponseToken{Token: row.ResponseToken, ExpiresAt: time.Unix(row.TokenExpiresUnix, int64(row.TokenExpiresNanos)).UTC()}
		return nil
	})
	return out, err
}

func (r reader) Interactions(ctx context.Context, req model.ListRequest) (model.Page[model.Interaction], error) {
	f := req.Filter
	return listBodies[model.Interaction](ctx, r.h, req.Page,
		func() error {
			return r.h.checkFilter(f, filterSupport{inboxes: true, conversation: true, identity: true})
		},
		func(after int64, lim int32) ([]positioned, error) {
			inbox, conv := conversationArgs(f)
			p := db.ListInteractionsParams{
				TenantID: r.h.tenantID, After: after, Inboxes: inboxIDs(f), ConversationInbox: inbox, ConversationID: conv, Lim: lim,
			}
			if f.Identity != nil {
				p.ActorInbox, p.ActorID = optID(f.Identity.Inbox.ID), optID(f.Identity.ID)
			}
			rows, err := r.h.q.ListInteractions(ctx, p)
			return positions(rows, err, func(x db.ListInteractionsRow) positioned { return positioned{x.Pos, x.Body} })
		})
}

// UnlinkedInteractions reads the inbox's unlinked interactions on message
// through the partial index of unlinked rows.
func (r reader) UnlinkedInteractions(ctx context.Context, inbox model.InboxRef, message model.ExternalRef, page model.PageRequest) (model.Page[model.Interaction], error) {
	return listBodies[model.Interaction](ctx, r.h, page,
		func() error { return r.h.scoped(inbox, message) },
		func(after int64, lim int32) ([]positioned, error) {
			rows, err := r.h.q.ListUnlinkedInteractions(ctx, db.ListUnlinkedInteractionsParams{
				TenantID: r.h.tenantID, InboxID: string(inbox.ID), ExtID: message.ID,
				ChannelTenant: string(message.Channel.Tenant), ChannelID: string(message.Channel.ID),
				Namespace: message.Namespace, Kind: message.Kind, ParentID: message.ParentID, After: after, Lim: lim,
			})
			return positions(rows, err, func(x db.ListUnlinkedInteractionsRow) positioned { return positioned{x.Pos, x.Body} })
		})
}

func (t txn) PutInteraction(ctx context.Context, rec store.InteractionRecord) error {
	h := t.h
	return h.call(ctx, func() error {
		ref := rec.Interaction.Ref
		if err := interactions.CheckPut(h.tenant, rec); err != nil {
			return err
		}
		if err := h.requireConversation(ctx, ref.Conversation); err != nil {
			return err
		}
		k := h.interactionKey(ref)
		if _, err := h.q.GetInteraction(ctx, k); err == nil {
			return conflict("interaction already exists")
		} else if !missing(err) {
			return h.db(ctx, err)
		}
		c := rec.Interaction.Clone()
		body, err := encode(&c)
		if err != nil {
			return err
		}
		p := db.InsertInteractionParams{
			TenantID: k.TenantID, InboxID: k.InboxID, ConversationID: k.ConversationID, ID: k.ID,
			IngressID: string(rec.Ingress), Body: body, Revision: string(c.Revision),
			ResponseToken: rec.ResponseToken, TokenExpiresUnix: rec.TokenExpiresAt.Unix(),
			TokenExpiresNanos: int32(rec.TokenExpiresAt.Nanosecond()), // #nosec G115 -- below 1e9
		}
		if id := c.Actor.Identity; !id.IsZero() {
			p.ActorInboxID, p.ActorID = optID(id.Inbox.ID), optID(id.ID)
		}
		if m := c.MessageExternal; m != nil {
			p.MessageChannelTenant, p.MessageChannelID = new(string(m.Channel.Tenant)), new(string(m.Channel.ID))
			p.MessageNamespace, p.MessageKind = new(m.Namespace), new(m.Kind)
			p.MessageExtID, p.MessageParentID = new(m.ID), new(m.ParentID)
		}
		p.MessageLinked = c.Message != nil
		return h.db(ctx, h.q.InsertInteraction(ctx, p))
	})
}

func (t txn) RecordInteractionResponse(ctx context.Context, claim store.ResponseClaim) (model.Interaction, error) {
	h := t.h
	var out model.Interaction
	err := h.call(ctx, func() error {
		if err := interactions.CheckClaim(h.tenant, claim); err != nil {
			return err
		}
		k := h.interactionKey(claim.Interaction)
		row, err := h.q.GetInteraction(ctx, k)
		if err != nil {
			return h.lookupErr(ctx, err)
		}
		cur, err := decode[model.Interaction](row.Body)
		if err != nil {
			return err
		}
		next, err := interactions.Claim(cur, claim)
		if err != nil {
			return err
		}
		body, err := encode(&next)
		if err != nil {
			return err
		}
		n, err := h.q.UpdateInteraction(ctx, db.UpdateInteractionParams{
			TenantID: k.TenantID, InboxID: k.InboxID, ConversationID: k.ConversationID, ID: k.ID,
			Body: body, Revision: string(next.Revision), Expected: row.Revision,
		})
		if err != nil {
			return h.db(ctx, err)
		}
		if n != 1 {
			return conflict("interaction changed concurrently")
		}
		out = next
		return nil
	})
	return out, err
}

func (t txn) LinkInteraction(ctx context.Context, link store.InteractionLink) (model.Interaction, error) {
	h := t.h
	var out model.Interaction
	err := h.call(ctx, func() error {
		if err := interactions.CheckLink(h.tenant, link); err != nil {
			return err
		}
		k := h.interactionKey(link.Interaction)
		row, err := h.q.GetInteraction(ctx, k)
		if err != nil {
			return h.lookupErr(ctx, err)
		}
		cur, err := decode[model.Interaction](row.Body)
		if err != nil {
			return err
		}
		next, changed, err := interactions.Link(cur, link)
		if err != nil || !changed {
			out = next
			return err
		}
		body, err := encode(&next)
		if err != nil {
			return err
		}
		n, err := h.q.LinkInteraction(ctx, db.LinkInteractionParams{
			TenantID: k.TenantID, InboxID: k.InboxID, ConversationID: k.ConversationID, ID: k.ID,
			Body: body, Revision: string(next.Revision), Expected: row.Revision,
		})
		if err != nil {
			return h.db(ctx, err)
		}
		if n != 1 {
			return conflict("interaction changed concurrently")
		}
		out = next
		return nil
	})
	return out, err
}

func (h *handle) redactInteraction(ctx context.Context, ref model.InteractionRef, expected, rev model.Revision) error {
	if err := h.scoped(ref); err != nil {
		return err
	}
	k := h.interactionKey(ref)
	row, err := h.q.GetInteraction(ctx, k)
	if err != nil {
		return h.lookupErr(ctx, err)
	}
	if model.Revision(row.Revision) != expected {
		return conflict("stale expected revision")
	}
	cur, err := decode[model.Interaction](row.Body)
	if err != nil {
		return err
	}
	// Only the interaction part of the record is rewritten here; the token
	// column is cleared by the same statement.
	next := tombstone.Interaction(store.InteractionRecord{Interaction: cur}, rev).Interaction
	body, err := encode(&next)
	if err != nil {
		return err
	}
	n, err := h.q.RedactInteraction(ctx, db.RedactInteractionParams{
		TenantID: k.TenantID, InboxID: k.InboxID, ConversationID: k.ConversationID, ID: k.ID,
		Body: body, Revision: string(rev), Expected: row.Revision,
	})
	if err != nil {
		return h.db(ctx, err)
	}
	if n != 1 {
		return conflict("stale expected revision")
	}
	return h.tombstoneInteractionIngress(ctx, model.ID(row.IngressID))
}

// tombstoneInteractionIngress rewrites the ingress record an interaction
// was created from, when retention has not deleted it.
func (h *handle) tombstoneInteractionIngress(ctx context.Context, id model.ID) error {
	b, err := h.q.GetIngress(ctx, db.GetIngressParams{TenantID: h.tenantID, ID: string(id)})
	switch {
	case missing(err):
		return nil
	case err != nil:
		return h.db(ctx, err)
	}
	rec, err := decode[store.IngressRecord](b)
	if err != nil {
		return err
	}
	next := tombstone.Ingress(rec)
	body, err := encode(&next)
	if err != nil {
		return err
	}
	return h.db(ctx, h.q.UpdateIngress(ctx, db.UpdateIngressParams{TenantID: h.tenantID, ID: string(rec.ID), Body: body}))
}

// purgeInteractionTokens clears up to lim response tokens that expired
// before before; the rows keep their revision, body and response state.
func (h *handle) purgeInteractionTokens(ctx context.Context, before time.Time, lim int32) (store.PurgeResult, error) {
	unix, nanos := before.Unix(), int32(before.Nanosecond()) // #nosec G115 -- below 1e9
	n, err := h.q.ClearExpiredInteractionTokens(ctx, db.ClearExpiredInteractionTokensParams{
		TenantID: h.tenantID, BeforeUnix: unix, BeforeNanos: nanos, Lim: lim,
	})
	if err != nil {
		return store.PurgeResult{}, h.db(ctx, err)
	}
	remain, err := h.q.ExpiredInteractionTokensRemain(ctx, db.ExpiredInteractionTokensRemainParams{
		TenantID: h.tenantID, BeforeUnix: unix, BeforeNanos: nanos,
	})
	if err != nil {
		return store.PurgeResult{}, h.db(ctx, err)
	}
	return store.PurgeResult{Purged: int(n), Complete: !remain}, nil
}
