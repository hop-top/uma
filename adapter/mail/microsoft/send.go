package microsoft

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/recipients"
	"hop.top/uma/model"
)

// AttemptProperty is the named single-value extended property every
// outbound draft carries: "<tenant>/<operation>/<attempt>". Graph assigns
// the Internet Message-ID itself, at draft creation; this property is the
// attempt's own key, filterable with $filter, so Reconcile finds the sent
// copy of an attempt whose answer was lost.
const AttemptProperty = "String {31c7380f-d7fb-4c46-affc-b4bed2fb34b9} Name uma-attempt"

// cleanupTimeout bounds the best-effort removal of an unsent draft.
const cleanupTimeout = 10 * time.Second

// Send delivers req's text to the stored route. In SendModeDirect it is one
// sendMail call (sendDirect); in SendModeDraft, the default, it goes through
// a draft:
//
//  1. a route with ParentID (the Graph message the peer wrote) becomes
//     createReply on that message, so Graph sets the reply subject,
//     In-Reply-To, References and conversation; the draft is then updated
//     with the body, the route's address as the recipient (with the
//     request's recipients, if any: createReplyAll for a reply to all,
//     whose parent To and Cc replace the route as the draft's recipients,
//     minus the mailbox itself) and the attempt property. A route without ParentID, or whose message is
//     gone, becomes a new draft (POST /messages);
//  2. POST /messages/{id}/send.
//
// Graph assigns the draft's Internet Message-ID; the outcome reports it
// (email.header message_id) next to the immutable Graph message ID.
//
// Failures before the send call are retryable or final as mapped; nothing
// was sent: a duplicate draft is never delivered. The send call itself has
// no idempotency key: 202 is provider_accepted; an answer that never
// arrived after the request may have been written, or any 5xx but 503, is
// outcome_unknown and is never retried before Reconcile; a connect, DNS or
// TLS failure the HTTP client reports before writing the request is
// transient; 429 and 503 (throttling with Retry-After) are retried after
// the wait; other 4xx are rejected. A draft known to be unsent is deleted,
// best effort.
func (i *instance) Send(ctx context.Context, req adapter.SendRequest) (adapter.Outcome, error) {
	return i.send(ctx, req, "")
}

// send is Send with a subject of its own, used for a draft that answers no
// message (a reply's subject is Graph's).
func (i *instance) send(ctx context.Context, req adapter.SendRequest, subject string) (adapter.Outcome, error) {
	if err := i.checkRoute(req.Route); err != nil {
		return adapter.Outcome{}, err
	}
	text, err := plainText(req.Content)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if i.cfg.sendMode() == SendModeDirect {
		return i.sendDirect(ctx, req, text, subject)
	}
	s := i.c.session()
	draft, err := i.draft(ctx, s, req.Route, text, attemptKey(req.Effect), subject, req.Recipients)
	if err != nil {
		return rejectedOr(err)
	}
	refs := i.references(draft)
	resp, err := s.do(ctx, request{method: "POST", url: i.messageURL(draft.ID, "/send")})
	switch {
	case err == nil:
		return adapter.Outcome{State: model.ProviderAccepted, References: refs}, nil
	case errors.Is(err, errUnsent) && !isContext(err):
		// The HTTP client failed to connect and never wrote the request.
		i.discard(ctx, s, draft.ID)
		return adapter.Outcome{}, model.NewError(model.Transient, "graph unreachable; send not attempted")
	case errors.Is(err, errTransport), sendAmbiguous(resp.status):
		// The send has no idempotency key: never retried before
		// Reconcile looks for the attempt's sent copy.
		return adapter.Outcome{References: refs}, model.NewError(model.OutcomeUnknown, "graph send answer lost")
	}
	i.discard(ctx, s, draft.ID)
	return rejectedOr(err)
}

// sendAmbiguous reports a send answer that does not say whether Graph
// accepted the message: any server error but 503 (documented, with 429,
// as throttling with Retry-After), and 408.
func sendAmbiguous(status int) bool {
	return status == http.StatusRequestTimeout || (status >= 500 && status != http.StatusServiceUnavailable)
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

// Reconcile looks for the attempt's message by its AttemptProperty:
//
//   - in Sent Items: it was sent (provider_accepted, with its Message-ID);
//   - elsewhere and no longer a draft (Outbox): the send may yet complete,
//     so the answer is a retryable failure and reconciliation repeats;
//   - still a draft: it is deleted first, and once Graph confirms the
//     deletion it can no longer be sent; a draft that cannot be deleted is
//     answered like the Outbox case;
//   - nowhere (or only deleted drafts): never sent; the outcome is failed,
//     which allows a new attempt.
//
// Graph documents that the Sent Items copy may appear only some time after
// a send: the adapter declares ReconcileSettle, inside which the engine
// treats "nowhere" as inconclusive and reconciles again later.
//
// A categories effect (model.CapabilityExtensionExecute) is never left
// outcome_unknown, since the update replaces the list; asked anyway, the
// answer is failed with a transient failure: a new attempt is safe. An
// unsubscribe effect is reconciled like a send: only its mailto method
// sends, and it does so under the attempt property; a one-click request
// leaves nothing in Sent Items, so "nowhere" is failed and a new attempt,
// which repeats the idempotent request, is safe.
func (i *instance) Reconcile(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	if i.cfg.sendMode() == SendModeDirect {
		// A direct send leaves nothing the adapter may read back: no draft
		// to find, and Mail.Send cannot read Sent Items.
		return adapter.Outcome{}, &model.Error{Code: model.Unsupported, SafeMessage: "a direct-mode channel cannot reconcile", Retry: model.NoRetry}
	}
	eff := req.Effect
	if err := i.checkRoute(eff.Route); err != nil {
		return adapter.Outcome{}, err
	}
	if slices.Contains(stateCapabilities, req.Capability) {
		return i.reconcileState(ctx, req)
	}
	if req.Capability == model.CapabilityExtensionExecute && req.Action.Name != ExtensionUnsubscribe {
		return adapter.Outcome{State: model.Failed, Failure: model.NewError(model.Transient, "a category change is idempotent; a new attempt is safe")}, nil
	}
	s := i.c.session()
	key := attemptKey(eff)
	sent, err := i.findAttempt(ctx, s, "/mailFolders/sentitems/messages", key)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if len(sent) > 0 {
		return adapter.Outcome{State: model.ProviderAccepted, References: i.references(sent[0])}, nil
	}
	pending, err := i.findAttempt(ctx, s, "/messages", key)
	if err != nil {
		return adapter.Outcome{}, err
	}
	for _, m := range pending {
		if !m.IsDraft || !i.deleted(ctx, s, m.ID) {
			return adapter.Outcome{}, model.NewError(model.Transient, "attempt not in Sent Items yet")
		}
	}
	return adapter.Outcome{State: model.Failed, Failure: model.NewError(model.Transient, "graph holds no sent message of the attempt")}, nil
}

// deleted removes an unsent draft and reports whether Graph confirmed it:
// a deleted draft can no longer be sent, so a new attempt cannot
// duplicate it.
func (i *instance) deleted(ctx context.Context, s *session, id string) bool {
	_, err := s.do(ctx, request{method: "DELETE", url: i.messageURL(id, "")})
	return err == nil
}

func (i *instance) findAttempt(ctx context.Context, s *session, folder, key string) ([]gMessage, error) {
	q := url.Values{
		"$filter": {"singleValueExtendedProperties/Any(ep: ep/id eq '" + odataString(AttemptProperty) +
			"' and ep/value eq '" + odataString(key) + "')"},
		"$select": {"id,internetMessageId,isDraft"},
	}
	resp, err := s.do(ctx, request{method: "GET", url: i.c.api(i.cfg.mailboxPath()+folder, q), prefer: []string{preferImmutableID}})
	if err != nil {
		return nil, transient(err)
	}
	var page gMessagePage
	if err := decode(resp.body, &page); err != nil {
		return nil, err
	}
	return page.Value, nil
}

// draft creates the attempt's draft and returns it with its Graph ID and
// Message-ID. rec adds the reply's recipients (see recipients.Plan): a reply
// to all is createReplyAll, whose draft holds the parent's To and Cc, which
// the update then sets minus the mailbox itself; a parent that is gone adds
// none. A set of recipients that cannot be sent is a final model.Validation
// error, found before any draft is created unless it depends on the
// parent's.
func (i *instance) draft(ctx context.Context, s *session, route model.ExternalRef, text, key, subject string, rec *model.ReplyRecipients) (gMessage, error) {
	self := i.cfg.selfAddresses()
	set, err := recipients.Plan(rec, route.ID, self, nil, nil)
	if err != nil {
		return gMessage{}, err
	}
	body := gDraft{
		Subject:                       subject,
		Body:                          gItemBody{ContentType: "text", Content: text},
		ToRecipients:                  graphRecipients(set.To),
		CcRecipients:                  graphRecipients(set.Cc),
		BccRecipients:                 graphRecipients(set.Bcc),
		SingleValueExtendedProperties: []gExtendedProperty{{ID: AttemptProperty, Value: key}},
	}
	if route.ParentID != "" {
		verb := "/createReply"
		if rec != nil && rec.ReplyAll {
			verb = "/createReplyAll"
		}
		m, err := i.messageCall(ctx, s, "POST", i.messageURL(route.ParentID, verb), nil)
		switch {
		case err == nil:
			if rec != nil && rec.ReplyAll {
				set, err := recipients.Plan(rec, route.ID, self, recipientsOf(m.ToRecipients), recipientsOf(m.CcRecipients))
				if err != nil {
					i.discard(ctx, s, m.ID)
					return gMessage{}, err
				}
				body.ToRecipients, body.CcRecipients, body.BccRecipients = graphRecipients(set.To), graphRecipients(set.Cc), graphRecipients(set.Bcc)
			}
			updated, err := i.messageCall(ctx, s, "PATCH", i.messageURL(m.ID, ""), encode(body))
			if err != nil {
				i.discard(ctx, s, m.ID)
				return gMessage{}, err
			}
			return updated, nil
		case !model.IsCode(err, model.NotFound):
			return gMessage{}, err
		}
	}
	return i.messageCall(ctx, s, "POST", i.c.api(i.cfg.mailboxPath()+"/messages", nil), encode(body))
}

// messageCall runs a draft call that answers a message.
func (i *instance) messageCall(ctx context.Context, s *session, method, u string, body []byte) (gMessage, error) {
	r := request{method: method, url: u, body: body, prefer: []string{preferImmutableID}}
	if body != nil {
		r.contentType = "application/json"
	}
	resp, err := s.do(ctx, r)
	if err != nil {
		return gMessage{}, transient(err)
	}
	var m gMessage
	if err := decode(resp.body, &m); err != nil {
		return gMessage{}, err
	}
	if m.ID == "" {
		return gMessage{}, &model.Error{Code: model.Permanent, SafeMessage: "graph answered a draft without an ID", Retry: model.NoRetry}
	}
	return m, nil
}

// discard deletes an unsent draft, best effort, even after ctx ended.
func (i *instance) discard(ctx context.Context, s *session, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	_, _ = s.do(ctx, request{method: "DELETE", url: i.messageURL(id, "")})
}

func (i *instance) messageURL(id, suffix string) string {
	return i.c.api(i.cfg.mailboxPath()+"/messages/"+url.PathEscape(id)+suffix, nil)
}

// references are the provider references of a sent message: its immutable
// Graph ID and, when Graph reported one, its Message-ID.
func (i *instance) references(m gMessage) []model.ExternalRef {
	refs := []model.ExternalRef{model.ProviderMessageRef(i.ch.Ref, m.ID)}
	if ids := model.ParseMsgIDs(m.InternetMessageID); len(ids) > 0 {
		refs = append(refs, model.ExternalRef{
			Channel: i.ch.Ref, Namespace: model.EmailHeaderNamespace, Kind: model.RefKindEmailMessageID, ID: ids[0],
		})
	}
	return refs
}

// checkRoute accepts only an address route of this channel.
func (i *instance) checkRoute(r model.ExternalRef) error {
	if r.Channel != i.ch.Ref || r.Namespace != IdentityNamespace || r.Kind != refKindAddress || !plainToken(r.ID) {
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

// attemptKey identifies one attempt of one operation.
func attemptKey(e adapter.Effect) string {
	return string(e.Operation.Tenant) + "/" + string(e.Operation.ID) + "/" + string(e.Attempt)
}

// odataString escapes a string literal for $filter.
func odataString(s string) string { return strings.ReplaceAll(s, "'", "''") }
