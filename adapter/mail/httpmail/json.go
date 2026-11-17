package httpmail

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/adapter/mail/stage"
	"hop.top/uma/model"
)

// decodeJSON reads a FormatJSON body: one object whose members are named by
// Config.JSONFields. Members it does not name are ignored.
func (i *Instance) decodeJSON(body []byte) (inbound, error) {
	f := i.cfg.JSONFields
	var members map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&members); err != nil {
		return inbound{}, malformed
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return inbound{}, malformed
	}
	text := func(name string) (string, bool) { return textMember(members[name]) }
	m := inbound{format: FormatJSON, raw: body, claimedAttachments: true}

	from, ok := text(f.From)
	if !ok {
		return inbound{}, malformed
	}
	sender, err := mail.ParseAddress(from)
	if err != nil {
		return inbound{}, malformed
	}
	if m.sender, err = model.NormalizeEmail(sender.Address); err != nil {
		return inbound{}, malformed
	}
	m.header.From = []mime.Address{{Name: sender.Name, Address: sender.Address}}

	to, ok := addressList(members[f.To])
	if !ok || len(to) == 0 || len(to) > maxRecipients {
		return inbound{}, malformed
	}
	for _, a := range to {
		m.to = append(m.to, a.Address)
	}
	m.header.To = capAddrs(to)

	for _, p := range []struct {
		name string
		dst  *string
	}{
		{f.Subject, &m.header.Subject}, {f.MessageID, &m.header.MessageID},
		{f.InReplyTo, &m.header.InReplyTo}, {f.References, &m.header.References},
	} {
		s, ok := text(p.name)
		if !ok {
			return inbound{}, malformed
		}
		*p.dst = cut(s, maxFieldBytes)
	}
	bodyText, ok := text(f.Body)
	if !ok {
		return inbound{}, malformed
	}
	m.text, m.truncated = boundText(bodyText)

	if s, ok := text(f.Date); ok {
		if t, ok := parseDate(s); ok {
			m.occurred = t
			m.header.Date = t.UTC().Format(time.RFC3339)
		}
	}
	if name := i.cfg.Evidence.AuthenticationResultsField; name != "" {
		ar, ok := text(name)
		if !ok {
			return inbound{}, malformed
		}
		if ar != "" {
			m.evidence.Claimed.AuthenticationResults = claims([]string{ar})
			m.evidence = i.trust(m.evidence, m.sender, []string{ar})
		}
	}
	m.attachments, m.omitted, m.kept = describeAttachments(members[f.Attachments], f.AttachmentFields)
	return m, nil
}

// textMember reads a JSON string; an absent or null member is the empty
// string, any other type is not text.
func textMember(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", true
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// addressList reads recipients: a string holding an address list, or an
// array of such strings.
func addressList(raw json.RawMessage) ([]mime.Address, bool) {
	var lists []string
	var one string
	switch {
	case json.Unmarshal(raw, &one) == nil && len(raw) > 0 && raw[0] == '"':
		lists = []string{one}
	case json.Unmarshal(raw, &lists) != nil || len(raw) == 0 || raw[0] != '[':
		return nil, false
	}
	var out []mime.Address
	for _, l := range lists {
		if strings.TrimSpace(l) == "" {
			continue
		}
		as, err := mail.ParseAddressList(l)
		if err != nil {
			return nil, false
		}
		for _, a := range as {
			out = append(out, mime.Address{Name: a.Name, Address: a.Address})
		}
	}
	return out, true
}

// boundText normalizes line ends and keeps at most the content limit.
func boundText(s string) (string, bool) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if limit := model.DefaultContentLimits().MaxTextBytes; len(s) > limit {
		return cut(s, limit), true
	}
	return s, false
}

// parseDate reads a Date as RFC 3339 or RFC 5322.
func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := mail.ParseDate(s); err == nil {
		return t, true
	}
	// RFC 5322 with a colon in the zone, as some servers write it (and
	// Postmark passes it on).
	for _, layout := range []string{"Mon, 2 Jan 2006 15:04:05 -07:00", "2 Jan 2006 15:04:05 -07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// describeAttachments reads an array of attachment descriptions. An entry
// that is no object, or names none of the members read, is dropped; a URL
// is kept only when absolute http or https, and is never fetched. Bytes
// the sender embedded in an entry are decoded and never kept in the
// description; those within the staging bounds are returned as the
// attachments to stage, index for index with the descriptions (nil when
// there are none). At most maxAttachments entries are kept; the rest are
// counted.
func describeAttachments(raw json.RawMessage, f AttachmentFields) ([]attachmentRecord, int, *mime.Message) {
	var entries []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &entries) != nil {
		return nil, 0, nil
	}
	var (
		out     []attachmentRecord
		omitted int
		kept    mime.Message
		staged  int
	)
	for _, e := range entries {
		var obj map[string]json.RawMessage
		if json.Unmarshal(e, &obj) != nil || obj == nil {
			continue
		}
		rec, present := attachmentOf(obj, f)
		data := embeddedBytes(obj[f.Content])
		if data != nil {
			sum := sha256.Sum256(data)
			rec.Size, rec.Digest, present = int64(len(data)), "sha256:"+hex.EncodeToString(sum[:]), true
		}
		switch {
		case !present:
		case len(out) >= maxAttachments:
			omitted++
		default:
			out = append(out, rec)
			kept.Attachments = append(kept.Attachments, mime.Attachment{
				Filename: rec.Filename, MediaType: rec.MediaType, Size: rec.Size, Digest: rec.Digest,
			})
			if data != nil && staged < stage.MaxAttachments {
				staged++
			} else {
				data = nil
			}
			kept.Data = append(kept.Data, data)
		}
	}
	if staged == 0 {
		return out, omitted, nil
	}
	return out, omitted, &kept
}

// embeddedBytes decodes the base64 an attachment entry carries; nil when
// there is none, it is no base64, or it decodes to nothing or beyond the
// staging bound (the entry is then described only).
func embeddedBytes(raw json.RawMessage) []byte {
	s, ok := textMember(raw)
	if !ok || s == "" {
		return nil
	}
	// Line breaks are tolerated, as in MIME; the bound is checked on the
	// decoded size.
	s = strings.NewReplacer("\r", "", "\n", "").Replace(s)
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(s)
	}
	if err != nil || len(b) == 0 || len(b) > stage.MaxAttachmentBytes {
		return nil
	}
	return b
}

// attachmentOf reads one entry; present is false when it names none of the
// members.
func attachmentOf(obj map[string]json.RawMessage, f AttachmentFields) (attachmentRecord, bool) {
	var rec attachmentRecord
	present := false
	read := func(name string) string {
		s, _ := textMember(obj[name])
		if s != "" {
			present = true
		}
		return s
	}
	rec.Kind = cut(read(f.Type), 64)
	rec.Filename = mime.SanitizeFilename(read(f.Name))
	rec.MediaType = cut(read(f.MimeType), 255)
	rec.URL = linkOf(read(f.URL))
	if n, ok := sizeMember(obj[f.SizeBytes]); ok {
		rec.Size, present = n, true
	}
	return rec, present
}

// sizeMember reads a byte count: a non-negative whole JSON number.
func sizeMember(raw json.RawMessage) (int64, bool) {
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	v, err := n.Int64()
	return v, err == nil && v > 0
}

const maxURLBytes = 2048

// linkOf returns s when it is an absolute http or https URL of sane length,
// else the empty string. The adapter never fetches it.
func linkOf(s string) string {
	if s == "" || len(s) > maxURLBytes || strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return s
}
