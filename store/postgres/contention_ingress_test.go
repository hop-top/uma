package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
)

// Inbound messages and conversation starts contending in one tenant
// through the engine, as the reply tests in contention_test.go: callers in
// step, each issuing its calls back to back, the server's serialization
// failures logged by statement and reason, latency of each call with
// reruns. Every call must succeed; units may still lose to one another and
// be rerun. The claims of these units insert first, but the units read
// what concurrent ones write: the
// ingress record each completes, the message references the inbox holds
// (an echo, a known provider message), the candidate conversations of a
// selection, a start's operation key, and, on a small table, a tenant's
// or an inbox's range of conversations and messages where the planner
// prefers a shorter index.

const textContentionType model.TypeKey = "com.example.contention.text"

const (
	textContentionNS   = "com.example.contention.number"
	textContentionDest = "+15550000"
)

// textChannel receives messages under provider-assigned IDs, sends and
// starts conversations. Its receiver hands the test its sink.
type textChannel struct{ sinks chan adapter.Sink }

func (*textChannel) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: textContentionType, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: string(textContentionType) + ".config", Version: 1},
		Capabilities: []model.Capability{
			model.CapabilityReceive, model.CapabilitySend, model.CapabilityStartConversation,
		},
		Events:             []string{model.EventMessageReceived, model.EventDeliveryUpdated},
		IdentityNamespaces: []string{textContentionNS},
		Exchange:           adapter.Exchange{MessageIDs: adapter.MessageIDsProvider},
	}
}

func (*textChannel) Validate(model.Extension) []model.ValidationIssue { return nil }

func (c *textChannel) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	return c, nil
}

func (*textChannel) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return []model.CapabilityStatus{
		{Name: model.CapabilityReceive, Available: true},
		{Name: model.CapabilitySend, Available: true},
		{Name: model.CapabilityStartConversation, Available: true},
	}, nil
}

func (*textChannel) Close(context.Context) error { return nil }

func (c *textChannel) Run(ctx context.Context, s adapter.Sink) error {
	select {
	case c.sinks <- s:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func (*textChannel) Send(_ context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{
		model.ProviderMessageRef(r.Route.Channel, "sent-"+string(r.Operation.ID)),
	}}, nil
}

// textRig is a contention rig whose engine has a text channel and a
// per-contact inbox on it, configured through the engine, and the sink
// Runtime.Receive hands the channel's receiver.
type textRig struct {
	*contentionRig
	admin uma.Client
	ch    model.ChannelRef
	inbox model.InboxRef
	snk   adapter.Sink
}

func newTextRig(t *testing.T) *textRig {
	t.Helper()
	return textRigOn(t, newDatabase(t, true).Config(), int32(2*contentionCallers))
}

// textRigOn is a text rig on the database cfg connects to, with a pool of
// conns connections.
func textRigOn(t testing.TB, base *pgxpool.Config, conns int32) *textRig {
	t.Helper()
	return textRigWith(t, base, conns, nil)
}

// textRigWith is textRigOn with the engine's configuration adjusted by tune
// before the engine is built; tune is given the text channel the rig drives.
func textRigWith(t testing.TB, base *pgxpool.Config, conns int32, tune func(*uma.Config, *textChannel)) *textRig {
	t.Helper()
	ctx := t.Context()
	tr := &abortTracer{}
	cfg := base.Copy()
	cfg.ConnConfig.Tracer = tr
	cfg.MaxConns = conns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	st, err := Open(ctx, pool, Options{})
	if err != nil {
		t.Fatal(err)
	}
	uc := &unitCounter{Store: st}
	text := &textChannel{sinks: make(chan adapter.Sink, 1)}
	ecfg := uma.Config{
		Store: uc, Authorizer: benchAllow(), Adapters: []adapter.Factory{text},
		Clock: benchClock{}, IDs: portstest.NewSequenceIDs(0xc2),
	}
	if tune != nil {
		tune(&ecfg, text)
	}
	e, err := uma.New(ecfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	r := &textRig{contentionRig: &contentionRig{tr: tr, uc: uc, e: e}}
	r.admin = r.client(t, "admin")
	acc, err := r.admin.Configuration().ConfigureChannel(ctx, model.ConfigureChannel{
		Command: contentionCommand("text-channel"),
		Channel: model.Channel{
			Ref: model.ChannelRef{Tenant: contentionTenant}, Type: textContentionType,
			Configuration: model.Extension{Name: string(textContentionType) + ".config", Version: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.ch, err = acc.Operation.Result.AsChannel(); err != nil {
		t.Fatal(err)
	}
	lock := true
	acc, err = r.admin.Configuration().ConfigureInbox(ctx, model.ConfigureInbox{
		Command: contentionCommand("text-inbox"),
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: contentionTenant}, Channel: r.ch, Name: "texts", Destinations: []string{textContentionDest},
			Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.inbox, err = acc.Operation.Result.AsInbox(); err != nil {
		t.Fatal(err)
	}
	rctx, stop := context.WithCancel(ctx)
	received := make(chan error, 1)
	go func() {
		received <- e.Runtime().Receive(rctx, uma.RuntimeCall{
			Principal: model.Principal{Tenant: contentionTenant, Actor: "receiver"}, Call: model.Call{Correlation: "receive"},
		}, r.ch)
	}()
	t.Cleanup(func() {
		stop()
		<-received
	})
	select {
	case r.snk = <-text.sinks:
	case err := <-received:
		t.Fatalf("Receive returned %v before its receiver ran", err)
	case <-time.After(30 * time.Second):
		t.Fatal("receiver never ran")
	}
	return r
}

// number is the text number of sender or recipient n.
func (r *textRig) number(n string) model.ExternalRef {
	return model.ExternalRef{Channel: r.ch, Namespace: textContentionNS, Kind: "phone", ID: n}
}

// receive accepts inbound message id from number from.
func (r *textRig) receive(ctx context.Context, id, from string) error {
	_, err := r.snk.Accept(ctx, model.Ingress{
		Call: model.Call{Correlation: model.CorrelationID("c-" + id)},
		Envelope: model.Envelope{
			EventID: "e-" + id, MessageID: "m-" + id, Destination: textContentionDest, Kind: model.EnvelopeMessage,
			Sender: r.number(from), Assurance: "verified",
			Content:  model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "inbound " + id}}},
			Audience: model.Audience{Visibility: model.Public}, OccurredAt: time.Unix(1_700_000_000, 0).UTC(),
		},
	})
	return err
}

// start starts a conversation with number to under operation op.
func (r *textRig) start(ctx context.Context, cl uma.Client, op, to string) error {
	_, err := cl.Conversations().Start(ctx, model.StartConversation{
		Command: contentionCommand(op), Inbox: r.inbox, Recipient: r.number(to),
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello " + to}}},
	})
	return err
}

// measured clears the abort and unit counts of the rig's setup.
func (r *textRig) measured() {
	r.tr.mu.Lock()
	r.tr.aborts = nil
	r.tr.mu.Unlock()
	r.uc.units.Store(0)
	r.uc.lost.Store(0)
}

// TestConcurrentIngressFirstContacts: callers each receiving messages
// from senders the inbox has never seen, at once, all have their messages
// accepted.
func TestConcurrentIngressFirstContacts(t *testing.T) {
	r := newTextRig(t)
	r.measured()
	r.race(t, func(c, i int) error {
		return r.receive(t.Context(), fmt.Sprintf("%d-%d", c, i), fmt.Sprintf("+1555%03d%03d", c, i))
	})
}

// TestConcurrentIngressKnownContacts: callers each receiving messages
// from their own known sender, into that sender's conversation, at once,
// all have their messages accepted.
func TestConcurrentIngressKnownContacts(t *testing.T) {
	r := newTextRig(t)
	for c := range contentionCallers {
		if err := r.receive(t.Context(), fmt.Sprintf("seed-%d", c), fmt.Sprintf("+1555%03d", c)); err != nil {
			t.Fatal(err)
		}
	}
	r.measured()
	r.race(t, func(c, i int) error {
		return r.receive(t.Context(), fmt.Sprintf("%d-%d", c, i), fmt.Sprintf("+1555%03d", c))
	})
}

// TestConcurrentIngressOneContact: callers receiving messages from one
// known sender, into one conversation, at once, all have their messages
// accepted.
func TestConcurrentIngressOneContact(t *testing.T) {
	r := newTextRig(t)
	if err := r.receive(t.Context(), "seed", "+1555000"); err != nil {
		t.Fatal(err)
	}
	r.measured()
	r.race(t, func(c, i int) error {
		return r.receive(t.Context(), fmt.Sprintf("%d-%d", c, i), "+1555000")
	})
}

// TestConcurrentStarts: callers each starting conversations with numbers
// the inbox has never seen, at once, all have their starts accepted.
func TestConcurrentStarts(t *testing.T) {
	r := newTextRig(t)
	clients := make([]uma.Client, contentionCallers)
	for i := range clients {
		clients[i] = r.client(t, fmt.Sprintf("agent-%d", i))
	}
	r.measured()
	r.race(t, func(c, i int) error {
		return r.start(t.Context(), clients[c], fmt.Sprintf("start-%d-%d", c, i), fmt.Sprintf("+1666%03d%03d", c, i))
	})
}
