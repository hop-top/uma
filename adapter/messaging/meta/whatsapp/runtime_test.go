package whatsapp_test

import (
	"context"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// outcomeServerFault scripts a Graph server error object (code 131000,
// HTTP 500) answered after the request arrived; the fake counts the
// message as sent, as Meta may have.
const outcomeServerFault conformance.Outcome = "server_fault"

// pacer reports each idle worker wait and holds it until resumed.
type pacer struct{ idle, resume chan struct{} }

func (p *pacer) Wait(ctx context.Context, _ time.Duration) error {
	select {
	case p.idle <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.resume:
		return nil
	}
}

func awaitIdle(t *testing.T, p *pacer, run <-chan error) {
	t.Helper()
	select {
	case <-p.idle:
	case err := <-run:
		t.Fatalf("Runtime.Run returned early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Runtime.Run did not go idle")
	}
}

// A server fault answered after the send is outcome unknown: the runtime
// never sends the message a second time, however long it runs.
func TestServerFaultAfterSendIsNeverRetried(t *testing.T) {
	f := fakeWithAccount(t)
	clock := portstest.NewFixedClock(testNow)
	st := memory.New(memory.Options{Clock: clock})
	p := &pacer{idle: make(chan struct{}, 16), resume: make(chan struct{})}
	engine, err := uma.New(uma.Config{
		Store: st,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{f.factory()},
		Credentials: newCreds(),
		HTTP:        f.client(),
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(0x5f),
		Waiter:      p,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	inbox := model.InboxRef{Tenant: testTenant, ID: "00000000-0000-7000-8000-00000000b0a2"}
	conv := model.ConversationRef{Inbox: inbox, ID: "00000000-0000-7000-8000-00000000c0c1"}
	lock := true
	sel := model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1}
	if err := st.Transact(t.Context(), testTenant, func(tx store.Tx) error {
		if err := tx.PutChannel(t.Context(), channel(testPNID), ""); err != nil {
			return err
		}
		if err := tx.PutInbox(t.Context(), model.Inbox{Ref: inbox, Channel: testChannel, Name: "support", Destinations: []string{testPNID}, Selection: sel, Revision: "in1"}, ""); err != nil {
			return err
		}
		return tx.PutConversation(t.Context(), model.Conversation{
			Ref: conv, Mode: model.ModePerContact, Status: model.ConversationOpen,
			ReplyRoute: userRoute(testUser), CreatedAt: testNow, Revision: "cv1",
		}, "")
	}); err != nil {
		t.Fatal(err)
	}
	if err := (provider{f: f}).Respond(testChannel, conformance.Response{Outcome: outcomeServerFault}); err != nil {
		t.Fatal(err)
	}
	client, err := engine.For(model.Principal{Tenant: testTenant, Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	acc, err := client.Messages().Reply(t.Context(), model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: "c-fault"}, Operation: "op-fault"},
		Conversation: conv,
		Content:      textContent("Your order shipped."),
	})
	if err != nil {
		t.Fatal(err)
	}
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: testTenant, Actor: "worker"}, Call: model.Call{Correlation: "run"}}
	run := make(chan error, 1)
	go func() { run <- engine.Runtime().Run(t.Context(), call, uma.WorkerOptions{Owner: "w", Limit: 10}) }()
	awaitIdle(t, p, run)
	for range 3 {
		clock.Advance(time.Hour)
		p.resume <- struct{}{}
		awaitIdle(t, p, run)
	}
	if err := engine.Runtime().Drain(t.Context(), call); err != nil {
		t.Fatal(err)
	}
	if err := <-run; err != nil {
		t.Fatalf("Runtime.Run: %v", err)
	}
	op, err := client.Operations().Get(t.Context(), model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "get"}, Ref: acc.Operation.Ref})
	if err != nil {
		t.Fatal(err)
	}
	if op.State != model.Unknown || op.Failure == nil || op.Failure.Code != model.OutcomeUnknown {
		t.Fatalf("operation %+v, want outcome_unknown", op)
	}
	if calls := (provider{f: f}).Calls(testChannel); len(calls) != 1 {
		t.Fatalf("provider sends = %d, want 1: a server fault after send is never resent", len(calls))
	}
}
