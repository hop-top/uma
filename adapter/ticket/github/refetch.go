package github

import (
	"context"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v92/github"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Refetch reads an issue or comment again when two webhooks reported it
// under the same one-second updated_at with different content: GitHub's
// copy now is the authoritative one. A work item target is the issue's
// state (a work_item envelope); a comment or description target is its
// current body (a message_edited envelope), located through the work item
// reference the tied observation carried. Reads retry on any 5xx or
// transport failure.
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
	n := normalizer{x: x, ev: &event{}, repo: route.owner + "/" + route.repo, ref: wi}
	switch {
	case t.Kind == model.RefKindWorkItem:
		iss, err := x.getIssue(ctx, route)
		if err != nil {
			return model.Envelope{}, err
		}
		return n.workItem(n.state(iss), iss.GetUpdatedAt().Time), nil
	case t.Channel != x.channel.Ref || t.Namespace != model.ProviderMessageNamespace || t.Kind != model.RefKindMessage:
		return model.Envelope{}, final(model.Validation, "refetch target is not a GitHub message of this channel")
	case strings.HasPrefix(t.ID, "comment:"):
		id, err := strconv.ParseInt(strings.TrimPrefix(t.ID, "comment:"), 10, 64)
		if err != nil || id <= 0 {
			return model.Envelope{}, final(model.Validation, "malformed comment message ID")
		}
		c, resp, err := call(ctx, x, func(ctx context.Context) (*gh.IssueComment, *gh.Response, error) {
			return x.client.Issues.GetComment(ctx, route.owner, route.repo, id)
		})
		if err != nil {
			return model.Envelope{}, x.readFailure(ctx, resp, err)
		}
		n.ev.Sender = c.GetUser()
		return n.update(model.EnvelopeMessageEdited, t.ID, n.commentText(c), c.GetUpdatedAt().Time).ingress.Envelope, nil
	case strings.HasPrefix(t.ID, "issue:"):
		iss, err := x.getIssue(ctx, route)
		if err != nil {
			return model.Envelope{}, err
		}
		if issueMessageID(iss.GetID()) != t.ID {
			return model.Envelope{}, final(model.NotFound, "description belongs to another issue")
		}
		n.ev.Sender = iss.GetUser()
		return n.update(model.EnvelopeMessageEdited, t.ID, bodyText(iss.GetBody()), iss.GetUpdatedAt().Time).ingress.Envelope, nil
	}
	return model.Envelope{}, final(model.Validation, "refetch target is not a GitHub message")
}

func (x *Instance) getIssue(ctx context.Context, route issueRoute) (*gh.Issue, error) {
	iss, resp, err := call(ctx, x, func(ctx context.Context) (*gh.Issue, *gh.Response, error) {
		return x.client.Issues.Get(ctx, route.owner, route.repo, route.number)
	})
	if err != nil {
		return nil, x.readFailure(ctx, resp, err)
	}
	return iss, nil
}

// readFailure is the error of a failed read.
func (x *Instance) readFailure(ctx context.Context, resp *gh.Response, err error) error {
	out, ferr := x.failure(ctx, resp, err, readCall)
	if ferr == nil {
		ferr = out.Failure
	}
	return ferr
}
