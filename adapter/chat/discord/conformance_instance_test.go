package discord

import (
	"context"
	"net/http"
	"slices"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// recordingFactory opens instances that tell the provider the clock and
// each effect the engine hands them, declaring caps and implementing
// exactly their interfaces.
type recordingFactory struct {
	*Factory
	p    *provider
	caps []model.Capability
}

func (f *recordingFactory) Describe() adapter.Descriptor {
	d := f.Factory.Describe()
	d.Capabilities = f.caps
	return d
}

func (f *recordingFactory) Open(ctx context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	if deps.Credentials != nil {
		deps.Credentials = suiteCredentials{next: deps.Credentials}
	}
	inst, err := f.Factory.Open(ctx, ch, deps)
	if err != nil {
		return nil, err
	}
	f.p.mu.Lock()
	f.p.clock = deps.Clock
	f.p.mu.Unlock()
	x := inst.(*Instance)
	x.gw.retry, x.gw.invalid = 0, 0
	x.handled = func(seq int64, refusal error) { f.p.handled(ch.Ref, seq, refusal) }
	r := &recordingInstance{x: x, p: f.p, pre: f.p.prefix(ch.Ref), caps: f.caps}
	members := slices.Contains(f.caps, model.CapabilityMembership)
	switch {
	case f.p.transport == ReceiveGateway && members:
		return &gatewayMemberInstance{recordingInstance: r}, nil
	case f.p.transport == ReceiveGateway:
		return &gatewayInstance{recordingInstance: r}, nil
	case members:
		return &endpointMemberInstance{recordingInstance: r}, nil
	}
	return &endpointInstance{recordingInstance: r}, nil
}

// recordingInstance forwards the capabilities every run of the suite
// declares, recording which effect made which calls.
type recordingInstance struct {
	x    *Instance
	p    *provider
	pre  string
	caps []model.Capability
}

func (r *recordingInstance) record(rec recorded, call func()) {
	r.p.serial.Lock()
	defer r.p.serial.Unlock()
	rec.from = len(r.p.f.calls(r.pre))
	call()
	rec.to = len(r.p.f.calls(r.pre))
	r.p.mu.Lock()
	if r.p.effects == nil {
		r.p.effects = map[string][]recorded{}
	}
	r.p.effects[r.pre] = append(r.p.effects[r.pre], rec)
	r.p.mu.Unlock()
}

func (r *recordingInstance) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	st, err := r.x.Capabilities(ctx)
	return slices.DeleteFunc(st, func(s model.CapabilityStatus) bool { return !slices.Contains(r.caps, s.Name) }), err
}

func (r *recordingInstance) Close(ctx context.Context) error { return r.x.Close(ctx) }

func (r *recordingInstance) Handler(s adapter.Sink) http.Handler { return r.x.Handler(s) }

func (r *recordingInstance) Send(ctx context.Context, req adapter.SendRequest) (out adapter.Outcome, err error) {
	r.record(recorded{capability: model.CapabilitySend, effect: req.Effect, route: req.Route}, func() { out, err = r.x.Send(ctx, req) })
	return out, err
}

func (r *recordingInstance) Edit(ctx context.Context, req adapter.EditRequest) (out adapter.Outcome, err error) {
	r.record(recorded{capability: model.CapabilityEdit, effect: req.Effect, route: req.Route}, func() { out, err = r.x.Edit(ctx, req) })
	return out, err
}

func (r *recordingInstance) Delete(ctx context.Context, req adapter.DeleteRequest) (out adapter.Outcome, err error) {
	r.record(recorded{capability: model.CapabilityDelete, effect: req.Effect, route: req.Route}, func() { out, err = r.x.Delete(ctx, req) })
	return out, err
}

func (r *recordingInstance) React(ctx context.Context, req adapter.ReactRequest) (out adapter.Outcome, err error) {
	r.record(recorded{capability: model.CapabilityReact, effect: req.Effect, route: req.Route, emoji: req.Emoji, remove: req.Remove},
		func() { out, err = r.x.React(ctx, req) })
	return out, err
}

func (r *recordingInstance) Reconcile(ctx context.Context, req adapter.ReconcileRequest) (out adapter.Outcome, err error) {
	r.record(recorded{capability: model.CapabilityReconcile, effect: req.Effect, route: req.Route}, func() { out, err = r.x.Reconcile(ctx, req) })
	return out, err
}

func (r *recordingInstance) History(ctx context.Context, req adapter.HistoryRequest) (page adapter.HistoryPage, err error) {
	r.record(recorded{capability: model.CapabilityHistory}, func() { page, err = r.x.History(ctx, req) })
	return page, err
}

func (r *recordingInstance) Refetch(ctx context.Context, req adapter.RefetchRequest) (env model.Envelope, err error) {
	r.record(recorded{capability: model.CapabilityRefetch}, func() { env, err = r.x.Refetch(ctx, req) })
	return env, err
}

func (r *recordingInstance) Signal(ctx context.Context, req adapter.SignalRequest) (err error) {
	r.record(recorded{capability: model.CapabilitySignal, route: req.Route, kind: req.Kind, effect: adapter.Effect{Route: req.Route, Audience: req.Audience}},
		func() { err = r.x.Signal(ctx, req) })
	return err
}

func (r *recordingInstance) Respond(ctx context.Context, req adapter.RespondRequest) (out adapter.Outcome, err error) {
	r.record(recorded{capability: model.CapabilityRespond, effect: req.Effect, route: req.Route, mode: req.Mode, followUp: req.FollowUp},
		func() { out, err = r.x.Respond(ctx, req) })
	return out, err
}

func (r *recordingInstance) changeMembership(ctx context.Context, req adapter.MembershipRequest) (out adapter.Outcome, err error) {
	r.record(recorded{capability: model.CapabilityMembership, effect: req.Effect, route: req.Route, member: req.Participant, remove: req.Remove},
		func() { out, err = r.x.ChangeMembership(ctx, req) })
	return out, err
}

func (r *recordingInstance) archive(ctx context.Context, req adapter.ArchiveRequest) (out adapter.Outcome, err error) {
	r.record(recorded{capability: model.CapabilityArchive, effect: req.Effect, route: req.Route, items: req.Items, undo: req.Undo},
		func() { out, err = r.x.Archive(ctx, req) })
	return out, err
}

func (r *recordingInstance) run(ctx context.Context, sink adapter.Sink) error {
	return r.x.Run(ctx, sink)
}

// The four shapes of instance the suite runs: the gateway receiver or the
// endpoint alone, with or without the capabilities of a thread, membership
// and archive, which the suite declares together (per thread only).
type (
	gatewayInstance        struct{ *recordingInstance }
	gatewayMemberInstance  struct{ *recordingInstance }
	endpointInstance       struct{ *recordingInstance }
	endpointMemberInstance struct{ *recordingInstance }
)

func (i *gatewayInstance) Run(ctx context.Context, s adapter.Sink) error { return i.run(ctx, s) }

func (i *gatewayMemberInstance) Run(ctx context.Context, s adapter.Sink) error { return i.run(ctx, s) }

func (i *gatewayMemberInstance) ChangeMembership(ctx context.Context, req adapter.MembershipRequest) (adapter.Outcome, error) {
	return i.changeMembership(ctx, req)
}

func (i *gatewayMemberInstance) Archive(ctx context.Context, req adapter.ArchiveRequest) (adapter.Outcome, error) {
	return i.archive(ctx, req)
}

func (i *endpointMemberInstance) Archive(ctx context.Context, req adapter.ArchiveRequest) (adapter.Outcome, error) {
	return i.archive(ctx, req)
}

func (i *endpointMemberInstance) ChangeMembership(ctx context.Context, req adapter.MembershipRequest) (adapter.Outcome, error) {
	return i.changeMembership(ctx, req)
}
