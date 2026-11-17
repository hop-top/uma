package sms_test

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio/sms"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// pacer reports each idle Runtime.Run wait and blocks it until resumed.
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

// replyRun is one reply on a twilio.sms channel backed by fake, worked by
// a runtime whose clock and idle waits the test drives.
type replyRun struct {
	t      *testing.T
	fake   *fakeTwilio
	engine uma.Engine
	client uma.Client
	clock  *portstest.FixedClock
	pacer  *pacer
	call   uma.RuntimeCall
	done   chan error
	op     model.OperationRef
}

// startReply queues one reply and runs a worker until it first goes idle.
func startReply(t *testing.T, fake *fakeTwilio) *replyRun {
	t.Helper()
	ctx := t.Context()
	clock := portstest.NewFixedClock(time.Unix(1_760_000_000, 0))
	st := memory.New(memory.Options{Clock: clock})
	p := &pacer{idle: make(chan struct{}, 16), resume: make(chan struct{})}
	engine, err := uma.New(uma.Config{
		Store: st,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{sms.NewFactory(sms.Options{BaseURL: fake.srv.URL, Timeout: 5 * time.Second})},
		Credentials: &tokenHost{},
		HTTP:        fake.client(),
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(21),
		Waiter:      p,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close(context.Background()) })

	lock := true
	conv := model.ConversationRef{Inbox: hookInbox, ID: "00000000-0000-7000-8000-0000000006c1"}
	err = st.Transact(ctx, hookTenant, func(tx store.Tx) error {
		if err := tx.PutChannel(ctx, model.Channel{
			Ref: hookChannel, Type: model.TypeTwilioSms, Configuration: hookConfig().Extension(), CredentialRef: "twilio", Revision: "ch1",
		}, ""); err != nil {
			return err
		}
		if err := tx.PutInbox(ctx, model.Inbox{
			Ref: hookInbox, Channel: hookChannel, Name: "sms", Destinations: []string{senderNumber}, Revision: "in1",
			Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
		}, ""); err != nil {
			return err
		}
		return tx.PutConversation(ctx, model.Conversation{
			Ref: conv, Mode: model.ModePerRoom, Status: model.ConversationOpen, CreatedAt: clock.Now(), Revision: "cv1",
			ReplyRoute: model.ExternalRef{Channel: hookChannel, Namespace: sms.Namespace, Kind: sms.RouteKind, ID: "+15558675310"},
		}, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := engine.For(model.Principal{Tenant: hookTenant, Actor: "replier"})
	if err != nil {
		t.Fatal(err)
	}
	acc, err := client.Messages().Reply(ctx, model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: "reply"}, Operation: "op-reply"},
		Conversation: conv,
		Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello"}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := &replyRun{
		t: t, fake: fake, engine: engine, client: client, clock: clock, pacer: p,
		call: uma.RuntimeCall{Principal: model.Principal{Tenant: hookTenant, Actor: "worker"}, Call: model.Call{Correlation: "run"}},
		done: make(chan error, 1), op: acc.Operation.Ref,
	}
	go func() { r.done <- engine.Runtime().Run(ctx, r.call, uma.WorkerOptions{Owner: "w1", Limit: 10}) }()
	r.idle()
	return r
}

func (r *replyRun) idle() {
	r.t.Helper()
	select {
	case <-r.pacer.idle:
	case err := <-r.done:
		r.t.Fatalf("Run returned early: %v", err)
	case <-time.After(10 * time.Second):
		r.t.Fatal("Run did not go idle")
	}
}

// later advances the clock by d and lets the worker run until idle again.
func (r *replyRun) later(d time.Duration) {
	r.t.Helper()
	r.clock.Advance(d)
	r.pacer.resume <- struct{}{}
	r.idle()
}

// stop drains the worker and returns the operation.
func (r *replyRun) stop() model.Operation {
	r.t.Helper()
	ctx := r.t.Context()
	if err := r.engine.Runtime().Drain(ctx, r.call); err != nil {
		r.t.Fatal(err)
	}
	if err := <-r.done; err != nil {
		r.t.Fatalf("Run: %v", err)
	}
	op, err := r.client.Operations().Get(ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "get"}, Ref: r.op})
	if err != nil {
		r.t.Fatal(err)
	}
	return op
}

func (r *replyRun) methods() []string {
	var out []string
	for _, c := range r.fake.calls(hookAccount) {
		out = append(out, c.Method)
	}
	return out
}

// A 5xx answer arrives after Twilio read the request, which may have
// created the message: the runtime leaves the reply outcome_unknown and
// sends nothing more before a listing shows Twilio holds no such message.
// Here the server created none, so exactly one resend follows the listing.
func TestServerErrorAfterSendIsReconciledBeforeAnyResend(t *testing.T) {
	for _, code := range []int{500, 502, 503, 504} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			fake := newFakeTwilio(t)
			fake.respond(hookAccount, conformance.Response{Outcome: outcomeStatus(code)})
			r := startReply(t, fake)
			for range 3 {
				r.later(time.Hour)
			}
			op := r.stop()
			if op.State != model.ProviderAccepted || len(op.ProviderReferences) == 0 {
				t.Fatalf("operation = %s (%v), want provider_accepted from the reconciling listing", op.State, op.Failure)
			}
			if want := []string{http.MethodPost, http.MethodGet, http.MethodPost}; !slices.Equal(r.methods(), want) {
				t.Fatalf("Twilio calls = %v, want %v: create, the reconciling listing, one resend", r.methods(), want)
			}
		})
	}
}

// Twilio keeps a create it refuses as a failed message (error 21266, To
// equal to From, recorded in testdata/cassettes/twilio/rejected). When the
// refusal's answer is lost, reconciling finds that message: the reply
// settles rejected for good and is never sent again.
func TestLostRefusalIsNeverResent(t *testing.T) {
	fake := newFakeTwilio(t)
	fake.respond(hookAccount, conformance.Response{Outcome: outcomeRefusedAnswerLost})
	r := startReply(t, fake)
	for range 3 {
		r.later(time.Hour)
	}
	op := r.stop()
	if op.State != model.Rejected || op.Failure == nil || op.Failure.Retry != model.NoRetry || len(op.ProviderReferences) != 1 {
		t.Fatalf("operation = %s (%v, refs %v), want rejected for good with the refused message", op.State, op.Failure, op.ProviderReferences)
	}
	if want := []string{http.MethodPost, http.MethodGet}; !slices.Equal(r.methods(), want) {
		t.Fatalf("Twilio calls = %v, want %v: the refused create and the listing finding it, no resend", r.methods(), want)
	}
}

// Twilio's Messages list shows a new message only some time after creating
// it (a refused create recorded in testdata/cassettes/twilio/rejected was
// missing from a listing made right after). The adapter declares that lag
// as its reconcile settle window: a listing inside it that misses the
// message whose answer was lost never leads to a resend.
func TestLaggingListingNeverCausesResend(t *testing.T) {
	if got := sms.NewFactory(sms.Options{}).Describe().ReconcileSettle; got != sms.ReconcileSettle || got <= 0 {
		t.Fatalf("ReconcileSettle = %v, want the declared %v", got, sms.ReconcileSettle)
	}
	fake := newFakeTwilio(t)
	fake.respond(hookAccount, conformance.Response{Outcome: conformance.OutcomeAnswerLost})
	fake.lagListings()
	r := startReply(t, fake)
	for elapsed := time.Duration(0); elapsed < sms.ReconcileSettle-time.Minute; elapsed += 30 * time.Second {
		r.later(30 * time.Second)
	}
	fake.catchUp()
	for range 3 {
		r.later(time.Hour)
	}
	op := r.stop()
	if op.State != model.ProviderAccepted || len(op.ProviderReferences) != 1 {
		t.Fatalf("operation = %s (%v), want provider_accepted from the listing that caught up", op.State, op.Failure)
	}
	var creates int
	for _, m := range r.methods() {
		if m == http.MethodPost {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("Twilio calls = %v, want one create: a listing that has not caught up must not cause a resend", r.methods())
	}
}
