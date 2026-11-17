package microsoft

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

func send(t *testing.T, e *env, r model.ExternalRef, attempt model.ID) (adapter.Outcome, error) {
	t.Helper()
	return e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(r, attempt), Content: textContent("thanks")})
}

func TestReplyOnParentUsesCreateReplyAndStoredRouteOnly(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", "peer@example.test", "hi")
	// The original asked for replies elsewhere; the stored route wins.
	e.box.messages["m1"].m.From = &gRecipient{EmailAddress: gEmailAddress{Address: "someone-else@example.test"}}

	out, err := send(t, e, route("peer@example.test", "m1"), "a1")
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v; want provider_accepted", out, err)
	}
	var calls []string
	for _, r := range e.g.requests() {
		if strings.Contains(r, "/messages") {
			calls = append(calls, strings.SplitN(r, "?", 2)[0])
		}
	}
	want := []string{
		"POST /v1.0/users/" + testMailbox + "/messages/m1/createReply",
		"PATCH /v1.0/users/" + testMailbox + "/messages/draft-1",
		"POST /v1.0/users/" + testMailbox + "/messages/draft-1/send",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
	sent := e.box.messages["draft-1"]
	if sent.folder != "sentitems" || len(sent.m.ToRecipients) != 1 || sent.m.ToRecipients[0].EmailAddress.Address != "peer@example.test" {
		t.Fatalf("sent %+v to %+v, want only the stored route", sent.folder, sent.m.ToRecipients)
	}
	if sent.m.Subject != "RE: hello" || sent.props[AttemptProperty] != attemptKey(effect(route("", ""), "a1")) {
		t.Fatalf("draft subject %q props %v", sent.m.Subject, sent.props)
	}
	wantRefs := []model.ExternalRef{
		model.ProviderMessageRef(testChan, "draft-1"),
		{Channel: testChan, Namespace: model.EmailHeaderNamespace, Kind: model.RefKindEmailMessageID, ID: "<draft-1.desk.contoso.test@fake.outlook.test>"},
	}
	if !slices.Equal(out.References, wantRefs) {
		t.Fatalf("references = %+v, want %+v", out.References, wantRefs)
	}
}

func TestReplyWhoseParentIsGoneStartsANewMessage(t *testing.T) {
	e := newEnv(t, Config{})
	out, err := send(t, e, route("peer@example.test", "gone"), "a1")
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	if n := e.g.count("POST /v1.0/users/" + testMailbox + "/messages?"); n != 0 {
		t.Fatalf("unexpected query on draft create")
	}
	if e.g.count("POST /v1.0/users/"+testMailbox+"/messages") != 3 { // createReply, new draft, send
		t.Fatalf("requests = %q", e.g.requests())
	}
	if d := e.box.messages["draft-1"]; d == nil || d.parent != "" || d.folder != "sentitems" {
		t.Fatalf("new draft = %+v", d)
	}
}

func TestSendRefusesNonTextContent(t *testing.T) {
	e := newEnv(t, Config{})
	_, err := e.inst.Send(t.Context(), adapter.SendRequest{
		Effect:  effect(route("peer@example.test", ""), "a1"),
		Content: model.Content{Parts: []model.Part{{Kind: model.PartAttachment, Attachment: &model.AttachmentRef{}}}},
	})
	wantCode(t, err, model.Unsupported)
	if n := len(e.g.requests()); n != 0 {
		t.Fatalf("requests = %d, want none", n)
	}
}

func TestSendRouteOfAnotherChannelIsRefused(t *testing.T) {
	e := newEnv(t, Config{})
	r := route("peer@example.test", "")
	r.Channel.ID = "00000000-0000-7000-8000-0000000000c2"
	_, err := send(t, e, r, "a1")
	wantCode(t, err, model.Permanent)
	if n := len(e.g.requests()); n != 0 {
		t.Fatalf("requests = %d, want none", n)
	}
}

func TestThrottlingIsRetryAfterNeverRejected(t *testing.T) {
	for _, c := range []struct {
		name    string
		resp    conformance.Response
		status  int
		code    model.ErrorCode
		waitFor time.Duration
	}{
		{"429", conformance.Response{Outcome: conformance.OutcomeRateLimited, RetryAfter: 7 * time.Second}, 429, model.RateLimited, 7 * time.Second},
		{"503", conformance.Response{Outcome: conformance.OutcomeTransient}, 503, model.Transient, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, Config{})
			e.box.responses = []conformance.Response{c.resp}
			out, err := send(t, e, route("peer@example.test", ""), "a1")
			if out.State == model.Rejected {
				t.Fatalf("HTTP %d mapped to rejected; it is retryable", c.status)
			}
			me := wantErr(t, err, c.code)
			if me.Retry != model.RetrySameOperation || me.RetryAfter != c.waitFor {
				t.Fatalf("failure %+v, want retry same operation after %v", me, c.waitFor)
			}
			if _, ok := e.box.messages["draft-1"]; ok {
				t.Fatal("unsent draft kept")
			}
		})
	}
}

func TestRetryAfterHeaderForms(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "120")
	if d := retryAfter(h); d != 2*time.Minute {
		t.Fatalf("seconds form = %v", d)
	}
	h.Set("Retry-After", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
	if d := retryAfter(h); d < 59*time.Minute || d > time.Hour {
		t.Fatalf("date form = %v", d)
	}
	h.Set("Retry-After", "soon")
	if d := retryAfter(h); d != 0 {
		t.Fatalf("junk = %v", d)
	}
}

func TestRejectionIsFinal(t *testing.T) {
	e := newEnv(t, Config{})
	e.box.responses = []conformance.Response{{Outcome: conformance.OutcomeRejected}}
	out, err := send(t, e, route("peer@example.test", ""), "a1")
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Retry != model.NoRetry {
		t.Fatalf("Send = %+v, %v; want rejected without retry", out, err)
	}
	if strings.Contains(out.Failure.SafeMessage, "ErrorInvalidRecipients") {
		t.Fatalf("provider error text in failure: %q", out.Failure.SafeMessage)
	}
}

func TestUnauthorizedResolvesFreshOnce(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.reject["tok-1"] = true
	out, err := send(t, e, route("peer@example.test", ""), "a1")
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	reqs := e.creds.graphRequests()
	if len(reqs) != 2 || reqs[0].Fresh || !reqs[1].Fresh || !slices.Equal(reqs[1].Accept, reqs[0].Accept) {
		t.Fatalf("credential requests = %+v, want one plain then one Fresh", reqs)
	}
}

func TestSecondUnauthorizedIsFinal(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.reject["tok-1"], e.g.reject["tok-fresh"] = true, true
	_, err := send(t, e, route("peer@example.test", ""), "a1")
	me := wantErr(t, err, model.Unauthorized)
	if me.Retry != model.NoRetry {
		t.Fatalf("retry advice %q, want none", me.Retry)
	}
	if n := len(e.creds.graphRequests()); n != 2 {
		t.Fatalf("credential resolutions = %d, want 2 (one Fresh)", n)
	}
	if n := len(e.g.requests()); n != 2 {
		t.Fatalf("Graph requests = %d, want 2: a second 401 is never retried", n)
	}
}

func TestTimeoutAfterSendIsOutcomeUnknown(t *testing.T) {
	e := newEnv(t, Config{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/send") {
			return false
		}
		<-release // Graph never answers in time
		return true
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	out, err := e.inst.Send(ctx, adapter.SendRequest{Effect: effect(route("peer@example.test", ""), "a1"), Content: textContent("x")})
	me := wantErr(t, err, model.OutcomeUnknown)
	if me.Retry != model.ReconcileFirst {
		t.Fatalf("retry = %q, want reconcile_first", me.Retry)
	}
	if len(out.References) == 0 || out.References[0] != model.ProviderMessageRef(testChan, "draft-1") {
		t.Fatalf("references = %+v, want the draft's", out.References)
	}
	if _, ok := e.box.messages["draft-1"]; !ok {
		t.Fatal("draft deleted although the send may have happened")
	}
}

func TestReconcileFindsSentCopy(t *testing.T) {
	e := newEnv(t, Config{})
	e.box.responses = []conformance.Response{{Outcome: conformance.OutcomeAnswerLost}}
	if _, err := send(t, e, route("peer@example.test", ""), "a1"); model.CodeOf(err) != model.OutcomeUnknown {
		t.Fatalf("lost answer = %v, want outcome_unknown", err)
	}
	out, err := e.inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: effect(route("peer@example.test", ""), "a1")})
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 2 {
		t.Fatalf("Reconcile = %+v, %v", out, err)
	}
	other, err := e.inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: effect(route("peer@example.test", ""), "a2")})
	if err != nil || other.State != model.Failed {
		t.Fatalf("Reconcile of another attempt = %+v, %v; want failed", other, err)
	}
}

func TestReconcileDeletesUnsentDraftBeforeAllowingRetry(t *testing.T) {
	e := newEnv(t, Config{})
	e.box.responses = []conformance.Response{{Outcome: conformance.OutcomeRequestLost}}
	_, _ = send(t, e, route("peer@example.test", ""), "a1")
	out, err := e.inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: effect(route("peer@example.test", ""), "a1")})
	if err != nil || out.State != model.Failed {
		t.Fatalf("Reconcile = %+v, %v; want failed", out, err)
	}
	if _, ok := e.box.messages["draft-1"]; ok {
		t.Fatal("unsent draft left behind: it could still be sent next to a retry")
	}
}

func TestReconcileWaitsOnOutboxCopy(t *testing.T) {
	e := newEnv(t, Config{})
	e.box.responses = []conformance.Response{{Outcome: conformance.OutcomeRequestLost}}
	_, _ = send(t, e, route("peer@example.test", ""), "a1")
	e.box.messages["draft-1"].m.IsDraft = false // submitted, still in the Outbox
	_, err := e.inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: effect(route("peer@example.test", ""), "a1")})
	wantCode(t, err, model.Transient)
}

// The send call has no idempotency key: a server error or a dropped answer
// after the request was written is outcome_unknown, and Send never sends
// again on its own; only Reconcile may decide a retry is safe.
func TestAmbiguousSendIsUnknownAndNeverResent(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusGatewayTimeout, 0} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			e := newEnv(t, Config{})
			e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasSuffix(r.URL.Path, "/send") {
					return false
				}
				if status == 0 {
					panic(http.ErrAbortHandler) // written, answer lost
				}
				graphError(w, status, "UnknownError")
				return true
			}
			out, err := send(t, e, route("peer@example.test", ""), "a1")
			me := wantErr(t, err, model.OutcomeUnknown)
			if me.Retry != model.ReconcileFirst || len(out.References) == 0 {
				t.Fatalf("outcome %+v, %+v; want reconcile_first with the draft's references", out, me)
			}
			if n := e.g.count("POST /v1.0/users/" + testMailbox + "/messages/draft-1/send"); n != 1 {
				t.Fatalf("send calls = %d, want exactly 1", n)
			}
			if _, ok := e.box.messages["draft-1"]; !ok {
				t.Fatal("draft deleted although Graph may have sent it")
			}
		})
	}
}

// unreachableSend routes the send call to a closed port, so the HTTP
// client fails to connect before writing anything.
type unreachableSend struct {
	inner  http.RoundTripper
	closed string
}

func (u unreachableSend) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/send") {
		r = r.Clone(r.Context())
		r.URL.Host = u.closed
		return (&http.Transport{}).RoundTrip(r)
	}
	return u.inner.RoundTrip(r)
}

func TestSendNeverWrittenIsTransient(t *testing.T) {
	e := newEnv(t, Config{})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := l.Addr().String()
	_ = l.Close()
	e.inst.c.http = &http.Client{Transport: unreachableSend{inner: e.g.srv.Client().Transport, closed: closed}}
	out, err := send(t, e, route("peer@example.test", ""), "a1")
	me := wantErr(t, err, model.Transient)
	if me.Retry != model.RetrySameOperation || out.State != "" {
		t.Fatalf("outcome %+v, %+v; want a plain retryable failure", out, me)
	}
	if _, ok := e.box.messages["draft-1"]; ok {
		t.Fatal("unsent draft kept")
	}
}

// Graph's Sent Items copy may appear after the send: the descriptor
// declares a settle window, so the engine does not read an early "not
// found" as "not sent".
func TestDescribeDeclaresReconcileSettle(t *testing.T) {
	if d := NewFactory(Options{}).Describe(); d.ReconcileSettle != ReconcileSettle || ReconcileSettle <= 0 {
		t.Fatalf("ReconcileSettle = %v, want the declared window %v", d.ReconcileSettle, ReconcileSettle)
	}
}

func graphAddrs(rs []gRecipient) []string { return recipientsOf(rs) }

// parentWithRecipients delivers a message from the peer to the mailbox and
// bob, copied to carol.
func (e *env) parentWithRecipients() {
	e.inbound("m1", "peer@example.test", "hi")
	e.box.messages["m1"].m.ConversationID = "conv-1"
	e.box.messages["m1"].m.ToRecipients = []gRecipient{
		{EmailAddress: gEmailAddress{Address: testMailbox}}, {EmailAddress: gEmailAddress{Address: "bob@example.test"}},
	}
	e.box.messages["m1"].m.CcRecipients = []gRecipient{{EmailAddress: gEmailAddress{Address: "carol@example.test"}}}
}

// A reply to all is createReplyAll; the draft's recipients are the peer and
// the other To recipients, never the mailbox, with the Cc of the original,
// the explicit Cc and the blind recipients.
func TestSendReplyAllUsesCreateReplyAll(t *testing.T) {
	e := newEnv(t, Config{Addresses: []string{testMailbox}})
	e.parentWithRecipients()
	rec := &model.ReplyRecipients{ReplyAll: true, Cc: []string{"extra@example.test"}, Bcc: []string{"audit@example.test"}}
	out, err := e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(route("peer@example.test", "m1"), "a1"), Content: textContent("thanks"), Recipients: rec})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	want := []string{
		"POST /v1.0/users/" + testMailbox + "/messages/m1/createReplyAll",
		"PATCH /v1.0/users/" + testMailbox + "/messages/draft-1",
		"POST /v1.0/users/" + testMailbox + "/messages/draft-1/send",
	}
	if calls := e.messageCalls(); !slices.Equal(calls, want) {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
	sent := e.box.messages["draft-1"]
	if got, want := graphAddrs(sent.m.ToRecipients), []string{"peer@example.test", "bob@example.test"}; !slices.Equal(got, want) {
		t.Errorf("To = %v, want %v", got, want)
	}
	if got, want := graphAddrs(sent.m.CcRecipients), []string{"carol@example.test", "extra@example.test"}; !slices.Equal(got, want) {
		t.Errorf("Cc = %v, want %v", got, want)
	}
	if got := graphAddrs(sent.bcc); !slices.Equal(got, []string{"audit@example.test"}) {
		t.Errorf("Bcc = %v", got)
	}
}

// Without reply to all the parent's recipients are not used: createReply,
// the route alone, and the explicit Cc.
func TestSendCcWithoutReplyAllUsesCreateReply(t *testing.T) {
	e := newEnv(t, Config{Addresses: []string{testMailbox}})
	e.parentWithRecipients()
	rec := &model.ReplyRecipients{Cc: []string{"extra@example.test"}}
	if out, err := e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(route("peer@example.test", "m1"), "a1"), Content: textContent("thanks"), Recipients: rec}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	if e.g.count("POST /v1.0/users/"+testMailbox+"/messages/m1/createReply") != 1 || e.g.count("createReplyAll") != 0 {
		t.Fatalf("requests = %q", e.g.requests())
	}
	sent := e.box.messages["draft-1"]
	if got := graphAddrs(sent.m.ToRecipients); !slices.Equal(got, []string{"peer@example.test"}) {
		t.Errorf("To = %v", got)
	}
	if got := graphAddrs(sent.m.CcRecipients); !slices.Equal(got, []string{"extra@example.test"}) {
		t.Errorf("Cc = %v", got)
	}
}

func TestSendReplyAllToGoneParentStartsANewMessageToTheRoute(t *testing.T) {
	e := newEnv(t, Config{})
	rec := &model.ReplyRecipients{ReplyAll: true, Cc: []string{"extra@example.test"}}
	if out, err := e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(route("peer@example.test", "gone"), "a1"), Content: textContent("hi"), Recipients: rec}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	d := e.box.messages["draft-1"]
	if d == nil || d.parent != "" || !slices.Equal(graphAddrs(d.m.ToRecipients), []string{"peer@example.test"}) || !slices.Equal(graphAddrs(d.m.CcRecipients), []string{"extra@example.test"}) {
		t.Fatalf("new draft = %+v", d)
	}
}

func TestSendTooManyRecipientsIsRefusedBeforeAnyDraft(t *testing.T) {
	e := newEnv(t, Config{})
	var cc []string
	for i := 0; i < 50; i++ {
		cc = append(cc, fmt.Sprintf("u%d@example.test", i))
	}
	_, err := e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(route("peer@example.test", "m1"), "a1"), Content: textContent("hi"), Recipients: &model.ReplyRecipients{Cc: cc}})
	if !model.IsCode(err, model.Validation) {
		t.Fatalf("Send = %v, want validation", err)
	}
	if calls := e.messageCalls(); len(calls) != 0 {
		t.Errorf("a refused send reached Graph: %q", calls)
	}
}

// A reply to all that grows past the bound once the parent's recipients are
// known removes the draft it made.
func TestSendReplyAllTooManyRemovesTheDraft(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", "peer@example.test", "hi")
	var to []gRecipient
	for i := 0; i < 50; i++ {
		to = append(to, gRecipient{EmailAddress: gEmailAddress{Address: fmt.Sprintf("u%d@example.test", i)}})
	}
	e.box.messages["m1"].m.ToRecipients = to
	_, err := e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(route("peer@example.test", "m1"), "a1"), Content: textContent("hi"), Recipients: &model.ReplyRecipients{ReplyAll: true}})
	if !model.IsCode(err, model.Validation) {
		t.Fatalf("Send = %v, want validation", err)
	}
	if e.message("draft-1") != nil || e.g.count("DELETE") != 1 {
		t.Errorf("draft left behind: requests %q", e.g.requests())
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
