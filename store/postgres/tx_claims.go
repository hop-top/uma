package postgres

import (
	"context"
	"slices"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres/internal/db"
)

func (t txn) ClaimIdentity(ctx context.Context, key model.IdentityKey, v model.Identity) (model.Identity, bool, error) {
	h := t.h
	var out model.Identity
	var created bool
	err := h.call(ctx, func() error {
		if err := firstErr(h.scoped(key, v.Ref, v.Contact), newRevision(v.Revision, "")); err != nil {
			return err
		}
		if v.Ref.Inbox != key.Inbox || v.Namespace != key.Namespace || v.SourceID != key.SourceID {
			return invalid("identity: must agree with its key")
		}
		if err := h.requireInbox(ctx, key.Inbox); err != nil {
			return err
		}
		c := v.Clone()
		body, encErr := encode(&c)
		if encErr == nil {
			// The identity is inserted before its key is read: a read of the
			// key or ID index would leave a predicate lock on the page new
			// claims share, and concurrent claims would fail one another on
			// it. Only a claim that finds its key or ID held reads it.
			n, err := h.q.InsertIdentity(ctx, db.InsertIdentityParams{
				TenantID: h.tenantID, InboxID: string(key.Inbox.ID), ID: string(v.Ref.ID),
				Namespace: key.Namespace, SourceID: key.SourceID, ContactID: string(v.Contact.ID),
				Body: body, Revision: string(v.Revision),
			})
			if err != nil {
				return h.db(ctx, err)
			}
			if n == 1 {
				out, created = v.Clone(), true
				return nil
			}
		}
		// A held key answers with its identity, a held ID conflicts; both
		// answer before the identity's encoding.
		row, err := h.q.GetIdentityByKey(ctx, db.GetIdentityByKeyParams{
			TenantID: h.tenantID, InboxID: string(key.Inbox.ID), Namespace: key.Namespace, SourceID: key.SourceID,
		})
		switch {
		case err == nil:
			out, err = decode[model.Identity](row.Body)
			return err
		case !missing(err):
			return h.db(ctx, err)
		case encErr == nil:
			// The insert met a held ID.
			return conflict("identity reference already claimed under another key")
		}
		taken, err := h.q.IdentityIDExists(ctx, db.IdentityIDExistsParams{TenantID: h.tenantID, ID: string(v.Ref.ID)})
		switch {
		case err != nil:
			return h.db(ctx, err)
		case taken:
			return conflict("identity reference already claimed under another key")
		}
		return encErr
	})
	return out, created, err
}

func (t txn) LinkIdentity(ctx context.Context, rec store.LinkRecord) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := h.checkLink(rec); err != nil {
			return err
		}
		ref := rec.Identity
		row, err := h.q.GetIdentity(ctx, db.GetIdentityParams{
			TenantID: h.tenantID, InboxID: string(ref.Inbox.ID), ID: string(ref.ID),
		})
		if err != nil {
			return h.lookupErr(ctx, err)
		}
		id, err := decode[model.Identity](row.Body)
		if err != nil {
			return err
		}
		if id.Revision != rec.RevisionBefore {
			return conflict("stale identity revision")
		}
		var head model.ID
		if row.LinkHead != nil {
			head = model.ID(*row.LinkHead)
		}
		key := model.IdentityKey{Inbox: id.Ref.Inbox, Namespace: id.Namespace, SourceID: id.SourceID}
		switch {
		case rec.IdentityKey != key:
			return invalid("identity_key: must be the identity's key")
		case id.Contact != rec.From:
			return invalid("from: must be the identity's current contact")
		case head != rec.Supersedes:
			return invalid("supersedes: must be the identity's latest link record")
		}
		dup, err := h.q.LinkIDExists(ctx, db.LinkIDExistsParams{TenantID: h.tenantID, ID: string(rec.ID)})
		if err != nil {
			return h.db(ctx, err)
		}
		if dup {
			return conflict("link record already exists")
		}
		ev := rec.Evidence
		if ev.Reference != "" {
			used, err := h.q.EvidenceUsed(ctx, db.EvidenceUsedParams{
				TenantID: h.tenantID, Verifier: ev.Verifier, Reference: ev.Reference,
			})
			if err != nil {
				return h.db(ctx, err)
			}
			if used {
				return conflict("evidence reference already consumed")
			}
		}
		id.Contact, id.LinkAssurance, id.Revision = rec.To, rec.Assurance, rec.RevisionAfter
		idBody, err := encode(&id)
		if err != nil {
			return err
		}
		c := rec.Clone()
		linkBody, err := encode(&c)
		if err != nil {
			return err
		}
		n, err := h.q.LinkIdentity(ctx, db.LinkIdentityParams{
			TenantID: h.tenantID, InboxID: string(ref.Inbox.ID), ID: string(ref.ID),
			ContactID: string(rec.To.ID), LinkHead: optID(rec.ID), Body: idBody,
			Revision: string(rec.RevisionAfter), Expected: string(rec.RevisionBefore),
		})
		if err != nil {
			return h.db(ctx, err)
		}
		if n != 1 {
			return conflict("stale identity revision")
		}
		return h.db(ctx, h.q.InsertLink(ctx, db.InsertLinkParams{
			TenantID: h.tenantID, ID: string(rec.ID), InboxID: string(ref.Inbox.ID), IdentityID: string(ref.ID),
			FromContact: string(rec.From.ID), ToContact: string(rec.To.ID), Supersedes: optID(rec.Supersedes),
			EvidenceVerifier: ev.Verifier, EvidenceReference: ev.Reference, Body: linkBody, WrittenAt: h.now(),
		}))
	})
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

func (t txn) ClaimConversation(ctx context.Context, c store.ConversationClaim) (model.Conversation, bool, error) {
	h := t.h
	var out model.Conversation
	var created bool
	err := h.call(ctx, func() error {
		sel, v := c.Selection, c.Conversation
		if err := firstErr(
			h.scoped(sel.Inbox),
			required("selection.key", sel.Key),
			required("create_key", c.CreateKey),
		); err != nil {
			return err
		}
		if err := h.checkConversation(ctx, v); err != nil {
			return err
		}
		if err := newRevision(v.Revision, ""); err != nil {
			return err
		}
		switch {
		case !model.IsSelectionMode(sel.Mode):
			return invalid("selection.mode: must be a declared selection mode")
		case v.Ref.Inbox != sel.Inbox || v.Mode != sel.Mode:
			return invalid("conversation: must agree with its selection key")
		}
		inbox := string(sel.Inbox.ID)
		cv := v.Clone()
		body, encErr := encode(&cv)
		if encErr == nil {
			// The conversation and its claim are inserted before the claim
			// is read, as ClaimIdentity. Only a claim that finds its
			// conversation ID or create key held reads it.
			done, err := h.insertClaimedConversation(ctx, c, cv, body)
			switch {
			case err != nil:
				return err
			case done:
				out, created = v.Clone(), true
				return nil
			}
		}
		// A held create key answers with its conversation, a held ID
		// conflicts; both answer before the conversation's encoding.
		b, err := h.q.GetConversationClaim(ctx, db.GetConversationClaimParams{
			TenantID: h.tenantID, InboxID: inbox, Mode: sel.Mode, SelectionKey: sel.Key, CreateKey: c.CreateKey,
		})
		switch {
		case err == nil:
			out, err = decode[model.Conversation](b)
			return err
		case !missing(err):
			return h.db(ctx, err)
		case encErr == nil:
			// The insert met a held ID.
			return conflict("conversation already exists")
		}
		taken, err := h.q.ConversationIDExists(ctx, db.ConversationIDExistsParams{TenantID: h.tenantID, ID: string(v.Ref.ID)})
		switch {
		case err != nil:
			return h.db(ctx, err)
		case taken:
			return conflict("conversation already exists")
		}
		return encErr
	})
	return out, created, err
}

// insertClaimedConversation inserts a claim's conversation, its claim and
// its labels, and reports whether it did. Nothing is left inserted when
// the conversation ID or the create key is held.
func (h *handle) insertClaimedConversation(ctx context.Context, c store.ConversationClaim, cv model.Conversation, body []byte) (bool, error) {
	sel, inbox, id := c.Selection, string(c.Selection.Inbox.ID), string(cv.Ref.ID)
	unix, nanos := createdOrder(cv)
	wf := workflowColumnsOf(cv)
	n, err := h.q.InsertConversation(ctx, db.InsertConversationParams{
		TenantID: h.tenantID, InboxID: inbox, ID: id,
		Mode: cv.Mode, Status: cv.Status, CreatedUnix: unix, CreatedNanos: nanos, Body: body, Revision: string(cv.Revision),
		Assignee: wf.assignee, SnoozeUntilUnix: wf.untilUnix, SnoozeUntilNanos: wf.untilNanos,
		SnoozeAtUnix: wf.atUnix, SnoozeAtNanos: wf.atNanos, LinkCount: wf.links,
	})
	if err != nil || n == 0 {
		return false, h.db(ctx, err)
	}
	n, err = h.q.InsertConversationClaim(ctx, db.InsertConversationClaimParams{
		TenantID: h.tenantID, InboxID: inbox, Mode: sel.Mode, SelectionKey: sel.Key,
		CreateKey: c.CreateKey, ConversationID: id,
	})
	switch {
	case err != nil:
		return false, h.db(ctx, err)
	case n == 0:
		// The create key is held: the conversation goes again.
		return false, h.db(ctx, h.q.DeleteNewConversation(ctx, db.DeleteNewConversationParams{TenantID: h.tenantID, ID: id}))
	}
	// A new conversation: no label row names it yet.
	return true, h.insertConversationLabels(ctx, id, cv.Labels)
}

func (t txn) ClaimMessage(ctx context.Context, c store.MessageClaim) (model.Message, bool, error) {
	h := t.h
	var out model.Message
	var created bool
	err := h.call(ctx, func() error {
		v, r := c.Message, c.Reference
		if err := firstErr(h.checkMessage(ctx, v), r.Validate(), h.scoped(r.Channel)); err != nil {
			return err
		}
		if !slices.Contains(v.External, r) {
			return invalid("reference: must be one of the message's external references")
		}
		if r.Channel.ID.Validate() != nil {
			return invalid("reference: channel ID must be an internal ID")
		}
		inbox := string(v.Ref.Conversation.Inbox.ID)
		row, prepErr := h.messageRow(v)
		if prepErr == nil {
			prepErr = newRevision(v.Revision, "")
		}
		inserted := false
		if prepErr == nil {
			// The message and its claim are inserted before the claim is
			// read, as ClaimIdentity. Only a claim that finds its message ID
			// or reference held reads it.
			var err error
			if inserted, err = h.insertNewMessage(ctx, row); err != nil {
				return err
			}
		}
		if inserted {
			n, err := h.q.InsertMessageClaim(ctx, db.InsertMessageClaimParams{
				TenantID: h.tenantID, InboxID: inbox, ChannelTenant: string(r.Channel.Tenant), ChannelID: string(r.Channel.ID),
				Namespace: r.Namespace, Kind: r.Kind, ExtID: r.ID, ParentID: r.ParentID, MessageID: string(v.Ref.ID),
			})
			switch {
			case err != nil:
				return h.db(ctx, err)
			case n == 1:
				out, created = v.Clone(), true
				return nil
			}
			// The reference is held: the message goes again, with its
			// reference and attachment rows.
			if err := h.db(ctx, h.q.DeleteNewMessage(ctx, db.DeleteNewMessageParams{TenantID: h.tenantID, ID: string(v.Ref.ID)})); err != nil {
				return err
			}
		}
		// A held reference answers with its message before the message's
		// own checks and its held ID.
		body, err := h.q.GetMessageClaim(ctx, db.GetMessageClaimParams{
			TenantID: h.tenantID, InboxID: inbox, ChannelTenant: string(r.Channel.Tenant), ChannelID: string(r.Channel.ID),
			Namespace: r.Namespace, Kind: r.Kind, ExtID: r.ID, ParentID: r.ParentID,
		})
		switch {
		case err == nil:
			out, err = decode[model.Message](body)
			return err
		case !missing(err):
			return h.db(ctx, err)
		case prepErr != nil:
			return prepErr
		case inserted:
			// The insert met a held reference no read finds.
			return conflict("message reference already claimed")
		}
		return h.heldMessage(ctx, row.p)
	})
	return out, created, err
}

func (t txn) ClaimIngress(ctx context.Context, rec store.IngressRecord) (store.IngressRecord, bool, error) {
	h := t.h
	var out store.IngressRecord
	var created bool
	err := h.call(ctx, func() error {
		if err := firstErr(rec.ID.Validate(), h.scoped(rec.Channel), required("dedup_key", rec.DedupKey)); err != nil {
			return err
		}
		if err := h.requireChannel(ctx, rec.Channel); err != nil {
			return err
		}
		c := rec.Clone()
		body, encErr := encode(&c)
		if encErr == nil {
			// The record is inserted before its dedup key is read, as
			// ClaimIdentity. Only a claim that finds its dedup key or ID held
			// reads it.
			n, err := h.q.InsertIngress(ctx, db.InsertIngressParams{
				TenantID: h.tenantID, ID: string(rec.ID), ChannelID: string(rec.Channel.ID), DedupKey: rec.DedupKey,
				MessageID: rec.Input.Envelope.MessageID, Body: body, WrittenAt: h.now(),
			})
			if err != nil {
				return h.db(ctx, err)
			}
			if n == 1 {
				out, created = rec.Clone(), true
				return nil
			}
		}
		// A held dedup key answers with its record, a held ID conflicts;
		// both answer before the record's encoding.
		b, err := h.q.GetIngressByDedup(ctx, db.GetIngressByDedupParams{
			TenantID: h.tenantID, ChannelID: string(rec.Channel.ID), DedupKey: rec.DedupKey,
		})
		switch {
		case err == nil:
			out, err = decode[store.IngressRecord](b)
			return err
		case !missing(err):
			return h.db(ctx, err)
		case encErr == nil:
			// The insert met a held ID.
			return conflict("ingress ID already claimed under another dedup key")
		}
		_, err = h.q.GetIngress(ctx, db.GetIngressParams{TenantID: h.tenantID, ID: string(rec.ID)})
		switch {
		case err == nil:
			return conflict("ingress ID already claimed under another dedup key")
		case !missing(err):
			return h.db(ctx, err)
		}
		return encErr
	})
	return out, created, err
}

func (t txn) CompleteIngress(ctx context.Context, id model.ID, receipt model.IngressReceipt) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := id.Validate(); err != nil {
			return err
		}
		if receipt.ID != id {
			return invalid("receipt.id: must be the ingress ID")
		}
		if err := receipt.Validate(); err != nil {
			return err
		}
		if receipt.Operation != nil {
			if err := h.scoped(receipt.Operation); err != nil {
				return err
			}
		}
		if receipt.Conversation != nil {
			// A valid receipt's references share its conversation's tenant.
			if err := h.scoped(receipt.Conversation); err != nil {
				return err
			}
		}
		b, err := h.q.GetIngress(ctx, db.GetIngressParams{TenantID: h.tenantID, ID: string(id)})
		if err != nil {
			return h.lookupErr(ctx, err)
		}
		rec, err := decode[store.IngressRecord](b)
		if err != nil {
			return err
		}
		switch {
		case rec.Receipt.Equal(receipt):
			return nil
		case !rec.Receipt.Equal(model.IngressReceipt{}):
			return conflict("ingress already completed with another receipt")
		}
		rec.Receipt = receipt
		body, err := encode(&rec)
		if err != nil {
			return err
		}
		return h.db(ctx, h.q.UpdateIngress(ctx, db.UpdateIngressParams{TenantID: h.tenantID, ID: string(id), Body: body}))
	})
}
