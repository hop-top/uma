package mime_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/model"
)

func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

func parse(t *testing.T, raw string, l mime.Limits) *mime.Message {
	t.Helper()
	m, err := mime.Parse(strings.NewReader(crlf(raw)), l)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return m
}

const multipartMail = `From: "Jane =?utf-8?q?D=C3=B6e?=" <jane@example.org>
To: Support <support@example.com>, other@example.com
Subject: =?iso-8859-1?q?Caf=E9?= order
Date: Tue, 10 Oct 2026 09:30:00 +0200
Message-ID: <m2@example.org>
In-Reply-To: <m1@example.com>
References: <m0@example.com> <m1@example.com>
List-Id: Orders <orders.example.org>
Auto-Submitted: no
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="outer"

--outer
Content-Type: multipart/alternative; boundary="alt"

--alt
Content-Type: text/plain; charset=iso-8859-1
Content-Transfer-Encoding: quoted-printable

Caf=E9 au lait, please.
--alt
Content-Type: text/html; charset=utf-8

<p>Café au lait, <b>please</b>.</p>
--alt--
--outer
Content-Type: application/pdf; name="invoice.pdf"
Content-Disposition: attachment; filename="invoice.pdf"
Content-Transfer-Encoding: base64

aGVsbG8=
--outer
Content-Type: image/png
Content-Disposition: inline
Content-ID: <logo@example.org>
Content-Transfer-Encoding: base64

iVBORw==
--outer--
`

func TestParseMultipart(t *testing.T) {
	m := parse(t, multipartMail, mime.Limits{})
	h := m.Header
	if len(h.From) != 1 || h.From[0] != (mime.Address{Name: "Jane Döe", Address: "jane@example.org"}) {
		t.Errorf("From = %+v", h.From)
	}
	if len(h.To) != 2 || h.To[0].Address != "support@example.com" || h.To[1].Address != "other@example.com" {
		t.Errorf("To = %+v", h.To)
	}
	if h.Subject != "Café order" {
		t.Errorf("Subject = %q", h.Subject)
	}
	if want := time.Date(2026, 10, 10, 7, 30, 0, 0, time.UTC); !h.Date.Equal(want) {
		t.Errorf("Date = %v, want %v", h.Date, want)
	}
	raw := model.EmailHeaders{MessageID: h.MessageID, InReplyTo: h.InReplyTo, References: h.References, ListID: h.ListID, AutoSubmitted: h.AutoSubmitted}
	if raw != (model.EmailHeaders{MessageID: "<m2@example.org>", InReplyTo: "<m1@example.com>", References: "<m0@example.com> <m1@example.com>", ListID: "Orders <orders.example.org>", AutoSubmitted: "no"}) {
		t.Errorf("threading headers = %+v", raw)
	}
	if m.Text != "Café au lait, please." || m.FromHTML || m.TextTruncated {
		t.Errorf("text = %q (html %v, truncated %v), want the decoded text/plain part", m.Text, m.FromHTML, m.TextTruncated)
	}
	sum := sha256.Sum256([]byte("hello"))
	want := []mime.Attachment{
		{Filename: "invoice.pdf", MediaType: "application/pdf", Size: 5, Digest: "sha256:" + hex.EncodeToString(sum[:])},
		{MediaType: "image/png", Size: 4, Digest: m.Attachments[1].Digest, ContentID: "logo@example.org", Inline: true},
	}
	if len(m.Attachments) != 2 || m.Attachments[0] != want[0] || m.Attachments[1] != want[1] {
		t.Errorf("attachments = %+v, want %+v", m.Attachments, want)
	}
	if got := h.Values("references"); len(got) != 1 {
		t.Errorf("Values(references) = %v", got)
	}
}

func TestParseHTMLOnly(t *testing.T) {
	m := parse(t, `From: a@example.org
Content-Type: text/html; charset=windows-1252

<html><head><style>p{}</style><script>x()</script></head><body><p>Line one</p><p>Line&nbsp;two<br>three</p></body></html>
`, mime.Limits{})
	if !m.FromHTML || m.Text != "Line one\n\nLine two\nthree" {
		t.Fatalf("text = %q (html %v)", m.Text, m.FromHTML)
	}
}

func TestParseTextAttachmentIsNotBody(t *testing.T) {
	m := parse(t, `From: a@example.org
Content-Type: multipart/mixed; boundary=b

--b
Content-Type: text/plain
Content-Disposition: attachment; filename=notes.txt

attached notes
--b--
`, mime.Limits{})
	if m.Text != "" || len(m.Attachments) != 1 || m.Attachments[0].Filename != "notes.txt" {
		t.Fatalf("text %q attachments %+v, want no body and one attachment", m.Text, m.Attachments)
	}
}

func TestParseTruncatesAtRuneBoundary(t *testing.T) {
	m := parse(t, "From: a@example.org\nContent-Type: text/plain; charset=utf-8\n\naéb\n", mime.Limits{MaxTextBytes: 2})
	if m.Text != "a" || !m.TextTruncated {
		t.Fatalf("text = %q truncated %v, want %q truncated", m.Text, m.TextTruncated, "a")
	}
}

func TestParseLimits(t *testing.T) {
	nested := "From: a@example.org\nContent-Type: multipart/mixed; boundary=b1\n\n--b1\nContent-Type: multipart/mixed; boundary=b2\n\n--b2\nContent-Type: text/plain\n\nx\n--b2--\n--b1--\n"
	for name, c := range map[string]struct {
		raw string
		l   mime.Limits
	}{
		"parts":  {multipartMail, mime.Limits{MaxParts: 3}},
		"depth":  {nested, mime.Limits{MaxDepth: 1}},
		"header": {"From: a@example.org\nX-Long: " + strings.Repeat("a", 200) + "\n\nbody\n", mime.Limits{MaxHeaderBytes: 64}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := mime.Parse(strings.NewReader(crlf(c.raw)), c.l)
			if !errors.Is(err, mime.ErrMalformed) {
				t.Fatalf("Parse = %v, want ErrMalformed", err)
			}
		})
	}
}

func TestParseUnknownCharsetKeepsBytes(t *testing.T) {
	m := parse(t, "From: a@example.org\nContent-Type: text/plain; charset=x-unknown\n\nplain ascii\n", mime.Limits{})
	if m.Text != "plain ascii" {
		t.Fatalf("text = %q", m.Text)
	}
}

func TestComposeRoundTrip(t *testing.T) {
	out := mime.Outgoing{
		From:       mime.Address{Name: "Support Désk", Address: "support@example.com"},
		To:         []mime.Address{{Address: "jane@example.org"}},
		Subject:    "Re: Café order",
		Date:       time.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC),
		MessageID:  "<op1@example.com>",
		InReplyTo:  "<m2@example.org>",
		References: []string{"<m0@example.com>", "<m2@example.org>"},
		Text:       "Ça marche.\nSee you",
	}
	var buf bytes.Buffer
	if err := mime.Compose(&buf, out); err != nil {
		t.Fatal(err)
	}
	m, err := mime.Parse(&buf, mime.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	h := m.Header
	if h.MessageID != out.MessageID || h.InReplyTo != out.InReplyTo || h.References != "<m0@example.com> <m2@example.org>" {
		t.Errorf("threading = %q %q %q", h.MessageID, h.InReplyTo, h.References)
	}
	if h.Subject != out.Subject || len(h.From) != 1 || h.From[0] != out.From || len(h.To) != 1 || h.To[0] != out.To[0] {
		t.Errorf("header = %+v", h)
	}
	if !h.Date.Equal(out.Date) || m.Text != out.Text {
		t.Errorf("date %v text %q", h.Date, m.Text)
	}
	if got := h.Values("Mime-Version"); len(got) != 1 || got[0] != "1.0" {
		t.Errorf("MIME-Version = %v", got)
	}
}

func TestComposeCcAndBcc(t *testing.T) {
	out := mime.Outgoing{
		From:      mime.Address{Address: "support@example.com"},
		To:        []mime.Address{{Address: "jane@example.org"}, {Address: "bob@example.org"}},
		Cc:        []mime.Address{{Address: "carol@example.org"}},
		Subject:   "hi",
		Date:      time.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC),
		MessageID: "<op1@example.com>",
		Text:      "hello",
	}
	var buf bytes.Buffer
	if err := mime.Compose(&buf, out); err != nil {
		t.Fatal(err)
	}
	m, err := mime.Parse(bytes.NewReader(buf.Bytes()), mime.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Header.To) != 2 || len(m.Header.Cc) != 1 || m.Header.Cc[0].Address != "carol@example.org" {
		t.Errorf("to %+v cc %+v, want two to and one cc", m.Header.To, m.Header.Cc)
	}
	if strings.Contains(strings.ToLower(buf.String()), "bcc:") {
		t.Errorf("a message without Bcc carries a Bcc header:\n%s", buf.String())
	}
	out.Bcc = []mime.Address{{Address: "dave@example.org"}}
	buf.Reset()
	if err := mime.Compose(&buf, out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "dave@example.org") || !strings.Contains(buf.String(), "Bcc:") {
		t.Errorf("Bcc header missing:\n%s", buf.String())
	}
}

func TestComposeRejectsInvalid(t *testing.T) {
	ok := mime.Outgoing{From: mime.Address{Address: "a@example.com"}, To: []mime.Address{{Address: "b@example.org"}}, MessageID: "<x@example.com>", Text: "hi"}
	for name, mut := range map[string]func(*mime.Outgoing){
		"from display":   func(o *mime.Outgoing) { o.From.Address = "A <a@example.com>" },
		"no recipient":   func(o *mime.Outgoing) { o.To = nil },
		"bad recipient":  func(o *mime.Outgoing) { o.To = []mime.Address{{Address: "nobody"}} },
		"bad cc":         func(o *mime.Outgoing) { o.Cc = []mime.Address{{Address: "C <c@example.org>"}} },
		"bad bcc":        func(o *mime.Outgoing) { o.Bcc = []mime.Address{{Address: "nobody"}} },
		"subject header": func(o *mime.Outgoing) { o.Subject = "hi\r\nBcc: x@example.org" },
		"name header":    func(o *mime.Outgoing) { o.From.Name = "x\nBcc: y" },
		"message id":     func(o *mime.Outgoing) { o.MessageID = "x@example.com" },
		"in reply to":    func(o *mime.Outgoing) { o.InReplyTo = "<a> <b@c>" },
		"references":     func(o *mime.Outgoing) { o.References = []string{"<a@b>", "c"} },
	} {
		t.Run(name, func(t *testing.T) {
			o := ok
			mut(&o)
			if err := mime.Compose(&bytes.Buffer{}, o); !errors.Is(err, mime.ErrInvalid) {
				t.Fatalf("Compose = %v, want ErrInvalid", err)
			}
		})
	}
	if err := mime.Compose(&bytes.Buffer{}, ok); err != nil {
		t.Fatalf("valid message: %v", err)
	}
}

const keptMail = `From: jane@example.org
Subject: files
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="b"

--b
Content-Type: text/plain

see attached
--b
Content-Type: application/pdf; name="a.pdf"
Content-Disposition: attachment; filename="a.pdf"
Content-Transfer-Encoding: base64

aGVsbG8=
--b
Content-Type: application/zip; name="big.zip"
Content-Disposition: attachment; filename="big.zip"
Content-Transfer-Encoding: base64

dGhpcyBpcyB0b28gYmlnIGZvciB0aGUgYnVk
--b
Content-Type: image/png
Content-Disposition: attachment; filename="c.png"
Content-Transfer-Encoding: base64

cG5n
--b
Content-Type: text/csv
Content-Disposition: attachment; filename="d.csv"
Content-Transfer-Encoding: base64

YSxi
--b--
`

// Bytes are kept only on request: per attachment up to KeepBytes, and for
// at most KeepParts attachments. The others are described as before, and
// an attachment over the bound does not use one of the KeepParts.
func TestParseKeepsBytes(t *testing.T) {
	none := parse(t, keptMail, mime.Limits{})
	if none.Data != nil {
		t.Errorf("kept %q without being asked", none.Data)
	}
	m := parse(t, keptMail, mime.Limits{KeepBytes: 10, KeepParts: 2})
	if len(m.Attachments) != 4 {
		t.Fatalf("attachments = %+v", m.Attachments)
	}
	if got := string(m.Data[0]); got != "hello" {
		t.Errorf("a.pdf data = %q, want hello", got)
	}
	if m.Data[1] != nil || m.Attachments[1].Size != 27 {
		t.Errorf("big.zip = %+v, want described, over the per-attachment bound", m.Attachments[1])
	}
	if got := string(m.Data[2]); got != "png" {
		t.Errorf("c.png data = %q, want png: the oversize attachment used no slot", got)
	}
	if m.Data[3] != nil || m.Attachments[3].Size != 3 {
		t.Errorf("d.csv = %+v, want described, past KeepParts", m.Attachments[3])
	}
	for n := range none.Attachments {
		if m.Attachments[n].Digest != none.Attachments[n].Digest || m.Attachments[n].Size != none.Attachments[n].Size {
			t.Errorf("attachment %d changed with keeping: %+v", n, m.Attachments[n])
		}
	}
}
