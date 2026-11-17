package runtime_test

import (
	"context"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
)

const depsType model.TypeKey = "com.example.ingress.deps"

// depsAdapter keeps the dependencies of the last instance it opened.
type depsAdapter struct{ deps adapter.Dependencies }

func (a *depsAdapter) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: depsType, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: string(depsType) + ".ingress-test", Version: 1},
		Capabilities:        []model.Capability{model.CapabilityReceiveHTTP},
		Events:              []string{model.EventMessageReceived},
		IdentityNamespaces:  []string{ingressNS},
	}
}
func (*depsAdapter) Validate(model.Extension) []model.ValidationIssue { return nil }
func (a *depsAdapter) Open(_ context.Context, _ model.Channel, d adapter.Dependencies) (adapter.Instance, error) {
	a.deps = d
	return &hookInstance{hookBare{a: &hookAdapter{}}}, nil
}

func openProtocol(t *testing.T, h *harness, a *depsAdapter, ch model.Channel) adapter.ProtocolState {
	t.Helper()
	inst, _, _, err := runtime.OpenOutbound(h.Ctx, h.E, ch)
	if err != nil {
		t.Fatalf("OpenOutbound: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	if a.deps.Protocol == nil {
		t.Fatal("instance opened without protocol state")
	}
	return a.deps.Protocol
}

// Every instance gets its channel's protocol state from the store:
// compare-and-swap on engine revisions, scoped to the channel, with the
// engine's own keys out of reach.
func TestInstanceProtocolStateIsTheChannelsStoreState(t *testing.T) {
	a := &depsAdapter{}
	h := newHarness(t, depsType, func(c *engine.Config) { c.Adapters = append(c.Adapters, a) })
	p := openProtocol(t, h, a, h.Channel)
	v := model.Extension{Name: "com.example.window", Version: 1, Data: []byte(`{"at":1}`)}

	if _, _, err := p.Load(h.Ctx, "window/u1"); model.CodeOf(err) != model.NotFound {
		t.Fatalf("Load of a missing key = %v, want not_found", err)
	}
	if err := p.CompareAndSwap(h.Ctx, "window/u1", "", v); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, rev, err := p.Load(h.Ctx, "window/u1")
	if err != nil || rev == "" || string(got.Data) != `{"at":1}` || got.Name != v.Name {
		t.Fatalf("Load = %+v %q %v, want the stored value under an engine revision", got, rev, err)
	}
	requireCode(t, p.CompareAndSwap(h.Ctx, "window/u1", "", v), model.Conflict, "")
	requireCode(t, p.CompareAndSwap(h.Ctx, "window/u1", "stale", v), model.Conflict, "")
	v.Data = []byte(`{"at":2}`)
	if err := p.CompareAndSwap(h.Ctx, "window/u1", rev, v); err != nil {
		t.Fatalf("swap at the current revision: %v", err)
	}

	requireCode(t, p.CompareAndSwap(h.Ctx, "uma.receiver.cursor", "", v), model.Unauthorized, "")
	_, _, err = p.Load(h.Ctx, "uma.receiver.cursor")
	requireCode(t, err, model.Unauthorized, "")
	requireCode(t, p.CompareAndSwap(h.Ctx, "", "", v), model.Validation, "")
	requireCode(t, p.CompareAndSwap(h.Ctx, "k", "", model.Extension{}), model.Validation, "")

	other := h.PutChannel(depsType)
	q := openProtocol(t, h, a, other)
	if _, _, err := q.Load(h.Ctx, "window/u1"); model.CodeOf(err) != model.NotFound {
		t.Fatalf("another channel's instance read the key: %v", err)
	}
}
