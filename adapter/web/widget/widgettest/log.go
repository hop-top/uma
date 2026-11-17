// Package widgettest holds the shared test suite every widget.Log passes:
// widget.MemoryLog, the store-backed log in widget/storelog and a host's
// own implementation run the same cases.
//
//	func TestLog(t *testing.T) {
//		widgettest.RunLogSuite(t, func(t *testing.T, retain int) widgettest.LogFixture {
//			return widgettest.LogFixture{Log: mylog.New(retain)}
//		})
//	}
package widgettest

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
)

// LogFixture is one fresh, empty log under test.
type LogFixture struct {
	// Log is the implementation under test, keeping the retain entries per
	// visitor the factory was asked for. Required.
	Log widget.Log
	// Channel readies a channel the suite's keys name, such as storing it
	// in the store the log keeps entries in. Nil when any channel works.
	Channel func(t *testing.T, ref model.ChannelRef)
}

// LogFactory returns a fresh fixture keeping retain entries per visitor. It
// may register cleanup with t.Cleanup.
type LogFactory func(t *testing.T, retain int) LogFixture

// Tenants and the channel ID the suite's keys use. Both tenants hold a
// channel with the same ID, so a log that ignores the tenant mixes them.
const (
	TenantA model.TenantID = "widgettest-tenant-a"
	TenantB model.TenantID = "widgettest-tenant-b"
	Channel model.ID       = "00000000-0000-7000-8000-00000000c0f1"
)

// RunLogSuite runs the shared widget.Log cases against factory: gapless
// sequences per visitor key, reads after a cursor, retention of the latest
// entries with the reset condition the event stream detects, entry round
// trips and snapshots, tenant and key isolation, watches, cancellation and
// concurrent appends.
func RunLogSuite(t *testing.T, factory LogFactory) {
	t.Helper()
	for _, c := range logCases {
		t.Run(c.name, func(t *testing.T) {
			retain := c.retain
			if retain == 0 {
				retain = 100
			}
			fx := factory(t, retain)
			if fx.Log == nil {
				t.Fatal("LogFixture needs Log")
			}
			e := &logEnv{fx: fx, ctx: t.Context()}
			for _, tenant := range []model.TenantID{TenantA, TenantB} {
				if fx.Channel != nil {
					fx.Channel(t, model.ChannelRef{Tenant: tenant, ID: Channel})
				}
			}
			c.run(t, e)
		})
	}
}

type logCase struct {
	name   string
	retain int
	run    func(*testing.T, *logEnv)
}

type logEnv struct {
	fx  LogFixture
	ctx context.Context
}

// key is the log of visitor in tenant under the visitor namespace.
func key(tenant model.TenantID, visitor string) widget.LogKey {
	return widget.LogKey{Channel: model.ChannelRef{Tenant: tenant, ID: Channel}, Namespace: widget.NamespaceVisitor, Visitor: visitor}
}

func message(id string) widget.Entry {
	return widget.Entry{Event: widget.Event{Type: widget.EventMessage, Message: id,
		Parts: []widget.Part{{Kind: "text", Text: "text of " + id}}}}
}

func (e *logEnv) append(t *testing.T, k widget.LogKey, entry widget.Entry) uint64 {
	t.Helper()
	seq, err := e.fx.Log.Append(e.ctx, k, entry)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return seq
}

func (e *logEnv) read(t *testing.T, k widget.LogKey, after uint64, limit int) []widget.Entry {
	t.Helper()
	got, err := e.fx.Log.Read(e.ctx, k, after, limit)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return got
}

// wantSeqs requires the entries of k after the cursor to have seqs.
func (e *logEnv) wantSeqs(t *testing.T, k widget.LogKey, after uint64, seqs ...uint64) []widget.Entry {
	t.Helper()
	got := e.read(t, k, after, 0)
	if !slices.Equal(seqsOf(got), seqs) {
		t.Fatalf("Read(%s, after %d) seqs = %v, want %v", k.Visitor, after, seqsOf(got), seqs)
	}
	return got
}

func seqsOf(es []widget.Entry) []uint64 {
	out := make([]uint64, 0, len(es))
	for _, x := range es {
		out = append(out, x.Seq)
	}
	return out
}

// resets reports whether the event stream, reading entries after the
// cursor, sends a reset first.
func resets(entries []widget.Entry, after uint64) bool {
	return len(entries) > 0 && entries[0].Seq > after+1
}

var logCases = []logCase{
	{name: "SequencesAreGaplessPerKey", run: func(t *testing.T, e *logEnv) {
		a, b := key(TenantA, "visitor-a"), key(TenantA, "visitor-b")
		user := a
		user.Namespace = widget.NamespaceUser
		for i, c := range []struct {
			k    widget.LogKey
			want uint64
		}{{a, 1}, {a, 2}, {b, 1}, {a, 3}, {user, 1}, {b, 2}} {
			entry := message("m" + string(rune('a'+i)))
			entry.Seq = 99 // ignored
			if got := e.append(t, c.k, entry); got != c.want {
				t.Fatalf("append %d = seq %d, want %d", i, got, c.want)
			}
		}
		got := e.wantSeqs(t, a, 0, 1, 2, 3)
		if ids := []string{got[0].Event.Message, got[1].Event.Message, got[2].Event.Message}; !slices.Equal(ids, []string{"ma", "mb", "md"}) {
			t.Errorf("messages of visitor-a = %v, want [ma mb md]", ids)
		}
		e.wantSeqs(t, b, 0, 1, 2)
		e.wantSeqs(t, user, 0, 1)
	}},
	{name: "ReadsAfterTheCursor", run: func(t *testing.T, e *logEnv) {
		k := key(TenantA, "visitor")
		if got := e.read(t, k, 0, 0); len(got) != 0 {
			t.Fatalf("Read of an empty log = %+v", got)
		}
		for range 5 {
			e.append(t, k, message("m"))
		}
		e.wantSeqs(t, k, 0, 1, 2, 3, 4, 5)
		e.wantSeqs(t, k, 3, 4, 5)
		e.wantSeqs(t, k, 5)
		e.wantSeqs(t, k, 9)
		if got := seqsOf(e.read(t, k, 1, 2)); !slices.Equal(got, []uint64{2, 3}) {
			t.Errorf("Read(after 1, limit 2) seqs = %v, want [2 3]", got)
		}
	}},
	{name: "RetainKeepsTheLatestEntries", retain: 3, run: func(t *testing.T, e *logEnv) {
		k := key(TenantA, "visitor")
		for range 3 {
			e.append(t, k, message("m"))
		}
		if got := e.wantSeqs(t, k, 0, 1, 2, 3); resets(got, 0) {
			t.Error("a full log that dropped nothing resets a stream from the start")
		}
		for range 2 {
			e.append(t, k, message("m"))
		}
		// Entries 1 and 2 are gone: a cursor before 2 has missed one.
		for _, c := range []struct {
			after uint64
			want  []uint64
			reset bool
		}{{0, []uint64{3, 4, 5}, true}, {1, []uint64{3, 4, 5}, true}, {2, []uint64{3, 4, 5}, false}, {3, []uint64{4, 5}, false}} {
			got := e.read(t, k, c.after, 0)
			if !slices.Equal(seqsOf(got), c.want) {
				t.Errorf("Read(after %d) seqs = %v, want %v", c.after, seqsOf(got), c.want)
			}
			if resets(got, c.after) != c.reset {
				t.Errorf("Read(after %d): reset = %v, want %v", c.after, !c.reset, c.reset)
			}
		}
		// The next append continues the sequence and drops one more.
		if seq := e.append(t, k, message("m")); seq != 6 {
			t.Fatalf("append after trimming = seq %d, want 6", seq)
		}
		e.wantSeqs(t, k, 0, 4, 5, 6)
	}},
	{name: "EntriesRoundTrip", run: func(t *testing.T, e *logEnv) {
		k := key(TenantA, "visitor")
		expires := time.Date(2030, 1, 2, 3, 4, 5, 6000, time.UTC)
		want := []widget.Entry{
			{
				Event: widget.Event{Type: widget.EventMessage, Message: "pm-1", Parts: []widget.Part{
					{Kind: "text", Text: "hello"},
					{Kind: "attachment", Attachment: "00000000-0000-7000-8000-00000000c0f2"},
					{Kind: "structured", Structured: &model.Extension{Name: "widgettest.card", Version: 2, Data: json.RawMessage(`{"title":"t"}`)}},
				}},
				Session:   "session-1",
				Operation: model.OperationRef{Tenant: TenantA, ID: "00000000-0000-7000-8000-00000000c0f3"},
			},
			{Event: widget.Event{Type: widget.EventStreamUpdate, Message: "pm-2", Sequence: 7,
				Parts: []widget.Part{{Kind: "text", Text: "so far"}}}},
			{Event: widget.Event{Type: widget.EventSignal, Kind: "typing"}, ExpiresAt: expires},
			{
				Event: widget.Event{Type: widget.EventInteractionResponse, Message: "pm-3", Interaction: "vi-1", Mode: model.ResponseModeEphemeral,
					FollowUp: true, Parts: []widget.Part{{Kind: "text", Text: "only you"}}},
				Operation: model.OperationRef{Tenant: TenantA, ID: "00000000-0000-7000-8000-00000000c0f4"},
				Interaction: model.InteractionRef{
					Conversation: model.ConversationRef{Inbox: model.InboxRef{Tenant: TenantA, ID: "00000000-0000-7000-8000-00000000c0f5"}, ID: "00000000-0000-7000-8000-00000000c0f6"},
					ID:           "00000000-0000-7000-8000-00000000c0f7",
				},
				ExpiresAt: expires,
			},
		}
		for i := range want {
			want[i].Seq = e.append(t, k, want[i])
		}
		got := e.read(t, k, 0, 0)
		if len(got) != len(want) {
			t.Fatalf("Read = %d entries, want %d", len(got), len(want))
		}
		for i := range want {
			g, w := got[i], want[i]
			if !g.ExpiresAt.Equal(w.ExpiresAt) {
				t.Errorf("entry %d ExpiresAt = %v, want %v", i, g.ExpiresAt, w.ExpiresAt)
			}
			g.ExpiresAt, w.ExpiresAt = time.Time{}, time.Time{}
			if !reflect.DeepEqual(g, w) {
				t.Errorf("entry %d = %+v, want %+v", i, g, w)
			}
		}
	}},
	{name: "EntriesAreSnapshots", run: func(t *testing.T, e *logEnv) {
		k := key(TenantA, "visitor")
		in := widget.Entry{Event: widget.Event{Type: widget.EventMessage, Message: "pm", Parts: []widget.Part{
			{Kind: "structured", Structured: &model.Extension{Name: "widgettest.card", Version: 1, Data: json.RawMessage(`{"a":1}`)}},
		}}}
		e.append(t, k, in)
		in.Event.Parts[0].Structured.Data[5] = '2'
		in.Event.Parts[0].Text = "changed"
		got := e.read(t, k, 0, 0)
		got[0].Event.Parts[0].Structured.Data[5] = '3'
		got[0].Event.Parts[0].Kind = "changed"
		again := e.read(t, k, 0, 0)
		if p := again[0].Event.Parts[0]; p.Kind != "structured" || p.Text != "" || string(p.Structured.Data) != `{"a":1}` {
			t.Errorf("stored part = %+v (%s), want the appended one", p, p.Structured.Data)
		}
	}},
	{name: "TenantsAreIsolated", run: func(t *testing.T, e *logEnv) {
		a, b := key(TenantA, "visitor"), key(TenantB, "visitor")
		e.append(t, a, message("a1"))
		e.append(t, a, message("a2"))
		if seq := e.append(t, b, message("b1")); seq != 1 {
			t.Fatalf("first append in tenant B = seq %d, want 1", seq)
		}
		if got := e.wantSeqs(t, b, 0, 1); got[0].Event.Message != "b1" {
			t.Errorf("tenant B reads %q, want b1", got[0].Event.Message)
		}
		if got := e.wantSeqs(t, a, 0, 1, 2); got[0].Event.Message != "a1" || got[1].Event.Message != "a2" {
			t.Errorf("tenant A reads %q %q, want a1 a2", got[0].Event.Message, got[1].Event.Message)
		}
	}},
	{name: "WatchWakesOnAppendToItsKey", run: func(t *testing.T, e *logEnv) {
		k, other := key(TenantA, "visitor"), key(TenantA, "other")
		w, stop := e.fx.Log.Watch(k)
		ow, ostop := e.fx.Log.Watch(other)
		defer ostop()
		e.append(t, k, message("m"))
		select {
		case <-w:
		case <-time.After(5 * time.Second):
			t.Fatal("watcher not woken by an append to its key")
		}
		e.append(t, k, message("m"))
		e.append(t, k, message("m"))
		select {
		case <-ow:
			t.Error("watcher of another key woken")
		default:
		}
		// Wake-ups coalesce: at most one is pending.
		<-w
		select {
		case <-w:
			t.Error("two wake-ups pending; want them coalesced")
		default:
		}
		stop()
		stop() // idempotent
		e.append(t, k, message("m"))
		select {
		case <-w:
			t.Error("stopped watcher woken")
		case <-time.After(50 * time.Millisecond):
		}
	}},
	{name: "CancelledContextFails", run: func(t *testing.T, e *logEnv) {
		k := key(TenantA, "visitor")
		ctx, cancel := context.WithCancel(e.ctx)
		cancel()
		if _, err := e.fx.Log.Append(ctx, k, message("m")); err == nil {
			t.Error("Append with a cancelled context succeeded")
		}
		if _, err := e.fx.Log.Read(ctx, k, 0, 0); err == nil {
			t.Error("Read with a cancelled context succeeded")
		}
		if seq := e.append(t, k, message("m")); seq != 1 {
			t.Errorf("append after a cancelled one = seq %d, want 1", seq)
		}
	}},
	{name: "ConcurrentAppendsGetDistinctGaplessSequences", retain: 200, run: func(t *testing.T, e *logEnv) {
		k := key(TenantA, "visitor")
		const writers, each = 8, 10
		var (
			mu   sync.Mutex
			seqs []uint64
			wg   sync.WaitGroup
		)
		for range writers {
			wg.Go(func() {
				for range each {
					// A retryable failure stored nothing; retry it as the
					// engine would.
					for {
						seq, err := e.fx.Log.Append(e.ctx, k, message("m"))
						if err == nil {
							mu.Lock()
							seqs = append(seqs, seq)
							mu.Unlock()
							break
						}
						if c := model.CodeOf(err); c != model.Transient && c != model.Unavailable {
							t.Errorf("Append: %v", err)
							return
						}
					}
				}
			})
		}
		wg.Wait()
		slices.Sort(seqs)
		want := make([]uint64, writers*each)
		for i := range want {
			want[i] = uint64(i + 1) // #nosec G115 -- small positive
		}
		if !slices.Equal(seqs, want) {
			t.Fatalf("returned seqs = %v, want 1..%d once each", seqs, len(want))
		}
		e.wantSeqs(t, k, 0, want...)
	}},
}
