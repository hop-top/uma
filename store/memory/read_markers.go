package memory

import (
	"context"

	"hop.top/uma/model"
)

// readMarkerKey keys a read marker by conversation and actor.
type readMarkerKey struct {
	conv  model.ConversationRef
	actor model.ActorID
}

func (r reader) ReadMarker(ctx context.Context, conv model.ConversationRef, actor model.ActorID) (model.ReadMarker, error) {
	if err := r.h.enter(ctx); err != nil {
		return model.ReadMarker{}, err
	}
	defer r.h.mu.Unlock()
	if err := firstErr(r.h.scoped(conv), required("actor", string(actor))); err != nil {
		return model.ReadMarker{}, err
	}
	e, ok := r.h.st.readMarkers.get(readMarkerKey{conv, actor})
	if !ok {
		return model.ReadMarker{}, notFound()
	}
	return e.v, nil
}

func (t tx) PutReadMarker(ctx context.Context, v model.ReadMarker, expected model.Revision) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := firstErr(h.scoped(v.Conversation), v.Validate()); err != nil {
		return err
	}
	if _, ok := h.st.conversations.get(v.Conversation); !ok {
		return notFound()
	}
	return put(h, &h.st.readMarkers, readMarkerKey{v.Conversation, v.Actor}, v, readMarkerRev, expected)
}

func readMarkerRev(v model.ReadMarker) model.Revision { return v.Revision }
