package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	netsmtp "net/smtp"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
	"github.com/jackc/pgx/v5/pgxpool"

	"hop.top/uma"
	"hop.top/uma/adapter/mail/smtp"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/adapter/web/widget/storelog"
	"hop.top/uma/model"
	"hop.top/uma/store/postgres"
)

const tenant model.TenantID = "inbox-test"

const (
	operatorToken = "operator-token-0123456789"
	relayUser     = "relay-user"
	relayPassword = "relay-secret-7d1c" // #nosec G101 -- test fixture
)

func testSecrets() *Secrets {
	return &Secrets{
		WidgetKey: []byte("0123456789abcdef0123456789abcdef"),
		RelayUser: relayUser, RelayPassword: []byte(relayPassword),
	}
}

// testConfig is a host on loopback ports chosen by the kernel, with fast
// polling and retries so a test does not wait on production pacing.
func testConfig(t *testing.T, pool *pgxpool.Pool, secrets *Secrets, relayPort int) Config {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		Pool:          pool,
		Tenant:        tenant,
		HTTP:          ln,
		Site:          "support",
		MailAddr:      "127.0.0.1:0",
		MailHostname:  "mx.example.com",
		MailAddress:   "support@example.com",
		MailResolver:  noDNS{},
		Relay:         smtp.Relay{Host: "127.0.0.1", PlainPort: relayPort, AllowPlaintextLoopback: true, CommandTimeoutMS: 5000, DataTimeoutMS: 5000},
		Credentials:   secrets,
		OperatorToken: operatorToken,
		Retry:         uma.RetryPolicy{MaxAttempts: 10, BaseDelay: 100 * time.Millisecond, MaxDelay: 200 * time.Millisecond},
		Poll:          20 * time.Millisecond,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// start runs a host for the test and shuts it down when the test ends,
// unless the test already did.
func start(t *testing.T, cfg Config) *Host {
	t.Helper()
	h, err := Start(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { h.stop(t) })
	return h
}

var stopped sync.Map

// stop shuts h down once, failing the test on error.
func (h *Host) stop(t *testing.T) {
	t.Helper()
	if _, done := stopped.LoadOrStore(h, true); done {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := h.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// --- HTTP clients ---------------------------------------------------------

// client talks to one host's HTTP surface.
type client struct {
	t    *testing.T
	base string
}

func httpClient(t *testing.T, h *Host) client {
	return client{t: t, base: "http://" + h.cfg.HTTP.Addr().String()}
}

// do sends a request and decodes a JSON answer into out when it is not nil.
func (c client) do(method, path, token string, body, out any, header ...string) int {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(c.t.Context(), method, c.base+path, r)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	data, _ := io.ReadAll(res.Body)
	if out != nil && res.StatusCode < 300 {
		if err := json.Unmarshal(data, out); err != nil {
			c.t.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
	return res.StatusCode
}

func (c client) session() widget.SessionResponse {
	c.t.Helper()
	var s widget.SessionResponse
	if code := c.do(http.MethodPost, "/widget/session", "", struct{}{}, &s); code != http.StatusOK {
		c.t.Fatalf("session = %d", code)
	}
	return s
}

func (c client) say(s widget.SessionResponse, id, text string) {
	c.t.Helper()
	if code := c.do(http.MethodPost, "/widget/messages", s.Token, widget.MessageRequest{ID: id, Text: text}, nil); code != http.StatusAccepted {
		c.t.Fatalf("visitor message = %d", code)
	}
}

func (c client) conversations() []model.Conversation {
	c.t.Helper()
	var p model.Page[model.Conversation]
	if code := c.do(http.MethodGet, "/operator/conversations", operatorToken, nil, &p); code != http.StatusOK {
		c.t.Fatalf("list conversations = %d", code)
	}
	return p.Items
}

func convPath(conv model.ConversationRef) string {
	return "/operator/inboxes/" + string(conv.Inbox.ID) + "/conversations/" + string(conv.ID)
}

func (c client) messages(conv model.ConversationRef) []model.Message {
	c.t.Helper()
	var p model.Page[model.Message]
	if code := c.do(http.MethodGet, convPath(conv)+"/messages", operatorToken, nil, &p); code != http.StatusOK {
		c.t.Fatalf("list messages = %d", code)
	}
	return p.Items
}

// command posts a reply or note under an idempotency key.
func (c client) command(conv model.ConversationRef, kind, key string, body commandBody) model.Acceptance {
	c.t.Helper()
	var acc model.Acceptance
	code := c.do(http.MethodPost, convPath(conv)+"/"+kind, operatorToken, body, &acc, "Idempotency-Key", key)
	if code != http.StatusAccepted {
		c.t.Fatalf("%s = %d", kind, code)
	}
	return acc
}

func (c client) operation(ref model.OperationRef) model.Operation {
	c.t.Helper()
	var op model.Operation
	if code := c.do(http.MethodGet, "/operator/operations/"+string(ref.ID), operatorToken, nil, &op); code != http.StatusOK {
		c.t.Fatalf("get operation = %d", code)
	}
	return op
}

// settle polls the operation until it is settled: neither queued nor
// failed with a retry still to come.
func (c client) settle(ref model.OperationRef) model.Operation {
	c.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		op := c.operation(ref)
		if !pending(op) || time.Now().After(deadline) {
			return op
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pending reports whether the engine will still act on op: it is queued,
// or waits for its next attempt.
func pending(op model.Operation) bool {
	return op.State == model.Queued || op.State == model.RetryPending
}

// eventually polls cond for up to ten seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func textOf(m model.Message) string {
	var parts []string
	for _, p := range m.Content.Parts {
		parts = append(parts, p.Text)
	}
	return strings.Join(parts, " ")
}

// --- event stream ---------------------------------------------------------

type sseEvent struct {
	ID, Name string
	Data     widget.Event
}

// stream is one visitor event stream.
type stream struct {
	events chan sseEvent
	cancel context.CancelFunc
	done   chan struct{}
}

func (c client) stream(s widget.SessionResponse, lastEventID string) *stream {
	c.t.Helper()
	ctx, cancel := context.WithCancel(c.t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/widget/events?token="+url.QueryEscape(s.Token), nil)
	if err != nil {
		c.t.Fatal(err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		c.t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		_ = res.Body.Close()
		cancel()
		c.t.Fatalf("event stream = %d", res.StatusCode)
	}
	st := &stream{events: make(chan sseEvent, 64), cancel: cancel, done: make(chan struct{})}
	go st.read(res.Body)
	c.t.Cleanup(st.close)
	return st
}

func (s *stream) close() {
	s.cancel()
	<-s.done
}

func (s *stream) read(body io.ReadCloser) {
	defer close(s.done)
	defer close(s.events)
	defer func() { _ = body.Close() }()
	sc := bufio.NewScanner(body)
	var ev sseEvent
	var data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if data != "" {
				_ = json.Unmarshal([]byte(data), &ev.Data)
				s.events <- ev
			}
			ev, data = sseEvent{}, ""
		case strings.HasPrefix(line, "id: "):
			ev.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			ev.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func (s *stream) next(t *testing.T) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-s.events:
		if !ok {
			t.Fatal("event stream ended")
		}
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("no event within 10s")
	}
	return sseEvent{}
}

// quiet fails if the stream delivers an event within d.
func (s *stream) quiet(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case ev, ok := <-s.events:
		if ok {
			t.Fatalf("unexpected event %+v", ev)
		}
	case <-time.After(d):
	}
}

func eventText(e widget.Event) string {
	var parts []string
	for _, p := range e.Parts {
		parts = append(parts, p.Text)
	}
	return strings.Join(parts, " ")
}

// --- mail -----------------------------------------------------------------

// sendMail delivers one message to the host's MX receiver as a sending MTA
// would.
func sendMail(t *testing.T, h *Host, from, id, inReplyTo, body string) {
	t.Helper()
	eventually(t, "mail receiver", func() bool { return h.MailAddr() != "" })
	headers := "From: " + from + "\r\nTo: support@example.com\r\nSubject: Help\r\nMessage-ID: " + id + "\r\n"
	if inReplyTo != "" {
		headers += "In-Reply-To: " + inReplyTo + "\r\nReferences: " + inReplyTo + "\r\n"
	}
	msg := headers + "\r\n" + body + "\r\n"
	if err := netsmtp.SendMail(h.MailAddr(), nil, from, []string{"support@example.com"}, []byte(msg)); err != nil {
		t.Fatalf("send mail: %v", err)
	}
}

// noDNS answers every sender evidence lookup with "no such name", so mail
// is graded DMARC none without reaching the network.
type noDNS struct{}

func notFound(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (noDNS) LookupTXT(_ context.Context, name string) ([]string, error) { return nil, notFound(name) }
func (noDNS) LookupMX(_ context.Context, name string) ([]*net.MX, error) { return nil, notFound(name) }
func (noDNS) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	return nil, notFound(host)
}
func (noDNS) LookupAddr(_ context.Context, addr string) ([]string, error) { return nil, notFound(addr) }

// relay is a loopback submission server taking AUTH PLAIN without TLS, the
// shape of a local development relay.
type relay struct {
	port int
	mu   sync.Mutex
	mail []relayed
}

type relayed struct {
	from string
	to   []string
	data string
}

func newRelay(t *testing.T) *relay {
	t.Helper()
	r := &relay{}
	srv := gosmtp.NewServer(r)
	srv.Domain = "relay.test"
	srv.AllowInsecureAuth = true
	srv.ReadTimeout, srv.WriteTimeout = 10*time.Second, 10*time.Second
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.port = ln.Addr().(*net.TCPAddr).Port
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return r
}

func (r *relay) received() []relayed {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]relayed(nil), r.mail...)
}

func (r *relay) NewSession(*gosmtp.Conn) (gosmtp.Session, error) { return &relaySession{r: r}, nil }

type relaySession struct {
	r      *relay
	authed bool
	cur    relayed
}

func (s *relaySession) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *relaySession) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, user, pass string) error {
		if user != relayUser || pass != relayPassword {
			return gosmtp.ErrAuthFailed
		}
		s.authed = true
		return nil
	}), nil
}

func (s *relaySession) Mail(from string, _ *gosmtp.MailOptions) error {
	if !s.authed {
		return gosmtp.ErrAuthRequired
	}
	s.cur = relayed{from: from}
	return nil
}

func (s *relaySession) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	s.cur.to = append(s.cur.to, to)
	return nil
}

func (s *relaySession) Data(r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.cur.data = string(b)
	s.r.mu.Lock()
	s.r.mail = append(s.r.mail, s.cur)
	s.r.mu.Unlock()
	return nil
}

func (s *relaySession) Reset()        { s.cur = relayed{} }
func (s *relaySession) Logout() error { return nil }

// storeLog is the visitor log the host keeps by default, over pool. It
// migrates the database first, as the host does, since a test may build the
// log before any host has started.
func storeLog(t *testing.T, pool *pgxpool.Pool) *storelog.Log {
	t.Helper()
	if err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("postgres.Migrate: %v", err)
	}
	st, err := postgres.Open(t.Context(), pool, postgres.Options{})
	if err != nil {
		t.Fatalf("postgres.Open: %v", err)
	}
	l, err := storelog.New(st, storelog.Options{})
	if err != nil {
		t.Fatalf("storelog.New: %v", err)
	}
	return l
}
