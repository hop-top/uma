package services

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/workitems"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// actRoute is the path of one WorkItems.Act request: provider when it is
// an outbound operation, local otherwise.
type actRoute struct{ provider bool }

// routeAct reads the work item req targets and decides the path of req.
// A claimed operation key keeps the path its stored operation took, so a
// retry replays (or conflicts) whatever the work item became. Otherwise
// the stored work item is classified: a provider-owned status takes the
// provider path once its channel's adapter declares the transition and
// reason (model.Unsupported otherwise, before any policy check or claim).
// Any other update, refusals included, takes the local path, whose
// claiming unit classifies it again after authorization.
func routeAct(ctx context.Context, c *engine.Client, r store.Reader, req model.WorkItemAction, u model.WorkItemUpdate) (actRoute, error) {
	var cur model.WorkItem
	if err := loadWorkItem(ctx, r, req.Target, &cur); err != nil {
		return actRoute{}, err
	}
	key := store.OperationKey{
		Tenant: c.Principal().Tenant, Caller: c.Principal().Actor, Kind: model.ActionWorkItemAct,
		Target: req.Target.Ref(), ID: req.Operation,
	}
	switch rec, err := r.OperationByKey(ctx, key); {
	case err == nil:
		return actRoute{provider: rec.Command.Name == operations.IntentWorkItemAct}, nil
	case !model.IsCode(err, model.NotFound):
		return actRoute{}, err
	}
	if path, err := workitems.Classify(cur, u); err != nil || path != workitems.PathProvider {
		return actRoute{}, nil
	}
	if err := actDeclared(ctx, c.Engine(), r, cur, u); err != nil {
		return actRoute{}, err
	}
	return actRoute{provider: true}, nil
}

// actDeclared checks that the adapter of cur's channel declares moving
// cur from its current status to u's status with u's reason. A work item
// whose reference is on another channel than its inbox's has no
// transition.
func actDeclared(ctx context.Context, e *engine.Engine, r store.Reader, cur model.WorkItem, u model.WorkItemUpdate) error {
	inbox, err := r.GetInbox(ctx, cur.Ref.Conversation.Inbox)
	if err != nil {
		return err
	}
	ch, err := r.GetChannel(ctx, inbox.Channel)
	if err != nil {
		return err
	}
	if cur.External.Channel != ch.Ref {
		return model.NewError(model.Unsupported, "work item reference is not on its inbox's channel")
	}
	_, d, ok := e.Adapter(ch.Type)
	if !ok {
		return model.NewError(model.Unsupported, "channel adapter not registered")
	}
	return engine.WorkItemActDeclared(d, cur.Status, deref(u.Status), deref(u.StatusReason))
}

// actOnProvider queues a provider-owned status change as an outbound
// operation: authorized as model.ActionWorkItemAct on the work item at
// ExpectedRevision, keyed (kind work_item.act, target the work item ref)
// and fingerprinted as the local path is, its result the work item. The
// claiming unit of a new operation checks the work item again: present,
// at ExpectedRevision, still routed to the provider and the transition
// still declared. Nothing changes locally: the runtime's work item act
// effector asks the provider and applies its answer.
func (s WorkItems) actOnProvider(ctx context.Context, req model.WorkItemAction, u model.WorkItemUpdate) (model.Acceptance, error) {
	intent, err := operations.WorkItemActIntent{
		WorkItem: req.Target, Status: deref(u.Status), Reason: deref(u.StatusReason),
	}.Extension()
	if err != nil {
		return model.Acceptance{}, err
	}
	e := s.c.Engine()
	res := req.Target.Ref()
	return operations.Submit(ctx, s.c, operations.Submission{
		Kind: model.ActionWorkItemAct, Target: res, Owner: res, Command: req.Command,
		Fingerprint: operations.WorkItemActFingerprint(req), Intent: intent, Result: &res,
		Precheck: func(ctx context.Context, tx store.Tx) error {
			var cur model.WorkItem
			if err := loadWorkItem(ctx, tx, req.Target, &cur); err != nil {
				return err
			}
			if cur.Revision != req.ExpectedRevision {
				return model.NewError(model.Conflict, "work item revision changed")
			}
			path, err := workitems.Classify(cur, u)
			if err != nil {
				return err
			}
			if path != workitems.PathProvider {
				return model.NewError(model.Conflict, "work item status is local now; submit the action again")
			}
			return actDeclared(ctx, e, tx, cur, u)
		},
	})
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
