package memory

import (
	"context"
	"encoding/json"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// recipientLog is one recipient log: the last sequence appended and the
// retained entries, oldest first. A stored recipientLog is never mutated:
// an append stores a new one.
type recipientLog struct {
	last    uint64
	entries []store.RecipientLogEntry
}

func (r reader) RecipientLogEntries(ctx context.Context, key store.RecipientLogKey, after uint64, limit int) ([]store.RecipientLogEntry, error) {
	if err := r.h.enter(ctx); err != nil {
		return nil, err
	}
	defer r.h.mu.Unlock()
	if err := r.h.scoped(key); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, invalid("limit: must be positive")
	}
	limit = min(limit, maxPage)
	e, ok := r.h.st.recipientLogs.get(key)
	if !ok {
		return nil, nil
	}
	var out []store.RecipientLogEntry
	for _, x := range e.v.entries {
		if x.Seq <= after {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, x.Clone())
	}
	return out, nil
}

func (t tx) AppendRecipientLog(ctx context.Context, key store.RecipientLogKey, value model.Extension, retain int) (uint64, error) {
	h, err := t.write(ctx)
	if err != nil {
		return 0, err
	}
	defer h.mu.Unlock()
	if err := firstErr(h.scoped(key), checkRecipientValue(value)); err != nil {
		return 0, err
	}
	if retain <= 0 {
		return 0, invalid("retain: must be positive")
	}
	if err := h.requireChannel(key.Channel); err != nil {
		return 0, err
	}
	cur, found := h.st.recipientLogs.get(key)
	seq := cur.seq
	if !found {
		seq = h.st.next()
	}
	next := cur.v.last + 1
	kept := cur.v.entries
	if over := len(kept) + 1 - retain; over > 0 {
		kept = kept[over:]
	}
	entries := make([]store.RecipientLogEntry, 0, len(kept)+1)
	entries = append(entries, kept...)
	entries = append(entries, store.RecipientLogEntry{Seq: next, Value: value.Clone()})
	h.st.recipientLogs.set(key, entry[recipientLog]{seq: seq, v: recipientLog{last: next, entries: entries}})
	return next, nil
}

// checkRecipientValue requires a named value whose data, when present, is
// valid JSON.
func checkRecipientValue(v model.Extension) error {
	if err := required("value name", v.Name); err != nil {
		return err
	}
	if len(v.Data) > 0 && !json.Valid(v.Data) {
		return invalid("value data: must be valid JSON")
	}
	return nil
}

// EraseRecipientLog deletes the retained entries of key's log and keeps its
// last sequence; see store.Tx.EraseRecipientLog.
func (t tx) EraseRecipientLog(ctx context.Context, key store.RecipientLogKey) (int, error) {
	h, err := t.write(ctx)
	if err != nil {
		return 0, err
	}
	defer h.mu.Unlock()
	if err := h.scoped(key); err != nil {
		return 0, err
	}
	cur, found := h.st.recipientLogs.get(key)
	if !found || len(cur.v.entries) == 0 {
		return 0, nil
	}
	h.st.recipientLogs.set(key, entry[recipientLog]{seq: cur.seq, v: recipientLog{last: cur.v.last}})
	return len(cur.v.entries), nil
}
