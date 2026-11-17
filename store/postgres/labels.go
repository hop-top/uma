package postgres

import (
	"context"

	"hop.top/uma/model"
	"hop.top/uma/store/postgres/internal/db"
)

func (r reader) GetLabel(ctx context.Context, ref model.LabelRef) (model.Label, error) {
	return getBody[model.Label](ctx, r.h, ref, func() ([]byte, error) {
		row, err := r.h.q.GetLabel(ctx, db.GetLabelParams{TenantID: r.h.tenantID, ID: string(ref.ID)})
		return row.Body, err
	})
}

func (r reader) LabelByName(ctx context.Context, name string) (model.Label, error) {
	var out model.Label
	err := r.h.call(ctx, func() error {
		if err := required("name", name); err != nil {
			return err
		}
		row, err := r.h.q.GetLabelByNameKey(ctx, db.GetLabelByNameKeyParams{TenantID: r.h.tenantID, NameKey: model.LabelNameKey(name)})
		if err != nil {
			return r.h.lookupErr(ctx, err)
		}
		out, err = decode[model.Label](row.Body)
		return err
	})
	return out, err
}

func (r reader) Labels(ctx context.Context, req model.ListRequest) (model.Page[model.Label], error) {
	return listBodies[model.Label](ctx, r.h, req.Page,
		func() error { return r.h.checkFilter(req.Filter, filterSupport{}) },
		func(after int64, lim int32) ([]positioned, error) {
			rows, err := r.h.q.ListLabels(ctx, db.ListLabelsParams{TenantID: r.h.tenantID, After: after, Lim: lim})
			return positions(rows, err, func(x db.ListLabelsRow) positioned { return positioned{x.Pos, x.Body} })
		})
}

func (t txn) PutLabel(ctx context.Context, v model.Label, expected model.Revision) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := firstErr(h.scoped(v.Ref), v.Validate()); err != nil {
			return err
		}
		key, id := model.LabelNameKey(v.Name), string(v.Ref.ID)
		holder, err := h.keyHolder(ctx, key, func() (string, error) {
			row, err := h.q.GetLabelByNameKey(ctx, db.GetLabelByNameKeyParams{TenantID: h.tenantID, NameKey: key})
			return row.ID, err
		})
		if err != nil {
			return err
		}
		if holder != "" && holder != id {
			return conflict("name already names another label")
		}
		body, err := encode(&v)
		if err != nil {
			return err
		}
		return h.put(ctx, v.Revision, expected,
			func() (string, error) {
				row, err := h.q.GetLabel(ctx, db.GetLabelParams{TenantID: h.tenantID, ID: id})
				return row.Revision, err
			},
			func() error {
				return h.q.InsertLabel(ctx, db.InsertLabelParams{
					TenantID: h.tenantID, ID: id, NameKey: key, Body: body, Revision: string(v.Revision),
				})
			},
			func() (int64, error) {
				return h.q.UpdateLabel(ctx, db.UpdateLabelParams{
					TenantID: h.tenantID, ID: id, NameKey: key, Body: body,
					Revision: string(v.Revision), Expected: string(expected),
				})
			})
	})
}

func (t txn) DeleteLabel(ctx context.Context, ref model.LabelRef, expected model.Revision) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := firstErr(h.scoped(ref), required("expected revision", string(expected))); err != nil {
			return err
		}
		id := string(ref.ID)
		row, err := h.q.GetLabel(ctx, db.GetLabelParams{TenantID: h.tenantID, ID: id})
		if err != nil {
			return h.lookupErr(ctx, err)
		}
		if err := cas(true, model.Revision(row.Revision), expected); err != nil {
			return err
		}
		// An assignment committed after this unit's snapshot fails the
		// delete as a serialization failure, never a missing label.
		held, err := h.q.LabelAssigned(ctx, db.LabelAssignedParams{TenantID: h.tenantID, LabelID: id})
		if err != nil {
			return h.db(ctx, err)
		}
		if held {
			return conflict("label is assigned; remove it first")
		}
		n, err := h.q.DeleteLabel(ctx, db.DeleteLabelParams{TenantID: h.tenantID, ID: id, Expected: string(expected)})
		if err != nil {
			return h.db(ctx, err)
		}
		if n != 1 {
			return conflict("stale expected revision")
		}
		return nil
	})
}

// checkLabels requires a conversation's labels to be valid and stored.
func (h *handle) checkLabels(ctx context.Context, v model.Conversation) error {
	if err := v.ValidateLabels(); err != nil {
		return err
	}
	if len(v.Labels) == 0 {
		return nil
	}
	n, err := h.q.CountLabels(ctx, db.CountLabelsParams{TenantID: h.tenantID, Ids: labelIDs(v.Labels)})
	if err != nil {
		return h.db(ctx, err)
	}
	if n != int64(len(v.Labels)) {
		return notFound()
	}
	return nil
}

// putConversationLabels replaces the label rows of a stored conversation
// with labels, the index the conversation label selector reads.
func (h *handle) putConversationLabels(ctx context.Context, conversation string, labels []model.LabelRef) error {
	if err := h.q.DeleteConversationLabels(ctx, db.DeleteConversationLabelsParams{TenantID: h.tenantID, ConversationID: conversation}); err != nil {
		return h.db(ctx, err)
	}
	return h.insertConversationLabels(ctx, conversation, labels)
}

// insertConversationLabels adds the label rows of a conversation that has
// none.
func (h *handle) insertConversationLabels(ctx context.Context, conversation string, labels []model.LabelRef) error {
	if len(labels) == 0 {
		return nil
	}
	return h.db(ctx, h.q.InsertConversationLabels(ctx, db.InsertConversationLabelsParams{
		TenantID: h.tenantID, ConversationID: conversation, LabelIds: labelIDs(labels),
	}))
}

// labelIDs returns label IDs as query arguments, never nil.
func labelIDs(labels []model.LabelRef) []string {
	out := make([]string, len(labels))
	for i, l := range labels {
		out[i] = string(l.ID)
	}
	return out
}

// selectorArgs returns the filter's label selector parts as query
// arguments; an empty part is no constraint.
func selectorArgs(f model.Filter) (anyOf, allOf, noneOf []string) {
	if f.Labels == nil {
		return []string{}, []string{}, []string{}
	}
	return labelIDs(f.Labels.Any), labelIDs(f.Labels.All), labelIDs(f.Labels.None)
}
