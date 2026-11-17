package conformance

import (
	"context"
	"slices"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// The policy-bypass cases check that an adapter's own conduct cannot widen
// what the engine's binding allows: traffic lands only in the inbox bound to
// the account it arrived on, a rejected selection leaves the event with the
// provider, a private message never becomes public, an effect goes only to
// the stored route, and payload metadata never names the author.

var bypassCases = []adapterCase{
	{name: "BoundInboxOnly", inbound: true, run: func(t *testing.T, e *adapterEnv) {
		// Two accounts of one type bind the same destination and see equal
		// event, message and sender IDs.
		e.inbound(t)
		e.seed(t, adapterChanB, adapterInboxB, e.selection(), "support", "only-b")
		onA, onB := e.message(adapterChanA, "1", "peer-1", "support"), e.message(adapterChanB, "2", "peer-2", "support")
		e.deliver(t, adapterChanA, onA)
		wantAcked(t, e.receiveAll(t, adapterChanB, onB))
		if got := e.messages(t, adapterInboxB); len(got) != 1 || findMessage(got, adapterChanB, onB.MessageID) == nil {
			t.Fatalf("inbox B holds %s, want only account B's event", describeMessages(got))
		}
		wantAcked(t, e.pump(t, adapterChanA, 0, onA))
		if got := e.messages(t, adapterInboxA); len(got) != 1 || findMessage(got, adapterChanA, onA.MessageID) == nil {
			t.Fatalf("inbox A holds %s, want only account A's event", describeMessages(got))
		}
		// An event on account A addressed to a destination only B binds
		// reaches no inbox.
		e.receiveAll(t, adapterChanA, e.message(adapterChanA, "3", "peer-3", "only-b"))
		if n := len(e.messages(t, adapterInboxB)); n != 1 {
			t.Fatalf("inbox B holds %d messages after account A's event to its destination, want 1", n)
		}
	}},
	{name: "SelectorStaysInsideCandidates", inbound: true, run: func(t *testing.T, e *adapterEnv) {
		e.inbound(t)
		for _, c := range []struct{ policy, why string }{
			{"conformance.foreign", "a conversation outside the candidate snapshot"},
			{"conformance.both", "both an existing conversation and a create key"},
			{"conformance.reopen_open", "a reopen of a conversation that is not resolved"},
		} {
			t.Run(c.policy, func(t *testing.T) {
				e := newAdapterEnvWith(t, e.h, envOptions{selectors: hostileSelectors()})
				e.inbound(t)
				e.setPolicy(t, c.policy)
				first := e.receiveAll(t, adapterChanA, e.message(adapterChanA, "1", "peer-1", "support"))
				if c.policy == "conformance.reopen_open" {
					// The first message creates the conversation the second
					// one's selection tries to reopen.
					wantAcked(t, first)
					first = e.receiveAll(t, adapterChanA, e.message(adapterChanA, "2", "peer-1", "support"))
				}
				if len(first) == 0 || first[len(first)-1].Acked {
					t.Fatalf("event acknowledged although the engine rejected %s; it must stay with the provider", c.why)
				}
				want := 0
				if c.policy == "conformance.reopen_open" {
					want = 1
				}
				if n := len(e.messages(t, adapterInboxA)); n != want {
					t.Fatalf("messages = %d after a selector returned %s, want %d", n, c.why, want)
				}
			})
		}
		t.Run("conformance.constant_key", func(t *testing.T) {
			e := newAdapterEnvWith(t, e.h, envOptions{selectors: hostileSelectors()})
			e.inbound(t)
			e.setPolicy(t, "conformance.constant_key")
			wantAcked(t, e.receiveAll(t, adapterChanA, e.message(adapterChanA, "1", "peer-1", "support")))
			wantAcked(t, e.receiveAll(t, adapterChanA, e.message(adapterChanA, "2", "peer-1", "support")))
			if n := len(e.conversations(t, adapterInboxA)); n != 1 {
				t.Fatalf("conversations = %d under one create key, want 1", n)
			}
		})
	}},
	{name: "AudienceNeverWidened", inbound: true, run: func(t *testing.T, e *adapterEnv) {
		e.inbound(t)
		ev := e.message(adapterChanA, "1", "peer-1", "support")
		ev.Visibility = model.Internal
		got := e.receiveAll(t, adapterChanA, ev)
		msgs := e.messages(t, adapterInboxA)
		if len(msgs) == 0 {
			if got[0].Acked {
				t.Fatal("private event dropped: acknowledged without being stored")
			}
			return
		}
		for _, m := range msgs {
			if m.Audience.Visibility == model.Public {
				t.Fatalf("private provider message stored with audience %+v", m.Audience)
			}
		}
	}},
	{name: "StoredRouteOnly", needs: []model.Capability{model.CapabilitySend}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		ref := e.mustReply(t, conv.Ref, "op-route")
		e.start(t)
		e.stop(t)
		sends := e.calls(adapterChanA, model.CapabilitySend)
		if len(sends) == 0 {
			t.Fatalf("no provider send; operation %+v", e.op(t, ref))
		}
		for _, c := range sends {
			if c.Route != conv.ReplyRoute {
				t.Fatalf("provider delivered to %+v, want only the stored route %+v", c.Route, conv.ReplyRoute)
			}
		}
	}},
	{name: "NoIdentityFromMetadata", inbound: true, run: func(t *testing.T, e *adapterEnv) {
		e.inbound(t)
		ev := e.message(adapterChanA, "1", "peer-1", "support")
		ev.Claims = e.claims(adapterChanA, "peer-admin", "peer-2")
		wantAcked(t, e.receiveAll(t, adapterChanA, ev))
		msgs := e.messages(t, adapterInboxA)
		if len(msgs) != 1 || msgs[0].Author.External.ID != ev.Sender {
			t.Fatalf("messages %s, want one authored by the authenticated sender %q", describeMessages(msgs), ev.Sender)
		}
		ids := e.identities(t, adapterInboxA)
		if len(ids) != 1 || ids[0].SourceID != ev.Sender || ids[0].LinkAssurance != model.LinkAssuranceNone {
			t.Fatalf("identities %+v, want only the sender's, unlinked", ids)
		}
		e.view(t, func(r store.Reader) error {
			links, err := r.IdentityLinks(e.ctx, store.LinkFilter{Identity: &ids[0].Ref}, model.PageRequest{Limit: 10})
			if err != nil {
				return err
			}
			if len(links.Items) != 0 {
				t.Errorf("identity links %+v created from inbound metadata", links.Items)
			}
			return nil
		})
	}},
}

// setPolicy points inbox A at the registered selection policy name.
func (e *adapterEnv) setPolicy(t *testing.T, name string) {
	t.Helper()
	err := e.store.Transact(e.ctx, adapterTenant, func(tx store.Tx) error {
		in, err := tx.GetInbox(e.ctx, adapterInboxA)
		if err != nil {
			return err
		}
		prev := in.Revision
		in.Selection.Policy, in.Revision = name, prev+"-policy"
		return tx.PutInbox(e.ctx, in, prev)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// hostileSelectors are custom selection policies whose results core must
// reject or contain.
func hostileSelectors() map[string]policy.ConversationSelector {
	foreign := model.ConversationRef{
		Inbox: model.InboxRef{Tenant: "conformance-foreign", ID: "00000ad0-0000-7000-8000-0000000000f1"},
		ID:    "00000ad0-0000-7000-8000-0000000000f2",
	}
	return map[string]policy.ConversationSelector{
		"conformance.foreign": selectFunc(func(policy.SelectionInput) policy.SelectionResult {
			return policy.SelectionResult{Existing: &foreign}
		}),
		"conformance.both": selectFunc(func(policy.SelectionInput) policy.SelectionResult {
			return policy.SelectionResult{Existing: &foreign, CreateKey: "both"}
		}),
		"conformance.reopen_open": selectFunc(func(in policy.SelectionInput) policy.SelectionResult {
			if len(in.Candidates) == 0 {
				return policy.SelectionResult{CreateKey: "first"}
			}
			ref := in.Candidates[0].Ref
			return policy.SelectionResult{Existing: &ref, Reopen: true}
		}),
		"conformance.constant_key": selectFunc(func(policy.SelectionInput) policy.SelectionResult {
			return policy.SelectionResult{CreateKey: "constant"}
		}),
	}
}

type selectFunc func(policy.SelectionInput) policy.SelectionResult

func (f selectFunc) Select(_ context.Context, in policy.SelectionInput) (policy.SelectionResult, error) {
	return f(in), nil
}

func findMessage(msgs []model.Message, ch model.ChannelRef, providerID string) *model.Message {
	ref := model.ProviderMessageRef(ch, providerID)
	for i := range msgs {
		if slices.Contains(msgs[i].External, ref) {
			return &msgs[i]
		}
	}
	return nil
}

func describeMessages(msgs []model.Message) string {
	out := "["
	for i, m := range msgs {
		if i > 0 {
			out += " "
		}
		out += string(m.Author.External.Channel.ID) + "/" + m.Author.External.ID
	}
	return out + "]"
}
