package gitlab

import (
	"context"
	"net/http"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// messageTarget is the provider message of a mutation: a note, or the
// description of the issue the route names (note is zero).
type messageTarget struct {
	route issueRoute
	id    string
	note  int64
}

// refuse is the final rejection of a mutation the adapter or GitLab will
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

// mutationTarget checks that mutations are enabled and the instance can call
// GitLab, and parses the effect's route and target. The target is a
// provider message of this channel: note:<id>, or issue:<id> for the
// route's issue description. No authorship is checked: GitLab answers for
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
	bad := final(model.Validation, "target is not a GitLab message of this channel")
	if t == nil || t.Channel != x.channel.Ref || t.Namespace != model.ProviderMessageNamespace || t.Kind != model.RefKindMessage {
		return messageTarget{}, bad
	}
	kind, num, ok := strings.Cut(t.ID, ":")
	n, okNum := canonicalInt(num)
	if !ok || !okNum {
		return messageTarget{}, bad
	}
	switch kind {
	case "note":
		return messageTarget{route: route, id: t.ID, note: n}, nil
	case "issue":
		return messageTarget{route: route, id: t.ID}, nil
	}
	return messageTarget{}, bad
}

// Edit replaces the body of the note the target names, or of the issue
// description, with the rendered content and the marker of the attempt.
// Only public text is sent. Any note the token may change is edited,
// another user's included; GitLab's refusal (403, or 404 for a note that is
// gone) is a final rejection, never a new note.
func (x *Instance) Edit(ctx context.Context, r adapter.EditRequest) (adapter.Outcome, error) {
	t, err := x.mutationTarget(r.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if r.Audience.Visibility != model.Public {
		return refuse(model.Unsupported, "GitLab replies are public notes"), nil
	}
	limit := maxNoteRunes
	if t.note == 0 {
		limit = maxDescriptionRunes
	}
	body, err := renderBody(r.Content, r.Operation, r.Attempt, limit)
	if err != nil {
		return refusal(err)
	}
	var a *answer
	if t.note != 0 {
		a, err = x.call(ctx, spec{method: http.MethodPut, path: t.route.notePath(t.note), body: map[string]string{"body": body}})
	} else {
		a, err = x.call(ctx, spec{method: http.MethodPut, path: t.route.issuePath(), body: map[string]string{"description": body}})
	}
	if err != nil || !a.ok() {
		return x.failure(ctx, a, err, mutateCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

// Delete deletes the note the target names. An issue description cannot be
// deleted: that is a final rejection, unsupported.
func (x *Instance) Delete(ctx context.Context, r adapter.DeleteRequest) (adapter.Outcome, error) {
	t, err := x.mutationTarget(r.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if t.note == 0 {
		return refuse(model.Unsupported, "an issue description cannot be deleted"), nil
	}
	a, err := x.call(ctx, spec{method: http.MethodDelete, path: t.route.notePath(t.note)})
	if err != nil || !a.ok() {
		return x.failure(ctx, a, err, mutateCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

// reconcileMutation resolves a mutation whose answer was lost, by reading
// what GitLab holds:
//
//   - edit: the note (or issue) body ends with the attempt's marker →
//     applied; a note gone is rejected not_found; any other body means the
//     attempt did not apply.
//   - delete: the note is gone (404) → applied; present → not applied.
//
// Not applied is failed with not_found, so a new attempt is safe.
func (x *Instance) reconcileMutation(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	t, err := x.mutationTarget(req.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	notApplied := adapter.Outcome{State: model.Failed, Failure: final(model.NotFound, "GitLab does not show the attempt's effect")}
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
		if t.note == 0 {
			return refuse(model.Unsupported, "an issue description cannot be deleted"), nil
		}
		a, err := x.call(ctx, spec{method: http.MethodGet, path: t.route.notePath(t.note)})
		switch {
		case err == nil && a.ok():
			return notApplied, nil
		case err == nil && a.status == http.StatusNotFound:
			return applied, nil
		}
		return adapter.Outcome{}, x.readFailure(ctx, a, err)
	}
	return adapter.Outcome{}, final(model.Unsupported, "reconciliation of this capability is not supported")
}

// currentBody reads the body of the target. A note that is gone is a
// rejected outcome, not_found.
func (x *Instance) currentBody(ctx context.Context, t messageTarget) (string, adapter.Outcome, error) {
	if t.note == 0 {
		iss, err := x.getIssue(ctx, t.route)
		if err != nil {
			return "", adapter.Outcome{}, err
		}
		return iss.Description, adapter.Outcome{}, nil
	}
	a, err := x.call(ctx, spec{method: http.MethodGet, path: t.route.notePath(t.note)})
	switch {
	case err == nil && a.ok():
		n, ok := decodeNote(a.body)
		if !ok {
			return "", adapter.Outcome{}, model.NewError(model.Transient, "GitLab answer is not a note")
		}
		return n.Body, adapter.Outcome{}, nil
	case err == nil && a.status == http.StatusNotFound:
		return "", refuse(model.NotFound, "note not found"), nil
	}
	return "", adapter.Outcome{}, x.readFailure(ctx, a, err)
}
