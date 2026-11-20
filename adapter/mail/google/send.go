package google

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/adapter/mail/recipients"
	"hop.top/uma/model"
)

// Send composes req's text as a raw RFC 5322 message and submits it with
// users.messages.send:
//
//   - a route with ParentID (the Gmail message the peer wrote) is a reply:
//     the parent's Subject, Message-ID, References and threadId are read
//     (format=metadata) and the message carries In-Reply-To, References,
//     the matching subject and the threadId, which Gmail documents as the
//     conditions for adding it to the thread. A parent that is gone starts
//     a new thread;
//   - req.Recipients adds the reply's Cc and Bcc and, replying to all, the
//     parent's To and Cc minus the mailbox itself; Bcc travels in a Bcc
//     header, which Gmail removes before delivery;
//   - the Message-ID is uma's: derived from the operation, the same for
//     every attempt, revealing neither tenant nor operation. Reconcile
//     searches Sent for it.
//
// messages.send has no idempotency key. A 2xx answer is provider_accepted
// with the Gmail message ID and the Message-ID as references. A connect,
// DNS or TLS failure the HTTP client reports before writing the request is
// transient. Rate limits (429, 403 rateLimitExceeded and friends) are
// rate_limited with the Retry-After wait when stated: Gmail refused the
// call. Any 5xx or 408 answer, and any failure once the request may have
// been written, is outcome_unknown and is never retried before Reconcile.
// Other 4xx are rejected; a missing scope (403) is unauthorized.
func (i *instance) Send(ctx context.Context, req adapter.SendRequest) (adapter.Outcome, error) {
	return i.send(ctx, req, "")
}

// send is Send with a subject of its own, used when set and the route has
// no parent whose subject a reply carries.
func (i *instance) send(ctx context.Context, req adapter.SendRequest, subject string) (adapter.Outcome, error) {
	if err := i.checkRoute(req.Route); err != nil {
		return adapter.Outcome{}, err
	}
	text, err := plainText(req.Content)
	if err != nil {
		return adapter.Outcome{}, err
	}
	s := i.c.session()
	body, msgID, composeErr, err := i.composeReply(ctx, s, req.Route, req.Operation, text, subject, req.Recipients)
	if composeErr != nil {
		return adapter.Outcome{}, composeErr
	}
	if err != nil {
		return rejectedOr(err)
	}
	return i.submit(ctx, s, body, msgID)
}

// submit posts the composed message with users.messages.send; Send and the
// forward effect share its outcomes.
func (i *instance) submit(ctx context.Context, s *session, body gSend, msgID string) (adapter.Outcome, error) {
	resp, err := s.do(ctx, request{method: "POST", url: i.c.api("/messages/send", nil), body: encode(body)})
	switch {
	case err == nil:
	case errors.Is(err, errUnsent) && !isContext(err):
		// The HTTP client failed to connect and never wrote the request.
		return adapter.Outcome{}, model.NewError(model.Transient, "gmail unreachable; send not attempted")
	case errors.Is(err, errTransport), resp.status >= 500, resp.status == http.StatusRequestTimeout:
		// No idempotency key: never retried before Reconcile searches
		// Sent for the Message-ID.
		return adapter.Outcome{References: []model.ExternalRef{i.messageIDRef(msgID)}},
			model.NewError(model.OutcomeUnknown, "gmail send answer lost")
	default:
		return rejectedOr(err)
	}
	var sent gMessage
	if err := decode(resp.body, &sent); err != nil || sent.ID == "" {
		// Accepted, but the answer does not name the message.
		return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{i.messageIDRef(msgID)}}, nil
	}
	return adapter.Outcome{State: model.ProviderAccepted, References: i.references(sent.ID, msgID)}, nil
}

// composeReply builds the raw RFC 5322 reply to the route's parent message
// that Send submits and a reply draft stores: From the mailbox, To the
// route's address, the parent's threading headers and threadId when the
// parent is still there, and the operation's Message-ID. rec adds the
// reply's recipients: its Cc and Bcc, and, replying to all, the parent's To
// and Cc minus the mailbox (see recipients.Plan); a parent that is gone adds
// none. composeErr is a message that cannot be composed (a final validation
// error); err is a provider failure to map with rejectedOr.
func (i *instance) composeReply(ctx context.Context, s *session, route model.ExternalRef, op model.OperationRef, text, subject string, rec *model.ReplyRecipients) (body gSend, msgID string, composeErr, err error) {
	from, err := i.mailboxAddress(ctx, s)
	if err != nil {
		return gSend{}, "", nil, err
	}
	out := mime.Outgoing{
		From:    mime.Address{Address: from},
		To:      []mime.Address{{Address: route.ID}},
		Subject: i.cfg.Subject,
		Date:    i.now(),
		Text:    text,
	}
	if subject != "" {
		out.Subject = subject
	}
	out.MessageID = i.messageID(op, from)
	var threadID string
	var parentTo, parentCc []string
	if route.ParentID != "" {
		parent, err := i.parent(ctx, s, route.ParentID)
		if err != nil {
			return gSend{}, "", nil, err
		}
		if parent != nil {
			threadID = parent.threadID
			parentTo, parentCc = parent.to, parent.cc
			out.Subject = replySubject(parent.subject)
			if parent.messageID != "" {
				out.InReplyTo = parent.messageID
				out.References = append(parent.references, parent.messageID)
			}
		}
	}
	if rec != nil {
		set, err := recipients.Plan(rec, route.ID, []string{from}, parentTo, parentCc)
		if err != nil {
			var me *model.Error
			errors.As(err, &me)
			return gSend{}, "", me, nil
		}
		out.To, out.Cc, out.Bcc = recipients.Addresses(set.To), recipients.Addresses(set.Cc), recipients.Addresses(set.Bcc)
	}
	var raw bytes.Buffer
	if err := mime.Compose(&raw, out); err != nil {
		return gSend{}, "", &model.Error{Code: model.Validation, SafeMessage: "message cannot be composed", Retry: model.NoRetry}, nil
	}
	return gSend{Raw: base64.RawURLEncoding.EncodeToString(raw.Bytes()), ThreadID: threadID}, out.MessageID, nil, nil
}

// safeMessage is the message of a model error that is safe to show.
func safeMessage(err error) string {
	var me *model.Error
	if errors.As(err, &me) {
		return me.SafeMessage
	}
	return "message cannot be composed"
}

// rejectedOr turns a permanent provider refusal into a rejected outcome and
// passes other failures through.
func rejectedOr(err error) (adapter.Outcome, error) {
	if model.IsCode(err, model.Permanent) || model.IsCode(err, model.NotFound) || model.IsCode(err, model.Conflict) {
		var me *model.Error
		errors.As(err, &me)
		return adapter.Outcome{State: model.Rejected, Failure: me}, nil
	}
	return adapter.Outcome{}, transient(err)
}

// Reconcile searches the mailbox's Sent label (trash and spam included)
// for the operation's Message-ID with the documented rfc822msgid: search
// operator:
//
//   - found: it was sent (provider_accepted, with the Gmail message ID);
//   - not found: not sent; the outcome is failed, which allows a new
//     attempt once ReconcileSettle has passed since the attempt (the
//     engine treats an earlier "not found" as inconclusive): the search
//     index may lag the Sent label.
//
// A labels effect (model.CapabilityExtensionExecute) is never left
// outcome_unknown, since messages.modify is idempotent; asked anyway, the
// answer is failed with a transient failure: a new attempt is safe. An
// forward effect is reconciled like a send, under the operation's
// Message-ID. An unsubscribe effect is reconciled like a send: only its mailto method
// sends, and it does so under the operation's Message-ID; a one-click
// request leaves nothing in Sent, so "not found" is failed and a new
// attempt, which repeats the idempotent request, is safe.
func (i *instance) Reconcile(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	eff := req.Effect
	if err := i.checkRoute(eff.Route); err != nil {
		return adapter.Outcome{}, err
	}
	if slices.Contains(stateCapabilities, req.Capability) {
		return i.reconcileState(ctx, req)
	}
	if req.Capability == model.CapabilityExtensionExecute && req.Action.Name == ExtensionReplyDraft {
		return i.reconcileReplyDraft(ctx, req)
	}
	if req.Capability == model.CapabilityExtensionExecute && req.Action.Name != ExtensionUnsubscribe && req.Action.Name != ExtensionForward {
		return adapter.Outcome{State: model.Failed, Failure: model.NewError(model.Transient, "a label change is idempotent; a new attempt is safe")}, nil
	}
	s := i.c.session()
	from, err := i.mailboxAddress(ctx, s)
	if err != nil {
		return adapter.Outcome{}, transient(err)
	}
	id := i.messageID(eff.Operation, from)
	q := url.Values{
		"labelIds":         {labelSent},
		"q":                {"rfc822msgid:" + bareID(id)},
		"includeSpamTrash": {"true"},
		"maxResults":       {"10"},
	}
	resp, err := s.do(ctx, request{method: "GET", url: i.c.api("/messages", q)})
	if err != nil {
		return adapter.Outcome{}, transient(err)
	}
	var page gMessageList
	if err := decode(resp.body, &page); err != nil {
		return adapter.Outcome{}, err
	}
	for _, m := range page.Messages {
		if m.ID != "" {
			return adapter.Outcome{State: model.ProviderAccepted, References: i.references(m.ID, id)}, nil
		}
	}
	return adapter.Outcome{State: model.Failed, Failure: model.NewError(model.Transient, "gmail holds no sent message of the operation")}, nil
}

// parentInfo is the threading evidence of a reply's parent message.
type parentInfo struct {
	threadID   string
	subject    string
	messageID  string
	references []string
	// to and cc are the parent's recipients, as bare addresses.
	to, cc []string
}

// parent reads the metadata of Gmail message id; nil when it is gone.
func (i *instance) parent(ctx context.Context, s *session, id string) (*parentInfo, error) {
	q := url.Values{"format": {"metadata"}, "metadataHeaders": {"Subject", "Message-ID", "References", "To", "Cc"}}
	resp, err := s.do(ctx, request{method: "GET", url: i.c.api("/messages/"+url.PathEscape(id), q)})
	if model.IsCode(err, model.NotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, transient(err)
	}
	var m gMessage
	if err := decode(resp.body, &m); err != nil {
		return nil, err
	}
	p := &parentInfo{threadID: m.ThreadID}
	if m.Payload != nil {
		for _, h := range m.Payload.Headers {
			switch strings.ToLower(h.Name) {
			case "subject":
				p.subject = h.Value
			case "message-id":
				if ids := model.ParseMsgIDs(h.Value); len(ids) > 0 {
					p.messageID = ids[0]
				}
			case "references":
				p.references = append(p.references, model.ParseMsgIDs(h.Value)...)
			case "to":
				p.to = append(p.to, recipients.ParseList(h.Value)...)
			case "cc":
				p.cc = append(p.cc, recipients.ParseList(h.Value)...)
			}
		}
	}
	return p, nil
}

// replySubject is the subject of a reply to subject: "Re: " once.
func replySubject(subject string) string {
	subject = strings.Join(strings.Fields(subject), " ")
	if len(subject) >= 3 && strings.EqualFold(subject[:3], "re:") {
		return subject
	}
	if subject == "" {
		return ""
	}
	return "Re: " + subject
}

var msgIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// messageID derives the Message-ID of an operation: the same for every
// attempt, distinct across operations and tenants, revealing neither.
func (i *instance) messageID(op model.OperationRef, from string) string {
	sum := sha256.Sum256([]byte(string(Type) + "\x00" + string(op.Tenant) + "\x00" + string(op.ID)))
	domain := i.cfg.MessageIDDomain
	if domain == "" {
		domain = strings.ToLower(from[strings.LastIndexByte(from, '@')+1:])
	}
	return "<" + strings.ToLower(msgIDEncoding.EncodeToString(sum[:20])) + "@" + domain + ">"
}

func bareID(id string) string { return strings.TrimSuffix(strings.TrimPrefix(id, "<"), ">") }

func (i *instance) messageIDRef(id string) model.ExternalRef {
	return model.ExternalRef{Channel: i.ch.Ref, Namespace: model.EmailHeaderNamespace, Kind: model.RefKindEmailMessageID, ID: id}
}

// references are the provider references of a sent message: its Gmail
// message ID and its Message-ID.
func (i *instance) references(gmailID, msgID string) []model.ExternalRef {
	return []model.ExternalRef{model.ProviderMessageRef(i.ch.Ref, gmailID), i.messageIDRef(msgID)}
}

// checkRoute accepts only an address route of this channel.
func (i *instance) checkRoute(r model.ExternalRef) error {
	if r.Channel != i.ch.Ref || r.Namespace != IdentityNamespace || r.Kind != refKindAddress || !bareAddress(r.ID) {
		return &model.Error{Code: model.Permanent, SafeMessage: "route is not a " + string(Type) + " address of the channel", Retry: model.NoRetry}
	}
	return nil
}

// plainText joins the text parts of c. Attachments and structured parts
// are refused: the adapter receives no blob store to read attachment bytes
// from, and unknown structured content must not be rendered.
func plainText(c model.Content) (string, error) {
	var parts []string
	for _, p := range c.Parts {
		if p.Kind != model.PartText {
			return "", &model.Error{Code: model.Unsupported, SafeMessage: "only text content can be sent", Retry: model.NoRetry}
		}
		parts = append(parts, p.Text)
	}
	if len(parts) == 0 {
		return "", &model.Error{Code: model.Validation, SafeMessage: "no text to send", Retry: model.NoRetry}
	}
	return strings.Join(parts, "\n\n"), nil
}
