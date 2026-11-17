package conformance

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

const adapterTenant model.TenantID = "conformance-adapter"

// Fixed channel and inbox IDs. Engine IDs come from a sequence with another
// seed, so they never collide.
var (
	adapterChanA  = model.ChannelRef{Tenant: adapterTenant, ID: "00000ad0-0000-7000-8000-00000000000a"}
	adapterChanB  = model.ChannelRef{Tenant: adapterTenant, ID: "00000ad0-0000-7000-8000-00000000000b"}
	adapterInboxA = model.InboxRef{Tenant: adapterTenant, ID: "00000ad0-0000-7000-8000-0000000000a1"}
	adapterInboxB = model.InboxRef{Tenant: adapterTenant, ID: "00000ad0-0000-7000-8000-0000000000b1"}
)

// adapterSecret is the credential every resolution returns, at least 32
// bytes so adapters requiring a full-strength HMAC key accept it. It must
// never appear in anything the engine records or reports.
const adapterSecret = "conformance-secret-7d1c2a9e4b6f03d8c5a1e7b2" // #nosec G101 -- fixture value, not a credential

// adapterEnv is one engine over a fresh store with the adapter under test
// registered, one channel and inbox seeded per account, and control of the
// clock and the worker's idle waits.
type adapterEnv struct {
	h        AdapterHarness
	ctx      context.Context
	factory  adapter.Factory
	provider Provider
	desc     adapter.Descriptor
	store    store.Store
	clock    *portstest.FixedClock
	engine   uma.Engine
	client   uma.Client
	waiter   *idleWaiter
	creds    *credentialHost
	http     ports.HTTPClient
	observed *observations
	run      chan error
	// items counts the work items stored by the work item act cases.
	items int
	// rconv and rid are the responder cases' conversation and actor.
	rconv *model.Conversation
	rid   model.Identity
	// subs counts the subscriptions opened by event reads.
	subs int
}

// envOptions adjusts the engine of one case.
type envOptions struct {
	selectors map[string]policy.ConversationSelector
	// blobs is the host's blob store; nil leaves the engine without one.
	blobs ports.BlobStore
}

func newAdapterEnv(t *testing.T, h AdapterHarness) *adapterEnv {
	return newAdapterEnvWith(t, h, envOptions{})
}

func newAdapterEnvWith(t *testing.T, h AdapterHarness, opts envOptions) *adapterEnv {
	t.Helper()
	factory, provider := h.New(t)
	if factory == nil || provider == nil {
		t.Fatal("AdapterHarness.New returned a nil factory or provider")
	}
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0).UTC())
	e := &adapterEnv{
		h: h, ctx: t.Context(), factory: factory, provider: provider, desc: factory.Describe(), clock: clock,
		waiter:   &idleWaiter{idle: make(chan struct{}, 16), resume: make(chan struct{})},
		creds:    &credentialHost{},
		observed: &observations{},
	}
	if h.Store != nil {
		e.store = h.Store(t, clock)
	} else {
		e.store = memory.New(memory.Options{Clock: clock})
	}
	if hp, ok := provider.(HTTPProvider); ok {
		e.http = hp.HTTPClient()
	}
	engine, err := uma.New(uma.Config{
		Store: e.store,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Selectors:   opts.selectors,
		Blobs:       opts.blobs,
		Adapters:    []adapter.Factory{factory},
		Extensions:  h.Extensions,
		Credentials: e.creds,
		HTTP:        e.http,
		Observer:    e.observed,
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(0xada),
		Waiter:      e.waiter,
	})
	if err != nil {
		t.Fatalf("uma.New with the adapter registered: %v", err)
	}
	e.engine = engine
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	if e.client, err = engine.For(model.Principal{Tenant: adapterTenant, Actor: "conformance-agent"}); err != nil {
		t.Fatal(err)
	}
	return e
}

// selection is the inbox selection of inbound cases.
func (e *adapterEnv) selection() model.Selection {
	if e.h.Selection.Mode != "" {
		return e.h.Selection
	}
	lock := true
	return model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1}
}

// shape returns the provider form of fixture value id on ch's account.
func (e *adapterEnv) shape(ch model.ChannelRef, kind FixtureKind, id string) string {
	if s, ok := e.provider.(FixtureShaper); ok {
		return s.ShapeID(ch, kind, id)
	}
	return id
}

// route is the provider route on ch to fixture peer.
func (e *adapterEnv) route(ch model.ChannelRef, peer string) model.ExternalRef {
	return e.provider.Route(ch, e.shape(ch, FixtureSender, peer))
}

// seed stores channel with the provider account's settings and its inbox
// bound to destinations under sel.
func (e *adapterEnv) seed(t *testing.T, ch model.ChannelRef, inbox model.InboxRef, sel model.Selection, destinations ...string) {
	t.Helper()
	for i, d := range destinations {
		destinations[i] = e.shape(ch, FixtureDestination, d)
	}
	acct := e.provider.Account(ch)
	err := e.store.Transact(e.ctx, adapterTenant, func(tx store.Tx) error {
		if err := tx.PutChannel(e.ctx, model.Channel{
			Ref: ch, Type: e.desc.Type, Configuration: acct.Configuration, CredentialRef: acct.CredentialRef, Revision: "ch1",
		}, ""); err != nil {
			return err
		}
		return tx.PutInbox(e.ctx, model.Inbox{
			Ref: inbox, Channel: ch, Name: "conformance", Destinations: destinations, Selection: sel, Revision: "in1",
		}, "")
	})
	if err != nil {
		t.Fatalf("seed channel %s: %v", ch.ID, err)
	}
}

// conversation stores an open conversation in inbox whose reply route is
// the provider route to peer.
func (e *adapterEnv) conversation(t *testing.T, ch model.ChannelRef, inbox model.InboxRef, id model.ID, peer string) model.Conversation {
	t.Helper()
	c := model.Conversation{
		Ref: model.ConversationRef{Inbox: inbox, ID: id}, Mode: model.ModePerRoom, Status: model.ConversationOpen,
		ReplyRoute: e.route(ch, peer), CreatedAt: e.clock.Now(), Revision: "cv1",
	}
	if err := e.store.Transact(e.ctx, adapterTenant, func(tx store.Tx) error {
		return tx.PutConversation(e.ctx, c, "")
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

func runtimeCall() uma.RuntimeCall {
	return uma.RuntimeCall{Principal: model.Principal{Tenant: adapterTenant, Actor: "conformance-service"}, Call: model.Call{Correlation: "conformance-runtime"}}
}

func (e *adapterEnv) list() model.ListRequest {
	return model.ListRequest{Page: model.PageRequest{Limit: 200}}
}

func (e *adapterEnv) messages(t *testing.T, inbox model.InboxRef) []model.Message {
	t.Helper()
	var out []model.Message
	e.view(t, func(r store.Reader) error {
		req := e.list()
		req.Filter.Inboxes = []model.InboxRef{inbox}
		p, err := r.Messages(e.ctx, req)
		out = p.Items
		return err
	})
	return out
}

func (e *adapterEnv) conversations(t *testing.T, inbox model.InboxRef) []model.Conversation {
	t.Helper()
	var out []model.Conversation
	e.view(t, func(r store.Reader) error {
		req := e.list()
		req.Filter.Inboxes = []model.InboxRef{inbox}
		p, err := r.Conversations(e.ctx, req)
		out = p.Items
		return err
	})
	return out
}

func (e *adapterEnv) identities(t *testing.T, inbox model.InboxRef) []model.Identity {
	t.Helper()
	var out []model.Identity
	e.view(t, func(r store.Reader) error {
		req := e.list()
		req.Filter.Inboxes = []model.InboxRef{inbox}
		p, err := r.Identities(e.ctx, req)
		out = p.Items
		return err
	})
	return out
}

func (e *adapterEnv) view(t *testing.T, fn func(store.Reader) error) {
	t.Helper()
	if err := e.store.View(e.ctx, adapterTenant, fn); err != nil {
		t.Fatalf("View: %v", err)
	}
}

// deliver hands ev to channel's account, skipping the case when the
// provider cannot express it.
func (e *adapterEnv) deliver(t *testing.T, ch model.ChannelRef, ev InboundEvent) {
	t.Helper()
	if err := e.provider.Deliver(ch, ev); err != nil {
		if errors.Is(err, ErrNotExpressible) {
			t.Skipf("provider cannot express inbound event %s", ev.EventID)
		}
		t.Fatalf("Deliver %s: %v", ev.EventID, err)
	}
}

// message is a plain inbound text message from sender to destination, in
// the fixture shape of ch's account, where it is delivered.
func (e *adapterEnv) message(ch model.ChannelRef, id, sender, destination string) InboundEvent {
	ev := InboundEvent{
		EventID: e.shape(ch, FixtureEvent, "evt-"+id), MessageID: e.shape(ch, FixtureMessage, "msg-"+id),
		Destination: e.shape(ch, FixtureDestination, destination),
		Sender:      e.shape(ch, FixtureSender, sender), Room: e.shape(ch, FixtureRoom, "room-1"), Text: "hello " + id,
	}
	if s, ok := e.provider.(EventShaper); ok {
		ev = s.ShapeEvent(ch, ev)
	}
	return ev
}

// claims shapes unauthenticated sender claims for ch's account.
func (e *adapterEnv) claims(ch model.ChannelRef, ids ...string) []string {
	for i, id := range ids {
		ids[i] = e.shape(ch, FixtureSender, id)
	}
	return ids
}

// adapterUser is the user name of the password and OAuth2 credentials the
// host returns.
const adapterUser = "conformance-user"

// credentialHost is the host credential port: it records every request,
// returns adapterSecret as the first kind the request accepts (a secret
// when it names none) and can revoke consent.
type credentialHost struct {
	mu      sync.Mutex
	revoked bool
	reqs    []ports.CredentialRequest
}

func (h *credentialHost) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reqs = append(h.reqs, r)
	if h.revoked {
		return nil, model.NewError(model.Unauthorized, "consent revoked")
	}
	if len(r.Accept) == 0 {
		return ports.Secret{Bytes: []byte(adapterSecret)}, nil
	}
	for _, k := range r.Accept {
		switch k {
		case ports.KindSecret:
			return ports.Secret{Bytes: []byte(adapterSecret)}, nil
		case ports.KindPassword:
			return ports.Password{User: adapterUser, Secret: []byte(adapterSecret)}, nil
		case ports.KindOAuth2:
			return ports.OAuth2{User: adapterUser, AccessToken: adapterSecret}, nil
		}
	}
	return nil, model.NewError(model.Unsupported, "no accepted credential kind")
}

func (h *credentialHost) revoke() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.revoked = true
}

func (h *credentialHost) requests() []ports.CredentialRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ports.CredentialRequest(nil), h.reqs...)
}

// observations records every engine and adapter observation.
type observations struct {
	mu  sync.Mutex
	all []ports.Observation
}

func (o *observations) Observe(_ context.Context, ob ports.Observation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.all = append(o.all, ob)
}

func (o *observations) disclose(secret string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, ob := range o.all {
		if strings.Contains(ob.Kind, secret) || strings.Contains(string(ob.Correlation), secret) {
			return true
		}
	}
	return false
}
