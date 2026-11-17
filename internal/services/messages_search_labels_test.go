package services_test

import (
	"context"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// labelledMessages seeds one public message in each conversation of
// labelled(a, b), holding {a}, {b}, {a, b}, {} in that order.
func (s *svc) labelledMessages(a, b model.LabelRef) (cs []model.Conversation, ms []model.Message) {
	s.T.Helper()
	cs = s.labelled(a, b)
	for _, c := range cs {
		ms = append(ms, s.searchMsg(c.Ref, model.Public, searchT0, "ping"))
	}
	return cs, ms
}

func sel(sl model.LabelSelector) model.Filter { return model.Filter{Labels: &sl} }

// Messages.List honours Filter.Labels on the labels the message's
// conversation holds: Any holds one, All holds every one, parts in
// conjunction, composed with the other search fields.
func TestSearchByConversationLabelAny(t *testing.T) {
	s := newSvc(t)
	a := s.configure("op-a", "A").Ref
	b := s.configure("op-b", "B").Ref
	_, ms := s.labelledMessages(a, b)
	ghost := model.LabelRef{Tenant: tenantA, ID: s.id()}
	wantIDs(t, "any a", s.search(sel(model.LabelSelector{Any: []model.LabelRef{a}}), 1), ms[0], ms[2])
	wantIDs(t, "any a b", s.search(sel(model.LabelSelector{Any: []model.LabelRef{a, b}}), 10), ms[0], ms[1], ms[2])
	wantIDs(t, "any ghost", s.search(sel(model.LabelSelector{Any: []model.LabelRef{ghost}}), 10))
	wantIDs(t, "any a ghost", s.search(sel(model.LabelSelector{Any: []model.LabelRef{a, ghost}}), 10), ms[0], ms[2])
	wantIDs(t, "any a and text", s.search(model.Filter{Text: "ping", Labels: &model.LabelSelector{Any: []model.LabelRef{b}}}, 10), ms[1], ms[2])
	wantIDs(t, "any a, other text", s.search(model.Filter{Text: "pong", Labels: &model.LabelSelector{Any: []model.LabelRef{b}}}, 10))
}

func TestSearchByConversationLabelAll(t *testing.T) {
	s := newSvc(t)
	a := s.configure("op-a", "A").Ref
	b := s.configure("op-b", "B").Ref
	ghost := model.LabelRef{Tenant: tenantA, ID: s.id()}
	_, ms := s.labelledMessages(a, b)
	wantIDs(t, "all a b", s.search(sel(model.LabelSelector{All: []model.LabelRef{a, b}}), 1), ms[2])
	wantIDs(t, "all a", s.search(sel(model.LabelSelector{All: []model.LabelRef{a}}), 10), ms[0], ms[2])
	wantIDs(t, "all a ghost", s.search(sel(model.LabelSelector{All: []model.LabelRef{a, ghost}}), 10))
}

func TestSearchByConversationLabelNone(t *testing.T) {
	s := newSvc(t)
	a := s.configure("op-a", "A").Ref
	b := s.configure("op-b", "B").Ref
	ghost := model.LabelRef{Tenant: tenantA, ID: s.id()}
	_, ms := s.labelledMessages(a, b)
	wantIDs(t, "none a", s.search(sel(model.LabelSelector{None: []model.LabelRef{a}}), 1), ms[1], ms[3])
	wantIDs(t, "none a b", s.search(sel(model.LabelSelector{None: []model.LabelRef{a, b}}), 10), ms[3])
	wantIDs(t, "none ghost", s.search(sel(model.LabelSelector{None: []model.LabelRef{ghost}}), 10), ms...)
	wantIDs(t, "any b none a", s.search(sel(model.LabelSelector{Any: []model.LabelRef{b}, None: []model.LabelRef{a}}), 10), ms[1])
}

// A label removed from, or added to, a conversation changes what search
// returns as soon as the change commits.
func TestSearchLabelRemovedNoLongerMatches(t *testing.T) {
	s := newSvc(t)
	a := s.configure("op-a", "A").Ref
	b := s.configure("op-b", "B").Ref
	cs, ms := s.labelledMessages(a, b)
	byA := sel(model.LabelSelector{Any: []model.LabelRef{a}})
	wantIDs(t, "before", s.search(byA, 10), ms[0], ms[2])
	if _, err := s.removeLabels("op-rm", cs[0].Ref, "", a); err != nil {
		t.Fatal(err)
	}
	wantIDs(t, "after removal", s.search(byA, 10), ms[2])
	if _, err := s.addLabels("op-add", cs[3].Ref, "", a); err != nil {
		t.Fatal(err)
	}
	wantIDs(t, "after assignment", s.search(byA, 10), ms[2], ms[3])
	wantIDs(t, "none after assignment", s.search(sel(model.LabelSelector{None: []model.LabelRef{a}}), 10), ms[0], ms[1])
}

// A hidden message never appears because its conversation carries the
// label: the label narrows, admission still decides.
func TestSearchByLabelSkipsInternalNote(t *testing.T) {
	s := newSvc(t)
	a := s.configure("op-a", "A").Ref
	b := s.configure("op-b", "B").Ref
	cs := s.labelled(a, b)
	pub := s.searchMsg(cs[0].Ref, model.Public, searchT0, "ping")
	note := s.searchMsg(cs[0].Ref, model.Internal, searchT0, "ping")
	f := sel(model.LabelSelector{Any: []model.LabelRef{a}})
	s.pol.set(publicOnly)
	hidden, err := s.C.Messages().List(s.Ctx, listReq(f, 10, ""))
	if err != nil || len(hidden.Items) != 1 || hidden.Items[0].Ref != pub.Ref || hidden.Next != "" || !hidden.Complete {
		t.Fatalf("public page = %+v, %v", hidden, err)
	}
	s.pol.set(nil)
	wantIDs(t, "staff", s.search(f, 1), pub, note)
}

// A label search is a search: the page that completes it carries no
// Next, and its cursors bind the selector.
func TestSearchLabelSealedAndBound(t *testing.T) {
	s := newSvc(t)
	a := s.configure("op-a", "A").Ref
	b := s.configure("op-b", "B").Ref
	s.labelledMessages(a, b)
	byA := sel(model.LabelSelector{Any: []model.LabelRef{a}})
	done, err := s.C.Messages().List(s.Ctx, listReq(byA, 10, ""))
	if err != nil || !done.Complete || done.Next != "" || len(done.Items) != 2 {
		t.Fatalf("complete page = %+v, %v", done, err)
	}
	p, err := s.C.Messages().List(s.Ctx, listReq(byA, 1, ""))
	if err != nil || p.Next == "" {
		t.Fatalf("first page = %+v, %v", p, err)
	}
	if _, err := s.C.Messages().List(s.Ctx, listReq(byA, 1, p.Next)); err != nil {
		t.Fatalf("same selector: %v", err)
	}
	for name, f := range map[string]model.Filter{
		"no selector": {},
		"other any":   sel(model.LabelSelector{Any: []model.LabelRef{b}}),
		"all":         sel(model.LabelSelector{All: []model.LabelRef{a}}),
		"none":        sel(model.LabelSelector{None: []model.LabelRef{a}}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.C.Messages().List(s.Ctx, listReq(f, 1, p.Next))
			requireCode(t, err, model.Validation, "cursor")
		})
	}
}

// A selector label of another tenant is not found; an empty selector is
// malformed.
func TestSearchLabelSelectorScopeAndShape(t *testing.T) {
	s := newSvc(t)
	a := s.configure("op-a", "A").Ref
	foreign := model.LabelRef{Tenant: tenantB, ID: a.ID}
	_, err := s.C.Messages().List(s.Ctx, listReq(sel(model.LabelSelector{None: []model.LabelRef{foreign}}), 1, ""))
	requireCode(t, err, model.NotFound, "")
	_, err = s.C.Messages().List(s.Ctx, listReq(sel(model.LabelSelector{}), 1, ""))
	requireCode(t, err, model.Validation, "labels")
}

// A store that ignores the selector cannot widen a search: a label search
// the store answered unfiltered is rechecked against the conversation's
// labels before any message is exposed.
type labelBlindMessages struct{ store.Store }

func (s labelBlindMessages) View(ctx context.Context, t model.TenantID, fn func(store.Reader) error) error {
	return s.Store.View(ctx, t, func(r store.Reader) error { return fn(labelBlindMessageReader{r}) })
}

type labelBlindMessageReader struct{ store.Reader }

func (r labelBlindMessageReader) Messages(ctx context.Context, req model.ListRequest) (model.Page[model.Message], error) {
	req.Filter.Labels = nil
	return r.Reader.Messages(ctx, req)
}

func TestSearchLabelsRecheckedOutsideStore(t *testing.T) {
	s := newSvc(t)
	a := s.configure("op-a", "A").Ref
	b := s.configure("op-b", "B").Ref
	_, ms := s.labelledMessages(a, b)
	s.Store.Set(labelBlindMessages{s.Store.Inner()})
	wantIDs(t, "all a b", s.search(sel(model.LabelSelector{All: []model.LabelRef{a, b}}), 10), ms[2])
}
