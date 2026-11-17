package examples

import (
	"context"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store/memory"
)

func composition() uma.Config {
	return uma.Config{
		Store:      memory.New(memory.Options{}),
		Authorizer: AccessPolicy{},
		Selectors:  map[string]policy.ConversationSelector{"single": ScopedSelector{}},
		Adapters:   []adapter.Factory{SourceFactory{}},
		Clock:      portstest.NewFixedClock(time.Unix(0, 0)),
		IDs:        portstest.NewSequenceIDs(1),
	}
}

// The external sample adapter registers through the same path as a
// maintained one.
func TestSourceFactoryRegisters(t *testing.T) {
	engine, err := uma.New(composition())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Close(context.Background()) }()
	d := engine.Describe()
	if len(d) != 1 || d[0].Type != (SourceFactory{}).Describe().Type {
		t.Fatalf("Describe = %+v", d)
	}
	if issues := engine.ValidateChannel(d[0].Type, d[0].ConfigurationSchema); len(issues) != 0 {
		t.Fatalf("ValidateChannel: %v", issues)
	}
}

// DurableInbox requires a durable store; the memory store is rejected at
// composition, before any webhook is mounted.
func TestDurableInboxRejectsMemoryStore(t *testing.T) {
	_, _, err := DurableInbox(context.Background(), composition(),
		model.Principal{Tenant: "tenant", Actor: "service"}, model.ChannelRef{})
	if model.CodeOf(err) != model.Validation {
		t.Fatalf("err = %v, want validation", err)
	}
}
