package conformance

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// matrixField is one model.Filter field.
type matrixField struct {
	name string
	// set stores a well-formed value naming the world's own records.
	set func(f *model.Filter, w *matrixWorld)
	// foreign stores a well-formed reference owned by another tenant; nil
	// for a field that names no reference.
	foreign func(f *model.Filter, w *matrixWorld)
}

func otherTenant(in model.InboxRef) model.InboxRef { return model.InboxRef{Tenant: tenantB, ID: in.ID} }

// matrixFields lists every model.Filter field. A test fails when the model
// gains one, so each read's treatment of it is pinned.
var matrixFields = []matrixField{
	{"inboxes",
		func(f *model.Filter, w *matrixWorld) { f.Inboxes = []model.InboxRef{w.i1} },
		func(f *model.Filter, w *matrixWorld) { f.Inboxes = []model.InboxRef{otherTenant(w.i1)} }},
	{"conversation",
		func(f *model.Filter, w *matrixWorld) { f.Conversation = &w.cv1 },
		func(f *model.Filter, w *matrixWorld) {
			c := model.ConversationRef{Inbox: otherTenant(w.cv1.Inbox), ID: w.cv1.ID}
			f.Conversation = &c
		}},
	{"contact",
		func(f *model.Filter, w *matrixWorld) { f.Contact = &w.idA.Contact },
		func(f *model.Filter, w *matrixWorld) {
			c := model.ContactRef{Tenant: tenantB, ID: w.idA.Contact.ID}
			f.Contact = &c
		}},
	{"kinds", func(f *model.Filter, _ *matrixWorld) { f.Kinds = []string{model.ContactKindPerson} }, nil},
	{"statuses", func(f *model.Filter, _ *matrixWorld) { f.Statuses = []string{model.ConversationOpen} }, nil},
	{"identity",
		func(f *model.Filter, w *matrixWorld) { f.Identity = &w.idA.Ref },
		func(f *model.Filter, w *matrixWorld) {
			id := model.IdentityRef{Inbox: otherTenant(w.idA.Ref.Inbox), ID: w.idA.Ref.ID}
			f.Identity = &id
		}},
	{"labels",
		func(f *model.Filter, w *matrixWorld) { f.Labels = &model.LabelSelector{Any: []model.LabelRef{w.l1}} },
		func(f *model.Filter, w *matrixWorld) {
			f.Labels = &model.LabelSelector{Any: []model.LabelRef{{Tenant: tenantB, ID: w.l1.ID}}}
		}},
	{"task",
		func(f *model.Filter, w *matrixWorld) { f.Task = &w.tk1 },
		func(f *model.Filter, w *matrixWorld) {
			tk := model.TaskRef{Conversation: model.ConversationRef{Inbox: otherTenant(w.cv1.Inbox), ID: w.cv1.ID}, ID: w.tk1.ID}
			f.Task = &tk
		}},
	{"text", func(f *model.Filter, _ *matrixWorld) { f.Text = "alpha" }, nil},
	{"since", func(f *model.Filter, _ *matrixWorld) { at := time.Unix(1_700_000_000, 0).UTC(); f.Since = &at }, nil},
	{"before", func(f *model.Filter, _ *matrixWorld) { at := time.Unix(1_700_000_000, 0).UTC(); f.Before = &at }, nil},
	{"attachments", func(f *model.Filter, _ *matrixWorld) {
		yes := true
		f.Attachments = &model.AttachmentFilter{Has: &yes}
	}, nil},
	{"assignee", func(f *model.Filter, _ *matrixWorld) { f.Assignee = &model.AssigneeFilter{Actor: matrixAssignee} }, nil},
	{"snoozed", func(f *model.Filter, _ *matrixWorld) {
		yes := true
		f.Snoozed = &yes
	}, nil},
	{"linked", func(f *model.Filter, _ *matrixWorld) {
		yes := true
		f.Linked = &yes
	}, nil},
	{"unread", func(f *model.Filter, _ *matrixWorld) { f.Unread = &model.UnreadFilter{Actor: matrixAssignee} }, nil},
}

// matrixAssignee is the actor the matrix world assigns a conversation to.
const matrixAssignee model.ActorID = "matrix-desk-1"

// engineFilterFields are the model.Filter fields the engine evaluates
// itself and never passes to a store read, with the reason. The matrix does
// not pin them: a store read never receives them, so how it treats one is
// not part of the store contract.
var engineFilterFields = map[string]string{
	"Actor": "Operations.List matches it against each record's key in the engine; every other engine query refuses it before any store read",
}

// filterMatrixCases pin, for every read that takes a model.Filter and every
// filter field, that the read either narrows its result by the field or
// rejects it with model.Validation: a field is never silently ignored. They
// restate the filter matrix in the store package documentation as
// assertions, so a store that ignores a field fails by naming the read and
// the field.
var filterMatrixCases = slices.Concat([]storeCase{
	{"EveryFilterFieldHasARow", func(t *testing.T, _ *env) {
		typ := reflect.TypeFor[model.Filter]()
		for name := range engineFilterFields {
			if _, ok := typ.FieldByName(name); !ok {
				t.Errorf("engineFilterFields lists %s, which model.Filter does not declare", name)
			}
		}
		if typ.NumField() != len(matrixFields)+len(engineFilterFields) {
			t.Fatalf("model.Filter has %d fields, the matrix lists %d: add the new field to matrixFields and to each read's honours", typ.NumField(), len(matrixFields))
		}
		known := map[string]bool{}
		for _, f := range matrixFields {
			known[f.name] = true
		}
		for _, rd := range filteredReads {
			for _, name := range rd.honours {
				if !known[name] {
					t.Errorf("%s honours unknown filter field %q", rd.name, name)
				}
			}
		}
	}},
}, filterReadCases())

func filterReadCases() []storeCase {
	cases := make([]storeCase, len(filteredReads))
	for i, rd := range filteredReads {
		cases[i] = storeCase{rd.name, func(t *testing.T, e *env) { runFilterMatrix(t, e, rd) }}
	}
	return cases
}

func runFilterMatrix(t *testing.T, e *env, rd matrixRead) {
	w := e.seedMatrixWorld(t)
	all := sortedKeys(w.readAll(t, e, rd, model.Filter{}, 100))
	read := func(f model.Filter) error {
		return e.s.View(e.ctx, tenantA, func(r store.Reader) error {
			_, err := rd.read(w, r, f, model.PageRequest{Limit: 100})
			return err
		})
	}
	for _, f := range matrixFields {
		t.Run(f.name, func(t *testing.T) {
			if !slices.Contains(rd.honours, f.name) {
				var filter model.Filter
				f.set(&filter, w)
				err := read(filter)
				if err == nil {
					t.Fatalf("%s accepted filter.%s without error: a read rejects a field it does not honour with model.Validation, never ignores it", rd.name, f.name)
				}
				wantCode(t, err, model.Validation)
				return
			}
			row, ok := w.rows[rd.name][f.name]
			if !ok {
				t.Fatalf("%s honours filter.%s but the world has no row for it", rd.name, f.name)
			}
			want := sortedKeys(idStrings(row.want))
			if len(want) == 0 || len(want) >= len(all) {
				t.Fatalf("fixture row %s.%s selects %d of %d items, want a non-empty proper subset", rd.name, f.name, len(want), len(all))
			}
			for _, limit := range []int{100, 1} {
				got := sortedKeys(w.readAll(t, e, rd, row.filter, limit))
				if !slices.Equal(got, want) {
					t.Errorf("%s with filter.%s at page size %d returned %d items %v, want %d items %v: the read ignores or mis-applies the field",
						rd.name, f.name, limit, len(got), got, len(want), want)
				}
			}
			if f.foreign != nil {
				var filter model.Filter
				f.foreign(&filter, w)
				wantCode(t, read(filter), model.NotFound)
			}
		})
	}
	t.Run("malformed", func(t *testing.T) {
		wantCode(t, read(model.Filter{Inboxes: []model.InboxRef{{}}}), model.Validation)
		wantCode(t, read(model.Filter{Kinds: []string{""}}), model.Validation)
	})
}
