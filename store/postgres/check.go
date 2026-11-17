package postgres

import (
	"strconv"
	"strings"

	"hop.top/uma/model"
)

// scoped validates each ref and requires it to belong to the unit's tenant.
// Foreign refs are not_found, never confirmed.
func (h *handle) scoped(refs ...interface {
	model.Scoped
	Validate() error
}) error {
	for _, r := range refs {
		if err := r.Validate(); err != nil {
			return err
		}
		if err := model.CheckScope(h.tenant, r); err != nil {
			return err
		}
	}
	return nil
}

// sameTenant requires a record's own tenant field to be the unit's tenant.
func (h *handle) sameTenant(t model.TenantID) error {
	if t != h.tenant {
		return notFound()
	}
	return nil
}

// newRevision checks the revision a write stores against the expected one.
func newRevision(next, expected model.Revision) error {
	if next == "" {
		return invalid("revision: required")
	}
	if next == expected {
		return invalid("revision: must differ from the expected revision")
	}
	return nil
}

// cas compares the stored revision with expected; empty expected means the
// record must not exist.
func cas(found bool, current, expected model.Revision) error {
	switch {
	case !found && expected != "":
		return conflict("record does not exist at the expected revision")
	case found && expected == "":
		return conflict("record already exists")
	case found && current != expected:
		return conflict("stale expected revision")
	}
	return nil
}

func required(field, v string) error {
	if v == "" {
		return invalid(field + ": required")
	}
	return nil
}

// Cursors are positions: the pos or seq of the last item returned, or an
// event log position. Position 0 is the empty cursor.
const cursorPrefix = "pg."

func encodeCursor(pos int64) model.Cursor {
	if pos <= 0 {
		return ""
	}
	return model.Cursor(cursorPrefix + strconv.FormatInt(pos, 10))
}

func decodeCursor(c model.Cursor) (int64, error) {
	if c == "" {
		return 0, nil
	}
	s, ok := strings.CutPrefix(string(c), cursorPrefix)
	if !ok {
		return 0, invalid("cursor: malformed")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != s {
		return 0, invalid("cursor: malformed")
	}
	return n, nil
}

// pageLimit validates limit and caps it at maxPage.
func pageLimit(limit int) (int32, error) {
	if limit <= 0 {
		return 0, invalid("page.limit: must be positive")
	}
	return int32(min(limit, maxPage)), nil // #nosec G115 -- capped at maxPage
}

// filterSupport lists the filter fields a read honours; any other non-empty
// field is rejected rather than ignored.
type filterSupport struct {
	inboxes, conversation, contact, kinds, statuses, labels, task, identity bool
	// Message search fields.
	text, since, before, attachments bool
	// Conversation workflow fields.
	assignee, snoozed, snoozeEnded, linked, unread bool
}

func (h *handle) checkFilter(f model.Filter, ok filterSupport) error {
	if err := f.Validate(); err != nil {
		return err
	}
	unsupported := func(field string, set, allowed bool) error {
		if set && !allowed {
			return invalid("filter." + field + ": not supported by this read")
		}
		return nil
	}
	if err := firstErr(
		unsupported("inboxes", len(f.Inboxes) > 0, ok.inboxes),
		unsupported("conversation", f.Conversation != nil, ok.conversation),
		unsupported("contact", f.Contact != nil, ok.contact),
		unsupported("kinds", len(f.Kinds) > 0, ok.kinds),
		unsupported("statuses", len(f.Statuses) > 0, ok.statuses),
		unsupported("labels", f.Labels != nil, ok.labels),
		unsupported("task", f.Task != nil, ok.task),
		unsupported("identity", f.Identity != nil, ok.identity),
		unsupported("text", f.Text != "", ok.text),
		unsupported("since", f.Since != nil, ok.since),
		unsupported("before", f.Before != nil, ok.before),
		unsupported("attachments", f.Attachments != nil, ok.attachments),
		unsupported("assignee", f.Assignee != nil, ok.assignee),
		unsupported("assignee.self", f.Assignee != nil && f.Assignee.Self, false),
		unsupported("snoozed", f.Snoozed != nil, ok.snoozed),
		unsupported("snooze_ended", f.SnoozeEnded != nil, ok.snoozeEnded),
		unsupported("linked", f.Linked != nil, ok.linked),
		unsupported("unread", f.Unread != nil, ok.unread),
		unsupported("unread.actor", f.Unread != nil && f.Unread.Actor == "", !ok.unread),
	); err != nil {
		return err
	}
	for _, in := range f.Inboxes {
		if err := model.CheckScope(h.tenant, in); err != nil {
			return err
		}
	}
	if f.Conversation != nil {
		if err := model.CheckScope(h.tenant, f.Conversation); err != nil {
			return err
		}
	}
	if f.Contact != nil {
		if err := model.CheckScope(h.tenant, f.Contact); err != nil {
			return err
		}
	}
	if f.Task != nil {
		if err := model.CheckScope(h.tenant, f.Task); err != nil {
			return err
		}
	}
	if f.Identity != nil {
		if err := model.CheckScope(h.tenant, f.Identity); err != nil {
			return err
		}
	}
	if f.Labels != nil {
		return f.Labels.CheckScope(h.tenant)
	}
	return nil
}

// inboxIDs returns the filter's inbox IDs as query arguments; empty means
// every inbox.
func inboxIDs(f model.Filter) []string {
	out := make([]string, len(f.Inboxes))
	for i, in := range f.Inboxes {
		out[i] = string(in.ID)
	}
	return out
}

// strs returns s as query arguments, never nil.
func strs(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// optID returns id as a nullable uuid argument.
func optID(id model.ID) *string {
	if id == "" {
		return nil
	}
	s := string(id)
	return &s
}

// conversationArgs returns the nullable conversation filter arguments.
func conversationArgs(f model.Filter) (inbox, id *string) {
	if f.Conversation == nil {
		return nil, nil
	}
	return optID(f.Conversation.Inbox.ID), optID(f.Conversation.ID)
}
