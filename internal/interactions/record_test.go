package interactions_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

func (r *rig) get(ref model.InteractionRef) (model.Interaction, error) {
	return r.s.Get(r.ctx, model.Lookup[model.InteractionRef]{Call: model.Call{Correlation: "get"}, Ref: ref})
}

func (r *rig) list(f model.Filter, limit int, after model.Cursor) (model.Page[model.Interaction], error) {
	return r.s.List(r.ctx, model.ListRequest{Call: model.Call{Correlation: "list"}, Filter: f, Page: model.PageRequest{Limit: limit, After: after}})
}

// Get returns a stored interaction after authorizing interaction.read on it
// under its own audience; the result and the store reads never include the
// response token.
func TestInteractionGetAuthorized(t *testing.T) {
	r := newRig(t, everything...)
	in := r.seed(func(rec *store.InteractionRecord) {
		rec.Interaction.Audience = model.Audience{Visibility: model.Restricted, PolicyRef: "vip"}
	})
	got, err := r.get(in.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ref != in.Ref || got.Kind != model.InteractionAction || got.Action != "approve" || got.Revision != in.Revision {
		t.Fatalf("Get = %+v, want the stored interaction", got)
	}
	if s := fmt.Sprintf("%+v", got); strings.Contains(s, enginetest.InteractionToken) {
		t.Fatalf("Get result holds the response token: %s", s)
	}
	if n := r.spy.TokenReads(); n != 0 {
		t.Fatalf("Get read the token %d times", n)
	}
	asks := r.policy.asked(model.ActionInteractionRead)
	if len(asks) != 1 || asks[0].Resource != in.Ref.Ref() || asks[0].Audience != in.Audience || asks[0].Revision != in.Revision {
		t.Fatalf("interaction.read authorizations = %+v, want one on the record under its audience", asks)
	}
}

// A principal the policy refuses cannot read an interaction; the refusal is
// the policy's, and the audience it saw is the record's.
func TestInteractionGetDenied(t *testing.T) {
	r := newRig(t, everything...)
	in := r.seed(func(rec *store.InteractionRecord) {
		rec.Interaction.Audience = model.Audience{Visibility: model.Internal}
	})
	r.policy.deny = func(a policy.Authorization) bool {
		return a.Action == model.ActionInteractionRead && a.Audience.Visibility == model.Internal
	}
	_, err := r.get(in.Ref)
	enginetest.RequireCode(t, err, model.Unauthorized, "")
}

// An interaction that is missing, in another conversation, inbox or tenant
// than its reference names is not found, never confirmed.
func TestInteractionGetNotFound(t *testing.T) {
	r := newRig(t, everything...)
	in := r.seed()
	other := model.ConversationRef{Inbox: r.conv.Ref.Inbox, ID: "00000005-0000-7000-8000-0000000000f1"}
	foreign := model.InteractionRef{Conversation: model.ConversationRef{
		Inbox: model.InboxRef{Tenant: enginetest.TenantB, ID: r.conv.Ref.Inbox.ID}, ID: r.conv.Ref.ID,
	}, ID: in.Ref.ID}
	cases := map[string]model.InteractionRef{
		"missing":            {Conversation: r.conv.Ref, ID: "00000005-0000-7000-8000-0000000000f2"},
		"other conversation": {Conversation: other, ID: in.Ref.ID},
		"other tenant":       foreign,
	}
	for name, ref := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := r.get(ref)
			enginetest.RequireCode(t, err, model.NotFound, "resource not found")
		})
	}
	if _, err := r.s.Get(r.ctx, model.Lookup[model.InteractionRef]{}); err == nil {
		t.Fatal("Get accepted an empty lookup")
	}
}

// A redacted interaction is still readable: the tombstone keeps refs, actor
// and response state.
func TestInteractionGetRedacted(t *testing.T) {
	r := newRig(t, everything...)
	in := r.seed(func(rec *store.InteractionRecord) {
		rec.Interaction.Redacted = true
		rec.ResponseToken = ""
	})
	got, err := r.get(in.Ref)
	if err != nil || !got.Redacted {
		t.Fatalf("Get = %+v, %v; want the redacted record", got, err)
	}
}

func (r *rig) seedConversation(id model.ID) model.Conversation {
	r.t.Helper()
	conv := r.conv
	conv.Ref.ID = id
	conv.Participants = nil
	conv.ReplyRoute.ID = "room-" + string(id)
	r.tx(func(tx store.Tx) error { return tx.PutConversation(r.ctx, conv, "") })
	return conv
}

// List pages interactions per conversation, inbox or actor identity in
// recording order, a bounded page at a time, with no token anywhere.
func TestInteractionList(t *testing.T) {
	r := newRig(t, everything...)
	second := r.seedConversation("00000005-0000-7000-8000-0000000000c2")
	var mine []model.InteractionRef
	for range 5 {
		mine = append(mine, r.seed().Ref)
	}
	r.ixSeq += 10
	theirs := enginetest.InteractionSeed{
		Conv: second.Ref, ID: "00000005-0000-7000-8000-0000000000d1", Actor: r.other,
		Channel: r.ch, Audience: public(), Now: r.clock.Now(), RespondUntil: r.clock.Now().Add(time.Hour),
	}.Record()
	r.tx(func(tx store.Tx) error { return tx.PutInteraction(r.ctx, theirs) })

	refs := func(p model.Page[model.Interaction]) []model.InteractionRef {
		var out []model.InteractionRef
		for _, in := range p.Items {
			out = append(out, in.Ref)
		}
		return out
	}
	equal := func(got, want []model.InteractionRef) bool { return slices.Equal(got, want) }

	conv := r.conv.Ref
	p, err := r.list(model.Filter{Conversation: &conv}, 0, "")
	if err != nil || !equal(refs(p), mine) || !p.Complete {
		t.Fatalf("conversation list = %v (complete %v), %v; want %v", refs(p), p.Complete, err, mine)
	}
	// Pages of two walk the same records exactly once.
	var walked []model.InteractionRef
	var after model.Cursor
	for range 10 {
		p, err := r.list(model.Filter{Conversation: &conv}, 2, after)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Items) > 2 {
			t.Fatalf("page of %d items, limit 2", len(p.Items))
		}
		walked = append(walked, refs(p)...)
		if p.Complete {
			break
		}
		after = p.Next
	}
	if !equal(walked, mine) {
		t.Fatalf("paged = %v, want %v", walked, mine)
	}
	// By inbox: every interaction of both conversations.
	p, err = r.list(model.Filter{Inboxes: []model.InboxRef{r.conv.Ref.Inbox}}, 0, "")
	if err != nil || len(p.Items) != 6 {
		t.Fatalf("inbox list = %d items, %v; want 6", len(p.Items), err)
	}
	// By actor identity.
	actor := r.other.Ref
	p, err = r.list(model.Filter{Identity: &actor}, 0, "")
	if err != nil || len(p.Items) != 1 || p.Items[0].Ref != theirs.Interaction.Ref {
		t.Fatalf("identity list = %v, %v; want the other actor's interaction", refs(p), err)
	}
	if s := fmt.Sprintf("%+v", p); strings.Contains(s, enginetest.InteractionToken) {
		t.Fatalf("List result holds the response token: %s", s)
	}
	if n := r.spy.TokenReads(); n != 0 {
		t.Fatalf("List read the token %d times", n)
	}
}

// List authorizes interaction.read per item under the item's audience and
// skips what the principal may not read, without a trace.
func TestInteractionListSkipsUnreadable(t *testing.T) {
	r := newRig(t, everything...)
	a := r.seed()
	hidden := r.seed(func(rec *store.InteractionRecord) {
		rec.Interaction.Audience = model.Audience{Visibility: model.Internal}
	})
	c := r.seed()
	r.policy.deny = func(x policy.Authorization) bool { return x.Resource == hidden.Ref.Ref() }
	p, err := r.list(model.Filter{}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 2 || p.Items[0].Ref != a.Ref || p.Items[1].Ref != c.Ref {
		t.Fatalf("List = %+v, want the two readable interactions", p.Items)
	}
	asks := r.policy.asked(model.ActionInteractionRead)
	if len(asks) != 3 {
		t.Fatalf("interaction.read asked %d times, want once per scanned item", len(asks))
	}
}

// A filter field the query does not honour is a validation error, and a
// filter naming another tenant is not found.
func TestInteractionListFilterRules(t *testing.T) {
	r := newRig(t, everything...)
	r.seed()
	contact := model.ContactRef{Tenant: enginetest.TenantA, ID: r.member.Contact.ID}
	_, err := r.list(model.Filter{Contact: &contact}, 0, "")
	enginetest.RequireCode(t, err, model.Validation, "filter.contact")
	_, err = r.list(model.Filter{Kinds: []string{"action"}}, 0, "")
	enginetest.RequireCode(t, err, model.Validation, "filter.kinds")
	foreign := model.ConversationRef{Inbox: model.InboxRef{Tenant: enginetest.TenantB, ID: r.conv.Ref.Inbox.ID}, ID: r.conv.Ref.ID}
	_, err = r.list(model.Filter{Conversation: &foreign}, 0, "")
	enginetest.RequireCode(t, err, model.NotFound, "")
	_, err = r.list(model.Filter{}, 0, "q1.forged")
	enginetest.RequireCode(t, err, model.Validation, "cursor")
}
