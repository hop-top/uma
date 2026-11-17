package fakeprovider

import (
	"context"
	"sync"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// instance is the state shared by the variant types: the bound channel, its
// script and the declared capabilities. Variant types expose only the
// methods their capabilities need.
type instance struct {
	channel model.Channel
	script  *Script
	caps    []model.Capability
	creds   ports.Credentials
	clock   ports.Clock
	sent    func(model.ExternalRef) []model.ExternalRef

	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

func newInstance(channel model.Channel, script *Script, caps []model.Capability) *instance {
	return &instance{channel: channel, script: script, caps: caps, done: make(chan struct{})}
}

func errClosed() error { return model.NewError(model.Unavailable, "fake provider instance is closed") }

func (i *instance) isClosed() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.closed
}

func (i *instance) capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i.isClosed() {
		return nil, errClosed()
	}
	out := make([]model.CapabilityStatus, len(i.caps))
	for k, c := range i.caps {
		out[k] = model.CapabilityStatus{Name: c, Available: true, Limits: stateLimits(c, i.script.shape)}
		if i.script.isRevoked(c) {
			out[k].Available, out[k].Reason = false, model.Unauthorized
		}
	}
	return out, nil
}

// close is idempotent and stops a running Receiver.Run.
func (i *instance) close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.closed {
		i.closed = true
		close(i.done)
	}
	return nil
}

// ready checks a call before it reaches the provider.
func (i *instance) ready(ctx context.Context, c model.Capability) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if i.isClosed() {
		return errClosed()
	}
	if i.script.isRevoked(c) {
		return model.NewError(model.Unauthorized, "provider permission for "+string(c)+" revoked")
	}
	return nil
}

// authenticate resolves the channel's credential for one outbound call
// when the instance has a credentials port. The credential itself is not
// used: the scripted provider only needs to know it was issued.
func (i *instance) authenticate(ctx context.Context) error {
	if i.creds == nil {
		return nil
	}
	_, err := i.creds.Resolve(ctx, ports.CredentialRequest{
		Channel: i.channel.Ref, Reference: i.channel.CredentialRef, Purpose: CredentialPurpose,
		Accept: []ports.CredentialKind{ports.KindSecret, ports.KindOAuth2},
	})
	return err
}

// run delivers queued ingress to sink in order until ctx ends or the
// instance closes. An ingress is acknowledged, and leaves the queue, only
// once Accept succeeds; an Accept error ends Run and the ingress stays queued
// for the next Run. Every receipt is acknowledged: the fake makes no
// durability demand of its own.
func (i *instance) run(ctx context.Context, sink adapter.Sink) error {
	if sink == nil {
		return model.NewError(model.Validation, "sink required")
	}
	if err := i.ready(ctx, model.CapabilityReceive); err != nil {
		return err
	}
	for {
		if i.isClosed() {
			return nil
		}
		if err := i.deliverNext(ctx, sink); err != nil {
			return err
		}
		if i.script.Pending() > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-i.done:
			return nil
		case <-i.script.wake:
		}
	}
}

func (i *instance) deliverNext(ctx context.Context, sink adapter.Sink) error {
	s := i.script
	s.delivering.Lock()
	defer s.delivering.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if i.isClosed() {
		return nil
	}
	in, ok := s.head()
	if !ok {
		return nil
	}
	receipt, err := sink.Accept(ctx, in)
	s.delivered(Delivery{Ingress: in, Receipt: receipt, Err: err, Acked: err == nil})
	return err
}

// effect performs one outbound call on the channel's own route.
func (i *instance) effect(ctx context.Context, c model.Capability, call Call, newRef bool) (adapter.Outcome, error) {
	if err := i.ready(ctx, c); err != nil {
		return adapter.Outcome{}, err
	}
	if err := i.authenticate(ctx); err != nil {
		return adapter.Outcome{}, err
	}
	if call.Effect.Route.Channel != i.channel.Ref {
		return adapter.Outcome{}, model.NewError(model.Validation, "route belongs to another channel")
	}
	if !newRef && call.Effect.Target == nil {
		return adapter.Outcome{}, model.NewError(model.Validation, "target required")
	}
	if call.Effect.Target != nil && call.Effect.Target.Channel != i.channel.Ref {
		return adapter.Outcome{}, model.NewError(model.Validation, "target belongs to another channel")
	}
	call.Effect = cloneEffect(call.Effect)
	call.Content = call.Content.Clone()
	return i.script.apply(call, newRef, i.sent)
}

func (i *instance) send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	return i.effect(ctx, model.CapabilitySend, Call{Method: MethodSend, Effect: r.Effect, Content: r.Content}, true)
}

func (i *instance) reconcile(ctx context.Context, e adapter.Effect) (adapter.Outcome, error) {
	if err := i.ready(ctx, model.CapabilityReconcile); err != nil {
		return adapter.Outcome{}, err
	}
	if e.Route.Channel != i.channel.Ref {
		return adapter.Outcome{}, model.NewError(model.Validation, "route belongs to another channel")
	}
	if err := i.authenticate(ctx); err != nil {
		return adapter.Outcome{}, err
	}
	return i.script.reconcile(e), nil
}

// IngressOnly is the ingress-only variant: receive only.
type IngressOnly struct{ i *instance }

// Capabilities reports receive.
func (x *IngressOnly) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	return x.i.capabilities(ctx)
}

// Close closes the instance; it is idempotent.
func (x *IngressOnly) Close(context.Context) error { return x.i.close() }

// Run delivers the script's queued ingress to sink.
func (x *IngressOnly) Run(ctx context.Context, sink adapter.Sink) error { return x.i.run(ctx, sink) }

// SendOnly is the send-only variant: send only.
type SendOnly struct{ i *instance }

// Capabilities reports send.
func (x *SendOnly) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	return x.i.capabilities(ctx)
}

// Close closes the instance; it is idempotent.
func (x *SendOnly) Close(context.Context) error { return x.i.close() }

// Send sends a new message under the next scripted result.
func (x *SendOnly) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	return x.i.send(ctx, r)
}

// Full is the full variant: receive, send, edit, delete, react, reconcile,
// extension_execute, probe_permissions, receipt, membership and history.
type Full struct{ i *instance }

// Capabilities reports every declared capability.
func (x *Full) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	return x.i.capabilities(ctx)
}

// Close closes the instance; it is idempotent.
func (x *Full) Close(context.Context) error { return x.i.close() }

// Run delivers the script's queued ingress to sink.
func (x *Full) Run(ctx context.Context, sink adapter.Sink) error { return x.i.run(ctx, sink) }

// Send sends a new message under the next scripted result.
func (x *Full) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	return x.i.send(ctx, r)
}

// Edit edits the target message under the next scripted result.
func (x *Full) Edit(ctx context.Context, r adapter.EditRequest) (adapter.Outcome, error) {
	return x.i.effect(ctx, model.CapabilityEdit, Call{Method: MethodEdit, Effect: r.Effect, Content: r.Content}, false)
}

// Delete deletes the target message under the next scripted result.
func (x *Full) Delete(ctx context.Context, r adapter.DeleteRequest) (adapter.Outcome, error) {
	return x.i.effect(ctx, model.CapabilityDelete, Call{Method: MethodDelete, Effect: r.Effect}, false)
}

// React adds or removes a reaction on the target under the next scripted
// result.
func (x *Full) React(ctx context.Context, r adapter.ReactRequest) (adapter.Outcome, error) {
	return x.i.effect(ctx, model.CapabilityReact, Call{Method: MethodReact, Effect: r.Effect, Emoji: r.Emoji, Remove: r.Remove}, false)
}

// SendReceipt sends a delivery or read receipt for the target under the
// next scripted result; the call records the receipt kind as Kind.
func (x *Full) SendReceipt(ctx context.Context, r adapter.ReceiptRequest) (adapter.Outcome, error) {
	return x.i.effect(ctx, model.CapabilityReceipt, Call{Method: MethodReceipt, Effect: r.Effect, Kind: r.Kind}, false)
}

// ChangeMembership adds the participant to the route's conversation, or
// removes it, under the next scripted result. The effect has no target.
func (x *Full) ChangeMembership(ctx context.Context, r adapter.MembershipRequest) (adapter.Outcome, error) {
	return x.i.effect(ctx, model.CapabilityMembership, Call{Method: MethodMembership, Effect: r.Effect, Participant: r.Participant, Remove: r.Remove}, true)
}

// Execute applies an ExtensionPin action under the next scripted result.
// Any other action is model.SchemaMismatch and never reaches the provider.
func (x *Full) Execute(ctx context.Context, r adapter.ExecuteRequest) (adapter.Outcome, error) {
	if r.Action.Name != ExtensionPin || r.Action.Version != ExtensionPinVersion {
		return adapter.Outcome{}, model.NewError(model.SchemaMismatch, "fake provider: unknown extension action")
	}
	a := r.Action.Clone()
	return x.i.effect(ctx, model.CapabilityExtensionExecute, Call{Method: MethodExecute, Effect: r.Effect, Action: &a}, true)
}

// Reconcile reports what the provider recorded for the effect's operation:
// model.ProviderAccepted with references when an attempt was applied (even
// one whose answer was lost), model.Rejected when the latest decided attempt
// was refused, and model.Failed with a model.NotFound failure when no attempt
// reached the provider, so a retry is safe.
func (x *Full) Reconcile(ctx context.Context, r adapter.ReconcileRequest) (adapter.Outcome, error) {
	return x.i.reconcile(ctx, r.Effect)
}

// Probe reports current provider permissions, as Capabilities does.
func (x *Full) Probe(ctx context.Context) ([]model.CapabilityStatus, error) {
	if err := x.i.ready(ctx, model.CapabilityProbePermissions); err != nil {
		return nil, err
	}
	return x.i.capabilities(ctx)
}

var (
	_ adapter.Receiver = (*IngressOnly)(nil)

	_ adapter.Sender = (*SendOnly)(nil)

	_ adapter.Receiver          = (*Full)(nil)
	_ adapter.Sender            = (*Full)(nil)
	_ adapter.Editor            = (*Full)(nil)
	_ adapter.Deleter           = (*Full)(nil)
	_ adapter.Reactor           = (*Full)(nil)
	_ adapter.Reconciler        = (*Full)(nil)
	_ adapter.ExtensionExecutor = (*Full)(nil)
	_ adapter.PermissionProber  = (*Full)(nil)
)
