package relay_test

import (
	"fmt"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Identities a start claimed on the address as typed, before the factory
// lowercased iMessage email addresses, are keyed on that spelling. A start
// typed exactly so reuses that identity by one exact read; the lookup is
// partial by design (see adapter.LegacyRecipientReuser). Inbound is
// unchanged: it still claims the lowercased identity.

// seedLegacy stores what such a start left: the identity claimed as typed,
// its provisional contact and a conversation routed to the typed address.
func (r *engineRig) seedLegacy(typed string, withConversation bool) (model.Identity, model.Conversation) {
	r.t.Helper()
	ctx := r.t.Context()
	n := len(r.identities()) + 1
	id := model.Identity{
		Ref:      model.IdentityRef{Inbox: engineInbox, ID: model.ID(fmt.Sprintf("00000009-0000-7000-8000-%012d", n))},
		Contact:  model.ContactRef{Tenant: testTenant, ID: model.ID(fmt.Sprintf("0000000a-0000-7000-8000-%012d", n))},
		SourceID: typed, Namespace: relay.NamespaceIMessage, Kind: model.ContactKindPerson,
		LinkAssurance: model.LinkAssuranceNone, Revision: "r1",
	}
	owner := id.Ref
	conv := model.Conversation{
		Ref:  model.ConversationRef{Inbox: engineInbox, ID: model.ID(fmt.Sprintf("0000000b-0000-7000-8000-%012d", n))},
		Mode: model.ModePerContact, Status: model.ConversationOpen, Owner: &owner,
		ReplyRoute: model.ExternalRef{Channel: testChannel, Namespace: relay.NamespaceIMessage, Kind: relay.RouteKind, ID: typed},
		CreatedAt:  testNow, Revision: "c1",
		Participants: []model.Participant{{Identity: id.Ref, Role: "contact"}},
	}
	err := r.st.Transact(ctx, testTenant, func(tx store.Tx) error {
		key := model.IdentityKey{Inbox: engineInbox, Namespace: id.Namespace, SourceID: typed}
		if _, _, err := tx.ClaimIdentity(ctx, key, id); err != nil {
			return err
		}
		if err := tx.PutContact(ctx, model.Contact{Ref: id.Contact, Kind: model.ContactKindPerson, Revision: "r1"}, ""); err != nil {
			return err
		}
		if !withConversation {
			return nil
		}
		_, _, err := tx.ClaimConversation(ctx, store.ConversationClaim{
			Selection: store.SelectionKey{Inbox: engineInbox, Mode: model.ModePerContact, Key: "identity:" + string(id.Ref.ID)},
			CreateKey: "legacy", Conversation: conv,
		})
		return err
	})
	if err != nil {
		r.t.Fatalf("seed legacy identity: %v", err)
	}
	return id, conv
}

// A start typed as the legacy identity was claimed lands on it and its
// conversation, and the callback goes to the route the conversation stores:
// the typed spelling, as every send on it did before.
func TestStartTypedAsALegacyIdentityReusesItAndItsConversation(t *testing.T) {
	r := newEngineRigWith(t, newLossyGateway(t, true), false, onMedium(relay.MediumIMessage))
	legacy, conv := r.seedLegacy("Ann@Example.COM", true)
	acc, err := r.startTo("start-1", relay.NamespaceIMessage, "Ann@Example.COM")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := acc.Operation.ResultMessage; got == nil || got.Conversation != conv.Ref {
		t.Fatalf("start result %+v, want a message in the legacy conversation %v", got, conv.Ref)
	}
	if ids := r.identities(); len(ids) != 1 || ids[0].Ref != legacy.Ref {
		t.Fatalf("identities %+v, want only the legacy one", ids)
	}
	if convs := r.conversations(); len(convs) != 1 || convs[0].Ref != conv.Ref {
		t.Fatalf("conversations %+v, want only the legacy one", convs)
	}
	r.step()
	if posts, _ := r.gw.counts(); posts != 1 {
		t.Fatalf("callbacks = %d, want the start's one", posts)
	}
	if to := r.gw.lastTo(); to != "Ann@Example.COM" {
		t.Fatalf("callback to %q, want the route the legacy conversation stores", to)
	}
}

// A legacy identity with no conversation to join gets a new one on the
// lowercased address, which is where the callback goes.
func TestStartTypedAsALegacyIdentityWithoutConversationRoutesToTheNormalizedAddress(t *testing.T) {
	r := newEngineRigWith(t, newLossyGateway(t, true), false, onMedium(relay.MediumIMessage))
	legacy, _ := r.seedLegacy("Ann@Example.COM", false)
	if _, err := r.startTo("start-1", relay.NamespaceIMessage, "Ann@Example.COM"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if ids := r.identities(); len(ids) != 1 || ids[0].Ref != legacy.Ref {
		t.Fatalf("identities %+v, want only the legacy one", ids)
	}
	convs := r.conversations()
	if len(convs) != 1 || convs[0].Owner == nil || *convs[0].Owner != legacy.Ref || convs[0].ReplyRoute.ID != "ann@example.com" {
		t.Fatalf("conversations %+v, want one owned by the legacy identity, routed to the lowercased address", convs)
	}
	r.step()
	if to := r.gw.lastTo(); to != "ann@example.com" {
		t.Fatalf("callback to %q, want the lowercased address", to)
	}
}

// The lookup is exact. Typed in the lowercase spelling, or in any other
// case than the legacy identity, a start claims the lowercased identity as it
// does without a legacy one. This is the known partial limit.
func TestStartInAnotherCaseThanALegacyIdentityIsFresh(t *testing.T) {
	for _, typed := range []string{"ann@example.com", "ANN@EXAMPLE.COM", "Ann@Example.com"} {
		t.Run(typed, func(t *testing.T) {
			r := newEngineRigWith(t, newLossyGateway(t, true), false, onMedium(relay.MediumIMessage))
			_, conv := r.seedLegacy("Ann@Example.COM", true)
			acc, err := r.startTo("start-1", relay.NamespaceIMessage, typed)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if got := acc.Operation.ResultMessage; got == nil || got.Conversation == conv.Ref {
				t.Fatalf("start result %+v, want a message outside the legacy conversation", got)
			}
			wantIdentity(t, r.identities(), relay.NamespaceIMessage, "Ann@Example.COM", "ann@example.com")
			r.step()
			if to := r.gw.lastTo(); to != "ann@example.com" {
				t.Fatalf("callback to %q, want the lowercased address", to)
			}
		})
	}
}

// With an identity under the lowercased spelling too, that one wins.
func TestStartPrefersTheLowercasedIdentityOverALegacyOne(t *testing.T) {
	r := newEngineRigWith(t, newLossyGateway(t, true), false, onMedium(relay.MediumIMessage))
	_, legacyConv := r.seedLegacy("Ann@Example.COM", true)
	_, lowerConv := r.seedLegacy("ann@example.com", true)
	acc, err := r.startTo("start-1", relay.NamespaceIMessage, "Ann@Example.COM")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := acc.Operation.ResultMessage; got == nil || got.Conversation != lowerConv.Ref {
		t.Fatalf("start result %+v, want the lowercased identity's conversation %v (legacy %v)", got, lowerConv.Ref, legacyConv.Ref)
	}
	r.step()
	if to := r.gw.lastTo(); to != "ann@example.com" {
		t.Fatalf("callback to %q, want the lowercased address", to)
	}
}

// The same operation key under any spelling is the one operation.
func TestStartReplayOverALegacyIdentityIsTheSameOperation(t *testing.T) {
	r := newEngineRigWith(t, newLossyGateway(t, true), false, onMedium(relay.MediumIMessage))
	r.seedLegacy("Ann@Example.COM", true)
	first, err := r.startTo("start-1", relay.NamespaceIMessage, "Ann@Example.COM")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, id := range []string{"Ann@Example.COM", "ann@example.com", "ANN@EXAMPLE.COM"} {
		again, err := r.startTo("start-1", relay.NamespaceIMessage, id)
		if err != nil {
			t.Fatalf("replay as %q: %v", id, err)
		}
		if !again.Replayed || again.Operation.Ref != first.Operation.Ref {
			t.Fatalf("replay as %q = %+v, want the first operation replayed", id, again)
		}
	}
	r.step()
	if posts, _ := r.gw.counts(); posts != 1 {
		t.Fatalf("callbacks = %d, want one for the one operation", posts)
	}
}

// Inbound is unchanged: a reply from the address claims the lowercased
// identity, whatever case the relay reports, and never the legacy one.
func TestInboundKeepsClaimingTheLowercasedIdentityBesideALegacyOne(t *testing.T) {
	r := newEngineRigWith(t, newLossyGateway(t, true), false, onMedium(relay.MediumIMessage))
	r.seedLegacy("Ann@Example.COM", true)
	if _, err := r.startTo("start-1", relay.NamespaceIMessage, "Ann@Example.COM"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.inboundFrom("Ann@Example.COM", "m-1")
	r.inboundFrom("ann@example.com", "m-2")
	wantIdentity(t, r.identities(), relay.NamespaceIMessage, "Ann@Example.COM", "ann@example.com")
}

// Only an iMessage address asks for a legacy lookup: the factory's answer
// is pure and leaves the other mediums alone.
func TestFactoryReuseLegacyRecipient(t *testing.T) {
	var f adapter.Factory = relay.NewFactory(relay.Options{})
	u, ok := f.(adapter.LegacyRecipientReuser)
	if !ok {
		t.Fatal("the relay factory does not implement adapter.LegacyRecipientReuser")
	}
	ref := func(ns, id string) model.ExternalRef {
		return model.ExternalRef{Channel: testChannel, Namespace: ns, Kind: relay.RouteKind, ID: id}
	}
	for _, tt := range []struct {
		name string
		in   model.ExternalRef
		want bool
	}{
		{"imessage email", ref(relay.NamespaceIMessage, "Ann@Example.COM"), true},
		{"sms", ref(relay.NamespaceSMS, "Ann@Example.COM"), false},
		{"whatsapp", ref(relay.NamespaceWhatsApp, "Ann@Example.COM"), false},
	} {
		if got := u.ReuseLegacyRecipient(tt.in); got != tt.want {
			t.Errorf("%s: ReuseLegacyRecipient = %v, want %v", tt.name, got, tt.want)
		}
	}
}
