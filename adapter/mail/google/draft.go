package google

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Extension effect for a reply left in the mailbox's Drafts.
const (
	// ExtensionReplyDraft keeps a reply to the conversation's peer as a
	// Gmail draft, for a person to finish and send in their mail client;
	// nothing is sent. Its data is
	// {"text": "...", "draft": "<draft ID>", "delete": false}:
	//
	//   - "reply_all": true, "cc": [...] and "bcc": [...] address the draft
	//     like a send's recipients (model.ReplyRecipients): the parent
	//     message's To and Cc minus the mailbox, and explicit addresses.
	//     They apply wherever the draft is composed (create and update) and
	//     are invalid on a deletion;
	//   - without "draft" it creates the draft: users.drafts.create with
	//     the reply composed as for a send (From the mailbox, To the
	//     route's address, Subject, In-Reply-To and References of the
	//     peer's message, its threadId). "text" is required;
	//   - with "draft" it replaces that draft's content:
	//     users.drafts.update with the same composition. "text" is
	//     required. The draft keeps its ID, so a person's edit made since
	//     is overwritten;
	//   - with "draft" and "delete": true it removes the draft:
	//     users.drafts.delete, permanent (the draft does not go to Trash).
	//     "text" must be absent.
	//
	// The outcome's provider references name the draft (DraftRef): the
	// operation keeps them, and a host passes the ID of an earlier
	// operation's reference as "draft" to update or delete it. A new
	// operation ID is needed for each call: a repeated operation ID with
	// the same payload replays the stored result and one with another
	// payload conflicts, as for every effect. The conversation's reply
	// route names the peer and the message the reply answers (the
	// route's ParentID); a missing parent message starts a new thread, as
	// a send does. Fields the adapter does not know are ignored. It is
	// authorized as model.ActionMessageReply on the conversation and needs
	// ScopeModify (users.drafts.* take gmail.compose, gmail.modify or
	// full access).
	ExtensionReplyDraft = string(Type) + ".reply_draft"

	// RefKindDraft is the kind of the provider reference to a draft, under
	// model.ProviderMessageNamespace: a draft is not a message, and its
	// ID differs from the ID of the message it holds, which changes on
	// every update.
	RefKindDraft = "draft"
)

// DraftRef returns the provider reference of Gmail draft id on channel.
func DraftRef(ch model.ChannelRef, id string) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: model.ProviderMessageNamespace, Kind: RefKindDraft, ID: id}
}

func replyDraftDefinition() adapter.ExtensionDefinition {
	return adapter.ExtensionDefinition{
		Name: ExtensionReplyDraft, Version: ExtensionVersion, Kind: model.ExtensionKindEffect,
		InputSchema:        model.Extension{Name: ExtensionReplyDraft + ".input", Version: ExtensionVersion},
		RequiredCapability: model.CapabilityExtensionExecute, AuthorizeAs: model.ActionMessageReply,
	}
}

// replyDraftAction are the fields of an ExtensionReplyDraft action.
type replyDraftAction struct {
	Text   string
	Draft  string
	Delete bool
	// ReplyAll, Cc and Bcc address the draft like a send's recipients
	// (model.ReplyRecipients).
	ReplyAll bool
	Cc, Bcc  []string
}

// recipients is the draft's recipients, nil when it names none.
func (a replyDraftAction) recipients() *model.ReplyRecipients {
	r := model.ReplyRecipients{ReplyAll: a.ReplyAll, Cc: a.Cc, Bcc: a.Bcc}
	if r.IsZero() {
		return nil
	}
	return &r
}

// create reports an action that makes a new draft.
func (a replyDraftAction) create() bool { return a.Draft == "" && !a.Delete }

// checkReplyDraft is the one set of field rules of the reply draft effect.
// It returns the action's fields when they are valid, otherwise the first
// issue, whose message is safe to show.
func checkReplyDraft(action model.Extension) (replyDraftAction, *model.ValidationIssue) {
	refuse := func(field, msg string) (replyDraftAction, *model.ValidationIssue) {
		return replyDraftAction{}, &model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(action.Data), &fields); err != nil || fields == nil {
		return refuse("data", "action data must be a JSON object")
	}
	var a replyDraftAction
	for _, f := range []struct {
		name string
		dst  any
		want string
	}{
		{"text", &a.Text, "a string"}, {"draft", &a.Draft, "a string"}, {"delete", &a.Delete, "a boolean"},
		{"reply_all", &a.ReplyAll, "a boolean"}, {"cc", &a.Cc, "a list of addresses"}, {"bcc", &a.Bcc, "a list of addresses"},
	} {
		raw, set := fields[f.name]
		if !set || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		if err := json.Unmarshal(raw, f.dst); err != nil {
			return refuse("data."+f.name, f.name+" must be "+f.want)
		}
	}
	switch {
	case a.Draft != "" && strings.TrimSpace(a.Draft) != a.Draft:
		return refuse("data.draft", "draft must be a draft ID")
	case a.Delete && a.Draft == "":
		return refuse("data.draft", "a deletion names the draft to delete")
	case a.Delete && a.Text != "":
		return refuse("data.text", "a deletion carries no text")
	case !a.Delete && strings.TrimSpace(a.Text) == "":
		return refuse("data.text", "a reply draft needs text")
	case a.Delete && a.recipients() != nil:
		return refuse("data.cc", "a deletion carries no recipients")
	}
	if r := a.recipients(); r != nil {
		if err := r.Validate(); err != nil {
			field := "data.cc"
			if len(a.Cc) == 0 {
				field = "data.bcc"
			}
			return refuse(field, "recipients must be at most "+strconv.Itoa(model.MaxReplyRecipients)+" bare addresses")
		}
	}
	return a, nil
}

// executeReplyDraft runs the reply draft effect. Update and delete are
// idempotent, so a lost answer or a 5xx is transient and the engine
// repeats the attempt. A create has no idempotency key: a 5xx or a lost
// answer is outcome_unknown and Reconcile searches Drafts for the
// operation's Message-ID before another attempt. A 404 on update (the
// draft is gone, sent or deleted by the person) is a final rejected
// outcome with not_found; on delete it is a success, the draft being
// gone. A 403 (the token lacks a draft scope) is unauthorized.
func (i *instance) executeReplyDraft(ctx context.Context, req adapter.ExecuteRequest) (adapter.Outcome, error) {
	a, issue := checkReplyDraft(req.Action)
	if issue != nil {
		return rejected(model.Validation, issue.Message), nil
	}
	s := i.c.session()
	if a.Draft != "" {
		// The draft ID is the caller's: act only on a draft this adapter
		// made for this conversation.
		own, err := i.ownsDraft(ctx, s, req.Route, a.Draft)
		switch {
		case model.IsCode(err, model.NotFound) && a.Delete:
			return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{DraftRef(i.ch.Ref, a.Draft)}}, nil
		case err != nil:
			return rejectedOr(err)
		case !own:
			return rejected(model.Validation, "draft is not a reply draft of this conversation"), nil
		}
	}
	if a.Delete {
		return i.deleteDraft(ctx, s, a.Draft)
	}
	body, msgID, composeErr, err := i.composeReply(ctx, s, req.Route, req.Operation, a.Text, "", a.recipients())
	if composeErr != nil {
		return rejected(model.Validation, safeMessage(composeErr)), nil
	}
	if err != nil {
		return rejectedOr(err)
	}
	r := request{method: "POST", url: i.c.api("/drafts", nil), body: encode(gDraftBody{Message: body})}
	if !a.create() {
		r = request{method: "PUT", url: i.c.api("/drafts/"+url.PathEscape(a.Draft), nil), body: r.body}
	}
	resp, err := s.do(ctx, r)
	switch {
	case err == nil:
	case a.create() && errors.Is(err, errUnsent) && !isContext(err):
		return adapter.Outcome{}, model.NewError(model.Transient, "gmail unreachable; draft not created")
	case a.create() && (errors.Is(err, errTransport) || resp.status >= 500 || resp.status == http.StatusRequestTimeout):
		return adapter.Outcome{References: []model.ExternalRef{i.messageIDRef(msgID)}},
			model.NewError(model.OutcomeUnknown, "gmail draft answer lost")
	default:
		return rejectedOr(err)
	}
	var d gDraft
	if err := decode(resp.body, &d); err != nil || d.ID == "" {
		if a.create() {
			// Created, but the answer names no draft: reconcile finds it.
			return adapter.Outcome{References: []model.ExternalRef{i.messageIDRef(msgID)}},
				model.NewError(model.OutcomeUnknown, "gmail draft answer unreadable")
		}
		d.ID = a.Draft
	}
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{DraftRef(i.ch.Ref, d.ID)}}, nil
}

// deleteDraft is users.drafts.delete; a draft already gone is deleted.
func (i *instance) deleteDraft(ctx context.Context, s *session, id string) (adapter.Outcome, error) {
	_, err := s.do(ctx, request{method: "DELETE", url: i.c.api("/drafts/"+url.PathEscape(id), nil)})
	if err != nil && !model.IsCode(err, model.NotFound) {
		return rejectedOr(err)
	}
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{DraftRef(i.ch.Ref, id)}}, nil
}

// reconcileReplyDraft answers an outcome_unknown draft creation: users.drafts.list
// with the documented rfc822msgid: search operator for the operation's
// Message-ID. Found: the draft exists (provider_accepted with its ID). Not
// found: failed, which allows a new attempt once ReconcileSettle has
// passed (the search index may lag). An update or a deletion is never left
// unknown: asked anyway, the answer is failed with a transient failure, a
// new attempt being safe.
func (i *instance) reconcileReplyDraft(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	a, issue := checkReplyDraft(req.Action)
	if issue != nil || !a.create() {
		return adapter.Outcome{State: model.Failed, Failure: model.NewError(model.Transient, "a draft update or deletion is idempotent; a new attempt is safe")}, nil
	}
	s := i.c.session()
	from, err := i.mailboxAddress(ctx, s)
	if err != nil {
		return adapter.Outcome{}, transient(err)
	}
	id := i.messageID(req.Operation, from)
	q := url.Values{"q": {"rfc822msgid:" + bareID(id)}, "maxResults": {"10"}}
	resp, err := s.do(ctx, request{method: "GET", url: i.c.api("/drafts", q)})
	if err != nil {
		return adapter.Outcome{}, transient(err)
	}
	var page gDraftList
	if err := decode(resp.body, &page); err != nil {
		return adapter.Outcome{}, err
	}
	for _, d := range page.Drafts {
		if d.ID != "" {
			return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{DraftRef(i.ch.Ref, d.ID)}}, nil
		}
	}
	return adapter.Outcome{State: model.Failed, Failure: model.NewError(model.Transient, "gmail holds no draft of the operation")}, nil
}

// ownedMessageID matches the Message-ID uma derives for an operation
// (messageID): 32 base32 characters, then the adapter's domain.
var ownedMessageID = regexp.MustCompile(`^<[a-z2-7]{32}@(.+)>$`)

// ownsDraft reports whether draft id is one this adapter made for the
// effect's conversation (users.drafts.get, format=metadata): its Message-ID
// is of uma's derived form on the adapter's Message-ID domain, and it sits
// in the thread of the route's parent message; a route whose parent is gone
// or unknown binds by the draft's To header being the route's address alone
// (reply to all adds recipients only from a parent message). A
// draft that is gone is a model.NotFound error.
func (i *instance) ownsDraft(ctx context.Context, s *session, route model.ExternalRef, id string) (bool, error) {
	q := url.Values{"format": {"metadata"}, "metadataHeaders": {"Message-ID", "To"}}
	resp, err := s.do(ctx, request{method: "GET", url: i.c.api("/drafts/"+url.PathEscape(id), q)})
	if err != nil {
		return false, transient(err)
	}
	var d gDraft
	if err := decode(resp.body, &d); err != nil {
		return false, err
	}
	from, err := i.mailboxAddress(ctx, s)
	if err != nil {
		return false, transient(err)
	}
	var msgID, to string
	if d.Message.Payload != nil {
		for _, h := range d.Message.Payload.Headers {
			switch strings.ToLower(h.Name) {
			case "message-id":
				msgID = h.Value
			case "to":
				to = h.Value
			}
		}
	}
	domain := i.cfg.MessageIDDomain
	if domain == "" {
		domain = strings.ToLower(from[strings.LastIndexByte(from, '@')+1:])
	}
	if m := ownedMessageID.FindStringSubmatch(strings.TrimSpace(msgID)); m == nil || m[1] != domain {
		return false, nil
	}
	if route.ParentID != "" {
		parent, err := i.parent(ctx, s, route.ParentID)
		if err != nil {
			return false, err
		}
		if parent != nil && parent.threadID != "" {
			return d.Message.ThreadID == parent.threadID, nil
		}
	}
	// No thread to bind by: the draft is addressed to the route's address
	// alone (a parsed address, compared whole and without case).
	addrs, err := mail.ParseAddressList(to)
	return err == nil && len(addrs) == 1 && strings.EqualFold(addrs[0].Address, route.ID), nil
}
