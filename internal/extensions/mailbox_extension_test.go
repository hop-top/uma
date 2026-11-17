package extensions_test

import (
	"encoding/json"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/extensions"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
)

// The shape of the mail adapters' mailbox extensions: a read and an effect
// of a channel, each gated by a capability of its own name, the effect
// authorized as extension.execute and the read as extension.query.
const (
	boxList   = "com.example.chat.messages.mailboxes"
	boxCreate = "com.example.chat.messages.mailbox_create"
)

func mailboxDefs() []adapter.ExtensionDefinition {
	return []adapter.ExtensionDefinition{
		{
			Name: boxList, Version: 1, Kind: model.ExtensionKindRead,
			ResultSchema:       model.Extension{Name: boxList + ".result", Version: 1},
			RequiredCapability: boxList, AuthorizeAs: model.ActionExtensionQuery,
		},
		{
			Name: boxCreate, Version: 1, Kind: model.ExtensionKindEffect,
			InputSchema:        model.Extension{Name: boxCreate + ".input", Version: 1},
			RequiredCapability: boxCreate, AuthorizeAs: model.ActionExtensionExecute,
		},
	}
}

func newMailboxEnv(t *testing.T, declare ...model.Capability) *env {
	t.Helper()
	x := newEnv(t)
	x.f.extra = declare
	x.e = x.compose(append(definitions(), mailboxDefs()...))
	x.rt = runtime.New(x.e, extensions.Effector())
	x.rt.SetJitter(func(time.Duration) time.Duration { return 0 })
	var err error
	if x.c, err = enginetest.For(x.e, enginetest.PrincipalA()); err != nil {
		t.Fatal(err)
	}
	return x
}

func mailboxQuery(x *env) (model.Extension, error) {
	return x.c.Extensions().Query(x.ctx, model.QueryExtension{
		Call: model.Call{Correlation: "corr-boxes"}, Target: x.ch.Ref.Ref(), Audience: audience,
		Query: model.Extension{Name: boxList, Version: 1},
	})
}

func mailboxCreate(op model.OperationID, name string) model.ExecuteExtension {
	return model.ExecuteExtension{
		Command:  model.Command{Call: model.Call{Correlation: "corr-" + model.CorrelationID(op)}, Operation: op},
		Audience: audience,
		Action:   model.Extension{Name: boxCreate, Version: 1, Data: json.RawMessage(`{"name":"` + name + `"}`)},
	}
}

// A definition gated by a capability of its own name is served only by a
// channel whose adapter declares that capability and the builtin one, per
// channel, never by another's: a host reads the declaration to gate its
// verb.
func TestNamespacedCapabilityGatesTheRead(t *testing.T) {
	declared := newMailboxEnv(t, boxList, boxCreate)
	declared.f.result = func(model.QueryExtension) (model.Extension, error) {
		return model.Extension{Name: boxList + ".result", Version: 1, Data: json.RawMessage(`{"mailboxes":[]}`)}, nil
	}
	if _, err := mailboxQuery(declared); err != nil {
		t.Fatalf("a channel declaring the capability: %v", err)
	}
	// Declaring the builtin capability alone is not enough.
	undeclared := newMailboxEnv(t)
	_, err := mailboxQuery(undeclared)
	enginetest.RequireCode(t, err, model.Unsupported, boxList)
	if len(undeclared.f.queried()) != 0 {
		t.Fatal("the adapter was asked without the capability")
	}
}

// The create is a write like any other: a plugin actor can have it held for
// a person, and it reaches the adapter, once, only when approved.
func TestMailboxCreateCanBeHeld(t *testing.T) {
	x := newMailboxEnv(t, boxList, boxCreate)
	plugin, err := enginetest.For(x.e, model.Principal{Tenant: enginetest.TenantA, Actor: "plugin:triage"})
	if err != nil {
		t.Fatal(err)
	}
	req := mailboxCreate("op-box", "Clients/Acme")
	req.Target = x.ch.Ref.Ref()
	req.Hold = &model.HoldProposal{Reason: "new folder", TrustKey: "triage/mailbox"}
	held, err := plugin.Extensions().Execute(x.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if held.Operation.State != model.Held || held.Operation.Held == nil {
		t.Fatalf("operation = %+v, want held", held.Operation)
	}
	x.step(x.rt)
	if got := x.f.executed(); len(got) != 0 {
		t.Fatalf("a held create reached the adapter: %+v", got)
	}
	// The person's own create needs no hold.
	if _, err := x.c.Extensions().Execute(x.ctx, func() model.ExecuteExtension {
		r := mailboxCreate("op-own", "Own")
		r.Target = x.ch.Ref.Ref()
		return r
	}()); err != nil {
		t.Fatal(err)
	}
	x.step(x.rt)
	if got := x.f.executed(); len(got) != 1 || string(got[0].Action.Data) != `{"name":"Own"}` {
		t.Fatalf("executions = %+v, want the person's own create only", got)
	}
	if _, err := x.c.Operations().Approve(x.ctx, model.DecideHeld{
		Call: model.Call{Correlation: "corr-approve"}, Operation: held.Operation.Ref, ExpectedRevision: held.Operation.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	x.step(x.rt)
	got := x.f.executed()
	if len(got) != 2 || string(got[1].Action.Data) != `{"name":"Clients/Acme"}` || got[1].Route != (model.ExternalRef{Channel: x.ch.Ref}) {
		t.Fatalf("executions = %+v, want the approved create on the channel route", got)
	}
}
