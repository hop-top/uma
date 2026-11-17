package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// records reads id's link records from the store, tombstones included.
func (env *contactsEnv) records(ref model.IdentityRef) []model.IdentityLink {
	env.t.Helper()
	var out []model.IdentityLink
	env.view(func(r store.Reader) error {
		p, err := r.IdentityLinks(env.ctx, store.LinkFilter{Identity: &ref}, model.PageRequest{Limit: 100})
		out = p.Items
		return err
	})
	return out
}

func (env *contactsEnv) linkedEvents() []model.Event {
	env.t.Helper()
	var out []model.Event
	env.view(func(r store.Reader) error {
		after, err := r.EventBase(env.ctx)
		if err != nil {
			return err
		}
		for {
			p, err := r.Events(env.ctx, model.Filter{Kinds: []string{model.EventIdentityLinked}}, after, 100)
			if err != nil {
				return err
			}
			out = append(out, p.Items...)
			if p.Complete || len(p.Items) == 0 {
				return nil
			}
			after = p.Next
		}
	})
	return out
}

func (env *contactsEnv) eventHead() model.Cursor {
	env.t.Helper()
	var c model.Cursor
	env.view(func(r store.Reader) (err error) { c, err = r.EventHead(env.ctx); return err })
	return c
}

// claimed reports whether the principal's operation op of kind on target
// was claimed.
func (env *contactsEnv) claimed(kind string, target model.Ref, op model.OperationID) bool {
	env.t.Helper()
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) error {
		_, err := r.OperationByKey(env.ctx, store.OperationKey{Tenant: serviceTenant, Caller: contactsActor, Kind: kind, Target: target, ID: op})
		return err
	})
	if err != nil && !model.IsCode(err, model.NotFound) {
		env.t.Fatalf("OperationByKey: %v", err)
	}
	return err == nil
}

func (env *contactsEnv) history(f model.Filter, limit int) []model.IdentityLink {
	env.t.Helper()
	return drain(env, env.client.Contacts().History, f, limit)
}

func (env *contactsEnv) identities(f model.Filter, limit int) []model.Identity {
	env.t.Helper()
	return drain(env, env.client.Contacts().Identities, f, limit)
}

func (env *contactsEnv) contacts(f model.Filter, limit int) []model.Contact {
	env.t.Helper()
	return drain(env, env.client.Contacts().List, f, limit)
}

// drain pages list under f to the end, requiring every page to carry its
// items, cursor and completeness only.
func drain[T any](env *contactsEnv, list func(context.Context, model.ListRequest) (model.Page[T], error), f model.Filter, limit int) []T {
	env.t.Helper()
	var out []T
	var after model.Cursor
	for range 1000 {
		p, err := list(env.ctx, model.ListRequest{Call: model.Call{Correlation: "corr"}, Filter: f, Page: model.PageRequest{Limit: limit, After: after}})
		if err != nil {
			env.t.Fatalf("list: %v", err)
		}
		requireNoTotals(env.t, p)
		out = append(out, p.Items...)
		if p.Complete {
			return out
		}
		after = p.Next
	}
	env.t.Fatal("list never completed")
	return nil
}

// requireNoTotals fails when a page serializes anything beyond its items,
// cursor and completeness: no count can reveal hidden items.
func requireNoTotals[T any](t *testing.T, p model.Page[T]) {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	for k := range keys {
		if k != "items" && k != "next" && k != "complete" {
			t.Fatalf("page exposes %q", k)
		}
	}
}

func requireReconcile(t *testing.T, err error) {
	t.Helper()
	serviceCode(t, err, model.Conflict)
	var me *model.Error
	if !errors.As(err, &me) || me.Retry != model.ReconcileFirst {
		t.Fatalf("retry advice of %v, want reconcile_first", err)
	}
}

// unchanged fails unless id still has its revision and contact, and the
// identity.linked log has n events.
func (env *contactsEnv) unchanged(before model.Identity, n int) {
	env.t.Helper()
	if got := env.identity(before.Ref); got.Revision != before.Revision || got.Contact != before.Contact || got.LinkAssurance != before.LinkAssurance {
		env.t.Fatalf("identity changed: %+v, was %+v", got, before)
	}
	if got := len(env.linkedEvents()); got != n {
		env.t.Fatalf("identity.linked events = %d, want %d", got, n)
	}
}

// purge runs Runtime.Purge as the staff principal.
func (env *contactsEnv) purge() uma.PurgeReport {
	env.t.Helper()
	rep, err := env.engine.Runtime().Purge(env.ctx, uma.RuntimeCall{
		Principal: model.Principal{Tenant: serviceTenant, Actor: contactsActor}, Call: model.Call{Correlation: "purge"},
	})
	if err != nil {
		env.t.Fatalf("Purge: %v", err)
	}
	return rep
}

// pastHorizon moves the store and engine clocks past every horizon.
func (env *contactsEnv) pastHorizon() {
	env.fx.Advance(2 * serviceHorizon)
	env.clock.Advance(2 * serviceHorizon)
}

// run runs Runtime.Run workers until done accepts the operation, then
// stops them and returns it.
func (env *contactsEnv) run(ref model.OperationRef, done func(model.Operation) bool) model.Operation {
	env.t.Helper()
	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: serviceTenant, Actor: "worker"}, Call: model.Call{Correlation: "run"}}
	ended := make(chan error, 1)
	go func() {
		ended <- env.engine.Runtime().Run(ctx, call, uma.WorkerOptions{Owner: "worker", Limit: 4, Poll: time.Millisecond})
	}()
	deadline := time.Now().Add(time.Minute)
	for {
		op, err := env.client.Operations().Get(env.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "c"}, Ref: ref})
		if err != nil {
			env.t.Fatalf("Operations.Get: %v", err)
		}
		if done(op) {
			cancel()
			<-ended
			return op
		}
		if time.Now().After(deadline) {
			env.t.Fatalf("operation stuck at %+v", op)
		}
		select {
		case err := <-ended:
			env.t.Fatalf("Run returned early: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// settled reports an operation no longer waiting for a worker.
func settled(op model.Operation) bool {
	return op.State != model.Queued && op.State != model.RetryPending
}
