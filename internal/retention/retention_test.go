package retention_test

import (
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/retention"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

const horizon = time.Hour

func withRetention(p engine.RetentionPolicy) enginetest.RigOption {
	return func(c *engine.Config, _ *enginetest.OutFactory) { c.Retention = p }
}

var every = engine.RetentionPolicy{
	Events: horizon, Operations: horizon, Ingress: horizon, Attempts: horizon, Staging: horizon, Exports: horizon,
	Contacts: horizon,
}

func principal() model.Principal { return enginetest.PrincipalA() }

// Purge deletes what is past its horizon and keeps what work still needs:
// an old settled reply's operation, evidence and event go; an old reply
// whose work is claimed and a recent settled one stay; the event base moves
// past the purged prefix; and the purged operation ID is a new command
// afterwards.
func TestPurgeDeletesPastTheHorizonOnly(t *testing.T) {
	r := enginetest.NewRig(t, withRetention(every))
	settled := r.Reply("op-settled", "hello")
	r.Step("w")
	enginetest.RequireState(t, r.Op(settled.Operation.Ref), model.ProviderAccepted, "")
	queued := r.Reply("op-queued", "later")
	old := len(r.Events())
	r.Clock.Advance(2 * horizon)
	if n := len(r.Claim("other")); n != 1 {
		t.Fatalf("claimed %d items, want the old queued reply", n)
	}
	recent := r.Reply("op-recent", "now")
	r.Step("w")
	enginetest.RequireState(t, r.Op(recent.Operation.Ref), model.ProviderAccepted, "")
	recentEvents := len(r.Events()) - old

	rep, err := retention.Purge(r.Ctx, r.E, principal())
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if rep.Operations != 1 || rep.Attempts != 2 || rep.Events != old || rep.EventBase == "" {
		t.Fatalf("report = %+v, want the settled reply's operation, 2 evidence entries and %d events", rep, old)
	}
	_, err = r.C.Operations().Get(r.Ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "c"}, Ref: settled.Operation.Ref})
	enginetest.RequireCode(t, err, model.NotFound, "")
	enginetest.RequireState(t, r.Op(queued.Operation.Ref), model.Queued, "")
	enginetest.RequireState(t, r.Op(recent.Operation.Ref), model.ProviderAccepted, "")
	if n := len(r.Evidence(recent.Operation.Ref)); n != 2 {
		t.Errorf("recent evidence = %d entries, want 2 kept", n)
	}
	err = r.E.Store().View(r.Ctx, enginetest.TenantA, func(rd store.Reader) error {
		base, err := rd.EventBase(r.Ctx)
		if err != nil {
			return err
		}
		if base != rep.EventBase {
			t.Errorf("store base = %q, report says %q", base, rep.EventBase)
		}
		kept, err := rd.Events(r.Ctx, model.Filter{}, base, 10)
		if err == nil && (recentEvents == 0 || len(kept.Items) != recentEvents) {
			t.Errorf("retained events = %d, want the %d recent ones", len(kept.Items), recentEvents)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	again := r.Reply("op-settled", "hello")
	if again.Replayed || again.Operation.Ref == settled.Operation.Ref {
		t.Fatalf("reused operation ID past the horizon = %+v, want a new operation", again)
	}
	if rep2, err := retention.Purge(r.Ctx, r.E, principal()); err != nil || rep2 != (retention.Report{EventBase: rep.EventBase}) {
		t.Fatalf("second purge = %+v, %v; want nothing more", rep2, err)
	}
}

// Every horizon is required; there are no defaults. A negative one is a
// composition error.
func TestPurgeRequiresEveryHorizon(t *testing.T) {
	clock := portstest.NewFixedClock(time.Unix(enginetest.TestEpochS, 0))
	compose := func(p engine.RetentionPolicy) (*engine.Engine, error) {
		return engine.New(engine.Config{
			Store: memory.New(memory.Options{Clock: clock}), Authorizer: &enginetest.Allow{},
			Clock: clock, IDs: portstest.NewSequenceIDs(1), Retention: p,
		})
	}
	for field, p := range map[string]engine.RetentionPolicy{
		"Retention.Events":     {Operations: horizon, Ingress: horizon, Attempts: horizon, Staging: horizon, Exports: horizon},
		"Retention.Operations": {Events: horizon, Ingress: horizon, Attempts: horizon, Staging: horizon, Exports: horizon},
		"Retention.Ingress":    {Events: horizon, Operations: horizon, Attempts: horizon, Staging: horizon, Exports: horizon},
		"Retention.Attempts":   {Events: horizon, Operations: horizon, Ingress: horizon, Staging: horizon, Exports: horizon},
		"Retention.Staging":    {Events: horizon, Operations: horizon, Ingress: horizon, Attempts: horizon, Exports: horizon},
		"Retention.Exports":    {Events: horizon, Operations: horizon, Ingress: horizon, Attempts: horizon, Staging: horizon},
	} {
		e, err := compose(p)
		if err != nil {
			t.Fatal(err)
		}
		_, err = retention.Purge(t.Context(), e, principal())
		enginetest.RequireCode(t, err, model.Validation, field)
		p2 := every
		switch field {
		case "Retention.Events":
			p2.Events = -1
		case "Retention.Operations":
			p2.Operations = -1
		case "Retention.Ingress":
			p2.Ingress = -1
		case "Retention.Attempts":
			p2.Attempts = -1
		case "Retention.Staging":
			p2.Staging = -1
		case "Retention.Exports":
			p2.Exports = -1
		}
		_, err = compose(p2)
		enginetest.RequireCode(t, err, model.Validation, field+": must not be negative")
	}
	e, err := compose(every)
	if err != nil {
		t.Fatal(err)
	}
	_, err = retention.Purge(t.Context(), e, model.Principal{})
	enginetest.RequireCode(t, err, model.Validation, "")
	if err := e.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err = retention.Purge(t.Context(), e, principal())
	enginetest.RequireCode(t, err, model.Unavailable, "closed")
}
