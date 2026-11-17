package linear

import (
	"context"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// refuse is the final rejection of a mutation the adapter or Linear will not
// carry out.
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

// mutationTarget checks that replies are enabled and the instance can call
// Linear, and returns the comment UUID the effect's target names. Only
// comments are targets: an issue description (issue:<id>) is rejected
// unsupported, since uma sends no issue updates.
func (x *Instance) mutationTarget(e adapter.Effect) (string, *adapter.Outcome, error) {
	if !x.cfg.Replies {
		return "", nil, final(model.Disabled, "replies are not enabled for the channel")
	}
	if err := x.ready(); err != nil {
		return "", nil, err
	}
	if _, err := x.parseRoute(e.Route); err != nil {
		return "", nil, err
	}
	t := e.Target
	bad := final(model.Validation, "target is not a Linear message of this channel")
	if t == nil || t.Channel != x.channel.Ref || t.Namespace != model.ProviderMessageNamespace || t.Kind != model.RefKindMessage {
		return "", nil, bad
	}
	if id, ok := strings.CutPrefix(t.ID, "comment:"); ok && validUUID(id) {
		return id, nil, nil
	}
	if id, ok := strings.CutPrefix(t.ID, "issue:"); ok && validUUID(id) {
		o := refuse(model.Unsupported, "an issue description is not changed through comments")
		return "", &o, nil
	}
	return "", nil, bad
}

// Edit replaces the body of the comment the target names with the rendered
// content and the marker of the attempt. Only public text is sent. Linear's
// refusal (a comment it cannot find, one the account may not edit, or one
// synced from another tool) is a final rejection, never a new comment.
func (x *Instance) Edit(ctx context.Context, r adapter.EditRequest) (adapter.Outcome, error) {
	id, refused, err := x.mutationTarget(r.Effect)
	if err != nil || refused != nil {
		return orRefused(refused, err)
	}
	if r.Audience.Visibility != model.Public {
		return refuse(model.Unsupported, "Linear comments are public"), nil
	}
	body, err := renderBody(r.Content, r.Operation, r.Attempt)
	if err != nil {
		return refusal(err)
	}
	var out struct {
		CommentUpdate struct {
			Success bool `json:"success"`
		} `json:"commentUpdate"`
	}
	a, err := x.exec(ctx, mutationCommentUpdate, map[string]any{"id": id, "input": map[string]any{"body": body}}, &out)
	if err != nil || a.failed() {
		return x.failure(ctx, a, err, mutateCall)
	}
	if !out.CommentUpdate.Success {
		return refuse(model.Validation, "Linear did not apply the edit"), nil
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

// Delete deletes the comment the target names. Linear deletes a root
// comment's replies with it.
func (x *Instance) Delete(ctx context.Context, r adapter.DeleteRequest) (adapter.Outcome, error) {
	id, refused, err := x.mutationTarget(r.Effect)
	if err != nil || refused != nil {
		return orRefused(refused, err)
	}
	var out struct {
		CommentDelete struct {
			Success bool `json:"success"`
		} `json:"commentDelete"`
	}
	a, err := x.exec(ctx, mutationCommentDelete, map[string]any{"id": id}, &out)
	if err != nil || a.failed() {
		return x.failure(ctx, a, err, mutateCall)
	}
	if !out.CommentDelete.Success {
		return refuse(model.Validation, "Linear did not apply the deletion"), nil
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

func orRefused(o *adapter.Outcome, err error) (adapter.Outcome, error) {
	if o != nil {
		return *o, nil
	}
	return adapter.Outcome{}, err
}

// reconcileMutation resolves an edit or delete whose answer was lost, by
// reading the comment:
//
//   - edit: the body ends with the attempt's marker → applied; a comment
//     gone is rejected not_found; any other body means the attempt did not
//     apply.
//   - delete: the comment is gone (not found, or archived) → applied;
//     present → not applied.
//
// Not applied is failed with not_found, so a new attempt is safe.
func (x *Instance) reconcileMutation(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	id, refused, err := x.mutationTarget(req.Effect)
	if err != nil || refused != nil {
		return orRefused(refused, err)
	}
	c, gone, err := x.readComment(ctx, id)
	if err != nil {
		return adapter.Outcome{}, err
	}
	notApplied := adapter.Outcome{State: model.Failed, Failure: final(model.NotFound, "Linear does not show the attempt's effect")}
	if req.Capability == model.CapabilityDelete {
		if gone {
			return adapter.Outcome{State: model.ProviderAccepted}, nil
		}
		return notApplied, nil
	}
	if gone {
		return refuse(model.NotFound, "comment not found"), nil
	}
	if op, att, _, ok := parseMarker(c.Body); ok && op == req.Operation.ID && att == req.Attempt {
		return adapter.Outcome{State: model.ProviderAccepted}, nil
	}
	return notApplied, nil
}
