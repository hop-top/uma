package fakeprovider_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/examples/fakeprovider"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// The outbound executor end to end: uma.New over the memory store, with the
// scripted fake provider registered like any third-party adapter.

const actor model.ActorID = "agent-1"

var (
	inboxRef = model.InboxRef{Tenant: tenant, ID: "00000000-0000-7000-8000-0000000000a1"}
	convRef  = model.ConversationRef{Inbox: inboxRef, ID: "00000000-0000-7000-8000-0000000000c1"}
)

// idleWaiter reports each idle Runtime.Run wait and blocks until resumed or
// cancelled.
type idleWaiter struct {
	idle   chan struct{}
	resume chan struct{}
}

func (w *idleWaiter) Wait(ctx context.Context, _ time.Duration) error {
	select {
	case w.idle <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.resume:
		return nil
	}
}

// credentialHost is a host credential port with revocation.
type credentialHost struct {
	mu      sync.Mutex
	revoked bool
	reqs    []ports.CredentialRequest
}

func (h *credentialHost) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reqs = append(h.reqs, r)
	if h.revoked {
		return nil, model.NewError(model.Unauthorized, "consent revoked")
	}
	return ports.Secret{Bytes: []byte("token")}, nil
}

type host struct {
	t       *testing.T
	ctx     context.Context
	engine  uma.Engine
	client  uma.Client
	store   *memory.Store
	clock   *portstest.FixedClock
	factory *fakeprovider.Factory
	channel model.Channel
	conv    model.Conversation
	waiter  *idleWaiter
	creds   *credentialHost
	run     chan error
}

func newHost(t *testing.T, f *fakeprovider.Factory) *host {
	t.Helper()
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0))
	h := &host{
		t: t, ctx: t.Context(), clock: clock, factory: f,
		store:  memory.New(memory.Options{Clock: clock}),
		waiter: &idleWaiter{idle: make(chan struct{}, 8), resume: make(chan struct{})},
		creds:  &credentialHost{},
	}
	engine, err := uma.New(uma.Config{
		Store: h.store,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{f},
		Extensions:  f.Extensions(),
		Credentials: h.creds,
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(3),
		Waiter:      h.waiter,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.engine = engine
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	h.channel = model.Channel{
		Ref: chanA, Type: f.Describe().Type, Configuration: f.Config(), CredentialRef: "fake-cred", Revision: "ch1",
	}
	h.conv = model.Conversation{
		Ref: convRef, Mode: model.ModePerRoom, Status: model.ConversationOpen,
		ReplyRoute: model.ExternalRef{Channel: chanA, Namespace: fakeprovider.Namespace, Kind: "room", ID: "room-1"},
		CreatedAt:  clock.Now(), Revision: "cv1",
	}
	err = h.store.Transact(h.ctx, tenant, func(tx store.Tx) error {
		if err := tx.PutChannel(h.ctx, h.channel, ""); err != nil {
			return err
		}
		if err := tx.PutInbox(h.ctx, model.Inbox{Ref: inboxRef, Channel: chanA, Name: "fake", Revision: "in1"}, ""); err != nil {
			return err
		}
		return tx.PutConversation(h.ctx, h.conv, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.client, err = engine.For(model.Principal{Tenant: tenant, Actor: actor}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *host) reply(opID model.OperationID, text string) (model.Acceptance, error) {
	return h.client.Messages().Reply(h.ctx, model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: model.CorrelationID("c-" + string(opID))}, Operation: opID},
		Conversation: convRef,
		Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
	})
}

func (h *host) mustReply(opID model.OperationID, text string) model.OperationRef {
	h.t.Helper()
	a, err := h.reply(opID, text)
	if err != nil {
		h.t.Fatalf("Reply: %v", err)
	}
	return a.Operation.Ref
}

func runtimeCall() uma.RuntimeCall {
	return uma.RuntimeCall{Principal: model.Principal{Tenant: tenant, Actor: "service"}, Call: model.Call{Correlation: "rt"}}
}

// start runs a worker in the background until it first goes idle.
func (h *host) start(owner string) {
	h.t.Helper()
	h.run = make(chan error, 1)
	go func() {
		h.run <- h.engine.Runtime().Run(h.ctx, runtimeCall(), uma.WorkerOptions{Owner: owner, Limit: 10})
	}()
	h.idle()
}

// idle waits until the worker has no claimable work.
func (h *host) idle() {
	h.t.Helper()
	select {
	case <-h.waiter.idle:
	case err := <-h.run:
		h.t.Fatalf("Run returned early: %v", err)
	}
}

// later advances the shared clock past any backoff and lets the idle worker
// claim again.
func (h *host) later() {
	h.t.Helper()
	h.clock.Advance(time.Hour)
	h.waiter.resume <- struct{}{}
	h.idle()
}

func (h *host) stop() {
	h.t.Helper()
	if err := h.engine.Runtime().Drain(h.ctx, runtimeCall()); err != nil {
		h.t.Fatal(err)
	}
	if err := <-h.run; err != nil {
		h.t.Fatalf("Run: %v", err)
	}
}

func (h *host) op(ref model.OperationRef) model.Operation {
	h.t.Helper()
	op, err := h.client.Operations().Get(h.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "get"}, Ref: ref})
	if err != nil {
		h.t.Fatal(err)
	}
	return op
}

func sends(calls []fakeprovider.Call) int {
	n := 0
	for _, c := range calls {
		if c.Method == fakeprovider.MethodSend {
			n++
		}
	}
	return n
}

func TestRuntimeDeliversOnStoredRoute(t *testing.T) {
	h := newHost(t, fakeprovider.NewFull())
	ref := h.mustReply("op-1", "hello")
	h.start("worker")
	h.stop()
	op := h.op(ref)
	calls := h.factory.Script(chanA).Calls()
	if op.State != model.ProviderAccepted || len(calls) != 1 || calls[0].Effect.Route != h.conv.ReplyRoute {
		t.Fatalf("op = %+v, calls = %+v; want one accepted send on the stored route", op, calls)
	}
	if len(op.ProviderReferences) != 1 || op.ProviderReferences[0] != calls[0].References[0] {
		t.Fatalf("references = %v, want the provider's %v", op.ProviderReferences, calls[0].References)
	}
	if len(h.creds.reqs) != 1 || h.creds.reqs[0].Purpose != fakeprovider.CredentialPurpose || h.creds.reqs[0].Reference != "fake-cred" {
		t.Fatalf("credential requests = %+v", h.creds.reqs)
	}
}

func TestRuntimeReplayAndConflict(t *testing.T) {
	h := newHost(t, fakeprovider.NewFull())
	ref := h.mustReply("op-1", "hello")
	h.start("worker")
	h.stop()
	replay, err := h.reply("op-1", "hello")
	if err != nil || !replay.Replayed || replay.Operation.Ref != ref || replay.Operation.State != model.ProviderAccepted {
		t.Fatalf("replay = %+v, %v; want the stored accepted result", replay, err)
	}
	if _, err := h.reply("op-1", "changed"); model.CodeOf(err) != model.Conflict {
		t.Fatalf("changed payload = %v, want conflict", err)
	}
	if n := sends(h.factory.Script(chanA).Calls()); n != 1 {
		t.Fatalf("sends = %d, want 1", n)
	}
}

// Lost acknowledgement: the provider applied the send but its answer was
// lost. The operation is outcome_unknown until the next run reconciles it;
// nothing is sent twice.
func TestRuntimeLostAckReconcilesFirst(t *testing.T) {
	h := newHost(t, fakeprovider.NewFull())
	h.factory.Script(chanA).Respond(fakeprovider.OutcomeUnknown)
	ref := h.mustReply("op-1", "hello")
	h.start("worker")
	if op := h.op(ref); op.State != model.Unknown || op.Failure == nil || op.Failure.Retry != model.ReconcileFirst {
		t.Fatalf("after lost answer op = %+v, want outcome_unknown with reconcile_first", op)
	}
	h.later()
	h.stop()
	calls := h.factory.Script(chanA).Calls()
	if op := h.op(ref); op.State != model.ProviderAccepted {
		t.Fatalf("after reconcile op = %+v", op)
	}
	if len(calls) != 2 || calls[0].Method != fakeprovider.MethodSend || calls[1].Method != fakeprovider.MethodReconcile {
		t.Fatalf("calls = %+v, want send then reconcile", calls)
	}
}

// A worker that dispatched and then crashed leaves an unresolved attempt
// under a lease that expires. The next owner claims a higher generation,
// reconciles the attempt and adopts the provider's record; the stale fence
// can no longer change the operation.
func TestRuntimeStaleGenerationCrashAfterDispatch(t *testing.T) {
	h := newHost(t, fakeprovider.NewFull())
	ref := h.mustReply("op-1", "hello")
	var stale store.Work
	attempt := model.ID("00000000-0000-7000-8000-0000000000e1")
	err := h.store.Transact(h.ctx, tenant, func(tx store.Tx) error {
		works, err := tx.ClaimWork(h.ctx, store.WorkClaim{
			Kinds: []string{uma.WorkKindOperation}, Limit: 1, Owner: "crashed", Until: h.clock.Now().Add(time.Minute),
		})
		if err != nil || len(works) != 1 {
			t.Fatalf("claim = %v, %v", works, err)
		}
		stale = works[0]
		return tx.AppendAttemptEvidence(h.ctx, store.AttemptEvidence{
			Operation: ref, Attempt: attempt, Fence: stale.Fence, State: model.Unknown, ObservedAt: h.clock.Now(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := h.factory.Open(h.ctx, h.channel, adapter.Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inst.Close(context.Background()) }()
	if _, err := inst.(adapter.Sender).Send(h.ctx, adapter.SendRequest{
		Effect: adapter.Effect{Operation: ref, Attempt: attempt, Route: h.conv.ReplyRoute, Audience: model.Audience{Visibility: model.Public}},
	}); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(2 * time.Minute)
	h.start("worker")
	h.stop()
	if op := h.op(ref); op.State != model.ProviderAccepted {
		t.Fatalf("op = %+v, want the reconciled provider record", op)
	}
	calls := h.factory.Script(chanA).Calls()
	if sends(calls) != 1 || calls[len(calls)-1].Method != fakeprovider.MethodReconcile {
		t.Fatalf("calls = %+v, want the crashed send and one reconcile", calls)
	}
	err = h.store.Transact(h.ctx, tenant, func(tx store.Tx) error {
		rec, err := tx.Operation(h.ctx, ref)
		if err != nil {
			return err
		}
		next := rec.Operation
		next.State, next.Revision = model.Failed, "stale-write"
		return tx.UpdateOperation(h.ctx, next, rec.Operation.Revision, stale.Fence)
	})
	if model.CodeOf(err) != model.Conflict {
		t.Fatalf("stale fence update = %v, want conflict", err)
	}
}

// An adapter without a reconciler leaves the operation outcome_unknown and
// never retries it.
func TestRuntimeWithoutReconcilerStaysUnknown(t *testing.T) {
	h := newHost(t, fakeprovider.NewSendOnly())
	h.factory.Script(chanA).Respond(fakeprovider.OutcomeUnknown)
	ref := h.mustReply("op-1", "hello")
	h.start("worker")
	h.later()
	h.stop()
	if op := h.op(ref); op.State != model.Unknown {
		t.Fatalf("op = %+v, want outcome_unknown", op)
	}
	if n := len(h.factory.Script(chanA).Calls()); n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
}

// A revoked credential fails the operation unauthorized, with no provider
// call and no retry.
func TestRuntimeRevokedCredential(t *testing.T) {
	h := newHost(t, fakeprovider.NewFull())
	h.creds.revoked = true
	ref := h.mustReply("op-1", "hello")
	h.start("worker")
	h.later()
	h.stop()
	op := h.op(ref)
	if op.State != model.Failed || op.Failure == nil || op.Failure.Code != model.Unauthorized || op.Failure.Retry != model.NoRetry {
		t.Fatalf("op = %+v, want failed unauthorized without retry", op)
	}
	if n := len(h.factory.Script(chanA).Calls()); n != 0 {
		t.Fatalf("provider calls = %d, want none", n)
	}
	if n := len(h.creds.reqs); n != 1 {
		t.Fatalf("credential resolutions = %d, want 1", n)
	}
}

// A transient provider failure is retried by the engine, never by the
// adapter, and the retry resolves the credential again.
func TestRuntimeTransientFailureRetries(t *testing.T) {
	h := newHost(t, fakeprovider.NewSendOnly())
	h.factory.Script(chanA).Respond(fakeprovider.Transient)
	ref := h.mustReply("op-1", "hello")
	h.start("worker")
	if op := h.op(ref); op.State != model.RetryPending || op.Failure == nil || op.Failure.Code != model.Transient {
		t.Fatalf("after transient op = %+v, want retry_pending with the transient failure", op)
	}
	h.later()
	h.stop()
	if op := h.op(ref); op.State != model.ProviderAccepted {
		t.Fatalf("op = %+v", op)
	}
	if calls := h.factory.Script(chanA).Calls(); len(calls) != 2 || len(h.creds.reqs) != 2 {
		t.Fatalf("calls = %d, credential resolutions = %d; want 2 and 2", len(calls), len(h.creds.reqs))
	}
}

// An edit follows a send through the worker: it reaches the provider
// message the send created, and the stored message changes only after the
// provider applied it.
func TestRuntimeEditRoundTrip(t *testing.T) {
	h := newHost(t, fakeprovider.NewFull())
	sent := h.mustReply("op-1", "hello")
	h.start("worker")
	op := h.op(sent)
	if op.ResultMessage == nil || len(op.ProviderReferences) != 1 {
		t.Fatalf("send = %+v, want a message with one provider reference", op)
	}
	msg, err := h.client.Messages().Get(h.ctx, model.Lookup[model.MessageRef]{Call: model.Call{Correlation: "get"}, Ref: *op.ResultMessage})
	if err != nil {
		t.Fatal(err)
	}
	a, err := h.client.Messages().Edit(h.ctx, model.EditMessage{
		Command: model.Command{Call: model.Call{Correlation: "c-edit"}, Operation: "op-edit", ExpectedRevision: msg.Revision},
		Message: msg.Ref, Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello again"}}},
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if got, _ := h.client.Messages().Get(h.ctx, model.Lookup[model.MessageRef]{Call: model.Call{Correlation: "get"}, Ref: msg.Ref}); got.Content.Parts[0].Text != "hello" {
		t.Fatalf("message = %+v before the edit was applied, want the sent content", got.Content)
	}
	h.later()
	h.stop()
	if op := h.op(a.Operation.Ref); op.State != model.ProviderAccepted || op.Pending != nil {
		t.Fatalf("edit = %+v, want it accepted and no longer pending", op)
	}
	var edits []fakeprovider.Call
	for _, c := range h.factory.Script(chanA).Calls() {
		if c.Method == fakeprovider.MethodEdit {
			edits = append(edits, c)
		}
	}
	if len(edits) != 1 || !edits[0].Applied || *edits[0].Effect.Target != op.ProviderReferences[0] {
		t.Fatalf("provider edits = %+v, want one applied edit of %v", edits, op.ProviderReferences[0])
	}
	got, err := h.client.Messages().Get(h.ctx, model.Lookup[model.MessageRef]{Call: model.Call{Correlation: "get"}, Ref: msg.Ref})
	if err != nil || got.Content.Parts[0].Text != "hello again" || got.LastEdit == nil || got.LastEdit.Operation == nil || *got.LastEdit.Operation != a.Operation.Ref {
		t.Fatalf("message = %+v, %v; want the edit applied under %v", got, err, a.Operation.Ref)
	}
}
