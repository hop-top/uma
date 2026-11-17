package conformance

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// operationListCases pin Reader.Operations beyond the shared filter and
// cursor matrices: how an operation's inbox and conversation are resolved,
// the write order, the window, tenants and bad requests.
var operationListCases = []storeCase{
	{"HeldOperationsListByStateAndKeepTheirBlock", func(t *testing.T, e *env) {
		i2 := e.putInbox(t)
		conv := e.conversationRef(t, e.inbox)
		conv2 := e.conversationRef(t, i2)
		queued := e.putOperation(t, opSpec{target: conv.Ref()})
		e.tick(t)
		at := time.Unix(1_700_000_000, 0).UTC()
		ext := func(n string) model.Extension {
			return model.Extension{Name: "conformance.held." + n, Version: 2, Data: []byte(`{"n":"` + n + `"}`)}
		}
		full := func(id model.OperationID, target model.ConversationRef, fp byte) store.OperationRecord {
			rec := e.operation(t, id, []byte{fp})
			rec.Key.Target = target.Ref()
			rec.Operation.State = model.Held
			prev, edited, intent := ext("preview"), ext("edited"), ext("intent")
			rec.Operation.Held = &model.HeldBlock{
				Proposer: "plugin:held", OnBehalfOf: "actor-1", Reason: "needs review", TrustKey: "k",
				TrustLabels: map[string]string{"a": "b", "c": "d"}, ExpiresAt: &at, ProposedFingerprint: []byte{fp, fp},
				Preview: &prev, Intent: &intent, EditedArgs: &edited,
				Edits: []model.HeldEdit{{By: "actor-2", At: at.Add(time.Minute), Before: ext("b0"), After: ext("a0")}},
			}
			return rec
		}
		claim := func(rec store.OperationRecord) {
			e.mustTx(t, func(tx store.Tx) error {
				_, _, err := tx.ClaimOperation(e.ctx, rec)
				return err
			})
			e.tick(t)
		}
		held1, held2 := full("held-op-1", conv, 9), full("held-op-2", conv2, 8)
		claim(held1)
		claim(held2)
		// A decided operation keeps its block, with the decision filled in.
		decided := full("held-op-3", conv, 7)
		decidedAt := at.Add(time.Hour)
		decided.Operation.State = model.Rejected
		decided.Operation.Held.Decision, decided.Operation.Held.DecidedBy, decided.Operation.Held.DecidedAt = model.HeldRejected, "actor-3", &decidedAt
		claim(decided)
		id := func(r store.OperationRecord) model.ID { return r.Operation.Ref.ID }
		heldFilter := model.Filter{Statuses: []string{string(model.Held)}}
		if got := e.listOperations(t, heldFilter, 10); !slices.Equal(got, []model.ID{id(held2), id(held1)}) {
			t.Fatalf("held list = %v, want both held operations newest first", got)
		}
		inbox := heldFilter
		inbox.Inboxes = []model.InboxRef{e.inbox}
		if got := e.listOperations(t, inbox, 10); !slices.Equal(got, []model.ID{id(held1)}) {
			t.Fatalf("held list of one inbox = %v, want %v", got, id(held1))
		}
		inbox.Inboxes = []model.InboxRef{i2}
		if got := e.listOperations(t, inbox, 1); !slices.Equal(got, []model.ID{id(held2)}) {
			t.Fatalf("held list of the other inbox = %v, want %v", got, id(held2))
		}
		if got := e.listOperations(t, model.Filter{Statuses: []string{string(model.Queued)}}, 10); !slices.Equal(got, []model.ID{queued.Operation.Ref.ID}) {
			t.Fatalf("queued list = %v", got)
		}
		e.mustView(t, func(r store.Reader) error {
			for _, want := range []store.OperationRecord{held1, held2, decided} {
				got, err := r.OperationByKey(e.ctx, want.Key)
				if err != nil {
					return err
				}
				if got.Operation.State != want.Operation.State || !reflect.DeepEqual(got.Operation.Held, want.Operation.Held) {
					t.Errorf("stored operation = %+v, want %+v", got.Operation, want.Operation)
				}
			}
			return nil
		})
	}},
	{"ScopeFollowsKeyAndResults", func(t *testing.T, e *env) {
		i2 := e.putInbox(t)
		c1, c2 := e.conversationRef(t, e.inbox), e.conversationRef(t, e.inbox)
		c3 := e.conversationRef(t, i2)
		msg := func(c model.ConversationRef) *model.MessageRef {
			return &model.MessageRef{Conversation: c, ID: e.id(t)}
		}
		type want struct {
			inbox model.InboxRef
			conv  *model.ConversationRef
		}
		cases := []struct {
			name string
			spec opSpec
			want want
		}{
			{"conversation target", opSpec{target: c1.Ref()}, want{e.inbox, &c1}},
			{"message result", opSpec{target: e.inbox.Ref(), msg: msg(c2)}, want{e.inbox, &c2}},
			{"message target and result", opSpec{kind: "message.edit", target: model.MessageRef{Conversation: c1, ID: e.id(t)}.Ref(), msg: msg(c1)}, want{e.inbox, &c1}},
			{"stream result", opSpec{kind: "stream.write", target: model.StreamRef{Conversation: c2, ID: e.id(t)}.Ref(), stream: &model.StreamRef{Conversation: c2, ID: e.id(t)}}, want{e.inbox, &c2}},
			{"task result", opSpec{kind: "task.act", target: model.TaskRef{Conversation: c3, ID: e.id(t)}.Ref(), task: &model.TaskRef{Conversation: c3, ID: e.id(t)}}, want{i2, &c3}},
			{"generic conversation result", opSpec{kind: "work_item.act", target: model.WorkItemRef{Conversation: c1, ID: e.id(t)}.Ref(), result: refPtr(c1.Ref())}, want{e.inbox, &c1}},
			{"inbox target alone", opSpec{kind: "inbox.configure", target: i2.Ref()}, want{i2, nil}},
			{"work item without a result", opSpec{kind: "work_item.act", target: model.WorkItemRef{Conversation: c2, ID: e.id(t)}.Ref()}, want{e.inbox, nil}},
			{"result in another inbox is not the scope", opSpec{target: model.WorkItemRef{Conversation: c1, ID: e.id(t)}.Ref(), msg: msg(c3)}, want{e.inbox, nil}},
			{"tenant-scoped target", opSpec{kind: "contact.write", target: model.ContactRef{Tenant: tenantA, ID: e.id(t)}.Ref()}, want{}},
		}
		recs := make([]store.OperationRecord, len(cases))
		for i, c := range cases {
			e.tick(t)
			recs[i] = e.putOperation(t, c.spec)
		}
		all := e.listOperations(t, model.Filter{}, 100)
		if !slices.Equal(all, reversed(opIDs(recs...))) {
			t.Fatalf("unfiltered list = %v, want every operation newest first %v", all, reversed(opIDs(recs...)))
		}
		for _, in := range []model.InboxRef{e.inbox, i2} {
			var wantIDs []model.ID
			for i, c := range cases {
				if c.want.inbox == in {
					wantIDs = append(wantIDs, recs[i].Operation.Ref.ID)
				}
			}
			got := e.listOperations(t, model.Filter{Inboxes: []model.InboxRef{in}}, 100)
			if !slices.Equal(got, reversed(wantIDs)) {
				t.Errorf("inbox %s lists %v, want %v", in.ID, got, reversed(wantIDs))
			}
		}
		var scoped []model.ID
		for i, c := range cases {
			if c.want.inbox.ID != "" {
				scoped = append(scoped, recs[i].Operation.Ref.ID)
			}
		}
		if got := e.listOperations(t, model.Filter{Inboxes: []model.InboxRef{e.inbox, i2}}, 100); !slices.Equal(got, reversed(scoped)) {
			t.Errorf("two inboxes list %v, want every operation that has one, %v", got, reversed(scoped))
		}
		for _, conv := range []model.ConversationRef{c1, c2, c3} {
			var wantIDs []model.ID
			for i, c := range cases {
				if c.want.conv != nil && *c.want.conv == conv {
					wantIDs = append(wantIDs, recs[i].Operation.Ref.ID)
				}
			}
			got := e.listOperations(t, model.Filter{Conversation: &conv}, 100)
			if !slices.Equal(got, reversed(wantIDs)) {
				t.Errorf("conversation %s lists %v, want %v", conv.ID, got, reversed(wantIDs))
			}
		}
		for i, c := range cases {
			sc := recs[i].Scope()
			if sc.Inbox != c.want.inbox.ID {
				t.Errorf("%s: Scope inbox = %q, want %q", c.name, sc.Inbox, c.want.inbox.ID)
			}
			if wantConv := convID(c.want.conv); sc.Conversation != wantConv {
				t.Errorf("%s: Scope conversation = %q, want %q", c.name, sc.Conversation, wantConv)
			}
		}
	}},
	{"WriteOrderNewestFirstTiesByID", func(t *testing.T, e *env) {
		a := e.putOperation(t, opSpec{target: e.inbox.Ref()})
		b := e.putOperation(t, opSpec{target: e.inbox.Ref()}) // same instant as a: the greater ID is first
		e.tick(t)
		c := e.putOperation(t, opSpec{target: e.inbox.Ref()})
		want := []model.ID{c.Operation.Ref.ID, b.Operation.Ref.ID, a.Operation.Ref.ID}
		for _, limit := range []int{1, 2, 100} {
			if got := e.listOperations(t, model.Filter{}, limit); !slices.Equal(got, want) {
				t.Errorf("list at limit %d = %v, want %v", limit, got, want)
			}
		}
	}},
	{"StateChangeReordersAndRefilters", func(t *testing.T, e *env) {
		a := e.putOperation(t, opSpec{target: e.inbox.Ref(), state: model.Queued})
		e.tick(t)
		b := e.putOperation(t, opSpec{target: e.inbox.Ref(), state: model.Queued})
		e.tick(t)
		queued := model.Filter{Statuses: []string{string(model.Queued)}}
		if got := e.listOperations(t, queued, 100); !slices.Equal(got, []model.ID{b.Operation.Ref.ID, a.Operation.Ref.ID}) {
			t.Fatalf("queued = %v, want both, newest first", got)
		}
		e.moveOperation(t, &a, model.Delivered)
		if got := e.listOperations(t, queued, 100); !slices.Equal(got, []model.ID{b.Operation.Ref.ID}) {
			t.Errorf("queued after a settled = %v, want only b: the state filter reads the current state", got)
		}
		delivered := model.Filter{Statuses: []string{string(model.Delivered)}}
		if got := e.listOperations(t, delivered, 100); !slices.Equal(got, []model.ID{a.Operation.Ref.ID}) {
			t.Errorf("delivered = %v, want a", got)
		}
		both := model.Filter{Statuses: []string{string(model.Delivered), string(model.Queued)}}
		if got := e.listOperations(t, both, 100); !slices.Equal(got, []model.ID{a.Operation.Ref.ID, b.Operation.Ref.ID}) {
			t.Errorf("delivered or queued = %v, want a (written last) then b", got)
		}
		if got := e.listOperations(t, model.Filter{}, 100); !slices.Equal(got, []model.ID{a.Operation.Ref.ID, b.Operation.Ref.ID}) {
			t.Errorf("unfiltered = %v, want the operation written last first", got)
		}
		if got := e.listOperations(t, model.Filter{Statuses: []string{"no_such_state"}}, 100); len(got) != 0 {
			t.Errorf("unknown state lists %v, want none", got)
		}
	}},
	{"WindowEdges", func(t *testing.T, e *env) {
		var recs []store.OperationRecord
		var at []time.Time
		for range 4 {
			recs = append(recs, e.putOperation(t, opSpec{target: e.inbox.Ref()}))
			at = append(at, e.fx.Now())
			e.tick(t)
		}
		id := func(i ...int) []model.ID {
			var out []model.ID
			for _, n := range i {
				out = append(out, recs[n].Operation.Ref.ID)
			}
			return out
		}
		window := func(since, before *time.Time) model.Filter { return model.Filter{Since: since, Before: before} }
		cases := []struct {
			name          string
			since, before *time.Time
			want          []model.ID
		}{
			{"since is inclusive", &at[1], nil, id(3, 2, 1)},
			{"before is exclusive", nil, &at[2], id(1, 0)},
			{"both", &at[1], &at[3], id(2, 1)},
			{"one instant", &at[2], timePtr(at[2].Add(time.Microsecond)), id(2)},
			{"before the first", nil, &at[0], nil},
			{"after the last", timePtr(at[3].Add(time.Microsecond)), nil, nil},
		}
		for _, c := range cases {
			if got := e.listOperations(t, window(c.since, c.before), 100); !slices.Equal(got, c.want) {
				t.Errorf("%s: window lists %v, want %v", c.name, got, c.want)
			}
			if got := e.listOperations(t, window(c.since, c.before), 1); !slices.Equal(got, c.want) {
				t.Errorf("%s: window at page size 1 lists %v, want %v", c.name, got, c.want)
			}
		}
		for name, f := range map[string]model.Filter{
			"empty window":    window(&at[2], &at[2]),
			"reversed window": window(&at[3], &at[1]),
		} {
			e.mustViewErr(t, f, model.Validation, name)
		}
	}},
	{"PagingAcrossWrites", func(t *testing.T, e *env) {
		var recs []store.OperationRecord
		for range 5 {
			e.tick(t)
			recs = append(recs, e.putOperation(t, opSpec{target: e.inbox.Ref(), state: model.Queued}))
		}
		var first model.Page[store.OperationRecord]
		e.mustView(t, func(r store.Reader) error {
			var err error
			first, err = r.Operations(e.ctx, model.ListRequest{Page: page(2)})
			return err
		})
		if len(first.Items) != 2 || first.Complete || first.Next == "" {
			t.Fatalf("first page = %d items complete=%v next=%q, want two and a cursor", len(first.Items), first.Complete, first.Next)
		}
		// Between the pages a new operation is accepted and one already
		// paged past is written again.
		e.tick(t)
		fresh := e.putOperation(t, opSpec{target: e.inbox.Ref()})
		e.tick(t)
		e.moveOperation(t, &recs[4], model.Delivered)
		var rest []model.ID
		after := first.Next
		for range 20 {
			var p model.Page[store.OperationRecord]
			e.mustView(t, func(r store.Reader) error {
				var err error
				p, err = r.Operations(e.ctx, model.ListRequest{Page: model.PageRequest{Limit: 2, After: after}})
				return err
			})
			for _, it := range p.Items {
				rest = append(rest, it.Operation.Ref.ID)
			}
			if p.Complete {
				break
			}
			after = p.Next
		}
		want := reversed(opIDs(recs[:3]...))
		if !slices.Equal(rest, want) {
			t.Errorf("pages after the first = %v, want the three older operations %v, with neither the new operation %s nor the rewritten one repeated", rest, want, fresh.Operation.Ref.ID)
		}
	}},
	{"RolledBackClaimIsNotListed", func(t *testing.T, e *env) {
		kept := e.putOperation(t, opSpec{target: e.inbox.Ref()})
		_ = e.txErr(func(tx store.Tx) error {
			rec := e.operation(t, "list-rolled-back", []byte{2})
			if _, _, err := tx.ClaimOperation(e.ctx, rec); err != nil {
				return err
			}
			return errAbort
		})
		if got := e.listOperations(t, model.Filter{}, 100); !slices.Equal(got, opIDs(kept)) {
			t.Errorf("list = %v, want only the committed operation", got)
		}
	}},
	{"TenantIsolation", func(t *testing.T, e *env) {
		mine := e.putOperation(t, opSpec{target: e.inbox.Ref()})
		foreign := e.operation(t, "list-foreign", []byte{3})
		foreign.Operation.Ref.Tenant = tenantB
		foreign.Key.Tenant = tenantB
		foreign.Key.Target = model.InboxRef{Tenant: tenantB, ID: e.inbox.ID}.Ref()
		if err := e.s.Transact(e.ctx, tenantB, func(tx store.Tx) error {
			_, _, err := tx.ClaimOperation(e.ctx, foreign)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if got := e.listOperations(t, model.Filter{}, 100); !slices.Equal(got, opIDs(mine)) {
			t.Errorf("tenant A lists %v, want only its own operation", got)
		}
		inB := model.Filter{Inboxes: []model.InboxRef{{Tenant: tenantB, ID: e.inbox.ID}}}
		e.mustViewErr(t, inB, model.NotFound, "foreign inbox filter")
		var p model.Page[store.OperationRecord]
		if err := e.s.View(e.ctx, tenantB, func(r store.Reader) error {
			var err error
			p, err = r.Operations(e.ctx, list(100))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(p.Items) != 1 || p.Items[0].Operation.Ref.ID != foreign.Operation.Ref.ID || !p.Complete {
			t.Errorf("tenant B lists %d items, want only its own operation", len(p.Items))
		}
	}},
	{"BadRequests", func(t *testing.T, e *env) {
		e.putOperation(t, opSpec{target: e.inbox.Ref()})
		for _, limit := range []int{0, -1} {
			wantCode(t, e.viewErr(func(r store.Reader) error {
				_, err := r.Operations(e.ctx, list(limit))
				return err
			}), model.Validation, "page limit ", limit)
		}
		var p model.Page[store.OperationRecord]
		e.mustView(t, func(r store.Reader) error {
			var err error
			p, err = r.Operations(e.ctx, list(1))
			return err
		})
		for _, c := range []model.Cursor{"x", "mem.1", "pg.1", p.Next + "~", "mem.o.1", "pg.o.x.y", "mem.o.1.not-an-id"} {
			wantCode(t, e.viewErr(func(r store.Reader) error {
				_, err := r.Operations(e.ctx, model.ListRequest{Page: model.PageRequest{Limit: 1, After: c}})
				return err
			}), model.Validation, "cursor ", fmt.Sprintf("%q", c))
		}
	}},
}

func (e *env) mustViewErr(t *testing.T, f model.Filter, code model.ErrorCode, what string) {
	t.Helper()
	err := e.viewErr(func(r store.Reader) error {
		_, err := r.Operations(e.ctx, model.ListRequest{Filter: f, Page: page(10)})
		return err
	})
	if err == nil {
		t.Errorf("%s: Operations succeeded, want %s", what, code)
		return
	}
	if got := model.CodeOf(err); got != code {
		t.Errorf("%s: code = %s, want %s", what, got, code)
	}
}

func refPtr(r model.Ref) *model.Ref { return &r }

func timePtr(t time.Time) *time.Time { return &t }

func convID(c *model.ConversationRef) model.ID {
	if c == nil {
		return ""
	}
	return c.ID
}
