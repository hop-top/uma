package events_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/events"
	"hop.top/uma/internal/ingress"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// BenchmarkEventDelivery measures how soon a subscriber sees a committed
// message.received event on the memory store, through Events.Poll and Ack,
// with the system clock and the engine's default waiter.
//
// ready commits one inbound message, then polls without waiting and
// acknowledges; poll-µs is the Poll and Ack part. wakeup parks a long poll
// first and commits the message while it waits, at an offset spread evenly
// over one second across iterations; delivery-ms is the time from the
// committed Accept to Poll returning the event, reported as p50, p95 and
// max over the run. A waiting Poll rescans the log once per wait step, so
// wakeup latency follows the step, not the commit.
func BenchmarkEventDelivery(b *testing.B) {
	b.Run("ready", func(b *testing.B) {
		eb := eventBench(b)
		var polling time.Duration
		b.ReportAllocs()
		b.ResetTimer()
		for i := range b.N {
			eb.ingest(b, i)
			start := time.Now()
			eb.pollAck(b, i, 0)
			polling += time.Since(start)
		}
		b.StopTimer()
		b.ReportMetric(float64(polling.Microseconds())/float64(b.N), "poll-µs")
	})
	b.Run("wakeup", func(b *testing.B) {
		eb := eventBench(b)
		lat := make([]time.Duration, 0, b.N)
		b.ResetTimer()
		for i := range b.N {
			got := make(chan time.Time, 1)
			go func() {
				eb.pollAck(b, i, 5*time.Second)
				got <- time.Now()
			}()
			// Golden-ratio offsets cover [0, 1s) evenly for any b.N.
			time.Sleep(time.Duration(float64(time.Second) * frac(float64(i)*0.6180339887)))
			eb.ingest(b, i)
			committed := time.Now()
			lat = append(lat, (<-got).Sub(committed))
		}
		b.StopTimer()
		reportLatency(b, lat)
	})
}

func frac(x float64) float64 { return x - float64(int64(x)) }

// reportLatency reports the p50, p95 and max of lat in milliseconds.
func reportLatency(b *testing.B, lat []time.Duration) {
	slices.Sort(lat)
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	b.ReportMetric(ms(lat[len(lat)/2]), "p50-delivery-ms")
	b.ReportMetric(ms(lat[(len(lat)*95)/100]), "p95-delivery-ms")
	b.ReportMetric(ms(lat[len(lat)-1]), "max-delivery-ms")
}

type eventRig struct {
	snk *ingress.Sink
	ev  events.Service
	sub model.SubscriptionRef
	ch  model.ChannelRef
}

// eventBench composes an engine over a memory store with one receive-only
// channel and inbox, and a subscriber opened at the tail of the log.
func eventBench(b *testing.B) *eventRig {
	b.Helper()
	ctx := b.Context()
	st := memory.New(memory.Options{})
	e := enginetest.MustNew(b, engine.Config{
		Store: st, Authorizer: &enginetest.Allow{},
		Adapters: []adapter.Factory{enginetest.IngressAdapter{Typ: enginetest.ChatType}},
		Clock:    systemClock{}, IDs: portstest.NewSequenceIDs(1),
	})
	b.Cleanup(func() { _ = e.Close(context.Background()) })
	ch := model.Channel{
		Ref: model.ChannelRef{Tenant: enginetest.TenantA, ID: "0000000b-0000-7000-8000-000000000001"}, Type: enginetest.ChatType,
		Configuration: model.Extension{Name: string(enginetest.ChatType) + ".ingress-test", Version: 1}, Revision: "r1",
	}
	if err := st.Transact(ctx, enginetest.TenantA, func(tx store.Tx) error { return tx.PutChannel(ctx, ch, "") }); err != nil {
		b.Fatal(err)
	}
	admin, err := enginetest.For(e, enginetest.PrincipalA())
	if err != nil {
		b.Fatal(err)
	}
	if _, err := admin.Configuration().ConfigureInbox(ctx, model.ConfigureInbox{
		Command: model.Command{Call: model.Call{Correlation: "bench"}, Operation: "bench-inbox"},
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: enginetest.TenantA}, Channel: ch.Ref, Name: "bench",
			Destinations: []string{enginetest.Dest}, Selection: enginetest.PerContact(true),
		},
	}); err != nil {
		b.Fatal(err)
	}
	snk, err := ingress.Bind(ctx, e, model.Principal{Tenant: enginetest.TenantA, Actor: enginetest.ServiceAct}, ch.Ref)
	if err != nil {
		b.Fatal(err)
	}
	c, err := e.For(model.Principal{Tenant: enginetest.TenantA, Actor: "bench-subscriber"})
	if err != nil {
		b.Fatal(err)
	}
	ev := events.New(c)
	opened, err := ev.Open(ctx, model.OpenSubscription{
		Command: model.Command{Call: model.Call{Correlation: "bench"}, Operation: "bench-open"}, Consumer: "bench",
		Filter: model.Filter{Kinds: []string{model.EventMessageReceived}},
		Start:  model.StartTail, PayloadMajor: model.PayloadVersion1,
	})
	if err != nil {
		b.Fatal(err)
	}
	return &eventRig{snk: snk, ev: ev, sub: opened.Subscription.Ref, ch: ch.Ref}
}

func (r *eventRig) ingest(b *testing.B, i int) {
	id := fmt.Sprintf("e-%d", i)
	if _, err := r.snk.Accept(b.Context(), model.Ingress{
		Call: model.Call{Correlation: model.CorrelationID("c-" + id)},
		Envelope: model.Envelope{
			EventID: id, MessageID: "m-" + id, Destination: enginetest.Dest, Kind: model.EnvelopeMessage,
			Sender:    model.ExternalRef{Channel: r.ch, Namespace: enginetest.IngressNS, Kind: "user", ID: "u-0"},
			Assurance: "verified",
			Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "benchmark message " + id}}},
			Audience:  model.Audience{Visibility: model.Public}, OccurredAt: enginetest.T0,
		},
	}); err != nil {
		b.Errorf("Accept %d: %v", i, err)
	}
}

// pollAck polls until the i-th message arrives within wait, then
// acknowledges it. It reports failures with Errorf: it also runs off the
// benchmark goroutine.
func (r *eventRig) pollAck(b *testing.B, i int, wait time.Duration) {
	ctx := b.Context()
	batch, err := r.ev.Poll(ctx, model.Poll{Call: model.Call{Correlation: "bench-poll"}, Subscription: r.sub, Wait: wait})
	if err != nil || len(batch.Events) != 1 {
		b.Errorf("Poll %d = %d events, %v; want the message", i, len(batch.Events), err)
		return
	}
	op := model.OperationID(fmt.Sprintf("ack-%d", i))
	if _, err := r.ev.Ack(ctx, model.Acknowledge{
		Command: model.Command{Call: model.Call{Correlation: "bench-ack"}, Operation: op}, Subscription: r.sub, Token: batch.AckToken,
	}); err != nil {
		b.Errorf("Ack %d: %v", i, err)
	}
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
