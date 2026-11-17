package google

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
)

func (e *env) st(t *testing.T) receiverState {
	t.Helper()
	st, err := e.inst.loadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestFirstRoundIsBoundedFullSync(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.now = func() time.Time { return testNow.Add(-2 * time.Hour) }
	e.inbound("m0", testPeer, "too old")
	e.g.now = e.clock.Now
	e.inbound("m1", testPeer, "one")
	e.inbound("m2", testPeer, "two")
	e.inbound("m3", testPeer, "three")
	sink := &testSink{}
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	if got := sink.ids(); !slices.Equal(got, []string{"m1", "m2", "m3"}) {
		t.Fatalf("accepted %v, want m1 m2 m3 oldest first, without the message outside the window", got)
	}
	st := e.st(t)
	if st.HistoryID != 1004 || st.FullResyncs != 0 {
		t.Fatalf("state = %+v, want the profile historyId and no forced resync", st)
	}
	after := testNow.Add(-time.Hour).Unix()
	if e.g.count("GET /gmail/v1/users/"+testMailbox+"/messages?labelIds=INBOX&maxResults=2&q=after:"+itoa(after)) != 1 {
		t.Errorf("full sync list not issued: %q", e.g.requests())
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestHistoryRoundPagesFromStoredID(t *testing.T) {
	e := newEnv(t, Config{})
	sink := &testSink{}
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	start := e.st(t).HistoryID
	for _, id := range []string{"m1", "m2", "m3"} {
		e.inbound(id, testPeer, id)
	}
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	if got := sink.ids(); !slices.Equal(got, []string{"m1", "m2", "m3"}) {
		t.Fatalf("accepted %v", got)
	}
	var pages []string
	for _, r := range e.g.requests() {
		if strings.Contains(r, "/history?") {
			pages = append(pages, r)
		}
	}
	if len(pages) != 2 || !strings.Contains(pages[0], "startHistoryId="+start.String()) ||
		!strings.Contains(pages[1], "startHistoryId="+start.String()) || !strings.Contains(pages[1], "pageToken=2") ||
		!strings.Contains(pages[0], "historyTypes=messageAdded") || strings.Contains(pages[0], "labelId=") {
		t.Fatalf("history requests = %q", pages)
	}
	if st := e.st(t); st.HistoryID != 1003 {
		t.Fatalf("historyId = %d, want the last page's 1003", st.HistoryID)
	}
	// Nothing new: one more history call, nothing accepted.
	if _, err := e.inst.round(t.Context(), sink); err != nil || len(sink.ids()) != 3 {
		t.Fatalf("idle round: %v %v", err, sink.ids())
	}
}

func TestSinkRefusalHoldsHistory(t *testing.T) {
	e := newEnv(t, Config{})
	sink := &testSink{}
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"m1", "m2", "m3"} {
		e.inbound(id, testPeer, id)
	}
	sink.refuse = func(in model.Ingress) error {
		if in.Envelope.MessageID == "m3" {
			return model.NewError(model.Unavailable, "store down")
		}
		return nil
	}
	if _, err := e.inst.round(t.Context(), sink); model.CodeOf(err) != model.Unavailable {
		t.Fatalf("round = %v, want the sink error", err)
	}
	if st := e.st(t); st.HistoryID != 1002 {
		t.Fatalf("historyId = %d, want 1002: the page of m1 and m2 was accepted", st.HistoryID)
	}
	sink.refuse = nil
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	if got := sink.ids(); !slices.Equal(got, []string{"m1", "m2", "m3"}) {
		t.Fatalf("accepted %v, want m3 read again", got)
	}
}

func TestOutOfDateHistoryIDResyncs(t *testing.T) {
	e := newEnv(t, Config{})
	sink := &testSink{}
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	e.inbound("m1", testPeer, "in the gap")
	e.g.mu.Lock()
	e.box.minHistory = e.box.historyID // Gmail dropped every record so far
	e.g.mu.Unlock()
	e.clock.Advance(time.Minute)
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	st := e.st(t)
	if st.FullResyncs != 1 || !st.LastFullResync.Equal(e.clock.Now()) || st.HistoryID != 1001 {
		t.Fatalf("state = %+v, want one counted resync from the profile historyId", st)
	}
	if got := sink.ids(); !slices.Equal(got, []string{"m1"}) {
		t.Fatalf("accepted %v, want the gap message from the full sync", got)
	}
	if n := e.g.count("GET /gmail/v1/users/" + testMailbox + "/profile"); n != 2 {
		t.Errorf("profile reads = %d, want 2 (initial sync and resync)", n)
	}
}

func TestSkipsDraftsAndSent(t *testing.T) {
	e := newEnv(t, Config{})
	sink := &testSink{}
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	e.g.deliver(testMailbox, "self", "t", rawMessage(testMailbox, testMailbox, "<self@example.test>", "s", "x"), labelInbox, labelSent)
	e.g.deliver(testMailbox, "draft", "t", rawMessage(testMailbox, testPeer, "<d@example.test>", "s", "x"), labelInbox, labelDraft)
	e.inbound("m1", testPeer, "real")
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	if got := sink.ids(); !slices.Equal(got, []string{"m1"}) {
		t.Fatalf("accepted %v, want only m1", got)
	}
	if e.g.count("GET /gmail/v1/users/"+testMailbox+"/messages/self") != 0 {
		t.Error("a message labelled SENT was fetched")
	}
}

func TestWatchMaintenance(t *testing.T) {
	e := newEnv(t, Config{}, pushConfig)
	sink := &testSink{}
	wait, err := e.inst.round(t.Context(), sink)
	if err != nil {
		t.Fatal(err)
	}
	e.g.mu.Lock()
	watches := append([]gWatchRequest(nil), e.box.watches...)
	e.g.mu.Unlock()
	if len(watches) != 1 || watches[0].TopicName != testTopic || !slices.Equal(watches[0].LabelIDs, []string{"INBOX"}) ||
		watches[0].LabelFilterBehavior != "INCLUDE" {
		t.Fatalf("watch requests = %+v", watches)
	}
	st := e.st(t)
	if !st.WatchExpires.Equal(testNow.Add(7*24*time.Hour)) || !st.WatchedAt.Equal(testNow) {
		t.Fatalf("watch state = %+v", st)
	}
	if wait != 5*time.Minute {
		t.Errorf("wait = %v, want the poll interval", wait)
	}
	e.clock.Advance(23 * time.Hour)
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	if n := e.g.count("POST /gmail/v1/users/" + testMailbox + "/watch"); n != 1 {
		t.Fatalf("watch calls after 23h = %d, want 1", n)
	}
	e.clock.Advance(time.Hour)
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	if n := e.g.count("POST /gmail/v1/users/" + testMailbox + "/watch"); n != 2 {
		t.Fatalf("watch calls after a day = %d, want a daily renewal", n)
	}
}

func TestWatchRenewedBeforeExpiry(t *testing.T) {
	e := newEnv(t, Config{WatchRenewIntervalMS: int64(6 * 24 * time.Hour / time.Millisecond)}, pushConfig)
	st := receiverState{HistoryID: 1000, WatchedAt: testNow.Add(-5 * 24 * time.Hour), WatchExpires: testNow.Add(30 * time.Minute)}
	if err := e.inst.saveState(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	if _, err := e.inst.round(t.Context(), &testSink{}); err != nil {
		t.Fatal(err)
	}
	if n := e.g.count("POST /gmail/v1/users/" + testMailbox + "/watch"); n != 1 {
		t.Fatalf("watch calls = %d, want a renewal inside the expiry margin", n)
	}
}

func TestNoTopicNoWatch(t *testing.T) {
	e := newEnv(t, Config{})
	if _, err := e.inst.round(t.Context(), &testSink{}); err != nil {
		t.Fatal(err)
	}
	if n := e.g.count("POST /gmail/v1/users/" + testMailbox + "/watch"); n != 0 {
		t.Fatalf("watch calls = %d without a topic", n)
	}
}

func TestStateInProtocolStore(t *testing.T) {
	p := &memProtocol{}
	e := newEnv(t, Config{}, withProtocol(p))
	sink := &testSink{}
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	if p.saves == 0 {
		t.Fatal("receiver state not saved in the protocol store")
	}
	ext, _, _ := p.Load(t.Context(), protocolKey)
	if ext.Name != stateSchema || ext.Version != stateVersion || !strings.Contains(string(ext.Data), `"history_id":"1000"`) {
		t.Fatalf("stored state = %s %d %s", ext.Name, ext.Version, ext.Data)
	}
}

func TestRunWakesOnNotify(t *testing.T) {
	e := newEnv(t, Config{PollIntervalMS: int64(time.Hour / time.Millisecond)})
	sink := &testSink{}
	done := make(chan error, 1)
	ctx, cancel := contextWithCancel(t)
	go func() { done <- e.inst.Run(ctx, sink) }()
	waitFor(t, func() bool { return e.st(t).HistoryID != 0 })
	e.inbound("m1", testPeer, "x")
	e.inst.state.notify()
	waitFor(t, func() bool { return len(sink.ids()) == 1 })
	cancel()
	if err := <-done; err == nil {
		t.Fatal("Run returned nil after cancel")
	}
}

func TestNormalize(t *testing.T) {
	e := newEnv(t, Config{Addresses: []string{"sales@example.test", "help@example.test"}})
	raw := strings.Join([]string{
		`From: "Peer, Claimed Admin" <` + testPeer + `>`,
		"To: someone@else.test, help@example.test",
		"Subject: =?utf-8?q?Caf=C3=A9?=",
		"Message-ID: <m2@remote.test>",
		"In-Reply-To: <m1@example.test>",
		"References: <m0@example.test> <m1@example.test>",
		"List-Id: Announce <announce.remote.test>",
		"Auto-Submitted: auto-generated",
		"Authentication-Results: mx.google.com; spf=pass",
		"MIME-Version: 1.0",
		`Content-Type: multipart/mixed; boundary="b"`,
		"",
		"--b",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"body text",
		"--b",
		`Content-Type: application/pdf; name="a.pdf"`,
		"Content-Disposition: attachment; filename=a.pdf",
		"Content-Transfer-Encoding: base64",
		"",
		"JVBERi0=",
		"--b--",
		"",
	}, "\r\n")
	e.g.deliver(testMailbox, "m2", "thread-9", []byte(raw))
	in, skip, err := e.inst.fetchIngress(t.Context(), e.inst.c.session(), nil, "m2")
	if err != nil || skip {
		t.Fatalf("fetchIngress: %v %v", skip, err)
	}
	env := in.Envelope
	if env.MessageID != "m2" || env.Destination != "help@example.test" || env.Kind != model.EnvelopeAutomatedMessage {
		t.Errorf("envelope id %q destination %q kind %q", env.MessageID, env.Destination, env.Kind)
	}
	want := model.ExternalRef{Channel: testChan, Namespace: IdentityNamespace, Kind: refKindAddress, ID: testPeer, ParentID: "m2"}
	if env.Sender != want {
		t.Errorf("sender = %+v, want %+v", env.Sender, want)
	}
	var kinds []string
	for _, r := range env.References {
		kinds = append(kinds, r.Kind+"="+r.ID)
	}
	wantRefs := []string{
		model.RefKindEmailMessageID + "=<m2@remote.test>", model.RefKindEmailInReplyTo + "=<m1@example.test>",
		model.RefKindEmailReferences + "=<m0@example.test>", model.RefKindEmailReferences + "=<m1@example.test>",
	}
	if len(kinds) < 4 || !slices.Equal(kinds[:4], wantRefs) {
		t.Errorf("references = %v", kinds)
	}
	if env.InReplyTo != "<m1@example.test>" {
		t.Errorf("InReplyTo = %q, want the In-Reply-To msg-id", env.InReplyTo)
	}
	for _, r := range env.References {
		if strings.Contains(r.ID, "thread-9") {
			t.Errorf("threadId became a reference: %+v", r)
		}
	}
	if len(env.Content.Parts) != 2 || env.Content.Parts[0].Text != "body text" || env.Content.Parts[1].Structured.Name != PartAttachment {
		t.Fatalf("parts = %+v", env.Content.Parts)
	}
	var att attachmentPart
	_ = json.Unmarshal(env.Content.Parts[1].Structured.Data, &att)
	if att.Filename != "a.pdf" || att.MediaType != "application/pdf" || !strings.HasPrefix(att.Digest, "sha256:") {
		t.Errorf("attachment = %+v", att)
	}
	var p messagePayload
	if err := json.Unmarshal(env.Payload.Data, &p); err != nil || env.Payload.Name != PayloadMessage {
		t.Fatal(err)
	}
	if p.ThreadID != "thread-9" || p.Subject != "Café" || p.AuthenticationResults != "mx.google.com; spf=pass" ||
		!slices.Equal(p.To, []string{"someone@else.test", "help@example.test"}) || !p.ReceivedAt.Equal(testNow) {
		t.Errorf("payload = %+v", p)
	}
	if strings.Contains(string(env.Payload.Data), "Claimed Admin") && env.Sender.ID != testPeer {
		t.Error("display name became identity")
	}
}

func TestUnparseableMessageSkipped(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.mu.Lock()
	b := e.box
	b.messages["bad"] = &fakeMsg{id: "bad", labels: []string{labelInbox}, raw: []byte("no header terminator \x00"), internal: testNow}
	e.g.mu.Unlock()
	if _, _, err := e.inst.fetchIngress(t.Context(), e.inst.c.session(), nil, "bad"); !errors.Is(err, errUnreadable) {
		t.Fatalf("fetchIngress = %v, want the message unreadable", err)
	}
	_, skip, err := e.inst.fetchIngress(t.Context(), e.inst.c.session(), nil, "gone")
	if err != nil || !skip {
		t.Fatalf("missing message = %v %v, want skip", skip, err)
	}
}
