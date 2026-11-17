package github

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	gh "github.com/google/go-github/v92/github"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// ReconcileWindow bounds how far back Reconcile looks for an attempt's
// comment: comments updated earlier are not listed.
const ReconcileWindow = 7 * 24 * time.Hour

// maxBodyRunes is GitHub's comment body limit.
const maxBodyRunes = 65536

// reconcilePages bounds the comment pages one reconciliation reads; past
// it the result is inconclusive.
const reconcilePages = 20

// issueRoute is a parsed work item reference.
type issueRoute struct {
	owner, repo string
	number      int
}

// GitHub owner and repository names: ASCII letters, digits, '-', '_' and
// '.'; never "." or "..".
var namePart = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

// workItemRef returns the reference of issue number in repository
// fullName on ch.
func workItemRef(ch model.ChannelRef, fullName string, number int) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: Namespace, Kind: model.RefKindWorkItem, ID: strconv.Itoa(number), ParentID: fullName}
}

// parseRoute checks that r is a work item reference of the instance's
// channel and returns the issue it names.
func (x *Instance) parseRoute(r model.ExternalRef) (issueRoute, error) {
	bad := final(model.Validation, "route is not a GitHub issue of this channel")
	if r.Channel != x.channel.Ref || r.Namespace != Namespace || r.Kind != model.RefKindWorkItem {
		return issueRoute{}, bad
	}
	n, err := strconv.Atoi(r.ID)
	if err != nil || n <= 0 || strconv.Itoa(n) != r.ID {
		return issueRoute{}, bad
	}
	owner, repo, ok := strings.Cut(r.ParentID, "/")
	if !ok || !validName(owner) || !validName(repo) {
		return issueRoute{}, bad
	}
	return issueRoute{owner: owner, repo: repo, number: n}, nil
}

func validName(s string) bool { return namePart.MatchString(s) && s != "." && s != ".." }

// Echo marker: the last line of a comment uma posts, an HTML comment GitHub
// does not render, naming the operation and attempt.
const (
	markerOpen  = "<!-- uma:"
	markerClose = " -->"
)

func marker(op model.OperationRef, attempt model.ID) string {
	return markerOpen + string(op.ID) + ":" + string(attempt) + markerClose
}

// parseMarker returns the operation and attempt of a body's trailing
// marker and the body without it.
func parseMarker(body string) (op, attempt model.ID, rest string, ok bool) {
	trimmed := strings.TrimRight(body, " \t\r\n")
	i := strings.LastIndex(trimmed, markerOpen)
	if i < 0 || !strings.HasSuffix(trimmed, markerClose) {
		return "", "", body, false
	}
	inner := trimmed[i+len(markerOpen) : len(trimmed)-len(markerClose)]
	a, b, found := strings.Cut(inner, ":")
	opID, err1 := model.ParseID(a)
	attID, err2 := model.ParseID(b)
	if !found || err1 != nil || err2 != nil {
		return "", "", body, false
	}
	return opID, attID, strings.TrimRight(trimmed[:i], " \t\r\n"), true
}

// renderBody returns the comment body for content: its text parts, then
// the echo marker. Other parts are unsupported.
func renderBody(c model.Content, op model.OperationRef, attempt model.ID) (string, error) {
	var parts []string
	for _, p := range c.Parts {
		if p.Kind != model.PartText {
			return "", final(model.Unsupported, "GitHub comments carry text only")
		}
		parts = append(parts, p.Text)
	}
	if len(parts) == 0 {
		return "", final(model.Validation, "content has no text")
	}
	body := strings.Join(parts, "\n\n") + "\n\n" + marker(op, attempt)
	if utf8.RuneCountInString(body) > maxBodyRunes {
		return "", final(model.Validation, "comment exceeds GitHub's body limit")
	}
	return body, nil
}

func commentMessageID(id int64) string { return "comment:" + strconv.FormatInt(id, 10) }
func issueMessageID(id int64) string   { return "issue:" + strconv.FormatInt(id, 10) }

// Send posts content as a comment on the issue the route names. Only
// public audiences are sent: an issue comment is visible to every reader
// of the repository.
func (x *Instance) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	if !x.cfg.Replies {
		return adapter.Outcome{}, final(model.Disabled, "replies are not enabled for the channel")
	}
	if err := x.ready(); err != nil {
		return adapter.Outcome{}, err
	}
	route, err := x.parseRoute(r.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if r.Audience.Visibility != model.Public {
		return adapter.Outcome{}, final(model.Unsupported, "GitHub comments are public")
	}
	body, err := renderBody(r.Content, r.Operation, r.Attempt)
	if err != nil {
		return adapter.Outcome{}, err
	}
	c, resp, err := call(ctx, x, func(ctx context.Context) (*gh.IssueComment, *gh.Response, error) {
		return x.client.Issues.CreateComment(ctx, route.owner, route.repo, route.number, gh.IssueCommentRequest{Body: body})
	})
	if err != nil {
		return x.failure(ctx, resp, err, commentCall)
	}
	if c == nil || c.GetID() == 0 {
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "GitHub answer has no comment ID; reconcile before retrying")
	}
	return x.accepted(c), nil
}

func (x *Instance) accepted(c *gh.IssueComment) adapter.Outcome {
	return adapter.Outcome{
		State:      model.ProviderAccepted,
		References: []model.ExternalRef{model.ProviderMessageRef(x.channel.Ref, commentMessageID(c.GetID()))},
	}
}

// Reconcile resolves an attempt whose answer was lost. A work item act
// (req.Status set) is resolved by reading the issue; see reconcileAct. An
// edit, delete or reaction (req.Capability) is resolved by reading the
// target; see reconcileMutation. An update effect is resolved by reading
// the issue; see reconcileUpdate.
//
// For a reply it looks for the comment of the attempt: a comment by the
// channel's account, updated within ReconcileWindow, whose marker names
// the attempt's operation and attempt. Found is provider_accepted with its
// reference; a complete listing without it is failed with not_found, so a
// retry is safe; a listing cut short is inconclusive (state unknown).
func (x *Instance) Reconcile(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	if req.Status != "" {
		return x.reconcileAct(ctx, req)
	}
	switch req.Capability {
	case model.CapabilityEdit, model.CapabilityDelete, model.CapabilityReact:
		return x.reconcileMutation(ctx, req)
	case model.CapabilityExtensionExecute:
		return x.reconcileUpdate(ctx, req)
	}
	e := req.Effect
	if !x.cfg.Replies {
		return adapter.Outcome{}, final(model.Disabled, "replies are not enabled for the channel")
	}
	if err := x.ready(); err != nil {
		return adapter.Outcome{}, err
	}
	route, err := x.parseRoute(e.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	since := x.clock.Now().Add(-ReconcileWindow).UTC()
	opts := &gh.IssueListCommentsOptions{Since: &since, ListOptions: gh.ListOptions{PerPage: 100}}
	for range reconcilePages {
		cs, resp, err := call(ctx, x, func(ctx context.Context) ([]*gh.IssueComment, *gh.Response, error) {
			return x.client.Issues.ListComments(ctx, route.owner, route.repo, route.number, opts)
		})
		if err != nil {
			return adapter.Outcome{}, x.readFailure(ctx, resp, err)
		}
		for _, c := range cs {
			if x.own(c) {
				if op, att, _, ok := parseMarker(c.GetBody()); ok && op == e.Operation.ID && att == e.Attempt {
					return x.accepted(c), nil
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return adapter.Outcome{State: model.Failed, Failure: final(model.NotFound, "no comment recorded for the attempt")}, nil
		}
		opts.Page = resp.NextPage
	}
	return adapter.Outcome{State: model.Unknown}, nil
}

// own reports whether c was written by the channel's account.
func (x *Instance) own(c *gh.IssueComment) bool {
	return x.cfg.AccountID != 0 && c.GetUser().GetID() == x.cfg.AccountID
}
