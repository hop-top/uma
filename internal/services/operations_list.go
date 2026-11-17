package services

import (
	"context"
	"slices"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// ListOperations pages the operations the client's principal may read, most
// recently written first, optionally limited to Filter.Inboxes,
// Filter.Conversation, Filter.Statuses (operation states), Filter.Kinds
// (operation kinds), Filter.Actor (the caller of the operation key) and the
// window Filter.Since (inclusive) and Filter.Before (exclusive) on the time
// the operation was last written. Any other filter field is
// model.Validation.
//
// Kinds and Actor are evaluated here, on the record's key: the store read
// never sees them (store.Reader.Operations does not honour them).
//
// Each operation is exposed under the check Operations.Get makes
// (operations.AuthorizeRead) and shown as Get shows it, Pending
// included; one the principal may not read is left out without trace.
func ListOperations(ctx context.Context, c *engine.Client, req model.ListRequest) (model.Page[model.Operation], error) {
	recs, err := list(ctx, c, listQuery[store.OperationRecord]{
		name:   "operations",
		fields: filterFields{inboxes: true, conversation: true, statuses: true, kinds: true, actor: true, since: true, before: true},
		require: func(f model.Filter) error {
			for _, st := range f.Statuses {
				if !model.IsDeliveryState(model.DeliveryState(st)) {
					return model.NewError(model.Validation, "filter.statuses: must be operation states")
				}
			}
			return nil
		},
		read: func(r store.Reader, ctx context.Context, req model.ListRequest) (model.Page[store.OperationRecord], error) {
			req.Filter.Kinds, req.Filter.Actor = nil, nil
			return r.Operations(ctx, req)
		},
		// The window is the store's, on the write time that each record
		// then shows as Operation.UpdatedAt.
		within: func(f model.Filter, rec store.OperationRecord) bool {
			sc := rec.Scope()
			return (len(f.Inboxes) == 0 || sc.Inbox != "" && inInboxes(f, model.InboxRef{Tenant: rec.Operation.Ref.Tenant, ID: sc.Inbox})) &&
				(f.Conversation == nil || sc.Inbox == f.Conversation.Inbox.ID && sc.Conversation == f.Conversation.ID) &&
				hasStatus(f, string(rec.Operation.State)) &&
				(len(f.Kinds) == 0 || slices.Contains(f.Kinds, rec.Key.Kind)) &&
				(f.Actor == nil || *f.Actor == rec.Key.Caller)
		},
		admit: func(ctx context.Context, c *engine.Client, rec store.OperationRecord) error {
			return operations.AuthorizeRead(ctx, c, rec)
		},
		clone: store.OperationRecord.Clone,
		// A complete page names the position of the last operation scanned,
		// which may be one the principal cannot read.
		sealed: true,
	}, req)
	if err != nil {
		return model.Page[model.Operation]{}, err
	}
	out := model.Page[model.Operation]{Items: make([]model.Operation, 0, len(recs.Items)), Next: recs.Next, Complete: recs.Complete}
	for _, rec := range recs.Items {
		op, err := operations.Present(ctx, c, rec)
		if err != nil {
			return model.Page[model.Operation]{}, err
		}
		out.Items = append(out.Items, op)
	}
	return out, nil
}
