package memory

import (
	"context"

	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// checkKey validates a blob key and requires it in the unit's tenant.
func (h *handle) checkKey(k ports.BlobKey) error {
	if err := k.Validate(); err != nil {
		return err
	}
	return h.sameTenant(k.Tenant)
}

// checkOwner requires exactly one owner, in the unit's tenant.
func (h *handle) checkOwner(in store.StagingIntent) error {
	switch {
	case in.Attachment.IsZero() == in.Export.IsZero():
		return invalid("staging intent: exactly one of attachment and export required")
	case !in.Export.IsZero():
		return h.scoped(in.Export)
	}
	return h.scoped(in.Attachment)
}

func (r reader) Staging(ctx context.Context, k ports.BlobKey) (store.StagingIntent, error) {
	if err := r.h.enter(ctx); err != nil {
		return store.StagingIntent{}, err
	}
	defer r.h.mu.Unlock()
	if err := r.h.checkKey(k); err != nil {
		return store.StagingIntent{}, err
	}
	e, ok := r.h.st.staging.get(k)
	if !ok {
		return store.StagingIntent{}, notFound()
	}
	return e.v, nil
}

func (t tx) RecordStaging(ctx context.Context, in store.StagingIntent) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := firstErr(h.checkKey(in.Key), h.checkOwner(in)); err != nil {
		return err
	}
	if in.Abandoned {
		return invalid("abandoned: a recorded intent is not abandoned")
	}
	if _, ok := h.st.staging.get(in.Key); ok {
		return conflict("blob key already recorded")
	}
	h.st.staging.set(in.Key, entry[store.StagingIntent]{seq: h.st.next(), at: h.now(), v: in})
	return nil
}

func (t tx) BindStaging(ctx context.Context, k ports.BlobKey) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := h.checkKey(k); err != nil {
		return err
	}
	e, ok := h.st.staging.get(k)
	switch {
	case !ok:
		return notFound()
	case e.v.Abandoned:
		return conflict("staging intent abandoned")
	}
	h.st.staging.del(k)
	return nil
}

func (t tx) ClearStaging(ctx context.Context, k ports.BlobKey) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := h.checkKey(k); err != nil {
		return err
	}
	if _, ok := h.st.staging.get(k); ok {
		h.st.staging.del(k)
	}
	return nil
}

func (t tx) AbandonStaging(ctx context.Context, sw store.StagingSweep) (store.StagingBatch, error) {
	h, err := t.write(ctx)
	if err != nil {
		return store.StagingBatch{}, err
	}
	defer h.mu.Unlock()
	if sw.Before.IsZero() {
		return store.StagingBatch{}, invalid("before: required")
	}
	limit, err := pageLimit(sw.Limit)
	if err != nil {
		return store.StagingBatch{}, invalid("limit: must be positive")
	}
	b := store.StagingBatch{Complete: true}
	for _, e := range h.st.staging.sorted(nil) {
		if !e.v.Abandoned && !e.at.Before(sw.Before) {
			continue
		}
		if len(b.Intents) == limit {
			b.Complete = false
			break
		}
		e.v.Abandoned = true
		h.st.staging.set(e.v.Key, e)
		b.Intents = append(b.Intents, e.v)
	}
	return b, nil
}
