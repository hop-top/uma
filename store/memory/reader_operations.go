package memory

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Operations pages the ledger most recently written first, after the
// cursor's (write time, ID) position. The memory store keeps no secondary
// index, so it visits every operation once, keeps only the page being built
// and clones only what it returns; a durable backend indexes the order.
// Times compare to the microsecond.
func (r reader) Operations(ctx context.Context, req model.ListRequest) (model.Page[store.OperationRecord], error) {
	if err := r.h.enter(ctx); err != nil {
		return model.Page[store.OperationRecord]{}, err
	}
	defer r.h.mu.Unlock()
	limit, err := pageLimit(req.Page.Limit)
	if err != nil {
		return model.Page[store.OperationRecord]{}, err
	}
	after, paged, err := decodeOperationCursor(req.Page.After)
	if err != nil {
		return model.Page[store.OperationRecord]{}, err
	}
	f := req.Filter
	if err := r.h.checkFilter(f, filterSupport{inboxes: true, conversation: true, statuses: true, since: true, before: true}); err != nil {
		return model.Page[store.OperationRecord]{}, err
	}
	var since, before int64
	if f.Since != nil {
		since = f.Since.UnixMicro()
	}
	if f.Before != nil {
		before = f.Before.UnixMicro()
	}

	// top holds the limit+1 most recent candidates, most recent first; the
	// extra one only tells whether the page is the last.
	type candidate struct {
		pos operationPos
		rec *store.OperationRecord
		at  time.Time
	}
	top := make([]candidate, 0, min(limit, 64)+1)
	for _, e := range r.h.st.operations.m {
		pos := operationPos{e.at.UnixMicro(), e.v.Operation.Ref.ID}
		if paged && compareOperationPos(pos, after) <= 0 {
			continue
		}
		if len(top) > limit && compareOperationPos(pos, top[limit].pos) >= 0 {
			continue
		}
		rec := &e.v
		if f.Since != nil && pos.micro < since || f.Before != nil && pos.micro >= before ||
			len(f.Statuses) > 0 && !slices.Contains(f.Statuses, string(rec.Operation.State)) {
			continue
		}
		if len(f.Inboxes) > 0 || f.Conversation != nil {
			sc := rec.Scope()
			if len(f.Inboxes) > 0 && !slices.ContainsFunc(f.Inboxes, func(in model.InboxRef) bool { return in.ID == sc.Inbox }) {
				continue
			}
			if c := f.Conversation; c != nil && (c.Inbox.ID != sc.Inbox || c.ID != sc.Conversation) {
				continue
			}
		}
		at, _ := slices.BinarySearchFunc(top, pos, func(x candidate, p operationPos) int {
			return compareOperationPos(x.pos, p)
		})
		top = slices.Insert(top, at, candidate{pos, rec, e.at})
		if len(top) > limit+1 {
			top = top[:limit+1]
		}
	}
	p := model.Page[store.OperationRecord]{Complete: len(top) <= limit, Next: req.Page.After}
	top = top[:min(limit, len(top))]
	for _, c := range top {
		p.Items = append(p.Items, shownOperation(entry[store.OperationRecord]{at: c.at, v: *c.rec}))
	}
	if len(top) > 0 {
		p.Next = encodeOperationCursor(top[len(top)-1].pos)
	}
	return p, nil
}

// operationPos is an operation's place in write order.
type operationPos struct {
	micro int64
	id    model.ID
}

// compareOperationPos orders the most recent write first, then the greater
// ID.
func compareOperationPos(a, b operationPos) int {
	switch {
	case a.micro != b.micro:
		if a.micro > b.micro {
			return -1
		}
		return 1
	}
	return -strings.Compare(string(a.id), string(b.id))
}

// Operation cursors are "mem.o.<unix microseconds>.<id>", the position of
// the last operation returned.
const operationCursorPrefix = cursorPrefix + "o."

func encodeOperationCursor(p operationPos) model.Cursor {
	return model.Cursor(operationCursorPrefix + strconv.FormatInt(p.micro, 10) + "." + string(p.id))
}

// decodeOperationCursor returns the cursor's position and whether it names
// one; the empty cursor is the start.
func decodeOperationCursor(c model.Cursor) (operationPos, bool, error) {
	if c == "" {
		return operationPos{}, false, nil
	}
	malformed := invalid("cursor: malformed")
	s, ok := strings.CutPrefix(string(c), operationCursorPrefix)
	if !ok {
		return operationPos{}, false, malformed
	}
	micro, id, ok := strings.Cut(s, ".")
	if !ok {
		return operationPos{}, false, malformed
	}
	us, err := strconv.ParseInt(micro, 10, 64)
	if err != nil || model.ID(id).Validate() != nil {
		return operationPos{}, false, malformed
	}
	return operationPos{us, model.ID(id)}, true, nil
}

// shownOperation is the stored operation as a reader gets it: a copy whose
// UpdatedAt is the time of its latest write, to the microsecond. The store
// keeps no such value in the record: it is the entry's write time, the one
// Operations orders and bounds by, so a record handed to a write can never
// supply it.
func shownOperation(e entry[store.OperationRecord]) store.OperationRecord {
	rec := e.v.Clone()
	rec.Operation.UpdatedAt = time.UnixMicro(e.at.UnixMicro()).UTC()
	return rec
}

// storedOperation is rec as the store keeps it, without a write time.
func storedOperation(rec store.OperationRecord) store.OperationRecord {
	rec = rec.Clone()
	rec.Operation.UpdatedAt = time.Time{}
	return rec
}
