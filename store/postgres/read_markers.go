package postgres

import (
	"context"

	"hop.top/uma/model"
	"hop.top/uma/store/postgres/internal/db"
)

func (r reader) ReadMarker(ctx context.Context, conv model.ConversationRef, actor model.ActorID) (model.ReadMarker, error) {
	var out model.ReadMarker
	err := r.h.call(ctx, func() error {
		if err := firstErr(r.h.scoped(conv), required("actor", string(actor))); err != nil {
			return err
		}
		row, err := r.h.q.GetReadMarker(ctx, db.GetReadMarkerParams{TenantID: r.h.tenantID, ConversationID: string(conv.ID), Actor: string(actor)})
		if err != nil {
			return r.h.lookupErr(ctx, err)
		}
		if out, err = decode[model.ReadMarker](row.Body); err != nil {
			return err
		}
		if out.Conversation != conv {
			out = model.ReadMarker{}
			return notFound()
		}
		return nil
	})
	return out, err
}

func (t txn) PutReadMarker(ctx context.Context, v model.ReadMarker, expected model.Revision) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := firstErr(h.scoped(v.Conversation), v.Validate()); err != nil {
			return err
		}
		if err := h.requireConversation(ctx, v.Conversation); err != nil {
			return err
		}
		m := v
		body, err := encode(&m)
		if err != nil {
			return err
		}
		unix, nanos := instant(v.ReadThrough)
		key := db.GetReadMarkerParams{TenantID: h.tenantID, ConversationID: string(v.Conversation.ID), Actor: string(v.Actor)}
		return h.put(ctx, v.Revision, expected,
			func() (string, error) {
				row, err := h.q.GetReadMarker(ctx, key)
				return row.Revision, err
			},
			func() error {
				return h.q.InsertReadMarker(ctx, db.InsertReadMarkerParams{
					TenantID: h.tenantID, InboxID: string(v.Conversation.Inbox.ID), ConversationID: string(v.Conversation.ID),
					Actor: string(v.Actor), ReadThroughUnix: unix, ReadThroughNanos: nanos, Body: body, Revision: string(v.Revision),
				})
			},
			func() (int64, error) {
				return h.q.UpdateReadMarker(ctx, db.UpdateReadMarkerParams{
					TenantID: h.tenantID, ConversationID: string(v.Conversation.ID), Actor: string(v.Actor),
					ReadThroughUnix: unix, ReadThroughNanos: nanos, Body: body, Revision: string(v.Revision), Expected: string(expected),
				})
			})
	})
}
