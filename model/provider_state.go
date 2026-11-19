package model

import (
	"encoding/json"
	"slices"
	"strconv"
	"time"
)

// Provider conversation state: what the provider itself holds about a
// conversation, apart from uma's local workflow state. It is never merged
// into Conversation.Status or Conversation.Labels.

// StateVerb is one provider state verb. Each pair is one capability:
// archive and unarchive are CapabilityArchive, move is CapabilityMove, read
// and unread are CapabilityReadState, flag and unflag are CapabilityFlag,
// trash and untrash are CapabilityTrash.
type StateVerb string

const (
	StateVerbArchive   StateVerb = "archive"
	StateVerbUnarchive StateVerb = "unarchive"
	StateVerbMove      StateVerb = "move"
	StateVerbRead      StateVerb = "read"
	StateVerbUnread    StateVerb = "unread"
	StateVerbFlag      StateVerb = "flag"
	StateVerbUnflag    StateVerb = "unflag"
	StateVerbTrash     StateVerb = "trash"
	StateVerbUntrash   StateVerb = "untrash"
)

// Capability returns the capability that carries v, and false for an
// undeclared verb.
func (v StateVerb) Capability() (Capability, bool) {
	switch v {
	case StateVerbArchive, StateVerbUnarchive:
		return CapabilityArchive, true
	case StateVerbMove:
		return CapabilityMove, true
	case StateVerbRead, StateVerbUnread:
		return CapabilityReadState, true
	case StateVerbFlag, StateVerbUnflag:
		return CapabilityFlag, true
	case StateVerbTrash, StateVerbUntrash:
		return CapabilityTrash, true
	}
	return "", false
}

// StateScope says whose view a state verb changes, reported as the "scope"
// limit of the verb's CapabilityStatus.
type StateScope string

const (
	// ScopeAccount: only the provider account's own view changes, such as
	// a mailbox.
	ScopeAccount StateScope = "account"
	// ScopeShared: the state changes for everyone, such as a chat room or
	// a ticket.
	ScopeShared StateScope = "shared"
)

// Valid reports whether s is a declared scope.
func (s StateScope) Valid() bool { return s == ScopeAccount || s == ScopeShared }

// Action returns the policy action that authorizes a state verb of scope s,
// and false for an undeclared scope.
func (s StateScope) Action() (string, bool) {
	switch s {
	case ScopeAccount:
		return ActionConversationStateAccount, true
	case ScopeShared:
		return ActionConversationStateShared, true
	}
	return "", false
}

// MoveShape is how a provider holds a conversation's place, reported as the
// "shape" limit of the move status.
type MoveShape string

const (
	// MoveShapeLabels: membership in several labels; a move adds and
	// removes label IDs.
	MoveShapeLabels MoveShape = "labels"
	// MoveShapeFolder: exclusive membership in one folder; a move names the
	// destination folder.
	MoveShapeFolder MoveShape = "folder"
)

// Valid reports whether s is a declared shape.
func (s MoveShape) Valid() bool { return s == MoveShapeLabels || s == MoveShapeFolder }

// Keys of CapabilityStatus.Limits for the state capabilities.
const (
	// StateLimitScope holds a StateScope; every state capability reports it.
	StateLimitScope = "scope"
	// StateLimitKinds holds the flag kinds the provider supports.
	StateLimitKinds = "kinds"
	// StateLimitShape holds a MoveShape; the move capability reports it.
	StateLimitShape = "shape"
	// StateLimitRoles holds the destination roles the provider resolves.
	StateLimitRoles = "roles"
)

// Flag kinds (FlagConversation.Kind, ProviderConversationState.Flags).
const (
	FlagStar      = "star"
	FlagImportant = "important"
)

// IsFlagKind reports whether k is a declared flag kind.
func IsFlagKind(k string) bool {
	return k == FlagStar || k == FlagImportant
}

// Destination roles (MoveDestination.Role): special-use folders the adapter
// resolves to the provider's own.
const (
	RoleInbox   = "inbox"
	RoleArchive = "archive"
	RoleTrash   = "trash"
	RoleJunk    = "junk"
)

// IsDestinationRole reports whether r is a declared destination role.
func IsDestinationRole(r string) bool {
	switch r {
	case RoleInbox, RoleArchive, RoleTrash, RoleJunk:
		return true
	}
	return false
}

// Folder is the provider folder a conversation sits in. ID is the
// provider's, Name its display name, Role the special use when it has one.
type Folder struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// ProviderLabel is a provider label on a conversation. It is not a tenant
// Label and is never matched to one by name. Name is the provider's display
// name where the adapter can read it (a mail adapter reads its mailbox list,
// ProviderMailbox); where it cannot, Name is the ID.
type ProviderLabel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ProviderConversationState is one channel instance's view of a
// conversation. Folder and Labels are exclusive by the provider's move
// shape: a folder channel sets Folder, a label channel sets Labels. Read is
// nil when the provider reported no read state. Complete is true only when
// every provider item of the conversation on the channel was observed.
// ObservedAt is the provider's event or sync time, not the commit time.
type ProviderConversationState struct {
	Channel    ChannelRef      `json:"channel"`
	Archived   bool            `json:"archived"`
	Folder     *Folder         `json:"folder,omitempty"`
	Labels     []ProviderLabel `json:"labels,omitempty"`
	Read       *bool           `json:"read,omitempty"`
	Flags      []string        `json:"flags,omitempty"`
	Trashed    bool            `json:"trashed"`
	ObservedAt time.Time       `json:"observed_at"`
	Complete   bool            `json:"complete"`
}

// Clone returns a deep copy.
func (s ProviderConversationState) Clone() ProviderConversationState {
	s.Folder = clonePtr(s.Folder, same)
	s.Labels = slices.Clone(s.Labels)
	s.Read = clonePtr(s.Read, same)
	s.Flags = slices.Clone(s.Flags)
	return s
}

// validate checks the shape. The channel is checked only when
// requireChannel is set: an envelope's state takes it from the binding.
func (s ProviderConversationState) validate(path string, requireChannel bool) error {
	if requireChannel {
		if err := s.Channel.validate(fieldPath(path, "channel"), false); err != nil {
			return err
		}
	}
	if s.Folder != nil && len(s.Labels) > 0 {
		return invalidField(fieldPath(path, "labels"), "exclusive with folder")
	}
	if s.Folder != nil {
		if err := checkToken(fieldPath(path, "folder.id"), s.Folder.ID); err != nil {
			return err
		}
		if s.Folder.Role != "" && !IsDestinationRole(s.Folder.Role) {
			return invalidField(fieldPath(path, "folder.role"), "must be a declared destination role")
		}
	}
	seen := map[string]bool{}
	for _, l := range s.Labels {
		if err := checkToken(fieldPath(path, "labels.id"), l.ID); err != nil {
			return err
		}
		if seen[l.ID] {
			return invalidField(fieldPath(path, "labels"), "must not repeat a label")
		}
		seen[l.ID] = true
	}
	for i, f := range s.Flags {
		if !IsFlagKind(f) {
			return invalidField(fieldPath(path, "flags"), "must be declared flag kinds")
		}
		if slices.Contains(s.Flags[:i], f) {
			return invalidField(fieldPath(path, "flags"), "must not repeat a kind")
		}
	}
	if s.ObservedAt.IsZero() {
		return invalidField(fieldPath(path, "observed_at"), "required")
	}
	return nil
}

// Validate checks a stored state: channel, exclusivity, tokens, flag kinds
// and the observation time.
func (s ProviderConversationState) Validate() error { return s.validate("", true) }

// ProviderStateReport is the payload of a conversation.state envelope
// (Envelope.State): the provider's state of one conversation on the bound
// channel. Conversation is the conversation reference as the adapter
// reports it (a room, thread or work item); State.Channel is ignored, the
// channel is the ingress binding's.
type ProviderStateReport struct {
	Conversation ExternalRef               `json:"conversation"`
	State        ProviderConversationState `json:"state"`
}

// Clone returns a deep copy.
func (r ProviderStateReport) Clone() ProviderStateReport {
	r.State = r.State.Clone()
	return r
}

// Validate checks the conversation reference and the state.
func (r ProviderStateReport) Validate() error { return r.validate("") }

func (r ProviderStateReport) validate(path string) error {
	return firstError(
		r.Conversation.validate(fieldPath(path, "conversation")),
		r.State.validate(fieldPath(path, "state"), false),
	)
}

// ArchiveConversation archives a conversation at the provider, or
// unarchives it when Undo is set (Conversations.Archive).
type ArchiveConversation struct {
	Command
	Conversation ConversationRef `json:"conversation"`
	Undo         bool            `json:"undo"`
	// Messages, when set, names the messages of the conversation the
	// change applies to, and only those; the conversation's other
	// messages are left alone, and a provider that changes state per
	// thread must not widen it. Each must belong to the conversation
	// (else model.Validation). Empty: every message of the conversation.
	Messages []ID `json:"messages,omitempty"`
}

// MoveDestination names where Conversations.Move puts a conversation:
// exactly one of ID, the provider's own identifier (a label ID, a folder ID
// or a mailbox name), or Role, a special-use folder the adapter resolves.
type MoveDestination struct {
	ID   string `json:"id"`
	Role string `json:"role"`
}

// IsZero reports whether d names nothing.
func (d MoveDestination) IsZero() bool { return d == MoveDestination{} }

func (d MoveDestination) validate(path string) error {
	switch {
	case d.ID != "" && d.Role != "":
		return invalidField(path, "name an ID or a role, not both")
	case d.Role != "":
		if !IsDestinationRole(d.Role) {
			return invalidField(fieldPath(path, "role"), "must be a declared destination role")
		}
		return nil
	}
	return checkToken(fieldPath(path, "id"), d.ID)
}

// MoveConversation moves a conversation at the provider
// (Conversations.Move). A folder-shaped provider takes Destination; a
// label-shaped one takes Destination or the Add and Remove label IDs, which
// a folder-shaped provider rejects; the engine checks the shape against the
// status the adapter reported. The request names a destination or label
// changes, never both.
type MoveConversation struct {
	Command
	Conversation ConversationRef `json:"conversation"`
	Destination  MoveDestination `json:"destination"`
	Add          []string        `json:"add,omitempty"`
	Remove       []string        `json:"remove,omitempty"`
	// Messages, when set, names the messages of the conversation the
	// change applies to, and only those; the conversation's other
	// messages are left alone, and a provider that changes state per
	// thread must not widen it. Each must belong to the conversation
	// (else model.Validation). Empty: every message of the conversation.
	Messages []ID `json:"messages,omitempty"`
}

// SetConversationRead marks a conversation read or unread at the provider
// (Conversations.SetRead). It never changes the per-actor read marker.
type SetConversationRead struct {
	Command
	Conversation ConversationRef `json:"conversation"`
	Read         bool            `json:"read"`
	// Messages, when set, names the messages of the conversation the
	// change applies to, and only those; the conversation's other
	// messages are left alone, and a provider that changes state per
	// thread must not widen it. Each must belong to the conversation
	// (else model.Validation). Empty: every message of the conversation.
	Messages []ID `json:"messages,omitempty"`
}

// FlagConversation sets or, with Undo, clears a flag of Kind at the
// provider (Conversations.Flag).
type FlagConversation struct {
	Command
	Conversation ConversationRef `json:"conversation"`
	Kind         string          `json:"kind"`
	Undo         bool            `json:"undo"`
	// Messages, when set, names the messages of the conversation the
	// change applies to, and only those; the conversation's other
	// messages are left alone, and a provider that changes state per
	// thread must not widen it. Each must belong to the conversation
	// (else model.Validation). Empty: every message of the conversation.
	Messages []ID `json:"messages,omitempty"`
}

// TrashConversation moves the account's copy to the provider's trash, or
// restores it when Undo is set (Conversations.Trash). It is not a message
// delete at the peer.
type TrashConversation struct {
	Command
	Conversation ConversationRef `json:"conversation"`
	Undo         bool            `json:"undo"`
	// Messages, when set, names the messages of the conversation the
	// change applies to, and only those; the conversation's other
	// messages are left alone, and a provider that changes state per
	// thread must not widen it. Each must belong to the conversation
	// (else model.Validation). Empty: every message of the conversation.
	Messages []ID `json:"messages,omitempty"`
}

// RevisionRule is RevisionOptional: a caller may change provider state
// without reading the conversation first. The same holds for every state
// request.
func (ArchiveConversation) RevisionRule() RevisionRule { return RevisionOptional }

// RevisionRule is RevisionOptional.
func (MoveConversation) RevisionRule() RevisionRule { return RevisionOptional }

// RevisionRule is RevisionOptional.
func (SetConversationRead) RevisionRule() RevisionRule { return RevisionOptional }

// RevisionRule is RevisionOptional.
func (FlagConversation) RevisionRule() RevisionRule { return RevisionOptional }

// RevisionRule is RevisionOptional.
func (TrashConversation) RevisionRule() RevisionRule { return RevisionOptional }

// Validate checks the command and the conversation.
func (r ArchiveConversation) Validate() error {
	return firstError(r.Command.Validate(r.RevisionRule()), r.Conversation.validate("conversation", false), validateMessages(r.Messages))
}

// Validate checks the command, the conversation, that the request names a
// destination or label changes but not both, and the names themselves.
func (r MoveConversation) Validate() error {
	if err := firstError(r.Command.Validate(r.RevisionRule()), r.Conversation.validate("conversation", false), validateMessages(r.Messages)); err != nil {
		return err
	}
	labels := len(r.Add)+len(r.Remove) > 0
	switch {
	case !r.Destination.IsZero() && labels:
		return invalidField("destination", "exclusive with add and remove")
	case r.Destination.IsZero() && !labels:
		return invalidField("destination", "required without add or remove")
	case !labels:
		return r.Destination.validate("destination")
	}
	for _, l := range []struct {
		path string
		ids  []string
	}{{"add", r.Add}, {"remove", r.Remove}} {
		for i, id := range l.ids {
			if err := checkToken(l.path, id); err != nil {
				return err
			}
			if slices.Contains(l.ids[:i], id) {
				return invalidField(l.path, "must not repeat a label")
			}
		}
	}
	for _, id := range r.Add {
		if slices.Contains(r.Remove, id) {
			return invalidField("remove", "must not name a label in add")
		}
	}
	return nil
}

// Validate checks the command and the conversation.
func (r SetConversationRead) Validate() error {
	return firstError(r.Command.Validate(r.RevisionRule()), r.Conversation.validate("conversation", false), validateMessages(r.Messages))
}

// Validate checks the command, the conversation and the kind.
func (r FlagConversation) Validate() error {
	if err := firstError(r.Command.Validate(r.RevisionRule()), r.Conversation.validate("conversation", false), validateMessages(r.Messages)); err != nil {
		return err
	}
	if !IsFlagKind(r.Kind) {
		return invalidField("kind", "must be star or important")
	}
	return nil
}

// Validate checks the command and the conversation.
func (r TrashConversation) Validate() error {
	return firstError(r.Command.Validate(r.RevisionRule()), r.Conversation.validate("conversation", false), validateMessages(r.Messages))
}

// StateLimitsName is the Extension name of the limits a state capability
// reports in CapabilityStatus.Limits; its Data is a JSON object of
// StateLimits.
const StateLimitsName = "uma.state.limits"

// StateLimits are the limits a state capability reports (CapabilityStatus
// Limits, keyed by the StateLimit* names). Scope is required for every
// state capability; Kinds is required for flag, Shape for move; Roles lists
// the destination roles a move resolves.
type StateLimits struct {
	Scope StateScope `json:"scope"`
	Kinds []string   `json:"kinds,omitempty"`
	Shape MoveShape  `json:"shape"`
	Roles []string   `json:"roles,omitempty"`
}

// Extension encodes l as the Limits of a capability status.
func (l StateLimits) Extension() Extension {
	data, _ := json.Marshal(l) // fixed shape of strings: cannot fail
	return Extension{Name: StateLimitsName, Version: 1, Data: data}
}

// ParseStateLimits reads the limits a state capability reported. A report
// that is not state limits, is unreadable or carries an undeclared scope,
// shape, flag kind or role is Validation: the engine reads it fail closed.
func ParseStateLimits(e Extension) (StateLimits, error) {
	var l StateLimits
	if e.Name != StateLimitsName || e.Version != 1 {
		return l, NewError(Validation, "limits: not state limits")
	}
	if err := json.Unmarshal(e.Data, &l); err != nil {
		return StateLimits{}, NewError(Validation, "limits: unreadable")
	}
	switch {
	case !l.Scope.Valid():
		return StateLimits{}, invalidField(StateLimitScope, "must be account or shared")
	case l.Shape != "" && !l.Shape.Valid():
		return StateLimits{}, invalidField(StateLimitShape, "must be labels or folder")
	}
	for _, k := range l.Kinds {
		if !IsFlagKind(k) {
			return StateLimits{}, invalidField(StateLimitKinds, "must be declared flag kinds")
		}
	}
	for _, r := range l.Roles {
		if !IsDestinationRole(r) {
			return StateLimits{}, invalidField(StateLimitRoles, "must be declared destination roles")
		}
	}
	return l, nil
}

// PendingState is the unapplied change of an unsettled provider state
// operation, as Operations.Get reveals it (see Operation.PendingState): the
// verb and what it names. It is derived from the stored intent while the
// operation is unsettled and the caller may read the conversation; it is
// never stored, never emitted in an event and absent from an Acceptance.
type PendingState struct {
	// Operation is the state operation.
	Operation OperationRef `json:"operation"`
	// Conversation is the conversation the change applies to.
	Conversation ConversationRef `json:"conversation"`
	// Verb is the change.
	Verb StateVerb `json:"verb"`
	// Destination, Add and Remove are a move's.
	Destination *MoveDestination `json:"destination,omitempty"`
	Add         []string         `json:"add,omitempty"`
	Remove      []string         `json:"remove,omitempty"`
	// Kind is a flag's.
	Kind string `json:"kind"`
}

// Validate checks the operation, the conversation and the verb.
func (p PendingState) Validate() error {
	const path = "pending_state"
	var verb error
	if _, ok := p.Verb.Capability(); !ok {
		verb = invalidField(fieldPath(path, "verb"), "must be a declared state verb")
	}
	return firstError(
		p.Operation.validate(fieldPath(path, "operation"), false),
		p.Conversation.validate(fieldPath(path, "conversation"), false),
		verb,
	)
}

// Clone returns a deep copy.
func (p PendingState) Clone() PendingState {
	p.Destination = clonePtr(p.Destination, same)
	p.Add, p.Remove = slices.Clone(p.Add), slices.Clone(p.Remove)
	return p
}

// validateMessages checks the message IDs of a state request: tokens, none
// repeated.
func validateMessages(ids []ID) error {
	for i, id := range ids {
		if err := checkToken(fieldPath("messages", strconv.Itoa(i)), string(id)); err != nil {
			return err
		}
		if slices.Contains(ids[:i], id) {
			return invalidField("messages", "must not repeat a message")
		}
	}
	return nil
}
