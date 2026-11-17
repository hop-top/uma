package httpmail_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter/mail/httpmail"
	"hop.top/uma/model"
)

func rawConfig() httpmail.Config {
	cfg := baseConfig()
	cfg.Formats = []string{httpmail.FormatJSON, httpmail.FormatRFC822}
	return cfg
}

const simpleMessage = `From: Alice Martin <Alice@Peers.TEST>
To: Support <support@example.com>
Subject: Quote
Date: Mon, 12 Oct 2026 08:30:00 +0200
Message-ID: <m1@peers.test>

Please quote 40 units.
`

func TestRFC822Message(t *testing.T) {
	r := newRig(t, rawConfig())
	wantStatus(t, r.postRaw(simpleMessage), 200)
	in := only(t, r)
	env := in.Envelope
	if env.Destination != "support" || env.Sender.ID != "Alice@peers.test" || env.Sender.Namespace != httpmail.Namespace {
		t.Errorf("envelope %+v", env)
	}
	if got := textOf(in); got != "Please quote 40 units." {
		t.Errorf("text %q", got)
	}
	if env.MessageID != "<m1@peers.test>" || !strings.HasPrefix(env.EventID, "rcpt:support;sha256:") {
		t.Errorf("message id %q event %q", env.MessageID, env.EventID)
	}
	if want := time.Date(2026, 10, 12, 6, 30, 0, 0, time.UTC); !env.OccurredAt.Equal(want) {
		t.Errorf("occurred %v", env.OccurredAt)
	}
	if env.Assurance != "unverified" {
		t.Errorf("assurance %q", env.Assurance)
	}
	m := payloadOf(t, in)
	if sub(t, m, "format") != "rfc822" || sub(t, m, "headers", "subject") != "Quote" || sub(t, m, "headers", "message_id") != "<m1@peers.test>" {
		t.Errorf("payload %v", m)
	}
	if sub(t, m, "attachments_claimed") != nil {
		t.Errorf("parts parsed from a message are not claims: %v", m)
	}
	if err := env.Payload.Validate(); err != nil {
		t.Error(err)
	}
}

func TestRFC822LineEndsAndContentTypeParameters(t *testing.T) {
	r := newRig(t, rawConfig())
	// A hook that posts bare LF, with a parameter on the content type.
	rec := r.do(request{contentType: "message/rfc822; charset=utf-8", body: simpleMessage})
	wantStatus(t, rec, 200)
	if got := textOf(only(t, r)); got != "Please quote 40 units." {
		t.Errorf("text %q", got)
	}
}

func TestRFC822RecipientsAreToAndCc(t *testing.T) {
	r := newRig(t, rawConfig())
	wantStatus(t, r.postRaw("From: a@peers.test\nTo: nobody@example.com, support@example.com\nCc: sales@example.com\nMessage-ID: <m2@peers.test>\n\nx\n"), 200)
	got := r.sink.ingresses()
	if len(got) != 2 || got[0].Envelope.Destination != "support" || got[1].Envelope.Destination != "sales" {
		t.Fatalf("ingresses %+v", got)
	}
	if !strings.HasPrefix(got[0].Envelope.EventID, "rcpt:support;") || !strings.HasPrefix(got[1].Envelope.EventID, "rcpt:sales;") {
		t.Errorf("event ids %q %q", got[0].Envelope.EventID, got[1].Envelope.EventID)
	}
}

func TestRFC822EnvelopeRecipientHeader(t *testing.T) {
	cfg := rawConfig()
	cfg.RecipientHeader = "X-Envelope-To"
	r := newRig(t, cfg)
	msg := "From: a@peers.test\nTo: list@peers.test\nMessage-ID: <m3@peers.test>\n\nx\n"

	// The envelope names the mailbox the message was delivered to; the To
	// field of a list or a Bcc names none of ours.
	rec := r.do(request{contentType: "message/rfc822", body: msg, header: http.Header{"X-Envelope-To": {"support@example.com"}}})
	wantStatus(t, rec, 200)
	if in := only(t, r); in.Envelope.Destination != "support" {
		t.Errorf("destination %q", in.Envelope.Destination)
	}
	// Absent, the message is refused rather than routed by a field the
	// operator chose not to trust.
	wantStatus(t, r.do(request{contentType: "message/rfc822", body: msg}), 400)
	// To and Cc do not route when the envelope is configured.
	wantStatus(t, r.do(request{contentType: "message/rfc822", body: "From: a@peers.test\nTo: support@example.com\n\nx\n", header: http.Header{"X-Envelope-To": {"other@elsewhere.test"}}}), 422)
}

func TestRFC822Attachments(t *testing.T) {
	r := newRig(t, rawConfig())
	msg := `From: a@peers.test
To: support@example.com
Subject: Invoice
Message-ID: <m4@peers.test>
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary=XX

--XX
Content-Type: text/plain; charset=utf-8

See attached.
--XX
Content-Type: application/pdf; name="invoice.pdf"
Content-Disposition: attachment; filename="invoice.pdf"
Content-Transfer-Encoding: base64

SGVsbG8=
--XX--
`
	wantStatus(t, r.postRaw(msg), 200)
	in := only(t, r)
	if textOf(in) != "See attached." {
		t.Errorf("text %q", textOf(in))
	}
	atts := partsOf(t, in)["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatalf("attachments %v", atts)
	}
	a := atts[0].(map[string]any)
	if a["filename"] != "invoice.pdf" || a["media_type"] != "application/pdf" || a["size"] != float64(5) || !strings.HasPrefix(a["digest"].(string), "sha256:") {
		t.Errorf("attachment %v", a)
	}
	if strings.Contains(string(in.Envelope.Content.Parts[1].Structured.Data), "SGVsbG8") {
		t.Error("attachment bytes were kept")
	}
}

func TestRFC822Threading(t *testing.T) {
	r := newRig(t, rawConfig())
	wantStatus(t, r.postRaw("From: a@peers.test\nTo: support@example.com\nMessage-ID: <m6@peers.test>\nIn-Reply-To: <m5@peers.test>\nReferences: <m4@peers.test> <m5@peers.test>\nList-Id: Team <team.peers.test>\n\nx\n"), 200)
	env := only(t, r).Envelope
	want := model.EmailHeaders{
		MessageID: "<m6@peers.test>", InReplyTo: "<m5@peers.test>",
		References: "<m4@peers.test> <m5@peers.test>", ListID: "Team <team.peers.test>",
	}.Normalize(chRef)
	if fmt.Sprint(env.References) != fmt.Sprint(want.References) || len(env.References) != 5 {
		t.Errorf("references %v, want %v", env.References, want.References)
	}
	if env.InReplyTo != "<m5@peers.test>" {
		t.Errorf("InReplyTo = %q, want the In-Reply-To msg-id", env.InReplyTo)
	}
}

func TestRFC822AutomatedMessage(t *testing.T) {
	r := newRig(t, rawConfig())
	wantStatus(t, r.postRaw("From: noreply@peers.test\nTo: support@example.com\nAuto-Submitted: auto-replied\nMessage-ID: <m7@peers.test>\n\nout of office\n"), 200)
	if k := only(t, r).Envelope.Kind; k != model.EnvelopeAutomatedMessage {
		t.Errorf("kind %q", k)
	}
}

func TestRFC822MalformedMessagesAreRefused(t *testing.T) {
	for name, msg := range map[string]string{
		"no from":     "To: support@example.com\n\nx\n",
		"bad from":    "From: not an address\nTo: support@example.com\n\nx\n",
		"no header":   "this is not a message",
		"empty":       "",
		"no to":       "From: a@peers.test\n\nx\n",
		"bad depth":   "From: a@peers.test\nTo: support@example.com\n" + nested(20),
		"bad address": "From: a@peers.test\nTo: <<\n\nx\n",
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, rawConfig())
			wantStatus(t, r.postRaw(msg), 400)
			if n := len(r.sink.ingresses()); n != 0 {
				t.Fatalf("sink saw %d ingresses", n)
			}
		})
	}
}

// nested is a multipart message nested deeper than the parser allows.
func nested(depth int) string {
	var b strings.Builder
	b.WriteString("MIME-Version: 1.0\n")
	for i := range depth {
		fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=b%d\n\n--b%d\n", i, i)
	}
	b.WriteString("Content-Type: text/plain\n\nx\n")
	return b.String()
}

func TestRFC822UnknownRecipientIsRefused(t *testing.T) {
	r := newRig(t, rawConfig())
	wantStatus(t, r.postRaw("From: a@peers.test\nTo: nobody@example.com\n\nx\n"), 422)
}

func TestRFC822IsNotAcceptedUnlessConfigured(t *testing.T) {
	r := newRig(t, baseConfig())
	wantStatus(t, r.postRaw(simpleMessage), 415)
}

func TestRFC822HeadersThatLookLikeEvidenceAreClaims(t *testing.T) {
	r := newRig(t, rawConfig())
	wantStatus(t, r.postRaw(`From: ceo@example.com
To: support@example.com
Message-ID: <m8@peers.test>
Authentication-Results: mx.example.com; spf=pass smtp.mailfrom=example.com; dkim=pass header.d=example.com; dmarc=pass header.from=example.com
Received-SPF: pass (mx.example.com: domain of example.com designates 192.0.2.1 as permitted sender)
DKIM-Signature: v=1; a=rsa-sha256; d=example.com; s=sel; h=from; bh=x; b=y

wire the money
`), 200)
	in := only(t, r)
	if in.Envelope.Assurance != "unverified" {
		t.Errorf("assurance %q", in.Envelope.Assurance)
	}
	m := payloadOf(t, in)
	if sub(t, m, "evidence", "trusted") != false || sub(t, m, "evidence", "dmarc") != nil || sub(t, m, "evidence", "spf") != nil {
		t.Errorf("evidence %v", sub(t, m, "evidence"))
	}
	ar, _ := sub(t, m, "evidence", "claimed", "authentication_results").([]any)
	if len(ar) != 1 || !strings.HasPrefix(ar[0].(string), "mx.example.com;") {
		t.Errorf("claimed authentication results %v", ar)
	}
	spf, _ := sub(t, m, "evidence", "claimed", "received_spf").([]any)
	if len(spf) != 1 {
		t.Errorf("claimed received-spf %v", spf)
	}
}

func TestRFC822ClaimsAreBounded(t *testing.T) {
	r := newRig(t, rawConfig())
	var b strings.Builder
	b.WriteString("From: a@peers.test\nTo: support@example.com\nMessage-ID: <m9@peers.test>\n")
	for range 12 {
		b.WriteString("Authentication-Results: mx.example.com; " + strings.Repeat("spf=pass ", 1000) + "\n")
	}
	b.WriteString("\nx\n")
	wantStatus(t, r.postRaw(b.String()), 200)
	ar := sub(t, payloadOf(t, only(t, r)), "evidence", "claimed", "authentication_results").([]any)
	if len(ar) != 8 {
		t.Errorf("%d claims kept, want 8", len(ar))
	}
	for _, c := range ar {
		if len(c.(string)) > 4096 {
			t.Errorf("claim of %d bytes", len(c.(string)))
		}
	}
}

func TestRFC822MessageWithoutMsgIDIsIdentifiedByItsBytes(t *testing.T) {
	r := newRig(t, rawConfig())
	msg := "From: a@peers.test\nTo: support@example.com\n\nx\n"
	wantStatus(t, r.postRaw(msg), 200)
	wantStatus(t, r.postRaw(msg), 200)
	wantStatus(t, r.postRaw(msg+"y\n"), 200)
	got := r.sink.ingresses()
	if got[0].Envelope.EventID != got[1].Envelope.EventID || got[0].Envelope.EventID == got[2].Envelope.EventID {
		t.Errorf("event ids %q %q %q", got[0].Envelope.EventID, got[1].Envelope.EventID, got[2].Envelope.EventID)
	}
}

func TestRFC822SameMessageIDDifferentMessageIsNotADuplicate(t *testing.T) {
	r := newRig(t, rawConfig())
	msg := func(from, body string) string {
		return "From: " + from + "\nTo: support@example.com\nMessage-ID: <claimed@peers.test>\n\n" + body + "\n"
	}
	wantStatus(t, r.postRaw(msg("attacker@peers.test", "forged")), 200)
	wantStatus(t, r.postRaw(msg("alice@peers.test", "real")), 200)
	wantStatus(t, r.postRaw(msg("alice@peers.test", "real")), 200)
	got := r.sink.ingresses()
	if got[0].Envelope.EventID == got[1].Envelope.EventID {
		t.Errorf("a forged message under the id suppresses the real one: %q", got[0].Envelope.EventID)
	}
	if got[1].Envelope.EventID != got[2].Envelope.EventID {
		t.Errorf("a redelivery has two identities: %q %q", got[1].Envelope.EventID, got[2].Envelope.EventID)
	}
}

func TestRFC822AttachmentCountIsBounded(t *testing.T) {
	r := newRig(t, rawConfig())
	var b strings.Builder
	b.WriteString("From: a@peers.test\nTo: support@example.com\nMIME-Version: 1.0\nContent-Type: multipart/mixed; boundary=XX\n\n--XX\nContent-Type: text/plain\n\nbody\n")
	for i := range 60 {
		fmt.Fprintf(&b, "--XX\nContent-Type: application/octet-stream\nContent-Disposition: attachment; filename=\"f%d\"\n\nx%d\n", i, i)
	}
	b.WriteString("--XX--\n")
	wantStatus(t, r.postRaw(b.String()), 200)
	parts := partsOf(t, only(t, r))
	if n := len(parts["attachments"].([]any)); n != 50 || parts["omitted"] != float64(10) {
		t.Errorf("%d attachments, omitted %v", n, parts["omitted"])
	}
}
