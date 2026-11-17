package adapter

import (
	"context"
	"fmt"
	"time"

	"hop.top/uma/model"
)

// InteractionContext is the interaction an effect answers, as the
// effector loaded it at attempt time. ProviderID is the provider
// interaction ID (model.InteractionInput.ID), such as a callback path or
// callback query ID. Token is the provider response credential, empty for
// providers without one; it lives in memory only, never in attempt
// evidence, and formatting a context never prints it. ExpiresAt is the
// interaction's respond window (model.Interaction.RespondUntil), zero when
// the provider states none. Message is the provider message the control
// sits on, the target of model.ResponseModeUpdate.
type InteractionContext struct {
	Ref        model.InteractionRef
	ProviderID string
	Kind       string
	Token      string
	ExpiresAt  time.Time
	Message    *model.ExternalRef
}

// String formats the context without its token.
func (c InteractionContext) String() string {
	return fmt.Sprintf("{Ref:%v ProviderID:%s Kind:%s Token:[redacted] ExpiresAt:%v Message:%v}",
		c.Ref, c.ProviderID, c.Kind, c.ExpiresAt, c.Message)
}

// GoString formats the context without its token.
func (c InteractionContext) GoString() string { return "adapter.InteractionContext" + c.String() }

// Clone returns a deep copy.
func (c InteractionContext) Clone() InteractionContext {
	if c.Message != nil {
		m := *c.Message
		c.Message = &m
	}
	return c
}

// Clone returns a deep copy.
func (e Effect) Clone() Effect {
	if e.Target != nil {
		t := *e.Target
		e.Target = &t
	}
	if e.Interaction != nil {
		ic := e.Interaction.Clone()
		e.Interaction = &ic
	}
	return e
}

// RespondRequest answers the interaction in Effect.Interaction.
// Mode is a model.ResponseMode* value and Effect.Audience the explicit
// audience; for model.ResponseModeUpdate Effect.Target is the provider
// message to replace. FollowUp marks a follow-up response, sent only by an
// adapter declaring model.CapabilityRespondFollowUp; otherwise it is the
// single initial response.
type RespondRequest struct {
	Effect
	Mode     string
	Content  model.Content
	FollowUp bool
}

// InteractionResponder answers interactions (model.CapabilityRespond, and
// with model.CapabilityRespondFollowUp follow-ups). It never falls back to
// another mode or a wider audience: a response the provider cannot give as
// asked, such as one past an exhausted provider window, is a rejected
// outcome.
type InteractionResponder interface {
	Respond(context.Context, RespondRequest) (Outcome, error)
}
