package zendesk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"hop.top/uma/model"
)

// Inbound normalization, shared by the webhook handler and the polling
// receiver. Both put every ticket of the channel in a per-work-item
// conversation keyed by the ticket's work item reference:
//
//   - a comment is a message (one dedup identity per comment, whichever
//     path reads it) authored by its Zendesk user, carrying the ticket's
//     state; a public comment is public, any other comment (an internal
//     note, or visibility the provider does not state) is restricted and
//     never public;
//   - any other ticket change is a work_item envelope with the ticket's
//     state; a merge binds the ticket merged into its target, a deletion
//     or spam mark deleted;
//   - a comment made private narrows the message it was to restricted.
//
// The envelope destination is the ticket's brand ID, falling back to the
// subdomain when Zendesk names no brand, so inboxes bind brands.

// commentIn is one inbound comment with its ticket.
type commentIn struct {
	ticket     ticket
	comment    comment
	occurredAt time.Time
	// mayEcho reports that the comment may be the adapter's own: staff
	// authored (webhook) or created through the API (poll).
	mayEcho bool
	// auditID is the comment's audit, when the source names it.
	auditID zid
}

// echoAction is what an echo check decided.
type echoAction int

const (
	echoNone echoAction = iota // ordinary inbound comment
	echoJoin                   // a reply or delivered note echo: hand on with EchoRef
	echoDrop                   // an extension effect's echo: acknowledge, drop
)

// checkEcho reads the comment's audit for the adapter's marker.
func (x *instance) checkEcho(ctx context.Context, c commentIn) (echoAction, marker, error) {
	if !c.mayEcho {
		return echoNone, marker{}, nil
	}
	a, ok, err := x.commentAudit(ctx, string(c.ticket.ID), c.comment.ID, c.auditID)
	if err != nil || !ok || a.Metadata == nil {
		return echoNone, marker{}, err
	}
	m, ok := decodeMarker(a.Metadata.Custom)
	switch {
	case !ok:
		return echoNone, marker{}, nil
	case m.Effect == effectReply, m.Effect == effectMessageNote:
		return echoJoin, m, nil
	}
	return echoDrop, m, nil
}

// commentAudit returns the audit holding comment id: the named audit, or
// the newest page of the ticket's audits searched for it.
func (x *instance) commentAudit(ctx context.Context, ticketID string, id, auditID zid) (audit, bool, error) {
	if auditID != "" {
		var out struct {
			Audit audit `json:"audit"`
		}
		err := x.api.do(ctx, http.MethodGet, ticketPath(ticketID)+"/audits/"+url.PathEscape(string(auditID)), nil, nil, &out)
		if statusOf(err) == http.StatusNotFound {
			return audit{}, false, nil
		}
		if err != nil {
			return audit{}, false, readFailure(err)
		}
		return out.Audit, true, nil
	}
	var page auditPage
	q := url.Values{"sort_order": {"desc"}, "page[size]": {"100"}}
	err := x.api.do(ctx, http.MethodGet, ticketPath(ticketID)+"/audits", q, nil, &page)
	if statusOf(err) == http.StatusNotFound {
		return audit{}, false, nil
	}
	if err != nil {
		return audit{}, false, readFailure(err)
	}
	for _, a := range page.Audits {
		for _, e := range a.Events {
			if e.isComment() && e.ID == id {
				return a, true, nil
			}
		}
	}
	return audit{}, false, nil
}

// destination is the inbox destination of a ticket.
func (x *instance) destination(t ticket) string {
	if t.BrandID != "" {
		return string(t.BrandID)
	}
	return x.cfg.Subdomain
}

// commentIngress normalizes one comment. It reports false for a comment
// that is not ingested: the adapter's own extension echo, or one with no
// text.
func (x *instance) commentIngress(ctx context.Context, c commentIn) (model.Ingress, bool, error) {
	action, m, err := x.checkEcho(ctx, c)
	if err != nil || action == echoDrop {
		return model.Ingress{}, false, err
	}
	content := textContent(c.comment.Body)
	if len(content.Parts) == 0 {
		return model.Ingress{}, false, nil
	}
	ch := x.channel.Ref
	refs := []model.ExternalRef{ticketRef(ch, c.ticket.ID)}
	if action == echoJoin {
		refs = append(refs, m.echoRef(ch))
	}
	env := model.Envelope{
		EventID: messageEventID(c.comment.ID), MessageID: string(c.comment.ID),
		Destination: x.destination(c.ticket), Kind: model.EnvelopeMessage,
		Sender: userRef(ch, c.comment.authorID()), Assurance: Assurance,
		References: refs, Content: content, Audience: audience(c.comment.public()),
		OccurredAt: c.occurredAt,
		WorkItem:   x.state(c.ticket, model.WorkItemBindingActive, nil),
	}
	if a := c.comment.Author; a != nil {
		data, _ := json.Marshal(map[string]any{"author_name": a.Name, "author_is_staff": a.IsStaff}) // always encodes
		env.Payload = model.Extension{Name: PayloadComment, Version: 1, Data: data}
	}
	return x.ingress(env), true, nil
}

// ingress wraps an envelope.
func (x *instance) ingress(env model.Envelope) model.Ingress {
	return model.Ingress{Call: model.Call{Correlation: model.CorrelationID("zendesk-" + env.EventID)}, Envelope: env}
}
