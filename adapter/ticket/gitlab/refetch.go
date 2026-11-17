package gitlab

import (
	"context"
	"net/http"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Refetch reads an issue or note again when two webhooks reported it under
// the same updated_at with different content: GitLab's copy now is the
// authoritative one. A work item target is the issue's state (a work_item
// envelope); a note or description target is its current body (a
// message_edited envelope), located through the work item reference the tied
// observation carried. Reads retry on any 5xx or transport failure.
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
	route, err := x.parseRoute(wi)
	if err != nil {
		return model.Envelope{}, err
	}
	if t.Kind != model.RefKindWorkItem && (t.Channel != x.channel.Ref || t.Namespace != model.ProviderMessageNamespace || t.Kind != model.RefKindMessage) {
		return model.Envelope{}, final(model.Validation, "refetch target is not a GitLab message of this channel")
	}
	iss, err := x.getIssue(ctx, route)
	if err != nil {
		return model.Envelope{}, err
	}
	n := normalizer{x: x, ev: &event{}, dest: iss.destination(), ref: wi}
	switch {
	case t.Kind == model.RefKindWorkItem:
		v := iss.view()
		return n.workItem(n.state(v), v.updated), nil
	case strings.HasPrefix(t.ID, "note:"):
		id, ok := canonicalInt(strings.TrimPrefix(t.ID, "note:"))
		if !ok {
			return model.Envelope{}, final(model.Validation, "malformed note message ID")
		}
		a, err := x.call(ctx, spec{method: http.MethodGet, path: route.notePath(id)})
		if err != nil || !a.ok() {
			return model.Envelope{}, x.readFailure(ctx, a, err)
		}
		note, ok := decodeNote(a.body)
		if !ok {
			return model.Envelope{}, model.NewError(model.Transient, "GitLab answer is not a note")
		}
		v := note.view()
		n.ev.User = hookUser{ID: v.author.id, Username: v.author.username}
		return n.update(model.EnvelopeMessageEdited, t.ID, bodyText(v.body), v.internal, v.updated).ingress.Envelope, nil
	case strings.HasPrefix(t.ID, "issue:"):
		if issueMessageID(iss.ID) != t.ID {
			return model.Envelope{}, final(model.NotFound, "description belongs to another issue")
		}
		v := iss.view()
		n.ev.User = hookUser{ID: v.author.id, Username: v.author.username}
		return n.update(model.EnvelopeMessageEdited, t.ID, bodyText(v.description), false, v.updated).ingress.Envelope, nil
	}
	return model.Envelope{}, final(model.Validation, "refetch target is not a GitLab message")
}
