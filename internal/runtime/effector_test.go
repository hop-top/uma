package runtime_test

import (
	"context"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

const testIntent = "com.example.test.intent"

// testEffector executes testIntent operations on the rig's conversation
// route, recording the effects it is handed.
type testEffector struct {
	ch      model.Channel
	route   model.ExternalRef
	effects *[]adapter.Effect
}

func (f testEffector) Intent() string { return testIntent }

func (f testEffector) Decode(e model.Extension) (runtime.Job, error) {
	if e.Version != 1 {
		return nil, &model.Error{Code: model.SchemaMismatch, SafeMessage: "test intent version", Retry: model.NoRetry}
	}
	return testJob{f}, nil
}

type testJob struct{ f testEffector }

func (j testJob) Audience() model.Audience {
	return model.Audience{Visibility: model.Internal, PolicyRef: "test-policy"}
}

func (j testJob) Load(context.Context, *engine.Engine, model.TenantID) (model.Channel, error) {
	return j.f.ch, nil
}

func (j testJob) Route() (model.ExternalRef, *model.ExternalRef) { return j.f.route, nil }

func (j testJob) Recheck(ctx context.Context, actor *engine.Client, rec store.OperationRecord) error {
	res := j.f.ch.Ref.Ref()
	return actor.Authorize(ctx, policy.Authorization{Action: rec.Key.Kind, Resource: res, Audience: j.Audience()}, res)
}

func (j testJob) Capabilities() []model.Capability { return []model.Capability{model.CapabilitySend} }

func (j testJob) Bind(adapter.Instance) (runtime.ProviderCall, bool) {
	return func(_ context.Context, eff adapter.Effect) (adapter.Outcome, error) {
		*j.f.effects = append(*j.f.effects, eff)
		return adapter.Outcome{State: model.ProviderAccepted}, nil
	}, true
}

// queueIntent stores a queued operation with intent and its pending work,
// as a service submitting a command does.
func queueIntent(t *testing.T, r *rig, intent model.Extension) model.OperationRef {
	t.Helper()
	ref := model.OperationRef{Tenant: tenantA, ID: "00000003-0000-7000-8000-000000000001"}
	err := r.E.Store().Transact(r.Ctx, tenantA, func(tx store.Tx) error {
		_, _, err := tx.ClaimOperation(r.Ctx, store.OperationRecord{
			Operation: model.Operation{Ref: ref, State: model.Queued, Revision: "op1"},
			Key: store.OperationKey{
				Tenant: tenantA, Caller: r.C.Principal().Actor, Kind: "com.example.test.act",
				Target: r.Ch.Ref.Ref(), ID: "op-test",
			},
			FingerprintVersion: operations.FingerprintVersion1,
			Fingerprint:        []byte("fp"),
			Command:            intent,
		})
		if err != nil {
			return err
		}
		return tx.EnqueueWork(r.Ctx, store.Work{
			ID: "00000003-0000-7000-8000-000000000002", Kind: operations.WorkKindOperation, Resource: ref.Ref(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func step(t *testing.T, r *rig, rt *runtime.Runtime) {
	t.Helper()
	works, err := rt.ClaimWork(r.Ctx, tenantA, runtime.WorkerOptions{
		Owner: "w", Limit: 10, Kinds: []string{operations.WorkKindOperation}, Lease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range works {
		if err := rt.ExecuteClaimed(r.Ctx, tenantA, w); err != nil {
			t.Fatal(err)
		}
	}
}

// An effector passed to New executes its intent through the shared
// executor: recheck as the originating actor, capability check, recorded
// attempt and delivery event under the job's audience.
func TestExtraEffectorExecutesItsIntent(t *testing.T) {
	r := newRig(t)
	var effects []adapter.Effect
	rt := runtime.New(r.E, testEffector{ch: r.Ch, route: r.Conv.ReplyRoute, effects: &effects})
	ref := queueIntent(t, r, model.Extension{Name: testIntent, Version: 1})
	step(t, r, rt)
	requireState(t, r.Op(ref), model.ProviderAccepted, "")
	want := testJob{}.Audience()
	if len(effects) != 1 || effects[0].Operation != ref || effects[0].Route != r.Conv.ReplyRoute || effects[0].Audience != want {
		t.Fatalf("effects = %+v, want one on the job's route under its audience", effects)
	}
	if got := r.Policy.Actions(); len(got) == 0 || got[0] != "com.example.test.act" {
		t.Fatalf("policy checks = %v, want the stored kind rechecked", got)
	}
	events := r.Events()
	if len(events) != 1 || events[0].Audience != want {
		t.Fatalf("events = %+v, want one delivery event under the job's audience", events)
	}
	if n := len(r.Evidence(ref)); n != 2 {
		t.Fatalf("evidence entries = %d, want the attempt before and after the call", n)
	}
}

// An intent no effector names never reaches the provider: the operation
// fails with schema_mismatch and is not retried.
func TestUnknownIntentFailsWithoutProviderCall(t *testing.T) {
	r := newRig(t)
	ref := queueIntent(t, r, model.Extension{Name: testIntent, Version: 1})
	step(t, r, r.RT)
	op := r.Op(ref)
	requireState(t, op, model.Failed, model.SchemaMismatch)
	if op.Failure.Retry != model.NoRetry {
		t.Fatalf("retry advice = %q, want no retry", op.Failure.Retry)
	}
	if n := len(r.F.ProviderSends()); n != 0 || r.F.Opens != 0 {
		t.Fatalf("provider sends = %d, opens = %d, want none", n, r.F.Opens)
	}
}

// gatedJob is a testJob that may wait, settle locally and record its
// settle step.
type gatedJob struct {
	testJob
	wait, local *bool
	settled     *[]model.DeliveryState
}

func (j gatedJob) Wait() bool  { return *j.wait }
func (j gatedJob) Local() bool { return *j.local }

func (j gatedJob) Settle(_ context.Context, _ store.Tx, op model.Operation) error {
	*j.settled = append(*j.settled, op.State)
	return nil
}

type gatedEffector struct{ job gatedJob }

func (gatedEffector) Intent() string                                { return testIntent }
func (f gatedEffector) Decode(model.Extension) (runtime.Job, error) { return f.job, nil }

// A waiting job is deferred without an attempt or state change; once it no
// longer waits it runs, and its settle step runs in the settling unit.
func TestGatedJobWaitsThenSettles(t *testing.T) {
	r := newRig(t)
	var effects []adapter.Effect
	var settled []model.DeliveryState
	wait, local := true, false
	job := gatedJob{testJob{testEffector{ch: r.Ch, route: r.Conv.ReplyRoute, effects: &effects}}, &wait, &local, &settled}
	rt := runtime.New(r.E, gatedEffector{job})
	ref := queueIntent(t, r, model.Extension{Name: testIntent, Version: 1})
	step(t, r, rt)
	requireState(t, r.Op(ref), model.Queued, "")
	if len(effects) != 0 || len(settled) != 0 || len(r.Evidence(ref)) != 0 || r.F.Opens != 0 {
		t.Fatalf("waiting job: effects %v, settled %v, evidence %d, opens %d; want nothing", effects, settled, len(r.Evidence(ref)), r.F.Opens)
	}
	wait = false
	r.Clock.Advance(time.Hour)
	step(t, r, rt)
	requireState(t, r.Op(ref), model.ProviderAccepted, "")
	if len(effects) != 1 || len(settled) != 1 || settled[0] != model.ProviderAccepted {
		t.Fatalf("effects %v, settled %v; want one effect then one settle", effects, settled)
	}
}

// A local job settles as committed without opening the adapter.
func TestGatedJobSettlesLocally(t *testing.T) {
	r := newRig(t)
	var effects []adapter.Effect
	var settled []model.DeliveryState
	wait, local := false, true
	job := gatedJob{testJob{testEffector{ch: r.Ch, route: r.Conv.ReplyRoute, effects: &effects}}, &wait, &local, &settled}
	rt := runtime.New(r.E, gatedEffector{job})
	ref := queueIntent(t, r, model.Extension{Name: testIntent, Version: 1})
	step(t, r, rt)
	requireState(t, r.Op(ref), model.Committed, "")
	if len(effects) != 0 || r.F.Opens != 0 || len(settled) != 1 || settled[0] != model.Committed {
		t.Fatalf("effects %v, opens %d, settled %v; want a committed settle and no adapter", effects, r.F.Opens, settled)
	}
}
