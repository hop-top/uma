package runtime

import (
	"context"
	"encoding/json"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/workitems"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// workItemActEffector executes provider work item acts: a provider-owned
// status change WorkItems.Act queued, sent to the adapter's
// adapter.WorkItemActor on the work item's external reference.
type workItemActEffector struct{ e *engine.Engine }

func (workItemActEffector) Intent() string { return operations.IntentWorkItemAct }

func (fx workItemActEffector) Decode(e model.Extension) (Job, error) {
	in, err := operations.DecodeWorkItemActIntent(e)
	if err != nil {
		return nil, err
	}
	return &workItemActJob{intent: in, e: fx.e}, nil
}

// workItemActJob is one work item act: its intent, the work item, inbox
// and channel it loaded, and the provider's answer to the latest attempt
// of this execution.
type workItemActJob struct {
	intent  operations.WorkItemActIntent
	e       *engine.Engine
	item    model.WorkItem
	inbox   model.Inbox
	channel model.Channel
	actor   model.ActorID
	// answer is the provider's applied answer, nil when the latest attempt
	// of this execution was not applied or none ran.
	answer *adapter.WorkItemActOutcome
	// lost is the work item binding state the latest provider refusal
	// showed, if any.
	lost string
}

// Audience is internal: work item changes and their operations are staff
// records, as work_item.updated is.
func (j *workItemActJob) Audience() model.Audience { return model.Audience{Visibility: model.Internal} }

// Load reads the work item, its inbox and the inbox's channel.
func (j *workItemActJob) Load(ctx context.Context, e *engine.Engine, tenant model.TenantID) (model.Channel, error) {
	j.e = e
	ref := j.intent.WorkItem
	if ref.Scope() != tenant {
		return model.Channel{}, engine.ErrNotFound()
	}
	err := e.Store().View(ctx, tenant, func(r store.Reader) error {
		var err error
		if j.item, err = r.GetWorkItem(ctx, ref); err != nil {
			return err
		}
		if j.item.Ref != ref {
			return engine.ErrNotFound()
		}
		if j.inbox, err = r.GetInbox(ctx, ref.Conversation.Inbox); err != nil {
			return err
		}
		j.channel, err = r.GetChannel(ctx, j.inbox.Channel)
		return err
	})
	return j.channel, err
}

// Route is the work item's external reference; the act has no separate
// target.
func (j *workItemActJob) Route() (model.ExternalRef, *model.ExternalRef) { return j.item.External, nil }

// Recheck requires the work item's reference on its inbox's channel and an
// active binding, and reauthorizes the originating actor. The work item's
// current status is not compared with the requested one: the local copy
// may be stale, so the provider is asked whatever it shows.
func (j *workItemActJob) Recheck(ctx context.Context, actor *engine.Client, rec store.OperationRecord) error {
	j.actor = rec.Key.Caller
	switch b := j.item.Binding; {
	case j.item.External.Channel != j.channel.Ref:
		return &model.Error{Code: model.Permanent, SafeMessage: "work item reference is not on its inbox's channel", Retry: model.NoRetry}
	case b != "" && b != model.WorkItemBindingActive:
		return &model.Error{Code: model.Conflict, SafeMessage: "work item binding is " + b, Retry: model.NoRetry}
	}
	res := j.item.Ref.Ref()
	return actor.Authorize(ctx, policy.Authorization{Action: rec.Key.Kind, Resource: res}, res)
}

// RequestedAct is the status and reason the act requests, for Reconcile.
func (j *workItemActJob) RequestedAct() (status, reason string) {
	return j.intent.Status, j.intent.Reason
}

func (j *workItemActJob) Capabilities() []model.Capability {
	return []model.Capability{model.CapabilityWorkItemAct}
}

func (j *workItemActJob) Bind(inst adapter.Instance) (ProviderCall, bool) {
	actor, ok := inst.(adapter.WorkItemActor)
	if !ok {
		return nil, false
	}
	return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
		out, err := actor.ActWorkItem(ctx, adapter.WorkItemActRequest{Effect: eff, Status: j.intent.Status, Reason: j.intent.Reason})
		j.answer, j.lost = nil, out.Binding
		if err == nil && operations.AppliedState(out.Outcome.State) {
			j.answer = &out
		}
		return out.Outcome, err
	}, true
}

// Settle applies the provider's answer once the operation settled applied:
// the state the provider returned updates the work item in source order,
// as an inbound observation would, so it never overwrites a newer one,
// with a work_item.updated event of trigger command naming the operation.
// An applied answer without state, or a settlement from evidence alone,
// changes nothing: the provider's own notification brings the state. A
// refusal showing the work item gone moves its binding; any other
// failure changes nothing. The conversation is never touched.
func (j *workItemActJob) Settle(ctx context.Context, tx store.Tx, op model.Operation) error {
	if j.e == nil {
		return nil
	}
	if j.lost != "" && (op.State == model.Rejected || op.State == model.Failed) {
		return loseBinding(ctx, tx, j.e, j.item.Ref.Conversation, j.lost, j.actor, op.Ref)
	}
	if j.answer == nil || j.answer.State == nil || !operations.AppliedState(op.State) || !j.ownState(*j.answer.State) {
		return nil
	}
	cur, err := tx.GetWorkItem(ctx, j.intent.WorkItem)
	if model.IsCode(err, model.NotFound) || (err == nil && cur.Ref != j.intent.WorkItem) {
		return nil
	}
	if err != nil {
		return err
	}
	obs := workitems.Observation{
		External: cur.External, State: j.answer.State, SourceRevision: j.answer.SourceRevision, OccurredAt: j.answer.OccurredAt,
	}
	ch, ok, err := workitems.Import(&cur, nil, cur.Ref.Conversation, obs, func() (model.ID, error) { return operations.NewID(ctx, j.e) })
	if err != nil || !ok {
		return err
	}
	if err := tx.PutWorkItem(ctx, ch.Item, ch.Previous); err != nil {
		return err
	}
	if len(ch.Changes) == 0 {
		return nil
	}
	return j.updated(ctx, tx, ch, op.Ref)
}

// ownState reports whether s is a valid work item state whose references
// all belong to the work item's channel, as ingress requires of an
// inbound one. Any other is not applied.
func (j *workItemActJob) ownState(s model.WorkItemState) bool {
	if s.Validate() != nil {
		return false
	}
	refs := append([]model.ExternalRef{}, s.Assignees...)
	if !s.Requester.IsZero() {
		refs = append(refs, s.Requester)
	}
	if s.Successor != nil {
		refs = append(refs, *s.Successor)
	}
	for _, r := range refs {
		if r.Channel != j.channel.Ref {
			return false
		}
	}
	return true
}

// updated appends the work_item.updated event of ch, command-triggered by
// the originating actor's operation op.
func (j *workItemActJob) updated(ctx context.Context, tx store.Tx, ch workitems.Change, op model.OperationRef) error {
	id, err := operations.NewID(ctx, j.e)
	if err != nil {
		return err
	}
	data, err := json.Marshal(workitems.Payload(ch, model.TriggerCommand, j.answer.SourceRevision, &op))
	if err != nil {
		return model.NewError(model.Permanent, "event payload encoding failed")
	}
	now := j.e.Clock().Now().UTC()
	res := ch.Item.Ref.Ref()
	return tx.AppendEvents(ctx, []model.Event{{
		Version: model.EventEnvelopeVersion, ID: id, Kind: model.EventWorkItemUpdated,
		Tenant: res.Tenant, Inbox: res.Inbox, Resource: res, Actor: j.actor,
		OccurredAt: now, RecordedAt: now, Audience: j.Audience(), Revision: ch.Item.Revision,
		Payload: model.Extension{Name: model.SchemaWorkItemUpdated, Version: model.PayloadVersion1, Data: data},
	}})
}
