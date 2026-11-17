package services_test

import (
	"context"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"hop.top/uma/internal/services"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// labelled seeds conversations in inbox two holding, in order: {a}, {b},
// {a, b}, {} and returns them.
func (s *svc) labelled(a, b model.LabelRef) []model.Conversation {
	s.T.Helper()
	sets := [][]model.LabelRef{{a}, {b}, {a, b}, nil}
	out := make([]model.Conversation, len(sets))
	for i, set := range sets {
		out[i] = s.conversation(s.inbox2.Ref, model.ConversationOpen)
		if len(set) > 0 {
			if _, err := s.addLabels(model.OperationID("op-seed-"+out[i].Ref.ID), out[i].Ref, "", set...); err != nil {
				s.T.Fatalf("seed labels: %v", err)
			}
		}
	}
	return out
}

func convIDs(cs []model.Conversation) []model.ID {
	out := make([]model.ID, len(cs))
	for i, c := range cs {
		out[i] = c.Ref.ID
	}
	return out
}

// Conversations.List honours Filter.Labels: Any holds one, All holds
// every one, None holds none, parts in conjunction; an unknown label of
// the tenant is a set member only, and one of another tenant is not found.
func TestListConversationsByLabelsAnyAllNone(t *testing.T) {
	s := newSvc(t)
	a := s.configure("op-a", "A").Ref
	b := s.configure("op-b", "B").Ref
	cs := s.labelled(a, b)
	ghost := model.LabelRef{Tenant: tenantA, ID: s.id()}
	in2 := []model.InboxRef{s.inbox2.Ref}
	list := func(r model.ListRequest) (model.Page[model.Conversation], error) {
		return s.C.Conversations().List(s.Ctx, r)
	}
	cases := map[string]struct {
		sel  model.LabelSelector
		want []model.Conversation
	}{
		"any a":         {model.LabelSelector{Any: []model.LabelRef{a}}, []model.Conversation{cs[0], cs[2]}},
		"any a b":       {model.LabelSelector{Any: []model.LabelRef{a, b}}, cs[:3]},
		"all a b":       {model.LabelSelector{All: []model.LabelRef{a, b}}, cs[2:3]},
		"none a":        {model.LabelSelector{None: []model.LabelRef{a}}, []model.Conversation{cs[1], cs[3]}},
		"any b none a":  {model.LabelSelector{Any: []model.LabelRef{b}, None: []model.LabelRef{a}}, cs[1:2]},
		"any ghost":     {model.LabelSelector{Any: []model.LabelRef{ghost}}, nil},
		"all a ghost":   {model.LabelSelector{All: []model.LabelRef{a, ghost}}, nil},
		"none ghost":    {model.LabelSelector{None: []model.LabelRef{ghost}}, cs},
		"any a ghost":   {model.LabelSelector{Any: []model.LabelRef{a, ghost}}, []model.Conversation{cs[0], cs[2]}},
		"all b, none a": {model.LabelSelector{All: []model.LabelRef{b}, None: []model.LabelRef{a}}, cs[1:2]},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sel := tc.sel
			got := drain(t, list, model.Filter{Inboxes: in2, Labels: &sel}, 1)
			if !slices.Equal(convIDs(got), convIDs(tc.want)) {
				t.Fatalf("got %v, want %v", convIDs(got), convIDs(tc.want))
			}
		})
	}
	foreign := model.LabelRef{Tenant: tenantB, ID: a.ID}
	_, err := list(listReq(model.Filter{Labels: &model.LabelSelector{None: []model.LabelRef{foreign}}}, 1, ""))
	requireCode(t, err, model.NotFound, "")
	_, err = list(listReq(model.Filter{Labels: &model.LabelSelector{}}, 1, ""))
	requireCode(t, err, model.Validation, "labels")
}

// A cursor binds the label selector: it pages on under the same selector,
// in any part order, and is invalid under another.
func TestConversationLabelCursor(t *testing.T) {
	s := newSvc(t)
	a := s.configure("op-a", "A").Ref
	b := s.configure("op-b", "B").Ref
	s.labelled(a, b)
	sel := func(any ...model.LabelRef) model.Filter {
		return model.Filter{Labels: &model.LabelSelector{Any: any}}
	}
	p, err := s.C.Conversations().List(s.Ctx, listReq(sel(a, b), 1, ""))
	if err != nil || p.Next == "" {
		t.Fatalf("first page %+v, %v", p, err)
	}
	if _, err := s.C.Conversations().List(s.Ctx, listReq(sel(b, a), 1, p.Next)); err != nil {
		t.Fatalf("same selector, other order: %v", err)
	}
	for name, f := range map[string]model.Filter{
		"no selector": {},
		"other any":   sel(a),
		"all":         {Labels: &model.LabelSelector{All: []model.LabelRef{a, b}}},
		"none":        {Labels: &model.LabelSelector{None: []model.LabelRef{a, b}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.C.Conversations().List(s.Ctx, listReq(f, 1, p.Next))
			requireCode(t, err, model.Validation, "cursor")
		})
	}
}

// The canonical filter appends the selector last, only when set, behind
// its name: a filter without it encodes as before, so its cursors stay
// valid. Equivalent selectors encode alike; parts never collide.
func TestCanonicalFilterLabels(t *testing.T) {
	in := model.InboxRef{Tenant: tenantA, ID: "inbox-1"}
	conv := model.ConversationRef{Inbox: in, ID: "conv-1"}
	base := model.Filter{Inboxes: []model.InboxRef{in}, Conversation: &conv}
	const baseHex = "013107696e626f782d3101310874656e616e742d610c636f6e766572736174696f6e07696e626f782d3106636f6e762d31013001300130"
	enc := func(f model.Filter) string { return hex.EncodeToString(services.CanonicalFilter(f)) }
	if got := enc(base); got != baseHex {
		t.Fatalf("base filter encoding changed: %s", got)
	}
	la, lb := model.LabelRef{Tenant: tenantA, ID: "la"}, model.LabelRef{Tenant: tenantA, ID: "lb"}
	withText := base
	withText.Text = "invoice"
	labelled := withText
	labelled.Labels = &model.LabelSelector{Any: []model.LabelRef{la}}
	if got, prefix := enc(labelled), enc(withText); !strings.HasPrefix(got, prefix) || got == prefix {
		t.Fatalf("labels not appended after the search fields: %s", got)
	}
	// "labels", then Any, All, None as counted sorted ID lists.
	only := model.Filter{Labels: &model.LabelSelector{Any: []model.LabelRef{lb, la}, None: []model.LabelRef{la}}}
	const onlyHex = "01300130013001300130" + "066c6162656c73" + "0132" + "026c61" + "026c62" + "0130" + "0131" + "026c61"
	if got := enc(only); got != onlyHex {
		t.Fatalf("selector encoding = %s, want %s", got, onlyHex)
	}
	parts := []model.LabelSelector{
		{Any: []model.LabelRef{la}}, {All: []model.LabelRef{la}}, {None: []model.LabelRef{la}},
		{Any: []model.LabelRef{la, lb}}, {Any: []model.LabelRef{la}, All: []model.LabelRef{lb}},
	}
	seen := map[string]bool{}
	for _, sel := range parts {
		got := enc(model.Filter{Labels: &sel})
		if seen[got] {
			t.Fatalf("selectors share an encoding: %+v", sel)
		}
		seen[got] = true
	}
}

// labelBlindStore models a backend that ignores Filter.Labels on
// conversations: the service still exposes only matching conversations.
type labelBlindStore struct{ store.Store }

func (s labelBlindStore) View(ctx context.Context, t model.TenantID, fn func(store.Reader) error) error {
	return s.Store.View(ctx, t, func(r store.Reader) error { return fn(labelBlindReader{r}) })
}

type labelBlindReader struct{ store.Reader }

func (r labelBlindReader) Conversations(ctx context.Context, req model.ListRequest) (model.Page[model.Conversation], error) {
	req.Filter.Labels = nil
	return r.Reader.Conversations(ctx, req)
}

func TestConversationLabelsRecheckedOutsideStore(t *testing.T) {
	s := newSvc(t)
	a := s.configure("op-a", "A").Ref
	b := s.configure("op-b", "B").Ref
	cs := s.labelled(a, b)
	s.Store.Set(labelBlindStore{s.Store.Inner()})
	list := func(r model.ListRequest) (model.Page[model.Conversation], error) {
		return s.C.Conversations().List(s.Ctx, r)
	}
	got := drain(t, list, model.Filter{Labels: &model.LabelSelector{All: []model.LabelRef{a, b}}}, 10)
	if !slices.Equal(convIDs(got), convIDs(cs[2:3])) {
		t.Fatalf("got %v, want only the conversation holding both", convIDs(got))
	}
}
