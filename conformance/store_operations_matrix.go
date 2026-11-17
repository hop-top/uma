package conformance

import (
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// operationsRead is Reader.Operations in the filter and cursor matrices.
var operationsRead = matrixRead{name: "Operations", filtered: true,
	honours: []string{"inboxes", "conversation", "statuses", "since", "before"},
	read: func(w *matrixWorld, r store.Reader, f model.Filter, p model.PageRequest) (readView, error) {
		pg, err := r.Operations(w.ctx, listReq(f, p))
		return viewOf(pg, err, func(v store.OperationRecord) model.ID { return v.Operation.Ref.ID })
	},
	add: func(t *testing.T, e *env, w *matrixWorld) {
		e.tick(t)
		e.putOperation(t, opSpec{target: w.cv1.Ref()})
	}}

// seedOperations stores the world's operations, one second apart:
//
//	oA  reply in cv1 (inbox I1), delivered
//	oB  reply in cv2 (inbox I1), failed
//	oC  reply in cv3 (inbox I2), delivered
//	oD  contact write (no inbox), committed
//	oE  inbox configuration of I2 (no conversation), committed
func (w *matrixWorld) seedOperations(t *testing.T, e *env, in2 model.Filter) {
	t.Helper()
	put := func(s opSpec) store.OperationRecord {
		e.tick(t)
		return e.putOperation(t, s)
	}
	oA := put(opSpec{target: w.cv1.Ref(), state: model.Delivered})
	oB := put(opSpec{target: w.cv2.Ref(), state: model.Failed})
	oC := put(opSpec{target: w.cv3.Ref(), state: model.Delivered})
	oD := put(opSpec{kind: "contact.write", target: model.ContactRef{Tenant: tenantA, ID: e.id(t)}.Ref(), state: model.Committed})
	oE := put(opSpec{kind: "inbox.configure", target: w.i2.Ref(), state: model.Committed})
	// oE was written now, oD a second before, oB three before: the window
	// from oD on is inclusive and the one before oB leaves oB out.
	since, before := e.fx.Now().Add(-time.Second), e.fx.Now().Add(-3*time.Second)
	w.row("Operations", "inboxes", in2, oC.Operation.Ref.ID, oE.Operation.Ref.ID)
	w.row("Operations", "conversation", model.Filter{Conversation: &w.cv2}, oB.Operation.Ref.ID)
	w.row("Operations", "statuses", model.Filter{Statuses: []string{string(model.Failed)}}, oB.Operation.Ref.ID)
	w.row("Operations", "since", model.Filter{Since: &since}, oD.Operation.Ref.ID, oE.Operation.Ref.ID)
	w.row("Operations", "before", model.Filter{Before: &before}, oA.Operation.Ref.ID)
}
