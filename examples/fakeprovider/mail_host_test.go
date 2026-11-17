package fakeprovider_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/examples/fakeprovider"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// The mail variant end to end: an email adapter outside the maintained
// set, registered like any third-party adapter, gets uma's email
// threading from the semantics it declares, with no change to core. Its
// sender-chosen Message-IDs thread, but never make an echo.

const mailTenant model.TenantID = "mail-tenant"

var (
	mailChannel = model.ChannelRef{Tenant: mailTenant, ID: "00000000-0000-7000-8000-0000000003a0"}
	mailInbox   = model.InboxRef{Tenant: mailTenant, ID: "00000000-0000-7000-8000-0000000003a1"}
	mailService = model.Principal{Tenant: mailTenant, Actor: "mail-service"}
	mailAgent   = model.Principal{Tenant: mailTenant, Actor: "mail-agent"}
)

type mailHost struct {
	t       *testing.T
	ctx     context.Context
	engine  uma.Engine
	client  uma.Client
	store   *memory.Store
	factory *fakeprovider.Factory
	running []chan error
}

func newMailHost(t *testing.T) *mailHost {
	t.Helper()
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0))
	h := &mailHost{t: t, ctx: t.Context(), store: memory.New(memory.Options{Clock: clock}), factory: fakeprovider.NewMail()}
	engine, err := uma.New(uma.Config{
		Store: h.store,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{h.factory},
		Credentials: &credentialHost{},
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(13),
		Retry:       uma.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.engine = engine
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	lock := false
	err = h.store.Transact(h.ctx, mailTenant, func(tx store.Tx) error {
		ch := model.Channel{Ref: mailChannel, Type: fakeprovider.TypeMail, Configuration: h.factory.Config(), CredentialRef: "mail-cred", Revision: "ch1"}
		if err := tx.PutChannel(h.ctx, ch, ""); err != nil {
			return err
		}
		return tx.PutInbox(h.ctx, model.Inbox{
			Ref: mailInbox, Channel: mailChannel, Name: "support", Destinations: []string{"support"},
			Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
			Revision:  "in1",
		}, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.client, err = engine.For(mailAgent); err != nil {
		t.Fatal(err)
	}
	call := uma.RuntimeCall{Principal: mailService, Call: model.Call{Correlation: "mail-host"}}
	rx, wk := make(chan error, 1), make(chan error, 1)
	go func() { rx <- engine.Runtime().Receive(h.ctx, call, mailChannel) }()
	go func() {
		wk <- engine.Runtime().Run(h.ctx, call, uma.WorkerOptions{Owner: "mail-worker", Limit: 10, Poll: time.Millisecond})
	}()
	h.running = []chan error{rx, wk}
	t.Cleanup(func() {
		if err := engine.Runtime().Drain(context.Background(), call); err != nil {
			t.Error(err)
		}
		for _, done := range h.running {
			if err := <-done; err != nil {
				t.Logf("Run: %v", err)
			}
		}
	})
	return h
}

func (h *mailHost) messages() []model.Message {
	h.t.Helper()
	var out []model.Message
	err := h.store.View(h.ctx, mailTenant, func(r store.Reader) error {
		p, err := r.Messages(h.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{mailInbox}}, Page: model.PageRequest{Limit: 100}})
		out = p.Items
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

// receive delivers one email and waits until uma stored it.
func (h *mailHost) receive(event, from string, hdr model.EmailHeaders) model.Message {
	h.t.Helper()
	before := len(h.messages())
	h.factory.Script(mailChannel).Push(fakeprovider.MailIngress(mailChannel, fakeprovider.Email{
		EventID: event, Destination: "support", From: from, Headers: hdr, Text: "mail " + event,
		OccurredAt: time.Unix(1_700_000_000, 0),
	}))
	inEventually(h.t, "mail "+event+" stored", func() bool { return len(h.messages()) == before+1 })
	return h.byProviderID(model.ParseMsgIDs(hdr.MessageID)[0])
}

func (h *mailHost) byProviderID(id string) model.Message {
	h.t.Helper()
	ms := slices.DeleteFunc(h.messages(), func(m model.Message) bool {
		return !slices.Contains(m.External, model.ProviderMessageRef(mailChannel, id)) || m.Origin != nil
	})
	if len(ms) != 1 {
		h.t.Fatalf("inbound messages with ID %s: %d, want 1", id, len(ms))
	}
	return ms[0]
}

func TestMailVariantThreadsOnDeclaredSemanticsWithoutEchoTrust(t *testing.T) {
	h := newMailHost(t)
	first := h.receive("e1", "alice@a.example", model.EmailHeaders{MessageID: "<a1@a.example>"})
	conv := first.Ref.Conversation

	// A reply from another sender joins the thread it names.
	if got := h.receive("e2", "bob@b.example", model.EmailHeaders{MessageID: "<b1@b.example>", InReplyTo: "<a1@a.example>"}); got.Ref.Conversation != conv {
		t.Fatalf("reply to an inbound Message-ID landed in %+v, want %+v", got.Ref.Conversation, conv)
	}

	// uma answers; the provider reports the Message-ID it generated.
	acc, err := h.client.Messages().Reply(h.ctx, model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: "answer"}, Operation: "answer"},
		Conversation: conv,
		Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "our answer"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sent := "<m1@" + fakeprovider.MailDomain + ">"
	var out model.Message
	inEventually(t, "answer recorded with its Message-ID", func() bool {
		for _, m := range h.messages() {
			if m.Origin != nil && m.Origin.Operation == acc.Operation.Ref && slices.Contains(m.External, model.ProviderMessageRef(mailChannel, sent)) {
				out = m
				return true
			}
		}
		return false
	})

	// A reply to the answer threads on the outbound Message-ID.
	if got := h.receive("e3", "carol@c.example", model.EmailHeaders{MessageID: "<c1@c.example>", InReplyTo: sent}); got.Ref.Conversation != conv {
		t.Fatalf("reply to the outbound Message-ID landed in %+v, want %+v", got.Ref.Conversation, conv)
	}

	// Mail reusing the outbound Message-ID is somebody's mail, never the
	// answer's echo: it is stored, unlinked, and the answer is unchanged.
	forged := h.receive("e4", "mallory@m.example", model.EmailHeaders{MessageID: sent})
	if forged.EchoOf != nil || forged.Origin != nil {
		t.Fatalf("mail reusing an outbound Message-ID treated as an echo: %+v", forged)
	}
	for _, m := range h.messages() {
		if m.Ref == out.Ref && m.Revision != out.Revision {
			t.Fatalf("answer changed by mail reusing its Message-ID: %+v", m)
		}
	}
}
