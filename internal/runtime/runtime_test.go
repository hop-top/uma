package runtime_test

import (
	"context"
	"encoding/json"
	goruntime "runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

func TestDeliversOnStoredRouteAndRecordsEvidence(t *testing.T) {
	r := newRig(t)
	a := r.Reply("op-1", "hello")
	if n := r.Step("w"); n != 1 {
		t.Fatalf("executed %d items, want 1", n)
	}
	op := r.Op(a.Operation.Ref)
	requireState(t, op, model.ProviderAccepted, "")
	sends := r.F.ProviderSends()
	if len(sends) != 1 || sends[0].Route != r.Conv.ReplyRoute || sends[0].Operation != a.Operation.Ref {
		t.Fatalf("provider sends = %+v, want one on the stored route", sends)
	}
	if len(op.ProviderReferences) != 1 {
		t.Fatalf("provider references = %v", op.ProviderReferences)
	}
	ev := r.Evidence(a.Operation.Ref)
	if len(ev) != 2 || ev[0].State != model.Unknown || ev[1].State != model.ProviderAccepted ||
		ev[0].Attempt != sends[0].Attempt || ev[1].Attempt != sends[0].Attempt {
		t.Fatalf("evidence = %+v, want the attempt recorded before and after the call", ev)
	}
	if got := r.Policy.Actions(); len(got) < 2 || got[len(got)-2] != model.ActionMessageReply {
		t.Fatalf("policy checks = %v, want reauthorization of message.reply at execution", got)
	}
	events := r.Events()
	if len(events) != 2 || events[0].Kind != model.EventMessageCreated || events[1].Kind != model.EventDeliveryUpdated {
		t.Fatalf("events = %v, want message.created at commit, then delivery.updated", eventKinds(events))
	}
	var p struct {
		State         model.DeliveryState `json:"state"`
		PreviousState model.DeliveryState `json:"previous_state"`
		Trigger       string              `json:"trigger"`
	}
	if err := json.Unmarshal(events[1].Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	if p.State != model.ProviderAccepted || p.PreviousState != model.Queued || p.Trigger != model.TriggerProvider {
		t.Fatalf("payload = %+v", p)
	}
	if r.Step("w") != 0 {
		t.Fatal("settled operation left claimable work")
	}
	if r.F.Opens != r.F.Closes {
		t.Fatalf("opened %d instances, closed %d", r.F.Opens, r.F.Closes)
	}
}

// Lost acknowledgement: the provider applied the effect but its answer was
// lost. The operation is outcome_unknown, then reconciled before any retry;
// the effect is sent once.
func TestLostAckIsReconciledBeforeAnyRetry(t *testing.T) {
	r := newRig(t, withReconciler)
	r.F.Respond(lostAnswer)
	a := r.Reply("op-1", "hello")
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.Unknown, model.OutcomeUnknown)
	if f := r.Op(a.Operation.Ref).Failure; f.Retry != model.ReconcileFirst {
		t.Fatalf("retry advice = %s, want reconcile_first", f.Retry)
	}
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	if got := r.F.Calls(); !slices.Equal(got, []string{"send", "reconcile"}) {
		t.Fatalf("calls = %v, want one send then one reconcile", got)
	}
}

// Reconcile first: when reconciliation shows the effect was not applied, a
// new attempt follows the reconciliation, never precedes it.
func TestReconcileFirstThenRetryWhenNotApplied(t *testing.T) {
	r := newRig(t, withReconciler)
	r.F.Respond(func(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
		return adapter.Outcome{State: model.Unknown}, nil, false
	})
	a := r.Reply("op-1", "hello")
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.Unknown, model.OutcomeUnknown)
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	if got := r.F.Calls(); !slices.Equal(got, []string{"send", "reconcile", "send"}) {
		t.Fatalf("calls = %v, want send, reconcile, send", got)
	}
	if rc := r.F.Reconciled(); len(rc) != 1 || rc[0].Attempt != r.F.ProviderSends()[0].Attempt {
		t.Fatalf("reconciled %+v, want the first attempt", rc)
	}
}

// Reconcile receives the attempt's original effect: its route, the content
// it sent and when the attempt was recorded, before the provider call.
func TestReconcileCarriesTheOriginalEffect(t *testing.T) {
	r := newRig(t, withReconciler)
	r.F.Respond(func(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
		return adapter.Outcome{State: model.Unknown}, nil, false
	})
	a := r.Reply("op-1", "hello")
	attemptedAt := r.Clock.Now()
	r.Step("w")
	r.Clock.Advance(time.Minute)
	r.Step("w")
	rc := r.F.Reconciled()
	sent := r.F.ProviderSends()[0]
	if len(rc) != 1 {
		t.Fatalf("reconciled %d times, want once", len(rc))
	}
	got := rc[0]
	if got.Operation != a.Operation.Ref || got.Attempt != sent.Attempt || got.Route != sent.Route ||
		len(got.Content.Parts) != 1 || got.Content.Parts[0].Text != "hello" || !got.AttemptedAt.Equal(attemptedAt) {
		t.Fatalf("reconcile request %+v, want the attempt's effect, content and record time %v", got, attemptedAt)
	}
}

// An adapter without a reconciler leaves the operation outcome_unknown,
// visible through Operations.Get, and never retries it.
func TestWithoutReconcilerOutcomeUnknownIsNeverRetried(t *testing.T) {
	r := newRig(t)
	r.F.Respond(lostAnswer)
	a := r.Reply("op-1", "hello")
	r.Step("w")
	op := r.Op(a.Operation.Ref)
	requireState(t, op, model.Unknown, model.OutcomeUnknown)
	if op.Failure.Retry != model.ReconcileFirst {
		t.Fatalf("retry advice = %s", op.Failure.Retry)
	}
	r.Clock.Advance(time.Hour)
	if n := r.Step("w"); n != 0 {
		t.Fatalf("claimed %d items after outcome_unknown, want none", n)
	}
	if n := len(r.F.ProviderSends()); n != 1 {
		t.Fatalf("provider sends = %d, want 1", n)
	}
}

// A worker whose lease expired and was reclaimed cannot dispatch: its fence
// is checked when the attempt is recorded, before the provider call.
func TestStaleGenerationCannotDispatch(t *testing.T) {
	r := newRig(t)
	r.Reply("op-1", "hello")
	old := r.Claim("worker-a")
	r.Clock.Advance(2 * time.Minute)
	cur := r.Claim("worker-b")
	if len(old) != 1 || len(cur) != 1 || cur[0].Fence.Generation <= old[0].Fence.Generation {
		t.Fatalf("claims old=%+v cur=%+v, want a higher generation", old, cur)
	}
	if err := r.RT.ExecuteClaimed(r.Ctx, tenantA, old[0]); err != nil {
		t.Fatalf("stale execute: %v", err)
	}
	if n := len(r.F.ProviderSends()); n != 0 {
		t.Fatalf("stale owner reached the provider %d times", n)
	}
	if err := r.RT.ExecuteClaimed(r.Ctx, tenantA, cur[0]); err != nil {
		t.Fatal(err)
	}
	if n := len(r.F.ProviderSends()); n != 1 {
		t.Fatalf("provider sends = %d, want 1 by the current owner", n)
	}
}

// A late receipt from an owner whose lease expired mid-call is kept as
// attempt evidence; it cannot change the operation, and the next owner
// adopts it without sending again.
func TestLateReceiptIsKeptAndAdopted(t *testing.T) {
	r := newRig(t)
	entered, release := make(chan struct{}), make(chan struct{})
	r.F.Respond(func(ctx context.Context, req adapter.SendRequest) (adapter.Outcome, error, bool) {
		close(entered)
		<-release
		return adapter.Outcome{State: model.Delivered}, nil, true
	})
	a := r.Reply("op-1", "hello")
	works := r.Claim("worker-a")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := r.RT.ExecuteClaimed(r.Ctx, tenantA, works[0]); err != nil {
			t.Errorf("stale execute: %v", err)
		}
	}()
	<-entered
	r.Clock.Advance(2 * time.Minute)
	close(release)
	wg.Wait()
	requireState(t, r.Op(a.Operation.Ref), model.Queued, "")
	ev := r.Evidence(a.Operation.Ref)
	if len(ev) != 2 || ev[1].State != model.Delivered || ev[1].Fence != works[0].Fence {
		t.Fatalf("evidence = %+v, want the late receipt under the stale fence", ev)
	}
	r.Step("worker-b")
	op := r.Op(a.Operation.Ref)
	requireState(t, op, model.Delivered, "")
	if len(op.ProviderReferences) != 1 || op.ProviderReferences[0] != ev[1].ProviderReferences[0] {
		t.Fatalf("references = %v, want the late receipt's", op.ProviderReferences)
	}
	if n := len(r.F.ProviderSends()); n != 1 {
		t.Fatalf("provider sends = %d, want 1", n)
	}
}

// A revoked credential fails the operation as unauthorized with no
// provider call and no retry. Credentials resolve per attempt, scoped to
// the channel, its credential reference and the adapter's purpose.
func TestRevokedCredentialFailsWithoutProviderCallOrRetry(t *testing.T) {
	r := newRig(t, withCredentialResolution)
	r.Creds.Revoked = true
	a := r.Reply("op-1", "hello")
	r.Step("w")
	op := r.Op(a.Operation.Ref)
	requireState(t, op, model.Failed, model.Unauthorized)
	if op.Failure.Retry != model.NoRetry {
		t.Fatalf("retry advice = %s, want none", op.Failure.Retry)
	}
	if n := len(r.F.ProviderSends()); n != 0 {
		t.Fatalf("provider sends = %d, want none", n)
	}
	r.Clock.Advance(time.Hour)
	if n := r.Step("w"); n != 0 {
		t.Fatalf("claimed %d items after revocation, want none", n)
	}
	reqs := r.Creds.Requests()
	if len(reqs) != 1 || reqs[0].Channel != r.Ch.Ref || reqs[0].Reference != credRef || reqs[0].Purpose != outPurpose {
		t.Fatalf("credential requests = %+v, want one scoped request", reqs)
	}
}

func TestCredentialsResolvePerAttempt(t *testing.T) {
	r := newRig(t, withCredentialResolution)
	r.F.Respond(transientFailure)
	a := r.Reply("op-1", "hello")
	r.Step("w")
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	if n := len(r.Creds.Requests()); n != 2 {
		t.Fatalf("credential resolutions = %d, want one per attempt", n)
	}
}

// A provider Retry-After longer than the backoff defers the retry.
func TestRetryAfterIsHonoured(t *testing.T) {
	r := newRig(t)
	r.F.Respond(rateLimited(30 * time.Second))
	a := r.Reply("op-1", "hello")
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.RetryPending, model.RateLimited)
	r.Clock.Advance(29 * time.Second)
	if n := r.Step("w"); n != 0 {
		t.Fatalf("retried %ds early", 1)
	}
	r.Clock.Advance(time.Second)
	if n := r.Step("w"); n != 1 {
		t.Fatal("retry not claimable once Retry-After passed")
	}
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	if n := len(r.F.ProviderSends()); n != 2 {
		t.Fatalf("provider sends = %d, want 2", n)
	}
}

// Backoff is full jitter over an exponential ceiling, and the jittered
// delay is the earliest retry time.
func TestBackoffIsExponentialWithFullJitter(t *testing.T) {
	r := newRig(t, withRetry(engine.RetryPolicy{MaxAttempts: 5, BaseDelay: time.Second, MaxDelay: 3 * time.Second}))
	var ceilings []time.Duration
	r.RT.SetJitter(func(max time.Duration) time.Duration {
		ceilings = append(ceilings, max)
		return max / 2
	})
	r.F.Respond(transientFailure, transientFailure, transientFailure)
	r.Reply("op-1", "hello")
	r.Step("w")
	for _, wait := range []time.Duration{500 * time.Millisecond, time.Second, 1500 * time.Millisecond} {
		r.Clock.Advance(wait - time.Millisecond)
		if n := r.Step("w"); n != 0 {
			t.Fatalf("retried before the jittered delay %v", wait)
		}
		r.Clock.Advance(time.Millisecond)
		if n := r.Step("w"); n != 1 {
			t.Fatalf("no retry at the jittered delay %v", wait)
		}
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}; !slices.Equal(ceilings, want) {
		t.Fatalf("jitter ceilings = %v, want %v", ceilings, want)
	}
}

func TestRetryBudgetIsBounded(t *testing.T) {
	r := newRig(t, withRetry(engine.RetryPolicy{MaxAttempts: 2}))
	r.F.Respond(transientFailure, transientFailure, transientFailure)
	a := r.Reply("op-1", "hello")
	r.Step("w")
	r.Step("w")
	op := r.Op(a.Operation.Ref)
	requireState(t, op, model.Failed, model.Transient)
	if op.Failure.Retry != model.NoRetry {
		t.Fatalf("retry advice = %s, want none once exhausted", op.Failure.Retry)
	}
	if n := r.Step("w"); n != 0 || len(r.F.ProviderSends()) != 2 {
		t.Fatalf("attempts = %d, want 2", len(r.F.ProviderSends()))
	}
}

func TestProviderRejectionIsFinal(t *testing.T) {
	r := newRig(t)
	r.F.Respond(func(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
		return adapter.Outcome{State: model.Rejected, Failure: model.NewError(model.Permanent, "refused")}, nil, false
	})
	a := r.Reply("op-1", "hello")
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.Rejected, model.Permanent)
	if r.Step("w") != 0 {
		t.Fatal("rejected operation retried")
	}
}

// An untyped adapter error is not evidence that nothing was applied.
func TestUntypedErrorIsOutcomeUnknown(t *testing.T) {
	r := newRig(t)
	r.F.Respond(func(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
		return adapter.Outcome{}, context.DeadlineExceeded, true
	})
	a := r.Reply("op-1", "hello")
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.Unknown, model.OutcomeUnknown)
}

// Execution reauthorizes the originating actor: a revoked grant fails the
// operation before any provider call.
func TestExecutionRechecksPolicy(t *testing.T) {
	r := newRig(t)
	a := r.Reply("op-1", "hello")
	r.Policy.Set(true)
	r.Step("w")
	requireState(t, op2(r, a.Operation.Ref), model.Failed, model.Unauthorized)
	if n := len(r.F.ProviderSends()); n != 0 {
		t.Fatalf("provider sends = %d, want none", n)
	}
}

// An unavailable policy defers the attempt without a provider call, and
// each deferral spends the retry budget: an outage cannot retry forever.
func TestPolicyOutageRetriesAreBounded(t *testing.T) {
	r := newRig(t, withRetry(engine.RetryPolicy{MaxAttempts: 3}))
	a := r.Reply("op-1", "hello")
	r.Policy.Outage(true)
	for i := range 2 {
		if n := r.Step("w"); n != 1 {
			t.Fatalf("round %d claimed %d items, want 1", i+1, n)
		}
		requireState(t, op2(r, a.Operation.Ref), model.RetryPending, model.Unavailable)
	}
	r.Step("w")
	op := op2(r, a.Operation.Ref)
	requireState(t, op, model.Failed, model.Unavailable)
	if op.Failure.Retry != model.NoRetry {
		t.Fatalf("retry advice = %s, want none once the budget is spent", op.Failure.Retry)
	}
	if n := r.Step("w"); n != 0 {
		t.Fatalf("claimed %d items after the budget was spent", n)
	}
	if n := len(r.F.ProviderSends()); n != 0 {
		t.Fatalf("provider sends = %d, want none", n)
	}
}

// A deferral before dispatch spends budget but recovers once the policy is
// back, and never masks an earlier unresolved attempt.
func TestPolicyOutageRecovers(t *testing.T) {
	r := newRig(t, withRetry(engine.RetryPolicy{MaxAttempts: 3}))
	a := r.Reply("op-1", "hello")
	r.Policy.Outage(true)
	r.Step("w")
	r.Policy.Outage(false)
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	if n := len(r.F.ProviderSends()); n != 1 {
		t.Fatalf("provider sends = %d, want 1", n)
	}
}

// Opening the adapter failing transiently while an attempt is unresolved
// keeps the operation outcome_unknown and is bounded too.
func TestUnresolvedAttemptDeferralsAreBounded(t *testing.T) {
	r := newRig(t, withReconciler, withRetry(engine.RetryPolicy{MaxAttempts: 2}))
	r.F.Respond(lostAnswer)
	a := r.Reply("op-1", "hello")
	r.Step("w")
	r.F.FailOpen(model.NewError(model.Unavailable, "provider SDK init failed"))
	for range 4 {
		r.Step("w")
	}
	requireState(t, op2(r, a.Operation.Ref), model.Unknown, model.OutcomeUnknown)
	if n := r.Step("w"); n != 0 {
		t.Fatalf("claimed %d items after the budget was spent", n)
	}
	if got := r.F.Calls(); len(got) != 1 {
		t.Fatalf("calls = %v, want the single lost send", got)
	}
}

// Only the conversation's stored route is used: a route on another channel
// (for example a linked identity on another provider) is never a substitute
// destination, and a missing route fails.
func TestStoredRouteOnly(t *testing.T) {
	for name, route := range map[string]model.ExternalRef{
		"other channel": {Channel: model.ChannelRef{Tenant: tenantA, ID: chanTwo}, Namespace: routeNSpace, Kind: "room", ID: "room-1"},
		"no route":      {},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			a := r.Reply("op-1", "hello")
			r.SetRoute(route)
			r.Step("w")
			requireState(t, r.Op(a.Operation.Ref), model.Failed, model.Permanent)
			if n := len(r.F.ProviderSends()); n != 0 {
				t.Fatalf("provider sends = %d, want none", n)
			}
		})
	}
}

func TestWithdrawnPermissionFailsWithoutCall(t *testing.T) {
	r := newRig(t)
	r.F.Withdraw(model.CapabilitySend, model.Unauthorized)
	a := r.Reply("op-1", "hello")
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.Failed, model.Unauthorized)
	if n := len(r.F.ProviderSends()); n != 0 {
		t.Fatalf("provider sends = %d, want none", n)
	}
}

func runtimeCall() runtime.Call {
	return runtime.Call{Principal: model.Principal{Tenant: tenantA, Actor: "service"}, Call: model.Call{Correlation: "rt"}}
}

// startRun runs Runtime.Run in the background and returns its result
// channel.
func startRun(r *rig, ctx context.Context, owner string) <-chan error {
	done := make(chan error, 1)
	go func() { done <- r.RT.Run(ctx, runtimeCall(), runtime.WorkerOptions{Owner: owner, Limit: 5}) }()
	return done
}

func waiterOf(r *rig) *signalWaiter { return r.E.Waiter().(*signalWaiter) }

// Drain stops new claims: work held when it starts is finished, Run
// returns, and later runs for the tenant are refused.
func TestDrainFinishesOwnedWorkAndStopsClaims(t *testing.T) {
	r := newRig(t)
	entered, release := make(chan struct{}), make(chan struct{})
	r.F.Respond(func(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
		close(entered)
		<-release
		return adapter.Outcome{State: model.ProviderAccepted}, nil, true
	})
	a := r.Reply("op-1", "hello")
	done := startRun(r, r.Ctx, "w")
	<-entered
	drained := make(chan error, 1)
	go func() { drained <- r.RT.Drain(r.Ctx, runtimeCall()) }()
	close(release)
	if err := <-drained; err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	b := r.Reply("op-2", "after drain")
	ctx, cancel := context.WithTimeout(r.Ctx, 5*time.Second)
	defer cancel()
	err := r.RT.Run(ctx, runtimeCall(), runtime.WorkerOptions{Owner: "w"})
	requireCode(t, err, model.Unavailable, "draining")
	requireState(t, r.Op(b.Operation.Ref), model.Queued, "")
	if n := len(r.F.ProviderSends()); n != 1 {
		t.Fatalf("provider sends = %d, want 1", n)
	}
}

// An idle Run waits through the Waiter and stops on drain, close or
// cancellation.
func TestRunStopsWhenIdle(t *testing.T) {
	t.Run("drain", func(t *testing.T) {
		r := newRig(t)
		done := startRun(r, r.Ctx, "w")
		<-waiterOf(r).Idle
		if err := r.RT.Drain(r.Ctx, runtimeCall()); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatalf("Run: %v", err)
		}
	})
	t.Run("close", func(t *testing.T) {
		r := newRig(t)
		done := startRun(r, r.Ctx, "w")
		<-waiterOf(r).Idle
		if err := r.E.Close(r.Ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatalf("Run: %v", err)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		r := newRig(t)
		ctx, cancel := context.WithCancel(r.Ctx)
		done := startRun(r, ctx, "w")
		<-waiterOf(r).Idle
		cancel()
		if err := <-done; err != context.Canceled {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	})
}

func TestRunProcessesQueuedWorkThenIdles(t *testing.T) {
	r := newRig(t)
	a := r.Reply("op-1", "one")
	b := r.Reply("op-2", "two")
	done := startRun(r, r.Ctx, "w")
	<-waiterOf(r).Idle
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	requireState(t, r.Op(b.Operation.Ref), model.ProviderAccepted, "")
	if err := r.RT.Drain(r.Ctx, runtimeCall()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunValidatesOptions(t *testing.T) {
	r := newRig(t)
	rt := r.RT
	requireCode(t, rt.Run(r.Ctx, runtimeCall(), runtime.WorkerOptions{}), model.Validation, "owner")
	requireCode(t, rt.Run(r.Ctx, runtimeCall(), runtime.WorkerOptions{Owner: "w", Kinds: []string{"ingress.process"}}), model.Unsupported, "")
}

// Without a Waiter, Run paces idle rounds with a stdlib timer that honours
// cancellation and leaves no goroutine behind.
func TestRunWithoutWaiterUsesTimer(t *testing.T) {
	cfg := validConfig()
	cfg.Store = memory.New(memory.Options{Clock: cfg.Clock})
	e := mustNew(t, cfg)
	rt := runtime.New(e)
	before := goruntime.NumGoroutine()
	done := make(chan error, 1)
	go func() {
		done <- rt.Run(t.Context(), runtimeCall(), runtime.WorkerOptions{Owner: "w", Poll: time.Millisecond})
	}()
	time.Sleep(20 * time.Millisecond) // several idle rounds
	select {
	case err := <-done:
		t.Fatalf("Run returned while idle: %v", err)
	default:
	}
	if err := rt.Drain(t.Context(), runtimeCall()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for goruntime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := goruntime.NumGoroutine(); n > before {
		t.Fatalf("goroutines = %d after Run, %d before", n, before)
	}
}

func TestTimerWaiter(t *testing.T) {
	if err := (runtime.TimerWaiter{}).Wait(t.Context(), time.Millisecond); err != nil {
		t.Fatalf("Wait = %v, want nil after the duration", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	if err := (runtime.TimerWaiter{}).Wait(ctx, time.Hour); err != context.Canceled {
		t.Fatalf("Wait = %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("Wait ignored cancellation")
	}
}

func TestRetryPolicyValidation(t *testing.T) {
	cfg := validConfig()
	cfg.Retry = engine.RetryPolicy{MaxAttempts: -1}
	if _, err := engine.New(cfg); model.CodeOf(err) != model.Validation {
		t.Fatalf("New = %v, want validation", err)
	}
	cfg.Retry = engine.RetryPolicy{BaseDelay: time.Minute, MaxDelay: time.Second}
	if _, err := engine.New(cfg); model.CodeOf(err) != model.Validation {
		t.Fatalf("New = %v, want validation", err)
	}
}

// op2 reads an operation through the store: policy may deny Get.
func op2(r *rig, ref model.OperationRef) model.Operation {
	r.T.Helper()
	var rec store.OperationRecord
	err := r.E.Store().View(r.Ctx, tenantA, func(rd store.Reader) error {
		var err error
		rec, err = rd.Operation(r.Ctx, ref)
		return err
	})
	if err != nil {
		r.T.Fatal(err)
	}
	return rec.Operation
}

// A reply records its outbound message at commit, before any delivery, and
// the provider message ID the adapter reports joins that message once the
// operation settles, so an echo of it is recognized at ingress.
func TestReplyRecordsOutboundMessageAndProviderID(t *testing.T) {
	r := newRig(t)
	sent := model.ProviderMessageRef(r.Ch.Ref, "pm-42")
	r.F.Respond(func(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
		return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{sent}}, nil, true
	})
	a := r.Reply("op-1", "hello")
	ref := a.Operation.ResultMessage
	if ref == nil || ref.Conversation != r.Conv.Ref || a.Operation.Result == nil || *a.Operation.Result != ref.Ref() {
		t.Fatalf("acceptance = %+v, want the outbound message as result", a.Operation)
	}
	read := func() model.Message {
		var m model.Message
		if err := r.E.Store().View(r.Ctx, tenantA, func(rd store.Reader) (err error) {
			m, err = rd.GetMessage(r.Ctx, *ref)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := read()
	if m.Origin == nil || m.Origin.Operation != a.Operation.Ref || m.Audience.Visibility != model.Public ||
		m.Source != r.Ch.Type || len(m.External) != 0 || m.Content.Parts[0].Text != "hello" {
		t.Fatalf("outbound message = %+v", m)
	}
	if got := eventKinds(r.Events()); !slices.Equal(got, []string{model.EventMessageCreated}) {
		t.Fatalf("events at commit = %v, want message.created only", got)
	}
	if r.Step("w") != 1 {
		t.Fatal("no delivery")
	}
	if m := read(); !slices.Equal(m.External, []model.ExternalRef{sent}) || m.Revision == "" {
		t.Fatalf("outbound message references = %v, want the provider message ID", m.External)
	}
	again, err := r.C.Messages().Reply(r.Ctx, r.ReplyReq("op-1", "hello"))
	if err != nil || !again.Replayed || again.Operation.ResultMessage == nil || *again.Operation.ResultMessage != *ref {
		t.Fatalf("replay = %+v, %v; want the same outbound message", again, err)
	}
}
