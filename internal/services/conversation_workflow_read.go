package services

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// workflowVisible reports whether the principal may read conv's workflow
// state: model.ActionConversationReadWorkflow on the conversation at its
// revision. A denial is false; a policy failure is returned.
func workflowVisible(ctx context.Context, c *engine.Client, conv model.Conversation) (bool, error) {
	res := conv.Ref.Ref()
	err := c.Authorize(ctx, policy.Authorization{
		Action: model.ActionConversationReadWorkflow, Resource: res, Revision: conv.Revision,
	}, res)
	switch {
	case err == nil:
		return true, nil
	case model.IsCode(err, model.Unauthorized):
		return false, nil
	}
	return false, err
}

// showUnderWorkflowFilter returns an admitted conversation as the
// principal sees it. Under a workflow filter (Filter.Assignee, Snoozed,
// Linked or Unread) the conversation is left out unless the principal may
// read its workflow state, even when it holds none: being unassigned or
// awake is workflow state too. Snoozed and Linked are checked again on
// what the principal sees, so a page never shows a snooze the filter
// excluded, nor a conversation whose only links it may not read.
func showUnderWorkflowFilter(ctx context.Context, c *engine.Client, f model.Filter, conv model.Conversation) (model.Conversation, bool, error) {
	if f.Assignee != nil || f.Snoozed != nil || f.Linked != nil || f.Unread != nil {
		ok, err := workflowVisible(ctx, c, conv)
		if err != nil || !ok {
			return model.Conversation{}, false, err
		}
	}
	shown, err := shownConversation(ctx, c, conv)
	if err != nil {
		return model.Conversation{}, false, err
	}
	if f.Snoozed != nil && (shown.Snooze != nil) != *f.Snoozed {
		return model.Conversation{}, false, nil
	}
	if f.Linked != nil && (len(shown.Links) > 0) != *f.Linked {
		return model.Conversation{}, false, nil
	}
	return shown, true, nil
}

// shownConversation returns conv, already admitted for reading, as the
// principal sees it: without workflow state unless it may read it, and
// then with only the links whose far end it may read (the others left out
// without trace) and its snooze only while the snooze is in effect at the
// engine clock (not expired, not woken by an inbound message), so a reader
// never has to evaluate the wake rule itself. A conversation without workflow
// state has nothing to hide, and the policy is not asked about it.
func shownConversation(ctx context.Context, c *engine.Client, conv model.Conversation) (model.Conversation, error) {
	if conv.Assignee == nil && conv.Snooze == nil && len(conv.Links) == 0 {
		return conv, nil
	}
	ok, err := workflowVisible(ctx, c, conv)
	if err != nil {
		return model.Conversation{}, err
	}
	if !ok {
		return conv.WithoutWorkflow(), nil
	}
	if len(conv.Links) > 0 {
		if conv.Links, err = readableLinks(ctx, c, conv.Links); err != nil {
			return model.Conversation{}, err
		}
	}
	if conv.Snooze == nil {
		return conv, nil
	}
	now := c.Engine().Clock().Now()
	var in bool
	if err := view(ctx, c, func(r store.Reader) (err error) {
		in, err = snoozeInEffect(ctx, r, conv, now)
		return err
	}); err != nil {
		return model.Conversation{}, err
	}
	if !in {
		conv.Snooze = nil
	}
	return conv, nil
}
