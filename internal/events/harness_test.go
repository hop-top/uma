package events_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// Event tests append committed events straight to the memory store and drive
// Events through a client bound to the bot principal.

const (
	botActor   model.ActorID = "bot-1"
	otherActor model.ActorID = "bot-2"
)

var evT0 = time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)

// gatePolicy allows everything except the resources and actions it denies.
type gatePolicy struct {
	mu       sync.Mutex
	denied   map[model.Ref]bool
	actions  map[string]bool
	fail     error
	requests []policy.Authorization
}

func (g *gatePolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, a)
	if g.fail != nil {
		return policy.Decision{}, g.fail
	}
	if g.denied[a.Resource] || g.actions[a.Action] {
		return policy.Decision{}, nil
	}
	return policy.Decision{Allow: true}, nil
}

func (g *gatePolicy) deny(r model.Ref) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.denied[r] = true
}

func (g *gatePolicy) denyAction(a string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.actions[a] = true
}

func (g *gatePolicy) failWith(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fail = err
}

// countingWaiter records waits and runs a hook on each.
type countingWaiter struct {
	calls atomic.Int32
	hook  func()
}

func (w *countingWaiter) Wait(ctx context.Context, _ time.Duration) error {
	w.calls.Add(1)
	if w.hook != nil {
		w.hook()
	}
	return ctx.Err()
}

type evHarness struct {
	t      *testing.T
	ctx    context.Context
	clock  *portstest.FixedClock
	mem    *memory.Store
	gate   *gatePolicy
	waiter *countingWaiter
	e      *engine.Engine
	bot    enginetest.Client
	seq    int
	ops    int
}

func newEvHarness(t *testing.T, wrap func(store.Store) store.Store, opts ...func(*engine.Config)) *evHarness {
	t.Helper()
	clock := portstest.NewFixedClock(evT0)
	mem := memory.New(memory.Options{Clock: clock})
	var st store.Store = mem
	if wrap != nil {
		st = wrap(mem)
	}
	gate := &gatePolicy{denied: map[model.Ref]bool{}, actions: map[string]bool{}}
	waiter := &countingWaiter{}
	cfg := engine.Config{
		Store: st, Authorizer: gate, Clock: clock, IDs: portstest.NewSequenceIDs(7), Waiter: waiter,
	}
	for _, o := range opts {
		o(&cfg)
	}
	e := enginetest.MustNew(t, cfg)
	h := &evHarness{t: t, ctx: t.Context(), clock: clock, mem: mem, gate: gate, waiter: waiter, e: e}
	h.bot = h.client(botActor)
	return h
}

func (h *evHarness) client(a model.ActorID) enginetest.Client {
	h.t.Helper()
	c, err := enginetest.For(h.e, model.Principal{Tenant: enginetest.TenantA, Actor: a})
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *evHarness) inbox(id model.ID) model.InboxRef {
	return model.InboxRef{Tenant: enginetest.TenantA, ID: id}
}

// message appends one message.received event for a new message in inbox and
// returns it.
func (h *evHarness) message(inbox model.ID) model.Event {
	h.t.Helper()
	ev := h.newMessage(inbox)
	h.append(ev)
	return ev
}

// newMessage returns a message.received event for a new message in inbox.
func (h *evHarness) newMessage(inbox model.ID) model.Event {
	h.t.Helper()
	h.seq++
	conv := model.ConversationRef{Inbox: h.inbox(inbox), ID: seqID(0xc0, h.seq)}
	msg := model.MessageRef{Conversation: conv, ID: seqID(0xa0, h.seq)}
	data, err := json.Marshal(model.MessageReceivedPayload{Message: msg, Trigger: model.TriggerProvider, Source: enginetest.ChatType})
	if err != nil {
		h.t.Fatal(err)
	}
	ev := model.Event{
		Version: model.EventEnvelopeVersion, ID: seqID(0xe0, h.seq), Kind: model.EventMessageReceived,
		Tenant: enginetest.TenantA, Inbox: inbox, Resource: msg.Ref(), Actor: "ingress",
		OccurredAt: evT0, RecordedAt: evT0, Correlation: "corr", Audience: model.Audience{Visibility: model.Public},
		Revision: "r1",
		Payload:  model.Extension{Name: model.SchemaMessageReceived, Version: model.PayloadVersion1, Data: data},
	}
	return ev
}

func (h *evHarness) append(evs ...model.Event) {
	h.t.Helper()
	err := h.mem.Transact(h.ctx, enginetest.TenantA, func(tx store.Tx) error { return tx.AppendEvents(h.ctx, evs) })
	if err != nil {
		h.t.Fatal(err)
	}
}

func seqID(prefix byte, n int) model.ID {
	return model.ID(fmt.Sprintf("%02x%06x-0000-7000-8000-%012x", prefix, n, n))
}

func (h *evHarness) op(prefix string) model.Command {
	h.ops++
	return model.Command{
		Call:      model.Call{Correlation: model.CorrelationID("corr-" + prefix)},
		Operation: model.OperationID(fmt.Sprintf("%s-%d", prefix, h.ops)),
	}
}

func (h *evHarness) openReq(consumer, start string, f model.Filter) model.OpenSubscription {
	return model.OpenSubscription{
		Command: h.op("open"), Consumer: consumer, Filter: f, Start: start, PayloadMajor: model.PayloadVersion1,
	}
}

func (h *evHarness) open(c enginetest.Client, req model.OpenSubscription) model.SubscriptionResult {
	h.t.Helper()
	r, err := c.Events().Open(h.ctx, req)
	if err != nil {
		h.t.Fatalf("Open: %v", err)
	}
	return r
}

func (h *evHarness) openFrom(start string, f model.Filter) model.SubscriptionRef {
	h.t.Helper()
	return h.open(h.bot, h.openReq("bot", start, f)).Subscription.Ref
}

func (h *evHarness) pollReq(sub model.SubscriptionRef, limit int) model.Poll {
	return model.Poll{Call: model.Call{Correlation: "poll"}, Subscription: sub, Limit: limit}
}

func (h *evHarness) poll(sub model.SubscriptionRef, limit int) model.EventBatch {
	h.t.Helper()
	b, err := h.bot.Events().Poll(h.ctx, h.pollReq(sub, limit))
	if err != nil {
		h.t.Fatalf("Poll: %v", err)
	}
	return b
}

func (h *evHarness) ackReq(sub model.SubscriptionRef, token string) model.Acknowledge {
	return model.Acknowledge{Command: h.op("ack"), Subscription: sub, Token: token}
}

func (h *evHarness) ack(sub model.SubscriptionRef, token string) model.SubscriptionResult {
	h.t.Helper()
	r, err := h.bot.Events().Ack(h.ctx, h.ackReq(sub, token))
	if err != nil {
		h.t.Fatalf("Ack: %v", err)
	}
	return r
}

func (h *evHarness) record(sub model.SubscriptionRef) store.SubscriptionRecord {
	h.t.Helper()
	var rec store.SubscriptionRecord
	err := h.mem.View(h.ctx, enginetest.TenantA, func(r store.Reader) error {
		var err error
		rec, err = r.Subscription(h.ctx, sub)
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return rec
}

func eventIDs(evs []model.Event) []model.ID {
	out := make([]model.ID, len(evs))
	for i, e := range evs {
		out[i] = e.ID
	}
	return out
}

func requireIDs(t *testing.T, got []model.Event, want ...model.Event) {
	t.Helper()
	g, w := eventIDs(got), eventIDs(want)
	if fmt.Sprint(g) != fmt.Sprint(w) {
		t.Fatalf("batch events = %v, want %v", g, w)
	}
}
