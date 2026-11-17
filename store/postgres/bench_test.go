package postgres

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// Benchmarks against PostgreSQL in the shared test container, one migrated
// database per benchmark. Engine benchmarks compose uma.New over the store
// through the public API; BenchmarkClaimWorkContention drives the store
// directly. Like the tests, they skip without a healthy container runtime
// unless UMA_REQUIRE_DOCKER=1.

const benchTenant model.TenantID = "bench-tenant"

// benchDatabase is newDatabase for benchmarks.
func benchDatabase(b *testing.B) *Store {
	b.Helper()
	return benchStore(b, benchCreate(b))
}

// benchServer starts the shared test container, or skips b without a
// healthy container runtime.
func benchServer(b *testing.B) {
	b.Helper()
	if os.Getenv(requireDockerEnv) != "1" {
		provider, err := testcontainers.ProviderDocker.GetProvider()
		if err != nil || provider.Health(b.Context()) != nil {
			b.Skip("no healthy container runtime")
		}
	}
	server.once.Do(func() { server.err = startServer() })
	if server.err != nil {
		b.Fatalf("postgres test server: %v", server.err)
	}
}

// benchCreate creates a migrated database and returns its name. It is
// dropped when b ends.
func benchCreate(b *testing.B) string {
	b.Helper()
	benchServer(b)
	name := fmt.Sprintf("uma_bench_%d", server.next.Add(1))
	if _, err := server.admin.Exec(b.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+
		" TEMPLATE "+pgx.Identifier{server.template}.Sanitize()); err != nil {
		b.Fatalf("create database: %v", err)
	}
	b.Cleanup(func() {
		drop := "DROP DATABASE " + pgx.Identifier{name}.Sanitize() + " WITH (FORCE)"
		if _, err := server.admin.Exec(context.Background(), drop); err != nil {
			b.Errorf("drop database: %v", err)
		}
	})
	return name
}

// benchStore opens a store on its own pool to database name, as one
// process holds it.
func benchStore(b *testing.B, name string) *Store {
	b.Helper()
	pool, err := connect(b.Context(), name)
	if err != nil {
		b.Fatalf("connect: %v", err)
	}
	b.Cleanup(pool.Close)
	st, err := Open(b.Context(), pool, Options{Clock: benchClock{}})
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	return st
}

type benchClock struct{}

func (benchClock) Now() time.Time { return time.Now() }

// BenchmarkIngestCommit is the memory store benchmark of the same name in
// internal/ingress, on PostgreSQL: one verified inbound message from
// Sink.Accept to its committed unit. The sink is the one Runtime.Receive
// hands the channel's receiver.
func BenchmarkIngestCommit(b *testing.B) {
	for _, tc := range []struct {
		name      string
		newSender bool
	}{
		{"first_contact", true},
		{"existing_conversation", false},
	} {
		b.Run(tc.name, func(b *testing.B) {
			r := newIngestRig(b, benchDatabase(b))
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				from := "u-0"
				if tc.newSender {
					from = fmt.Sprintf("u-%d", i)
				}
				if _, err := r.accept(b.Context(), i, from); err != nil {
					b.Fatalf("Accept %d: %v", i, err)
				}
			}
		})
	}
}

// BenchmarkReplyDispatch is the memory store benchmark of the same name in
// internal/runtime, on PostgreSQL. accept is Messages.Reply alone; dispatch
// runs one reply through a Runtime.Run worker the bench wakes, to the
// provider call (send-µs) and back to an idle worker (ns/op).
func BenchmarkReplyDispatch(b *testing.B) {
	b.Run("accept", func(b *testing.B) {
		r := newReplyRig(b, benchDatabase(b))
		b.ReportAllocs()
		b.ResetTimer()
		for i := range b.N {
			r.reply(b, i)
		}
	})
	b.Run("dispatch", func(b *testing.B) {
		r := newReplyRig(b, benchDatabase(b))
		ctx, stop := context.WithCancel(b.Context())
		ran := make(chan error, 1)
		go func() {
			ran <- r.e.Runtime().Run(ctx, benchCall(), uma.WorkerOptions{Owner: "bench-worker", Lease: time.Minute})
		}()
		r.waiter.awaitIdle(b, ran)
		var toSend time.Duration
		b.ReportAllocs()
		b.ResetTimer()
		for i := range b.N {
			start := time.Now()
			r.reply(b, i)
			r.waiter.kick()
			select {
			case at := <-r.sender.sent:
				toSend += at.Sub(start)
			case err := <-ran:
				b.Fatalf("Run returned %v before the provider call", err)
			}
			r.waiter.awaitIdle(b, ran)
		}
		b.StopTimer()
		b.ReportMetric(float64(toSend.Microseconds())/float64(b.N), "send-µs")
		stop()
		<-ran
	})
}

// BenchmarkEventDelivery is the memory store benchmark of the same name in
// internal/events, on PostgreSQL: ready polls a committed message without
// waiting (poll-µs is Poll and Ack); wakeup commits it while a long poll
// waits and reports commit-to-Poll latency percentiles.
func BenchmarkEventDelivery(b *testing.B) {
	b.Run("ready", func(b *testing.B) {
		r := newIngestRig(b, benchDatabase(b))
		sub := r.subscribe(b)
		var polling time.Duration
		b.ReportAllocs()
		b.ResetTimer()
		for i := range b.N {
			if _, err := r.accept(b.Context(), i, "u-0"); err != nil {
				b.Fatalf("Accept %d: %v", i, err)
			}
			start := time.Now()
			r.pollAck(b, sub, i, 0)
			polling += time.Since(start)
		}
		b.StopTimer()
		b.ReportMetric(float64(polling.Microseconds())/float64(b.N), "poll-µs")
	})
	b.Run("wakeup", func(b *testing.B) {
		r := newIngestRig(b, benchDatabase(b))
		sub := r.subscribe(b)
		lat := make([]time.Duration, 0, b.N)
		b.ResetTimer()
		for i := range b.N {
			got := make(chan time.Time, 1)
			go func() {
				r.pollAck(b, sub, i, 5*time.Second)
				got <- time.Now()
			}()
			x := float64(i) * 0.6180339887
			time.Sleep(time.Duration(float64(time.Second) * (x - float64(int64(x)))))
			if _, err := r.accept(b.Context(), i, "u-0"); err != nil {
				b.Fatalf("Accept %d: %v", i, err)
			}
			committed := time.Now()
			lat = append(lat, (<-got).Sub(committed))
		}
		b.StopTimer()
		slices.Sort(lat)
		ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
		b.ReportMetric(ms(lat[len(lat)/2]), "p50-delivery-ms")
		b.ReportMetric(ms(lat[(len(lat)*95)/100]), "p95-delivery-ms")
		b.ReportMetric(ms(lat[len(lat)-1]), "max-delivery-ms")
	})
}

// BenchmarkClaimWorkContention drains a queue of b.N work items, one
// resource each, with concurrent claimers, each a store on its own pool as
// a separate process holds it. A claimer claims one item per unit,
// completes it in a second unit and reruns any unit that failed with
// model.Transient. ns/op is wall time per completed item; attempts/claim
// counts claim units per item claimed (1: no serialization failure);
// over3-% is the share of claims that needed more than three units, the
// bound one engine claim round retries within before it waits for its next
// poll. in_flight=16 applies the engine's default MaxInFlight cap, which
// adds a count of claimed work to every claim unit. lost-completes counts
// completions refused because another claim held or had finished the
// item: zero while claims are exclusive.
func BenchmarkClaimWorkContention(b *testing.B) {
	for _, inFlight := range []int{0, 16} {
		for _, n := range []int{1, 4, 8, 16} {
			b.Run(fmt.Sprintf("in_flight=%d/claimers=%d", inFlight, n), func(b *testing.B) {
				first := benchDatabase(b)
				name := first.pool.Config().ConnConfig.Database
				stores := []*Store{first}
				for len(stores) < n {
					stores = append(stores, benchStore(b, name))
				}
				benchClaims(b, stores, n, inFlight)
			})
		}
	}
}

const benchKind = "bench.work"

// benchLease outlasts any stall of a loaded machine, so a lease expiring
// between claim and completion never reads as contention.
const benchLease = 10 * time.Minute

func benchClaims(b *testing.B, stores []*Store, claimers, maxInFlight int) {
	ctx := b.Context()
	for lo := 0; lo < b.N; lo += 500 {
		if err := stores[0].Transact(ctx, benchTenant, func(tx store.Tx) error {
			for i := lo; i < min(lo+500, b.N); i++ {
				w := store.Work{
					ID: benchID(1, i), Kind: benchKind,
					Resource: model.OperationRef{Tenant: benchTenant, ID: benchID(2, i)}.Ref(),
				}
				if err := tx.EnqueueWork(ctx, w); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			b.Fatalf("enqueue: %v", err)
		}
	}
	var done, claimUnits, claims, over3, completeRetries, lost atomic.Int64
	var failed atomic.Bool
	b.ResetTimer()
	var wg sync.WaitGroup
	for c := range claimers {
		st, owner := stores[c%len(stores)], fmt.Sprintf("claimer-%d", c)
		wg.Go(func() {
			for done.Load() < int64(b.N) && !failed.Load() {
				var got []store.Work
				units := 0
				for {
					units++
					err := st.Transact(ctx, benchTenant, func(tx store.Tx) error {
						var err error
						got, err = tx.ClaimWork(ctx, store.WorkClaim{
							Kinds: []string{benchKind}, Limit: 1, Owner: owner,
							Until: time.Now().Add(benchLease), MaxInFlight: maxInFlight,
						})
						return err
					})
					if model.IsCode(err, model.Transient) || model.IsCode(err, model.Unavailable) {
						continue
					}
					if err != nil {
						failed.Store(true)
						b.Errorf("%s claim: %v", owner, err)
						return
					}
					break
				}
				if len(got) == 0 {
					continue
				}
				claimUnits.Add(int64(units))
				claims.Add(1)
				if units > 3 {
					over3.Add(1)
				}
				for {
					err := st.Transact(ctx, benchTenant, func(tx store.Tx) error { return tx.CompleteWork(ctx, got[0]) })
					if model.IsCode(err, model.Transient) {
						completeRetries.Add(1)
						continue
					}
					if model.IsCode(err, model.Conflict) || model.IsCode(err, model.NotFound) {
						// Another claim holds or finished the item.
						lost.Add(1)
						break
					}
					if err != nil {
						failed.Store(true)
						b.Errorf("%s complete: %v", owner, err)
						return
					}
					done.Add(1)
					break
				}
			}
		})
	}
	wg.Wait()
	b.StopTimer()
	if n := claims.Load(); n > 0 {
		b.ReportMetric(float64(claimUnits.Load())/float64(n), "attempts/claim")
		b.ReportMetric(100*float64(over3.Load())/float64(n), "over3-%")
		b.ReportMetric(float64(completeRetries.Load())/float64(n), "complete-retries/item")
		b.ReportMetric(float64(lost.Load()), "lost-completes")
	}
}

// benchID is a well-formed internal ID unique per (space, i).
func benchID(space, i int) model.ID {
	return model.ID(fmt.Sprintf("%08x-0000-7000-8000-%012x", space, i))
}

func benchCall() uma.RuntimeCall {
	return uma.RuntimeCall{Principal: model.Principal{Tenant: benchTenant, Actor: "bench-service"}, Call: model.Call{Correlation: "bench"}}
}

func benchCommand(op string) model.Command {
	return model.Command{Call: model.Call{Correlation: model.CorrelationID(op)}, Operation: model.OperationID(op)}
}

func benchAllow() policy.Authorizer {
	return policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
		return policy.Decision{Allow: true}, nil
	})
}

const (
	benchIngestType model.TypeKey = "com.example.bench.ingest"
	benchSendType   model.TypeKey = "com.example.bench.send"
	benchNS                       = "com.example.bench.user"
	benchDest                     = "support"
)

// ingestRig is an engine with one receive-only channel, a per-contact
// inbox on it, and a Runtime.Receive call whose receiver hands the bench
// its sink.
type ingestRig struct {
	e   uma.Engine
	ch  model.ChannelRef
	snk adapter.Sink
}

func newIngestRig(b *testing.B, st store.Store) *ingestRig {
	b.Helper()
	ctx := b.Context()
	recv := &benchReceiver{sinks: make(chan adapter.Sink, 1)}
	e, err := uma.New(uma.Config{
		Store: st, Authorizer: benchAllow(), Adapters: []adapter.Factory{recv},
		Clock: benchClock{}, IDs: portstest.NewSequenceIDs(0xb1),
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = e.Close(context.Background()) })
	admin, err := e.For(model.Principal{Tenant: benchTenant, Actor: "bench-admin"})
	if err != nil {
		b.Fatal(err)
	}
	acc, err := admin.Configuration().ConfigureChannel(ctx, model.ConfigureChannel{
		Command: benchCommand("bench-channel"),
		Channel: model.Channel{
			Ref: model.ChannelRef{Tenant: benchTenant}, Type: benchIngestType,
			Configuration: model.Extension{Name: string(benchIngestType) + ".config", Version: 1},
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	ch, err := acc.Operation.Result.AsChannel()
	if err != nil {
		b.Fatal(err)
	}
	lock := true
	if _, err := admin.Configuration().ConfigureInbox(ctx, model.ConfigureInbox{
		Command: benchCommand("bench-inbox"),
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: benchTenant}, Channel: ch, Name: "bench", Destinations: []string{benchDest},
			Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
		},
	}); err != nil {
		b.Fatal(err)
	}
	rctx, stop := context.WithCancel(ctx)
	received := make(chan error, 1)
	go func() { received <- e.Runtime().Receive(rctx, benchCall(), ch) }()
	b.Cleanup(func() {
		stop()
		<-received
	})
	r := &ingestRig{e: e, ch: ch}
	select {
	case r.snk = <-recv.sinks:
	case err := <-received:
		b.Fatalf("Receive returned %v before its receiver ran", err)
	case <-time.After(30 * time.Second):
		b.Fatal("receiver never ran")
	}
	return r
}

func (r *ingestRig) accept(ctx context.Context, i int, from string) (model.IngressReceipt, error) {
	id := fmt.Sprintf("e-%d", i)
	return r.snk.Accept(ctx, model.Ingress{
		Call: model.Call{Correlation: model.CorrelationID("c-" + id)},
		Envelope: model.Envelope{
			EventID: id, MessageID: "m-" + id, Destination: benchDest, Kind: model.EnvelopeMessage,
			Sender:    model.ExternalRef{Channel: r.ch, Namespace: benchNS, Kind: "user", ID: from},
			Assurance: "verified",
			Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "benchmark message " + id}}},
			Audience:  model.Audience{Visibility: model.Public}, OccurredAt: time.Unix(1_700_000_000, 0).UTC(),
		},
	})
}

func (r *ingestRig) subscribe(b *testing.B) model.SubscriptionRef {
	b.Helper()
	cl, err := r.e.For(model.Principal{Tenant: benchTenant, Actor: "bench-subscriber"})
	if err != nil {
		b.Fatal(err)
	}
	opened, err := cl.Events().Open(b.Context(), model.OpenSubscription{
		Command: benchCommand("bench-open"), Consumer: "bench",
		Filter: model.Filter{Kinds: []string{model.EventMessageReceived}},
		Start:  model.StartTail, PayloadMajor: model.PayloadVersion1,
	})
	if err != nil {
		b.Fatal(err)
	}
	return opened.Subscription.Ref
}

// pollAck polls until the i-th message arrives within wait, then
// acknowledges it. It reports failures with Errorf: it also runs off the
// benchmark goroutine.
func (r *ingestRig) pollAck(b *testing.B, sub model.SubscriptionRef, i int, wait time.Duration) {
	ctx := b.Context()
	cl, err := r.e.For(model.Principal{Tenant: benchTenant, Actor: "bench-subscriber"})
	if err != nil {
		b.Error(err)
		return
	}
	batch, err := cl.Events().Poll(ctx, model.Poll{Call: model.Call{Correlation: "bench-poll"}, Subscription: sub, Wait: wait})
	if err != nil || len(batch.Events) != 1 {
		b.Errorf("Poll %d = %d events, %v; want the message", i, len(batch.Events), err)
		return
	}
	if _, err := cl.Events().Ack(ctx, model.Acknowledge{
		Command: benchCommand(fmt.Sprintf("ack-%d", i)), Subscription: sub, Token: batch.AckToken,
	}); err != nil {
		b.Errorf("Ack %d: %v", i, err)
	}
}

// benchReceiver is a receive-only adapter whose receiver hands its sink to
// the bench and runs until stopped.
type benchReceiver struct{ sinks chan adapter.Sink }

func (*benchReceiver) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: benchIngestType, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: string(benchIngestType) + ".config", Version: 1},
		Capabilities:        []model.Capability{model.CapabilityReceive},
		Events:              []string{model.EventMessageReceived},
		IdentityNamespaces:  []string{benchNS},
	}
}

func (*benchReceiver) Validate(model.Extension) []model.ValidationIssue { return nil }

func (r *benchReceiver) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	return r, nil
}

func (*benchReceiver) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return []model.CapabilityStatus{{Name: model.CapabilityReceive, Available: true}}, nil
}

func (*benchReceiver) Close(context.Context) error { return nil }

func (r *benchReceiver) Run(ctx context.Context, s adapter.Sink) error {
	select {
	case r.sinks <- s:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

// replyRig is an engine with one send-capable channel, an inbox and a
// conversation routed to it.
type replyRig struct {
	e      uma.Engine
	cl     uma.Client
	conv   model.ConversationRef
	sender *benchSender
	waiter *kickWaiter
}

func newReplyRig(b *testing.B, st *Store) *replyRig {
	b.Helper()
	ctx := b.Context()
	snd := &benchSender{sent: make(chan time.Time, 1)}
	w := &kickWaiter{idle: make(chan struct{}, 1), wake: make(chan struct{}, 1)}
	e, err := uma.New(uma.Config{
		Store: st, Authorizer: benchAllow(), Adapters: []adapter.Factory{snd},
		Clock: benchClock{}, IDs: portstest.NewSequenceIDs(0xb2), Waiter: w,
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = e.Close(context.Background()) })
	ch := model.ChannelRef{Tenant: benchTenant, ID: "0000000b-0000-7000-8000-000000000001"}
	in := model.InboxRef{Tenant: benchTenant, ID: "0000000b-0000-7000-8000-000000000002"}
	conv := model.ConversationRef{Inbox: in, ID: "0000000b-0000-7000-8000-000000000003"}
	if err := st.Transact(ctx, benchTenant, func(tx store.Tx) error {
		return firstErr(
			tx.PutChannel(ctx, model.Channel{
				Ref: ch, Type: benchSendType, Configuration: model.Extension{Name: string(benchSendType) + ".config", Version: 1}, Revision: "ch1",
			}, ""),
			tx.PutInbox(ctx, model.Inbox{Ref: in, Channel: ch, Name: "bench", Revision: "in1"}, ""),
			tx.PutConversation(ctx, model.Conversation{
				Ref: conv, Mode: model.ModePerRoom, Status: model.ConversationOpen,
				ReplyRoute: model.ExternalRef{Channel: ch, Namespace: string(benchSendType), Kind: "room", ID: "room-1"},
				CreatedAt:  time.Now(), Revision: "cv1",
			}, ""),
		)
	}); err != nil {
		b.Fatalf("seed: %v", err)
	}
	cl, err := e.For(model.Principal{Tenant: benchTenant, Actor: "bench-agent"})
	if err != nil {
		b.Fatal(err)
	}
	return &replyRig{e: e, cl: cl, conv: conv, sender: snd, waiter: w}
}

func (r *replyRig) reply(b *testing.B, i int) {
	if _, err := r.cl.Messages().Reply(b.Context(), model.Reply{
		Command:      benchCommand(fmt.Sprintf("op-%d", i)),
		Conversation: r.conv,
		Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "benchmark reply"}}},
	}); err != nil {
		b.Fatalf("Reply %d: %v", i, err)
	}
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
