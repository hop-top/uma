package smtp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	netsmtp "net/smtp"
	"net/textproto"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/internal/livemail"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/adapter/mail/smtp"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// The live tests run smtp.mail against real SMTP software: the receiver is
// driven by the standard library's SMTP client and by a Postfix container
// acting as a sending MTA, and the sending side submits to a real submission
// server (Mailpit) over STARTTLS and AUTH. They skip unless UMA_LIVE_MAIL=1:
// the default test run and make check need neither Docker nor a mail server.
// docs/recording/imap-smtp.md says what a green run proves.

// liveTx is one transaction of the standard library's client.
type liveTx struct {
	from     string
	rcpts    []string
	startTLS bool
	size     int // declared SIZE on MAIL FROM; zero declares none
}

// liveSend runs one SMTP transaction to addr with net/smtp, an
// implementation independent of the go-smtp the receiver is built on, and
// returns the first error reply. TLS is verified against the test CA.
func liveSend(t *testing.T, addr string, tx liveTx, raw string) error {
	t.Helper()
	c, err := netsmtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Hello("client.example.test"); err != nil {
		return err
	}
	if tx.startTLS {
		if err := c.StartTLS(&tls.Config{ServerName: "localhost", RootCAs: certs(t).roots, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
		if _, ok := c.TLSConnectionState(); !ok {
			t.Fatal("STARTTLS returned without a TLS connection")
		}
	}
	if tx.size > 0 {
		if err := liveCmd(c, 250, "MAIL FROM:<%s> SIZE=%d", tx.from, tx.size); err != nil {
			return err
		}
	} else if err := c.Mail(tx.from); err != nil {
		return err
	}
	for _, r := range tx.rcpts {
		if err := c.Rcpt(r); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(crlf(raw))); err != nil {
		return err
	}
	return w.Close()
}

// liveCmd sends one raw command and returns the error of its reply unless
// the reply has the expected code.
func liveCmd(c *netsmtp.Client, expect int, format string, args ...any) error {
	id, err := c.Text.Cmd(format, args...)
	if err != nil {
		return err
	}
	c.Text.StartResponse(id)
	defer c.Text.EndResponse(id)
	_, _, err = c.Text.ReadResponse(expect)
	return err
}

func liveCode(err error) int {
	var te *textproto.Error
	if errors.As(err, &te) {
		return te.Code
	}
	return 0
}

func liveMsg(err error) string {
	var te *textproto.Error
	if errors.As(err, &te) {
		return te.Msg
	}
	return fmt.Sprint(err)
}

// liveEvent is the event identity ingress drops duplicates by.
func liveEvent(destination, raw string) string {
	h := sha256.Sum256([]byte(crlf(raw)))
	return "rcpt:" + destination + ";sha256:" + hex.EncodeToString(h[:])
}

func (s *sink) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *sink) events() []string {
	var out []string
	for _, in := range s.accepted() {
		out = append(out, in.Envelope.EventID)
	}
	return out
}

// --- the receiver ----------------------------------------------------------------

// TestLiveReceiveSurface: what a client sees of the receiver. STARTTLS is
// offered exactly when the host supplied a TLS configuration, mail is taken
// with or without it, and AUTH is neither advertised nor accepted.
func TestLiveReceiveSurface(t *testing.T) {
	livemail.Require(t)
	t.Run("with TLS", func(t *testing.T) {
		m := newMX(t, config(1), true)
		c, err := netsmtp.Dial(m.addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		if err := c.Hello("client.example.test"); err != nil {
			t.Fatal(err)
		}
		if ok, _ := c.Extension("STARTTLS"); !ok {
			t.Fatal("EHLO does not offer STARTTLS")
		}
		if ok, _ := c.Extension("AUTH"); ok {
			t.Error("EHLO offers AUTH: the receiver is an MX and takes no credentials")
		}
		if err := c.StartTLS(&tls.Config{ServerName: "localhost", RootCAs: certs(t).roots, MinVersion: tls.VersionTLS12}); err != nil {
			t.Fatalf("STARTTLS: %v", err)
		}
		st, ok := c.TLSConnectionState()
		if !ok || st.Version < tls.VersionTLS12 {
			t.Fatalf("TLS state %+v, %v", st, ok)
		}
		if ok, _ := c.Extension("AUTH"); ok {
			t.Error("EHLO offers AUTH after STARTTLS")
		}
		if err := liveCmd(c, 235, "AUTH PLAIN AGphZABzZWNyZXQ="); liveCode(err) < 500 {
			t.Errorf("AUTH PLAIN over TLS = %v, want a 5xx refusal", err)
		}
		_ = c.Quit()
	})
	t.Run("sends over TLS and in the clear", func(t *testing.T) {
		m := newMX(t, config(1), true)
		for i, startTLS := range []bool{true, false} {
			tx := liveTx{from: "jane@example.org", rcpts: []string{"support@example.com"}, startTLS: startTLS}
			raw := mail("<live-surface-"+strconv.Itoa(i)+"@example.org>", "jane@example.org", "", "hello "+strconv.FormatBool(startTLS))
			if err := liveSend(t, m.addr, tx, raw); err != nil {
				t.Fatalf("startTLS=%v: %v", startTLS, err)
			}
		}
		if n := len(m.sink.accepted()); n != 2 {
			t.Fatalf("sink calls = %d, want 2", n)
		}
	})
	t.Run("without TLS", func(t *testing.T) {
		m := newMX(t, config(1), false)
		c, err := netsmtp.Dial(m.addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		if err := c.Hello("client.example.test"); err != nil {
			t.Fatal(err)
		}
		if ok, _ := c.Extension("STARTTLS"); ok {
			t.Error("EHLO offers STARTTLS with no TLS configuration")
		}
		if err := c.StartTLS(&tls.Config{InsecureSkipVerify: true}); err == nil { // #nosec G402 -- asserting refusal
			t.Error("STARTTLS accepted with no TLS configuration")
		}
	})
}

// TestLiveReceiveLimits: the receiver enforces its size and recipient bounds
// with the reply codes a real client acts on.
func TestLiveReceiveLimits(t *testing.T) {
	livemail.Require(t)
	cfg := config(1)
	cfg.MaxMessageBytes, cfg.MaxRecipients = 3000, 2
	m := newMX(t, cfg, true)
	from := "jane@example.org"
	ok := mail("<live-limit-ok@example.org>", from, "", "fits")
	if err := liveSend(t, m.addr, liveTx{from: from, rcpts: []string{"support@example.com"}}, ok); err != nil {
		t.Fatalf("a message within the limit: %v", err)
	}

	big := mail("<live-limit-big@example.org>", from, "", strings.Repeat("0123456789abcdef\n", 400))
	err := liveSend(t, m.addr, liveTx{from: from, rcpts: []string{"support@example.com"}}, big)
	if liveCode(err) != 552 {
		t.Errorf("an oversize message = %v, want 552", err)
	}
	err = liveSend(t, m.addr, liveTx{from: from, rcpts: []string{"support@example.com"}, size: 100_000}, big)
	if liveCode(err) != 552 {
		t.Errorf("a declared oversize message = %v, want 552 at MAIL FROM", err)
	}
	err = liveSend(t, m.addr, liveTx{from: from, rcpts: []string{"support@example.com", "sales@example.com", "support@example.com"}}, ok)
	if c := liveCode(err); c < 400 || c >= 500 {
		t.Errorf("three recipients with a limit of two = %v, want a 4xx so the sender retries the rest", err)
	}
	t.Logf("oversize: %v; declared: %v; recipients: %v", liveMsg(liveSendErr(t, m.addr, from, big, 0)), liveMsg(liveSendErr(t, m.addr, from, big, 100_000)), liveMsg(err))
	if n := len(m.sink.accepted()); n != 1 {
		t.Errorf("sink calls = %d, want only the message within the limits", n)
	}
}

func liveSendErr(t *testing.T, addr, from, raw string, size int) error {
	t.Helper()
	return liveSend(t, addr, liveTx{from: from, rcpts: []string{"support@example.com"}, size: size}, raw)
}

// TestLiveTemporaryFailureIs4xx: when the sink cannot take a message the
// client gets a 4xx and keeps the message, and the retry of the same bytes
// carries the same event identity: one message, however many attempts.
func TestLiveTemporaryFailureIs4xx(t *testing.T) {
	livemail.Require(t)
	m := newMX(t, config(1), true)
	tx := liveTx{from: "jane@example.org", rcpts: []string{"support@example.com"}, startTLS: true}
	raw := mail("<live-retry@example.org>", tx.from, "", "needs a retry")

	m.sink.setErr(errors.New("the store is down"))
	err := liveSend(t, m.addr, tx, raw)
	if c := liveCode(err); c != 451 {
		t.Fatalf("send with the sink down = %v, want 451", err)
	}
	t.Logf("sink down: %d %s", liveCode(err), liveMsg(err))
	m.sink.setErr(nil)
	for range 2 { // the retry, then a duplicate of it
		if err := liveSend(t, m.addr, tx, raw); err != nil {
			t.Fatalf("retry: %v", err)
		}
	}
	want := liveEvent("support", raw)
	events := m.sink.events()
	if len(events) != 3 {
		t.Fatalf("sink attempts %v, want 3", events)
	}
	for _, e := range events {
		if e != want {
			t.Errorf("event %q, want %q: a retry of the same bytes must keep its identity", e, want)
		}
	}
}

// liveEngine is the real engine over a memory store with smtp.mail receiving
// on a loopback listener with STARTTLS, relaying (for sends) through the
// live submission server when the environment has one.
func liveEngine(t *testing.T, env livemail.Env, tweak func(*smtp.Config)) *engineRig {
	t.Helper()
	clock := portstest.NewFixedClock(now)
	r := &engineRig{t: t, ctx: t.Context(), addrs: map[model.ChannelRef]string{}, store: memory.New(memory.Options{Clock: clock})}
	f := smtp.NewFactory(smtp.Options{
		Listen: func(_ context.Context, ch model.Channel) (net.Listener, error) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err == nil {
				r.mu.Lock()
				r.addrs[ch.Ref] = ln.Addr().String()
				r.mu.Unlock()
			}
			return ln, err
		},
		TLS: serverTLS(t), Resolver: newZone(), RootCAs: env.Roots,
	})
	creds := &credHost{cred: ports.Password{User: env.SMTPUser, Secret: []byte(env.SMTPPass)}}
	e, err := uma.New(uma.Config{
		Store: r.store,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters: []adapter.Factory{f}, Credentials: creds, Clock: clock, IDs: portstest.NewSequenceIDs(0xc0),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	r.engine = e
	if r.client, err = e.For(model.Principal{Tenant: tenant, Actor: "operator"}); err != nil {
		t.Fatal(err)
	}
	cfg := liveConfig(env)
	if tweak != nil {
		tweak(&cfg)
	}
	lock := false
	sel := model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1}
	if err := r.store.Transact(r.ctx, tenant, func(tx store.Tx) error {
		if err := tx.PutChannel(r.ctx, channel(channelA, cfg), ""); err != nil {
			return err
		}
		return tx.PutInbox(r.ctx, model.Inbox{Ref: inboxA, Channel: channelA, Name: "mail", Destinations: []string{"support", "sales"}, Selection: sel, Revision: "in1"}, "")
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

// liveConfig is config with the relay set to the live submission server
// (an unreachable one when the environment has none: the receiver tests
// never send).
func liveConfig(env livemail.Env) smtp.Config {
	cfg := config(1)
	if !env.HasSubmission() {
		return cfg
	}
	cfg.Relay = smtp.Relay{Host: env.SMTPHost, CommandTimeoutMS: 20_000, DataTimeoutMS: 20_000}
	if env.SMTPStartTLSPort != 0 {
		cfg.Relay.StartTLSPort = env.SMTPStartTLSPort
	} else {
		cfg.Relay.PlainPort, cfg.Relay.AllowPlaintextLoopback = env.SMTPPlainPort, true
	}
	return cfg
}

// awaitInbox waits until the inbox holds n messages.
func awaitInbox(t *testing.T, r *engineRig, n int) []model.Message {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if msgs := r.messages(inboxA); len(msgs) >= n {
			return msgs
		}
		if time.Now().After(deadline) {
			t.Fatalf("inbox holds %d messages, want %d", len(r.messages(inboxA)), n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func inboxTexts(msgs []model.Message) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, strings.TrimSpace(m.Content.Parts[0].Text))
	}
	slices.Sort(out)
	return out
}

// TestLiveFanOutAndDedup: one DATA to two destinations lands once in each; a
// recipient the channel does not serve is refused at RCPT while the others
// are taken; the same bytes sent again (an MTA retry) land nowhere; a mail
// that forges the Message-ID of another is a second message, not a duplicate.
func TestLiveFanOutAndDedup(t *testing.T) {
	env := livemail.Require(t)
	r := liveEngine(t, env, nil)
	m := r.receive(channelA)
	from := "jane@example.org"
	tx := liveTx{from: from, rcpts: []string{"support@example.com", "sales@example.com"}, startTLS: true}

	fan := mail("<live-fan@example.org>", from, "", "to two destinations")
	for range 2 { // the second is the MTA's retry
		if err := liveSend(t, m.addr, tx, fan); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(r.messages(inboxA)); n != 2 {
		t.Fatalf("messages = %d, want 2: one per destination, none for the retry", n)
	}

	stranger := liveTx{from: from, rcpts: []string{"nobody@example.com"}, startTLS: true}
	err := liveSend(t, m.addr, stranger, fan)
	if liveCode(err) != 550 || !strings.Contains(liveMsg(err), "5.1.1") {
		t.Errorf("unknown recipient = %v, want 550 5.1.1", err)
	}

	const id = "<live-claimed@example.org>"
	forged := mail(id, "attacker@example.net", "", "forged")
	genuine := mail(id, "Jane <jane@example.org>", "", "the real question")
	one := liveTx{from: from, rcpts: []string{"support@example.com"}, startTLS: true}
	for _, s := range []struct {
		tx  liveTx
		raw string
	}{{liveTx{from: "attacker@example.net", rcpts: one.rcpts, startTLS: true}, forged}, {one, genuine}, {one, genuine}, {one, forged}} {
		if err := liveSend(t, m.addr, s.tx, s.raw); err != nil {
			t.Fatal(err)
		}
	}
	msgs := r.messages(inboxA)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4: the fan-out pair, the forged mail and the genuine one", len(msgs))
	}
	got := inboxTexts(msgs)
	for _, want := range []string{"forged", "the real question", "to two destinations"} {
		if !slices.Contains(got, want) {
			t.Errorf("stored texts %q lack %q", got, want)
		}
	}
}

// --- the sending side --------------------------------------------------------------

func liveOpen(t *testing.T, env livemail.Env, creds ports.Credentials) *smtp.Instance {
	t.Helper()
	f := smtp.NewFactory(smtp.Options{RootCAs: env.Roots, Resolver: newZone()})
	return open(t, f, channel(channelA, liveConfig(env)), creds)
}

// TestLiveSendThroughSubmission: a reply is submitted to a real submission
// server over STARTTLS and AUTH, under the operation's Message-ID, threaded
// on its target, and a retry of the operation reuses the Message-ID.
func TestLiveSendThroughSubmission(t *testing.T) {
	env := livemail.Require(t, livemail.Submission)
	inst := liveOpen(t, env, &credHost{cred: ports.Password{User: env.SMTPUser, Secret: []byte(env.SMTPPass)}})
	op := model.OperationRef{Tenant: tenant, ID: model.ID("live-op-" + livemail.Token(t))}
	token := string(op.ID)
	req := textRequest(op, "Thanks, on it. "+token)
	parent := model.ProviderMessageRef(channelA, "<live-m2@example.org>")
	req.Target = &parent

	out, err := inst.Send(t.Context(), req)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	caught := env.AwaitCaptured(t, token, 1)
	if env.SMTPStartTLSPort != 0 && caught[0].Username != env.SMTPUser {
		t.Errorf("submission authenticated as %q, want %q", caught[0].Username, env.SMTPUser)
	}
	m, err := mime.Parse(bytes.NewReader(caught[0].Raw), mime.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	h := m.Header
	if out.State != model.ProviderAccepted || len(out.References) != 1 || out.References[0] != model.ProviderMessageRef(channelA, h.MessageID) {
		t.Fatalf("outcome %+v, want provider_accepted referencing Message-ID %q", out, h.MessageID)
	}
	if !strings.HasSuffix(h.MessageID, "@example.com>") || h.InReplyTo != "<live-m2@example.org>" || h.References != "<live-m2@example.org>" ||
		h.From[0].Address != "support@example.com" || h.To[0].Address != "jane@example.org" || !strings.Contains(m.Text, token) {
		t.Errorf("submitted header %+v text %q", h, m.Text)
	}
	again, err := inst.Send(t.Context(), req)
	if err != nil || again.References[0] != out.References[0] {
		t.Fatalf("second attempt %+v, %v: the Message-ID must be stable per operation", again, err)
	}
}

// TestLiveSendRefusesWrongPassword: the submission server refuses a wrong
// password and nothing is submitted.
func TestLiveSendRefusesWrongPassword(t *testing.T) {
	env := livemail.Require(t, livemail.Submission)
	if env.SMTPStartTLSPort == 0 {
		t.Skip("the catcher has no AUTH over TLS (UMA_LIVE_SMTP_STARTTLS_PORT unset)")
	}
	inst := liveOpen(t, env, &credHost{cred: ports.Password{User: env.SMTPUser, Secret: []byte("wrong-" + env.SMTPPass)}})
	op := model.OperationRef{Tenant: tenant, ID: model.ID("live-bad-" + livemail.Token(t))}
	out, err := inst.Send(t.Context(), textRequest(op, "never sent"))
	if err == nil && out.State == model.ProviderAccepted {
		t.Fatalf("Send = %+v, want a refusal", out)
	}
	t.Logf("wrong password: outcome %+v, error %v", out, err)
	if n := env.CountCaptured(t, string(op.ID)); n != 0 {
		t.Errorf("catcher holds %d copies of a refused submission", n)
	}
}

// --- a real MTA --------------------------------------------------------------------

// mtaDelivery waits for a line the MTA logged about a delivery to the
// receiver at port (it logs the relay as host[address]:port) and returns it.
func mtaDelivery(env livemail.Env, t *testing.T, port int, within time.Duration, substrings ...string) string {
	t.Helper()
	return env.AwaitMTALog(t, within, append([]string{"relay=" + env.MTARelayHost + "[", "]:" + strconv.Itoa(port) + ","}, substrings...)...)
}

// TestLiveMTARetriesA4xx: Postfix relays to the receiver over STARTTLS. When
// the receiver answers 451 it defers the message and keeps it; when the
// receiver recovers it delivers the very same bytes, once per destination,
// one DATA for both. A recipient the receiver refuses with 550 is bounced,
// never retried.
func TestLiveMTARetriesA4xx(t *testing.T) {
	env := livemail.Require(t, livemail.MTA)
	m := newMX(t, config(1), true)
	_, portStr, _ := net.SplitHostPort(m.addr)
	port, _ := strconv.Atoi(portStr)
	env.RelayMTATo(t, port)

	token := livemail.Token(t)
	from := "jane@example.org"
	raw := mail("<live-mta-"+token+"@example.org>", from, "", "through a real MTA "+token)
	m.sink.setErr(errors.New("the store is down"))
	env.SubmitToMTA(t, from, []string{"support@example.com", "sales@example.com"}, []byte(crlf(raw)))
	line := mtaDelivery(env, t, port, 60*time.Second, "status=deferred", "451")
	t.Logf("MTA: %s", line)
	if n := len(m.sink.accepted()); n < 1 {
		t.Fatalf("sink saw %d attempts before the deferral", n)
	}

	m.sink.setErr(nil)
	env.FlushMTA(t)
	waitFor(t, func() bool {
		seen := map[string]bool{}
		for _, in := range m.sink.accepted() {
			if in.Envelope.MessageID == "<live-mta-"+token+"@example.org>" && in.Envelope.EventID != "" {
				seen[in.Envelope.Destination] = true
			}
		}
		return seen["support"] && seen["sales"]
	})
	line = mtaDelivery(env, t, port, 30*time.Second, "status=sent", "250")
	t.Logf("MTA: %s", line)
	log := env.MTALog(t)
	if !regexp.MustCompile(`TLS connection established to ` + regexp.QuoteMeta(env.MTARelayHost) + `\[[^\]]*\]:` + strconv.Itoa(port) + `: TLSv`).MatchString(log) {
		t.Errorf("the MTA did not negotiate STARTTLS with the receiver:\n%s", log)
	}

	// Every attempt carried the same bytes as far as the event identity can
	// tell: one identity per destination across the failed and the good one.
	byDest := map[string]map[string]bool{}
	for _, in := range m.sink.accepted() {
		if in.Envelope.MessageID != "<live-mta-"+token+"@example.org>" {
			continue
		}
		d := in.Envelope.Destination
		if byDest[d] == nil {
			byDest[d] = map[string]bool{}
		}
		byDest[d][in.Envelope.EventID] = true
	}
	for d, ids := range byDest {
		if len(ids) != 1 {
			t.Errorf("destination %s saw event identities %v across the MTA's attempts, want one", d, ids)
		}
	}

	// A permanent refusal is a bounce, not a retry.
	gone := mail("<live-mta-gone-"+token+"@example.org>", from, "", "nobody here "+token)
	env.SubmitToMTA(t, from, []string{"nobody@example.com"}, []byte(crlf(gone)))
	line = mtaDelivery(env, t, port, 60*time.Second, "to=<nobody@example.com>", "status=bounced", "550")
	t.Logf("MTA: %s", line)
}

// TestLiveMTADelivery: through the engine, mail from Postfix lands once per
// destination with its Message-ID and the sender evidence the receiver
// records; two submissions that share a Message-ID are two messages.
func TestLiveMTADelivery(t *testing.T) {
	env := livemail.Require(t, livemail.MTA)
	r := liveEngine(t, env, nil)
	m := r.receive(channelA)
	_, portStr, _ := net.SplitHostPort(m.addr)
	port, _ := strconv.Atoi(portStr)
	env.RelayMTATo(t, port)

	token := livemail.Token(t)
	from := "jane@example.org"
	id := "<live-mtad-" + token + "@example.org>"
	rcpts := []string{"support@example.com", "sales@example.com"}
	env.SubmitToMTA(t, from, rcpts, []byte(crlf(mail(id, from, "", "the genuine mail "+token))))
	env.SubmitToMTA(t, from, rcpts, []byte(crlf(mail(id, from, "", "a mail reusing the Message-ID "+token))))
	awaitInbox(t, r, 4)
	time.Sleep(5 * time.Second)
	msgs := r.messages(inboxA)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4: two mails with one Message-ID, two destinations each", len(msgs))
	}
	for _, msg := range msgs {
		if byProviderID([]model.Message{msg}, channelA, id) == nil {
			t.Errorf("message %+v lacks the provider Message-ID", msg)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
