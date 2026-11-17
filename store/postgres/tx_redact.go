package postgres

import (
	"context"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/internal/tombstone"
	"hop.top/uma/store/postgres/internal/db"
)

// Redact tombstones a message, interaction or attachment and the records holding copies
// of its content; see store.Tx.Redact. Every check runs before the first
// write.
func (t txn) Redact(ctx context.Context, req store.Redaction) (store.RedactResult, error) {
	h := t.h
	var res store.RedactResult
	err := h.call(ctx, func() error {
		if err := tombstone.Check(req); err != nil {
			return err
		}
		var err error
		switch {
		case req.Interaction != nil:
			err = h.redactInteraction(ctx, *req.Interaction, req.Expected, req.Revision)
		case req.Attachment != nil:
			res, err = h.redactAttachment(ctx, *req.Attachment, req.Expected, req.Revision)
		default:
			res, err = h.redactMessage(ctx, *req.Message, req.Expected, req.Revision)
		}
		return err
	})
	return res, err
}

// storedAttachment is an attachment record read for redaction with its
// stored revision.
type storedAttachment struct {
	rec store.AttachmentRecord
	rev string
}

func (h *handle) getAttachment(ctx context.Context, ref model.AttachmentRef) (storedAttachment, bool, error) {
	row, err := h.q.GetAttachment(ctx, db.GetAttachmentParams{TenantID: h.tenantID, InboxID: string(ref.Inbox.ID), ID: string(ref.ID)})
	switch {
	case missing(err):
		return storedAttachment{}, false, nil
	case err != nil:
		return storedAttachment{}, false, h.db(ctx, err)
	}
	rec, err := decode[store.AttachmentRecord](row.Body)
	return storedAttachment{rec: rec, rev: row.Revision}, true, err
}

func (h *handle) redactAttachment(ctx context.Context, ref model.AttachmentRef, expected, rev model.Revision) (store.RedactResult, error) {
	if err := h.scoped(ref); err != nil {
		return store.RedactResult{}, err
	}
	cur, found, err := h.getAttachment(ctx, ref)
	if err != nil {
		return store.RedactResult{}, err
	}
	if !found {
		return store.RedactResult{}, notFound()
	}
	if model.Revision(cur.rev) != expected {
		return store.RedactResult{}, conflict("stale expected revision")
	}
	var res store.RedactResult
	return res, h.tombstoneAttachment(ctx, cur, rev, &res)
}

func (h *handle) redactMessage(ctx context.Context, ref model.MessageRef, expected, rev model.Revision) (store.RedactResult, error) {
	if err := h.scoped(ref); err != nil {
		return store.RedactResult{}, err
	}
	conv := ref.Conversation
	key := db.GetMessageParams{TenantID: h.tenantID, InboxID: string(conv.Inbox.ID), ConversationID: string(conv.ID), ID: string(ref.ID)}
	row, err := h.q.GetMessage(ctx, key)
	if err != nil {
		return store.RedactResult{}, h.lookupErr(ctx, err)
	}
	if model.Revision(row.Revision) != expected {
		return store.RedactResult{}, conflict("stale expected revision")
	}
	m, err := decode[model.Message](row.Body)
	if err != nil {
		return store.RedactResult{}, err
	}
	var op *store.OperationRecord
	if m.Origin != nil && m.Origin.Operation.Tenant == h.tenant {
		if op, err = h.idleOperation(ctx, m.Origin.Operation); err != nil {
			return store.RedactResult{}, err
		}
	}
	var atts []storedAttachment
	for _, a := range tombstone.Attachments(m) {
		if a.Scope() != h.tenant {
			continue
		}
		cur, found, err := h.getAttachment(ctx, a)
		if err != nil {
			return store.RedactResult{}, err
		}
		if found && cur.rec.State != store.AttachmentRedacted {
			atts = append(atts, cur)
		}
	}

	next := tombstone.Message(m, rev)
	body, err := encode(&next)
	if err != nil {
		return store.RedactResult{}, err
	}
	task, parent, err := messageLinks(next)
	if err != nil {
		return store.RedactResult{}, err
	}
	// The tombstone keeps OccurredAt and loses every search token.
	sc := searchColumnsOf(next)
	n, err := h.q.UpdateMessage(ctx, db.UpdateMessageParams{
		TenantID: h.tenantID, InboxID: key.InboxID, ConversationID: key.ConversationID, ID: key.ID,
		TaskID: task, InReplyToID: parent, Body: body, Revision: string(rev), Expected: string(expected),
		SearchTokens: sc.tokens, OccurredUnix: sc.unix, OccurredNanos: sc.nanos,
		RecordedUnix: sc.recUnix, RecordedNanos: sc.recNanos,
	})
	if err != nil {
		return store.RedactResult{}, h.db(ctx, err)
	}
	if n != 1 {
		return store.RedactResult{}, conflict("stale expected revision")
	}
	if err := h.putMessageAttachments(ctx, key.ID, nil); err != nil {
		return store.RedactResult{}, err
	}
	var res store.RedactResult
	for _, a := range atts {
		if err := h.tombstoneAttachment(ctx, a, rev, &res); err != nil {
			return store.RedactResult{}, err
		}
	}
	for _, pm := range tombstone.ProviderMessageIDs(m) {
		if pm.Channel.Tenant != h.tenant {
			continue
		}
		if err := h.tombstoneIngress(ctx, pm); err != nil {
			return store.RedactResult{}, err
		}
	}
	if op != nil {
		next := tombstone.Operation(*op)
		body, err := encode(&next)
		if err != nil {
			return store.RedactResult{}, err
		}
		if err := h.q.RedactOperation(ctx, db.RedactOperationParams{TenantID: h.tenantID, ID: string(op.Operation.Ref.ID), Body: body}); err != nil {
			return store.RedactResult{}, h.db(ctx, err)
		}
	}
	return res, nil
}

// idleOperation returns the stored operation ref names, or nil when it was
// purged. Work referring to it or an unexpired lease on it is a conflict.
func (h *handle) idleOperation(ctx context.Context, ref model.OperationRef) (*store.OperationRecord, error) {
	busy, err := h.q.OperationBusy(ctx, db.OperationBusyParams{
		TenantID: h.tenantID, ResourceKind: operationKind, ID: string(ref.ID), Now: h.now(),
	})
	if err != nil {
		return nil, h.db(ctx, err)
	}
	if busy {
		return nil, conflict("originating operation still has work or a lease")
	}
	row, err := h.q.GetOperation(ctx, db.GetOperationParams{TenantID: h.tenantID, ID: string(ref.ID)})
	switch {
	case missing(err):
		return nil, nil
	case err != nil:
		return nil, h.db(ctx, err)
	}
	rec, err := decode[store.OperationRecord](row.Body)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// tombstoneAttachment rewrites one stored attachment record under rev and
// records the blob key it unbound.
func (h *handle) tombstoneAttachment(ctx context.Context, cur storedAttachment, rev model.Revision, res *store.RedactResult) error {
	next, blob := tombstone.Attachment(cur.rec, rev)
	body, err := encode(&next)
	if err != nil {
		return err
	}
	ref := next.Attachment.Ref
	n, err := h.q.UpdateAttachment(ctx, db.UpdateAttachmentParams{
		TenantID: h.tenantID, InboxID: string(ref.Inbox.ID), ID: string(ref.ID),
		Body: body, Revision: string(rev), Expected: cur.rev,
	})
	if err != nil {
		return h.db(ctx, err)
	}
	if n != 1 {
		return conflict("stale expected revision")
	}
	if blob != nil {
		// An intent already recorded for the key is kept as it is.
		if _, err := h.q.InsertStaging(ctx, h.stagingParams(store.StagingIntent{Key: *blob, Attachment: ref})); err != nil {
			return h.db(ctx, err)
		}
		res.Blobs = append(res.Blobs, *blob)
	}
	return nil
}

// tombstoneIngress rewrites every ingress record naming the provider
// message pm on its channel.
func (h *handle) tombstoneIngress(ctx context.Context, pm model.ExternalRef) error {
	bodies, err := h.q.ListIngressByMessage(ctx, db.ListIngressByMessageParams{
		TenantID: h.tenantID, ChannelID: string(pm.Channel.ID), MessageID: pm.ID,
	})
	if err != nil {
		return h.db(ctx, err)
	}
	for _, b := range bodies {
		rec, err := decode[store.IngressRecord](b)
		if err != nil {
			return err
		}
		next := tombstone.Ingress(rec)
		body, err := encode(&next)
		if err != nil {
			return err
		}
		if err := h.db(ctx, h.q.UpdateIngress(ctx, db.UpdateIngressParams{TenantID: h.tenantID, ID: string(rec.ID), Body: body})); err != nil {
			return err
		}
	}
	return nil
}

// RedactLinks tombstones identity's link history records; see
// store.Tx.RedactLinks.
func (t txn) RedactLinks(ctx context.Context, ref model.IdentityRef) (int, error) {
	h := t.h
	n := 0
	err := h.call(ctx, func() error {
		if err := h.scoped(ref); err != nil {
			return err
		}
		if _, err := h.q.GetIdentity(ctx, db.GetIdentityParams{
			TenantID: h.tenantID, InboxID: string(ref.Inbox.ID), ID: string(ref.ID),
		}); err != nil {
			return h.lookupErr(ctx, err)
		}
		rows, err := h.q.ListIdentityLinkBodies(ctx, db.ListIdentityLinkBodiesParams{
			TenantID: h.tenantID, InboxID: string(ref.Inbox.ID), IdentityID: string(ref.ID),
		})
		if err != nil {
			return h.db(ctx, err)
		}
		for _, row := range rows {
			rec, err := decode[store.LinkRecord](row.Body)
			if err != nil {
				return err
			}
			next, changed := tombstone.Link(rec)
			if !changed {
				continue
			}
			body, err := encode(&next)
			if err != nil {
				return err
			}
			if err := h.db(ctx, h.q.SetLinkBody(ctx, db.SetLinkBodyParams{TenantID: h.tenantID, ID: row.ID, Body: body})); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}
