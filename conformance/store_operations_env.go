package conformance

import (
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// opSpec describes one ledger entry for the operation list cases: the key's
// kind and target and the typed results that give it its scope.
type opSpec struct {
	kind   string
	target model.Ref
	state  model.DeliveryState
	msg    *model.MessageRef
	stream *model.StreamRef
	task   *model.TaskRef
	result *model.Ref
}

// putOperation claims an operation of the spec's shape in tenantA, in its
// own unit, and returns the record as stored.
func (e *env) putOperation(t *testing.T, s opSpec) store.OperationRecord {
	t.Helper()
	rec := e.operation(t, model.OperationID("list-"+string(e.id(t))), []byte{1})
	rec.Key.Kind, rec.Key.Target = s.kind, s.target
	if rec.Key.Kind == "" {
		rec.Key.Kind = "messages.reply"
	}
	if s.state != "" {
		rec.Operation.State = s.state
	}
	rec.Operation.ResultMessage, rec.Operation.ResultStream = s.msg, s.stream
	rec.Operation.ResultTask, rec.Operation.Result = s.task, s.result
	e.mustTx(t, func(tx store.Tx) error {
		_, _, err := tx.ClaimOperation(e.ctx, rec)
		return err
	})
	return rec
}

// moveOperation writes the operation again in state, so its write time and
// state both change.
func (e *env) moveOperation(t *testing.T, rec *store.OperationRecord, state model.DeliveryState) {
	t.Helper()
	next := rec.Operation
	next.State, next.Revision = state, model.Revision("r-"+string(e.id(t)))
	e.mustTx(t, func(tx store.Tx) error {
		return tx.UpdateOperation(e.ctx, next, rec.Operation.Revision, store.Fence{})
	})
	rec.Operation = next
}

// tick moves the store clock forward one second, so the next write is
// strictly later than the last.
func (e *env) tick(t *testing.T) {
	t.Helper()
	if e.fx.Advance == nil {
		t.Skip("fixture cannot advance the store clock")
	}
	e.fx.Advance(time.Second)
}

// listOperations pages Reader.Operations under f to its end, one unit per
// page, and returns the operation IDs in the order returned.
func (e *env) listOperations(t *testing.T, f model.Filter, limit int) []model.ID {
	t.Helper()
	var ids []model.ID
	var after model.Cursor
	for range 200 {
		var p model.Page[store.OperationRecord]
		e.mustView(t, func(r store.Reader) error {
			var err error
			p, err = r.Operations(e.ctx, model.ListRequest{Filter: f, Page: model.PageRequest{Limit: limit, After: after}})
			return err
		})
		for _, it := range p.Items {
			ids = append(ids, it.Operation.Ref.ID)
		}
		if p.Complete {
			return ids
		}
		if p.Next == "" || p.Next == after {
			t.Fatalf("incomplete page after %q returned next %q, which does not advance", after, p.Next)
		}
		after = p.Next
	}
	t.Fatal("paging did not complete")
	return nil
}

func opIDs(recs ...store.OperationRecord) []model.ID {
	out := make([]model.ID, len(recs))
	for i, r := range recs {
		out[i] = r.Operation.Ref.ID
	}
	return out
}

// reversed returns ids newest-written first when they are given oldest first.
func reversed(ids []model.ID) []model.ID {
	out := slices.Clone(ids)
	slices.Reverse(out)
	return out
}

func (e *env) conversationRef(t *testing.T, in model.InboxRef) model.ConversationRef {
	return model.ConversationRef{Inbox: in, ID: e.id(t)}
}
