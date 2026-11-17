package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// ReconcileWindow bounds how far back Reconcile looks for an attempt's
// comment: comments created earlier are not listed.
const ReconcileWindow = 7 * 24 * time.Hour

// reconcilePages bounds the comment pages one reconciliation reads, and
// reconcileLookups the property reads: past either the result is
// inconclusive.
const (
	reconcilePages   = 20
	reconcileLookups = 50
	pageSize         = 100
)

const (
	commentPrefix = "comment:"
	issuePrefix   = "issue:"
)

func commentMessageID(id string) string { return commentPrefix + id }
func issueMessageID(id string) string   { return issuePrefix + id }

// workItemRef returns the reference of the issue with numeric ID id on ch.
// It names no project: an issue keeps its ID when its key changes or it
// moves to another project.
func workItemRef(ch model.ChannelRef, id string) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: Namespace, Kind: model.RefKindWorkItem, ID: id}
}

// parseRoute checks that r is a work item reference of the instance's
// channel and returns the issue ID it names.
func (x *Instance) parseRoute(r model.ExternalRef) (string, error) {
	if r.Channel != x.channel.Ref || r.Namespace != Namespace || r.Kind != model.RefKindWorkItem || !idOK(r.ID) {
		return "", final(model.Validation, "route is not a Jira issue of this channel")
	}
	return r.ID, nil
}

func issuePath(id string) string { return "/rest/api/3/issue/" + id }

func commentsPath(issueID string) string { return issuePath(issueID) + "/comment" }

func commentPath(issueID, id string) string { return issuePath(issueID) + "/comment/" + id }

func propertyPath(commentID string) string {
	return "/rest/api/3/comment/" + commentID + "/properties/" + url.PathEscape(EchoPropertyKey)
}

// commentBody is the request body of creating or editing a comment: the
// ADF body and the echo property of the attempt.
func commentBody(c model.Content, op model.OperationRef, attempt model.ID) (map[string]any, error) {
	doc, err := renderADF(c)
	if err != nil {
		return nil, err
	}
	return map[string]any{"body": doc, "properties": []propertyEntry{echoEntry(op, attempt)}}, nil
}

// Send posts content as a comment on the issue the route names. Only public
// audiences are sent: the comment has no visibility restriction, so every
// reader of the issue sees it.
func (x *Instance) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	if !x.cfg.Replies {
		return adapter.Outcome{}, final(model.Disabled, "replies are not enabled for the channel")
	}
	if err := x.ready(); err != nil {
		return adapter.Outcome{}, err
	}
	issueID, err := x.parseRoute(r.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if r.Audience.Visibility != model.Public {
		return adapter.Outcome{}, final(model.Unsupported, "Jira comments are sent public")
	}
	body, err := commentBody(r.Content, r.Operation, r.Attempt)
	if err != nil {
		return adapter.Outcome{}, err
	}
	a, err := x.send(ctx, http.MethodPost, commentsPath(issueID), nil, body)
	if err != nil || a.status != http.StatusCreated {
		return x.failure(ctx, a, err, commentCall)
	}
	var c comment
	if json.Unmarshal(a.body, &c) != nil || !idOK(c.ID) {
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Jira answer has no comment ID; reconcile before retrying")
	}
	return x.accepted(c.ID), nil
}

func (x *Instance) accepted(commentID string) adapter.Outcome {
	return adapter.Outcome{
		State:      model.ProviderAccepted,
		References: []model.ExternalRef{model.ProviderMessageRef(x.channel.Ref, commentMessageID(commentID))},
	}
}

// target is the provider message of a mutation: a comment of an issue.
type target struct{ issueID, commentID string }

// mutationTarget checks that mutations are enabled and the instance can
// call Jira, and parses the effect's route and target. The target is a
// comment of this channel (comment:<id>). The description of an issue
// (issue:<id>) is not editable or deletable: that is a final rejection,
// unsupported. No authorship is checked: Jira answers for what the
// credential may change.
func (x *Instance) mutationTarget(e adapter.Effect) (target, *model.Error, error) {
	if !x.cfg.Replies {
		return target{}, nil, final(model.Disabled, "replies are not enabled for the channel")
	}
	if err := x.ready(); err != nil {
		return target{}, nil, err
	}
	issueID, err := x.parseRoute(e.Route)
	if err != nil {
		return target{}, nil, err
	}
	t := e.Target
	bad := final(model.Validation, "target is not a Jira message of this channel")
	if t == nil || t.Channel != x.channel.Ref || t.Namespace != model.ProviderMessageNamespace || t.Kind != model.RefKindMessage {
		return target{}, nil, bad
	}
	if strings.HasPrefix(t.ID, issuePrefix) && idOK(strings.TrimPrefix(t.ID, issuePrefix)) {
		return target{}, final(model.Unsupported, "an issue description cannot be edited or deleted"), nil
	}
	id, ok := strings.CutPrefix(t.ID, commentPrefix)
	if !ok || !idOK(id) {
		return target{}, nil, bad
	}
	return target{issueID: issueID, commentID: id}, nil, nil
}

// Edit replaces the body of the comment the target names with the rendered
// content and sets the attempt's echo property. Only public text is sent.
// Any comment the credential may change is edited, another user's
// included; Jira's refusal (403, or 404 for a comment that is gone) is a
// final rejection, never a new comment.
func (x *Instance) Edit(ctx context.Context, r adapter.EditRequest) (adapter.Outcome, error) {
	t, refusal, err := x.mutationTarget(r.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if refusal != nil {
		return adapter.Outcome{State: model.Rejected, Failure: refusal}, nil
	}
	if r.Audience.Visibility != model.Public {
		return rejected(model.Unsupported, "Jira comments are sent public"), nil
	}
	body, err := commentBody(r.Content, r.Operation, r.Attempt)
	if err != nil {
		if me, ok := err.(*model.Error); ok {
			return adapter.Outcome{State: model.Rejected, Failure: me}, nil
		}
		return adapter.Outcome{}, err
	}
	a, err := x.send(ctx, http.MethodPut, commentPath(t.issueID, t.commentID), nil, body)
	if err != nil || a.status != http.StatusOK {
		return x.failure(ctx, a, err, mutateCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{*r.Target}}, nil
}

// Delete deletes the comment the target names (204).
func (x *Instance) Delete(ctx context.Context, r adapter.DeleteRequest) (adapter.Outcome, error) {
	t, refusal, err := x.mutationTarget(r.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if refusal != nil {
		return adapter.Outcome{State: model.Rejected, Failure: refusal}, nil
	}
	a, err := x.send(ctx, http.MethodDelete, commentPath(t.issueID, t.commentID), nil, nil)
	if err != nil || a.status != http.StatusNoContent {
		return x.failure(ctx, a, err, mutateCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

// Reconcile resolves an attempt whose answer was lost, by reading what Jira
// holds; it never resends.
//
//   - reply: looks for the comment of the attempt among the issue's
//     comments created within ReconcileWindow by the channel's account,
//     newest first, reading each one's echo property. Found is
//     provider_accepted with its reference; a complete listing without it
//     is failed with not_found, so a retry is safe; a listing cut short is
//     inconclusive (state unknown).
//   - edit: the comment's echo property names the attempt: applied; the
//     comment is gone: rejected not_found; any other: failed not_found.
//   - delete: the comment is gone: applied; still there: failed not_found.
func (x *Instance) Reconcile(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	switch req.Capability {
	case model.CapabilityEdit, model.CapabilityDelete:
		return x.reconcileMutation(ctx, req)
	}
	if !x.cfg.Replies {
		return adapter.Outcome{}, final(model.Disabled, "replies are not enabled for the channel")
	}
	if err := x.ready(); err != nil {
		return adapter.Outcome{}, err
	}
	issueID, err := x.parseRoute(req.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	since := x.clock.Now().Add(-ReconcileWindow)
	lookups := 0
	for page := range reconcilePages {
		q := url.Values{
			"orderBy": {"-created"}, "maxResults": {strconv.Itoa(pageSize)}, "startAt": {strconv.Itoa(page * pageSize)},
		}
		a, err := x.send(ctx, http.MethodGet, commentsPath(issueID), q, nil)
		if err != nil || a.status != http.StatusOK {
			return adapter.Outcome{}, x.readError(ctx, a, err)
		}
		var list struct {
			Comments []comment `json:"comments"`
			Total    int       `json:"total"`
		}
		if json.Unmarshal(a.body, &list) != nil {
			return adapter.Outcome{}, model.NewError(model.Transient, "Jira answer unreadable")
		}
		for _, c := range list.Comments {
			if at := parseTime(c.Created); !at.IsZero() && at.Before(since) {
				return notFound(), nil
			}
			if !x.own(c) || !idOK(c.ID) {
				continue
			}
			if lookups++; lookups > reconcileLookups {
				return adapter.Outcome{State: model.Unknown}, nil
			}
			op, att, found, err := x.echoProperty(ctx, c.ID)
			if err != nil {
				return adapter.Outcome{}, err
			}
			if found && op == req.Operation.ID && att == req.Attempt {
				return x.accepted(c.ID), nil
			}
		}
		if len(list.Comments) == 0 || (page+1)*pageSize >= list.Total {
			return notFound(), nil
		}
	}
	return adapter.Outcome{State: model.Unknown}, nil
}

func notFound() adapter.Outcome {
	return adapter.Outcome{State: model.Failed, Failure: final(model.NotFound, "no comment recorded for the attempt")}
}

// own reports whether c was written by the channel's account.
func (x *Instance) own(c comment) bool {
	return x.cfg.AccountID != "" && c.Author != nil && c.Author.AccountID == x.cfg.AccountID
}

// echoProperty reads the echo property of a comment; found is false when
// it has none (404).
func (x *Instance) echoProperty(ctx context.Context, commentID string) (op, attempt model.ID, found bool, err error) {
	a, err := x.send(ctx, http.MethodGet, propertyPath(commentID), nil, nil)
	if err == nil && a.status == http.StatusNotFound {
		return "", "", false, nil
	}
	if err != nil || a.status != http.StatusOK {
		return "", "", false, x.readError(ctx, a, err)
	}
	var p propertyEntry
	if json.Unmarshal(a.body, &p) != nil {
		return "", "", false, model.NewError(model.Transient, "Jira answer unreadable")
	}
	op, attempt, found = echoOf([]propertyEntry{{Key: EchoPropertyKey, Value: p.Value}})
	return op, attempt, found, nil
}

func (x *Instance) reconcileMutation(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	t, refusal, err := x.mutationTarget(req.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if refusal != nil {
		return adapter.Outcome{State: model.Rejected, Failure: refusal}, nil
	}
	a, err := x.send(ctx, http.MethodGet, commentPath(t.issueID, t.commentID), nil, nil)
	if err == nil && a.status == http.StatusNotFound {
		if req.Capability == model.CapabilityDelete {
			return adapter.Outcome{State: model.ProviderAccepted}, nil
		}
		return rejected(model.NotFound, "comment not found or inaccessible"), nil
	}
	if err != nil || a.status != http.StatusOK {
		return adapter.Outcome{}, x.readError(ctx, a, err)
	}
	if req.Capability == model.CapabilityEdit {
		op, att, found, err := x.echoProperty(ctx, t.commentID)
		if err != nil {
			return adapter.Outcome{}, err
		}
		if found && op == req.Operation.ID && att == req.Attempt {
			return adapter.Outcome{State: model.ProviderAccepted}, nil
		}
	}
	return adapter.Outcome{State: model.Failed, Failure: final(model.NotFound, "Jira does not hold the change")}, nil
}
