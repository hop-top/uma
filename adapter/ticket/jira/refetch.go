package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// issueFields lists the issue fields a read asks for.
const issueFields = "summary,description,status,labels,project,reporter,creator,assignee,created,updated"

// Refetch reads an issue or comment again when two webhooks reported it
// under the same source revision with different content: Jira's copy now
// is the authoritative one. A work item target is the issue's state (a
// work_item envelope); a comment target is its current body (a
// message_edited envelope), located through the work item reference the
// tied observation carried. Reads retry on any 5xx or transport failure.
func (x *Instance) Refetch(ctx context.Context, req adapter.RefetchRequest) (model.Envelope, error) {
	if err := x.ready(); err != nil {
		return model.Envelope{}, err
	}
	t := req.Target
	wi := t
	if t.Kind != model.RefKindWorkItem {
		wi = model.ExternalRef{}
		for _, r := range req.References {
			if r.Channel == x.channel.Ref && r.Namespace == Namespace && r.Kind == model.RefKindWorkItem {
				wi = r
				break
			}
		}
	}
	issueID, err := x.parseRoute(wi)
	if err != nil {
		return model.Envelope{}, err
	}
	switch {
	case t.Kind == model.RefKindWorkItem:
		iss, err := x.getIssue(ctx, issueID)
		if err != nil {
			return model.Envelope{}, err
		}
		n := normalizer{x: x, p: &payload{}, issue: iss, ref: wi, destination: req.Destination}
		if p := n.project(); p != "" {
			n.destination = p
		}
		return n.workItem(n.state(iss), firstSet(parseTime(iss.Fields.Updated))), nil
	case t.Channel != x.channel.Ref || t.Namespace != model.ProviderMessageNamespace || t.Kind != model.RefKindMessage:
		return model.Envelope{}, final(model.Validation, "refetch target is not a Jira message of this channel")
	case strings.HasPrefix(t.ID, commentPrefix):
		id := strings.TrimPrefix(t.ID, commentPrefix)
		if !idOK(id) {
			return model.Envelope{}, final(model.Validation, "malformed comment message ID")
		}
		a, err := x.send(ctx, http.MethodGet, commentPath(issueID, id), nil, nil)
		if err != nil || a.status != http.StatusOK {
			return model.Envelope{}, x.readError(ctx, a, err)
		}
		var c comment
		if json.Unmarshal(a.body, &c) != nil || c.ID != id {
			return model.Envelope{}, model.NewError(model.Transient, "Jira answer unreadable")
		}
		n := normalizer{x: x, p: &payload{}, issue: &issue{ID: issueID}, ref: wi, destination: req.Destination}
		env := n.update(model.EnvelopeMessageEdited, t.ID, bodyText(c.Body), firstSet(parseTime(c.Updated), parseTime(c.Created)), firstUser(c.UpdateAuthor, c.Author)).ingress.Envelope
		env.Audience = audienceOf(c)
		return env, nil
	}
	return model.Envelope{}, final(model.Validation, "refetch target is not a Jira message")
}

func (x *Instance) getIssue(ctx context.Context, issueID string) (*issue, error) {
	a, err := x.send(ctx, http.MethodGet, issuePath(issueID), url.Values{"fields": {issueFields}}, nil)
	if err != nil || a.status != http.StatusOK {
		return nil, x.readError(ctx, a, err)
	}
	var iss issue
	if json.Unmarshal(a.body, &iss) != nil || iss.ID != issueID {
		return nil, model.NewError(model.Transient, "Jira answer unreadable")
	}
	return &iss, nil
}
