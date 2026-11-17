package model

// eventLogID is the fixed ID of every tenant's event log resource: the log
// is one per tenant and has no record of its own.
const eventLogID ID = "00000000-0000-8000-8000-00000000e1a9"

// EventLogRef returns the event log resource of tenant t: the resource
// Events.List authorizes ActionEventList on. Its kind is KindEvent and its
// ID the same fixed value in every tenant.
func EventLogRef(t TenantID) Ref { return Ref{Tenant: t, Kind: resourceEvent, ID: eventLogID} }

// ListEvents reads one page of the principal's tenant event log without a
// subscription (Events.List).
//
// Start is StartBeginning (the retained beginning of the log), StartTail
// (the head: an empty, complete page whose Next is the head) or
// StartCheckpoint, with no default; After is set exactly with
// StartCheckpoint. Positions are event log positions, the space of
// Subscription.Checkpoint and EventBatch.ScannedThrough, not query cursors:
// a page's Next can resume a list or open a subscription with
// StartCheckpoint.
//
// Filter honours Inboxes, Conversation, Kinds and Since only; any other
// set field is Validation. An inbox selects the events about it as well as
// those about its conversations and messages. Conversation selects the
// events about one conversation: those about the conversation itself and
// about its messages, tasks, work items and interactions (see
// store.EventConversation); an event about no conversation (a delivery
// update, a contact, identity, channel or inbox event, an extension event
// about anything but a conversation) is never selected while it is set.
// Since selects the events whose RecordedAt, the time uma recorded them,
// is at or after it; unlike Messages.List it does not compare OccurredAt,
// which the provider or host sets. Limit bounds the events one call scans
// (those the filter keeps), not the events it returns, which exclude the
// ones the principal may not read: zero asks for the default and larger
// values are capped (see the Events.List documentation). A negative Limit
// is Validation.
type ListEvents struct {
	Call
	Filter Filter `json:"filter"`
	Start  string `json:"start"`
	After  Cursor `json:"after"`
	Limit  int    `json:"limit"`
}

// Validate checks the call, the filter and the fields Events.List does not
// honour, the start mode and After, and a non-negative Limit. Whether each
// kind is a known event kind and each inbox in the principal's tenant is
// checked by the engine.
func (r ListEvents) Validate() error {
	start, after := validateStart(r.Start, r.After)
	return firstError(
		r.Call.Validate(),
		r.Filter.validate("filter"),
		r.Filter.eventListFields("filter"),
		start,
		after,
		notNegative("limit", int64(r.Limit)),
	)
}

// eventListFields rejects the filter fields Events.List does not honour:
// every field except Inboxes, Conversation, Kinds and Since.
func (f Filter) eventListFields(path string) error {
	reject := func(field string, set bool) error {
		if set {
			return invalidField(fieldPath(path, field), "not supported by Events.List")
		}
		return nil
	}
	return firstError(
		reject("contact", f.Contact != nil),
		reject("statuses", len(f.Statuses) > 0),
		reject("identity", f.Identity != nil),
		reject("labels", f.Labels != nil),
		reject("task", f.Task != nil),
		reject("text", f.Text != ""),
		reject("before", f.Before != nil),
		reject("attachments", f.Attachments != nil),
		reject("assignee", f.Assignee != nil),
		reject("snoozed", f.Snoozed != nil),
		reject("snooze_ended", f.SnoozeEnded != nil),
		reject("linked", f.Linked != nil),
		reject("unread", f.Unread != nil),
		reject("actor", f.Actor != nil),
	)
}
