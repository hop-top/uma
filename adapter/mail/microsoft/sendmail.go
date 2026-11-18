package microsoft

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/recipients"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// postSendMail posts one sendMail call and returns Graph's answer: 202
// Accepted with an empty body and no message ID. It is the single code path
// of both the adapter's direct mode and the standalone Mailer, so the token
// handling (a 401 resolves one fresh token, the second is
// model.Unauthorized), the status mapping and the content-free errors are
// the same for both.
func postSendMail(ctx context.Context, s *session, u string, m gSendMail) (response, error) {
	return s.do(ctx, request{method: "POST", url: u, body: encode(m), contentType: "application/json"})
}

// sendDirect is Send in SendModeDirect: the draft, update and send calls
// are one sendMail call. A route with a parent message (a reply in a
// thread) is refused: createReply needs a draft. The outcomes follow the
// draft send call: 202 is provider_accepted, with no provider reference; 429
// and 503 are the retryable throttling answers; a connect failure that
// proves nothing was written is transient; a lost answer and any other 5xx
// are outcome_unknown, never repeated by the adapter and, since a direct
// channel cannot reconcile, left to the host; other 4xx are rejected.
func (i *instance) sendDirect(ctx context.Context, req adapter.SendRequest, text, subject string) (adapter.Outcome, error) {
	if req.Route.ParentID != "" {
		return adapter.Outcome{State: model.Rejected, Failure: &model.Error{
			Code: model.Unsupported, SafeMessage: "direct mode sends new messages only; a reply in a thread needs draft mode", Retry: model.NoRetry,
		}}, nil
	}
	if req.Recipients != nil && req.Recipients.ReplyAll {
		return adapter.Outcome{State: model.Rejected, Failure: &model.Error{
			Code: model.Unsupported, SafeMessage: "direct mode cannot reply to all", Retry: model.NoRetry,
		}}, nil
	}
	set, err := recipients.Plan(req.Recipients, req.Route.ID, i.cfg.selfAddresses(), nil, nil)
	if err != nil {
		return rejectedOr(err)
	}
	msg := gSendMail{
		Message: gSendMessage{
			Subject:       subject,
			Body:          gItemBody{ContentType: "text", Content: text},
			ToRecipients:  graphRecipients(set.To),
			CcRecipients:  graphRecipients(set.Cc),
			BccRecipients: graphRecipients(set.Bcc),
		},
		SaveToSentItems: i.cfg.SaveToSentItems,
	}
	resp, err := postSendMail(ctx, i.c.session(), i.c.api(i.cfg.mailboxPath()+"/sendMail", nil), msg)
	switch {
	case err == nil:
		return adapter.Outcome{State: model.ProviderAccepted}, nil
	case errors.Is(err, errUnsent) && !isContext(err):
		return adapter.Outcome{}, model.NewError(model.Transient, "graph unreachable; send not attempted")
	case errors.Is(err, errTransport), sendAmbiguous(resp.status):
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "graph send answer lost")
	}
	return rejectedOr(err)
}

// Token is an access token and, when known, its expiry; a zero ExpiresAt is
// a token the source does not date.
type Token struct {
	AccessToken string
	ExpiresAt   time.Time
}

// TokenSource supplies the Graph access token to a Mailer. It has the shape
// of hop.top/kit's storage/secret/oauth TokenSource (a cached
// client-credentials grant): Token returns the cached token, minting one
// when none is valid, and Invalidate drops it so the next Token mints
// another. The Mailer calls Invalidate before it asks again after a 401 or
// an expired token, so a rejected token is never handed back.
//
// This package does not import kit, so kit's Source does not satisfy the
// interface directly (its Token type is kit's own); wrap it with
// TokenSourceFunc.
type TokenSource interface {
	Token(ctx context.Context) (Token, error)
	Invalidate()
}

// TokenSourceFunc builds a TokenSource from a token getter and an
// invalidate function (nil is a no-op). With a kit source src:
//
//	microsoft.TokenSourceFunc(func(ctx context.Context) (string, time.Time, error) {
//		t, err := src.Token(ctx)
//		return t.AccessToken, t.ExpiresAt, err
//	}, src.Invalidate)
func TokenSourceFunc(get func(ctx context.Context) (accessToken string, expiresAt time.Time, err error), invalidate func()) TokenSource {
	return funcTokens{get: get, invalidate: invalidate}
}

type funcTokens struct {
	get        func(context.Context) (string, time.Time, error)
	invalidate func()
}

func (f funcTokens) Token(ctx context.Context) (Token, error) {
	a, e, err := f.get(ctx)
	return Token{AccessToken: a, ExpiresAt: e}, err
}

func (f funcTokens) Invalidate() {
	if f.invalidate != nil {
		f.invalidate()
	}
}

// Message is a message for a Mailer to send.
type Message struct {
	// From is the sender address; empty sends as the Mailer's mailbox. Graph
	// refuses an address the token may not send as.
	From string
	// To, Cc and Bcc are bare recipient addresses: at least one in all, at
	// most recipients.Max.
	To, Cc, Bcc []string
	// ReplyTo are the addresses replies go to.
	ReplyTo []string
	Subject string
	// Text is the plain-text body. HTML, when set, is the HTML alternative:
	// Graph's JSON message holds a single body, so the message is sent as
	// that HTML, and Text is the body only when HTML is empty. Set both to
	// keep one source of truth in the caller; a recipient's client shows
	// the HTML.
	Text string
	HTML string
}

// MailerConfig configures a Mailer.
type MailerConfig struct {
	// Mailbox is the user ID or user principal name the message is sent
	// from (/users/{mailbox}/sendMail). Empty is the signed-in user (/me),
	// which needs a delegated token.
	Mailbox string
	// Cloud selects the national cloud endpoint; empty is CloudGlobal.
	Cloud string
	// Endpoint overrides the cloud's Graph root, for tests.
	Endpoint string
	// HTTP is the host's HTTP client: a ports.HTTPClient, so an
	// *http.Client works. Its timeouts, proxy and instrumentation apply.
	HTTP ports.HTTPClient
	// Tokens supplies the access token (Mail.Send).
	Tokens TokenSource
	// SaveToSentItems keeps a copy of each message in the mailbox's Sent
	// Items. False (the default) keeps none.
	SaveToSentItems bool
	// MaxRetries bounds the retries after the first attempt of a throttled
	// (429), unavailable (503) or failed (other 5xx) call. Zero is three;
	// negative is none.
	MaxRetries int
	// RetryBackoff is the first wait when Graph sends no Retry-After; each
	// retry doubles it. Zero is 500 ms.
	RetryBackoff time.Duration
	// MaxRetryWait caps one wait. A Retry-After above it is not waited
	// on: the call fails at once with its RetryAfter set. Zero is 30 s.
	MaxRetryWait time.Duration
	// Sleep waits d or until ctx ends, returning ctx's error then. Nil
	// uses a timer; tests inject a recorder.
	Sleep func(ctx context.Context, d time.Duration) error
}

const (
	defaultMaxRetries   = 3
	defaultRetryBackoff = 500 * time.Millisecond
	defaultMaxRetryWait = 30 * time.Second
)

// Mailer sends messages through Graph's sendMail with Mail.Send alone, with
// no engine: no channel, route, effect or reconcile. It shares its HTTP and
// token handling with the adapter's SendModeDirect (postSendMail).
//
// Send is at-least-once: a 5xx other than 503 may have followed a delivery,
// and is retried like a throttling answer, so a message can arrive twice.
// A call whose answer never arrived is not retried: its error is
// model.OutcomeUnknown. A Mailer is safe for concurrent use when its
// TokenSource and HTTP client are.
type Mailer struct {
	c          *client
	mailbox    string
	save       bool
	maxRetries int
	backoff    time.Duration
	maxWait    time.Duration
	sleep      func(context.Context, time.Duration) error
}

// NewMailer checks cfg and returns a Mailer. It makes no call.
func NewMailer(cfg MailerConfig) (*Mailer, error) {
	switch {
	case cfg.HTTP == nil:
		return nil, model.NewError(model.Validation, "mailer needs an HTTP client")
	case cfg.Tokens == nil:
		return nil, model.NewError(model.Validation, "mailer needs a token source")
	case cfg.Mailbox != "" && !plainToken(cfg.Mailbox):
		return nil, model.NewError(model.Validation, "mailbox must be a user ID or user principal name")
	}
	if _, ok := cloudEndpoints[cfg.Cloud]; cfg.Cloud != "" && !ok {
		return nil, model.NewError(model.Validation, "cloud must be global, usgov, usgov_dod or china")
	}
	endpoint := strings.TrimRight(Config{Cloud: cfg.Cloud}.endpoint(), "/")
	if cfg.Endpoint != "" {
		endpoint = strings.TrimRight(cfg.Endpoint, "/")
	}
	origin, err := url.Parse(endpoint)
	if err != nil || origin.Host == "" {
		return nil, model.NewError(model.Validation, "invalid Graph endpoint")
	}
	m := &Mailer{
		mailbox: cfg.Mailbox, save: cfg.SaveToSentItems,
		maxRetries: cfg.MaxRetries, backoff: cfg.RetryBackoff, maxWait: cfg.MaxRetryWait, sleep: cfg.Sleep,
	}
	switch {
	case m.maxRetries == 0:
		m.maxRetries = defaultMaxRetries
	case m.maxRetries < 0:
		m.maxRetries = 0
	}
	if m.backoff <= 0 {
		m.backoff = defaultRetryBackoff
	}
	if m.maxWait <= 0 {
		m.maxWait = defaultMaxRetryWait
	}
	if m.sleep == nil {
		m.sleep = sleepContext
	}
	m.c = &client{
		http: cfg.HTTP, endpoint: endpoint, origin: origin, now: time.Now,
		tokens: tokenFunc(cfg.Tokens),
	}
	return m, nil
}

// tokenFunc adapts a TokenSource to the client's token hook: a Fresh
// request invalidates the source first, so the token handed back is new.
// A source's own error text never surfaces: it may hold token material.
func tokenFunc(src TokenSource) func(context.Context, bool) (string, time.Time, error) {
	return func(ctx context.Context, fresh bool) (string, time.Time, error) {
		if fresh {
			src.Invalidate()
		}
		t, err := src.Token(ctx)
		if err != nil {
			var me *model.Error
			switch {
			case errors.As(err, &me):
				return "", time.Time{}, me
			case ctx.Err() != nil:
				return "", time.Time{}, ctx.Err()
			}
			return "", time.Time{}, model.NewError(model.Unavailable, "token source failed")
		}
		if t.AccessToken == "" {
			return "", time.Time{}, &model.Error{Code: model.Unauthorized, SafeMessage: "empty access token", Retry: model.NoRetry}
		}
		return t.AccessToken, t.ExpiresAt, nil
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Send validates msg and delivers it with one sendMail call, retrying
// throttling and server errors within the bounds of MaxRetries. A
// model.Error reports the failure, never carrying the message, an address
// or a token: model.Validation for an invalid message (nothing sent),
// model.Unauthorized after a fresh token was refused or for a missing
// permission, model.RateLimited or model.Transient when the retries ran
// out, model.OutcomeUnknown when an answer was lost, model.Permanent for a
// refused message. ctx's error is returned when it ends.
func (m *Mailer) Send(ctx context.Context, msg Message) error {
	body, err := m.build(msg)
	if err != nil {
		return err
	}
	s := m.c.session()
	u := m.c.api(m.mailboxPath()+"/sendMail", nil)
	for attempt := 0; ; attempt++ {
		_, err := postSendMail(ctx, s, u, body)
		if err == nil {
			return nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		wait, retry, final := m.next(attempt, err)
		if !retry || attempt >= m.maxRetries {
			return final
		}
		if err := m.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// next classifies a failed call: whether it may be repeated, after what
// wait, and the safe error to return when it is not.
func (m *Mailer) next(attempt int, err error) (wait time.Duration, retry bool, final error) {
	switch {
	case errors.Is(err, errUnsent):
		// Nothing was written: repeating cannot duplicate.
		return m.backoffFor(attempt), true, model.NewError(model.Transient, "graph unreachable; send not attempted")
	case errors.Is(err, errTransport):
		return 0, false, model.NewError(model.OutcomeUnknown, "graph send answer lost")
	}
	var me *model.Error
	if !errors.As(err, &me) {
		return 0, false, model.NewError(model.Unavailable, "send failed")
	}
	if me.Code != model.RateLimited && me.Code != model.Transient {
		return 0, false, me
	}
	if me.RetryAfter > m.maxWait {
		return 0, false, me
	}
	if me.RetryAfter > 0 {
		return me.RetryAfter, true, me
	}
	return m.backoffFor(attempt), true, me
}

// backoffFor is the wait before retry number attempt+1: the first wait,
// doubled per retry, at most the cap.
func (m *Mailer) backoffFor(attempt int) time.Duration {
	d := m.backoff
	for range attempt {
		if d >= m.maxWait {
			break
		}
		d *= 2
	}
	return min(d, m.maxWait)
}

func (m *Mailer) mailboxPath() string { return Config{Mailbox: m.mailbox}.mailboxPath() }

// build checks msg and returns the sendMail body. The errors name the
// field, never its value.
func (m *Mailer) build(msg Message) (gSendMail, error) {
	invalid := func(reason string) (gSendMail, error) {
		return gSendMail{}, model.NewError(model.Validation, reason)
	}
	if len(msg.To)+len(msg.Cc)+len(msg.Bcc) == 0 {
		return invalid("message needs a recipient")
	}
	if len(msg.To)+len(msg.Cc)+len(msg.Bcc) > recipients.Max {
		return invalid("message has too many recipients")
	}
	for field, list := range map[string][]string{"to": msg.To, "cc": msg.Cc, "bcc": msg.Bcc, "reply-to": msg.ReplyTo} {
		for _, a := range list {
			if !bareAddress(a) {
				return invalid(field + " must be bare addresses")
			}
		}
	}
	if msg.From != "" && !bareAddress(msg.From) {
		return invalid("from must be a bare address")
	}
	if strings.ContainsAny(msg.Subject, "\r\n") {
		return invalid("subject must be one line")
	}
	body := gItemBody{ContentType: "text", Content: msg.Text}
	if msg.HTML != "" {
		body = gItemBody{ContentType: "html", Content: msg.HTML}
	}
	if body.Content == "" {
		return invalid("message needs a body")
	}
	out := gSendMail{
		Message: gSendMessage{
			Subject: msg.Subject, Body: body,
			ToRecipients: graphRecipients(msg.To), CcRecipients: graphRecipients(msg.Cc), BccRecipients: graphRecipients(msg.Bcc),
			ReplyTo: graphRecipients(msg.ReplyTo),
		},
		SaveToSentItems: m.save,
	}
	if msg.From != "" {
		out.Message.From = &gRecipient{EmailAddress: gEmailAddress{Address: msg.From}}
	}
	return out, nil
}

// bareAddress reports a plain token holding an "@" between two non-empty
// parts.
func bareAddress(a string) bool {
	at := strings.IndexByte(a, '@')
	return plainToken(a) && at > 0 && at < len(a)-1 && strings.Count(a, "@") == 1
}
