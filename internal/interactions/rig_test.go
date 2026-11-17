package interactions_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/interactions"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

const (
	fakeKey model.TypeKey = "com.example.chat.interactive"
	fakeNS  string        = "com.example.chat"
)

// answer is one scripted provider answer.
type answer struct {
	out adapter.Outcome
	err error
}

// call is one provider call as the fake recorded it.
type call struct {
	method    string
	op        model.OperationRef
	sequence  uint64
	text      string
	target    *model.ExternalRef
	recipient model.ExternalRef
	stream    string
	kind      string
	audience  model.Audience
	// Respond calls, and the interaction an ephemeral or response named.
	mode     string
	followUp bool
	ic       *adapter.InteractionContext
}

// fake is a scripted adapter implementing every interaction interface;
// caps decides what it declares.
type fake struct {
	caps []model.Capability

	mu     sync.Mutex
	script []answer
	calls  []call
	seq    int
	block  chan struct{}
	// reconcile answers Reconcile, which the engine calls only when caps
	// declares model.CapabilityReconcile. Unset, the provider holds no
	// record of the effect.
	reconcile func(context.Context, adapter.Effect) (adapter.Outcome, error)
}

func (f *fake) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: fakeKey, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: string(fakeKey) + ".config", Version: 1},
		Capabilities:        slices.Clone(f.caps), Events: []string{model.EventDeliveryUpdated},
	}
}
func (f *fake) Validate(model.Extension) []model.ValidationIssue { return nil }
func (f *fake) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	return &inst{f}, nil
}

func (f *fake) record(c call) (adapter.Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	if len(f.script) > 0 {
		a := f.script[0]
		f.script = f.script[1:]
		return a.out, a.err
	}
	f.seq++
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{{
		Channel:   model.ChannelRef{Tenant: enginetest.TenantA, ID: enginetest.ChanOne},
		Namespace: fakeNS, Kind: "message", ID: fmt.Sprintf("m%d", f.seq),
	}}}, nil
}

func (f *fake) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.method)
	}
	return out
}

func (f *fake) recorded() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

type inst struct{ f *fake }

func (i *inst) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	var out []model.CapabilityStatus
	for _, c := range i.f.caps {
		out = append(out, model.CapabilityStatus{Name: c, Available: true})
	}
	return out, nil
}
func (i *inst) Close(context.Context) error { return nil }

func text(c model.Content) string {
	if len(c.Parts) == 0 {
		return ""
	}
	return c.Parts[0].Text
}

func (i *inst) Send(_ context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	return i.f.record(call{method: "send", op: r.Operation, text: text(r.Content)})
}

// Edit answers with the edited message's own reference, as a provider
// does.
func (i *inst) Edit(_ context.Context, r adapter.EditRequest) (adapter.Outcome, error) {
	out, err := i.f.record(call{method: "edit", op: r.Operation, text: text(r.Content), target: r.Target})
	if err == nil && out.State == model.ProviderAccepted && r.Target != nil {
		out.References = []model.ExternalRef{*r.Target}
	}
	return out, err
}

func (i *inst) SendEphemeral(_ context.Context, r adapter.EphemeralRequest) (adapter.Outcome, error) {
	return i.f.record(call{method: "ephemeral", op: r.Operation, text: text(r.Content), recipient: r.Recipient, audience: r.Audience, ic: r.Interaction})
}

func (i *inst) Respond(_ context.Context, r adapter.RespondRequest) (adapter.Outcome, error) {
	return i.f.record(call{
		method: "respond", op: r.Operation, text: text(r.Content), target: r.Target, audience: r.Audience,
		mode: r.Mode, followUp: r.FollowUp, ic: r.Interaction,
	})
}

func (i *inst) Signal(ctx context.Context, r adapter.SignalRequest) error {
	if b := i.f.block; b != nil {
		select {
		case <-b:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	_, err := i.f.record(call{method: "signal", kind: r.Kind, audience: r.Audience})
	return err
}

func (i *inst) stream(method string, r adapter.StreamRequest, ref string) (adapter.StreamOutcome, error) {
	out, err := i.f.record(call{method: method, op: r.Operation, sequence: r.Sequence, text: text(r.Content), stream: ref})
	return adapter.StreamOutcome{Outcome: out, ProviderStreamRef: "ps-1"}, err
}

func (i *inst) Start(_ context.Context, r adapter.StreamRequest) (adapter.StreamOutcome, error) {
	return i.stream("start", r, "")
}

func (i *inst) Update(_ context.Context, ref string, r adapter.StreamRequest) (adapter.StreamOutcome, error) {
	return i.stream("update", r, ref)
}

func (i *inst) Finish(_ context.Context, ref string, r adapter.StreamRequest) (adapter.StreamOutcome, error) {
	return i.stream("finish", r, ref)
}

func (i *inst) Cancel(_ context.Context, ref string, e adapter.Effect) (adapter.StreamOutcome, error) {
	return i.stream("cancel", adapter.StreamRequest{Effect: e}, ref)
}

// Reconcile reports the provider's record of the effect.
func (i *inst) Reconcile(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	e := req.Effect
	i.f.mu.Lock()
	i.f.calls = append(i.f.calls, call{method: "reconcile", op: e.Operation})
	fn := i.f.reconcile
	i.f.mu.Unlock()
	if fn != nil {
		return fn(ctx, e)
	}
	return adapter.Outcome{State: model.Failed, Failure: model.NewError(model.NotFound, "no record")}, nil
}

var (
	_ adapter.Reconciler           = (*inst)(nil)
	_ adapter.Sender               = (*inst)(nil)
	_ adapter.Editor               = (*inst)(nil)
	_ adapter.EphemeralSender      = (*inst)(nil)
	_ adapter.InteractionResponder = (*inst)(nil)
	_ adapter.Signaler             = (*inst)(nil)
	_ adapter.Streamer             = (*inst)(nil)
)

// rig is one engine over a memory store with the fake registered, one
// channel, inbox and conversation seeded, and a participant identity.
type rig struct {
	t      *testing.T
	ctx    context.Context
	clock  *portstest.FixedClock
	f      *fake
	e      *engine.Engine
	rt     *runtime.Runtime
	s      interactions.Service
	conv   model.Conversation
	ch     model.Channel
	member model.Identity
	other  model.Identity
	opSeq  int
	ixSeq  int
	// policy is the engine's authorizer; spy counts token reads and
	// response claims.
	policy *testPolicy
	spy    *enginetest.InteractionSpy
}

// testPolicy allows everything except what deny refuses, and records every
// authorization it was asked for.
type testPolicy struct {
	mu   sync.Mutex
	deny func(policy.Authorization) bool
	seen []policy.Authorization
}

func (p *testPolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, a)
	if p.deny != nil && p.deny(a) {
		return policy.Decision{}, nil
	}
	return policy.Decision{Allow: true, PolicyVersion: "test"}, nil
}

// asked returns the authorizations of action so far.
func (p *testPolicy) asked(action string) []policy.Authorization {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []policy.Authorization
	for _, a := range p.seen {
		if a.Action == action {
			out = append(out, a)
		}
	}
	return out
}

var everything = []model.Capability{
	model.CapabilitySend, model.CapabilityEdit, model.CapabilityStream,
	model.CapabilitySendEphemeral, model.CapabilitySignal, model.CapabilityRespond, model.CapabilityRespondFollowUp,
}

func newRig(t *testing.T, caps ...model.Capability) *rig {
	t.Helper()
	clock := portstest.NewFixedClock(time.Unix(enginetest.TestEpochS, 0).UTC())
	f := &fake{caps: caps}
	pol := &testPolicy{}
	spy := &enginetest.InteractionSpy{Store: memory.New(memory.Options{Clock: clock})}
	e := enginetest.MustNew(t, engine.Config{
		Store:      spy,
		Authorizer: pol,
		Adapters:   []adapter.Factory{f},
		Clock:      clock,
		IDs:        portstest.NewSequenceIDs(11),
	})
	rt := runtime.New(e, interactions.EphemeralEffector(), interactions.ResponseEffector())
	rt.SetJitter(func(time.Duration) time.Duration { return 0 })
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	cl, err := enginetest.For(e, enginetest.PrincipalA())
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, ctx: t.Context(), clock: clock, f: f, e: e, rt: rt, s: interactions.New(cl.Client), policy: pol, spy: spy}
	ch := model.Channel{
		Ref: model.ChannelRef{Tenant: enginetest.TenantA, ID: enginetest.ChanOne}, Type: fakeKey,
		Configuration: model.Extension{Name: string(fakeKey) + ".config", Version: 1}, Revision: "ch1",
	}
	r.ch = ch
	inbox := model.Inbox{Ref: model.InboxRef{Tenant: enginetest.TenantA, ID: enginetest.InboxOne}, Channel: ch.Ref, Name: "support", Revision: "in1"}
	ident := func(id model.ID, source string) model.Identity {
		return model.Identity{
			Ref: model.IdentityRef{Inbox: inbox.Ref, ID: id}, Contact: model.ContactRef{Tenant: enginetest.TenantA, ID: id},
			SourceID: source, Namespace: fakeNS, Kind: model.ContactKindPerson, Revision: "id1",
		}
	}
	r.member = ident("00000003-0000-7000-8000-000000000001", "user-1")
	r.other = ident("00000003-0000-7000-8000-000000000002", "user-2")
	r.conv = model.Conversation{
		Ref: model.ConversationRef{Inbox: inbox.Ref, ID: enginetest.ConvOne}, Mode: model.ModePerRoom,
		Status: model.ConversationOpen, Participants: []model.Participant{{Identity: r.member.Ref, Role: "participant"}},
		ReplyRoute: model.ExternalRef{Channel: ch.Ref, Namespace: fakeNS, Kind: "room", ID: "room-1"},
		CreatedAt:  clock.Now(), Revision: "cv1",
	}
	r.tx(func(tx store.Tx) error {
		if err := tx.PutChannel(r.ctx, ch, ""); err != nil {
			return err
		}
		if err := tx.PutInbox(r.ctx, inbox, ""); err != nil {
			return err
		}
		for _, id := range []model.Identity{r.member, r.other} {
			key := model.IdentityKey{Inbox: inbox.Ref, Namespace: id.Namespace, SourceID: id.SourceID}
			if _, _, err := tx.ClaimIdentity(r.ctx, key, id); err != nil {
				return err
			}
		}
		return tx.PutConversation(r.ctx, r.conv, "")
	})
	return r
}

func (r *rig) tx(fn func(store.Tx) error) {
	r.t.Helper()
	if err := r.e.Store().Transact(r.ctx, enginetest.TenantA, fn); err != nil {
		r.t.Fatalf("seed: %v", err)
	}
}

// seed stores an action interaction of the rig conversation by the member,
// public, with a response token valid for an hour; mod adjusts the record
// before it is stored.
func (r *rig) seed(mod ...func(*store.InteractionRecord)) model.Interaction {
	r.t.Helper()
	r.ixSeq++
	rec := enginetest.InteractionSeed{
		Conv: r.conv.Ref, ID: model.ID(fmt.Sprintf("00000005-0000-7000-8000-%012x", r.ixSeq)), Actor: r.member,
		Channel: r.ch, Audience: public(), Now: r.clock.Now(), RespondUntil: r.clock.Now().Add(time.Hour),
	}.Record()
	for _, m := range mod {
		m(&rec)
	}
	r.tx(func(tx store.Tx) error { return tx.PutInteraction(r.ctx, rec) })
	return rec.Interaction
}

// stored returns the interaction as the store holds it now.
func (r *rig) stored(ref model.InteractionRef) model.Interaction {
	r.t.Helper()
	var in model.Interaction
	err := r.e.Store().View(r.ctx, enginetest.TenantA, func(rd store.Reader) (err error) {
		in, err = rd.Interaction(r.ctx, ref)
		return err
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return in
}

func (r *rig) cmd() model.Command {
	r.opSeq++
	return model.Command{Call: model.Call{Correlation: "corr"}, Operation: model.OperationID(fmt.Sprintf("op-%d", r.opSeq))}
}

func content(s string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}}
}

func public() model.Audience { return model.Audience{Visibility: model.Public} }

// claim leases every claimable item.
func (r *rig) claim() []store.Work {
	r.t.Helper()
	works, err := r.rt.ClaimWork(r.ctx, enginetest.TenantA, runtime.WorkerOptions{
		Owner: "w", Limit: 20, Kinds: []string{operations.WorkKindOperation}, Lease: time.Minute,
	})
	if err != nil {
		r.t.Fatalf("claim: %v", err)
	}
	return works
}

func (r *rig) exec(works []store.Work) {
	r.t.Helper()
	for _, w := range works {
		if err := r.rt.ExecuteClaimed(r.ctx, enginetest.TenantA, w); err != nil {
			r.t.Fatalf("execute: %v", err)
		}
	}
}

// drain runs work, advancing the clock between rounds, until none is left.
func (r *rig) drain() {
	r.t.Helper()
	for range 50 {
		works := r.claim()
		if len(works) == 0 {
			r.clock.Advance(time.Minute)
			if works = r.claim(); len(works) == 0 {
				return
			}
		}
		r.exec(works)
	}
	r.t.Fatal("work did not drain")
}

func (r *rig) op(a model.Acceptance) model.Operation {
	r.t.Helper()
	var rec store.OperationRecord
	err := r.e.Store().View(r.ctx, enginetest.TenantA, func(rd store.Reader) error {
		var err error
		rec, err = rd.Operation(r.ctx, a.Operation.Ref)
		return err
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return rec.Operation
}

// recorded returns messages and events stored so far.
func (r *rig) recorded() ([]model.Message, []model.Event) {
	r.t.Helper()
	var msgs []model.Message
	var evs []model.Event
	err := r.e.Store().View(r.ctx, enginetest.TenantA, func(rd store.Reader) error {
		p, err := rd.Messages(r.ctx, model.ListRequest{Page: model.PageRequest{Limit: 100}})
		if err != nil {
			return err
		}
		msgs = p.Items
		ep, err := rd.Events(r.ctx, model.Filter{}, "", 100)
		evs = ep.Items
		return err
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return msgs, evs
}

func wantState(t *testing.T, op model.Operation, s model.DeliveryState) {
	t.Helper()
	if op.State != s {
		t.Fatalf("operation state = %q (failure %v), want %q", op.State, op.Failure, s)
	}
}
