package google

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"hop.top/uma/model"
)

// smallRawLimit is below the size of bigMessage and above that of every
// message of the tests that are not over the bound.
const smallRawLimit = 4 << 10

func bigMessage(id string) []byte {
	return rawMessage(`"Big Sender" <big@example.org>`, "support@example.test", "<"+id+"@example.org>", "Big one",
		strings.Repeat("0123456789abcdef", 1024), "In-Reply-To: <t0@example.org>", "References: <t0@example.org>")
}

func payloadOf(t *testing.T, in model.Ingress) messagePayload {
	t.Helper()
	var p messagePayload
	if err := json.Unmarshal(in.Envelope.Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func ingressOf(sink *testSink, id string) (model.Ingress, bool) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, in := range sink.got {
		if in.Envelope.MessageID == id {
			return in, true
		}
	}
	return model.Ingress{}, false
}

// An over-cap message must not stop the mailbox: the messages around it are
// accepted, the message itself is delivered from its headers alone and says
// so, and the history cursor moves past all of them.
func TestOverCapMessageDoesNotHaltTheReceiver(t *testing.T) {
	e := newEnv(t, Config{})
	e.inst.c.rawLimit = smallRawLimit
	sink := &testSink{}
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	e.inbound("m1", testPeer, "small before")
	e.g.deliver(e.box.user, "big", "thread-big", bigMessage("big"))
	e.inbound("m3", testPeer, "small after")
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatalf("round = %v, want the over-cap message handled on its own", err)
	}
	if got := sink.ids(); !slices.Equal(got, []string{"m1", "big", "m3"}) {
		t.Fatalf("accepted %v, want the message after the over-cap one too", got)
	}
	if st := e.st(t); st.HistoryID != 1003 {
		t.Fatalf("historyId = %d, want 1003: every message was handled", st.HistoryID)
	}

	big, _ := ingressOf(sink, "big")
	p := payloadOf(t, big)
	if !p.BodyOmitted || p.SizeEstimate != int64(len(bigMessage("big"))) {
		t.Errorf("payload = %+v, want body_omitted and the size estimate", p)
	}
	if p.Subject != "Big one" || p.ThreadID != "thread-big" || p.InternetMessageID != "<big@example.org>" || !slices.Equal(p.To, []string{"support@example.test"}) {
		t.Errorf("payload = %+v, want the header fields", p)
	}
	env := big.Envelope
	if env.Sender.ID != "big@example.org" || env.Sender.ParentID != "big" || env.Destination != testMailbox {
		t.Errorf("sender %+v destination %q", env.Sender, env.Destination)
	}
	if len(env.References) == 0 {
		t.Error("threading headers lost")
	}
	for _, part := range env.Content.Parts {
		if part.Kind == model.PartText {
			t.Errorf("header-only message has a text part %q", part.Text)
		}
	}
	if len(env.Content.Parts) == 0 {
		t.Error("no content: the payload is the content of a message without a body")
	}

	small, _ := ingressOf(sink, "m3")
	if payloadOf(t, small).BodyOmitted || payloadOf(t, small).SizeEstimate != 0 || small.Envelope.Content.Parts[0].Text != "small after" {
		t.Errorf("a message under the bound changed: %+v", small.Envelope)
	}
	if n := e.g.count("GET /gmail/v1/users/" + testMailbox + "/messages/big?format=metadata"); n != 1 {
		t.Errorf("metadata reads of the over-cap message = %d, want 1", n)
	}
}

// A failure that a repeat can fix, in reading the over-cap message's
// headers, holds the cursor like any other fetch failure.
func TestOverCapMessageFailureHoldsHistory(t *testing.T) {
	e := newEnv(t, Config{})
	e.inst.c.rawLimit = smallRawLimit
	sink := &testSink{}
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	e.inbound("m1", testPeer, "small before")
	e.g.deliver(e.box.user, "big", "thread-big", bigMessage("big"))
	e.inbound("m3", testPeer, "small after")
	failing := true
	e.g.mu.Lock()
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		e.g.mu.Lock()
		defer e.g.mu.Unlock()
		if failing && r.URL.Query().Get("format") == "metadata" {
			googleError(w, http.StatusServiceUnavailable, "backendError", "UNAVAILABLE")
			return true
		}
		return false
	}
	e.g.mu.Unlock()
	_, err := e.inst.round(t.Context(), sink)
	if model.CodeOf(err) != model.Transient {
		t.Fatalf("round = %v, want a transient error", err)
	}
	if st := e.st(t); st.HistoryID >= 1002 {
		t.Fatalf("historyId = %d: the cursor passed a message not taken", st.HistoryID)
	}
	if ids := sink.ids(); slices.Contains(ids, "m3") {
		t.Fatalf("accepted %v: m3 is behind the failing message", ids)
	}
	e.g.mu.Lock()
	failing = false
	e.g.mu.Unlock()
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	got := sink.ids()
	for _, id := range []string{"m1", "big", "m3"} {
		if !slices.Contains(got, id) {
			t.Fatalf("accepted %v, want %s after the repeat", got, id)
		}
	}
	if st := e.st(t); st.HistoryID != 1003 {
		t.Fatalf("historyId = %d, want 1003", st.HistoryID)
	}
}

// An over-cap message that is gone, or a draft, is skipped like any other.
func TestOverCapMessageSkips(t *testing.T) {
	e := newEnv(t, Config{})
	e.inst.c.rawLimit = smallRawLimit
	e.g.deliver(e.box.user, "draft", "thread-d", bigMessage("draft"), labelDraft)
	_, skip, err := e.inst.fetchIngress(t.Context(), e.inst.c.session(), nil, "draft")
	if err != nil || !skip {
		t.Fatalf("over-cap draft = %v %v, want skip", skip, err)
	}
}

// Over the real engine: the receiver keeps running past an over-cap message
// that arrived by push, and a poll round brings the next one.
func TestEngineOverCapMessageDoesNotStopTheMailbox(t *testing.T) {
	r := newEngineRigLimit(t, nil, smallRawLimit)
	r.seed(testChan, engineInboxA, testMailbox, "support@example.test")
	r.receive(testChan, testMailbox)

	r.g.deliver(testMailbox, "m1", "thread-1", rawMessage("jane@example.org", "support@example.test", "<t1@example.org>", "Help", "before"))
	r.g.deliver(testMailbox, "big", "thread-big", bigMessage("big"))
	r.g.deliver(testMailbox, "m3", "thread-3", rawMessage("jane@example.org", "support@example.test", "<t3@example.org>", "Help 3", "after"))
	if code := r.push(testChan, testMailbox); code != http.StatusNoContent {
		t.Fatalf("push = %d", code)
	}
	msgs := r.waitMessages(engineInboxA, 3)
	big := byProviderID(msgs, testChan, "big")
	if big == nil || byProviderID(msgs, testChan, "m3") == nil || byProviderID(msgs, testChan, "m1") == nil {
		t.Fatalf("messages = %+v, want m1, big and m3", msgs)
	}
	var found bool
	for _, x := range big.Metadata {
		if x.Name == PayloadMessage {
			var p messagePayload
			if err := json.Unmarshal(x.Data, &p); err != nil {
				t.Fatal(err)
			}
			found = p.BodyOmitted
		}
	}
	if !found {
		t.Errorf("stored message does not say its body was omitted: %+v", big.Metadata)
	}
	// The receiver is still running: a later message arrives.
	r.g.deliver(testMailbox, "m4", "thread-4", rawMessage("jane@example.org", "support@example.test", "<t4@example.org>", "Help 4", "later"))
	if code := r.push(testChan, testMailbox); code != http.StatusNoContent {
		t.Fatalf("push = %d", code)
	}
	r.waitMessages(engineInboxA, 4)
}

// The bound covers the largest message Gmail documents it receives: 70 MB
// after encoding (Enterprise Plus), which raw returns base64url encoded
// again inside the JSON answer.
func TestRawBoundCoversTheDocumentedReceiveLimit(t *testing.T) {
	const documented = 70 << 20 // read as MiB: the larger reading of "70 MB"
	enc := int64(base64.URLEncoding.EncodedLen(documented))
	if maxRawMessageBytes < enc+(512<<10) {
		t.Fatalf("maxRawMessageBytes = %d, below the %d bytes a %d byte message takes as raw plus JSON headroom", int64(maxRawMessageBytes), enc, int64(documented))
	}
}

// A message within the documented limit but over the former 64 MiB bound is
// taken whole, at the real bound.
func TestLargeMessageWithinDocumentedLimitIsTaken(t *testing.T) {
	if testing.Short() {
		t.Skip("moves about 100 MB through the fake")
	}
	e := newEnv(t, Config{})
	sink := &testSink{}
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	e.g.deliver(e.box.user, "large", "thread-l", rawMessage(testPeer, testMailbox, "<large@remote.test>", "large", strings.Repeat("A", 60_000_000)))
	e.inbound("m2", testPeer, "after")
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatalf("round = %v", err)
	}
	large, ok := ingressOf(sink, "large")
	if !ok || payloadOf(t, large).BodyOmitted || !payloadOf(t, large).TextTruncated {
		t.Fatalf("large message = %v %+v, want it parsed whole", ok, large.Envelope.Payload)
	}
	if got := sink.ids(); !slices.Equal(got, []string{"large", "m2"}) {
		t.Fatalf("accepted %v", got)
	}
}
