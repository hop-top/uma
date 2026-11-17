package conformance

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

const faultType model.TypeKey = "com.example.conformance.faults"

const (
	faultNS   = "com.example.conformance.faults"
	faultDest = "faults@example.test"
	// faultPoll is the worker's poll interval; the waiter recognizes the
	// worker's idle waits by it.
	faultPoll = 7 * time.Second
)

// gate holds one party at a point until the case opens it. Stopping any
// process of the env opens every gate first, so a failing case never
// leaves a party blocked.
type gate struct {
	name     string
	reached  chan struct{}
	released chan struct{}
	arrive   sync.Once
	release  sync.Once
}

// gate returns a gate for the party named what, which a failure names.
func (f *faultEnv) gate(what string) *gate {
	g := &gate{name: what, reached: make(chan struct{}), released: make(chan struct{})}
	f.mu.Lock()
	f.gates = append(f.gates, g)
	f.mu.Unlock()
	return g
}

func (f *faultEnv) openGates() {
	f.mu.Lock()
	gs := slices.Clone(f.gates)
	f.mu.Unlock()
	for _, g := range gs {
		g.open()
	}
}

// reach signals arrival and blocks until the gate opens.
func (g *gate) reach() {
	g.arrive.Do(func() { close(g.reached) })
	<-g.released
}

// open releases the party, now and at any later reach.
func (g *gate) open() { g.release.Do(func() { close(g.released) }) }

// gateWait is how long await waits for a party.
var gateWait = 10 * time.Second

// await waits until the party reached the gate.
func (g *gate) await(t *testing.T) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(gateWait):
		t.Fatalf("%s: the %q party never reached its gate within %v; a unit callback is probably blocked on another unit, "+
			"so a store that serializes units never lets both parties in", t.Name(), g.name, gateWait)
	}
}

// faultWaiter hands each idle wait of the worker to the case, which resumes
// it. Every other wait, such as a receiver lease renewal, lasts until its
// run ends: nothing renews behind the case's back.
type faultWaiter struct {
	idle   chan struct{}
	resume chan struct{}
}

func (w *faultWaiter) Wait(ctx context.Context, d time.Duration) error {
	if d != faultPoll {
		<-ctx.Done()
		return ctx.Err()
	}
	select {
	case w.idle <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-w.resume:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type nowFunc func() time.Time

func (f nowFunc) Now() time.Time { return f() }

// faultEnv runs processes, one engine each, over the durable env's store
// with the fault provider registered, a channel, an inbox and a
// conversation seeded. Processes share the env's clock; a restart crosses
// the case's persistence point.
type faultEnv struct {
	*env
	p     *faultProvider
	work  *uma.WorkPolicy
	gen   uint32
	ch    model.ChannelRef
	inbox model.InboxRef
	conv  model.Conversation
	// proc is the current process.
	proc *faultProc

	mu    sync.Mutex
	gates []*gate
}

func newFaultEnv(t *testing.T, e *env, reconciles bool) *faultEnv {
	t.Helper()
	f := &faultEnv{env: e, p: newFaultProvider(reconciles)}
	f.ch = model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
	f.inbox = model.InboxRef{Tenant: tenantA, ID: e.id(t)}
	lock := true
	f.conv = model.Conversation{
		Ref: model.ConversationRef{Inbox: f.inbox, ID: e.id(t)}, Mode: model.ModePerRoom, Status: model.ConversationOpen,
		ReplyRoute: model.ExternalRef{Channel: f.ch, Namespace: faultNS, Kind: "room", ID: "room-1"},
		CreatedAt:  e.fx.Now(), Revision: "cv1",
	}
	e.mustTx(t, func(tx store.Tx) error {
		if err := tx.PutChannel(e.ctx, model.Channel{
			Ref: f.ch, Type: faultType, Configuration: model.Extension{Name: string(faultType) + ".config", Version: 1}, Revision: "ch1",
		}, ""); err != nil {
			return err
		}
		if err := tx.PutInbox(e.ctx, model.Inbox{
			Ref: f.inbox, Channel: f.ch, Name: "faults", Destinations: []string{faultDest},
			Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
			Revision:  "in1",
		}, ""); err != nil {
			return err
		}
		return tx.PutConversation(e.ctx, f.conv, "")
	})
	return f
}

// faultProc is one process: an engine over its own handle on the store.
type faultProc struct {
	f      *faultEnv
	name   string
	store  *crashStore
	engine uma.Engine
	client uma.Client
	waiter *faultWaiter
	ctx    context.Context
	cancel context.CancelFunc
	run    chan error
	once   sync.Once
}

// start begins a new process and makes it current.
func (f *faultEnv) start(t *testing.T) *faultProc {
	t.Helper()
	p := f.spawn(t)
	f.proc = p
	return p
}

// spawn begins another process sharing the store, without making it
// current.
func (f *faultEnv) spawn(t *testing.T) *faultProc {
	t.Helper()
	f.gen++
	ctx, cancel := context.WithCancel(f.ctx)
	p := &faultProc{
		f: f, name: fmt.Sprintf("process-%d", f.gen), ctx: ctx, cancel: cancel,
		store:  &crashStore{inner: f.s, onCrash: cancel},
		waiter: &faultWaiter{idle: make(chan struct{}), resume: make(chan struct{})},
	}
	eng, err := uma.New(uma.Config{
		Store: p.store,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters: []adapter.Factory{f.p},
		Clock:    nowFunc(f.fx.Now),
		IDs:      portstest.NewSequenceIDs(0xfa00 + f.gen),
		Waiter:   p.waiter,
		Work:     f.work,
	})
	if err != nil {
		t.Fatalf("uma.New: %v", err)
	}
	p.engine = eng
	if p.client, err = eng.For(model.Principal{Tenant: tenantA, Actor: "conformance-agent"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.stop)
	return p
}

// stop ends the process: its work is cancelled and its engine closed.
func (p *faultProc) stop() {
	p.once.Do(func() {
		p.f.openGates()
		p.cancel()
		if p.run != nil {
			<-p.run
		}
		_ = p.engine.Close(context.Background())
	})
}

// restart crashes the current process, if it is still alive, crosses the
// case's persistence point and starts the next process.
func (f *faultEnv) restart(t *testing.T, cross func(*testing.T)) *faultProc {
	t.Helper()
	if old := f.proc; old != nil {
		_ = old.store.crash()
		old.stop()
	}
	cross(t)
	return f.start(t)
}

func faultCall() uma.RuntimeCall {
	return uma.RuntimeCall{Principal: model.Principal{Tenant: tenantA, Actor: "conformance-service"}, Call: model.Call{Correlation: "conformance-faults"}}
}

// work starts the process's worker in the background.
func (p *faultProc) work() {
	p.run = make(chan error, 1)
	go func() {
		p.run <- p.engine.Runtime().Run(p.ctx, faultCall(), uma.WorkerOptions{
			Owner: p.name, Limit: 10, Lease: time.Minute, Poll: faultPoll,
		})
	}()
}

// idle waits until the worker found no claimable work.
func (p *faultProc) idle(t *testing.T) {
	t.Helper()
	select {
	case <-p.waiter.idle:
	case err := <-p.run:
		p.run = nil
		t.Fatalf("%s: Runtime.Run returned %v, want it idle", p.name, err)
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: Runtime.Run did not go idle", p.name)
	}
}

// crashed waits until the worker ended with its crashed process.
func (p *faultProc) crashed(t *testing.T) {
	t.Helper()
	select {
	case err := <-p.run:
		p.run = nil
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%s: Runtime.Run = %v, want the crash to cancel it", p.name, err)
		}
	case <-p.waiter.idle:
		t.Fatalf("%s: worker went idle without crashing", p.name)
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: worker did not crash", p.name)
	}
}

// later advances the clock by d and lets the idle worker claim again.
func (p *faultProc) later(t *testing.T, d time.Duration) {
	t.Helper()
	p.f.fx.Advance(d)
	p.resume(t)
	p.idle(t)
}

// resume lets the idle worker claim again.
func (p *faultProc) resume(t *testing.T) {
	t.Helper()
	select {
	case p.waiter.resume <- struct{}{}:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: worker not waiting", p.name)
	}
}

// reply queues a public reply on the seeded conversation.
func (p *faultProc) reply(t *testing.T, opID model.OperationID) model.OperationRef {
	t.Helper()
	a, err := p.client.Messages().Reply(p.ctx, model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: "c-" + model.CorrelationID(opID)}, Operation: opID},
		Conversation: p.f.conv.Ref,
		Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "reply " + string(opID)}}},
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	return a.Operation.Ref
}

func (p *faultProc) op(t *testing.T, ref model.OperationRef) model.Operation {
	t.Helper()
	op, err := p.client.Operations().Get(p.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "get"}, Ref: ref})
	if err != nil {
		t.Fatalf("Operations.Get: %v", err)
	}
	return op
}

// evidence reads op's attempt evidence through the current store.
func (f *faultEnv) evidence(t *testing.T, op model.OperationRef) []store.AttemptEvidence {
	t.Helper()
	var out []store.AttemptEvidence
	f.mustView(t, func(r store.Reader) error {
		p, err := r.Attempts(f.ctx, op, page(100))
		out = p.Items
		return err
	})
	return out
}

// deliveries counts delivery.updated events of op in each state.
func (f *faultEnv) deliveries(t *testing.T, op model.OperationRef) []model.Event {
	t.Helper()
	var out []model.Event
	for _, ev := range f.allEvents(t, model.Filter{}, 50) {
		if ev.Kind == model.EventDeliveryUpdated && ev.Resource == op.Ref() {
			out = append(out, ev)
		}
	}
	return out
}

func wantOpState(t *testing.T, op model.Operation, state model.DeliveryState, code model.ErrorCode) {
	t.Helper()
	switch {
	case op.State != state:
		t.Fatalf("operation state = %s (failure %v), want %s", op.State, op.Failure, state)
	case code == "" && op.Failure != nil:
		t.Fatalf("operation failure = %v, want none", op.Failure)
	case code != "" && (op.Failure == nil || op.Failure.Code != code):
		t.Fatalf("operation failure = %v, want code %s", op.Failure, code)
	}
}
