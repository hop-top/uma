package postgres

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// A control pressed on a sent message while the send settles: the
// interaction's ingress unit and the send's settling unit overlap, each
// reading what the other writes. Ingress looks the provider message up
// among the inbox's message references, which the settling unit writes;
// the settling unit looks the provider message up among the unlinked
// interactions, which ingress writes. Whichever unit the store lets commit
// first, the other must not commit on what it read: it fails, reruns and
// sees the first, so the interaction ends linked to the sent message in
// both orders. The tests hold each unit at the point that makes the
// overlap certain and drive the engine through a channel that holds its
// sends at the provider.

const (
	linkTenant model.TenantID = "postgres-interaction-link"
	linkType   model.TypeKey  = "com.example.link.chat"
	linkNS                    = "com.example.link.user"
	linkDest                  = "link-support"
)

// linkChannel sends under provider-assigned message IDs, holding each send
// at the provider until released, and receives through the sink its
// receiver hands the test.
type linkChannel struct {
	sinks   chan adapter.Sink
	sending chan model.ExternalRef
	release chan struct{}
}

func (*linkChannel) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: linkType, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: string(linkType) + ".config", Version: 1},
		Capabilities:        []model.Capability{model.CapabilityReceive, model.CapabilitySend},
		Events:              []string{model.EventMessageReceived, model.EventDeliveryUpdated, model.EventInteractionReceived},
		IdentityNamespaces:  []string{linkNS},
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsProvider},
	}
}

func (*linkChannel) Validate(model.Extension) []model.ValidationIssue { return nil }

func (c *linkChannel) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	return c, nil
}

func (*linkChannel) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return []model.CapabilityStatus{{Name: model.CapabilityReceive, Available: true}, {Name: model.CapabilitySend, Available: true}}, nil
}

func (*linkChannel) Close(context.Context) error { return nil }

func (c *linkChannel) Run(ctx context.Context, s adapter.Sink) error {
	c.sinks <- s
	<-ctx.Done()
	return ctx.Err()
}

func (c *linkChannel) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	ref := model.ProviderMessageRef(r.Route.Channel, "pm-"+string(r.Operation.ID))
	c.sending <- ref
	select {
	case <-c.release:
	case <-ctx.Done():
		return adapter.Outcome{}, ctx.Err()
	}
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{ref}}, nil
}

// linkStore holds units at the two points the tests interleave on: an
// ingress unit before it writes its interaction, and a settling unit
// before its last write, completing the send's work item, when it has
// recorded the provider message ID and looked for unlinked interactions.
// Each hold fires once. It reports each committed settling unit and counts
// the units of each kind the store failed as transient.
type linkStore struct {
	store.Store
	mu           sync.Mutex
	beforePut    func()
	beforeFinish func()
	settled      chan struct{}
	lostIngest   atomic.Int64
	lostSettle   atomic.Int64
}

func (s *linkStore) take(f *func()) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	hold := *f
	*f = nil
	return hold
}

func (s *linkStore) Transact(ctx context.Context, tenant model.TenantID, fn func(store.Tx) error) error {
	var u linkUnit
	err := s.Store.Transact(ctx, tenant, func(tx store.Tx) error {
		u = linkUnit{}
		return fn(&linkTx{Tx: tx, s: s, u: &u})
	})
	switch {
	case model.IsCode(err, model.Transient) && u.ingest:
		s.lostIngest.Add(1)
	case model.IsCode(err, model.Transient) && u.settle:
		s.lostSettle.Add(1)
	case err == nil && u.settle:
		select {
		case s.settled <- struct{}{}:
		default:
		}
	}
	return err
}

// linkUnit records what one unit did: ingest wrote an interaction, settle
// completed a work item, as the unit settling a send does.
type linkUnit struct{ ingest, settle bool }

type linkTx struct {
	store.Tx
	s *linkStore
	u *linkUnit
}

func (t *linkTx) PutInteraction(ctx context.Context, rec store.InteractionRecord) error {
	t.u.ingest = true
	if hold := t.s.take(&t.s.beforePut); hold != nil {
		hold()
	}
	return t.Tx.PutInteraction(ctx, rec)
}

func (t *linkTx) CompleteWork(ctx context.Context, w store.Work) error {
	t.u.settle = true
	if hold := t.s.take(&t.s.beforeFinish); hold != nil {
		hold()
	}
	return t.Tx.CompleteWork(ctx, w)
}

// linkRig is an engine over a linkStore with the link channel, an inbox on
// it and a conversation with a reply route, its receiver running.
type linkRig struct {
	st     *linkStore
	ch     *linkChannel
	client uma.Client
	chRef  model.ChannelRef
	inbox  model.InboxRef
	conv   model.ConversationRef
	sink   adapter.Sink
}

func newLinkRig(t *testing.T) *linkRig {
	t.Helper()
	ctx := t.Context()
	inner, err := Open(ctx, newDatabase(t, true), Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := &linkRig{
		st: &linkStore{Store: inner, settled: make(chan struct{}, 1)},
		ch: &linkChannel{sinks: make(chan adapter.Sink, 1), sending: make(chan model.ExternalRef, 1), release: make(chan struct{})},
	}
	r.chRef = model.ChannelRef{Tenant: linkTenant, ID: benchID(0xb1, 1)}
	r.inbox = model.InboxRef{Tenant: linkTenant, ID: benchID(0xb1, 2)}
	r.conv = model.ConversationRef{Inbox: r.inbox, ID: benchID(0xb1, 3)}
	lock := true
	if err := inner.Transact(ctx, linkTenant, func(tx store.Tx) error {
		return firstErr(
			tx.PutChannel(ctx, model.Channel{
				Ref: r.chRef, Type: linkType, Configuration: model.Extension{Name: string(linkType) + ".config", Version: 1}, Revision: "ch1",
			}, ""),
			tx.PutInbox(ctx, model.Inbox{
				Ref: r.inbox, Channel: r.chRef, Name: "link", Destinations: []string{linkDest},
				Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
				Revision:  "in1",
			}, ""),
			tx.PutConversation(ctx, model.Conversation{
				Ref: r.conv, Mode: model.ModePerRoom, Status: model.ConversationOpen,
				ReplyRoute: model.ExternalRef{Channel: r.chRef, Namespace: linkNS, Kind: "room", ID: "room-1"},
				CreatedAt:  time.Now(), Revision: "cv1",
			}, ""),
		)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	e, err := uma.New(uma.Config{
		Store: r.st, Authorizer: benchAllow(), Adapters: []adapter.Factory{r.ch},
		Clock: benchClock{}, IDs: portstest.NewSequenceIDs(0xb2),
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: linkTenant, Actor: "link-runtime"}, Call: model.Call{Correlation: "link"}}
	var wg sync.WaitGroup
	wg.Go(func() { _ = e.Runtime().Receive(runCtx, call, r.chRef) })
	wg.Go(func() {
		_ = e.Runtime().Run(runCtx, call, uma.WorkerOptions{Owner: "link-worker", Lease: time.Minute, Poll: 10 * time.Millisecond})
	})
	t.Cleanup(func() {
		stop()
		wg.Wait()
		_ = e.Close(context.Background())
	})
	if r.client, err = e.For(model.Principal{Tenant: linkTenant, Actor: "link-agent"}); err != nil {
		t.Fatal(err)
	}
	select {
	case r.sink = <-r.ch.sinks:
	case <-time.After(30 * time.Second):
		t.Fatal("receiver never ran")
	}
	return r
}

// reply sends a reply on the conversation and returns its operation once
// the send reached the provider, with the provider message it posts.
func (r *linkRig) reply(t *testing.T) (model.OperationRef, model.ExternalRef) {
	t.Helper()
	a, err := r.client.Messages().Reply(t.Context(), model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: "c-reply"}, Operation: "op-reply"},
		Conversation: r.conv,
		Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Approve the change?"}}},
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	select {
	case ref := <-r.ch.sending:
		return a.Operation.Ref, ref
	case <-time.After(30 * time.Second):
		t.Fatal("the send never reached the provider")
		return model.OperationRef{}, model.ExternalRef{}
	}
}

// press accepts an action on provider message src.
func (r *linkRig) press(ctx context.Context, src model.ExternalRef) error {
	actor := model.ExternalRef{Channel: r.chRef, Namespace: linkNS, Kind: "user", ID: "u-1"}
	_, err := r.sink.Accept(ctx, model.Ingress{Call: model.Call{Correlation: "c-press"}, Envelope: model.Envelope{
		EventID: "ix-1", Destination: linkDest, Kind: model.EnvelopeInteraction, Sender: actor, Assurance: "verified",
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: time.Now(),
		Interaction: &model.InteractionInput{
			ID: "ix-1", Kind: model.InteractionAction, Action: "approve", Actor: actor, Message: &src,
			Deadline: time.Now().Add(time.Minute),
		},
	}})
	return err
}

func (r *linkRig) awaitSettled(t *testing.T) {
	t.Helper()
	select {
	case <-r.st.settled:
	case <-time.After(30 * time.Second):
		t.Fatal("the send never settled")
	}
}

// wantLinked requires the one interaction to name the reply's message.
func (r *linkRig) wantLinked(t *testing.T, op model.OperationRef) {
	t.Helper()
	got, err := r.client.Operations().Get(t.Context(), model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "get"}, Ref: op})
	if err != nil {
		t.Fatal(err)
	}
	if got.ResultMessage == nil || got.State != model.ProviderAccepted {
		t.Fatalf("reply operation = %+v, want it settled with its message", got)
	}
	var ins []model.Interaction
	if err := r.st.View(t.Context(), linkTenant, func(rd store.Reader) error {
		p, err := rd.Interactions(t.Context(), model.ListRequest{Page: model.PageRequest{Limit: 10}})
		ins = p.Items
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(ins) != 1 || ins[0].Message == nil || *ins[0].Message != *got.ResultMessage {
		t.Fatalf("interactions = %+v, want the one linked to the reply's message %v", ins, *got.ResultMessage)
	}
}

// signal returns a hold that reports reaching it and waits until proceed,
// which the test's end also releases.
func signal(t *testing.T) (reached <-chan struct{}, proceed func(), hold func()) {
	r, p := make(chan struct{}), make(chan struct{})
	var once sync.Once
	proceed = func() { once.Do(func() { close(p) }) }
	t.Cleanup(proceed)
	return r, proceed, func() {
		close(r)
		<-p
	}
}

func await(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s never happened", what)
	}
}

// TestInteractionIngressReadsBeforeSettleCommits: ingress finds no
// message holding the provider message ID, then the settling unit records
// it, finds no unlinked interaction and commits; ingress then writes its
// interaction. Ingress must not commit on its stale lookup: it reruns,
// finds the sent message and links the interaction itself.
func TestInteractionIngressReadsBeforeSettleCommits(t *testing.T) {
	r := newLinkRig(t)
	reached, proceed, hold := signal(t)
	r.st.beforePut = hold
	op, src := r.reply(t)
	pressed := make(chan error, 1)
	go func() { pressed <- r.press(t.Context(), src) }()
	await(t, reached, "the interaction's write")
	close(r.ch.release)
	r.awaitSettled(t)
	proceed()
	if err := <-pressed; err != nil {
		t.Fatalf("press: %v", err)
	}
	r.wantLinked(t, op)
	if n := r.st.lostIngest.Load(); n == 0 {
		t.Errorf("ingress units lost = %d, want the overlapping one rerun", n)
	}
}

// TestSettleReadsBeforeInteractionIngressCommits: the settling unit
// records the provider message ID and finds no unlinked interaction; the
// interaction's ingress, which cannot see that ID yet, then commits the
// interaction unlinked. The settling unit must not commit on its stale
// lookup: it reruns, finds the interaction and links it.
func TestSettleReadsBeforeInteractionIngressCommits(t *testing.T) {
	r := newLinkRig(t)
	reached, proceed, hold := signal(t)
	r.st.beforeFinish = hold
	op, src := r.reply(t)
	close(r.ch.release)
	await(t, reached, "the settling unit's last write")
	if err := r.press(t.Context(), src); err != nil {
		t.Fatalf("press: %v", err)
	}
	proceed()
	r.awaitSettled(t)
	r.wantLinked(t, op)
	t.Logf("units rerun: ingress %d, settling %d", r.st.lostIngest.Load(), r.st.lostSettle.Load())
	if n := r.st.lostSettle.Load() + r.st.lostIngest.Load(); n == 0 {
		t.Errorf("units lost = %d, want one of the overlapping units rerun", n)
	}
}
