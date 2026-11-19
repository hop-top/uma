package runtime

import (
	"context"
	"slices"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/route"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// stateCapabilities are the capabilities that carry provider state verbs;
// each has its own stored intent and effector.
var stateCapabilities = []model.Capability{
	model.CapabilityArchive, model.CapabilityMove, model.CapabilityReadState, model.CapabilityFlag, model.CapabilityTrash,
}

// stateEffector executes the provider state operations of one capability
// through the adapter's interface for it.
type stateEffector struct {
	e   *engine.Engine
	cap model.Capability
}

func (fx stateEffector) Intent() string { return operations.StateIntentName(fx.cap) }

func (fx stateEffector) Decode(x model.Extension) (Job, error) {
	in, err := operations.DecodeStateIntent(x, fx.cap)
	if err != nil {
		return nil, err
	}
	return &stateJob{e: fx.e, cap: fx.cap, in: in}, nil
}

// stateJob changes the provider state of one conversation, on the
// conversation's stored route. The effect has no target: the request
// carries the provider messages of the conversation on the channel, for a
// provider that fans out per message.
type stateJob struct {
	e   *engine.Engine
	cap model.Capability
	in  operations.StateIntent
	rt  route.Route
	// items are the provider messages of the conversation on the route's
	// channel.
	items []model.ExternalRef
	// changes are the reference changes the provider reported for the
	// applied effect.
	changes []adapter.ReferenceChange
}

func (j *stateJob) changeReferences(c []adapter.ReferenceChange) { j.changes = c }

func (j *stateJob) Audience() model.Audience { return operations.StateAudience() }

func (j *stateJob) Capabilities() []model.Capability { return []model.Capability{j.cap} }

// statePage bounds one read of the conversation's messages.
const statePage = 100

// Load reads the conversation's route and its provider messages.
func (j *stateJob) Load(ctx context.Context, e *engine.Engine, tenant model.TenantID) (model.Channel, error) {
	j.e = e
	err := e.Store().View(ctx, tenant, func(r store.Reader) error {
		var err error
		if j.rt, err = route.Load(ctx, r, j.in.Conversation); err != nil {
			return err
		}
		return j.loadItems(ctx, r)
	})
	return j.rt.Channel, err
}

func (j *stateJob) loadItems(ctx context.Context, r store.Reader) error {
	j.items = nil
	channel := j.rt.Conv.ReplyRoute.Channel
	after := model.Cursor("")
	want := j.in.Messages
	var found int
	for {
		p, err := r.Messages(ctx, model.ListRequest{
			Filter: model.Filter{Inboxes: []model.InboxRef{j.rt.Inbox.Ref}, Conversation: &j.in.Conversation},
			Page:   model.PageRequest{Limit: statePage, After: after},
		})
		if err != nil {
			return err
		}
		for _, m := range p.Items {
			if m.Deleted || m.Redacted {
				continue
			}
			if len(want) > 0 && !slices.Contains(want, m.Ref.ID) {
				continue
			}
			ref := operations.ProviderMessageRef(m, channel)
			if ref == nil {
				continue
			}
			found++
			if !slices.Contains(j.items, *ref) {
				j.items = append(j.items, *ref)
			}
		}
		if p.Complete || p.Next == "" || p.Next == after {
			return j.checkTargeted(found)
		}
		after = p.Next
	}
}

// checkTargeted fails a targeted change when a message it names is not
// among the loaded items: it must never fall back to the whole
// conversation.
func (j *stateJob) checkTargeted(found int) error {
	if len(j.in.Messages) > 0 && found != len(j.in.Messages) {
		return model.NewError(model.Validation, "a targeted message is no longer a provider message of the conversation")
	}
	return nil
}

// Route is the conversation's stored route; the effect has no target.
func (j *stateJob) Route() (model.ExternalRef, *model.ExternalRef) {
	return j.rt.Conv.ReplyRoute, nil
}

// Recheck revalidates the route and reauthorizes the originating actor under
// the operation's action on the conversation.
func (j *stateJob) Recheck(ctx context.Context, actor *engine.Client, rec store.OperationRecord) error {
	return route.Recheck(ctx, actor, rec, j.rt, j.Audience())
}

// StateChange is the request an attempt carries, handed to Reconcile with
// the attempt.
func (j *stateJob) StateChange() StateChange {
	c := StateChange{
		Items: slices.Clone(j.items), Targeted: len(j.in.Messages) > 0, FlagKind: j.in.Kind,
		Destination: j.in.Destination, AddLabels: slices.Clone(j.in.Add), RemoveLabels: slices.Clone(j.in.Remove),
	}
	switch j.in.Verb {
	case model.StateVerbUnarchive, model.StateVerbUnflag, model.StateVerbUntrash:
		c.Undo = true
	case model.StateVerbRead:
		c.Read = true
	}
	return c
}

func (j *stateJob) Bind(inst adapter.Instance) (ProviderCall, bool) {
	c := j.StateChange()
	switch j.cap {
	case model.CapabilityArchive:
		if a, ok := inst.(adapter.Archiver); ok {
			return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
				return a.Archive(ctx, adapter.ArchiveRequest{Effect: eff, Items: c.Items, Targeted: c.Targeted, Undo: c.Undo})
			}, true
		}
	case model.CapabilityMove:
		if m, ok := inst.(adapter.Mover); ok {
			return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
				return m.Move(ctx, adapter.MoveRequest{Effect: eff, Items: c.Items, Targeted: c.Targeted, Destination: c.Destination, Add: c.AddLabels, Remove: c.RemoveLabels})
			}, true
		}
	case model.CapabilityReadState:
		if s, ok := inst.(adapter.ReadStateSetter); ok {
			return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
				return s.SetReadState(ctx, adapter.ReadStateRequest{Effect: eff, Items: c.Items, Targeted: c.Targeted, Read: c.Read})
			}, true
		}
	case model.CapabilityFlag:
		if f, ok := inst.(adapter.Flagger); ok {
			return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
				return f.Flag(ctx, adapter.FlagRequest{Effect: eff, Items: c.Items, Targeted: c.Targeted, Kind: c.FlagKind, Undo: c.Undo})
			}, true
		}
	case model.CapabilityTrash:
		if t, ok := inst.(adapter.Trasher); ok {
			return func(ctx context.Context, eff adapter.Effect) (adapter.Outcome, error) {
				return t.Trash(ctx, adapter.TrashRequest{Effect: eff, Items: c.Items, Targeted: c.Targeted, Undo: c.Undo})
			}, true
		}
	}
	return nil, false
}

// Settle records the state change once the provider applied it (see
// operations.SettleState).
func (j *stateJob) Settle(ctx context.Context, tx store.Tx, op model.Operation) error {
	if err := operations.SettleState(ctx, tx, j.e, op, j.in); err != nil {
		return err
	}
	if len(j.changes) == 0 || !operations.AppliedState(op.State) {
		return nil
	}
	moved := make([]operations.ReferenceMove, len(j.changes))
	for i, c := range j.changes {
		moved[i] = operations.ReferenceMove{Old: c.Old, New: c.New}
	}
	return operations.RelinkMessages(ctx, tx, op, j.in.Conversation, moved)
}
