package conformance

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

var (
	convA = model.ID("00000ad0-0000-7000-8000-0000000000c1")
	convB = model.ID("00000ad0-0000-7000-8000-0000000000c2")
)

// outbound seeds channel A with one conversation to peer-1 and returns it.
func (e *adapterEnv) outbound(t *testing.T) model.Conversation {
	t.Helper()
	e.seed(t, adapterChanA, adapterInboxA, e.selection(), "support")
	return e.conversation(t, adapterChanA, adapterInboxA, convA, "peer-1")
}

func (e *adapterEnv) respond(t *testing.T, ch model.ChannelRef, rs ...Response) {
	t.Helper()
	if err := e.provider.Respond(ch, rs...); err != nil {
		if errors.Is(err, ErrNotExpressible) {
			t.Skipf("provider cannot script %+v", rs)
		}
		t.Fatalf("Respond: %v", err)
	}
}

func (e *adapterEnv) calls(ch model.ChannelRef, c model.Capability) []ProviderCall {
	var out []ProviderCall
	for _, call := range e.provider.Calls(ch) {
		if call.Capability == c {
			out = append(out, call)
		}
	}
	return out
}

func (e *adapterEnv) reconciles() bool {
	return slices.Contains(e.desc.Capabilities, model.CapabilityReconcile)
}

func wantOp(t *testing.T, op model.Operation, state model.DeliveryState, code model.ErrorCode) {
	t.Helper()
	if op.State != state {
		t.Fatalf("operation state = %q (failure %v), want %q", op.State, op.Failure, state)
	}
	switch {
	case code == "" && op.Failure != nil:
		t.Fatalf("operation failure = %v, want none", op.Failure)
	case code != "" && (op.Failure == nil || op.Failure.Code != code):
		t.Fatalf("operation failure = %v, want code %q", op.Failure, code)
	}
}

var outboundCases = []adapterCase{
	{name: "AcceptedOnStoredRoute", needs: []model.Capability{model.CapabilitySend}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		e.respond(t, adapterChanA, Response{Outcome: OutcomeAccepted})
		ref := e.mustReply(t, conv.Ref, "op-accepted")
		e.start(t)
		e.stop(t)
		op := e.op(t, ref)
		wantOp(t, op, model.ProviderAccepted, "")
		sends := e.calls(adapterChanA, model.CapabilitySend)
		if len(sends) != 1 || sends[0].Operation != ref || !sends[0].Applied {
			t.Fatalf("provider sends = %+v, want one applied send of %v", sends, ref)
		}
		if len(op.ProviderReferences) == 0 || !slices.Equal(op.ProviderReferences, sends[0].References) {
			t.Fatalf("operation references = %v, want the provider's %v", op.ProviderReferences, sends[0].References)
		}
	}},
	{name: "RejectedIsFinal", needs: []model.Capability{model.CapabilitySend}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		e.respond(t, adapterChanA, Response{Outcome: OutcomeRejected})
		ref := e.mustReply(t, conv.Ref, "op-rejected")
		e.start(t)
		e.later(t, time.Hour)
		e.stop(t)
		op := e.op(t, ref)
		if op.State != model.Rejected || op.Failure == nil || op.Failure.Retry != model.NoRetry {
			t.Fatalf("operation = %+v, want rejected without retry", op)
		}
		if n := len(e.calls(adapterChanA, model.CapabilitySend)); n != 1 {
			t.Fatalf("provider sends = %d, want 1", n)
		}
	}},
	{name: "TransientIsRetried", needs: []model.Capability{model.CapabilitySend}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		e.respond(t, adapterChanA, Response{Outcome: OutcomeTransient}, Response{Outcome: OutcomeAccepted})
		ref := e.mustReply(t, conv.Ref, "op-transient")
		e.start(t)
		wantOp(t, e.op(t, ref), model.RetryPending, model.Transient)
		e.later(t, time.Hour)
		e.stop(t)
		wantOp(t, e.op(t, ref), model.ProviderAccepted, "")
		sends := e.calls(adapterChanA, model.CapabilitySend)
		if len(sends) != 2 || sends[0].Applied || !sends[1].Applied {
			t.Fatalf("provider sends = %+v, want an unapplied then an applied send", sends)
		}
	}},
	{name: "AnswerLostIsNeverResent", needs: []model.Capability{model.CapabilitySend}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		e.respond(t, adapterChanA, Response{Outcome: OutcomeAnswerLost})
		ref := e.mustReply(t, conv.Ref, "op-answer-lost")
		e.start(t)
		op := e.op(t, ref)
		wantOp(t, op, model.Unknown, model.OutcomeUnknown)
		if op.Failure.Retry != model.ReconcileFirst {
			t.Fatalf("lost answer retry advice = %q, want reconcile_first", op.Failure.Retry)
		}
		e.later(t, time.Hour)
		e.stop(t)
		if n := len(e.calls(adapterChanA, model.CapabilitySend)); n != 1 {
			t.Fatalf("provider sends = %d, want 1: a lost answer is never resent", n)
		}
		if !e.reconciles() {
			wantOp(t, e.op(t, ref), model.Unknown, model.OutcomeUnknown)
			return
		}
		wantOp(t, e.op(t, ref), model.ProviderAccepted, "")
		if n := len(e.calls(adapterChanA, model.CapabilityReconcile)); n == 0 {
			t.Fatal("no reconcile call; the applied effect was adopted without reconciling")
		}
	}},
	{name: "ReconcileBeforeRetry", needs: []model.Capability{model.CapabilitySend, model.CapabilityReconcile}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		e.respond(t, adapterChanA, Response{Outcome: OutcomeRequestLost}, Response{Outcome: OutcomeAccepted})
		ref := e.mustReply(t, conv.Ref, "op-request-lost")
		e.start(t)
		wantOp(t, e.op(t, ref), model.Unknown, model.OutcomeUnknown)
		e.later(t, time.Hour)
		e.stop(t)
		wantOp(t, e.op(t, ref), model.ProviderAccepted, "")
		var seq []model.Capability
		for _, c := range e.provider.Calls(adapterChanA) {
			seq = append(seq, c.Capability)
		}
		want := []model.Capability{model.CapabilitySend, model.CapabilityReconcile, model.CapabilitySend}
		if !slices.Equal(seq, want) {
			t.Fatalf("provider calls = %v, want %v", seq, want)
		}
	}},
	{name: "RetryAfterHonoured", needs: []model.Capability{model.CapabilitySend}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		const wait = 10 * time.Minute
		e.respond(t, adapterChanA, Response{Outcome: OutcomeRateLimited, RetryAfter: wait}, Response{Outcome: OutcomeAccepted})
		ref := e.mustReply(t, conv.Ref, "op-rate-limited")
		e.start(t)
		op := e.op(t, ref)
		wantOp(t, op, model.RetryPending, model.RateLimited)
		if op.Failure.RetryAfter != wait {
			t.Fatalf("Retry-After = %v, want the provider's %v", op.Failure.RetryAfter, wait)
		}
		e.later(t, wait-time.Minute)
		if n := len(e.calls(adapterChanA, model.CapabilitySend)); n != 1 {
			t.Fatalf("provider sends before Retry-After elapsed = %d, want 1", n)
		}
		e.later(t, 2*time.Minute)
		e.stop(t)
		wantOp(t, e.op(t, ref), model.ProviderAccepted, "")
		if n := len(e.calls(adapterChanA, model.CapabilitySend)); n != 2 {
			t.Fatalf("provider sends = %d, want 2", n)
		}
	}},
	{name: "CredentialsPerAttemptNeverDisclosed", needs: []model.Capability{model.CapabilitySend}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		e.respond(t, adapterChanA, Response{Outcome: OutcomeTransient}, Response{Outcome: OutcomeAccepted})
		ref := e.mustReply(t, conv.Ref, "op-credentials")
		e.start(t)
		first := len(e.creds.requests())
		if first == 0 {
			t.Fatal("first attempt resolved no credential")
		}
		e.later(t, time.Hour)
		e.stop(t)
		wantOp(t, e.op(t, ref), model.ProviderAccepted, "")
		reqs := e.creds.requests()
		if len(reqs) <= first {
			t.Fatalf("credential resolutions = %d after the retry, %d before; want a resolution per attempt", len(reqs), first)
		}
		acct := e.provider.Account(adapterChanA)
		for _, r := range reqs {
			if r.Channel != adapterChanA || r.Reference != acct.CredentialRef || r.Purpose == "" {
				t.Errorf("credential request %+v outside the channel scope", r)
			}
		}
		e.noSecret(t, ref)
	}},
	{name: "RevokedCredentialMakesNoCall", needs: []model.Capability{model.CapabilitySend}, run: func(t *testing.T, e *adapterEnv) {
		if p, ok := e.provider.(CredentiallessProvider); ok && p.Credentialless() {
			t.Skip("the adapter resolves no credential for its outbound effects")
		}
		conv := e.outbound(t)
		e.creds.revoke()
		ref := e.mustReply(t, conv.Ref, "op-revoked-credential")
		e.start(t)
		e.later(t, time.Hour)
		e.stop(t)
		op := e.op(t, ref)
		wantOp(t, op, model.Failed, model.Unauthorized)
		if op.Failure.Retry != model.NoRetry {
			t.Fatalf("retry advice = %q, want none", op.Failure.Retry)
		}
		if calls := e.provider.Calls(adapterChanA); len(calls) != 0 {
			t.Fatalf("provider calls = %+v, want none", calls)
		}
		e.noSecret(t, ref)
	}},
	{name: "RevokedPermissionMakesNoCall", needs: []model.Capability{model.CapabilitySend}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		if err := e.provider.Revoke(adapterChanA, model.CapabilitySend); err != nil {
			if errors.Is(err, ErrNotExpressible) {
				t.Skip("provider cannot revoke the send permission")
			}
			t.Fatal(err)
		}
		ref := e.mustReply(t, conv.Ref, "op-revoked-permission")
		e.start(t)
		e.later(t, time.Hour)
		e.stop(t)
		wantOp(t, e.op(t, ref), model.Failed, model.Unauthorized)
		if calls := e.provider.Calls(adapterChanA); len(calls) != 0 {
			t.Fatalf("provider calls = %+v, want none", calls)
		}
	}},
	{name: "AccountsOfOneTypeStayIsolated", needs: []model.Capability{model.CapabilitySend}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		e.seed(t, adapterChanB, adapterInboxB, e.selection(), "support")
		e.conversation(t, adapterChanB, adapterInboxB, convB, "peer-1")
		ref := e.mustReply(t, conv.Ref, "op-isolated")
		e.start(t)
		e.stop(t)
		wantOp(t, e.op(t, ref), model.ProviderAccepted, "")
		if n := len(e.calls(adapterChanA, model.CapabilitySend)); n != 1 {
			t.Fatalf("account A sends = %d, want 1", n)
		}
		if calls := e.provider.Calls(adapterChanB); len(calls) != 0 {
			t.Fatalf("account B calls = %+v, want none: equal peer IDs must not cross accounts", calls)
		}
		for _, r := range e.creds.requests() {
			if r.Channel != adapterChanA {
				t.Errorf("credential request %+v resolved for another channel", r)
			}
		}
	}},
}

// noSecret fails when the credential secret appears in an observation, the
// operation, its attempt evidence or a stored message.
func (e *adapterEnv) noSecret(t *testing.T, ref model.OperationRef) {
	t.Helper()
	if e.observed.disclose(adapterSecret) {
		t.Error("an observation discloses the credential secret")
	}
	op := e.op(t, ref)
	if op.Failure != nil && strings.Contains(op.Failure.Error(), adapterSecret) {
		t.Error("the operation failure discloses the credential secret")
	}
	e.view(t, func(r store.Reader) error {
		p, err := r.Attempts(e.ctx, ref, model.PageRequest{Limit: 200})
		if err != nil {
			return err
		}
		for _, a := range p.Items {
			if a.Failure != nil && strings.Contains(a.Failure.Error(), adapterSecret) {
				t.Errorf("attempt %s discloses the credential secret", a.Attempt)
			}
		}
		return nil
	})
	for _, m := range e.messages(t, adapterInboxA) {
		for _, x := range m.Metadata {
			if strings.Contains(string(x.Data), adapterSecret) {
				t.Errorf("message %s metadata discloses the credential secret", m.Ref.ID)
			}
		}
	}
}
