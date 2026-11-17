package conformance

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// labelCases cover label definitions and assignments: compare-and-set
// writes, names unique per tenant under model.LabelNameKey, lookup by name,
// deletion refused while a conversation holds the label, assignments
// stored on the conversation, and the conversation label selector.
var labelCases = []storeCase{
	{"LabelPutGetList", func(t *testing.T, e *env) {
		urgent := e.label(t, "Urgent", "r1")
		urgent.Description, urgent.Color = "reply today", "#d73a4a"
		later := e.label(t, "later", "r1")
		e.mustTx(t, func(tx store.Tx) error {
			return firstErr(tx.PutLabel(e.ctx, urgent, ""), tx.PutLabel(e.ctx, later, ""))
		})
		e.wantLabel(t, urgent)
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutLabel(e.ctx, urgent, "") }), model.Conflict)
		stale := urgent
		stale.Revision = "r3"
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutLabel(e.ctx, stale, "r2") }), model.Conflict)
		wantCode(t, e.txErr(func(tx store.Tx) error {
			return tx.PutLabel(e.ctx, model.Label{Ref: e.labelRef(t), Name: " padded", Revision: "r1"}, "")
		}), model.Validation)
		// An update keeps the label's list position.
		urgent.Name, urgent.Color, urgent.Revision = "Urgent!", "", "r2"
		e.mustTx(t, func(tx store.Tx) error { return tx.PutLabel(e.ctx, urgent, "r1") })
		e.wantLabel(t, urgent)
		if got := e.allLabels(t, 1); !slices.Equal(got, []model.Label{urgent, later}) {
			t.Fatalf("Labels = %+v, want %+v", got, []model.Label{urgent, later})
		}
		e.mustView(t, func(r store.Reader) error {
			_, err := r.GetLabel(e.ctx, e.labelRef(t))
			wantCode(t, err, model.NotFound)
			_, err = r.Labels(e.ctx, model.ListRequest{Filter: model.Filter{Statuses: []string{"open"}}, Page: page(2)})
			wantCode(t, err, model.Validation)
			_, err = r.Labels(e.ctx, model.ListRequest{Filter: model.Filter{Labels: &model.LabelSelector{Any: []model.LabelRef{urgent.Ref}}}, Page: page(2)})
			wantCode(t, err, model.Validation)
			return nil
		})
	}},
	{"LabelByNameFolds", func(t *testing.T, e *env) {
		urgent := e.label(t, "Urgent", "r1")
		strasse := e.label(t, "Straße", "r1")
		e.mustTx(t, func(tx store.Tx) error {
			return firstErr(tx.PutLabel(e.ctx, urgent, ""), tx.PutLabel(e.ctx, strasse, ""))
		})
		for name, want := range map[string]model.Label{
			"Urgent": urgent, "urgent": urgent, "URGENT": urgent, "uRgEnT": urgent,
			"STRAßE": strasse, "straße": strasse,
		} {
			e.mustView(t, func(r store.Reader) error {
				got, err := r.LabelByName(e.ctx, name)
				if err == nil && got != want {
					t.Errorf("LabelByName(%q) = %+v, want %+v", name, got, want)
				}
				return err
			})
		}
		for name, code := range map[string]model.ErrorCode{"": model.Validation, "Urgent ": model.NotFound, "urgen": model.NotFound, "strasse": model.NotFound} {
			wantCode(t, e.viewErr(func(r store.Reader) error {
				_, err := r.LabelByName(e.ctx, name)
				return err
			}), code, "LabelByName ", fmt.Sprintf("%q", name))
		}
	}},
	{"LabelNameUniquePerTenant", func(t *testing.T, e *env) {
		first := e.label(t, "Urgent", "r1")
		e.mustTx(t, func(tx store.Tx) error { return tx.PutLabel(e.ctx, first, "") })
		for _, name := range []string{"Urgent", "urgent", "URGENT"} {
			clash := e.label(t, name, "r1")
			wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutLabel(e.ctx, clash, "") }), model.Conflict, "PutLabel ", fmt.Sprintf("%q", name))
		}
		// Renaming another label onto a held name clashes as well.
		second := e.label(t, "Later", "r1")
		e.mustTx(t, func(tx store.Tx) error { return tx.PutLabel(e.ctx, second, "") })
		renamed := second
		renamed.Name, renamed.Revision = "uRGENT", "r2"
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutLabel(e.ctx, renamed, "r1") }), model.Conflict)
		// The holder may change the case of its own name.
		first.Name, first.Revision = "URGENT", "r2"
		e.mustTx(t, func(tx store.Tx) error { return tx.PutLabel(e.ctx, first, "r1") })
		// Another tenant may hold the same name.
		other := model.Label{Ref: model.LabelRef{Tenant: tenantB, ID: e.id(t)}, Name: "urgent", Revision: "r1"}
		if err := e.s.Transact(e.ctx, tenantB, func(tx store.Tx) error { return tx.PutLabel(e.ctx, other, "") }); err != nil {
			t.Fatalf("PutLabel in another tenant: %v", err)
		}
		if err := e.s.View(e.ctx, tenantB, func(r store.Reader) error {
			got, err := r.LabelByName(e.ctx, "Urgent")
			if err == nil && got != other {
				t.Errorf("LabelByName in tenant B = %+v, want %+v", got, other)
			}
			return err
		}); err != nil {
			t.Fatalf("View in another tenant: %v", err)
		}
		// Renaming releases the old name.
		first.Name, first.Revision = "Soon", "r3"
		e.mustTx(t, func(tx store.Tx) error { return tx.PutLabel(e.ctx, first, "r2") })
		e.mustTx(t, func(tx store.Tx) error { return tx.PutLabel(e.ctx, renamed, "r1") })
		e.mustView(t, func(r store.Reader) error {
			got, err := r.LabelByName(e.ctx, "urgent")
			if err == nil && got != renamed {
				t.Errorf("LabelByName after rename = %+v, want %+v", got, renamed)
			}
			return err
		})
	}},
	{"LabelForeignTenantNotFound", func(t *testing.T, e *env) {
		foreign := model.Label{Ref: model.LabelRef{Tenant: tenantB, ID: e.id(t)}, Name: "Urgent", Revision: "r1"}
		if err := e.s.Transact(e.ctx, tenantB, func(tx store.Tx) error { return tx.PutLabel(e.ctx, foreign, "") }); err != nil {
			t.Fatalf("PutLabel in tenant B: %v", err)
		}
		e.mustView(t, func(r store.Reader) error {
			_, err := r.GetLabel(e.ctx, foreign.Ref)
			wantCode(t, err, model.NotFound)
			_, err = r.LabelByName(e.ctx, "Urgent")
			wantCode(t, err, model.NotFound)
			p, err := r.Labels(e.ctx, list(10))
			if err == nil && len(p.Items) != 0 {
				t.Errorf("Labels in tenant A = %+v, want none", p.Items)
			}
			_, err = r.Conversations(e.ctx, model.ListRequest{Filter: model.Filter{Labels: &model.LabelSelector{None: []model.LabelRef{foreign.Ref}}}, Page: page(10)})
			wantCode(t, err, model.NotFound)
			return nil
		})
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutLabel(e.ctx, foreign, "") }), model.NotFound)
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.DeleteLabel(e.ctx, foreign.Ref, "r1") }), model.NotFound)
		// An unstored label of the unit's tenant cannot be assigned.
		conv := e.conversation(model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, "r1")
		conv.Labels = []model.LabelRef{e.labelRef(t)}
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutConversation(e.ctx, conv, "") }), model.NotFound)
		conv.Labels = []model.LabelRef{foreign.Ref}
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutConversation(e.ctx, conv, "") }), model.Validation)
	}},
	{"ConversationLabelsStored", func(t *testing.T, e *env) {
		a, b := e.putLabel(t, "a"), e.putLabel(t, "b")
		conv := e.conversation(model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, "r1")
		conv.Labels = []model.LabelRef{b, a}
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutConversation(e.ctx, conv, "") }), model.Validation)
		conv.Labels = sortedLabels(a, b)
		e.mustTx(t, func(tx store.Tx) error { return tx.PutConversation(e.ctx, conv, "") })
		e.wantConversationLabels(t, conv.Ref, a, b)
		// Assignments change with the conversation's revision only.
		next := conv.Clone()
		next.Labels, next.Revision = []model.LabelRef{b}, "r2"
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutConversation(e.ctx, next, "r0") }), model.Conflict)
		e.wantConversationLabels(t, conv.Ref, a, b)
		e.mustTx(t, func(tx store.Tx) error { return tx.PutConversation(e.ctx, next, "r1") })
		e.wantConversationLabels(t, conv.Ref, b)
		e.mustView(t, func(r store.Reader) error {
			got, err := r.GetConversation(e.ctx, conv.Ref)
			if err == nil && got.Revision != "r2" {
				t.Errorf("revision = %q, want r2", got.Revision)
			}
			return err
		})
		// A rolled-back assignment leaves none behind.
		rolled := next.Clone()
		rolled.Labels, rolled.Revision = sortedLabels(a, b), "r3"
		_ = e.txErr(func(tx store.Tx) error {
			if err := tx.PutConversation(e.ctx, rolled, "r2"); err != nil {
				return err
			}
			return model.NewError(model.Permanent, "roll back")
		})
		e.wantConversationLabels(t, conv.Ref, b)
		e.wantFiltered(t, model.LabelSelector{Any: []model.LabelRef{a}})
		// A claimed conversation holds its labels as a written one does.
		claim := store.ConversationClaim{
			Selection: store.SelectionKey{Inbox: e.inbox, Mode: model.ModePerContact, Key: "labelled"},
			CreateKey: "k", Conversation: e.conversation(model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, "r1"),
		}
		claim.Conversation.Labels = []model.LabelRef{e.labelRef(t)}
		wantCode(t, e.txErr(func(tx store.Tx) error {
			_, _, err := tx.ClaimConversation(e.ctx, claim)
			return err
		}), model.NotFound)
		claim.Conversation.Labels = []model.LabelRef{a}
		e.mustTx(t, func(tx store.Tx) error {
			_, _, err := tx.ClaimConversation(e.ctx, claim)
			return err
		})
		e.wantFiltered(t, model.LabelSelector{Any: []model.LabelRef{a}}, claim.Conversation.Ref)
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.DeleteLabel(e.ctx, a, "r1") }), model.Conflict)
	}},
	{"LabelDeleteWhileAssigned", func(t *testing.T, e *env) {
		held, free := e.putLabel(t, "held"), e.putLabel(t, "free")
		conv := e.conversation(model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, "r1")
		conv.Labels = []model.LabelRef{held}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutConversation(e.ctx, conv, "") })
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.DeleteLabel(e.ctx, held, "r1") }), model.Conflict)
		e.wantLabelExists(t, held, true)
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.DeleteLabel(e.ctx, free, "") }), model.Validation)
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.DeleteLabel(e.ctx, free, "r0") }), model.Conflict)
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.DeleteLabel(e.ctx, e.labelRef(t), "r1") }), model.NotFound)
		e.mustTx(t, func(tx store.Tx) error { return tx.DeleteLabel(e.ctx, free, "r1") })
		e.wantLabelExists(t, free, false)
		// Removing the assignment frees the label, and its name.
		conv.Labels, conv.Revision = nil, "r2"
		e.mustTx(t, func(tx store.Tx) error { return tx.PutConversation(e.ctx, conv, "r1") })
		e.mustTx(t, func(tx store.Tx) error { return tx.DeleteLabel(e.ctx, held, "r1") })
		e.wantLabelExists(t, held, false)
		wantCode(t, e.viewErr(func(r store.Reader) error {
			_, err := r.LabelByName(e.ctx, "held")
			return err
		}), model.NotFound)
		e.putLabel(t, "HELD")
		// A deleted label cannot be assigned again.
		conv.Labels, conv.Revision = []model.LabelRef{held}, "r3"
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutConversation(e.ctx, conv, "r2") }), model.NotFound)
	}},
	{"ConversationLabelFilterTruthTable", func(t *testing.T, e *env) {
		l1, l2, l3 := e.putLabel(t, "one"), e.putLabel(t, "two"), e.putLabel(t, "three")
		unknown := e.labelRef(t)
		c0 := e.labelled(t)
		c1 := e.labelled(t, l1)
		c12 := e.labelled(t, l1, l2)
		c123 := e.labelled(t, l1, l2, l3)
		c2 := e.labelled(t, l2)
		every := []model.ConversationRef{c0, c1, c12, c123, c2}
		ls := func(refs ...model.LabelRef) []model.LabelRef { return refs }
		cs := func(refs ...model.ConversationRef) []model.ConversationRef { return refs }
		for _, tc := range []struct {
			name string
			sel  model.LabelSelector
			want []model.ConversationRef
		}{
			{"any one", model.LabelSelector{Any: ls(l1)}, cs(c1, c12, c123)},
			{"any many", model.LabelSelector{Any: ls(l1, l2)}, cs(c1, c12, c123, c2)},
			{"any held by none", model.LabelSelector{Any: ls(unknown)}, nil},
			{"any with unknown", model.LabelSelector{Any: ls(l3, unknown)}, cs(c123)},
			{"all one", model.LabelSelector{All: ls(l2)}, cs(c12, c123, c2)},
			{"all many", model.LabelSelector{All: ls(l1, l2)}, cs(c12, c123)},
			{"all three", model.LabelSelector{All: ls(l1, l2, l3)}, cs(c123)},
			{"all with unknown", model.LabelSelector{All: ls(l1, unknown)}, nil},
			{"none one", model.LabelSelector{None: ls(l1)}, cs(c0, c2)},
			{"none many", model.LabelSelector{None: ls(l1, l2)}, cs(c0)},
			{"none unknown", model.LabelSelector{None: ls(unknown)}, every},
			{"any and none", model.LabelSelector{Any: ls(l2), None: ls(l3)}, cs(c12, c2)},
			{"all and none", model.LabelSelector{All: ls(l1, l2), None: ls(l3)}, cs(c12)},
			{"any and all", model.LabelSelector{Any: ls(l3, l2), All: ls(l1)}, cs(c12, c123)},
			{"every part", model.LabelSelector{Any: ls(l2), All: ls(l1), None: ls(l3)}, cs(c12)},
		} {
			t.Run(tc.name, func(t *testing.T) { e.wantFiltered(t, tc.sel, tc.want...) })
		}
		// The selector composes with the other filter fields.
		closed := e.conversation(c12, "r2")
		closed.Status, closed.Labels = model.ConversationClosed, sortedLabels(l1, l2)
		e.mustTx(t, func(tx store.Tx) error { return tx.PutConversation(e.ctx, closed, "r1") })
		got := e.filteredConversations(t, model.Filter{
			Inboxes: []model.InboxRef{e.inbox}, Statuses: []string{model.ConversationOpen},
			Labels: &model.LabelSelector{All: ls(l1)},
		}, 10)
		if !slices.Equal(got, cs(c1, c123)) {
			t.Errorf("open conversations holding one = %v, want %v", got, cs(c1, c123))
		}
		// Relabelling moves a conversation in and out of a selection.
		moved := e.conversation(c0, "r2")
		moved.Labels = []model.LabelRef{l3}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutConversation(e.ctx, moved, "r1") })
		e.wantFiltered(t, model.LabelSelector{Any: ls(l3)}, c0, c123)
		e.wantFiltered(t, model.LabelSelector{None: ls(l1, l2)}, c0)
		e.wantFiltered(t, model.LabelSelector{None: ls(l1, l3)}, c2)
		// An empty selector is malformed.
		wantCode(t, e.viewErr(func(r store.Reader) error {
			_, err := r.Conversations(e.ctx, model.ListRequest{Filter: model.Filter{Labels: &model.LabelSelector{}}, Page: page(10)})
			return err
		}), model.Validation)
	}},
	{"LabelFilterRejectedElsewhere", func(t *testing.T, e *env) {
		sel := &model.LabelSelector{Any: []model.LabelRef{e.putLabel(t, "x")}}
		e.mustView(t, func(r store.Reader) error {
			for name, read := range map[string]func(model.ListRequest) error{
				"Inboxes":    func(q model.ListRequest) error { _, err := r.Inboxes(e.ctx, q); return err },
				"Contacts":   func(q model.ListRequest) error { _, err := r.Contacts(e.ctx, q); return err },
				"Identities": func(q model.ListRequest) error { _, err := r.Identities(e.ctx, q); return err },
				"WorkItems":  func(q model.ListRequest) error { _, err := r.WorkItems(e.ctx, q); return err },
				"Tasks":      func(q model.ListRequest) error { _, err := r.Tasks(e.ctx, q); return err },
			} {
				err := read(model.ListRequest{Filter: model.Filter{Labels: sel}, Page: page(2)})
				if !model.IsCode(err, model.Validation) {
					t.Errorf("%s with a label selector: %v, want validation", name, err)
				}
			}
			return nil
		})
	}},
}

func (e *env) labelRef(t *testing.T) model.LabelRef {
	t.Helper()
	return model.LabelRef{Tenant: tenantA, ID: e.id(t)}
}

func (e *env) label(t *testing.T, name string, rev model.Revision) model.Label {
	t.Helper()
	return model.Label{Ref: e.labelRef(t), Name: name, Revision: rev}
}

// putLabel stores a label named name at revision r1.
func (e *env) putLabel(t *testing.T, name string) model.LabelRef {
	t.Helper()
	l := e.label(t, name, "r1")
	e.mustTx(t, func(tx store.Tx) error { return tx.PutLabel(e.ctx, l, "") })
	return l.Ref
}

// labelled stores an open conversation holding labels, at revision r1.
func (e *env) labelled(t *testing.T, labels ...model.LabelRef) model.ConversationRef {
	t.Helper()
	c := e.conversation(model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, "r1")
	if len(labels) > 0 {
		c.Labels = sortedLabels(labels...)
	}
	e.mustTx(t, func(tx store.Tx) error { return tx.PutConversation(e.ctx, c, "") })
	return c.Ref
}

func sortedLabels(refs ...model.LabelRef) []model.LabelRef {
	out := slices.Clone(refs)
	slices.SortFunc(out, func(a, b model.LabelRef) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return out
}

func (e *env) wantLabel(t *testing.T, want model.Label) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		got, err := r.GetLabel(e.ctx, want.Ref)
		if err == nil && got != want {
			t.Errorf("GetLabel = %+v, want %+v", got, want)
		}
		return err
	})
}

func (e *env) wantLabelExists(t *testing.T, ref model.LabelRef, exists bool) {
	t.Helper()
	err := e.viewErr(func(r store.Reader) error {
		_, err := r.GetLabel(e.ctx, ref)
		return err
	})
	if exists && err != nil {
		t.Fatalf("GetLabel: %v", err)
	}
	if !exists {
		wantCode(t, err, model.NotFound)
	}
}

func (e *env) wantConversationLabels(t *testing.T, ref model.ConversationRef, want ...model.LabelRef) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		got, err := r.GetConversation(e.ctx, ref)
		if err == nil && !slices.Equal(got.Labels, sortedLabels(want...)) {
			t.Errorf("Labels = %v, want %v", got.Labels, sortedLabels(want...))
		}
		return err
	})
}

// allLabels reads every label page by page.
func (e *env) allLabels(t *testing.T, limit int) []model.Label {
	t.Helper()
	var out []model.Label
	req := list(limit)
	for range 100 {
		var p model.Page[model.Label]
		e.mustView(t, func(r store.Reader) (err error) {
			p, err = r.Labels(e.ctx, req)
			return err
		})
		out = append(out, p.Items...)
		if p.Complete {
			return out
		}
		req.Page.After = p.Next
	}
	t.Fatal("Labels never completed")
	return nil
}

// filteredConversations reads every conversation filter selects, page by
// page, so a selector must keep its cursors stable.
func (e *env) filteredConversations(t *testing.T, filter model.Filter, limit int) []model.ConversationRef {
	t.Helper()
	var out []model.ConversationRef
	req := model.ListRequest{Filter: filter, Page: page(limit)}
	for range 100 {
		var p model.Page[model.Conversation]
		e.mustView(t, func(r store.Reader) (err error) {
			p, err = r.Conversations(e.ctx, req)
			return err
		})
		for _, c := range p.Items {
			out = append(out, c.Ref)
		}
		if p.Complete {
			return out
		}
		req.Page.After = p.Next
	}
	t.Fatal("Conversations never completed")
	return nil
}

// wantFiltered requires sel to select want, in insertion order, both in one
// page and one conversation per page.
func (e *env) wantFiltered(t *testing.T, sel model.LabelSelector, want ...model.ConversationRef) {
	t.Helper()
	for _, limit := range []int{50, 1} {
		got := e.filteredConversations(t, model.Filter{Labels: &sel}, limit)
		if !slices.Equal(got, want) {
			t.Errorf("selector %s (page %d) = %v, want %v", brief(sel), limit, got, want)
		}
	}
}
