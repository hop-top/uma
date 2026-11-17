package memory

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/model"
)

// taskRefKey is a task's core reference in one inbox: the key
// TaskByReference reads and PutTask keeps unique.
type taskRefKey struct {
	inbox model.InboxRef
	ref   model.ExternalRef
}

func (t tx) PutTask(ctx context.Context, v model.Task, expected model.Revision) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := firstErr(h.scoped(v.Ref), h.requireConversation(v.Ref.Conversation)); err != nil {
		return err
	}
	inbox := v.Ref.Conversation.Inbox
	if !v.External.IsZero() {
		if err := h.scoped(v.External); err != nil {
			return err
		}
		if held, ok := h.st.taskRefs.get(taskRefKey{inbox, v.External}); ok && held.v != v.Ref {
			return conflict("task reference held by another task")
		}
	}
	prev, found := h.st.tasks.get(v.Ref)
	if err := put(h, &h.st.tasks, v.Ref, v.Clone(), taskRev, expected); err != nil {
		return err
	}
	if found && !prev.v.External.IsZero() && prev.v.External != v.External {
		h.st.taskRefs.del(taskRefKey{inbox, prev.v.External})
	}
	if !v.External.IsZero() {
		h.st.taskRefs.set(taskRefKey{inbox, v.External}, entry[model.TaskRef]{v: v.Ref})
	}
	return nil
}

func (r reader) TaskByReference(ctx context.Context, inbox model.InboxRef, ref model.ExternalRef) (model.Task, error) {
	if err := r.h.enter(ctx); err != nil {
		return model.Task{}, err
	}
	defer r.h.mu.Unlock()
	if err := r.h.scoped(inbox, ref); err != nil {
		return model.Task{}, err
	}
	held, ok := r.h.st.taskRefs.get(taskRefKey{inbox, ref})
	if !ok {
		return model.Task{}, notFound()
	}
	e, ok := r.h.st.tasks.get(held.v)
	if !ok {
		return model.Task{}, notFound()
	}
	return e.v.Clone(), nil
}

// Tasks pages in status order, most recent first, after the cursor's
// (StatusAt, ID) position.
func (r reader) Tasks(ctx context.Context, req model.ListRequest) (model.Page[model.Task], error) {
	if err := r.h.enter(ctx); err != nil {
		return model.Page[model.Task]{}, err
	}
	defer r.h.mu.Unlock()
	limit, err := pageLimit(req.Page.Limit)
	if err != nil {
		return model.Page[model.Task]{}, err
	}
	after, paged, err := decodeTaskCursor(req.Page.After)
	if err != nil {
		return model.Page[model.Task]{}, err
	}
	f := req.Filter
	if err := r.h.checkFilter(f, filterSupport{inboxes: true, conversation: true, statuses: true, identity: true}); err != nil {
		return model.Page[model.Task]{}, err
	}
	var rows []model.Task
	for _, e := range r.h.st.tasks.m {
		v := e.v
		if !inInboxes(f, v.Ref.Conversation.Inbox) || !inConversation(f, v.Ref.Conversation) || !matchAny(f.Statuses, v.State) ||
			!r.ownedBy(f.Identity, v.Ref.Conversation) {
			continue
		}
		if paged && compareTaskPos(taskPosOf(v), after) <= 0 {
			continue
		}
		rows = append(rows, v)
	}
	slices.SortFunc(rows, func(a, b model.Task) int { return compareTaskPos(taskPosOf(a), taskPosOf(b)) })
	p := model.Page[model.Task]{Complete: len(rows) <= limit, Next: req.Page.After}
	rows = rows[:min(limit, len(rows))]
	for _, v := range rows {
		p.Items = append(p.Items, v.Clone())
	}
	if len(rows) > 0 {
		p.Next = encodeTaskCursor(taskPosOf(rows[len(rows)-1]))
	}
	return p, nil
}

// ownedBy reports whether owner is nil or the Owner of conversation c.
func (r reader) ownedBy(owner *model.IdentityRef, c model.ConversationRef) bool {
	if owner == nil {
		return true
	}
	e, ok := r.h.st.conversations.get(c)
	return ok && e.v.Owner != nil && *e.v.Owner == *owner
}

func (r reader) Replies(ctx context.Context, parent model.MessageRef, pr model.PageRequest) (model.Page[model.Message], error) {
	if err := r.h.scoped(parent); err != nil {
		return model.Page[model.Message]{}, err
	}
	return listTable(ctx, r.h, &r.h.st.messages, model.ListRequest{Page: pr}, filterSupport{},
		func(_ model.Filter, v model.Message) bool { return v.InReplyTo != nil && *v.InReplyTo == parent }, model.Message.Clone)
}

// taskPos is a task's place in status order.
type taskPos struct {
	unix  int64
	nanos int
	id    model.ID
}

func taskPosOf(v model.Task) taskPos {
	return taskPos{v.StatusAt.Unix(), v.StatusAt.Nanosecond(), v.Ref.ID}
}

// compareTaskPos orders the most recent status first, then the greater ID.
func compareTaskPos(a, b taskPos) int {
	switch {
	case a.unix != b.unix:
		return compareDesc(a.unix, b.unix)
	case a.nanos != b.nanos:
		return compareDesc(a.nanos, b.nanos)
	}
	return -strings.Compare(string(a.id), string(b.id))
}

func compareDesc[T int | int64](a, b T) int {
	if a > b {
		return -1
	}
	return 1
}

// Task cursors are "mem.t.<unix>.<nanos>.<id>", the status position of
// the last task returned.
const taskCursorPrefix = cursorPrefix + "t."

func encodeTaskCursor(p taskPos) model.Cursor {
	return model.Cursor(taskCursorPrefix + strconv.FormatInt(p.unix, 10) + "." + strconv.Itoa(p.nanos) + "." + string(p.id))
}

// decodeTaskCursor returns the cursor's position and whether it names one;
// the empty cursor is the start.
func decodeTaskCursor(c model.Cursor) (taskPos, bool, error) {
	if c == "" {
		return taskPos{}, false, nil
	}
	malformed := invalid("cursor: malformed")
	s, ok := strings.CutPrefix(string(c), taskCursorPrefix)
	if !ok {
		return taskPos{}, false, malformed
	}
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return taskPos{}, false, malformed
	}
	unix, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return taskPos{}, false, malformed
	}
	nanos, err := strconv.Atoi(parts[1])
	if err != nil || nanos < 0 || nanos >= int(time.Second) {
		return taskPos{}, false, malformed
	}
	id := model.ID(parts[2])
	if id.Validate() != nil {
		return taskPos{}, false, malformed
	}
	return taskPos{unix, nanos, id}, true, nil
}
