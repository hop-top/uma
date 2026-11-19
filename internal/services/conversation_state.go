package services

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/route"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Archive queues archiving a conversation at the provider, or unarchiving
// it when req.Undo is set, as an outbound operation; operations.ChangeState
// holds its rules and gateState the gate in front of it.
func (s Conversations) Archive(ctx context.Context, req model.ArchiveConversation) (model.Acceptance, error) {
	if err := s.open(req.Validate()); err != nil {
		return model.Acceptance{}, err
	}
	verb := model.StateVerbArchive
	if req.Undo {
		verb = model.StateVerbUnarchive
	}
	return s.changeState(ctx, operations.StateRequest{Command: req.Command, Conversation: req.Conversation, Verb: verb, Messages: req.Messages})
}

// Move queues moving a conversation at the provider, as Archive.
func (s Conversations) Move(ctx context.Context, req model.MoveConversation) (model.Acceptance, error) {
	if err := s.open(req.Validate()); err != nil {
		return model.Acceptance{}, err
	}
	return s.changeState(ctx, operations.StateRequest{
		Command: req.Command, Conversation: req.Conversation, Verb: model.StateVerbMove,
		Destination: req.Destination, Add: req.Add, Remove: req.Remove, Messages: req.Messages,
	})
}

// SetRead queues marking a conversation read or unread at the provider, as
// Archive. It never changes the per-actor read marker.
func (s Conversations) SetRead(ctx context.Context, req model.SetConversationRead) (model.Acceptance, error) {
	if err := s.open(req.Validate()); err != nil {
		return model.Acceptance{}, err
	}
	verb := model.StateVerbUnread
	if req.Read {
		verb = model.StateVerbRead
	}
	return s.changeState(ctx, operations.StateRequest{Command: req.Command, Conversation: req.Conversation, Verb: verb, Messages: req.Messages})
}

// Flag queues flagging, or unflagging, a conversation at the provider, as
// Archive.
func (s Conversations) Flag(ctx context.Context, req model.FlagConversation) (model.Acceptance, error) {
	if err := s.open(req.Validate()); err != nil {
		return model.Acceptance{}, err
	}
	verb := model.StateVerbFlag
	if req.Undo {
		verb = model.StateVerbUnflag
	}
	return s.changeState(ctx, operations.StateRequest{
		Command: req.Command, Conversation: req.Conversation, Verb: verb, Kind: req.Kind, Messages: req.Messages,
	})
}

// Trash queues moving a conversation to the provider's trash, or restoring
// it, as Archive.
func (s Conversations) Trash(ctx context.Context, req model.TrashConversation) (model.Acceptance, error) {
	if err := s.open(req.Validate()); err != nil {
		return model.Acceptance{}, err
	}
	verb := model.StateVerbTrash
	if req.Undo {
		verb = model.StateVerbUntrash
	}
	return s.changeState(ctx, operations.StateRequest{Command: req.Command, Conversation: req.Conversation, Verb: verb, Messages: req.Messages})
}

// open reports the closed engine before it reports invalid, as every
// method does, else validation.
func (s Conversations) open(validation error) error {
	if s.c.Engine().IsClosed() {
		return engine.ErrClosed()
	}
	return validation
}

// changeState gates req on what the channel's adapter reported for the
// verb, then queues it. The gate resolves the scope that picks the policy
// action and the shape a move is checked against.
func (s Conversations) changeState(ctx context.Context, req operations.StateRequest) (model.Acceptance, error) {
	limits, err := s.gateState(ctx, req)
	if err != nil {
		return model.Acceptance{}, err
	}
	req.Scope, req.Shape = limits.Scope, limits.Shape
	return operations.ChangeState(ctx, s.c, req)
}

// gateState is the fail-closed gate of a state command, run before
// anything is stored (0007). The conversation must be stored in the
// caller's tenant (else model.NotFound) and readable by the caller (the
// check that keeps a caller who could not read it from causing provider
// I/O). The channel's adapter must declare the verb's capability, report
// it available with readable limits (model.Unsupported otherwise; a
// withdrawn permission is model.Unauthorized) and accept the request's
// shape, role and kind against them.
func (s Conversations) gateState(ctx context.Context, req operations.StateRequest) (model.StateLimits, error) {
	c, e := s.c, s.c.Engine()
	capability, ok := req.Verb.Capability()
	if !ok {
		return model.StateLimits{}, model.NewError(model.Unsupported, "state: undeclared verb")
	}
	if req.Conversation.Inbox.Tenant != c.Principal().Tenant {
		return model.StateLimits{}, engine.ErrNotFound()
	}
	var rt route.Route
	if err := view(ctx, c, func(r store.Reader) error {
		var err error
		rt, err = route.Load(ctx, r, req.Conversation)
		if err = exactRef(err, rt.Conv.Ref == req.Conversation); err != nil {
			return err
		}
		return operations.CheckStateMessages(ctx, r, rt.Conv, req.Messages)
	}); err != nil {
		return model.StateLimits{}, err
	}
	if err := route.Declares(e, rt.Channel.Type, capability); err != nil {
		return model.StateLimits{}, err
	}
	if err := c.Authorize(ctx, conversationRead(rt.Conv), req.Conversation.Ref()); err != nil {
		return model.StateLimits{}, err
	}
	done, err := e.Begin()
	if err != nil {
		return model.StateLimits{}, err
	}
	defer done()
	inst, desc, _, err := runtime.OpenOutbound(ctx, e, rt.Channel)
	if err != nil {
		return model.StateLimits{}, runtime.AsFailure(err)
	}
	defer func() { _ = inst.Close(context.WithoutCancel(ctx)) }()
	st, f := runtime.CapabilityStatusOf(ctx, desc, inst, capability)
	if f != nil {
		return model.StateLimits{}, f
	}
	limits, err := model.ParseStateLimits(st.Limits)
	if err != nil {
		return model.StateLimits{}, &model.Error{Code: model.Unsupported, SafeMessage: string(capability) + " reported no usable limits", Retry: model.NoRetry}
	}
	if err := req.CheckLimits(limits); err != nil {
		return model.StateLimits{}, err
	}
	return limits, nil
}
