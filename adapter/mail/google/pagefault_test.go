package google

import (
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
)

// garbledBodies are answers that are not, whole, the JSON asked for: a
// proxy's error page, a truncated or empty body, JSON of another shape.
var garbledBodies = map[string]string{
	"html page":  "<html><body>502 Bad Gateway</body></html>",
	"truncated":  `{"history":[{"id":"1001","messagesAdded":[{"message":{"id":"m1"`,
	"empty body": "",
	"json array": `[]`,
}

// wantRetry fails the test unless err is transient, the same operation
// retried.
func wantRetry(t *testing.T, err error) {
	t.Helper()
	me, ok := err.(*model.Error)
	if !ok || me.Code != model.Transient || me.Retry != model.RetrySameOperation {
		t.Fatalf("err = %v, want a transient error, the same operation retried", err)
	}
}

// onPath matches requests for the user's API path, and with second only
// those of a later page (a pageToken), else only those of the first.
func onPath(path string, second bool) func(*http.Request) bool {
	return func(r *http.Request) bool {
		return r.URL.Path == "/gmail/v1/users/"+testMailbox+path && (r.URL.Query().Get("pageToken") != "") == second
	}
}

// A garbled history.list page is a fault of the response, as a garbled
// message answer is: the round fails as transient and the cursor stays
// where the last whole page left it, so nothing on the garbled page is
// lost or applied, and the next round reads it again.
func TestGarbledHistoryPageHoldsCursor(t *testing.T) {
	for _, second := range []bool{false, true} {
		for name, body := range garbledBodies {
			page := "first page/"
			if second {
				page = "second page/"
			}
			t.Run(page+name, func(t *testing.T) {
				e := newEnv(t, Config{})
				sink := &testSink{}
				if _, err := e.inst.round(t.Context(), sink); err != nil {
					t.Fatal(err)
				}
				for _, id := range []string{"m1", "m2", "m3"} {
					e.inbound(id, testPeer, id) // two pages of two
				}
				var failing atomic.Bool
				failing.Store(true)
				served := answerWhen(e.g, onPath("/history", second), http.StatusOK, body, failing.Load)
				_, err := e.inst.round(t.Context(), sink)
				wantRetry(t, err)
				if served.Load() == 0 {
					t.Fatal("the scripted answer was never served")
				}
				wantID, wantIDs := int64s(1000), []string(nil)
				if second {
					wantID, wantIDs = 1002, []string{"m1", "m2"}
				}
				if st := e.st(t); st.HistoryID != wantID {
					t.Fatalf("historyId = %d, want %d: the cursor stays after the last whole page", st.HistoryID, wantID)
				}
				if got := sink.ids(); !slices.Equal(got, wantIDs) {
					t.Fatalf("accepted %v, want %v", got, wantIDs)
				}
				failing.Store(false)
				if _, err := e.inst.round(t.Context(), sink); err != nil {
					t.Fatal(err)
				}
				if got := sink.ids(); !slices.Equal(got, []string{"m1", "m2", "m3"}) {
					t.Fatalf("accepted %v, want m1 m2 m3 once each", got)
				}
				if st := e.st(t); st.HistoryID != 1003 {
					t.Fatalf("historyId = %d, want 1003", st.HistoryID)
				}
			})
		}
	}
}

// A garbled messages.list page, or profile, during a full synchronization
// fails it as transient before any message is handed over; the next round
// runs it again.
func TestGarbledFullSyncAnswerHoldsCursor(t *testing.T) {
	cases := map[string]struct {
		path   string
		second bool
		body   string
	}{
		"list html page":             {"/messages", false, garbledBodies["html page"]},
		"list second page truncated": {"/messages", true, garbledBodies["truncated"]},
		"list empty body":            {"/messages", false, ""},
		"profile html page":          {"/profile", false, garbledBodies["html page"]},
		"profile without history id": {"/profile", false, `{"emailAddress":"` + testMailbox + `"}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, Config{})
			for _, id := range []string{"m1", "m2", "m3"} {
				e.inbound(id, testPeer, id) // two list pages
			}
			var failing atomic.Bool
			failing.Store(true)
			served := answerWhen(e.g, onPath(c.path, c.second), http.StatusOK, c.body, failing.Load)
			sink := &testSink{}
			_, err := e.inst.round(t.Context(), sink)
			wantRetry(t, err)
			if served.Load() == 0 {
				t.Fatal("the scripted answer was never served")
			}
			if st := e.st(t); st.HistoryID != 0 {
				t.Fatalf("historyId = %d, want none yet", st.HistoryID)
			}
			if got := sink.ids(); len(got) != 0 {
				t.Fatalf("accepted %v, want nothing yet", got)
			}
			failing.Store(false)
			if _, err := e.inst.round(t.Context(), sink); err != nil {
				t.Fatal(err)
			}
			if got := sink.ids(); !slices.Equal(got, []string{"m1", "m2", "m3"}) {
				t.Fatalf("accepted %v, want m1 m2 m3", got)
			}
			if st := e.st(t); st.HistoryID != 1003 {
				t.Fatalf("historyId = %d, want 1003", st.HistoryID)
			}
		})
	}
}

// A push request that meets a garbled history page answers 503, so
// Pub/Sub redelivers the notification.
func TestPushGarbledHistoryPageIsRedelivered(t *testing.T) {
	e := pushEnv(t)
	e.inbound("m1", testPeer, "pushed")
	answerWhen(e.g, onPath("/history", false), http.StatusOK, garbledBodies["html page"], always)
	if w := e.push(t, e.token(t, nil), pushBody(testMailbox, 1001)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("push = %d, want 503", w.Code)
	}
}

// Over the real engine: a garbled history page met by a push request and a
// receiver round does not stop the mailbox; the receiver reconnects and
// stores the message.
func TestEngineGarbledHistoryPageIsRetried(t *testing.T) {
	r := newEngineRigObserved(t, &observer{})
	r.seed(testChan, engineInboxA, testMailbox, "support@example.test")
	ended := r.receive(testChan, testMailbox)
	var left atomic.Int32
	left.Store(2)
	served := answerWhen(r.g, onPath("/history", false), http.StatusOK, garbledBodies["html page"], func() bool {
		return left.Add(-1) >= 0
	})
	r.g.deliver(testMailbox, "m1", "thread-1", rawMessage("jane@example.org", "support@example.test", "<t1@example.org>", "Help", "after the garbled page"))
	if code := r.push(testChan, testMailbox); code != http.StatusServiceUnavailable {
		t.Fatalf("push = %d, want 503", code)
	}
	msgs := r.waitMessages(engineInboxA, 1)
	if byProviderID(msgs, testChan, "m1") == nil {
		t.Fatalf("messages = %+v, want m1", msgs)
	}
	if n := served.Load(); n != 2 {
		t.Errorf("garbled pages served = %d, want 2", n)
	}
	select {
	case err := <-ended:
		t.Fatalf("Receive returned %v: the mailbox stopped", err)
	case <-time.After(20 * time.Millisecond):
	}
}

// A round that fails part way through a page keeps the cursor past every
// history record it handed over whole, so the next round does not hand
// those messages over again: a reconnection that only meets the garbled
// answer accepts nothing.
func TestGarbledAnswerBehindAcceptedMessage(t *testing.T) {
	e := newEnv(t, Config{})
	if _, err := e.inst.round(t.Context(), &testSink{}); err != nil {
		t.Fatal(err)
	}
	e.inbound("m1", testPeer, "accepted")
	e.inbound("bad", testPeer, "garbled for good") // same page
	answerFor(e.g, testMailbox, "bad", "raw", http.StatusOK, garbledBodies["html page"], always)
	sink := &testSink{}
	_, err := e.inst.round(t.Context(), sink)
	wantRetry(t, err)
	if got := sink.ids(); !slices.Equal(got, []string{"m1"}) {
		t.Fatalf("accepted %v, want m1", got)
	}
	if st := e.st(t); st.HistoryID != 1001 {
		t.Fatalf("historyId = %d, want 1001: past m1's record, before bad's", st.HistoryID)
	}
	again := &testSink{}
	_, err = e.inst.round(t.Context(), again)
	wantRetry(t, err)
	if got := again.ids(); len(got) != 0 {
		t.Fatalf("accepted %v again: a repeat delivery counts as progress", got)
	}
}

// Over the real engine: an answer garbled for good behind a message already
// accepted ends Receive after Retry.MaxAttempts connections, as one with
// nothing before it does; the accepted message is not taken for progress.
func TestEngineGarbledAnswerBehindAcceptedMessageEndsReceive(t *testing.T) {
	const attempts = 3
	r := newEngineRigConfig(t, nil, 0, func(c *uma.Config) {
		c.Retry = uma.RetryPolicy{MaxAttempts: attempts, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	})
	r.seed(testChan, engineInboxA, testMailbox, "support@example.test")
	ended := r.receive(testChan, testMailbox)
	served := answerFor(r.g, testMailbox, "bad", "raw", http.StatusOK, garbledBodies["html page"], always)
	r.g.deliver(testMailbox, "m1", "thread-1", rawMessage("jane@example.org", "support@example.test", "<t1@example.org>", "Help", "accepted"))
	r.g.deliver(testMailbox, "bad", "thread-bad", rawMessage("jane@example.org", "support@example.test", "<tb@example.org>", "Help 2", "garbled for good"))
	if code := r.push(testChan, testMailbox); code != http.StatusServiceUnavailable {
		t.Fatalf("push = %d, want 503", code)
	}
	select {
	case err := <-ended:
		if model.CodeOf(err) != model.Transient {
			t.Fatalf("Receive = %v, want the transient failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Receive still running after %d garbled answers: the repeat of m1 counted as progress", served.Load())
	}
	// The push request, then one answer per connection.
	if n := served.Load(); n != 1+attempts {
		t.Errorf("garbled answers served = %d, want %d", n, 1+attempts)
	}
	if msgs := r.messages(engineInboxA); len(msgs) != 1 || byProviderID(msgs, testChan, "m1") == nil {
		t.Fatalf("messages = %+v, want m1 alone", msgs)
	}
}

// An unreadable message is observed by the receiver round that moves the
// cursor past it, once; push requests read history without moving the
// cursor and do not observe it.
func TestUnreadableMessageObservedOncePerPass(t *testing.T) {
	obs := &observer{}
	e := newEnv(t, Config{}, pushConfig, withObserver(obs))
	if err := e.inst.saveState(t.Context(), receiverState{HistoryID: 1000}); err != nil {
		t.Fatal(err)
	}
	e.inbound("bad", testPeer, "never read")
	answerFor(e.g, testMailbox, "bad", "raw", http.StatusOK, undecodableRaw("bad"), always)
	for range 2 {
		if w := e.push(t, e.token(t, nil), pushBody(testMailbox, 1001)); w.Code != http.StatusNoContent {
			t.Fatalf("push = %d", w.Code)
		}
	}
	if n := obs.count(ObservationSkipped); n != 0 {
		t.Fatalf("%s observations = %d after push requests, want 0", ObservationSkipped, n)
	}
	if _, err := e.inst.round(t.Context(), &testSink{}); err != nil {
		t.Fatal(err)
	}
	if n := obs.count(ObservationSkipped); n != 1 {
		t.Fatalf("%s observations = %d after the round, want 1", ObservationSkipped, n)
	}
	if n := e.g.count("GET /gmail/v1/users/" + testMailbox + "/messages/bad?format=raw"); n != 3 {
		t.Errorf("raw reads = %d, want 3: two push requests and the round", n)
	}
}

// An unreadable message before a garbled answer on the same page is
// observed once: the failed round keeps the cursor past it, so the next
// round does not meet it again.
func TestUnreadableMessageBeforeGarbledAnswerObservedOnce(t *testing.T) {
	obs := &observer{}
	e := newEnv(t, Config{}, withObserver(obs))
	if _, err := e.inst.round(t.Context(), &testSink{}); err != nil {
		t.Fatal(err)
	}
	e.inbound("poison", testPeer, "never read")
	e.inbound("bad", testPeer, "garbled for good") // same page
	e.g.mu.Lock()
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		var body string
		switch r.URL.Path {
		case "/gmail/v1/users/" + testMailbox + "/messages/poison":
			body = undecodableRaw("poison")
		case "/gmail/v1/users/" + testMailbox + "/messages/bad":
			body = garbledBodies["html page"]
		default:
			return false
		}
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = w.Write([]byte(body))
		return true
	}
	e.g.mu.Unlock()
	for range 2 {
		_, err := e.inst.round(t.Context(), &testSink{})
		wantRetry(t, err)
	}
	if n := obs.count(ObservationSkipped); n != 1 {
		t.Fatalf("%s observations = %d after two rounds, want 1", ObservationSkipped, n)
	}
	if st := e.st(t); st.HistoryID != 1001 {
		t.Fatalf("historyId = %d, want 1001: past the unreadable message", st.HistoryID)
	}
}
