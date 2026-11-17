// Package interactions holds the interaction record rules every maintained
// store backend applies the same way: what a stored record must look like
// and how a response claim and a message link change it.
package interactions

import (
	"math"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// CheckPut validates rec for PutInteraction in tenant: well-formed
// references that belong to the tenant, a declared kind, a revision, the
// creating ingress record's ID, and an expiry whenever a token is held. It
// reports the reference errors before any other, a foreign reference as
// model.NotFound. The error never carries the token.
func CheckPut(tenant model.TenantID, rec store.InteractionRecord) error {
	in := rec.Interaction
	if err := scoped(tenant, in.Ref); err != nil {
		return err
	}
	if !in.Actor.Identity.IsZero() {
		if err := scoped(tenant, in.Actor.Identity); err != nil {
			return err
		}
	}
	switch {
	case !model.IsInteractionKind(in.Kind):
		return invalid("interaction.kind: must be a declared interaction kind")
	case in.Revision == "":
		return invalid("interaction.revision: required")
	case rec.Ingress.Validate() != nil:
		return invalid("ingress: must be a valid ID")
	case rec.ResponseToken != "" && rec.TokenExpiresAt.IsZero():
		return invalid("token_expires_at: required with a response token")
	}
	return nil
}

// CheckClaim validates c for RecordInteractionResponse in tenant: the
// interaction and the operation are well-formed references of the tenant
// and the new revision is set.
func CheckClaim(tenant model.TenantID, c store.ResponseClaim) error {
	if err := scoped(tenant, c.Interaction); err != nil {
		return err
	}
	if err := scoped(tenant, c.Operation); err != nil {
		return err
	}
	if c.Revision == "" {
		return invalid("revision: required")
	}
	return nil
}

// Claim returns cur as RecordInteractionResponse stores it after c: the
// initial claim sets Response.Initial when unset, a follow-up counts one,
// and both store c.Revision. A redacted interaction and a second initial
// claim are model.Conflict. No revision is compared, so a follow-up never
// fails an initial claim; only c.Revision must differ from the stored one.
func Claim(cur model.Interaction, c store.ResponseClaim) (model.Interaction, error) {
	switch {
	case cur.Redacted:
		return model.Interaction{}, model.NewError(model.Conflict, "interaction is redacted")
	case c.Revision == cur.Revision:
		return model.Interaction{}, invalid("revision: must differ from the stored revision")
	}
	next := cur.Clone()
	if c.FollowUp {
		if next.Response.FollowUps == math.MaxUint32 {
			return model.Interaction{}, model.NewError(model.Conflict, "interaction follow-up count is exhausted")
		}
		next.Response.FollowUps++
	} else {
		if next.Response.Initial != nil {
			return model.Interaction{}, model.NewError(model.Conflict, "initial response already claimed")
		}
		op := c.Operation
		next.Response.Initial = &op
	}
	next.Revision = c.Revision
	return next, nil
}

// CheckLink validates l for LinkInteraction in tenant: the interaction and
// the message are well-formed references of the tenant, the message is in
// the interaction's inbox and the new revision is set.
func CheckLink(tenant model.TenantID, l store.InteractionLink) error {
	if err := scoped(tenant, l.Interaction); err != nil {
		return err
	}
	if err := scoped(tenant, l.Message); err != nil {
		return err
	}
	switch {
	case l.Message.Conversation.Inbox != l.Interaction.Conversation.Inbox:
		return invalid("message: must be in the interaction's inbox")
	case l.Revision == "":
		return invalid("revision: required")
	}
	return nil
}

// Link returns cur as LinkInteraction stores it after l, and whether that
// is a change: Message set to l.Message under l.Revision. An interaction
// already naming l.Message is returned unchanged; one naming another
// message or no provider message is model.Conflict. Redaction does not
// stop a link. l.Revision must differ from the stored one when it links.
func Link(cur model.Interaction, l store.InteractionLink) (model.Interaction, bool, error) {
	switch {
	case cur.Message != nil && *cur.Message == l.Message:
		return cur.Clone(), false, nil
	case cur.Message != nil:
		return model.Interaction{}, false, model.NewError(model.Conflict, "interaction is linked to another message")
	case cur.MessageExternal == nil:
		return model.Interaction{}, false, model.NewError(model.Conflict, "interaction names no provider message")
	case l.Revision == cur.Revision:
		return model.Interaction{}, false, invalid("revision: must differ from the stored revision")
	}
	next := cur.Clone()
	m := l.Message
	next.Message = &m
	next.Revision = l.Revision
	return next, true, nil
}

// Unlinked reports whether in names message as its provider message and
// no local message: the set Reader.UnlinkedInteractions returns.
func Unlinked(in model.Interaction, message model.ExternalRef) bool {
	return in.Message == nil && in.MessageExternal != nil && *in.MessageExternal == message
}

func scoped(tenant model.TenantID, ref interface {
	model.Scoped
	Validate() error
}) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	return model.CheckScope(tenant, ref)
}

func invalid(msg string) error { return model.NewError(model.Validation, msg) }
