package engine

import (
	"fmt"
	"slices"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// checkWorkItemActs enforces the work item transition declaration rules:
// transitions are declared exactly when work_item_act is; each names a
// declared target status once, at least one declared source status and
// well-formed reasons, without duplicates.
func checkWorkItemActs(field string, d adapter.Descriptor) error {
	declared := slices.Contains(d.Capabilities, model.CapabilityWorkItemAct)
	switch {
	case declared && len(d.WorkItemActs) == 0:
		return ConfigError(field, "work item acts: required by the work_item_act capability")
	case !declared && len(d.WorkItemActs) > 0:
		return ConfigError(field+".WorkItemActs", "declared without the work_item_act capability")
	}
	for i, tr := range d.WorkItemActs {
		f := fmt.Sprintf("%s.WorkItemActs[%d]", field, i)
		if !model.IsWorkItemStatus(tr.To) {
			return ConfigError(f+".To", "must be a declared work item status")
		}
		if slices.ContainsFunc(d.WorkItemActs[:i], func(x adapter.WorkItemTransition) bool { return x.To == tr.To }) {
			return ConfigError(f+".To", "duplicate")
		}
		if len(tr.From) == 0 {
			return ConfigError(f+".From", "required")
		}
		for j, from := range tr.From {
			ff := fmt.Sprintf("%s.From[%d]", f, j)
			if !model.IsWorkItemStatus(from) {
				return ConfigError(ff, "must be a declared work item status")
			}
			if slices.Contains(tr.From[:j], from) {
				return ConfigError(ff, "duplicate")
			}
		}
		for j, r := range tr.Reasons {
			rf := fmt.Sprintf("%s.Reasons[%d]", f, j)
			if err := model.ValidateStatusReason(r); err != nil {
				return ConfigError(rf, SafeMessage(err))
			}
			if slices.Contains(tr.Reasons[:j], r) {
				return ConfigError(rf, "duplicate")
			}
		}
	}
	return nil
}

// WorkItemActDeclared reports, as model.Unsupported, whether d does not
// declare a provider work item act moving a work item from status from to
// status to with reason, empty meaning the adapter's default. A status the
// adapter's mapping does not cover (empty from) has no transition. Nothing
// is coerced: an undeclared target, source or reason is refused, so a
// closed provider status reopens only where the adapter declares it.
func WorkItemActDeclared(d adapter.Descriptor, from, to, reason string) error {
	if !slices.Contains(d.Capabilities, model.CapabilityWorkItemAct) {
		return model.NewError(model.Unsupported, "channel adapter does not declare work_item_act")
	}
	i := slices.IndexFunc(d.WorkItemActs, func(x adapter.WorkItemTransition) bool { return x.To == to })
	if i < 0 || from == "" || !slices.Contains(d.WorkItemActs[i].From, from) {
		return model.NewError(model.Unsupported, "channel adapter declares no work item transition from the current status to "+to)
	}
	if reason != "" && !slices.Contains(d.WorkItemActs[i].Reasons, reason) {
		return model.NewError(model.Unsupported, "channel adapter does not declare this status reason for the transition")
	}
	return nil
}

func cloneWorkItemActs(xs []adapter.WorkItemTransition) []adapter.WorkItemTransition {
	if xs == nil {
		return nil
	}
	out := make([]adapter.WorkItemTransition, len(xs))
	for i, x := range xs {
		out[i] = adapter.WorkItemTransition{To: x.To, From: slices.Clone(x.From), Reasons: slices.Clone(x.Reasons)}
	}
	return out
}
