package examples

import (
	"context"

	"hop.top/uma"
	"hop.top/uma/model"
)

// Approver is host-owned and bound to the current actor, target, operation and exact payload.
type Approver interface {
	Approve(context.Context, model.Reply) (bool, error)
}
type ReplyDraft struct{ Text string }

// ApprovedReply keeps actor, route and operation metadata outside model-generated draft content.
// The caller persists its decision for replay; uma reauthorizes execution after approval.
func ApprovedReply(ctx context.Context, client uma.Client, target model.ConversationRef,
	command model.Command, draft ReplyDraft, approvals Approver) (model.Acceptance, error) {
	request := model.Reply{
		Command: command, Conversation: target,
		Content: model.Content{Parts: []model.Part{{Kind: "text", Text: draft.Text}}},
	}
	if approvals == nil {
		return model.Acceptance{}, &model.Error{Code: model.Unauthorized, SafeMessage: "approval required"}
	}
	allowed, err := approvals.Approve(ctx, request)
	if err != nil {
		return model.Acceptance{}, err
	}
	if !allowed {
		return model.Acceptance{}, &model.Error{Code: model.Unauthorized, SafeMessage: "approval denied"}
	}
	return client.Messages().Reply(ctx, request)
}
