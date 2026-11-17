package events_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/events"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

func TestOpenStartModes(t *testing.T) {
	h := newEvHarness(t, nil)
	e1 := h.message(enginetest.InboxOne)
	e2 := h.message(enginetest.InboxOne)

	beginning := h.open(h.bot, h.openReq("from-start", model.StartBeginning, model.Filter{}))
	if beginning.Subscription.Checkpoint != "" || beginning.Subscription.Ref.Tenant != enginetest.TenantA ||
		beginning.Subscription.Revision == "" || beginning.Acceptance.Replayed {
		t.Fatalf("beginning = %+v", beginning)
	}
	if beginning.Acceptance.Operation.State != model.Committed {
		t.Fatalf("open operation state = %s, want committed", beginning.Acceptance.Operation.State)
	}
	requireIDs(t, h.poll(beginning.Subscription.Ref, 10).Events, e1, e2)

	tail := h.open(h.bot, h.openReq("from-tail", model.StartTail, model.Filter{}))
	if tail.Subscription.Checkpoint == "" {
		t.Fatal("tail subscription starts at the retained beginning")
	}
	if b := h.poll(tail.Subscription.Ref, 10); len(b.Events) != 0 {
		t.Fatalf("tail poll = %v, want nothing before new events", eventIDs(b.Events))
	}
	e3 := h.message(enginetest.InboxOne)
	requireIDs(t, h.poll(tail.Subscription.Ref, 10).Events, e3)

	source := h.openFrom(model.StartBeginning, model.Filter{Inboxes: []model.InboxRef{h.inbox(enginetest.InboxOne)}})
	first := h.poll(source, 1)
	requireIDs(t, first.Events, e1)
	req := h.openReq("resume", model.StartCheckpoint, model.Filter{})
	req.After = first.ScannedThrough
	resumed := h.open(h.bot, req)
	if resumed.Subscription.Checkpoint != first.ScannedThrough {
		t.Fatalf("checkpoint = %q, want %q", resumed.Subscription.Checkpoint, first.ScannedThrough)
	}
	requireIDs(t, h.poll(resumed.Subscription.Ref, 10).Events, e2, e3)

	req = h.openReq("beyond", model.StartCheckpoint, model.Filter{})
	req.After = "not-a-cursor"
	_, err := h.bot.Events().Open(h.ctx, req)
	enginetest.RequireCode(t, err, model.Validation, "cursor")
}

func TestOpenRejectsUnsupportedPayloadMajor(t *testing.T) {
	h := newEvHarness(t, nil)
	req := h.openReq("bot", model.StartTail, model.Filter{})
	req.PayloadMajor = 2
	_, err := h.bot.Events().Open(h.ctx, req)
	enginetest.RequireCode(t, err, model.SchemaMismatch, "payload major")
	req.PayloadMajor = 0
	_, err = h.bot.Events().Open(h.ctx, req)
	enginetest.RequireCode(t, err, model.Validation, "payload_major")
}

func TestOpenIdentityIsConsumerFilterActor(t *testing.T) {
	h := newEvHarness(t, nil)
	filter := model.Filter{Inboxes: []model.InboxRef{h.inbox(enginetest.InboxOne)}, Kinds: []string{model.EventMessageReceived}}
	first := h.open(h.bot, h.openReq("bot", model.StartTail, filter))

	// Replay with the same operation ID returns the stored result.
	again, err := h.bot.Events().Open(h.ctx, model.OpenSubscription{
		Command:  model.Command{Call: model.Call{Correlation: "x"}, Operation: model.OperationID("open-1")},
		Consumer: "bot", Filter: filter, Start: model.StartTail, PayloadMajor: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Acceptance.Replayed || again.Subscription.Ref != first.Subscription.Ref ||
		again.Acceptance.Operation.Ref != first.Acceptance.Operation.Ref {
		t.Fatalf("replay = %+v, want the first result replayed", again)
	}
	if k := again.Acceptance.Operation.Key; k == nil || k.Kind != model.ActionSubscriptionOpen || k.ID != "open-1" || k.Caller == "" {
		t.Fatalf("replayed acceptance key = %+v, want subscription.open, open-1", k)
	}
	// A different payload under the same operation ID conflicts.
	_, err = h.bot.Events().Open(h.ctx, model.OpenSubscription{
		Command:  model.Command{Call: model.Call{Correlation: "x"}, Operation: model.OperationID("open-1")},
		Consumer: "bot", Filter: filter, Start: model.StartBeginning, PayloadMajor: 1,
	})
	enginetest.RequireCode(t, err, model.Conflict, "operation ID")

	// A new operation for the same consumer and filter returns the same
	// subscription without replacing its position.
	h.message(enginetest.InboxOne)
	same := h.open(h.bot, h.openReq("bot", model.StartBeginning, filter))
	if same.Subscription.Ref != first.Subscription.Ref || same.Acceptance.Replayed ||
		same.Subscription.Checkpoint != first.Subscription.Checkpoint {
		t.Fatalf("second open = %+v, want the existing subscription %+v", same, first)
	}

	// A changed filter under the same consumer name is a new subscription,
	// which needs a new name.
	_, err = h.bot.Events().Open(h.ctx, h.openReq("bot", model.StartTail, model.Filter{}))
	enginetest.RequireCode(t, err, model.Conflict, "filter")

	// Another actor with the same consumer name gets its own subscription
	// and cannot see this one.
	other := h.client(otherActor)
	theirs, err := other.Events().Open(h.ctx, h.openReq("bot", model.StartTail, filter))
	if err != nil {
		t.Fatal(err)
	}
	if theirs.Subscription.Ref == first.Subscription.Ref {
		t.Fatal("subscriptions of different actors share a reference")
	}
	_, err = other.Events().Poll(h.ctx, h.pollReq(first.Subscription.Ref, 1))
	enginetest.RequireCode(t, err, model.NotFound, "")
	_, err = other.Events().Ack(h.ctx, h.ackReq(first.Subscription.Ref, "t"))
	enginetest.RequireCode(t, err, model.NotFound, "")
}

func TestOpenValidatesFilterScope(t *testing.T) {
	h := newEvHarness(t, nil)
	conv := model.ConversationRef{Inbox: h.inbox(enginetest.InboxOne), ID: enginetest.ConvOne}
	_, err := h.bot.Events().Open(h.ctx, h.openReq("bot", model.StartTail, model.Filter{Conversation: &conv}))
	enginetest.RequireCode(t, err, model.Validation, "filter.conversation")
	who := model.IdentityRef{Inbox: conv.Inbox, ID: enginetest.ConvOne}
	_, err = h.bot.Events().Open(h.ctx, h.openReq("bot", model.StartTail, model.Filter{Identity: &who}))
	enginetest.RequireCode(t, err, model.Validation, "filter.identity")
	labels := &model.LabelSelector{Any: []model.LabelRef{{Tenant: enginetest.TenantA, ID: enginetest.ConvOne}}}
	_, err = h.bot.Events().Open(h.ctx, h.openReq("bot", model.StartTail, model.Filter{Labels: labels}))
	enginetest.RequireCode(t, err, model.Validation, "filter.labels: not supported by subscriptions")
	tk := model.TaskRef{Conversation: conv, ID: enginetest.ConvOne}
	_, err = h.bot.Events().Open(h.ctx, h.openReq("bot", model.StartTail, model.Filter{Task: &tk}))
	enginetest.RequireCode(t, err, model.Validation, "filter.task")
	if e := (*model.Error)(nil); !errors.As(err, &e) || e.SafeMessage != "filter.task: not supported by subscriptions" {
		t.Fatalf("task filter refused by %v, want the subscription check", err)
	}
	_, err = h.bot.Events().Open(h.ctx, h.openReq("bot", model.StartTail, model.Filter{Kinds: []string{"bogus"}}))
	enginetest.RequireCode(t, err, model.Validation, "filter.kinds[0]")
	_, err = h.bot.Events().Open(h.ctx, h.openReq("bot", model.StartTail,
		model.Filter{Inboxes: []model.InboxRef{{Tenant: enginetest.TenantB, ID: enginetest.InboxOne}}}))
	enginetest.RequireCode(t, err, model.NotFound, "")
	h.gate.denyAction(model.ActionSubscriptionOpen)
	_, err = h.bot.Events().Open(h.ctx, h.openReq("bot", model.StartTail, model.Filter{}))
	enginetest.RequireCode(t, err, model.Unauthorized, "")
}

// Kind filters name event kinds the engine can emit: builtin kinds and
// namespaced kinds with a registered event-kind definition.
func TestOpenChecksKindsAgainstRegistry(t *testing.T) {
	const pinned = "com.example.chat.messages.pinned"
	h := newEvHarness(t, nil, func(c *engine.Config) {
		c.Extensions = []adapter.ExtensionDefinition{{
			Name: pinned, Version: 1, RequiredCapability: "com.example.chat.messages.pin",
			AuthorizeAs: model.ActionMessageRead, Kind: model.ExtensionKindEvent,
		}}
	})
	h.open(h.bot, h.openReq("known", model.StartTail,
		model.Filter{Kinds: []string{model.EventMessageReceived, pinned}}))
	for _, k := range []string{"com.example.chat.messages.unpinned", "message.sent", "bogus"} {
		_, err := h.bot.Events().Open(h.ctx, h.openReq("unknown-"+k, model.StartTail,
			model.Filter{Kinds: []string{model.EventMessageReceived, k}}))
		enginetest.RequireCode(t, err, model.Validation, "filter.kinds[1]")
	}
}

func TestPollAckAdvancesCheckpoint(t *testing.T) {
	h := newEvHarness(t, nil)
	e1, e2, e3 := h.message(enginetest.InboxOne), h.message(enginetest.InboxOne), h.message(enginetest.InboxOne)
	sub := h.openFrom(model.StartBeginning, model.Filter{})

	b := h.poll(sub, 2)
	requireIDs(t, b.Events, e1, e2)
	if b.AckToken == "" || b.ScannedThrough == "" || !b.ExpiresAt.Equal(evT0.Add(events.AckLease)) {
		t.Fatalf("batch = %+v", b)
	}
	if rec := h.record(sub); rec.Subscription.Checkpoint != "" || rec.PendingBatch == nil {
		t.Fatalf("poll moved the checkpoint or bound no batch: %+v", rec)
	}
	for _, ev := range b.Events {
		if ev.Payload.Name != model.SchemaMessageReceived || ev.Payload.Version != model.PayloadVersion1 ||
			ev.Audience.Visibility == "" || ev.Correlation == "" {
			t.Fatalf("envelope %+v", ev)
		}
	}

	r := h.ack(sub, b.AckToken)
	if r.Subscription.Checkpoint != b.ScannedThrough || r.Acceptance.Replayed ||
		r.Acceptance.Operation.State != model.Committed {
		t.Fatalf("ack = %+v", r)
	}
	if rec := h.record(sub); rec.PendingBatch != nil {
		t.Fatal("ack left the batch pending")
	}
	requireIDs(t, h.poll(sub, 10).Events, e3)
}

func TestRePollReturnsOutstandingBatch(t *testing.T) {
	h := newEvHarness(t, nil)
	e1, e2 := h.message(enginetest.InboxOne), h.message(enginetest.InboxOne)
	sub := h.openFrom(model.StartBeginning, model.Filter{})
	first := h.poll(sub, 10)
	h.message(enginetest.InboxOne)
	h.clock.Advance(events.AckLease / 2)
	again := h.poll(sub, 10)
	requireIDs(t, again.Events, e1, e2)
	if again.AckToken != first.AckToken || again.ScannedThrough != first.ScannedThrough ||
		!again.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("re-poll = %+v, want the outstanding batch %+v", again, first)
	}
	h.ack(sub, again.AckToken)
}

func TestLeaseExpiryRedeliversAndFencesStaleAck(t *testing.T) {
	h := newEvHarness(t, nil)
	e1, e2 := h.message(enginetest.InboxOne), h.message(enginetest.InboxOne)
	sub := h.openFrom(model.StartBeginning, model.Filter{})
	first := h.poll(sub, 10)
	h.clock.Advance(events.AckLease)

	// The stale token can no longer acknowledge, before and after
	// redelivery.
	_, err := h.bot.Events().Ack(h.ctx, h.ackReq(sub, first.AckToken))
	enginetest.RequireCode(t, err, model.Conflict, "expired")
	if rec := h.record(sub); rec.Subscription.Checkpoint != "" {
		t.Fatal("stale ack moved the checkpoint")
	}

	second := h.poll(sub, 10)
	requireIDs(t, second.Events, e1, e2)
	if second.AckToken == first.AckToken || !second.ExpiresAt.After(first.ExpiresAt) {
		t.Fatalf("redelivery reused the lease: %+v", second)
	}
	_, err = h.bot.Events().Ack(h.ctx, h.ackReq(sub, first.AckToken))
	enginetest.RequireCode(t, err, model.Conflict, "token")
	r := h.ack(sub, second.AckToken)
	if r.Subscription.Checkpoint != second.ScannedThrough {
		t.Fatalf("checkpoint = %q", r.Subscription.Checkpoint)
	}

	// Acknowledging twice: replay returns the stored result, a new
	// operation with the spent token conflicts.
	_, err = h.bot.Events().Ack(h.ctx, h.ackReq(sub, second.AckToken))
	enginetest.RequireCode(t, err, model.Conflict, "token")
	_, err = h.bot.Events().Ack(h.ctx, h.ackReq(sub, "garbage"))
	enginetest.RequireCode(t, err, model.Conflict, "token")
}

func TestAckReplaysByOperationID(t *testing.T) {
	h := newEvHarness(t, nil)
	h.message(enginetest.InboxOne)
	sub := h.openFrom(model.StartBeginning, model.Filter{})
	b := h.poll(sub, 10)
	req := h.ackReq(sub, b.AckToken)
	first, err := h.bot.Events().Ack(h.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.bot.Events().Ack(h.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Acceptance.Replayed || again.Acceptance.Operation.Ref != first.Acceptance.Operation.Ref ||
		again.Subscription.Checkpoint != first.Subscription.Checkpoint {
		t.Fatalf("replay = %+v, want %+v replayed", again, first)
	}
	req.Token = "other"
	_, err = h.bot.Events().Ack(h.ctx, req)
	enginetest.RequireCode(t, err, model.Conflict, "operation ID")
}

func TestConcurrentPollersShareOneLease(t *testing.T) {
	h := newEvHarness(t, nil)
	e1, e2 := h.message(enginetest.InboxOne), h.message(enginetest.InboxOne)
	sub := h.openFrom(model.StartBeginning, model.Filter{})
	const n = 6
	var wg sync.WaitGroup
	batches := make([]model.EventBatch, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				b, err := h.bot.Events().Poll(h.ctx, h.pollReq(sub, 10))
				if model.IsCode(err, model.Transient) {
					continue
				}
				batches[i], errs[i] = b, err
				return
			}
			errs[i] = errors.New("poller gave up after transient failures")
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("poller %d: %v", i, errs[i])
		}
		requireIDs(t, batches[i].Events, e1, e2)
		if batches[i].AckToken != batches[0].AckToken {
			t.Fatalf("poller %d holds a different token", i)
		}
	}
	if rec := h.record(sub); rec.PendingBatch == nil || rec.PendingBatch.Fence.Generation != 1 {
		t.Fatalf("pending batch = %+v, want one lease generation", rec.PendingBatch)
	}
	// Every poller holds the same token; the first Ack wins and the others
	// learn by conflict that the batch was handled.
	h.ack(sub, batches[0].AckToken)
	for i := 1; i < n; i++ {
		_, err := h.bot.Events().Ack(h.ctx, h.ackReq(sub, batches[i].AckToken))
		enginetest.RequireCode(t, err, model.Conflict, "token")
	}
}

func TestPollFiltersByInboxKindAndAuthorization(t *testing.T) {
	h := newEvHarness(t, nil)
	e1 := h.message(enginetest.InboxOne)
	h.message(enginetest.InboxTwo)
	e3 := h.message(enginetest.InboxOne)
	updated := e3
	updated.ID, updated.Kind, updated.Payload.Name = seqID(0xf0, 1), model.EventMessageUpdated, model.SchemaMessageUpdated
	h.append(updated)
	e5 := h.message(enginetest.InboxOne)
	sub := h.openFrom(model.StartBeginning, model.Filter{
		Inboxes: []model.InboxRef{h.inbox(enginetest.InboxOne)}, Kinds: []string{model.EventMessageReceived},
	})
	requireIDs(t, h.poll(sub, 10).Events, e1, e3, e5)
	if len(h.gate.requests) == 0 {
		t.Fatal("no authorization requested")
	}
	for _, a := range h.gate.requests {
		if a.Principal.Actor != botActor {
			t.Fatalf("authorized as %q", a.Principal.Actor)
		}
	}
}

func TestPollSkipsForbiddenEventsWithoutExposure(t *testing.T) {
	h := newEvHarness(t, nil)
	e1, e2, e3 := h.message(enginetest.InboxOne), h.message(enginetest.InboxOne), h.message(enginetest.InboxTwo)
	h.gate.deny(e2.Resource)
	h.gate.deny(h.inbox(enginetest.InboxTwo).Ref())
	sub := h.openFrom(model.StartBeginning, model.Filter{})
	b := h.poll(sub, 10)
	requireIDs(t, b.Events, e1)
	if b.AckToken == "" {
		t.Fatal("filtered range carries no token")
	}
	// An ACK of the filtered range moves past the skipped events.
	r := h.ack(sub, b.AckToken)
	if r.Subscription.Checkpoint != b.ScannedThrough {
		t.Fatalf("checkpoint = %q, want %q", r.Subscription.Checkpoint, b.ScannedThrough)
	}
	if next := h.poll(sub, 10); len(next.Events) != 0 {
		t.Fatalf("skipped events redelivered: %v", eventIDs(next.Events))
	}

	// Access revoked between delivery and redelivery drops the event from
	// the outstanding batch; the range and token are unchanged.
	e4 := h.message(enginetest.InboxOne)
	first := h.poll(sub, 10)
	requireIDs(t, first.Events, e4)
	h.gate.deny(e4.Resource)
	again := h.poll(sub, 10)
	if len(again.Events) != 0 || again.AckToken != first.AckToken {
		t.Fatalf("redelivery after revocation = %+v", again)
	}
	_ = e3

	// An entirely empty scan carries no token.
	h.ack(sub, again.AckToken)
	if empty := h.poll(sub, 10); empty.AckToken != "" || empty.ScannedThrough != again.ScannedThrough {
		t.Fatalf("empty poll = %+v", empty)
	}

	h.gate.denyAction(model.ActionSubscriptionPoll)
	_, err := h.bot.Events().Poll(h.ctx, h.pollReq(sub, 10))
	enginetest.RequireCode(t, err, model.Unauthorized, "")
	h.gate.failWith(errors.New("policy down"))
	_, err = h.bot.Events().Poll(h.ctx, h.pollReq(sub, 10))
	enginetest.RequireCode(t, err, model.Unavailable, "")
}

// Interaction events reach a subscriber like any event about a readable
// resource: the subscriber needs interaction.read on the interaction, under
// the event's audience, and an event it may not read is skipped.
func TestPollDeliversInteractionEvents(t *testing.T) {
	h := newEvHarness(t, nil)
	h.seq++
	conv := model.ConversationRef{Inbox: h.inbox(enginetest.InboxOne), ID: seqID(0xc0, h.seq)}
	mk := func(n int, kind string) model.Event {
		ref := model.InteractionRef{Conversation: conv, ID: seqID(0xa5, n)}
		return model.Event{
			Version: model.EventEnvelopeVersion, ID: seqID(0xe5, n), Kind: kind,
			Tenant: enginetest.TenantA, Inbox: enginetest.InboxOne, Resource: ref.Ref(), Actor: "ingress",
			OccurredAt: evT0, RecordedAt: evT0, Correlation: "corr", Audience: model.Audience{Visibility: model.Public},
			Revision: "ix1", Payload: model.Extension{Name: model.SchemaInteractionReceived, Version: model.PayloadVersion1, Data: []byte(`{}`)},
		}
	}
	received, updated, hidden := mk(1, model.EventInteractionReceived), mk(2, model.EventInteractionUpdated), mk(3, model.EventInteractionReceived)
	h.append(received, updated, hidden)
	h.gate.deny(hidden.Resource)
	sub := h.openFrom(model.StartBeginning, model.Filter{Kinds: []string{model.EventInteractionReceived, model.EventInteractionUpdated}})
	requireIDs(t, h.poll(sub, 10).Events, received, updated)
	var asked bool
	for _, a := range h.gate.requests {
		asked = asked || a.Action == model.ActionInteractionRead && a.Resource == received.Resource
	}
	if !asked {
		t.Fatal("interaction.read was never authorized on the interaction")
	}
}

func TestPollWaitsForEvents(t *testing.T) {
	h := newEvHarness(t, nil)
	sub := h.openFrom(model.StartTail, model.Filter{})
	var ev model.Event
	h.waiter.hook = func() {
		if h.waiter.calls.Load() == 2 {
			ev = h.message(enginetest.InboxOne)
		}
		h.clock.Advance(events.PollStep)
	}
	req := h.pollReq(sub, 10)
	req.Wait = 5 * events.PollStep
	b, err := h.bot.Events().Poll(h.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	requireIDs(t, b.Events, ev)
	if h.waiter.calls.Load() != 2 {
		t.Fatalf("waits = %d, want 2", h.waiter.calls.Load())
	}
	h.ack(sub, b.AckToken)

	// The wait ends at the deadline with an empty batch and no token.
	h.waiter.calls.Store(0)
	h.waiter.hook = func() { h.clock.Advance(events.PollStep) }
	req.Wait = 2 * events.PollStep
	b, err = h.bot.Events().Poll(h.ctx, req)
	if err != nil || len(b.Events) != 0 || b.AckToken != "" {
		t.Fatalf("poll = %+v, %v", b, err)
	}
	if h.waiter.calls.Load() != 2 {
		t.Fatalf("waits = %d, want 2", h.waiter.calls.Load())
	}

	// Cancellation ends the wait with the context error.
	ctx, cancel := context.WithCancel(h.ctx)
	h.waiter.hook = cancel
	req.Wait = time.Hour
	_, err = h.bot.Events().Poll(ctx, req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// expiringStore reports CursorExpired for event reads at the given cursor.
type expiringStore struct {
	store.Store
	mu      sync.Mutex
	expired map[model.Cursor]bool
}

type expiringReader struct {
	store.Reader
	s *expiringStore
}

type expiringTx struct {
	store.Tx
	s *expiringStore
}

func (s *expiringStore) expire(c model.Cursor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expired[c] = true
}

func (s *expiringStore) events(r store.Reader, ctx context.Context, f model.Filter, after model.Cursor, limit int) (model.Page[model.Event], error) {
	s.mu.Lock()
	gone := s.expired[after]
	s.mu.Unlock()
	if gone {
		return model.Page[model.Event]{}, model.NewError(model.CursorExpired, "cursor behind retention")
	}
	return r.Events(ctx, f, after, limit)
}

func (r expiringReader) Events(ctx context.Context, f model.Filter, after model.Cursor, limit int) (model.Page[model.Event], error) {
	return r.s.events(r.Reader, ctx, f, after, limit)
}

func (t expiringTx) Events(ctx context.Context, f model.Filter, after model.Cursor, limit int) (model.Page[model.Event], error) {
	return t.s.events(t.Tx, ctx, f, after, limit)
}

func (s *expiringStore) View(ctx context.Context, t model.TenantID, fn func(store.Reader) error) error {
	return s.Store.View(ctx, t, func(r store.Reader) error { return fn(expiringReader{r, s}) })
}

func (s *expiringStore) Transact(ctx context.Context, t model.TenantID, fn func(store.Tx) error) error {
	return s.Store.Transact(ctx, t, func(tx store.Tx) error { return fn(expiringTx{tx, s}) })
}

func TestPollReportsCursorExpiredWithRecovery(t *testing.T) {
	var exp *expiringStore
	h := newEvHarness(t, func(s store.Store) store.Store {
		exp = &expiringStore{Store: s, expired: map[model.Cursor]bool{}}
		return exp
	})
	h.message(enginetest.InboxOne)
	sub := h.openFrom(model.StartBeginning, model.Filter{})
	b := h.poll(sub, 10)
	h.ack(sub, b.AckToken)
	h.message(enginetest.InboxOne)
	exp.expire(b.ScannedThrough)

	_, err := h.bot.Events().Poll(h.ctx, h.pollReq(sub, 10))
	enginetest.RequireCode(t, err, model.CursorExpired, "reopen")
	var me *model.Error
	if !errors.As(err, &me) || me.Retry != model.ReconcileFirst {
		t.Fatalf("retry advice = %+v", me)
	}
	if rec := h.record(sub); rec.PendingBatch != nil || rec.Subscription.Checkpoint != b.ScannedThrough {
		t.Fatalf("expired poll changed the subscription: %+v", rec)
	}

	// Opening at an expired checkpoint fails the same way; reopening from
	// the retained beginning recovers.
	req := h.openReq("recover", model.StartCheckpoint, model.Filter{})
	req.After = b.ScannedThrough
	_, err = h.bot.Events().Open(h.ctx, req)
	enginetest.RequireCode(t, err, model.CursorExpired, "reopen")
	fresh := h.open(h.bot, h.openReq("recovered", model.StartBeginning, model.Filter{})).Subscription.Ref
	if len(h.poll(fresh, 10).Events) != 2 {
		t.Fatal("recovery from the beginning did not deliver")
	}
}

func TestCloseStopsDeliveryAndReopenRevives(t *testing.T) {
	h := newEvHarness(t, nil)
	e1 := h.message(enginetest.InboxOne)
	filter := model.Filter{Inboxes: []model.InboxRef{h.inbox(enginetest.InboxOne)}}
	sub := h.openFrom(model.StartBeginning, filter)
	b := h.poll(sub, 10)
	lookup := model.Lookup[model.SubscriptionRef]{Call: model.Call{Correlation: "close"}, Ref: sub}
	acc, err := h.bot.Events().Close(h.ctx, lookup, h.op("close"))
	if err != nil {
		t.Fatal(err)
	}
	if acc.Operation.State != model.Committed || acc.Replayed {
		t.Fatalf("close = %+v", acc)
	}
	_, err = h.bot.Events().Poll(h.ctx, h.pollReq(sub, 10))
	enginetest.RequireCode(t, err, model.Conflict, "closed")
	_, err = h.bot.Events().Ack(h.ctx, h.ackReq(sub, b.AckToken))
	enginetest.RequireCode(t, err, model.Conflict, "closed")
	_, err = h.bot.Events().Close(h.ctx, lookup, h.op("close"))
	enginetest.RequireCode(t, err, model.Conflict, "closed")
	_, err = h.client(otherActor).Events().Close(h.ctx, lookup, h.op("close"))
	enginetest.RequireCode(t, err, model.NotFound, "")

	// History is kept: reopening the consumer revives it at the requested
	// start and delivers from there.
	e2 := h.message(enginetest.InboxOne)
	revived := h.open(h.bot, h.openReq("bot", model.StartBeginning, filter))
	if revived.Subscription.Ref != sub || revived.Subscription.Checkpoint != "" {
		t.Fatalf("revived = %+v", revived)
	}
	requireIDs(t, h.poll(sub, 10).Events, e1, e2)
}

func TestEventsRefuseClosedEngine(t *testing.T) {
	h := newEvHarness(t, nil)
	sub := h.openFrom(model.StartTail, model.Filter{})
	if err := h.e.Close(h.ctx); err != nil {
		t.Fatal(err)
	}
	_, err := h.bot.Events().Open(h.ctx, h.openReq("x", model.StartTail, model.Filter{}))
	enginetest.RequireCode(t, err, model.Unavailable, "closed")
	_, err = h.bot.Events().Poll(h.ctx, h.pollReq(sub, 1))
	enginetest.RequireCode(t, err, model.Unavailable, "closed")
	_, err = h.bot.Events().Ack(h.ctx, h.ackReq(sub, "t"))
	enginetest.RequireCode(t, err, model.Unavailable, "closed")
}
