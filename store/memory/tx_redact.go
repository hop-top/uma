package memory

import (
	"context"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/internal/tombstone"
)

// Redact tombstones a message, interaction or attachment and the records holding copies
// of its content; see store.Tx.Redact. Every check runs before the first
// write.
func (t tx) Redact(ctx context.Context, req store.Redaction) (store.RedactResult, error) {
	h, err := t.write(ctx)
	if err != nil {
		return store.RedactResult{}, err
	}
	defer h.mu.Unlock()
	if err := tombstone.Check(req); err != nil {
		return store.RedactResult{}, err
	}
	if req.Interaction != nil {
		return h.redactInteraction(*req.Interaction, req.Expected, req.Revision)
	}
	if req.Attachment != nil {
		return h.redactAttachment(*req.Attachment, req.Expected, req.Revision)
	}
	return h.redactMessage(*req.Message, req.Expected, req.Revision)
}

func (h *handle) redactAttachment(ref model.AttachmentRef, expected, rev model.Revision) (store.RedactResult, error) {
	if err := h.scoped(ref); err != nil {
		return store.RedactResult{}, err
	}
	cur, ok := h.st.attachments.get(ref)
	if !ok {
		return store.RedactResult{}, notFound()
	}
	if err := cas(true, cur.v.Attachment.Revision, expected); err != nil {
		return store.RedactResult{}, err
	}
	var res store.RedactResult
	h.tombstoneAttachment(cur, rev, &res)
	return res, nil
}

func (h *handle) redactMessage(ref model.MessageRef, expected, rev model.Revision) (store.RedactResult, error) {
	if err := h.scoped(ref); err != nil {
		return store.RedactResult{}, err
	}
	cur, ok := h.st.messages.get(ref)
	if !ok {
		return store.RedactResult{}, notFound()
	}
	if err := cas(true, cur.v.Revision, expected); err != nil {
		return store.RedactResult{}, err
	}
	m := cur.v
	var op *entry[store.OperationRecord]
	if m.Origin != nil && m.Origin.Operation.Tenant == h.tenant {
		opRef := m.Origin.Operation
		if !h.idle(opRef.Ref()) {
			return store.RedactResult{}, conflict("originating operation still has work or a lease")
		}
		if e, found := h.st.operations.get(opRef); found {
			op = &e
		}
	}

	h.st.messages.set(ref, entry[model.Message]{seq: cur.seq, at: cur.at, v: tombstone.Message(m, rev)})
	var res store.RedactResult
	for _, a := range tombstone.Attachments(m) {
		if a.Scope() != h.tenant {
			continue
		}
		if e, found := h.st.attachments.get(a); found && e.v.State != store.AttachmentRedacted {
			h.tombstoneAttachment(e, rev, &res)
		}
	}
	for _, pm := range tombstone.ProviderMessageIDs(m) {
		for _, e := range h.st.ingress.sorted(func(r store.IngressRecord) bool {
			return r.Channel == pm.Channel && r.Input.Envelope.MessageID == pm.ID
		}) {
			h.st.ingress.set(e.v.ID, entry[store.IngressRecord]{seq: e.seq, at: e.at, v: tombstone.Ingress(e.v)})
		}
	}
	if op != nil {
		h.st.operations.set(op.v.Operation.Ref, entry[store.OperationRecord]{seq: op.seq, at: op.at, v: tombstone.Operation(op.v)})
	}
	return res, nil
}

// tombstoneAttachment rewrites one stored attachment record under rev and
// records the blob key it unbound.
func (h *handle) tombstoneAttachment(e entry[store.AttachmentRecord], rev model.Revision, res *store.RedactResult) {
	next, blob := tombstone.Attachment(e.v.Clone(), rev)
	h.st.attachments.set(next.Attachment.Ref, entry[store.AttachmentRecord]{seq: e.seq, at: e.at, v: next})
	if blob != nil {
		if _, held := h.st.staging.get(*blob); !held {
			h.st.staging.set(*blob, entry[store.StagingIntent]{
				seq: h.st.next(), at: h.now(), v: store.StagingIntent{Key: *blob, Attachment: next.Attachment.Ref},
			})
		}
		res.Blobs = append(res.Blobs, *blob)
	}
}

// RedactLinks tombstones identity's link history records; see
// store.Tx.RedactLinks.
func (t tx) RedactLinks(ctx context.Context, ref model.IdentityRef) (int, error) {
	h, err := t.write(ctx)
	if err != nil {
		return 0, err
	}
	defer h.mu.Unlock()
	if err := h.scoped(ref); err != nil {
		return 0, err
	}
	if _, ok := h.st.identities.get(ref); !ok {
		return 0, notFound()
	}
	n := 0
	for i, rec := range h.st.links.items {
		if rec.Identity != ref {
			continue
		}
		if next, changed := tombstone.Link(rec); changed {
			h.st.links.set(i, next)
			n++
		}
	}
	return n, nil
}
