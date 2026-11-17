package whatsapp_test

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio/whatsapp"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// pacer reports each idle Runtime.Run wait and blocks it until resumed.
type pacer struct{ idle, resume chan struct{} }

func (p *pacer) Wait(ctx context.Context, _ time.Duration) error {
	select {
	case p.idle <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.resume:
		return nil
	}
}

// host is an engine on the memory store with the given adapters.
type host struct {
	t      *testing.T
	ctx    context.Context
	clock  *portstest.FixedClock
	store  *memory.Store
	engine uma.Engine
	pacer  *pacer
	creds  *tokenHost
}

const hostTenant model.TenantID = "twilio-whatsapp-host"

func newHost(t *testing.T, httpc ports.HTTPClient, factories ...adapter.Factory) *host {
	t.Helper()
	clock := portstest.NewFixedClock(testNow)
	h := &host{
		t: t, ctx: t.Context(), clock: clock, store: memory.New(memory.Options{Clock: clock}),
		pacer: &pacer{idle: make(chan struct{}, 16), resume: make(chan struct{})}, creds: &tokenHost{},
	}
	engine, err := uma.New(uma.Config{
		Store: h.store,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    factories,
		Credentials: h.creds,
		HTTP:        httpc,
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(31),
		Waiter:      h.pacer,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.engine = engine
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	return h
}

// channel stores a channel, its inbox (destination dest) and an open
// conversation replying to route.
func (h *host) channel(ch model.ChannelRef, typ model.TypeKey, cfg model.Extension, inbox model.InboxRef, dest string, route model.ExternalRef) model.ConversationRef {
	h.t.Helper()
	lock := true
	conv := model.ConversationRef{Inbox: inbox, ID: model.ID("00000000-0000-7000-8000-0000000009" + string(inbox.ID[len(inbox.ID)-2:]))}
	err := h.store.Transact(h.ctx, hostTenant, func(tx store.Tx) error {
		if err := tx.PutChannel(h.ctx, model.Channel{Ref: ch, Type: typ, Configuration: cfg, CredentialRef: "twilio", Revision: "ch1"}, ""); err != nil {
			return err
		}
		if err := tx.PutInbox(h.ctx, model.Inbox{
			Ref: inbox, Channel: ch, Name: string(typ), Destinations: []string{dest}, Revision: "in1",
			Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
		}, ""); err != nil {
			return err
		}
		return tx.PutConversation(h.ctx, model.Conversation{
			Ref: conv, Mode: model.ModePerRoom, Status: model.ConversationOpen, CreatedAt: h.clock.Now(), Revision: "cv1", ReplyRoute: route,
		}, "")
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return conv
}

func (h *host) client() uma.Client {
	h.t.Helper()
	c, err := h.engine.For(model.Principal{Tenant: hostTenant, Actor: "replier"})
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *host) reply(conv model.ConversationRef, op string) model.Acceptance {
	h.t.Helper()
	acc, err := h.client().Messages().Reply(h.ctx, model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: model.CorrelationID("reply-" + op)}, Operation: model.OperationID(op)},
		Conversation: conv,
		Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello"}}},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return acc
}

// run lets the worker go idle rounds times, an hour apart, then drains.
func (h *host) run(rounds int) {
	h.t.Helper()
	steps := make([]time.Duration, rounds)
	for i := range steps {
		steps[i] = time.Hour
	}
	h.runSteps(steps, nil)
}

// runSteps lets the worker go idle, then once after each step: it advances
// the clock by the step and resumes the worker, which runs until idle
// again. before, if set, runs before each step with its index. It then
// drains.
func (h *host) runSteps(steps []time.Duration, before func(i int)) {
	h.t.Helper()
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: hostTenant, Actor: "worker"}, Call: model.Call{Correlation: "run"}}
	done := make(chan error, 1)
	go func() { done <- h.engine.Runtime().Run(h.ctx, call, uma.WorkerOptions{Owner: "w1", Limit: 10}) }()
	idle := func() {
		h.t.Helper()
		select {
		case <-h.pacer.idle:
		case err := <-done:
			h.t.Fatalf("Run returned early: %v", err)
		case <-time.After(10 * time.Second):
			h.t.Fatal("Run did not go idle")
		}
	}
	idle()
	for i, d := range steps {
		if before != nil {
			before(i)
		}
		h.clock.Advance(d)
		h.pacer.resume <- struct{}{}
		idle()
	}
	if err := h.engine.Runtime().Drain(h.ctx, call); err != nil {
		h.t.Fatal(err)
	}
	if err := <-done; err != nil {
		h.t.Fatalf("Run: %v", err)
	}
}

func (h *host) operation(ref model.OperationRef) model.Operation {
	h.t.Helper()
	op, err := h.client().Operations().Get(h.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "get"}, Ref: ref})
	if err != nil {
		h.t.Fatal(err)
	}
	return op
}

var (
	hostChannel = model.ChannelRef{Tenant: hostTenant, ID: "00000000-0000-7000-8000-0000000008a0"}
	hostInbox   = model.InboxRef{Tenant: hostTenant, ID: "00000000-0000-7000-8000-0000000008b1"}
	hostRoute   = model.ExternalRef{Channel: hostChannel, Namespace: "twilio.whatsapp", Kind: "user", ID: userNumber}
)

// A 5xx answer arrives after Twilio read the request, which may have
// created the message: the runtime leaves the reply outcome_unknown and
// sends nothing more before a listing shows Twilio holds no such message.
// Here the server created none, so exactly one resend follows the listing.
func TestServerErrorAfterSendIsReconciledBeforeAnyResend(t *testing.T) {
	for _, code := range []int{500, 502, 503, 504} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			fake := newFakeTwilio(t)
			fake.respond(hookAccount, conformance.Response{Outcome: outcomeStatus(code)})
			h := newHost(t, fake.client(), whatsappFactory(fake))
			conv := h.channel(hostChannel, model.TypeTwilioWhatsapp, hookConfig().Extension(), hostInbox, senderNumber, hostRoute)
			acc := h.reply(conv, "op-5xx")
			h.run(3)
			op := h.operation(acc.Operation.Ref)
			if op.State != model.ProviderAccepted || len(op.ProviderReferences) == 0 {
				t.Fatalf("operation = %s (%v), want provider_accepted from the reconciling listing", op.State, op.Failure)
			}
			var methods []string
			for _, c := range fake.calls(hookAccount) {
				methods = append(methods, c.Method)
			}
			if want := []string{http.MethodPost, http.MethodGet, http.MethodPost}; !slices.Equal(methods, want) {
				t.Fatalf("Twilio calls = %v, want %v: create, the reconciling listing, one resend", methods, want)
			}
		})
	}
}

// Twilio's Messages list shows a new message only some time after creating
// it. The adapter declares that lag as its reconcile settle window, as
// twilio.sms does: a listing inside it that misses the message whose answer
// was lost never leads to a resend.
func TestLaggingListingNeverCausesResend(t *testing.T) {
	if got := whatsapp.NewFactory(whatsapp.Options{}).Describe().ReconcileSettle; got != whatsapp.ReconcileSettle || got <= 0 {
		t.Fatalf("ReconcileSettle = %v, want the declared %v", got, whatsapp.ReconcileSettle)
	}
	fake := newFakeTwilio(t)
	fake.respond(hookAccount, conformance.Response{Outcome: conformance.OutcomeAnswerLost})
	fake.lagListings()
	h := newHost(t, fake.client(), whatsappFactory(fake))
	conv := h.channel(hostChannel, model.TypeTwilioWhatsapp, hookConfig().Extension(), hostInbox, senderNumber, hostRoute)
	acc := h.reply(conv, "op-lag")
	var steps []time.Duration
	for elapsed := time.Duration(0); elapsed < whatsapp.ReconcileSettle-time.Minute; elapsed += 30 * time.Second {
		steps = append(steps, 30*time.Second)
	}
	caughtUp := len(steps)
	steps = append(steps, time.Hour, time.Hour, time.Hour)
	h.runSteps(steps, func(i int) {
		if i == caughtUp {
			fake.catchUp()
		}
	})
	op := h.operation(acc.Operation.Ref)
	if op.State != model.ProviderAccepted || len(op.ProviderReferences) != 1 {
		t.Fatalf("operation = %s (%v), want provider_accepted from the listing that caught up", op.State, op.Failure)
	}
	var creates int
	var methods []string
	for _, c := range fake.calls(hookAccount) {
		methods = append(methods, c.Method)
		if c.Method == http.MethodPost {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("Twilio calls = %v, want one create: a listing that has not caught up must not cause a resend", methods)
	}
}
