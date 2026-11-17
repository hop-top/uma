package operations

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Provider conversation state: Conversations.Archive, Move, SetRead, Flag
// and Trash change what the provider holds about a conversation through
// the operation ledger, like a membership change. Submission claims the
// operation key (kind the policy action of the verb's reported scope,
// target the conversation) and queues the work; it never writes the
// conversation. The conversation's provider state changes only once the
// provider applied the effect, and not when the same part of that state
// advanced since the claim (a newer inbound report wins).

// IntentStatePrefix starts the stored intent name of every provider state
// operation; the capability follows (StateIntentName).
const IntentStatePrefix = "uma.intent.conversation_state."

// StateIntentName names the stored intent of a state operation carried by
// capability c.
func StateIntentName(c model.Capability) string { return IntentStatePrefix + string(c) }

// stateAudience is the audience of a state operation's events and effect:
// provider state is desk business, never public content.
var stateAudience = model.Audience{Visibility: model.Internal}

// StateAudience returns the audience of provider state operations.
func StateAudience() model.Audience { return stateAudience }

// StateBase is the channel's provider state as the claiming unit read it,
// when the conversation held any: what the settle compares against to find
// that a newer inbound report moved the part the verb changes.
type StateBase struct {
	Held  bool                            `json:"held"`
	State model.ProviderConversationState `json:"state"`
}

// StateIntent is the durable intent of a provider state operation: the
// conversation and its channel, the verb and what it names, the scope and
// move shape the adapter reported at the call, and the base snapshot.
type StateIntent struct {
	Conversation model.ConversationRef `json:"conversation"`
	Channel      model.ChannelRef      `json:"channel"`
	Verb         model.StateVerb       `json:"verb"`
	Scope        model.StateScope      `json:"scope"`
	Shape        model.MoveShape       `json:"shape"`
	Destination  model.MoveDestination `json:"destination"`
	Add          []string              `json:"add,omitempty"`
	Remove       []string              `json:"remove,omitempty"`
	Kind         string                `json:"kind"`
	Base         StateBase             `json:"base"`
}

// StateRequest is one state command ready for the ledger, with the scope
// and move shape the adapter reported for its verb.
type StateRequest struct {
	Command      model.Command
	Conversation model.ConversationRef
	Verb         model.StateVerb
	Scope        model.StateScope
	Shape        model.MoveShape
	Destination  model.MoveDestination
	Add, Remove  []string
	Kind         string
}

// CheckLimits refuses, with model.Unsupported, a request the limits the
// adapter reported for its capability cannot carry: a move that names
// label changes to a folder-shaped provider or a role it does not resolve,
// and a flag of a kind it does not report. A move needs the shape, a flag
// the kinds: limits without them are read as no support.
func (r StateRequest) CheckLimits(l model.StateLimits) error {
	switch r.Verb {
	case model.StateVerbMove:
		if !l.Shape.Valid() {
			return model.NewError(model.Unsupported, "move: adapter reported no shape")
		}
		if l.Shape == model.MoveShapeFolder && len(r.Add)+len(r.Remove) > 0 {
			return model.NewError(model.Unsupported, "move: a folder-shaped provider takes a destination only")
		}
		if r.Destination.Role != "" && !slices.Contains(l.Roles, r.Destination.Role) {
			return model.NewError(model.Unsupported, "move: adapter does not resolve role "+r.Destination.Role)
		}
	case model.StateVerbFlag, model.StateVerbUnflag:
		if !slices.Contains(l.Kinds, r.Kind) {
			return model.NewError(model.Unsupported, "flag: adapter does not support kind "+r.Kind)
		}
	}
	return nil
}

// ChangeState queues the state change req names, under its verb's
// capability (the caller has checked the adapter declares and reports it)
// and the policy action of req.Scope on the conversation. The conversation
// must be stored in the caller's tenant (else model.NotFound); in the
// claiming unit a set ExpectedRevision must be the conversation's
// (model.Conflict).
// Nothing is written to the conversation.
func ChangeState(ctx context.Context, c *engine.Client, req StateRequest) (model.Acceptance, error) {
	if c.Engine().IsClosed() {
		return model.Acceptance{}, engine.ErrClosed()
	}
	action, ok := req.Scope.Action()
	capability, known := req.Verb.Capability()
	if !ok || !known {
		return model.Acceptance{}, model.NewError(model.Unsupported, "state: undeclared verb or scope")
	}
	if req.Conversation.Inbox.Tenant != c.Principal().Tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	var channel model.ChannelRef
	err := c.Engine().Store().View(ctx, c.Principal().Tenant, func(r store.Reader) error {
		conv, err := r.GetConversation(ctx, req.Conversation)
		if err != nil {
			return err
		}
		if conv.Ref != req.Conversation {
			return engine.ErrNotFound()
		}
		inbox, err := r.GetInbox(ctx, conv.Ref.Inbox)
		channel = inbox.Channel
		return err
	})
	if model.IsCode(err, model.NotFound) {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	if err != nil {
		return model.Acceptance{}, err
	}
	in := StateIntent{
		Conversation: req.Conversation, Channel: channel, Verb: req.Verb, Scope: req.Scope, Shape: req.Shape,
		Destination: req.Destination, Add: slices.Clone(req.Add), Remove: slices.Clone(req.Remove),
		Kind: req.Kind,
	}
	res := req.Conversation.Ref()
	return submit(ctx, c, submission{
		kind:        action,
		target:      res,
		owner:       res,
		command:     req.Command,
		audience:    stateAudience,
		fingerprint: stateFingerprint(action, req),
		workKind:    WorkKindOperation,
		build: func(ctx context.Context, tx store.Tx) (model.Extension, error) {
			conv, err := tx.GetConversation(ctx, req.Conversation)
			switch {
			case err == nil && conv.Ref == req.Conversation:
				in.Base = baseState(conv, channel)
			case err != nil && !model.IsCode(err, model.NotFound):
				return model.Extension{}, err
			}
			return encodeIntent(StateIntentName(capability), in)
		},
		precheck: func(ctx context.Context, tx store.Tx) error {
			conv, err := tx.GetConversation(ctx, req.Conversation)
			switch {
			case model.IsCode(err, model.NotFound) || (err == nil && conv.Ref != req.Conversation):
				return engine.ErrNotFound()
			case err != nil:
				return err
			case req.Command.ExpectedRevision != "" && conv.Revision != req.Command.ExpectedRevision:
				return model.NewError(model.Conflict, "conversation revision changed")
			}
			return nil
		},
		result: &res,
	})
}

// baseState snapshots channel's provider state of conv.
func baseState(conv model.Conversation, channel model.ChannelRef) StateBase {
	if i := stateIndex(conv, channel); i >= 0 {
		return StateBase{Held: true, State: conv.ProviderState[i].Clone()}
	}
	return StateBase{}
}

// stateIndex is the index of channel's entry in conv.ProviderState, or -1.
func stateIndex(conv model.Conversation, channel model.ChannelRef) int {
	return slices.IndexFunc(conv.ProviderState, func(s model.ProviderConversationState) bool { return s.Channel == channel })
}

// stateFingerprint returns the version 1 fingerprint of a state operation:
// kind (the action), target conversation, verb, destination, label changes,
// flag kind and expected revision. The scope follows from the
// kind and the move shape from the adapter, so neither is covered.
func stateFingerprint(action string, req StateRequest) []byte {
	list := func(ids []string) *Writer {
		w := Field().Str(strconv.Itoa(len(ids)))
		for _, id := range ids {
			w.Str(id)
		}
		return w
	}
	f := NewFingerprint()
	f.Set("kind", Field().Str(action))
	f.Set("target", Field().Ref(req.Conversation.Ref()))
	f.Set("verb", Field().Str(string(req.Verb)))
	f.Set("destination", Field().Str(req.Destination.ID).Str(req.Destination.Role))
	f.Set("add", list(req.Add))
	f.Set("remove", list(req.Remove))
	f.Set("flag_kind", Field().Str(req.Kind))
	f.Set("expected_revision", Field().Str(string(req.Command.ExpectedRevision)))
	return f.Sum()
}

// DecodeStateIntent reads a stored state intent of capability c.
func DecodeStateIntent(e model.Extension, c model.Capability) (StateIntent, error) {
	return decodeIntent[StateIntent](e, StateIntentName(c))
}

// StateIntentOf reads the state intent an operation's stored command holds,
// and reports false for any other operation or one unreadable.
func StateIntentOf(rec store.OperationRecord) (StateIntent, bool) {
	name, ok := strings.CutPrefix(rec.Command.Name, IntentStatePrefix)
	if !ok {
		return StateIntent{}, false
	}
	in, err := DecodeStateIntent(rec.Command, model.Capability(name))
	return in, err == nil
}

// Echoes reports whether reported, the provider's own report of a channel's
// state, shows the change in asked: the part its verb changes holds what
// applying the verb to held would leave. A move to a role is shown by the
// reported folder's role, as the adapter alone resolves the folder itself.
func (in StateIntent) Echoes(held, reported model.ProviderConversationState) bool {
	if in.Verb == model.StateVerbMove && in.Destination.Role != "" {
		// A label-shaped provider reports no role: nothing shows the move.
		return in.Shape == model.MoveShapeFolder && reported.Folder != nil && reported.Folder.Role == in.Destination.Role
	}
	want := held.Clone()
	applyState(&want, in)
	return statePart(reported, in) == statePart(want, in)
}

// pendingStateOf decodes a state intent into the change it is pending,
// reporting false for any other intent or one that is unreadable or
// invalid.
func pendingStateOf(op model.OperationRef, intent model.Extension) (model.PendingState, bool) {
	name, ok := strings.CutPrefix(intent.Name, IntentStatePrefix)
	if !ok {
		return model.PendingState{}, false
	}
	in, err := DecodeStateIntent(intent, model.Capability(name))
	if err != nil {
		return model.PendingState{}, false
	}
	p := model.PendingState{
		Operation: op, Conversation: in.Conversation, Verb: in.Verb,
		Add: slices.Clone(in.Add), Remove: slices.Clone(in.Remove), Kind: in.Kind,
	}
	if !in.Destination.IsZero() {
		d := in.Destination
		p.Destination = &d
	}
	if p.Validate() != nil {
		return model.PendingState{}, false
	}
	return p, true
}

// applyState applies in's verb to st, a channel's provider state.
func applyState(st *model.ProviderConversationState, in StateIntent) {
	switch in.Verb {
	case model.StateVerbArchive, model.StateVerbUnarchive:
		st.Archived = in.Verb == model.StateVerbArchive
	case model.StateVerbTrash, model.StateVerbUntrash:
		st.Trashed = in.Verb == model.StateVerbTrash
	case model.StateVerbRead, model.StateVerbUnread:
		v := in.Verb == model.StateVerbRead
		st.Read = &v
	case model.StateVerbFlag:
		if !slices.Contains(st.Flags, in.Kind) {
			st.Flags = append(st.Flags, in.Kind)
		}
	case model.StateVerbUnflag:
		st.Flags = slices.DeleteFunc(st.Flags, func(k string) bool { return k == in.Kind })
	case model.StateVerbMove:
		applyMove(st, in)
	}
}

// applyMove applies a move. A folder-shaped provider holds the destination
// as the folder; a label-shaped one adds and removes labels, a destination
// naming one label to add. A destination named by role records nothing: the
// provider's own folder is known only to its report.
func applyMove(st *model.ProviderConversationState, in StateIntent) {
	add, remove := in.Add, in.Remove
	switch {
	case in.Shape == model.MoveShapeFolder:
		if in.Destination.ID != "" {
			st.Folder = &model.Folder{ID: in.Destination.ID}
		}
		return
	case in.Destination.ID != "":
		add = []string{in.Destination.ID}
	}
	for _, id := range remove {
		st.Labels = slices.DeleteFunc(st.Labels, func(l model.ProviderLabel) bool { return l.ID == id })
	}
	for _, id := range add {
		if !slices.ContainsFunc(st.Labels, func(l model.ProviderLabel) bool { return l.ID == id }) {
			st.Labels = append(st.Labels, model.ProviderLabel{ID: id})
		}
	}
}

// statePart is the part of st the verb of in changes, as a comparable
// string: two states hold the same part when their strings are equal.
func statePart(st model.ProviderConversationState, in StateIntent) string {
	b := func(v bool) string { return strconv.FormatBool(v) }
	switch in.Verb {
	case model.StateVerbArchive, model.StateVerbUnarchive:
		return b(st.Archived)
	case model.StateVerbTrash, model.StateVerbUntrash:
		return b(st.Trashed)
	case model.StateVerbRead, model.StateVerbUnread:
		if st.Read == nil {
			return "unknown"
		}
		return b(*st.Read)
	case model.StateVerbFlag, model.StateVerbUnflag:
		return b(slices.Contains(st.Flags, in.Kind))
	case model.StateVerbMove:
		if in.Shape == model.MoveShapeFolder {
			if st.Folder == nil {
				return ""
			}
			return st.Folder.ID
		}
		touched := append(slices.Clone(in.Add), in.Remove...)
		if in.Destination.ID != "" {
			touched = append(touched, in.Destination.ID)
		}
		slices.Sort(touched)
		out := ""
		for _, id := range touched {
			held := slices.ContainsFunc(st.Labels, func(l model.ProviderLabel) bool { return l.ID == id })
			out += id + "=" + b(held) + ";"
		}
		return out
	}
	return ""
}

// SettleState applies an applied state operation to the stored
// conversation: the verb's part of the channel's provider state becomes
// what was asked, the conversation moves to a new revision and
// conversation.changed provider_state is emitted, command-triggered by the
// caller under the operation, internal audience. Nothing applies to an
// operation that was not applied, to a conversation gone since, when the
// part already is what was asked (an echo that beat the answer, or a verb
// that records nothing), or when the part moved since the claim without
// being what was asked: a newer inbound report wins, and the operation
// keeps the provider's answer. Local status and labels are never touched.
func SettleState(ctx context.Context, tx store.Tx, e *engine.Engine, op model.Operation, in StateIntent) error {
	if op.State != model.ProviderAccepted && op.State != model.Delivered {
		return nil
	}
	cur, err := tx.GetConversation(ctx, in.Conversation)
	switch {
	case model.IsCode(err, model.NotFound) || (err == nil && cur.Ref != in.Conversation):
		return nil
	case err != nil:
		return err
	}
	idx := stateIndex(cur, in.Channel)
	held := model.ProviderConversationState{Channel: in.Channel}
	if idx >= 0 {
		held = cur.ProviderState[idx].Clone()
	}
	want := held.Clone()
	applyState(&want, in)
	switch now := statePart(held, in); {
	case now == statePart(want, in):
		return nil
	case now != statePart(in.Base.State, in):
		return nil
	}
	now := e.Clock().Now().UTC()
	want.ObservedAt = now
	next := cur.Clone()
	if idx >= 0 {
		next.ProviderState[idx] = want
	} else {
		next.ProviderState = append(next.ProviderState, want)
	}
	if next.Revision, err = NewRevision(ctx, e); err != nil {
		return err
	}
	if err := tx.PutConversation(ctx, next, cur.Revision); err != nil {
		return err
	}
	rec, err := tx.Operation(ctx, op.Ref)
	if err != nil {
		return err
	}
	id, err := NewID(ctx, e)
	if err != nil {
		return err
	}
	opRef := op.Ref
	data, err := json.Marshal(model.ConversationChangedPayload{
		Conversation: next.Ref, Changes: []string{model.ConversationFieldProviderState}, Trigger: model.TriggerCommand,
		PreviousRevision: cur.Revision, Operation: &opRef,
	})
	if err != nil {
		return model.NewError(model.Permanent, "event payload encoding failed")
	}
	res := next.Ref.Ref()
	return tx.AppendEvents(ctx, []model.Event{{
		Version: model.EventEnvelopeVersion, ID: id, Kind: model.EventConversationChanged,
		Tenant: res.Tenant, Inbox: res.Inbox, Resource: res, Actor: rec.Key.Caller,
		OccurredAt: now, RecordedAt: now, Audience: stateAudience, Revision: next.Revision,
		Payload: model.Extension{Name: model.SchemaConversationChanged, Version: model.PayloadVersion1, Data: data},
	}})
}
