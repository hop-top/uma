package zendesk

import (
	"strings"
	"time"

	"hop.top/uma/model"
)

// Namespace is the external reference and identity namespace of Zendesk
// users, tickets and audits.
const Namespace = string(model.TypeZendeskTickets)

// Reference kinds under Namespace.
const (
	// RefKindUser names a Zendesk user: comment authors, requesters and
	// assignees.
	RefKindUser = "user"
	// RefKindAudit names a ticket audit; ParentID is its ticket.
	RefKindAudit = "audit"
)

// Assurance is the source assurance of inbound authors: the author ID comes
// from a signed webhook or an authenticated API read, never from content.
const Assurance = "provider_authenticated"

// PayloadComment names the provider payload of an inbound comment (version
// 1): {"author_name": ..., "author_is_staff": ...}, display data only, never
// identity.
const PayloadComment = string(model.TypeZendeskTickets) + ".comment"

// statusMap is the declared mapping of Zendesk ticket statuses onto work
// item statuses. Custom statuses report their category, which is one of
// these. "deleted" and anything else map to no status; the raw status is
// always kept.
var statusMap = map[string]string{
	"new":     model.WorkItemOpen,
	"open":    model.WorkItemOpen,
	"pending": model.WorkItemPending,
	"hold":    model.WorkItemPending,
	"solved":  model.WorkItemResolved,
	"closed":  model.WorkItemClosed,
}

// StatusMapping returns the declared status mapping: Zendesk status (or
// status category) to work item status.
func StatusMapping() map[string]string {
	out := make(map[string]string, len(statusMap))
	for k, v := range statusMap {
		out[k] = v
	}
	return out
}

func userRef(ch model.ChannelRef, id zid) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: Namespace, Kind: RefKindUser, ID: string(id)}
}

// ticketRef is the work item reference of a ticket: the scoping reference
// of its conversation and the reply route of every effect on it.
func ticketRef(ch model.ChannelRef, id zid) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: Namespace, Kind: model.RefKindWorkItem, ID: string(id)}
}

func auditRef(ch model.ChannelRef, ticketID string, id zid) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: Namespace, Kind: RefKindAudit, ID: string(id), ParentID: ticketID}
}

// isTicketRoute reports whether route names a ticket of ch.
func isTicketRoute(ch model.ChannelRef, route model.ExternalRef) bool {
	return route.Channel == ch && route.Namespace == Namespace && route.Kind == model.RefKindWorkItem && route.ID != ""
}

// state is the work item state of t: an active binding unless binding
// says otherwise.
func (x *instance) state(t ticket, binding string, successor *model.ExternalRef) *model.WorkItemState {
	ch := x.channel.Ref
	raw := strings.ToLower(t.Status)
	s := &model.WorkItemState{
		Subject: t.Subject, RawStatus: raw, Status: statusMap[raw],
		URL:         x.ticketURL(t.ID),
		Binding:     binding,
		Successor:   successor,
		LocalFields: append([]string(nil), x.cfg.LocalFields...),
	}
	if t.RequesterID != "" {
		s.Requester = userRef(ch, t.RequesterID)
	}
	if t.AssigneeID != "" {
		s.Assignees = []model.ExternalRef{userRef(ch, t.AssigneeID)}
	}
	return s
}

func (x *instance) ticketURL(id zid) string {
	return "https://" + x.cfg.Subdomain + ".zendesk.com/agent/tickets/" + string(id)
}

// textOf returns the text of content made of text parts only.
func textOf(c model.Content) (string, bool) {
	var b strings.Builder
	for i, p := range c.Parts {
		if p.Kind != model.PartText || p.Attachment != nil || p.Structured != nil {
			return "", false
		}
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(p.Text)
	}
	return b.String(), b.Len() > 0
}

func textContent(s string) model.Content {
	if s == "" {
		return model.Content{}
	}
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}}
}

// audience is the audience of an inbound comment: public only when the
// provider says so explicitly, restricted otherwise.
func audience(public bool) model.Audience {
	if public {
		return model.Audience{Visibility: model.Public}
	}
	return model.Audience{Visibility: model.Restricted}
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// messageEventID is the dedup identity of a comment: one per comment, so a
// comment read by a poll and by a webhook is ingested once.
func messageEventID(id zid) string { return "comment:" + string(id) }
