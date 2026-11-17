package google

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/quotedprintable"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// The recorded Gmail test carries mail addresses in places the xrrtest
// placeholders (literal matches on URLs and bodies) cannot reach: the
// outgoing message is base64url in the "raw" field of messages.send, and a
// format=raw fetch returns messages the same way. The hook in this file
// decodes those messages, replaces the mailbox and peer addresses (and
// their domains) with placeholders and encodes them again, so a cassette
// holds none of them and a replay derives the same request bytes.

const (
	// recordedMessageIDDomain is the fixed message_id_domain of the
	// recorded channel. The adapter derives the Message-ID domain from the
	// mailbox otherwise, and the Sent search (rfc822msgid:) carries the
	// Message-ID in the request URL. It must not contain the domain of the
	// replay-default addresses, or the domain placeholder would rewrite it
	// on replay only.
	recordedMessageIDDomain = "msgid.uma.invalid"

	placeholderMailbox = "MAILBOX"
	placeholderPeer    = "PEER"
)

// recordedNow is the clock of the recorded scenario, and recordedSyncUntil
// the upper bound of its first inbox sync: the hour before it is the window
// (after: the clock less one hour, before: the clock). The window is in 2001,
// long before any real mail a mailbox can hold, so it holds only the
// messages inserted for the recording and a replay and a re-recording list
// the same ones. It is a day apart from the history window
// (recordedHistorySince). A wall clock must not be used here, or no
// recording would replay.
var (
	recordedNow       = time.Date(2001, time.January, 3, 12, 0, 0, 0, time.UTC)
	recordedSyncUntil = recordedNow
)

// publicDomains are mailbox providers whose domain is harmless public text
// and would match unrelated content: their domain is not scrubbed, their
// addresses still are.
var publicDomains = []string{"gmail.com", "googlemail.com"}

// identity is one real value and what replaces it: token in JSON strings
// (the same placeholder xrrtest writes for the address) and rawToken inside
// a decoded message, where an address-shaped value keeps headers parseable.
type identity struct{ name, value, token, rawToken string }

// recordedIdentities lists what must not reach a cassette: both addresses,
// then their domains (except public providers, and the peer's when it is the
// mailbox's). Addresses come first so a domain never splits one.
func recordedIdentities(mailbox, peer string) []identity {
	ids := []identity{
		{"mailbox address", mailbox, placeholderMailbox, "MAILBOX@mailbox.invalid"},
		{"peer address", peer, placeholderPeer, "PEER@peer.invalid"},
	}
	for _, d := range []identity{
		{"mailbox domain", addrDomain(mailbox), "MAILBOX_DOMAIN", "mailbox.invalid"},
		{"peer domain", addrDomain(peer), "PEER_DOMAIN", "peer.invalid"},
	} {
		if d.value == "" || slices.Contains(publicDomains, strings.ToLower(d.value)) {
			continue
		}
		if slices.ContainsFunc(ids, func(o identity) bool { return strings.EqualFold(o.value, d.value) }) {
			continue
		}
		ids = append(ids, d)
	}
	// Messages placed in the mailbox with users.messages.insert carry the
	// inserting Cloud project's number in their Received header; the
	// recording names it in GMAIL_INSERT_PROJECT so it is scrubbed too.
	ids = append(ids, identity{"inserting project", os.Getenv("GMAIL_INSERT_PROJECT"), "INSERT_PROJECT", "000000000000"})
	return slices.DeleteFunc(ids, func(i identity) bool { return i.value == "" })
}

func addrDomain(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return addr[i+1:]
	}
	return ""
}

// rawScrubber rewrites the identities in Gmail JSON bodies, including the
// RFC 5322 message inside "raw". Matching is case-insensitive, since mail
// systems change the case of addresses.
type rawScrubber struct {
	re  *regexp.Regexp
	ids map[string]identity // by lower-cased value
}

func newRawScrubber(ids []identity) *rawScrubber {
	s := &rawScrubber{ids: map[string]identity{}}
	var alts []string
	for _, id := range ids {
		if id.value == "" {
			continue
		}
		s.ids[strings.ToLower(id.value)] = id
		alts = append(alts, regexp.QuoteMeta(id.value))
	}
	if len(alts) == 0 {
		return s
	}
	slices.SortStableFunc(alts, func(a, b string) int { return len(b) - len(a) })
	s.re = regexp.MustCompile("(?i)" + strings.Join(alts, "|"))
	return s
}

// scrub is the xrrtest.ScrubFunc: JSON bodies only, other types are
// returned unchanged. It runs on request bodies before fingerprinting and
// on response bodies before saving.
func (s *rawScrubber) scrub(contentType string, body []byte) []byte {
	if s.re == nil || len(body) == 0 {
		return body
	}
	if mt, _, err := mime.ParseMediaType(contentType); err != nil || (mt != "application/json" && !strings.HasSuffix(mt, "+json")) {
		return body
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return body
	}
	v, changed := s.walk(v, "")
	if !changed {
		return body
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return body
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func (s *rawScrubber) walk(v any, key string) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		changed := false
		for k, c := range t {
			// The profile's counts describe the real mailbox; nothing the
			// adapter reads.
			if n, ok := c.(json.Number); ok && (k == "messagesTotal" || k == "threadsTotal") && n != "0" {
				t[k], changed = json.Number("0"), true
				continue
			}
			if nv, ch := s.walk(c, k); ch {
				t[k], changed = nv, true
			}
		}
		return t, changed
	case []any:
		changed := false
		for i, c := range t {
			if nv, ch := s.walk(c, key); ch {
				t[i], changed = nv, true
			}
		}
		return t, changed
	case string:
		var out string
		if key == "raw" {
			out = s.rawField(t)
		} else {
			out = s.text(t)
		}
		return out, out != t
	}
	return v, false
}

// text replaces the identities in a JSON string.
func (s *rawScrubber) text(v string) string {
	return s.re.ReplaceAllStringFunc(v, func(m string) string { return s.ids[strings.ToLower(m)].token })
}

// inMessage replaces the identities in message text.
func (s *rawScrubber) inMessage(v string) string {
	return s.re.ReplaceAllStringFunc(v, func(m string) string { return s.ids[strings.ToLower(m)].rawToken })
}

func (s *rawScrubber) bytes(b []byte) []byte {
	if !s.re.Match(b) {
		return b
	}
	return s.re.ReplaceAllFunc(b, func(m []byte) []byte { return []byte(s.ids[strings.ToLower(string(m))].rawToken) })
}

// b64 describes how a base64 string was written, so it is written back the
// same way: the alphabet and the padding the API expects.
type b64 struct{ std, padded bool }

func (e b64) encoding() *base64.Encoding {
	switch {
	case e.std && e.padded:
		return base64.StdEncoding
	case e.std:
		return base64.RawStdEncoding
	case e.padded:
		return base64.URLEncoding
	}
	return base64.RawURLEncoding
}

func decodeB64(s string) ([]byte, b64, bool) {
	e := b64{padded: strings.HasSuffix(s, "="), std: strings.ContainsAny(s, "+/")}
	b, err := e.encoding().DecodeString(s)
	return b, e, err == nil
}

// rawField rewrites a base64url message; a value that is not base64 is
// treated as text.
func (s *rawScrubber) rawField(v string) string {
	msg, enc, ok := decodeB64(v)
	if !ok {
		return s.text(v)
	}
	out := s.message(msg)
	if bytes.Equal(out, msg) {
		return v
	}
	return enc.encoding().EncodeToString(out)
}

var (
	headerEnd = regexp.MustCompile(`\r?\n\r?\n`)
	lineBreak = regexp.MustCompile(`\r?\n`)
)

// message rewrites one RFC 5322 message (or MIME part): its header block,
// then its body by Content-Type and Content-Transfer-Encoding. Text that
// holds no identity comes back byte for byte.
func (s *rawScrubber) message(msg []byte) []byte {
	var head, sep, body []byte
	switch loc := headerEnd.FindIndex(msg); {
	case bytes.HasPrefix(msg, []byte("\r\n")):
		sep, body = msg[:2], msg[2:]
	case bytes.HasPrefix(msg, []byte("\n")):
		sep, body = msg[:1], msg[1:]
	case loc == nil:
		head = msg
	default:
		head, sep, body = msg[:loc[0]], msg[loc[0]:loc[1]], msg[loc[1]:]
	}
	nl := "\n"
	if bytes.Contains(msg, []byte("\r\n")) {
		nl = "\r\n"
	}
	head, ctype, cte := s.headers(head)
	if len(body) > 0 {
		body = s.body(body, ctype, cte, nl)
	}
	out := make([]byte, 0, len(head)+len(sep)+len(body))
	return append(append(append(out, head...), sep...), body...)
}

// headers rewrites the header block. A field that holds an identity (also
// inside RFC 2047 encoded words) is unfolded onto one line first: where a
// writer folds depends on the length of what it wrote, so only an unfolded
// field is the same text whatever the real address was. Fields without an
// identity keep their bytes. It returns the Content-Type and
// Content-Transfer-Encoding values.
func (s *rawScrubber) headers(head []byte) ([]byte, string, string) {
	var out bytes.Buffer
	var ctype, cte string
	lines := splitKeep(head)
	for i := 0; i < len(lines); {
		j := i + 1
		for j < len(lines) && (lines[j][0] == ' ' || lines[j][0] == '\t') {
			j++
		}
		field := lines[i:j]
		i = j
		raw := strings.Join(field, "")
		text := lineBreak.ReplaceAllString(raw, "")
		name, value, found := strings.Cut(text, ":")
		if found {
			if nv := s.headerValue(value); nv != value {
				eol := raw[len(strings.TrimRight(raw, "\r\n")):]
				raw, value = name+":"+nv+eol, nv
			}
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "content-type":
				ctype = strings.TrimSpace(value)
			case "content-transfer-encoding":
				cte = strings.ToLower(strings.TrimSpace(value))
			}
		}
		out.WriteString(raw)
	}
	return out.Bytes(), ctype, cte
}

// headerValue rewrites an unfolded header value. Encoded words are decoded
// first, since an address inside one is not readable in its encoded form
// (its "@" is "=40" or part of base64), and a value that hid an identity is
// written as plain text (UTF-8, RFC 6532).
func (s *rawScrubber) headerValue(v string) string {
	if strings.Contains(v, "=?") {
		if dec, err := wordDecoder.DecodeHeader(v); err == nil && dec != v && s.re.MatchString(dec) {
			return s.inMessage(dec)
		}
	}
	return s.inMessage(v)
}

// wordDecoder decodes encoded words of any charset byte for byte: the
// identities are ASCII, which every charset a mail header names shares.
var wordDecoder = &mime.WordDecoder{CharsetReader: func(_ string, in io.Reader) (io.Reader, error) { return in, nil }}

// body rewrites a body according to its media type and transfer encoding.
func (s *rawScrubber) body(body []byte, ctype, cte, nl string) []byte {
	mt, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		mt = "text/plain"
	}
	if strings.HasPrefix(mt, "multipart/") && params["boundary"] != "" && (cte == "" || cte == "7bit" || cte == "8bit" || cte == "binary") {
		return s.multipart(body, params["boundary"])
	}
	f := s.bytes
	if strings.HasPrefix(mt, "message/") {
		f = s.message
	}
	return transfer(body, cte, nl, f)
}

// multipart rewrites each part and the text around them, keeping the
// delimiter lines.
func (s *rawScrubber) multipart(body []byte, boundary string) []byte {
	var out, part bytes.Buffer
	flush := func(isPart bool) {
		if part.Len() == 0 {
			return
		}
		if isPart {
			out.Write(s.message(part.Bytes()))
		} else {
			out.Write(s.bytes(part.Bytes()))
		}
		part.Reset()
	}
	inPart := false
	for _, line := range splitKeep(body) {
		trimmed := strings.TrimRight(line, " \t\r\n")
		if trimmed == "--"+boundary || trimmed == "--"+boundary+"--" {
			flush(inPart)
			out.WriteString(line)
			inPart = trimmed == "--"+boundary
			continue
		}
		part.WriteString(line)
	}
	flush(inPart)
	return out.Bytes()
}

// transfer decodes body from its transfer encoding, applies f and encodes
// the result again, only when f changed something. Base64 is wrapped at 76
// columns and quoted-printable at its own line limit, so the output does not
// depend on where the original broke its lines. A body that does not decode
// is treated as plain.
func transfer(body []byte, cte, nl string, f func([]byte) []byte) []byte {
	switch cte {
	case "base64":
		compact := strings.NewReplacer("\r", "", "\n", "", " ", "", "\t", "").Replace(string(body))
		dec, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(compact, "="))
		if err != nil {
			return f(body)
		}
		out := f(dec)
		if bytes.Equal(out, dec) {
			return body
		}
		enc := base64.StdEncoding.EncodeToString(out)
		var b strings.Builder
		for len(enc) > 76 {
			b.WriteString(enc[:76] + nl)
			enc = enc[76:]
		}
		b.WriteString(enc)
		if bytes.HasSuffix(body, []byte("\n")) {
			b.WriteString(nl)
		}
		return []byte(b.String())
	case "quoted-printable":
		dec, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		if err != nil {
			return f(body)
		}
		out := f(dec)
		if bytes.Equal(out, dec) {
			return body
		}
		var buf bytes.Buffer
		w := quotedprintable.NewWriter(&buf)
		_, _ = w.Write(out)
		_ = w.Close()
		enc := buf.String()
		if nl == "\n" {
			enc = strings.ReplaceAll(enc, "\r\n", "\n")
		}
		return []byte(enc)
	}
	return f(body)
}

// splitKeep splits b into lines, each keeping its line break.
func splitKeep(b []byte) []string {
	var lines []string
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			i = len(b) - 1
		}
		lines = append(lines, string(b[:i+1]))
		b = b[i+1:]
	}
	return lines
}

// The profile's message and thread counts never reach a cassette.
func TestScrubZeroesMailboxCounts(t *testing.T) {
	s := newRawScrubber(recordedIdentities("box@corp.test", "peer@corp.test"))
	got := string(s.scrub("application/json", []byte(`{"emailAddress":"box@corp.test","messagesTotal":4821,"threadsTotal":3170,"historyId":"77"}`)))
	if strings.Contains(got, "4821") || strings.Contains(got, "3170") || !strings.Contains(got, `"messagesTotal":0`) || !strings.Contains(got, `"historyId":"77"`) {
		t.Fatalf("scrubbed profile = %s", got)
	}
}
