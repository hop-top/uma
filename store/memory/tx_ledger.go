package memory

import (
	"bytes"
	"context"
	"slices"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

func (t tx) ClaimOperation(ctx context.Context, rec store.OperationRecord) (store.OperationRecord, bool, error) {
	h, err := t.write(ctx)
	if err != nil {
		return store.OperationRecord{}, false, err
	}
	defer h.mu.Unlock()
	k := rec.Key
	if err := firstErr(
		h.scoped(rec.Operation.Ref),
		h.checkOperationKey(k),
		newRevision(rec.Operation.Revision, ""),
	); err != nil {
		return store.OperationRecord{}, false, err
	}
	if rec.FingerprintVersion == 0 || len(rec.Fingerprint) == 0 {
		return store.OperationRecord{}, false, invalid("fingerprint: version and value required")
	}
	if ref, ok := h.st.operationKeys.get(k); ok {
		cur, _ := h.st.operations.get(ref.v)
		if cur.v.FingerprintVersion != rec.FingerprintVersion || !bytes.Equal(cur.v.Fingerprint, rec.Fingerprint) {
			return store.OperationRecord{}, false, conflict("operation ID reused with a different payload")
		}
		return shownOperation(cur), false, nil
	}
	if _, ok := h.st.operations.get(rec.Operation.Ref); ok {
		return store.OperationRecord{}, false, conflict("operation reference already claimed under another key")
	}
	e := entry[store.OperationRecord]{seq: h.st.next(), at: h.now(), v: storedOperation(rec)}
	h.st.operations.set(rec.Operation.Ref, e)
	h.st.operationKeys.set(k, entry[model.OperationRef]{v: rec.Operation.Ref})
	return shownOperation(e), true, nil
}

// checkOperationKey validates an operation key and scopes it to the unit's
// tenant.
func (h *handle) checkOperationKey(k store.OperationKey) error {
	return firstErr(
		h.scoped(k.Target),
		h.sameTenant(k.Tenant),
		k.Caller.Validate(),
		required("key.kind", k.Kind),
		k.ID.Validate(),
	)
}

func (t tx) AppendAttemptEvidence(ctx context.Context, ev store.AttemptEvidence) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := firstErr(h.scoped(ev.Operation), ev.Attempt.Validate()); err != nil {
		return err
	}
	if _, ok := h.st.operations.get(ev.Operation); !ok {
		return notFound()
	}
	cur, _ := h.st.attempts.get(ev.Operation)
	a := attempt{pos: h.st.next(), at: h.now(), ev: ev.Clone()}
	h.st.attempts.set(ev.Operation, entry[[]attempt]{v: append(slices.Clip(cur.v), a)})
	return nil
}

func (t tx) UpdateOperation(ctx context.Context, op model.Operation, expected model.Revision, fence store.Fence) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := firstErr(h.scoped(op.Ref), newRevision(op.Revision, expected)); err != nil {
		return err
	}
	cur, ok := h.st.operations.get(op.Ref)
	if !ok {
		return notFound()
	}
	if cur.v.Operation.Revision != expected {
		return conflict("stale expected revision")
	}
	lease, live := h.liveLease(op.Ref.Ref())
	switch {
	case fence.IsZero() && live:
		return conflict("operation is leased; fence required")
	case !fence.IsZero() && (!live || lease.Fence != fence):
		return conflict("fence is not the current lease")
	}
	rec := cur.v.Clone()
	rec.Operation = storedOperation(store.OperationRecord{Operation: op}).Operation
	h.st.operations.set(op.Ref, entry[store.OperationRecord]{seq: cur.seq, at: h.now(), v: rec})
	return nil
}

func (t tx) AppendEvents(ctx context.Context, events []model.Event) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	seen := make(map[model.ID]bool, len(events))
	for _, e := range events {
		if err := firstErr(e.Validate(), h.sameTenant(e.Tenant)); err != nil {
			return err
		}
		if _, dup := h.st.eventIDs.get(e.ID); dup || seen[e.ID] {
			return conflict("event ID already appended")
		}
		seen[e.ID] = true
	}
	for _, e := range events {
		if e.RecordedAt.After(h.st.eventRecordedMax) {
			h.st.eventRecordedMax = e.RecordedAt
		}
		h.st.events.append(e.Clone())
		h.st.eventIDs.set(e.ID, entry[uint64]{v: h.st.eventHead()})
		h.st.eventMeta.append(eventMeta{
			at: h.now(), conversation: store.EventConversation(e), recordedMax: h.st.eventRecordedMax,
		})
	}
	return nil
}

func (t tx) AdvanceCheckpoint(ctx context.Context, a store.CheckpointAdvance) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := firstErr(h.scoped(a.Subscription), required("revision", string(a.Revision))); err != nil {
		return err
	}
	cur, ok := h.st.subscriptions.get(a.Subscription)
	if !ok {
		return notFound()
	}
	rec := cur.v
	lease, live := h.liveLease(a.Subscription.Ref())
	switch b := rec.PendingBatch; {
	case rec.Subscription.Checkpoint != a.Prior:
		return conflict("stale prior checkpoint")
	case !live || lease.Fence != a.Fence:
		return conflict("fence is not the current lease")
	case b != nil && (b.Fence != a.Fence || b.Through != a.Next):
		return conflict("advance does not match the pending batch")
	case b != nil && !b.ExpiresAt.IsZero() && !h.now().Before(b.ExpiresAt):
		return conflict("pending batch expired")
	}
	prior, err := decodeCursor(a.Prior)
	if err != nil {
		return err
	}
	next, err := decodeCursor(a.Next)
	if err != nil {
		return err
	}
	if next < prior || next > h.st.eventHead() {
		return invalid("next: must lie between the prior checkpoint and the end of the log")
	}
	if a.Revision == rec.Subscription.Revision {
		return invalid("revision: must differ from the current revision")
	}
	rec = rec.Clone()
	rec.Subscription.Checkpoint, rec.Subscription.Revision, rec.PendingBatch = a.Next, a.Revision, nil
	h.st.subscriptions.set(a.Subscription, entry[store.SubscriptionRecord]{seq: cur.seq, v: rec})
	return nil
}

// liveLease returns the resource's lease when it has not expired.
func (h *handle) liveLease(res model.Ref) (store.Lease, bool) {
	e, ok := h.st.leases.get(res)
	if !ok || !h.now().Before(e.v.Until) {
		return store.Lease{}, false
	}
	return e.v, true
}

// grant leases res to owner under its next generation.
func (h *handle) grant(res model.Ref, owner string, until time.Time) store.Lease {
	g, _ := h.st.generations.get(res)
	l := store.Lease{Resource: res, Fence: store.Fence{Owner: owner, Generation: g.v + 1}, Until: until}
	h.st.generations.set(res, entry[uint64]{v: g.v + 1})
	h.st.leases.set(res, entry[store.Lease]{v: l})
	return l
}

func (h *handle) checkLeaseTerms(owner string, until time.Time) error {
	if err := required("owner", owner); err != nil {
		return err
	}
	if !until.After(h.now()) {
		return invalid("until: must be in the future")
	}
	return nil
}

func (t tx) EnqueueWork(ctx context.Context, w store.Work) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := firstErr(w.ID.Validate(), required("kind", w.Kind), h.scoped(w.Resource)); err != nil {
		return err
	}
	if !w.Fence.IsZero() || !w.Until.IsZero() || w.Claims != 0 {
		return invalid("work: fence, until and claims are set by claims")
	}
	if _, ok := h.st.work.get(w.ID); ok {
		return conflict("work ID already enqueued")
	}
	h.st.work.set(w.ID, entry[store.Work]{seq: h.st.next(), v: w})
	return nil
}

// ClaimWork marks the unit dirty only when it claims something: an idle poll
// writes nothing, so it must not commit a snapshot and invalidate units in
// flight.
func (t tx) ClaimWork(ctx context.Context, c store.WorkClaim) ([]store.Work, error) {
	h := t.h
	if err := h.enter(ctx); err != nil {
		return nil, err
	}
	defer h.mu.Unlock()
	if len(c.Kinds) == 0 {
		return nil, invalid("kinds: required")
	}
	limit, err := pageLimit(c.Limit)
	if err != nil {
		return nil, invalid("limit: must be positive")
	}
	if err := h.checkLeaseTerms(c.Owner, c.Until); err != nil {
		return nil, err
	}
	if c.MaxInFlight < 0 {
		return nil, invalid("max_in_flight: must not be negative")
	}
	inFlight := map[string]int{}
	if c.MaxInFlight > 0 {
		for _, k := range c.Kinds {
			inFlight[k] = h.workDepth(k).Claimed
		}
	}
	var out []store.Work
	heldBack := false
	for _, e := range h.st.work.sorted(func(w store.Work) bool { return slices.Contains(c.Kinds, w.Kind) }) {
		if len(out) == limit {
			break
		}
		if _, q := h.st.quarantine.get(e.v.ID); q {
			continue
		}
		if _, live := h.liveLease(e.v.Resource); live || h.now().Before(e.v.NotBefore) {
			continue
		}
		if c.MaxInFlight > 0 && inFlight[e.v.Kind] >= c.MaxInFlight {
			heldBack = true
			continue
		}
		l := h.grant(e.v.Resource, c.Owner, c.Until)
		w := e.v
		w.Fence, w.Until = l.Fence, l.Until
		w.Claims++
		h.st.work.set(w.ID, entry[store.Work]{seq: e.seq, v: w})
		inFlight[w.Kind]++
		out = append(out, w)
		h.st.dirty = true
	}
	if len(out) == 0 && heldBack {
		return nil, errSaturated()
	}
	return out, nil
}

func (t tx) CompleteWork(ctx context.Context, w store.Work) error {
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
	if !live || lease.Fence != w.Fence || cur.v.Fence != w.Fence {
		return conflict("fence is not the current lease")
	}
	h.st.work.del(w.ID)
	h.st.leases.del(cur.v.Resource)
	return nil
}

func (t tx) AcquireLease(ctx context.Context, r store.LeaseRequest) (store.Lease, error) {
	h, err := t.write(ctx)
	if err != nil {
		return store.Lease{}, err
	}
	defer h.mu.Unlock()
	if err := firstErr(h.scoped(r.Resource), h.checkLeaseTerms(r.Owner, r.Until)); err != nil {
		return store.Lease{}, err
	}
	if _, live := h.liveLease(r.Resource); live {
		return store.Lease{}, conflict("resource is leased")
	}
	return h.grant(r.Resource, r.Owner, r.Until), nil
}

func (t tx) RenewLease(ctx context.Context, l store.Lease) (store.Lease, error) {
	h, err := t.write(ctx)
	if err != nil {
		return store.Lease{}, err
	}
	defer h.mu.Unlock()
	if err := firstErr(h.scoped(l.Resource), h.checkLeaseTerms(l.Fence.Owner, l.Until)); err != nil {
		return store.Lease{}, err
	}
	cur, live := h.liveLease(l.Resource)
	if !live || cur.Fence != l.Fence {
		return store.Lease{}, conflict("fence is not the current lease")
	}
	cur.Until = l.Until
	h.st.leases.set(l.Resource, entry[store.Lease]{v: cur})
	return cur, nil
}

func (t tx) ReleaseLease(ctx context.Context, l store.Lease) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := h.scoped(l.Resource); err != nil {
		return err
	}
	cur, ok := h.st.leases.get(l.Resource)
	if !ok || cur.v.Fence != l.Fence {
		return conflict("fence is not the current lease")
	}
	h.st.leases.del(l.Resource)
	return nil
}
