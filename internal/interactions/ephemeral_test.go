package interactions_test

import (
	"testing"
	"time"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

func (r *rig) ephemeral(to model.IdentityRef, ttl time.Duration) model.Ephemeral {
	return model.Ephemeral{
		Command: r.cmd(), Conversation: r.conv.Ref, Recipient: to,
		Content: content("only you"), ExpiresAt: r.clock.Now().Add(ttl),
	}
}

// An ephemeral reply reaches its recipient through the ephemeral sender
// and is never stored as a message or announced as one.
func TestEphemeralReachesRecipientOnly(t *testing.T) {
	r := newRig(t, everything...)
	a, err := r.s.Ephemeral(r.ctx, r.ephemeral(r.member.Ref, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	wantState(t, a.Operation, model.Queued)
	if a.Operation.ResultMessage != nil || a.Operation.Result != nil {
		t.Fatalf("ephemeral result = %+v, want no message", a.Operation)
	}
	r.drain()
	calls := r.f.recorded()
	if len(calls) != 1 || calls[0].method != "ephemeral" {
		t.Fatalf("provider calls = %+v, want one ephemeral send", calls)
	}
	if to := calls[0].recipient; to.ID != r.member.SourceID || to.Namespace != r.member.Namespace || to.Channel != r.conv.ReplyRoute.Channel {
		t.Fatalf("recipient = %+v, want the participant's identity", to)
	}
	if calls[0].audience.Visibility != model.Restricted {
		t.Fatalf("audience = %+v, want restricted", calls[0].audience)
	}
	wantState(t, r.op(a), model.ProviderAccepted)
	msgs, evs := r.recorded()
	if len(msgs) != 0 {
		t.Fatalf("ephemeral stored messages %+v", msgs)
	}
	for _, ev := range evs {
		if ev.Kind != model.EventDeliveryUpdated || ev.Audience.Visibility != model.Restricted {
			t.Fatalf("event %s under %+v, want only restricted delivery updates", ev.Kind, ev.Audience)
		}
	}
}

// Without send_ephemeral the reply is rejected: no public reply, no
// direct message.
func TestEphemeralUnsupportedHasNoFallback(t *testing.T) {
	r := newRig(t, model.CapabilitySend, model.CapabilityEdit, model.CapabilityStream, model.CapabilitySignal)
	_, err := r.s.Ephemeral(r.ctx, r.ephemeral(r.member.Ref, time.Hour))
	enginetest.RequireCode(t, err, model.Unsupported, "send_ephemeral")
	r.drain()
	if n := len(r.f.recorded()); n != 0 {
		t.Fatalf("provider calls = %v, want none", r.f.methods())
	}
}

// The recipient must be a stored participant of the conversation.
func TestEphemeralRecipientIsParticipant(t *testing.T) {
	r := newRig(t, everything...)
	_, err := r.s.Ephemeral(r.ctx, r.ephemeral(r.other.Ref, time.Hour))
	enginetest.RequireCode(t, err, model.Validation, "recipient")
}

// A participant whose identity record is not stored is not found: the
// engine never substitutes another identity.
func TestEphemeralRecipientMustBeStored(t *testing.T) {
	r := newRig(t, everything...)
	ghost := model.IdentityRef{Inbox: r.conv.Ref.Inbox, ID: "00000003-0000-7000-8000-000000000009"}
	conv := r.conv.Clone()
	conv.Participants = append(conv.Participants, model.Participant{Identity: ghost, Role: "participant"})
	conv.Revision = "cv2"
	r.tx(func(tx store.Tx) error { return tx.PutConversation(r.ctx, conv, r.conv.Revision) })
	r.conv = conv
	_, err := r.s.Ephemeral(r.ctx, r.ephemeral(ghost, time.Hour))
	enginetest.RequireCode(t, err, model.NotFound, "")
	r.drain()
	if n := len(r.f.recorded()); n != 0 {
		t.Fatalf("provider calls = %v, want none", r.f.methods())
	}
}

// An ephemeral reply is not attempted at or after its deadline.
func TestEphemeralDeadline(t *testing.T) {
	r := newRig(t, everything...)
	_, err := r.s.Ephemeral(r.ctx, r.ephemeral(r.member.Ref, 0))
	enginetest.RequireCode(t, err, model.DeadlineExpired, "deadline")
	a, err := r.s.Ephemeral(r.ctx, r.ephemeral(r.member.Ref, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(time.Second)
	r.drain()
	op := r.op(a)
	if op.State != model.Failed || op.Failure == nil || op.Failure.Code != model.DeadlineExpired {
		t.Fatalf("expired ephemeral = %+v, want failed with deadline_expired", op)
	}
	if n := len(r.f.recorded()); n != 0 {
		t.Fatalf("provider calls = %v, want none", r.f.methods())
	}
}

// An ephemeral reply naming no interaction needs none: with only
// send_ephemeral declared and no interaction stored it is delivered
// without an interaction in the effect, and the token custody and the
// response claim are never touched.
func TestEphemeralWithoutInteractionSucceeds(t *testing.T) {
	r := newRig(t, model.CapabilitySend, model.CapabilitySendEphemeral)
	a, err := r.s.Ephemeral(r.ctx, r.ephemeral(r.member.Ref, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	r.drain()
	wantState(t, r.op(a), model.ProviderAccepted)
	calls := r.f.recorded()
	if len(calls) != 1 || calls[0].method != "ephemeral" || calls[0].ic != nil {
		t.Fatalf("provider calls = %+v, want one ephemeral send without an interaction", calls)
	}
	if r.spy.TokenReads() != 0 || r.spy.Claims() != 0 {
		t.Fatalf("token reads %d, claims %d; want neither", r.spy.TokenReads(), r.spy.Claims())
	}
	if n := len(r.eventsOf(model.EventInteractionUpdated)); n != 0 {
		t.Fatalf("interaction.updated events = %d, want none", n)
	}
}

// ephemeralFor answers in's actor, for an hour.
func (r *rig) ephemeralFor(in model.Interaction) model.Ephemeral {
	req := r.ephemeral(in.Actor.Identity, time.Hour)
	req.Interaction = &in.Ref
	return req
}

// An ephemeral reply naming an interaction is a follow-up to its actor,
// whether or not the actor is a participant: it counts one follow-up and
// emits interaction.updated, hands the adapter the interaction with its
// token at the attempt, and never claims the initial response.
func TestEphemeralNamingInteractionUsesToken(t *testing.T) {
	r := newRig(t, respondCaps()...)
	in := r.seed(func(rec *store.InteractionRecord) {
		actor := r.other
		rec.Interaction.Actor.Identity = actor.Ref
	})
	a, err := r.s.Ephemeral(r.ctx, r.ephemeralFor(in))
	if err != nil {
		t.Fatal(err)
	}
	got := r.stored(in.Ref)
	if got.Response.Initial != nil || got.Response.FollowUps != 1 || got.Revision == in.Revision {
		t.Fatalf("after the ephemeral: %+v revision %s, want one follow-up and no initial", got.Response, got.Revision)
	}
	ups := r.eventsOf(model.EventInteractionUpdated)
	if len(ups) != 1 {
		t.Fatalf("interaction.updated events = %d, want 1", len(ups))
	}
	if u := decode[model.InteractionUpdatedPayload](t, ups[0]); u.Change != model.InteractionChangeFollowUp || u.Operation == nil ||
		*u.Operation != a.Operation.Ref || u.PreviousRevision != in.Revision {
		t.Fatalf("interaction.updated payload = %+v", u)
	}
	if asks := r.policy.asked(model.ActionInteractionRespond); len(asks) == 0 || asks[0].Resource != in.Ref.Ref() {
		t.Fatalf("interaction.respond authorizations = %+v, want one on the interaction", asks)
	}
	if asks := r.policy.asked(model.ActionMessageEphemeral); len(asks) == 0 {
		t.Fatal("message.ephemeral was never authorized")
	}
	r.drain()
	wantState(t, r.op(a), model.ProviderAccepted)
	calls := r.f.recorded()
	if len(calls) != 1 || calls[0].method != "ephemeral" || calls[0].ic == nil || calls[0].ic.Token != enginetest.InteractionToken ||
		calls[0].ic.Ref != in.Ref {
		t.Fatalf("provider calls = %+v, want one ephemeral send carrying the interaction and its token", calls)
	}
	if to := calls[0].recipient; to.ID != r.other.SourceID || to.Namespace != r.other.Namespace {
		t.Fatalf("recipient = %+v, want the interaction's actor", to)
	}
	if r.spy.Claims() != 1 {
		t.Fatalf("response claims = %d, want the one follow-up", r.spy.Claims())
	}
	if msgs, _ := r.recorded(); len(msgs) != 0 {
		t.Fatalf("ephemeral stored messages %+v", msgs)
	}
	r.requireNoToken(a.Operation.Ref)
	// The initial response is still open to Respond.
	r.respond(r.respondReq(in, model.ResponseModeMessage))
}

// The follow-up needs respond_follow_up and the interaction's own actor as
// recipient; refusals count and claim nothing.
func TestEphemeralNamingInteractionRules(t *testing.T) {
	r := newRig(t, model.CapabilitySend, model.CapabilitySendEphemeral, model.CapabilityRespond)
	in := r.seed()
	_, err := r.s.Ephemeral(r.ctx, r.ephemeralFor(in))
	enginetest.RequireCode(t, err, model.Unsupported, "respond_follow_up")

	r = newRig(t, respondCaps()...)
	in = r.seed()
	req := r.ephemeralFor(in)
	req.Recipient = r.other.Ref
	_, err = r.s.Ephemeral(r.ctx, req)
	enginetest.RequireCode(t, err, model.Validation, "recipient")

	req = r.ephemeralFor(in)
	req.Interaction = &model.InteractionRef{Conversation: r.conv.Ref, ID: "00000005-0000-7000-8000-0000000000f9"}
	_, err = r.s.Ephemeral(r.ctx, req)
	enginetest.RequireCode(t, err, model.NotFound, "resource not found")

	r.policy.deny = func(a policy.Authorization) bool { return a.Action == model.ActionInteractionRespond }
	_, err = r.s.Ephemeral(r.ctx, r.ephemeralFor(in))
	enginetest.RequireCode(t, err, model.Unauthorized, "")

	if got := r.stored(in.Ref); got.Response.FollowUps != 0 || got.Revision != in.Revision || r.spy.Claims() != 0 {
		t.Fatalf("interaction after refusals = %+v (claims %d), want untouched", got.Response, r.spy.Claims())
	}
	r.drain()
	if calls := r.f.recorded(); len(calls) != 0 {
		t.Fatalf("provider calls = %+v, want none", calls)
	}
}

// A follow-up ephemeral is not attempted once the interaction's window is
// gone or the interaction was redacted.
func TestEphemeralNamingInteractionExpiresAndRedacts(t *testing.T) {
	r := newRig(t, respondCaps()...)
	late, gone := r.seed(), r.seed()
	a, err := r.s.Ephemeral(r.ctx, r.ephemeralFor(late))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.s.Ephemeral(r.ctx, r.ephemeralFor(gone))
	if err != nil {
		t.Fatal(err)
	}
	r.tx(func(tx store.Tx) error {
		_, err := tx.Redact(r.ctx, store.Redaction{Interaction: &gone.Ref, Expected: r.stored(gone.Ref).Revision, Revision: "ix-redacted"})
		return err
	})
	r.drain()
	if op := r.op(b); op.State != model.Failed || op.Failure == nil || op.Failure.Code != model.Permanent {
		t.Fatalf("follow-up after redaction = %+v, want failed permanent", op)
	}
	wantState(t, r.op(a), model.ProviderAccepted)

	r2 := newRig(t, respondCaps()...)
	in := r2.seed(func(rec *store.InteractionRecord) {
		w := r2.clock.Now().Add(10 * time.Minute)
		rec.Interaction.RespondUntil, rec.TokenExpiresAt = w, w
	})
	c, err := r2.s.Ephemeral(r2.ctx, r2.ephemeralFor(in))
	if err != nil {
		t.Fatal(err)
	}
	r2.clock.Advance(11 * time.Minute)
	r2.drain()
	if op := r2.op(c); op.State != model.Failed || op.Failure == nil || op.Failure.Code != model.DeadlineExpired {
		t.Fatalf("follow-up after the window = %+v, want failed deadline_expired", op)
	}
	if n := len(r2.f.recorded()); n != 0 {
		t.Fatalf("provider calls = %v, want none", r2.f.methods())
	}
}
