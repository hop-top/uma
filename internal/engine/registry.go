package engine

import (
	"errors"
	"fmt"
	"slices"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// registry holds an engine's immutable startup registrations: adapter
// factories with a snapshot of their descriptors, and extension
// definitions. It is built once by New and never changes; there is no
// global registry and no registration at import.
type registry struct {
	adapters   []registration
	byType     map[model.TypeKey]int
	extensions []adapter.ExtensionDefinition
	// eventKinds maps each namespaced event kind with a registered
	// event-kind extension definition to the policy action its versions
	// share.
	eventKinds map[string]string
}

type registration struct {
	factory adapter.Factory
	desc    adapter.Descriptor
}

type schemaKey struct {
	name    string
	version uint32
}

// newRegistry validates and copies factories and definitions. It calls each
// factory's Describe once and nothing else. Every rejection is a
// model.Validation error naming the offending Config field; no override flag
// or last-wins rule exists.
func newRegistry(factories []adapter.Factory, defs []adapter.ExtensionDefinition) (*registry, error) {
	r := &registry{byType: make(map[model.TypeKey]int, len(factories))}
	seenDefs := map[schemaKey]bool{}
	required := map[model.Capability]bool{}
	eventDefs := map[string]string{}
	for i, d := range defs {
		field := fmt.Sprintf("Extensions[%d]", i)
		if err := checkDefinition(field, d); err != nil {
			return nil, err
		}
		k := schemaKey{d.Name, d.Version}
		if seenDefs[k] {
			return nil, ConfigError(field, "duplicate name and version")
		}
		if err := checkVersions(field, d, r.extensions); err != nil {
			return nil, err
		}
		seenDefs[k] = true
		required[d.RequiredCapability] = true
		if d.Kind == model.ExtensionKindEvent {
			eventDefs[d.Name] = d.AuthorizeAs
		}
		r.extensions = append(r.extensions, cloneDefinition(d))
	}
	configSchemas := map[schemaKey]bool{}
	for i, f := range factories {
		field := fmt.Sprintf("Adapters[%d]", i)
		if f == nil {
			return nil, ConfigError(field, "nil factory")
		}
		d := CloneDescriptor(f.Describe())
		if err := model.ValidateTypeKey(d.Type); err != nil {
			return nil, ConfigError(field, SafeMessage(err))
		}
		if _, dup := r.byType[d.Type]; dup {
			return nil, ConfigError(field, "type: duplicate")
		}
		if d.ContractMajor != adapter.ContractMajor {
			return nil, ConfigError(field, "contract major: unsupported")
		}
		if err := checkCapabilities(field, d.Capabilities, required); err != nil {
			return nil, err
		}
		if err := checkEvents(field, d.Events, eventDefs); err != nil {
			return nil, err
		}
		if err := checkSenderKinds(field, d.SenderKinds); err != nil {
			return nil, err
		}
		if d.ReconcileSettle < 0 {
			return nil, ConfigError(field, "reconcile settle window: negative")
		}
		if err := checkExchange(field, d.Exchange); err != nil {
			return nil, err
		}
		if err := checkWorkItemActs(field, d); err != nil {
			return nil, err
		}
		if s := d.ConfigurationSchema; s.Name != "" {
			k := schemaKey{s.Name, s.Version}
			if configSchemas[k] {
				return nil, ConfigError(field, "configuration schema: registered by another adapter")
			}
			if slices.ContainsFunc(r.extensions, func(x adapter.ExtensionDefinition) bool { return x.Name == s.Name }) {
				return nil, ConfigError(field, "configuration schema: registered as an extension definition")
			}
			configSchemas[k] = true
		}
		r.byType[d.Type] = len(r.adapters)
		r.adapters = append(r.adapters, registration{factory: f, desc: d})
	}
	r.eventKinds = eventDefs
	return r, nil
}

// EventKind reports whether k is an event kind this engine can emit: a
// builtin kind, or a namespaced kind with a registered event-kind
// extension definition.
func (e *Engine) EventKind(k string) bool {
	_, registered := e.reg.eventKinds[k]
	return registered || slices.Contains(builtinEvents(), k)
}

// EventAction returns the policy action of the registered event kind k:
// the AuthorizeAs its definition versions share. A subscriber receives an
// event of the kind only when it holds this action on the event's
// resource, in addition to reading the resource. ok is false for builtin
// kinds and names with no event-kind definition.
func (e *Engine) EventAction(k string) (action string, ok bool) {
	action, ok = e.reg.eventKinds[k]
	return action, ok
}

func checkDefinition(field string, d adapter.ExtensionDefinition) error {
	if err := model.ValidateExtensionName(d.Name); err != nil {
		return ConfigError(field, SafeMessage(err))
	}
	if d.Version == 0 {
		return ConfigError(field, "version: must be positive")
	}
	switch d.Kind {
	case model.ExtensionKindRead, model.ExtensionKindEffect, model.ExtensionKindEvent:
	default:
		return ConfigError(field, "kind: must be read, effect or event")
	}
	class, builtin := builtinActionClasses()[d.AuthorizeAs]
	switch {
	case d.AuthorizeAs == "":
		return ConfigError(field, "authorize as: required")
	case builtin:
		switch {
		case d.Kind == model.ExtensionKindEffect && class == classRead:
			return ConfigError(field, "authorize as: an effect cannot authorize as a read action")
		case d.Kind == model.ExtensionKindEffect && class == classCore:
			return ConfigError(field, "authorize as: an effect cannot authorize as a core-only action")
		case d.Kind == model.ExtensionKindEvent && class != classRead:
			return ConfigError(field, "authorize as: an event kind authorizes as a read action")
		}
	default:
		if err := model.ValidateExtensionName(d.AuthorizeAs); err != nil {
			return ConfigError(field, "authorize as: "+SafeMessage(err))
		}
	}
	if d.RequiredCapability == "" {
		return ConfigError(field, "required capability: required")
	}
	if err := checkCapabilityName(d.RequiredCapability); err != nil {
		return ConfigError(field, "required capability: "+SafeMessage(err))
	}
	for _, s := range []struct {
		label string
		ext   model.Extension
	}{{"input schema", d.InputSchema}, {"result schema", d.ResultSchema}} {
		if d.Kind == model.ExtensionKindEvent && extensionSet(s.ext) {
			// The definition itself is an event's payload schema.
			return ConfigError(field, s.label+": an event kind declares none")
		}
		if s.ext.Name == "" {
			continue
		}
		if err := model.ValidateExtensionName(s.ext.Name); err != nil {
			return ConfigError(field, s.label+": "+SafeMessage(err))
		}
	}
	return nil
}

// extensionSet reports whether any field of x is set.
func extensionSet(x model.Extension) bool {
	return x.Name != "" || x.Version != 0 || len(x.Data) > 0
}

// checkVersions rejects a definition that conflicts with an earlier
// version of its name: every version shares one kind and one policy
// action, so choosing a version never chooses a weaker grant or turns a
// read into an effect. Schemas and the required capability may change.
func checkVersions(field string, d adapter.ExtensionDefinition, earlier []adapter.ExtensionDefinition) error {
	for _, x := range earlier {
		switch {
		case x.Name != d.Name:
		case x.Kind != d.Kind:
			return ConfigError(field, "kind: conflicts with another version of the name")
		case x.AuthorizeAs != d.AuthorizeAs:
			return ConfigError(field, "authorize as: conflicts with another version of the name")
		}
	}
	return nil
}

// checkCapabilityName accepts builtin capabilities and namespaced names.
// Other dot-free tokens are reserved for future core capabilities.
func checkCapabilityName(c model.Capability) error {
	if slices.Contains(builtinCapabilities(), c) {
		return nil
	}
	return model.ValidateExtensionName(string(c))
}

// checkCapabilities rejects unknown or duplicate capability names,
// namespaced capabilities no extension definition requires and builtin
// capabilities declared without the capability they require. Whether a
// builtin capability's interface is implemented is known only once an
// instance is open; see CheckInstance.
func checkCapabilities(field string, caps []model.Capability, required map[model.Capability]bool) error {
	for i, c := range caps {
		f := fmt.Sprintf("%s.Capabilities[%d]", field, i)
		if slices.Contains(caps[:i], c) {
			return ConfigError(f, "duplicate")
		}
		if err := checkCapabilityName(c); err != nil {
			return ConfigError(f, SafeMessage(err))
		}
		if !slices.Contains(builtinCapabilities(), c) && !required[c] {
			return ConfigError(f, "no extension definition requires it")
		}
		if need, ok := capabilityRequires(c); ok && !slices.Contains(caps, need) {
			return ConfigError(f, "requires "+string(need))
		}
	}
	return nil
}

// checkEvents accepts builtin event kinds and namespaced kinds with a
// registered event-kind extension definition.
func checkEvents(field string, events []string, eventDefs map[string]string) error {
	for i, ev := range events {
		f := fmt.Sprintf("%s.Events[%d]", field, i)
		if slices.Contains(events[:i], ev) {
			return ConfigError(f, "duplicate")
		}
		if slices.Contains(builtinEvents(), ev) {
			continue
		}
		if err := model.ValidateExtensionName(ev); err != nil {
			return ConfigError(f, SafeMessage(err))
		}
		if _, ok := eventDefs[ev]; !ok {
			return ConfigError(f, "no event-kind extension definition")
		}
	}
	return nil
}

// checkSenderKinds requires each declared sender kind to be a contact kind
// other than person, the undeclared default, and declared once.
func checkSenderKinds(field string, kinds []string) error {
	for i, k := range kinds {
		f := fmt.Sprintf("%s.SenderKinds[%d]", field, i)
		switch {
		case slices.Contains(kinds[:i], k):
			return ConfigError(f, "duplicate")
		case k == model.ContactKindPerson:
			return ConfigError(f, "person is the default and is never declared")
		case !model.IsContactKind(k):
			return ConfigError(f, "must be a contact kind")
		}
	}
	return nil
}

// CheckInstance verifies that an opened instance implements the adapter
// interface of every builtin capability its descriptor declares, so no
// capability is advertised without callable behaviour. The engine runs it
// on each instance it opens, before using it.
func CheckInstance(d adapter.Descriptor, inst adapter.Instance) error {
	if inst == nil {
		return model.NewError(model.Validation, "instance: nil")
	}
	for i, c := range d.Capabilities {
		if implemented, builtin := capabilityImplemented(c, inst); builtin && !implemented {
			return model.NewError(model.Validation,
				fmt.Sprintf("capabilities[%d]: declared without its adapter interface", i))
		}
	}
	return nil
}

// Adapter returns the factory registered for t and the descriptor snapshot
// taken at New. The descriptor is shared: callers that keep or hand it on
// clone it with CloneDescriptor.
func (e *Engine) Adapter(t model.TypeKey) (adapter.Factory, adapter.Descriptor, bool) {
	reg, ok := e.reg.lookup(t)
	return reg.factory, reg.desc, ok
}

// ExtensionVersions returns copies of the extension definitions registered
// under name, in registration order; none when name is not registered.
func (e *Engine) ExtensionVersions(name string) []adapter.ExtensionDefinition {
	var out []adapter.ExtensionDefinition
	for _, d := range e.reg.extensions {
		if d.Name == name {
			out = append(out, cloneDefinition(d))
		}
	}
	return out
}

func (r *registry) lookup(t model.TypeKey) (registration, bool) {
	i, ok := r.byType[t]
	if !ok {
		return registration{}, false
	}
	return r.adapters[i], true
}

// describe returns copies of the registered descriptors in registration
// order.
func (r *registry) describe() []adapter.Descriptor {
	out := make([]adapter.Descriptor, len(r.adapters))
	for i, a := range r.adapters {
		out[i] = CloneDescriptor(a.desc)
	}
	return out
}

// CloneDescriptor returns a deep copy of d.
// checkExchange accepts declared exchange semantics only: the zero value
// or the adapter package constants.
func checkExchange(field string, x adapter.Exchange) error {
	switch x.MessageIDs {
	case "", adapter.MessageIDsProvider, adapter.MessageIDsSender:
	default:
		return ConfigError(field, "exchange message IDs: unknown authority")
	}
	switch x.Threading {
	case "", adapter.ThreadingEmail:
	default:
		return ConfigError(field, "exchange threading: unknown policy")
	}
	switch x.Reactions {
	case adapter.ReactionsMultiple, adapter.ReactionsSingle:
	default:
		return ConfigError(field, "exchange reactions: unknown cardinality")
	}
	return nil
}

// ProviderAssigned reports whether d declares provider-assigned message
// IDs, the only IDs core treats as message identity or echo evidence.
func ProviderAssigned(d adapter.Descriptor) bool {
	return d.Exchange.MessageIDs == adapter.MessageIDsProvider
}

// EmailThreading reports whether d declares the email threading policy.
func EmailThreading(d adapter.Descriptor) bool {
	return d.Exchange.Threading == adapter.ThreadingEmail
}

func CloneDescriptor(d adapter.Descriptor) adapter.Descriptor {
	d.ConfigurationSchema = d.ConfigurationSchema.Clone()
	d.Capabilities = slices.Clone(d.Capabilities)
	d.Events = slices.Clone(d.Events)
	d.IdentityNamespaces = slices.Clone(d.IdentityNamespaces)
	d.RequiredPermissions = slices.Clone(d.RequiredPermissions)
	d.WorkItemActs = cloneWorkItemActs(d.WorkItemActs)
	d.SenderKinds = slices.Clone(d.SenderKinds)
	return d
}

func cloneDefinition(d adapter.ExtensionDefinition) adapter.ExtensionDefinition {
	d.InputSchema = d.InputSchema.Clone()
	d.ResultSchema = d.ResultSchema.Clone()
	return d
}

// ConfigError is the validation error for Config field. Reasons are fixed
// text and never echo caller values.
func ConfigError(field, reason string) error {
	return model.NewError(model.Validation, "Config."+field+": "+reason)
}

// SafeMessage returns the safe message of a *model.Error, or a fixed
// placeholder for any other error.
func SafeMessage(err error) string {
	var me *model.Error
	if errors.As(err, &me) && me.SafeMessage != "" {
		return me.SafeMessage
	}
	return "invalid"
}
