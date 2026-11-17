package interactions_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// respondCaps is an adapter that answers interactions, follow-ups included.
func respondCaps(extra ...model.Capability) []model.Capability {
	return append([]model.Capability{
		model.CapabilitySend, model.CapabilityRespond, model.CapabilityRespondFollowUp, model.CapabilitySendEphemeral,
	}, extra...)
}

// respondReq answers in with mode: ephemeral to the actor only, any other
// mode to everyone, for half an hour.
func (r *rig) respondReq(in model.Interaction, mode string) model.Respond {
	audience := public()
	if mode == model.ResponseModeEphemeral {
		audience = model.Audience{Visibility: model.Restricted}
	}
	return model.Respond{
		Command: r.cmd(), Interaction: in.Ref, Mode: mode, Audience: audience,
		Content: content("answer"), ExpiresAt: r.clock.Now().Add(30 * time.Minute),
	}
}

func (r *rig) respond(req model.Respond) model.Acceptance {
	r.t.Helper()
	a, err := r.s.Respond(r.ctx, req)
	if err != nil {
		r.t.Fatalf("Respond: %v", err)
	}
	return a
}

// eventsOf returns the stored events of kind, in order.
func (r *rig) eventsOf(kind string) []model.Event {
	_, evs := r.recorded()
	var out []model.Event
	for _, ev := range evs {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func decode[T any](t *testing.T, ev model.Event) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(ev.Payload.Data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// requireNoToken fails when anything the engine stores or emits for op
// holds the response token: the operation record with its intent, its
// attempt evidence, every event and every message.
func (r *rig) requireNoToken(op model.OperationRef) {
	r.t.Helper()
	var dump strings.Builder
	err := r.e.Store().View(r.ctx, enginetest.TenantA, func(rd store.Reader) error {
		rec, err := rd.Operation(r.ctx, op)
		if err != nil {
			return err
		}
		fmt.Fprintf(&dump, "%+v %s", rec, rec.Command.Data)
		at, err := rd.Attempts(r.ctx, op, model.PageRequest{Limit: 100})
		fmt.Fprintf(&dump, "%+v", at.Items)
		return err
	})
	if err != nil {
		r.t.Fatal(err)
	}
	msgs, evs := r.recorded()
	for _, ev := range evs {
		fmt.Fprintf(&dump, "%+v %s", ev, ev.Payload.Data)
	}
	fmt.Fprintf(&dump, "%+v", msgs)
	if strings.Contains(dump.String(), enginetest.InteractionToken) {
		r.t.Fatal("the response token leaked into the ledger, attempt evidence, events or messages")
	}
}

// Respond needs the adapter to declare respond, and respond_follow_up for a
// follow-up: without them it is unsupported, claims nothing and queues
// nothing, and nothing falls back to another send.
func TestRespondRequiresCapability(t *testing.T) {
	for name, tc := range map[string]struct {
		caps     []model.Capability
		followUp bool
		want     string
	}{
		"no respond":            {[]model.Capability{model.CapabilitySend, model.CapabilitySendEphemeral}, false, "respond"},
		"follow-up not offered": {[]model.Capability{model.CapabilitySend, model.CapabilityRespond}, true, "respond_follow_up"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, tc.caps...)
			in := r.seed()
			req := r.respondReq(in, model.ResponseModeMessage)
			req.FollowUp = tc.followUp
			_, err := r.s.Respond(r.ctx, req)
			enginetest.RequireCode(t, err, model.Unsupported, tc.want)
			r.drain()
			if calls := r.f.recorded(); len(calls) != 0 {
				t.Fatalf("provider calls = %+v, want none", calls)
			}
			got := r.stored(in.Ref)
			if got.Response.Initial != nil || got.Response.FollowUps != 0 || got.Revision != in.Revision || r.spy.Claims() != 0 {
				t.Fatalf("interaction after refusal = %+v (claims %d), want untouched", got, r.spy.Claims())
			}
			if msgs, _ := r.recorded(); len(msgs) != 0 {
				t.Fatalf("refused respond stored messages %+v", msgs)
			}
		})
	}
}

// Respond is authorized as interaction.respond on the interaction under its
// audience, and an interaction that is not the caller's to see is not found.
func TestRespondAuthorization(t *testing.T) {
	r := newRig(t, respondCaps()...)
	in := r.seed(func(rec *store.InteractionRecord) {
		rec.Interaction.Audience = model.Audience{Visibility: model.Internal, PolicyRef: "staff"}
	})
	r.policy.deny = func(a policy.Authorization) bool { return a.Action == model.ActionInteractionRespond }
	_, err := r.s.Respond(r.ctx, r.respondReq(in, model.ResponseModeMessage))
	enginetest.RequireCode(t, err, model.Unauthorized, "")
	asks := r.policy.asked(model.ActionInteractionRespond)
	if len(asks) == 0 || asks[0].Resource != in.Ref.Ref() || asks[0].Audience != in.Audience {
		t.Fatalf("interaction.respond authorizations = %+v, want one on the record under its audience", asks)
	}
	if r.spy.Claims() != 0 {
		t.Fatal("a refused respond claimed the interaction")
	}
	r.policy.deny = nil
	missing := r.respondReq(in, model.ResponseModeMessage)
	missing.Interaction.ID = "00000005-0000-7000-8000-0000000000f9"
	_, err = r.s.Respond(r.ctx, missing)
	enginetest.RequireCode(t, err, model.NotFound, "resource not found")
}

// The mode and audience of a response are explicit and checked: an
// ephemeral response is restricted and stores no message, a message
// response stores one under its audience, an update needs the message the
// control sits on and stores none; the adapter is handed the mode, the
// audience and, for an update, the provider message as the target.
func TestRespondModesAndAudience(t *testing.T) {
	r := newRig(t, respondCaps()...)
	msgIn, updIn, ephIn := r.seed(), r.seed(), r.seed()

	bad := r.respondReq(ephIn, model.ResponseModeEphemeral)
	bad.Audience = public()
	_, err := r.s.Respond(r.ctx, bad)
	enginetest.RequireCode(t, err, model.Validation, "restricted")
	_, err = r.s.Respond(r.ctx, r.respondReq(msgIn, "shout"))
	enginetest.RequireCode(t, err, model.Validation, "mode")

	staff := model.Audience{Visibility: model.Internal, PolicyRef: "staff"}
	msgReq := r.respondReq(msgIn, model.ResponseModeMessage)
	msgReq.Audience = staff
	r.respond(msgReq)
	r.respond(r.respondReq(updIn, model.ResponseModeUpdate))
	r.respond(r.respondReq(ephIn, model.ResponseModeEphemeral))
	r.drain()

	calls := r.f.recorded()
	if len(calls) != 3 {
		t.Fatalf("provider calls = %+v, want three responds", calls)
	}
	byMode := map[string]call{}
	for _, c := range calls {
		if c.method != "respond" || c.followUp {
			t.Fatalf("call %+v, want only initial respond calls", c)
		}
		byMode[c.mode] = c
	}
	if c := byMode[model.ResponseModeMessage]; c.audience != staff || c.target != nil || c.ic == nil || c.ic.Ref != msgIn.Ref {
		t.Fatalf("message response call = %+v, want the internal audience, no target, its interaction", c)
	}
	if c := byMode[model.ResponseModeEphemeral]; c.audience.Visibility != model.Restricted || c.ic == nil || c.ic.Ref != ephIn.Ref {
		t.Fatalf("ephemeral response call = %+v, want restricted", c)
	}
	if c := byMode[model.ResponseModeUpdate]; c.target == nil || *c.target != *updIn.MessageExternal || c.ic == nil || c.ic.Ref != updIn.Ref {
		t.Fatalf("update response call = %+v, want the control's provider message as target", c)
	}
	msgs, _ := r.recorded()
	if len(msgs) != 1 || msgs[0].Audience != staff || msgs[0].Origin == nil || msgs[0].InResponseTo == nil || *msgs[0].InResponseTo != msgIn.Ref {
		t.Fatalf("stored messages = %+v, want only the message response, correlated", msgs)
	}
}

// An update needs the provider message the control sits on.
func TestRespondUpdateNeedsSourceMessage(t *testing.T) {
	r := newRig(t, respondCaps()...)
	in := r.seed(func(rec *store.InteractionRecord) { rec.Interaction.MessageExternal = nil })
	_, err := r.s.Respond(r.ctx, r.respondReq(in, model.ResponseModeUpdate))
	enginetest.RequireCode(t, err, model.Validation, "message")
	if r.spy.Claims() != 0 {
		t.Fatal("a refused respond claimed the interaction")
	}
}

// A message response commits its message with the operation, correlated
// with the interaction, announces it with message.created (origin
// response, in_response_to) and, once delivered, links the provider
// message to it; interaction.updated names the response and the operation,
// under the interaction's audience, with no content.
func TestRespondMessageLinked(t *testing.T) {
	r := newRig(t, respondCaps()...)
	in := r.seed()
	pm := model.ProviderMessageRef(r.ch.Ref, "pm-9")
	r.f.script = []answer{{out: adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{pm}}}}
	a := r.respond(r.respondReq(in, model.ResponseModeMessage))
	if a.Operation.ResultMessage == nil {
		t.Fatalf("operation = %+v, want the response message as its result", a.Operation)
	}
	r.drain()
	wantState(t, r.op(a), model.ProviderAccepted)

	msgs, _ := r.recorded()
	if len(msgs) != 1 || msgs[0].Ref != *a.Operation.ResultMessage {
		t.Fatalf("messages = %+v, want the operation's result", msgs)
	}
	m := msgs[0]
	if m.Origin == nil || m.Origin.Operation != a.Operation.Ref || m.InResponseTo == nil || *m.InResponseTo != in.Ref || m.Source != fakeKey {
		t.Fatalf("message = %+v, want origin %v correlated with %v", m, a.Operation.Ref, in.Ref)
	}
	if len(m.External) != 1 || m.External[0] != pm {
		t.Fatalf("message references = %+v, want the provider message after delivery", m.External)
	}
	created := r.eventsOf(model.EventMessageCreated)
	if len(created) != 1 {
		t.Fatalf("message.created events = %d, want 1", len(created))
	}
	p := decode[model.MessageCreatedPayload](t, created[0])
	if p.Origin != model.MessageOriginResponse || p.InResponseTo == nil || *p.InResponseTo != in.Ref || p.Message != m.Ref || p.Operation != a.Operation.Ref {
		t.Fatalf("message.created payload = %+v", p)
	}
	updated := r.eventsOf(model.EventInteractionUpdated)
	if len(updated) != 1 {
		t.Fatalf("interaction.updated events = %d, want 1", len(updated))
	}
	u := decode[model.InteractionUpdatedPayload](t, updated[0])
	if u.Change != model.InteractionChangeResponded || u.Trigger != model.TriggerCommand || u.PreviousRevision != in.Revision ||
		u.Operation == nil || *u.Operation != a.Operation.Ref || u.Interaction != in.Ref {
		t.Fatalf("interaction.updated payload = %+v", u)
	}
	if updated[0].Audience != in.Audience || updated[0].Resource != in.Ref.Ref() {
		t.Fatalf("interaction.updated event = %+v, want the interaction's resource and audience", updated[0])
	}
	got := r.stored(in.Ref)
	if got.Response.Initial == nil || *got.Response.Initial != a.Operation.Ref || got.Response.FollowUps != 0 || got.Revision == in.Revision {
		t.Fatalf("interaction response = %+v revision %s, want the initial claimed by %v", got.Response, got.Revision, a.Operation.Ref)
	}
	if updated[0].Revision != got.Revision {
		t.Fatalf("event revision %s, interaction revision %s", updated[0].Revision, got.Revision)
	}
}

// Two concurrent initial responses: exactly one is accepted and wins the
// claim; the other conflicts and leaves no operation, message or work.
func TestRespondInitialSingleWinner(t *testing.T) {
	r := newRig(t, respondCaps()...)
	in := r.seed()
	reqs := []model.Respond{r.respondReq(in, model.ResponseModeMessage), r.respondReq(in, model.ResponseModeMessage)}
	reqs[1].Content = content("other answer")
	accepted := make([]model.Acceptance, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			accepted[i], errs[i] = r.s.Respond(r.ctx, reqs[i])
		}()
	}
	close(start)
	wg.Wait()

	winner := -1
	for i, err := range errs {
		switch {
		case err == nil && winner < 0:
			winner = i
		case err == nil:
			t.Fatalf("both responses accepted: %+v and %+v", accepted[winner], accepted[i])
		default:
			enginetest.RequireCode(t, err, model.Conflict, "")
		}
	}
	if winner < 0 {
		t.Fatalf("no response accepted: %v, %v", errs[0], errs[1])
	}
	got := r.stored(in.Ref)
	if got.Response.Initial == nil || *got.Response.Initial != accepted[winner].Operation.Ref {
		t.Fatalf("initial = %v, want the winner %v", got.Response.Initial, accepted[winner].Operation.Ref)
	}
	r.drain()
	if calls := r.f.recorded(); len(calls) != 1 || calls[0].text != text(reqs[winner].Content) {
		t.Fatalf("provider calls = %+v, want only the winner's", calls)
	}
	if msgs, _ := r.recorded(); len(msgs) != 1 {
		t.Fatalf("messages = %d, want the winner's alone", len(msgs))
	}
	if n := len(r.eventsOf(model.EventInteractionUpdated)); n != 1 {
		t.Fatalf("interaction.updated events = %d, want 1", n)
	}
}

// A second initial response, sequentially, conflicts too; a follow-up
// needs no initial first and never consumes the initial claim.
func TestRespondFollowUpGated(t *testing.T) {
	r := newRig(t, respondCaps()...)
	in := r.seed()
	follow := func() model.Respond {
		q := r.respondReq(in, model.ResponseModeMessage)
		q.FollowUp = true
		return q
	}
	f1 := r.respond(follow())
	got := r.stored(in.Ref)
	if got.Response.Initial != nil || got.Response.FollowUps != 1 || got.Revision == in.Revision {
		t.Fatalf("after a follow-up: %+v revision %s, want one follow-up and no initial", got.Response, got.Revision)
	}
	first := r.respond(r.respondReq(in, model.ResponseModeMessage))
	_, err := r.s.Respond(r.ctx, r.respondReq(in, model.ResponseModeMessage))
	enginetest.RequireCode(t, err, model.Conflict, "")
	r.respond(follow())
	got = r.stored(in.Ref)
	if got.Response.Initial == nil || *got.Response.Initial != first.Operation.Ref || got.Response.FollowUps != 2 {
		t.Fatalf("response = %+v, want the initial %v and two follow-ups", got.Response, first.Operation.Ref)
	}
	r.drain()
	var initial, follows int
	for _, c := range r.f.recorded() {
		if c.followUp {
			follows++
		} else {
			initial++
		}
	}
	if initial != 1 || follows != 2 {
		t.Fatalf("provider calls: %d initial, %d follow-up; want 1 and 2", initial, follows)
	}
	changes := map[string]int{}
	for _, ev := range r.eventsOf(model.EventInteractionUpdated) {
		changes[decode[model.InteractionUpdatedPayload](t, ev).Change]++
	}
	if changes[model.InteractionChangeResponded] != 1 || changes[model.InteractionChangeFollowUp] != 2 {
		t.Fatalf("interaction.updated changes = %v, want one responded and two follow_up", changes)
	}
	wantState(t, r.op(f1), model.ProviderAccepted)
}
