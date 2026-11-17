package operations_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// parentPolicy denies reading the messages in deny, fails on those in
// fail, allows the rest, and records the messages it was asked about in
// order.
type parentPolicy struct {
	mu         sync.Mutex
	deny, fail map[model.Ref]bool
	asked      []model.Ref
}

func (p *parentPolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a.Action != model.ActionMessageRead {
		return policy.Decision{Allow: true}, nil
	}
	p.asked = append(p.asked, a.Resource)
	if p.fail[a.Resource] {
		return policy.Decision{}, fmt.Errorf("policy backend down")
	}
	return policy.Decision{Allow: !p.deny[a.Resource]}, nil
}

func (p *parentPolicy) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = nil
}

func (p *parentPolicy) log() []model.Ref {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.asked)
}

// referenceReplyParent is the selection as specified: the conversation's
// latest message, in recording order, that is inbound (no Origin, no
// EchoOf), neither redacted nor deleted and public under its read
// audience, and that the caller may read. It lists the whole history and
// is the oracle the bounded read is held to.
func referenceReplyParent(ctx context.Context, c *engine.Client, conv model.ConversationRef) (*model.MessageRef, error) {
	var all []model.Message
	req := model.ListRequest{Filter: model.Filter{Conversation: &conv}, Page: model.PageRequest{Limit: 37}}
	for {
		var p model.Page[model.Message]
		if err := c.Engine().Store().View(ctx, c.Principal().Tenant, func(r store.Reader) (err error) {
			p, err = r.Messages(ctx, req)
			return err
		}); err != nil {
			return nil, err
		}
		all = append(all, p.Items...)
		if p.Complete {
			break
		}
		req.Page.After = p.Next
	}
	for _, m := range slices.Backward(all) {
		if m.Origin != nil || m.EchoOf != nil || m.Redacted || m.Deleted ||
			engine.ReadAudience(m.Audience).Visibility != model.Public {
			continue
		}
		switch err := c.Authorize(ctx, engine.MessageRead(m), m.Ref.Ref()); {
		case err == nil:
			ref := m.Ref
			return &ref, nil
		case model.IsCode(err, model.Unauthorized), model.IsCode(err, model.NotFound):
		default:
			return nil, err
		}
	}
	return nil, nil
}

// msgKind is one way a stored message relates to the default parent.
type msgKind int

const (
	kPublic     msgKind = iota // a candidate
	kInternal                  // inbound, internal
	kRestricted                // inbound, restricted
	kUnknown                   // inbound, visibility not valid: restricted
	kOutbound                  // authored by uma
	kEcho                      // inbound copy of an outbound
	kRedacted                  // public, redacted
	kDeleted                   // public, deleted
	kDenied                    // public, the caller may not read it
	kFails                     // public, the policy fails on it
	nKinds
)

// A reply that names no parent selects exactly what the whole-history
// selection selects, over varied histories: the same message, the same
// error and the same authorization checks in the same order. Histories
// cross the store's page boundaries, hold every kind of message the rules
// distinguish, record messages out of occurrence order, rewrite some in
// place, and keep other conversations beside.
func TestDefaultReplyParentMatchesWholeHistory(t *testing.T) {
	sizes := []int{0, 1, 2, 7, 49, 50, 99, 100, 101, 150, 199, 200, 201, 260, 333}
	// weights per kind: a dense mix, mostly non-candidates, and a history
	// whose only candidates are old.
	profiles := map[string][nKinds]int{
		"mixed":      {3, 1, 1, 1, 4, 1, 1, 1, 1, 0},
		"sparse":     {1, 2, 2, 1, 8, 2, 2, 2, 2, 0},
		"denied":     {2, 1, 0, 0, 2, 0, 1, 1, 6, 0},
		"failing":    {2, 0, 0, 0, 2, 0, 0, 0, 2, 1},
		"all-public": {1, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	}
	for name, weights := range profiles {
		for _, n := range sizes {
			for seed := uint64(1); seed <= 3; seed++ {
				t.Run(fmt.Sprintf("%s/n=%d/seed=%d", name, n, seed), func(t *testing.T) {
					checkParentHistory(t, rand.New(rand.NewPCG(seed, 0x756d61)), n, weights) // #nosec G404 -- seeded test data, not a secret
				})
			}
		}
	}
}

func checkParentHistory(t *testing.T, rnd *rand.Rand, n int, weights [nKinds]int) {
	pol := &parentPolicy{deny: map[model.Ref]bool{}, fail: map[model.Ref]bool{}}
	r := newRig(t, func(c *engine.Config, _ *enginetest.OutFactory) { c.Authorizer = pol })
	conv := r.Conv.Ref
	sibling := model.Conversation{Ref: model.ConversationRef{Inbox: conv.Inbox, ID: "00000005-0000-7000-8000-0000000000f1"},
		Mode: model.ModePerRoom, Status: model.ConversationOpen, Revision: "cv"}
	if err := r.E.Store().Transact(r.Ctx, tenantA, func(tx store.Tx) error { return tx.PutConversation(r.Ctx, sibling, "") }); err != nil {
		t.Fatal(err)
	}

	total := 0
	for _, w := range weights {
		total += w
	}
	pick := func() msgKind {
		x := rnd.IntN(total)
		for k, w := range weights {
			if x < w {
				return msgKind(k)
			}
			x -= w
		}
		return kPublic
	}
	next := 0
	epoch := r.Clock.Now()
	build := func(c model.ConversationRef, k msgKind, rev model.Revision) model.Message {
		next++
		m := model.Message{
			Ref:        model.MessageRef{Conversation: c, ID: model.ID(fmt.Sprintf("00000005-0000-7000-8000-%012x", next))},
			Audience:   model.Audience{Visibility: model.Public},
			Content:    model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "m"}}},
			OccurredAt: epoch.Add(time.Duration(rnd.IntN(1000)) * time.Second), // not in recording order
			RecordedAt: epoch, Revision: rev,
		}
		switch k {
		case kInternal:
			m.Audience.Visibility = model.Internal
		case kRestricted:
			m.Audience.Visibility = model.Restricted
		case kUnknown:
			m.Audience.Visibility = "nonsense"
		case kOutbound:
			m.Origin = &model.Origin{Actor: "agent-1"}
		case kEcho:
			e := model.MessageRef{Conversation: c, ID: "00000005-0000-7000-8000-00000000ffff"}
			m.EchoOf = &e
		case kRedacted:
			m.Redacted = true
		case kDeleted:
			m.Deleted = true
		case kDenied:
			pol.deny[m.Ref.Ref()] = true
		case kFails:
			pol.fail[m.Ref.Ref()] = true
		}
		return m
	}

	compare := func(stage string) {
		t.Helper()
		pol.reset()
		want, wantErr := referenceReplyParent(r.Ctx, r.C.Client, conv)
		wantLog := pol.log()
		pol.reset()
		got, gotErr := operations.DefaultReplyParent(r.Ctx, r.C.Client, conv)
		gotLog := pol.log()
		if (wantErr == nil) != (gotErr == nil) || wantErr != nil && model.CodeOf(wantErr) != model.CodeOf(gotErr) {
			t.Fatalf("%s: error = %v, want %v", stage, gotErr, wantErr)
		}
		switch {
		case (want == nil) != (got == nil), want != nil && *want != *got:
			t.Fatalf("%s: parent = %v, want %v", stage, got, want)
		}
		if !slices.Equal(gotLog, wantLog) {
			t.Fatalf("%s: authorization checks = %d (%v), want %d (%v)", stage, len(gotLog), gotLog, len(wantLog), wantLog)
		}
	}

	var mine []model.Message
	for done := 0; done < n; {
		batch := min(n-done, 1+rnd.IntN(40))
		if err := r.E.Store().Transact(r.Ctx, tenantA, func(tx store.Tx) error {
			for range batch {
				m := build(conv, pick(), "m1")
				mine = append(mine, m)
				if err := tx.PutMessage(r.Ctx, m, ""); err != nil {
					return err
				}
				if rnd.IntN(4) == 0 { // a message of another conversation, interleaved
					if err := tx.PutMessage(r.Ctx, build(sibling.Ref, kPublic, "m1"), ""); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		done += batch
		if rnd.IntN(3) == 0 {
			compare(fmt.Sprintf("after %d", done))
		}
	}
	compare("loaded")

	// Rewrites keep a message's place in recording order whatever they change.
	if len(mine) > 0 {
		if err := r.E.Store().Transact(r.Ctx, tenantA, func(tx store.Tx) error {
			for range 1 + len(mine)/8 {
				m := mine[rnd.IntN(len(mine))]
				if m.Revision != "m1" {
					continue
				}
				m.Revision = "m2"
				if rnd.IntN(2) == 0 {
					m.Redacted = !m.Redacted
				} else {
					m.Deleted = !m.Deleted
				}
				if err := tx.PutMessage(r.Ctx, m, "m1"); err != nil {
					return err
				}
				for i := range mine {
					if mine[i].Ref == m.Ref {
						mine[i] = m
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		compare("rewritten")
	}
}
