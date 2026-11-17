package uma_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store/memory"
)

const watchType model.TypeKey = "com.example.watch"

// watchAdapter's receiver fails its first connection with a stated wait,
// reports connected on the second and holds.
type watchAdapter struct {
	runs chan int
	n    int
}

func (*watchAdapter) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: watchType, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: "com.example.watch-config", Version: 1},
		Capabilities:        []model.Capability{model.CapabilityReceive},
		Events:              []string{model.EventMessageReceived},
		IdentityNamespaces:  []string{"com.example.watch.user"},
	}
}
func (*watchAdapter) Validate(model.Extension) []model.ValidationIssue { return nil }
func (a *watchAdapter) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	return &watchInstance{a: a}, nil
}

type watchInstance struct {
	a      *watchAdapter
	report func(bool)
}

func (*watchInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return []model.CapabilityStatus{{Name: model.CapabilityReceive, Available: true}}, nil
}
func (*watchInstance) Close(context.Context) error        { return nil }
func (i *watchInstance) ReportReceiver(report func(bool)) { i.report = report }
func (i *watchInstance) Run(ctx context.Context, _ adapter.Sink) error {
	n := i.a.n
	i.a.n++
	if n == 0 {
		e := model.NewError(model.RateLimited, "slow down")
		e.RetryAfter = time.Hour
		i.a.runs <- n
		return e
	}
	i.report(true)
	i.a.runs <- n
	<-ctx.Done()
	return ctx.Err()
}

// holdWaiter parks the supervisor's waits, and tells of each.
type holdWaiter struct {
	waits chan time.Duration
	free  chan struct{}
}

func (w holdWaiter) Wait(ctx context.Context, d time.Duration) error {
	if d >= time.Minute/6 && d <= time.Minute/3*11/10 { // the lease renewal (spread per channel)
		<-ctx.Done()
		return ctx.Err()
	}
	w.waits <- d
	select {
	case <-w.free:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Runtime.ReceiverStatus carries the supervisor's phase, counters, reason,
// retry time and the adapter's link to the host through the public types.
func TestRuntimeReceiverStatusReportsTheCall(t *testing.T) {
	ctx := t.Context()
	a := &watchAdapter{runs: make(chan int, 4)}
	w := holdWaiter{waits: make(chan time.Duration, 1), free: make(chan struct{})}
	clock := portstest.NewFixedClock(time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC))
	e, err := uma.New(uma.Config{
		Store: memory.New(memory.Options{Clock: clock}),
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters: []adapter.Factory{a}, Waiter: w, Clock: clock, IDs: portstest.NewSequenceIDs(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close(ctx) }()
	host := model.Principal{Tenant: "acme", Actor: "host"}
	c, err := e.For(host)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := c.Configuration().ConfigureChannel(ctx, model.ConfigureChannel{
		Command: model.Command{Call: model.Call{Correlation: "c"}, Operation: "channel"},
		Channel: model.Channel{Ref: model.ChannelRef{Tenant: "acme"}, Type: watchType, Configuration: model.Extension{Name: "com.example.watch-config", Version: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	channel, _ := acc.Operation.Result.AsChannel()
	call := uma.RuntimeCall{Principal: host, Call: model.Call{Correlation: "rx"}}
	done := make(chan error, 1)
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { done <- e.Runtime().Receive(rctx, call, channel, uma.ReceiveOwner("host-a")) }()
	next := func() {
		t.Helper()
		select {
		case <-a.runs:
		case <-time.After(10 * time.Second):
			t.Fatal("receiver did not run")
		}
	}
	status := func() uma.ReceiverStatus {
		t.Helper()
		st, err := e.Runtime().ReceiverStatus(ctx, call, channel)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	next()
	select {
	case d := <-w.waits:
		if d != time.Hour {
			t.Fatalf("wait = %v, want the stated hour", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor did not wait")
	}
	st := status()
	want := uma.ReceiverStatus{
		Owner: "host-a", Until: st.Until, Phase: uma.ReceiverRetrying, Since: clock.Now(), Failures: 1,
		Reason: model.RateLimited, RetryAt: clock.Now().Add(time.Hour),
	}
	if st != want || st.Until.IsZero() {
		t.Fatalf("status while retrying = %+v, want %+v", st, want)
	}
	if ms, ok := e.Runtime().ReceiverState(call, channel); !ok || ms.Owner != "host-a" || !ms.Until.IsZero() ||
		ms.Phase != st.Phase || ms.Failures != st.Failures || ms.Reason != st.Reason || ms.RetryAt != st.RetryAt {
		t.Fatalf("ReceiverState = %+v, %v; want the memory fields of %+v", ms, ok, st)
	}
	w.free <- struct{}{}
	next()
	st = status()
	want = uma.ReceiverStatus{
		Owner: "host-a", Until: st.Until, Phase: uma.ReceiverRunning, Since: clock.Now(), Failures: 1,
		Reason: model.RateLimited, Link: uma.ReceiverConnected,
	}
	if st != want {
		t.Fatalf("status after reconnecting = %+v, want %+v", st, want)
	}
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Receive = %v", err)
	}
	if _, ok := e.Runtime().ReceiverState(call, channel); ok {
		t.Fatal("ReceiverState after Receive returned")
	}
}

// The public phase and link names are the runtime's: the conversion in
// Runtime.ReceiverStatus is by value.
func TestReceiverStatusNamesMatchTheRuntime(t *testing.T) {
	for _, p := range [][2]string{
		{string(uma.ReceiverRunning), string(runtime.ReceiverRunning)},
		{string(uma.ReceiverRetrying), string(runtime.ReceiverRetrying)},
		{string(uma.ReceiverEnded), string(runtime.ReceiverEnded)},
		{string(uma.ReceiverConnecting), string(runtime.ReceiverConnecting)},
		{string(uma.ReceiverConnected), string(runtime.ReceiverConnected)},
	} {
		if p[0] != p[1] || p[0] == "" {
			t.Fatalf("public %q, runtime %q", p[0], p[1])
		}
	}
}

// ReceiveStartSpread refuses a negative window.
func TestReceiveStartSpreadNegativeRefused(t *testing.T) {
	ctx := t.Context()
	clock := portstest.NewFixedClock(time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC))
	a := &watchAdapter{runs: make(chan int, 4)}
	e, err := uma.New(uma.Config{
		Store: memory.New(memory.Options{Clock: clock}),
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters: []adapter.Factory{a}, Clock: clock, IDs: portstest.NewSequenceIDs(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close(ctx) }()
	host := model.Principal{Tenant: "acme", Actor: "host"}
	c, _ := e.For(host)
	acc, err := c.Configuration().ConfigureChannel(ctx, model.ConfigureChannel{
		Command: model.Command{Call: model.Call{Correlation: "c"}, Operation: "channel"},
		Channel: model.Channel{Ref: model.ChannelRef{Tenant: "acme"}, Type: watchType, Configuration: model.Extension{Name: "com.example.watch-config", Version: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	channel, _ := acc.Operation.Result.AsChannel()
	call := uma.RuntimeCall{Principal: host, Call: model.Call{Correlation: "rx"}}
	err = e.Runtime().Receive(ctx, call, channel, uma.ReceiveStartSpread(-time.Second))
	if !model.IsCode(err, model.Validation) {
		t.Fatalf("err = %v, want validation", err)
	}
}
