package operations

import (
	"context"
	"maps"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// block is the held block of a new proposal by proposer, whose command
// fingerprint is fp.
func (h Hold) block(proposer model.ActorID, fp []byte) *model.HeldBlock {
	b := &model.HeldBlock{
		Proposer: proposer, OnBehalfOf: h.OnBehalfOf, Reason: h.Reason, TrustKey: h.TrustKey,
		TrustLabels: maps.Clone(h.TrustLabels), ProposedFingerprint: append([]byte(nil), fp...),
	}
	if h.Preview != nil {
		p := h.Preview.Clone()
		b.Preview = &p
	}
	if h.ExpiresAt != nil {
		at := h.ExpiresAt.UTC()
		b.ExpiresAt = &at
	}
	return b
}

// heldPage bounds one page of the duplicate-guard scan.
const heldPage = 100

// heldDuplicate finds the held operation of the same kind, target and
// scope (see Submission.HoldScope) as key, claimed under another key, in the claiming unit. A key already
// claimed is never a duplicate of itself: the claim answers it as a replay.
// Both proposals of a concurrent pair read and write the ledger in one
// unit each, so the store serializes them and the loser sees the winner.
func heldDuplicate(ctx context.Context, tx store.Tx, key store.OperationKey, scope string) (store.OperationRecord, bool, error) {
	switch _, err := tx.OperationByKey(ctx, key); {
	case err == nil:
		return store.OperationRecord{}, false, nil
	case !model.IsCode(err, model.NotFound):
		return store.OperationRecord{}, false, err
	}
	req := model.ListRequest{Filter: model.Filter{Statuses: []string{string(model.Held)}}, Page: model.PageRequest{Limit: heldPage}}
	for {
		p, err := tx.Operations(ctx, req)
		if err != nil {
			return store.OperationRecord{}, false, err
		}
		for _, rec := range p.Items {
			if rec.Key.Kind == key.Kind && rec.Key.Target == key.Target && rec.Operation.Held != nil && rec.Operation.Held.Scope == scope {
				return rec, true, nil
			}
		}
		if p.Complete || p.Next == "" || p.Next == req.Page.After {
			return store.OperationRecord{}, false, nil
		}
		req.Page.After = p.Next
	}
}

// scrubbed returns the held block with the content of its draft cleared,
// as a message redaction clears a message: the preview, the stored
// intent, the edited arguments and each edit's before and after lose their
// data; names and versions stay, as do who decided what and when. It reports whether
// anything was cleared.
func scrubbed(h model.HeldBlock) (model.HeldBlock, bool) {
	h = h.Clone()
	changed := false
	clear := func(e *model.Extension) {
		if e != nil && len(e.Data) > 0 {
			e.Data, changed = nil, true
		}
	}
	clear(h.Preview)
	clear(h.Intent)
	clear(h.EditedArgs)
	for i := range h.Edits {
		clear(&h.Edits[i].Before)
		clear(&h.Edits[i].After)
	}
	return h, changed
}

// ScrubHeld clears the draft content of the held block of the operation
// that originated msg, in tx, under revision rev. It belongs in the unit of
// a message redaction, beside store.Tx.Redact, which clears the operation's
// intent but not its held block: the preview and the edits are copies of
// the draft the message came from. A message no operation originated, an
// operation never held or a block already scrubbed writes nothing.
func ScrubHeld(ctx context.Context, tx store.Tx, msg model.MessageRef, rev model.Revision) error {
	m, err := tx.GetMessage(ctx, msg)
	if err != nil {
		return err
	}
	if m.Origin == nil || m.Origin.Operation == (model.OperationRef{}) {
		return nil
	}
	rec, err := tx.Operation(ctx, m.Origin.Operation)
	switch {
	case model.IsCode(err, model.NotFound):
		return nil
	case err != nil:
		return err
	case rec.Operation.Held == nil:
		return nil
	}
	h, changed := scrubbed(*rec.Operation.Held)
	if !changed {
		return nil
	}
	next := rec.Operation.Clone()
	next.Held, next.Revision = &h, rev
	return tx.UpdateOperation(ctx, next, rec.Operation.Revision, store.Fence{})
}
