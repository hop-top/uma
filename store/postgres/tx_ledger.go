package postgres

import (
	"bytes"
	"context"
	"math"
	"slices"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres/internal/db"
)

func (t txn) ClaimOperation(ctx context.Context, rec store.OperationRecord) (store.OperationRecord, bool, error) {
	h := t.h
	var out store.OperationRecord
	var created bool
	err := h.call(ctx, func() error {
		k := rec.Key
		if err := firstErr(
			h.scoped(rec.Operation.Ref),
			h.checkOperationKey(k),
			newRevision(rec.Operation.Revision, ""),
		); err != nil {
			return err
		}
		if rec.FingerprintVersion == 0 || len(rec.Fingerprint) == 0 {
			return invalid("fingerprint: version and value required")
		}
		c := rec.Clone()
		body, err := encodeOperation(c)
		if err != nil {
			// A held key or reference answers before the record's encoding.
			held, ok, herr := h.heldClaim(ctx, rec)
			switch {
			case herr != nil:
				return herr
			case ok:
				out = held
				return nil
			}
			return err
		}
		at := h.writeTime()
		var expires *time.Time
		if !rec.ExpiresAt.IsZero() {
			e := rec.ExpiresAt
			expires = &e
		}
		scope := c.Scope()
		// The claim is inserted before the key is read: a read of the key or
		// ID index would leave a predicate lock on the page new claims share,
		// and concurrent claims would fail one another on it. Only a claim
		// that finds its key or reference held reads it.
		n, err := h.q.InsertOperation(ctx, db.InsertOperationParams{
			TenantID: h.tenantID, ID: string(rec.Operation.Ref.ID),
			Caller: string(k.Caller), Kind: k.Kind, TargetKind: string(k.Target.Kind),
			TargetInbox: optID(k.Target.Inbox), TargetID: string(k.Target.ID), OperationKey: string(k.ID),
			FingerprintVersion: int64(rec.FingerprintVersion), Fingerprint: rec.Fingerprint,
			State: string(rec.Operation.State), ExpiresAt: expires, Body: body, Revision: string(rec.Operation.Revision),
			WrittenAt: at, InboxID: optID(scope.Inbox), ConversationID: optID(scope.Conversation),
		})
		if err != nil {
			return h.db(ctx, err)
		}
		if n == 1 {
			out, created = c, true
			out.Operation.UpdatedAt = at
			return nil
		}
		held, ok, err := h.heldClaim(ctx, rec)
		switch {
		case err != nil:
			return err
		case !ok:
			// The insert met a held key or reference neither read finds.
			return conflict("operation key or reference already claimed")
		}
		out = held
		return nil
	})
	return out, created, err
}

// heldClaim answers a claim whose key or reference is held: the stored
// claim of its key when the fingerprint matches, model.Conflict when it
// does not or when the reference is claimed under another key. ok is false
// when neither is held.
func (h *handle) heldClaim(ctx context.Context, rec store.OperationRecord) (store.OperationRecord, bool, error) {
	row, err := h.operationByKey(ctx, rec.Key)
	switch {
	case err == nil:
		cur, err := decodeOperation(row.Body, row.WrittenAt)
		if err != nil {
			return store.OperationRecord{}, false, err
		}
		if cur.FingerprintVersion != rec.FingerprintVersion || !bytes.Equal(cur.Fingerprint, rec.Fingerprint) {
			return store.OperationRecord{}, false, conflict("operation ID reused with a different payload")
		}
		return cur, true, nil
	case !missing(err):
		return store.OperationRecord{}, false, h.db(ctx, err)
	}
	_, err = h.q.GetOperation(ctx, db.GetOperationParams{TenantID: h.tenantID, ID: string(rec.Operation.Ref.ID)})
	switch {
	case err == nil:
		return store.OperationRecord{}, false, conflict("operation reference already claimed under another key")
	case !missing(err):
		return store.OperationRecord{}, false, h.db(ctx, err)
	}
	return store.OperationRecord{}, false, nil
}

func (t txn) AppendAttemptEvidence(ctx context.Context, ev store.AttemptEvidence) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := firstErr(h.scoped(ev.Operation), ev.Attempt.Validate()); err != nil {
			return err
		}
		if _, err := h.q.GetOperation(ctx, db.GetOperationParams{TenantID: h.tenantID, ID: string(ev.Operation.ID)}); err != nil {
			return h.lookupErr(ctx, err)
		}
		c := ev.Clone()
		body, err := encode(&c)
		if err != nil {
			return err
		}
		return h.db(ctx, h.q.InsertAttempt(ctx, db.InsertAttemptParams{
			TenantID: h.tenantID, OperationID: string(ev.Operation.ID), AttemptID: string(ev.Attempt), Body: body,
			WrittenAt: h.now(),
		}))
	})
}

func (t txn) UpdateOperation(ctx context.Context, op model.Operation, expected model.Revision, fence store.Fence) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := firstErr(h.scoped(op.Ref), newRevision(op.Revision, expected)); err != nil {
			return err
		}
		row, err := h.q.GetOperation(ctx, db.GetOperationParams{TenantID: h.tenantID, ID: string(op.Ref.ID)})
		if err != nil {
			return h.lookupErr(ctx, err)
		}
		if model.Revision(row.Revision) != expected {
			return conflict("stale expected revision")
		}
		lease, live, err := h.liveLease(ctx, op.Ref.Ref())
		if err != nil {
			return err
		}
		switch {
		case fence.IsZero() && live:
			return conflict("operation is leased; fence required")
		case !fence.IsZero() && (!live || lease.Fence != fence):
			return conflict("fence is not the current lease")
		}
		rec, err := decode[store.OperationRecord](row.Body)
		if err != nil {
			return err
		}
		rec.Operation = op.Clone()
		body, err := encodeOperation(rec)
		if err != nil {
			return err
		}
		scope := rec.Scope()
		n, err := h.q.UpdateOperation(ctx, db.UpdateOperationParams{
			TenantID: h.tenantID, ID: string(op.Ref.ID), State: string(op.State), Body: body,
			Revision: string(op.Revision), Expected: string(expected), WrittenAt: h.writeTime(),
			InboxID: optID(scope.Inbox), ConversationID: optID(scope.Conversation),
		})
		if err != nil {
			return h.db(ctx, err)
		}
		if n != 1 {
			return conflict("stale expected revision")
		}
		return nil
	})
}

func (t txn) AppendEvents(ctx context.Context, events []model.Event) error {
	h := t.h
	return h.call(ctx, func() error {
		seen := make(map[model.ID]bool, len(events))
		bodies := make([][]byte, len(events))
		for i, e := range events {
			if err := firstErr(e.Validate(), h.sameTenant(e.Tenant)); err != nil {
				return err
			}
			if seen[e.ID] {
				return conflict("event ID already appended")
			}
			seen[e.ID] = true
			c := e.Clone()
			body, err := encode(&c)
			if err != nil {
				return err
			}
			bodies[i] = body
		}
		// Events go in at placeholders and get their positions when placed:
		// after commit, or before the unit reads the log (placement.go).
		// No lookup precedes the inserts: a read of the ID index would leave
		// a predicate lock on the page that new, time-ordered IDs share,
		// and concurrent appenders would fail one another on it.
		added := make([]unplaced, 0, len(events))
		for i, e := range events {
			ru, rn := instant(e.RecordedAt)
			placeholder, err := h.q.InsertEvent(ctx, db.InsertEventParams{
				TenantID: h.tenantID, ID: string(e.ID), Kind: e.Kind, InboxID: optID(e.Inbox), Body: bodies[i],
				WrittenAt: h.now(), ConversationID: optID(store.EventConversation(e)),
				RecordedUnix: ru, RecordedNanos: rn,
			})
			switch {
			case missing(err):
				// The log already holds the ID: the call appends nothing.
				return h.dropUnplaced(ctx, added)
			case err != nil:
				return h.db(ctx, err)
			}
			added = append(added, unplaced{placeholder: placeholder, recordedAt: e.RecordedAt})
		}
		h.unplaced = append(h.unplaced, added...)
		if h.placing {
			// The unit placed its earlier events and holds the placement
			// lock: these follow them at once, adjacent.
			return h.placeOwn(ctx)
		}
		return nil
	})
}

// dropUnplaced removes the call's events inserted before a duplicate, so
// the unit stays usable having appended nothing, and reports the conflict.
func (h *handle) dropUnplaced(ctx context.Context, added []unplaced) error {
	if len(added) > 0 {
		ph := make([]int64, len(added))
		for i, a := range added {
			ph[i] = a.placeholder
		}
		if err := h.db(ctx, h.q.DeleteUnplacedEvents(ctx, db.DeleteUnplacedEventsParams{TenantID: h.tenantID, Placeholders: ph})); err != nil {
			return err
		}
	}
	return conflict("event ID already appended")
}

func (t txn) AdvanceCheckpoint(ctx context.Context, a store.CheckpointAdvance) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := firstErr(h.scoped(a.Subscription), required("revision", string(a.Revision))); err != nil {
			return err
		}
		row, err := h.q.GetSubscription(ctx, db.GetSubscriptionParams{TenantID: h.tenantID, ID: string(a.Subscription.ID)})
		if err != nil {
			return h.lookupErr(ctx, err)
		}
		rec, err := decode[store.SubscriptionRecord](row.Body)
		if err != nil {
			return err
		}
		lease, live, err := h.liveLease(ctx, a.Subscription.Ref())
		if err != nil {
			return err
		}
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
		if err := h.placeOwn(ctx); err != nil {
			return err
		}
		head, err := h.q.EventHead(ctx, h.tenantID)
		if err != nil {
			return h.db(ctx, err)
		}
		if next < prior || next > head {
			return invalid("next: must lie between the prior checkpoint and the end of the log")
		}
		if a.Revision == rec.Subscription.Revision {
			return invalid("revision: must differ from the current revision")
		}
		expected := rec.Subscription.Revision
		rec.Subscription.Checkpoint, rec.Subscription.Revision, rec.PendingBatch = a.Next, a.Revision, nil
		body, err := encode(&rec)
		if err != nil {
			return err
		}
		n, err := h.q.UpdateSubscription(ctx, db.UpdateSubscriptionParams{
			TenantID: h.tenantID, ID: string(a.Subscription.ID), Consumer: rec.Consumer, Body: body,
			Revision: string(a.Revision), Expected: string(expected),
		})
		if err != nil {
			return h.db(ctx, err)
		}
		if n != 1 {
			return conflict("stale subscription revision")
		}
		return nil
	})
}

// refArgs splits a generic resource reference into its key columns.
func refArgs(r model.Ref) (kind string, inbox *string, id string) {
	return string(r.Kind), optID(r.Inbox), string(r.ID)
}

// generation converts a stored generation to a fence generation.
func generation(g int64) uint64 {
	if g < 0 {
		return 0
	}
	return uint64(g)
}

// storedGeneration converts a fence generation to its column value; a value
// no lease can hold maps to -1, which matches no row.
func storedGeneration(g uint64) int64 {
	if g > math.MaxInt64 {
		return -1
	}
	return int64(g)
}

// liveLease returns the resource's lease when it is held and has not expired
// by the store clock.
func (h *handle) liveLease(ctx context.Context, res model.Ref) (store.Lease, bool, error) {
	kind, inbox, id := refArgs(res)
	row, err := h.q.GetLease(ctx, db.GetLeaseParams{TenantID: h.tenantID, ResourceKind: kind, ResourceInbox: inbox, ResourceID: id})
	switch {
	case missing(err):
		return store.Lease{}, false, nil
	case err != nil:
		return store.Lease{}, false, h.db(ctx, err)
	case row.Owner == nil || row.Until == nil || !h.now().Before(*row.Until):
		return store.Lease{}, false, nil
	}
	return store.Lease{
		Resource: res,
		Fence:    store.Fence{Owner: *row.Owner, Generation: generation(row.Generation)},
		Until:    *row.Until,
	}, true, nil
}

// claimLease leases res to owner under its next generation unless it holds
// a lease unexpired at now, in one statement. ok is false when it does.
func (h *handle) claimLease(ctx context.Context, res model.Ref, owner string, until, now time.Time) (store.Lease, bool, error) {
	kind, inbox, id := refArgs(res)
	g, err := h.q.ClaimLease(ctx, db.ClaimLeaseParams{
		TenantID: h.tenantID, ResourceKind: kind, ResourceInbox: inbox, ResourceID: id, Owner: owner, Until: until, Now: now,
	})
	switch {
	case missing(err):
		return store.Lease{}, false, nil
	case err != nil:
		return store.Lease{}, false, h.db(ctx, err)
	}
	return store.Lease{Resource: res, Fence: store.Fence{Owner: owner, Generation: generation(g)}, Until: until}, true, nil
}

// lockKinds takes the in-flight lock of each kind, in sorted order so
// concurrent claims of overlapping kinds cannot deadlock.
func (h *handle) lockKinds(ctx context.Context, kinds []string) error {
	sorted := slices.Clone(kinds)
	slices.Sort(sorted)
	for _, k := range slices.Compact(sorted) {
		if err := h.db(ctx, h.q.LockWorkKind(ctx, db.LockWorkKindParams{TenantID: h.tenantID, Kind: k})); err != nil {
			return err
		}
	}
	return nil
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

func (t txn) EnqueueWork(ctx context.Context, w store.Work) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := firstErr(w.ID.Validate(), required("kind", w.Kind), h.scoped(w.Resource)); err != nil {
			return err
		}
		if !w.Fence.IsZero() || !w.Until.IsZero() || w.Claims != 0 {
			return invalid("work: fence, until and claims are set by claims")
		}
		kind, inbox, id := refArgs(w.Resource)
		var notBefore *time.Time
		if !w.NotBefore.IsZero() {
			nb := w.NotBefore
			notBefore = &nb
		}
		// No lookup precedes the insert: a read of the ID index would leave
		// a predicate lock on the page that new, time-ordered IDs share, and
		// concurrent enqueuers would fail one another on it.
		n, err := h.q.InsertWork(ctx, db.InsertWorkParams{
			TenantID: h.tenantID, ID: string(w.ID), Kind: w.Kind, ResourceKind: kind, ResourceInbox: inbox, ResourceID: id,
			NotBefore: notBefore,
		})
		switch {
		case err != nil:
			return h.db(ctx, err)
		case n == 0:
			return conflict("work ID already enqueued")
		}
		return nil
	})
}

// ClaimWork leases pending work in enqueue order. Items whose resource is
// leased, including by an earlier item of the same claim, deferred past the
// store clock by NotBefore, or being claimed by another unit in flight, are
// skipped. Exclusivity holds at any isolation, so a claim unit runs at READ
// COMMITTED: the lease is taken only from an expired holder, judged on the
// lease row's latest committed version under its row lock, and an item
// whose resource another unit leased meanwhile is skipped. With a cap, a
// per-tenant, per-kind advisory lock held to commit serializes counting
// and claiming, so the count every claim sees is current.
func (t txn) ClaimWork(ctx context.Context, c store.WorkClaim) ([]store.Work, error) {
	h := t.h
	var out []store.Work
	err := h.claimCall(ctx, func() error {
		if len(c.Kinds) == 0 {
			return invalid("kinds: required")
		}
		limit, err := pageLimit(c.Limit)
		if err != nil {
			return invalid("limit: must be positive")
		}
		if err := h.checkLeaseTerms(c.Owner, c.Until); err != nil {
			return err
		}
		if c.MaxInFlight < 0 {
			return invalid("max_in_flight: must not be negative")
		}
		now := h.now()
		inFlight := map[string]int{}
		if c.MaxInFlight > 0 {
			if err := h.lockKinds(ctx, c.Kinds); err != nil {
				return err
			}
			if inFlight, err = h.claimedWork(ctx, c.Kinds, now); err != nil {
				return err
			}
		}
		heldBack := false
		granted := map[model.Ref]bool{}
		var after int64
		for len(out) < int(limit) {
			want := limit - int32(len(out)) // #nosec G115 -- len(out) < limit <= maxPage
			rows, err := h.q.ListPendingWork(ctx, db.ListPendingWorkParams{
				TenantID: h.tenantID, Kinds: c.Kinds, After: after, Now: now, Lim: want,
			})
			if err != nil {
				return h.db(ctx, err)
			}
			for _, row := range rows {
				after = row.Seq
				res := model.Ref{Tenant: h.tenant, Kind: model.ResourceKind(row.ResourceKind), ID: model.ID(row.ResourceID)}
				if row.ResourceInbox != nil {
					res.Inbox = model.ID(*row.ResourceInbox)
				}
				if granted[res] {
					continue
				}
				if c.MaxInFlight > 0 && inFlight[row.Kind] >= c.MaxInFlight {
					heldBack = true
					continue
				}
				l, ok, err := h.claimLease(ctx, res, c.Owner, c.Until, now)
				if err != nil {
					return err
				}
				if !ok {
					// Leased by a unit that committed after this query's
					// snapshot.
					continue
				}
				granted[res] = true
				if err := h.db(ctx, h.q.SetWorkFence(ctx, db.SetWorkFenceParams{
					TenantID: h.tenantID, ID: row.ID, FenceOwner: l.Fence.Owner,
					FenceGeneration: storedGeneration(l.Fence.Generation), Until: l.Until,
				})); err != nil {
					return err
				}
				inFlight[row.Kind]++
				w := store.Work{
					ID: model.ID(row.ID), Kind: row.Kind, Resource: res, Fence: l.Fence, Until: l.Until,
					Claims: int(row.Claims) + 1,
				}
				if row.NotBefore != nil {
					w.NotBefore = *row.NotBefore
				}
				out = append(out, w)
			}
			if len(rows) < int(want) {
				break
			}
		}
		if len(out) == 0 && heldBack {
			return errSaturated()
		}
		return nil
	})
	return out, err
}

func (t txn) CompleteWork(ctx context.Context, w store.Work) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := w.ID.Validate(); err != nil {
			return err
		}
		row, err := h.q.GetWork(ctx, db.GetWorkParams{TenantID: h.tenantID, ID: string(w.ID)})
		if err != nil {
			return h.lookupErr(ctx, err)
		}
		res := model.Ref{Tenant: h.tenant, Kind: model.ResourceKind(row.ResourceKind), ID: model.ID(row.ResourceID)}
		if row.ResourceInbox != nil {
			res.Inbox = model.ID(*row.ResourceInbox)
		}
		var stored store.Fence
		if row.FenceOwner != nil && row.FenceGeneration != nil {
			stored = store.Fence{Owner: *row.FenceOwner, Generation: generation(*row.FenceGeneration)}
		}
		lease, live, err := h.liveLease(ctx, res)
		if err != nil {
			return err
		}
		if !live || lease.Fence != w.Fence || stored != w.Fence {
			return conflict("fence is not the current lease")
		}
		if err := h.db(ctx, h.q.DeleteWork(ctx, db.DeleteWorkParams{TenantID: h.tenantID, ID: string(w.ID)})); err != nil {
			return err
		}
		kind, inbox, id := refArgs(res)
		_, err = h.q.ReleaseLease(ctx, db.ReleaseLeaseParams{
			TenantID: h.tenantID, ResourceKind: kind, ResourceInbox: inbox, ResourceID: id,
			Owner: w.Fence.Owner, Generation: storedGeneration(w.Fence.Generation),
		})
		return h.db(ctx, err)
	})
}

func (t txn) AcquireLease(ctx context.Context, r store.LeaseRequest) (store.Lease, error) {
	h := t.h
	var out store.Lease
	err := h.call(ctx, func() error {
		if err := firstErr(h.scoped(r.Resource), h.checkLeaseTerms(r.Owner, r.Until)); err != nil {
			return err
		}
		// One statement, no prior lookup: a read of uma_leases would leave a
		// predicate lock on the index page that holds the key, and on a small
		// table every key shares that page, so takes of different
		// resources would abort each other as read/write conflicts.
		l, ok, err := h.claimLease(ctx, r.Resource, r.Owner, r.Until, h.now())
		switch {
		case err != nil:
			return err
		case !ok:
			return conflict("resource is leased")
		}
		out = l
		return nil
	})
	return out, err
}

func (t txn) RenewLease(ctx context.Context, l store.Lease) (store.Lease, error) {
	h := t.h
	var out store.Lease
	err := h.call(ctx, func() error {
		if err := firstErr(h.scoped(l.Resource), h.checkLeaseTerms(l.Fence.Owner, l.Until)); err != nil {
			return err
		}
		cur, live, err := h.liveLease(ctx, l.Resource)
		if err != nil {
			return err
		}
		if !live || cur.Fence != l.Fence {
			return conflict("fence is not the current lease")
		}
		kind, inbox, id := refArgs(l.Resource)
		if _, err := h.q.RenewLease(ctx, db.RenewLeaseParams{
			TenantID: h.tenantID, ResourceKind: kind, ResourceInbox: inbox, ResourceID: id,
			Owner: l.Fence.Owner, Generation: storedGeneration(l.Fence.Generation), Until: l.Until,
		}); err != nil {
			return h.db(ctx, err)
		}
		cur.Until = l.Until
		out = cur
		return nil
	})
	return out, err
}

func (t txn) ReleaseLease(ctx context.Context, l store.Lease) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := h.scoped(l.Resource); err != nil {
			return err
		}
		kind, inbox, id := refArgs(l.Resource)
		n, err := h.q.ReleaseLease(ctx, db.ReleaseLeaseParams{
			TenantID: h.tenantID, ResourceKind: kind, ResourceInbox: inbox, ResourceID: id,
			Owner: l.Fence.Owner, Generation: storedGeneration(l.Fence.Generation),
		})
		if err != nil {
			return h.db(ctx, err)
		}
		if n != 1 {
			return conflict("fence is not the current lease")
		}
		return nil
	})
}
