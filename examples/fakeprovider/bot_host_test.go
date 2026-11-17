package fakeprovider_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/companions/bot"
	"hop.top/uma/examples/fakeprovider"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// The bot companion end to end: uma.New over the memory store with two
// instances of the full fake provider, ingress through Runtime.Receive,
// the dispatcher on the bot's own subscription and delivery through
// Runtime.Run. Both provider accounts use the same remote room and user
// IDs; uma keeps them apart.

const botTenant model.TenantID = "bot-tenant"

var (
	botChanA   = model.ChannelRef{Tenant: botTenant, ID: "00000000-0000-7000-8000-0000000002a0"}
	botChanB   = model.ChannelRef{Tenant: botTenant, ID: "00000000-0000-7000-8000-0000000002b0"}
	botInboxA  = model.InboxRef{Tenant: botTenant, ID: "00000000-0000-7000-8000-0000000002a1"}
	botInboxB  = model.InboxRef{Tenant: botTenant, ID: "00000000-0000-7000-8000-0000000002b1"}
	botService = model.Principal{Tenant: botTenant, Actor: "bot-service"}
	botActor   = model.Principal{Tenant: botTenant, Actor: "helper-bot"}
	errBotDied = errors.New("bot process died")
)

type botHost struct {
	t       *testing.T
	ctx     context.Context
	engine  uma.Engine
	store   *memory.Store
	units   *contended
	factory *fakeprovider.Factory
	sub     model.SubscriptionRef
	watches *bot.MemoryWatches
	n       int

	mu       sync.Mutex
	replayed []bool
	ops      []model.OperationRef

	receivers []chan error
	worker    chan error
}

func newBotHost(t *testing.T) *botHost {
	t.Helper()
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0))
	h := &botHost{
		t: t, ctx: t.Context(), store: memory.New(memory.Options{Clock: clock}),
		factory: fakeprovider.NewFull(), watches: bot.NewMemoryWatches(),
	}
	h.units = &contended{Store: h.store, lose: map[string]int{}, lost: map[string]int{}}
	engine, err := uma.New(uma.Config{
		Store: h.units,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{h.factory},
		Extensions:  h.factory.Extensions(),
		Credentials: &credentialHost{},
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(11),
		Retry:       uma.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.engine = engine
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	err = h.store.Transact(h.ctx, botTenant, func(tx store.Tx) error {
		for _, ch := range []struct {
			ref   model.ChannelRef
			inbox model.InboxRef
		}{{botChanA, botInboxA}, {botChanB, botInboxB}} {
			channel := model.Channel{Ref: ch.ref, Type: fakeprovider.TypeFull, Configuration: h.factory.Config(), CredentialRef: "bot-cred", Revision: "ch1"}
			if err := tx.PutChannel(h.ctx, channel, ""); err != nil {
				return err
			}
			inbox := model.Inbox{
				Ref: ch.inbox, Channel: ch.ref, Name: "rooms", Destinations: []string{"rooms"},
				Selection: model.Selection{Mode: model.ModePerRoom}, Revision: "in1",
			}
			if err := tx.PutInbox(h.ctx, inbox, ""); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := h.client(botActor).Events().Open(h.ctx, model.OpenSubscription{
		Command:  model.Command{Call: model.Call{Correlation: "bot-open"}, Operation: "bot-open"},
		Consumer: "helper", Filter: model.Filter{Kinds: []string{model.EventMessageReceived}},
		Start: model.StartBeginning, PayloadMajor: model.PayloadVersion1,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.sub = opened.Subscription.Ref
	return h
}

func (h *botHost) client(p model.Principal) uma.Client {
	h.t.Helper()
	c, err := h.engine.For(p)
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func botRuntimeCall() uma.RuntimeCall {
	return uma.RuntimeCall{Principal: botService, Call: model.Call{Correlation: "bot-host"}}
}

// start runs both receivers and one delivery worker until the test drains.
func (h *botHost) start() {
	for _, ch := range []model.ChannelRef{botChanA, botChanB} {
		done := make(chan error, 1)
		go func() { done <- h.engine.Runtime().Receive(h.ctx, botRuntimeCall(), ch) }()
		h.receivers = append(h.receivers, done)
	}
	h.worker = make(chan error, 1)
	go func() {
		h.worker <- h.engine.Runtime().Run(h.ctx, botRuntimeCall(),
			uma.WorkerOptions{Owner: "bot-worker", Limit: 10, Poll: time.Millisecond})
	}()
}

// drain stops the receivers and the worker after the work they hold.
func (h *botHost) drain() {
	h.t.Helper()
	if err := h.engine.Runtime().Drain(h.ctx, botRuntimeCall()); err != nil {
		h.t.Fatal(err)
	}
	for _, done := range append(h.receivers, h.worker) {
		select {
		case err := <-done:
			// A worker or receiver the drain beat to registration is refused
			// for good: the stop this drain asked for.
			if !stoppedByOwnDrain(err) {
				h.t.Fatalf("runtime returned %v after drain", err)
			}
		case <-time.After(5 * time.Second):
			h.t.Fatal("runtime did not stop after drain")
		}
	}
}

// newBot builds the dispatcher over client: a mention starts watching and
// is answered; a watched follow-up is echoed, and "unwatch" stops it.
func (h *botHost) newBot(client uma.Client) *bot.Bot {
	h.t.Helper()
	reply := func(ctx context.Context, t *bot.Turn, s string) error {
		a, err := t.Reply(ctx, model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}})
		h.mu.Lock()
		h.replayed = append(h.replayed, a.Replayed)
		h.ops = append(h.ops, a.Operation.Ref)
		h.mu.Unlock()
		return err
	}
	b, err := bot.New(bot.Config{
		Client: client, Self: botActor.Actor, Subscription: h.sub, Watches: h.watches,
		IsMention: func(m model.Message) bool { return strings.Contains(bot.Text(m.Content), "@helper") },
		Mention: func(ctx context.Context, t *bot.Turn) error {
			if err := t.Watch(ctx); err != nil {
				return err
			}
			return reply(ctx, t, "watching")
		},
		Watched: func(ctx context.Context, t *bot.Turn) error {
			body := bot.Text(t.Message.Content)
			if body == "unwatch" {
				if err := t.Unwatch(ctx); err != nil {
					return err
				}
				return reply(ctx, t, "bye")
			}
			return reply(ctx, t, "re: "+body)
		},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return b
}

// say pushes one inbound message from the shared remote user in the shared
// remote room of channel ch and waits until uma committed it.
func (h *botHost) say(ch model.ChannelRef, text string) {
	h.t.Helper()
	inbox := botInboxA
	if ch == botChanB {
		inbox = botInboxB
	}
	before := len(h.inbound(inbox))
	h.n++
	id := strconv.Itoa(h.n) + "-" + text
	h.factory.Script(ch).Push(model.Ingress{Call: model.Call{Correlation: "rx"}, Envelope: model.Envelope{
		EventID: "ev-" + id, MessageID: "m-" + id, Destination: "rooms", Kind: model.EnvelopeMessage,
		Sender:     model.ExternalRef{Channel: ch, Namespace: fakeprovider.Namespace, Kind: "user", ID: "u1"},
		Assurance:  "verified",
		References: []model.ExternalRef{{Channel: ch, Namespace: fakeprovider.Namespace, Kind: model.RefKindRoom, ID: "room-1"}},
		Content:    model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
		Audience:   model.Audience{Visibility: model.Public}, OccurredAt: time.Unix(1_700_000_000, 0).UTC(),
	}})
	inEventually(h.t, "message committed", func() bool { return len(h.inbound(inbox)) == before+1 })
}

// inbound returns the provider messages of inbox, as the bot reads them.
func (h *botHost) inbound(inbox model.InboxRef) []model.Message {
	h.t.Helper()
	page, err := h.client(botActor).Messages().List(h.ctx, model.ListRequest{
		Call: model.Call{Correlation: "bot-list"}, Filter: model.Filter{Inboxes: []model.InboxRef{inbox}},
		Page: model.PageRequest{Limit: uma.MaxPageLimit},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	var out []model.Message
	for _, m := range page.Items {
		if m.Origin == nil {
			out = append(out, m)
		}
	}
	return out
}

// step runs b until it handled the next delivered batch.
func (h *botHost) step(b *bot.Bot) (bot.Report, error) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := b.Step(h.ctx)
		if err != nil || len(r.Results) > 0 {
			return r, err
		}
		if time.Now().After(deadline) {
			h.t.Fatal("no batch delivered")
		}
		time.Sleep(time.Millisecond)
	}
}

func (h *botHost) handled(b *bot.Bot, want bot.Outcome, route bot.Route) {
	h.t.Helper()
	r, err := h.step(b)
	if err != nil {
		h.t.Fatalf("Step: %v", err)
	}
	if len(r.Results) != 1 || r.Results[0].Outcome != want || r.Results[0].Route != route || !r.Acked {
		h.t.Fatalf("report = %+v, want one %s event on route %q, acknowledged", r, want, route)
	}
}

// sent returns the texts the provider account behind ch received.
func (h *botHost) sent(ch model.ChannelRef) []string {
	var out []string
	for _, c := range h.factory.Script(ch).Calls() {
		if c.Method == fakeprovider.MethodSend && c.Applied {
			if c.Effect.Route.ID != "room-1" || c.Effect.Route.Channel != ch {
				h.t.Errorf("send on route %+v", c.Effect.Route)
			}
			out = append(out, bot.Text(c.Content))
		}
	}
	return out
}

func (h *botHost) delivered(ch model.ChannelRef, want ...string) {
	h.t.Helper()
	inEventually(h.t, "replies delivered", func() bool { return len(h.sent(ch)) >= len(want) })
	if got := h.sent(ch); strings.Join(got, "|") != strings.Join(want, "|") {
		h.t.Fatalf("channel %s received %q, want %q", ch.ID, got, want)
	}
}

// diesBeforeReply fails every reply, as a process killed inside its
// handler.
type diesBeforeReply struct{ uma.Client }

func (c diesBeforeReply) Messages() uma.Messages { return dyingMessages{c.Client.Messages()} }

type dyingMessages struct{ uma.Messages }

func (dyingMessages) Reply(context.Context, model.Reply) (model.Acceptance, error) {
	return model.Acceptance{}, errBotDied
}

// diesBeforeAck fails every acknowledgment, as a process killed between
// its handlers and its ACK.
type diesBeforeAck struct{ uma.Client }

func (c diesBeforeAck) Events() uma.Events { return dyingEvents{c.Client.Events()} }

type dyingEvents struct{ uma.Events }

func (dyingEvents) Ack(context.Context, model.Acknowledge) (model.SubscriptionResult, error) {
	return model.SubscriptionResult{}, errBotDied
}

func TestBotMentionFollowUpRestartUnwatch(t *testing.T) {
	h := newBotHost(t)
	h.start()
	b := h.newBot(h.client(botActor))

	// Not addressed and not watched: retained, not answered.
	h.say(botChanA, "anyone here?")
	h.handled(b, bot.Ignored, "")

	// Mention: the bot starts watching and answers through Runtime.Run.
	h.say(botChanA, "@helper hi")
	h.handled(b, bot.Handled, bot.RouteMention)
	h.delivered(botChanA, "watching")

	// Follow-up without mention reaches the watched handler.
	h.say(botChanA, "how are you")
	h.handled(b, bot.Handled, bot.RouteWatched)
	h.delivered(botChanA, "watching", "re: how are you")

	// Same remote room and user on the other provider account: a different
	// conversation the bot does not watch.
	h.say(botChanB, "how are you")
	h.handled(b, bot.Ignored, "")

	// The bot dies inside its handler, before its reply was accepted: the
	// batch stays unacknowledged and the next bot answers it.
	h.say(botChanA, "are you ok")
	r, err := h.step(h.newBot(diesBeforeReply{h.client(botActor)}))
	if err != nil || len(r.Results) != 1 || r.Results[0].Outcome != bot.Retried || r.Acked {
		t.Fatalf("bot dying in its handler: report %+v, err %v", r, err)
	}
	h.handled(b, bot.Handled, bot.RouteWatched)
	h.delivered(botChanA, "watching", "re: how are you", "re: are you ok")

	// The bot replies, then dies before its ACK.
	h.say(botChanA, "still there?")
	r, err = h.step(h.newBot(diesBeforeAck{h.client(botActor)}))
	if !errors.Is(err, errBotDied) || len(r.Results) != 1 || r.Results[0].Outcome != bot.Handled {
		t.Fatalf("dying bot: report %+v, err %v", r, err)
	}
	h.delivered(botChanA, "watching", "re: how are you", "re: are you ok", "re: still there?")

	// A restarted bot gets the same batch again, reruns the handler under
	// the same operation IDs and uma replays the reply: no second send.
	b = h.newBot(h.client(botActor))
	h.handled(b, bot.Handled, bot.RouteWatched)
	h.mu.Lock()
	last := h.replayed[len(h.replayed)-1]
	h.mu.Unlock()
	if !last {
		t.Fatal("redelivered reply was not a replay")
	}

	// The watch outlived the restart; unwatch is answered, then replies stop.
	h.say(botChanA, "unwatch")
	h.handled(b, bot.Handled, bot.RouteWatched)
	h.say(botChanA, "hello?")
	h.handled(b, bot.Ignored, "")

	// One handler serves the other provider account too.
	h.say(botChanB, "@helper hi")
	h.handled(b, bot.Handled, bot.RouteMention)

	want := []string{"watching", "re: how are you", "re: are you ok", "re: still there?", "bye"}
	h.delivered(botChanA, want...)
	h.delivered(botChanB, "watching")
	h.drain()
	h.delivered(botChanA, want...) // still exactly once each after the worker stopped
	h.delivered(botChanB, "watching")

	// Unwatching stopped replies only: every message is still retained.
	if a, bb := len(h.inbound(botInboxA)), len(h.inbound(botInboxB)); a != 7 || bb != 2 {
		t.Fatalf("retained messages: %d in A, %d in B; want 7 and 2", a, bb)
	}
}

// contended is the host's store with contention injected by unit kind: an
// armed unit loses to a unit of the same tenant committed between its work
// and its commit, as one of another worker would, and fails with
// model.Transient.
type contended struct {
	*memory.Store
	mu      sync.Mutex
	lose    map[string]int
	lost    map[string]int
	fillers int
}

// Unit kinds the contended store recognizes.
const (
	unitPoll  = model.ActionSubscriptionPoll
	unitReply = model.ActionMessageReply
	unitAck   = model.ActionSubscriptionAck
)

// arm makes the next n units of kind lose.
func (s *contended) arm(kind string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lose[kind] = n
}

func (s *contended) losses(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lost[kind]
}

func (s *contended) take(kind string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind == "" || s.lose[kind] == 0 {
		return 0, false
	}
	s.lose[kind]--
	s.lost[kind]++
	s.fillers++
	return s.fillers, true
}

func (s *contended) Transact(ctx context.Context, tenant model.TenantID, fn func(store.Tx) error) error {
	return s.Store.Transact(ctx, tenant, func(tx store.Tx) error {
		w := &unitTx{Tx: tx}
		if err := fn(w); err != nil {
			return err
		}
		n, ok := s.take(w.kind())
		if !ok {
			return nil
		}
		return s.Store.Transact(ctx, tenant, func(other store.Tx) error {
			return other.PutChannel(ctx, model.Channel{
				Ref:  model.ChannelRef{Tenant: tenant, ID: model.ID(fmt.Sprintf("00000000-0000-7000-8000-%012x", 0xf00+n))},
				Type: fakeprovider.TypeFull, Configuration: model.Extension{Name: string(fakeprovider.TypeFull) + ".config", Version: 1},
				Revision: "filler",
			}, "")
		})
	})
}

// unitTx records what a unit does, to name its kind.
type unitTx struct {
	store.Tx
	claimed string
	leased  bool
}

func (t *unitTx) ClaimOperation(ctx context.Context, r store.OperationRecord) (store.OperationRecord, bool, error) {
	t.claimed = r.Key.Kind
	return t.Tx.ClaimOperation(ctx, r)
}

func (t *unitTx) PutSubscription(ctx context.Context, r store.SubscriptionRecord, expected model.Revision) error {
	t.leased = true
	return t.Tx.PutSubscription(ctx, r, expected)
}

func (t *unitTx) kind() string {
	switch {
	case t.claimed != "":
		return t.claimed
	case t.leased:
		return unitPoll
	}
	return ""
}

// A poll, a reply and an acknowledgment that each lose to concurrent units
// of the tenant are resubmitted: the event is answered once and its batch
// acknowledged, without a Step failure.
func TestBotSurvivesConcurrentUnits(t *testing.T) {
	h := newBotHost(t)
	h.start()
	b := h.newBot(h.client(botActor))
	h.say(botChanA, "@helper hi")
	for _, kind := range []string{unitPoll, unitReply, unitAck} {
		h.units.arm(kind, 3) // as many as the engine reruns a unit itself
	}
	h.handled(b, bot.Handled, bot.RouteMention)
	for _, kind := range []string{unitPoll, unitReply, unitAck} {
		if n := h.units.losses(kind); n != 3 {
			t.Errorf("%s units lost %d times, want 3", kind, n)
		}
	}
	h.delivered(botChanA, "watching")
	h.drain()
	h.delivered(botChanA, "watching")
}

// One provider message reported under two event IDs, as a webhook and a
// poll both report it, reaches the bot once: one message, one
// message.received, one reply.
func TestBotAnswersAMessageReportedTwiceOnce(t *testing.T) {
	h := newBotHost(t)
	h.start()
	b := h.newBot(h.client(botActor))
	report := func(event string) model.Ingress {
		return model.Ingress{Call: model.Call{Correlation: "rx"}, Envelope: model.Envelope{
			EventID: event, MessageID: "m-twice", Destination: "rooms", Kind: model.EnvelopeMessage,
			Sender:     model.ExternalRef{Channel: botChanA, Namespace: fakeprovider.Namespace, Kind: "user", ID: "u1"},
			Assurance:  "verified",
			References: []model.ExternalRef{{Channel: botChanA, Namespace: fakeprovider.Namespace, Kind: model.RefKindRoom, ID: "room-1"}},
			Content:    model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "@helper hi"}}},
			Audience:   model.Audience{Visibility: model.Public}, OccurredAt: time.Unix(1_700_000_000, 0).UTC(),
		}}
	}
	h.factory.Script(botChanA).Push(report("webhook-1"), report("poll-1"))
	inEventually(t, "both reports acknowledged", func() bool {
		n := 0
		for _, d := range h.factory.Script(botChanA).Deliveries() {
			if d.Acked {
				n++
			}
		}
		return n == 2
	})
	h.handled(b, bot.Handled, bot.RouteMention)
	h.delivered(botChanA, "watching")
	if r, err := b.Step(h.ctx); err != nil || len(r.Results) != 0 {
		t.Fatalf("after the answer: report %+v, err %v; want nothing more for the bot", r, err)
	}
	if n := len(h.inbound(botInboxA)); n != 1 {
		t.Fatalf("inbound messages = %d, want 1", n)
	}
	h.drain()
	h.delivered(botChanA, "watching")
}
