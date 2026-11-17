package relay_test

import (
	"fmt"
	"net/http"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
)

// A start names its recipient before any inbound message does, and core
// keys the recipient's identity on the bytes it is given. These tests drive
// the engine to show that the factory's normalization (the same lowercasing
// inbound applies) puts a start and the replies it provokes on one identity.

func onMedium(medium string) func(*relay.Config) {
	return func(c *relay.Config) { c.Medium = medium }
}

// startTo queues a first message to id in namespace under operation op.
func (r *engineRig) startTo(op model.OperationID, namespace, id string) (model.Acceptance, error) {
	r.t.Helper()
	return r.client.Conversations().Start(r.t.Context(), model.StartConversation{
		Command:   model.Command{Call: model.Call{Correlation: "relay-recipient"}, Operation: op},
		Inbox:     engineInbox,
		Recipient: model.ExternalRef{Channel: testChannel, Namespace: namespace, Kind: relay.RouteKind, ID: id},
		Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Your appointment is tomorrow."}}},
	})
}

// inboundFrom posts a message from id to the inbox's line.
func (r *engineRig) inboundFrom(id, messageID string) {
	r.t.Helper()
	body := fmt.Sprintf(`{"from":%q,"to":%q,"body":"ok","message_id":%q}`, id, gatewayNumber, messageID)
	if rec := r.post(body); rec.Code != http.StatusOK {
		r.t.Fatalf("webhook answered %d for %s", rec.Code, body)
	}
}

func (r *engineRig) identities() []model.Identity {
	r.t.Helper()
	p, err := r.client.Contacts().Identities(r.t.Context(), model.ListRequest{
		Call: model.Call{Correlation: "relay-identities"}, Filter: model.Filter{Inboxes: []model.InboxRef{engineInbox}}, Page: model.PageRequest{Limit: 50},
	})
	if err != nil {
		r.t.Fatalf("Identities: %v", err)
	}
	return p.Items
}

func (r *engineRig) conversations() []model.Conversation {
	r.t.Helper()
	p, err := r.client.Conversations().List(r.t.Context(), model.ListRequest{
		Call: model.Call{Correlation: "relay-conversations"}, Filter: model.Filter{Inboxes: []model.InboxRef{engineInbox}}, Page: model.PageRequest{Limit: 50},
	})
	if err != nil {
		r.t.Fatalf("Conversations: %v", err)
	}
	return p.Items
}

func wantIdentity(t *testing.T, ids []model.Identity, namespace string, sources ...string) {
	t.Helper()
	got := map[string]bool{}
	for _, i := range ids {
		if i.Namespace != namespace {
			t.Fatalf("identity %s in namespace %q, want %q", i.SourceID, i.Namespace, namespace)
		}
		got[i.SourceID] = true
	}
	if len(ids) != len(sources) {
		t.Fatalf("identities %v, want %v", got, sources)
	}
	for _, s := range sources {
		if !got[s] {
			t.Fatalf("identities %v, want %v", got, sources)
		}
	}
}

// A start to a mixed-case Apple ID address claims the lowercased identity,
// and an inbound from that address, in any case, lands in the same
// conversation and identity.
func TestStartToMixedCaseEmailMeetsTheInboundIdentity(t *testing.T) {
	r := newEngineRigWith(t, newLossyGateway(t, true), false, onMedium(relay.MediumIMessage))
	if _, err := r.startTo("start-1", relay.NamespaceIMessage, "Ann.Lee@Example.COM"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	wantIdentity(t, r.identities(), relay.NamespaceIMessage, "ann.lee@example.com")
	convs := r.conversations()
	if len(convs) != 1 || convs[0].ReplyRoute.ID != "ann.lee@example.com" {
		t.Fatalf("conversations %+v, want one routed to the lowercased address", convs)
	}
	r.inboundFrom("ann.lee@example.com", "m-1")
	r.inboundFrom("ANN.LEE@example.com", "m-2")
	wantIdentity(t, r.identities(), relay.NamespaceIMessage, "ann.lee@example.com")
	if got := r.conversations(); len(got) != 1 || got[0].Ref != convs[0].Ref {
		t.Fatalf("conversations %+v after the replies, want the start's one", got)
	}
}

// The reply the start queues goes to the address as the identity holds it.
func TestStartToMixedCaseEmailSendsToTheNormalizedRoute(t *testing.T) {
	r := newEngineRigWith(t, newLossyGateway(t, true), false, onMedium(relay.MediumIMessage))
	acc, err := r.startTo("start-1", relay.NamespaceIMessage, "Ann@Example.COM")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.step()
	if posts, _ := r.gw.counts(); posts != 1 {
		t.Fatalf("callbacks = %d, want the start's one", posts)
	}
	wantState(t, r.op(acc.Operation.Ref), model.Unknown)
	if to := r.gw.lastTo(); to != "ann@example.com" {
		t.Fatalf("callback to %q, want the lowercased address", to)
	}
}

// Replaying a start under its operation key with the recipient in another
// case is the same operation: replayed, never a conflict, nothing second
// recorded.
func TestStartReplayInAnotherCaseIsTheSameOperation(t *testing.T) {
	r := newEngineRigWith(t, newLossyGateway(t, true), false, onMedium(relay.MediumIMessage))
	first, err := r.startTo("start-1", relay.NamespaceIMessage, "Ann@Example.COM")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, id := range []string{"ann@example.com", "ANN@EXAMPLE.COM", "Ann@Example.COM"} {
		again, err := r.startTo("start-1", relay.NamespaceIMessage, id)
		if err != nil {
			t.Fatalf("replay as %q: %v", id, err)
		}
		if !again.Replayed || again.Operation.Ref != first.Operation.Ref {
			t.Fatalf("replay as %q = %+v, want the first operation replayed", id, again)
		}
	}
	wantIdentity(t, r.identities(), relay.NamespaceIMessage, "ann@example.com")
	if n := len(r.conversations()); n != 1 {
		t.Fatalf("conversations = %d, want 1", n)
	}
	r.step()
	if posts, _ := r.gw.counts(); posts != 1 {
		t.Fatalf("callbacks = %d, want one for the one operation", posts)
	}
}

// Phone numbers and the other mediums' addresses are kept exactly as sent:
// only an iMessage address holding "@" is lowercased.
func TestStartKeepsEverythingElseAsSent(t *testing.T) {
	for _, tt := range []struct {
		name, medium, namespace, id string
	}{
		{"imessage number", relay.MediumIMessage, relay.NamespaceIMessage, "+15550100001"},
		{"imessage number with letters", relay.MediumIMessage, relay.NamespaceIMessage, "TEL-5550100001"},
		{"sms", relay.MediumSMS, relay.NamespaceSMS, "+15550100001"},
		{"sms with an at sign", relay.MediumSMS, relay.NamespaceSMS, "Ann@Example.COM"},
		{"whatsapp", relay.MediumWhatsApp, relay.NamespaceWhatsApp, "+15550100001"},
		{"whatsapp with an at sign", relay.MediumWhatsApp, relay.NamespaceWhatsApp, "Ann@Example.COM"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newEngineRigWith(t, newLossyGateway(t, true), false, onMedium(tt.medium))
			if _, err := r.startTo("start-1", tt.namespace, tt.id); err != nil {
				t.Fatalf("Start: %v", err)
			}
			wantIdentity(t, r.identities(), tt.namespace, tt.id)
			if convs := r.conversations(); len(convs) != 1 || convs[0].ReplyRoute.ID != tt.id {
				t.Fatalf("conversations %+v, want one routed to %q", convs, tt.id)
			}
		})
	}
}

// NormalizeRecipient changes the ID of an iMessage email address and
// nothing else, and is idempotent.
func TestFactoryNormalizeRecipient(t *testing.T) {
	var f adapter.Factory = relay.NewFactory(relay.Options{})
	n, ok := f.(adapter.RecipientNormalizer)
	if !ok {
		t.Fatal("the relay factory does not implement adapter.RecipientNormalizer")
	}
	ref := func(ns, id string) model.ExternalRef {
		return model.ExternalRef{Channel: testChannel, Namespace: ns, Kind: relay.RouteKind, ID: id, ParentID: "p"}
	}
	for _, tt := range []struct {
		name string
		in   model.ExternalRef
		want string
	}{
		{"mixed case email", ref(relay.NamespaceIMessage, "Ann@Example.COM"), "ann@example.com"},
		{"lowercase email", ref(relay.NamespaceIMessage, "ann@example.com"), "ann@example.com"},
		{"ascii only", ref(relay.NamespaceIMessage, "ÉA@Ex.com"), "Éa@ex.com"},
		{"number", ref(relay.NamespaceIMessage, "+15550100001"), "+15550100001"},
		{"no at sign", ref(relay.NamespaceIMessage, "ABC"), "ABC"},
		{"sms", ref(relay.NamespaceSMS, "Ann@Example.COM"), "Ann@Example.COM"},
		{"whatsapp", ref(relay.NamespaceWhatsApp, "Ann@Example.COM"), "Ann@Example.COM"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := n.NormalizeRecipient(tt.in)
			if err != nil {
				t.Fatalf("NormalizeRecipient: %v", err)
			}
			want := tt.in
			want.ID = tt.want
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
			again, err := n.NormalizeRecipient(got)
			if err != nil || again != got {
				t.Fatalf("second pass %+v, %v; want %+v", again, err, got)
			}
		})
	}
}
