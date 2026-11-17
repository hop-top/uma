package fakeprovider

import (
	"context"
	"fmt"
	"slices"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Type keys of the state variants.
const (
	TypeStateLabels        model.TypeKey = "org.example.fake.labels"
	TypeStateFolders       model.TypeKey = "org.example.fake.folders"
	TypeStateFoldersStable model.TypeKey = "org.example.fake.folders.stable"
)

var stateCaps = []model.Capability{
	model.CapabilityReceive, model.CapabilitySend, model.CapabilityReconcile,
	model.CapabilityArchive, model.CapabilityMove, model.CapabilityReadState, model.CapabilityFlag, model.CapabilityTrash,
}

func stateVariant(typ model.TypeKey, shape model.MoveShape, rename bool) variant {
	return variant{
		typ: typ, exchange: providerIDs, caps: stateCaps,
		events: []string{model.EventMessageReceived, model.EventDeliveryUpdated},
		shape:  shape, rename: rename,
		open: func(i *instance) adapter.Instance { return &State{i: i} },
	}
}

// NewStateLabels returns the factory of the labels variant: a mailbox that
// labels its items, as Gmail does. A move adds and removes labels, and
// references never change.
func NewStateLabels() *Factory {
	return newFactory(stateVariant(TypeStateLabels, model.MoveShapeLabels, false))
}

// NewStateFolders returns the factory of the folders variant: a mailbox
// that files each item in one folder and, on a move, gives it the reference
// of the destination, as IMAP does. The old reference names nothing after
// it.
func NewStateFolders() *Factory {
	return newFactory(stateVariant(TypeStateFolders, model.MoveShapeFolder, true))
}

// NewStateFoldersStable returns the factory of the stable folders variant:
// folders whose items keep their reference when moved, as Graph's
// immutable IDs do.
func NewStateFoldersStable() *Factory {
	return newFactory(stateVariant(TypeStateFoldersStable, model.MoveShapeFolder, false))
}

// Item is what the provider holds for one message: Folder for a variant
// that files its items, Labels for one that labels them. Archive and trash
// are flags here: they never move an item.
type Item struct {
	Archived bool
	Trashed  bool
	Read     bool
	Folder   *model.Folder
	Labels   []string
	Flags    []string
}

func (it Item) clone() Item {
	if it.Folder != nil {
		f := *it.Folder
		it.Folder = &f
	}
	it.Labels, it.Flags = slices.Clone(it.Labels), slices.Clone(it.Flags)
	return it
}

// Special-use folders of the folder variants, by role.
var roleFolders = map[string]string{
	model.RoleInbox: "INBOX", model.RoleArchive: "Archive", model.RoleTrash: "Trash", model.RoleJunk: "Junk",
}

func folderOf(id string) *model.Folder {
	f := &model.Folder{ID: id, Name: id}
	for role, rid := range roleFolders {
		if rid == id {
			f.Role = role
		}
	}
	return f
}

// stateLimits are the limits a variant of the given shape reports for c,
// empty for a capability that is no state verb: every state verb changes
// the account's own view; the folder variants resolve every role.
func stateLimits(c model.Capability, shape model.MoveShape) model.Extension {
	l := model.StateLimits{Scope: model.ScopeAccount}
	switch c {
	case model.CapabilityMove:
		l.Shape = shape
		if shape == model.MoveShapeFolder {
			l.Roles = []string{model.RoleInbox, model.RoleArchive, model.RoleTrash, model.RoleJunk}
		}
	case model.CapabilityFlag:
		l.Kinds = []string{model.FlagStar, model.FlagImportant}
	case model.CapabilityArchive, model.CapabilityReadState, model.CapabilityTrash:
	default:
		return model.Extension{}
	}
	return l.Extension()
}

// hold makes the provider hold an item under id, in the inbox of a folder
// variant. The caller holds s.mu.
func (s *Script) hold(id string) {
	if s.items == nil {
		s.items = map[string]*Item{}
	}
	if _, ok := s.items[id]; ok {
		return
	}
	it := &Item{}
	if s.shape == model.MoveShapeFolder {
		it.Folder = folderOf(roleFolders[model.RoleInbox])
	}
	s.items[id] = it
}

// Item returns what the provider holds under the provider message ID id,
// and false when it holds none: an item a folder move renamed is held under
// its new ID only.
func (s *Script) Item(id string) (Item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok {
		return Item{}, false
	}
	return it.clone(), true
}

// applyState takes the next scripted result for a state call, records it
// and, when the provider applied it, changes the items it names. A call
// naming an item the provider does not hold, such as one under a reference
// a move retired, is refused as not found and changes nothing.
func (s *Script) applyState(call Call) (adapter.Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.fallback
	if len(s.results) > 0 {
		r = s.results[0]
		s.results = s.results[1:]
	}
	call.Result = r
	applied := r == Accepted || r == OutcomeUnknown
	if applied && !s.holdsAll(call.Items) {
		call.Result = Rejected
		s.calls = append(s.calls, cloneCall(call))
		return adapter.Outcome{State: model.Rejected, Failure: model.NewError(model.NotFound, "no such item")}, nil
	}
	if applied {
		call.Applied = true
		call.Changes = s.change(call)
	}
	s.calls = append(s.calls, cloneCall(call))
	out, err := outcomeOf(r, nil, s.after)
	if err == nil && out.State == model.ProviderAccepted {
		out.ReferenceChanges = slices.Clone(call.Changes)
	}
	return out, err
}

func (s *Script) holdsAll(refs []model.ExternalRef) bool {
	if len(refs) == 0 {
		return false
	}
	for _, ref := range refs {
		if _, ok := s.items[ref.ID]; !ok {
			return false
		}
	}
	return true
}

// change applies call to its items and returns the reference changes a
// move made. The caller holds s.mu.
func (s *Script) change(call Call) (changes []adapter.ReferenceChange) {
	for _, ref := range call.Items {
		it := s.items[ref.ID]
		switch call.Method {
		case MethodArchive:
			it.Archived = !call.Undo
		case MethodTrash:
			it.Trashed = !call.Undo
		case MethodReadState:
			it.Read = call.Read
		case MethodFlag:
			it.Flags = slices.DeleteFunc(it.Flags, func(k string) bool { return k == call.Kind })
			if !call.Undo {
				it.Flags = append(it.Flags, call.Kind)
			}
		case MethodMove:
			if c, ok := s.move(ref, it, call); ok {
				changes = append(changes, c)
			}
		}
	}
	return changes
}

// move moves one item. A variant that renames gives the item the reference
// of its place in the destination, even when that is the folder it is in (a
// copy and an expunge), and forgets the old one.
func (s *Script) move(ref model.ExternalRef, it *Item, call Call) (adapter.ReferenceChange, bool) {
	if s.shape != model.MoveShapeFolder {
		add := slices.Clone(call.Add)
		if call.Destination.ID != "" {
			add = append(add, call.Destination.ID)
		}
		for _, id := range call.Drop {
			it.Labels = slices.DeleteFunc(it.Labels, func(l string) bool { return l == id })
		}
		for _, id := range add {
			if !slices.Contains(it.Labels, id) {
				it.Labels = append(it.Labels, id)
			}
		}
		return adapter.ReferenceChange{}, false
	}
	dest := call.Destination.ID
	if dest == "" {
		dest = roleFolders[call.Destination.Role]
	}
	it.Folder = folderOf(dest)
	if !s.rename {
		return adapter.ReferenceChange{}, false
	}
	s.moves++
	renamed := fmt.Sprintf("%s:%d", dest, s.moves)
	s.items[renamed] = it
	delete(s.items, ref.ID)
	return adapter.ReferenceChange{Old: ref, New: model.ProviderMessageRef(s.channel, renamed)}, true
}

// reconcileState reports what the provider recorded for the state
// operation of e. A move is not idempotent, so the provider is asked where
// the items are: a recorded move whose items are in the destination is
// applied, with the references it gave them; the call alone does not show it.
func (s *Script) reconcileState(e adapter.Effect, items []model.ExternalRef, dest model.MoveDestination) adapter.Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := adapter.Outcome{State: model.Failed, Failure: model.NewError(model.NotFound, "provider has no record of the effect")}
	for _, c := range slices.Backward(s.calls) {
		if c.Method == MethodReconcile || c.Effect.Operation != e.Operation {
			continue
		}
		switch {
		case c.Applied && c.Method == MethodMove && s.shape == model.MoveShapeFolder:
			if s.inDestination(items, c.Changes, dest) {
				out = adapter.Outcome{State: model.ProviderAccepted, ReferenceChanges: slices.Clone(c.Changes)}
			}
		case c.Applied:
			out = adapter.Outcome{State: model.ProviderAccepted}
		case c.Result == Rejected || c.Result == Unauthorized:
			out, _ = outcomeOf(c.Result, nil, 0)
		}
		break
	}
	s.calls = append(s.calls, Call{Method: MethodReconcile, Effect: cloneEffect(e)})
	return out
}

// inDestination reports whether every item, held under its reference or
// the one a move gave it, sits in the destination folder.
func (s *Script) inDestination(items []model.ExternalRef, changes []adapter.ReferenceChange, dest model.MoveDestination) bool {
	id := dest.ID
	if id == "" {
		id = roleFolders[dest.Role]
	}
	for _, ref := range items {
		for _, c := range changes {
			if c.Old == ref {
				ref = c.New
			}
		}
		it, ok := s.items[ref.ID]
		if !ok || it.Folder == nil || it.Folder.ID != id {
			return false
		}
	}
	return len(items) > 0
}

// State is the state variants' instance: receive, send, reconcile and the
// five provider state verbs over the script's items. Every verb takes the
// next scripted result like a send. A request names the conversation's
// items and no target.
type State struct{ i *instance }

// Capabilities reports every declared capability, the state verbs with the
// limits of their variant.
func (x *State) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	return x.i.capabilities(ctx)
}

// Close closes the instance; it is idempotent.
func (x *State) Close(context.Context) error { return x.i.close() }

// Run delivers the script's queued ingress to sink.
func (x *State) Run(ctx context.Context, sink adapter.Sink) error { return x.i.run(ctx, sink) }

// Send sends a new message under the next scripted result.
func (x *State) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	return x.i.send(ctx, r)
}

// Reconcile reports what the provider recorded for the effect's operation;
// for a state operation it looks at the items themselves.
func (x *State) Reconcile(ctx context.Context, r adapter.ReconcileRequest) (adapter.Outcome, error) {
	if !slices.Contains(stateCaps[3:], r.Capability) {
		return x.i.reconcile(ctx, r.Effect)
	}
	i := x.i
	if err := i.ready(ctx, model.CapabilityReconcile); err != nil {
		return adapter.Outcome{}, err
	}
	if r.Route.Channel != i.channel.Ref {
		return adapter.Outcome{}, model.NewError(model.Validation, "route belongs to another channel")
	}
	if err := i.authenticate(ctx); err != nil {
		return adapter.Outcome{}, err
	}
	return i.script.reconcileState(r.Effect, r.Items, r.Destination), nil
}

func (x *State) state(ctx context.Context, c model.Capability, call Call) (adapter.Outcome, error) {
	i := x.i
	if err := i.ready(ctx, c); err != nil {
		return adapter.Outcome{}, err
	}
	if err := i.authenticate(ctx); err != nil {
		return adapter.Outcome{}, err
	}
	if call.Effect.Route.Channel != i.channel.Ref || call.Effect.Target != nil {
		return adapter.Outcome{}, model.NewError(model.Validation, "a state request names the channel's conversation and no target")
	}
	call.Effect = cloneEffect(call.Effect)
	return i.script.applyState(call)
}

// Archive archives the items, or unarchives them with Undo.
func (x *State) Archive(ctx context.Context, r adapter.ArchiveRequest) (adapter.Outcome, error) {
	return x.state(ctx, model.CapabilityArchive, Call{Method: MethodArchive, Effect: r.Effect, Items: r.Items, Undo: r.Undo})
}

// Move moves the items to the destination of a folder variant, or changes
// their labels in a label variant, a destination naming a label to add.
func (x *State) Move(ctx context.Context, r adapter.MoveRequest) (adapter.Outcome, error) {
	return x.state(ctx, model.CapabilityMove, Call{Method: MethodMove, Effect: r.Effect, Items: r.Items, Destination: r.Destination, Add: r.Add, Drop: r.Remove})
}

// SetReadState marks the items read or unread.
func (x *State) SetReadState(ctx context.Context, r adapter.ReadStateRequest) (adapter.Outcome, error) {
	return x.state(ctx, model.CapabilityReadState, Call{Method: MethodReadState, Effect: r.Effect, Items: r.Items, Read: r.Read})
}

// Flag sets or clears a flag on the items.
func (x *State) Flag(ctx context.Context, r adapter.FlagRequest) (adapter.Outcome, error) {
	return x.state(ctx, model.CapabilityFlag, Call{Method: MethodFlag, Effect: r.Effect, Items: r.Items, Kind: r.Kind, Undo: r.Undo})
}

// Trash trashes the items, or restores them with Undo.
func (x *State) Trash(ctx context.Context, r adapter.TrashRequest) (adapter.Outcome, error) {
	return x.state(ctx, model.CapabilityTrash, Call{Method: MethodTrash, Effect: r.Effect, Items: r.Items, Undo: r.Undo})
}

var (
	_ adapter.Receiver        = (*State)(nil)
	_ adapter.Sender          = (*State)(nil)
	_ adapter.Reconciler      = (*State)(nil)
	_ adapter.Archiver        = (*State)(nil)
	_ adapter.Mover           = (*State)(nil)
	_ adapter.ReadStateSetter = (*State)(nil)
	_ adapter.Flagger         = (*State)(nil)
	_ adapter.Trasher         = (*State)(nil)
)
