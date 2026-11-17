package model

import "strings"

// Envelope kinds (Envelope.Kind): the normalized event classes an adapter
// hands to its sink. The set is closed; the engine rejects any other kind.
const (
	// EnvelopeMessage is a new inbound message authored by its sender.
	EnvelopeMessage = "message"
	// EnvelopeAutomatedMessage is a new inbound message the adapter marks
	// automated, e.g. email with an Auto-Submitted value other than "no"
	// (RFC 3834 §5). It never reopens a conversation.
	EnvelopeAutomatedMessage = "message.automated"
	// EnvelopeMessageEdited replaces the content of the known message whose
	// provider ID is Envelope.MessageID. An Envelope.Audience narrowing a
	// public message to internal or restricted narrows it, even when the
	// content is older than the applied one; an audience is never widened.
	// An edit without content changes only the audience. An edit applies
	// as the sender's: Message.LastEdit records the sender, the ingress
	// principal and the envelope time, and the original author stays. An
	// edit whose content equals what is stored changes nothing.
	EnvelopeMessageEdited = "message.edited"
	// EnvelopeMessageDeleted tombstones the known message Envelope.MessageID
	// (Message.Deleted, content cleared); it never redacts.
	EnvelopeMessageDeleted = "message.deleted"
	// EnvelopeReactionAdded and EnvelopeReactionRemoved carry one reaction
	// on the known message Envelope.MessageID; the emoji is the single text
	// part of Envelope.Content and follows ValidateEmoji. A removal may
	// carry no content when the provider names no emoji: it removes the
	// sender's only reaction on the message. From a system actor a
	// reaction.added carries instead one structured part holding a
	// ReactionSummary (SchemaReactionSummary): the provider's per-emoji
	// counts, stored without any reacting identity; only an authoritative
	// copy (a refetch) marks the set complete.
	EnvelopeReactionAdded   = "reaction.added"
	EnvelopeReactionRemoved = "reaction.removed"
	// EnvelopeStatus is a system or status event (delivery, read, typing).
	// It runs no conversation selection and creates no contact or message.
	EnvelopeStatus = "status"
	// EnvelopeWorkItem is a provider-side change of an issue or ticket
	// (opened, edited, status, assignees, fields, merge, move, deletion,
	// loss of access) carrying its state on Envelope.WorkItem. It binds the
	// work item's conversation in a per-work-item inbox, never reopens one
	// and creates no contact or message.
	EnvelopeWorkItem = "work_item"
	// EnvelopeExtensionEvent is a provider notice of a registered event
	// kind, carried on Envelope.Event: Name an event-kind extension
	// definition the adapter declares in Descriptor.Events, Version one of
	// its registered versions, Data its payload. Core appends it to the
	// event log as one event of that kind, about the resolved inbox or,
	// when Envelope.MessageID names a known message under provider-assigned
	// IDs, about that message; it creates no contact, identity,
	// conversation or message. EventID is required, Audience is required
	// and the event is never wider than its message; a sender, content,
	// references and a provider payload are refused. A notice about a
	// redacted or provider-deleted message is accepted and appends nothing.
	// An Inboxes filter selects the events about its inbox as well as those
	// about its messages.
	//
	// The event log is immutable until retention and redaction never
	// reaches it, so the adapter puts only provider references,
	// identifiers and a change summary in Data: never message bodies,
	// submitted values, attachment bytes or credentials. Core checks size
	// and JSON validity only.
	EnvelopeExtensionEvent = "extension.event"
	// EnvelopeInteraction is a structured interaction (command, action or
	// submission) carried on Envelope.Interaction: Sender is its actor and
	// EventID its provider interaction ID. It records an interaction, never
	// a message.
	EnvelopeInteraction = "interaction"
	// EnvelopeConversationState is the provider's state of one conversation
	// on the channel (archived, folder or labels, read, flags, trashed),
	// carried on Envelope.State. It creates no contact, message or
	// conversation: a state for an unknown conversation is accepted and
	// recorded as nothing. Applying it sets Conversation.ProviderState and
	// reports ConversationFieldProviderState.
	EnvelopeConversationState = "conversation.state"
)

// IsEnvelopeKind reports whether k is a declared envelope kind.
func IsEnvelopeKind(k string) bool {
	switch k {
	case EnvelopeMessage, EnvelopeAutomatedMessage, EnvelopeStatus, EnvelopeWorkItem, EnvelopeExtensionEvent,
		EnvelopeInteraction, EnvelopeConversationState:
		return true
	}
	return IsMessageUpdateKind(k)
}

// IsMessageUpdateKind reports whether k targets a known message: edits,
// deletions and reactions. Updates join the target message's conversation;
// they never select, create or reopen one.
func IsMessageUpdateKind(k string) bool {
	switch k {
	case EnvelopeMessageEdited, EnvelopeMessageDeleted, EnvelopeReactionAdded, EnvelopeReactionRemoved:
		return true
	}
	return false
}

// Scoping reference kinds (ExternalRef.Kind on Envelope.References). Each
// non-contact selection mode keys its conversation on one of them, scoped by
// the reference's channel and namespace.
const (
	// RefKindRoom names a provider room or channel (per-room mode).
	RefKindRoom = "room"
	// RefKindThread names a provider thread; ParentID is its room (per-thread mode).
	RefKindThread = "thread"
	// RefKindWorkItem names a provider issue or ticket (per-work-item mode).
	RefKindWorkItem = "work_item"
	// RefKindContext names an agent context (per-agent-context mode).
	RefKindContext = "context"
	// RefKindMessage is the kind of ProviderMessageRef.
	RefKindMessage = "message"
	// RefKindInteraction is the kind of ProviderInteractionRef.
	RefKindInteraction = "interaction"
	// RefKindTask names an agent task: the kind of TaskExternalRef, of a
	// continuation's TaskBinding.External and of the tasks a message
	// references (never used for selection).
	RefKindTask = "task"
)

// ProviderMessageNamespace is the namespace of ProviderMessageRef.
const ProviderMessageNamespace = CoreNamespace + ".provider"

// ProviderMessageRef returns the reference under which a stored message
// records its provider message ID on channel: inbound messages at ingress,
// outbound messages once the provider reports their ID (including a
// provider-rewritten email Message-ID). Message updates and email threading
// look messages up by it. id stays byte-exact.
func ProviderMessageRef(ch ChannelRef, id string) ExternalRef {
	return ExternalRef{Channel: ch, Namespace: ProviderMessageNamespace, Kind: RefKindMessage, ID: id}
}

// ProviderInteractionRef returns the reference under which a stored
// interaction records its provider interaction ID on channel. id stays
// byte-exact.
func ProviderInteractionRef(ch ChannelRef, id string) ExternalRef {
	return ExternalRef{Channel: ch, Namespace: ProviderMessageNamespace, Kind: RefKindInteraction, ID: id}
}

// RuleSetContactLifecycleV1 is the approved per-contact lifecycle rule set
// (Selection.LifecycleRules): the contact lifecycle truth table and email
// threading policy pinned by conformance/testdata/contact_lifecycle.json.
const RuleSetContactLifecycleV1 = "contact-lifecycle/v1"

// Validate checks a selection configuration. Mode must be declared.
// Per-contact mode requires an explicit LockToSingle (false is not unset)
// and LifecycleRules RuleSetContactLifecycleV1; every other mode requires
// both unset. Policy, when set, is a token naming a registered selector.
func (s Selection) Validate() error {
	if !IsSelectionMode(s.Mode) {
		return invalidField("selection.mode", "must be a declared selection mode")
	}
	if s.Mode == ModePerContact {
		switch {
		case s.LockToSingle == nil:
			return invalidField("selection.lock_to_single", "required in per-contact mode")
		case s.LifecycleRules == "":
			return invalidField("selection.lifecycle_rules", "required in per-contact mode")
		case s.LifecycleRules != RuleSetContactLifecycleV1:
			return invalidField("selection.lifecycle_rules", "unknown rule set")
		}
	} else {
		switch {
		case s.LockToSingle != nil:
			return invalidField("selection.lock_to_single", "must be unset outside per-contact mode")
		case s.LifecycleRules != "":
			return invalidField("selection.lifecycle_rules", "must be unset outside per-contact mode")
		}
	}
	return checkOptionalToken("selection.policy", s.Policy)
}

// Email header reference kinds. A mail adapter normalizes threading headers
// into Envelope.References under EmailHeaderNamespace (see
// EmailHeaders.Normalize); core matches them against stored messages of the
// same inbox.
const (
	EmailHeaderNamespace   = "email.header"
	RefKindEmailMessageID  = "email.message_id"
	RefKindEmailInReplyTo  = "email.in_reply_to"
	RefKindEmailReferences = "email.references"
	RefKindEmailListID     = "email.list_id"
)

// EmailHeaders holds raw threading header values of one inbound mail.
// Subject, Resent-*, X-Forwarded-Message-Id and Thread-Index are not
// threading evidence and have no field.
type EmailHeaders struct {
	MessageID     string `json:"message_id"`
	InReplyTo     string `json:"in_reply_to"`
	References    string `json:"references"`
	ListID        string `json:"list_id"`
	AutoSubmitted string `json:"auto_submitted"`
}

// NormalizedEmail is the envelope view of EmailHeaders.
type NormalizedEmail struct {
	// MessageID is the first valid msg-id of Message-ID, or empty.
	MessageID string `json:"message_id"`
	// References lists, in order: the own Message-ID, the first valid
	// In-Reply-To msg-id, every References msg-id in header order, and the
	// List-Id identifier, each only when present.
	References []ExternalRef `json:"references,omitempty"`
	// InReplyTo is the first valid In-Reply-To msg-id, or empty: the value
	// for Envelope.InReplyTo.
	InReplyTo string `json:"in_reply_to"`
	// Automated reports an Auto-Submitted value other than "no".
	Automated bool `json:"automated"`
}

// Normalize returns the envelope fields for h on channel ch. In-Reply-To
// contributes only its first syntactically valid msg-id; surrounding text is
// ignored.
func (h EmailHeaders) Normalize(ch ChannelRef) NormalizedEmail {
	ref := func(kind, id string) ExternalRef {
		return ExternalRef{Channel: ch, Namespace: EmailHeaderNamespace, Kind: kind, ID: id}
	}
	var out NormalizedEmail
	if ids := ParseMsgIDs(h.MessageID); len(ids) > 0 {
		out.MessageID = ids[0]
		out.References = append(out.References, ref(RefKindEmailMessageID, ids[0]))
	}
	if ids := ParseMsgIDs(h.InReplyTo); len(ids) > 0 {
		out.References = append(out.References, ref(RefKindEmailInReplyTo, ids[0]))
		out.InReplyTo = ids[0]
	}
	for _, id := range ParseMsgIDs(h.References) {
		out.References = append(out.References, ref(RefKindEmailReferences, id))
	}
	if id := listIdentifier(h.ListID); id != "" {
		out.References = append(out.References, ref(RefKindEmailListID, id))
	}
	out.Automated = IsAutoSubmitted(h.AutoSubmitted)
	return out
}

// IsAutoSubmitted reports whether an Auto-Submitted header value marks the
// message automated: any keyword other than "no" (RFC 3834 §5),
// case-insensitive, ignoring parameters.
func IsAutoSubmitted(v string) bool {
	keyword, _, _ := strings.Cut(v, ";")
	keyword = strings.TrimSpace(keyword)
	return keyword != "" && !strings.EqualFold(keyword, "no")
}

// ParseMsgIDs returns the syntactically valid msg-id tokens of a header
// value in order, each as "<id-left@id-right>": angle brackets around one
// "@" with non-empty sides and no whitespace or brackets. Other text is
// skipped.
func ParseMsgIDs(s string) []string {
	var out []string
	for {
		open := strings.IndexByte(s, '<')
		if open < 0 {
			return out
		}
		end := strings.IndexByte(s[open+1:], '>')
		if end < 0 {
			return out
		}
		inner := s[open+1 : open+1+end]
		if i := strings.LastIndexByte(inner, '<'); i >= 0 {
			// "<a <b@c>": restart at the innermost opening bracket.
			s = s[open+1+i:]
			continue
		}
		if validMsgID(inner) {
			out = append(out, "<"+inner+">")
		}
		s = s[open+1+end+1:]
	}
}

func validMsgID(inner string) bool {
	left, right, ok := strings.Cut(inner, "@")
	if !ok || left == "" || right == "" || strings.Contains(right, "@") {
		return false
	}
	for i := 0; i < len(inner); i++ {
		if c := inner[i]; c <= ' ' || c == 0x7f {
			return false
		}
	}
	return true
}

// listIdentifier returns the bracketed list identifier of a List-Id value
// (RFC 2919), without brackets, or the trimmed value when it has none.
func listIdentifier(v string) string {
	open := strings.LastIndexByte(v, '<')
	if open >= 0 {
		if end := strings.IndexByte(v[open:], '>'); end > 1 {
			return strings.TrimSpace(v[open+1 : open+end])
		}
	}
	if strings.ContainsAny(v, "<>") {
		return ""
	}
	return strings.TrimSpace(v)
}
