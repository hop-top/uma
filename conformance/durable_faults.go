package conformance

import (
	"slices"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
)

// Fault cases run the engine over the store under test and crash its
// process at the points where state must persist: around the attempt
// record, the provider call, the outcome commit, ingress and checkpoint
// commits. cross is the restart: a no-op in the logical suite, where the
// next process opens the same store, and a store restart in the Restart
// group. After every crash the next process recovers from what was
// committed: no effect is sent twice unless the provider showed it was not
// applied, no event is accepted twice, and no batch is lost.
var faultCases = append(slices.Concat(faultRestartCases, reconcileCases),
	durableCase{"StaleWorkerIsFenced", staleWorkerIsFenced},
	durableCase{"DuplicateDuringIngressCommit", duplicateDuringIngressCommit},
	durableCase{"InteractionBeforeSendAnswerIsLinked", interactionBeforeSendAnswerIsLinked},
	durableCase{"FirstContactSelection", firstContactSelection},
	durableCase{"ConcurrentFirstContactsConverge", concurrentFirstContacts},
)

// faultRestartCases cross a restart; the Restart group runs them again
// with cross restarting the store.
var faultRestartCases = slices.Concat(statusRankCases, []durableCase{
	{"CrashAfterAttemptRecord", crashAfterAttemptRecord},
	{"CrashAfterAttemptRecordWithoutReconciler", crashAfterAttemptRecordWithoutReconciler},
	{"CrashAfterProviderCall", crashAfterProviderCall},
	{"CrashBeforeOutcomeCommit", crashBeforeOutcomeCommit},
	{"LostAnswerAcrossRestart", lostAnswerAcrossRestart},
	{"EchoBeforeSendAnswerIsLinked", echoBeforeSendAnswerIsLinked},
	{"EchoOfEditSettlesAfterRestart", echoOfEditSettlesAfterRestart},
	{"StatusReportsAdvanceAcrossRestart", statusReportsAdvanceAcrossRestart},
	{"InboundSingleReactionReplaces", inboundSingleReactionReplaces},
	{"LaggingSearchWithinSettleWindow", laggingSearchWithinSettleWindow},
	{"CoarseRevisionTieRefetches", coarseRevisionTieRefetches},
	{"RepeatedCrashesQuarantine", repeatedCrashesQuarantine},
	{"IngressCrashBeforeCommit", ingressCrashBeforeCommit},
	{"AgentIngressCrashBeforeCommit", agentIngressCrashBeforeCommit},
	{"AgentIngressLostAfterCommit", agentIngressLostAfterCommit},
	{"BatchSurvivesRestart", batchSurvivesRestart},
	{"AckCrashBeforeCommit", ackCrashBeforeCommit},
	{"AckLostAfterCommit", ackLostAfterCommit},
	{"WorkItemActResumesAfterCrash", workItemActResumesAfterCrash},
	{"WorkItemActCrashAfterAttemptRecord", workItemActCrashAfterAttemptRecord},
	{"WorkItemActCrashAfterProviderCall", workItemActCrashAfterProviderCall},
})

// leaseExpiry outlasts the worker lease of a crashed process.
const leaseExpiry = 2 * time.Minute

// The attempt is recorded, then the process dies before calling the
// provider. The next owner reconciles first: the provider holds no record,
// so the one send happens then.
func crashAfterAttemptRecord(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, true)
	needsAdvance(t, e)
	p := f.start(t)
	p.store.at(afterCommit, (*unitLog).begins, p.store.crash)
	op := p.reply(t, "op-1")
	p.work()
	p.crashed(t)
	if n := f.p.sent(); n != 0 {
		t.Fatalf("provider sends = %d, want none before the crash", n)
	}
	p = f.restart(t, cross)
	p.work()
	p.idle(t)
	if got := f.p.log(); len(got) != 0 {
		t.Fatalf("provider calls = %v while the crashed worker's lease was live", got)
	}
	p.later(t, leaseExpiry)
	wantOpState(t, p.op(t, op), model.ProviderAccepted, "")
	if got := f.p.log(); !slices.Equal(got, []string{"reconcile", "send"}) {
		t.Fatalf("provider calls = %v, want reconcile then the one send", got)
	}
}

// Without a reconciler the unresolved attempt is never retried: the
// operation stays outcome_unknown and nothing reaches the provider.
func crashAfterAttemptRecordWithoutReconciler(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, false)
	needsAdvance(t, e)
	p := f.start(t)
	p.store.at(afterCommit, (*unitLog).begins, p.store.crash)
	op := p.reply(t, "op-1")
	p.work()
	p.crashed(t)
	p = f.restart(t, cross)
	p.work()
	p.idle(t)
	p.later(t, leaseExpiry)
	p.later(t, time.Hour)
	wantOpState(t, p.op(t, op), model.Unknown, model.OutcomeUnknown)
	if n := f.p.sent(); n != 0 {
		t.Fatalf("provider sends = %d, want none for an unresolved attempt", n)
	}
}

// The provider applies the effect; the process dies before its answer is
// recorded. The next owner reconciles and adopts the provider's record
// without sending again.
func crashAfterProviderCall(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, true)
	needsAdvance(t, e)
	p := f.start(t)
	p.store.at(beforeCommit, (*unitLog).answers, p.store.crash)
	op := p.reply(t, "op-1")
	p.work()
	p.crashed(t)
	p = f.restart(t, cross)
	if ev := f.evidence(t, op); len(ev) != 1 || ev[0].State != model.Unknown {
		t.Fatalf("evidence after the crash = %+v, want only the unresolved attempt", ev)
	}
	p.work()
	p.idle(t)
	p.later(t, leaseExpiry)
	got := p.op(t, op)
	wantOpState(t, got, model.ProviderAccepted, "")
	if calls := f.p.log(); !slices.Equal(calls, []string{"send", "reconcile"}) {
		t.Fatalf("provider calls = %v, want the one send, then reconcile", calls)
	}
	if len(got.ProviderReferences) != 1 {
		t.Fatalf("provider references = %v, want the provider's record", got.ProviderReferences)
	}
}

// The answer is recorded; the process dies before the operation settles.
// The next owner settles from the evidence: no call at all.
func crashBeforeOutcomeCommit(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, true)
	needsAdvance(t, e)
	p := f.start(t)
	p.store.at(beforeCommit, (*unitLog).settles, p.store.crash)
	op := p.reply(t, "op-1")
	p.work()
	p.crashed(t)
	p = f.restart(t, cross)
	wantOpState(t, p.op(t, op), model.Queued, "")
	p.work()
	p.idle(t)
	p.later(t, leaseExpiry)
	wantOpState(t, p.op(t, op), model.ProviderAccepted, "")
	if calls := f.p.log(); !slices.Equal(calls, []string{"send"}) {
		t.Fatalf("provider calls = %v, want only the one send", calls)
	}
	if evs := f.deliveries(t, op); len(evs) != 1 {
		t.Fatalf("delivery events = %d, want one for the one settled state", len(evs))
	}
}

// The provider applies the effect and its answer is lost; the process
// restarts while the operation is outcome_unknown. The next process
// reconciles before any retry.
func lostAnswerAcrossRestart(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, true)
	needsAdvance(t, e)
	f.p.respond(applyLost)
	p := f.start(t)
	op := p.reply(t, "op-1")
	p.work()
	p.idle(t)
	wantOpState(t, p.op(t, op), model.Unknown, model.OutcomeUnknown)
	p = f.restart(t, cross)
	p.work()
	p.idle(t)
	p.later(t, time.Hour)
	wantOpState(t, p.op(t, op), model.ProviderAccepted, "")
	if calls := f.p.log(); !slices.Equal(calls, []string{"send", "reconcile"}) {
		t.Fatalf("provider calls = %v, want the one send, then reconcile", calls)
	}
}

// A worker blocked in the provider call loses its lease to another
// process. Its late answer is kept as evidence under its stale fence but
// cannot change the operation; the current owner reconciles and settles.
func staleWorkerIsFenced(t *testing.T, e *env, _ func(*testing.T)) {
	f := newFaultEnv(t, e, true)
	needsAdvance(t, e)
	sending := f.gate("provider send")
	f.p.respond(held(sending))
	a := f.start(t)
	op := a.reply(t, "op-1")
	a.work()
	sending.await(t)
	e.fx.Advance(leaseExpiry)

	reconciling := f.gate("reconcile")
	f.p.onReconcile(reconciling.reach)
	b := f.spawn(t)
	b.work()
	reconciling.await(t)
	sending.open()
	a.idle(t)
	wantOpState(t, a.op(t, op), model.Queued, "")
	ev := f.evidence(t, op)
	if len(ev) != 2 || ev[1].State != model.ProviderAccepted || ev[1].Fence != ev[0].Fence {
		t.Fatalf("evidence = %+v, want the late answer kept under the stale fence", ev)
	}
	reconciling.open()
	b.idle(t)
	wantOpState(t, b.op(t, op), model.ProviderAccepted, "")
	if n := f.p.sent(); n != 1 {
		t.Fatalf("provider sends = %d, want 1", n)
	}
	if ev := f.evidence(t, op); len(ev) != 3 || ev[2].Fence.Generation <= ev[0].Fence.Generation {
		t.Fatalf("evidence = %+v, want the current owner's reconciliation under a newer fence", ev)
	}
}

// A process that dies on the same item every time does not crash-loop:
// once its claims exceed the work policy's budget the item is quarantined
// and its operation dead-lettered, still outcome_unknown, with nothing
// sent.
func repeatedCrashesQuarantine(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, true)
	needsAdvance(t, e)
	f.work = &uma.WorkPolicy{MaxClaims: 2, MaxInFlight: uma.DefaultMaxInFlight}
	p := f.start(t)
	p.store.at(afterCommit, (*unitLog).begins, p.store.crash)
	op := p.reply(t, "op-1")
	p.work()
	p.crashed(t)

	p = f.restart(t, cross)
	p.store.at(afterCommit, (*unitLog).begins, p.store.crash)
	p.work()
	p.idle(t)
	e.fx.Advance(leaseExpiry)
	p.resume(t)
	p.crashed(t)

	p = f.restart(t, cross)
	p.work()
	p.idle(t)
	p.later(t, leaseExpiry)
	got := p.op(t, op)
	wantOpState(t, got, model.DeadLettered, model.OutcomeUnknown)
	if got.Failure.Retry != model.ReconcileFirst {
		t.Fatalf("dead-letter retry advice = %s, want reconcile_first", got.Failure.Retry)
	}
	p.later(t, time.Hour)
	if n := f.p.sent(); n != 0 {
		t.Fatalf("provider sends = %d, want none", n)
	}
	if calls := f.p.log(); !slices.Equal(calls, []string{"reconcile"}) {
		t.Fatalf("provider calls = %v, want the second claim's reconcile only", calls)
	}
}

func needsAdvance(t *testing.T, e *env) {
	t.Helper()
	if e.fx.Advance == nil {
		t.Skip("fixture cannot advance the store clock")
	}
}
