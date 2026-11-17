package conformance

import (
	"errors"
	"slices"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// HistoryProvider is implemented by the Provider of an adapter declaring
// history: the history cases need past messages on the provider to read.
// Without it they skip.
type HistoryProvider interface {
	// HoldHistory makes ev, a new message (InboundEvent.Kind empty), a past
	// message on channel's account that occurred at at, in the conversation
	// of its sender: the one Route returns for ev.Sender. The adapter's
	// HistoryReader reads it back as the same provider message it reports
	// inbound. An event the provider already delivered is held once, as
	// the message the provider holds. It returns ErrNotExpressible when the
	// provider cannot hold such a message.
	HoldHistory(channel model.ChannelRef, ev InboundEvent, at time.Time) error
}

// historyReadCap bounds the pages one window read takes, so a reader whose
// cursor never ends fails rather than hangs.
const historyReadCap = 100

// historyCursor is a cursor no provider issued.
const historyCursor = "conformance-unknown-cursor"

// historyReader opens an instance on channel A and returns its reader. It
// skips the case when the provider holds no history.
func (e *adapterEnv) historyReader(t *testing.T) adapter.HistoryReader {
	t.Helper()
	if _, ok := e.provider.(HistoryProvider); !ok {
		t.Skip("provider does not implement HistoryProvider")
	}
	hr, ok := e.open(t, adapterChanA).(adapter.HistoryReader)
	if !ok {
		t.Fatal("history declared but the instance is no adapter.HistoryReader")
	}
	return hr
}

// since is the start of the window the history cases read.
func (e *adapterEnv) since() time.Time { return e.clock.Now().Add(-24 * time.Hour) }

// hold makes a message id from peer, to "support", a past message on
// channel A that occurred at, and returns it.
func (e *adapterEnv) hold(t *testing.T, id, peer string, at time.Time) InboundEvent {
	t.Helper()
	ev := e.message(adapterChanA, id, peer, "support")
	if err := e.provider.(HistoryProvider).HoldHistory(adapterChanA, ev, at); err != nil {
		if errors.Is(err, ErrNotExpressible) {
			t.Skipf("provider cannot hold message %s", ev.MessageID)
		}
		t.Fatalf("HoldHistory %s: %v", ev.MessageID, err)
	}
	return ev
}

// readHistory reads the whole window from since in pages of at most limit
// envelopes, checking each page, and returns the envelopes in order.
func (e *adapterEnv) readHistory(t *testing.T, hr adapter.HistoryReader, route *model.ExternalRef, limit int) []model.Envelope {
	t.Helper()
	return e.readWindow(t, hr, route, time.Time{}, limit)
}

// readWindow is readHistory for the window ending, exclusively, at until
// (the zero time: open).
func (e *adapterEnv) readWindow(t *testing.T, hr adapter.HistoryReader, route *model.ExternalRef, until time.Time, limit int) []model.Envelope {
	t.Helper()
	var out []model.Envelope
	cursor := ""
	for range historyReadCap {
		p, err := hr.History(e.ctx, adapter.HistoryRequest{Route: route, Since: e.since(), Until: until, Cursor: cursor, Limit: limit})
		if err != nil {
			t.Fatalf("History(cursor %q, limit %d): %v", cursor, limit, err)
		}
		if len(p.Envelopes) > limit {
			t.Fatalf("History(limit %d) returned %d envelopes", limit, len(p.Envelopes))
		}
		out = append(out, p.Envelopes...)
		if p.Next == "" {
			return out
		}
		if p.Next == cursor {
			t.Fatalf("History cursor %q did not advance", cursor)
		}
		cursor = p.Next
	}
	t.Fatalf("History window not read after %d pages", historyReadCap)
	return nil
}

func envelopeIDs(envs []model.Envelope) []string {
	out := make([]string, len(envs))
	for i, env := range envs {
		out[i] = env.MessageID
	}
	return out
}

// wantWindow requires got to hold exactly the messages of want, in order,
// each once, as the provider reported them, within the window.
func (e *adapterEnv) wantWindow(t *testing.T, got []model.Envelope, want ...InboundEvent) {
	t.Helper()
	ids := envelopeIDs(got)
	for i, id := range ids {
		if slices.Contains(ids[:i], id) {
			t.Fatalf("history holds provider message %q twice", id)
		}
	}
	var wantIDs []string
	for _, ev := range want {
		wantIDs = append(wantIDs, ev.MessageID)
	}
	if !slices.Equal(ids, wantIDs) {
		t.Fatalf("history messages = %q, want %q: the window's messages, oldest first, each once", ids, wantIDs)
	}
	for i, env := range got {
		if env.Kind != model.EnvelopeMessage && env.Kind != model.EnvelopeAutomatedMessage {
			t.Errorf("history envelope %q kind %q, want a message", env.MessageID, env.Kind)
		}
		if env.Destination != want[i].Destination {
			t.Errorf("history envelope %q destination %q, want %q as inbound", env.MessageID, env.Destination, want[i].Destination)
		}
		if env.OccurredAt.Before(e.since()) {
			t.Errorf("history envelope %q occurred at %v, before the window's start %v", env.MessageID, env.OccurredAt, e.since())
		}
	}
}

// wantNothingApplied requires the provider to have applied no effect.
func (e *adapterEnv) wantNothingApplied(t *testing.T) {
	t.Helper()
	for _, c := range e.provider.Calls(adapterChanA) {
		if c.Applied {
			t.Fatalf("provider applied %+v during history reads: a read changes nothing", c)
		}
	}
}

var historyCap = []model.Capability{model.CapabilityHistory}

var adapterHistoryCases = []adapterCase{
	{name: "HistoryWindowOldestFirstInBoundedPages", needs: historyCap, run: func(t *testing.T, e *adapterEnv) {
		hr := e.historyReader(t)
		since := e.since()
		e.hold(t, "h-old", "peer-1", since.Add(-time.Minute))
		var want []InboundEvent
		for i, id := range []string{"h-1", "h-2", "h-3", "h-4", "h-5"} {
			// The first message occurs at the window's start, which the
			// window includes.
			want = append(want, e.hold(t, id, "peer-1", since.Add(time.Duration(i)*time.Hour)))
		}
		route := e.route(adapterChanA, "peer-1")
		for _, limit := range []int{2, 1, 3} {
			e.wantWindow(t, e.readHistory(t, hr, &route, limit), want...)
		}
		first, err := hr.History(e.ctx, adapter.HistoryRequest{Route: &route, Since: since, Limit: 2})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		again, err := hr.History(e.ctx, adapter.HistoryRequest{Route: &route, Since: since, Limit: 2})
		if err != nil || again.Next != first.Next || !slices.Equal(envelopeIDs(again.Envelopes), envelopeIDs(first.Envelopes)) {
			t.Fatalf("first page read again = %q next %q, %v; want %q next %q: a read changes nothing",
				envelopeIDs(again.Envelopes), again.Next, err, envelopeIDs(first.Envelopes), first.Next)
		}
		e.wantNothingApplied(t)
	}},
	{name: "HistoryConversationOrChannel", needs: historyCap, run: func(t *testing.T, e *adapterEnv) {
		hr := e.historyReader(t)
		since := e.since()
		a1 := e.hold(t, "a-1", "peer-1", since.Add(time.Hour))
		b1 := e.hold(t, "b-1", "peer-2", since.Add(2*time.Hour))
		a2 := e.hold(t, "a-2", "peer-1", since.Add(3*time.Hour))
		route := e.route(adapterChanA, "peer-1")
		e.wantWindow(t, e.readHistory(t, hr, &route, 10), a1, a2)
		e.wantWindow(t, e.readHistory(t, hr, nil, 10), a1, b1, a2)
		e.wantNothingApplied(t)
	}},
	{name: "HistoryUnknownCursorIsValidation", needs: historyCap, run: func(t *testing.T, e *adapterEnv) {
		hr := e.historyReader(t)
		e.hold(t, "h-1", "peer-1", e.since().Add(time.Hour))
		route := e.route(adapterChanA, "peer-1")
		_, err := hr.History(e.ctx, adapter.HistoryRequest{Route: &route, Since: e.since(), Cursor: historyCursor, Limit: 10})
		if model.CodeOf(err) != model.Validation {
			t.Fatalf("History with a cursor no provider issued = %v, want validation", err)
		}
		e.wantNothingApplied(t)
	}},
	{name: "HistoryImportSkipsInboundCopy", needs: historyCap, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		e.historyReader(t)
		e.inbound(t)
		live := e.message(adapterChanA, "live", "peer-1", "support")
		wantAcked(t, e.receiveAll(t, adapterChanA, live))
		m := findMessage(e.messages(t, adapterInboxA), adapterChanA, live.MessageID)
		if m == nil {
			t.Skip("adapter stored no message for the event")
		}
		since := e.since()
		h1 := e.hold(t, "h-1", "peer-1", since.Add(time.Hour))
		if err := e.provider.(HistoryProvider).HoldHistory(adapterChanA, live, since.Add(2*time.Hour)); err != nil {
			t.Fatalf("HoldHistory %s: %v", live.MessageID, err)
		}
		h2 := e.hold(t, "h-2", "peer-1", since.Add(3*time.Hour))

		conv := m.Ref.Conversation
		req := uma.ImportRequest{Requester: "conformance-agent", Conversation: &conv, Since: since}
		rep, err := e.engine.Runtime().Import(e.ctx, runtimeCall(), req)
		if err != nil || rep != (uma.ImportReport{Imported: 2, Skipped: 1, Complete: true}) {
			t.Fatalf("Import = %+v, %v; want 2 imported and the inbound copy skipped", rep, err)
		}
		msgs := e.messages(t, adapterInboxA)
		for _, ev := range []InboundEvent{live, h1, h2} {
			n := 0
			for _, msg := range msgs {
				if slices.Contains(msg.External, model.ProviderMessageRef(adapterChanA, ev.MessageID)) {
					n++
					imported := ev.MessageID != live.MessageID
					if msg.Ref.Conversation != conv || (msg.ImportedBy == req.Requester) != imported {
						t.Errorf("message %q in %v imported by %q, want it in %v, imported: %v", ev.MessageID, msg.Ref.Conversation, msg.ImportedBy, conv, imported)
					}
				}
			}
			if n != 1 {
				t.Errorf("provider message %q stored %d times, want once", ev.MessageID, n)
			}
		}
		again, err := e.engine.Runtime().Import(e.ctx, runtimeCall(), req)
		if err != nil || again != (uma.ImportReport{Skipped: 3, Complete: true}) {
			t.Fatalf("second Import = %+v, %v; want all 3 skipped", again, err)
		}
	}},
	{name: "HistoryUntilIsExclusive", needs: historyCap, run: func(t *testing.T, e *adapterEnv) {
		hr := e.historyReader(t)
		since := e.since()
		// The times are the ones the other cases give these IDs, so a
		// provider whose message IDs carry their time (a snowflake) can
		// hold them.
		a := e.hold(t, "h-1", "peer-1", since)
		b := e.hold(t, "h-2", "peer-1", since.Add(3*time.Hour))
		e.hold(t, "h-3", "peer-1", since.Add(4*time.Hour)) // at Until: out
		e.hold(t, "h-4", "peer-1", since.Add(5*time.Hour))
		for _, limit := range []int{1, 10} {
			e.wantWindow(t, e.readWindow(t, hr, nil, since.Add(4*time.Hour), limit), a, b)
		}
		e.wantNothingApplied(t)
	}},
	{name: "HistoryResumesFromACursorOnAnotherReader", needs: historyCap, run: func(t *testing.T, e *adapterEnv) {
		hr := e.historyReader(t)
		since := e.since()
		var want []InboundEvent
		for i, id := range []string{"h-1", "h-2", "h-3", "h-4", "h-5"} {
			want = append(want, e.hold(t, id, "peer-1", since.Add(time.Duration(i+1)*time.Hour)))
		}
		first, err := hr.History(e.ctx, adapter.HistoryRequest{Since: since, Limit: 2})
		if err != nil || first.Next == "" {
			t.Fatalf("History = %+v, %v; want a first page with more to read", first, err)
		}
		// A cursor is a position in the window, not state of the reader that
		// issued it: a reader opened later, as after a restart, resumes it.
		later, ok := e.open(t, adapterChanA).(adapter.HistoryReader)
		if !ok {
			t.Fatal("history declared but the instance is no adapter.HistoryReader")
		}
		var got []model.Envelope
		got = append(got, first.Envelopes...)
		cursor := first.Next
		for range historyReadCap {
			p, err := later.History(e.ctx, adapter.HistoryRequest{Since: since, Cursor: cursor, Limit: 2})
			if err != nil {
				t.Fatalf("History(resumed cursor %q): %v", cursor, err)
			}
			got = append(got, p.Envelopes...)
			if cursor = p.Next; cursor == "" {
				break
			}
		}
		e.wantWindow(t, got, want...)
	}},
	{name: "HistoryEnvelopeEqualsInbound", needs: historyCap, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		hr := e.historyReader(t)
		e.inbound(t)
		live := e.message(adapterChanA, "live", "peer-1", "support")
		wantAcked(t, e.receiveAll(t, adapterChanA, live))
		if err := e.provider.(HistoryProvider).HoldHistory(adapterChanA, live, e.since().Add(time.Hour)); err != nil {
			t.Skipf("provider cannot hold the delivered message: %v", err)
		}
		got := e.readHistory(t, hr, nil, 10)
		if len(got) != 1 {
			t.Fatalf("history = %q, want the delivered message once", envelopeIDs(got))
		}
		// The receiver stored the message under the ID, and for the
		// destination, the history envelope names.
		if got[0].MessageID != live.MessageID || got[0].Destination != live.Destination {
			t.Errorf("history envelope = message %q destination %q; the receiver reported message %q destination %q",
				got[0].MessageID, got[0].Destination, live.MessageID, live.Destination)
		}
		if findMessage(e.messages(t, adapterInboxA), adapterChanA, got[0].MessageID) == nil {
			t.Errorf("the receiver stored no message under the history envelope's ID %q", got[0].MessageID)
		}
	}},
	{name: "HistoryImportedThenReceivedStoresOnce", needs: historyCap, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		e.historyReader(t)
		e.inbound(t)
		// Open the conversation, then import a message the provider holds
		// and the receiver has not taken yet.
		seed := e.message(adapterChanA, "seed", "peer-1", "support")
		wantAcked(t, e.receiveAll(t, adapterChanA, seed))
		m := findMessage(e.messages(t, adapterInboxA), adapterChanA, seed.MessageID)
		if m == nil {
			t.Skip("adapter stored no message for the event")
		}
		late := e.message(adapterChanA, "late", "peer-1", "support")
		before := len(e.provider.Inbound(adapterChanA))
		e.deliver(t, adapterChanA, late)
		if err := e.provider.(HistoryProvider).HoldHistory(adapterChanA, late, e.clock.Now().Add(-time.Minute)); err != nil {
			t.Skipf("provider cannot hold the delivered message: %v", err)
		}
		conv := m.Ref.Conversation
		req := uma.ImportRequest{Requester: "conformance-agent", Conversation: &conv, Since: e.since()}
		if rep, err := e.engine.Runtime().Import(e.ctx, runtimeCall(), req); err != nil || rep.Imported != 1 {
			t.Fatalf("Import = %+v, %v; want the message imported", rep, err)
		}
		wantAcked(t, e.pump(t, adapterChanA, before, late))
		n := 0
		for _, msg := range e.messages(t, adapterInboxA) {
			if slices.Contains(msg.External, model.ProviderMessageRef(adapterChanA, late.MessageID)) {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("provider message %q stored %d times after import then receive, want once", late.MessageID, n)
		}
	}},
	{name: "HistoryConversationImportTakesTheSendersMessagesOnly", needs: historyCap, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		e.historyReader(t)
		e.inbound(t)
		seed := e.message(adapterChanA, "seed", "peer-1", "support")
		wantAcked(t, e.receiveAll(t, adapterChanA, seed))
		m := findMessage(e.messages(t, adapterInboxA), adapterChanA, seed.MessageID)
		if m == nil {
			t.Skip("adapter stored no message for the event")
		}
		since := e.since()
		mine := []InboundEvent{e.hold(t, "p1-1", "peer-1", since.Add(time.Hour)), e.hold(t, "p1-2", "peer-1", since.Add(3*time.Hour))}
		theirs := e.hold(t, "p2-1", "peer-2", since.Add(2*time.Hour))
		conv := m.Ref.Conversation
		rep, err := e.engine.Runtime().Import(e.ctx, runtimeCall(), uma.ImportRequest{Requester: "conformance-agent", Conversation: &conv, Since: since})
		if err != nil || rep.Imported != len(mine) || !rep.Complete {
			t.Fatalf("Import = %+v, %v; want the %d messages of the conversation's sender", rep, err, len(mine))
		}
		msgs := e.messages(t, adapterInboxA)
		if findMessage(msgs, adapterChanA, theirs.MessageID) != nil {
			t.Errorf("a conversation import stored %q, another sender's message", theirs.MessageID)
		}
		for _, ev := range mine {
			if got := findMessage(msgs, adapterChanA, ev.MessageID); got == nil || got.Ref.Conversation != conv {
				t.Errorf("message %q not stored in %v", ev.MessageID, conv)
			}
		}
	}},
}
