package smtp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"strings"
	"time"
	"unicode/utf8"

	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/model"
)

// Metadata bounds: the payload must stay within the structured payload
// limit, so long raw fields are cut and the ARC record is capped.
const (
	maxFieldBytes = 16 << 10
	maxARCFields  = 30
)

// transaction is one received message and what the SMTP session saw.
type transaction struct {
	mailFrom string
	helo     string
	clientIP net.IP
	tls      bool
	rcpts    []rcpt
	sender   string
	raw      []byte
	msg      *mime.Message
	ev       evidence
}

// messageRecord is the MessageSchema payload.
type messageRecord struct {
	MailFrom              string        `json:"mail_from"`
	Helo                  string        `json:"helo"`
	ClientIP              string        `json:"client_ip"`
	TLS                   bool          `json:"tls"`
	Recipients            []string      `json:"recipients,omitempty"`
	Headers               headersRecord `json:"headers"`
	AuthenticationResults string        `json:"authentication_results"`
	Evidence              evidence      `json:"evidence"`
	Text                  textRecord    `json:"text"`
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
}

// partsRecord is the PartsSchema content part: MIME parts other than the
// body text, described, not stored.
type partsRecord struct {
	Attachments []mime.Attachment `json:"attachments"`
}

// ingress builds the sink input of tx for one destination.
func (i *Instance) ingress(tx transaction, destination string) (model.Ingress, error) {
	h := tx.msg.Header
	norm := model.EmailHeaders{
		MessageID: h.MessageID, InReplyTo: h.InReplyTo, References: h.References,
		ListID: h.ListID, AutoSubmitted: h.AutoSubmitted,
	}.Normalize(i.ch.Ref)
	kind := model.EnvelopeMessage
	if norm.Automated || tx.mailFrom == "" {
		kind = model.EnvelopeAutomatedMessage
	}
	content, err := content(tx.msg)
	if err != nil {
		return model.Ingress{}, err
	}
	payload, err := i.payload(tx, destination)
	if err != nil {
		return model.Ingress{}, err
	}
	occurred := h.Date.UTC()
	if h.Date.IsZero() {
		occurred = i.deps.Clock.Now().UTC()
	}
	env := model.Envelope{
		EventID:     eventID(destination, tx.raw),
		MessageID:   norm.MessageID,
		Destination: destination,
		Kind:        kind,
		Sender:      model.ExternalRef{Channel: i.ch.Ref, Namespace: Namespace, Kind: RouteKind, ID: tx.sender},
		Assurance:   tx.ev.assurance(),
		References:  norm.References,
		InReplyTo:   norm.InReplyTo,
		Content:     content,
		Audience:    model.Audience{Visibility: model.Public},
		OccurredAt:  occurred,
		Payload:     payload,
	}
	return model.Ingress{Envelope: env}, nil
}

// eventID scopes deduplication to the destination and the bytes: one
// message to two destinations lands once in each, and a redelivery of the
// same DATA to the same destination never twice. A Message-ID does not
// identify a message here. The sender chooses it and anyone can repeat it,
// so letting it decide would let a forged message that reuses the id of a
// genuine one suppress the genuine one. Two messages sharing an id but not
// bytes are two messages; the id still threads.
//
// The digest covers the DATA exactly as the client sent it: an MTA retries
// the bytes it queued, so a retry reproduces it. A client that composes the
// message afresh on each attempt (a new Date or Message-ID) sends a new
// message by construction.
func eventID(destination string, raw []byte) string {
	sum := sha256.Sum256(raw)
	return "rcpt:" + destination + ";sha256:" + hex.EncodeToString(sum[:])
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
		data, err := json.Marshal(partsRecord{Attachments: atts})
		if err != nil {
			return c, err
		}
		c.Parts = append(c.Parts, model.Part{Kind: model.PartStructured, Structured: &model.Extension{Name: PartsSchema, Version: PartsVersion, Data: data}})
	}
	return c, c.Validate()
}

// payload is the MessageSchema metadata of tx at destination.
func (i *Instance) payload(tx transaction, destination string) (model.Extension, error) {
	h := tx.msg.Header
	var rcpts []string
	for _, r := range tx.rcpts {
		if r.destination == destination {
			rcpts = append(rcpts, r.address)
		}
	}
	rec := messageRecord{
		MailFrom: cut(tx.mailFrom, maxFieldBytes), Helo: cut(tx.helo, 255), TLS: tx.tls,
		Recipients: rcpts,
		Headers: headersRecord{
			From: h.From, Sender: h.Sender, ReplyTo: h.ReplyTo, To: capAddrs(h.To), Cc: capAddrs(h.Cc),
			Subject: cut(h.Subject, maxFieldBytes), MessageID: cut(h.MessageID, maxFieldBytes),
			InReplyTo: cut(h.InReplyTo, maxFieldBytes), References: cut(h.References, maxFieldBytes),
			ListID: cut(h.ListID, maxFieldBytes), AutoSubmitted: cut(h.AutoSubmitted, 255),
		},
		AuthenticationResults: tx.ev.authenticationResults(i.cfg.Hostname, tx.mailFrom, tx.helo),
		Evidence:              tx.ev,
		Text:                  textRecord{FromHTML: tx.msg.FromHTML, Truncated: tx.msg.TextTruncated},
	}
	if tx.clientIP != nil {
		rec.ClientIP = tx.clientIP.String()
	}
	if !h.Date.IsZero() {
		rec.Headers.Date = h.Date.UTC().Format(time.RFC3339)
	}
	if len(rec.Evidence.ARC.Fields) > maxARCFields {
		rec.Evidence.ARC.Fields = rec.Evidence.ARC.Fields[:maxARCFields]
	}
	arc := make([]mime.Field, len(rec.Evidence.ARC.Fields))
	for j, f := range rec.Evidence.ARC.Fields {
		arc[j] = mime.Field{Key: f.Key, Value: cut(f.Value, maxFieldBytes/4)}
	}
	rec.Evidence.ARC.Fields = arc
	data, err := json.Marshal(rec)
	if err != nil {
		return model.Extension{}, err
	}
	x := model.Extension{Name: MessageSchema, Version: MessageVersion, Data: data}
	return x, x.Validate()
}

const maxAddrs = 50

func capAddrs(a []mime.Address) []mime.Address {
	if len(a) > maxAddrs {
		return a[:maxAddrs]
	}
	return a
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

func equalFold(a, b string) bool { return strings.EqualFold(a, b) }
