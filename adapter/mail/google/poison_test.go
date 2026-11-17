package google

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// observer counts observations by kind.
type observer struct {
	mu   sync.Mutex
	seen map[string]int
}

func (o *observer) Observe(_ context.Context, ob ports.Observation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seen == nil {
		o.seen = map[string]int{}
	}
	o.seen[ob.Kind]++
}

func (o *observer) count(kind string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.seen[kind]
}

func withObserver(o ports.Observer) envOption {
	return func(_ *Config, d *adapter.Dependencies) { d.Observer = o }
}

// undecodableRaw is a users.messages.get answer for id whose raw field is
// not base64url: whole, well-formed JSON naming the message asked for.
func undecodableRaw(id string) string {
	return `{"id":"` + id + `","threadId":"thread-` + id + `","labelIds":["INBOX","UNREAD"],"internalDate":"1791806400000","raw":"@@ not base64url @@"}`
}

// answerFor makes g answer GET /messages/<id> in format with status and
// body, for as long as on reports true. It returns how many such answers
// were served.
func answerFor(g *fakeGmail, user, id, format string, status int, body string, on func() bool) *atomic.Int32 {
	return answerWhen(g, func(r *http.Request) bool {
		return r.Method == http.MethodGet && r.URL.Path == "/gmail/v1/users/"+user+"/messages/"+id && r.URL.Query().Get("format") == format
	}, status, body, on)
}

// answerWhen makes g answer every request match reports with status and
// body, for as long as on reports true. It returns how many such answers
// were served.
func answerWhen(g *fakeGmail, match func(*http.Request) bool, status int, body string, on func() bool) *atomic.Int32 {
	var served atomic.Int32
	g.mu.Lock()
	defer g.mu.Unlock()
	g.override = func(w http.ResponseWriter, r *http.Request) bool {
		if !match(r) || !on() {
			return false
		}
		served.Add(1)
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
		return true
	}
	return &served
}

func always() bool { return true }

// A message whose raw field is not base64url can never be read: Gmail does
// not change a message's bytes, and the answer carrying it is whole. It is
// skipped and observed, and the cursor moves past it, so the messages after
// it arrive and it is not fetched again.
func TestUndecodableRawIsSkippedAndObserved(t *testing.T) {
	obs := &observer{}
	e := newEnv(t, Config{}, withObserver(obs))
	sink := &testSink{}
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	e.inbound("m1", testPeer, "before")
	e.inbound("bad", testPeer, "never read")
	e.inbound("m3", testPeer, "after")
	answerFor(e.g, testMailbox, "bad", "raw", http.StatusOK, undecodableRaw("bad"), always)
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatalf("round = %v, want the undecodable message skipped", err)
	}
	if got := sink.ids(); !slices.Equal(got, []string{"m1", "m3"}) {
		t.Fatalf("accepted %v, want m1 and m3", got)
	}
	if st := e.st(t); st.HistoryID != 1003 {
		t.Fatalf("historyId = %d, want 1003: the cursor moves past the skipped message", st.HistoryID)
	}
	if n := obs.count(ObservationSkipped); n != 1 {
		t.Fatalf("%s observations = %d, want 1", ObservationSkipped, n)
	}
	// The next round starts after it.
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	if n := e.g.count("GET /gmail/v1/users/" + testMailbox + "/messages/bad?format=raw"); n != 1 {
		t.Errorf("raw reads of the skipped message = %d, want 1", n)
	}
	if n := obs.count(ObservationSkipped); n != 1 {
		t.Errorf("%s observations = %d after another round, want 1", ObservationSkipped, n)
	}
}

// An answer that is not the message asked for, whole, is a fault of the
// response, not of the message: a proxy's error page, a truncated body, an
// answer without the message's ID or raw form. The fetch fails as transient,
// the cursor stays before the message, nothing is observed as skipped, and
// the message arrives once Gmail answers it.
func TestUnexpectedAnswerHoldsHistory(t *testing.T) {
	cases := map[string]struct {
		format string
		status int
		body   string
	}{
		"html page":           {"raw", http.StatusOK, "<html><body>502 Bad Gateway</body></html>"},
		"truncated json":      {"raw", http.StatusOK, `{"id":"bad","threadId":"thread-bad","raw":"RnJvbTog`},
		"empty body":          {"raw", http.StatusOK, ""},
		"no message id":       {"raw", http.StatusOK, `{}`},
		"another message":     {"raw", http.StatusOK, `{"id":"other","labelIds":["INBOX"],"raw":"RnJvbTogYUBiLmM"}`},
		"no raw form":         {"raw", http.StatusOK, `{"id":"bad","labelIds":["INBOX"]}`},
		"metadata html":       {"metadata", http.StatusOK, "<html><body>502 Bad Gateway</body></html>"},
		"metadata no id":      {"metadata", http.StatusOK, `{"payload":{"headers":[]}}`},
		"metadata other id":   {"metadata", http.StatusOK, `{"id":"other","payload":{"headers":[]}}`},
		"metadata no payload": {"metadata", http.StatusOK, `{"id":"bad","labelIds":["INBOX"],"sizeEstimate":"5000"}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			obs := &observer{}
			e := newEnv(t, Config{}, withObserver(obs))
			sink := &testSink{}
			if _, err := e.inst.round(t.Context(), sink); err != nil {
				t.Fatal(err)
			}
			e.inbound("m1", testPeer, "before")
			if c.format == "metadata" {
				// Over the raw bound: the headers are read instead.
				e.inst.c.rawLimit = smallRawLimit
				e.g.deliver(e.box.user, "bad", "thread-bad", bigMessage("bad"))
			} else {
				e.inbound("bad", testPeer, "garbled once")
			}
			e.inbound("m3", testPeer, "after")
			var failing atomic.Bool
			failing.Store(true)
			served := answerFor(e.g, testMailbox, "bad", c.format, c.status, c.body, failing.Load)
			_, err := e.inst.round(t.Context(), sink)
			if model.CodeOf(err) != model.Transient {
				t.Fatalf("round = %v, want a transient error", err)
			}
			if me, ok := err.(*model.Error); !ok || me.Retry != model.RetrySameOperation {
				t.Fatalf("round = %v, want the same operation retried", err)
			}
			if served.Load() == 0 {
				t.Fatal("the scripted answer was never served")
			}
			if st := e.st(t); st.HistoryID >= 1002 {
				t.Fatalf("historyId = %d: the cursor passed a message not taken", st.HistoryID)
			}
			if ids := sink.ids(); slices.Contains(ids, "bad") || slices.Contains(ids, "m3") {
				t.Fatalf("accepted %v: bad and m3 are not taken yet", ids)
			}
			if n := obs.count(ObservationSkipped); n != 0 {
				t.Fatalf("%s observations = %d: a garbled answer was taken for a poison message", ObservationSkipped, n)
			}
			failing.Store(false)
			if _, err := e.inst.round(t.Context(), sink); err != nil {
				t.Fatal(err)
			}
			got := sink.ids()
			for _, id := range []string{"m1", "bad", "m3"} {
				if !slices.Contains(got, id) {
					t.Fatalf("accepted %v, want %s after the repeat", got, id)
				}
			}
			if st := e.st(t); st.HistoryID != 1003 {
				t.Fatalf("historyId = %d, want 1003", st.HistoryID)
			}
		})
	}
}

// A message whose raw form does not parse is skipped as before, and now
// observed.
func TestUnparseableMessageObserved(t *testing.T) {
	obs := &observer{}
	e := newEnv(t, Config{}, withObserver(obs))
	e.g.mu.Lock()
	e.box.messages["bad"] = &fakeMsg{id: "bad", labels: []string{labelInbox}, raw: []byte("no header terminator \x00"), internal: testNow}
	e.g.mu.Unlock()
	sink := &testSink{}
	if err := e.inst.ingest(t.Context(), e.inst.c.session(), sink, "bad", true); err != nil || len(sink.ids()) != 0 {
		t.Fatalf("ingest = %v, accepted %v; want the message skipped", err, sink.ids())
	}
	if n := obs.count(ObservationSkipped); n != 1 {
		t.Fatalf("%s observations = %d, want 1", ObservationSkipped, n)
	}
	// Gone, a draft or sent by the mailbox: nothing to accept, nothing
	// observed.
	e.g.deliver(e.box.user, "draft", "thread-d", rawMessage(testMailbox, testPeer, "<d@example.test>", "draft", "x"), labelDraft)
	for _, id := range []string{"gone", "draft"} {
		if err := e.inst.ingest(t.Context(), e.inst.c.session(), sink, id, true); err != nil || len(sink.ids()) != 0 {
			t.Fatalf("%s = %v, accepted %v; want skip", id, err, sink.ids())
		}
	}
	if n := obs.count(ObservationSkipped); n != 1 {
		t.Fatalf("%s observations = %d, want 1: only an unreadable message is observed", ObservationSkipped, n)
	}
}

// Over the real engine: an undecodable message is skipped and observed, the
// messages around it are stored, and the receiver keeps running.
func TestEngineUndecodableRawDoesNotStopTheMailbox(t *testing.T) {
	obs := &observer{}
	r := newEngineRigObserved(t, obs)
	r.seed(testChan, engineInboxA, testMailbox, "support@example.test")
	ended := r.receive(testChan, testMailbox)
	answerFor(r.g, testMailbox, "bad", "raw", http.StatusOK, undecodableRaw("bad"), always)

	r.g.deliver(testMailbox, "m1", "thread-1", rawMessage("jane@example.org", "support@example.test", "<t1@example.org>", "Help", "before"))
	r.g.deliver(testMailbox, "bad", "thread-bad", rawMessage("jane@example.org", "support@example.test", "<tb@example.org>", "Help", "never read"))
	r.g.deliver(testMailbox, "m3", "thread-3", rawMessage("jane@example.org", "support@example.test", "<t3@example.org>", "Help 3", "after"))
	if code := r.push(testChan, testMailbox); code != http.StatusNoContent {
		t.Fatalf("push = %d", code)
	}
	msgs := r.waitMessages(engineInboxA, 2)
	if byProviderID(msgs, testChan, "m1") == nil || byProviderID(msgs, testChan, "m3") == nil || byProviderID(msgs, testChan, "bad") != nil {
		t.Fatalf("messages = %+v, want m1 and m3 only", msgs)
	}
	waitFor(t, func() bool { return obs.count(ObservationSkipped) > 0 })

	// The receiver is still running: a later message arrives, by push and
	// by the poll round it wakes.
	r.g.deliver(testMailbox, "m4", "thread-4", rawMessage("jane@example.org", "support@example.test", "<t4@example.org>", "Help 4", "later"))
	if code := r.push(testChan, testMailbox); code != http.StatusNoContent {
		t.Fatalf("push = %d", code)
	}
	r.waitMessages(engineInboxA, 3)
	select {
	case err := <-ended:
		t.Fatalf("Receive returned %v: the mailbox stopped", err)
	case <-time.After(20 * time.Millisecond):
	}
}

// Over the real engine: a garbled answer for one message fails the round as
// transient; the receiver reconnects, reads the message again from the held
// cursor and stores it with the ones after it. Nothing is lost or skipped.
func TestEngineUnexpectedAnswerIsRetried(t *testing.T) {
	obs := &observer{}
	r := newEngineRigObserved(t, obs)
	r.seed(testChan, engineInboxA, testMailbox, "support@example.test")
	ended := r.receive(testChan, testMailbox)
	// The push handler and two receiver rounds meet the garbled answer.
	var left atomic.Int32
	left.Store(3)
	served := answerFor(r.g, testMailbox, "bad", "raw", http.StatusOK, "<html><body>502 Bad Gateway</body></html>", func() bool {
		return left.Add(-1) >= 0
	})

	r.g.deliver(testMailbox, "m1", "thread-1", rawMessage("jane@example.org", "support@example.test", "<t1@example.org>", "Help", "before"))
	r.g.deliver(testMailbox, "bad", "thread-bad", rawMessage("jane@example.org", "support@example.test", "<tb@example.org>", "Help 2", "garbled first"))
	r.g.deliver(testMailbox, "m3", "thread-3", rawMessage("jane@example.org", "support@example.test", "<t3@example.org>", "Help 3", "after"))
	if code := r.push(testChan, testMailbox); code != http.StatusServiceUnavailable {
		t.Fatalf("push = %d, want 503: Pub/Sub redelivers a notification the garbled answer failed", code)
	}
	msgs := r.waitMessages(engineInboxA, 3)
	bad := byProviderID(msgs, testChan, "bad")
	if bad == nil || byProviderID(msgs, testChan, "m1") == nil || byProviderID(msgs, testChan, "m3") == nil {
		t.Fatalf("messages = %+v, want m1, bad and m3", msgs)
	}
	if bad.Content.Parts[0].Text != "garbled first" {
		t.Errorf("bad = %+v, want its text", bad.Content)
	}
	if n := served.Load(); n != 3 {
		t.Errorf("garbled answers served = %d, want 3", n)
	}
	if n := obs.count(ObservationSkipped); n != 0 {
		t.Errorf("%s observations = %d: a garbled answer was taken for a poison message", ObservationSkipped, n)
	}
	select {
	case err := <-ended:
		t.Fatalf("Receive returned %v: the mailbox stopped", err)
	case <-time.After(20 * time.Millisecond):
	}
}
