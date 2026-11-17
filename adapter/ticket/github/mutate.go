package github

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v92/github"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// RefKindReaction is the reference kind of a GitHub reaction: ID the
// reaction's numeric ID, parent ID the provider message ID it is on
// ("comment:<id>" or "issue:<id>"). An applied reaction reports it in its
// outcome; the engine keeps it on the reaction, never on the message.
const RefKindReaction = "reaction"

// reactionContents maps the emoji uma accepts to the reaction contents
// GitHub offers, and back. An emoji is compared byte for byte: a variant
// (a skin tone, or a heart without U+FE0F) is another emoji and unmapped.
var reactionContents = []struct{ emoji, content string }{
	{"👍", "+1"}, {"👎", "-1"}, {"😄", "laugh"}, {"😕", "confused"},
	{"❤️", "heart"}, {"🎉", "hooray"}, {"🚀", "rocket"}, {"👀", "eyes"},
}

func contentOf(emoji string) (string, bool) {
	for _, rc := range reactionContents {
		if rc.emoji == emoji {
			return rc.content, true
		}
	}
	return "", false
}

// messageTarget is the provider message of a mutation: a comment, or the
// description of the issue the route names (comment is zero).
type messageTarget struct {
	route   issueRoute
	id      string
	comment int64
}

// refuse is the final rejection of a mutation the adapter or GitHub will
// not carry out.
func refuse(code model.ErrorCode, msg string) adapter.Outcome {
	return adapter.Outcome{State: model.Rejected, Failure: final(code, msg)}
}

// refusal is refuse for an error from rendering content.
func refusal(err error) (adapter.Outcome, error) {
	me, ok := err.(*model.Error)
	if !ok {
		return adapter.Outcome{}, err
	}
	return adapter.Outcome{State: model.Rejected, Failure: me}, nil
}

// mutationTarget checks that mutations are enabled and the instance can
// call GitHub, and parses the effect's route and target. The target is a
// provider message of this channel: comment:<id>, or issue:<id> for the
// route's issue description. No authorship is checked: GitHub answers for
// what the token may change.
func (x *Instance) mutationTarget(e adapter.Effect) (messageTarget, error) {
	if !x.cfg.Replies {
		return messageTarget{}, final(model.Disabled, "replies are not enabled for the channel")
	}
	if err := x.ready(); err != nil {
		return messageTarget{}, err
	}
	route, err := x.parseRoute(e.Route)
	if err != nil {
		return messageTarget{}, err
	}
	t := e.Target
	bad := final(model.Validation, "target is not a GitHub message of this channel")
	if t == nil || t.Channel != x.channel.Ref || t.Namespace != model.ProviderMessageNamespace || t.Kind != model.RefKindMessage {
		return messageTarget{}, bad
	}
	kind, num, ok := strings.Cut(t.ID, ":")
	n, perr := strconv.ParseInt(num, 10, 64)
	if !ok || perr != nil || n <= 0 || strconv.FormatInt(n, 10) != num {
		return messageTarget{}, bad
	}
	switch kind {
	case "comment":
		return messageTarget{route: route, id: t.ID, comment: n}, nil
	case "issue":
		return messageTarget{route: route, id: t.ID}, nil
	}
	return messageTarget{}, bad
}

// Edit replaces the body of the comment the target names, or of the issue
// description, with the rendered content and the marker of the attempt.
// Only public text is sent. Any comment the token may change is edited,
// another user's included; GitHub's refusal (403, or 404 for a comment
// that is gone) is a final rejection, never a new comment.
func (x *Instance) Edit(ctx context.Context, r adapter.EditRequest) (adapter.Outcome, error) {
	t, err := x.mutationTarget(r.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if r.Audience.Visibility != model.Public {
		return refuse(model.Unsupported, "GitHub comments are public"), nil
	}
	body, err := renderBody(r.Content, r.Operation, r.Attempt)
	if err != nil {
		return refusal(err)
	}
	var resp *gh.Response
	if t.comment != 0 {
		_, resp, err = call(ctx, x, func(ctx context.Context) (*gh.IssueComment, *gh.Response, error) {
			return x.client.Issues.UpdateComment(ctx, t.route.owner, t.route.repo, t.comment, gh.IssueCommentRequest{Body: body})
		})
	} else {
		_, resp, err = call(ctx, x, func(ctx context.Context) (*gh.Issue, *gh.Response, error) {
			return x.client.Issues.Update(ctx, t.route.owner, t.route.repo, t.route.number, gh.UpdateIssueRequest{Body: &body})
		})
	}
	if err != nil {
		return x.failure(ctx, resp, err, mutateCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

// Delete deletes the comment the target names. An issue description
// cannot be deleted: that is a final rejection, unsupported.
func (x *Instance) Delete(ctx context.Context, r adapter.DeleteRequest) (adapter.Outcome, error) {
	t, err := x.mutationTarget(r.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if t.comment == 0 {
		return refuse(model.Unsupported, "an issue description cannot be deleted"), nil
	}
	resp, err := x.deleteComment(ctx, t)
	if err != nil {
		return x.failure(ctx, resp, err, mutateCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

func (x *Instance) deleteComment(ctx context.Context, t messageTarget) (*gh.Response, error) {
	_, resp, err := call(ctx, x, func(ctx context.Context) (struct{}, *gh.Response, error) {
		resp, err := x.client.Issues.DeleteComment(ctx, t.route.owner, t.route.repo, t.comment)
		return struct{}{}, resp, err
	})
	return resp, err
}

// React adds the account's reaction to the comment or issue description
// the target names, or removes it. The emoji maps to GitHub's content
// (👍 +1, 👎 -1, 😄 laugh, 😕 confused, ❤️ heart, 🎉 hooray, 🚀 rocket,
// 👀 eyes); any other emoji is a final rejection, unsupported, before any
// call. Adding what the account already holds is GitHub's 200 and applied.
// A removal looks up the account's own reaction with that content and
// deletes it; none is an applied no-op.
func (x *Instance) React(ctx context.Context, r adapter.ReactRequest) (adapter.Outcome, error) {
	t, err := x.mutationTarget(r.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	content, ok := contentOf(r.Emoji)
	if !ok {
		return refuse(model.Unsupported, "GitHub has no reaction for this emoji"), nil
	}
	if r.Remove {
		return x.unreact(ctx, t, content)
	}
	rx, resp, err := call(ctx, x, func(ctx context.Context) (*gh.Reaction, *gh.Response, error) {
		if t.comment != 0 {
			return x.client.Reactions.CreateIssueCommentReaction(ctx, t.route.owner, t.route.repo, t.comment, content)
		}
		return x.client.Reactions.CreateIssueReaction(ctx, t.route.owner, t.route.repo, t.route.number, content)
	})
	if err != nil {
		return x.failure(ctx, resp, err, mutateCall)
	}
	if rx.GetID() == 0 {
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "GitHub answer has no reaction ID; reconcile before retrying")
	}
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{x.reactionRef(t, rx.GetID())}}, nil
}

func (x *Instance) reactionRef(t messageTarget, id int64) model.ExternalRef {
	return model.ExternalRef{
		Channel: x.channel.Ref, Namespace: Namespace, Kind: RefKindReaction,
		ID: strconv.FormatInt(id, 10), ParentID: t.id,
	}
}

func (x *Instance) unreact(ctx context.Context, t messageTarget, content string) (adapter.Outcome, error) {
	rx, out, err := x.ownReaction(ctx, t, content, lookupCall)
	if err != nil || out.State != "" {
		return out, err
	}
	if rx == nil {
		return adapter.Outcome{State: model.ProviderAccepted}, nil
	}
	_, resp, err := call(ctx, x, func(ctx context.Context) (struct{}, *gh.Response, error) {
		var resp *gh.Response
		var err error
		if t.comment != 0 {
			resp, err = x.client.Reactions.DeleteIssueCommentReaction(ctx, t.route.owner, t.route.repo, t.comment, rx.GetID())
		} else {
			resp, err = x.client.Reactions.DeleteIssueReaction(ctx, t.route.owner, t.route.repo, t.route.number, rx.GetID())
		}
		return struct{}{}, resp, err
	})
	if err != nil {
		if status(resp) == http.StatusNotFound {
			// Removed meanwhile: the reaction is gone, as asked.
			return adapter.Outcome{State: model.ProviderAccepted}, nil
		}
		return x.failure(ctx, resp, err, mutateCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

// ownReaction returns the channel account's reaction with content on the
// target, nil when it has none, reading the target's reactions of that
// content page by page. A failed read is the outcome and error of kind.
// Reading stops after reconcilePages pages: a longer list is a transient
// error.
func (x *Instance) ownReaction(ctx context.Context, t messageTarget, content string, kind callKind) (*gh.Reaction, adapter.Outcome, error) {
	opts := &gh.ListReactionOptions{Content: content, ListOptions: gh.ListOptions{PerPage: 100}}
	for range reconcilePages {
		rs, resp, err := call(ctx, x, func(ctx context.Context) ([]*gh.Reaction, *gh.Response, error) {
			if t.comment != 0 {
				return x.client.Reactions.ListIssueCommentReactions(ctx, t.route.owner, t.route.repo, t.comment, opts)
			}
			return x.client.Reactions.ListIssueReactions(ctx, t.route.owner, t.route.repo, t.route.number, opts)
		})
		if err != nil {
			out, ferr := x.failure(ctx, resp, err, kind)
			return nil, out, ferr
		}
		for _, rx := range rs {
			if rx.GetContent() == content && x.cfg.AccountID != 0 && rx.GetUser().GetID() == x.cfg.AccountID {
				return rx, adapter.Outcome{}, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return nil, adapter.Outcome{}, nil
		}
		opts.Page = resp.NextPage
	}
	return nil, adapter.Outcome{}, model.NewError(model.Transient, "too many reactions to find the account's own")
}

// reconcileMutation resolves a mutation whose answer was lost, by reading
// what GitHub holds:
//
//   - edit: the comment (or issue) body ends with the attempt's marker →
//     applied; a comment gone is rejected not_found; any other body means
//     the attempt did not apply.
//   - delete: the comment is gone (404 or 410) → applied; present → not
//     applied.
//   - react: the account holds the reaction (or, for a removal, does not)
//     → applied; otherwise not applied.
//
// Not applied is failed with not_found, so a new attempt is safe.
func (x *Instance) reconcileMutation(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	t, err := x.mutationTarget(req.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	notApplied := adapter.Outcome{State: model.Failed, Failure: final(model.NotFound, "GitHub does not show the attempt's effect")}
	applied := adapter.Outcome{State: model.ProviderAccepted}
	switch req.Capability {
	case model.CapabilityEdit:
		body, out, err := x.currentBody(ctx, t)
		if err != nil || out.State != "" {
			return out, err
		}
		if op, att, _, ok := parseMarker(body); ok && op == req.Operation.ID && att == req.Attempt {
			return applied, nil
		}
		return notApplied, nil
	case model.CapabilityDelete:
		if t.comment == 0 {
			return refuse(model.Unsupported, "an issue description cannot be deleted"), nil
		}
		_, resp, err := call(ctx, x, func(ctx context.Context) (*gh.IssueComment, *gh.Response, error) {
			return x.client.Issues.GetComment(ctx, t.route.owner, t.route.repo, t.comment)
		})
		switch {
		case err == nil:
			return notApplied, nil
		case gone(resp):
			return applied, nil
		}
		return adapter.Outcome{}, x.readFailure(ctx, resp, err)
	case model.CapabilityReact:
		content, ok := contentOf(req.Emoji)
		if !ok {
			return refuse(model.Unsupported, "GitHub has no reaction for this emoji"), nil
		}
		rx, out, err := x.ownReaction(ctx, t, content, readCall)
		if err != nil || out.State != "" {
			return out, err
		}
		switch {
		case rx != nil && !req.Remove:
			return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{x.reactionRef(t, rx.GetID())}}, nil
		case rx == nil && req.Remove:
			return applied, nil
		}
		return notApplied, nil
	}
	return adapter.Outcome{}, final(model.Unsupported, "reconciliation of this capability is not supported")
}

// currentBody reads the body of the target. A comment that is gone is a
// rejected outcome, not_found.
func (x *Instance) currentBody(ctx context.Context, t messageTarget) (string, adapter.Outcome, error) {
	if t.comment == 0 {
		iss, err := x.getIssue(ctx, t.route)
		if err != nil {
			return "", adapter.Outcome{}, err
		}
		return iss.GetBody(), adapter.Outcome{}, nil
	}
	c, resp, err := call(ctx, x, func(ctx context.Context) (*gh.IssueComment, *gh.Response, error) {
		return x.client.Issues.GetComment(ctx, t.route.owner, t.route.repo, t.comment)
	})
	switch {
	case err == nil:
		return c.GetBody(), adapter.Outcome{}, nil
	case gone(resp):
		return "", refuse(model.NotFound, "comment not found"), nil
	}
	return "", adapter.Outcome{}, x.readFailure(ctx, resp, err)
}

// gone reports a 404 or 410 answer.
func gone(resp *gh.Response) bool {
	s := status(resp)
	return s == http.StatusNotFound || s == http.StatusGone
}

var (
	_ adapter.Editor  = (*Instance)(nil)
	_ adapter.Deleter = (*Instance)(nil)
	_ adapter.Reactor = (*Instance)(nil)
)
