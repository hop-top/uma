package ingress_test

import (
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

const externalMail model.TypeKey = "org.example.mail"

// withAdapter registers a third-party receive adapter declaring ex.
func withAdapter(typ model.TypeKey, ex adapter.Exchange) func(*engine.Config) {
	return func(c *engine.Config) {
		c.Adapters = append(c.Adapters, ingressAdapter{Typ: typ, Exchange: ex})
	}
}

// A third-party adapter declaring email threading threads exactly like a
// maintained mail adapter: a reply joins the conversation of the inbound
// or outbound Message-ID it names.
func TestThirdPartyMailAdapterThreadsOnDeclaredSemantics(t *testing.T) {
	h := newHarness(t, externalMail, withAdapter(externalMail, enginetest.MailExchange))
	in := h.Configure(perContact(false), dest)
	mail := func(event, msgID, from string, hdr model.EmailHeaders) model.Envelope {
		n := hdr.Normalize(h.Channel.Ref)
		env := h.Inbound(event, msgID, from, dest)
		env.References = n.References
		return env
	}
	h.MustAccept(mail("e1", "ext-1", "alice", model.EmailHeaders{MessageID: "<a1@a.example>"}))
	convs := h.Conversations(in.Ref)
	if len(convs) != 1 {
		t.Fatalf("conversations = %d, want 1", len(convs))
	}
	conv := convs[0]
	h.MustAccept(mail("e2", "ext-2", "bob", model.EmailHeaders{MessageID: "<b1@b.example>", InReplyTo: "<a1@a.example>"}))
	if got := h.MessageByProviderID(in.Ref, "ext-2"); got.Ref.Conversation != conv.Ref {
		t.Fatalf("reply to an inbound Message-ID landed in %+v, want %+v", got.Ref.Conversation, conv.Ref)
	}

	sent := model.Message{
		Ref:      model.MessageRef{Conversation: conv.Ref, ID: h.ID()},
		Content:  model.Content{Parts: []model.Part{{Kind: "text", Text: "our answer"}}},
		Audience: model.Audience{Visibility: model.Public}, RecordedAt: t0, Revision: "r1",
		Origin: &model.Origin{Actor: "agent", Operation: model.OperationRef{Tenant: tenantA, ID: h.ID()}},
		External: []model.ExternalRef{
			model.ProviderMessageRef(h.Channel.Ref, "ext-out"),
			{Channel: h.Channel.Ref, Namespace: model.EmailHeaderNamespace, Kind: model.RefKindEmailMessageID, ID: "<out1@uma.example>"},
		},
	}
	h.Tx(func(tx store.Tx) error { return tx.PutMessage(h.Ctx, sent, "") })
	h.MustAccept(mail("e3", "ext-3", "carol", model.EmailHeaders{MessageID: "<c1@c.example>", InReplyTo: "<out1@uma.example>"}))
	if got := h.MessageByProviderID(in.Ref, "ext-3"); got.Ref.Conversation != conv.Ref {
		t.Fatalf("reply to an outbound Message-ID landed in %+v, want %+v", got.Ref.Conversation, conv.Ref)
	}
}

// Without a declared threading policy, headers select nothing: the same
// reply starts its own conversation, whatever the type key.
func TestUndeclaredThreadingIgnoresHeaders(t *testing.T) {
	h := newHarness(t, externalMail, withAdapter(externalMail, adapter.Exchange{MessageIDs: adapter.MessageIDsProvider}))
	in := h.Configure(perContact(false), dest)
	mail := func(event, msgID, from string, hdr model.EmailHeaders) model.Envelope {
		env := h.Inbound(event, msgID, from, dest)
		env.References = hdr.Normalize(h.Channel.Ref).References
		return env
	}
	h.MustAccept(mail("e1", "ext-1", "alice", model.EmailHeaders{MessageID: "<a1@a.example>"}))
	h.MustAccept(mail("e2", "ext-2", "bob", model.EmailHeaders{MessageID: "<b1@b.example>", InReplyTo: "<a1@a.example>"}))
	if n := len(h.Conversations(in.Ref)); n != 2 {
		t.Fatalf("conversations = %d, want 2: headers threaded without a declared policy", n)
	}
}

// An inbound message whose provider ID equals the one a uma-authored
// message holds is that message's echo only when the adapter declares
// provider-assigned IDs. A sender-chosen or undeclared ID proves nothing:
// the message is recorded, whatever the type key.
func TestEchoByProviderIDRequiresProviderAssignedIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  model.TypeKey
		ex   adapter.Exchange
		echo bool
	}{
		{"third-party provider-assigned", "org.example.chat", enginetest.ChatExchange, true},
		{"third-party sender-chosen", externalMail, enginetest.MailExchange, false},
		{"third-party undeclared", "org.example.other", adapter.Exchange{}, false},
		{"maintained mail", model.TypeSmtpMail, enginetest.MailExchange, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opts []enginetest.HarnessOption
			if tc.typ != model.TypeSmtpMail {
				opts = append(opts, withAdapter(tc.typ, tc.ex))
			}
			h := newHarness(t, tc.typ, opts...)
			in := h.Configure(perContact(false), dest)
			h.MustAccept(h.Inbound("e1", "first", "alice", dest))
			conv := h.Conversations(in.Ref)[0]
			sent := model.Message{
				Ref:      model.MessageRef{Conversation: conv.Ref, ID: h.ID()},
				Content:  model.Content{Parts: []model.Part{{Kind: "text", Text: "our answer"}}},
				Audience: model.Audience{Visibility: model.Public}, RecordedAt: t0, Revision: "r1",
				Origin:   &model.Origin{Actor: "agent", Operation: model.OperationRef{Tenant: tenantA, ID: h.ID()}},
				External: []model.ExternalRef{model.ProviderMessageRef(h.Channel.Ref, "<out1@uma.example>")},
			}
			h.Tx(func(tx store.Tx) error { return tx.PutMessage(h.Ctx, sent, "") })
			before := len(h.Messages(in.Ref))
			h.MustAccept(h.Inbound("e2", "<out1@uma.example>", "mallory", dest))
			recorded := len(h.Messages(in.Ref)) - before
			if tc.echo && recorded != 0 {
				t.Fatalf("provider-assigned echo recorded %d messages, want 0", recorded)
			}
			if !tc.echo && recorded != 1 {
				t.Fatalf("message reusing an outbound ID recorded %d messages, want 1 (not an echo)", recorded)
			}
		})
	}
}

// senderDesk is the desk adapter declaring sender-chosen message IDs.
type senderDesk struct{ deskFactory }

func (f senderDesk) Describe() adapter.Descriptor {
	d := f.deskFactory.Describe()
	d.Exchange = enginetest.MailExchange
	return d
}

// A message recorded before a send's answer reports the same ID is linked
// as the send's echo only when the adapter declares provider-assigned IDs;
// a sender-chosen ID leaves the earlier message as it is.
func TestEarlyCopyLinksAsEchoOnlyForProviderAssignedIDs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sender bool
	}{{"provider-assigned", false}, {"sender-chosen", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := &enginetest.OutFactory{}
			var fac adapter.Factory = deskFactory{f}
			if tc.sender {
				fac = senderDesk{deskFactory{f}}
			}
			h := newHarness(t, deskType, func(c *engine.Config) { c.Adapters = append(c.Adapters, fac) })
			d := &desk{harness: h, f: f, inbox: h.Configure(model.Selection{Mode: model.ModePerWorkItem}, dest)}
			d.MustAccept(d.comment("e-1", "c-1", "customer", "7"))
			conv := d.onlyConversation()
			d.MustAccept(d.comment("e-2", "<out@uma.example>", "someone", "7"))
			d.sentAs("<out@uma.example>")
			a := d.reply(conv.Ref, "op-1", "on it")
			d.deliver()
			if op := d.op(a.Operation.Ref); op.State != model.ProviderAccepted {
				t.Fatalf("reply state = %s", op.State)
			}
			early := d.MessageByProviderID(d.inbox.Ref, "<out@uma.example>")
			if linked := early.EchoOf != nil; linked == tc.sender {
				t.Fatalf("early message EchoOf = %v; linked %v, want %v", early.EchoOf, linked, !tc.sender)
			}
		})
	}
}
