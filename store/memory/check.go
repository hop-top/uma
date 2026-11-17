package memory

import (
	"strconv"
	"strings"

	"hop.top/uma/model"
)

func invalid(msg string) error  { return model.NewError(model.Validation, msg) }
func conflict(msg string) error { return model.NewError(model.Conflict, msg) }
func notFound() error           { return model.NewError(model.NotFound, "resource not found") }

// firstErr returns the first non-nil error.
func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

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

// Cursors are positions: the insertion sequence of the last item returned,
// or the count of log entries scanned. Position 0 is the empty cursor.
const cursorPrefix = "mem."

func encodeCursor(pos uint64) model.Cursor {
	if pos == 0 {
		return ""
	}
	return model.Cursor(cursorPrefix + strconv.FormatUint(pos, 10))
}

func decodeCursor(c model.Cursor) (uint64, error) {
	if c == "" {
		return 0, nil
	}
	s, ok := strings.CutPrefix(string(c), cursorPrefix)
	if !ok {
		return 0, invalid("cursor: malformed")
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n == 0 {
		return 0, invalid("cursor: malformed")
	}
	return n, nil
}

func pageLimit(limit int) (int, error) {
	if limit <= 0 {
		return 0, invalid("page.limit: must be positive")
	}
	return min(limit, maxPage), nil
}

// filterSupport lists the filter fields a read honours; any other non-empty
// field is rejected rather than ignored.
type filterSupport struct {
	inboxes, conversation, contact, kinds, statuses, labels, task, identity bool
	// Message search fields.
	text, since, before, attachments bool
	// Conversation workflow fields.
	assignee, snoozed, linked, unread bool
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

// matchAny reports whether set is empty or holds v.
func matchAny[T comparable](set []T, v T) bool {
	if len(set) == 0 {
		return true
	}
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
