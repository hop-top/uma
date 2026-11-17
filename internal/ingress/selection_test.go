package ingress_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"hop.top/uma/internal/ingress"
	"hop.top/uma/model"
	"hop.top/uma/policy"
)

// permutations returns every ordering of cs.
func permutations(cs []model.Conversation) [][]model.Conversation {
	if len(cs) <= 1 {
		return [][]model.Conversation{append([]model.Conversation(nil), cs...)}
	}
	var out [][]model.Conversation
	for i := range cs {
		rest := append(append([]model.Conversation(nil), cs[:i]...), cs[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]model.Conversation{cs[i]}, p...))
		}
	}
	return out
}

// requireOutcome checks a built-in selector result against a routing case's
// expected outcome.
func requireOutcome(t *testing.T, c lcCase, res policy.SelectionResult, err error) {
	t.Helper()
	switch c.Expect.Outcome {
	case "conflict":
		requireCode(t, err, model.Conflict, "")
		return
	case "create":
		if err != nil || res.CreateKey == "" || res.Existing != nil || res.Reopen || res.AddParticipant {
			t.Fatalf("result %+v, %v; want a create key only", res, err)
		}
		return
	case "reuse", "reuse_reopen":
	default:
		t.Fatalf("unexpected outcome %q for a selector case", c.Expect.Outcome)
	}
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	want := model.ConversationRef{Inbox: fixtureInbox, ID: fixtureConvID(c.Expect.Conversation)}
	switch {
	case res.Existing == nil || *res.Existing != want:
		t.Fatalf("existing = %v, want %v", res.Existing, want)
	case res.CreateKey != "":
		t.Fatalf("create key %q alongside existing", res.CreateKey)
	case res.Reopen != c.Expect.Reopen:
		t.Fatalf("reopen = %v, want %v", res.Reopen, c.Expect.Reopen)
	case res.AddParticipant != c.Expect.AddParticipant:
		t.Fatalf("add participant = %v, want %v", res.AddParticipant, c.Expect.AddParticipant)
	}
}

// The built-in per-contact selector satisfies every lifecycle, tie-break
// and race row, identically under every permutation of the candidates.
func TestContactSelectorTruthTable(t *testing.T) {
	f := loadLifecycleFixture(t)
	byID := map[string]lcCase{}
	for _, c := range f.Cases {
		byID[c.ID] = c
	}
	n := 0
	for _, c := range f.Cases {
		switch c.Group {
		case "lifecycle", "tie_break", "race":
		default:
			continue
		}
		n++
		t.Run(c.ID, func(t *testing.T) {
			in := selectionInput(c)
			var first *policy.SelectionResult
			for _, perm := range permutations(in.Candidates) {
				in.Candidates = perm
				res, err := ingress.ContactSelector{}.Select(context.Background(), in)
				requireOutcome(t, c, res, err)
				if err != nil {
					continue
				}
				if first == nil {
					first = &res
				} else if !reflect.DeepEqual(*first, res) {
					t.Fatalf("permutation changed the result: %+v vs %+v", *first, res)
				}
				if _, err := ingress.CheckSelection(res, in); err != nil {
					t.Fatalf("core rejects the built-in result: %v", err)
				}
			}
			if ref := c.Expect.SameCreateKeyAs; ref != "" {
				other, err := ingress.ContactSelector{}.Select(context.Background(), selectionInput(byID[ref]))
				if err != nil || first == nil || other.CreateKey != first.CreateKey {
					t.Fatalf("create key %+v differs from %s's %+v (%v)", first, ref, other, err)
				}
			}
		})
	}
	if n != 35 {
		t.Fatalf("selector rows = %d, want 35 (20 lifecycle, 11 tie-break, 4 race)", n)
	}
}

// Core validates any selector's result against the snapshot it was given.
func TestCheckSelectionCoreRows(t *testing.T) {
	f := loadLifecycleFixture(t)
	n := 0
	for _, c := range f.Cases {
		if c.Group != "core_check" {
			continue
		}
		n++
		t.Run(c.ID, func(t *testing.T) {
			_, err := ingress.CheckSelection(fixtureResult(c), selectionInput(c))
			if c.Expect.Outcome == "accept" {
				if err != nil {
					t.Fatalf("checkSelection: %v", err)
				}
				return
			}
			requireCode(t, err, expectedCode(c), "")
		})
	}
	if n != 7 {
		t.Fatalf("core rows = %d, want 7", n)
	}
}

func fixtureResult(c lcCase) policy.SelectionResult {
	r := c.SelectorResult
	res := policy.SelectionResult{CreateKey: r.CreateKey, Reopen: r.Reopen}
	if r.Existing != "" {
		res.Existing = &model.ConversationRef{Inbox: fixtureInbox, ID: fixtureConvID(r.Existing)}
	}
	return res
}

func TestCheckSelectionRejectsCreateKeyModifiers(t *testing.T) {
	in := policy.SelectionInput{}
	for name, res := range map[string]policy.SelectionResult{
		"reopen":          {CreateKey: "k", Reopen: true},
		"add participant": {CreateKey: "k", AddParticipant: true},
		"oversized key":   {CreateKey: string(make([]byte, ingress.MaxCreateKeyBytes+1))},
		"nul in key":      {CreateKey: "a\x00b"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ingress.CheckSelection(res, in)
			requireCode(t, err, model.Validation, "")
		})
	}
	foreign := model.ConversationRef{Inbox: model.InboxRef{Tenant: tenantB, ID: fixtureInbox.ID}, ID: fixtureConvID("c1")}
	_, err := ingress.CheckSelection(policy.SelectionResult{Existing: &foreign}, selectionInput(lcCase{
		Candidates: []lcCandidate{{ID: "c1", Status: model.ConversationOpen, CreatedAt: t0, Owner: "sender", SenderIsParticipant: true}},
	}))
	requireCode(t, err, model.Validation, "not a candidate")
}

// The scoped selector reuses the newest live conversation of a room,
// thread or work item, never reopens and adds new senders.
func TestScopedSelector(t *testing.T) {
	conv := func(id string, status string, at time.Time) model.Conversation {
		return model.Conversation{
			Ref:    model.ConversationRef{Inbox: fixtureInbox, ID: fixtureConvID(id)},
			Status: status, CreatedAt: at,
		}
	}
	sender := &model.Identity{Ref: fixtureSender}
	cases := []struct {
		name  string
		cands []model.Conversation
		want  string // conversation name, "create" or "conflict"
	}{
		{"none", nil, "create"},
		{"only closed", []model.Conversation{conv("c1", model.ConversationClosed, t0)}, "create"},
		{"resolved reused", []model.Conversation{conv("c1", model.ConversationResolved, t0)}, "c1"},
		{"newest live", []model.Conversation{
			conv("c1", model.ConversationOpen, t0), conv("c2", model.ConversationPending, t0.Add(time.Hour)),
			conv("c3", model.ConversationClosed, t0.Add(2*time.Hour)),
		}, "c2"},
		{"unorderable", []model.Conversation{conv("c1", model.ConversationOpen, time.Time{}), conv("c2", model.ConversationOpen, t0)}, "conflict"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, perm := range permutations(tc.cands) {
				res, err := ingress.ScopedSelector{}.Select(context.Background(), policy.SelectionInput{Sender: sender, Candidates: perm})
				switch tc.want {
				case "conflict":
					requireCode(t, err, model.Conflict, "")
				case "create":
					if err != nil || res.CreateKey == "" || res.Existing != nil {
						t.Fatalf("result %+v, %v; want create", res, err)
					}
				default:
					if err != nil || res.Existing == nil || res.Existing.ID != fixtureConvID(tc.want) || res.Reopen || !res.AddParticipant {
						t.Fatalf("result %+v, %v; want reuse %s with participant", res, err, tc.want)
					}
				}
			}
		})
	}
}

// Selector failures never leak foreign error text.
func TestSelectorErrorMapping(t *testing.T) {
	ctx := context.Background()
	requireCode(t, ingress.SelectorError(ctx, model.NewError(model.Conflict, "ambiguous")), model.Conflict, "ambiguous")
	requireCode(t, ingress.SelectorError(ctx, errString("secret backend detail")), model.Unavailable, "conversation selection unavailable")
}

type errString string

func (e errString) Error() string { return string(e) }
