package operations

import (
	"context"
	"encoding/json"
	"slices"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Membership changes: Conversations.AddParticipant and RemoveParticipant
// change who takes part in a provider conversation through the operation
// ledger, like a message mutation. Submission claims the operation key
// (kind model.ActionConversationMembership, target the conversation) and
// queues the work; it never writes the conversation. The participants
// change only once the provider applied the effect.

// IntentMembership names the stored intent of a membership operation.
const IntentMembership = "uma.intent.membership"

// participantRole is the role an added participant takes, the role
// ingress gives a sender who joins a conversation.
const participantRole = "participant"

// MembershipIntent is the durable intent of a membership operation: the
// conversation, the identity and whether it is removed.
type MembershipIntent struct {
	Conversation model.ConversationRef `json:"conversation"`
	Identity     model.IdentityRef     `json:"identity"`
	Remove       bool                  `json:"remove"`
}

// membershipAudience is the audience of a membership operation's events
// and effect: membership is desk business, never public content.
var membershipAudience = model.Audience{Visibility: model.Internal}

// MembershipAudience returns the audience of membership operations.
func MembershipAudience() model.Audience { return membershipAudience }

// ChangeMembership queues adding req.Identity to req.Conversation at the
// provider, or removing it when remove is set, under
// model.CapabilityMembership and model.ActionConversationMembership on the
// conversation. The channel's adapter must declare the capability (else
// model.Unsupported) and the identity must be stored in the conversation's
// inbox (else model.NotFound), both before anything is stored. In the
// claiming unit a set ExpectedRevision must be the conversation's, an
// added identity must not be a participant yet and a removed one must be
// one, other than the conversation's owner (each model.Conflict).
func ChangeMembership(ctx context.Context, c *engine.Client, req model.ParticipantChange, remove bool) (model.Acceptance, error) {
	if c.Engine().IsClosed() {
		return model.Acceptance{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if req.Conversation.Inbox.Tenant != c.Principal().Tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	err := c.Engine().Store().View(ctx, c.Principal().Tenant, func(r store.Reader) error {
		conv, err := r.GetConversation(ctx, req.Conversation)
		if err != nil {
			return err
		}
		if conv.Ref != req.Conversation {
			return engine.ErrNotFound()
		}
		inbox, err := r.GetInbox(ctx, conv.Ref.Inbox)
		if err != nil {
			return err
		}
		ch, err := r.GetChannel(ctx, inbox.Channel)
		if err != nil {
			return err
		}
		if err := outboundCapability(c.Engine(), ch.Type, model.CapabilityMembership); err != nil {
			return err
		}
		_, err = MembershipIdentity(ctx, r, req.Identity)
		return err
	})
	if model.IsCode(err, model.NotFound) {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	if err != nil {
		return model.Acceptance{}, err
	}
	intent, err := encodeIntent(IntentMembership, MembershipIntent{Conversation: req.Conversation, Identity: req.Identity, Remove: remove})
	if err != nil {
		return model.Acceptance{}, err
	}
	res := req.Conversation.Ref()
	return submit(ctx, c, submission{
		kind:        model.ActionConversationMembership,
		target:      res,
		owner:       res,
		command:     req.Command,
		audience:    membershipAudience,
		fingerprint: membershipFingerprint(req, remove),
		intent:      intent,
		workKind:    WorkKindOperation,
		precheck: func(ctx context.Context, tx store.Tx) error {
			conv, err := tx.GetConversation(ctx, req.Conversation)
			switch {
			case model.IsCode(err, model.NotFound) || (err == nil && conv.Ref != req.Conversation):
				return engine.ErrNotFound()
			case err != nil:
				return err
			case req.ExpectedRevision != "" && conv.Revision != req.ExpectedRevision:
				return model.NewError(model.Conflict, "conversation revision changed")
			}
			if _, err := MembershipIdentity(ctx, tx, req.Identity); err != nil {
				return err
			}
			return CheckMembership(conv, req.Identity, remove)
		},
		result: &res,
	})
}

// MembershipIdentity reads the identity a membership change names, stored
// under its exact reference, else not found.
func MembershipIdentity(ctx context.Context, r store.Reader, ref model.IdentityRef) (model.Identity, error) {
	id, err := r.GetIdentity(ctx, ref)
	switch {
	case model.IsCode(err, model.NotFound) || (err == nil && id.Ref != ref):
		return model.Identity{}, engine.ErrNotFound()
	case err != nil:
		return model.Identity{}, err
	}
	return id, nil
}

// CheckMembership refuses, with model.Conflict, a change that conv's
// stored participants make pointless or unsafe: adding a participant,
// removing an identity that is none, or removing the conversation's owner.
func CheckMembership(conv model.Conversation, id model.IdentityRef, remove bool) error {
	held := isParticipant(conv, id)
	switch {
	case !remove && held:
		return model.NewError(model.Conflict, "identity: already a participant")
	case remove && !held:
		return model.NewError(model.Conflict, "identity: not a participant")
	case remove && conv.Owner != nil && *conv.Owner == id:
		return model.NewError(model.Conflict, "identity: the conversation's owner cannot be removed")
	}
	return nil
}

func isParticipant(conv model.Conversation, id model.IdentityRef) bool {
	return slices.ContainsFunc(conv.Participants, func(p model.Participant) bool { return p.Identity == id })
}

// ParticipantRef is identity id as the adapter of channel reports it
// inbound: the channel, the identity's namespace and source ID, kind
// model.KindIdentity.
func ParticipantRef(channel model.ChannelRef, id model.Identity) model.ExternalRef {
	return model.ExternalRef{Channel: channel, Namespace: id.Namespace, Kind: string(model.KindIdentity), ID: id.SourceID}
}

// membershipFingerprint returns the version 1 fingerprint of a membership
// operation: kind, target conversation, identity, remove and expected
// revision.
func membershipFingerprint(req model.ParticipantChange, remove bool) []byte {
	f := NewFingerprint()
	f.Set("kind", Field().Str(model.ActionConversationMembership))
	f.Set("target", Field().Ref(req.Conversation.Ref()))
	f.Set("identity", Field().Ref(req.Identity.Ref()))
	f.Set("remove", Field().Present(remove))
	f.Set("expected_revision", Field().Str(string(req.ExpectedRevision)))
	return f.Sum()
}

// DecodeMembershipIntent reads a stored membership intent.
func DecodeMembershipIntent(e model.Extension) (MembershipIntent, error) {
	return decodeIntent[MembershipIntent](e, IntentMembership)
}

// SettleMembership applies an applied membership operation to the stored
// conversation: the identity joins the participants (role participant) or
// leaves them, the conversation moves to a new revision and
// conversation.changed participants is emitted, command-triggered by the
// caller under the operation, internal audience. Nothing applies to an
// operation that was not applied, to a conversation gone since, or when
// the participants already are what was asked, such as after an inbound
// message from the added participant.
func SettleMembership(ctx context.Context, tx store.Tx, e *engine.Engine, op model.Operation, in MembershipIntent) error {
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
	held := isParticipant(cur, in.Identity)
	if held != in.Remove {
		return nil
	}
	next := cur.Clone()
	if in.Remove {
		next.Participants = slices.DeleteFunc(next.Participants, func(p model.Participant) bool { return p.Identity == in.Identity })
	} else {
		next.Participants = append(next.Participants, model.Participant{Identity: in.Identity, Role: participantRole})
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
		Conversation: next.Ref, Changes: []string{model.ConversationFieldParticipants}, Trigger: model.TriggerCommand,
		PreviousRevision: cur.Revision, Operation: &opRef,
	})
	if err != nil {
		return model.NewError(model.Permanent, "event payload encoding failed")
	}
	now := e.Clock().Now().UTC()
	res := next.Ref.Ref()
	return tx.AppendEvents(ctx, []model.Event{{
		Version: model.EventEnvelopeVersion, ID: id, Kind: model.EventConversationChanged,
		Tenant: res.Tenant, Inbox: res.Inbox, Resource: res, Actor: rec.Key.Caller,
		OccurredAt: now, RecordedAt: now, Audience: membershipAudience, Revision: next.Revision,
		Payload: model.Extension{Name: model.SchemaConversationChanged, Version: model.PayloadVersion1, Data: data},
	}})
}
