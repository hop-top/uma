package memory

import (
	"context"
	"slices"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

func (t tx) ClaimIdentity(ctx context.Context, key model.IdentityKey, v model.Identity) (model.Identity, bool, error) {
	h, err := t.write(ctx)
	if err != nil {
		return model.Identity{}, false, err
	}
	defer h.mu.Unlock()
	if err := firstErr(h.scoped(key, v.Ref, v.Contact), newRevision(v.Revision, "")); err != nil {
		return model.Identity{}, false, err
	}
	if v.Ref.Inbox != key.Inbox || v.Namespace != key.Namespace || v.SourceID != key.SourceID {
		return model.Identity{}, false, invalid("identity: must agree with its key")
	}
	if err := h.requireInbox(key.Inbox); err != nil {
		return model.Identity{}, false, err
	}
	if ref, ok := h.st.identityKeys.get(key); ok {
		cur, _ := h.st.identities.get(ref.v)
		return cur.v.Clone(), false, nil
	}
	if _, ok := h.st.identities.get(v.Ref); ok {
		return model.Identity{}, false, conflict("identity reference already claimed under another key")
	}
	h.st.identities.set(v.Ref, entry[model.Identity]{seq: h.st.next(), v: v.Clone()})
	h.st.identityKeys.set(key, entry[model.IdentityRef]{v: v.Ref})
	return v.Clone(), true, nil
}

func (t tx) LinkIdentity(ctx context.Context, rec store.LinkRecord) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := h.checkLink(rec); err != nil {
		return err
	}
	cur, ok := h.st.identities.get(rec.Identity)
	if !ok {
		return notFound()
	}
	id := cur.v
	if id.Revision != rec.RevisionBefore {
		return conflict("stale identity revision")
	}
	key := model.IdentityKey{Inbox: id.Ref.Inbox, Namespace: id.Namespace, SourceID: id.SourceID}
	head, _ := h.st.linkHeads.get(rec.Identity)
	switch {
	case rec.IdentityKey != key:
		return invalid("identity_key: must be the identity's key")
	case id.Contact != rec.From:
		return invalid("from: must be the identity's current contact")
	case head.v != rec.Supersedes:
		return invalid("supersedes: must be the identity's latest link record")
	}
	if _, dup := h.st.linkIDs.get(rec.ID); dup {
		return conflict("link record already exists")
	}
	ev := evidenceKey{rec.Evidence.Verifier, rec.Evidence.Reference}
	if ev.reference != "" {
		if _, used := h.st.evidence.get(ev); used {
			return conflict("evidence reference already consumed")
		}
		h.st.evidence.set(ev, entry[model.ID]{v: rec.ID})
	}
	id = id.Clone()
	id.Contact, id.LinkAssurance, id.Revision = rec.To, rec.Assurance, rec.RevisionAfter
	h.st.identities.set(rec.Identity, entry[model.Identity]{seq: cur.seq, v: id})
	h.st.linkIDs.set(rec.ID, entry[struct{}]{})
	h.st.linkHeads.set(rec.Identity, entry[model.ID]{v: rec.ID})
	h.st.links.append(rec.Clone())
	h.st.linkAt.append(h.now())
	return nil
}

// checkLink validates a link record's shape and scope.
func (h *handle) checkLink(rec store.LinkRecord) error {
	if err := firstErr(
		rec.ID.Validate(),
		h.sameTenant(rec.Tenant),
		h.scoped(rec.Identity, rec.IdentityKey, rec.From, rec.To, rec.Operation),
		rec.Class.Validate(),
		rec.Assurance.Validate(),
		required("revision_before", string(rec.RevisionBefore)),
		newRevision(rec.RevisionAfter, rec.RevisionBefore),
	); err != nil {
		return err
	}
	if rec.Supersedes != "" {
		if err := rec.Supersedes.Validate(); err != nil {
			return err
		}
	}
	if rec.Anchor != nil {
		if err := h.scoped(rec.Anchor); err != nil {
			return err
		}
	}
	if rec.From == rec.To {
		return invalid("to: must differ from from; a no-op link has no record")
	}
	return nil
}

func (t tx) ClaimConversation(ctx context.Context, c store.ConversationClaim) (model.Conversation, bool, error) {
	h, err := t.write(ctx)
	if err != nil {
		return model.Conversation{}, false, err
	}
	defer h.mu.Unlock()
	sel, v := c.Selection, c.Conversation
	if err := firstErr(
		h.scoped(sel.Inbox),
		required("selection.key", sel.Key),
		required("create_key", c.CreateKey),
		h.checkConversation(v),
		newRevision(v.Revision, ""),
	); err != nil {
		return model.Conversation{}, false, err
	}
	switch {
	case !model.IsSelectionMode(sel.Mode):
		return model.Conversation{}, false, invalid("selection.mode: must be a declared selection mode")
	case v.Ref.Inbox != sel.Inbox || v.Mode != sel.Mode:
		return model.Conversation{}, false, invalid("conversation: must agree with its selection key")
	}
	ck := claimKey{sel: sel, create: c.CreateKey}
	if ref, ok := h.st.convClaims.get(ck); ok {
		cur, _ := h.st.conversations.get(ref.v)
		return cur.v.Clone(), false, nil
	}
	if _, ok := h.st.conversations.get(v.Ref); ok {
		return model.Conversation{}, false, conflict("conversation already exists")
	}
	h.st.conversations.set(v.Ref, entry[model.Conversation]{seq: h.st.next(), v: v.Clone()})
	h.st.convClaims.set(ck, entry[model.ConversationRef]{v: v.Ref})
	g, _ := h.st.convGroups.get(sel)
	h.st.convGroups.set(sel, entry[[]model.ConversationRef]{v: append(slices.Clip(g.v), v.Ref)})
	return v.Clone(), true, nil
}

func (t tx) ClaimMessage(ctx context.Context, c store.MessageClaim) (model.Message, bool, error) {
	h, err := t.write(ctx)
	if err != nil {
		return model.Message{}, false, err
	}
	defer h.mu.Unlock()
	v := c.Message
	if err := firstErr(
		h.scoped(v.Ref), h.requireConversation(v.Ref.Conversation),
		c.Reference.Validate(), h.scoped(c.Reference.Channel),
	); err != nil {
		return model.Message{}, false, err
	}
	if v.Task != nil {
		if err := h.scoped(v.Task); err != nil {
			return model.Message{}, false, err
		}
	}
	if !slices.Contains(v.External, c.Reference) {
		return model.Message{}, false, invalid("reference: must be one of the message's external references")
	}
	k := messageClaimKey{inbox: v.Ref.Conversation.Inbox, ref: c.Reference}
	if ref, ok := h.st.msgClaims.get(k); ok {
		if cur, ok := h.st.messages.get(ref.v); ok {
			return cur.v.Clone(), false, nil
		}
	}
	if err := put(h, &h.st.messages, v.Ref, v.Clone(), messageRev, ""); err != nil {
		return model.Message{}, false, err
	}
	h.st.msgClaims.set(k, entry[model.MessageRef]{v: v.Ref})
	return v.Clone(), true, nil
}

func (t tx) ClaimIngress(ctx context.Context, rec store.IngressRecord) (store.IngressRecord, bool, error) {
	h, err := t.write(ctx)
	if err != nil {
		return store.IngressRecord{}, false, err
	}
	defer h.mu.Unlock()
	if err := firstErr(
		rec.ID.Validate(), h.scoped(rec.Channel), required("dedup_key", rec.DedupKey), h.requireChannel(rec.Channel),
	); err != nil {
		return store.IngressRecord{}, false, err
	}
	dk := dedupKey{rec.Channel, rec.DedupKey}
	if id, ok := h.st.ingressDedup.get(dk); ok {
		cur, _ := h.st.ingress.get(id.v)
		return cur.v.Clone(), false, nil
	}
	if _, ok := h.st.ingress.get(rec.ID); ok {
		return store.IngressRecord{}, false, conflict("ingress ID already claimed under another dedup key")
	}
	h.st.ingress.set(rec.ID, entry[store.IngressRecord]{seq: h.st.next(), at: h.now(), v: rec.Clone()})
	h.st.ingressDedup.set(dk, entry[model.ID]{v: rec.ID})
	return rec.Clone(), true, nil
}

func (t tx) CompleteIngress(ctx context.Context, id model.ID, receipt model.IngressReceipt) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := id.Validate(); err != nil {
		return err
	}
	if receipt.ID != id {
		return invalid("receipt.id: must be the ingress ID")
	}
	if err := firstErr(receipt.Validate(), h.receiptScoped(receipt)); err != nil {
		return err
	}
	cur, ok := h.st.ingress.get(id)
	if !ok {
		return notFound()
	}
	switch {
	case cur.v.Receipt.Equal(receipt):
		return nil
	case !cur.v.Receipt.Equal(model.IngressReceipt{}):
		return conflict("ingress already completed with another receipt")
	}
	rec := cur.v.Clone()
	rec.Receipt = receipt.Clone()
	h.st.ingress.set(id, entry[store.IngressRecord]{seq: cur.seq, at: cur.at, v: rec})
	return nil
}

// receiptScoped requires a receipt's references in the unit's tenant; a
// valid receipt's conversation references all share its tenant.
func (h *handle) receiptScoped(r model.IngressReceipt) error {
	if r.Operation != nil {
		if err := h.scoped(r.Operation); err != nil {
			return err
		}
	}
	if r.Conversation == nil {
		return nil
	}
	return h.scoped(r.Conversation)
}
