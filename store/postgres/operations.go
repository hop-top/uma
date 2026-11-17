package postgres

import (
	"context"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres/internal/db"
)

// Operations pages the ledger most recently written first, after the
// cursor's (write time, ID) position, from the (written_at, id) indexes.
// Times compare to the microsecond the column holds.
func (r reader) Operations(ctx context.Context, req model.ListRequest) (model.Page[store.OperationRecord], error) {
	f := req.Filter
	var p model.Page[store.OperationRecord]
	err := r.h.call(ctx, func() error {
		limit, err := pageLimit(req.Page.Limit)
		if err != nil {
			return err
		}
		after, err := decodeOperationCursor(req.Page.After)
		if err != nil {
			return err
		}
		if err := r.h.checkFilter(f, filterSupport{inboxes: true, conversation: true, statuses: true, since: true, before: true}); err != nil {
			return err
		}
		inbox, conv := conversationArgs(f)
		// One inbox or state is an equality, which the indexes serve in
		// order; several are lists, which need a sort.
		p1 := db.ListOperationsParams{
			TenantID: r.h.tenantID, AfterAt: after.at, AfterID: after.id, Inboxes: []string{}, States: []string{},
			ConversationInbox: inbox, ConversationID: conv,
			Since: microsecond(f.Since), Before: microsecond(f.Before), Lim: limit + 1,
		}
		if len(f.Inboxes) == 1 {
			p1.Inbox = optID(f.Inboxes[0].ID)
		} else {
			p1.Inboxes = inboxIDs(f)
		}
		if len(f.Statuses) == 1 {
			p1.State = &f.Statuses[0]
		} else {
			p1.States = strs(f.Statuses)
		}
		rows, err := r.h.q.ListOperations(ctx, p1)
		if err != nil {
			return r.h.db(ctx, err)
		}
		p = model.Page[store.OperationRecord]{Complete: len(rows) <= int(limit), Next: req.Page.After}
		rows = rows[:min(int(limit), len(rows))]
		for _, row := range rows {
			rec, err := decodeOperation(row.Body, row.WrittenAt)
			if err != nil {
				return err
			}
			p.Items = append(p.Items, rec)
		}
		if n := len(rows); n > 0 {
			p.Next = encodeOperationCursor(operationPos{rows[n-1].WrittenAt, rows[n-1].ID})
		}
		return nil
	})
	return p, err
}

// decodeOperation reads a stored operation body and sets the operation's
// UpdatedAt to writtenAt, the write time the row holds. The body carries no
// such value (see encodeOperation): the column is the one source, the one the
// list orders and bounds by.
func decodeOperation(body []byte, writtenAt time.Time) (store.OperationRecord, error) {
	rec, err := decode[store.OperationRecord](body)
	if err != nil {
		return store.OperationRecord{}, err
	}
	rec.Operation.UpdatedAt = writtenAt.UTC()
	return rec, nil
}

// encodeOperation returns the body to store for rec: without UpdatedAt,
// whatever the caller put there, since a read takes it from the write time.
func encodeOperation(rec store.OperationRecord) ([]byte, error) {
	rec.Operation.UpdatedAt = time.Time{}
	return encode(&rec)
}

// writeTime is the instant a write is recorded at: now to the microsecond
// the written_at column holds, so the time a read shows is the time stored.
func (h *handle) writeTime() time.Time { return time.UnixMicro(h.now().UnixMicro()).UTC() }

// microsecond truncates t to the precision the written_at column holds.
func microsecond(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := time.UnixMicro(t.UnixMicro()).UTC()
	return &u
}

// operationPos is an operation's place in write order.
type operationPos struct {
	at time.Time
	id string
}

// Operation cursors are "pg.o.<unix microseconds>.<id>", the position of
// the last operation returned.
const operationCursorPrefix = cursorPrefix + "o."

var (
	// pastEveryOperation is the position the first page starts after: later
	// than any write time, with the greatest ID.
	pastEveryOperation = operationPos{time.UnixMicro(253402300799999999).UTC(), "ffffffff-ffff-ffff-ffff-ffffffffffff"}
)

func encodeOperationCursor(p operationPos) model.Cursor {
	return model.Cursor(operationCursorPrefix + strconv.FormatInt(p.at.UnixMicro(), 10) + "." + p.id)
}

// decodeOperationCursor returns the cursor's position; the empty cursor
// is the start.
func decodeOperationCursor(c model.Cursor) (operationPos, error) {
	if c == "" {
		return pastEveryOperation, nil
	}
	malformed := invalid("cursor: malformed")
	s, ok := strings.CutPrefix(string(c), operationCursorPrefix)
	if !ok {
		return operationPos{}, malformed
	}
	micro, id, ok := strings.Cut(s, ".")
	if !ok {
		return operationPos{}, malformed
	}
	us, err := strconv.ParseInt(micro, 10, 64)
	if err != nil || model.ID(id).Validate() != nil {
		return operationPos{}, malformed
	}
	return operationPos{time.UnixMicro(us).UTC(), id}, nil
}
