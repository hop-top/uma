package services

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/workitems"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// WorkItems is the WorkItems service of one client: authorized reads of
// work items and the WorkItems.Act command. The work item rules live
// in package workitems; this is their authorization and ledger boundary.
type WorkItems struct{ c *engine.Client }

// NewWorkItems returns the WorkItems service bound to c.
func NewWorkItems(c *engine.Client) WorkItems { return WorkItems{c} }

// Get returns a work item the principal may read. A work item outside the
// conversation, inbox or tenant the reference names is not found.
func (s WorkItems) Get(ctx context.Context, req model.Lookup[model.WorkItemRef]) (model.WorkItem, error) {
	c := s.c
	done, err := c.Engine().Begin()
	if err != nil {
		return model.WorkItem{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.WorkItem{}, err
	}
	if req.Ref.Scope() != c.Principal().Tenant {
		return model.WorkItem{}, engine.ErrNotFound()
	}
	var w model.WorkItem
	if err := view(ctx, c, func(r store.Reader) error {
		return loadWorkItem(ctx, r, req.Ref, &w)
	}); err != nil {
		return model.WorkItem{}, err
	}
	if err := c.Authorize(ctx, workItemRead(w), req.Ref.Ref()); err != nil {
		return model.WorkItem{}, err
	}
	return w.Clone(), nil
}

func workItemRead(w model.WorkItem) policy.Authorization {
	return policy.Authorization{Action: model.ActionWorkItemRead, Resource: w.Ref.Ref(), Revision: w.Revision}
}

// List pages the work items the principal may read, in recording order,
// optionally limited to Filter.Inboxes, Filter.Conversation and
// Filter.Statuses (normalized statuses).
func (s WorkItems) List(ctx context.Context, req model.ListRequest) (model.Page[model.WorkItem], error) {
	return list(ctx, s.c, listQuery[model.WorkItem]{
		name: "work_items", fields: filterFields{inboxes: true, conversation: true, statuses: true}, read: store.Reader.WorkItems,
		within: func(f model.Filter, w model.WorkItem) bool {
			return inInboxes(f, w.Ref.Conversation.Inbox) && inConversation(f, w.Ref.Conversation) && hasStatus(f, w.Status)
		},
		admit: func(ctx context.Context, c *engine.Client, w model.WorkItem) error {
			return c.Authorize(ctx, workItemRead(w), w.Ref.Ref())
		},
		clone: model.WorkItem.Clone,
	}, req)
}

// Act applies a model.SchemaWorkItemUpdate action to a work item at
// ExpectedRevision. Field ownership routes it (workitems.Classify): an
// update of fields the work item declares local commits as a local
// operation with a work_item.updated event of trigger command; a
// provider-owned status, with its optional reason, is an outbound
// operation (actOnProvider). Other provider-owned fields are
// model.Unsupported. A provider-owned status with a local field, or a
// status reason with a local status, is model.Validation. A binding that
// is not active, or leaving the terminal closed local status, is
// model.Conflict. A local update that changes nothing commits without
// event. Retrying with the same operation ID and payload returns the
// stored operation even after the work item moved on: a claimed key
// takes the path its stored operation took, before any classification.
func (s WorkItems) Act(ctx context.Context, req model.WorkItemAction) (model.Acceptance, error) {
	c, e := s.c, s.c.Engine()
	done, err := e.Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	update, err := model.DecodeWorkItemUpdate(req.Action)
	if err != nil {
		return model.Acceptance{}, err
	}
	if req.Target.Scope() != c.Principal().Tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	var route actRoute
	if err := view(ctx, c, func(r store.Reader) error {
		var err error
		route, err = routeAct(ctx, c, r, req, update)
		return err
	}); err != nil {
		return model.Acceptance{}, err
	}
	if route.provider {
		return s.actOnProvider(ctx, req, update)
	}
	ids, err := allocate(ctx, e, 4)
	if err != nil {
		return model.Acceptance{}, err
	}
	res := req.Target.Ref()
	return commit(ctx, c, localCommand{
		kind: model.ActionWorkItemAct, target: res, resource: res, owner: res,
		command: req.Command, fingerprint: operations.WorkItemActFingerprint(req), result: res,
		apply: func(ctx context.Context, tx store.Tx, op model.OperationRef) error {
			var cur model.WorkItem
			if err := loadWorkItem(ctx, tx, req.Target, &cur); err != nil {
				return err
			}
			if cur.Revision != req.ExpectedRevision {
				return model.NewError(model.Conflict, "work item revision changed")
			}
			ch, err := workitems.Update(cur, update)
			if err != nil || len(ch.Changes) == 0 {
				return err
			}
			ch.Item.Revision = model.Revision(ids[0])
			if err := tx.PutWorkItem(ctx, ch.Item, cur.Revision); err != nil {
				return err
			}
			ev, err := commandEvent(c, ids[1], model.EventWorkItemUpdated, model.SchemaWorkItemUpdated,
				res, ch.Item.Revision, model.Audience{Visibility: model.Internal}, req.Correlation,
				workitems.Payload(ch, model.TriggerCommand, "", &op))
			if err != nil {
				return err
			}
			return tx.AppendEvents(ctx, []model.Event{ev})
		},
	}, ids[2], model.Revision(ids[3]))
}

// loadWorkItem reads ref and requires the stored record to carry exactly
// that reference. A mismatch is not found.
func loadWorkItem(ctx context.Context, r store.Reader, ref model.WorkItemRef, out *model.WorkItem) error {
	w, err := r.GetWorkItem(ctx, ref)
	if err != nil {
		return err
	}
	if w.Ref != ref {
		return engine.ErrNotFound()
	}
	if out != nil {
		*out = w
	}
	return nil
}
