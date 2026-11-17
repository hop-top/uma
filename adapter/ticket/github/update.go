package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"

	gh "github.com/google/go-github/v92/github"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Extension effects the adapter ships, registered from Factory.Extensions.
const (
	// ExtensionUpdate updates the provider-owned fields of the
	// conversation's issue with one PATCH .../issues/{number}. Its data is
	// a JSON object with any of "assignees" (user logins), "labels" (label
	// names) and "milestone" (a milestone number, or null to clear it), at
	// least one; each replaces the issue's whole set, an empty array clears
	// it. The issue state is not a field (that is WorkItems.Act), nor the
	// title or body (Messages.Edit changes the description). Other fields
	// pass through to GitHub unchanged. It is authorized as
	// model.ActionWorkItemAct.
	ExtensionUpdate = string(Type) + ".update"
	// ExtensionVersion is the version of the effect.
	ExtensionVersion = 1
)

// Extensions returns the extension effects the adapter ships, for the host
// to register with the factory.
func (f *Factory) Extensions() []adapter.ExtensionDefinition {
	return []adapter.ExtensionDefinition{{
		Name: ExtensionUpdate, Version: ExtensionVersion, Kind: model.ExtensionKindEffect,
		InputSchema:        model.Extension{Name: ExtensionUpdate + ".input", Version: ExtensionVersion},
		RequiredCapability: model.CapabilityExtensionExecute, AuthorizeAs: model.ActionWorkItemAct,
	}}
}

// ValidateAction checks the fields of an update effect without I/O
// (adapter.ActionValidator): the rules Execute applies at the attempt, so
// an update GitHub would refuse, or another operation owns, is a
// model.Validation error at the call with nothing stored. Fields uma owns
// for the channel (Config.LocalFields) are refused by Execute: the
// validator sees no channel. An action of another name or version is not
// judged here; core refuses it by its registration and Execute by its name.
func (f *Factory) ValidateAction(_ model.Audience, action model.Extension) []model.ValidationIssue {
	if action.Version != ExtensionVersion || action.Name != ExtensionUpdate {
		return nil
	}
	if _, issue := checkUpdate(action.Data); issue != nil {
		return []model.ValidationIssue{*issue}
	}
	return nil
}

// issueUpdate is a validated update: the PATCH body, and the fields the
// adapter reads from it.
type issueUpdate struct {
	fields map[string]json.RawMessage

	assignees, labels                     []string
	hasAssignees, hasLabels, hasMilestone bool
	// milestone is the requested milestone number; 0 clears it.
	milestone int
}

// ownedFields are the issue fields other operations own, each with why it
// is refused.
var ownedFields = map[string]string{
	"title":              "an issue update sets no title",
	"body":               "an issue update sets no body: the description is edited as a message",
	"state":              "an issue update sets no state: issue state goes through work item acts",
	"state_reason":       "an issue update sets no state_reason: issue state goes through work item acts",
	"duplicate_issue_id": "an issue update sets no duplicate_issue_id: issue state goes through work item acts",
	"assignee":           "an issue update sets no assignee: use assignees",
}

// GitHub logins: letters, digits, hyphens and, for managed users, an
// underscore; at most 39 characters.
var loginPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,39}$`)

// checkUpdate is the one set of field rules of the update effect. It
// returns the update when its fields are valid, otherwise the first issue,
// whose message is safe to show.
func checkUpdate(data json.RawMessage) (issueUpdate, *model.ValidationIssue) {
	refuse := func(field, msg string) (issueUpdate, *model.ValidationIssue) {
		return issueUpdate{}, &model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
	}
	fields, err := object(data)
	if err != nil {
		return refuse("data", "action data must be a JSON object")
	}
	for _, k := range slices.Sorted(maps.Keys(ownedFields)) {
		if _, set := fields[k]; set {
			return refuse("data."+k, ownedFields[k])
		}
	}
	u := issueUpdate{fields: fields}
	if raw, set := fields["assignees"]; set {
		logins, ok := stringList(raw)
		if !ok || slices.ContainsFunc(logins, func(l string) bool { return !loginPattern.MatchString(l) }) || !distinctFold(logins) {
			return refuse("data.assignees", "assignees must be an array of distinct GitHub logins")
		}
		u.assignees, u.hasAssignees = logins, true
	}
	if raw, set := fields["labels"]; set {
		names, ok := stringList(raw)
		if !ok || slices.ContainsFunc(names, func(n string) bool { return strings.TrimSpace(n) == "" }) || !distinctFold(names) {
			return refuse("data.labels", "labels must be an array of distinct label names")
		}
		u.labels, u.hasLabels = names, true
	}
	if raw, set := fields["milestone"]; set {
		n, ok := milestoneNumber(raw)
		if !ok {
			return refuse("data.milestone", "milestone must be a milestone number, or null to clear it")
		}
		u.milestone, u.hasMilestone = n, true
	}
	if !u.hasAssignees && !u.hasLabels && !u.hasMilestone {
		return refuse("data", "an issue update sets no field: assignees, labels or milestone")
	}
	return u, nil
}

// object decodes data as a JSON object; empty data is an empty object.
func object(data json.RawMessage) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return nil, errors.New("not an object")
	}
	return m, nil
}

func stringList(raw json.RawMessage) ([]string, bool) {
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil || list == nil {
		return nil, false
	}
	return list, true
}

// distinctFold reports whether no two strings are equal ignoring case, as
// GitHub compares logins and label names.
func distinctFold(list []string) bool {
	for i, s := range list {
		if slices.ContainsFunc(list[:i], func(o string) bool { return strings.EqualFold(o, s) }) {
			return false
		}
	}
	return true
}

// milestoneNumber reads a milestone field: null clears (0), otherwise a
// positive integer.
func milestoneNumber(raw json.RawMessage) (int, bool) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return 0, false
	}
	n, ok := v.(json.Number) // a number token, never a quoted one
	if !ok {
		return 0, false
	}
	v64, err := n.Int64()
	if err != nil || v64 <= 0 || v64 > 1<<31-1 {
		return 0, false
	}
	return int(v64), true
}

// refusedLocal is why the update touches a field uma owns for the channel
// (Config.LocalFields): assignees when uma owns the assignees, labels and
// milestone when it owns the fields. Empty when it touches none.
func (x *Instance) refusedLocal(u issueUpdate) string {
	owned := func(f string) bool { return slices.Contains(x.cfg.LocalFields, f) }
	switch {
	case u.hasAssignees && owned(model.WorkItemFieldAssignees):
		return "assignees are owned by uma for this channel"
	case (u.hasLabels || u.hasMilestone) && owned(model.WorkItemFieldFields):
		return "labels and milestone are owned by uma for this channel"
	}
	return ""
}

// checkAction is the full check of an update action at the attempt: the
// name, version and fields, then the fields uma owns. A refusal is a final
// rejected outcome made before any call.
func (x *Instance) checkAction(action model.Extension) (issueUpdate, adapter.Outcome) {
	if action.Version != ExtensionVersion {
		return issueUpdate{}, refuse(model.SchemaMismatch, "unsupported action version")
	}
	if action.Name != ExtensionUpdate {
		return issueUpdate{}, refuse(model.Unsupported, "unknown github action")
	}
	u, issue := checkUpdate(action.Data)
	if issue != nil {
		return issueUpdate{}, refuse(model.Validation, issue.Message)
	}
	if msg := x.refusedLocal(u); msg != "" {
		return issueUpdate{}, refuse(model.Validation, msg)
	}
	return u, adapter.Outcome{}
}

// Execute runs the update effect: one PATCH of the issue the route names
// with the action's fields, whatever the issue holds, as a work item act
// is sent. GitHub replaces each set sent; a field left out is untouched.
// The answer maps as an issue update's: 200 is provider_accepted; 400 and
// 422 are rejected; 403 is rejected unauthorized; 404 and 410 are rejected
// not_found and name the work item binding they show; a 5xx or a lost
// answer after the request was written is outcome_unknown, reconciled
// before any retry. The webhook GitHub sends for the change is an ordinary
// state observation.
func (x *Instance) Execute(ctx context.Context, r adapter.ExecuteRequest) (adapter.Outcome, error) {
	if err := x.ready(); err != nil {
		return adapter.Outcome{}, err
	}
	route, err := x.parseRoute(r.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	u, refused := x.checkAction(r.Action)
	if refused.State != "" {
		return refused, nil
	}
	_, resp, err := call(ctx, x, func(ctx context.Context) (struct{}, *gh.Response, error) {
		req, err := x.client.NewRequest(ctx, http.MethodPatch, issuePath(route), u.fields)
		if err != nil {
			return struct{}{}, nil, err
		}
		resp, err := x.client.Do(req, nil)
		return struct{}{}, resp, err
	})
	if err != nil {
		return x.failure(ctx, resp, err, issueCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

func issuePath(r issueRoute) string {
	return fmt.Sprintf("repos/%s/%s/issues/%d", r.owner, r.repo, r.number)
}

// reconcileUpdate reads the issue of an update whose answer was lost and
// compares it with the fields the attempt requested: every one held →
// applied (provider_accepted); any other → failed with not_found, so a new
// attempt is safe (each PATCH replaces whole sets). The comparison cannot
// tell who wrote the fields: values a person set to the same meanwhile
// read as applied, and a later change by someone else reads as not
// applied, so the retry puts the requested values back. An attempt whose
// action the request does not carry is inconclusive (state unknown).
func (x *Instance) reconcileUpdate(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	if err := x.ready(); err != nil {
		return adapter.Outcome{}, err
	}
	route, err := x.parseRoute(req.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if req.Action.Name == "" {
		return adapter.Outcome{State: model.Unknown}, nil
	}
	u, refused := x.checkAction(req.Action)
	if refused.State != "" {
		return refused, nil
	}
	iss, err := x.getIssue(ctx, route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if u.heldBy(iss) {
		return adapter.Outcome{State: model.ProviderAccepted}, nil
	}
	return adapter.Outcome{State: model.Failed, Failure: final(model.NotFound, "issue does not hold the requested fields")}, nil
}

// heldBy reports whether iss holds every field of u: the same assignees
// and labels as sets, ignoring case as GitHub does, and the milestone.
func (u issueUpdate) heldBy(iss *gh.Issue) bool {
	if u.hasAssignees {
		logins := make([]string, 0, len(iss.Assignees))
		for _, a := range iss.Assignees {
			logins = append(logins, a.GetLogin())
		}
		if !sameFold(u.assignees, logins) {
			return false
		}
	}
	if u.hasLabels {
		names := make([]string, 0, len(iss.Labels))
		for _, l := range iss.Labels {
			names = append(names, l.GetName())
		}
		if !sameFold(u.labels, names) {
			return false
		}
	}
	if u.hasMilestone {
		if u.milestone == 0 {
			return iss.Milestone == nil
		}
		return iss.Milestone != nil && iss.Milestone.GetNumber() == u.milestone
	}
	return true
}

// sameFold reports whether a and b hold the same strings ignoring case.
func sameFold(a, b []string) bool {
	fold := func(l []string) []string {
		out := make([]string, len(l))
		for i, s := range l {
			out[i] = strings.ToLower(s)
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(fold(a), fold(b))
}

var (
	_ adapter.ExtensionExecutor = (*Instance)(nil)
	_ adapter.ActionValidator   = (*Factory)(nil)
)
