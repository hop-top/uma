package interactions_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// A response that has already expired, or whose window is gone, is refused
// at submit with deadline_expired and claims nothing; one that outlives the
// window is invalid.
func TestRespondExpiredRejectsAtSubmit(t *testing.T) {
	r := newRig(t, respondCaps()...)
	in := r.seed()
	past := r.respondReq(in, model.ResponseModeMessage)
	past.ExpiresAt = r.clock.Now()
	_, err := r.s.Respond(r.ctx, past)
	enginetest.RequireCode(t, err, model.DeadlineExpired, "")
	long := r.respondReq(in, model.ResponseModeMessage)
	long.ExpiresAt = in.RespondUntil.Add(time.Second)
	_, err = r.s.Respond(r.ctx, long)
	enginetest.RequireCode(t, err, model.Validation, "expires_at")
	r.clock.Advance(2 * time.Hour)
	gone := r.respondReq(in, model.ResponseModeMessage)
	_, err = r.s.Respond(r.ctx, gone)
	enginetest.RequireCode(t, err, model.DeadlineExpired, "")
	if r.spy.Claims() != 0 || r.stored(in.Ref).Response.Initial != nil {
		t.Fatal("a refused respond claimed the interaction")
	}
	r.drain()
	if calls := r.f.recorded(); len(calls) != 0 {
		t.Fatalf("provider calls = %+v, want none", calls)
	}
}

// A response accepted in time but attempted after its deadline, after the
// provider's window, or without its token settles failed deadline_expired
// without a provider call, a reconciliation or any fallback.
func TestRespondExpiredRejectsAtAttempt(t *testing.T) {
	for name, tc := range map[string]struct {
		advance time.Duration
		sweep   bool
		gone    bool
	}{
		"deadline passes inside the window": {advance: 31 * time.Minute},
		"window closes":                     {advance: 2 * time.Hour},
		"token swept":                       {advance: 2 * time.Hour, sweep: true},
		"token gone inside the window":      {gone: true},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, respondCaps(model.CapabilityReconcile)...)
			in := r.seed(func(rec *store.InteractionRecord) {
				if tc.gone {
					rec.ResponseToken = ""
				}
			})
			a := r.respond(r.respondReq(in, model.ResponseModeMessage))
			r.clock.Advance(tc.advance)
			if tc.sweep {
				r.tx(func(tx store.Tx) error {
					_, err := tx.Purge(r.ctx, store.PurgeRequest{Family: store.FamilyInteractionTokens, Before: r.clock.Now(), Limit: 10})
					return err
				})
			}
			r.drain()
			op := r.op(a)
			wantState(t, op, model.Failed)
			if op.Failure == nil || op.Failure.Code != model.DeadlineExpired || op.Failure.Retry != model.NoRetry {
				t.Fatalf("failure = %+v, want a final deadline_expired", op.Failure)
			}
			if calls := r.f.recorded(); len(calls) != 0 {
				t.Fatalf("provider calls = %+v, want none: no attempt, no fallback", calls)
			}
		})
	}
}

// The token reaches the adapter in the effect, for the attempt and for its
// reconciliation, and appears nowhere the engine stores or emits.
func TestRespondTokenReachesAdapter(t *testing.T) {
	r := newRig(t, respondCaps(model.CapabilityReconcile)...)
	in := r.seed()
	var reconciled *adapter.InteractionContext
	r.f.mu.Lock()
	r.f.script = []answer{{out: adapter.Outcome{State: model.Unknown, Failure: model.NewError(model.OutcomeUnknown, "answer lost")}}}
	r.f.reconcile = func(_ context.Context, e adapter.Effect) (adapter.Outcome, error) {
		reconciled = e.Interaction
		return adapter.Outcome{State: model.ProviderAccepted}, nil
	}
	r.f.mu.Unlock()
	a := r.respond(r.respondReq(in, model.ResponseModeMessage))
	r.exec(r.claim())
	wantState(t, r.op(a), model.Unknown)
	r.clock.Advance(30 * time.Second)
	r.drain()
	wantState(t, r.op(a), model.ProviderAccepted)

	calls := r.f.recorded()
	if len(calls) < 2 || calls[0].method != "respond" {
		t.Fatalf("provider calls = %v, want a respond then its reconciliation", r.f.methods())
	}
	ic := calls[0].ic
	if ic == nil || ic.Token != enginetest.InteractionToken || ic.Ref != in.Ref || ic.Kind != model.InteractionAction ||
		ic.ProviderID != "pi-"+string(in.Ref.ID) || !ic.ExpiresAt.Equal(in.RespondUntil) {
		t.Fatalf("effect interaction = %v, want the loaded context with its token", ic)
	}
	if ic.Message == nil || *ic.Message != *in.MessageExternal {
		t.Fatalf("effect message = %v, want %v", ic.Message, in.MessageExternal)
	}
	if reconciled == nil || reconciled.Token != enginetest.InteractionToken {
		t.Fatalf("reconcile effect interaction = %v, want the token too", reconciled)
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", ic, ic, ic), enginetest.InteractionToken) {
		t.Fatal("formatting the interaction context printed the token")
	}
	r.requireNoToken(a.Operation.Ref)
	if n := r.spy.TokenReads(); n == 0 {
		t.Fatal("the effector never read the token")
	}
}

// A provider without a response token is answered with an empty one, and
// without a window only the response's own expiry bounds it.
func TestRespondTokenless(t *testing.T) {
	r := newRig(t, respondCaps()...)
	in := r.seed(func(rec *store.InteractionRecord) {
		rec.ResponseToken, rec.TokenExpiresAt = "", time.Time{}
		rec.Interaction.RespondUntil = time.Time{}
	})
	req := r.respondReq(in, model.ResponseModeMessage)
	req.ExpiresAt = r.clock.Now().Add(24 * time.Hour)
	a := r.respond(req)
	r.drain()
	wantState(t, r.op(a), model.ProviderAccepted)
	calls := r.f.recorded()
	if len(calls) != 1 || calls[0].ic == nil || calls[0].ic.Token != "" || !calls[0].ic.ExpiresAt.IsZero() {
		t.Fatalf("calls = %+v, want one with an empty token and no window", calls)
	}
}

// The same operation ID and payload replays the accepted response, also
// after the window is gone, claiming and counting once; the same ID with
// another payload conflicts.
func TestRespondReplay(t *testing.T) {
	r := newRig(t, respondCaps()...)
	in := r.seed()
	req := r.respondReq(in, model.ResponseModeMessage)
	first := r.respond(req)
	r.clock.Advance(2 * time.Hour)
	again, err := r.s.Respond(r.ctx, req)
	if err != nil || !again.Replayed || again.Operation.Ref != first.Operation.Ref {
		t.Fatalf("retry = %+v, %v; want a replay of %v", again, err, first.Operation.Ref)
	}
	changed := req
	changed.Content = content("different")
	_, err = r.s.Respond(r.ctx, changed)
	enginetest.RequireCode(t, err, model.Conflict, "different payload")
	if r.spy.Claims() != 1 {
		t.Fatalf("claims = %d, want 1", r.spy.Claims())
	}
	if msgs, _ := r.recorded(); len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	if n := len(r.eventsOf(model.EventInteractionUpdated)); n != 1 {
		t.Fatalf("interaction.updated events = %d, want 1", n)
	}
}

// Redaction wins over a response: a redacted interaction refuses a new one
// with conflict, and a queued one fails permanent at its attempt without a
// provider call.
func TestRespondAfterRedactionFails(t *testing.T) {
	r := newRig(t, respondCaps()...)
	redact := func(in model.Interaction) {
		r.tx(func(tx store.Tx) error {
			_, err := tx.Redact(r.ctx, store.Redaction{Interaction: &in.Ref, Expected: in.Revision, Revision: "ix-redacted"})
			return err
		})
	}
	queued := r.seed()
	a := r.respond(r.respondReq(queued, model.ResponseModeMessage))
	redact(r.stored(queued.Ref))
	r.drain()
	op := r.op(a)
	wantState(t, op, model.Failed)
	if op.Failure == nil || op.Failure.Code != model.Permanent || op.Failure.Retry != model.NoRetry {
		t.Fatalf("failure = %+v, want a final permanent", op.Failure)
	}
	if calls := r.f.recorded(); len(calls) != 0 {
		t.Fatalf("provider calls = %+v, want none after redaction", calls)
	}

	fresh := r.seed()
	redact(fresh)
	_, err := r.s.Respond(r.ctx, r.respondReq(r.stored(fresh.Ref), model.ResponseModeMessage))
	enginetest.RequireCode(t, err, model.Conflict, "redacted")
}

// A redaction that lands while the previous attempt is being reconciled is
// seen by the next attempt: the token loaded before the reconciliation is
// never used to retry the response.
func TestRespondRedactedWhileReconciling(t *testing.T) {
	r := newRig(t, respondCaps(model.CapabilityReconcile)...)
	in := r.seed()
	r.f.mu.Lock()
	r.f.script = []answer{{out: adapter.Outcome{State: model.Unknown, Failure: model.NewError(model.OutcomeUnknown, "answer lost")}}}
	r.f.reconcile = func(context.Context, adapter.Effect) (adapter.Outcome, error) {
		r.tx(func(tx store.Tx) error {
			_, err := tx.Redact(r.ctx, store.Redaction{Interaction: &in.Ref, Expected: r.stored(in.Ref).Revision, Revision: "ix-redacted"})
			return err
		})
		return adapter.Outcome{State: model.Failed, Failure: model.NewError(model.NotFound, "no record")}, nil
	}
	r.f.mu.Unlock()
	a := r.respond(r.respondReq(in, model.ResponseModeEphemeral))
	r.exec(r.claim())
	r.clock.Advance(30 * time.Second)
	r.drain()
	op := r.op(a)
	wantState(t, op, model.Failed)
	if op.Failure == nil || op.Failure.Code != model.Permanent {
		t.Fatalf("failure = %+v, want permanent", op.Failure)
	}
	if got := r.f.methods(); len(got) != 2 || got[0] != "respond" || got[1] != "reconcile" {
		t.Fatalf("provider calls = %v, want the first respond and its reconciliation only", got)
	}
}
