package workitems

import (
	"slices"

	"hop.top/uma/model"
)

// Path is where a WorkItems.Act update applies.
type Path int

const (
	// PathLocal commits the update in uma: every field it sets is local.
	PathLocal Path = iota + 1
	// PathProvider sends the update to the provider: it sets a
	// provider-owned status, with or without a reason, and nothing else.
	PathProvider
)

// Classify routes update u of cur by field ownership. The binding must be
// active (model.Conflict otherwise: act on the successor). A provider-owned
// field other than status is ingest-only (model.Unsupported). A
// provider-owned status with any other field is model.Validation: one
// operation never mixes a provider change with a local one. A status
// reason with a local status is model.Validation: reasons are provider
// vocabulary. Whether the provider path is declared for the work item's
// channel is the caller's check.
func Classify(cur model.WorkItem, u model.WorkItemUpdate) (Path, error) {
	if b := cur.Binding; b != "" && b != model.WorkItemBindingActive {
		return 0, model.NewError(model.Conflict, "work item binding is "+b)
	}
	changed := u.Changed()
	for _, f := range changed {
		if f != model.WorkItemFieldStatus && !slices.Contains(cur.LocalFields, f) {
			return 0, model.NewError(model.Unsupported, "the provider owns work item field "+f+"; outbound work item writes are not enabled for it")
		}
	}
	if u.Status == nil || slices.Contains(cur.LocalFields, model.WorkItemFieldStatus) {
		if u.StatusReason != nil {
			return 0, model.NewError(model.Validation, "action.data.status_reason: only for a status the provider owns")
		}
		return PathLocal, nil
	}
	if len(changed) > 1 {
		return 0, model.NewError(model.Validation, "action.data: send provider-owned and uma-owned fields in separate actions")
	}
	return PathProvider, nil
}
