package telegram

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// cursorSink is a sink that keeps the cursor the engine would: the one of the
// last ingress that carried one.
type cursorSink struct {
	sinkRecorder
	cursor string
}

func (s *cursorSink) Accept(ctx context.Context, in model.Ingress) (model.IngressReceipt, error) {
	rec, err := s.sinkRecorder.Accept(ctx, in)
	if err != nil {
		return rec, err
	}
	s.mu.Lock()
	if in.ReceiverCursor != "" {
		s.cursor = in.ReceiverCursor
	}
	s.mu.Unlock()
	return rec, nil
}

func (s *cursorSink) ReceiverCursor(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor, nil
}

var _ adapter.CursorSink = (*cursorSink)(nil)

// run runs the receiver until it idles with n ingress accepted, or the run
// ends, and returns its error: the idle read after the updates is what lets
// the next offset be seen, and it is where the run is stopped.
func (r *rig) run(t *testing.T, sink *cursorSink, n int) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	r.x.wait = func(ctx context.Context, d time.Duration) error {
		sink.mu.Lock()
		have := len(sink.got)
		sink.mu.Unlock()
		if have >= n {
			cancel()
		}
		return ctx.Err()
	}
	return r.x.Run(ctx, sink)
}

func (r *rig) queue(updates ...obj) {
	for _, u := range updates {
		r.f.queueUpdate("acct1", u)
	}
}

func msgUpdate(id int64, text string) obj {
	return updateOf(id, "message", textMessage(privateChat(testPerson), person(testPerson), id, text, nil))
}

func (r *rig) polls() []map[string]any {
	var out []map[string]any
	for _, c := range r.callsOf("getUpdates") {
		out = append(out, c.args())
	}
	return out
}

func TestReceiverReadsAndHandsUpdatesOver(t *testing.T) {
	r := newRig(t)
	r.queue(msgUpdate(10, "one"), msgUpdate(11, "two"), msgUpdate(12, "three"))
	sink := &cursorSink{}
	err := r.run(t, sink, 3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want the cancellation that ended it", err)
	}
	got := sink.envelopes()
	if len(got) != 3 || got[0].Content.Parts[0].Text != "one" || got[2].Content.Parts[0].Text != "three" {
		t.Fatalf("envelopes %+v, want the three in order", got)
	}
	// The first read names no offset; the next confirms what was handled.
	polls := r.polls()
	if polls[0]["offset"] != nil || polls[0]["limit"] != float64(100) || polls[0]["timeout"] != float64(30) {
		t.Errorf("first read %v", polls[0])
	}
	if want := []any{"message", "edited_message", "channel_post", "edited_channel_post", "message_reaction", "callback_query"}; !slices.Equal(polls[0]["allowed_updates"].([]any), want) {
		t.Errorf("allowed_updates %v", polls[0]["allowed_updates"])
	}
	if len(polls) < 2 || polls[1]["offset"] != float64(13) {
		t.Errorf("second read %v, want offset 13: one past the last update handled", polls)
	}
	if sink.cursor != "12" {
		t.Errorf("cursor %q, want the last update's ID", sink.cursor)
	}
}

func TestReceiverResumesFromTheCursor(t *testing.T) {
	r := newRig(t)
	r.queue(msgUpdate(42, "one"), msgUpdate(43, "two"))
	sink := &cursorSink{cursor: "41"}
	_ = r.run(t, sink, 2)
	if p := r.polls()[0]; p["offset"] != float64(42) {
		t.Errorf("first read %v, want offset 42 after cursor 41", p)
	}
	// Garbage in the cursor is no offset.
	r2 := newRig(t)
	sink2 := &cursorSink{cursor: "not a number"}
	r2.queue(msgUpdate(10, "one"))
	_ = r2.run(t, sink2, 1)
	if p := r2.polls()[0]; p["offset"] != nil {
		t.Errorf("first read %v, want no offset", p)
	}
}

// An update reports several envelopes, and the cursor rides on the last, so a
// stop between them resumes at the update, not after it.
func TestCursorRidesOnTheLastEnvelopeOfAnUpdate(t *testing.T) {
	r := newRig(t)
	emoji := func(e string) []obj { return []obj{{"type": "emoji", "emoji": e}} }
	r.queue(updateOf(20, "message_reaction", obj{
		"chat": privateChat(testPerson), "message_id": 9, "user": person(testPerson), "date": testNow.Unix(),
		"old_reaction": emoji("👍"), "new_reaction": emoji("🔥"),
	}))
	sink := &cursorSink{}
	_ = r.run(t, sink, 2)
	if len(sink.got) != 2 || sink.got[0].ReceiverCursor != "" || sink.got[1].ReceiverCursor != "20" {
		t.Fatalf("cursors %q %q, want none then 20", sink.got[0].ReceiverCursor, sink.got[1].ReceiverCursor)
	}
}

func TestUpdatesThatMakeNothingAreConfirmedByTheNextRead(t *testing.T) {
	r := newRig(t)
	r.queue(obj{"update_id": 30, "my_chat_member": obj{}}, msgUpdate(31, "kept"),
		updateOf(32, "message", obj{"text": "malformed"}), msgUpdate(33, "after"))
	sink := &cursorSink{}
	_ = r.run(t, sink, 2)
	if got := sink.envelopes(); len(got) != 2 || got[0].Content.Parts[0].Text != "kept" || got[1].Content.Parts[0].Text != "after" {
		t.Fatalf("envelopes %+v: ignored and malformed updates are skipped", got)
	}
	if polls := r.polls(); polls[1]["offset"] != float64(34) {
		t.Errorf("reads %v, want the skipped updates confirmed too", polls)
	}
	if sink.cursor != "33" {
		t.Errorf("cursor %q", sink.cursor)
	}
}

func TestReceiverFallsBackAndDropsLikeTheWebhook(t *testing.T) {
	r := newRig(t)
	edit := textMessage(groupChat(), person(testPerson), 9, "edited", obj{"edit_date": testNow.Unix() + 60})
	emoji := []obj{{"type": "emoji", "emoji": "👍"}}
	r.queue(updateOf(40, "edited_message", edit),
		updateOf(41, "message_reaction", obj{"chat": groupChat(), "message_id": 9, "user": person(testPerson), "date": testNow.Unix(), "old_reaction": []obj{}, "new_reaction": emoji}),
		msgUpdate(42, "last"))
	sink := &cursorSink{}
	sink.fail = func(in model.Ingress) error {
		if k := in.Envelope.Kind; k == model.EnvelopeMessageEdited || k == model.EnvelopeReactionAdded {
			return model.NewError(model.NotFound, "no such message")
		}
		return nil
	}
	_ = r.run(t, sink, 2)
	if len(sink.got) != 2 || sink.got[0].Envelope.Kind != model.EnvelopeMessage || sink.got[0].Envelope.MessageID != "-1001234567890:9" {
		t.Fatalf("ingress %+v: the edit of an unknown message records the message", sink.got)
	}
	if sink.got[0].ReceiverCursor != "40" {
		t.Errorf("the fallback carries cursor %q, want 40", sink.got[0].ReceiverCursor)
	}
}

func TestSinkRefusalEndsTheRunAndLeavesTheUpdateUnconfirmed(t *testing.T) {
	r := newRig(t)
	r.queue(msgUpdate(50, "one"), msgUpdate(51, "refused"), msgUpdate(52, "three"))
	sink := &cursorSink{}
	refusal := model.NewError(model.Unavailable, "store down")
	sink.fail = func(in model.Ingress) error {
		if in.Envelope.Content.Parts[0].Text == "refused" {
			return refusal
		}
		return nil
	}
	err := r.run(t, sink, 99)
	if !model.IsCode(err, model.Unavailable) {
		t.Fatalf("Run = %v, want the refusal", err)
	}
	if len(sink.got) != 1 || sink.cursor != "50" {
		t.Fatalf("ingress %d cursor %q: only the first was accepted", len(sink.got), sink.cursor)
	}
	// Telegram still holds 51 and 52: a new connection from cursor 50 reads
	// them again.
	sink.fail = nil
	_ = r.run(t, sink, 3)
	got := sink.envelopes()
	if len(got) != 3 || got[1].Content.Parts[0].Text != "refused" || got[2].Content.Parts[0].Text != "three" {
		t.Fatalf("after a reconnect: %+v", got)
	}
}

func TestReceiverFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(r *rig)
		code  model.ErrorCode
	}{
		"webhook set":  {func(r *rig) { r.acct.webhook = true }, model.Conflict},
		"bad token":    {func(r *rig) { r.creds.token = "wrong" }, model.Unauthorized},
		"server error": {func(r *rig) { r.respondGetUpdates(failWith(502, "Bad Gateway")) }, model.Transient},
		"unreachable":  {func(r *rig) { r.respondGetUpdates(unreachable) }, model.Transient},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			tc.setup(r)
			err := r.run(t, &cursorSink{}, 0)
			if !model.IsCode(err, tc.code) {
				t.Fatalf("Run = %v, want %s", err, tc.code)
			}
			if n := len(r.callsOf("getUpdates")); n > 2 {
				t.Errorf("%d reads: a refusal that retrying cannot cure must end the run", n)
			}
		})
	}
}

// respondGetUpdates scripts an answer to getUpdates, which the fake does not
// count among effects.
func (r *rig) respondGetUpdates(s scripted) { r.f.respondPolls("acct1", s) }

func TestFloodControlIsWaitedOut(t *testing.T) {
	r := newRig(t)
	r.queue(msgUpdate(60, "one"))
	r.respondGetUpdates(rateLimitedFor(7 * time.Second))
	sink := &cursorSink{}
	var waits []time.Duration
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	r.x.wait = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		if len(sink.got) > 0 {
			cancel()
		}
		return ctx.Err()
	}
	_ = r.x.Run(ctx, sink)
	if len(sink.got) != 1 || len(waits) == 0 || waits[0] != 7*time.Second {
		t.Fatalf("waits %v ingress %d, want a 7s wait and then the update", waits, len(sink.got))
	}
}

func TestReceiverPacesAnIdleServer(t *testing.T) {
	r := newRig(t)
	r.x.opts.PollInterval = 250 * time.Millisecond
	r.x.opts.Jitter = func() float64 { return 0.5 }
	var waits []time.Duration
	var mu sync.Mutex
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r.x.wait = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		n := len(waits)
		mu.Unlock()
		if n >= 2 {
			cancel()
		}
		return ctx.Err()
	}
	if err := r.x.Run(ctx, &cursorSink{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	for _, d := range waits {
		if d <= 0 || d > 250*time.Millisecond {
			t.Errorf("wait %v, want the rest of 250ms", d)
		}
	}
	if len(r.callsOf("getUpdates")) < 2 {
		t.Error("the receiver did not read again")
	}
}

func TestPollTimeoutIsConfigurable(t *testing.T) {
	r := newRig(t, func(c *Config) { c.PollTimeoutSeconds = 9 })
	r.queue(msgUpdate(70, "x"))
	_ = r.run(t, &cursorSink{}, 1)
	if p := r.polls()[0]; p["timeout"] != float64(9) {
		t.Errorf("read %v, want timeout 9", p)
	}
}

func TestRunOnAnUnreadyInstance(t *testing.T) {
	r := newRig(t)
	_ = r.x.Close(t.Context())
	err := r.x.Run(t.Context(), &cursorSink{})
	_ = wantErr(t, err, model.Unavailable)
}

// linkEvents is the order of what the receiver reported and handed over.
type linkEvents struct {
	mu  sync.Mutex
	log []string
}

func (l *linkEvents) add(e string) {
	l.mu.Lock()
	l.log = append(l.log, e)
	l.mu.Unlock()
}

func (l *linkEvents) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.log)
}

// eventSink logs each accepted ingress into events.
type eventSink struct {
	cursorSink
	events *linkEvents
}

func (s *eventSink) Accept(ctx context.Context, in model.Ingress) (model.IngressReceipt, error) {
	s.events.add("accept")
	return s.cursorSink.Accept(ctx, in)
}

// Connected is a poll answered: not before the first read, not after a
// flood-limited one, and before what the answer carried is handed over.
func TestReceiverReportsItsLink(t *testing.T) {
	r := newRig(t)
	ev := &linkEvents{}
	r.x.ReportReceiver(func(c bool) { ev.add("link:" + map[bool]string{true: "up", false: "down"}[c]) })
	r.respondGetUpdates(rateLimitedFor(time.Second))
	r.queue(msgUpdate(80, "one"))
	sink := &eventSink{events: ev}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	r.x.wait = func(ctx context.Context, d time.Duration) error {
		ev.add("wait")
		sink.mu.Lock()
		have := len(sink.got)
		sink.mu.Unlock()
		if have > 0 && d != time.Second {
			cancel()
		}
		return ctx.Err()
	}
	if err := r.x.Run(ctx, sink); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want the cancellation", err)
	}
	want := []string{"link:down", "wait", "link:up", "accept", "link:up", "wait", "link:down"}
	if got := ev.all(); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// An answer with no update is still a link, and a failing read ends it.
func TestReceiverIdlePollIsConnectedAndAFailureIsNot(t *testing.T) {
	r := newRig(t)
	ev := &linkEvents{}
	r.x.ReportReceiver(func(c bool) { ev.add(map[bool]string{true: "up", false: "down"}[c]) })
	r.respondGetUpdates(scripted{status: 200, body: `{"ok":true,"result":[]}`})
	r.respondGetUpdates(failWith(502, "Bad Gateway"))
	err := r.x.Run(t.Context(), &cursorSink{})
	if !model.IsCode(err, model.Transient) {
		t.Fatalf("Run = %v, want transient", err)
	}
	if got, want := ev.all(), []string{"up", "down", "down"}; !slices.Equal(got, want) {
		t.Fatalf("reports = %v, want %v: up for the empty answer, down for the failed read and for Run's end", got, want)
	}
}
