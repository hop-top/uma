package runtime

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// mutationJob is the part edit, delete and react jobs share: the target
// message on its conversation's stored route. The effect's target is the
// message's provider message on the route's channel. While the message
// has none, the job waits only as long as the operation that sends the
// message is queued, retry_pending or outcome_unknown and the intent's
// WaitUntil has not passed; otherwise the attempt fails permanently. No
// authorship is checked: the provider answers for what the account may
// change.
type mutationJob struct {
	e          *engine.Engine
	in         operations.MutationIntent
	capability model.Capability
	conv       model.Conversation
	inbox      model.Inbox
	channel    model.Channel
	// msg is the target as last read; nil when it is gone.
	msg  *model.Message
	wait bool
}

func (j *mutationJob) Audience() model.Audience { return j.in.Audience }

func (j *mutationJob) Capabilities() []model.Capability { return []model.Capability{j.capability} }

// Load reads the conversation, its inbox and channel, and the target.
func (j *mutationJob) Load(ctx context.Context, e *engine.Engine, tenant model.TenantID) (model.Channel, error) {
	j.e = e
	err := e.Store().View(ctx, tenant, func(r store.Reader) error {
		var err error
		if j.conv, err = r.GetConversation(ctx, j.in.Message.Conversation); err != nil {
			return err
		}
		if j.inbox, err = r.GetInbox(ctx, j.conv.Ref.Inbox); err != nil {
			return err
		}
		if j.channel, err = r.GetChannel(ctx, j.inbox.Channel); err != nil {
			return err
		}
		m, err := r.GetMessage(ctx, j.in.Message)
		switch {
		case model.IsCode(err, model.NotFound):
			return nil
		case err != nil:
			return err
		case m.Ref == j.in.Message:
			j.msg = &m
		}
		j.wait, err = j.waitForProviderMessage(ctx, r)
		return err
	})
	return j.channel, err
}

// waitForProviderMessage reports whether the target, present and without
// a provider message on the route's channel, may still get one: its
// sending operation has no final answer yet and WaitUntil has not passed.
func (j *mutationJob) waitForProviderMessage(ctx context.Context, r store.Reader) (bool, error) {
	m := j.msg
	if m == nil || m.Deleted || m.Redacted || m.Origin == nil || j.target() != nil ||
		!j.e.Clock().Now().Before(j.in.WaitUntil) {
		return false, nil
	}
	rec, err := r.Operation(ctx, m.Origin.Operation)
	if model.IsCode(err, model.NotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch rec.Operation.State {
	case model.Queued, model.RetryPending, model.Unknown:
		return true, nil
	}
	return false, nil
}

func (j *mutationJob) Wait() bool  { return j.wait }
func (j *mutationJob) Local() bool { return false }

// target is the target's provider message on the route's channel.
func (j *mutationJob) target() *model.ExternalRef {
	if j.msg == nil {
		return nil
	}
	return operations.ProviderMessageRef(*j.msg, j.conv.ReplyRoute.Channel)
}

// Route is the stored conversation route and the target's provider
// message on its channel.
func (j *mutationJob) Route() (model.ExternalRef, *model.ExternalRef) {
	return j.conv.ReplyRoute, j.target()
}

// Recheck revalidates the route and the target, which must be present,
// neither deleted nor redacted, no live stream's message and hold a
// provider message on the route's channel, then reauthorizes the
// originating actor under the operation's action with the message's
// audience.
func (j *mutationJob) Recheck(ctx context.Context, actor *engine.Client, rec store.OperationRecord) error {
	if err := checkRoute(j.conv, j.inbox, j.channel); err != nil {
		return err
	}
	var m model.Message
	err := j.e.Store().View(ctx, j.conv.Ref.Inbox.Tenant, func(r store.Reader) (err error) {
		m, err = operations.CheckMutationTarget(ctx, r, j.in.Message)
		return err
	})
	if err != nil {
		if f := AsFailure(err); f.Code == model.NotFound || f.Code == model.Conflict {
			return noRetry(f)
		}
		return err
	}
	j.msg = &m
	if j.target() == nil {
		return &model.Error{Code: model.Permanent, SafeMessage: "message has no provider message on the route's channel", Retry: model.NoRetry}
	}
	res := m.Ref.Ref()
	return actor.Authorize(ctx, policy.Authorization{
		Action: rec.Key.Kind, Resource: res, Audience: engine.ReadAudience(m.Audience),
	}, res)
}
