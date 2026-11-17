package model

import "testing"

func TestReceiptValidate(t *testing.T) {
	valid := func() Receipt {
		return Receipt{Command: Command{Call: Call{Correlation: "corr-1"}, Operation: "op-1"}, Message: msgA, Kind: ReceiptRead}
	}
	requireOK(t, valid().Validate())
	delivered := valid()
	delivered.Kind = ReceiptDelivered
	requireOK(t, delivered.Validate())
	for _, tc := range []struct {
		name, field string
		mut         func(*Receipt)
	}{
		{"no kind", "kind", func(r *Receipt) { r.Kind = "" }},
		{"unknown kind", "kind", func(r *Receipt) { r.Kind = "seen" }},
		{"uppercase kind", "kind", func(r *Receipt) { r.Kind = "Read" }},
		{"no message", "message.conversation.inbox.tenant", func(r *Receipt) { r.Message = MessageRef{} }},
		{"expected revision", "expected_revision", func(r *Receipt) { r.ExpectedRevision = "r1" }},
		{"no operation", "operation", func(r *Receipt) { r.Operation = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := valid()
			tc.mut(&r)
			requireError(t, r.Validate(), Validation, tc.field)
		})
	}
}
