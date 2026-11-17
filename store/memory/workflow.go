package memory

import (
	"time"

	"hop.top/uma/model"
)

// conversationReads is the support Conversations declares, the workflow
// fields Assignee (resolved, never Self) and Snoozed included.
var conversationReads = filterSupport{inboxes: true, statuses: true, labels: true, assignee: true, snoozed: true, snoozeEnded: true, linked: true, unread: true}

// workflowMatch returns the workflow part of the Conversations filter: the
// assignee, and whether the snooze is in effect at now. The newest live
// inbound recording time of every conversation is collected on first need,
// in the caller's unit, by one pass over the messages.
func (h *handle) workflowMatch(now time.Time) func(model.Filter, model.Conversation) bool {
	var newest map[model.ConversationRef]time.Time
	var unread map[model.ConversationRef]bool
	return func(f model.Filter, v model.Conversation) bool {
		if f.Assignee != nil && !f.Assignee.Matches(v.Assignee) {
			return false
		}
		if f.Linked != nil && (len(v.Links) > 0) != *f.Linked {
			return false
		}
		if f.Unread != nil {
			if unread == nil {
				unread = h.unreadFor(f.Unread.Actor)
			}
			if !unread[v.Ref] {
				return false
			}
		}
		if f.Snoozed == nil && f.SnoozeEnded == nil {
			return true
		}
		if newest == nil {
			newest = h.newestInbound()
		}
		var at *time.Time
		if t, ok := newest[v.Ref]; ok {
			at = &t
		}
		in := v.Snooze.InEffect(now, at)
		if f.Snoozed != nil && in != *f.Snoozed {
			return false
		}
		return f.SnoozeEnded == nil || (v.Snooze != nil && !in) == *f.SnoozeEnded
	}
}

// unreadFor returns the conversations holding a message unread for actor
// (model.ReadMarker.Unread against the actor's marker, if any), by one
// pass over the messages.
func (h *handle) unreadFor(actor model.ActorID) map[model.ConversationRef]bool {
	out := map[model.ConversationRef]bool{}
	for _, e := range h.st.messages.m {
		conv := e.v.Ref.Conversation
		if out[conv] {
			continue
		}
		var marker *model.ReadMarker
		if m, ok := h.st.readMarkers.get(readMarkerKey{conv, actor}); ok {
			marker = &m.v
		}
		if marker.Unread(e.v, actor) {
			out[conv] = true
		}
	}
	return out
}

// newestInbound returns, per conversation, the latest RecordedAt of its
// live inbound messages that were not imported: no origin, no echo,
// neither redacted nor deleted, as InboundMessages selects them, and no
// ImportedBy (history never wakes a snooze).
func (h *handle) newestInbound() map[model.ConversationRef]time.Time {
	out := map[model.ConversationRef]time.Time{}
	for _, e := range h.st.messages.m {
		m := &e.v
		if m.Origin != nil || m.EchoOf != nil || m.Redacted || m.Deleted || m.ImportedBy != "" {
			continue
		}
		if cur, ok := out[m.Ref.Conversation]; !ok || m.RecordedAt.After(cur) {
			out[m.Ref.Conversation] = m.RecordedAt
		}
	}
	return out
}
