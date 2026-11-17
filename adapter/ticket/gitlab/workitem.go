package gitlab

import (
	"context"
	"net/http"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// stateEvent is the state_event of an issue update for a requested status,
// and the issue state GitLab then holds.
type issueAct struct{ event, state string }

// toIssueAct maps a requested status and reason to the update that applies
// it. GitLab has no close reason, so any reason is unsupported, as is any
// status other than open and closed.
func toIssueAct(status, reason string) (issueAct, error) {
	if reason != "" {
		return issueAct{}, final(model.Unsupported, "GitLab issues have no state reason")
	}
	switch status {
	case model.WorkItemClosed:
		return issueAct{"close", "closed"}, nil
	case model.WorkItemOpen:
		return issueAct{"reopen", "opened"}, nil
	}
	return issueAct{}, final(model.Unsupported, "GitLab issues are only open or closed")
}

// ActWorkItem closes or reopens the issue the route names with one update
// (state_event close or reopen), whatever the issue's state. An applied
// change reports the issue state GitLab returned, with its updated_at as
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
	a, err := x.call(ctx, spec{method: http.MethodPut, path: route.issuePath(), body: map[string]string{"state_event": act.event}})
	if err != nil || !a.ok() {
		out, ferr := x.failure(ctx, a, err, issueCall)
		return adapter.WorkItemActOutcome{Outcome: out}, ferr
	}
	out := adapter.WorkItemActOutcome{Outcome: adapter.Outcome{State: model.Delivered}}
	iss, ok := decodeIssue(a.body)
	if !ok || iss.IID != route.iid {
		// Applied, but the answer names no issue state to import.
		return out, nil
	}
	n := normalizer{x: x, ev: &event{}, dest: iss.destination(), ref: r.Route}
	s := n.state(iss.view())
	at := parseTime(iss.UpdatedAt)
	out.State, out.SourceRevision, out.OccurredAt = &s, revision(at), at.UTC()
	return out, nil
}

// reconcileAct reads the issue of a work item act whose answer was lost: it
// holds the requested state → delivered; anything else → failed with
// not_found, so a new attempt is safe.
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
	if iss.State == act.state {
		return adapter.Outcome{State: model.Delivered}, nil
	}
	return adapter.Outcome{State: model.Failed, Failure: final(model.NotFound, "issue does not hold the requested state")}, nil
}

// getIssue reads the issue the route names.
func (x *Instance) getIssue(ctx context.Context, route issueRoute) (apiIssue, error) {
	a, err := x.call(ctx, spec{method: http.MethodGet, path: route.issuePath()})
	if err != nil || !a.ok() {
		return apiIssue{}, x.readFailure(ctx, a, err)
	}
	iss, ok := decodeIssue(a.body)
	if !ok || iss.IID != route.iid {
		return apiIssue{}, model.NewError(model.Transient, "GitLab answer is not the issue")
	}
	return iss, nil
}
