package ingress_test

import (
	"context"
	"fmt"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/ingress"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// BenchmarkIngestCommit measures one verified inbound message from
// Sink.Accept to its committed unit on the memory store: deduplication,
// identity and conversation selection, the message record and its events.
// first_contact gives every message a new sender, so each one also creates
// a contact, an identity and a conversation; existing_conversation sends
// every message from one sender into one locked conversation.
//
// The memory store copies a table on its first write in a unit, so the cost
// per message grows with the records already stored: compare runs at the
// same -benchtime=Nx.
func BenchmarkIngestCommit(b *testing.B) {
	for _, tc := range []struct {
		name      string
		newSender bool
	}{
		{"first_contact", true},
		{"existing_conversation", false},
	} {
		b.Run(tc.name, func(b *testing.B) {
			snk, ch := ingestBench(b)
			ctx := b.Context()
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				from := "u-0"
				if tc.newSender {
					from = fmt.Sprintf("u-%d", i)
				}
				if _, err := snk.Accept(ctx, benchIngress(ch, i, from)); err != nil {
					b.Fatalf("Accept %d: %v", i, err)
				}
			}
		})
	}
}

// ingestBench composes an engine over a memory store with one receive-only
// channel and a per-contact inbox on it, and returns the service sink.
func ingestBench(b *testing.B) (*ingress.Sink, model.ChannelRef) {
	b.Helper()
	ctx := b.Context()
	clock := portstest.NewFixedClock(t0)
	st := memory.New(memory.Options{Clock: clock})
	e := enginetest.MustNew(b, engine.Config{
		Store:      st,
		Authorizer: &enginetest.Allow{},
		Adapters:   []adapter.Factory{ingressAdapter{Typ: chatType}},
		Clock:      clock,
		IDs:        portstest.NewSequenceIDs(1),
	})
	b.Cleanup(func() { _ = e.Close(context.Background()) })
	ch := model.Channel{
		Ref: model.ChannelRef{Tenant: tenantA, ID: "0000000b-0000-7000-8000-000000000001"}, Type: chatType,
		Configuration: model.Extension{Name: string(chatType) + ".ingress-test", Version: 1}, Revision: "r1",
	}
	if err := st.Transact(ctx, tenantA, func(tx store.Tx) error { return tx.PutChannel(ctx, ch, "") }); err != nil {
		b.Fatal(err)
	}
	cl, err := enginetest.For(e, enginetest.PrincipalA())
	if err != nil {
		b.Fatal(err)
	}
	if _, err := cl.Configuration().ConfigureInbox(ctx, model.ConfigureInbox{
		Command: model.Command{Call: model.Call{Correlation: "bench"}, Operation: "bench-inbox"},
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: tenantA}, Channel: ch.Ref, Name: "bench",
			Destinations: []string{dest}, Selection: perContact(true),
		},
	}); err != nil {
		b.Fatal(err)
	}
	snk, err := ingress.Bind(ctx, e, model.Principal{Tenant: tenantA, Actor: serviceAct}, ch.Ref)
	if err != nil {
		b.Fatal(err)
	}
	return snk, ch.Ref
}

// benchIngress is the i-th inbound text message, from sender from.
func benchIngress(ch model.ChannelRef, i int, from string) model.Ingress {
	id := fmt.Sprintf("e-%d", i)
	return model.Ingress{
		Call: model.Call{Correlation: model.CorrelationID("c-" + id)},
		Envelope: model.Envelope{
			EventID: id, MessageID: "m-" + id, Destination: dest, Kind: model.EnvelopeMessage,
			Sender:    model.ExternalRef{Channel: ch, Namespace: ingressNS, Kind: "user", ID: from},
			Assurance: "verified",
			Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "benchmark message " + id}}},
			Audience:  model.Audience{Visibility: model.Public}, OccurredAt: t0,
		},
	}
}
