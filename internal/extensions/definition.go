// Package extensions implements the Extensions service: registered,
// versioned extension reads (Query) and effects (Execute) on a channel,
// inbox or conversation, under the same scope, policy and operation
// ledger as the builtin services, and the runtime effector that executes
// queued effects.
//
// A request's extension (QueryExtension.Query, ExecuteExtension.Action)
// names a registered definition by name and version. An unregistered name
// is model.Unsupported; a registered name in another version, or a
// definition of another kind, is model.SchemaMismatch. Neither reaches the
// adapter, is stored or is authorized. Extension data is opaque to core: it
// is checked for JSON validity and size, then handed on unchanged, so
// fields core does not know survive, but only within the declared name and
// version and under the request's audience.
package extensions

import (
	"slices"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
)

// definition returns the registered definition x names, which must be of
// kind.
func definition(e *engine.Engine, x model.Extension, kind string) (adapter.ExtensionDefinition, error) {
	versions := e.ExtensionVersions(x.Name)
	if len(versions) == 0 {
		return adapter.ExtensionDefinition{}, &model.Error{
			Code: model.Unsupported, SafeMessage: "extension not registered", Retry: model.NoRetry,
		}
	}
	for _, d := range versions {
		if d.Version != x.Version {
			continue
		}
		if d.Kind != kind {
			return adapter.ExtensionDefinition{}, mismatch("extension is not " + article(kind))
		}
		return d, nil
	}
	return adapter.ExtensionDefinition{}, mismatch("extension version not registered")
}

func article(kind string) string {
	if kind == model.ExtensionKindEffect {
		return "an effect"
	}
	return "a read"
}

func mismatch(msg string) error {
	return &model.Error{Code: model.SchemaMismatch, SafeMessage: msg, Retry: model.NoRetry}
}

// resultSchema is the name and version a read's result carries: the
// declared result schema, or the definition's own when none is declared.
func resultSchema(d adapter.ExtensionDefinition) (string, uint32) {
	if d.ResultSchema.Name != "" {
		return d.ResultSchema.Name, d.ResultSchema.Version
	}
	return d.Name, d.Version
}

// capabilities are the capabilities a channel's adapter must declare to
// serve d: the builtin capability of the call plus the definition's.
func capabilities(call model.Capability, d adapter.ExtensionDefinition) []model.Capability {
	out := []model.Capability{call}
	if !slices.Contains(out, d.RequiredCapability) {
		out = append(out, d.RequiredCapability)
	}
	return out
}

// declared fails with model.Unsupported unless the adapter registered for
// t declares every capability in caps.
func declared(e *engine.Engine, t model.TypeKey, caps []model.Capability) error {
	_, desc, ok := e.Adapter(t)
	if !ok {
		return &model.Error{Code: model.Unsupported, SafeMessage: "channel adapter not registered", Retry: model.NoRetry}
	}
	for _, c := range caps {
		if !slices.Contains(desc.Capabilities, c) {
			return &model.Error{Code: model.Unsupported, SafeMessage: "channel adapter does not declare " + string(c), Retry: model.NoRetry}
		}
	}
	return nil
}
