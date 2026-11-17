package linear

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// GraphQL documents. Every field is in Linear's published schema
// (https://github.com/linear/linear/blob/master/packages/sdk/src/schema.graphql).
const (
	mutationCommentCreate = `mutation UmaCommentCreate($input: CommentCreateInput!) {
  commentCreate(input: $input) { success comment { id } }
}`
	mutationCommentUpdate = `mutation UmaCommentUpdate($id: String!, $input: CommentUpdateInput!) {
  commentUpdate(id: $id, input: $input) { success comment { id } }
}`
	mutationCommentDelete = `mutation UmaCommentDelete($id: String!) {
  commentDelete(id: $id) { success }
}`
	queryComment = `query UmaComment($id: String!) {
  comment(id: $id) {
    id body createdAt updatedAt editedAt archivedAt url issueId parentId
    user { id name app }
    botActor { id name type }
    issue { id identifier title url team { key } }
  }
}`
	queryIssue = `query UmaIssue($id: String!) {
  issue(id: $id) {
    id identifier number title url description createdAt updatedAt archivedAt trashed
    team { id key }
    state { id name type }
    creator { id name }
    assignee { id name }
    labels(first: 250) { nodes { name } }
  }
}`
)

// Echo marker: the last line of a comment uma posts, an HTML comment naming
// the operation and attempt.
const (
	markerOpen  = "<!-- uma:"
	markerClose = " -->"
)

func marker(op model.OperationRef, attempt model.ID) string {
	return markerOpen + string(op.ID) + ":" + string(attempt) + markerClose
}

// parseMarker returns the operation and attempt of a body's trailing marker
// and the body without it.
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

// renderBody returns the comment body for content: its text parts, then the
// echo marker. Other parts are unsupported.
func renderBody(c model.Content, op model.OperationRef, attempt model.ID) (string, error) {
	var parts []string
	for _, p := range c.Parts {
		if p.Kind != model.PartText {
			return "", final(model.Unsupported, "Linear comments carry text only")
		}
		parts = append(parts, p.Text)
	}
	if len(parts) == 0 {
		return "", final(model.Validation, "content has no text")
	}
	return strings.Join(parts, "\n\n") + "\n\n" + marker(op, attempt), nil
}

// commentID is the comment ID an attempt creates: a version 4 UUID derived
// from the operation and attempt. CommentCreateInput accepts the ID of the
// new comment ("the identifier in UUID v4 format"), so an attempt names its
// comment before the request is sent and a lost answer is reconciled by
// reading that one comment.
func commentID(op model.OperationRef, attempt model.ID) string {
	sum := sha256.Sum256([]byte("uma.linear.issues.comment:" + string(op.ID) + ":" + string(attempt)))
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case (c < '0' || c > '9') && (c < 'a' || c > 'f'):
			return false
		}
	}
	return true
}

func commentMessageID(id string) string { return "comment:" + id }
func issueMessageID(id string) string   { return "issue:" + id }

// workItemRef returns the reference of the issue with UUID id on ch. The
// UUID is stable across moves between teams and identifier changes.
func workItemRef(ch model.ChannelRef, id string) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: Namespace, Kind: model.RefKindWorkItem, ID: id}
}

// parseRoute checks that r is a work item reference of the instance's
// channel and returns the issue UUID it names.
func (x *Instance) parseRoute(r model.ExternalRef) (string, error) {
	if r.Channel != x.channel.Ref || r.Namespace != Namespace || r.Kind != model.RefKindWorkItem || !validUUID(r.ID) {
		return "", final(model.Validation, "route is not a Linear issue of this channel")
	}
	return r.ID, nil
}

// Send posts content as a comment on the issue the route names. Only public
// audiences are sent: an issue comment is visible to the whole team. The
// comment's ID is derived from the attempt (commentID).
func (x *Instance) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	if !x.cfg.Replies {
		return adapter.Outcome{}, final(model.Disabled, "replies are not enabled for the channel")
	}
	if err := x.ready(); err != nil {
		return adapter.Outcome{}, err
	}
	issue, err := x.parseRoute(r.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if r.Audience.Visibility != model.Public {
		return adapter.Outcome{}, final(model.Unsupported, "Linear comments are public")
	}
	body, err := renderBody(r.Content, r.Operation, r.Attempt)
	if err != nil {
		return adapter.Outcome{}, err
	}
	var out struct {
		CommentCreate struct {
			Success bool `json:"success"`
			Comment *struct {
				ID string `json:"id"`
			} `json:"comment"`
		} `json:"commentCreate"`
	}
	a, err := x.exec(ctx, mutationCommentCreate, map[string]any{"input": map[string]any{
		"issueId": issue, "body": body, "id": commentID(r.Operation, r.Attempt),
	}}, &out)
	if err != nil || a.failed() {
		return x.failure(ctx, a, err, createCall)
	}
	c := out.CommentCreate
	if !c.Success || c.Comment == nil || c.Comment.ID == "" {
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Linear answer has no comment ID; reconcile before retrying")
	}
	return x.accepted(c.Comment.ID), nil
}

func (x *Instance) accepted(id string) adapter.Outcome {
	return adapter.Outcome{
		State:      model.ProviderAccepted,
		References: []model.ExternalRef{model.ProviderMessageRef(x.channel.Ref, commentMessageID(id))},
	}
}

// Reconcile resolves an attempt whose answer was lost. An edit or delete
// (req.Capability) is resolved by reading the target; see
// reconcileMutation. A reply is resolved by reading the comment ID the
// attempt derives (commentID): found is provider_accepted with its
// reference; absent is failed with not_found, so a retry is safe.
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
	if _, err := x.parseRoute(req.Route); err != nil {
		return adapter.Outcome{}, err
	}
	id := commentID(req.Operation, req.Attempt)
	_, gone, err := x.readComment(ctx, id)
	switch {
	case err != nil:
		return adapter.Outcome{}, err
	case gone:
		return adapter.Outcome{State: model.Failed, Failure: final(model.NotFound, "no comment recorded for the attempt")}, nil
	}
	return x.accepted(id), nil
}
