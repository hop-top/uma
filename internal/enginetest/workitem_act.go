package enginetest

import (
	"context"
	"slices"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
)

// ActFunc scripts one provider answer to a work item act.
type ActFunc func(context.Context, adapter.WorkItemActRequest) (adapter.WorkItemActOutcome, error)

// IssueActs are the work item transitions of an issue tracker with
// open and closed states only: close with reason completed or
// not_planned, reopen with reason reopened.
func IssueActs() []adapter.WorkItemTransition {
	return []adapter.WorkItemTransition{
		{To: model.WorkItemClosed, From: []string{model.WorkItemOpen}, Reasons: []string{"completed", "not_planned"}},
		{To: model.WorkItemOpen, From: []string{model.WorkItemClosed}, Reasons: []string{"reopened"}},
	}
}

// WithWorkItemActs declares the work_item_act capability with IssueActs.
func WithWorkItemActs(_ *engine.Config, f *OutFactory) { f.WorkItemActs = true }

// RespondActs queues scripted work item act answers, consumed in call
// order. Without one, an act is delivered with no provider state.
func (f *OutFactory) RespondActs(fns ...ActFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actScript = append(f.actScript, fns...)
}

// Acts returns the work item act requests that reached the provider.
func (f *OutFactory) Acts() []adapter.WorkItemActRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.acts)
}

// ActWorkItem records the request and answers it from the script.
func (i *OutInstance) ActWorkItem(ctx context.Context, r adapter.WorkItemActRequest) (adapter.WorkItemActOutcome, error) {
	i.F.mu.Lock()
	var fn ActFunc
	if len(i.F.actScript) > 0 {
		fn, i.F.actScript = i.F.actScript[0], i.F.actScript[1:]
	}
	i.F.acts = append(i.F.acts, r)
	i.F.log = append(i.F.log, "act")
	i.F.mu.Unlock()
	if fn == nil {
		return adapter.WorkItemActOutcome{Outcome: adapter.Outcome{State: model.Delivered}}, nil
	}
	return fn(ctx, r)
}
