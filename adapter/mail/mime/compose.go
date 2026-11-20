package mime

import (
	"errors"
	"fmt"
	"io"
	"net/mail"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	gomail "github.com/emersion/go-message/mail"

	"hop.top/uma/model"
)

// Outgoing is one outbound message.
type Outgoing struct {
	From Address
	To   []Address
	// Cc is written as the Cc header. Bcc is written as a Bcc header only
	// for a provider that reads the blind recipients from the message (the
	// Gmail API removes the header before delivery); a path that delivers
	// by envelope leaves it empty and names them in RCPT instead.
	Cc      []Address
	Bcc     []Address
	Subject string
	Date    time.Time
	// MessageID, InReplyTo and References are msg-ids in angle brackets,
	// "<left@right>".
	MessageID  string
	InReplyTo  string
	References []string
	// Text is the UTF-8 body.
	Text string
	// Embedded, when set, is carried whole as a message/rfc822 attachment
	// after the text: the message is multipart/mixed.
	Embedded *Embedded
}

// Embedded is an RFC 5322 message carried inside another as a
// message/rfc822 part (RFC 2046 section 5.2.1), the way a mail client
// forwards "as attachment": every header, body part and attachment of the
// original survives untouched.
type Embedded struct {
	// Name is the attachment's filename, as it is shown; empty leaves the
	// part unnamed. It is written as given: sanitize it first.
	Name string
	// Raw is the message's bytes, CRLF line endings, headers first.
	Raw []byte
}

// ErrInvalid is wrapped by Compose errors caused by the Outgoing value.
var ErrInvalid = errors.New("mime: invalid outgoing message")

// Compose writes m as a single-part text/plain UTF-8 message, or, when
// m.Embedded is set, as multipart/mixed: the text, then the embedded
// message as an attachment. The embedded bytes are written as they are,
// "7bit" when all are ASCII, else "8bit": RFC 2046 allows no other
// transfer encoding for a message/rfc822 part.
func Compose(w io.Writer, m Outgoing) error {
	if err := m.validate(); err != nil {
		return err
	}
	var h gomail.Header
	h.SetDate(m.Date)
	h.SetAddressList("From", []*gomail.Address{{Name: m.From.Name, Address: m.From.Address}})
	to := make([]*gomail.Address, 0, len(m.To))
	for _, a := range m.To {
		to = append(to, &gomail.Address{Name: a.Name, Address: a.Address})
	}
	h.SetAddressList("To", to)
	for _, f := range []struct {
		name string
		list []Address
	}{{"Cc", m.Cc}, {"Bcc", m.Bcc}} {
		if len(f.list) == 0 {
			continue
		}
		as := make([]*gomail.Address, 0, len(f.list))
		for _, a := range f.list {
			as = append(as, &gomail.Address{Name: a.Name, Address: a.Address})
		}
		h.SetAddressList(f.name, as)
	}
	if m.Subject != "" {
		h.SetSubject(m.Subject)
	}
	h.SetMessageID(bare(m.MessageID))
	if m.InReplyTo != "" {
		h.SetMsgIDList("In-Reply-To", []string{bare(m.InReplyTo)})
	}
	if len(m.References) > 0 {
		refs := make([]string, 0, len(m.References))
		for _, r := range m.References {
			refs = append(refs, bare(r))
		}
		h.SetMsgIDList("References", refs)
	}
	if m.Embedded != nil {
		return composeEmbedded(w, h, m)
	}
	h.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
	body, err := gomail.CreateSingleInlineWriter(w, h)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(body, m.Text); err != nil {
		return err
	}
	return body.Close()
}

// composeEmbedded writes the headers h and the multipart/mixed body of m.
func composeEmbedded(w io.Writer, h gomail.Header, m Outgoing) error {
	mw, err := gomail.CreateWriter(w, h)
	if err != nil {
		return err
	}
	inline, err := mw.CreateInline()
	if err != nil {
		return err
	}
	var th gomail.InlineHeader
	th.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
	text, err := inline.CreatePart(th)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(text, m.Text); err != nil {
		return err
	}
	if err := text.Close(); err != nil {
		return err
	}
	if err := inline.Close(); err != nil {
		return err
	}
	var ah gomail.AttachmentHeader
	ah.SetContentType("message/rfc822", nil)
	if m.Embedded.Name != "" {
		ah.SetFilename(m.Embedded.Name)
	}
	enc := "7bit"
	if slices.ContainsFunc(m.Embedded.Raw, func(b byte) bool { return b >= 0x80 }) {
		enc = "8bit"
	}
	ah.Set("Content-Transfer-Encoding", enc)
	att, err := mw.CreateAttachment(ah)
	if err != nil {
		return err
	}
	if _, err := att.Write(m.Embedded.Raw); err != nil {
		return err
	}
	if err := att.Close(); err != nil {
		return err
	}
	return mw.Close()
}

func (m Outgoing) validate() error {
	bad := func(field, why string) error { return fmt.Errorf("%w: %s %s", ErrInvalid, field, why) }
	if !bareAddress(m.From.Address) {
		return bad("from", "must be a bare address")
	}
	if len(m.To) == 0 {
		return bad("to", "required")
	}
	for field, list := range map[string][]Address{"to": m.To, "cc": m.Cc, "bcc": m.Bcc} {
		for _, a := range list {
			if !bareAddress(a.Address) {
				return bad(field, "must be bare addresses")
			}
		}
	}
	for field, v := range map[string]string{"from name": m.From.Name, "subject": m.Subject} {
		if strings.ContainsAny(v, "\r\n") || !utf8.ValidString(v) {
			return bad(field, "must be one line of UTF-8")
		}
	}
	if !msgID(m.MessageID) {
		return bad("message_id", "must be one msg-id")
	}
	if m.InReplyTo != "" && !msgID(m.InReplyTo) {
		return bad("in_reply_to", "must be one msg-id")
	}
	for _, r := range m.References {
		if !msgID(r) {
			return bad("references", "must be msg-ids")
		}
	}
	if !utf8.ValidString(m.Text) {
		return bad("text", "must be UTF-8")
	}
	if e := m.Embedded; e != nil {
		switch {
		case len(e.Raw) == 0:
			return bad("embedded", "needs the message's bytes")
		case slices.Contains(e.Raw, 0):
			return bad("embedded", "must not hold NUL bytes")
		case strings.ContainsAny(e.Name, "\r\n") || !utf8.ValidString(e.Name):
			return bad("embedded name", "must be one line of UTF-8")
		}
	}
	return nil
}

// bareAddress reports whether s is an addr-spec without display name,
// brackets or surrounding space.
func bareAddress(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Name == "" && a.Address == s
}

// msgID reports whether s is exactly one valid "<left@right>" msg-id.
func msgID(s string) bool {
	ids := model.ParseMsgIDs(s)
	return len(ids) == 1 && ids[0] == s
}

func bare(id string) string { return strings.TrimSuffix(strings.TrimPrefix(id, "<"), ">") }
