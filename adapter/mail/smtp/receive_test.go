package smtp_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	gosmtp "github.com/emersion/go-smtp"

	"hop.top/uma/adapter/mail/smtp"
	"hop.top/uma/model"
)

// sink records Accept calls and answers with err.
type sink struct {
	mu  sync.Mutex
	got []model.Ingress
	err error
}

func (s *sink) Accept(_ context.Context, in model.Ingress) (model.IngressReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, in)
	return model.IngressReceipt{Committed: s.err == nil}, s.err
}

func (s *sink) EvaluateInteraction(context.Context, model.InteractionInput) (model.InteractionDecision, error) {
	return model.InteractionDecision{}, model.NewError(model.Unsupported, "no interactions")
}

func (s *sink) accepted() []model.Ingress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Ingress(nil), s.got...)
}

// mx runs one receiver on a fresh listener and returns its address.
type mx struct {
	addr string
	sink *sink
	zone *zone
}

func newMX(t *testing.T, cfg smtp.Config, tlsOn bool) *mx {
	t.Helper()
	ln := listen(t)
	m := &mx{addr: ln.Addr().String(), sink: &sink{}, zone: newZone()}
	opts := smtp.Options{
		Listen:   func(context.Context, model.Channel) (net.Listener, error) { return ln, nil },
		Resolver: m.zone,
	}
	if tlsOn {
		opts.TLS = serverTLS(t)
	}
	inst := open(t, smtp.NewFactory(opts), channel(channelA, cfg), passwordHost())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- inst.Run(ctx, m.sink) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v, want context.Canceled", err)
		}
	})
	return m
}

// mta is the sending side: one SMTP transaction to the receiver.
type mta struct {
	helo, from string
	rcpts      []string
	startTLS   bool
}

func (m *mx) send(t *testing.T, s mta, raw string) error {
	t.Helper()
	conn, err := net.Dial("tcp", m.addr)
	if err != nil {
		t.Fatal(err)
	}
	var c *gosmtp.Client
	if s.startTLS {
		c, err = gosmtp.NewClientStartTLS(conn, &tls.Config{ServerName: "localhost", RootCAs: certs(t).roots, MinVersion: tls.VersionTLS12})
		if err != nil {
			t.Fatal(err)
		}
	} else {
		c = gosmtp.NewClient(conn)
	}
	defer func() { _ = c.Close() }()
	helo := s.helo
	if helo == "" {
		helo = "mail.example.org"
	}
	if err := c.Hello(helo); err != nil {
		return err
	}
	if err := c.Mail(s.from, nil); err != nil {
		return err
	}
	for _, r := range s.rcpts {
		if err := c.Rcpt(r, nil); err != nil {
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

func replyCode(err error) int {
	var se *gosmtp.SMTPError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

const plainMail = `From: Jane Doe <jane@Example.ORG>
To: support@example.com
Subject: Order 42
Date: Sat, 10 Oct 2026 08:00:00 +0000
Message-ID: <m2@example.org>
In-Reply-To: <m1@example.com>
References: <m0@example.com> <m1@example.com>
Authentication-Results: mx.example.com; dmarc=pass header.from=example.org
Content-Type: text/plain; charset=utf-8

Where is my order?
`

var janeTx = mta{from: "jane@example.org", rcpts: []string{"support@example.com"}}

func TestReceiveNormalizes(t *testing.T) {
	m := newMX(t, config(1), true)
	tx := janeTx
	tx.startTLS = true
	if err := m.send(t, tx, plainMail); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := m.sink.accepted()
	if len(got) != 1 {
		t.Fatalf("sink calls = %d, want 1", len(got))
	}
	env := got[0].Envelope
	if env.Destination != "support" || env.Kind != model.EnvelopeMessage || env.MessageID != "<m2@example.org>" {
		t.Errorf("envelope destination %q kind %q message %q", env.Destination, env.Kind, env.MessageID)
	}
	want := model.ExternalRef{Channel: channelA, Namespace: smtp.Namespace, Kind: smtp.RouteKind, ID: "jane@example.org"}
	if env.Sender != want {
		t.Errorf("sender = %+v, want %+v", env.Sender, want)
	}
	norm := model.EmailHeaders{MessageID: "<m2@example.org>", InReplyTo: "<m1@example.com>", References: "<m0@example.com> <m1@example.com>"}.Normalize(channelA)
	if !equalRefs(env.References, norm.References) {
		t.Errorf("references = %+v, want %+v", env.References, norm.References)
	}
	if env.InReplyTo != "<m1@example.com>" {
		t.Errorf("InReplyTo = %q, want the In-Reply-To msg-id", env.InReplyTo)
	}
	if len(env.Content.Parts) != 1 || env.Content.Parts[0].Text != "Where is my order?" || env.Audience.Visibility != model.Public {
		t.Errorf("content %+v audience %+v", env.Content, env.Audience)
	}
	if !env.OccurredAt.Equal(time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("occurred at %v", env.OccurredAt)
	}
	rec := record(t, env)
	if rec.Headers.Subject != "Order 42" || rec.MailFrom != "jane@example.org" || !rec.TLS || rec.Helo != "mail.example.org" || rec.ClientIP != "127.0.0.1" {
		t.Errorf("record = %+v", rec)
	}
	if len(rec.Recipients) != 1 || rec.Recipients[0] != "support@example.com" {
		t.Errorf("recipients = %v", rec.Recipients)
	}
	// The message's own Authentication-Results claim is not evidence.
	if env.Assurance != smtp.AssuranceDMARCNone || !strings.HasPrefix(rec.AuthenticationResults, "mx.example.com; spf=none") {
		t.Errorf("assurance %q results %q, want the receiver's own evaluation", env.Assurance, rec.AuthenticationResults)
	}
}

func equalRefs(a, b []model.ExternalRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type recordView struct {
	MailFrom              string   `json:"mail_from"`
	Helo                  string   `json:"helo"`
	ClientIP              string   `json:"client_ip"`
	TLS                   bool     `json:"tls"`
	Recipients            []string `json:"recipients"`
	AuthenticationResults string   `json:"authentication_results"`
	Headers               struct {
		Subject string `json:"subject"`
	} `json:"headers"`
	Evidence struct {
		SPF struct {
			Result, Domain, Scope string
		} `json:"spf"`
		DKIM []struct {
			Result, Domain string
		} `json:"dkim"`
		DMARC struct {
			Result      string `json:"result"`
			Policy      string `json:"policy"`
			SPFAligned  bool   `json:"spf_aligned"`
			DKIMAligned bool   `json:"dkim_aligned"`
		} `json:"dmarc"`
		ARC struct {
			Verified bool `json:"verified"`
			Fields   []struct{ Key, Value string }
		} `json:"arc"`
	} `json:"evidence"`
}

func record(t *testing.T, env model.Envelope) recordView {
	t.Helper()
	if env.Payload.Name != smtp.MessageSchema || env.Payload.Version != smtp.MessageVersion {
		t.Fatalf("payload %s v%d, want %s v%d", env.Payload.Name, env.Payload.Version, smtp.MessageSchema, smtp.MessageVersion)
	}
	var r recordView
	if err := json.Unmarshal(env.Payload.Data, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReceiveRecipients(t *testing.T) {
	m := newMX(t, config(1), false)
	t.Run("unknown recipient refused at RCPT", func(t *testing.T) {
		tx := janeTx
		tx.rcpts = []string{"nobody@example.com"}
		if err := m.send(t, tx, plainMail); replyCode(err) != 550 {
			t.Fatalf("send = %v, want 550 at RCPT", err)
		}
		if n := len(m.sink.accepted()); n != 0 {
			t.Fatalf("sink calls = %d, want none", n)
		}
	})
	t.Run("domain case-insensitive, local part exact", func(t *testing.T) {
		tx := janeTx
		tx.rcpts = []string{"Support@example.com"}
		if err := m.send(t, tx, plainMail); replyCode(err) != 550 {
			t.Fatalf("send to another local part = %v, want 550", err)
		}
		tx.rcpts = []string{"support@EXAMPLE.com"}
		if err := m.send(t, tx, plainMail); err != nil {
			t.Fatalf("send with an upper-case domain = %v", err)
		}
	})
	t.Run("one ingress per destination", func(t *testing.T) {
		before := len(m.sink.accepted())
		tx := janeTx
		tx.rcpts = []string{"support@example.com", "sales@example.com", "support@example.com"}
		if err := m.send(t, tx, plainMail); err != nil {
			t.Fatal(err)
		}
		got := m.sink.accepted()[before:]
		if len(got) != 2 || got[0].Envelope.Destination != "support" || got[1].Envelope.Destination != "sales" {
			t.Fatalf("ingress = %+v, want support then sales", got)
		}
		if got[0].Envelope.EventID == got[1].Envelope.EventID {
			t.Error("destinations share an event ID; the second would deduplicate away")
		}
	})
}

// The event ID is the destination and a digest of the DATA bytes the client
// sent: nothing the receiver adds or the recipient list changes enters it,
// so an MTA retry of one message always reproduces it, per destination.
func TestReceiveEventIDIsPerDestinationAndBytes(t *testing.T) {
	m := newMX(t, config(1), false)
	both := janeTx
	both.rcpts = []string{"support@example.com", "sales@example.com"}
	swapped := janeTx
	swapped.rcpts = []string{"sales@example.com", "support@example.com"}
	for _, tx := range []mta{both, swapped, janeTx} {
		if err := m.send(t, tx, plainMail); err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string][]string{}
	for _, in := range m.sink.accepted() {
		ids[in.Envelope.Destination] = append(ids[in.Envelope.Destination], in.Envelope.EventID)
	}
	sum := sha256.Sum256([]byte(crlf(plainMail)))
	for dest, got := range ids {
		want := "rcpt:" + dest + ";sha256:" + hex.EncodeToString(sum[:])
		for _, id := range got {
			if id != want {
				t.Errorf("destination %s: event ID %q, want %q (digest of the DATA bytes sent)", dest, id, want)
			}
		}
	}
	if len(ids["support"]) != 3 || len(ids["sales"]) != 2 {
		t.Errorf("ingress per destination = %v, want support x3 and sales x2", ids)
	}
	other := strings.Replace(plainMail, "Where is my order?", "Where is my refund?", 1)
	if err := m.send(t, janeTx, other); err != nil {
		t.Fatal(err)
	}
	got := m.sink.accepted()
	if a, b := got[len(got)-2].Envelope, got[len(got)-1].Envelope; a.MessageID != b.MessageID || a.EventID == b.EventID {
		t.Errorf("same Message-ID, different bytes: message %q/%q event %q/%q, want one id and two events", a.MessageID, b.MessageID, a.EventID, b.EventID)
	}
}

func TestReceiveLimits(t *testing.T) {
	cfg := config(1)
	cfg.MaxMessageBytes = 512
	cfg.MaxRecipients = 1
	m := newMX(t, cfg, false)
	big := plainMail + strings.Repeat("x", 1024) + "\n"
	if err := m.send(t, janeTx, big); replyCode(err) != 552 {
		t.Fatalf("oversized send = %v, want 552", err)
	}
	tx := janeTx
	tx.rcpts = []string{"support@example.com", "sales@example.com"}
	if err := m.send(t, tx, plainMail); replyCode(err) != 452 {
		t.Fatalf("too many recipients = %v, want 452", err)
	}
	if n := len(m.sink.accepted()); n != 0 {
		t.Fatalf("sink calls = %d, want none", n)
	}
}

func TestReceiveSinkFailures(t *testing.T) {
	for name, c := range map[string]struct {
		err  error
		code int
	}{
		"unbound destination": {model.NewError(model.NotFound, "destination is not bound to an inbox"), 550},
		"disabled inbox":      {model.NewError(model.Disabled, "inbox disabled"), 550},
		"transient":           {model.NewError(model.Transient, "store busy"), 451},
		"selection rejected":  {model.NewError(model.Validation, "selector result outside candidates"), 451},
		"untyped":             {errors.New("boom"), 451},
	} {
		t.Run(name, func(t *testing.T) {
			m := newMX(t, config(1), false)
			m.sink.mu.Lock()
			m.sink.err = c.err
			m.sink.mu.Unlock()
			if err := m.send(t, janeTx, plainMail); replyCode(err) != c.code {
				t.Fatalf("send = %v, want %d", err, c.code)
			}
		})
	}
}

func TestReceiveKinds(t *testing.T) {
	m := newMX(t, config(1), false)
	auto := strings.Replace(plainMail, "Subject:", "Auto-Submitted: auto-replied\nSubject:", 1)
	if err := m.send(t, janeTx, auto); err != nil {
		t.Fatal(err)
	}
	bounce := janeTx
	bounce.from = ""
	if err := m.send(t, bounce, plainMail); err != nil {
		t.Fatal(err)
	}
	noID := strings.Replace(plainMail, "Message-ID: <m2@example.org>\n", "", 1)
	if err := m.send(t, janeTx, noID); err != nil {
		t.Fatal(err)
	}
	if err := m.send(t, janeTx, "To: support@example.com\n\nno sender\n"); replyCode(err) != 550 {
		t.Fatalf("message without From = %v, want 550", err)
	}
	got := m.sink.accepted()
	if len(got) != 3 {
		t.Fatalf("ingress = %d, want 3", len(got))
	}
	if got[0].Envelope.Kind != model.EnvelopeAutomatedMessage || got[1].Envelope.Kind != model.EnvelopeAutomatedMessage {
		t.Errorf("auto-submitted kind %q, null reverse path kind %q; want automated", got[0].Envelope.Kind, got[1].Envelope.Kind)
	}
	if e := got[2].Envelope; e.MessageID != "" || !strings.Contains(e.EventID, "sha256:") {
		t.Errorf("message without Message-ID: message %q event %q, want a content-derived event ID", e.MessageID, e.EventID)
	}
}

func TestReceiveMIMEParts(t *testing.T) {
	m := newMX(t, config(1), false)
	raw := `From: jane@example.org
To: support@example.com
Message-ID: <m3@example.org>
Content-Type: multipart/mixed; boundary=b

--b
Content-Type: application/pdf
Content-Disposition: attachment; filename=invoice.pdf

%PDF
--b--
`
	if err := m.send(t, janeTx, raw); err != nil {
		t.Fatal(err)
	}
	env := m.sink.accepted()[0].Envelope
	if len(env.Content.Parts) != 1 || env.Content.Parts[0].Kind != model.PartStructured || env.Content.Parts[0].Structured.Name != smtp.PartsSchema {
		t.Fatalf("content = %+v, want one %s part", env.Content, smtp.PartsSchema)
	}
	var parts struct {
		Attachments []struct {
			Filename string `json:"filename"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(env.Content.Parts[0].Structured.Data, &parts); err != nil || len(parts.Attachments) != 1 || parts.Attachments[0].Filename != "invoice.pdf" {
		t.Fatalf("parts = %s (%v)", env.Content.Parts[0].Structured.Data, err)
	}
	if err := m.send(t, janeTx, "From: jane@example.org\nContent-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: text/plain\n"); replyCode(err) != 550 {
		t.Fatalf("malformed MIME = %v, want 550", err)
	}
}

// --- sender evidence ---------------------------------------------------------

type dkimKey struct {
	priv ed25519.PrivateKey
	txt  string
}

func newDKIMKey(t *testing.T) dkimKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return dkimKey{priv: priv, txt: "v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(pub)}
}

func sign(t *testing.T, k dkimKey, domain, raw string) string {
	t.Helper()
	var out bytes.Buffer
	err := dkim.Sign(&out, strings.NewReader(crlf(raw)), &dkim.SignOptions{
		Domain: domain, Selector: "s1", Signer: k.priv,
		HeaderKeys: []string{"From", "To", "Subject", "Message-ID"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(out.String(), "\r\n", "\n")
}

const evidenceMail = `From: jane@example.org
To: support@example.com
Subject: Evidence
Message-ID: <e1@example.org>

Signed body.
`

func TestSenderEvidence(t *testing.T) {
	key := newDKIMKey(t)
	signed := sign(t, key, "example.org", evidenceMail)
	signedByOther := sign(t, key, "mailer.example.net", evidenceMail)
	for name, c := range map[string]struct {
		dns       func(z *zone)
		from      string
		raw       string
		assurance string
		spf, dkim string
		check     func(t *testing.T, r recordView)
	}{
		"aligned DKIM and SPF pass": {
			dns: func(z *zone) {
				z.setTXT("example.org", "v=spf1 ip4:127.0.0.1 -all")
				z.setTXT("s1._domainkey.example.org", key.txt)
				z.setTXT("_dmarc.example.org", "v=DMARC1; p=reject")
			},
			from: "jane@example.org", raw: signed, assurance: smtp.AssuranceDMARCPass, spf: "pass", dkim: "pass",
			check: func(t *testing.T, r recordView) {
				if !r.Evidence.DMARC.SPFAligned || !r.Evidence.DMARC.DKIMAligned || r.Evidence.DMARC.Policy != "reject" {
					t.Errorf("dmarc = %+v", r.Evidence.DMARC)
				}
				if !strings.Contains(r.AuthenticationResults, "dkim=pass") || !strings.Contains(r.AuthenticationResults, "dmarc=pass") {
					t.Errorf("Authentication-Results = %q", r.AuthenticationResults)
				}
			},
		},
		"DKIM body tampered fails": {
			dns: func(z *zone) {
				z.setTXT("s1._domainkey.example.org", key.txt)
				z.setTXT("_dmarc.example.org", "v=DMARC1; p=reject")
			},
			from: "jane@example.org", raw: strings.Replace(signed, "Signed body.", "Tampered body.", 1),
			assurance: smtp.AssuranceDMARCFail, spf: "none", dkim: "fail",
		},
		"unaligned DKIM pass is no DMARC pass": {
			dns: func(z *zone) {
				z.setTXT("s1._domainkey.mailer.example.net", key.txt)
				z.setTXT("_dmarc.example.org", "v=DMARC1; p=quarantine")
			},
			from: "jane@example.org", raw: signedByOther, assurance: smtp.AssuranceDMARCFail, spf: "none", dkim: "pass",
		},
		"SPF pass for an unaligned bounce domain": {
			dns: func(z *zone) {
				z.setTXT("bounces.example.net", "v=spf1 ip4:127.0.0.1 -all")
				z.setTXT("_dmarc.example.org", "v=DMARC1; p=none")
			},
			from: "bounce@bounces.example.net", raw: evidenceMail, assurance: smtp.AssuranceDMARCFail, spf: "pass",
		},
		"relaxed alignment accepts a subdomain": {
			dns: func(z *zone) {
				z.setTXT("mail.example.org", "v=spf1 ip4:127.0.0.1 -all")
				z.setTXT("_dmarc.example.org", "v=DMARC1; p=reject")
			},
			from: "bounce@mail.example.org", raw: evidenceMail, assurance: smtp.AssuranceDMARCPass, spf: "pass",
		},
		"strict alignment refuses a subdomain": {
			dns: func(z *zone) {
				z.setTXT("mail.example.org", "v=spf1 ip4:127.0.0.1 -all")
				z.setTXT("_dmarc.example.org", "v=DMARC1; p=reject; aspf=s")
			},
			from: "bounce@mail.example.org", raw: evidenceMail, assurance: smtp.AssuranceDMARCFail, spf: "pass",
		},
		"organizational record and subdomain policy": {
			dns: func(z *zone) {
				z.setTXT("_dmarc.example.org", "v=DMARC1; p=none; sp=reject")
			},
			from:      "jane@eu.example.org",
			raw:       strings.Replace(evidenceMail, "From: jane@example.org", "From: jane@eu.example.org", 1),
			assurance: smtp.AssuranceDMARCFail, spf: "none",
			check: func(t *testing.T, r recordView) {
				if r.Evidence.DMARC.Policy != "reject" {
					t.Errorf("policy = %q, want the subdomain policy", r.Evidence.DMARC.Policy)
				}
			},
		},
		"no DMARC record": {
			dns:  func(z *zone) { z.setTXT("example.org", "v=spf1 ip4:127.0.0.1 -all") },
			from: "jane@example.org", raw: evidenceMail, assurance: smtp.AssuranceDMARCNone, spf: "pass",
		},
		"DNS failure": {
			dns:  func(z *zone) { z.failTemp("_dmarc.example.org") },
			from: "jane@example.org", raw: evidenceMail, assurance: smtp.AssuranceDMARCTempError,
		},
		"ARC recorded, never verified": {
			dns:       func(z *zone) { z.setTXT("_dmarc.example.org", "v=DMARC1; p=reject") },
			from:      "jane@example.org",
			raw:       "ARC-Seal: i=1; a=rsa-sha256; cv=none; d=lists.example.net; s=arc; b=AAAA\nARC-Authentication-Results: i=1; lists.example.net; dmarc=pass\n" + evidenceMail,
			assurance: smtp.AssuranceDMARCFail, spf: "none",
			check: func(t *testing.T, r recordView) {
				if r.Evidence.ARC.Verified || len(r.Evidence.ARC.Fields) != 2 || !strings.EqualFold(r.Evidence.ARC.Fields[0].Key, "ARC-Seal") {
					t.Errorf("arc = %+v, want both fields, unverified", r.Evidence.ARC)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := newMX(t, config(1), false)
			c.dns(m.zone)
			tx := janeTx
			tx.from = c.from
			if err := m.send(t, tx, c.raw); err != nil {
				t.Fatalf("send: %v", err)
			}
			env := m.sink.accepted()[0].Envelope
			r := record(t, env)
			if env.Assurance != c.assurance {
				t.Errorf("assurance = %q, want %q (dmarc %+v)", env.Assurance, c.assurance, r.Evidence.DMARC)
			}
			if c.spf != "" && r.Evidence.SPF.Result != c.spf {
				t.Errorf("spf = %+v, want %s", r.Evidence.SPF, c.spf)
			}
			if c.dkim != "" && (len(r.Evidence.DKIM) != 1 || r.Evidence.DKIM[0].Result != c.dkim) {
				t.Errorf("dkim = %+v, want one %s", r.Evidence.DKIM, c.dkim)
			}
			if c.check != nil {
				c.check(t, r)
			}
		})
	}
}
