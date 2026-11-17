package postgres

import (
	"context"
	"slices"
	"strings"

	"hop.top/uma/model"
	"hop.top/uma/store/postgres/internal/db"
)

// StreamsOf reads the conversation's streams that are not terminal. The
// state lives in the stream body; the scan is bounded by the
// conversation's rows.
func (r reader) StreamsOf(ctx context.Context, conv model.ConversationRef) ([]model.Stream, error) {
	var out []model.Stream
	err := r.h.call(ctx, func() error {
		if err := r.h.scoped(conv); err != nil {
			return err
		}
		rows, err := r.h.q.StreamsOf(ctx, db.StreamsOfParams{
			TenantID: r.h.tenantID, InboxID: string(conv.Inbox.ID), ConversationID: string(conv.ID),
		})
		if err != nil {
			return r.h.db(ctx, err)
		}
		out = make([]model.Stream, 0, len(rows))
		for _, b := range rows {
			s, err := decode[model.Stream](b)
			if err != nil {
				return err
			}
			out = append(out, s)
		}
		slices.SortFunc(out, func(a, b model.Stream) int { return strings.Compare(string(a.Ref.ID), string(b.Ref.ID)) })
		return nil
	})
	return out, err
}
