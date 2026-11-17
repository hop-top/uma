package operations

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Settling mutations. An edit, delete or reaction changes its target only
// once the provider applied it: the operation settled provider_accepted
// or delivered. The settle runs inside the unit that settles the
// operation, possibly without the job having loaded anything (applied
// evidence settles before Load), so it reads the target, the caller and
// the channel's adapter itself. Rejected, failed and unknown outcomes
// touch nothing. A redacted target is never written. The provider
// references of a mutation are never joined to the target message: for a
// reaction they name the reaction and live on its entry.

// SettleEdit applies an applied edit unless the target's edit state
// advanced since the claim: a newer inbound edit, a deletion or a
// redaction. Not advanced, the content becomes the edit's, the caller is
// recorded as the editor and message.updated edited is emitted. Advanced,
// the content stays: when it already equals the edit's (an unmarked echo
// beat the answer) nothing happens; otherwise message.updated
// edit_superseded names the operation ("not applied locally because the
// base advanced", not "the provider never applied it") and, when the
// channel's adapter declares refetch, a refetch of the target is queued
// under the caller. The operation keeps the provider's answer either way.
func SettleEdit(ctx context.Context, tx store.Tx, e *engine.Engine, op model.Operation, in EditIntent) error {
	s, ok, err := openSettle(ctx, tx, e, op, in.Message)
	if !ok || err != nil {
		return err
	}
	cur := s.msg
	if sameEdit(cur.LastEdit, in.Base.LastEdit) && !cur.Deleted {
		prev := cur.Revision
		cur.Content = in.Content.Clone()
		op := s.op.Ref
		cur.LastEdit = &model.Edit{Actor: s.caller, At: s.now, Operation: &op}
		return s.put(cur, prev, model.MessageChangeEdited, nil)
	}
	if SameContent(cur.Content, in.Content) {
		return nil
	}
	if err := s.event(cur, cur.Revision, model.MessageChangeEditSuperseded, nil); err != nil {
		return err
	}
	if cur.Deleted {
		// A tombstone has no content left to diverge.
		return nil
	}
	return s.refetch(cur)
}

// SettleDelete tombstones the target of an applied delete: deleted, its
// content and reactions cleared, its editor record kept, with
// message.updated deleted. An already deleted target is left as it is.
func SettleDelete(ctx context.Context, tx store.Tx, e *engine.Engine, op model.Operation, in DeleteIntent) error {
	s, ok, err := openSettle(ctx, tx, e, op, in.Message)
	if !ok || err != nil || s.msg.Deleted {
		return err
	}
	cur := s.msg
	prev := cur.Revision
	cur.Deleted, cur.Content = true, model.Content{}
	cur.Reactions, cur.ReactionsComplete = nil, false
	return s.put(cur, prev, model.MessageChangeDeleted, nil)
}

// SettleReact adds or removes the caller's reaction of an applied react
// operation: an entry attributed to the caller (By nil, Count 1) holding
// the provider's references of the reaction, with message.updated
// reaction_added or reaction_removed. A present key added or an absent
// one removed changes nothing. When the channel's adapter declares
// single-reaction cardinality, the caller's earlier reaction is removed
// first, and a removal removes it whatever its emoji.
func SettleReact(ctx context.Context, tx store.Tx, e *engine.Engine, op model.Operation, in ReactIntent) error {
	s, ok, err := openSettle(ctx, tx, e, op, in.Message)
	if !ok || err != nil || s.msg.Deleted {
		return err
	}
	entry := model.MessageReaction{
		Emoji: in.Emoji, Actor: s.caller, Count: 1, External: slices.Clone(op.ProviderReferences), At: s.now,
	}
	single := false
	if r, ok, err := s.route(); err != nil {
		return err
	} else if ok {
		single = r.desc.Exchange.Reactions == adapter.ReactionsSingle
	}
	for _, step := range s.msg.ReactionSteps(entry, in.Remove, single) {
		if err := s.react(step); err != nil {
			return err
		}
	}
	return nil
}

// react applies step to the settling target, storing and announcing a
// change.
func (s *settling) react(step model.ReactionStep) error {
	cur := s.msg.Clone()
	prev := cur.Revision
	change := model.MessageChangeReactionAdded
	if step.Remove {
		change = model.MessageChangeReactionRemoved
	}
	if !cur.ApplyReactionStep(step) {
		return nil
	}
	return s.put(cur, prev, change, &model.PayloadReaction{Emoji: step.Reaction.Emoji})
}

// settling is the settling unit's view of a mutation's target.
type settling struct {
	ctx    context.Context
	tx     store.Tx
	e      *engine.Engine
	op     model.Operation
	caller model.ActorID
	msg    model.Message
	now    time.Time
}

// openSettle reads what a settle needs, reporting false when nothing
// applies: the operation not applied, the target gone or redacted.
func openSettle(ctx context.Context, tx store.Tx, e *engine.Engine, op model.Operation, ref model.MessageRef) (*settling, bool, error) {
	if op.State != model.ProviderAccepted && op.State != model.Delivered {
		return nil, false, nil
	}
	m, err := tx.GetMessage(ctx, ref)
	switch {
	case model.IsCode(err, model.NotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	case m.Ref != ref || m.Redacted:
		return nil, false, nil
	}
	rec, err := tx.Operation(ctx, op.Ref)
	if err != nil {
		return nil, false, err
	}
	return &settling{ctx: ctx, tx: tx, e: e, op: op, caller: rec.Key.Caller, msg: m, now: e.Clock().Now().UTC()}, true, nil
}

// put stores m, changed from revision prev, under a new revision and
// announces change.
func (s *settling) put(m model.Message, prev model.Revision, change string, r *model.PayloadReaction) error {
	rev, err := NewRevision(s.ctx, s.e)
	if err != nil {
		return err
	}
	m.Revision = rev
	if err := s.tx.PutMessage(s.ctx, m, prev); err != nil {
		return err
	}
	s.msg = m
	return s.event(m, prev, change, r)
}

// event appends message.updated for m, command-triggered by the caller
// under the operation, under m's audience. Its payload carries no
// content.
func (s *settling) event(m model.Message, prev model.Revision, change string, r *model.PayloadReaction) error {
	id, err := NewID(s.ctx, s.e)
	if err != nil {
		return err
	}
	op := s.op.Ref
	data, err := json.Marshal(model.MessageUpdatedPayload{
		Message: m.Ref, Change: change, Trigger: model.TriggerCommand, PreviousRevision: prev, Reaction: r, Operation: &op,
	})
	if err != nil {
		return model.NewError(model.Permanent, "event payload encoding failed")
	}
	res := m.Ref.Ref()
	return s.tx.AppendEvents(s.ctx, []model.Event{{
		Version: model.EventEnvelopeVersion, ID: id, Kind: model.EventMessageUpdated,
		Tenant: res.Tenant, Inbox: res.Inbox, Resource: res, Actor: s.caller,
		OccurredAt: s.now, RecordedAt: s.now, Audience: engine.ReadAudience(m.Audience), Revision: m.Revision,
		Payload: model.Extension{Name: model.SchemaMessageUpdated, Version: model.PayloadVersion1, Data: data},
	}})
}

// targetRoute is the target's conversation, inbox, channel and the
// descriptor of the channel's adapter.
type targetRoute struct {
	conv  model.Conversation
	inbox model.Inbox
	desc  adapter.Descriptor
}

// route reads the target's route, reporting false when the channel's
// adapter is no longer registered.
func (s *settling) route() (targetRoute, bool, error) {
	var r targetRoute
	var err error
	if r.conv, err = s.tx.GetConversation(s.ctx, s.msg.Ref.Conversation); err != nil {
		return r, false, err
	}
	if r.inbox, err = s.tx.GetInbox(s.ctx, r.conv.Ref.Inbox); err != nil {
		return r, false, err
	}
	ch, err := s.tx.GetChannel(s.ctx, r.inbox.Channel)
	if err != nil {
		return r, false, err
	}
	_, d, ok := s.e.Adapter(ch.Type)
	r.desc = d
	return r, ok, nil
}

// refetch queues a refetch of m's provider message under the caller when
// the channel's adapter declares refetch, keyed by the operation, so a
// repeated settle queues it once. The descriptor decides, as the unit has
// no adapter instance to ask: the channel's reported availability gates
// the step when it runs, which settles it failed when refetch is
// unavailable for good and retries it after a temporary outage.
func (s *settling) refetch(m model.Message) error {
	r, ok, err := s.route()
	if err != nil || !ok || !slices.Contains(r.desc.Capabilities, model.CapabilityRefetch) {
		return err
	}
	target := ProviderMessageRef(m, r.conv.ReplyRoute.Channel)
	if target == nil || r.conv.ReplyRoute.Channel != r.inbox.Channel {
		return nil
	}
	in := RefetchIntent{Channel: r.inbox.Channel, Target: *target}
	if len(r.inbox.Destinations) > 0 {
		in.Destination = r.inbox.Destinations[0]
	}
	return RecordRefetch(s.ctx, s.tx, s.e, s.caller, model.OperationID(RefetchKeyPrefix+string(s.op.Ref.ID)), m.Ref.Ref(), in)
}

// sameEdit reports whether a and b record the same edit: same time,
// actor, provider identity and operation.
func sameEdit(a, b *model.Edit) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.At.Equal(b.At) && a.Actor == b.Actor && samePtr(a.By, b.By) && samePtr(a.Operation, b.Operation)
}

func samePtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// SameContent reports whether a and b are the same content, part for
// part.
func SameContent(a, b model.Content) bool {
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ja) == string(jb)
}
