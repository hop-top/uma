package microsoft

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Extension effect for a reply left in the mailbox's Drafts folder.
const (
	// ExtensionReplyDraft keeps a reply to the conversation's peer as an
	// Outlook draft, for a person to finish and send in their mail client;
	// nothing is sent. Its data is
	// {"text": "...", "draft": "<draft ID>", "delete": false}:
	//
	//   - "reply_all": true, "cc": [...] and "bcc": [...] address a new draft
	//     like a send's recipients (model.ReplyRecipients). An update leaves
	//     the recipients as they are, so a person's changes stay; they are
	//     invalid on a deletion;
	//   - without "draft" it creates the draft the way Send does before
	//     its send call: createReply on the peer's message (Graph sets the
	//     reply subject, In-Reply-To, References and conversation), then
	//     a PATCH with the text, the route's address as the only
	//     recipient and the attempt property; a route without a parent
	//     message, or whose message is gone, becomes a new draft. "text"
	//     is required;
	//   - with "draft" it replaces that draft's body: PATCH of the body
	//     alone, so recipients and subject a person changed since stay.
	//     "text" is required;
	//   - with "draft" and "delete": true it deletes the draft
	//     (DELETE /messages/{id}; the message goes to Deleted Items).
	//     "text" must be absent.
	//
	// The outcome's provider references name the draft (DraftRef): the
	// operation keeps them, and a host passes the ID of an earlier
	// operation's reference as "draft" to update or delete it. A new
	// operation ID is needed for each call: a repeated operation ID with
	// the same payload replays the stored result and one with another
	// payload conflicts, as for every effect. Fields the adapter does not
	// know are ignored. It is authorized as model.ActionMessageReply on
	// the conversation; Mail.ReadWrite covers it.
	ExtensionReplyDraft = string(Type) + ".reply_draft"

	// RefKindDraft is the kind of the provider reference to a draft, under
	// model.ProviderMessageNamespace. A Graph draft is a message whose
	// immutable ID the reference carries; it stays the draft's ID when
	// the draft is sent, so a sent draft no longer updates (Graph
	// refuses a body change on a message that is no draft).
	RefKindDraft = "draft"
)

// DraftRef returns the provider reference of Graph draft id on channel.
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
	// ReplyAll, Cc and Bcc address a new draft like a send's recipients
	// (model.ReplyRecipients). An update leaves recipients alone.
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

// executeReplyDraft runs the reply draft effect. A final refusal of the
// provider is a rejected outcome; a 404 on update (the draft is gone) is
// rejected with not_found, on delete a success, the draft being gone; 403
// (the token lacks Mail.ReadWrite on the mailbox) is unauthorized. Update
// and delete are idempotent, so a lost answer or a 5xx is transient and
// the engine repeats the attempt. A create has no idempotency key
// (createReply and POST /messages each make a draft), so a lost answer is
// transient too: the repeat may leave an extra, unsent draft in Drafts
// (a draft is never sent by this effect). It is never outcome_unknown.
func (i *instance) executeReplyDraft(ctx context.Context, req adapter.ExecuteRequest) (adapter.Outcome, error) {
	a, issue := checkReplyDraft(req.Action)
	if issue != nil {
		return rejected(model.Validation, issue.Message), nil
	}
	s := i.c.session()
	var id string
	if a.Draft != "" {
		// The draft ID is the caller's: act only on a draft this adapter
		// made for this conversation.
		own, err := i.ownsDraft(ctx, s, req, a.Draft)
		switch {
		case model.IsCode(err, model.NotFound):
			if !a.Delete {
				return rejectedOr(err)
			}
			return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{DraftRef(i.ch.Ref, a.Draft)}}, nil
		case err != nil:
			return rejectedOr(err)
		case !own:
			return rejected(model.Validation, "draft is not a reply draft of this conversation"), nil
		}
	}
	switch {
	case a.Delete:
		_, err := s.do(ctx, request{method: "DELETE", url: i.messageURL(a.Draft, "")})
		if err != nil && !model.IsCode(err, model.NotFound) {
			return rejectedOr(err)
		}
		id = a.Draft
	case a.Draft != "":
		m, err := i.messageCall(ctx, s, "PATCH", i.messageURL(a.Draft, ""), encode(gDraftBodyUpdate{Body: gItemBody{ContentType: "text", Content: a.Text}}))
		if err != nil {
			return rejectedOr(err)
		}
		id = m.ID
	default:
		m, err := i.draft(ctx, s, req.Route, a.Text, attemptKey(req.Effect), "", a.recipients())
		if model.IsCode(err, model.Validation) {
			var me *model.Error
			errors.As(err, &me)
			return rejected(model.Validation, me.SafeMessage), nil
		}
		if err != nil {
			return rejectedOr(err)
		}
		id = m.ID
	}
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{DraftRef(i.ch.Ref, id)}}, nil
}

// ownsDraft reports whether message id is a draft this adapter made for the
// effect's conversation: still a draft, carrying the attempt property every
// reply draft is created with (its tenant the effect's), and in the
// conversation of the route's parent message; a route whose parent is gone
// or unknown binds by the draft's recipient being the route's address. A
// message that is gone is a model.NotFound error.
func (i *instance) ownsDraft(ctx context.Context, s *session, req adapter.ExecuteRequest, id string) (bool, error) {
	q := url.Values{
		"$select": {"id,isDraft,conversationId,toRecipients"},
		"$expand": {"singleValueExtendedProperties($filter=id eq '" + odataString(AttemptProperty) + "')"},
	}
	resp, err := s.do(ctx, request{method: "GET", url: i.c.api(i.cfg.mailboxPath()+"/messages/"+url.PathEscape(id), q), prefer: []string{preferImmutableID}})
	if err != nil {
		return false, transient(err)
	}
	var d gMessage
	if err := decode(resp.body, &d); err != nil {
		return false, err
	}
	marked := slices.ContainsFunc(d.SingleValueExtendedProperties, func(p gExtendedProperty) bool {
		return p.ID == AttemptProperty && strings.HasPrefix(p.Value, string(req.Operation.Tenant)+"/")
	})
	if !d.IsDraft || !marked {
		return false, nil
	}
	if req.Route.ParentID != "" {
		pq := url.Values{"$select": {"id,conversationId"}}
		presp, err := s.do(ctx, request{method: "GET", url: i.c.api(i.cfg.mailboxPath()+"/messages/"+url.PathEscape(req.Route.ParentID), pq), prefer: []string{preferImmutableID}})
		switch {
		case model.IsCode(err, model.NotFound):
		case err != nil:
			return false, transient(err)
		default:
			var p gMessage
			if err := decode(presp.body, &p); err != nil {
				return false, err
			}
			if p.ConversationID != "" {
				return d.ConversationID == p.ConversationID, nil
			}
		}
	}
	return len(d.ToRecipients) == 1 && strings.EqualFold(d.ToRecipients[0].EmailAddress.Address, req.Route.ID), nil
}
