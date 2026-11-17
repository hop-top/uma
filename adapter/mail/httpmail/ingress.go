package httpmail

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
	"unicode/utf8"

	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/model"
)

// Payload bounds: the metadata must stay within the structured payload
// limit, so long header fields are cut and lists are capped.
const (
	maxFieldBytes  = 16 << 10
	maxAddrs       = 50
	maxAttachments = 50
)

// inbound is one message as a request carried it, whatever its format.
type inbound struct {
	format string
	// sender is the normalized From address; from, to, cc and replyTo are
	// the header fields as recorded.
	sender string
	header headersRecord
	// to lists every recipient address that can select a destination.
	to []string
	// raw is the request body: the identity of a message without a msg-id.
	raw         []byte
	text        string
	truncated   bool
	fromHTML    bool
	attachments []attachmentRecord
	omitted     int
	// kept holds the bytes of the described attachments that may be
	// staged, index for index with attachments; nil when there are none.
	kept *mime.Message
	// claimedAttachments marks descriptions the sender wrote in its
	// payload, as opposed to parts parsed from a message.
	claimedAttachments bool
	occurred           time.Time
	evidence           evidence
}

// messageRecord is the MessageSchema payload.
type messageRecord struct {
	// Format is the body format the message arrived in.
	Format string `json:"format"`
	// Recipients are the addresses of the message that selected this
	// destination.
	Recipients         []string      `json:"recipients,omitempty"`
	Headers            headersRecord `json:"headers"`
	Text               textRecord    `json:"text"`
	AttachmentsClaimed bool          `json:"attachments_claimed,omitempty"`
	Evidence           evidence      `json:"evidence"`
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

// partsRecord is the PartsSchema content part: the attachments described,
// staged or not. Omitted counts the descriptions beyond the cap.
type partsRecord struct {
	Attachments []attachmentRecord `json:"attachments"`
	Omitted     int                `json:"omitted,omitempty"`
}

// attachmentRecord describes one attachment. A message parsed from RFC 5322
// fills Digest, ContentID and Inline from the bytes; a JSON description
// fills Kind and URL from what the sender wrote: a claim, never fetched.
// A JSON entry with embedded bytes has its Size and Digest taken from them.
type attachmentRecord struct {
	Filename  string `json:"filename,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Digest    string `json:"digest,omitempty"`
	ContentID string `json:"content_id,omitempty"`
	Inline    bool   `json:"inline,omitempty"`
	// Staged reports that the bytes were handed to the engine's attachment
	// sink and the message's content names them in an attachment part.
	Staged bool   `json:"staged,omitempty"`
	Kind   string `json:"kind,omitempty"`
	URL    string `json:"url,omitempty"`
}

// ingress builds the sink input of msg for one destination.
func (i *Instance) ingress(msg inbound, d destination) (model.Ingress, error) {
	h := msg.header
	norm := model.EmailHeaders{
		MessageID: h.MessageID, InReplyTo: h.InReplyTo, References: h.References,
		ListID: h.ListID, AutoSubmitted: h.AutoSubmitted,
	}.Normalize(i.ch.Ref)
	kind := model.EnvelopeMessage
	if norm.Automated {
		kind = model.EnvelopeAutomatedMessage
	}
	content, err := msg.content()
	if err != nil {
		return model.Ingress{}, err
	}
	payload, err := msg.payload(d)
	if err != nil {
		return model.Ingress{}, err
	}
	occurred := msg.occurred.UTC()
	if msg.occurred.IsZero() {
		occurred = i.deps.Clock.Now().UTC()
	}
	env := model.Envelope{
		EventID:     eventID(d.name, msg.raw),
		MessageID:   norm.MessageID,
		Destination: d.name,
		Kind:        kind,
		Sender:      model.ExternalRef{Channel: i.ch.Ref, Namespace: Namespace, Kind: RouteKind, ID: msg.sender},
		Assurance:   msg.evidence.assurance(),
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
// message to two destinations of a channel lands once in each, a redelivery
// of the same bytes to the same destination never twice. A Message-ID does
// not identify a message here. The sender chooses it and anyone can repeat
// it, so letting it decide would let a forged message that reuses the id of
// a genuine one suppress the genuine one. Two messages sharing an id but not
// bytes are two messages; the id still threads.
func eventID(destination string, raw []byte) string {
	sum := sha256.Sum256(raw)
	return "rcpt:" + destination + ";sha256:" + hex.EncodeToString(sum[:])
}

// content is the body text and, when the message has attachments or no
// text, a PartsSchema part describing them.
func (m inbound) content() (model.Content, error) {
	var c model.Content
	if m.text != "" {
		c.Parts = append(c.Parts, model.Part{Kind: model.PartText, Text: m.text})
	}
	if len(m.attachments) > 0 || m.text == "" {
		atts := m.attachments
		if atts == nil {
			atts = []attachmentRecord{}
		}
		data, err := json.Marshal(partsRecord{Attachments: atts, Omitted: m.omitted})
		if err != nil {
			return c, err
		}
		c.Parts = append(c.Parts, model.Part{Kind: model.PartStructured, Structured: &model.Extension{Name: PartsSchema, Version: PartsVersion, Data: data}})
	}
	return c, c.Validate()
}

// payload is the MessageSchema metadata of m at destination d.
func (m inbound) payload(d destination) (model.Extension, error) {
	rec := messageRecord{
		Format:             m.format,
		Recipients:         d.recipients,
		Headers:            m.header,
		Text:               textRecord{FromHTML: m.fromHTML, Truncated: m.truncated},
		AttachmentsClaimed: m.claimedAttachments && len(m.attachments) > 0,
		Evidence:           m.evidence,
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
