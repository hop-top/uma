package github

import (
	"context"

	gh "github.com/google/go-github/v92/github"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// issueAct is the GitHub form of a work item act: the issue state and the
// state_reason sent with it.
type issueAct struct{ state, reason string }

// toIssueAct maps a requested status and reason to the PATCH that applies
// it. An empty reason is the default, sent explicitly: completed on close,
// reopened on reopen. Anything the descriptor does not declare is
// model.Unsupported.
func toIssueAct(status, reason string) (issueAct, error) {
	switch status {
	case model.WorkItemClosed:
		switch reason {
		case "", reasonCompleted:
			return issueAct{"closed", reasonCompleted}, nil
		case reasonNotPlanned:
			return issueAct{"closed", reasonNotPlanned}, nil
		}
	case model.WorkItemOpen:
		if reason == "" || reason == reasonReopened {
			return issueAct{"open", reasonReopened}, nil
		}
	default:
		return issueAct{}, final(model.Unsupported, "GitHub issues are only open or closed")
	}
	return issueAct{}, final(model.Unsupported, "GitHub has no such state reason for the transition")
}

// ActWorkItem closes or reopens the issue the route names with one PATCH,
// whatever the issue's state: GitHub applies it idempotently. An applied
// change reports the issue state GitHub returned, with its updated_at as
// source revision and time.
func (x *Instance) ActWorkItem(ctx context.Context, r adapter.WorkItemActRequest) (adapter.WorkItemActOutcome, error) {
	if err := x.ready(); err != nil {
		return adapter.WorkItemActOutcome{}, err
	}
	route, err := x.parseRoute(r.Route)
	if err != nil {
		return adapter.WorkItemActOutcome{}, err
	}
	act, err := toIssueAct(r.Status, r.Reason)
	if err != nil {
		return adapter.WorkItemActOutcome{}, err
	}
	iss, resp, err := call(ctx, x, func(ctx context.Context) (*gh.Issue, *gh.Response, error) {
		return x.client.Issues.Update(ctx, route.owner, route.repo, route.number, gh.UpdateIssueRequest{State: &act.state, StateReason: &act.reason})
	})
	if err != nil {
		out, ferr := x.failure(ctx, resp, err, issueCall)
		return adapter.WorkItemActOutcome{Outcome: out}, ferr
	}
	out := adapter.WorkItemActOutcome{Outcome: adapter.Outcome{State: model.Delivered}}
	if iss == nil || iss.GetNumber() != route.number {
		// Applied, but the answer names no issue state to import.
		return out, nil
	}
	n := normalizer{x: x, ev: &event{}, repo: route.owner + "/" + route.repo, ref: r.Route}
	s := n.state(iss)
	at := iss.GetUpdatedAt().Time
	out.State, out.SourceRevision, out.OccurredAt = &s, revision(at), at.UTC()
	return out, nil
}

// reconcileAct reads the issue of a work item act whose answer was lost:
// it holds the requested state and reason (the default when none was
// requested) → delivered; anything else → failed with not_found, so a new
// attempt is safe.
func (x *Instance) reconcileAct(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	if err := x.ready(); err != nil {
		return adapter.Outcome{}, err
	}
	route, err := x.parseRoute(req.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	act, err := toIssueAct(req.Status, req.Reason)
	if err != nil {
		return adapter.Outcome{}, err
	}
	iss, err := x.getIssue(ctx, route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if iss.GetState() == act.state && iss.GetStateReason() == act.reason {
		return adapter.Outcome{State: model.Delivered}, nil
	}
	return adapter.Outcome{State: model.Failed, Failure: final(model.NotFound, "issue does not hold the requested state")}, nil
}
