package conformance

import (
	"context"
	"slices"
	"sync"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// faultSend scripts one provider answer; applied reports whether the
// provider carried out the effect, whatever the caller is told.
type faultSend func(ctx context.Context, r adapter.SendRequest) (out adapter.Outcome, err error, applied bool)

// faultProvider is the provider side of the fault cases. It outlives every
// process: what it applied stays applied across crashes and restarts.
type faultProvider struct {
	reconciles bool
	// settle is the declared reconcile settle window; lag delays, from the
	// attempt's record time read on now, when a search shows an applied
	// send.
	settle time.Duration
	lag    time.Duration
	now    func() time.Time
	// refetches declares the refetch capability; current is the
	// provider's copy of each message, by provider message ID, that a
	// refetch reads.
	refetches bool
	current   map[string]model.Envelope
	// mutations declares the edit capability; edits holds the requests
	// that reached the provider.
	mutations bool
	edits     []adapter.EditRequest
	// singleReactions declares single-reaction cardinality.
	singleReactions bool
	// workItemActs declares work_item_act (close from open, reopen from
	// closed); acted holds the acts that reached the provider and
	// actApplied those it applied, each answered as applied.
	workItemActs bool
	acted        []adapter.WorkItemActRequest
	actApplied   []adapter.WorkItemActRequest

	mu        sync.Mutex
	script    []faultSend
	sends     []adapter.SendRequest
	applied   []adapter.SendRequest
	calls     []string
	reconcile func()
	inbound   chan model.Ingress
	accepted  chan accepted
}

type accepted struct {
	receipt model.IngressReceipt
	err     error
}

func newFaultProvider(reconciles bool) *faultProvider {
	return &faultProvider{reconciles: reconciles, inbound: make(chan model.Ingress), accepted: make(chan accepted, 16)}
}

// respond queues scripted answers, consumed in send order. Without one, a
// send is applied and accepted.
func (p *faultProvider) respond(fns ...faultSend) {
	p.mu.Lock()
	p.script = append(p.script, fns...)
	p.mu.Unlock()
}

// onReconcile runs fn when a reconciliation reaches the provider.
func (p *faultProvider) onReconcile(fn func()) {
	p.mu.Lock()
	p.reconcile = fn
	p.mu.Unlock()
}

func (p *faultProvider) log() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

func (p *faultProvider) sent() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sends)
}

func (p *faultProvider) ref(r adapter.SendRequest) model.ExternalRef {
	return model.ExternalRef{Channel: r.Route.Channel, Namespace: model.ProviderMessageNamespace, Kind: model.RefKindMessage, ID: "pm-" + string(r.Attempt)}
}

func (p *faultProvider) Describe() adapter.Descriptor {
	caps := []model.Capability{model.CapabilitySend, model.CapabilityReceive}
	if p.reconciles {
		caps = append(caps, model.CapabilityReconcile)
	}
	if p.refetches {
		caps = append(caps, model.CapabilityRefetch)
	}
	if p.mutations {
		caps = append(caps, model.CapabilityEdit)
	}
	var acts []adapter.WorkItemTransition
	if p.workItemActs {
		caps = append(caps, model.CapabilityWorkItemAct)
		acts = []adapter.WorkItemTransition{
			{To: model.WorkItemClosed, From: []string{model.WorkItemOpen}, Reasons: []string{"completed", "not_planned"}},
			{To: model.WorkItemOpen, From: []string{model.WorkItemClosed}, Reasons: []string{"reopened"}},
		}
	}
	exchange := adapter.Exchange{MessageIDs: adapter.MessageIDsProvider}
	if p.singleReactions {
		exchange.Reactions = adapter.ReactionsSingle
	}
	return adapter.Descriptor{
		Type: faultType, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: string(faultType) + ".config", Version: 1},
		Capabilities:        caps,
		Events:              []string{model.EventMessageReceived, model.EventDeliveryUpdated},
		IdentityNamespaces:  []string{faultNS},
		ReconcileSettle:     p.settle,
		Exchange:            exchange,
		WorkItemActs:        acts,
	}
}

func (p *faultProvider) Validate(model.Extension) []model.ValidationIssue { return nil }

func (p *faultProvider) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	base := &faultInstance{p: p}
	if p.reconciles {
		return &faultReconciler{base}, nil
	}
	return base, nil
}

type faultInstance struct{ p *faultProvider }

func (i *faultInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	var out []model.CapabilityStatus
	for _, c := range i.p.Describe().Capabilities {
		out = append(out, model.CapabilityStatus{Name: c, Available: true})
	}
	return out, nil
}

func (i *faultInstance) Close(context.Context) error { return nil }

func (i *faultInstance) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	p := i.p
	p.mu.Lock()
	var fn faultSend
	if len(p.script) > 0 {
		fn, p.script = p.script[0], p.script[1:]
	}
	p.sends = append(p.sends, r)
	p.calls = append(p.calls, "send")
	p.mu.Unlock()
	if fn == nil {
		fn = applyAccepted
	}
	out, err, applied := fn(ctx, r)
	if applied {
		p.mu.Lock()
		p.applied = append(p.applied, r)
		p.mu.Unlock()
		if err == nil && out.State == model.ProviderAccepted && len(out.References) == 0 {
			out.References = []model.ExternalRef{p.ref(r)}
		}
	}
	return out, err
}

// ActWorkItem applies every act and answers with the work item moved to the
// requested status.
func (i *faultInstance) ActWorkItem(_ context.Context, r adapter.WorkItemActRequest) (adapter.WorkItemActOutcome, error) {
	p := i.p
	p.mu.Lock()
	defer p.mu.Unlock()
	p.acted = append(p.acted, r)
	p.actApplied = append(p.actApplied, r)
	p.calls = append(p.calls, "act")
	reason := r.Reason
	if reason == "" {
		reason = map[string]string{model.WorkItemClosed: "completed", model.WorkItemOpen: "reopened"}[r.Status]
	}
	return adapter.WorkItemActOutcome{
		Outcome: adapter.Outcome{State: model.Delivered},
		State: &model.WorkItemState{
			Subject: "Build fails", URL: "https://faults.example/issues/7", Status: r.Status, RawStatus: r.Status + ":" + reason,
			Requester: model.ExternalRef{Channel: r.Route.Channel, Namespace: faultNS, Kind: "user", ID: "reporter"},
			Binding:   model.WorkItemBindingActive,
		},
		SourceRevision: "fault-act-" + string(r.Attempt), OccurredAt: p.now(),
	}, nil
}

// acts returns the work item acts that reached the provider.
func (p *faultProvider) acts() []adapter.WorkItemActRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.acted)
}

// Run hands each event the provider pushes to the sink in its own
// goroutine, as concurrent webhook deliveries would, and reports each
// result.
func (i *faultInstance) Run(ctx context.Context, s adapter.Sink) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case in := <-i.p.inbound:
			wg.Go(func() {
				r, err := s.Accept(ctx, in)
				i.p.accepted <- accepted{r, err}
			})
		}
	}
}

// Refetch reads the provider's current copy of a message.
func (i *faultInstance) Refetch(_ context.Context, req adapter.RefetchRequest) (model.Envelope, error) {
	p := i.p
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "refetch")
	env, ok := p.current[req.Target.ID]
	if !ok {
		return model.Envelope{}, model.NewError(model.NotFound, "no such message")
	}
	return env.Clone(), nil
}

type faultReconciler struct{ *faultInstance }

// Reconcile reports the provider's record of the effect's operation.
func (i *faultReconciler) Reconcile(_ context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	e := req.Effect
	p := i.p
	p.mu.Lock()
	p.calls = append(p.calls, "reconcile")
	hook := p.reconcile
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lag > 0 && p.now().Before(req.AttemptedAt.Add(p.lag)) {
		return adapter.Outcome{State: model.Failed, Failure: model.NewError(model.NotFound, "no record of the effect yet")}, nil
	}
	if req.Capability == model.CapabilityWorkItemAct {
		for _, r := range slices.Backward(p.actApplied) {
			if r.Operation == e.Operation {
				return adapter.Outcome{State: model.Delivered}, nil
			}
		}
		return adapter.Outcome{State: model.Failed, Failure: model.NewError(model.NotFound, "no record of the act")}, nil
	}
	for _, r := range slices.Backward(p.applied) {
		if r.Operation == e.Operation {
			return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{p.ref(r)}}, nil
		}
	}
	return adapter.Outcome{State: model.Failed, Failure: model.NewError(model.NotFound, "no record of the effect")}, nil
}

var (
	_ adapter.Sender     = (*faultInstance)(nil)
	_ adapter.Receiver   = (*faultInstance)(nil)
	_ adapter.Reconciler = (*faultReconciler)(nil)
	_ adapter.Refetcher  = (*faultInstance)(nil)
	_ adapter.Editor     = (*faultInstance)(nil)

	_ adapter.WorkItemActor = (*faultInstance)(nil)
)

func applyAccepted(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
	return adapter.Outcome{State: model.ProviderAccepted}, nil, true
}

// applyLost applies the effect and loses its answer.
func applyLost(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
	return adapter.Outcome{State: model.Unknown, Failure: model.NewError(model.OutcomeUnknown, "answer lost")}, nil, true
}

// held blocks the send at the provider on g, then applies it.
func held(g *gate) faultSend {
	return func(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error, bool) {
		g.reach()
		return applyAccepted(ctx, r)
	}
}
