package events_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/events"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// countingStore counts the store units opened through it and records the
// limit of each event read made in a read unit.
type countingStore struct {
	store.Store
	views, transacts atomic.Int32
	mu               sync.Mutex
	limits           []int
}

type limitReader struct {
	store.Reader
	s *countingStore
}

func (r limitReader) Events(ctx context.Context, f model.Filter, after model.Cursor, limit int) (model.Page[model.Event], error) {
	r.s.mu.Lock()
	r.s.limits = append(r.s.limits, limit)
	r.s.mu.Unlock()
	return r.Reader.Events(ctx, f, after, limit)
}

func (s *countingStore) View(ctx context.Context, t model.TenantID, fn func(store.Reader) error) error {
	s.views.Add(1)
	return s.Store.View(ctx, t, func(r store.Reader) error { return fn(limitReader{r, s}) })
}

func (s *countingStore) Transact(ctx context.Context, t model.TenantID, fn func(store.Tx) error) error {
	s.transacts.Add(1)
	return s.Store.Transact(ctx, t, fn)
}

func (s *countingStore) reset() {
	s.views.Store(0)
	s.transacts.Store(0)
	s.mu.Lock()
	s.limits = nil
	s.mu.Unlock()
}

func (s *countingStore) readLimits() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.limits)
}

func newCountingHarness(t *testing.T) (*evHarness, *countingStore) {
	t.Helper()
	var cs *countingStore
	h := newEvHarness(t, func(s store.Store) store.Store {
		cs = &countingStore{Store: s}
		return cs
	})
	return h, cs
}

func listReq(start string, after model.Cursor, f model.Filter, limit int) model.ListEvents {
	return model.ListEvents{Call: model.Call{Correlation: "list"}, Filter: f, Start: start, After: after, Limit: limit}
}

func (h *evHarness) list(req model.ListEvents) model.Page[model.Event] {
	h.t.Helper()
	p, err := h.bot.Events().List(h.ctx, req)
	if err != nil {
		h.t.Fatalf("List: %v", err)
	}
	return p
}

func TestListReturnsCommittedEventsInOrder(t *testing.T) {
	h := newEvHarness(t, nil)
	e1, e2, e3 := h.message(enginetest.InboxOne), h.message(enginetest.InboxOne), h.message(enginetest.InboxOne)

	all := h.list(listReq(model.StartBeginning, "", model.Filter{}, 0))
	requireIDs(t, all.Items, e1, e2, e3)
	if !all.Complete || all.Next == "" {
		t.Fatalf("beginning page = %+v, want complete with a position", all)
	}

	first := h.list(listReq(model.StartBeginning, "", model.Filter{}, 2))
	requireIDs(t, first.Items, e1, e2)
	if first.Complete {
		t.Fatal("partial page reported complete")
	}
	rest := h.list(listReq(model.StartCheckpoint, first.Next, model.Filter{}, 2))
	requireIDs(t, rest.Items, e3)
	if !rest.Complete || rest.Next != all.Next {
		t.Fatalf("checkpoint page = %+v, want complete at %q", rest, all.Next)
	}

	tail := h.list(listReq(model.StartTail, "", model.Filter{}, 0))
	if len(tail.Items) != 0 || !tail.Complete || tail.Next != all.Next {
		t.Fatalf("tail page = %+v, want empty and complete at the head %q", tail, all.Next)
	}
	e4 := h.message(enginetest.InboxOne)
	requireIDs(t, h.list(listReq(model.StartCheckpoint, tail.Next, model.Filter{}, 0)).Items, e4)

	// Events are returned as stored, payload version included.
	if got := all.Items[0]; got.Payload.Version != model.PayloadVersion1 || got.Payload.Name != model.SchemaMessageReceived {
		t.Fatalf("payload = %+v, want the stored payload", got.Payload)
	}
}

func TestListFiltersByInboxAndKind(t *testing.T) {
	h := newEvHarness(t, nil)
	e1 := h.message(enginetest.InboxOne)
	h.message(enginetest.InboxTwo)
	e3 := h.message(enginetest.InboxOne)
	updated := e3
	updated.ID, updated.Kind, updated.Payload.Name = seqID(0xf0, 1), model.EventMessageUpdated, model.SchemaMessageUpdated
	h.append(updated)
	f := model.Filter{Inboxes: []model.InboxRef{h.inbox(enginetest.InboxOne)}, Kinds: []string{model.EventMessageReceived}}
	requireIDs(t, h.list(listReq(model.StartBeginning, "", f, 0)).Items, e1, e3)

	_, err := h.bot.Events().List(h.ctx, listReq(model.StartBeginning, "", model.Filter{Kinds: []string{"nope"}}, 0))
	enginetest.RequireCode(t, err, model.Validation, "filter.kinds[0]")
	foreign := model.Filter{Inboxes: []model.InboxRef{{Tenant: enginetest.TenantB, ID: enginetest.InboxOne}}}
	_, err = h.bot.Events().List(h.ctx, listReq(model.StartBeginning, "", foreign, 0))
	enginetest.RequireCode(t, err, model.NotFound, "")
	// The conversation is read from each message payload: e3 and its
	// update share one.
	conv := model.ConversationRef{Inbox: h.inbox(enginetest.InboxOne), ID: seqID(0xc0, 3)}
	requireIDs(t, h.list(listReq(model.StartBeginning, "", model.Filter{Conversation: &conv}, 0)).Items, e3, updated)
	foreignConv := model.ConversationRef{Inbox: model.InboxRef{Tenant: enginetest.TenantB, ID: enginetest.InboxOne}, ID: conv.ID}
	_, err = h.bot.Events().List(h.ctx, listReq(model.StartBeginning, "", model.Filter{Conversation: &foreignConv}, 0))
	enginetest.RequireCode(t, err, model.NotFound, "")
	_, err = h.bot.Events().List(h.ctx, listReq(model.StartBeginning, "", model.Filter{Statuses: []string{"open"}}, 0))
	enginetest.RequireCode(t, err, model.Validation, "filter.statuses")
}

// List writes nothing: no store write unit, so no subscription, lease,
// checkpoint, operation or event.
func TestListCreatesNoSubscription(t *testing.T) {
	h, cs := newCountingHarness(t)
	h.message(enginetest.InboxOne)
	e2 := h.message(enginetest.InboxOne)
	h.gate.deny(e2.Resource)
	cs.reset()
	p := h.list(listReq(model.StartBeginning, "", model.Filter{}, 0))
	h.list(listReq(model.StartTail, "", model.Filter{}, 0))
	h.list(listReq(model.StartCheckpoint, p.Next, model.Filter{}, 0))
	if n := cs.transacts.Load(); n != 0 {
		t.Fatalf("List opened %d write units, want none", n)
	}
	if n := cs.views.Load(); n != 3 {
		t.Fatalf("List opened %d read units for 3 calls, want one each", n)
	}
}

// The call is authorized for event.list on the tenant's event log before
// anything is read; a denial is Unauthorized and opens no store unit.
func TestListCallDenied(t *testing.T) {
	h, cs := newCountingHarness(t)
	h.message(enginetest.InboxOne)
	h.list(listReq(model.StartBeginning, "", model.Filter{}, 0))
	var call *policy.Authorization
	for i, a := range h.gate.requests {
		if a.Action == model.ActionEventList {
			call = &h.gate.requests[i]
		}
	}
	if call == nil || call.Resource != model.EventLogRef(enginetest.TenantA) || call.Principal.Actor != botActor {
		t.Fatalf("event.list authorization = %+v, want the bot on the tenant event log", call)
	}

	h.gate.denyAction(model.ActionEventList)
	cs.reset()
	for _, start := range []string{model.StartBeginning, model.StartTail} {
		_, err := h.bot.Events().List(h.ctx, listReq(start, "", model.Filter{}, 0))
		enginetest.RequireCode(t, err, model.Unauthorized, "")
	}
	if v, w := cs.views.Load(), cs.transacts.Load(); v+w != 0 {
		t.Fatalf("denied List opened %d read and %d write units, want none", v, w)
	}
}

// Events the principal may not read are skipped without trace: no
// placeholder, no count; Next still moves past them so they never reappear.
func TestListSkipsDeniedEvents(t *testing.T) {
	h := newEvHarness(t, nil)
	e1, e2, e3 := h.message(enginetest.InboxOne), h.message(enginetest.InboxOne), h.message(enginetest.InboxTwo)
	e4 := h.message(enginetest.InboxOne)
	h.gate.deny(e2.Resource)
	h.gate.deny(h.inbox(enginetest.InboxTwo).Ref())
	_ = e3

	all := h.list(listReq(model.StartBeginning, "", model.Filter{}, 0))
	requireIDs(t, all.Items, e1, e4)

	first := h.list(listReq(model.StartBeginning, "", model.Filter{}, 2))
	requireIDs(t, first.Items, e1)
	skipped := h.list(listReq(model.StartCheckpoint, first.Next, model.Filter{}, 1))
	if len(skipped.Items) != 0 || skipped.Complete || skipped.Next == first.Next {
		t.Fatalf("page of a denied event = %+v, want empty, incomplete and advanced", skipped)
	}
	rest := h.list(listReq(model.StartCheckpoint, skipped.Next, model.Filter{}, 10))
	requireIDs(t, rest.Items, e4)

	h.gate.failWith(errors.New("policy down"))
	_, err := h.bot.Events().List(h.ctx, listReq(model.StartBeginning, "", model.Filter{}, 0))
	enginetest.RequireCode(t, err, model.Unavailable, "")
}

// Limit bounds the events one call scans, not those it returns: a page of
// denied events comes back empty with Next past them, after one read.
func TestListScanBoundReturnsPartialPage(t *testing.T) {
	h, cs := newCountingHarness(t)
	for range 5 {
		h.gate.deny(h.message(enginetest.InboxOne).Resource)
	}
	e6 := h.message(enginetest.InboxOne)
	cs.reset()
	p := h.list(listReq(model.StartBeginning, "", model.Filter{}, 3))
	if len(p.Items) != 0 || p.Complete || p.Next == "" {
		t.Fatalf("page = %+v, want empty and incomplete after scanning 3", p)
	}
	if n := cs.views.Load(); n != 1 {
		t.Fatalf("List opened %d read units, want one", n)
	}
	p = h.list(listReq(model.StartCheckpoint, p.Next, model.Filter{}, 3))
	requireIDs(t, p.Items, e6)
	if !p.Complete {
		t.Fatalf("page = %+v, want complete", p)
	}
}

// A zero Limit scans DefaultListLimit events and a larger one is capped at
// MaxListLimit, in one store read of exactly that many.
func TestListLimitDefaultAndCap(t *testing.T) {
	h, cs := newCountingHarness(t)
	evs := make([]model.Event, events.MaxListLimit+1)
	for i := range evs {
		evs[i] = h.newMessage(enginetest.InboxOne)
	}
	h.append(evs...)
	p := h.list(listReq(model.StartBeginning, "", model.Filter{}, 0))
	if len(p.Items) != events.DefaultListLimit || p.Complete {
		t.Fatalf("default page = %d items, complete %v; want %d, incomplete", len(p.Items), p.Complete, events.DefaultListLimit)
	}
	cs.reset()
	p = h.list(listReq(model.StartBeginning, "", model.Filter{}, 10*events.MaxListLimit))
	if len(p.Items) != events.MaxListLimit || p.Complete {
		t.Fatalf("capped page = %d items, complete %v; want %d, incomplete", len(p.Items), p.Complete, events.MaxListLimit)
	}
	if got := cs.readLimits(); !slices.Equal(got, []int{events.MaxListLimit}) {
		t.Fatalf("store reads = %v, want one of %d", got, events.MaxListLimit)
	}
}

// A position behind retention is CursorExpired with List's own recovery,
// not the subscription's.
func TestListCursorExpired(t *testing.T) {
	var exp *expiringStore
	h := newEvHarness(t, func(s store.Store) store.Store {
		exp = &expiringStore{Store: s, expired: map[model.Cursor]bool{}}
		return exp
	})
	h.message(enginetest.InboxOne)
	p := h.list(listReq(model.StartBeginning, "", model.Filter{}, 0))
	h.message(enginetest.InboxOne)
	exp.expire(p.Next)

	_, err := h.bot.Events().List(h.ctx, listReq(model.StartCheckpoint, p.Next, model.Filter{}, 0))
	enginetest.RequireCode(t, err, model.CursorExpired, "restart the list from beginning or tail")
	var me *model.Error
	if !errors.As(err, &me) || me.Retry != model.ReconcileFirst || strings.Contains(me.SafeMessage, "subscription") {
		t.Fatalf("expired list error = %+v, want List recovery advice", me)
	}
	if got := h.list(listReq(model.StartBeginning, "", model.Filter{}, 0)); len(got.Items) != 2 {
		t.Fatalf("restart from beginning = %d events, want 2", len(got.Items))
	}
}

// List up to Next, then open a subscription at Next: the subscription
// delivers exactly the events after the list, without gap or duplicate.
func TestListHandsOffToSubscription(t *testing.T) {
	h := newEvHarness(t, nil)
	e1, e2 := h.message(enginetest.InboxOne), h.message(enginetest.InboxOne)
	p := h.list(listReq(model.StartBeginning, "", model.Filter{}, 0))
	requireIDs(t, p.Items, e1, e2)
	e3 := h.message(enginetest.InboxOne)
	req := h.openReq("handoff", model.StartCheckpoint, model.Filter{})
	req.After = p.Next
	sub := h.open(h.bot, req).Subscription.Ref
	requireIDs(t, h.poll(sub, 10).Events, e3)
}

// The tail of a log never written is the empty position, which a
// checkpoint start does not accept; the beginning continues from it.
func TestListTailOfEmptyLog(t *testing.T) {
	h := newEvHarness(t, nil)
	tail := h.list(listReq(model.StartTail, "", model.Filter{}, 0))
	if len(tail.Items) != 0 || !tail.Complete || tail.Next != "" {
		t.Fatalf("tail of an empty log = %+v, want empty and complete at the empty position", tail)
	}
	_, err := h.bot.Events().List(h.ctx, listReq(model.StartCheckpoint, tail.Next, model.Filter{}, 0))
	enginetest.RequireCode(t, err, model.Validation, "after")
	e1 := h.message(enginetest.InboxOne)
	requireIDs(t, h.list(listReq(model.StartBeginning, "", model.Filter{}, 0)).Items, e1)
}

func TestListRefusesClosedEngine(t *testing.T) {
	h := newEvHarness(t, nil)
	if err := h.e.Close(h.ctx); err != nil {
		t.Fatal(err)
	}
	_, err := h.bot.Events().List(h.ctx, listReq(model.StartTail, "", model.Filter{}, 0))
	enginetest.RequireCode(t, err, model.Unavailable, "closed")
	_, err = h.bot.Events().List(h.ctx, model.ListEvents{})
	enginetest.RequireCode(t, err, model.Unavailable, "closed")
}
