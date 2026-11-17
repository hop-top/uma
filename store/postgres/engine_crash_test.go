package postgres

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// TestServerCrashAfterProviderCall kills the database server with SIGKILL
// while the unit recording a provider's answer is in flight: the provider
// applied the reply, the engine never recorded that it did. After the
// server restarts, a new engine finds the attempt unresolved, reconciles
// it and settles the reply from the provider's record. The reply reaches
// the provider once.
func TestServerCrashAfterProviderCall(t *testing.T) {
	requireDocker(t)
	ctx := t.Context()
	ctr, err := tcpostgres.Run(ctx, testImage,
		tcpostgres.WithDatabase("uma"),
		tcpostgres.WithUsername("uma"),
		tcpostgres.WithPassword("uma"),
		tcpostgres.BasicWaitStrategies(),
	)
	t.Cleanup(func() {
		if ctr != nil {
			_ = ctr.Terminate(context.Background())
		}
	})
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	pool := dial(t, ctr)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0).UTC())
	st, err := Open(ctx, pool, Options{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	conv := seedCrashChannel(t, st, clock.Now())
	prov := &crashProvider{}

	held := &holdAnswer{Store: st, written: make(chan struct{}), commit: make(chan struct{}), ended: make(chan error, 1)}
	first, err := crashEngine(held, prov, clock, 0xe1, blockWaiter{})
	if err != nil {
		t.Fatal(err)
	}
	cl, err := first.For(model.Principal{Tenant: crashTenant, Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := cl.Messages().Reply(ctx, model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: "c-1"}, Operation: "op-1"},
		Conversation: conv,
		Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	ran := make(chan error, 1)
	go func() {
		ran <- first.Runtime().Run(runCtx, crashCall(), uma.WorkerOptions{Owner: "worker-a", Lease: time.Minute})
	}()
	select {
	case <-held.written:
	case <-time.After(30 * time.Second):
		t.Fatal("the provider's answer was never being recorded")
	}

	docker, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = docker.Close() }()
	if _, err := docker.ContainerKill(ctx, ctr.GetContainerID(), client.ContainerKillOptions{Signal: "KILL"}); err != nil {
		t.Fatalf("kill server: %v", err)
	}
	close(held.commit)
	if err := <-held.ended; err == nil {
		t.Fatal("the answer unit committed after the server was killed")
	}
	stop()
	<-ran
	_ = first.Close(context.Background())
	go pool.Close()

	if err := ctr.Start(ctx); err != nil {
		t.Fatalf("restart server: %v", err)
	}
	st, err = Open(ctx, dial(t, ctr), Options{Clock: clock})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	idle := &idleSignal{idle: make(chan struct{}), resume: make(chan struct{})}
	second, err := crashEngine(st, prov, clock, 0xe2, idle)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close(context.Background()) }()
	runCtx, stop = context.WithCancel(ctx)
	defer stop()
	go func() {
		ran <- second.Runtime().Run(runCtx, crashCall(), uma.WorkerOptions{Owner: "worker-b", Lease: time.Minute, Poll: time.Second})
	}()
	idle.await(t, ran)
	if got := prov.log(); !slices.Equal(got, []string{"send"}) {
		t.Fatalf("provider calls = %v while the dead worker's lease was live", got)
	}
	clock.Advance(2 * time.Minute)
	idle.resume <- struct{}{}
	idle.await(t, ran)

	cl, err = second.For(model.Principal{Tenant: crashTenant, Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	op, err := cl.Operations().Get(ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "get"}, Ref: a.Operation.Ref})
	if err != nil {
		t.Fatal(err)
	}
	if op.State != model.ProviderAccepted || op.Failure != nil {
		t.Fatalf("reply after the crash = %s (%v), want provider_accepted", op.State, op.Failure)
	}
	if got := prov.log(); !slices.Equal(got, []string{"send", "reconcile"}) {
		t.Fatalf("provider calls = %v, want the one send, then reconcile", got)
	}
}

const crashType model.TypeKey = "com.example.postgres.crash"

const crashTenant model.TenantID = "engine-crash-tenant"

func crashCall() uma.RuntimeCall {
	return uma.RuntimeCall{Principal: model.Principal{Tenant: crashTenant, Actor: "service"}, Call: model.Call{Correlation: "run"}}
}

func crashEngine(st store.Store, prov *crashProvider, clock *portstest.FixedClock, seed uint32, w interface {
	Wait(context.Context, time.Duration) error
}) (uma.Engine, error) {
	return uma.New(uma.Config{
		Store: st,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters: []adapter.Factory{prov},
		Clock:    clock,
		IDs:      portstest.NewSequenceIDs(seed),
		Waiter:   w,
	})
}

// seedCrashChannel stores a channel of the crash provider, an inbox and a
// conversation routed to it, and returns the conversation.
func seedCrashChannel(t *testing.T, st *Store, now time.Time) model.ConversationRef {
	t.Helper()
	ch := model.ChannelRef{Tenant: crashTenant, ID: "000000e0-0000-7000-8000-000000000001"}
	in := model.InboxRef{Tenant: crashTenant, ID: "000000e0-0000-7000-8000-000000000002"}
	conv := model.ConversationRef{Inbox: in, ID: "000000e0-0000-7000-8000-000000000003"}
	err := st.Transact(t.Context(), crashTenant, func(tx store.Tx) error {
		return firstErr(
			tx.PutChannel(t.Context(), model.Channel{
				Ref: ch, Type: crashType, Configuration: model.Extension{Name: string(crashType) + ".config", Version: 1}, Revision: "ch1",
			}, ""),
			tx.PutInbox(t.Context(), model.Inbox{Ref: in, Channel: ch, Name: "crash", Revision: "in1"}, ""),
			tx.PutConversation(t.Context(), model.Conversation{
				Ref: conv, Mode: model.ModePerRoom, Status: model.ConversationOpen,
				ReplyRoute: model.ExternalRef{Channel: ch, Namespace: string(crashType), Kind: "room", ID: "room-1"},
				CreatedAt:  now, Revision: "cv1",
			}, ""),
		)
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return conv
}

// holdAnswer holds the first unit recording a provider's answer once its
// writes are done, until commit is closed, and reports how it ended.
type holdAnswer struct {
	*Store
	once    sync.Once
	written chan struct{}
	commit  chan struct{}
	ended   chan error
}

func (h *holdAnswer) Transact(ctx context.Context, tn model.TenantID, fn func(store.Tx) error) error {
	var hold bool
	err := h.Store.Transact(ctx, tn, func(tx store.Tx) error {
		w := &answerTx{Tx: tx}
		if err := fn(w); err != nil {
			return err
		}
		if w.answer {
			h.once.Do(func() {
				hold = true
				close(h.written)
				<-h.commit
			})
		}
		return nil
	})
	if hold {
		h.ended <- err
	}
	return err
}

// answerTx notes whether its unit records a provider answer.
type answerTx struct {
	store.Tx
	answer bool
}

func (t *answerTx) AppendAttemptEvidence(ctx context.Context, ev store.AttemptEvidence) error {
	if ev.State != model.Unknown {
		t.answer = true
	}
	return t.Tx.AppendAttemptEvidence(ctx, ev)
}

// crashProvider applies every send and reconciles from what it applied. It
// outlives both engines.
type crashProvider struct {
	mu      sync.Mutex
	calls   []string
	applied []model.OperationRef
}

func (p *crashProvider) log() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

func (p *crashProvider) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: crashType, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: string(crashType) + ".config", Version: 1},
		Capabilities:        []model.Capability{model.CapabilitySend, model.CapabilityReconcile},
		Events:              []string{model.EventDeliveryUpdated},
	}
}

func (p *crashProvider) Validate(model.Extension) []model.ValidationIssue { return nil }

func (p *crashProvider) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	return crashInstance{p}, nil
}

type crashInstance struct{ p *crashProvider }

func (i crashInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return []model.CapabilityStatus{{Name: model.CapabilitySend, Available: true}, {Name: model.CapabilityReconcile, Available: true}}, nil
}

func (crashInstance) Close(context.Context) error { return nil }

func (i crashInstance) Send(_ context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	i.p.mu.Lock()
	defer i.p.mu.Unlock()
	i.p.calls = append(i.p.calls, "send")
	i.p.applied = append(i.p.applied, r.Operation)
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{providerRef(r.Effect)}}, nil
}

func (i crashInstance) Reconcile(_ context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	e := req.Effect
	i.p.mu.Lock()
	defer i.p.mu.Unlock()
	i.p.calls = append(i.p.calls, "reconcile")
	if slices.Contains(i.p.applied, e.Operation) {
		return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{providerRef(e)}}, nil
	}
	return adapter.Outcome{State: model.Failed, Failure: model.NewError(model.NotFound, "no record")}, nil
}

func providerRef(e adapter.Effect) model.ExternalRef {
	return model.ExternalRef{Channel: e.Route.Channel, Namespace: model.ProviderMessageNamespace, Kind: model.RefKindMessage, ID: "pm-" + string(e.Operation.ID)}
}

// blockWaiter never ends a wait before its run does.
type blockWaiter struct{}

func (blockWaiter) Wait(ctx context.Context, _ time.Duration) error {
	<-ctx.Done()
	return ctx.Err()
}

// idleSignal hands each idle wait of the worker to the test.
type idleSignal struct {
	idle   chan struct{}
	resume chan struct{}
}

func (w *idleSignal) Wait(ctx context.Context, _ time.Duration) error {
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

func (w *idleSignal) await(t *testing.T, ran <-chan error) {
	t.Helper()
	select {
	case <-w.idle:
	case err := <-ran:
		t.Fatalf("Runtime.Run returned %v, want it idle", err)
	case <-time.After(30 * time.Second):
		t.Fatal("Runtime.Run did not go idle")
	}
}
