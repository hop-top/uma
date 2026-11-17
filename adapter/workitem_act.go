package adapter

import (
	"context"
	"time"

	"hop.top/uma/model"
)

// WorkItemTransition is one declared provider work item transition: the
// normalized status To that the adapter can move a work item to, the
// statuses From it leaves, and the status reasons the provider records
// for it (model.WorkItemUpdate.StatusReason).
//
// Registration requires To and every From to be a declared work item
// status (model.IsWorkItemStatus), From to be non-empty, each reason to
// pass model.ValidateStatusReason, and no duplicate To, From entry or
// reason. A request may always omit the reason, leaving the adapter its
// default; a transition with no Reasons takes no explicit reason. A
// transition or reason not declared is model.Unsupported when the command
// is submitted, before any claim: core never coerces one status or reason
// onto another.
type WorkItemTransition struct {
	To      string
	From    []string
	Reasons []string
}

// WorkItemActRequest asks the provider to move a work item to Status. The
// effect's Route is the work item's external reference (model.RefKindWorkItem)
// on its channel; Target is nil.
type WorkItemActRequest struct {
	Effect
	// Status is the requested normalized status, the To of a declared
	// transition from the work item's current status.
	Status string
	// Reason is the requested status reason, one the transition declares;
	// empty asks for the adapter's default.
	Reason string
}

// WorkItemActOutcome is the provider's answer to a work item act.
type WorkItemActOutcome struct {
	Outcome
	// State is the provider's work item state after the change, when the
	// provider returned it with an applied outcome; nil otherwise, such as
	// for an outcome found by reconciliation.
	State *model.WorkItemState
	// SourceRevision and OccurredAt are State's provider version, as on an
	// inbound work item observation.
	SourceRevision string
	OccurredAt     time.Time
}

// WorkItemActor changes provider-owned work item statuses
// (model.CapabilityWorkItemAct) along the transitions its descriptor
// declares (Descriptor.WorkItemActs). The engine calls ActWorkItem only for
// a declared transition and reason, once the operation is claimed.
//
// ActWorkItem performs the change without first reading the provider's
// state to skip it: the local snapshot may be stale, so a request for the
// state the provider already holds is sent anyway. An applied outcome
// reports the provider's resulting state in State when the provider
// returned it; core applies that state in source order, so it never
// overwrites a newer inbound observation, and changes the work item only
// once the provider succeeded. Rejected and failed outcomes change nothing
// locally; Outcome.Binding reports a work item the provider no longer has.
// The provider's own notification of the change (a webhook) is an ordinary
// inbound observation: it never settles the operation, which only the
// provider's answer or reconciliation does. No work item act changes its
// conversation's status.
type WorkItemActor interface {
	ActWorkItem(context.Context, WorkItemActRequest) (WorkItemActOutcome, error)
}
