package runtime

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// startEffector executes start operations: the first message of an
// outbound-first conversation, sent like a reply answering nothing on the
// conversation's stored route.
type startEffector struct{ e *engine.Engine }

func (startEffector) Intent() string { return operations.IntentStart }

func (fx startEffector) Decode(e model.Extension) (Job, error) {
	in, err := operations.DecodeStartIntent(e)
	if err != nil {
		return nil, err
	}
	return &startJob{replyJob: replyJob{
		intent: operations.ReplyIntent{Conversation: in.Conversation, Content: in.Content, Audience: in.Audience},
		e:      fx.e,
	}, inbox: in.Inbox}, nil
}

// startJob is one start: a reply job without a parent, authorized on the
// inbox the start was submitted to and gated on the start capability.
type startJob struct {
	replyJob
	inbox model.InboxRef
}

// Recheck revalidates the stored destination and reauthorizes the
// originating actor for model.ActionConversationStart on the inbox, as at
// submission.
func (j *startJob) Recheck(ctx context.Context, actor *engine.Client, rec store.OperationRecord) error {
	j.actor = rec.Key.Caller
	if err := j.checkRoute(); err != nil {
		return err
	}
	if j.conv.Ref.Inbox != j.inbox {
		return &model.Error{Code: model.Permanent, SafeMessage: "started conversation is not in the start's inbox", Retry: model.NoRetry}
	}
	return actor.Authorize(ctx, policy.Authorization{
		Action: rec.Key.Kind, Resource: j.inbox.Ref(), Audience: j.intent.Audience,
	}, j.inbox.Ref())
}

func (j *startJob) Capabilities() []model.Capability {
	return []model.Capability{model.CapabilityStartConversation}
}
