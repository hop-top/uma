package fakeprovider_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/companions/bot"
	"hop.top/uma/examples/fakeprovider"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// The bot companion's interaction routes end to end: uma.New over the
// memory store with two provider accounts, a webhook one that reports
// interactions through a signed request and an interactive one that holds
// stored interactions and answers them, the dispatcher on each bot's own
// subscription and delivery through Runtime.Run.

const ixTenant model.TenantID = "ix-tenant"

var (
	ixWebChan  = model.ChannelRef{Tenant: ixTenant, ID: "00000000-0000-7000-8000-0000000003a0"}
	ixIntChan  = model.ChannelRef{Tenant: ixTenant, ID: "00000000-0000-7000-8000-0000000003b0"}
	ixWebInbox = model.InboxRef{Tenant: ixTenant, ID: "00000000-0000-7000-8000-0000000003a1"}
	ixIntInbox = model.InboxRef{Tenant: ixTenant, ID: "00000000-0000-7000-8000-0000000003b1"}
	ixConv     = model.ConversationRef{Inbox: ixIntInbox, ID: "00000000-0000-7000-8000-0000000003c1"}
	ixService  = model.Principal{Tenant: ixTenant, Actor: "ix-service"}
	ixAt       = time.Unix(1_700_000_000, 0).UTC()
)

// ixCredentials resolves the webhook signing secret and any other
// credential as an opaque token.
type ixCredentials struct{}

func (ixCredentials) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	if r.Purpose == fakeprovider.WebhookCredentialPurpose {
		return ports.Secret{Bytes: inSecret}, nil
	}
	return ports.Secret{Bytes: []byte("token")}, nil
}

type ixHost struct {
	t       *testing.T
	ctx     context.Context
	engine  uma.Engine
	store   *memory.Store
	clock   *portstest.FixedClock
	web     *fakeprovider.Factory
	inter   *fakeprovider.Factory
	webhook http.Handler
	worker  chan error
	seeded  int
}

func newIxHost(t *testing.T) *ixHost {
	t.Helper()
	clock := portstest.NewFixedClock(ixAt)
	h := &ixHost{
		t: t, ctx: t.Context(), clock: clock, store: memory.New(memory.Options{Clock: clock}),
		web: fakeprovider.NewWebhook(), inter: fakeprovider.NewInteractive(),
	}
	engine, err := uma.New(uma.Config{
		Store: durableStore{h.store},
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{h.web, h.inter},
		Credentials: ixCredentials{},
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(23),
		Retry:       uma.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.engine = engine
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	lock := false
	perContact := model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1}
	h.tx(func(tx store.Tx) error {
		for _, c := range []struct {
			ch    model.Channel
			inbox model.Inbox
		}{
			{
				model.Channel{Ref: ixWebChan, Type: fakeprovider.TypeWebhook, Configuration: h.web.Config(), CredentialRef: "web-cred", Revision: "ch1"},
				model.Inbox{Ref: ixWebInbox, Channel: ixWebChan, Name: "support", Destinations: []string{"support"}, Selection: perContact, Revision: "in1"},
			},
			{
				model.Channel{Ref: ixIntChan, Type: fakeprovider.TypeInteractive, Configuration: h.inter.Config(), CredentialRef: "int-cred", Revision: "ch1"},
				model.Inbox{Ref: ixIntInbox, Channel: ixIntChan, Name: "rooms", Revision: "in1"},
			},
		} {
			if err := tx.PutChannel(h.ctx, c.ch, ""); err != nil {
				return err
			}
			if err := tx.PutInbox(h.ctx, c.inbox, ""); err != nil {
				return err
			}
		}
		return tx.PutConversation(h.ctx, model.Conversation{
			Ref: ixConv, Mode: model.ModePerRoom, Status: model.ConversationOpen,
			ReplyRoute: model.ExternalRef{Channel: ixIntChan, Namespace: fakeprovider.Namespace, Kind: "room", ID: "room-1"},
			CreatedAt:  ixAt, Revision: "cv1",
		}, "")
	})
	if h.webhook, err = engine.Runtime().Webhook(h.ctx, ixRuntimeCall(), ixWebChan); err != nil {
		t.Fatal(err)
	}
	return h
}

func ixRuntimeCall() uma.RuntimeCall {
	return uma.RuntimeCall{Principal: ixService, Call: model.Call{Correlation: "ix-host"}}
}

func (h *ixHost) tx(fn func(store.Tx) error) {
	h.t.Helper()
	if err := h.store.Transact(h.ctx, ixTenant, fn); err != nil {
		h.t.Fatalf("Transact: %v", err)
	}
}

// start runs one delivery worker until drain.
func (h *ixHost) start() {
	h.worker = make(chan error, 1)
	go func() {
		h.worker <- h.engine.Runtime().Run(h.ctx, ixRuntimeCall(), uma.WorkerOptions{Owner: "ix-worker", Limit: 10, Poll: ixWorkerPoll})
	}()
}

func (h *ixHost) drain() {
	h.t.Helper()
	if err := h.engine.Runtime().Drain(h.ctx, ixRuntimeCall()); err != nil {
		h.t.Fatal(err)
	}
	select {
	case err := <-h.worker:
		// Drain can reach the runtime before the worker goroutine has
		// registered; uma then refuses the late Run for good, which is the
		// stop this drain asked for.
		if !stoppedByOwnDrain(err) {
			h.t.Fatalf("Run returned %v after drain", err)
		}
	case <-time.After(5 * time.Second):
		h.t.Fatal("worker did not stop after drain")
	}
}

// bot is one dispatcher over its own subscription to interaction.received.
type ixBot struct {
	h     *ixHost
	actor model.ActorID
	sub   model.SubscriptionRef

	mu      sync.Mutex
	replays []bool
	seen    []*model.Interaction
}

func (h *ixHost) newBot(actor model.ActorID) *ixBot {
	h.t.Helper()
	c, err := h.engine.For(model.Principal{Tenant: ixTenant, Actor: actor})
	if err != nil {
		h.t.Fatal(err)
	}
	opened, err := c.Events().Open(h.ctx, model.OpenSubscription{
		Command:  model.Command{Call: model.Call{Correlation: "ix-open"}, Operation: model.OperationID("open-" + string(actor))},
		Consumer: string(actor), Filter: model.Filter{Kinds: []string{model.EventInteractionReceived}},
		Start: model.StartBeginning, PayloadMajor: model.PayloadVersion1,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return &ixBot{h: h, actor: actor, sub: opened.Subscription.Ref}
}

func (h *ixHost) client(actor model.ActorID) uma.Client {
	h.t.Helper()
	c, err := h.engine.For(model.Principal{Tenant: ixTenant, Actor: actor})
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

// dispatcher builds the bot over client: "triage" answers with a message,
// then replies publicly; "approve" and the "feedback" form only note what
// they saw.
func (b *ixBot) dispatcher(client uma.Client) *bot.Bot {
	b.h.t.Helper()
	note := func(_ context.Context, t *bot.Turn) error {
		b.mu.Lock()
		defer b.mu.Unlock()
		if t.Message != nil || t.Interaction == nil || t.Conversation != t.Interaction.Ref.Conversation {
			b.h.t.Errorf("turn %+v: want an interaction in its conversation and no message", t)
		}
		b.seen = append(b.seen, t.Interaction)
		return nil
	}
	d, err := bot.New(bot.Config{
		Client: client, Self: b.actor, Subscription: b.sub,
		Commands: map[string]bot.Handler{"triage": func(ctx context.Context, t *bot.Turn) error {
			a, err := t.Respond(ctx, model.ResponseModeMessage, model.Audience{Visibility: model.Public},
				model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "on it"}}})
			b.mu.Lock()
			b.replays = append(b.replays, a.Replayed)
			b.mu.Unlock()
			if err != nil {
				return err
			}
			_, err = t.Reply(ctx, model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "logged"}}})
			return err
		}},
		Actions:     map[string]bot.Handler{"approve": note},
		Submissions: map[string]bot.Handler{"org.example.feedback": note},
	})
	if err != nil {
		b.h.t.Fatal(err)
	}
	return d
}

// step runs d until it handled the next delivered batch.
func (b *ixBot) step(d *bot.Bot) (bot.Report, error) {
	b.h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := d.Step(b.h.ctx)
		if err != nil || len(r.Results) > 0 {
			return r, err
		}
		if time.Now().After(deadline) {
			b.h.t.Fatal("no batch delivered")
		}
		time.Sleep(time.Millisecond)
	}
}

// seed stores a command interaction of the interactive account and queues
// its interaction.received event, as ingress would. respondUntil is the end
// of its respond window.
func (h *ixHost) seed(command string, respondUntil time.Time) model.Interaction {
	h.t.Helper()
	h.seeded++
	n := h.seeded
	id := model.ID("00000000-0000-7000-8000-0000000005" + pad2(n))
	actorID := model.ID("00000000-0000-7000-8000-0000000003d1")
	ident := model.Identity{
		Ref:     model.IdentityRef{Inbox: ixIntInbox, ID: actorID},
		Contact: model.ContactRef{Tenant: ixTenant, ID: actorID}, SourceID: "u1",
		Namespace: fakeprovider.Namespace, Kind: model.ContactKindPerson, Revision: "id1",
	}
	rec := store.InteractionRecord{
		Interaction: model.Interaction{
			Ref:  model.InteractionRef{Conversation: ixConv, ID: id},
			Kind: model.InteractionCommand, Command: command,
			Actor: model.Author{
				Identity: ident.Ref, Kind: ident.Kind,
				External: model.ExternalRef{Channel: ixIntChan, Namespace: ident.Namespace, Kind: string(model.KindIdentity), ID: ident.SourceID},
			},
			External: []model.ExternalRef{model.ProviderInteractionRef(ixIntChan, "pi-"+string(id))},
			Audience: model.Audience{Visibility: model.Public}, OccurredAt: ixAt, RecordedAt: ixAt,
			Deadline: ixAt.Add(3 * time.Second), RespondUntil: respondUntil, Revision: "ix1", Source: fakeprovider.TypeInteractive,
		},
		Ingress: "00000009-0000-7000-8000-000000000001", ResponseToken: "tok-ix-secret", TokenExpiresAt: respondUntil,
	}
	data, err := json.Marshal(model.InteractionReceivedPayload{
		Interaction: rec.Interaction.Ref, Trigger: model.TriggerProvider, Source: fakeprovider.TypeInteractive,
		Kind: rec.Interaction.Kind, Actor: ident.Ref, ActorKind: ident.Kind, Command: command,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	ev := model.Event{
		Version: model.EventEnvelopeVersion, ID: model.ID("00000000-0000-7000-8000-0000000006" + pad2(n)),
		Kind: model.EventInteractionReceived, Tenant: ixTenant, Inbox: ixIntInbox.ID, Resource: rec.Interaction.Ref.Ref(),
		Actor: "ix-ingress", OccurredAt: ixAt, RecordedAt: ixAt, Correlation: "ix-rx", Causation: rec.Ingress,
		Audience: rec.Interaction.Audience, Revision: rec.Interaction.Revision,
		Payload: model.Extension{Name: model.SchemaInteractionReceived, Version: model.PayloadVersion1, Data: data},
	}
	h.tx(func(tx store.Tx) error {
		key := model.IdentityKey{Inbox: ixIntInbox, Namespace: ident.Namespace, SourceID: ident.SourceID}
		if n == 1 {
			if _, _, err := tx.ClaimIdentity(h.ctx, key, ident); err != nil {
				return err
			}
		}
		if err := tx.PutInteraction(h.ctx, rec); err != nil {
			return err
		}
		return tx.AppendEvents(h.ctx, []model.Event{ev})
	})
	return rec.Interaction
}

func pad2(n int) string {
	return fmt.Sprintf("%02d", n)
}

// providerCalls returns the applied calls of method the interactive
// account received.
func (h *ixHost) providerCalls(method fakeprovider.Method) []fakeprovider.Call {
	var out []fakeprovider.Call
	for _, c := range h.inter.Script(ixIntChan).Calls() {
		if c.Method == method && c.Applied {
			out = append(out, c)
		}
	}
	return out
}

func (h *ixHost) settled(what string, cond func() bool) { inEventually(h.t, what, cond) }

// diesBeforeIxAck fails every acknowledgment, as a process killed between
// its handlers and its ACK.
type diesBeforeIxAck struct{ uma.Client }

func (c diesBeforeIxAck) Events() uma.Events { return dyingEvents{c.Client.Events()} }

// An interaction answered, then the bot dies before its ACK: the restarted
// bot gets the event again, resubmits the same operations and uma replays
// them. The provider answers once and sends the public reply once, and the
// reply names the interaction.
func TestBotRespondReplaysOnRedeliveryEndToEnd(t *testing.T) {
	h := newIxHost(t)
	in := h.seed("triage", ixAt.Add(time.Hour))
	h.start()
	b := h.newBot("helper-bot")

	r, err := b.step(b.dispatcher(diesBeforeIxAck{h.client(b.actor)}))
	if !errors.Is(err, errBotDied) || len(r.Results) != 1 || r.Results[0].Outcome != bot.Handled {
		t.Fatalf("dying bot: report %+v, err %v", r, err)
	}
	r, err = b.step(b.dispatcher(h.client(b.actor)))
	if err != nil || len(r.Results) != 1 || r.Results[0].Outcome != bot.Handled || r.Results[0].Route != bot.RouteCommand || !r.Acked {
		t.Fatalf("restarted bot: report %+v, err %v", r, err)
	}
	b.mu.Lock()
	replays := append([]bool(nil), b.replays...)
	b.mu.Unlock()
	if len(replays) != 2 || replays[0] || !replays[1] {
		t.Fatalf("response acceptances replayed = %v, want the first new and the redelivered one replayed", replays)
	}
	h.settled("response and reply delivered", func() bool {
		return len(h.providerCalls(fakeprovider.MethodRespond)) >= 1 && len(h.providerCalls(fakeprovider.MethodSend)) >= 1
	})
	h.drain()
	if got := len(h.providerCalls(fakeprovider.MethodRespond)); got != 1 {
		t.Fatalf("provider response calls = %d, want 1", got)
	}
	sends := h.providerCalls(fakeprovider.MethodSend)
	if len(sends) != 1 || bot.Text(sends[0].Content) != "logged" {
		t.Fatalf("provider sends = %+v, want the one public reply", sends)
	}
	var correlated int
	page, err := h.client(b.actor).Messages().List(h.ctx, model.ListRequest{
		Call: model.Call{Correlation: "ix-list"}, Filter: model.Filter{Inboxes: []model.InboxRef{ixIntInbox}}, Page: model.PageRequest{Limit: 50},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range page.Items {
		if m.InResponseTo != nil && *m.InResponseTo == in.Ref {
			correlated++
		}
	}
	if correlated != 2 {
		t.Fatalf("%d messages name the interaction, want the response message and the reply", correlated)
	}
}

// Two workers answer one interaction, each on its own subscription: the one
// that claims the initial response answers, the other's event is ignored and
// acknowledged, and the provider is called once.
func TestBotRespondLosesInitialClaimEndToEnd(t *testing.T) {
	h := newIxHost(t)
	h.seed("triage", ixAt.Add(time.Hour))
	h.start()
	first, second := h.newBot("helper-bot"), h.newBot("second-bot")

	r, err := first.step(first.dispatcher(h.client(first.actor)))
	if err != nil || len(r.Results) != 1 || r.Results[0].Outcome != bot.Handled {
		t.Fatalf("first worker: report %+v, err %v", r, err)
	}
	r, err = second.step(second.dispatcher(h.client(second.actor)))
	if err != nil || len(r.Results) != 1 || r.Results[0].Outcome != bot.Ignored || r.Results[0].Err != nil || !r.Acked {
		t.Fatalf("losing worker: report %+v, err %v; want ignored and acknowledged", r, err)
	}
	h.settled("response delivered", func() bool { return len(h.providerCalls(fakeprovider.MethodRespond)) >= 1 })
	h.drain()
	if got := len(h.providerCalls(fakeprovider.MethodRespond)); got != 1 {
		t.Fatalf("provider response calls = %d, want 1", got)
	}
}

// A response after the interaction's respond window is dropped, and the
// provider is never called.
func TestBotRespondExpiredDropsEndToEnd(t *testing.T) {
	h := newIxHost(t)
	h.seed("triage", ixAt.Add(-time.Minute))
	h.start()
	b := h.newBot("helper-bot")
	r, err := b.step(b.dispatcher(h.client(b.actor)))
	if err != nil || len(r.Results) != 1 || r.Results[0].Outcome != bot.Dropped || model.CodeOf(r.Results[0].Err) != model.DeadlineExpired || !r.Acked {
		t.Fatalf("report %+v, err %v; want dropped as expired, acknowledged", r, err)
	}
	h.drain()
	if got := len(h.providerCalls(fakeprovider.MethodRespond)); got != 0 {
		t.Fatalf("provider response calls = %d after the window ended, want none", got)
	}
}

// Interactions a provider reports through its webhook route by command,
// action and form schema; one no route names is ignored.
func TestBotRoutesWebhookInteractions(t *testing.T) {
	h := newIxHost(t)
	deadline := ixAt.Add(3 * time.Second)
	post := func(id string, mod func(*fakeprovider.WebhookEvent)) {
		t.Helper()
		body := interactionBody(t, id, deadline, mod)
		if code, _ := inPost(h.webhook, body, fakeprovider.Sign(inSecret, body)); code != http.StatusOK {
			t.Fatalf("webhook %s = %d, want 200", id, code)
		}
	}
	post("ix-a", func(ev *fakeprovider.WebhookEvent) {
		ev.Interaction.Kind, ev.Interaction.Action, ev.Interaction.Command, ev.Interaction.Arguments = model.InteractionCommand, "", "triage", "now"
	})
	post("ix-b", func(*fakeprovider.WebhookEvent) {})
	post("ix-c", func(ev *fakeprovider.WebhookEvent) {
		ev.Interaction.Kind, ev.Interaction.Action = model.InteractionSubmission, ""
		ev.Interaction.Fields = model.Extension{Name: "org.example.feedback", Version: 1, Data: json.RawMessage(`{"rating":5}`)}
	})
	post("ix-d", func(ev *fakeprovider.WebhookEvent) { ev.Interaction.Action = "unrouted" })

	b := h.newBot("helper-bot")
	d := b.dispatcher(h.client(b.actor))
	var results []bot.Result
	for len(results) < 4 {
		r, err := b.step(d)
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, r.Results...)
	}
	want := []struct {
		route   bot.Route
		outcome bot.Outcome
	}{{bot.RouteCommand, bot.Dropped}, {bot.RouteAction, bot.Handled}, {bot.RouteSubmission, bot.Handled}, {"", bot.Ignored}}
	for i, w := range want {
		// The webhook account cannot answer: the command handler's response
		// is refused as unsupported and dropped, never retried.
		if results[i].Route != w.route || results[i].Outcome != w.outcome {
			t.Errorf("result %d = route %q outcome %s (%v), want %q %s", i, results[i].Route, results[i].Outcome, results[i].Err, w.route, w.outcome)
		}
	}
	if code := model.CodeOf(results[0].Err); code != model.Unsupported {
		t.Errorf("response through a webhook-only account = %q, want unsupported", code)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.seen) != 2 || b.seen[0].Action != actionID || b.seen[1].Fields.Name != "org.example.feedback" {
		t.Fatalf("handled interactions = %+v, want the approve action and the feedback submission", b.seen)
	}
}

// ixWorkerPoll is how long the delivery worker idles between rounds.
const ixWorkerPoll = time.Millisecond
