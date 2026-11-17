package postgres

import (
	"context"

	"hop.top/uma/model"
	"hop.top/uma/store/postgres/internal/db"
)

// Event reads one event through the (tenant, id) unique index. Purge deletes
// the row with the event, so a purged event is not found.
func (r reader) Event(ctx context.Context, ref model.EventRef) (model.Event, error) {
	return getBody[model.Event](ctx, r.h, ref, func() ([]byte, error) {
		b, err := r.h.q.GetEvent(ctx, db.GetEventParams{TenantID: r.h.tenantID, ID: string(ref.ID)})
		return b, err
	})
}
