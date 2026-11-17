package store_test

import (
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

func TestOperationRecordScope(t *testing.T) {
	const tenant model.TenantID = "t"
	in := model.InboxRef{Tenant: tenant, ID: "00000000-0000-7000-8000-0000000000b1"}
	other := model.InboxRef{Tenant: tenant, ID: "00000000-0000-7000-8000-0000000000b2"}
	conv := model.ConversationRef{Inbox: in, ID: "00000000-0000-7000-8000-0000000000a1"}
	foreign := model.ConversationRef{Inbox: other, ID: "00000000-0000-7000-8000-0000000000a2"}
	msg := func(c model.ConversationRef) *model.MessageRef {
		return &model.MessageRef{Conversation: c, ID: "00000000-0000-7000-8000-0000000000e1"}
	}
	rec := func(target model.Ref, set func(*model.Operation)) store.OperationRecord {
		r := store.OperationRecord{Key: store.OperationKey{Target: target}}
		if set != nil {
			set(&r.Operation)
		}
		return r
	}
	for _, c := range []struct {
		name string
		rec  store.OperationRecord
		want store.OperationScope
	}{
		{"conversation target", rec(conv.Ref(), nil), store.OperationScope{Inbox: in.ID, Conversation: conv.ID}},
		{"inbox target", rec(in.Ref(), nil), store.OperationScope{Inbox: in.ID}},
		{"inbox target with a message result", rec(in.Ref(), func(o *model.Operation) { o.ResultMessage = msg(conv) }), store.OperationScope{Inbox: in.ID, Conversation: conv.ID}},
		{"result in another inbox", rec(in.Ref(), func(o *model.Operation) { o.ResultMessage = msg(foreign) }), store.OperationScope{Inbox: in.ID}},
		{"first usable result wins", rec(in.Ref(), func(o *model.Operation) {
			o.ResultMessage = msg(foreign)
			o.ResultStream = &model.StreamRef{Conversation: conv, ID: "00000000-0000-7000-8000-0000000000f1"}
		}), store.OperationScope{Inbox: in.ID, Conversation: conv.ID}},
		{"generic conversation result", rec(model.WorkItemRef{Conversation: conv, ID: "00000000-0000-7000-8000-0000000000f2"}.Ref(), func(o *model.Operation) {
			r := conv.Ref()
			o.Result = &r
		}), store.OperationScope{Inbox: in.ID, Conversation: conv.ID}},
		{"tenant-scoped target", rec(model.ContactRef{Tenant: tenant, ID: "00000000-0000-7000-8000-0000000000c1"}.Ref(), func(o *model.Operation) { o.ResultMessage = msg(conv) }), store.OperationScope{}},
	} {
		if got := c.rec.Scope(); got != c.want {
			t.Errorf("%s: Scope = %+v, want %+v", c.name, got, c.want)
		}
	}
}
