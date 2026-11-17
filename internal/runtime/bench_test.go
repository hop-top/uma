package runtime_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// BenchmarkReplyDispatch measures Messages.Reply on the memory store.
//
// accept is the reply command alone: authorization, the operation record,
// its durable intent and queued work, committed. dispatch is one reply from
// the command to an idle worker again: the reply is accepted, a Runtime.Run
// worker woken by the bench claims it, calls the provider and records the
// provider's answer, then finds no further work. send-µs is the part from
// the Reply call to the provider call.
func BenchmarkReplyDispatch(b *testing.B) {
	b.Run("accept", func(b *testing.B) {
		rb := replyBench(b)
		b.ReportAllocs()
		b.ResetTimer()
		for i := range b.N {
			rb.reply(b, i)
		}
	})
	b.Run("dispatch", func(b *testing.B) {
		rb := replyBench(b)
		ctx, stop := context.WithCancel(b.Context())
		ran := make(chan error, 1)
		go func() {
			ran <- rb.rt.Run(ctx, runtime.Call{Principal: rb.service, Call: model.Call{Correlation: "bench-run"}},
				runtime.WorkerOptions{Owner: "bench-worker", Lease: time.Minute})
		}()
		rb.waiter.awaitIdle(b, ran)
		var toSend time.Duration
		b.ReportAllocs()
		b.ResetTimer()
		for i := range b.N {
			start := time.Now()
			rb.reply(b, i)
			rb.waiter.kick()
			select {
			case at := <-rb.sender.sent:
				toSend += at.Sub(start)
			case err := <-ran:
				b.Fatalf("Run returned %v before the provider call", err)
			}
			rb.waiter.awaitIdle(b, ran)
		}
		b.StopTimer()
		b.ReportMetric(float64(toSend.Microseconds())/float64(b.N), "send-µs")
		stop()
		if err := <-ran; err != nil {
			b.Logf("Run: %v", err)
		}
	})
}

type replyRig struct {
	cl      enginetest.Client
	rt      *runtime.Runtime
	conv    model.ConversationRef
	sender  *benchSender
	waiter  *kickWaiter
	service model.Principal
}

// replyBench composes an engine over a memory store with one send-capable
// channel, an inbox and a conversation whose reply route is on it.
func replyBench(b *testing.B) *replyRig {
	b.Helper()
	clock := portstest.NewFixedClock(time.Unix(enginetest.TestEpochS, 0))
	st := memory.New(memory.Options{Clock: clock})
	snd := &benchSender{sent: make(chan time.Time, 1)}
	w := newKickWaiter()
	e := enginetest.MustNew(b, engine.Config{
		Store: st, Authorizer: &enginetest.Allow{}, Adapters: []adapter.Factory{snd},
		Clock: clock, IDs: portstest.NewSequenceIDs(7), Waiter: w,
	})
	b.Cleanup(func() { _ = e.Close(context.Background()) })
	conv := seedReplyRoute(b, st, clock.Now())
	cl, err := enginetest.For(e, enginetest.PrincipalA())
	if err != nil {
		b.Fatal(err)
	}
	return &replyRig{
		cl: cl, rt: runtime.New(e), conv: conv, sender: snd, waiter: w,
		service: model.Principal{Tenant: tenantA, Actor: "bench-service"},
	}
}

func (r *replyRig) reply(b *testing.B, i int) {
	op := model.OperationID(fmt.Sprintf("op-%d", i))
	if _, err := r.cl.Messages().Reply(b.Context(), model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: model.CorrelationID(op)}, Operation: op},
		Conversation: r.conv,
		Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "benchmark reply"}}},
	}); err != nil {
		b.Fatalf("Reply %d: %v", i, err)
	}
}

const benchSendType model.TypeKey = "com.example.bench.send"

// seedReplyRoute stores a channel of the bench sender, an inbox and a
// conversation routed to it.
func seedReplyRoute(b *testing.B, st store.Store, now time.Time) model.ConversationRef {
	b.Helper()
	ctx := b.Context()
	ch := model.ChannelRef{Tenant: tenantA, ID: "0000000b-0000-7000-8000-000000000001"}
	in := model.InboxRef{Tenant: tenantA, ID: "0000000b-0000-7000-8000-000000000002"}
	conv := model.ConversationRef{Inbox: in, ID: "0000000b-0000-7000-8000-000000000003"}
	err := st.Transact(ctx, tenantA, func(tx store.Tx) error {
		if err := tx.PutChannel(ctx, model.Channel{
			Ref: ch, Type: benchSendType, Configuration: model.Extension{Name: string(benchSendType) + ".config", Version: 1}, Revision: "ch1",
		}, ""); err != nil {
			return err
		}
		if err := tx.PutInbox(ctx, model.Inbox{Ref: in, Channel: ch, Name: "bench", Revision: "in1"}, ""); err != nil {
			return err
		}
		return tx.PutConversation(ctx, model.Conversation{
			Ref: conv, Mode: model.ModePerRoom, Status: model.ConversationOpen,
			ReplyRoute: model.ExternalRef{Channel: ch, Namespace: string(benchSendType), Kind: "room", ID: "room-1"},
			CreatedAt:  now, Revision: "cv1",
		}, "")
	})
	if err != nil {
		b.Fatalf("seed: %v", err)
	}
	return conv
}

// benchSender accepts every send and reports when each reached it.
type benchSender struct{ sent chan time.Time }

func (*benchSender) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: benchSendType, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: string(benchSendType) + ".config", Version: 1},
		Capabilities:        []model.Capability{model.CapabilitySend},
		Events:              []string{model.EventDeliveryUpdated},
	}
}

func (*benchSender) Validate(model.Extension) []model.ValidationIssue { return nil }

func (s *benchSender) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	return s, nil
}

func (*benchSender) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return []model.CapabilityStatus{{Name: model.CapabilitySend, Available: true}}, nil
}

func (*benchSender) Close(context.Context) error { return nil }

func (s *benchSender) Send(_ context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	select {
	case s.sent <- time.Now():
	default:
	}
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{{
		Channel: r.Route.Channel, Namespace: model.ProviderMessageNamespace, Kind: model.RefKindMessage, ID: "pm-" + string(r.Operation.ID),
	}}}, nil
}

// kickWaiter parks an idle worker until the bench kicks it, and reports
// each idle wait.
type kickWaiter struct {
	idle chan struct{}
	wake chan struct{}
}

func newKickWaiter() *kickWaiter {
	return &kickWaiter{idle: make(chan struct{}, 1), wake: make(chan struct{}, 1)}
}

func (w *kickWaiter) Wait(ctx context.Context, _ time.Duration) error {
	select {
	case w.idle <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.wake:
		return nil
	}
}

func (w *kickWaiter) kick() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *kickWaiter) awaitIdle(b *testing.B, ran <-chan error) {
	b.Helper()
	select {
	case <-w.idle:
	case err := <-ran:
		b.Fatalf("Run returned %v, want it idle", err)
	case <-time.After(30 * time.Second):
		b.Fatal("worker never went idle")
	}
}
