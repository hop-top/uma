package runtime

import (
	"context"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// noteEffector executes delivered internal notes: a private note on the
// conversation's stored route through the adapter's note sender.
type noteEffector struct{ e *engine.Engine }

func (noteEffector) Intent() string { return operations.IntentNote }

func (fx noteEffector) Decode(e model.Extension) (Job, error) {
	in, err := operations.DecodeNoteIntent(e)
	if err != nil {
		return nil, err
	}
	return &noteJob{intent: in, e: fx.e}, nil
}

// noteJob is one delivered note: its intent and the conversation, inbox
// and channel it loaded.
type noteJob struct {
	intent  operations.NoteIntent
	conv    model.Conversation
	inbox   model.Inbox
	channel model.Channel
	e       *engine.Engine
}

func (j *noteJob) Audience() model.Audience { return j.intent.Audience }

func (j *noteJob) Load(ctx context.Context, e *engine.Engine, tenant model.TenantID) (model.Channel, error) {
	err := e.Store().View(ctx, tenant, func(r store.Reader) error {
		var err error
		if j.conv, err = r.GetConversation(ctx, j.intent.Conversation); err != nil {
			return err
		}
		if j.inbox, err = r.GetInbox(ctx, j.conv.Ref.Inbox); err != nil {
			return err
		}
		j.channel, err = r.GetChannel(ctx, j.inbox.Channel)
		return err
	})
	return j.channel, err
}

func (j *noteJob) Route() (model.ExternalRef, *model.ExternalRef) { return j.conv.ReplyRoute, nil }

// Recheck refuses a note whose inbox no longer delivers notes or whose
// audience became public, revalidates the stored route and reauthorizes
// the originating actor. A refused note stays in uma.
func (j *noteJob) Recheck(ctx context.Context, actor *engine.Client, rec store.OperationRecord) error {
	route := j.conv.ReplyRoute
	switch {
	case !j.inbox.DeliverNotes:
		return &model.Error{Code: model.Disabled, SafeMessage: "inbox no longer delivers internal notes", Retry: model.NoRetry}
	case engine.ReadAudience(j.intent.Audience).Visibility == model.Public:
		return &model.Error{Code: model.Permanent, SafeMessage: "an internal note is never public", Retry: model.NoRetry}
	case route.IsZero():
		return &model.Error{Code: model.Permanent, SafeMessage: "conversation has no stored reply route", Retry: model.NoRetry}
	case route.Channel != j.inbox.Channel || j.channel.Ref != j.inbox.Channel:
		return &model.Error{Code: model.Permanent, SafeMessage: "stored reply route is not on the conversation's channel", Retry: model.NoRetry}
	}
	return actor.Authorize(ctx, policy.Authorization{
		Action: rec.Key.Kind, Resource: j.conv.Ref.Ref(), Audience: j.intent.Audience,
	}, j.conv.Ref.Ref())
}

func (j *noteJob) SentContent() model.Content { return j.intent.Content.Clone() }

func (j *noteJob) Capabilities() []model.Capability {
	return []model.Capability{model.CapabilityInternalNote}
}

func (j *noteJob) Bind(inst adapter.Instance) (ProviderCall, bool) {
	ns, ok := inst.(adapter.NoteSender)
	if !ok {
		return nil, false
	}
	return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
		return ns.SendNote(ctx, adapter.NoteRequest{Effect: eff, Content: j.intent.Content.Clone()})
	}, true
}

// Settle records the provider message IDs of the delivered note on it, so
// its echo is recognized at ingress.
func (j *noteJob) Settle(ctx context.Context, tx store.Tx, op model.Operation) error {
	return operations.RecordMessageReferences(ctx, tx, j.e, op)
}
