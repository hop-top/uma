package conformance

import (
	"errors"
	"testing"
	"time"

	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

func TestDurableSuiteGate(t *testing.T) {
	restart := func(_ *testing.T, fx StoreFixture) StoreFixture { return fx }
	cases := []struct {
		name    string
		p       store.Properties
		restart RestartFunc
		want    error
	}{
		{"durable with restart", store.Properties{Durable: true, Backend: "x"}, restart, nil},
		{"no restart", store.Properties{Durable: true, Backend: "x"}, nil, errNoRestart},
		{"not durable", store.Properties{Backend: "memory"}, restart, errNotDurable},
	}
	for _, c := range cases {
		if got := durableSuiteError(c.p, c.restart); !errors.Is(got, c.want) {
			t.Errorf("%s: error = %v, want %v", c.name, got, c.want)
		}
	}
}

// sharedState is a self-test double, not a durable backend: every restart
// hands out a new handle on one in-process memory store, so the Restart
// cases run end to end. It proves the cases are coherent, never that any
// store is durable.
type sharedState struct{ *memory.Store }

func (s sharedState) Properties() store.Properties {
	p := s.Store.Properties()
	p.Durable, p.Backend = true, "conformance-self-test"
	return p
}

func TestDurableSuiteSelfTest(t *testing.T) {
	factory := func(*testing.T) StoreFixture {
		clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0).UTC())
		return StoreFixture{
			Store:   &sharedState{memory.New(memory.Options{Clock: clock})},
			Now:     clock.Now,
			Advance: func(d time.Duration) { clock.Advance(d) },
		}
	}
	restart := func(_ *testing.T, fx StoreFixture) StoreFixture {
		next := fx
		next.Store = &sharedState{fx.Store.(*sharedState).Store}
		return next
	}
	RunDurableSuite(t, factory, restart)
}
