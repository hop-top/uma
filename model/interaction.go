package model

import (
	"slices"
	"strconv"
	"time"
)

// Interaction kinds (InteractionInput.Kind, Interaction.Kind). The set is
// closed so bots can route on it; IsInteractionKind accepts all four.
const (
	// InteractionCommand is a slash or bot command; Command names it and
	// Arguments holds the raw text typed after it.
	InteractionCommand = "command"
	// InteractionAction is a button press or menu selection; Action is
	// the provider action ID.
	InteractionAction = "action"
	// InteractionSubmission is a form or modal submission; Fields holds
	// the submitted values under a named schema.
	InteractionSubmission = "submission"
	// InteractionOptions asks for dynamic options. It is evaluated
	// synchronously (adapter.Sink.EvaluateInteraction) and never accepted
	// as durable ingress.
	InteractionOptions = "options"
)

// IsInteractionKind reports whether k is a declared interaction kind.
func IsInteractionKind(k string) bool {
	switch k {
	case InteractionCommand, InteractionAction, InteractionSubmission, InteractionOptions:
		return true
	}
	return false
}

// Response modes (Respond.Mode). The set is closed; the engine never
// substitutes one mode for another.
const (
	// ResponseModeMessage posts a new message answering the interaction.
	ResponseModeMessage = "message"
	// ResponseModeEphemeral answers the interaction's actor only; its
	// audience is restricted.
	ResponseModeEphemeral = "ephemeral"
	// ResponseModeUpdate replaces the provider message the control sits
	// on (Interaction.MessageExternal).
	ResponseModeUpdate = "update"
)

// IsResponseMode reports whether m is a declared response mode.
func IsResponseMode(m string) bool {
	switch m {
	case ResponseModeMessage, ResponseModeEphemeral, ResponseModeUpdate:
		return true
	}
	return false
}

// maxResponseTokenBytes bounds InteractionInput.ResponseToken: a provider
// response URL or interaction token.
const maxResponseTokenBytes = 4096

// InteractionRef names one interaction record in its conversation.
type InteractionRef struct {
	Conversation ConversationRef `json:"conversation"`
	ID           ID              `json:"id"`
}

// NewInteractionRef returns a validated interaction ref under conversation c.
func NewInteractionRef(c ConversationRef, id ID) (InteractionRef, error) {
	return newValid(InteractionRef{Conversation: c, ID: id})
}

// Validate checks the conversation chain and ID.
func (r InteractionRef) Validate() error { return r.validate("", false) }

// ValidateScope checks the conversation chain; an empty ID is allowed.
func (r InteractionRef) ValidateScope() error { return r.validate("", true) }

func (r InteractionRef) validate(path string, leafOptional bool) error {
	return checkConversationScoped(path, r.Conversation, r.ID, leafOptional)
}

// Scope returns the owning tenant.
func (r InteractionRef) Scope() TenantID { return r.Conversation.Inbox.Tenant }

// IsZero reports whether r is unset.
func (r InteractionRef) IsZero() bool { return r == InteractionRef{} }

// In reports whether r belongs to conversation c.
func (r InteractionRef) In(c ConversationRef) bool { return r.Conversation == c }

// Ref returns the generic form; the conversation is dropped.
func (r InteractionRef) Ref() Ref { return inboxRef(resourceInteraction, r.Conversation.Inbox, r.ID) }

// InteractionInput is adapter output: one structured interaction the
// provider reported, on Envelope.Interaction or to
// adapter.Sink.EvaluateInteraction. Its fields never establish identity or
// authority.
//
// ID is the provider interaction ID and Kind an interaction kind. Bot is
// the app or bot the interaction addressed, Actor the user who acted,
// Context the room, thread or similar references it happened in and
// Message the provider message the control sits on. Action, Command and
// Arguments are the provider identifiers and raw argument text; Fields the
// submitted values or typed options under a named schema.
//
// ResponseToken is the provider credential for answering, such as a
// response URL or interaction token; ResponseExpiresAt is when it stops
// working and is required with it. Core keeps the token in one place and
// never returns or emits it. Deadline is the protocol acknowledgement
// deadline.
type InteractionInput struct {
	ID                string        `json:"id"`
	Kind              string        `json:"kind"`
	Bot               *ExternalRef  `json:"bot,omitempty"`
	Action            string        `json:"action"`
	Command           string        `json:"command"`
	Arguments         string        `json:"arguments"`
	Actor             ExternalRef   `json:"actor"`
	Context           []ExternalRef `json:"context,omitempty"`
	Message           *ExternalRef  `json:"message,omitempty"`
	Fields            Extension     `json:"fields"`
	ResponseToken     string        `json:"response_token"`
	ResponseExpiresAt time.Time     `json:"response_expires_at"`
	Deadline          time.Time     `json:"deadline"`
}

// Validate checks the shape of an interaction: an ID, a declared kind, a
// deadline and a valid actor; Command for a command, Action for an
// action and named Fields for a submission; valid Bot, Message and Context
// references and Fields when named; and a response token only together
// with its expiry. Ownership of the references by the reporting channel is
// the sink's check. No error echoes the token.
func (i InteractionInput) Validate() error {
	const p = "interaction"
	kind := checkToken(fieldPath(p, "kind"), i.Kind)
	if kind == nil && !IsInteractionKind(i.Kind) {
		kind = invalidField(fieldPath(p, "kind"), "must be command, action, submission or options")
	}
	var perKind error
	switch i.Kind {
	case InteractionCommand:
		perKind = requireString(fieldPath(p, "command"), i.Command, "required for a command")
	case InteractionAction:
		perKind = requireString(fieldPath(p, "action"), i.Action, "required for an action")
	case InteractionSubmission:
		if i.Fields.Name == "" {
			perKind = invalidField(fieldPath(p, "fields.name"), "required for a submission")
		}
	}
	return firstError(
		checkRaw(fieldPath(p, "id"), i.ID, maxExternalIDBytes),
		kind,
		requireTime(fieldPath(p, "deadline"), i.Deadline.IsZero()),
		i.Actor.validate(fieldPath(p, "actor")),
		perKind,
		optionalRef(fieldPath(p, "bot"), i.Bot),
		optionalRef(fieldPath(p, "message"), i.Message),
		eachRef(fieldPath(p, "context"), i.Context),
		optionalRaw(fieldPath(p, "action"), i.Action, maxExternalIDBytes),
		optionalRaw(fieldPath(p, "command"), i.Command, maxExternalIDBytes),
		optionalRaw(fieldPath(p, "arguments"), i.Arguments, DefaultContentLimits().MaxTextBytes),
		i.Fields.validateOptional(fieldPath(p, "fields")),
		i.checkToken(p),
	)
}

func (i InteractionInput) checkToken(p string) error {
	if i.ResponseToken == "" {
		return nil
	}
	if err := checkRaw(fieldPath(p, "response_token"), i.ResponseToken, maxResponseTokenBytes); err != nil {
		return err
	}
	return requireTime(fieldPath(p, "response_expires_at"), i.ResponseExpiresAt.IsZero())
}

func requireString(field, s, reason string) error {
	if s == "" {
		return invalidField(field, reason)
	}
	return nil
}

func optionalRef(field string, r *ExternalRef) error {
	if r == nil {
		return nil
	}
	return r.validate(field)
}

func eachRef(field string, refs []ExternalRef) error {
	for n, r := range refs {
		if err := r.validate(field + "[" + strconv.Itoa(n) + "]"); err != nil {
			return err
		}
	}
	return nil
}

func optionalRaw(field, s string, limit int) error {
	if s == "" {
		return nil
	}
	return checkRaw(field, s, limit)
}

// Clone returns a deep copy.
func (i InteractionInput) Clone() InteractionInput {
	i.Bot = clonePtr(i.Bot, same)
	i.Context = slices.Clone(i.Context)
	i.Message = clonePtr(i.Message, same)
	i.Fields = i.Fields.Clone()
	return i
}

// Interaction is an accepted interaction record; Interactions.Get returns
// it. It never holds the response token: storage keeps the token beside
// the record and returns it to the effector only.
//
// Actor is the resolved actor, its kind the resolved identity's. Bot,
// Action, Command, Arguments, Fields and Context are as the adapter
// reported them (InteractionInput). Message is the local message the
// control sits on, when it resolves to exactly one; MessageExternal is
// that message as the provider named it. External holds the provider
// interaction reference. Deadline is the protocol acknowledgement deadline
// and RespondUntil the end of the provider's respond window, zero when the
// provider states none. Source is the channel type, as Message.Source.
// Redaction clears Fields data and Arguments and sets Redacted; refs,
// actor, kind, identifiers and response state stay.
type Interaction struct {
	Ref             InteractionRef      `json:"ref"`
	Kind            string              `json:"kind"`
	Actor           Author              `json:"actor"`
	Bot             *ExternalRef        `json:"bot,omitempty"`
	Action          string              `json:"action"`
	Command         string              `json:"command"`
	Arguments       string              `json:"arguments"`
	Fields          Extension           `json:"fields"`
	Message         *MessageRef         `json:"message,omitempty"`
	MessageExternal *ExternalRef        `json:"message_external,omitempty"`
	Context         []ExternalRef       `json:"context,omitempty"`
	External        []ExternalRef       `json:"external,omitempty"`
	Audience        Audience            `json:"audience"`
	OccurredAt      time.Time           `json:"occurred_at"`
	RecordedAt      time.Time           `json:"recorded_at"`
	Deadline        time.Time           `json:"deadline"`
	RespondUntil    time.Time           `json:"respond_until"`
	Response        InteractionResponse `json:"response"`
	Revision        Revision            `json:"revision"`
	Redacted        bool                `json:"redacted"`
	Source          TypeKey             `json:"source"`
	Metadata        []Extension         `json:"metadata,omitempty"`
}

// InteractionResponse is an interaction's response state: Initial is the
// operation that won the single initial response, nil until one did, and
// FollowUps counts accepted follow-up responses.
type InteractionResponse struct {
	Initial   *OperationRef `json:"initial,omitempty"`
	FollowUps uint32        `json:"follow_ups"`
}

// Clone returns a deep copy.
func (i Interaction) Clone() Interaction {
	i.Bot = clonePtr(i.Bot, same)
	i.Fields = i.Fields.Clone()
	i.Message = clonePtr(i.Message, same)
	i.MessageExternal = clonePtr(i.MessageExternal, same)
	i.Context = slices.Clone(i.Context)
	i.External = slices.Clone(i.External)
	i.Response.Initial = clonePtr(i.Response.Initial, same)
	i.Metadata = cloneEach(i.Metadata, Extension.Clone)
	return i
}

// Respond answers an interaction through the adapter that reported it.
// Mode is a ResponseMode* value; Audience is explicit, never implied by the
// mode, and restricted for ResponseModeEphemeral. FollowUp asks for a
// follow-up response instead of the single initial one. ExpiresAt bounds
// the attempt; it may not pass the interaction's RespondUntil when that is
// set.
type Respond struct {
	Command
	Interaction InteractionRef `json:"interaction"`
	Mode        string         `json:"mode"`
	Audience    Audience       `json:"audience"`
	Content     Content        `json:"content"`
	FollowUp    bool           `json:"follow_up"`
	ExpiresAt   time.Time      `json:"expires_at"`
}

// RevisionRule is RevisionAbsent: the initial response is guarded by its
// single claim, not by a revision.
func (Respond) RevisionRule() RevisionRule { return RevisionAbsent }

// Validate checks the interaction, a declared mode, the audience (restricted
// for an ephemeral response), content whose attachments belong to the
// interaction's inbox and an expiry. The respond window is the service's
// check.
func (r Respond) Validate() error {
	mode := checkToken("mode", r.Mode)
	if mode == nil && !IsResponseMode(r.Mode) {
		mode = invalidField("mode", "must be message, ephemeral or update")
	}
	audience := r.Audience.validate("audience")
	if audience == nil && r.Mode == ResponseModeEphemeral && r.Audience.Visibility != Restricted {
		audience = invalidField("audience.visibility", "must be restricted for an ephemeral response")
	}
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Interaction.validate("interaction", false),
		mode,
		audience,
		contentIn(r.Content, r.Interaction.Conversation.Inbox),
		requireTime("expires_at", r.ExpiresAt.IsZero()),
	)
}

// sameConversation validates an optional interaction ref and requires it
// to belong to conversation c.
func sameConversation(field string, r *InteractionRef, c ConversationRef) error {
	if r == nil {
		return nil
	}
	if err := r.validate(field, false); err != nil {
		return err
	}
	if !r.In(c) {
		return invalidField(field, "must belong to the conversation")
	}
	return nil
}
