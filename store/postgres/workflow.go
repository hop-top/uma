package postgres

import (
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store/postgres/internal/db"
)

// conversationReads is the support Conversations declares: the label
// selector and the workflow fields Assignee (resolved, never Self) and
// Snoozed are evaluated in SQL.
var conversationReads = filterSupport{inboxes: true, statuses: true, labels: true, assignee: true, snoozed: true, snoozeEnded: true, linked: true, unread: true}

// workflowColumns are the columns a conversation row stores beside its
// body for the workflow filters (migrations 20 and 21).
type workflowColumns struct {
	assignee            *string
	untilUnix, atUnix   *int64
	untilNanos, atNanos *int32
	links               int32
}

func workflowColumnsOf(c model.Conversation) workflowColumns {
	w := workflowColumns{links: int32(len(c.Links))} // #nosec G115 -- capped by model.MaxConversationLinks
	if c.Assignee != nil {
		a := string(*c.Assignee)
		w.assignee = &a
	}
	if s := c.Snooze; s != nil {
		uu, un := instant(s.Until)
		au, an := instant(s.At)
		w.untilUnix, w.untilNanos, w.atUnix, w.atNanos = &uu, &un, &au, &an
	}
	return w
}

// instant splits t into exact (Unix seconds, nanoseconds) columns.
func instant(t time.Time) (int64, int32) {
	return t.Unix(), int32(t.Nanosecond()) // #nosec G115 -- Nanosecond is below 1e9
}

// assigneeArgs returns the ListConversations assignee arguments of f:
// any assignee when unset, else the actor, or NULL for the unassigned.
func assigneeArgs(f model.Filter) (bool, *string) {
	switch a := f.Assignee; {
	case a == nil:
		return true, nil
	case a.None:
		return false, nil
	default:
		actor := string(a.Actor)
		return false, &actor
	}
}

// listConversationsParams returns the ListConversations arguments of f,
// judging snoozes at now.
func (h *handle) listConversationsParams(f model.Filter, after int64, lim int32, now time.Time) db.ListConversationsParams {
	anyOf, allOf, noneOf := selectorArgs(f)
	assigneeAny, assignee := assigneeArgs(f)
	nowUnix, nowNanos := instant(now)
	return db.ListConversationsParams{
		TenantID: h.tenantID, After: after, Inboxes: inboxIDs(f), Statuses: strs(f.Statuses),
		AnyLabels: anyOf, AllLabels: allOf, NoneLabels: noneOf,
		AssigneeAny: assigneeAny, Assignee: assignee, Snoozed: f.Snoozed, SnoozeEnded: f.SnoozeEnded, NowUnix: nowUnix, NowNanos: nowNanos,
		Linked: f.Linked, UnreadActor: unreadActor(f), Lim: lim,
	}
}

// unreadActor returns the actor of Filter.Unread, or nil when unset.
func unreadActor(f model.Filter) *string {
	if f.Unread == nil {
		return nil
	}
	a := string(f.Unread.Actor)
	return &a
}
