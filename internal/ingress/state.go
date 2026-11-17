package ingress

import (
	"slices"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Provider conversation state. A conversation.state envelope carries what
// the provider holds about one conversation on the bound channel (see
// model.ProviderStateReport). Ingress resolves the conversation, merges the
// report into Conversation.ProviderState and emits conversation.changed
// provider_state, provider-triggered, under a new revision. It never writes
// the conversation's local status, labels or read markers: provider state
// stays apart from them, and a provider read state never becomes a per-actor
// read mark.
//
// A report about a conversation the inbox does not hold is accepted and
// recorded as nothing: the ingress record is its only trace, and no
// conversation, message, identity or event follows from it.
//
// Reports are ordered by their observation time. A report observed before,
// or when, the held state was observed is stale and ignored: the held state
// is either newer or the settle of uma's own change (which stamps its own
// time), and an older view never undoes it. A report that changes nothing
// writes nothing and emits nothing, so a redelivery is harmless.
//
// A report over every item of the conversation (Complete) replaces the
// held view. A partial report, such as one message of a thread, overlays
// the fields it names on the held view and leaves it incomplete: its flags
// and booleans apply, a read state or folder it does not carry is kept. A
// partial report that leaves the held values as they are keeps a complete
// view complete.
//
// A report that shows what an unsettled state operation asked is also the
// echo of that operation: it is recorded as provider evidence for it (see
// stateEchoes) and the operation's worker settles from it, never sending
// the change again and never queueing a new operation.

// stateCapabilities are the capabilities of the provider state verbs; an
// adapter that declares none has no provider state to report.
var stateCapabilities = []model.Capability{
	model.CapabilityArchive, model.CapabilityMove, model.CapabilityReadState, model.CapabilityFlag, model.CapabilityTrash,
}

// checkConversationState validates a conversation.state envelope against
// the binding: the report with its event ID, the conversation on the bound
// channel, and no sender, content, references, message or payload of
// another kind. The adapter must declare a state capability.
func (s *Sink) checkConversationState(env model.Envelope) error {
	switch {
	case env.State == nil:
		return model.NewError(model.Validation, "envelope.state: required by a conversation.state envelope")
	case env.EventID == "":
		return model.NewError(model.Validation, "envelope.event_id: required by a conversation.state envelope")
	case env.MessageID != "":
		return model.NewError(model.Validation, "envelope.message_id: a conversation.state envelope names no message")
	case !env.Sender.IsZero() || env.SenderKind != "" || len(env.SenderAttributes) > 0:
		return model.NewError(model.Validation, "envelope.sender: a conversation.state envelope carries no sender")
	case len(env.Content.Parts) > 0:
		return model.NewError(model.Validation, "envelope.content: a conversation.state envelope carries no content")
	case len(env.References) > 0:
		return model.NewError(model.Validation, "envelope.references: a conversation.state envelope carries none")
	case env.Payload.Name != "" || env.Payload.Version != 0 || len(env.Payload.Data) > 0:
		return model.NewError(model.Validation, "envelope.payload: a conversation.state envelope carries none")
	case env.Task != nil:
		return model.NewError(model.Validation, "envelope.task: only a new message carries a task binding")
	case env.Event != nil || env.Interaction != nil || env.WorkItem != nil || env.Delivery != nil:
		return model.NewError(model.Validation, "envelope: a conversation.state envelope carries only a state")
	}
	if err := env.State.Validate(); err != nil {
		return err
	}
	if err := s.ownRef("envelope.state.conversation", env.State.Conversation); err != nil {
		return err
	}
	if !slices.ContainsFunc(stateCapabilities, func(c model.Capability) bool { return slices.Contains(s.desc.Capabilities, c) }) {
		return model.NewError(model.Unsupported, "envelope.kind: the adapter declares no provider state capability")
	}
	return nil
}

// applyConversationState applies the envelope's state report.
func (u *ingressUnit) applyConversationState() error {
	rep := u.env.State
	conv, ok, err := u.stateConversation(rep.Conversation)
	if err != nil || !ok {
		return err
	}
	ch := u.s.channel.Ref
	idx := slices.IndexFunc(conv.ProviderState, func(s model.ProviderConversationState) bool { return s.Channel == ch })
	held := model.ProviderConversationState{Channel: ch}
	if idx >= 0 {
		held = conv.ProviderState[idx].Clone()
		if !rep.State.ObservedAt.After(held.ObservedAt) {
			return nil
		}
	}
	next := mergeState(held, rep.State)
	if err := u.stateEchoes(conv, held, next); err != nil {
		return err
	}
	if idx >= 0 && sameStateValues(held, next) && (held.Complete || !next.Complete) {
		return nil
	}
	prev := conv.Revision
	updated := conv.Clone()
	if idx >= 0 {
		updated.ProviderState[idx] = next
	} else {
		updated.ProviderState = append(updated.ProviderState, next)
	}
	if updated.Revision, err = u.newRevision(); err != nil {
		return err
	}
	if err := u.tx.PutConversation(u.ctx, updated, prev); err != nil {
		return err
	}
	return u.providerStateChanged(updated, prev)
}

// scopedKind is the reference kind a conversation of an inbox in mode is
// scoped by, empty for a mode scoped by something else.
func scopedKind(mode string) string {
	return map[string]string{
		model.ModePerRoom:     model.RefKindRoom,
		model.ModePerThread:   model.RefKindThread,
		model.ModePerWorkItem: model.RefKindWorkItem,
	}[mode]
}

// stateConversation resolves the conversation the provider's reference
// names, within the inbox. In a room, thread or work item inbox a reference
// of the scoping kind selects the newest conversation of its candidate
// group; any other reference is one a stored message of the conversation
// carries, and the conversation of the newest such message is the one.
// ok is false for a conversation the inbox does not hold.
func (u *ingressUnit) stateConversation(ref model.ExternalRef) (c model.Conversation, ok bool, err error) {
	mode := u.inbox.Selection.Mode
	if kind := scopedKind(mode); kind != "" && ref.Kind == kind {
		key := store.SelectionKey{Inbox: u.inbox.Ref, Mode: mode, Key: kind + ":" + lengthPrefixed(ref.Namespace, ref.ParentID, ref.ID)}
		cands, err := u.tx.Candidates(u.ctx, key, candidateLimit)
		if err != nil || len(cands) == 0 {
			return model.Conversation{}, false, err
		}
		return cands[len(cands)-1], true, nil
	}
	msgs, err := u.tx.MessagesByReference(u.ctx, u.inbox.Ref, ref, candidateLimit)
	if err != nil || len(msgs) == 0 {
		return model.Conversation{}, false, err
	}
	c, err = u.tx.GetConversation(u.ctx, msgs[len(msgs)-1].Ref.Conversation)
	if model.IsCode(err, model.NotFound) {
		return model.Conversation{}, false, nil
	}
	return c, err == nil, err
}

// mergeState is the state held becomes under report in: a complete report
// replaces it, a partial one overlays the fields it names and is
// incomplete. Booleans and flags are always the report's; a read state or
// folder it does not carry is kept, and labels it does not carry are kept
// only beside a folder.
func mergeState(held, in model.ProviderConversationState) model.ProviderConversationState {
	next := in.Clone()
	next.Channel = held.Channel
	if in.Complete {
		return next
	}
	next.Complete = false
	if in.Read == nil {
		next.Read = held.Read
	}
	switch {
	case in.Folder != nil:
		next.Labels = nil
	case len(in.Labels) > 0:
		next.Folder = nil
	default:
		next.Folder, next.Labels = clonePtr(held.Folder), slices.Clone(held.Labels)
	}
	return next
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// sameStateValues reports whether a and b hold the same state, ignoring
// when it was observed and whether it is complete. Labels and flags are
// sets.
func sameStateValues(a, b model.ProviderConversationState) bool {
	sameLabels := len(a.Labels) == len(b.Labels) &&
		!slices.ContainsFunc(a.Labels, func(l model.ProviderLabel) bool { return !slices.Contains(b.Labels, l) })
	sameFlags := len(a.Flags) == len(b.Flags) &&
		!slices.ContainsFunc(a.Flags, func(f string) bool { return !slices.Contains(b.Flags, f) })
	sameRead := (a.Read == nil) == (b.Read == nil) && (a.Read == nil || *a.Read == *b.Read)
	sameFolder := (a.Folder == nil) == (b.Folder == nil) && (a.Folder == nil || *a.Folder == *b.Folder)
	return a.Archived == b.Archived && a.Trashed == b.Trashed && sameRead && sameFolder && sameLabels && sameFlags
}
