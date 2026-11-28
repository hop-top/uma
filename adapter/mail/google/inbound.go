package google

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/adapter/mail/stage"
	"hop.top/uma/model"
)

// assuranceUnverified is the source assurance of inbound mail: the adapter
// reads the From header; Gmail's own authentication results are recorded
// as found, not verified.
const assuranceUnverified = "unverified"

// maxAttachmentParts leaves room for the text part and the staged
// attachments under the default part limit.
const maxAttachmentParts = 64 - 1 - stage.MaxAttachments

// Payload field bounds.
const (
	maxFieldBytes = 16 << 10
	maxAddrs      = 50
)

// payloadHeaders is the headers record of the payload: the header values a
// host reads, as the other mail adapters record them under headers.
type payloadHeaders struct {
	// AutoSubmitted is the Auto-Submitted header (RFC 3834) as found, empty
	// when absent, so a host can tell automated mail apart.
	AutoSubmitted string `json:"auto_submitted,omitempty"`
}

// messagePayload is PayloadMessage version 1.
type messagePayload struct {
	Headers payloadHeaders `json:"headers"`
	Subject string         `json:"subject,omitempty"`
	// ThreadID is Gmail's thread: a provider hint, never conversation
	// identity.
	ThreadID          string   `json:"thread_id,omitempty"`
	LabelIDs          []string `json:"label_ids,omitempty"`
	InternetMessageID string   `json:"internet_message_id,omitempty"`
	To                []string `json:"to,omitempty"`
	Cc                []string `json:"cc,omitempty"`
	ReplyTo           []string `json:"reply_to,omitempty"`
	// ReceivedAt is Gmail's internalDate.
	ReceivedAt time.Time `json:"received_at,omitzero"`
	// AuthenticationResults is the receiving server's
	// Authentication-Results header, recorded as found, not verified.
	AuthenticationResults string `json:"authentication_results,omitempty"`
	// ListUnsubscribe and ListUnsubscribePost are the message's
	// List-Unsubscribe (RFC 2369) and List-Unsubscribe-Post (RFC 8058)
	// header values as found, so a host can tell that an unsubscribe is
	// possible. They decide nothing: the unsubscribe effect reads the
	// headers again from Gmail. A message too large to fetch raw carries
	// neither.
	ListUnsubscribe     string `json:"list_unsubscribe,omitempty"`
	ListUnsubscribePost string `json:"list_unsubscribe_post,omitempty"`
	TextFromHTML        bool   `json:"text_from_html,omitempty"`
	TextTruncated       bool   `json:"text_truncated,omitempty"`
	AttachmentsOmitted  int    `json:"attachments_omitted,omitempty"`
	// BodyOmitted reports a message over the size bound, delivered from
	// its headers alone: no text, and no attachment described or staged.
	BodyOmitted bool `json:"body_omitted,omitempty"`
	// SizeEstimate is Gmail's size estimate in bytes of a message whose
	// body was omitted.
	SizeEstimate int64 `json:"size_estimate,omitempty"`
	// PartsOmitted reports a message past a MIME structure limit (parts or
	// nesting): delivered with the header, and the text and parts read
	// before the limit.
	PartsOmitted bool `json:"parts_omitted,omitempty"`
}

// attachmentPart is PartAttachment version 1.
type attachmentPart struct {
	Filename  string `json:"filename,omitempty"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
	Digest    string `json:"digest"`
	ContentID string `json:"content_id,omitempty"`
	Inline    bool   `json:"inline,omitempty"`
	// Staged reports that the bytes were staged through the engine and the
	// content names them in an attachment part.
	Staged bool `json:"staged,omitempty"`
}

// fetchIngress reads message id in raw form and maps it to an ingress,
// staging its attachments through sink first. The raw form is the whole
// RFC 822 message, attachments included
// (https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.messages,
// Message.raw), so no users.messages.attachments.get call is needed. skip
// reports a message that is gone, a draft or sent by the mailbox: nothing
// to accept. A message whose raw answer is over the bound is taken by its
// headers alone (fetchHeaderOnly).
//
// A message Gmail answered whole whose raw form is not base64url or does
// not parse is errUnreadable: Gmail never changes a message's bytes, so no
// repeat reads it, and the round skips it (see ingest). An answer that is not
// the message asked for, whole (not JSON, truncated, without the message's
// ID or raw form), is a fault of the response, never of the message: it is
// a transient error, so the cursor stays before the message and the round
// reads it again. err may also be a staging failure a repeat of the fetch
// may fix.
func (i *instance) fetchIngress(ctx context.Context, s *session, sink adapter.Sink, id string) (in model.Ingress, skip bool, err error) {
	resp, err := s.do(ctx, request{
		method: "GET", url: i.c.api("/messages/"+url.PathEscape(id), url.Values{"format": {"raw"}}),
		limit: i.c.rawMax(),
	})
	if model.IsCode(err, model.NotFound) {
		return in, true, nil
	}
	if tooLarge(err) {
		// The message cannot be taken whole; the mailbox goes on without
		// its body.
		return i.fetchHeaderOnly(ctx, s, id)
	}
	if err != nil {
		return in, false, transient(err)
	}
	m, err := decodeMessage(resp.body, id)
	if err != nil {
		return in, false, err
	}
	if skipLabels(m.LabelIDs) {
		return in, true, nil
	}
	if m.Raw == "" {
		// Every message has bytes: an answer without them is not the raw
		// form asked for.
		return in, false, errUnexpectedAnswer()
	}
	raw, err := decodeRaw(m.Raw)
	if err != nil {
		// The answer is whole JSON naming the message: the encoding is
		// the message's own, and a repeat answers it alike.
		return in, false, errUnreadable
	}
	msg, err := mime.Parse(bytes.NewReader(raw), stage.Keep(mime.Limits{MaxTextBytes: model.DefaultContentLimits().MaxTextBytes}))
	// A message past a structure limit comes with what was read before it
	// and is delivered as such, the payload saying so.
	if err != nil && !errors.Is(err, mime.ErrLimit) {
		if errors.Is(err, mime.ErrMalformed) {
			// The message cannot be read; it stays in the mailbox.
			return in, false, errUnreadable
		}
		return in, false, model.NewError(model.Transient, "gmail raw message could not be read")
	}
	dest := i.destination(msg.Header)
	// The Gmail message ID is immutable and, with the destination, scopes
	// the keys of its attachments.
	parts, err := stage.Stage(ctx, sink, stage.Request{Key: dest + "\x00" + m.ID, Destination: dest, Audience: model.Audience{Visibility: model.Public}}, msg)
	if err != nil {
		return in, false, transient(err)
	}
	return i.normalize(m, msg, parts, false), false, nil
}

// headerFields are the header fields read of a message too large to fetch
// raw: the ones the raw path reads from it.
var headerFields = []string{
	"From", "Sender", "Reply-To", "To", "Cc", "Subject", "Date", "Message-ID",
	"In-Reply-To", "References", "List-Id", "Auto-Submitted", "Authentication-Results",
}

// fetchHeaderOnly reads message id with format=metadata, for a message whose
// raw form is over the response bound, and maps it to an ingress without
// body or attachments: body_omitted and size_estimate tell the receiver of
// the message. It is no failure: the cursor moves past the message once the
// sink accepted it. A read that a repeat can fix is an error like any
// fetch's, and the round meets the message again.
func (i *instance) fetchHeaderOnly(ctx context.Context, s *session, id string) (in model.Ingress, skip bool, err error) {
	resp, err := s.do(ctx, request{
		method: "GET", url: i.c.api("/messages/"+url.PathEscape(id), url.Values{"format": {"metadata"}, "metadataHeaders": headerFields}),
	})
	if model.IsCode(err, model.NotFound) {
		return in, true, nil
	}
	if err != nil {
		return in, false, transient(err)
	}
	m, err := decodeMessage(resp.body, id)
	if err != nil {
		return in, false, err
	}
	if skipLabels(m.LabelIDs) {
		return in, true, nil
	}
	if m.Payload == nil {
		// A metadata answer carries the payload, headers and all.
		return in, false, errUnexpectedAnswer()
	}
	var block strings.Builder
	for _, h := range m.Payload.Headers {
		if h.Name == "" || strings.ContainsAny(h.Name, ":\r\n ") {
			continue
		}
		// A header value is one line.
		block.WriteString(h.Name + ": " + strings.NewReplacer("\r", " ", "\n", " ").Replace(h.Value) + "\r\n")
	}
	block.WriteString("\r\n")
	msg, err := mime.Parse(strings.NewReader(block.String()), mime.Limits{})
	if err != nil {
		if errors.Is(err, mime.ErrMalformed) {
			return in, false, errUnreadable
		}
		return in, false, model.NewError(model.Transient, "gmail message headers could not be read")
	}
	return i.normalize(m, msg, nil, true), false, nil
}

// decodeMessage decodes a users.messages.get answer for message id. A body
// that is not JSON, or JSON naming no message or another one, is not the
// answer asked for: a proxy's error page, a truncated or garbled body. That
// is a fault of the response, which a repeat may not meet, and never a
// reason to skip the message: errUnexpectedAnswer.
func decodeMessage(b []byte, id string) (gMessage, error) {
	var m gMessage
	if json.Unmarshal(b, &m) != nil || m.ID != id {
		return gMessage{}, errUnexpectedAnswer()
	}
	return m, nil
}

// errUnreadable reports a message Gmail answered whole that can never be
// read: its raw form is not base64url or does not parse. It never leaves the
// adapter: the round skips the message and moves the cursor on.
var errUnreadable = errors.New("gmail: message unreadable")

// errUnexpectedAnswer is the transient failure of a read answered with a
// body that is not, whole, what was asked for: the message, the page, the
// profile.
func errUnexpectedAnswer() error {
	return model.NewError(model.Transient, "gmail answered an unexpected body")
}

// decodeRaw decodes Message.raw: base64url, with or without padding.
func decodeRaw(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// normalize maps a Gmail message and its parsed raw form to an ingress; the
// attachment parts of the attachments staged follow the descriptors.
// Threading evidence comes from the RFC 5322 headers only (Message-ID,
// In-Reply-To, References, List-Id); threadId is recorded in the payload as
// a provider hint and never selects a conversation.
func (i *instance) normalize(m gMessage, msg *mime.Message, staged []model.Part, bodyOmitted bool) model.Ingress {
	h := msg.Header
	norm := model.EmailHeaders{
		MessageID: h.MessageID, InReplyTo: h.InReplyTo, References: h.References,
		ListID: h.ListID, AutoSubmitted: h.AutoSubmitted,
	}.Normalize(i.ch.Ref)

	received := time.UnixMilli(int64(m.InternalDate)).UTC()
	if m.InternalDate == 0 {
		received = h.Date.UTC()
	}
	env := model.Envelope{
		MessageID:   m.ID,
		Destination: i.destination(h),
		Kind:        model.EnvelopeMessage,
		Assurance:   assuranceUnverified,
		References:  norm.References,
		InReplyTo:   norm.InReplyTo,
		Audience:    model.Audience{Visibility: model.Public},
		OccurredAt:  received,
	}
	if norm.Automated {
		env.Kind = model.EnvelopeAutomatedMessage
	}
	if from := author(h); from != "" {
		// ParentID is the Gmail message the address wrote: a reply route
		// stored from it anchors replies on that message.
		env.Sender = model.ExternalRef{Channel: i.ch.Ref, Namespace: IdentityNamespace, Kind: refKindAddress, ID: from, ParentID: m.ID}
	} else {
		env.SystemActor = true
	}

	p := messagePayload{
		Headers: payloadHeaders{AutoSubmitted: cut(h.AutoSubmitted, 255)},
		Subject: cut(h.Subject, maxFieldBytes), ThreadID: m.ThreadID, LabelIDs: m.LabelIDs,
		InternetMessageID: cut(h.MessageID, maxFieldBytes),
		To:                addresses(h.To), Cc: addresses(h.Cc), ReplyTo: addresses(h.ReplyTo),
		ReceivedAt: received, TextFromHTML: msg.FromHTML, TextTruncated: msg.TextTruncated, PartsOmitted: msg.PartsOmitted,
	}
	if bodyOmitted {
		p.BodyOmitted, p.SizeEstimate = true, int64(m.SizeEstimate)
	}
	if ar := h.Values("Authentication-Results"); len(ar) > 0 {
		p.AuthenticationResults = cut(ar[0], maxFieldBytes)
	}
	if v := h.Values("List-Unsubscribe"); len(v) > 0 {
		p.ListUnsubscribe = cut(v[0], maxFieldBytes)
	}
	if v := h.Values("List-Unsubscribe-Post"); len(v) > 0 {
		p.ListUnsubscribePost = cut(v[0], maxFieldBytes)
	}
	if msg.Text != "" {
		env.Content.Parts = append(env.Content.Parts, model.Part{Kind: model.PartText, Text: msg.Text})
	}
	for n, a := range msg.Attachments {
		if n == maxAttachmentParts {
			p.AttachmentsOmitted = len(msg.Attachments) - n
			break
		}
		data, _ := json.Marshal(attachmentPart{
			Filename: cut(a.Filename, 1024), MediaType: cut(a.MediaType, 255), Size: a.Size,
			Digest: a.Digest, ContentID: cut(a.ContentID, 1024), Inline: a.Inline, Staged: a.Staged,
		})
		env.Content.Parts = append(env.Content.Parts, model.Part{
			Kind: model.PartStructured, Structured: &model.Extension{Name: PartAttachment, Version: 1, Data: data},
		})
	}
	env.Content.Parts = append(env.Content.Parts, staged...)
	data, _ := json.Marshal(p)
	env.Payload = model.Extension{Name: PayloadMessage, Version: 1, Data: data}
	if len(env.Content.Parts) == 0 {
		// No body and no attachment: the payload is the content.
		env.Content.Parts = []model.Part{{Kind: model.PartStructured, Structured: &model.Extension{Name: PayloadMessage, Version: 1, Data: data}}}
	}
	return model.Ingress{Envelope: env}
}

// author is the address that wrote the message: From, else Sender, as an
// identity key (see identityAddress).
func author(h mime.Header) string {
	for _, l := range [][]mime.Address{h.From, h.Sender} {
		if len(l) > 0 && l[0].Address != "" {
			return identityAddress(l[0].Address)
		}
	}
	return ""
}

// identityAddress is addr as an identity key: its domain lowercased and its
// local part byte-exact (model.NormalizeEmail), the spelling
// Factory.NormalizeRecipient gives a start's recipient. An address that
// cannot be normalized, such as one with a quoted local part the parser
// unquoted, is keyed as given: the adapter never invents a spelling.
func identityAddress(addr string) string {
	if n, err := model.NormalizeEmail(addr); err == nil {
		return n
	}
	return addr
}

// destination is the first configured address among the To and Cc
// recipients (case-insensitive), else the primary address, else the
// mailbox address, else the user.
func (i *instance) destination(h mime.Header) string {
	for _, r := range slices.Concat(h.To, h.Cc) {
		for _, a := range i.cfg.Addresses {
			if strings.EqualFold(r.Address, a) {
				return a
			}
		}
	}
	switch {
	case len(i.cfg.Addresses) > 0:
		return i.cfg.Addresses[0]
	case i.cfg.Address != "":
		return i.cfg.Address
	}
	return i.cfg.userID()
}

func addresses(as []mime.Address) []string {
	var out []string
	for _, a := range as {
		if a.Address != "" && len(out) < maxAddrs {
			out = append(out, cut(a.Address, 320))
		}
	}
	return out
}

// cut bounds s to n bytes on a rune boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
