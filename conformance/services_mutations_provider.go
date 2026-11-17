package conformance

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// mutationProvider is the provider side of the mutation adapter: it
// applies every effect, records the mutations that reached it and runs
// the case's hook while it holds a mutation.
type mutationProvider struct {
	mu    sync.Mutex
	calls []adapter.Effect
	kinds []model.Capability
	// details holds, per call, what the effect carried besides its
	// effect, such as a receipt's kind; empty when nothing.
	details []string
	during  func(model.Capability)
	seq     int
	// answer, when set, replaces the applied answer for a capability it
	// returns true for.
	answer func(model.Capability) (adapter.Outcome, bool)
}

func (p *mutationProvider) hold(fn func(model.Capability)) {
	p.mu.Lock()
	p.during = fn
	p.mu.Unlock()
}

func (p *mutationProvider) log() []model.Capability {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.kinds)
}

// mutate records one mutation, runs the hook and answers it applied; a
// reaction reports the provider message that carried it.
func (p *mutationProvider) mutate(c model.Capability, e adapter.Effect, detail ...string) (adapter.Outcome, error) {
	p.mu.Lock()
	p.calls = append(p.calls, e)
	p.kinds = append(p.kinds, c)
	p.details = append(p.details, strings.Join(detail, ","))
	hook, answer := p.during, p.answer
	p.seq++
	n := p.seq
	p.mu.Unlock()
	if hook != nil {
		hook(c)
	}
	if answer != nil {
		if out, ok := answer(c); ok {
			return out, nil
		}
	}
	out := adapter.Outcome{State: model.ProviderAccepted}
	if c == model.CapabilityReact {
		out.References = []model.ExternalRef{model.ProviderMessageRef(e.Route.Channel, fmt.Sprintf("reaction-%d", n))}
	}
	return out, nil
}

type mutationInstance struct {
	caps []model.Capability
	p    *mutationProvider
}

func (i *mutationInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return available(i.caps), nil
}
func (i *mutationInstance) Close(context.Context) error { return nil }
func (i *mutationInstance) Send(_ context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{model.ProviderMessageRef(r.Route.Channel, "sent-"+string(r.Attempt))}}, nil
}
func (i *mutationInstance) Edit(_ context.Context, r adapter.EditRequest) (adapter.Outcome, error) {
	return i.p.mutate(model.CapabilityEdit, r.Effect)
}
func (i *mutationInstance) Delete(_ context.Context, r adapter.DeleteRequest) (adapter.Outcome, error) {
	return i.p.mutate(model.CapabilityDelete, r.Effect)
}
func (i *mutationInstance) React(_ context.Context, r adapter.ReactRequest) (adapter.Outcome, error) {
	return i.p.mutate(model.CapabilityReact, r.Effect)
}
func (i *mutationInstance) Refetch(context.Context, adapter.RefetchRequest) (model.Envelope, error) {
	return model.Envelope{}, model.NewError(model.NotFound, "no provider copy")
}

// sendOnlyInstance sends only.
type sendOnlyInstance struct{ caps []model.Capability }

func (i sendOnlyInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return available(i.caps), nil
}
func (sendOnlyInstance) Close(context.Context) error { return nil }
func (sendOnlyInstance) Send(context.Context, adapter.SendRequest) (adapter.Outcome, error) {
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

func available(caps []model.Capability) []model.CapabilityStatus {
	out := make([]model.CapabilityStatus, len(caps))
	for i, c := range caps {
		out[i] = model.CapabilityStatus{Name: c, Available: true}
	}
	return out
}

var (
	_ adapter.Editor    = (*mutationInstance)(nil)
	_ adapter.Deleter   = (*mutationInstance)(nil)
	_ adapter.Reactor   = (*mutationInstance)(nil)
	_ adapter.Refetcher = (*mutationInstance)(nil)
)

// deliver runs Runtime.Run workers until the operation settles.
func (env *mutationEnv) deliver(ref model.OperationRef) model.Operation {
	env.t.Helper()
	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: serviceTenant, Actor: "worker"}, Call: model.Call{Correlation: "run"}}
	ended := make(chan error, 1)
	go func() {
		ended <- env.engine.Runtime().Run(ctx, call, uma.WorkerOptions{Owner: "worker", Limit: 4, Poll: time.Millisecond})
	}()
	deadline := time.Now().Add(time.Minute)
	for {
		op := env.get(ref)
		if settled(op) {
			cancel()
			<-ended
			return op
		}
		if time.Now().After(deadline) {
			env.t.Fatalf("operation stuck at %+v", op)
		}
		select {
		case err := <-ended:
			env.t.Fatalf("Run returned early: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
}
