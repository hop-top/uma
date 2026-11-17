package streams_test

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
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/internal/streams"
	"hop.top/uma/model"
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
	// withdrawn reports a declared capability unavailable, with its reason.
	withdrawn map[model.Capability]model.ErrorCode
	// pause, when set, runs before each stream call reaches the provider.
	pause func(call)
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

// withdraw reports c unavailable for reason until restore.
func (f *fake) withdraw(c model.Capability, reason model.ErrorCode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.withdrawn == nil {
		f.withdrawn = map[model.Capability]model.ErrorCode{}
	}
	f.withdrawn[c] = reason
}

// restore reports c available again after withdraw.
func (f *fake) restore(c model.Capability) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.withdrawn, c)
}

func (f *fake) respond(as ...answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = append(f.script, as...)
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
	i.f.mu.Lock()
	defer i.f.mu.Unlock()
	var out []model.CapabilityStatus
	for _, c := range i.f.caps {
		st := model.CapabilityStatus{Name: c, Available: true}
		if reason, ok := i.f.withdrawn[c]; ok {
			st.Available, st.Reason = false, reason
		}
		out = append(out, st)
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
	return i.f.record(call{method: "ephemeral", op: r.Operation, text: text(r.Content), recipient: r.Recipient, audience: r.Audience})
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
	c := call{method: method, op: r.Operation, sequence: r.Sequence, text: text(r.Content), stream: ref}
	i.f.mu.Lock()
	pause := i.f.pause
	i.f.mu.Unlock()
	if pause != nil {
		pause(c)
	}
	out, err := i.f.record(c)
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
	_ adapter.Reconciler      = (*inst)(nil)
	_ adapter.Sender          = (*inst)(nil)
	_ adapter.Editor          = (*inst)(nil)
	_ adapter.EphemeralSender = (*inst)(nil)
	_ adapter.Signaler        = (*inst)(nil)
	_ adapter.Streamer        = (*inst)(nil)
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
	s      streams.Service
	conv   model.Conversation
	member model.Identity
	other  model.Identity
	opSeq  int
}

var everything = []model.Capability{
	model.CapabilitySend, model.CapabilityEdit, model.CapabilityStream,
	model.CapabilitySendEphemeral, model.CapabilitySignal,
}

func newRig(t *testing.T, caps ...model.Capability) *rig {
	t.Helper()
	clock := portstest.NewFixedClock(time.Unix(enginetest.TestEpochS, 0).UTC())
	f := &fake{caps: caps}
	e := enginetest.MustNew(t, engine.Config{
		Store:      memory.New(memory.Options{Clock: clock}),
		Authorizer: &enginetest.Allow{},
		Adapters:   []adapter.Factory{f},
		Clock:      clock,
		IDs:        portstest.NewSequenceIDs(11),
	})
	rt := runtime.New(e, streams.StreamEffector())
	rt.SetJitter(func(time.Duration) time.Duration { return 0 })
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	cl, err := enginetest.For(e, enginetest.PrincipalA())
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, ctx: t.Context(), clock: clock, f: f, e: e, rt: rt, s: streams.New(cl.Client)}
	ch := model.Channel{
		Ref: model.ChannelRef{Tenant: enginetest.TenantA, ID: enginetest.ChanOne}, Type: fakeKey,
		Configuration: model.Extension{Name: string(fakeKey) + ".config", Version: 1}, Revision: "ch1",
	}
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

func (r *rig) cmd() model.Command {
	r.opSeq++
	return model.Command{Call: model.Call{Correlation: "corr"}, Operation: model.OperationID(fmt.Sprintf("op-%d", r.opSeq))}
}

func content(s string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}}
}

func public() model.Audience { return model.Audience{Visibility: model.Public} }

func (r *rig) start(mode string) (model.Acceptance, model.StreamRef) {
	r.t.Helper()
	return r.startWith(model.StartStream{Mode: mode})
}

// startWith starts req in the rig's conversation under a fresh command.
func (r *rig) startWith(req model.StartStream) (model.Acceptance, model.StreamRef) {
	r.t.Helper()
	req.Command, req.Conversation, req.Audience = r.cmd(), r.conv.Ref, public()
	a, err := r.s.Start(r.ctx, req)
	if err != nil {
		r.t.Fatalf("Start(%s): %v", req.Mode, err)
	}
	if a.Operation.ResultStream == nil || a.Operation.Result == nil || *a.Operation.Result != a.Operation.ResultStream.Ref() {
		r.t.Fatalf("start result = %+v, want a stream", a.Operation)
	}
	return a, *a.Operation.ResultStream
}

func (r *rig) update(ref model.StreamRef, seq uint64, s string) model.Acceptance {
	r.t.Helper()
	a, err := r.s.Update(r.ctx, model.StreamUpdate{Command: r.cmd(), Stream: ref, Sequence: seq, Content: content(s)})
	if err != nil {
		r.t.Fatalf("Update(%d): %v", seq, err)
	}
	return a
}

func (r *rig) end(ref model.StreamRef, final uint64, cancel bool) model.Acceptance {
	r.t.Helper()
	a, err := r.s.End(r.ctx, model.EndStream{Command: r.cmd(), Stream: ref, FinalSequence: final, Cancel: cancel})
	if err != nil {
		r.t.Fatalf("End: %v", err)
	}
	return a
}

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

func (r *rig) stream(ref model.StreamRef) model.Stream {
	r.t.Helper()
	st, err := r.s.Get(r.ctx, model.Lookup[model.StreamRef]{Call: model.Call{Correlation: "corr"}, Ref: ref})
	if err != nil {
		r.t.Fatalf("Get: %v", err)
	}
	return st
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
