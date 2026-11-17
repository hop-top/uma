// Package retention runs host-invoked retention purges: each record family
// of one tenant is deleted past its configured horizon, in bounded units of
// work, the event log records where its retained history begins, and
// staged blobs no upload bound within the staging horizon are deleted.
package retention

import (
	"context"
	"errors"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// Report counts the records one Purge deleted per family, and the earliest
// event cursor the log still accepts. Its fields mirror the public
// PurgeReport one for one; the public API converts.
type Report struct {
	Events     int
	Operations int
	Ingress    int
	Attempts   int
	Blobs      int
	Exports    int
	Contacts   int
	EventBase  model.Cursor
}

// batch bounds the records one unit of work deletes.
const batch = 500

// Purge deletes, for the principal's tenant, every record older than its
// family's horizon in the engine's retention policy, judged against the
// engine clock. Every horizon is required; there are no defaults. Each
// family is purged in units of at most batch records until none is left,
// attempt evidence before operations so an operation's evidence never
// outlives it, then contacts. Records still needed are kept whatever their
// age: an operation or its evidence while work refers to it or it is
// leased, any event after a newer one, since the log is purged as a
// prefix, and any contact an identity points at or no link record ever
// left (store.FamilyContacts). Link history is never purged.
//
// A cursor or checkpoint behind the recorded event base fails later reads
// with model.CursorExpired; a purged operation key or ingress deduplication
// key is free again, so a retry past the horizon is a new command. Purge
// stops at the first failure, and keeps what earlier units deleted; a
// later call continues.
//
// Purged exports' bytes are deleted from the blob store after their unit,
// and their staging intents cleared; a failed delete leaves the intent the
// purge recorded for the sweep.
//
// The staging sweep then deletes, from the blob store, the bytes of every
// staging intent recorded before the Staging horizon: an upload binds its
// intent in the unit that commits the attachment, so an intent still
// recorded past the horizon belongs to an upload that crashed or failed
// before cleanup. See sweep.
func Purge(ctx context.Context, e *engine.Engine, p model.Principal) (Report, error) {
	if e.IsClosed() {
		return Report{}, engine.ErrClosed()
	}
	if err := p.Validate(); err != nil {
		return Report{}, err
	}
	policy := e.Retention()
	if err := policy.Required(); err != nil {
		return Report{}, err
	}
	done, err := e.Begin()
	if err != nil {
		return Report{}, err
	}
	defer done()
	now := e.Clock().Now()
	var r Report
	families := []struct {
		name    string
		horizon time.Duration
		count   *int
	}{
		{store.FamilyAttempts, policy.Attempts, &r.Attempts},
		{store.FamilyOperations, policy.Operations, &r.Operations},
		{store.FamilyContacts, policy.Contacts, &r.Contacts},
		{store.FamilyIngress, policy.Ingress, &r.Ingress},
		{store.FamilyExports, policy.Exports, &r.Exports},
		{store.FamilyEvents, policy.Events, &r.Events},
	}
	for _, f := range families {
		req := store.PurgeRequest{Family: f.name, Before: now.Add(-f.horizon), Limit: batch}
		for {
			var res store.PurgeResult
			err := operations.Transact(ctx, e, p.Tenant, func(tx store.Tx) error {
				var err error
				res, err = tx.Purge(ctx, req)
				return err
			})
			if err != nil {
				return r, err
			}
			*f.count += res.Purged
			if f.name == store.FamilyEvents {
				r.EventBase = res.EventBase
			}
			if blobs := e.Blobs(); blobs != nil && len(res.Blobs) > 0 {
				release(ctx, e, p.Tenant, blobs, res.Blobs)
			}
			if res.Complete || res.Purged == 0 {
				break
			}
		}
	}
	if blobs := e.Blobs(); blobs != nil {
		n, err := sweep(ctx, e, p.Tenant, blobs, now.Add(-policy.Staging))
		r.Blobs = n
		if err != nil {
			return r, err
		}
	}
	return r, nil
}

// sweep deletes the staged blobs of intents recorded before before, in
// batches: one unit abandons a batch, so a late commit can no longer bind
// any of its keys; the bytes are deleted outside any unit; a second unit
// clears the deleted keys' intents. A failed delete stops the sweep with
// its intent still abandoned, and the next sweep deletes it again.
func sweep(ctx context.Context, e *engine.Engine, tenant model.TenantID, blobs ports.BlobStore, before time.Time) (int, error) {
	n := 0
	for {
		var b store.StagingBatch
		err := operations.Transact(ctx, e, tenant, func(tx store.Tx) error {
			var err error
			b, err = tx.AbandonStaging(ctx, store.StagingSweep{Before: before, Limit: batch})
			return err
		})
		if err != nil {
			return n, err
		}
		deleted := make([]ports.BlobKey, 0, len(b.Intents))
		var derr error
		for _, in := range b.Intents {
			if derr = blobs.Delete(ctx, in.Key); derr != nil {
				break
			}
			deleted = append(deleted, in.Key)
		}
		if len(deleted) > 0 {
			if err := operations.Transact(ctx, e, tenant, func(tx store.Tx) error {
				for _, k := range deleted {
					if err := tx.ClearStaging(ctx, k); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return n, err
			}
			n += len(deleted)
		}
		if derr != nil {
			return n, blobError(ctx, derr)
		}
		if b.Complete || len(b.Intents) == 0 {
			return n, nil
		}
	}
}

// release deletes the bytes of purged exports and clears the staging
// intents the purge recorded for them. A failed delete leaves its intent
// for the sweep.
func release(ctx context.Context, e *engine.Engine, tenant model.TenantID, blobs ports.BlobStore, keys []ports.BlobKey) {
	var deleted []ports.BlobKey
	for _, k := range keys {
		if blobs.Delete(ctx, k) == nil {
			deleted = append(deleted, k)
		}
	}
	if len(deleted) == 0 {
		return
	}
	_ = operations.Transact(ctx, e, tenant, func(tx store.Tx) error {
		for _, k := range deleted {
			if err := tx.ClearStaging(ctx, k); err != nil {
				return err
			}
		}
		return nil
	})
}

// blobError keeps context errors standard and replaces any other blob
// store failure with fixed text.
func blobError(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return cerr
	}
	return model.NewError(model.Unavailable, "attachment storage unavailable")
}
