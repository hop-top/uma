package engine

import (
	"context"
	"errors"

	"hop.top/uma/model"
	"hop.top/uma/policy"
)

// Client is an actor-bound view of an engine. It holds the trusted
// principal and the engine, never a cached authorization grant.
type Client struct {
	e         *Engine
	principal model.Principal
}

// Engine returns the engine the client is bound to.
func (c *Client) Engine() *Engine { return c.e }

// Principal returns the client's trusted principal.
func (c *Client) Principal() model.Principal { return c.principal }

// Authorize admits one operation of the client's principal on req.Resource.
// owner is the resource's scope as storage records it (for a resource being
// created, the scope it is created in). It is Decide with every decision
// but an explicit allow mapped to model.Unauthorized.
func (c *Client) Authorize(ctx context.Context, req policy.Authorization, owner model.Ref) error {
	verdict, err := c.Decide(ctx, req, owner)
	if err != nil {
		return err
	}
	if !verdict.Allow {
		return model.NewError(model.Unauthorized, "denied by policy")
	}
	return nil
}

// Decide asks the Authorizer about one operation of the client's principal
// on req.Resource and returns its decision, so a caller can record the
// policy provenance of an allow. owner is as for Authorize.
//
// Scope comes first and holds whatever the policy says: a resource in
// another tenant, or one whose recorded tenant, inbox, kind or ID differs
// from the requested reference, is not found and the policy is not asked
// about it; so is a destination in another tenant. Then the Authorizer
// decides for the client's principal, overriding any principal on req.
// Only an explicit allow admits: callers must treat a zero Decision as a
// denial. An Authorizer error is returned mapped, never as a decision.
func (c *Client) Decide(ctx context.Context, req policy.Authorization, owner model.Ref) (policy.Decision, error) {
	done, err := c.e.Begin()
	if err != nil {
		return policy.Decision{}, err
	}
	defer done()
	if err := req.Resource.Validate(); err != nil {
		return policy.Decision{}, err
	}
	if req.Resource.Tenant != c.principal.Tenant || owner != req.Resource {
		return policy.Decision{}, ErrNotFound()
	}
	if d := req.Destination; d != nil && d.Tenant != c.principal.Tenant {
		return policy.Decision{}, ErrNotFound()
	}
	req.Principal = c.principal
	verdict, err := c.e.authorizer.Authorize(ctx, req)
	if err != nil {
		return policy.Decision{}, policyError(ctx, err)
	}
	return verdict, nil
}

// policyError maps an Authorizer failure to a denial. Context errors stay
// standard; a typed error with a declared code passes through; anything else
// becomes model.Unavailable without the policy's error text.
func policyError(ctx context.Context, err error) error {
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return err
	}
	var me *model.Error
	if errors.As(err, &me) && me.Code.Valid() {
		return me
	}
	return model.NewError(model.Unavailable, "authorization unavailable")
}

// ErrNotFound never confirms that a foreign or mismatched resource exists.
func ErrNotFound() error { return model.NewError(model.NotFound, "resource not found") }

// Unsupported reports op as not implemented, or the closed-engine error once
// Close has started.
func (c *Client) Unsupported(op string) error { return c.e.Unsupported(op) }

// ReadAudience is the audience a stored message is authorized under. A
// missing or unknown visibility is restricted: unknown visibility fails
// closed.
func ReadAudience(a model.Audience) model.Audience {
	if a.Visibility.Validate() != nil {
		return model.Audience{Visibility: model.Restricted, PolicyRef: a.PolicyRef}
	}
	return a
}

// MessageRead authorizes reading one message under its own audience, so an
// internal note is never readable merely because its conversation is.
func MessageRead(m model.Message) policy.Authorization {
	return policy.Authorization{
		Action: model.ActionMessageRead, Resource: m.Ref.Ref(), Audience: ReadAudience(m.Audience), Revision: m.Revision,
	}
}

// InteractionRead authorizes reading one interaction under its own
// audience.
func InteractionRead(i model.Interaction) policy.Authorization {
	return policy.Authorization{
		Action: model.ActionInteractionRead, Resource: i.Ref.Ref(), Audience: ReadAudience(i.Audience), Revision: i.Revision,
	}
}

// ConversationRead authorizes reading one conversation.
func ConversationRead(c model.Conversation) policy.Authorization {
	return policy.Authorization{Action: model.ActionConversationRead, Resource: c.Ref.Ref(), Revision: c.Revision}
}

// TaskRead authorizes reading one task.
func TaskRead(t model.Task) policy.Authorization {
	return policy.Authorization{Action: model.ActionTaskRead, Resource: t.Ref.Ref(), Revision: t.Revision}
}
