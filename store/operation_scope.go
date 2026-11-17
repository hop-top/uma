package store

import "hop.top/uma/model"

// OperationScope is the inbox and conversation an operation concerns, the
// two a Reader.Operations filter selects by. A zero ID means the operation
// has none; Conversation is set only with Inbox.
type OperationScope struct {
	Inbox        model.ID
	Conversation model.ID
}

// Scope resolves the inbox and conversation of the operation r holds, from
// the record alone, so every backend agrees on them:
//
//   - The inbox is the one the operation key's target lies in: the target
//     itself when it is an inbox, else the target's Inbox. A target that is
//     tenant-scoped (a channel, a contact, a label) has none.
//   - The conversation is the target when it is a conversation, else the
//     first of the operation's typed results naming one in that inbox:
//     ResultMessage, ResultStream, ResultTask, then a Result that is a
//     conversation. An operation on a work item, an interaction or an
//     attachment that produced no such result has none.
//
// The scope of a stored operation never changes: its key and its results
// are fixed when it is accepted.
func (r OperationRecord) Scope() OperationScope {
	var s OperationScope
	t := r.Key.Target
	if t.Kind == model.KindInbox {
		s.Inbox = t.ID
	} else {
		s.Inbox = t.Inbox
	}
	if s.Inbox == "" {
		return OperationScope{}
	}
	in := func(c model.ConversationRef) bool { return c.Inbox.ID == s.Inbox && c.ID != "" }
	op := r.Operation
	switch {
	case t.Kind == model.KindConversation:
		s.Conversation = t.ID
	case op.ResultMessage != nil && in(op.ResultMessage.Conversation):
		s.Conversation = op.ResultMessage.Conversation.ID
	case op.ResultStream != nil && in(op.ResultStream.Conversation):
		s.Conversation = op.ResultStream.Conversation.ID
	case op.ResultTask != nil && in(op.ResultTask.Conversation):
		s.Conversation = op.ResultTask.Conversation.ID
	case op.Result != nil && op.Result.Kind == model.KindConversation && op.Result.Inbox == s.Inbox:
		s.Conversation = op.Result.ID
	}
	return s
}
