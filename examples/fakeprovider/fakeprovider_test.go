package fakeprovider_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/examples/fakeprovider"
	"hop.top/uma/model"
)

const tenant model.TenantID = "fake-tenant"

func channelRef(id string) model.ChannelRef {
	return model.ChannelRef{Tenant: tenant, ID: model.ID(id)}
}

var (
	chanA = channelRef("00000000-0000-7000-8000-00000000000a")
	chanB = channelRef("00000000-0000-7000-8000-00000000000b")
)

func open(t *testing.T, f *fakeprovider.Factory, ref model.ChannelRef) adapter.Instance {
	t.Helper()
	inst, err := f.Open(t.Context(), model.Channel{Ref: ref, Type: f.Describe().Type, Configuration: f.Config(), Revision: "r1"}, adapter.Dependencies{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	return inst
}

// implements maps every builtin capability to its adapter interface.
var implements = map[model.Capability]func(adapter.Instance) bool{
	model.CapabilityReceive:          func(i adapter.Instance) bool { _, ok := i.(adapter.Receiver); return ok },
	model.CapabilityReceiveHTTP:      func(i adapter.Instance) bool { _, ok := i.(adapter.HTTPReceiver); return ok },
	model.CapabilitySend:             func(i adapter.Instance) bool { _, ok := i.(adapter.Sender); return ok },
	model.CapabilityEdit:             func(i adapter.Instance) bool { _, ok := i.(adapter.Editor); return ok },
	model.CapabilityDelete:           func(i adapter.Instance) bool { _, ok := i.(adapter.Deleter); return ok },
	model.CapabilityReact:            func(i adapter.Instance) bool { _, ok := i.(adapter.Reactor); return ok },
	model.CapabilityReconcile:        func(i adapter.Instance) bool { _, ok := i.(adapter.Reconciler); return ok },
	model.CapabilitySignal:           func(i adapter.Instance) bool { _, ok := i.(adapter.Signaler); return ok },
	model.CapabilitySendEphemeral:    func(i adapter.Instance) bool { _, ok := i.(adapter.EphemeralSender); return ok },
	model.CapabilityStream:           func(i adapter.Instance) bool { _, ok := i.(adapter.Streamer); return ok },
	model.CapabilityExtensionQuery:   func(i adapter.Instance) bool { _, ok := i.(adapter.ExtensionReader); return ok },
	model.CapabilityExtensionExecute: func(i adapter.Instance) bool { _, ok := i.(adapter.ExtensionExecutor); return ok },
	model.CapabilityProbePermissions: func(i adapter.Instance) bool { _, ok := i.(adapter.PermissionProber); return ok },
	model.CapabilityWorkItemAct:      func(i adapter.Instance) bool { _, ok := i.(adapter.WorkItemActor); return ok },

	model.CapabilityRespond:         func(i adapter.Instance) bool { _, ok := i.(adapter.InteractionResponder); return ok },
	model.CapabilityRespondFollowUp: func(i adapter.Instance) bool { _, ok := i.(adapter.InteractionResponder); return ok },
	model.CapabilityReceipt:         func(i adapter.Instance) bool { _, ok := i.(adapter.ReceiptSender); return ok },
	model.CapabilityMembership:      func(i adapter.Instance) bool { _, ok := i.(adapter.MembershipChanger); return ok },
	model.CapabilityHistory:         func(i adapter.Instance) bool { _, ok := i.(adapter.HistoryReader); return ok },
}

func TestVariantsImplementExactlyTheirCapabilities(t *testing.T) {
	for _, c := range []struct {
		name string
		f    *fakeprovider.Factory
		want []model.Capability
	}{
		{"ingress-only", fakeprovider.NewIngressOnly(), []model.Capability{model.CapabilityReceive}},
		{"send-only", fakeprovider.NewSendOnly(), []model.Capability{model.CapabilitySend}},
		{"full", fakeprovider.NewFull(), []model.Capability{model.CapabilityReceive, model.CapabilitySend, model.CapabilityEdit, model.CapabilityDelete, model.CapabilityReact, model.CapabilityReconcile, model.CapabilityExtensionExecute, model.CapabilityProbePermissions, model.CapabilityReceipt, model.CapabilityMembership, model.CapabilityHistory}},
		{"webhook", fakeprovider.NewWebhook(), []model.Capability{model.CapabilityReceiveHTTP}},
		{"interactive", fakeprovider.NewInteractive(), []model.Capability{model.CapabilitySend, model.CapabilityEdit, model.CapabilityStream, model.CapabilitySendEphemeral, model.CapabilitySignal, model.CapabilityRespond, model.CapabilityRespondFollowUp}},
		{"mail", fakeprovider.NewMail(), []model.Capability{model.CapabilityReceive, model.CapabilitySend}},
		{"tracker", fakeprovider.NewTracker(), []model.Capability{model.CapabilityReconcile, model.CapabilityWorkItemAct}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := c.f.Describe()
			if !slices.Equal(d.Capabilities, c.want) {
				t.Fatalf("declared %v, want %v", d.Capabilities, c.want)
			}
			if d.ContractMajor != fakeprovider.ContractMajor || d.ConfigurationSchema.Name != c.f.Config().Name {
				t.Errorf("descriptor = %+v", d)
			}
			inst := open(t, c.f, chanA)
			for capability, has := range implements {
				if declared := slices.Contains(d.Capabilities, capability); has(inst) != declared {
					t.Errorf("%s: implemented=%v declared=%v", capability, has(inst), declared)
				}
			}
			st, err := inst.Capabilities(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for k, s := range st {
				if s.Name != c.want[k] || !s.Available {
					t.Errorf("capability status %d = %+v, want %s available", k, s, c.want[k])
				}
			}
		})
	}
}

func TestValidateAndOpen(t *testing.T) {
	f := fakeprovider.NewSendOnly()
	if issues := f.Validate(f.Config()); len(issues) != 0 {
		t.Fatalf("valid config: %v", issues)
	}
	if issues := f.Validate(model.Extension{Name: "org.example.other.config", Version: 1}); len(issues) != 1 {
		t.Fatalf("foreign schema issues = %v, want one", issues)
	}
	ctx := t.Context()
	_, err := f.Open(ctx, model.Channel{Ref: chanA, Type: fakeprovider.TypeFull, Configuration: f.Config()}, adapter.Dependencies{})
	wantCode(t, err, model.Validation)
	_, err = f.Open(ctx, model.Channel{Ref: chanA, Type: fakeprovider.TypeSendOnly}, adapter.Dependencies{})
	wantCode(t, err, model.Validation)
	_, err = f.Open(ctx, model.Channel{Type: fakeprovider.TypeSendOnly, Configuration: f.Config()}, adapter.Dependencies{})
	if err == nil {
		t.Fatal("Open accepted a channel without a reference")
	}
}

// sink records accepted ingress and fails the scripted number of first calls.
type sink struct {
	mu       sync.Mutex
	fail     int
	accepted []model.Ingress
	got      chan struct{}
}

func newSink(fail int) *sink { return &sink{fail: fail, got: make(chan struct{}, 16)} }

func (s *sink) Accept(_ context.Context, in model.Ingress) (model.IngressReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail > 0 {
		s.fail--
		return model.IngressReceipt{}, model.NewError(model.Unavailable, "store down")
	}
	s.accepted = append(s.accepted, in)
	s.got <- struct{}{}
	return model.IngressReceipt{ID: model.ID(in.Envelope.EventID), Committed: true}, nil
}

func (*sink) EvaluateInteraction(context.Context, model.InteractionInput) (model.InteractionDecision, error) {
	return model.InteractionDecision{}, nil
}

func ingress(eventID string) model.Ingress {
	return model.Ingress{Envelope: model.Envelope{
		EventID: eventID, Kind: "message",
		Sender:  model.ExternalRef{Channel: chanA, Namespace: fakeprovider.Namespace, Kind: "user", ID: "u1"},
		Content: model.Content{Parts: []model.Part{{Kind: "text", Text: "hi " + eventID}}},
	}}
}

func TestIngressRedeliversUntilAccepted(t *testing.T) {
	f := fakeprovider.NewIngressOnly()
	rcv := open(t, f, chanA).(adapter.Receiver)
	script := f.Script(chanA)
	script.Push(ingress("e1"), ingress("e2"))

	failing := newSink(1)
	err := rcv.Run(t.Context(), failing)
	wantCode(t, err, model.Unavailable)
	if script.Pending() != 2 {
		t.Fatalf("pending = %d after a failed Accept, want 2", script.Pending())
	}

	ctx, cancel := context.WithCancel(t.Context())
	s := newSink(0)
	done := make(chan error, 1)
	go func() { done <- rcv.Run(ctx, s) }()
	waitN(t, s.got, 2)
	script.Push(ingress("e3"))
	waitN(t, s.got, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run after cancel = %v, want context.Canceled", err)
	}
	var ids []string
	for _, in := range s.accepted {
		ids = append(ids, in.Envelope.EventID)
	}
	if !slices.Equal(ids, []string{"e1", "e2", "e3"}) {
		t.Fatalf("accepted %v, want e1 e2 e3 in order", ids)
	}
	d := script.Deliveries()
	if len(d) != 4 || d[0].Acked || d[0].Err == nil || !d[1].Acked {
		t.Fatalf("deliveries = %+v, want one failed then three acked", d)
	}
}

func TestCloseStopsRun(t *testing.T) {
	f := fakeprovider.NewFull()
	inst := open(t, f, chanA)
	done := make(chan error, 1)
	go func() { done <- inst.(adapter.Receiver).Run(t.Context(), newSink(0)) }()
	time.Sleep(10 * time.Millisecond)
	if err := inst.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after Close = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Close")
	}
	_, err := inst.(adapter.Sender).Send(t.Context(), send(chanA, "op"))
	wantCode(t, err, model.Unavailable)
	_, err = inst.Capabilities(t.Context())
	wantCode(t, err, model.Unavailable)
	if err := inst.Close(t.Context()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func send(ch model.ChannelRef, op string) adapter.SendRequest {
	return adapter.SendRequest{
		Effect: adapter.Effect{
			Operation: model.OperationRef{Tenant: tenant, ID: model.ID(op)},
			Attempt:   model.ID(op + "-attempt"),
			Route:     model.ExternalRef{Channel: ch, Namespace: fakeprovider.Namespace, Kind: "room", ID: "room-1"},
		},
		Content: model.Content{Parts: []model.Part{{Kind: "text", Text: "reply"}}},
	}
}

func TestScriptedSendOutcomes(t *testing.T) {
	f := fakeprovider.NewSendOnly()
	snd := open(t, f, chanA).(adapter.Sender)
	script := f.Script(chanA)
	script.Respond(fakeprovider.Rejected, fakeprovider.Transient, fakeprovider.OutcomeUnknown)
	ctx := t.Context()

	out, err := snd.Send(ctx, send(chanA, "op1"))
	if err != nil || out.State != model.Rejected || model.CodeOf(out.Failure) != model.Permanent || len(out.References) != 0 {
		t.Fatalf("rejected = %+v, %v", out, err)
	}
	_, err = snd.Send(ctx, send(chanA, "op2"))
	wantCode(t, err, model.Transient)
	out, err = snd.Send(ctx, send(chanA, "op3"))
	if err != nil || out.State != model.Unknown || model.CodeOf(out.Failure) != model.OutcomeUnknown || len(out.References) != 0 {
		t.Fatalf("outcome unknown = %+v, %v", out, err)
	}
	out, err = snd.Send(ctx, send(chanA, "op4"))
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 || out.References[0].ID != "m2" {
		t.Fatalf("default accepted = %+v, %v; want reference m2 after the unknown send took m1", out, err)
	}
	script.Otherwise(fakeprovider.Transient)
	_, err = snd.Send(ctx, send(chanA, "op5"))
	wantCode(t, err, model.Transient)

	calls := script.Calls()
	var results []fakeprovider.Result
	for _, c := range calls {
		results = append(results, c.Result)
	}
	want := []fakeprovider.Result{fakeprovider.Rejected, fakeprovider.Transient, fakeprovider.OutcomeUnknown, fakeprovider.Accepted, fakeprovider.Transient}
	if !slices.Equal(results, want) {
		t.Fatalf("recorded results %v, want %v", results, want)
	}
	if !calls[2].Applied || calls[1].Applied || calls[0].Applied {
		t.Errorf("applied flags = %v %v %v, want only the unknown send applied", calls[0].Applied, calls[1].Applied, calls[2].Applied)
	}
}

func TestReconcileReportsProviderRecord(t *testing.T) {
	f := fakeprovider.NewFull()
	inst := open(t, f, chanA).(*fakeprovider.Full)
	script := f.Script(chanA)
	ctx := t.Context()
	script.Respond(fakeprovider.OutcomeUnknown, fakeprovider.Rejected, fakeprovider.Transient)
	lost, rej, unsent := send(chanA, "lost"), send(chanA, "rej"), send(chanA, "unsent")
	for _, r := range []adapter.SendRequest{lost, rej, unsent} {
		_, _ = inst.Send(ctx, r)
	}

	out, err := inst.Reconcile(ctx, adapter.ReconcileRequest{Effect: lost.Effect})
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 || out.References[0].ID != "m1" {
		t.Fatalf("reconcile lost answer = %+v, %v; want accepted m1", out, err)
	}
	out, err = inst.Reconcile(ctx, adapter.ReconcileRequest{Effect: rej.Effect})
	if err != nil || out.State != model.Rejected {
		t.Fatalf("reconcile rejected = %+v, %v", out, err)
	}
	out, err = inst.Reconcile(ctx, adapter.ReconcileRequest{Effect: unsent.Effect})
	if err != nil || out.State != model.Failed || model.CodeOf(out.Failure) != model.NotFound {
		t.Fatalf("reconcile unsent = %+v, %v; want failed/not_found", out, err)
	}
	if n := len(script.Calls()); n != 6 {
		t.Fatalf("calls = %d, want 3 sends and 3 reconciles", n)
	}
}

// A lost request applies nothing, so reconcile finds no record; a rate
// limit applies nothing and carries the scripted Retry-After.
func TestRequestLostAndRateLimited(t *testing.T) {
	f := fakeprovider.NewFull()
	inst := open(t, f, chanA).(*fakeprovider.Full)
	script := f.Script(chanA)
	ctx := t.Context()
	script.RetryAfter(7 * time.Minute)
	script.Respond(fakeprovider.RequestLost, fakeprovider.RateLimited)
	lost := send(chanA, "lost")
	out, err := inst.Send(ctx, lost)
	if err != nil || out.State != model.Unknown || model.CodeOf(out.Failure) != model.OutcomeUnknown || len(out.References) != 0 {
		t.Fatalf("request lost = %+v, %v; want outcome_unknown without reference", out, err)
	}
	_, err = inst.Send(ctx, send(chanA, "limited"))
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.RateLimited || me.RetryAfter != 7*time.Minute {
		t.Fatalf("rate limited = %v, want rate_limited with Retry-After 7m", err)
	}
	out, err = inst.Reconcile(ctx, adapter.ReconcileRequest{Effect: lost.Effect})
	if err != nil || out.State != model.Failed || model.CodeOf(out.Failure) != model.NotFound {
		t.Fatalf("reconcile lost request = %+v, %v; want failed/not_found", out, err)
	}
	for _, c := range script.Calls() {
		if c.Applied {
			t.Errorf("call %+v applied, want nothing applied", c)
		}
	}
}

func TestFullTargetedEffects(t *testing.T) {
	f := fakeprovider.NewFull()
	inst := open(t, f, chanA).(*fakeprovider.Full)
	ctx := t.Context()
	target := &model.ExternalRef{Channel: chanA, Namespace: fakeprovider.Namespace, Kind: "message", ID: "m9"}
	eff := send(chanA, "edit").Effect
	_, err := inst.Edit(ctx, adapter.EditRequest{Effect: eff})
	wantCode(t, err, model.Validation)
	eff.Target = target
	for _, call := range []func() (adapter.Outcome, error){
		func() (adapter.Outcome, error) { return inst.Edit(ctx, adapter.EditRequest{Effect: eff}) },
		func() (adapter.Outcome, error) {
			return inst.React(ctx, adapter.ReactRequest{Effect: eff, Emoji: "+1"})
		},
		func() (adapter.Outcome, error) { return inst.Delete(ctx, adapter.DeleteRequest{Effect: eff}) },
	} {
		out, err := call()
		if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 || out.References[0] != *target {
			t.Fatalf("targeted effect = %+v, %v; want accepted on the target", out, err)
		}
	}
	foreign := eff
	foreign.Target = &model.ExternalRef{Channel: chanB, Namespace: fakeprovider.Namespace, Kind: "message", ID: "m9"}
	_, err = inst.Delete(ctx, adapter.DeleteRequest{Effect: foreign})
	wantCode(t, err, model.Validation)
}

func TestInstancesOfOneTypeStayIsolated(t *testing.T) {
	f := fakeprovider.NewFull()
	a := open(t, f, chanA).(*fakeprovider.Full)
	b := open(t, f, chanB).(*fakeprovider.Full)
	ctx := t.Context()
	f.Script(chanB).Respond(fakeprovider.Rejected)

	outA, err := a.Send(ctx, send(chanA, "op"))
	if err != nil || outA.State != model.ProviderAccepted {
		t.Fatalf("a = %+v, %v", outA, err)
	}
	outB, err := b.Send(ctx, send(chanB, "op"))
	if err != nil || outB.State != model.Rejected {
		t.Fatalf("b took a's script: %+v, %v", outB, err)
	}
	outB, _ = b.Send(ctx, send(chanB, "op2"))
	// Overlapping provider IDs stay apart by channel.
	if outA.References[0].ID != outB.References[0].ID || outA.References[0] == outB.References[0] {
		t.Errorf("references %v and %v: want equal provider IDs on different channels", outA.References[0], outB.References[0])
	}
	_, err = a.Send(ctx, send(chanB, "cross"))
	wantCode(t, err, model.Validation)
	if n := len(f.Script(chanA).Calls()); n != 1 {
		t.Errorf("a recorded %d calls, want 1: a foreign route never reaches the provider", n)
	}
	f.Script(chanA).Push(ingress("only-a"))
	if f.Script(chanB).Pending() != 0 {
		t.Error("ingress pushed to a is pending on b")
	}
}

func TestRevokedPermission(t *testing.T) {
	f := fakeprovider.NewFull()
	inst := open(t, f, chanA).(*fakeprovider.Full)
	ctx := t.Context()
	f.Script(chanA).Revoke(model.CapabilitySend)
	_, err := inst.Send(ctx, send(chanA, "op"))
	wantCode(t, err, model.Unauthorized)
	if n := len(f.Script(chanA).Calls()); n != 0 {
		t.Fatalf("revoked send reached the provider: %d calls", n)
	}
	st, err := inst.Probe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range st {
		if (s.Name == model.CapabilitySend) == s.Available || (s.Name == model.CapabilitySend && s.Reason != model.Unauthorized) {
			t.Errorf("status %+v", s)
		}
	}
}

func waitN(t *testing.T, ch <-chan struct{}, n int) {
	t.Helper()
	for range n {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for delivery")
		}
	}
}

func wantCode(t *testing.T, err error, code model.ErrorCode) {
	t.Helper()
	if got := model.CodeOf(err); got != code {
		t.Fatalf("error = %v (code %q), want %q", err, got, code)
	}
}

// TestNamesPassModelRules checks every name the variants declare against
// model's third-party name rules, so a registry built on them accepts the
// descriptors.
func TestNamesPassModelRules(t *testing.T) {
	for _, f := range []*fakeprovider.Factory{fakeprovider.NewIngressOnly(), fakeprovider.NewSendOnly(), fakeprovider.NewFull(), fakeprovider.NewWebhook(), fakeprovider.NewInteractive(), fakeprovider.NewMail(), fakeprovider.NewTracker()} {
		d := f.Describe()
		if err := model.ValidateTypeKey(d.Type); err != nil {
			t.Errorf("type key %s: %v", d.Type, err)
		}
		if err := model.ValidateExtensionName(d.ConfigurationSchema.Name); err != nil {
			t.Errorf("config schema %s: %v", d.ConfigurationSchema.Name, err)
		}
		for _, ns := range d.IdentityNamespaces {
			if err := model.ValidateExtensionName(ns); err != nil {
				t.Errorf("identity namespace %s: %v", ns, err)
			}
		}
	}
}

// A send is reported under the provider message reference core addresses
// messages by, and an ownership refusal is a final rejection that reconcile
// reports the same way and that applies nothing.
func TestFullSendReferenceAndOwnershipRefusal(t *testing.T) {
	f := fakeprovider.NewFull()
	inst := open(t, f, chanA).(*fakeprovider.Full)
	ctx := t.Context()
	out, err := inst.Send(ctx, send(chanA, "one"))
	if err != nil || len(out.References) != 1 || out.References[0] != model.ProviderMessageRef(chanA, out.References[0].ID) {
		t.Fatalf("send = %+v, %v; want a provider message reference", out, err)
	}
	f.Script(chanA).Respond(fakeprovider.Unauthorized)
	eff := send(chanA, "edit").Effect
	eff.Target = &out.References[0]
	out, err = inst.Edit(ctx, adapter.EditRequest{Effect: eff})
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Unauthorized || out.Failure.Retry != model.NoRetry {
		t.Fatalf("refused edit = %+v, %v; want rejected, unauthorized, final", out, err)
	}
	rec, err := inst.Reconcile(ctx, adapter.ReconcileRequest{Effect: eff})
	if err != nil || rec.State != model.Rejected || model.CodeOf(rec.Failure) != model.Unauthorized {
		t.Fatalf("reconcile of a refused edit = %+v, %v; want the same rejection", rec, err)
	}
	for _, c := range f.Script(chanA).Calls() {
		if c.Method == fakeprovider.MethodEdit && c.Applied {
			t.Errorf("refused edit applied: %+v", c)
		}
	}
}
