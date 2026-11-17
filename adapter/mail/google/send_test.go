package google

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// lastSend decodes the raw message of the latest messages.send request.
func (e *env) lastSend(t *testing.T) (gSend, *mime.Message) {
	t.Helper()
	e.g.mu.Lock()
	defer e.g.mu.Unlock()
	if len(e.box.sends) == 0 {
		t.Fatal("no messages.send request")
	}
	s := e.box.sends[len(e.box.sends)-1]
	raw, err := base64.RawURLEncoding.DecodeString(s.Raw)
	if err != nil {
		t.Fatalf("raw is not unpadded base64url: %v", err)
	}
	m, err := mime.Parse(bytes.NewReader(raw), mime.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return s, m
}

func TestSendNewMessage(t *testing.T) {
	e := newEnv(t, Config{Subject: "From the desk"})
	out, err := e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(route(testPeer, ""), "a1"), Content: textContent("hello there")})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	req, m := e.lastSend(t)
	if req.ThreadID != "" {
		t.Errorf("threadId = %q, want none for a new message", req.ThreadID)
	}
	h := m.Header
	if len(h.From) != 1 || h.From[0].Address != testMailbox || len(h.To) != 1 || h.To[0].Address != testPeer {
		t.Errorf("From %v To %v", h.From, h.To)
	}
	if h.Subject != "From the desk" || m.Text != "hello there" || h.InReplyTo != "" {
		t.Errorf("subject %q text %q in-reply-to %q", h.Subject, m.Text, h.InReplyTo)
	}
	want := e.inst.messageID(effect(route(testPeer, ""), "a1").Operation, testMailbox)
	if h.MessageID != want || !strings.HasSuffix(want, "@example.test>") {
		t.Errorf("Message-ID = %q, want uma's %q", h.MessageID, want)
	}
	if len(out.References) != 2 || out.References[0] != model.ProviderMessageRef(testChan, "sent-1") ||
		out.References[1].Kind != model.RefKindEmailMessageID || out.References[1].ID != want {
		t.Errorf("references = %+v", out.References)
	}
	// A second attempt of the operation carries the same Message-ID.
	if _, err := e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(route(testPeer, ""), "a2"), Content: textContent("again")}); err != nil {
		t.Fatal(err)
	}
	if _, m2 := e.lastSend(t); m2.Header.MessageID != want {
		t.Errorf("attempt 2 Message-ID = %q, want %q", m2.Header.MessageID, want)
	}
	for _, r := range e.creds.requests() {
		if r.Purpose != PurposeGmail || len(r.Accept) != 1 || r.Accept[0] != "oauth2" || r.Channel != testChan || r.Reference != "cred-1" {
			t.Errorf("credential request %+v", r)
		}
	}
}

func TestSendReplyJoinsThread(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.deliver(testMailbox, "m1", "thread-77", rawMessage(testPeer, testMailbox, "<m1@remote.test>", "Order 42", "where is it?",
		"References: <m0@remote.test>"))
	out, err := e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(route(testPeer, "m1"), "a1"), Content: textContent("on its way")})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	req, m := e.lastSend(t)
	if req.ThreadID != "thread-77" {
		t.Errorf("threadId = %q, want the parent's", req.ThreadID)
	}
	if m.Header.Subject != "Re: Order 42" || m.Header.InReplyTo != "<m1@remote.test>" ||
		m.Header.References != "<m0@remote.test> <m1@remote.test>" {
		t.Errorf("subject %q in-reply-to %q references %q", m.Header.Subject, m.Header.InReplyTo, m.Header.References)
	}
	if e.g.count("GET /gmail/v1/users/"+testMailbox+"/messages/m1?format=metadata") != 1 {
		t.Errorf("parent metadata not read: %q", e.g.requests())
	}
}

func TestSendReplyToGoneParentStartsThread(t *testing.T) {
	e := newEnv(t, Config{})
	out, err := e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(route(testPeer, "gone"), "a1"), Content: textContent("hi")})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	if req, m := e.lastSend(t); req.ThreadID != "" || m.Header.InReplyTo != "" {
		t.Errorf("threadId %q in-reply-to %q, want a new thread", req.ThreadID, m.Header.InReplyTo)
	}
}

func TestSendFromProfileAddress(t *testing.T) {
	e := newEnv(t, Config{User: "me"})
	e.g.box("me", testChan) // the fake addresses users by the path userId
	e.g.mu.Lock()
	e.g.boxes["me"].user = "owner@example.test"
	e.g.mu.Unlock()
	for _, a := range []model.ID{"a1", "a2"} {
		if _, err := e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(route(testPeer, ""), a), Content: textContent("x")}); err != nil {
			t.Fatal(err)
		}
	}
	e.g.mu.Lock()
	raw, _ := base64.RawURLEncoding.DecodeString(e.g.boxes["me"].sends[0].Raw)
	e.g.mu.Unlock()
	m, _ := mime.Parse(bytes.NewReader(raw), mime.Limits{})
	if m.Header.From[0].Address != "owner@example.test" {
		t.Errorf("From = %v, want the profile address", m.Header.From)
	}
	if n := e.g.count("GET /gmail/v1/users/me/profile"); n != 1 {
		t.Errorf("profile reads = %d, want 1", n)
	}
}

// sendAnswer scripts the next messages.send answers.
func (e *env) sendAnswer(answers ...func(w http.ResponseWriter)) {
	e.g.mu.Lock()
	defer e.g.mu.Unlock()
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/messages/send") || len(answers) == 0 {
			return false
		}
		a := answers[0]
		answers = answers[1:]
		a(w)
		return true
	}
}

func status(code int, reason string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) { googleError(w, code, reason, "X") }
}

func TestSendOutcomeMapping(t *testing.T) {
	cases := []struct {
		name    string
		answer  func(w http.ResponseWriter)
		respond conformance.Outcome // scripted through the fake instead
		code    model.ErrorCode     // returned error; empty for an outcome
		state   model.DeliveryState // outcome state when code is empty
		retryIn time.Duration
	}{
		{name: "500", answer: status(500, "backendError"), code: model.OutcomeUnknown},
		{name: "502", answer: status(502, "backendError"), code: model.OutcomeUnknown},
		{name: "503", answer: status(503, "backendError"), code: model.OutcomeUnknown},
		{name: "504", answer: status(504, "backendError"), code: model.OutcomeUnknown},
		{name: "408", answer: status(408, "timeout"), code: model.OutcomeUnknown},
		{name: "answer lost", respond: conformance.OutcomeAnswerLost, code: model.OutcomeUnknown},
		{name: "request lost", respond: conformance.OutcomeRequestLost, code: model.OutcomeUnknown},
		{name: "connect refused", respond: conformance.OutcomeTransient, code: model.Transient},
		{name: "429", answer: func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "30")
			googleError(w, 429, "rateLimitExceeded", "RESOURCE_EXHAUSTED")
		}, code: model.RateLimited, retryIn: 30 * time.Second},
		{name: "403 rateLimitExceeded", answer: status(403, "rateLimitExceeded"), code: model.RateLimited},
		{name: "403 userRateLimitExceeded", answer: status(403, "userRateLimitExceeded"), code: model.RateLimited},
		{name: "403 insufficientPermissions", answer: status(403, "insufficientPermissions"), code: model.Unauthorized},
		{name: "400", answer: status(400, "invalidArgument"), state: model.Rejected},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, Config{})
			if c.answer != nil {
				e.sendAnswer(c.answer)
			} else {
				e.g.mu.Lock()
				e.box.responses = []conformance.Response{{Outcome: c.respond}}
				e.g.mu.Unlock()
			}
			out, err := e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(route(testPeer, ""), "a1"), Content: textContent("x")})
			if c.code == "" {
				if err != nil || out.State != c.state || out.Failure == nil || out.Failure.Retry != model.NoRetry {
					t.Fatalf("Send = %+v, %v; want %s", out, err, c.state)
				}
				return
			}
			me := wantCode(t, err, c.code)
			if c.retryIn != 0 && me.RetryAfter != c.retryIn {
				t.Errorf("RetryAfter = %v, want %v", me.RetryAfter, c.retryIn)
			}
			if c.code == model.OutcomeUnknown && me.Retry != model.ReconcileFirst {
				t.Errorf("retry advice = %q, want reconcile_first", me.Retry)
			}
		})
	}
}

func TestSend401RefreshesOnce(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.mu.Lock()
	e.g.reject["tok-1"] = true
	e.g.mu.Unlock()
	out, err := e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(route(testPeer, ""), "a1"), Content: textContent("x")})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	reqs := e.creds.requests()
	if len(reqs) != 2 || reqs[0].Fresh || !reqs[1].Fresh {
		t.Fatalf("credential requests = %+v, want one then one Fresh", reqs)
	}
	e.g.mu.Lock()
	e.g.reject["tok-fresh"] = true
	e.g.mu.Unlock()
	_, err = e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(route(testPeer, ""), "a2"), Content: textContent("x")})
	_ = wantCode(t, err, model.Unauthorized)
}

func TestSendRefusesBadInput(t *testing.T) {
	e := newEnv(t, Config{})
	other := route(testPeer, "")
	other.Channel.ID = "00000000-0000-7000-8000-0000000000c9"
	if _, err := e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(other, "a1"), Content: textContent("x")}); model.CodeOf(err) != model.Permanent {
		t.Errorf("foreign route: %v", err)
	}
	bad := route("not an address", "")
	if _, err := e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(bad, "a1"), Content: textContent("x")}); model.CodeOf(err) != model.Permanent {
		t.Errorf("bad address: %v", err)
	}
	att := model.Content{Parts: []model.Part{{Kind: model.PartStructured, Structured: &model.Extension{Name: "x", Version: 1}}}}
	if _, err := e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(route(testPeer, ""), "a1"), Content: att}); model.CodeOf(err) != model.Unsupported {
		t.Errorf("structured content: %v", err)
	}
	if n := e.g.count("POST"); n != 0 {
		t.Errorf("provider calls = %d, want none", n)
	}
}

func TestReconcile(t *testing.T) {
	e := newEnv(t, Config{})
	eff := effect(route(testPeer, ""), "a1")
	e.g.mu.Lock()
	e.box.responses = []conformance.Response{{Outcome: conformance.OutcomeAnswerLost}}
	e.g.mu.Unlock()
	_, err := e.inst.Send(t.Context(), adapter.SendRequest{Effect: eff, Content: textContent("x")})
	_ = wantCode(t, err, model.OutcomeUnknown)
	out, err := e.inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: eff})
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 2 || out.References[0].ID != "sent-1" {
		t.Fatalf("Reconcile after a lost answer = %+v, %v", out, err)
	}
	id := e.inst.messageID(eff.Operation, testMailbox)
	want := "GET /gmail/v1/users/" + testMailbox + "/messages?includeSpamTrash=true&labelIds=SENT&maxResults=10&q=rfc822msgid:" + bareID(id)
	if e.g.count(want) != 1 {
		t.Errorf("reconcile search not issued as %q: %q", want, e.g.requests())
	}

	other := effect(route(testPeer, ""), "a1")
	other.Operation.ID = "00000000-0000-7000-8000-0000000000e2"
	out, err = e.inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: other})
	if err != nil || out.State != model.Failed || out.Failure == nil {
		t.Fatalf("Reconcile of a never-sent operation = %+v, %v; want failed", out, err)
	}
}

func TestReplySubject(t *testing.T) {
	for in, want := range map[string]string{"": "", "Hi": "Re: Hi", "re: Hi": "re: Hi", "RE:Hi": "RE:Hi", "  a\r\n b ": "Re: a b"} {
		if got := replySubject(in); got != want {
			t.Errorf("replySubject(%q) = %q, want %q", in, got, want)
		}
	}
}

// The search Reconcile runs may lag the Sent label: the descriptor declares
// a settle window, so the engine does not read an early "not found" as
// "not sent".
func TestDescribeDeclaresReconcileSettle(t *testing.T) {
	if d := NewFactory(Options{}).Describe(); d.ReconcileSettle != ReconcileSettle || ReconcileSettle <= 0 {
		t.Fatalf("ReconcileSettle = %v, want the declared window %v", d.ReconcileSettle, ReconcileSettle)
	}
}

// parentWithRecipients delivers a message from the peer to the mailbox and
// two others, copied to a third.
func parentWithRecipients(e *env) {
	e.g.deliver(testMailbox, "m1", "thread-77", rawMessage(testPeer, testMailbox+`, "Bob" <bob@remote.test>`, "<m1@remote.test>", "Order 42", "where is it?",
		"Cc: carol@remote.test, "+strings.ToUpper(testMailbox)))
}

func addrList(l []mime.Address) []string {
	var out []string
	for _, a := range l {
		out = append(out, a.Address)
	}
	return out
}

func TestSendReplyAll(t *testing.T) {
	e := newEnv(t, Config{})
	parentWithRecipients(e)
	rec := &model.ReplyRecipients{ReplyAll: true, Cc: []string{"extra@remote.test"}, Bcc: []string{"audit@example.test"}}
	out, err := e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(route(testPeer, "m1"), "a1"), Content: textContent("on its way"), Recipients: rec})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	req, m := e.lastSend(t)
	if req.ThreadID != "thread-77" {
		t.Errorf("threadId = %q", req.ThreadID)
	}
	if got, want := addrList(m.Header.To), []string{testPeer, "bob@remote.test"}; !slices.Equal(got, want) {
		t.Errorf("To = %v, want %v: the peer and the other To recipient, never the mailbox", got, want)
	}
	if got, want := addrList(m.Header.Cc), []string{"carol@remote.test", "extra@remote.test"}; !slices.Equal(got, want) {
		t.Errorf("Cc = %v, want %v", got, want)
	}
	if got := m.Header.Values("Bcc"); len(got) != 1 || !strings.Contains(got[0], "audit@example.test") {
		t.Errorf("Bcc = %v, want the blind recipient", got)
	}
}

func TestSendCcWithoutReplyAllIgnoresTheParentsRecipients(t *testing.T) {
	e := newEnv(t, Config{})
	parentWithRecipients(e)
	rec := &model.ReplyRecipients{Cc: []string{"extra@remote.test"}}
	if _, err := e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(route(testPeer, "m1"), "a1"), Content: textContent("hi"), Recipients: rec}); err != nil {
		t.Fatal(err)
	}
	_, m := e.lastSend(t)
	if got := addrList(m.Header.To); !slices.Equal(got, []string{testPeer}) {
		t.Errorf("To = %v, want the peer alone", got)
	}
	if got := addrList(m.Header.Cc); !slices.Equal(got, []string{"extra@remote.test"}) {
		t.Errorf("Cc = %v", got)
	}
}

func TestSendReplyAllToGoneParentAddsNothing(t *testing.T) {
	e := newEnv(t, Config{})
	rec := &model.ReplyRecipients{ReplyAll: true}
	if _, err := e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(route(testPeer, "gone"), "a1"), Content: textContent("hi"), Recipients: rec}); err != nil {
		t.Fatal(err)
	}
	_, m := e.lastSend(t)
	if got := addrList(m.Header.To); !slices.Equal(got, []string{testPeer}) || len(m.Header.Cc) != 0 {
		t.Errorf("To %v Cc %v, want the peer alone", got, m.Header.Cc)
	}
}

func TestSendTooManyRecipientsIsFinal(t *testing.T) {
	e := newEnv(t, Config{})
	var cc []string
	for i := 0; i < 50; i++ {
		cc = append(cc, fmt.Sprintf("u%d@remote.test", i))
	}
	_, err := e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(route(testPeer, ""), "a1"), Content: textContent("hi"), Recipients: &model.ReplyRecipients{Cc: cc}})
	if !model.IsCode(err, model.Validation) {
		t.Fatalf("Send = %v, want validation", err)
	}
	e.g.mu.Lock()
	defer e.g.mu.Unlock()
	if len(e.box.sends) != 0 {
		t.Errorf("a refused send reached Gmail")
	}
}

func TestReplyRecipientsCapabilityReportsLimits(t *testing.T) {
	e := newEnv(t, Config{})
	statuses, err := e.inst.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(statuses, func(s model.CapabilityStatus) bool { return s.Name == model.CapabilityReplyRecipients })
	if i < 0 || !statuses[i].Available {
		t.Fatalf("statuses = %+v, want reply_recipients available", statuses)
	}
	if l, err := model.ParseRecipientLimits(statuses[i].Limits); err != nil || !l.ReplyAll || !l.Cc || !l.Bcc || l.MaxRecipients != 50 {
		t.Fatalf("limits = %+v, %v", l, err)
	}
}
