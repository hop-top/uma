package interactions

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/route"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// intentEphemeral names the stored intent of an ephemeral reply.
const intentEphemeral = "uma.intent.ephemeral"

// ephemeralIntent is the durable intent of an ephemeral reply.
type ephemeralIntent struct {
	Conversation model.ConversationRef `json:"conversation"`
	Recipient    model.IdentityRef     `json:"recipient"`
	Content      model.Content         `json:"content"`
	ExpiresAt    time.Time             `json:"expires_at"`
	// Interaction is the interaction a follow-up answers; nil for an
	// independent ephemeral reply.
	Interaction *model.InteractionRef `json:"interaction,omitempty"`
}

func decodeEphemeralIntent(e model.Extension) (ephemeralIntent, error) {
	var in ephemeralIntent
	if e.Name != intentEphemeral || e.Version != 1 {
		return in, &model.Error{Code: model.SchemaMismatch, SafeMessage: "unknown operation intent", Retry: model.NoRetry}
	}
	if err := json.Unmarshal(e.Data, &in); err != nil {
		return in, &model.Error{Code: model.Permanent, SafeMessage: "stored operation intent unreadable", Retry: model.NoRetry}
	}
	return in, nil
}

// ephemeralAudience is the audience of every ephemeral reply: it is shown
// to its recipient only, never to the conversation.
func ephemeralAudience() model.Audience { return model.Audience{Visibility: model.Restricted} }

// Ephemeral queues a reply shown only to the recipient, a stored
// participant of the conversation. The channel's adapter must declare
// send_ephemeral: there is no fallback to a public reply or a direct
// message. The reply is never stored as a message; it is not attempted
// once ExpiresAt has passed, and one already expired is rejected with
// model.DeadlineExpired.
//
// An Interaction ties the reply to that interaction as a follow-up
// response instead: the adapter must also declare respond_follow_up, the
// recipient must be the interaction's actor (a stored participant or not),
// and the principal needs interaction.respond on the interaction. The
// interaction counts one follow-up with the operation and the adapter
// receives the interaction, with its response token, at the attempt; an
// attempt past the interaction's respond window or after its redaction
// fails. It never claims the initial response. Without an Interaction no
// interaction state is read, and the token is never touched.
func (s Service) Ephemeral(ctx context.Context, req model.Ephemeral) (model.Acceptance, error) {
	e := s.c.Engine()
	if e.IsClosed() {
		return model.Acceptance{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if req.Conversation.Inbox.Tenant != s.c.Principal().Tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	if !e.Clock().Now().Before(req.ExpiresAt) {
		return model.Acceptance{}, expiredError()
	}
	var rt route.Route
	var answered *model.Interaction
	err := e.Store().View(ctx, s.c.Principal().Tenant, func(r store.Reader) error {
		var err error
		if rt, err = route.Load(ctx, r, req.Conversation); err != nil {
			return err
		}
		if req.Interaction == nil {
			if err := route.Declares(e, rt.Channel.Type, model.CapabilitySendEphemeral); err != nil {
				return err
			}
			_, err = recipient(ctx, r, rt.Conv, req.Recipient)
			return err
		}
		in, err := r.Interaction(ctx, *req.Interaction)
		if err != nil {
			return err
		}
		answered = &in
		if err := route.Declares(e, rt.Channel.Type, model.CapabilitySendEphemeral, model.CapabilityRespondFollowUp); err != nil {
			return err
		}
		return interactionRecipient(ctx, r, in, req.Recipient)
	})
	if err != nil {
		return model.Acceptance{}, route.NotFound(err)
	}
	if answered != nil {
		if answered.Ref != *req.Interaction {
			return model.Acceptance{}, engine.ErrNotFound()
		}
		if err := s.c.Authorize(ctx, authorizeRespond(*answered), answered.Ref.Ref()); err != nil {
			return model.Acceptance{}, err
		}
	}
	audience := ephemeralAudience()
	intent := ephemeralIntent{
		Conversation: req.Conversation, Recipient: req.Recipient, Content: req.Content.Clone(), ExpiresAt: req.ExpiresAt.UTC(),
		Interaction: req.Interaction,
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return model.Acceptance{}, model.NewError(model.Validation, "ephemeral content cannot be stored")
	}
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionMessageEphemeral))
	f.Set("target", operations.Field().Ref(req.Conversation.Ref()))
	f.Set("recipient", operations.Field().Str(string(req.Recipient.Inbox.Tenant)).Str(string(req.Recipient.Inbox.ID)).Str(string(req.Recipient.ID)))
	f.Set("content", operations.Field().Content(req.Content))
	f.Set("expires_at", operations.Field().Str(intent.ExpiresAt.Format(time.RFC3339Nano)))
	f.Set("audience", operations.Field().Audience(audience))
	f.Set("expected_revision", operations.Field().Str(string(req.ExpectedRevision)))
	if req.Interaction != nil {
		// Present only when set, so an independent reply keeps its fingerprint.
		f.Set("interaction", operations.Field().Ref(req.Interaction.Ref()))
	}
	expected := req.ExpectedRevision
	sub := operations.Submission{
		Kind: model.ActionMessageEphemeral, Target: req.Conversation.Ref(), Owner: rt.Conv.Ref.Ref(),
		Command: req.Command, Audience: audience, Fingerprint: f.Sum(),
		Intent: model.Extension{Name: intentEphemeral, Version: 1, Data: data},
		Precheck: func(ctx context.Context, tx store.Tx) error {
			if expected != "" {
				cur, err := tx.GetConversation(ctx, req.Conversation)
				if err != nil {
					return err
				}
				if cur.Revision != expected {
					return model.NewError(model.Conflict, "conversation revision changed")
				}
			}
			if req.Interaction == nil {
				return nil
			}
			return precheckResponse(ctx, tx, e, *req.Interaction, req.ExpiresAt)
		},
	}
	if req.Interaction != nil {
		eventID, err := operations.NewID(ctx, e)
		if err != nil {
			return model.Acceptance{}, err
		}
		actor := s.c.Principal().Actor
		sub.Apply = func(ctx context.Context, tx store.Tx, op model.OperationRef, rev model.Revision) error {
			return claimResponse(ctx, tx, e, actor, req.Correlation, *req.Interaction, op, true, rev, eventID)
		}
	}
	return submit(ctx, s.c, sub)
}

// interactionRecipient requires ref to be the actor of in, whose identity
// record must be stored: a follow-up answers the person who acted, a
// participant of the conversation or not.
func interactionRecipient(ctx context.Context, r store.Reader, in model.Interaction, ref model.IdentityRef) error {
	if in.Actor.Identity != ref {
		return model.NewError(model.Validation, "recipient: must be the actor of the interaction")
	}
	_, err := r.GetIdentity(ctx, ref)
	return err
}

// recipient resolves ref, which must be the conversation's owner or one of
// its stored participants, to its identity record. The engine never
// substitutes another identity, even one linked to the same contact.
func recipient(ctx context.Context, r store.Reader, conv model.Conversation, ref model.IdentityRef) (model.Identity, error) {
	member := conv.Owner != nil && *conv.Owner == ref ||
		slices.ContainsFunc(conv.Participants, func(p model.Participant) bool { return p.Identity == ref })
	if !member {
		return model.Identity{}, model.NewError(model.Validation, "recipient: not a participant of the conversation")
	}
	id, err := r.GetIdentity(ctx, ref)
	if model.IsCode(err, model.NotFound) {
		return model.Identity{}, engine.ErrNotFound()
	}
	return id, err
}

// recipientRef is the provider address of identity on channel: its
// namespace and source ID, under kind identity.
func recipientRef(channel model.ChannelRef, id model.Identity) model.ExternalRef {
	return model.ExternalRef{Channel: channel, Namespace: id.Namespace, Kind: string(model.KindIdentity), ID: id.SourceID}
}
