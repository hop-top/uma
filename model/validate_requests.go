package model

import (
	"strconv"
	"time"
)

// Request validation checks shape, scope chains, containment, content and the
// operation's revision rule. It does not check tenant ownership against the
// principal (CheckScope), authorization, registered schemas or current state;
// the engine does those at its boundary.

func notNegative(field string, n int64) error {
	if n < 0 {
		return invalidField(field, "must not be negative")
	}
	return nil
}

func contentIn(c Content, in InboxRef) error {
	return c.validate("content", DefaultContentLimits(), &in)
}

// RevisionRule is RevisionAbsent for creation (empty channel ID) and
// RevisionRequired for updates.
func (r ConfigureChannel) RevisionRule() RevisionRule { return createOrUpdate(r.Channel.Ref.ID) }

// Validate checks a channel create or update request.
func (r ConfigureChannel) Validate() error {
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Channel.Ref.validate("channel.ref", true),
		checkOptionalToken("channel.key", r.Channel.Key),
		checkToken("channel.type", string(r.Channel.Type)),
		r.Channel.Configuration.validateOptional("channel.configuration"),
	)
}

// RevisionRule is RevisionAbsent for creation (empty inbox ID) and
// RevisionRequired for updates.
func (r ConfigureInbox) RevisionRule() RevisionRule { return createOrUpdate(r.Inbox.Ref.ID) }

// Validate checks an inbox create or update request.
func (r ConfigureInbox) Validate() error {
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Inbox.Ref.validate("inbox.ref", true),
		r.Inbox.Channel.validate("inbox.channel", false),
		checkOptionalToken("inbox.key", r.Inbox.Key),
	)
}

func createOrUpdate(id ID) RevisionRule {
	if id == "" {
		return RevisionAbsent
	}
	return RevisionRequired
}

// RevisionRule is RevisionOptional: replies append.
func (Reply) RevisionRule() RevisionRule { return RevisionOptional }

// Validate checks the conversation, that InReplyTo and InResponseTo belong
// to it, and content whose attachments belong to the conversation's inbox.
func (r Reply) Validate() error {
	var parent error
	if r.InReplyTo != nil {
		parent = r.InReplyTo.validate("in_reply_to", false)
		if parent == nil && !r.InReplyTo.In(r.Conversation) {
			parent = invalidField("in_reply_to", "must belong to the conversation")
		}
	}
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Conversation.validate("conversation", false),
		parent,
		sameConversation("in_response_to", r.InResponseTo, r.Conversation),
		contentIn(r.Content, r.Conversation.Inbox),
		r.recipients(),
	)
}

func (r Reply) recipients() error {
	if r.Recipients == nil {
		return nil
	}
	return r.Recipients.Validate()
}

// RevisionRule is RevisionAbsent: a start names no existing conversation.
func (StartConversation) RevisionRule() RevisionRule { return RevisionAbsent }

// Validate checks the inbox, a recipient on a channel of the inbox's
// tenant, and content whose attachments belong to the inbox.
func (r StartConversation) Validate() error {
	recipient := r.Recipient.validate("recipient")
	if recipient == nil && r.Recipient.Channel.Tenant != r.Inbox.Tenant {
		recipient = invalidField("recipient.channel.tenant", "must be the inbox's tenant")
	}
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Inbox.validate("inbox", false),
		recipient,
		contentIn(r.Content, r.Inbox),
	)
}

// RevisionRule is RevisionOptional: notes append.
func (InternalNote) RevisionRule() RevisionRule { return RevisionOptional }

// Validate checks the conversation, content and policy reference token.
func (r InternalNote) Validate() error {
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Conversation.validate("conversation", false),
		contentIn(r.Content, r.Conversation.Inbox),
		checkOptionalToken("policy_ref", r.PolicyRef),
	)
}

// RevisionRule is RevisionRequired: edits replace content.
func (EditMessage) RevisionRule() RevisionRule { return RevisionRequired }

// Validate checks the message and replacement content.
func (r EditMessage) Validate() error {
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Message.validate("message", false),
		contentIn(r.Content, r.Message.Conversation.Inbox),
	)
}

// RevisionRule is RevisionRequired: deletes.
func (DeleteMessage) RevisionRule() RevisionRule { return RevisionRequired }

// Validate checks the message.
func (r DeleteMessage) Validate() error {
	return firstError(r.Command.Validate(r.RevisionRule()), r.Message.validate("message", false))
}

// RevisionRule is RevisionOptional: reactions append or retract.
func (Reaction) RevisionRule() RevisionRule { return RevisionOptional }

// Validate checks the message and the emoji (ValidateEmoji).
func (r Reaction) Validate() error {
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Message.validate("message", false),
		checkEmoji("emoji", r.Emoji),
	)
}

// RevisionRule is RevisionRequired: explicit state change.
func (Transition) RevisionRule() RevisionRule { return RevisionRequired }

// Validate checks the conversation and that State is a declared status.
func (r Transition) Validate() error {
	var state error
	if !IsConversationStatus(r.State) {
		state = invalidField("state", "must be a declared conversation status")
	}
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Conversation.validate("conversation", false),
		state,
	)
}

// RevisionRule is RevisionRequired: lifecycle and field changes.
func (WorkItemAction) RevisionRule() RevisionRule { return RevisionRequired }

// Validate checks the target and the action extension.
func (r WorkItemAction) Validate() error {
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Target.validate("target", false),
		r.Action.validate("action", DefaultContentLimits().MaxStructuredBytes),
	)
}

// RevisionRule is RevisionAbsent for SchemaTaskOpen, which creates a task,
// RevisionRequired for SchemaTaskUpdate, and RevisionOptional for any other
// action name, which the engine refuses.
func (r TaskAction) RevisionRule() RevisionRule {
	switch r.Action.Name {
	case SchemaTaskOpen:
		return RevisionAbsent
	case SchemaTaskUpdate:
		return RevisionRequired
	}
	return RevisionOptional
}

// Validate checks the target and the action extension's shape. A
// SchemaTaskOpen target names the conversation only: its ID is empty, the
// engine allocates the task. Every other action names a task. The action
// data is read by Decode.
func (r TaskAction) Validate() error {
	var target error
	if r.Action.Name == SchemaTaskOpen {
		target = r.Target.validate("target", true)
		if target == nil && r.Target.ID != "" {
			target = invalidField("target.id", "must be empty: opening allocates the task")
		}
	} else {
		target = r.Target.validate("target", false)
	}
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		target,
		r.Action.validate("action", DefaultContentLimits().MaxStructuredBytes),
	)
}

// RevisionRule is RevisionOptional at the model boundary; the registered
// extension definition decides.
func (ExecuteExtension) RevisionRule() RevisionRule { return RevisionOptional }

// Validate checks target, audience and action shape.
func (r ExecuteExtension) Validate() error {
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Target.validate("target", false),
		r.Audience.validate("audience"),
		r.Action.validate("action", DefaultContentLimits().MaxStructuredBytes),
	)
}

// Validate checks target, audience and query shape.
func (r QueryExtension) Validate() error {
	return firstError(
		r.Call.Validate(),
		r.Target.validate("target", false),
		r.Audience.validate("audience"),
		r.Query.validate("query", DefaultContentLimits().MaxStructuredBytes),
	)
}

// RevisionRule is RevisionRequired: redaction deletes retained data.
func (Redact) RevisionRule() RevisionRule { return RevisionRequired }

// Validate checks the target, that Message is set exactly for a message
// target and Interaction exactly for an interaction target, each agreeing
// with it, that Cascade is set exactly for a contact, identity or
// conversation target, and the reason token.
func (r Redact) Validate() error {
	var msg error
	switch {
	case r.Target.Kind == resourceMessage && r.Message == nil:
		msg = invalidField("message", "required for a message target")
	case r.Target.Kind == resourceMessage:
		if msg = r.Message.validate("message", false); msg == nil && r.Message.Ref() != r.Target {
			msg = invalidField("message", "must name the target")
		}
	case r.Message != nil:
		msg = invalidField("message", "only allowed for a message target")
	}
	var interaction error
	switch {
	case r.Target.Kind == resourceInteraction && r.Interaction == nil:
		interaction = invalidField("interaction", "required for an interaction target")
	case r.Target.Kind == resourceInteraction:
		if interaction = r.Interaction.validate("interaction", false); interaction == nil && r.Interaction.Ref() != r.Target {
			interaction = invalidField("interaction", "must name the target")
		}
	case r.Interaction != nil:
		interaction = invalidField("interaction", "only allowed for an interaction target")
	}
	var cascade error
	switch r.Target.Kind {
	case resourceContact, resourceIdentity, resourceConversation:
		if !r.Cascade {
			cascade = invalidField("cascade", "required for a contact, identity or conversation target")
		}
	default:
		if r.Cascade {
			cascade = invalidField("cascade", "only allowed for a contact, identity or conversation target")
		}
	}
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Target.validate("target", false),
		msg,
		interaction,
		cascade,
		checkOptionalToken("reason", r.Reason),
	)
}

// RevisionRule is RevisionAbsent: opening creates a subscription.
func (OpenSubscription) RevisionRule() RevisionRule { return RevisionAbsent }

// Validate checks consumer, filter, that a start mode is chosen (there is
// no implicit default), that After is set exactly for StartCheckpoint, and
// that a payload major is declared.
func (r OpenSubscription) Validate() error {
	var major error
	start, after := validateStart(r.Start, r.After)
	if r.PayloadMajor == 0 {
		major = invalidField("payload_major", "required")
	}
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		checkToken("consumer", r.Consumer),
		r.Filter.validate("filter"),
		start,
		after,
		major,
	)
}

// Validate checks the subscription and non-negative limit and wait.
func (r Poll) Validate() error {
	return firstError(
		r.Call.Validate(),
		r.Subscription.validate("subscription", false),
		notNegative("limit", int64(r.Limit)),
		notNegative("wait", int64(r.Wait)),
	)
}

// RevisionRule is RevisionAbsent: acknowledgement is guarded by the batch
// token and checkpoint, not a revision.
func (Acknowledge) RevisionRule() RevisionRule { return RevisionAbsent }

// Validate checks the subscription and that a token is present.
func (r Acknowledge) Validate() error {
	var token error
	if r.Token == "" {
		token = invalidField("token", "required")
	}
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Subscription.validate("subscription", false),
		token,
	)
}

// RevisionRule is RevisionAbsent: preparation creates an attachment.
func (PrepareAttachment) RevisionRule() RevisionRule { return RevisionAbsent }

// Validate checks the inbox, audience and declared size.
func (r PrepareAttachment) Validate() error {
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Inbox.validate("inbox", false),
		notNegative("metadata.size", r.Metadata.Size),
		r.Metadata.Audience.validate("metadata.audience"),
	)
}

// RevisionRule is RevisionAbsent: export creates an artifact.
func (ExportRequest) RevisionRule() RevisionRule { return RevisionAbsent }

// Validate checks the filter and format token.
func (r ExportRequest) Validate() error {
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Filter.validate("filter"),
		checkToken("format", r.Format),
	)
}

// RevisionRule is RevisionAbsent: starting creates a stream.
func (StartStream) RevisionRule() RevisionRule { return RevisionAbsent }

// Validate checks the conversation, audience, mode token and deadline.
func (r StartStream) Validate() error {
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Conversation.validate("conversation", false),
		r.Audience.validate("audience"),
		checkToken("mode", r.Mode),
		notNegative("deadline", int64(r.Deadline)),
		atMost("deadline", r.Deadline, MaxStreamDeadline),
	)
}

// atMost refuses a duration above limit.
func atMost(field string, d, limit time.Duration) error {
	if d > limit {
		return invalidField(field, "must be at most "+limit.String())
	}
	return nil
}

// RevisionRule is RevisionOptional: the sequence orders updates.
func (StreamUpdate) RevisionRule() RevisionRule { return RevisionOptional }

// Validate checks the stream and content.
func (r StreamUpdate) Validate() error {
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Stream.validate("stream", false),
		contentIn(r.Content, r.Stream.Conversation.Inbox),
	)
}

// RevisionRule is RevisionOptional: the final sequence orders the end.
func (EndStream) RevisionRule() RevisionRule { return RevisionOptional }

// Validate checks the stream.
func (r EndStream) Validate() error {
	return firstError(r.Command.Validate(r.RevisionRule()), r.Stream.validate("stream", false))
}

// Validate checks the conversation, kind, audience and expiry. Signals carry
// no operation ID: they expire and never replay.
func (r Signal) Validate() error {
	return firstError(
		r.Call.Validate(),
		r.Conversation.validate("conversation", false),
		checkToken("kind", r.Kind),
		r.Audience.validate("audience"),
		requireTime("expires_at", r.ExpiresAt.IsZero()),
	)
}

// RevisionRule is RevisionOptional: ephemeral replies append.
func (Ephemeral) RevisionRule() RevisionRule { return RevisionOptional }

// Validate checks the conversation, a recipient in its inbox, an optional
// interaction of the same conversation, content and expiry.
func (r Ephemeral) Validate() error {
	recipient := r.Recipient.validate("recipient", false)
	if recipient == nil && !r.Recipient.In(r.Conversation.Inbox) {
		recipient = invalidField("recipient", "must belong to the conversation inbox")
	}
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Conversation.validate("conversation", false),
		recipient,
		sameConversation("interaction", r.Interaction, r.Conversation),
		contentIn(r.Content, r.Conversation.Inbox),
		requireTime("expires_at", r.ExpiresAt.IsZero()),
	)
}

func requireTime(field string, zero bool) error {
	if zero {
		return invalidField(field, "required")
	}
	return nil
}

// Validate checks the filter and a non-negative page limit.
func (r ListRequest) Validate() error {
	return firstError(
		r.Call.Validate(),
		r.Filter.validate("filter"),
		notNegative("page.limit", int64(r.Page.Limit)),
	)
}

// Validate checks the call and, for reference types, the ref.
// Validate checks the call and the key token.
func (l LookupKey) Validate() error {
	return firstError(l.Call.Validate(), checkToken("key", l.Key))
}

func (l Lookup[T]) Validate() error {
	var ref error
	if v, ok := any(l.Ref).(interface{ validate(string, bool) error }); ok {
		ref = v.validate("ref", false)
	}
	return firstError(l.Call.Validate(), ref)
}

// Validate checks every set reference and value in f.
func (f Filter) Validate() error { return f.validate("") }

func (f Filter) validate(path string) error {
	for i, in := range f.Inboxes {
		if err := in.validate(fieldPath(path, "inboxes["+strconv.Itoa(i)+"]"), false); err != nil {
			return err
		}
	}
	if f.Conversation != nil {
		if err := f.Conversation.validate(fieldPath(path, "conversation"), false); err != nil {
			return err
		}
	}
	if f.Contact != nil {
		if err := f.Contact.validate(fieldPath(path, "contact"), false); err != nil {
			return err
		}
	}
	if f.Identity != nil {
		if err := f.Identity.validate(fieldPath(path, "identity"), false); err != nil {
			return err
		}
	}
	if f.Labels != nil {
		if err := f.Labels.validate(fieldPath(path, "labels")); err != nil {
			return err
		}
	}
	if f.Task != nil {
		if err := f.Task.validate(fieldPath(path, "task"), false); err != nil {
			return err
		}
	}
	if f.Assignee != nil {
		if err := f.Assignee.validate(fieldPath(path, "assignee")); err != nil {
			return err
		}
	}
	if f.Unread != nil {
		if err := f.Unread.validate(fieldPath(path, "unread")); err != nil {
			return err
		}
	}
	for i, k := range f.Kinds {
		if err := checkToken(fieldPath(path, "kinds["+strconv.Itoa(i)+"]"), k); err != nil {
			return err
		}
	}
	for i, s := range f.Statuses {
		if err := checkToken(fieldPath(path, "statuses["+strconv.Itoa(i)+"]"), s); err != nil {
			return err
		}
	}
	if f.Actor != nil {
		if err := checkToken(fieldPath(path, "actor"), string(*f.Actor)); err != nil {
			return err
		}
	}
	return f.validateSearch(path)
}

// validateStart checks an event log start mode (StartBeginning, StartTail
// or StartCheckpoint, no default) and that after is set exactly for
// StartCheckpoint. It reports the start and the after error separately.
func validateStart(start string, after Cursor) (startErr, afterErr error) {
	switch start {
	case StartBeginning, StartTail:
		if after != "" {
			afterErr = invalidField("after", "only allowed with start checkpoint")
		}
	case StartCheckpoint:
		if after == "" {
			afterErr = invalidField("after", "required with start checkpoint")
		}
	case "":
		startErr = invalidField("start", "required")
	default:
		startErr = invalidField("start", "must be beginning, tail or checkpoint")
	}
	return startErr, afterErr
}
