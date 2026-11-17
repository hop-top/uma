package conformance

import (
	"encoding/json"
	"slices"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// The state cases run the provider conversation state verbs through the
// engine (Conversations.Archive, Move, SetRead, Flag, Trash) against the
// adapter under test. The target is a conversation holding one provider
// message: the one uma sent when the adapter sends, else the one it
// received. A case runs once per verb the adapter declares, in a fresh
// environment, and takes the move shape, flag kinds and destination roles
// from the limits the adapter reports.

// Fixture names the state cases give the provider.
const (
	stateFolderA = "conformance-folder-a"
	stateFolderB = "conformance-folder-b"
	stateLabelA  = "conformance-label-a"
	stateLabelB  = "conformance-label-b"
)

var stateCapabilities = []model.Capability{
	model.CapabilityArchive, model.CapabilityMove, model.CapabilityReadState, model.CapabilityFlag, model.CapabilityTrash,
}

// stateStep is one provider state verb request: how it is submitted, what
// the provider is asked, and what applying it shows. A step may need
// another applied first (before), such as an undo.
type stateStep struct {
	name   string
	cap    model.Capability
	before *stateStep
	submit func(e *adapterEnv, conv model.ConversationRef, id model.OperationID) (model.Acceptance, error)
	// call, holds, item and report may be nil: the request fields the
	// provider call carries, what the conversation's stored state shows,
	// what the provider's own item shows, and the provider report that
	// shows the change.
	call   func(ProviderCall) bool
	holds  func(model.ProviderConversationState) bool
	item   func(ItemState) bool
	report func(*model.ProviderConversationState)
}

// declaredState returns the state capabilities the adapter declares.
func (e *adapterEnv) declaredState() []model.Capability {
	var out []model.Capability
	for _, c := range stateCapabilities {
		if e.declares(c) {
			out = append(out, c)
		}
	}
	return out
}

// stateLimits reads the limits the adapter reports for the state
// capability c, failing when it reports none or unreadable ones.
func (e *adapterEnv) stateLimits(t *testing.T, c model.Capability) model.StateLimits {
	t.Helper()
	statuses, err := e.open(t, adapterChanA).Capabilities(e.ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	for _, s := range statuses {
		if s.Name != c {
			continue
		}
		l, err := model.ParseStateLimits(s.Limits)
		if err != nil {
			t.Fatalf("%s limits %+v: %v, want readable state limits with a scope", c, s.Limits, err)
		}
		return l
	}
	t.Fatalf("Capabilities reports no %s", c)
	return model.StateLimits{}
}

func hasString(xs []string, s string) bool { return slices.Contains(xs, s) }

func hasLabel(s model.ProviderConversationState, id string) bool {
	return slices.ContainsFunc(s.Labels, func(l model.ProviderLabel) bool { return l.ID == id })
}

// boolValue reports a read value as a pointer.
func boolValue(b bool) *bool { return &b }

// stateSteps lists the steps of every declared state capability.
func (e *adapterEnv) stateSteps(t *testing.T) []*stateStep {
	t.Helper()
	var out []*stateStep
	for _, c := range e.declaredState() {
		out = append(out, e.capabilitySteps(t, c)...)
	}
	return out
}

func (e *adapterEnv) capabilitySteps(t *testing.T, c model.Capability) []*stateStep {
	t.Helper()
	l := e.stateLimits(t, c)
	switch c {
	case model.CapabilityArchive:
		do := &stateStep{
			name: "archive", cap: c,
			submit: func(e *adapterEnv, conv model.ConversationRef, id model.OperationID) (model.Acceptance, error) {
				return e.client.Conversations().Archive(e.ctx, model.ArchiveConversation{Command: command(id), Conversation: conv})
			},
			call:   func(p ProviderCall) bool { return !p.Undo },
			holds:  func(s model.ProviderConversationState) bool { return s.Archived },
			item:   func(i ItemState) bool { return i.Archived },
			report: func(s *model.ProviderConversationState) { s.Archived = true },
		}
		undo := &stateStep{
			name: "unarchive", cap: c, before: do,
			submit: func(e *adapterEnv, conv model.ConversationRef, id model.OperationID) (model.Acceptance, error) {
				return e.client.Conversations().Archive(e.ctx, model.ArchiveConversation{Command: command(id), Conversation: conv, Undo: true})
			},
			call:   func(p ProviderCall) bool { return p.Undo },
			holds:  func(s model.ProviderConversationState) bool { return !s.Archived },
			item:   func(i ItemState) bool { return !i.Archived },
			report: func(s *model.ProviderConversationState) { s.Archived = false },
		}
		return []*stateStep{do, undo}
	case model.CapabilityTrash:
		do := &stateStep{
			name: "trash", cap: c,
			submit: func(e *adapterEnv, conv model.ConversationRef, id model.OperationID) (model.Acceptance, error) {
				return e.client.Conversations().Trash(e.ctx, model.TrashConversation{Command: command(id), Conversation: conv})
			},
			call:   func(p ProviderCall) bool { return !p.Undo },
			holds:  func(s model.ProviderConversationState) bool { return s.Trashed },
			item:   func(i ItemState) bool { return i.Trashed },
			report: func(s *model.ProviderConversationState) { s.Trashed = true },
		}
		undo := &stateStep{
			name: "untrash", cap: c, before: do,
			submit: func(e *adapterEnv, conv model.ConversationRef, id model.OperationID) (model.Acceptance, error) {
				return e.client.Conversations().Trash(e.ctx, model.TrashConversation{Command: command(id), Conversation: conv, Undo: true})
			},
			call:   func(p ProviderCall) bool { return p.Undo },
			holds:  func(s model.ProviderConversationState) bool { return !s.Trashed },
			item:   func(i ItemState) bool { return !i.Trashed },
			report: func(s *model.ProviderConversationState) { s.Trashed = false },
		}
		return []*stateStep{do, undo}
	case model.CapabilityReadState:
		read := func(name string, value bool, before *stateStep) *stateStep {
			return &stateStep{
				name: name, cap: c, before: before,
				submit: func(e *adapterEnv, conv model.ConversationRef, id model.OperationID) (model.Acceptance, error) {
					return e.client.Conversations().SetRead(e.ctx, model.SetConversationRead{Command: command(id), Conversation: conv, Read: value})
				},
				call:   func(p ProviderCall) bool { return p.Read == value },
				holds:  func(s model.ProviderConversationState) bool { return s.Read != nil && *s.Read == value },
				item:   func(i ItemState) bool { return i.Read == value },
				report: func(s *model.ProviderConversationState) { s.Read = boolValue(value) },
			}
		}
		r := read("read", true, nil)
		return []*stateStep{r, read("unread", false, r)}
	case model.CapabilityFlag:
		var out []*stateStep
		for _, kind := range l.Kinds {
			do := &stateStep{
				name: "flag-" + kind, cap: c,
				submit: func(e *adapterEnv, conv model.ConversationRef, id model.OperationID) (model.Acceptance, error) {
					return e.client.Conversations().Flag(e.ctx, model.FlagConversation{Command: command(id), Conversation: conv, Kind: kind})
				},
				call:   func(p ProviderCall) bool { return p.FlagKind == kind && !p.Undo },
				holds:  func(s model.ProviderConversationState) bool { return hasString(s.Flags, kind) },
				item:   func(i ItemState) bool { return hasString(i.Flags, kind) },
				report: func(s *model.ProviderConversationState) { s.Flags = []string{kind} },
			}
			undo := &stateStep{
				name: "unflag-" + kind, cap: c, before: do,
				submit: func(e *adapterEnv, conv model.ConversationRef, id model.OperationID) (model.Acceptance, error) {
					return e.client.Conversations().Flag(e.ctx, model.FlagConversation{Command: command(id), Conversation: conv, Kind: kind, Undo: true})
				},
				call:   func(p ProviderCall) bool { return p.FlagKind == kind && p.Undo },
				holds:  func(s model.ProviderConversationState) bool { return !hasString(s.Flags, kind) },
				item:   func(i ItemState) bool { return !hasString(i.Flags, kind) },
				report: func(s *model.ProviderConversationState) { s.Flags = nil },
			}
			out = append(out, do, undo)
		}
		return out
	case model.CapabilityMove:
		return moveSteps(l)
	}
	return nil
}

func moveSubmit(m func(id model.OperationID, conv model.ConversationRef) model.MoveConversation) func(*adapterEnv, model.ConversationRef, model.OperationID) (model.Acceptance, error) {
	return func(e *adapterEnv, conv model.ConversationRef, id model.OperationID) (model.Acceptance, error) {
		return e.client.Conversations().Move(e.ctx, m(id, conv))
	}
}

// moveSteps are the moves of a provider with limits l: to a folder, then
// to another, for a folder-shaped one; label changes and a destination
// label for a label-shaped one; and every role it resolves.
func moveSteps(l model.StateLimits) []*stateStep {
	var out []*stateStep
	switch l.Shape {
	case model.MoveShapeFolder:
		to := func(name, id string, before *stateStep) *stateStep {
			return &stateStep{
				name: name, cap: model.CapabilityMove, before: before,
				submit: moveSubmit(func(op model.OperationID, conv model.ConversationRef) model.MoveConversation {
					return model.MoveConversation{Command: command(op), Conversation: conv, Destination: model.MoveDestination{ID: id}}
				}),
				call:   func(p ProviderCall) bool { return p.Destination.ID == id && len(p.AddLabels)+len(p.RemoveLabels) == 0 },
				holds:  func(s model.ProviderConversationState) bool { return s.Folder != nil && s.Folder.ID == id },
				item:   func(i ItemState) bool { return i.Folder != nil && i.Folder.ID == id && len(i.Labels) == 0 },
				report: func(s *model.ProviderConversationState) { s.Folder = &model.Folder{ID: id, Name: id} },
			}
		}
		a := to("move", stateFolderA, nil)
		out = append(out, a, to("move-again", stateFolderB, a))
	case model.MoveShapeLabels:
		add := &stateStep{
			name: "move", cap: model.CapabilityMove,
			submit: moveSubmit(func(op model.OperationID, conv model.ConversationRef) model.MoveConversation {
				return model.MoveConversation{Command: command(op), Conversation: conv, Add: []string{stateLabelA}}
			}),
			call: func(p ProviderCall) bool {
				return slices.Equal(p.AddLabels, []string{stateLabelA}) && len(p.RemoveLabels) == 0
			},
			holds: func(s model.ProviderConversationState) bool { return hasLabel(s, stateLabelA) && s.Folder == nil },
			item:  func(i ItemState) bool { return hasString(i.Labels, stateLabelA) && i.Folder == nil },
			report: func(s *model.ProviderConversationState) {
				s.Labels = []model.ProviderLabel{{ID: stateLabelA, Name: stateLabelA}}
			},
		}
		remove := &stateStep{
			name: "move-remove-label", cap: model.CapabilityMove, before: add,
			submit: moveSubmit(func(op model.OperationID, conv model.ConversationRef) model.MoveConversation {
				return model.MoveConversation{Command: command(op), Conversation: conv, Remove: []string{stateLabelA}}
			}),
			call: func(p ProviderCall) bool {
				return slices.Equal(p.RemoveLabels, []string{stateLabelA}) && len(p.AddLabels) == 0
			},
			holds:  func(s model.ProviderConversationState) bool { return !hasLabel(s, stateLabelA) },
			item:   func(i ItemState) bool { return !hasString(i.Labels, stateLabelA) },
			report: func(s *model.ProviderConversationState) { s.Labels = nil },
		}
		dest := &stateStep{
			name: "move-destination-label", cap: model.CapabilityMove,
			submit: moveSubmit(func(op model.OperationID, conv model.ConversationRef) model.MoveConversation {
				return model.MoveConversation{Command: command(op), Conversation: conv, Destination: model.MoveDestination{ID: stateLabelB}}
			}),
			call:  func(p ProviderCall) bool { return p.Destination.ID == stateLabelB },
			holds: func(s model.ProviderConversationState) bool { return hasLabel(s, stateLabelB) },
			item:  func(i ItemState) bool { return hasString(i.Labels, stateLabelB) },
			report: func(s *model.ProviderConversationState) {
				s.Labels = []model.ProviderLabel{{ID: stateLabelB, Name: stateLabelB}}
			},
		}
		out = append(out, add, remove, dest)
	}
	for _, role := range l.Roles {
		s := &stateStep{
			name: "move-" + role, cap: model.CapabilityMove,
			submit: moveSubmit(func(op model.OperationID, conv model.ConversationRef) model.MoveConversation {
				return model.MoveConversation{Command: command(op), Conversation: conv, Destination: model.MoveDestination{Role: role}}
			}),
			call: func(p ProviderCall) bool { return p.Destination.Role == role },
		}
		if l.Shape == model.MoveShapeFolder {
			s.item = func(i ItemState) bool { return i.Folder != nil && i.Folder.Role == role }
		}
		out = append(out, s)
	}
	return out
}

// firstSteps lists the steps that need nothing applied first: the cases
// that count a verb's provider calls run those.
func (e *adapterEnv) firstSteps(t *testing.T) []*stateStep {
	t.Helper()
	var out []*stateStep
	for _, s := range e.stateSteps(t) {
		if s.before == nil {
			out = append(out, s)
		}
	}
	return out
}

// eachStep runs fn in a fresh environment for each of steps.
func (e *adapterEnv) eachStep(t *testing.T, steps []*stateStep, fn func(*testing.T, *adapterEnv, *stateStep)) {
	t.Helper()
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) { fn(t, newAdapterEnv(t, e.h), s) })
	}
}

// stateTarget returns the conversation the state cases change, with the
// worker started and idle.
func (e *adapterEnv) stateTarget(t *testing.T) mutationTarget {
	t.Helper()
	if !e.declares(model.CapabilitySend) && !e.canReceive() {
		t.Skip("no conversation to change: capabilities send and receive not declared")
	}
	return e.anyTarget(t)
}

func (e *adapterEnv) stateProvider() (StateProvider, bool) {
	sp, ok := e.provider.(StateProvider)
	return sp, ok
}

// itemRef is the provider message reference the target's stored message
// is held under now: its first, the one a state request names.
func (e *adapterEnv) itemRef(t *testing.T, g mutationTarget) model.ExternalRef {
	t.Helper()
	id, ok := providerMessageID(e.storedMessage(t, g.msg.Ref), adapterChanA)
	if !ok {
		t.Fatalf("message %s holds no provider message reference", g.msg.Ref.ID)
	}
	return model.ProviderMessageRef(adapterChanA, id)
}

// channelState is the stored conversation's provider state on channel A.
func channelState(c model.Conversation) (model.ProviderConversationState, bool) {
	for _, s := range c.ProviderState {
		if s.Channel == adapterChanA {
			return s, true
		}
	}
	return model.ProviderConversationState{}, false
}

// prepare applies the steps s needs first, in order, each to the settled
// provider.
func (e *adapterEnv) prepare(t *testing.T, g mutationTarget, s *stateStep) {
	t.Helper()
	if s.before == nil {
		return
	}
	e.prepare(t, g, s.before)
	e.applyStep(t, g, s.before, model.OperationID("op-prepare-"+s.before.name))
}

// applyStep submits s and runs the worker until the operation is applied.
func (e *adapterEnv) applyStep(t *testing.T, g mutationTarget, s *stateStep, id model.OperationID) model.OperationRef {
	t.Helper()
	a, err := s.submit(e, g.conv.Ref, id)
	if err != nil {
		t.Fatalf("%s: %v", s.name, err)
	}
	e.later(t, 0)
	wantActApplied(t, e.op(t, a.Operation.Ref))
	return a.Operation.Ref
}

// wantHolds requires the stored conversation's provider state to show s,
// or not.
func (e *adapterEnv) wantHolds(t *testing.T, g mutationTarget, s *stateStep, want bool) model.Conversation {
	t.Helper()
	c := e.storedConversation(t, g.conv.Ref)
	if s.holds == nil {
		return c
	}
	st, _ := channelState(c)
	if s.holds(st) != want {
		t.Fatalf("%s: stored provider state %+v, shows the change = %v, want %v", s.name, st, !want, want)
	}
	return c
}

// wantItem requires the provider's own item to show s, when the provider
// can be asked.
func (e *adapterEnv) wantItem(t *testing.T, g mutationTarget, s *stateStep) {
	t.Helper()
	sp, ok := e.stateProvider()
	if !ok || s.item == nil {
		return
	}
	ref := e.itemRef(t, g)
	it, held := sp.ItemState(adapterChanA, ref)
	if !held {
		t.Fatalf("%s: the provider holds no item under the reference %v the message holds", s.name, ref)
	}
	if !s.item(it) {
		t.Fatalf("%s: provider item %+v does not show the change", s.name, it)
	}
}

// stateChange is a conversation.changed event reporting provider_state.
type stateChange struct {
	payload model.ConversationChangedPayload
	event   model.Event
}

// stateChanges returns the conversation.changed events that report
// provider_state, in order.
func (e *adapterEnv) stateChanges(t *testing.T) []stateChange {
	t.Helper()
	var out []stateChange
	e.view(t, func(r store.Reader) error {
		var after model.Cursor
		for {
			p, err := r.Events(e.ctx, model.Filter{Kinds: []string{model.EventConversationChanged}}, after, 100)
			if err != nil {
				return err
			}
			for _, ev := range p.Items {
				var pl model.ConversationChangedPayload
				if err := json.Unmarshal(ev.Payload.Data, &pl); err != nil {
					return err
				}
				if slices.Contains(pl.Changes, model.ConversationFieldProviderState) {
					out = append(out, stateChange{pl, ev})
				}
			}
			if p.Complete {
				return nil
			}
			after = p.Next
		}
	})
	return out
}

// stateEffect is the queued effect of s on the target: the conversation as
// it is now is the one an unapplied change leaves.
func (e *adapterEnv) stateEffect(g mutationTarget, s *stateStep) queuedEffect {
	var base model.Conversation
	var old model.ExternalRef
	return queuedEffect{
		capability: s.cap,
		submit: func(t *testing.T, id model.OperationID) model.OperationRef {
			t.Helper()
			base, old = e.storedConversation(t, g.conv.Ref), e.itemRef(t, g)
			a, err := s.submit(e, g.conv.Ref, id)
			if err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			if a.Operation.State != model.Queued {
				t.Fatalf("%s state = %q, want queued", s.name, a.Operation.State)
			}
			return a.Operation.Ref
		},
		untouched: func(t *testing.T) {
			t.Helper()
			if got := e.wantHolds(t, g, s, false); got.Revision != base.Revision {
				t.Fatalf("conversation revision %q, want it untouched at %q", got.Revision, base.Revision)
			}
		},
		applied: func(t *testing.T) {
			t.Helper()
			e.wantHolds(t, g, s, true)
			e.wantItem(t, g, s)
			if s.cap == model.CapabilityMove {
				e.wantReferencesResolved(t, g, old)
			}
		},
	}
}

// operationKind is the kind (the policy action) the operation was claimed
// under.
func (e *adapterEnv) operationKind(t *testing.T, ref model.OperationRef) string {
	t.Helper()
	var kind string
	e.view(t, func(r store.Reader) error {
		rec, err := r.Operation(e.ctx, ref)
		kind = rec.Key.Kind
		return err
	})
	return kind
}
