package memory

import (
	"context"
	"slices"
	"strings"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

func errCursorExpired() error {
	return model.NewError(model.CursorExpired, "cursor: behind the retained event log")
}

func errSaturated() error {
	return model.NewError(model.Unavailable, "work kind at its in-flight cap")
}

// eventHead is the position after the last committed event.
func (s *state) eventHead() uint64 { return s.eventBase + uint64(len(s.events.items)) }

func (r reader) EventBase(ctx context.Context) (model.Cursor, error) {
	if err := r.h.enter(ctx); err != nil {
		return "", err
	}
	defer r.h.mu.Unlock()
	return encodeCursor(r.h.st.eventBase), nil
}

func (r reader) WorkDepth(ctx context.Context, kind string) (store.WorkDepth, error) {
	if err := r.h.enter(ctx); err != nil {
		return store.WorkDepth{}, err
	}
	defer r.h.mu.Unlock()
	if err := required("kind", kind); err != nil {
		return store.WorkDepth{}, err
	}
	return r.h.workDepth(kind), nil
}

// workDepth counts kind's items; a claim counts while its until has not
// passed.
func (h *handle) workDepth(kind string) store.WorkDepth {
	var d store.WorkDepth
	now := h.now()
	for _, e := range h.st.work.m {
		switch {
		case e.v.Kind != kind:
		case h.quarantined(e.v.ID):
			d.Quarantined++
		case !e.v.Fence.IsZero() && now.Before(e.v.Until):
			d.Claimed++
		default:
			d.Pending++
		}
	}
	return d
}

func (h *handle) quarantined(id model.ID) bool {
	_, ok := h.st.quarantine.get(id)
	return ok
}

func (r reader) QuarantinedWork(ctx context.Context, res model.Ref) ([]store.Work, error) {
	if err := r.h.enter(ctx); err != nil {
		return nil, err
	}
	defer r.h.mu.Unlock()
	if err := r.h.scoped(res); err != nil {
		return nil, err
	}
	var out []store.Work
	for _, e := range r.h.st.work.sorted(func(w store.Work) bool { return w.Resource == res }) {
		if r.h.quarantined(e.v.ID) {
			out = append(out, e.v)
		}
	}
	return out, nil
}

func (t tx) QuarantineWork(ctx context.Context, w store.Work) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := w.ID.Validate(); err != nil {
		return err
	}
	cur, ok := h.st.work.get(w.ID)
	if !ok {
		return notFound()
	}
	lease, live := h.liveLease(cur.v.Resource)
	if !live || lease.Fence != w.Fence || cur.v.Fence != w.Fence || h.quarantined(w.ID) {
		return conflict("fence is not the current lease")
	}
	h.st.quarantine.set(w.ID, entry[struct{}]{})
	h.st.leases.del(cur.v.Resource)
	return nil
}

func (t tx) RequeueWork(ctx context.Context, id model.ID) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := id.Validate(); err != nil {
		return err
	}
	cur, ok := h.st.work.get(id)
	if !ok {
		return notFound()
	}
	if !h.quarantined(id) {
		return conflict("work is not quarantined")
	}
	w := cur.v
	w.Fence, w.Until, w.Claims, w.NotBefore = store.Fence{}, time.Time{}, 0, time.Time{}
	h.st.work.set(id, entry[store.Work]{seq: cur.seq, v: w})
	h.st.quarantine.del(id)
	return nil
}

func (t tx) Purge(ctx context.Context, req store.PurgeRequest) (store.PurgeResult, error) {
	h, err := t.write(ctx)
	if err != nil {
		return store.PurgeResult{}, err
	}
	defer h.mu.Unlock()
	if req.Before.IsZero() {
		return store.PurgeResult{}, invalid("before: required")
	}
	limit, err := pageLimit(req.Limit)
	if err != nil {
		return store.PurgeResult{}, invalid("limit: must be positive")
	}
	switch req.Family {
	case store.FamilyEvents:
		return h.purgeEvents(req.Before, limit), nil
	case store.FamilyOperations:
		return h.purgeOperations(req.Before, limit), nil
	case store.FamilyAttempts:
		return h.purgeAttempts(req.Before, limit), nil
	case store.FamilyIngress:
		return h.purgeIngress(req.Before, limit), nil
	case store.FamilyExports:
		return h.purgeExports(req.Before, limit), nil
	case store.FamilyContacts:
		return h.purgeContacts(req.Before, limit), nil
	case store.FamilyInteractionTokens:
		return h.purgeInteractionTokens(req.Before, limit), nil
	}
	return store.PurgeResult{}, invalid("family: unknown")
}

// purgeEvents drops the longest prefix of at most limit events written
// before before.
func (h *handle) purgeEvents(before time.Time, limit int) store.PurgeResult {
	meta := h.st.eventMeta.items
	n := 0
	for n < len(meta) && n < limit && meta[n].at.Before(before) {
		n++
	}
	if n > 0 {
		for _, ev := range h.st.events.items[:n] {
			h.st.eventIDs.del(ev.ID)
		}
		h.st.events.drop(n)
		h.st.eventMeta.drop(n)
		h.st.eventBase += uint64(n) // #nosec G115 -- n counts dropped items, never negative
	}
	rest := h.st.eventMeta.items
	more := len(rest) > 0 && rest[0].at.Before(before)
	return store.PurgeResult{Purged: n, Complete: !more, EventBase: encodeCursor(h.st.eventBase)}
}

// idle reports whether no work item refers to res and res holds no
// unexpired lease.
func (h *handle) idle(res model.Ref) bool {
	if _, live := h.liveLease(res); live {
		return false
	}
	for _, e := range h.st.work.m {
		if e.v.Resource == res {
			return false
		}
	}
	return true
}

func (h *handle) purgeOperations(before time.Time, limit int) store.PurgeResult {
	r := store.PurgeResult{Complete: true}
	for _, e := range h.st.operations.sorted(nil) {
		if !e.at.Before(before) || !h.idle(e.v.Operation.Ref.Ref()) {
			continue
		}
		if r.Purged == limit {
			r.Complete = false
			break
		}
		h.st.operations.del(e.v.Operation.Ref)
		h.st.operationKeys.del(e.v.Key)
		h.st.attempts.del(e.v.Operation.Ref)
		r.Purged++
	}
	return r
}

func (h *handle) purgeAttempts(before time.Time, limit int) store.PurgeResult {
	r := store.PurgeResult{Complete: true}
	for _, op := range h.st.operations.sorted(nil) {
		ref := op.v.Operation.Ref
		e, ok := h.st.attempts.get(ref)
		if !ok || !h.idle(ref.Ref()) {
			continue
		}
		var keep []attempt
		for _, a := range e.v {
			switch {
			case !a.at.Before(before):
				keep = append(keep, a)
			case r.Purged == limit:
				r.Complete = false
				keep = append(keep, a)
			default:
				r.Purged++
			}
		}
		if len(keep) != len(e.v) {
			h.st.attempts.set(ref, entry[[]attempt]{v: slices.Clip(keep)})
		}
	}
	return r
}

func (h *handle) purgeIngress(before time.Time, limit int) store.PurgeResult {
	r := store.PurgeResult{Complete: true}
	for _, e := range h.st.ingress.sorted(nil) {
		if !e.at.Before(before) {
			continue
		}
		if r.Purged == limit {
			r.Complete = false
			break
		}
		h.st.ingress.del(e.v.ID)
		h.st.ingressDedup.del(dedupKey{e.v.Channel, e.v.DedupKey})
		r.Purged++
	}
	return r
}

// purgeExports deletes up to limit exports written before before and
// records a staging intent for each one's blob key.
func (h *handle) purgeExports(before time.Time, limit int) store.PurgeResult {
	r := store.PurgeResult{Complete: true}
	for _, e := range h.st.exports.sorted(nil) {
		if !e.at.Before(before) {
			continue
		}
		if r.Purged == limit {
			r.Complete = false
			break
		}
		h.st.exports.del(e.v.Export.Ref)
		if _, held := h.st.staging.get(e.v.Blob); !held {
			h.st.staging.set(e.v.Blob, entry[store.StagingIntent]{
				seq: h.st.next(), at: h.now(), v: store.StagingIntent{Key: e.v.Blob, Export: e.v.Export.Ref},
			})
		}
		r.Blobs = append(r.Blobs, e.v.Blob)
		r.Purged++
	}
	return r
}

// purgeContacts deletes up to limit contacts no identity points at whose
// latest link record leaving them was written before before, oldest such
// record first.
func (h *handle) purgeContacts(before time.Time, limit int) store.PurgeResult {
	left := map[model.ContactRef]time.Time{}
	for i, rec := range h.st.links.items {
		if at := h.st.linkAt.items[i]; at.After(left[rec.From]) {
			left[rec.From] = at
		}
	}
	for _, e := range h.st.identities.m {
		delete(left, e.v.Contact)
	}
	type orphan struct {
		ref model.ContactRef
		at  time.Time
	}
	var due []orphan
	for ref, at := range left {
		if _, ok := h.st.contacts.get(ref); ok && at.Before(before) {
			due = append(due, orphan{ref, at})
		}
	}
	slices.SortFunc(due, func(a, b orphan) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}
		return strings.Compare(string(a.ref.ID), string(b.ref.ID))
	})
	r := store.PurgeResult{Purged: min(len(due), limit), Complete: len(due) <= limit}
	for _, o := range due[:r.Purged] {
		h.st.contacts.del(o.ref)
	}
	return r
}
