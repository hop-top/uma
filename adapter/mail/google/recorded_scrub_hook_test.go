package google

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"reflect"
	"strings"
	"testing"

	mimepkg "hop.top/uma/adapter/mail/mime"
)

// Synthetic fixtures: hand-built messages and bodies, no cassette and no
// network. The "real" addresses are made up and stand for what a recording
// must not keep.
const (
	realMailbox = "Ada.Lovelace@analytical-engine.example"
	realPeer    = "charles@difference-engine.example"
)

func realIdentities() []identity { return recordedIdentities(realMailbox, realPeer) }

func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

func wrap76(s, nl string) string {
	var b strings.Builder
	for len(s) > 76 {
		b.WriteString(s[:76] + nl)
		s = s[76:]
	}
	return b.String() + s
}

func b64Body(s string) string {
	return wrap76(base64.StdEncoding.EncodeToString([]byte(s)), "\r\n")
}

// rawJSON is a Gmail JSON body carrying msg in "raw" as enc writes it.
func rawJSON(msg []byte, enc b64) []byte {
	b, _ := json.Marshal(map[string]string{"raw": enc.encoding().EncodeToString(msg), "threadId": "t-1"})
	return b
}

// scrubMessage runs msg through the hook inside a JSON body and returns
// the decoded message and the scrubbed JSON.
func scrubMessage(t testing.TB, sc *rawScrubber, msg []byte, enc b64) ([]byte, []byte) {
	t.Helper()
	out := sc.scrub("application/json; charset=UTF-8", rawJSON(msg, enc))
	var got struct{ Raw, ThreadID string }
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("scrubbed body is not JSON: %v\n%s", err, out)
	}
	if got.ThreadID != "t-1" {
		t.Fatalf("threadId = %q", got.ThreadID)
	}
	dec, gotEnc, ok := decodeB64(got.Raw)
	if !ok {
		t.Fatalf("scrubbed raw is not base64: %q", got.Raw)
	}
	if gotEnc.padded != strings.HasSuffix(got.Raw, "=") {
		t.Fatalf("padding detection broken for %q", got.Raw)
	}
	return dec, out
}

// leafTexts parses raw with the standard library, which fails the test on
// a broken MIME structure, and returns the decoded content of every leaf
// part and of every nested message's headers.
func leafTexts(t testing.TB, raw []byte) []string {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("scrubbed message does not parse: %v\n%q", err, raw)
	}
	return partTexts(t, textproto.MIMEHeader(m.Header), m.Body)
}

func partTexts(t testing.TB, h textproto.MIMEHeader, body io.Reader) []string {
	t.Helper()
	mt, params, _ := mime.ParseMediaType(h.Get("Content-Type"))
	var out []string
	switch {
	case strings.HasPrefix(mt, "multipart/"):
		mr := multipart.NewReader(body, params["boundary"])
		for {
			p, err := mr.NextRawPart()
			if err == io.EOF {
				return out
			}
			if err != nil {
				t.Fatalf("multipart: %v", err)
			}
			out = append(out, partTexts(t, p.Header, p)...)
		}
	}
	r := io.Reader(body)
	switch strings.ToLower(h.Get("Content-Transfer-Encoding")) {
	case "quoted-printable":
		r = quotedprintable.NewReader(body)
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, newlineStripper{body})
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("decode part: %v", err)
	}
	if mt == "message/rfc822" {
		m, err := mail.ReadMessage(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("nested message: %v", err)
		}
		for _, vs := range m.Header {
			out = append(out, vs...)
		}
		return append(out, partTexts(t, textproto.MIMEHeader(m.Header), m.Body)...)
	}
	return append(out, string(b))
}

type newlineStripper struct{ r io.Reader }

func (n newlineStripper) Read(p []byte) (int, error) {
	c, err := n.r.Read(p)
	q := p[:0]
	for _, b := range p[:c] {
		if b != '\r' && b != '\n' {
			q = append(q, b)
		}
	}
	return len(q), err
}

type scrubCase struct {
	name string
	msg  string
	// want are substrings of the scrubbed message (the whole, as bytes).
	want []string
	// texts are substrings of the decoded leaf content and nested headers.
	texts []string
}

func scrubCases() []scrubCase {
	headers := crlf(`From: Ada <ada.lovelace@ANALYTICAL-ENGINE.example>
To: Charles Babbage <charles@difference-engine.example>,
 "A. L." <Ada.Lovelace@analytical-engine.example>
Subject: uma recorded scenario
Message-Id: <abc123@analytical-engine.example>
Date: Sun, 11 Oct 2026 12:00:00 +0000
MIME-Version: 1.0
Content-Type: text/plain; charset=utf-8

mail charles@difference-engine.example or Ada.Lovelace@analytical-engine.example
`)
	qp := crlf(`From: <desk@outside.example>
To: <Ada.Lovelace@analytical-engine.example>
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

write to charles@difference-=
engine.example or ada.lovelace=40analytical-engine.example=20now
`)
	b64 := crlf(`From: <desk@outside.example>
To: <Ada.Lovelace@analytical-engine.example>
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: base64

`) + b64Body("contact: ada.lovelace@analytical-engine.example\nand charles@difference-engine.example, thanks, and a long enough tail to wrap the base64 lines at 76 columns\n") + "\r\n"
	multi := crlf(`From: <desk@outside.example>
To: <Ada.Lovelace@analytical-engine.example>
Content-Type: multipart/mixed; boundary="outer"

preamble ada.lovelace@analytical-engine.example
--outer
Content-Type: multipart/alternative; boundary=inner

--inner
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

hello charles@difference-=
engine.example
--inner
Content-Type: text/html; charset=utf-8
Content-Transfer-Encoding: base64

`) + b64Body("<p>ada.lovelace@analytical-engine.example</p>") + crlf(`
--inner--
--outer
Content-Type: application/octet-stream
Content-Transfer-Encoding: base64
Content-Disposition: attachment; filename="notes.txt"

`) + b64Body("from charles@difference-engine.example, an attachment long enough to need two base64 lines\n") + crlf(`
--outer
Content-Type: message/rfc822

From: <charles@difference-engine.example>
To: <ada.lovelace@analytical-engine.example>
Subject: fwd

forwarded note
--outer--
epilogue charles@difference-engine.example
`)
	words := crlf(`From: <desk@outside.example>
To: <desk@outside.example>
Subject: =?UTF-8?Q?mail_ada.lovelace=40analytical-engine.example?=
X-Note: =?iso-8859-1?q?caf=E9_charles=40difference-engine.example?=
Content-Type: text/plain; charset=utf-8

plain body
`)
	return []scrubCase{
		{"folded and mixed-case headers", headers, []string{
			"From: Ada <MAILBOX@mailbox.invalid>\r\n",
			"To: Charles Babbage <PEER@peer.invalid>, \"A. L.\" <MAILBOX@mailbox.invalid>\r\n",
			"Message-Id: <abc123@mailbox.invalid>\r\n",
			"Subject: uma recorded scenario\r\n",
		}, []string{"mail PEER@peer.invalid or MAILBOX@mailbox.invalid"}},
		{"quoted-printable body, soft break and =40 inside the address", qp, nil,
			[]string{"write to PEER@peer.invalid or MAILBOX@mailbox.invalid now"}},
		{"base64 body", b64, nil,
			[]string{"contact: MAILBOX@mailbox.invalid\nand PEER@peer.invalid, thanks"}},
		{"nested multipart, attachment and message/rfc822", multi, []string{"preamble MAILBOX@mailbox.invalid", "epilogue PEER@peer.invalid", "boundary=\"outer\"", "--inner--\r\n"},
			[]string{"hello PEER@peer.invalid", "<p>MAILBOX@mailbox.invalid</p>", "from PEER@peer.invalid, an attachment", "<PEER@peer.invalid>", "<MAILBOX@mailbox.invalid>", "forwarded note"}},
		{"RFC 2047 encoded words", words, []string{"Subject: mail MAILBOX@mailbox.invalid\r\n"}, nil},
	}
}

func TestScrubRemovesIdentitiesFromRawMessages(t *testing.T) {
	sc := newRawScrubber(realIdentities())
	for _, tc := range scrubCases() {
		t.Run(tc.name, func(t *testing.T) {
			msg := []byte(tc.msg)
			if len(residueIn(msg, realIdentities())) == 0 && len(residueIn(rawJSON(msg, b64{}), realIdentities())) == 0 {
				t.Fatal("fixture holds no identity: the residue check cannot see it")
			}
			got, body := scrubMessage(t, sc, msg, b64{})
			if r := residueIn(got, realIdentities()); len(r) > 0 {
				t.Fatalf("decoded message still holds %v:\n%s", r, got)
			}
			if r := residueIn(body, realIdentities()); len(r) > 0 {
				t.Fatalf("scrubbed body still holds %v", r)
			}
			for _, w := range tc.want {
				if !bytes.Contains(got, []byte(w)) {
					t.Errorf("scrubbed message lacks %q:\n%q", w, got)
				}
			}
			texts := strings.Join(leafTexts(t, got), "\x00")
			for _, w := range tc.texts {
				if !strings.Contains(texts, w) {
					t.Errorf("decoded content lacks %q: %q", w, texts)
				}
			}
			// Scrubbing again changes nothing.
			again, _ := scrubMessage(t, sc, got, b64{})
			if !bytes.Equal(again, got) {
				t.Errorf("not idempotent:\n%q\n%q", got, again)
			}
		})
	}
}

func TestScrubKeepsLineEndings(t *testing.T) {
	sc := newRawScrubber(realIdentities())
	msg := strings.ReplaceAll(scrubCases()[0].msg, "\r\n", "\n")
	got, _ := scrubMessage(t, sc, []byte(msg), b64{})
	if bytes.Contains(got, []byte("\r")) {
		t.Fatalf("LF message came back with CR: %q", got)
	}
	if !bytes.Contains(got, []byte("To: Charles Babbage <PEER@peer.invalid>, \"A. L.\" <MAILBOX@mailbox.invalid>\n")) {
		t.Fatalf("folded To not unfolded: %q", got)
	}
	qp := strings.ReplaceAll(scrubCases()[1].msg, "\r\n", "\n")
	got, _ = scrubMessage(t, sc, []byte(qp), b64{})
	if bytes.Contains(got, []byte("\r")) || len(residueIn(got, realIdentities())) > 0 {
		t.Fatalf("LF quoted-printable message: %q", got)
	}
}

// TestScrubKeepsBase64Form pins that "raw" is written back the way the
// API expects it: URL-safe alphabet, padding as it came.
func TestScrubKeepsBase64Form(t *testing.T) {
	sc := newRawScrubber(realIdentities())
	msg := []byte(strings.Replace(scrubCases()[0].msg, "Subject:", "X-Pad: ~~~~~~~~~~~~~~~~~~\r\nSubject:", 1))
	for _, tc := range []struct {
		name string
		enc  b64
	}{
		{"url-safe, unpadded", b64{}},
		{"url-safe, padded", b64{padded: true}},
		{"standard, padded", b64{std: true, padded: true}},
		{"standard, unpadded", b64{std: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.enc.encoding().EncodeToString(msg)
			if tc.enc.std != strings.ContainsAny(in, "+/") || (!tc.enc.std && !strings.ContainsAny(in, "-_")) {
				t.Fatalf("fixture does not use the alphabet it should: %q", in)
			}
			var out struct{ Raw string }
			if err := json.Unmarshal(sc.scrub("application/json", rawJSON(msg, tc.enc)), &out); err != nil {
				t.Fatal(err)
			}
			if tc.enc.std && strings.ContainsAny(out.Raw, "-_") || !tc.enc.std && strings.ContainsAny(out.Raw, "+/") {
				t.Errorf("alphabet changed: %q", out.Raw)
			}
			if tc.enc.padded && len(out.Raw)%4 != 0 {
				t.Errorf("padded input came back unpadded: %q", out.Raw)
			}
			if !tc.enc.padded && strings.HasSuffix(out.Raw, "=") {
				t.Errorf("unpadded input came back padded: %q", out.Raw)
			}
		})
	}
}

func TestScrubLeavesUnrelatedBodiesAlone(t *testing.T) {
	sc := newRawScrubber(realIdentities())
	clean := crlf("From: <a@outside.example>\nTo: <b@outside.example>\nSubject: hi\n\nnothing to see\n")
	for name, tc := range map[string]struct {
		contentType string
		body        string
	}{
		"message without identity": {"application/json", string(rawJSON([]byte(clean), b64{padded: true}))},
		"other content type":       {"text/plain", `{"raw":"` + realMailbox + `"}`},
		"not JSON":                 {"application/json", "<html>" + realMailbox + "</html>"},
		"JSON followed by more":    {"application/json", `{"a":1} {"b":"` + realMailbox + `"}`},
		"empty":                    {"application/json", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := sc.scrub(tc.contentType, []byte(tc.body)); string(got) != tc.body {
				t.Fatalf("body changed:\n%s", got)
			}
		})
	}
	if newRawScrubber(nil).scrub("application/json", []byte(`{"raw":"x"}`)) == nil {
		t.Fatal("scrubber without identities dropped the body")
	}
}

func TestScrubJSONResponses(t *testing.T) {
	sc := newRawScrubber(realIdentities())
	msg := []byte(scrubCases()[0].msg)
	body := `{"id":"m1","threadId":"t1","labelIds":["INBOX","UNREAD"],"internalDate":"1760184000000","historyId":"12345678901234567890",` +
		`"snippet":"mail ada.lovelace\u0040analytical-engine.example and CHARLES@difference-engine.example","sizeEstimate":512,` +
		`"raw":"` + base64.URLEncoding.EncodeToString(msg) + `"}`
	got := sc.scrub("application/json; charset=UTF-8", []byte(body))
	if r := residueIn(got, realIdentities()); len(r) > 0 {
		t.Fatalf("response still holds %v:\n%s", r, got)
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(got))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"id": "m1", "threadId": "t1", "labelIds": []any{"INBOX", "UNREAD"}, "internalDate": "1760184000000",
		"historyId": "12345678901234567890", "sizeEstimate": json.Number("512"),
		"snippet": "mail MAILBOX and PEER",
	}
	for k, v := range want {
		if !reflect.DeepEqual(m[k], v) {
			t.Errorf("%s = %#v, want %#v", k, m[k], v)
		}
	}
	if !strings.HasSuffix(m["raw"].(string), "=") {
		t.Errorf("padded raw lost its padding: %q", m["raw"])
	}
}

// pinned messages built the way a writer folds and encodes them: the bytes
// depend on how long the addresses are.
func foldedMessage(mailbox, peer string) []byte {
	fold := func(s string) string {
		var b strings.Builder
		for len(s) > 76 {
			i := strings.LastIndexByte(s[:76], ' ')
			if i < 0 {
				break
			}
			b.WriteString(s[:i] + "\r\n ")
			s = s[i+1:]
		}
		return b.String() + s
	}
	var qp bytes.Buffer
	w := quotedprintable.NewWriter(&qp)
	_, _ = w.Write([]byte("write to " + peer + " or " + mailbox + " about the analytical engine and the difference engine, soon\r\n"))
	_ = w.Close()
	return []byte("From: Ada Lovelace <" + mailbox + ">\r\n" +
		fold("To: Charles Babbage <"+peer+">, Ada Lovelace <"+mailbox+">, Somebody Else <someone@outside.example>") + "\r\n" +
		fold("Cc: Charles Babbage <"+peer+">, Ada Lovelace <"+mailbox+">") + "\r\n" +
		"Subject: uma recorded scenario\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" + qp.String())
}

// TestScrubOutputDoesNotDependOnTheAddress pins replay: the scrubbed
// message a recording stores for real addresses is, byte for byte, the one
// a replay derives for the fake addresses, however the real addresses made
// a writer fold headers or break quoted-printable lines.
func TestScrubOutputDoesNotDependOnTheAddress(t *testing.T) {
	replayMailbox, replayPeer := "mailbox@example.test", "peer@example.test"
	pairs := [][2]string{
		{realMailbox, realPeer},
		{"me@x.test", "you@y.test"},
		{"a.very.long.local.part.of.a.mailbox@an-equally-long-domain-name.example", "another.long.local.part.for.the.peer@a-long-peer-domain.example"},
	}
	compose := func(mailbox, peer string) []byte {
		var b bytes.Buffer
		err := mimepkg.Compose(&b, mimepkg.Outgoing{
			From: mimepkg.Address{Address: mailbox}, To: []mimepkg.Address{{Address: peer}}, Subject: "uma recorded scenario", Date: testNow,
			MessageID: "<abcdefghijklmnopqrstuvwxyz234567abcdefgh@" + recordedMessageIDDomain + ">",
			Text:      "reply to " + mailbox + " and cc " + peer + " when you can, with a tail that is long enough to break lines",
		})
		if err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	for name, build := range map[string]func(string, string) []byte{"folded writer": foldedMessage, "adapter Compose": compose} {
		replay, _ := scrubMessage(t, newRawScrubber(recordedIdentities(replayMailbox, replayPeer)), build(replayMailbox, replayPeer), b64{})
		for _, p := range pairs {
			got, _ := scrubMessage(t, newRawScrubber(recordedIdentities(p[0], p[1])), build(p[0], p[1]), b64{})
			if !bytes.Equal(got, replay) {
				t.Errorf("%s with %s / %s scrubs differently from the replay addresses:\n%q\n%q", name, p[0], p[1], got, replay)
			}
		}
		if len(residueIn(replay, recordedIdentities(replayMailbox, replayPeer))) > 0 {
			t.Errorf("%s: replay-side scrub left an address", name)
		}
	}
}

func TestRecordedIdentities(t *testing.T) {
	names := func(ids []identity) []string {
		var out []string
		for _, id := range ids {
			out = append(out, id.name+"="+id.value)
		}
		return out
	}
	for name, tc := range map[string]struct {
		mailbox, peer string
		want          []string
	}{
		"own domains": {"a@corp.example", "b@other.example", []string{
			"mailbox address=a@corp.example", "peer address=b@other.example", "mailbox domain=corp.example", "peer domain=other.example"}},
		"public provider domains stay": {"me@gmail.com", "you@GoogleMail.com", []string{
			"mailbox address=me@gmail.com", "peer address=you@GoogleMail.com"}},
		"one shared domain": {"a@corp.example", "b@Corp.example", []string{
			"mailbox address=a@corp.example", "peer address=b@Corp.example", "mailbox domain=corp.example"}},
		"unset values": {"", "", nil},
	} {
		if got := names(recordedIdentities(tc.mailbox, tc.peer)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
