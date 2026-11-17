package operations_test

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/retention"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// lookupReq names the key a reply claims: kind message.reply, target the
// conversation ref.
func lookupReq(r *enginetest.Rig, id model.OperationID) model.OperationLookup {
	return model.OperationLookup{
		Call: model.Call{Correlation: "lookup"}, Kind: model.ActionMessageReply,
		Target: r.Conv.Ref.Ref(), ID: id,
	}
}

func getOp(r *enginetest.Rig, ref model.OperationRef) (model.Operation, error) {
	return r.C.Operations().Get(r.Ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "get"}, Ref: ref})
}

// countingStore counts the units a test's backend opens.
type countingStore struct {
	store.Store
	views, transacts atomic.Int32
}

func (s *countingStore) View(ctx context.Context, t model.TenantID, fn func(store.Reader) error) error {
	s.views.Add(1)
	return s.Store.View(ctx, t, fn)
}

func (s *countingStore) Transact(ctx context.Context, t model.TenantID, fn func(store.Tx) error) error {
	s.transacts.Add(1)
	return s.Store.Transact(ctx, t, fn)
}

func countUnits(r *enginetest.Rig) *countingStore {
	c := &countingStore{Store: r.Store.Inner()}
	r.Store.Set(c)
	return c
}

// A lost acceptance is recovered by the key the command claimed.
func TestLookupRecoversOwnOperation(t *testing.T) {
	r := newRig(t)
	a := r.Reply("op-1", "hello")
	r.Reply("op-2", "other")
	got, err := r.C.Operations().Lookup(r.Ctx, lookupReq(r, "op-1"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, a.Operation) {
		t.Fatalf("Lookup = %+v, want %+v", got, a.Operation)
	}
}

// Lookup returns exactly what Get returns for the found ref, also after
// the operation has moved on.
func TestLookupMatchesGet(t *testing.T) {
	r := newRig(t)
	a := r.Reply("op-1", "hello")
	r.Step("w")
	got, err := r.C.Operations().Lookup(r.Ctx, lookupReq(r, "op-1"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := getOp(r, a.Operation.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if want.State != model.ProviderAccepted || !reflect.DeepEqual(got, want) {
		t.Fatalf("Lookup = %+v, Get = %+v; want equal provider_accepted operations", got, want)
	}
	got.Result.ID = "mutated"
	if again := r.Op(a.Operation.Ref); again.Result.ID == "mutated" {
		t.Fatal("Lookup result aliases stored state")
	}
}

// The caller is part of the key: another actor's equal kind, target and ID
// is not found, and each caller finds only its own operation.
func TestLookupOtherCallerNotFound(t *testing.T) {
	r := newRig(t)
	other, err := enginetest.For(r.E, model.Principal{Tenant: tenantA, Actor: "actor-2"})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := other.Messages().Reply(r.Ctx, r.ReplyReq("op-1", "theirs"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.C.Operations().Lookup(r.Ctx, lookupReq(r, "op-1"))
	requireCode(t, err, model.NotFound, "")

	mine := r.Reply("op-1", "mine")
	got, err := r.C.Operations().Lookup(r.Ctx, lookupReq(r, "op-1"))
	if err != nil || got.Ref != mine.Operation.Ref {
		t.Fatalf("own Lookup = %+v, %v; want %v", got.Ref, err, mine.Operation.Ref)
	}
	got, err = other.Operations().Lookup(r.Ctx, lookupReq(r, "op-1"))
	if err != nil || got.Ref != theirs.Operation.Ref {
		t.Fatalf("other caller's Lookup = %+v, %v; want %v", got.Ref, err, theirs.Operation.Ref)
	}
}

// A target in another tenant is not found before any read or policy call.
func TestLookupOtherTenantNotFound(t *testing.T) {
	r := newRig(t)
	r.Reply("op-1", "hello")
	req := lookupReq(r, "op-1")
	req.Target.Tenant = tenantB
	units := countUnits(r)
	before := len(r.Policy.Actions())
	_, err := r.C.Operations().Lookup(r.Ctx, req)
	requireCode(t, err, model.NotFound, "resource not found")
	if n := units.views.Load() + units.transacts.Load(); n != 0 {
		t.Fatalf("store units = %d, want none before the scope check", n)
	}
	if n := len(r.Policy.Actions()) - before; n != 0 {
		t.Fatalf("policy calls = %d, want none", n)
	}
}

// A key never claimed, or claimed under another kind, target or ID, is
// not found, as is a kind nothing registers: that is not a validation
// error.
func TestLookupUnclaimedKeyNotFound(t *testing.T) {
	r := newRig(t)
	r.Reply("op-1", "hello")
	for name, mutate := range map[string]func(*model.OperationLookup){
		"id":           func(q *model.OperationLookup) { q.ID = "op-2" },
		"kind":         func(q *model.OperationLookup) { q.Kind = model.ActionMessageNote },
		"unregistered": func(q *model.OperationLookup) { q.Kind = "com.example.never" },
		"unknown core": func(q *model.OperationLookup) { q.Kind = "message.never" },
		"target":       func(q *model.OperationLookup) { q.Target = r.Conv.Ref.Inbox.Ref() },
		"target id":    func(q *model.OperationLookup) { q.Target.ID = inboxTwo },
	} {
		t.Run(name, func(t *testing.T) {
			q := lookupReq(r, "op-1")
			mutate(&q)
			_, want := getOp(r, model.OperationRef{Tenant: tenantA, ID: inboxTwo})
			_, err := r.C.Operations().Lookup(r.Ctx, q)
			requireCode(t, err, model.NotFound, "")
			if !reflect.DeepEqual(err, want) {
				t.Fatalf("Lookup error = %#v, want Get's not-found %#v", err, want)
			}
		})
	}
}

// A key purged past the operations horizon is not found.
func TestLookupPurgedKeyNotFound(t *testing.T) {
	const horizon = time.Hour
	r := newRig(t, func(c *engine.Config, _ *enginetest.OutFactory) {
		c.Retention = engine.RetentionPolicy{
			Events: horizon, Operations: horizon, Ingress: horizon, Attempts: horizon,
			Staging: horizon, Exports: horizon, Contacts: horizon,
		}
	})
	r.Reply("op-1", "hello")
	r.Step("w")
	if _, err := r.C.Operations().Lookup(r.Ctx, lookupReq(r, "op-1")); err != nil {
		t.Fatalf("Lookup before purge: %v", err)
	}
	r.Clock.Advance(2 * horizon)
	rep, err := retention.Purge(r.Ctx, r.E, enginetest.PrincipalA())
	if err != nil || rep.Operations != 1 {
		t.Fatalf("Purge = %+v, %v; want the operation purged", rep, err)
	}
	_, err = r.C.Operations().Lookup(r.Ctx, lookupReq(r, "op-1"))
	requireCode(t, err, model.NotFound, "")
}

// operation.read is checked on the found operation exactly as Get checks
// it, and a denial is Get's error.
func TestLookupDeniedByPolicy(t *testing.T) {
	r := newRig(t)
	a := r.Reply("op-1", "hello")
	r.Policy.Set(true)
	before := len(r.Policy.Actions())
	_, getErr := getOp(r, a.Operation.Ref)
	getActions := r.Policy.Actions()[before:]
	before = len(r.Policy.Actions())
	_, err := r.C.Operations().Lookup(r.Ctx, lookupReq(r, "op-1"))
	requireCode(t, err, model.Unauthorized, "")
	if !reflect.DeepEqual(err, getErr) {
		t.Fatalf("Lookup error = %#v, want Get's %#v", err, getErr)
	}
	if got := r.Policy.Actions()[before:]; !reflect.DeepEqual(got, getActions) ||
		!reflect.DeepEqual(got, []string{model.ActionOperationRead}) {
		t.Fatalf("Lookup policy actions = %v, Get = %v; want [%s]", got, getActions, model.ActionOperationRead)
	}
}

// Lookup is read-only: no unit of work, no event.
func TestLookupWritesNothing(t *testing.T) {
	r := newRig(t)
	r.Reply("op-1", "hello")
	events := len(r.Events())
	units := countUnits(r)
	for _, id := range []model.OperationID{"op-1", "op-missing"} {
		_, _ = r.C.Operations().Lookup(r.Ctx, lookupReq(r, id))
	}
	if n := units.transacts.Load(); n != 0 {
		t.Fatalf("transactions = %d, want none", n)
	}
	if n := units.views.Load(); n != 2 {
		t.Fatalf("views = %d, want one per lookup", n)
	}
	if n := len(r.Events()); n != events {
		t.Fatalf("events = %d, want %d", n, events)
	}
}

// A malformed request is a validation error before any read.
func TestLookupValidatesBeforeReading(t *testing.T) {
	r := newRig(t)
	units := countUnits(r)
	q := lookupReq(r, "op-1")
	q.Kind = ""
	_, err := r.C.Operations().Lookup(r.Ctx, q)
	requireCode(t, err, model.Validation, "kind")
	if n := units.views.Load(); n != 0 {
		t.Fatalf("views = %d, want none", n)
	}
}

// After Close, Lookup refuses with the closed-engine error.
func TestLookupAfterCloseIsClosed(t *testing.T) {
	r := newRig(t)
	if err := r.E.Close(r.Ctx); err != nil {
		t.Fatal(err)
	}
	q := lookupReq(r, "op-1")
	q.Kind = ""
	_, err := r.C.Operations().Lookup(r.Ctx, q)
	if !reflect.DeepEqual(err, engine.ErrClosed()) {
		t.Fatalf("Lookup after Close = %v, want %v", err, engine.ErrClosed())
	}
}

// detailedStore answers every key lookup with a store-specific not-found.
type detailedStore struct{ store.Store }

func (s detailedStore) View(ctx context.Context, t model.TenantID, fn func(store.Reader) error) error {
	return s.Store.View(ctx, t, func(r store.Reader) error { return fn(detailedReader{r}) })
}

type detailedReader struct{ store.Reader }

func (detailedReader) OperationByKey(context.Context, store.OperationKey) (store.OperationRecord, error) {
	return store.OperationRecord{}, model.NewError(model.NotFound, "no ledger row for key")
}

// A store's not-found becomes the engine's, whatever detail the store
// gives.
func TestLookupMapsStoreNotFound(t *testing.T) {
	r := newRig(t)
	r.Store.Set(detailedStore{r.Store.Inner()})
	_, err := r.C.Operations().Lookup(r.Ctx, lookupReq(r, "op-1"))
	if !reflect.DeepEqual(err, engine.ErrNotFound()) {
		t.Fatalf("Lookup = %#v, want %#v", err, engine.ErrNotFound())
	}
}
