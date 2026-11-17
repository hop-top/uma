package postgres

import (
	"context"
	"encoding/json"
	"math"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres/internal/db"
)

func (r reader) RecipientLogEntries(ctx context.Context, key store.RecipientLogKey, after uint64, limit int) ([]store.RecipientLogEntry, error) {
	var out []store.RecipientLogEntry
	err := r.h.call(ctx, func() error {
		if err := r.h.scoped(key); err != nil {
			return err
		}
		if limit <= 0 {
			return invalid("limit: must be positive")
		}
		if after >= math.MaxInt64 {
			return nil // no sequence is above it
		}
		rows, err := r.h.q.ListRecipientLog(ctx, db.ListRecipientLogParams{
			TenantID: r.h.tenantID, ChannelID: string(key.Channel.ID), Namespace: key.Namespace, Recipient: key.Recipient,
			After: int64(after), Lim: int32(min(limit, maxPage)), // #nosec G115 -- bounded above
		})
		if err != nil {
			return r.h.db(ctx, err)
		}
		for _, row := range rows {
			out = append(out, store.RecipientLogEntry{
				Seq: uint64(row.Seq), // #nosec G115 -- column CHECK keeps it positive
				Value: model.Extension{
					Name:    row.ValueName,
					Version: uint32(row.ValueVersion), // #nosec G115 -- column CHECK bounds it to uint32
					Data:    nonEmpty(row.ValueData),
				},
			})
		}
		return nil
	})
	return out, err
}

func (t txn) AppendRecipientLog(ctx context.Context, key store.RecipientLogKey, value model.Extension, retain int) (uint64, error) {
	h := t.h
	var seq int64
	err := h.call(ctx, func() error {
		if err := firstErr(h.scoped(key), required("value name", value.Name)); err != nil {
			return err
		}
		if len(value.Data) > 0 && !json.Valid(value.Data) {
			return invalid("value data: must be valid JSON")
		}
		if retain <= 0 {
			return invalid("retain: must be positive")
		}
		if err := h.requireChannel(ctx, key.Channel); err != nil {
			return err
		}
		ch := string(key.Channel.ID)
		var err error
		seq, err = h.q.NextRecipientLogSeq(ctx, db.NextRecipientLogSeqParams{
			TenantID: h.tenantID, ChannelID: ch, Namespace: key.Namespace, Recipient: key.Recipient,
		})
		if err != nil {
			return h.db(ctx, err)
		}
		if err := h.q.InsertRecipientLogEntry(ctx, db.InsertRecipientLogEntryParams{
			TenantID: h.tenantID, ChannelID: ch, Namespace: key.Namespace, Recipient: key.Recipient, Seq: seq,
			ValueName: value.Name, ValueVersion: int64(value.Version), ValueData: extData(value.Data),
		}); err != nil {
			return h.db(ctx, err)
		}
		if through := seq - int64(retain); through > 0 {
			if err := h.q.TrimRecipientLog(ctx, db.TrimRecipientLogParams{
				TenantID: h.tenantID, ChannelID: ch, Namespace: key.Namespace, Recipient: key.Recipient, Through: through,
			}); err != nil {
				return h.db(ctx, err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return uint64(seq), nil // #nosec G115 -- sequences start at 1
}

// EraseRecipientLog deletes the retained entries of key's log and keeps its
// head row, and so its last sequence; see store.Tx.EraseRecipientLog.
func (t txn) EraseRecipientLog(ctx context.Context, key store.RecipientLogKey) (int, error) {
	h := t.h
	var n int64
	err := h.call(ctx, func() error {
		if err := h.scoped(key); err != nil {
			return err
		}
		var err error
		n, err = h.q.EraseRecipientLog(ctx, db.EraseRecipientLogParams{
			TenantID: h.tenantID, ChannelID: string(key.Channel.ID), Namespace: key.Namespace, Recipient: key.Recipient,
		})
		return h.db(ctx, err)
	})
	return int(n), err
}
