package google

import (
	"slices"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// states are the conversation.state envelopes the sink took, in order.
func (s *testSink) states() []model.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.Envelope
	for _, in := range s.got {
		if in.Envelope.Kind == model.EnvelopeConversationState {
			out = append(out, in.Envelope)
		}
	}
	return out
}

// synced opens a mailbox with the threads of threadEnv, and runs the first
// receiver round (a full synchronization), so the cursor is stored and the
// next round reads history.
func syncedEnv(t *testing.T) (*env, *testSink) {
	t.Helper()
	e := threadEnv(t)
	e.f.opts.PageSize = 100 // a page of history holds the records a test makes
	sink := &testSink{}
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatalf("first round: %v", err)
	}
	sink.mu.Lock()
	sink.got = nil
	sink.mu.Unlock()
	return e, sink
}

func (e *env) round(t *testing.T, sink *testSink) {
	t.Helper()
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatalf("round: %v", err)
	}
}

func wantOneState(t *testing.T, sink *testSink, conv string) model.ProviderStateReport {
	t.Helper()
	got := sink.states()
	if len(got) != 1 {
		t.Fatalf("conversation.state envelopes = %d (%+v), want one", len(got), got)
	}
	env := got[0]
	if env.State == nil || env.State.Conversation != model.ProviderMessageRef(testChan, conv) {
		t.Fatalf("state = %+v, want the conversation named by message %s", env.State, conv)
	}
	if env.EventID == "" || env.MessageID != "" || env.Destination != testMailbox || !env.Sender.IsZero() || len(env.Content.Parts) != 0 {
		t.Fatalf("envelope = %+v", env)
	}
	if err := env.State.Validate(); err != nil {
		t.Fatalf("report is invalid: %v", err)
	}
	return *env.State
}

// A label delta in history becomes one state envelope for the thread, read
// from the thread as Gmail holds it now: every message of it.
func TestHistoryLabelDeltaReportsThreadState(t *testing.T) {
	e, sink := syncedEnv(t)
	e.g.relabelThread(testMailbox, "t1", []string{"STARRED", "Label_7"}, []string{"INBOX"})
	before := len(e.g.requests())
	e.round(t, sink)
	st := wantOneState(t, sink, "m2")
	if !st.State.Archived || st.State.Trashed || !st.State.Complete || st.State.ObservedAt != testNow {
		t.Errorf("state = %+v, want archived, not trashed, complete, observed at the receiver's clock", st.State)
	}
	if st.State.Read == nil || *st.State.Read {
		t.Errorf("read = %v, want unread: the messages still carry UNREAD", st.State.Read)
	}
	if !slices.Equal(st.State.Flags, []string{model.FlagStar}) {
		t.Errorf("flags = %v, want star", st.State.Flags)
	}
	if !slices.Equal(st.State.Labels, []model.ProviderLabel{{ID: "Label_7", Name: "Label_7"}}) || st.State.Folder != nil {
		t.Errorf("labels = %+v, want the user label alone", st.State.Labels)
	}
	reqs := strings.Join(e.reqs("GET "+mbox+"/history"), "\n")
	var reads []string
	for _, r := range e.g.requests()[before:] {
		if strings.HasPrefix(r, "GET "+mbox+"/threads/") {
			reads = append(reads, r)
		}
	}
	for _, ty := range []string{"messageAdded", "labelAdded", "labelRemoved", "messageDeleted"} {
		if !strings.Contains(reqs, "historyTypes="+ty) {
			t.Errorf("history request %q lacks historyTypes=%s", reqs, ty)
		}
	}
	if strings.Contains(reqs, "labelId=") {
		t.Errorf("history request %q filters on a label: a change to a message outside the inbox would be missed", reqs)
	}
	if len(reads) != 1 || !strings.Contains(reads[0], "/threads/t1?format=metadata") {
		t.Errorf("thread reads = %q, want one metadata read of t1: four records, one thread", reads)
	}
}

func TestHistoryStateMapping(t *testing.T) {
	for _, c := range []struct {
		name        string
		thread      string
		add, remove []string
		check       func(t *testing.T, s model.ProviderConversationState)
	}{
		{"trash", "t2", []string{"TRASH"}, []string{"INBOX"}, func(t *testing.T, s model.ProviderConversationState) {
			if !s.Trashed || !s.Archived {
				t.Errorf("state = %+v, want trashed, and archived: it has left the inbox", s)
			}
		}},
		{"spam is not trashed", "t2", []string{"SPAM"}, []string{"INBOX"}, func(t *testing.T, s model.ProviderConversationState) {
			if s.Trashed || !s.Archived || len(s.Labels) != 0 {
				t.Errorf("state = %+v", s)
			}
		}},
		{"read", "t2", nil, []string{"UNREAD"}, func(t *testing.T, s model.ProviderConversationState) {
			if s.Read == nil || !*s.Read {
				t.Errorf("read = %v, want read", s.Read)
			}
		}},
		{"important", "t2", []string{"IMPORTANT"}, nil, func(t *testing.T, s model.ProviderConversationState) {
			if !slices.Equal(s.Flags, []string{model.FlagImportant}) {
				t.Errorf("flags = %v", s.Flags)
			}
		}},
		{"both flags", "t2", []string{"STARRED", "IMPORTANT"}, nil, func(t *testing.T, s model.ProviderConversationState) {
			if !slices.Equal(s.Flags, []string{model.FlagStar, model.FlagImportant}) {
				t.Errorf("flags = %v", s.Flags)
			}
		}},
		{"labels are sorted and carry no system label", "t2", []string{"Label_9", "CATEGORY_UPDATES", "Label_1"}, nil, func(t *testing.T, s model.ProviderConversationState) {
			want := []model.ProviderLabel{{ID: "CATEGORY_UPDATES", Name: "CATEGORY_UPDATES"}, {ID: "Label_1", Name: "Label_1"}, {ID: "Label_9", Name: "Label_9"}}
			if !slices.Equal(s.Labels, want) {
				t.Errorf("labels = %+v, want %+v", s.Labels, want)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, sink := syncedEnv(t)
			e.g.relabelThread(testMailbox, c.thread, c.add, c.remove)
			e.round(t, sink)
			c.check(t, wantOneState(t, sink, "m3").State)
		})
	}
	t.Run("a thread is read only when every message is", func(t *testing.T) {
		e, sink := syncedEnv(t)
		e.g.mu.Lock()
		e.g.relabel(e.box, e.box.messages["m1"], nil, []string{"UNREAD"})
		e.g.mu.Unlock()
		e.round(t, sink)
		st := sink.states()
		if len(st) != 1 || st[0].State.State.Read == nil || *st[0].State.State.Read {
			t.Fatalf("states = %+v, want unread: m2 was never read", st)
		}
	})
	t.Run("a draft of the thread is not part of its state", func(t *testing.T) {
		e, sink := syncedEnv(t)
		e.g.deliver(testMailbox, "d1", "t2", rawMessage(testMailbox, testPeer, "<d1@remote.test>", "re", "draft"), "DRAFT")
		e.g.relabelThread(testMailbox, "t2", nil, []string{"UNREAD"})
		e.round(t, sink)
		st := wantOneState(t, sink, "m3").State
		if st.Read == nil || !*st.Read {
			t.Errorf("read = %v, want read: the draft carries no UNREAD and is not counted", st.Read)
		}
	})
}

// A burst of changes on one thread within a page of history is one
// envelope; two threads are two.
func TestHistoryOneStatePerThreadPerPage(t *testing.T) {
	e, sink := syncedEnv(t)
	e.g.relabelThread(testMailbox, "t1", nil, []string{"INBOX"}) // two records
	e.round(t, sink)
	if n := len(sink.states()); n != 1 {
		t.Fatalf("states = %d, want one for the two records of the thread", n)
	}
	e.g.relabelThread(testMailbox, "t2", []string{"STARRED"}, nil)
	e.g.relabelThread(testMailbox, "t1", []string{"STARRED"}, nil)
	e.round(t, sink)
	got := sink.states()
	if len(got) != 3 || got[1].State.Conversation.ID != "m3" || got[2].State.Conversation.ID != "m2" {
		t.Fatalf("states = %+v, want t2 then t1 in history order", got)
	}
	if got[0].EventID == got[1].EventID || got[1].EventID == got[2].EventID {
		t.Fatalf("event IDs %q %q %q are not distinct", got[0].EventID, got[1].EventID, got[2].EventID)
	}
}

// The adapter's own change comes back through history as the provider's
// view of the thread, which is the echo the engine settles it from.
func TestHistoryEchoesOwnChange(t *testing.T) {
	e, sink := syncedEnv(t)
	out, err := e.inst.Archive(t.Context(), adapter.ArchiveRequest{Effect: effect(route(testPeer, "m1"), "a"), Items: items("m1")})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Archive = %+v, %v", out, err)
	}
	e.round(t, sink)
	if st := wantOneState(t, sink, "m2").State; !st.Archived {
		t.Fatalf("echo = %+v, want archived", st)
	}
	// The same history again says nothing new.
	sink.mu.Lock()
	sink.got = nil
	sink.mu.Unlock()
	e.round(t, sink)
	if n := len(sink.got); n != 0 {
		t.Fatalf("second round handed over %d envelopes", n)
	}
}

func TestHistoryMessageDeleted(t *testing.T) {
	t.Run("the thread keeps other messages", func(t *testing.T) {
		e, sink := syncedEnv(t)
		e.g.remove(testMailbox, "m2")
		e.round(t, sink)
		wantOneState(t, sink, "m1")
	})
	t.Run("the thread is gone", func(t *testing.T) {
		e, sink := syncedEnv(t)
		e.g.remove(testMailbox, "m3")
		e.round(t, sink)
		if n := len(sink.states()); n != 0 {
			t.Fatalf("states = %+v, want none for a thread Gmail no longer holds", sink.states())
		}
		st, err := e.inst.loadState(t.Context())
		if err != nil || int64(st.HistoryID) != e.box.historyID {
			t.Fatalf("cursor %d, %v; want it past the record, at %d", st.HistoryID, err, e.box.historyID)
		}
	})
}

// A refused envelope holds the cursor before its record, so the next round
// hands it over again.
func TestHistoryRefusedStateIsReadAgain(t *testing.T) {
	e, sink := syncedEnv(t)
	e.g.relabelThread(testMailbox, "t2", []string{"STARRED"}, nil)
	refuse := true
	sink.refuse = func(in model.Ingress) error {
		if refuse && in.Envelope.Kind == model.EnvelopeConversationState {
			return model.NewError(model.Unavailable, "sink down")
		}
		return nil
	}
	if _, err := e.inst.round(t.Context(), sink); err == nil {
		t.Fatal("round succeeded past a refused state")
	}
	refuse = false
	e.round(t, sink)
	wantOneState(t, sink, "m3")
}

// The first synchronization reports the state of the threads it brings in,
// after their messages.
func TestFullSyncReportsState(t *testing.T) {
	e := threadEnv(t)
	e.g.relabelThread(testMailbox, "t2", []string{"STARRED"}, nil)
	sink := &testSink{}
	e.round(t, sink)
	var kinds []string
	for _, in := range sink.got {
		kinds = append(kinds, in.Envelope.Kind)
	}
	if len(kinds) != 5 || kinds[3] != model.EnvelopeConversationState || kinds[4] != model.EnvelopeConversationState {
		t.Fatalf("kinds = %v, want three messages then a state for each of the two threads", kinds)
	}
	got := sink.states()
	if got[0].State.Conversation.ID != "m2" || got[1].State.Conversation.ID != "m3" || !slices.Equal(got[1].State.State.Flags, []string{model.FlagStar}) {
		t.Fatalf("states = %+v", got)
	}
}

// A new message is a message envelope and nothing else: its labels are in
// its payload, and no state follows from messageAdded alone.
func TestHistoryNewMessageCarriesNoState(t *testing.T) {
	e, sink := syncedEnv(t)
	e.g.deliver(testMailbox, "m9", "t9", rawMessage(testPeer, testMailbox, "<m9@remote.test>", "hi", "text"))
	e.g.deliver(testMailbox, "s1", "t8", rawMessage(testPeer, testMailbox, "<s1@remote.test>", "spam", "text"), "SPAM")
	e.round(t, sink)
	if got := sink.ids(); !slices.Equal(got, []string{"m9"}) {
		t.Fatalf("envelopes = %v, want m9 only: a message outside the inbox is not inbound mail", got)
	}
}
