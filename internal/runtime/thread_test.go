package runtime_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// putInbound stores a provider message in the rig's conversation with the
// given provider ID, threading header and payload.
func putInbound(t *testing.T, r *rig, id model.ID, providerID string, a model.Audience) model.Message {
	t.Helper()
	m := model.Message{
		Ref:     model.MessageRef{Conversation: r.Conv.Ref, ID: id},
		Author:  model.Author{Kind: model.ContactKindPerson},
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "question"}}},
		External: []model.ExternalRef{
			model.ProviderMessageRef(r.Ch.Ref, providerID),
			{Channel: r.Ch.Ref, Namespace: model.EmailHeaderNamespace, Kind: model.RefKindEmailMessageID, ID: "<q1@example.test>"},
		},
		Metadata:   []model.Extension{{Name: "com.example.chat.envelope", Version: 1, Data: []byte(`{"subject":"Order 7"}`)}},
		Audience:   a,
		OccurredAt: r.Clock.Now(), RecordedAt: r.Clock.Now(), Revision: "m1", Source: enginetest.OutKey,
	}
	if err := r.E.Store().Transact(r.Ctx, enginetest.TenantA, func(tx store.Tx) error { return tx.PutMessage(r.Ctx, m, "") }); err != nil {
		t.Fatal(err)
	}
	return m
}

// A reply naming the message it answers sends with that message as its
// target and thread: its provider references and payload, so a provider
// can thread it (In-Reply-To, a reply context, the subject).
func TestReplyCarriesItsThread(t *testing.T) {
	r := newRig(t)
	parent := putInbound(t, r, "00000009-0000-7000-8000-0000000000f1", "pm-parent", model.Audience{Visibility: model.Public})
	req := r.ReplyReq("op-1", "answer")
	req.InReplyTo = &parent.Ref
	if _, err := r.C.Messages().Reply(r.Ctx, req); err != nil {
		t.Fatal(err)
	}
	r.Step("w")
	sends := r.F.ProviderSends()
	if len(sends) != 1 {
		t.Fatalf("sends = %d, want 1", len(sends))
	}
	s := sends[0]
	if s.Target == nil || *s.Target != model.ProviderMessageRef(r.Ch.Ref, "pm-parent") {
		t.Fatalf("target %+v, want the parent's provider message", s.Target)
	}
	if s.Thread == nil || !slices.Equal(s.Thread.References, parent.External) || len(s.Thread.Metadata) != 1 || s.Thread.Metadata[0].Name != "com.example.chat.envelope" {
		t.Fatalf("thread %+v, want the parent's references and payload", s.Thread)
	}

	// A reply naming no parent answers the latest inbound message.
	plain := r.Reply("op-2", "no parent")
	r.Step("w")
	if s := r.F.ProviderSends()[1]; s.Target == nil || *s.Target != model.ProviderMessageRef(r.Ch.Ref, "pm-parent") ||
		s.Thread == nil || s.Operation != plain.Operation.Ref {
		t.Fatalf("send naming no parent: target %+v thread %+v, want the latest inbound", s.Target, s.Thread)
	}
}

// A reply in a conversation holding no inbound message sends without a
// thread.
func TestReplyWithoutInboundHasNoThread(t *testing.T) {
	r := newRig(t)
	plain := r.Reply("op-1", "no parent")
	r.Step("w")
	if s := r.F.ProviderSends()[0]; s.Target != nil || s.Thread != nil || s.Operation != plain.Operation.Ref {
		t.Fatalf("send without a parent: target %+v thread %+v", s.Target, s.Thread)
	}
}

// An applied send's email Message-ID joins its outbound message, next to
// its provider message ID, so a mail reply to it threads.
func TestSentMessageKeepsItsMessageID(t *testing.T) {
	r := newRig(t)
	msgID := model.ExternalRef{Channel: r.Ch.Ref, Namespace: model.EmailHeaderNamespace, Kind: model.RefKindEmailMessageID, ID: "<out1@uma.example>"}
	r.F.Respond(func(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
		return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{model.ProviderMessageRef(r.Ch.Ref, "pm-out"), msgID}}, nil, true
	})
	a := r.Reply("op-1", "answer")
	r.Step("w")
	var m model.Message
	if err := r.E.Store().View(r.Ctx, enginetest.TenantA, func(rd store.Reader) error {
		var err error
		m, err = rd.GetMessage(r.Ctx, *a.Operation.ResultMessage)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(m.External, msgID) || !slices.Contains(m.External, model.ProviderMessageRef(r.Ch.Ref, "pm-out")) {
		t.Fatalf("outbound message references %+v, want its provider message ID and Message-ID", m.External)
	}
}

// A provider-deleted message is never a reply's parent: named, it is
// answered without a thread; the default parent skips it for the latest
// message still standing.
func TestReplyNeverAnswersDeletedMessage(t *testing.T) {
	r := newRig(t)
	putInbound(t, r, "00000009-0000-7000-8000-0000000000f1", "pm-older", model.Audience{Visibility: model.Public})
	r.Clock.Advance(time.Minute)
	parent := putInbound(t, r, "00000009-0000-7000-8000-0000000000f2", "pm-deleted", model.Audience{Visibility: model.Public})
	parent.Deleted, parent.Content = true, model.Content{}
	prev := parent.Revision
	parent.Revision = "m2"
	if err := r.E.Store().Transact(r.Ctx, enginetest.TenantA, func(tx store.Tx) error { return tx.PutMessage(r.Ctx, parent, prev) }); err != nil {
		t.Fatal(err)
	}
	req := r.ReplyReq("op-1", "answer")
	req.InReplyTo = &parent.Ref
	if _, err := r.C.Messages().Reply(r.Ctx, req); err != nil {
		t.Fatal(err)
	}
	r.Step("w")
	r.Reply("op-2", "no parent")
	r.Step("w")
	sends := r.F.ProviderSends()
	if len(sends) != 2 {
		t.Fatalf("sends = %d, want 2", len(sends))
	}
	if s := sends[0]; s.Target != nil || s.Thread != nil {
		t.Fatalf("send naming the deleted message: target %+v thread %+v", s.Target, s.Thread)
	}
	if s := sends[1]; s.Target == nil || *s.Target != model.ProviderMessageRef(r.Ch.Ref, "pm-older") {
		t.Fatalf("send naming no parent: target %+v, want the latest message not deleted", s.Target)
	}
}
