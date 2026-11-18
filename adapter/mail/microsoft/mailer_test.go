package microsoft

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/model"
)

// fakeTokens is a TokenSource with the shape of a cached client-credentials
// source: Token returns the cached token, Invalidate drops it so the next
// Token mints a new one (tok-0, tok-1, ...).
type fakeTokens struct {
	mu          sync.Mutex
	gen         int
	tokenCalls  int
	invalidates int
	expires     time.Time
	err         error
}

func (f *fakeTokens) Token(context.Context) (Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenCalls++
	if f.err != nil {
		return Token{}, f.err
	}
	return Token{AccessToken: fmt.Sprintf("tok-%d", f.gen), ExpiresAt: f.expires}, nil
}

func (f *fakeTokens) Invalidate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidates++
	f.gen++
	f.expires = time.Time{}
}

// sleeps records the waits a Mailer asked for and returns at once.
type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
	return ctx.Err()
}

func (s *sleeps) waits() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.d)
}

func newTestMailer(t *testing.T, g *fakeSendMail, tok TokenSource, mod func(*MailerConfig)) (*Mailer, *sleeps) {
	t.Helper()
	sl := &sleeps{}
	cfg := MailerConfig{
		Mailbox: testMailbox, Endpoint: g.endpoint(), HTTP: g.srv.Client(), Tokens: tok, Sleep: sl.sleep,
	}
	if mod != nil {
		mod(&cfg)
	}
	m, err := NewMailer(cfg)
	if err != nil {
		t.Fatalf("NewMailer: %v", err)
	}
	return m, sl
}

var codeMail = Message{To: []string{"user@example.test"}, Subject: "Your sign-in code", Text: "123456"}

func TestMailerSendsTextMessageWithSendMail(t *testing.T) {
	g := newFakeSendMail(t)
	m, _ := newTestMailer(t, g, &fakeTokens{}, nil)
	msg := Message{
		From: "no-reply@contoso.test", To: []string{"a@example.test", "b@example.test"}, Cc: []string{"c@example.test"},
		ReplyTo: []string{"help@contoso.test"}, Subject: "Your sign-in code", Text: "123456",
	}
	if err := m.Send(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	calls := g.seen()
	if len(calls) != 1 {
		t.Fatalf("requests = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.method != "POST" || c.path != "/v1.0/users/"+testMailbox+"/sendMail" || c.auth != "Bearer tok-0" || c.contentType != "application/json" {
		t.Fatalf("request = %+v", c)
	}
	b := decodeSendMail(t, c)
	if b.SaveToSentItems == nil || *b.SaveToSentItems {
		t.Errorf("saveToSentItems = %v, want an explicit false", b.SaveToSentItems)
	}
	if b.Message["subject"] != "Your sign-in code" {
		t.Errorf("subject = %v", b.Message["subject"])
	}
	body, _ := b.Message["body"].(map[string]any)
	if body["contentType"] != "text" || body["content"] != "123456" {
		t.Errorf("body = %v", body)
	}
	if got := wireAddrs(t, b.Message, "toRecipients"); !slices.Equal(got, msg.To) {
		t.Errorf("to = %v", got)
	}
	if got := wireAddrs(t, b.Message, "ccRecipients"); !slices.Equal(got, msg.Cc) {
		t.Errorf("cc = %v", got)
	}
	if got := wireAddrs(t, b.Message, "replyTo"); !slices.Equal(got, msg.ReplyTo) {
		t.Errorf("replyTo = %v", got)
	}
	from, _ := b.Message["from"].(map[string]any)
	if ea, _ := from["emailAddress"].(map[string]any); ea["address"] != "no-reply@contoso.test" {
		t.Errorf("from = %v", from)
	}
}

func TestMailerHTMLAlternativeIsSentAsTheHTMLBody(t *testing.T) {
	// Graph's JSON message holds one body: with an HTML alternative the body
	// is the HTML, and the text is the fallback only when there is no HTML.
	g := newFakeSendMail(t)
	m, _ := newTestMailer(t, g, &fakeTokens{}, nil)
	msg := codeMail
	msg.HTML = "<p>123456</p>"
	if err := m.Send(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	body, _ := decodeSendMail(t, g.seen()[0]).Message["body"].(map[string]any)
	if body["contentType"] != "html" || body["content"] != "<p>123456</p>" {
		t.Fatalf("body = %v", body)
	}
}

func TestMailerSaveToSentItemsAndMeMailbox(t *testing.T) {
	g := newFakeSendMail(t)
	m, _ := newTestMailer(t, g, &fakeTokens{}, func(c *MailerConfig) { c.Mailbox = ""; c.SaveToSentItems = true })
	if err := m.Send(t.Context(), codeMail); err != nil {
		t.Fatal(err)
	}
	c := g.seen()[0]
	if c.path != "/v1.0/me/sendMail" {
		t.Errorf("path = %s", c.path)
	}
	if b := decodeSendMail(t, c); b.SaveToSentItems == nil || !*b.SaveToSentItems {
		t.Errorf("saveToSentItems = %v, want true", b.SaveToSentItems)
	}
}

func TestMailerAcceptsAStdlibHTTPClient(t *testing.T) {
	g := newFakeSendMail(t)
	m, err := NewMailer(MailerConfig{Mailbox: testMailbox, Endpoint: g.endpoint(), HTTP: &http.Client{}, Tokens: &fakeTokens{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Send(t.Context(), codeMail); err != nil {
		t.Fatal(err)
	}
}

func TestMailerRefreshesTheTokenOnceOn401(t *testing.T) {
	g := newFakeSendMail(t, sendAnswer{status: 401})
	tok := &fakeTokens{}
	m, sl := newTestMailer(t, g, tok, nil)
	if err := m.Send(t.Context(), codeMail); err != nil {
		t.Fatal(err)
	}
	calls := g.seen()
	if len(calls) != 2 || calls[0].auth != "Bearer tok-0" || calls[1].auth != "Bearer tok-1" {
		t.Fatalf("calls = %+v, want tok-0 then tok-1", calls)
	}
	if tok.invalidates != 1 {
		t.Fatalf("Invalidate calls = %d, want 1", tok.invalidates)
	}
	if len(sl.waits()) != 0 {
		t.Fatalf("a 401 refresh waited: %v", sl.waits())
	}
}

func TestMailerSecond401IsUnauthorized(t *testing.T) {
	g := newFakeSendMail(t, sendAnswer{status: 401}, sendAnswer{status: 401}, sendAnswer{})
	tok := &fakeTokens{}
	m, _ := newTestMailer(t, g, tok, nil)
	err := m.Send(t.Context(), codeMail)
	wantCode(t, err, model.Unauthorized)
	if n := len(g.seen()); n != 2 || tok.invalidates != 1 {
		t.Fatalf("requests = %d, invalidates = %d; want 2 and 1", n, tok.invalidates)
	}
}

func TestMailerAnExpiredTokenIsRefreshedBeforeTheCall(t *testing.T) {
	g := newFakeSendMail(t)
	tok := &fakeTokens{expires: time.Now().Add(-time.Minute)}
	m, _ := newTestMailer(t, g, tok, nil)
	if err := m.Send(t.Context(), codeMail); err != nil {
		t.Fatal(err)
	}
	if calls := g.seen(); len(calls) != 1 || calls[0].auth != "Bearer tok-1" || tok.invalidates != 1 {
		t.Fatalf("calls = %+v, invalidates = %d; want one call with the fresh token", calls, tok.invalidates)
	}
}

func TestMailerHonoursRetryAfterOn429(t *testing.T) {
	g := newFakeSendMail(t, sendAnswer{status: 429, retryAfter: "2"}, sendAnswer{status: 503, retryAfter: "3"})
	m, sl := newTestMailer(t, g, &fakeTokens{}, nil)
	if err := m.Send(t.Context(), codeMail); err != nil {
		t.Fatal(err)
	}
	if got := sl.waits(); !slices.Equal(got, []time.Duration{2 * time.Second, 3 * time.Second}) {
		t.Fatalf("waits = %v, want [2s 3s]", got)
	}
	if n := len(g.seen()); n != 3 {
		t.Fatalf("requests = %d, want 3", n)
	}
}

func TestMailerBacksOffWithoutRetryAfter(t *testing.T) {
	g := newFakeSendMail(t, sendAnswer{status: 500}, sendAnswer{status: 500})
	m, sl := newTestMailer(t, g, &fakeTokens{}, func(c *MailerConfig) { c.RetryBackoff = 100 * time.Millisecond })
	if err := m.Send(t.Context(), codeMail); err != nil {
		t.Fatal(err)
	}
	if got := sl.waits(); !slices.Equal(got, []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}) {
		t.Fatalf("waits = %v, want [100ms 200ms]", got)
	}
}

func TestMailerRetriesAreBoundedAndExhaustTransient(t *testing.T) {
	g := newFakeSendMail(t, sendAnswer{status: 502}, sendAnswer{status: 502}, sendAnswer{status: 502}, sendAnswer{status: 502}, sendAnswer{})
	m, sl := newTestMailer(t, g, &fakeTokens{}, nil)
	err := m.Send(t.Context(), codeMail)
	wantCode(t, err, model.Transient)
	if n := len(g.seen()); n != 4 {
		t.Fatalf("requests = %d, want 4 (1 + the default 3 retries)", n)
	}
	if n := len(sl.waits()); n != 3 {
		t.Fatalf("waits = %d, want 3", n)
	}
}

func TestMailerMaxRetriesIsConfigurable(t *testing.T) {
	g := newFakeSendMail(t, sendAnswer{status: 500}, sendAnswer{status: 500}, sendAnswer{})
	m, _ := newTestMailer(t, g, &fakeTokens{}, func(c *MailerConfig) { c.MaxRetries = -1 })
	wantCode(t, m.Send(t.Context(), codeMail), model.Transient)
	if n := len(g.seen()); n != 1 {
		t.Fatalf("requests = %d, want 1 with retries off", n)
	}
}

func TestMailerExhaustedThrottlingIsRateLimited(t *testing.T) {
	g := newFakeSendMail(t, sendAnswer{status: 429, retryAfter: "1"}, sendAnswer{status: 429, retryAfter: "1"})
	m, _ := newTestMailer(t, g, &fakeTokens{}, func(c *MailerConfig) { c.MaxRetries = 1 })
	me := wantErr(t, m.Send(t.Context(), codeMail), model.RateLimited)
	if me.RetryAfter != time.Second {
		t.Fatalf("RetryAfter = %v", me.RetryAfter)
	}
}

func TestMailerDoesNotWaitPastTheRetryWaitCap(t *testing.T) {
	g := newFakeSendMail(t, sendAnswer{status: 429, retryAfter: "3600"}, sendAnswer{})
	m, sl := newTestMailer(t, g, &fakeTokens{}, func(c *MailerConfig) { c.MaxRetryWait = 5 * time.Second })
	me := wantErr(t, m.Send(t.Context(), codeMail), model.RateLimited)
	if me.RetryAfter != time.Hour || len(sl.waits()) != 0 || len(g.seen()) != 1 {
		t.Fatalf("RetryAfter %v waits %v requests %d; want the error at once", me.RetryAfter, sl.waits(), len(g.seen()))
	}
}

func TestMailerClientErrorsAreFinalAndNotRetried(t *testing.T) {
	for status, code := range map[int]model.ErrorCode{400: model.Permanent, 403: model.Unauthorized, 404: model.NotFound} {
		g := newFakeSendMail(t, sendAnswer{status: status}, sendAnswer{})
		m, _ := newTestMailer(t, g, &fakeTokens{}, nil)
		wantCode(t, m.Send(t.Context(), codeMail), code)
		if n := len(g.seen()); n != 1 {
			t.Errorf("HTTP %d: requests = %d, want 1", status, n)
		}
	}
}

func TestMailerLostAnswerIsOutcomeUnknownAndNotRetried(t *testing.T) {
	g := newFakeSendMail(t, sendAnswer{drop: true}, sendAnswer{})
	m, _ := newTestMailer(t, g, &fakeTokens{}, nil)
	wantCode(t, m.Send(t.Context(), codeMail), model.OutcomeUnknown)
	if n := len(g.seen()); n != 1 {
		t.Fatalf("requests = %d: a send whose answer was lost may have been delivered", n)
	}
}

func TestMailerCancelledContextStopsTheWait(t *testing.T) {
	g := newFakeSendMail(t, sendAnswer{status: 503, retryAfter: "1"}, sendAnswer{})
	ctx, cancel := context.WithCancel(t.Context())
	m, _ := newTestMailer(t, g, &fakeTokens{}, func(c *MailerConfig) {
		c.Sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	})
	err := m.Send(ctx, codeMail)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := len(g.seen()); n != 1 {
		t.Fatalf("requests = %d, want 1", n)
	}
}

func TestMailerTimeoutComesFromTheContext(t *testing.T) {
	g := newFakeSendMail(t)
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	m, _ := newTestMailer(t, g, &fakeTokens{}, nil)
	err := m.Send(ctx, codeMail)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestMailerErrorsCarryNoContentAddressOrToken(t *testing.T) {
	secrets := []string{"SECRET-BODY-TEXT", "SECRET-SUBJECT", "secret-recipient@leak.test", "secret-cc@leak.test", "tok-0", "tok-1", "SECRET-TOKEN-ERR"}
	msg := Message{To: []string{"secret-recipient@leak.test"}, Cc: []string{"secret-cc@leak.test"}, Subject: "SECRET-SUBJECT", Text: "SECRET-BODY-TEXT"}
	for _, status := range []int{400, 401, 403, 404, 429, 500, 503} {
		g := newFakeSendMail(t, sendAnswer{status: status}, sendAnswer{status: status}, sendAnswer{status: status}, sendAnswer{status: status})
		m, _ := newTestMailer(t, g, &fakeTokens{}, nil)
		err := m.Send(t.Context(), msg)
		if err == nil {
			t.Fatalf("HTTP %d: no error", status)
		}
		mustNotContain(t, fmt.Sprintf("HTTP %d error", status), err.Error(), secrets...)
		mustNotContain(t, fmt.Sprintf("HTTP %d error (%%+v)", status), fmt.Sprintf("%+v", err), secrets...)
	}
	// A failing token source's own text never surfaces either.
	g := newFakeSendMail(t)
	m, _ := newTestMailer(t, g, &fakeTokens{err: errors.New("idp said SECRET-TOKEN-ERR")}, nil)
	err := m.Send(t.Context(), msg)
	if err == nil {
		t.Fatal("token source failure was not reported")
	}
	mustNotContain(t, "token error", err.Error(), secrets...)
	if n := len(g.seen()); n != 0 {
		t.Fatalf("requests = %d, want none without a token", n)
	}
}

func TestMailerRefusesInvalidMessagesBeforeAnyRequest(t *testing.T) {
	for name, msg := range map[string]Message{
		"no recipient":          {Text: "x"},
		"no body":               {To: []string{"a@example.test"}},
		"bad address":           {To: []string{"secret leak@example.test"}, Text: "x"},
		"bad cc":                {To: []string{"a@example.test"}, Cc: []string{"a b"}, Text: "x"},
		"bad from":              {From: "a b", To: []string{"a@example.test"}, Text: "x"},
		"bad reply-to":          {ReplyTo: []string{"a b"}, To: []string{"a@example.test"}, Text: "x"},
		"line break in subject": {To: []string{"a@example.test"}, Subject: "a\r\nBcc: x@example.test", Text: "x"},
	} {
		g := newFakeSendMail(t)
		m, _ := newTestMailer(t, g, &fakeTokens{}, nil)
		err := m.Send(t.Context(), msg)
		wantCode(t, err, model.Validation)
		mustNotContain(t, name, err.Error(), "secret leak", "Bcc")
		if n := len(g.seen()); n != 0 {
			t.Errorf("%s: requests = %d, want none", name, n)
		}
	}
}

func TestNewMailerChecksItsConfig(t *testing.T) {
	g := newFakeSendMail(t)
	ok := MailerConfig{Mailbox: testMailbox, Endpoint: g.endpoint(), HTTP: g.srv.Client(), Tokens: &fakeTokens{}}
	for name, mod := range map[string]func(*MailerConfig){
		"no http":      func(c *MailerConfig) { c.HTTP = nil },
		"no tokens":    func(c *MailerConfig) { c.Tokens = nil },
		"bad mailbox":  func(c *MailerConfig) { c.Mailbox = "a/b" },
		"bad cloud":    func(c *MailerConfig) { c.Cloud = "moon" },
		"bad endpoint": func(c *MailerConfig) { c.Endpoint = "://" },
	} {
		c := ok
		mod(&c)
		if _, err := NewMailer(c); err == nil {
			t.Errorf("%s: NewMailer accepted the config", name)
		}
	}
	if _, err := NewMailer(ok); err != nil {
		t.Errorf("valid config: %v", err)
	}
	// Cloud selects the endpoint when none is given.
	c := ok
	c.Endpoint, c.Cloud = "", CloudUSGov
	m, err := NewMailer(c)
	if err != nil || m.c.endpoint != "https://graph.microsoft.us/v1.0" {
		t.Errorf("usgov endpoint: %v, %v", m, err)
	}
}

func TestTokenSourceFuncAdaptsAKitStyleSource(t *testing.T) {
	var invalidated int
	src := TokenSourceFunc(func(context.Context) (string, time.Time, error) { return "abc", time.Time{}, nil }, func() { invalidated++ })
	tok, err := src.Token(t.Context())
	if err != nil || tok.AccessToken != "abc" {
		t.Fatalf("Token = %+v, %v", tok, err)
	}
	src.Invalidate()
	if invalidated != 1 {
		t.Fatalf("Invalidate calls = %d", invalidated)
	}
}
