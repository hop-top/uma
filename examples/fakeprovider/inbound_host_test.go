package fakeprovider_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/examples/fakeprovider"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// Inbound tests run Runtime.Webhook and Runtime.Receive end to end: uma.New
// over the memory store with a fake provider variant registered like any
// third-party adapter, and one channel serving two inboxes.

const inTenant model.TenantID = "inbound-tenant"

var (
	inChannel = model.ChannelRef{Tenant: inTenant, ID: "00000000-0000-7000-8000-0000000001a0"}
	inSupport = model.InboxRef{Tenant: inTenant, ID: "00000000-0000-7000-8000-0000000001b1"}
	inSales   = model.InboxRef{Tenant: inTenant, ID: "00000000-0000-7000-8000-0000000001b2"}
	inService = model.Principal{Tenant: inTenant, Actor: "inbound-service"}
	inSecret  = []byte("signing-secret")
)

// inPacer is the injected Waiter. Lease renewal waits (longer than the test
// retry ceiling) block until the test releases one with renew; backoff
// waits are recorded and return at once.
type inPacer struct {
	mu       sync.Mutex
	backoffs []time.Duration
	renew    chan struct{}
}

func (p *inPacer) Wait(ctx context.Context, d time.Duration) error {
	if d > time.Millisecond {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.renew:
			return nil
		}
	}
	p.mu.Lock()
	p.backoffs = append(p.backoffs, d)
	p.mu.Unlock()
	return ctx.Err()
}

func (p *inPacer) waits() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.backoffs)
}

// inSigningHost resolves the webhook signing secret.
type inSigningHost struct{}

func (inSigningHost) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	if r.Purpose != fakeprovider.WebhookCredentialPurpose {
		return nil, model.NewError(model.Unauthorized, "unexpected purpose")
	}
	return ports.Secret{Bytes: inSecret}, nil
}

type inboundHost struct {
	t       *testing.T
	ctx     context.Context
	engine  uma.Engine
	store   *memory.Store
	clock   *portstest.FixedClock
	inPacer *inPacer
	channel model.Channel
}

func newInboundHost(t *testing.T, f adapter.Factory, cfg model.Extension) *inboundHost {
	t.Helper()
	return newInboundHostOn(t, f, cfg, false)
}

// durableStore is the memory store reporting itself durable, for the
// tests of an acknowledgement that waits for durable acceptance.
type durableStore struct{ store.Store }

func (d durableStore) Properties() store.Properties {
	p := d.Store.Properties()
	p.Durable = true
	return p
}

// newInboundHostOn is newInboundHost over a store that reports itself
// durable when durable is set.
func newInboundHostOn(t *testing.T, f adapter.Factory, cfg model.Extension, durable bool) *inboundHost {
	t.Helper()
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0))
	h := &inboundHost{
		t: t, ctx: t.Context(), clock: clock,
		store:   memory.New(memory.Options{Clock: clock}),
		inPacer: &inPacer{renew: make(chan struct{})},
	}
	var st store.Store = h.store
	if durable {
		st = durableStore{h.store}
	}
	engine, err := uma.New(uma.Config{
		Store: st,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{f},
		Credentials: inSigningHost{},
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(7),
		Waiter:      h.inPacer,
		Retry:       uma.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.engine = engine
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	h.channel = model.Channel{
		Ref: inChannel, Type: f.Describe().Type, Configuration: cfg, CredentialRef: "signing", Revision: "ch1",
	}
	lock := false
	sel := model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1}
	h.tx(func(tx store.Tx) error {
		if err := tx.PutChannel(h.ctx, h.channel, ""); err != nil {
			return err
		}
		for _, in := range []model.Inbox{
			{Ref: inSupport, Channel: inChannel, Name: "support", Destinations: []string{"support"}, Selection: sel, Revision: "in1"},
			{Ref: inSales, Channel: inChannel, Name: "sales", Destinations: []string{"sales"}, Selection: sel, Revision: "in1"},
		} {
			if err := tx.PutInbox(h.ctx, in, ""); err != nil {
				return err
			}
		}
		return nil
	})
	return h
}

func (h *inboundHost) tx(fn func(store.Tx) error) {
	h.t.Helper()
	if err := h.store.Transact(h.ctx, inTenant, fn); err != nil {
		h.t.Fatalf("Transact: %v", err)
	}
}

func inCall() uma.RuntimeCall {
	return uma.RuntimeCall{Principal: inService, Call: model.Call{Correlation: "inbound"}}
}

// messages returns the stored messages of inbox.
func (h *inboundHost) messages(inbox model.InboxRef) []model.Message {
	h.t.Helper()
	var out []model.Message
	err := h.store.View(h.ctx, inTenant, func(r store.Reader) error {
		p, err := r.Messages(h.ctx, model.ListRequest{
			Filter: model.Filter{Inboxes: []model.InboxRef{inbox}}, Page: model.PageRequest{Limit: 100},
		})
		out = p.Items
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

// eventually polls cond until it holds or five seconds pass.
func inEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func inCode(t *testing.T, err error, code model.ErrorCode) {
	t.Helper()
	if got := model.CodeOf(err); got != code {
		t.Fatalf("error = %v (code %q), want %q", err, got, code)
	}
}
