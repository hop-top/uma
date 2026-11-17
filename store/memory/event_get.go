package memory

import (
	"context"

	"hop.top/uma/model"
)

// Event looks the event up by the position eventIDs records for it. Purge
// drops the ID with the event, so a purged event is not found.
func (r reader) Event(ctx context.Context, ref model.EventRef) (model.Event, error) {
	if err := r.h.enter(ctx); err != nil {
		return model.Event{}, err
	}
	defer r.h.mu.Unlock()
	if err := r.h.scoped(ref); err != nil {
		return model.Event{}, err
	}
	e, ok := r.h.st.eventIDs.get(ref.ID)
	if !ok || e.v <= r.h.st.eventBase {
		return model.Event{}, notFound()
	}
	return r.h.st.events.items[e.v-r.h.st.eventBase-1].Clone(), nil
}
