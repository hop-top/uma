package conformance

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// respondToken is the response token the respond cases store beside their
// interactions. It must reach the adapter's call and nothing else.
const respondToken = "tok-conformance-respond-0001"

// responder seeds channel A with a conversation to peer-1 whose participant
// is the actor of the respond cases, once per case.
func (e *adapterEnv) responder(t *testing.T) (model.Conversation, model.Identity) {
	t.Helper()
	if e.rconv == nil {
		conv, id := e.participant(t)
		e.rconv, e.rid = &conv, id
	}
	return *e.rconv, e.rid
}

// seedInteraction stores an action interaction of the responder's
// conversation, public, with a response token and a respond window of an
// hour; mod adjusts the record before it is stored.
func (e *adapterEnv) seedInteraction(t *testing.T, mod ...func(*store.InteractionRecord)) model.Interaction {
	t.Helper()
	conv, actor := e.responder(t)
	e.items++
	id := model.ID(fmt.Sprintf("00000ad0-0000-7000-8000-%012x", 0x7000+e.items))
	until := e.clock.Now().Add(time.Hour)
	msg := model.ProviderMessageRef(adapterChanA, e.shape(adapterChanA, FixtureMessage, "pm-"+fmt.Sprint(e.items)))
	rec := store.InteractionRecord{
		Interaction: model.Interaction{
			Ref:  model.InteractionRef{Conversation: conv.Ref, ID: id},
			Kind: model.InteractionAction, Action: "approve",
			Actor: model.Author{
				Identity: actor.Ref, Kind: actor.Kind,
				External: model.ExternalRef{Channel: adapterChanA, Namespace: actor.Namespace, Kind: string(model.KindIdentity), ID: actor.SourceID},
			},
			MessageExternal: &msg,
			External:        []model.ExternalRef{model.ProviderInteractionRef(adapterChanA, "pi-"+string(id))},
			Audience:        model.Audience{Visibility: model.Public}, OccurredAt: e.clock.Now(), RecordedAt: e.clock.Now(),
			Deadline: e.clock.Now().Add(3 * time.Second), RespondUntil: until, Revision: "ix1", Source: e.desc.Type,
		},
		Ingress: "00000ad0-0000-7000-8000-00000000fff0", ResponseToken: respondToken, TokenExpiresAt: until,
	}
	for _, m := range mod {
		m(&rec)
	}
	if e.h.NoResponseToken {
		rec.ResponseToken, rec.TokenExpiresAt = "", time.Time{}
	}
	if err := e.store.Transact(e.ctx, adapterTenant, func(tx store.Tx) error { return tx.PutInteraction(e.ctx, rec) }); err != nil {
		t.Fatalf("seed interaction: %v", err)
	}
	return rec.Interaction
}

// answerReq answers in with mode: ephemeral to the actor only, any other
// mode to everyone, for half an hour.
func (e *adapterEnv) answerReq(in model.Interaction, mode string, followUp bool) model.Respond {
	e.items++
	audience := model.Audience{Visibility: model.Public}
	if mode == model.ResponseModeEphemeral {
		audience.Visibility = model.Restricted
	}
	return model.Respond{
		Command:     command(model.OperationID(fmt.Sprintf("op-answer-%d", e.items))),
		Interaction: in.Ref, Mode: mode, Audience: audience, FollowUp: followUp,
		Content: text("answer"), ExpiresAt: e.clock.Now().Add(30 * time.Minute),
	}
}

func (e *adapterEnv) answer(t *testing.T, req model.Respond) model.Acceptance {
	t.Helper()
	a, err := e.client.Interactions().Respond(e.ctx, req)
	if err != nil {
		t.Fatalf("Interactions.Respond(%s): %v", req.Mode, err)
	}
	return a
}

func (e *adapterEnv) storedInteraction(t *testing.T, ref model.InteractionRef) model.Interaction {
	t.Helper()
	var in model.Interaction
	e.view(t, func(r store.Reader) (err error) { in, err = r.Interaction(e.ctx, ref); return err })
	return in
}

// answers returns the provider calls that answered an interaction.
func (e *adapterEnv) answers() []ProviderCall { return e.calls(adapterChanA, model.CapabilityRespond) }

// noTokenAnywhere fails when the response token reached the operation, its
// attempt evidence, an event or a message.
func (e *adapterEnv) noTokenAnywhere(t *testing.T, ops ...model.OperationRef) {
	t.Helper()
	var dump strings.Builder
	e.view(t, func(r store.Reader) error {
		for _, ref := range ops {
			rec, err := r.Operation(e.ctx, ref)
			if err != nil {
				return err
			}
			fmt.Fprintf(&dump, "%+v %s", rec, rec.Command.Data)
			at, err := r.Attempts(e.ctx, ref, model.PageRequest{Limit: 100})
			if err != nil {
				return err
			}
			fmt.Fprintf(&dump, "%+v", at.Items)
		}
		return nil
	})
	for _, kind := range []string{model.EventInteractionUpdated, model.EventMessageCreated, model.EventDeliveryUpdated} {
		for _, ev := range e.eventsOf(t, kind) {
			fmt.Fprintf(&dump, "%+v %s", ev, ev.Payload.Data)
		}
	}
	fmt.Fprintf(&dump, "%+v", e.messages(t, adapterInboxA))
	if strings.Contains(dump.String(), respondToken) || e.observed.disclose(respondToken) {
		t.Fatal("the response token leaked into the ledger, attempt evidence, events, messages or observations")
	}
}

// respondCases need an adapter declaring respond: it answers an interaction
// in each mode and audience, as the explicit request says, with the
// interaction and its token in the call, and only the call carries the
// token.
var respondCases = []adapterCase{
	{name: "RespondModesAndAudiencesApplied", needs: []model.Capability{model.CapabilityRespond}, run: func(t *testing.T, e *adapterEnv) {
		type sent struct {
			in  model.Interaction
			req model.Respond
			a   model.Acceptance
		}
		var all []sent
		for _, mode := range []string{model.ResponseModeMessage, model.ResponseModeEphemeral, model.ResponseModeUpdate} {
			in := e.seedInteraction(t)
			req := e.answerReq(in, mode, false)
			all = append(all, sent{in, req, e.answer(t, req)})
		}
		e.settle(t)
		calls := e.answers()
		if len(calls) != len(all) {
			t.Fatalf("respond calls = %d, want one per response", len(calls))
		}
		for _, s := range all {
			mode := s.req.Mode
			wantOp(t, e.op(t, s.a.Operation.Ref), model.ProviderAccepted, "")
			i := slices.IndexFunc(calls, func(c ProviderCall) bool { return c.Operation == s.a.Operation.Ref })
			if i < 0 {
				t.Fatalf("%s: no provider call carried the operation", mode)
			}
			c := calls[i]
			wantToken := respondToken
			if e.h.NoResponseToken {
				wantToken = ""
			}
			if c.Mode != mode || c.FollowUp || c.Audience != s.req.Audience || c.Interaction != s.in.Ref || c.Token != wantToken || !c.Applied {
				t.Fatalf("%s: provider call mode %q follow-up %v audience %+v interaction %v, want an initial %s response under %+v",
					mode, c.Mode, c.FollowUp, c.Audience, c.Interaction, mode, s.req.Audience)
			}
			if mode == model.ResponseModeUpdate && c.Target != *s.in.MessageExternal {
				t.Fatalf("update targets %+v, want the control's message %+v", c.Target, *s.in.MessageExternal)
			}
			if (mode == model.ResponseModeMessage) != (s.a.Operation.ResultMessage != nil) {
				t.Fatalf("%s: result message %v: only a message response posts one", mode, s.a.Operation.ResultMessage)
			}
		}
		e.noTokenAnywhere(t, all[0].a.Operation.Ref, all[1].a.Operation.Ref, all[2].a.Operation.Ref)
		if msgs := e.messages(t, adapterInboxA); len(msgs) != 1 {
			t.Fatalf("messages = %d, want only the message response's", len(msgs))
		}
	}},
	{name: "InitialResponseHasOneWinner", needs: []model.Capability{model.CapabilityRespond}, run: func(t *testing.T, e *adapterEnv) {
		in := e.seedInteraction(t)
		first := e.answer(t, e.answerReq(in, model.ResponseModeMessage, false))
		_, err := e.client.Interactions().Respond(e.ctx, e.answerReq(in, model.ResponseModeEphemeral, false))
		if model.CodeOf(err) != model.Conflict {
			t.Fatalf("second initial response = %v, want conflict", err)
		}
		e.settle(t)
		if calls := e.answers(); len(calls) != 1 {
			t.Fatalf("respond calls = %d, want only the winner's", len(calls))
		}
		got := e.storedInteraction(t, in.Ref)
		if got.Response.Initial == nil || *got.Response.Initial != first.Operation.Ref {
			t.Fatalf("response = %+v, want the first operation's claim", got.Response)
		}
	}},
	{name: "ReplayedRespondReusesTheResult", needs: []model.Capability{model.CapabilityRespond}, run: func(t *testing.T, e *adapterEnv) {
		in := e.seedInteraction(t)
		req := e.answerReq(in, model.ResponseModeMessage, false)
		first := e.answer(t, req)
		again := e.answer(t, req)
		if !again.Replayed || again.Operation.Ref != first.Operation.Ref {
			t.Fatalf("retransmitted respond = %+v, want the stored result", again)
		}
		e.settle(t)
		if again = e.answer(t, req); !again.Replayed || again.Operation.Ref != first.Operation.Ref || again.Operation.State != model.ProviderAccepted {
			t.Fatalf("respond replayed after delivery = %+v, want the settled result", again.Operation)
		}
		if calls := e.answers(); len(calls) != 1 {
			t.Fatalf("respond calls = %d after replays, want 1", len(calls))
		}
		if got := e.storedInteraction(t, in.Ref); got.Response.FollowUps != 0 {
			t.Fatalf("response = %+v, want a replay to count nothing", got.Response)
		}
	}},
	{name: "ExpiredWindowIsDeadlineExpired", needs: []model.Capability{model.CapabilityRespond}, run: func(t *testing.T, e *adapterEnv) {
		soon := func(r *store.InteractionRecord) {
			until := e.clock.Now().Add(30 * time.Second)
			r.Interaction.RespondUntil, r.TokenExpiresAt = until, until
		}
		queued, late := e.seedInteraction(t, soon), e.seedInteraction(t, soon)
		req := e.answerReq(queued, model.ResponseModeMessage, false)
		req.ExpiresAt = e.clock.Now().Add(20 * time.Second)
		a := e.answer(t, req)
		e.clock.Advance(time.Minute)
		// At the attempt: explicit, final, never reaching the provider.
		e.settle(t)
		wantOp(t, e.op(t, a.Operation.Ref), model.Failed, model.DeadlineExpired)
		// At submit: the claiming unit refuses a response past the window.
		_, err := e.client.Interactions().Respond(e.ctx, e.answerReq(late, model.ResponseModeMessage, false))
		if model.CodeOf(err) != model.DeadlineExpired {
			t.Fatalf("respond past the window = %v, want deadline_expired", err)
		}
		if got := e.storedInteraction(t, late.Ref); got.Response.Initial != nil {
			t.Fatalf("response = %+v, want none claimed", got.Response)
		}
		if calls := e.answers(); len(calls) != 0 {
			t.Fatalf("respond calls = %d past the window, want none", len(calls))
		}
	}},
	{name: "FollowUpsCountAndLeaveTheInitialResponseOpen", needs: []model.Capability{model.CapabilityRespond, model.CapabilityRespondFollowUp}, run: func(t *testing.T, e *adapterEnv) {
		in := e.seedInteraction(t)
		follow := e.answer(t, e.answerReq(in, model.ResponseModeEphemeral, true))
		initial := e.answer(t, e.answerReq(in, model.ResponseModeMessage, false))
		again := e.answer(t, e.answerReq(in, model.ResponseModeEphemeral, true))
		e.settle(t)
		for _, ref := range []model.OperationRef{follow.Operation.Ref, initial.Operation.Ref, again.Operation.Ref} {
			wantOp(t, e.op(t, ref), model.ProviderAccepted, "")
		}
		got := e.storedInteraction(t, in.Ref)
		if got.Response.Initial == nil || *got.Response.Initial != initial.Operation.Ref || got.Response.FollowUps != 2 {
			t.Fatalf("response = %+v, want the initial and two follow-ups", got.Response)
		}
		var followed int
		for _, c := range e.answers() {
			if c.FollowUp {
				followed++
			}
		}
		if calls := e.answers(); len(calls) != 3 || followed != 2 {
			t.Fatalf("respond calls = %d with %d follow-ups, want 3 with 2", len(calls), followed)
		}
	}},
}

// respondUnsupportedCases: a response the adapter does not declare fails
// with model.Unsupported before any claim or provider call, never falling
// back to another send.
var respondUnsupportedCases = []adapterCase{
	{name: "RespondWithoutRespond", absent: []model.Capability{model.CapabilityRespond}, run: func(t *testing.T, e *adapterEnv) {
		in := e.seedInteraction(t)
		_, err := e.client.Interactions().Respond(e.ctx, e.answerReq(in, model.ResponseModeMessage, false))
		if model.CodeOf(err) != model.Unsupported {
			t.Fatalf("respond without respond = %v, want unsupported", err)
		}
		if got := e.storedInteraction(t, in.Ref); got.Response.Initial != nil || got.Revision != in.Revision {
			t.Fatalf("interaction after a refusal = %+v, want it untouched", got)
		}
		if calls := e.provider.Calls(adapterChanA); len(calls) != 0 {
			t.Fatalf("provider calls = %+v, want none: no fallback", calls)
		}
	}},
	{name: "FollowUpWithoutRespondFollowUp", needs: []model.Capability{model.CapabilityRespond}, absent: []model.Capability{model.CapabilityRespondFollowUp}, run: func(t *testing.T, e *adapterEnv) {
		in := e.seedInteraction(t)
		_, err := e.client.Interactions().Respond(e.ctx, e.answerReq(in, model.ResponseModeMessage, true))
		if model.CodeOf(err) != model.Unsupported {
			t.Fatalf("follow-up without respond_follow_up = %v, want unsupported", err)
		}
		_, err = e.client.Interactions().Ephemeral(e.ctx, model.Ephemeral{
			Command: command("op-follow-ephemeral"), Conversation: in.Ref.Conversation, Recipient: in.Actor.Identity,
			Content: text("only you"), ExpiresAt: e.clock.Now().Add(time.Hour), Interaction: &in.Ref,
		})
		if model.CodeOf(err) != model.Unsupported {
			t.Fatalf("ephemeral follow-up without respond_follow_up = %v, want unsupported", err)
		}
		if got := e.storedInteraction(t, in.Ref); got.Response.FollowUps != 0 || got.Revision != in.Revision {
			t.Fatalf("interaction after refusals = %+v, want it untouched", got)
		}
		if calls := e.provider.Calls(adapterChanA); len(calls) != 0 {
			t.Fatalf("provider calls = %+v, want none: no fallback", calls)
		}
		// The initial response is still the adapter's to give.
		a := e.answer(t, e.answerReq(in, model.ResponseModeMessage, false))
		e.settle(t)
		wantOp(t, e.op(t, a.Operation.Ref), model.ProviderAccepted, "")
	}},
}
