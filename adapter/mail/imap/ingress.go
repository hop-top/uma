package imap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mime"
	mailstage "hop.top/uma/adapter/mail/stage"
	"hop.top/uma/model"
)

// Metadata bounds: the payload must stay within the structured payload
// limit, so long raw fields are cut.
const (
	maxFieldBytes = 16 << 10
	maxAddrs      = 50
)

// fetched is one message as the mailbox holds it.
type fetched struct {
	uid          uint32
	uidValidity  uint32
	internalDate time.Time
	size         int64
	raw          []byte
	// headerOnly reports that raw is the header alone: the message exceeds
	// Config.MaxMessageBytes.
	headerOnly bool
}

// messageRecord is the MessageSchema payload.
type messageRecord struct {
	Mailbox      string        `json:"mailbox"`
	UIDValidity  uint32        `json:"uid_validity"`
	UID          uint32        `json:"uid"`
	InternalDate string        `json:"internal_date,omitempty"`
	Size         int64         `json:"size"`
	Headers      headersRecord `json:"headers"`
	Text         textRecord    `json:"text"`
	// PartsOmitted reports a message past a MIME structure limit (parts or
	// nesting): delivered with the header, and the text and parts read
	// before the limit.
	PartsOmitted bool `json:"parts_omitted,omitempty"`
}

type headersRecord struct {
	From          []mime.Address `json:"from,omitempty"`
	Sender        []mime.Address `json:"sender,omitempty"`
	ReplyTo       []mime.Address `json:"reply_to,omitempty"`
	To            []mime.Address `json:"to,omitempty"`
	Cc            []mime.Address `json:"cc,omitempty"`
	Subject       string         `json:"subject,omitempty"`
	Date          string         `json:"date,omitempty"`
	MessageID     string         `json:"message_id,omitempty"`
	InReplyTo     string         `json:"in_reply_to,omitempty"`
	References    string         `json:"references,omitempty"`
	ListID        string         `json:"list_id,omitempty"`
	AutoSubmitted string         `json:"auto_submitted,omitempty"`
}

type textRecord struct {
	FromHTML  bool `json:"from_html,omitempty"`
	Truncated bool `json:"truncated,omitempty"`
	// BodyOmitted reports a message over the size bound, delivered from
	// its header alone.
	BodyOmitted bool `json:"body_omitted,omitempty"`
}

// partsRecord is the PartsSchema content part: MIME parts other than the
// body text, described, not stored.
type partsRecord struct {
	Attachments []mime.Attachment `json:"attachments"`
	// AttachmentsOmitted counts the attachments past the ones described,
	// which are what fits the structured part limit.
	AttachmentsOmitted int `json:"attachments_omitted,omitempty"`
}

// ingress parses f into the sink input, staging the attachments of the
// message through sink first. ok is false for a message that cannot be
// delivered (malformed, or without a usable From address); the receiver
// skips it. err is a staging failure a redelivery may fix.
func (i *Instance) ingress(ctx context.Context, sink adapter.Sink, f fetched) (in model.Ingress, ok bool, err error) {
	msg, err := mime.Parse(bytes.NewReader(f.raw), mailstage.Keep(mime.Limits{MaxTextBytes: model.DefaultContentLimits().MaxTextBytes}))
	// A message past a structure limit comes with what was read before it,
	// and is delivered as such.
	if err != nil && !errors.Is(err, mime.ErrLimit) || len(msg.Header.From) == 0 {
		return model.Ingress{}, false, nil
	}
	sender, err := model.NormalizeEmail(msg.Header.From[0].Address)
	if err != nil {
		return model.Ingress{}, false, nil
	}
	h := msg.Header
	norm := model.EmailHeaders{
		MessageID: h.MessageID, InReplyTo: h.InReplyTo, References: h.References,
		ListID: h.ListID, AutoSubmitted: h.AutoSubmitted,
	}.Normalize(i.ch.Ref)
	kind := model.EnvelopeMessage
	if norm.Automated {
		kind = model.EnvelopeAutomatedMessage
	}
	dest := i.cfg.destination()
	eid := eventID(dest, f)
	parts, err := mailstage.Stage(ctx, sink, mailstage.Request{Key: eid, Destination: dest, Audience: model.Audience{Visibility: model.Public}}, msg)
	if err != nil {
		return model.Ingress{}, false, err
	}
	c, err := content(msg)
	if err != nil {
		return model.Ingress{}, false, nil
	}
	c.Parts = append(c.Parts, parts...)
	if c.Validate() != nil {
		return model.Ingress{}, false, nil
	}
	payload, err := i.payload(f, msg)
	if err != nil {
		return model.Ingress{}, false, nil
	}
	occurred := h.Date.UTC()
	if h.Date.IsZero() {
		occurred = f.internalDate.UTC()
	}
	if occurred.IsZero() {
		occurred = i.now().UTC()
	}
	return model.Ingress{Envelope: model.Envelope{
		EventID:     eid,
		MessageID:   norm.MessageID,
		Destination: dest,
		Kind:        kind,
		Sender:      model.ExternalRef{Channel: i.ch.Ref, Namespace: Namespace, Kind: RouteKind, ID: sender},
		References:  norm.References,
		InReplyTo:   norm.InReplyTo,
		Content:     c,
		Audience:    model.Audience{Visibility: model.Public},
		OccurredAt:  occurred,
		Payload:     payload,
	}}, true, nil
}

// eventID is the deduplication identity of a message: its destination and a
// digest of the bytes fetched. A Message-ID does not identify a message
// here. The sender chooses it and anyone who can mail the mailbox can repeat
// it, so letting it decide would let a forged mail that reuses the id of a
// genuine one, and arrives first, suppress the genuine one as a redelivery.
// Two mails sharing an id but not bytes are two messages; the id still
// threads.
//
// A UID never enters the key: after a UIDVALIDITY change, or a copy, the same
// message has a new UID and must still deduplicate. A mailbox does not change
// the text of a message it holds, so the same message fetched again, under
// any UID, reproduces the bytes.
//
// A message over Config.MaxMessageBytes is fetched as its header block alone,
// so the digest covers the header block and the key says so and carries the
// RFC822.SIZE the server reports: a header-only message never equals one
// whose whole text is that block, and two with one block but another size
// differ. Same block and same size with another body are not told apart
// without fetching the body; the block holds the headers the receiving server
// added (Received), which a sender cannot predict.
func eventID(destination string, f fetched) string {
	sum := sha256.Sum256(f.raw)
	if f.headerOnly {
		return "mailbox:" + destination + ";header-sha256:" + hex.EncodeToString(sum[:]) + ";size:" + strconv.FormatInt(f.size, 10)
	}
	return "mailbox:" + destination + ";sha256:" + hex.EncodeToString(sum[:])
}

// content is the body text and, when the message has other parts or no
// text, a PartsSchema part describing them.
func content(m *mime.Message) (model.Content, error) {
	var c model.Content
	if m.Text != "" {
		c.Parts = append(c.Parts, model.Part{Kind: model.PartText, Text: m.Text})
	}
	if len(m.Attachments) > 0 || m.Text == "" {
		atts := make([]mime.Attachment, len(m.Attachments))
		copy(atts, m.Attachments)
		for i := range atts {
			atts[i].Filename = cut(atts[i].Filename, 1024)
			atts[i].MediaType = cut(atts[i].MediaType, 255)
			atts[i].ContentID = cut(atts[i].ContentID, 1024)
		}
		rec, err := fitParts(atts)
		if err != nil {
			return c, err
		}
		data, err := json.Marshal(rec)
		if err != nil {
			return c, err
		}
		c.Parts = append(c.Parts, model.Part{Kind: model.PartStructured, Structured: &model.Extension{Name: PartsSchema, Version: PartsVersion, Data: data}})
	}
	return c, c.Validate()
}

// fitParts is the descriptors of atts within the structured part limit: the
// most that fit, in order, and the count of the others.
func fitParts(atts []mime.Attachment) (partsRecord, error) {
	limit := model.DefaultContentLimits().MaxStructuredBytes
	var firstErr error
	n := sort.Search(len(atts)+1, func(n int) bool {
		b, err := json.Marshal(partsRecord{Attachments: atts[:n], AttachmentsOmitted: len(atts) - n})
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return len(b) > limit
	})
	if firstErr != nil {
		return partsRecord{}, firstErr
	}
	// n is the first count that does not fit; the one before it does.
	n = max(n-1, 0)
	return partsRecord{Attachments: atts[:n], AttachmentsOmitted: len(atts) - n}, nil
}

// payload is the MessageSchema metadata of f.
func (i *Instance) payload(f fetched, m *mime.Message) (model.Extension, error) {
	h := m.Header
	rec := messageRecord{
		Mailbox: i.cfg.mailbox(), UIDValidity: f.uidValidity, UID: f.uid, Size: f.size,
		Headers: headersRecord{
			From: capAddrs(h.From), Sender: capAddrs(h.Sender), ReplyTo: capAddrs(h.ReplyTo), To: capAddrs(h.To), Cc: capAddrs(h.Cc),
			Subject: cut(h.Subject, maxFieldBytes), MessageID: cut(h.MessageID, maxFieldBytes),
			InReplyTo: cut(h.InReplyTo, maxFieldBytes), References: cut(h.References, maxFieldBytes),
			ListID: cut(h.ListID, maxFieldBytes), AutoSubmitted: cut(h.AutoSubmitted, 255),
		},
		Text:         textRecord{FromHTML: m.FromHTML, Truncated: m.TextTruncated, BodyOmitted: f.headerOnly},
		PartsOmitted: m.PartsOmitted,
	}
	if !f.internalDate.IsZero() {
		rec.InternalDate = f.internalDate.UTC().Format(time.RFC3339)
	}
	if !h.Date.IsZero() {
		rec.Headers.Date = h.Date.UTC().Format(time.RFC3339)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return model.Extension{}, err
	}
	x := model.Extension{Name: MessageSchema, Version: MessageVersion, Data: data}
	return x, x.Validate()
}

func capAddrs(a []mime.Address) []mime.Address {
	if len(a) > maxAddrs {
		a = a[:maxAddrs]
	}
	out := make([]mime.Address, len(a))
	for i, x := range a {
		out[i] = mime.Address{Name: cut(x.Name, 1024), Address: cut(x.Address, 1024)}
	}
	return out
}

// cut shortens s to at most n bytes at a UTF-8 boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
