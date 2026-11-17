package linear

import (
	"context"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Refetch reads an issue or comment again when two webhooks reported it
// under the same revision with different content: Linear's copy now is the
// authoritative one. A work item target is the issue's state (a work_item
// envelope); a comment or description target is its current body (a
// message_edited envelope).
func (x *Instance) Refetch(ctx context.Context, req adapter.RefetchRequest) (model.Envelope, error) {
	if err := x.ready(); err != nil {
		return model.Envelope{}, err
	}
	t := req.Target
	switch {
	case t.Kind == model.RefKindWorkItem:
		id, err := x.parseRoute(t)
		if err != nil {
			return model.Envelope{}, err
		}
		iss, err := x.readIssue(ctx, id)
		if err != nil {
			return model.Envelope{}, err
		}
		v := viewOfIssue(iss)
		n := normalizer{x: x, dest: v.team, ref: t, h: hook{}}
		return n.workItem(n.state(v), v.updated), nil
	case t.Channel != x.channel.Ref || t.Namespace != model.ProviderMessageNamespace || t.Kind != model.RefKindMessage:
		return model.Envelope{}, final(model.Validation, "refetch target is not a Linear message of this channel")
	}
	if id, ok := strings.CutPrefix(t.ID, "comment:"); ok && validUUID(id) {
		c, gone, err := x.readComment(ctx, id)
		switch {
		case err != nil:
			return model.Envelope{}, err
		case gone || c.Issue == nil:
			return model.Envelope{}, final(model.NotFound, "comment not found")
		}
		n := normalizer{x: x, dest: c.Issue.Team.Key, ref: workItemRef(x.channel.Ref, c.Issue.ID)}
		d := n.update(model.EnvelopeMessageEdited, t.ID, bodyText(c.Body), firstTime(parseTime(derefStr(c.EditedAt)), parseTime(c.UpdatedAt)))
		author := ""
		if c.User != nil {
			author = c.User.ID
		} else if c.BotActor != nil {
			author = c.BotActor.ID
		}
		d.ingress.Envelope.Sender = n.user(author)
		return d.ingress.Envelope, nil
	}
	if id, ok := strings.CutPrefix(t.ID, "issue:"); ok && validUUID(id) {
		wi, err := x.parseRoute(firstWorkItem(x.channel.Ref, req.References))
		if err != nil {
			return model.Envelope{}, err
		}
		iss, err := x.readIssue(ctx, wi)
		if err != nil {
			return model.Envelope{}, err
		}
		if iss.ID != id {
			return model.Envelope{}, final(model.NotFound, "description belongs to another issue")
		}
		v := viewOfIssue(iss)
		n := normalizer{x: x, dest: v.team, ref: workItemRef(x.channel.Ref, v.id)}
		d := n.update(model.EnvelopeMessageEdited, t.ID, v.description, v.updated)
		d.ingress.Envelope.Sender = n.user(v.creator)
		return d.ingress.Envelope, nil
	}
	return model.Envelope{}, final(model.Validation, "refetch target is not a Linear message")
}

func firstWorkItem(ch model.ChannelRef, refs []model.ExternalRef) model.ExternalRef {
	for _, r := range refs {
		if r.Channel == ch && r.Namespace == Namespace && r.Kind == model.RefKindWorkItem {
			return r
		}
	}
	return model.ExternalRef{}
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
