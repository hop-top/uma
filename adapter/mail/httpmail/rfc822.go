package httpmail

import (
	"bytes"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/adapter/mail/stage"
	"hop.top/uma/model"
)

// decodeRFC822 reads a FormatRFC822 body: the message itself, parsed by
// package mime like the mail the other adapters receive. The recipients are
// the address list of Config.RecipientHeader on the request, else the To and
// Cc fields of the message.
func (i *Instance) decodeRFC822(r *http.Request, body []byte) (inbound, error) {
	msg, err := mime.Parse(bytes.NewReader(body), stage.Keep(mime.Limits{MaxTextBytes: model.DefaultContentLimits().MaxTextBytes}))
	if err != nil {
		if errors.Is(err, mime.ErrMalformed) {
			return inbound{}, malformed
		}
		return inbound{}, refusal{http.StatusServiceUnavailable}
	}
	h := msg.Header
	if len(h.From) == 0 {
		return inbound{}, malformed
	}
	sender, err := model.NormalizeEmail(h.From[0].Address)
	if err != nil {
		return inbound{}, malformed
	}
	m := inbound{
		format: FormatRFC822, raw: body, sender: sender,
		text: msg.Text, truncated: msg.TextTruncated, fromHTML: msg.FromHTML,
		header: headersRecord{
			From: h.From, Sender: h.Sender, ReplyTo: h.ReplyTo, To: capAddrs(h.To), Cc: capAddrs(h.Cc),
			Subject: cut(h.Subject, maxFieldBytes), MessageID: cut(h.MessageID, maxFieldBytes),
			InReplyTo: cut(h.InReplyTo, maxFieldBytes), References: cut(h.References, maxFieldBytes),
			ListID: cut(h.ListID, maxFieldBytes), AutoSubmitted: cut(h.AutoSubmitted, 255),
		},
		occurred: h.Date.UTC(),
	}
	if !h.Date.IsZero() {
		m.header.Date = h.Date.UTC().Format(time.RFC3339)
	}
	m.evidence = i.trust(claimsOf(h), sender, h.Values("Authentication-Results"))
	recipients := append(append([]mime.Address(nil), h.To...), h.Cc...)
	if name := i.cfg.RecipientHeader; name != "" {
		if recipients, err = envelopeRecipients(r.Header.Values(name)); err != nil {
			return inbound{}, malformed
		}
	}
	if len(recipients) == 0 || len(recipients) > maxRecipients {
		return inbound{}, malformed
	}
	for _, a := range recipients {
		m.to = append(m.to, a.Address)
	}
	m.attachments, m.omitted = describeParts(msg.Attachments)
	m.kept = keptParts(msg, len(m.attachments))
	return m, nil
}

// envelopeRecipients reads the address lists of the request's recipient
// header fields.
func envelopeRecipients(values []string) ([]mime.Address, error) {
	var out []mime.Address
	for _, v := range values {
		if strings.TrimSpace(v) == "" {
			continue
		}
		as, err := mail.ParseAddressList(v)
		if err != nil {
			return nil, err
		}
		for _, a := range as {
			out = append(out, mime.Address{Name: a.Name, Address: a.Address})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no recipient")
	}
	return out, nil
}

// describeParts describes the attachments parsed from a message, at most
// maxAttachments; the rest are counted.
func describeParts(atts []mime.Attachment) ([]attachmentRecord, int) {
	var out []attachmentRecord
	for _, a := range atts {
		if len(out) == maxAttachments {
			return out, len(atts) - maxAttachments
		}
		out = append(out, attachmentRecord{
			Filename: cut(a.Filename, 1024), MediaType: cut(a.MediaType, 255), Size: a.Size,
			Digest: a.Digest, ContentID: cut(a.ContentID, 1024), Inline: a.Inline,
		})
	}
	return out, 0
}

// keptParts is the part of msg that is described, with the bytes mime.Parse
// kept for it: what staging may hand to the sink, so that every attachment
// staged has a description saying so. Nil when no bytes were kept.
func keptParts(msg *mime.Message, described int) *mime.Message {
	n := min(described, len(msg.Data))
	if n == 0 {
		return nil
	}
	return &mime.Message{Attachments: msg.Attachments[:n], Data: msg.Data[:n]}
}
