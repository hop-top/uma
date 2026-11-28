package microsoft

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/model"
)

// messageSelect is the $select of an inbound message fetch.
// internetMessageHeaders is returned only when selected.
const messageSelect = "id,internetMessageId,conversationId,subject,from,sender,toRecipients,ccRecipients," +
	"replyTo,body,receivedDateTime,hasAttachments,isDraft,internetMessageHeaders"

// assuranceUnverified is the source assurance of inbound mail: Graph
// reports the From header, with no sender authentication result of its own.
const assuranceUnverified = "unverified"

// maxAttachmentParts leaves room for the text part and the staged
// attachments under the default part limit.
const maxAttachmentParts = 64 - 1 - maxStagedAttachments

// maxAttachmentPages is the number of pages of a message's attachment list
// that are read.
const maxAttachmentPages = 20

// payloadHeaders is the headers record of the payload: the header values a
// host reads, as the other mail adapters record them under headers.
type payloadHeaders struct {
	// AutoSubmitted is the Auto-Submitted header (RFC 3834) as Graph reports
	// it in internetMessageHeaders, empty when absent, so a host can tell
	// automated mail apart.
	AutoSubmitted string `json:"auto_submitted,omitempty"`
}

// messagePayload is PayloadMessage version 1.
type messagePayload struct {
	Headers           payloadHeaders `json:"headers"`
	Subject           string         `json:"subject,omitempty"`
	ConversationID    string         `json:"conversation_id,omitempty"`
	InternetMessageID string         `json:"internet_message_id,omitempty"`
	To                []string       `json:"to,omitempty"`
	Cc                []string       `json:"cc,omitempty"`
	ReplyTo           []string       `json:"reply_to,omitempty"`
	ReceivedAt        time.Time      `json:"received_at,omitzero"`
	// AuthenticationResults is the receiving server's
	// Authentication-Results header, recorded as found, not verified.
	AuthenticationResults string `json:"authentication_results,omitempty"`
	// ListUnsubscribe and ListUnsubscribePost are the message's
	// List-Unsubscribe (RFC 2369) and List-Unsubscribe-Post (RFC 8058)
	// header values as Graph reports them in internetMessageHeaders (a
	// repeated header joined by a space), so a host can tell that an
	// unsubscribe is possible. They decide nothing: the unsubscribe effect
	// reads the message again from Graph.
	ListUnsubscribe     string `json:"list_unsubscribe,omitempty"`
	ListUnsubscribePost string `json:"list_unsubscribe_post,omitempty"`
	// TextTruncated reports a body cut to the text part limit.
	TextTruncated bool `json:"text_truncated,omitempty"`
	// AttachmentsOmitted counts attachments beyond the part limit.
	AttachmentsOmitted int `json:"attachments_omitted,omitempty"`
}

// attachmentPart is PartAttachment version 1. The bytes stay with Graph
// under ID unless Staged: then they were staged through the engine and the
// content names them in an attachment part.
type attachmentPart struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size"`
	Inline      bool   `json:"inline,omitempty"`
	Staged      bool   `json:"staged,omitempty"`
}

// fetchIngress reads message id and maps it to an ingress, staging its file
// attachments through sink first. skip reports a message that is gone or
// still a draft: nothing to accept. err may be a staging failure a repeat of
// the fetch may fix.
func (i *instance) fetchIngress(ctx context.Context, s *session, sink adapter.Sink, id string) (in model.Ingress, skip bool, err error) {
	q := url.Values{"$select": {messageSelect}}
	resp, err := s.do(ctx, request{
		method: "GET", url: i.c.api(i.cfg.mailboxPath()+"/messages/"+url.PathEscape(id), q),
		prefer: []string{preferImmutableID, preferTextBody},
	})
	if model.IsCode(err, model.NotFound) {
		return in, true, nil
	}
	if err != nil {
		return in, false, transient(err)
	}
	var m gMessage
	if err := decode(resp.body, &m); err != nil {
		return in, false, err
	}
	if m.IsDraft || m.ID == "" {
		return in, true, nil
	}
	var atts []gAttachment
	if m.HasAttachments {
		if atts, err = i.attachments(ctx, s, m.ID); err != nil {
			return in, false, err
		}
	}
	st, err := i.stageAttachments(ctx, s, sink, m.ID, i.destination(m), atts)
	if err != nil {
		return in, false, err
	}
	return i.normalize(m, atts, st), false, nil
}

// attachments lists a message's attachments, following @odata.nextLink for
// at most maxAttachmentPages pages. Microsoft Learn documents no page size
// for the list (https://learn.microsoft.com/en-us/graph/api/message-list-attachments)
// and says of every collection that "to read all results, you must continue
// to call Microsoft Graph with the @odata.nextLink property" until it is no
// longer returned (https://learn.microsoft.com/en-us/graph/paging). A link to
// another origin is not followed: the bearer token stays with the endpoint.
// A page that fails fails the list, so a partial list is not delivered as
// complete; a message gone meanwhile ends it with what was read.
func (i *instance) attachments(ctx context.Context, s *session, id string) ([]gAttachment, error) {
	q := url.Values{"$select": {"id,name,contentType,size,isInline"}}
	link := i.c.api(i.cfg.mailboxPath()+"/messages/"+url.PathEscape(id)+"/attachments", q)
	var out []gAttachment
	for range maxAttachmentPages {
		resp, err := s.do(ctx, request{method: "GET", url: link, prefer: []string{preferImmutableID}})
		if model.IsCode(err, model.NotFound) {
			return out, nil
		}
		if err != nil {
			return nil, transient(err)
		}
		var page gAttachmentPage
		if err := decode(resp.body, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Value...)
		if page.NextLink == "" || !i.c.sameOrigin(page.NextLink) {
			break
		}
		link = page.NextLink
	}
	return out, nil
}

// normalize maps a Graph message to an ingress. Threading evidence comes
// from the RFC 5322 headers only (Message-ID, In-Reply-To, References,
// List-Id); conversationId is recorded in the payload as a provider hint
// and never selects a conversation.
func (i *instance) normalize(m gMessage, atts []gAttachment, st staged) model.Ingress {
	h := headerMap(m.InternetMessageHeaders)
	eh := model.EmailHeaders{
		MessageID: h["message-id"], InReplyTo: h["in-reply-to"], References: h["references"],
		ListID: h["list-id"], AutoSubmitted: h["auto-submitted"],
	}
	if eh.MessageID == "" {
		eh.MessageID = m.InternetMessageID
	}
	norm := eh.Normalize(i.ch.Ref)

	env := model.Envelope{
		MessageID:   m.ID,
		Destination: i.destination(m),
		Kind:        model.EnvelopeMessage,
		Assurance:   assuranceUnverified,
		References:  norm.References,
		InReplyTo:   norm.InReplyTo,
		Audience:    model.Audience{Visibility: model.Public},
		OccurredAt:  m.ReceivedDateTime,
	}
	if norm.Automated {
		env.Kind = model.EnvelopeAutomatedMessage
	}
	if from := author(m); from != "" {
		// ParentID is the Graph message the address wrote: a per-contact
		// reply route stored from it anchors replies on that message.
		env.Sender = model.ExternalRef{Channel: i.ch.Ref, Namespace: IdentityNamespace, Kind: refKindAddress, ID: from, ParentID: m.ID}
	} else {
		env.SystemActor = true
	}

	p := messagePayload{
		Headers: payloadHeaders{AutoSubmitted: cutBytes(eh.AutoSubmitted, 255)},
		Subject: m.Subject, ConversationID: m.ConversationID, InternetMessageID: m.InternetMessageID,
		To: addresses(m.ToRecipients), Cc: addresses(m.CcRecipients), ReplyTo: addresses(m.ReplyTo),
		ReceivedAt: m.ReceivedDateTime, AuthenticationResults: h["authentication-results"],
		ListUnsubscribe: h["list-unsubscribe"], ListUnsubscribePost: h["list-unsubscribe-post"],
	}
	if m.Body != nil && m.Body.Content != "" {
		text, cut := truncateText(m.Body.Content, model.DefaultContentLimits().MaxTextBytes)
		p.TextTruncated = cut
		env.Content.Parts = append(env.Content.Parts, model.Part{Kind: model.PartText, Text: text})
	}
	for n, a := range atts {
		if n == maxAttachmentParts {
			p.AttachmentsOmitted = len(atts) - n
			break
		}
		data, _ := json.Marshal(attachmentPart{ID: a.ID, Name: mime.SanitizeFilename(a.Name), ContentType: a.ContentType, Size: a.Size, Inline: a.IsInline, Staged: st.has(n)})
		env.Content.Parts = append(env.Content.Parts, model.Part{
			Kind: model.PartStructured, Structured: &model.Extension{Name: PartAttachment, Version: 1, Data: data},
		})
	}
	env.Content.Parts = append(env.Content.Parts, st.parts...)
	data, _ := json.Marshal(p)
	env.Payload = model.Extension{Name: PayloadMessage, Version: 1, Data: data}
	if len(env.Content.Parts) == 0 {
		// No body and no attachment: the payload is the content.
		env.Content.Parts = []model.Part{{Kind: model.PartStructured, Structured: &model.Extension{Name: PayloadMessage, Version: 1, Data: data}}}
	}
	return model.Ingress{Envelope: env}
}

// author is the address that wrote m: from, else sender, as an identity key
// (see identityAddress).
func author(m gMessage) string {
	for _, r := range []*gRecipient{m.From, m.Sender} {
		if r != nil && r.EmailAddress.Address != "" {
			return identityAddress(r.EmailAddress.Address)
		}
	}
	return ""
}

// identityAddress is addr as an identity key: its domain lowercased and its
// local part byte-exact (model.NormalizeEmail), the spelling
// Factory.NormalizeRecipient gives a start's recipient. An address that
// cannot be normalized, such as an Exchange legacy distinguished name, is
// keyed as Graph gave it: the adapter never invents a spelling.
func identityAddress(addr string) string {
	if n, err := model.NormalizeEmail(addr); err == nil {
		return n
	}
	return addr
}

// destination is the first configured address among m's To and Cc
// recipients (case-insensitive), else the primary address, else the
// mailbox.
func (i *instance) destination(m gMessage) string {
	for _, r := range append(append([]gRecipient(nil), m.ToRecipients...), m.CcRecipients...) {
		for _, a := range i.cfg.Addresses {
			if strings.EqualFold(r.EmailAddress.Address, a) {
				return a
			}
		}
	}
	if len(i.cfg.Addresses) > 0 {
		return i.cfg.Addresses[0]
	}
	if i.cfg.Mailbox != "" {
		return i.cfg.Mailbox
	}
	return "me"
}

// headerMap indexes headers by lowercase name; repeated headers are joined
// with a space, which keeps msg-id lists parseable.
func headerMap(hs []gHeader) map[string]string {
	out := make(map[string]string, len(hs))
	for _, h := range hs {
		k := strings.ToLower(strings.TrimSpace(h.Name))
		if prev, ok := out[k]; ok {
			out[k] = prev + " " + h.Value
			continue
		}
		out[k] = h.Value
	}
	return out
}

func addresses(rs []gRecipient) []string {
	var out []string
	for _, r := range rs {
		if r.EmailAddress.Address != "" {
			out = append(out, r.EmailAddress.Address)
		}
	}
	return out
}

// truncateText cuts s to at most n bytes on a rune boundary, dropping
// invalid UTF-8.
func truncateText(s string, n int) (string, bool) {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= n {
		return s, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// cutBytes bounds s to n bytes on a rune boundary.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
