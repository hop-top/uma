package relay_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// The engine over a gateway whose first callback answer is lost: with and
// without the lookup, driven step by step through the public API.

var engineInbox = model.InboxRef{Tenant: testTenant, ID: "00000000-0000-7000-8000-0000000001b1"}

// lossyGateway loses the answer to its first callback, applied (the
// answer was lost) or not (the request was), and answers lookups from the
// keys it applied.
type lossyGateway struct {
	srv *httptest.Server

	mu      sync.Mutex
	firstOK bool // the lost first callback was applied
	posts   int
	lookups int
	applied map[string]string // id by Idempotency-Key
	// tags are the operation and attempt each callback carried, in order.
	tags []callbackTag
	// answerFirst answers the first callback like any other: nothing is lost.
	answerFirst bool
}

// callbackTag is the operation and attempt of one callback body.
type callbackTag struct{ Operation, Attempt, To string }

func newLossyGateway(t *testing.T, firstApplied bool) *lossyGateway {
	t.Helper()
	g := &lossyGateway{firstOK: firstApplied, applied: map[string]string{}}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

// newAnsweringGateway is a gateway that loses nothing: every callback is
// applied and answered.
func newAnsweringGateway(t *testing.T) *lossyGateway {
	t.Helper()
	g := newLossyGateway(t, true)
	g.answerFirst = true
	return g
}

func (g *lossyGateway) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+callbackSecret {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	g.mu.Lock()
	if r.Method == http.MethodGet {
		g.lookups++
		id, ok := g.applied[r.URL.Query().Get(relay.LookupQuery)]
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			_, _ = w.Write([]byte(`{"status":"not_found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"sent","message_id":"` + id + `"}`))
		return
	}
	g.posts++
	var tag struct {
		OperationID string `json:"operation_id"`
		AttemptID   string `json:"attempt_id"`
		To          string `json:"to"`
	}
	if raw, err := io.ReadAll(r.Body); err == nil {
		_ = json.Unmarshal(raw, &tag)
	}
	g.tags = append(g.tags, callbackTag{tag.OperationID, tag.AttemptID, tag.To})
	first := g.posts == 1
	key := r.Header.Get("Idempotency-Key")
	if !first || g.firstOK {
		g.applied[key] = "gw-" + key[len(key)-4:]
	}
	id := g.applied[key]
	g.mu.Unlock()
	if first && !g.answerFirst {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}
	_, _ = w.Write([]byte(`{"message_id":"` + id + `"}`))
}

// lastTo is the recipient of the latest callback.
func (g *lossyGateway) lastTo() string { return g.lastTag().To }

// lastTag is the operation and attempt of the latest callback.
func (g *lossyGateway) lastTag() callbackTag {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tags[len(g.tags)-1]
}

func (g *lossyGateway) counts() (posts, lookups int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.posts, g.lookups
}

type engineRig struct {
	t      *testing.T
	gw     *lossyGateway
	clock  *portstest.FixedClock
	engine uma.Engine
	client uma.Client
	st     store.Store
}

func newEngineRig(t *testing.T, gw *lossyGateway, lookup bool) *engineRig {
	t.Helper()
	return newEngineRigWith(t, gw, lookup, nil)
}

// newEngineRigWith is newEngineRig with the channel configuration adjusted
// by mutate before it is stored.
func newEngineRigWith(t *testing.T, gw *lossyGateway, lookup bool, mutate func(*relay.Config)) *engineRig {
	t.Helper()
	clock := portstest.NewFixedClock(testNow)
	st := memory.New(memory.Options{Clock: clock})
	host := strings.TrimPrefix(gw.srv.URL, "http://")
	e, err := uma.New(uma.Config{
		Store: st,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{relay.NewFactory(relay.Options{CallbackHosts: []string{host}, AllowHTTP: true})},
		Credentials: newCreds(),
		HTTP:        plainClient(),
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(0x7e1a),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	cfg := baseConfig()
	cfg.Callback = &relay.Callback{URL: gw.srv.URL + "/out"}
	if lookup {
		cfg.Callback.LookupURL = gw.srv.URL + "/lookup"
	}
	if mutate != nil {
		mutate(&cfg)
	}
	lock := true
	err = st.Transact(t.Context(), testTenant, func(tx store.Tx) error {
		if err := tx.PutChannel(t.Context(), channelOf(cfg), ""); err != nil {
			return err
		}
		return tx.PutInbox(t.Context(), model.Inbox{
			Ref: engineInbox, Channel: testChannel, Name: "sms", Destinations: []string{gatewayNumber}, Revision: "in1",
			Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
		}, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.For(model.Principal{Tenant: testTenant, Actor: "host"})
	if err != nil {
		t.Fatal(err)
	}
	return &engineRig{t: t, gw: gw, clock: clock, engine: e, client: c, st: st}
}

// start queues a first message to a number.
func (r *engineRig) start() model.OperationRef {
	r.t.Helper()
	acc, err := r.client.Conversations().Start(r.t.Context(), model.StartConversation{
		Command:   model.Command{Call: model.Call{Correlation: "relay-engine"}, Operation: "relay-engine-start"},
		Inbox:     engineInbox,
		Recipient: model.ExternalRef{Channel: testChannel, Namespace: relay.NamespaceSMS, Kind: relay.RouteKind, ID: "+15550100001"},
		Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Your appointment is tomorrow."}}},
	})
	if err != nil {
		r.t.Fatalf("Start: %v", err)
	}
	return acc.Operation.Ref
}

// step runs claim rounds until none claims anything.
func (r *engineRig) step() {
	r.t.Helper()
	for range 20 {
		if r.claimed() == 0 {
			return
		}
	}
	r.t.Fatal("work never ran out")
}

// claimed runs one claim round and returns how many items it ran.
func (r *engineRig) claimed() int {
	r.t.Helper()
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: testTenant, Actor: "worker"}, Call: model.Call{Correlation: "relay-worker"}}
	n, err := r.engine.Runtime().Step(r.t.Context(), call, uma.WorkerOptions{Owner: "relay-worker", Limit: 10})
	if err != nil {
		r.t.Fatalf("Step: %v", err)
	}
	return n
}

// settle advances the clock past every retry and settle window, stepping.
func (r *engineRig) settle() {
	r.t.Helper()
	for range 5 {
		r.clock.Advance(time.Hour)
		r.step()
	}
}

func (r *engineRig) op(ref model.OperationRef) model.Operation {
	r.t.Helper()
	op, err := r.client.Operations().Get(r.t.Context(), model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "relay-get"}, Ref: ref})
	if err != nil {
		r.t.Fatal(err)
	}
	return op
}

func (r *engineRig) reconcile(ref model.OperationRef) (model.Operation, error) {
	return r.client.Operations().Reconcile(r.t.Context(), model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "relay-reconcile"}, Ref: ref})
}

func wantState(t *testing.T, op model.Operation, state model.DeliveryState) {
	t.Helper()
	if op.State != state {
		t.Fatalf("operation %s (failure %v), want %s", op.State, op.Failure, state)
	}
}

// Without a lookup, behaviour is the one-POST contract: a lost answer is
// outcome_unknown for good, never sent again and never looked up, and
// Operations.Reconcile has nothing to ask.
func TestEngineWithoutLookupNeverResends(t *testing.T) {
	r := newEngineRig(t, newLossyGateway(t, true), false)
	ref := r.start()
	r.step()
	op := r.op(ref)
	wantState(t, op, model.Unknown)
	if op.Failure == nil || op.Failure.Retry != model.ReconcileFirst {
		t.Fatalf("failure %+v, want reconcile_first advice", op.Failure)
	}
	r.settle()
	wantState(t, r.op(ref), model.Unknown)
	if _, err := r.reconcile(ref); model.CodeOf(err) != model.Unsupported {
		t.Fatalf("Reconcile: %v, want unsupported", err)
	}
	r.settle()
	wantState(t, r.op(ref), model.Unknown)
	if posts, lookups := r.gw.counts(); posts != 1 || lookups != 0 {
		t.Fatalf("%d callbacks and %d lookups, want one callback and no lookup", posts, lookups)
	}
}

// Without a lookup the lost answer settles outcome_unknown in the pass
// that lost it: reconcile is declared for the type but disabled on the
// channel, so no reconcile pass is scheduled and none runs.
func TestEngineWithoutLookupSchedulesNoReconcilePass(t *testing.T) {
	r := newEngineRig(t, newLossyGateway(t, true), false)
	ref := r.start()
	r.step()
	op := r.op(ref)
	wantState(t, op, model.Unknown)
	if op.NextAttemptAt != nil {
		t.Fatalf("next attempt at %v, want none: the channel cannot reconcile", op.NextAttemptAt)
	}
	r.clock.Advance(time.Hour)
	if n := r.claimed(); n != 0 {
		t.Fatalf("claimed %d items after outcome_unknown, want no reconcile pass", n)
	}
}

// With a lookup, a lost answer the gateway applied is settled from it.
func TestEngineLookupSettlesALostAnswer(t *testing.T) {
	r := newEngineRig(t, newLossyGateway(t, true), true)
	ref := r.start()
	r.step()
	op := r.op(ref)
	wantState(t, op, model.Unknown)
	if op.NextAttemptAt == nil {
		t.Fatal("no reconcile pass scheduled with a lookup")
	}
	r.settle()
	op = r.op(ref)
	wantState(t, op, model.ProviderAccepted)
	if posts, lookups := r.gw.counts(); posts != 1 || lookups == 0 {
		t.Fatalf("%d callbacks and %d lookups, want one callback, looked up", posts, lookups)
	}
	if len(op.ProviderReferences) != 1 || !strings.HasPrefix(op.ProviderReferences[0].ID, "gw-") {
		t.Errorf("references %+v, want the gateway's id from the lookup", op.ProviderReferences)
	}
}

// With a lookup, a request the gateway never took is sent again, but only
// once not_found counts: inside the settle window it proves nothing.
func TestEngineLookupResendsOnlyAfterTheSettleWindow(t *testing.T) {
	r := newEngineRig(t, newLossyGateway(t, false), true)
	ref := r.start()
	r.step()
	wantState(t, r.op(ref), model.Unknown)
	op, err := r.reconcile(ref)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	wantState(t, op, model.Unknown)
	if posts, lookups := r.gw.counts(); posts != 1 || lookups != 1 {
		t.Fatalf("%d callbacks and %d lookups, want one of each: not_found inside the window resends nothing", posts, lookups)
	}
	r.clock.Advance(relay.ReconcileSettle + time.Second)
	if op, err = r.reconcile(ref); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	wantState(t, op, model.RetryPending)
	r.step()
	wantState(t, r.op(ref), model.ProviderAccepted)
	if posts, _ := r.gw.counts(); posts != 2 {
		t.Fatalf("%d callbacks, want the reply sent again once", posts)
	}
}
