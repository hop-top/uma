package fakeprovider

import (
	"context"
	"fmt"
	"slices"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// TypeTracker is the type key of the tracker variant: it declares reconcile
// and work_item_act and implements exactly the matching interfaces.
const TypeTracker model.TypeKey = "org.example.fake.tracker"

// MethodActWorkItem is the recorded call of a work item act.
const MethodActWorkItem Method = "act_work_item"

// TrackerActs are the transitions of the tracker's workflow: a helpdesk
// with four statuses and its own reasons. A work item pends only from
// open, is resolved from open or pending, closes from any of the three,
// and reopens from pending, resolved or closed. Without a reason, a
// transition takes its first.
func TrackerActs() []adapter.WorkItemTransition {
	return []adapter.WorkItemTransition{
		{To: model.WorkItemPending, From: []string{model.WorkItemOpen}, Reasons: []string{"awaiting_customer", "awaiting_vendor"}},
		{To: model.WorkItemResolved, From: []string{model.WorkItemOpen, model.WorkItemPending}, Reasons: []string{"solved", "workaround"}},
		{To: model.WorkItemClosed, From: []string{model.WorkItemOpen, model.WorkItemPending, model.WorkItemResolved}, Reasons: []string{"wont_fix", "obsolete", "spam"}},
		{To: model.WorkItemOpen, From: []string{model.WorkItemPending, model.WorkItemResolved, model.WorkItemClosed}, Reasons: []string{"reopened", "escalated"}},
	}
}

func cloneActs(xs []adapter.WorkItemTransition) []adapter.WorkItemTransition {
	if xs == nil {
		return nil
	}
	out := make([]adapter.WorkItemTransition, len(xs))
	for i, x := range xs {
		out[i] = adapter.WorkItemTransition{To: x.To, From: slices.Clone(x.From), Reasons: slices.Clone(x.Reasons)}
	}
	return out
}

var tracker = variant{
	typ: TypeTracker, exchange: providerIDs,
	caps:   []model.Capability{model.CapabilityReconcile, model.CapabilityWorkItemAct},
	events: []string{model.EventWorkItemUpdated},
	acts:   TrackerActs(),
	open:   func(i *instance) adapter.Instance { return &Tracker{i: i} },
}

// NewTracker returns the factory of the tracker variant.
func NewTracker() *Factory { return newFactory(tracker) }

// Tracker is the tracker variant: a helpdesk whose work items the test
// seeds on the script (Script.SeedWorkItem) and the engine moves along
// TrackerActs. Acts take scripted results like sends; an applied one moves
// the work item and answers with its state, under a source revision and
// time that count up per script. An act on a work item the script never
// seeded is rejected as not found. Reconcile reports the latest act of an
// operation as a send's.
type Tracker struct{ i *instance }

// Capabilities reports reconcile and work_item_act.
func (x *Tracker) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	return x.i.capabilities(ctx)
}

// Close closes the instance; it is idempotent.
func (x *Tracker) Close(context.Context) error { return x.i.close() }

// Reconcile reports what the provider recorded for the effect's operation.
func (x *Tracker) Reconcile(ctx context.Context, r adapter.ReconcileRequest) (adapter.Outcome, error) {
	return x.i.reconcile(ctx, r.Effect)
}

// ActWorkItem moves the work item the route names to the requested status
// under the next scripted result.
func (x *Tracker) ActWorkItem(ctx context.Context, r adapter.WorkItemActRequest) (adapter.WorkItemActOutcome, error) {
	i := x.i
	if err := i.ready(ctx, model.CapabilityWorkItemAct); err != nil {
		return adapter.WorkItemActOutcome{}, err
	}
	if err := i.authenticate(ctx); err != nil {
		return adapter.WorkItemActOutcome{}, err
	}
	if r.Route.Channel != i.channel.Ref || r.Target != nil {
		return adapter.WorkItemActOutcome{}, model.NewError(model.Validation, "a work item act names the channel's work item and no target")
	}
	r.Effect = cloneEffect(r.Effect)
	return i.script.applyAct(Call{Method: MethodActWorkItem, Effect: r.Effect, Status: r.Status, Reason: r.Reason})
}

// SeedWorkItem makes the provider hold the work item route names in status.
func (s *Script) SeedWorkItem(route model.ExternalRef, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.work[route.ID] = status
}

// WorkItemStatus returns the status the provider holds for the work item
// route names.
func (s *Script) WorkItemStatus(route model.ExternalRef) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status, ok := s.work[route.ID]
	return status, ok
}

// applyAct takes the next scripted result for the act, records it and moves
// the work item when the provider applied it.
func (s *Script) applyAct(call Call) (adapter.WorkItemActOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	route := call.Effect.Route
	r := s.fallback
	if len(s.results) > 0 {
		r = s.results[0]
		s.results = s.results[1:]
	}
	_, known := s.work[route.ID]
	call.Result = r
	call.Applied = known && (r == Accepted || r == OutcomeUnknown)
	var state *model.WorkItemState
	var rev string
	var at time.Time
	if call.Applied {
		s.acts++
		reason := call.Reason
		if reason == "" {
			reason = defaultReason(call.Status)
		}
		s.work[route.ID] = call.Status
		call.References = []model.ExternalRef{route}
		rev, at = fmt.Sprintf("act-%d", s.acts), time.Unix(1_700_000_000, 0).UTC().Add(time.Duration(s.acts)*time.Minute)
		state = &model.WorkItemState{
			Subject: "Tracked item", URL: "https://tracker.example/items/" + route.ID, Status: call.Status,
			RawStatus: call.Status + ":" + reason, Binding: model.WorkItemBindingActive,
			Requester: model.ExternalRef{Channel: s.channel, Namespace: Namespace, Kind: "user", ID: "requester-1"},
		}
	}
	s.calls = append(s.calls, cloneCall(call))
	if !known && (r == Accepted || r == OutcomeUnknown) {
		return adapter.WorkItemActOutcome{Outcome: adapter.Outcome{State: model.Rejected, Failure: model.NewError(model.NotFound, "no such work item")}}, nil
	}
	out, err := outcomeOf(r, call.References, s.after)
	if err != nil {
		return adapter.WorkItemActOutcome{}, err
	}
	if out.State == model.ProviderAccepted {
		out = adapter.Outcome{State: model.Delivered}
	}
	return adapter.WorkItemActOutcome{Outcome: out, State: state, SourceRevision: rev, OccurredAt: at}, nil
}

// defaultReason is the reason a transition to status takes when none is
// requested.
func defaultReason(status string) string {
	for _, tr := range TrackerActs() {
		if tr.To == status {
			return tr.Reasons[0]
		}
	}
	return ""
}

var _ adapter.WorkItemActor = (*Tracker)(nil)
var _ adapter.Reconciler = (*Tracker)(nil)
