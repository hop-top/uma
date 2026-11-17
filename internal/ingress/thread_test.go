package ingress_test

import (
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// A mail reply threads on a Message-ID however the stored message holds
// it: as its provider message ID, or as its email message_id reference,
// the form an outbound message records from its provider's answer and an
// adapter keying messages by another ID reports.
func TestEmailThreadsOnStoredMessageIDHeaders(t *testing.T) {
	h := newHarness(t, model.TypeSmtpMail)
	in := h.Configure(perContact(false), dest)
	mail := func(event, msgID, from string, hdr model.EmailHeaders) model.Envelope {
		n := hdr.Normalize(h.Channel.Ref)
		env := h.Inbound(event, msgID, from, dest)
		env.References = n.References
		return env
	}
	h.MustAccept(mail("e1", "graph-1", "alice", model.EmailHeaders{MessageID: "<a1@a.example>"}))
	convs := h.Conversations(in.Ref)
	if len(convs) != 1 {
		t.Fatalf("conversations = %d, want 1", len(convs))
	}
	conv := convs[0]
	h.MustAccept(mail("e2", "graph-2", "bob", model.EmailHeaders{MessageID: "<b1@b.example>", InReplyTo: "<a1@a.example>"}))
	if got := h.MessageByProviderID(in.Ref, "graph-2"); got.Ref.Conversation != conv.Ref {
		t.Fatalf("reply to an inbound Message-ID landed in %+v, want %+v", got.Ref.Conversation, conv.Ref)
	}

	sent := model.Message{
		Ref:      model.MessageRef{Conversation: conv.Ref, ID: h.ID()},
		Content:  model.Content{Parts: []model.Part{{Kind: "text", Text: "our answer"}}},
		Audience: model.Audience{Visibility: model.Public}, RecordedAt: t0, Revision: "r1",
		Origin: &model.Origin{Actor: "agent", Operation: model.OperationRef{Tenant: tenantA, ID: h.ID()}},
		External: []model.ExternalRef{
			model.ProviderMessageRef(h.Channel.Ref, "graph-out"),
			{Channel: h.Channel.Ref, Namespace: model.EmailHeaderNamespace, Kind: model.RefKindEmailMessageID, ID: "<out1@uma.example>"},
		},
	}
	h.Tx(func(tx store.Tx) error { return tx.PutMessage(h.Ctx, sent, "") })
	h.MustAccept(mail("e3", "graph-3", "carol", model.EmailHeaders{MessageID: "<c1@c.example>", InReplyTo: "<out1@uma.example>"}))
	if got := h.MessageByProviderID(in.Ref, "graph-3"); got.Ref.Conversation != conv.Ref {
		t.Fatalf("reply to an outbound Message-ID landed in %+v, want %+v", got.Ref.Conversation, conv.Ref)
	}
}
