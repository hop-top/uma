package enginetest

import (
	"context"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// StateCall is one provider state request that reached the provider: the
// request fields of the verb the capability carries.
type StateCall struct {
	Items []model.ExternalRef
	// Targeted: Items are the messages the caller chose.
	Targeted    bool
	Undo        bool
	Read        bool
	Kind        string
	Destination model.MoveDestination
	Add, Remove []string
}

// stateLimitsFunc returns the limits a state capability reports; the zero
// Extension reports none.
type stateLimitsFunc func(model.Capability) model.Extension

// StateCapabilities are the five provider state capabilities.
var StateCapabilities = []model.Capability{
	model.CapabilityArchive, model.CapabilityMove, model.CapabilityReadState, model.CapabilityFlag, model.CapabilityTrash,
}

// StateLimitsOf returns the limits a rig adapter of the given scope reports
// for c: a folder shape and every role for move, every kind for flag.
func StateLimitsOf(scope model.StateScope) func(model.Capability) model.Extension {
	return func(c model.Capability) model.Extension {
		l := model.StateLimits{Scope: scope}
		switch c {
		case model.CapabilityMove:
			l.Shape = model.MoveShapeFolder
			l.Roles = []string{model.RoleInbox, model.RoleArchive, model.RoleTrash, model.RoleJunk}
		case model.CapabilityFlag:
			l.Kinds = []string{model.FlagStar, model.FlagImportant}
		}
		return l.Extension()
	}
}

// WithState declares caps (every state capability when none) reporting
// limits of scope.
func WithState(scope model.StateScope, caps ...model.Capability) RigOption {
	if len(caps) == 0 {
		caps = StateCapabilities
	}
	return WithStateLimits(StateLimitsOf(scope), caps...)
}

// WithStateLimits declares caps reporting the limits limits returns.
func WithStateLimits(limits func(model.Capability) model.Extension, caps ...model.Capability) RigOption {
	return func(_ *engine.Config, f *OutFactory) {
		f.State, f.StateCaps, f.StateLimits = true, caps, limits
	}
}

// Archive answers like a send, consuming the same script.
func (i *OutInstance) Archive(ctx context.Context, r adapter.ArchiveRequest) (adapter.Outcome, error) {
	return i.state(ctx, model.CapabilityArchive, r.Effect, StateCall{Items: r.Items, Targeted: r.Targeted, Undo: r.Undo})
}

// Move answers like a send, consuming the same script.
func (i *OutInstance) Move(ctx context.Context, r adapter.MoveRequest) (adapter.Outcome, error) {
	return i.state(ctx, model.CapabilityMove, r.Effect, StateCall{Items: r.Items, Targeted: r.Targeted, Destination: r.Destination, Add: r.Add, Remove: r.Remove})
}

// SetReadState answers like a send, consuming the same script.
func (i *OutInstance) SetReadState(ctx context.Context, r adapter.ReadStateRequest) (adapter.Outcome, error) {
	return i.state(ctx, model.CapabilityReadState, r.Effect, StateCall{Items: r.Items, Targeted: r.Targeted, Read: r.Read})
}

// Flag answers like a send, consuming the same script.
func (i *OutInstance) Flag(ctx context.Context, r adapter.FlagRequest) (adapter.Outcome, error) {
	return i.state(ctx, model.CapabilityFlag, r.Effect, StateCall{Items: r.Items, Targeted: r.Targeted, Kind: r.Kind, Undo: r.Undo})
}

// Trash answers like a send, consuming the same script.
func (i *OutInstance) Trash(ctx context.Context, r adapter.TrashRequest) (adapter.Outcome, error) {
	return i.state(ctx, model.CapabilityTrash, r.Effect, StateCall{Items: r.Items, Targeted: r.Targeted, Undo: r.Undo})
}

func (i *OutInstance) state(ctx context.Context, c model.Capability, e adapter.Effect, s StateCall) (adapter.Outcome, error) {
	return i.mutate(ctx, MutationCall{Capability: c, Effect: e, State: &s})
}

// SeedProviderMessages stores n inbound public messages of the rig's
// conversation holding provider messages "pm-item-1" ... on the rig's
// channel, and returns those provider references.
func (r *Rig) SeedProviderMessages(n int) []model.ExternalRef {
	r.T.Helper()
	var refs []model.ExternalRef
	for k := 1; k <= n; k++ {
		refs = append(refs, r.SeedProviderMessage(k))
	}
	return refs
}

// SeedProviderMessage stores the k-th message of SeedProviderMessages (1-based)
// alone and returns its provider reference.
func (r *Rig) SeedProviderMessage(k int) model.ExternalRef {
	r.T.Helper()
	ref := model.ProviderMessageRef(r.Ch.Ref, "pm-item-"+string(rune('0'+k)))
	m := model.Message{
		Ref:      model.MessageRef{Conversation: r.Conv.Ref, ID: SeededMessageID(k)},
		Audience: model.Audience{Visibility: model.Public},
		Content:  model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "item"}}},
		External: []model.ExternalRef{ref},
		Source:   OutKey, OccurredAt: r.Clock.Now().UTC(), RecordedAt: r.Clock.Now().UTC(), Revision: "m1",
	}
	err := r.E.Store().Transact(r.Ctx, TenantA, func(tx store.Tx) error { return tx.PutMessage(r.Ctx, m, "") })
	if err != nil {
		r.T.Fatalf("seed message: %v", err)
	}
	return ref
}

// SeededMessageID is the ID of the k-th message SeedProviderMessages stores
// (1-based).
func SeededMessageID(k int) model.ID {
	return model.ID("00000005-0000-7000-8000-00000000010" + string(rune('0'+k)))
}

// StoredConversation reads the rig's conversation.
func (r *Rig) StoredConversation() model.Conversation {
	r.T.Helper()
	var c model.Conversation
	err := r.E.Store().View(r.Ctx, TenantA, func(rd store.Reader) (err error) {
		c, err = rd.GetConversation(r.Ctx, r.Conv.Ref)
		return err
	})
	if err != nil {
		r.T.Fatal(err)
	}
	return c
}

// SetProviderState rewrites the rig's conversation with state as the
// channel's provider state, as an inbound report would.
func (r *Rig) SetProviderState(fn func(*model.ProviderConversationState)) model.Conversation {
	r.T.Helper()
	var out model.Conversation
	err := r.E.Store().Transact(r.Ctx, TenantA, func(tx store.Tx) error {
		c, err := tx.GetConversation(r.Ctx, r.Conv.Ref)
		if err != nil {
			return err
		}
		prev := c.Revision
		st := model.ProviderConversationState{Channel: r.Ch.Ref}
		idx := -1
		for k, s := range c.ProviderState {
			if s.Channel == r.Ch.Ref {
				st, idx = s.Clone(), k
			}
		}
		fn(&st)
		st.ObservedAt = r.Clock.Now().UTC()
		if idx < 0 {
			c.ProviderState = append(c.ProviderState, st)
		} else {
			c.ProviderState[idx] = st
		}
		if c.Revision, err = operations.NewRevision(r.Ctx, r.E); err != nil {
			return err
		}
		out = c
		return tx.PutConversation(r.Ctx, c, prev)
	})
	if err != nil {
		r.T.Fatalf("set provider state: %v", err)
	}
	return out
}

// ProviderStateOf returns the rig channel's stored provider state of the
// rig's conversation, false when it holds none.
func (r *Rig) ProviderStateOf() (model.ProviderConversationState, bool) {
	r.T.Helper()
	for _, s := range r.StoredConversation().ProviderState {
		if s.Channel == r.Ch.Ref {
			return s, true
		}
	}
	return model.ProviderConversationState{}, false
}

// ArchiveReq is an archive request of the rig's conversation.
func (r *Rig) ArchiveReq(opID model.OperationID, undo bool) model.ArchiveConversation {
	return model.ArchiveConversation{
		Command:      model.Command{Call: model.Call{Correlation: "corr-state"}, Operation: opID},
		Conversation: r.Conv.Ref, Undo: undo,
	}
}

// Archive submits archiving, or unarchiving, the rig's conversation.
func (r *Rig) Archive(opID model.OperationID, undo bool) (model.Acceptance, error) {
	return r.C.Conversations().Archive(r.Ctx, r.ArchiveReq(opID, undo))
}

// Move submits moving the rig's conversation to dest.
func (r *Rig) Move(opID model.OperationID, dest model.MoveDestination) (model.Acceptance, error) {
	return r.C.Conversations().Move(r.Ctx, model.MoveConversation{
		Command:      model.Command{Call: model.Call{Correlation: "corr-state"}, Operation: opID},
		Conversation: r.Conv.Ref, Destination: dest,
	})
}

// SetRead submits marking the rig's conversation read, or unread.
func (r *Rig) SetRead(opID model.OperationID, read bool) (model.Acceptance, error) {
	return r.C.Conversations().SetRead(r.Ctx, model.SetConversationRead{
		Command:      model.Command{Call: model.Call{Correlation: "corr-state"}, Operation: opID},
		Conversation: r.Conv.Ref, Read: read,
	})
}

// Flag submits flagging, or unflagging, the rig's conversation.
func (r *Rig) Flag(opID model.OperationID, kind string, undo bool) (model.Acceptance, error) {
	return r.C.Conversations().Flag(r.Ctx, model.FlagConversation{
		Command:      model.Command{Call: model.Call{Correlation: "corr-state"}, Operation: opID},
		Conversation: r.Conv.Ref, Kind: kind, Undo: undo,
	})
}

// Trash submits trashing, or restoring, the rig's conversation.
func (r *Rig) Trash(opID model.OperationID, undo bool) (model.Acceptance, error) {
	return r.C.Conversations().Trash(r.Ctx, model.TrashConversation{
		Command:      model.Command{Call: model.Call{Correlation: "corr-state"}, Operation: opID},
		Conversation: r.Conv.Ref, Undo: undo,
	})
}

// StoredMessages reads the messages of the rig's conversation.
func (r *Rig) StoredMessages() []model.Message {
	r.T.Helper()
	var out []model.Message
	err := r.E.Store().View(r.Ctx, TenantA, func(rd store.Reader) error {
		p, err := rd.Messages(r.Ctx, model.ListRequest{
			Filter: model.Filter{Conversation: &r.Conv.Ref},
			Page:   model.PageRequest{Limit: 100},
		})
		out = p.Items
		return err
	})
	if err != nil {
		r.T.Fatal(err)
	}
	return out
}
