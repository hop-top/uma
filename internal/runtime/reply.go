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

// replyEffector executes reply operations: a send on the conversation's
// stored route.
type replyEffector struct{ e *engine.Engine }

func (replyEffector) Intent() string { return operations.IntentReply }

func (fx replyEffector) Decode(e model.Extension) (Job, error) {
	in, err := operations.DecodeReplyIntent(e)
	if err != nil {
		return nil, err
	}
	return &replyJob{intent: in, e: fx.e}, nil
}

// replyJob is one reply: its intent and the conversation, inbox and channel
// it loaded.
type replyJob struct {
	intent  operations.ReplyIntent
	conv    model.Conversation
	inbox   model.Inbox
	channel model.Channel
	// parent is the public message the reply answers, if any is stored.
	parent *model.Message
	e      *engine.Engine
	actor  model.ActorID
	// lost is the work item binding state the latest provider refusal
	// showed, if any.
	lost string
}

func (j *replyJob) Audience() model.Audience { return j.intent.Audience }

// Load reads the conversation, its inbox and channel. The stored
// conversation route is the only destination; a linked identity on another
// channel is never substituted.
func (j *replyJob) Load(ctx context.Context, e *engine.Engine, tenant model.TenantID) (model.Channel, error) {
	j.e = e
	err := e.Store().View(ctx, tenant, func(r store.Reader) error {
		var err error
		if j.conv, err = r.GetConversation(ctx, j.intent.Conversation); err != nil {
			return err
		}
		if j.inbox, err = r.GetInbox(ctx, j.conv.Ref.Inbox); err != nil {
			return err
		}
		if j.channel, err = r.GetChannel(ctx, j.inbox.Channel); err != nil {
			return err
		}
		return j.loadParent(ctx, r)
	})
	return j.channel, err
}

// loadParent reads the message the reply answers. One gone, deleted,
// redacted or no longer public since the reply was accepted is answered
// without a thread.
func (j *replyJob) loadParent(ctx context.Context, r store.Reader) error {
	ref := j.intent.InReplyTo
	if ref == nil || !ref.In(j.conv.Ref) {
		return nil
	}
	m, err := r.GetMessage(ctx, *ref)
	switch {
	case model.IsCode(err, model.NotFound):
		return nil
	case err != nil:
		return err
	case m.Ref != *ref || m.Redacted || m.Deleted || engine.ReadAudience(m.Audience).Visibility != model.Public:
		return nil
	}
	j.parent = &m
	return nil
}

// Route is the stored conversation route and, for a reply answering a
// message, that message's provider message on the route's channel.
func (j *replyJob) Route() (model.ExternalRef, *model.ExternalRef) {
	route := j.conv.ReplyRoute
	if j.parent == nil {
		return route, nil
	}
	return route, operations.ProviderMessageRef(*j.parent, route.Channel)
}

// thread is the send's view of the parent: its references on the route's
// channel and its metadata.
func (j *replyJob) thread() *adapter.Thread {
	if j.parent == nil {
		return nil
	}
	t := &adapter.Thread{}
	for _, x := range j.parent.External {
		if x.Channel == j.conv.ReplyRoute.Channel {
			t.References = append(t.References, x)
		}
	}
	for _, x := range j.parent.Metadata {
		t.Metadata = append(t.Metadata, x.Clone())
	}
	return t
}

// recipients is a copy of the reply's recipients, nil when it names none.
func (j *replyJob) recipients() *model.ReplyRecipients {
	if j.intent.Recipients == nil {
		return nil
	}
	r := j.intent.Recipients.Clone()
	return &r
}

// Recheck revalidates the stored destination and reauthorizes the
// originating actor.
func (j *replyJob) Recheck(ctx context.Context, actor *engine.Client, rec store.OperationRecord) error {
	j.actor = rec.Key.Caller
	if err := j.checkRoute(); err != nil {
		return err
	}
	return actor.Authorize(ctx, policy.Authorization{
		Action: rec.Key.Kind, Resource: j.conv.Ref.Ref(), Audience: j.intent.Audience,
	}, j.conv.Ref.Ref())
}

// checkRoute requires a stored reply route on the conversation's channel.
func (j *replyJob) checkRoute() error { return checkRoute(j.conv, j.inbox, j.channel) }

// checkRoute requires a stored reply route on conv's channel: the channel
// of its inbox, which ch is.
func checkRoute(conv model.Conversation, inbox model.Inbox, ch model.Channel) error {
	route := conv.ReplyRoute
	switch {
	case route.IsZero():
		return &model.Error{Code: model.Permanent, SafeMessage: "conversation has no stored reply route", Retry: model.NoRetry}
	case route.Channel != inbox.Channel || ch.Ref != inbox.Channel:
		return &model.Error{Code: model.Permanent, SafeMessage: "stored reply route is not on the conversation's channel", Retry: model.NoRetry}
	}
	return nil
}

func (j *replyJob) SentContent() model.Content { return j.intent.Content.Clone() }

func (j *replyJob) Capabilities() []model.Capability {
	if j.intent.Recipients != nil {
		return []model.Capability{model.CapabilitySend, model.CapabilityReplyRecipients}
	}
	return []model.Capability{model.CapabilitySend}
}

func (j *replyJob) Bind(inst adapter.Instance) (ProviderCall, bool) {
	sender, ok := inst.(adapter.Sender)
	if !ok {
		return nil, false
	}
	return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
		out, err := sender.Send(ctx, adapter.SendRequest{Effect: eff, Content: j.intent.Content.Clone(), Thread: j.thread(), Recipients: j.recipients()})
		j.lost = out.Binding
		return out, err
	}, true
}

// Settle records the provider message IDs the settled reply reported on
// its outbound message, so an echo of it is recognized at ingress, links
// an inbound copy that already holds one as that message's echo, and,
// for a reply refused because the conversation's work item is gone, moves
// the work item to the binding state the refusal showed.
func (j *replyJob) Settle(ctx context.Context, tx store.Tx, op model.Operation) error {
	if err := operations.RecordMessageReferences(ctx, tx, j.e, op); err != nil {
		return err
	}
	if j.lost == "" || j.e == nil || (op.State != model.Rejected && op.State != model.Failed) {
		return nil
	}
	return loseBinding(ctx, tx, j.e, j.conv.Ref, j.lost, j.actor, op.Ref)
}

// loseBinding moves the active work item of conv to binding, with its
// work_item.updated event.
func loseBinding(ctx context.Context, tx store.Tx, e *engine.Engine, conv model.ConversationRef, binding string, actor model.ActorID, op model.OperationRef) error {
	cur, err := workitems.Bound(ctx, tx, conv)
	if err != nil || cur == nil {
		return err
	}
	now := e.Clock().Now().UTC()
	ch, ok := workitems.Lose(*cur, binding, now)
	if !ok {
		return nil
	}
	rev, err := operations.NewRevision(ctx, e)
	if err != nil {
		return err
	}
	id, err := operations.NewID(ctx, e)
	if err != nil {
		return err
	}
	ch.Item.Revision = rev
	if err := tx.PutWorkItem(ctx, ch.Item, ch.Previous); err != nil {
		return err
	}
	data, err := json.Marshal(workitems.Payload(ch, model.TriggerProvider, "", &op))
	if err != nil {
		return model.NewError(model.Permanent, "event payload encoding failed")
	}
	res := ch.Item.Ref.Ref()
	return tx.AppendEvents(ctx, []model.Event{{
		Version: model.EventEnvelopeVersion, ID: id, Kind: model.EventWorkItemUpdated,
		Tenant: res.Tenant, Inbox: res.Inbox, Resource: res, Actor: actor,
		OccurredAt: now, RecordedAt: now, Audience: model.Audience{Visibility: model.Internal}, Revision: rev,
		Payload: model.Extension{Name: model.SchemaWorkItemUpdated, Version: model.PayloadVersion1, Data: data},
	}})
}
