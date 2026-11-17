package engine_test

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
)

const extCapability model.Capability = "com.example.chat.messages.pin"

const (
	extEvent  = "com.example.chat.messages.pinned"
	extAction = "com.example.chat.messages.pin"
)

func pinDefinition() adapter.ExtensionDefinition {
	return adapter.ExtensionDefinition{
		Name:               "com.example.chat.messages.pin",
		Version:            1,
		InputSchema:        model.Extension{Name: "com.example.chat.messages.pin_input", Version: 1},
		ResultSchema:       model.Extension{Name: "com.example.chat.messages.pin_result", Version: 1},
		RequiredCapability: extCapability,
		AuthorizeAs:        extAction,
		Kind:               model.ExtensionKindEffect,
	}
}

func pinnedDefinition() adapter.ExtensionDefinition {
	return adapter.ExtensionDefinition{
		Name:               extEvent,
		Version:            1,
		RequiredCapability: extCapability,
		AuthorizeAs:        model.ActionMessageRead,
		Kind:               model.ExtensionKindEvent,
	}
}

// fullDescriptor declares every builtin capability plus an extension
// capability and event, all of which have their registrations, every
// exchange semantic and the work item transitions work_item_act needs.
func fullDescriptor() adapter.Descriptor {
	d := enginetest.ReceiveDescriptor(enginetest.ThirdPartyKey)
	d.Exchange = adapter.Exchange{MessageIDs: adapter.MessageIDsProvider, Threading: adapter.ThreadingEmail}
	d.Capabilities = append(engine.BuiltinCapabilities(), extCapability)
	d.Events = append(engine.BuiltinEvents(), extEvent)
	d.WorkItemActs = issueActs()
	d.SenderKinds = []string{model.ContactKindBot, model.ContactKindAgent}
	return d
}

func registryConfig(descs []adapter.Descriptor, defs ...adapter.ExtensionDefinition) engine.Config {
	cfg := enginetest.ValidConfig()
	for _, d := range descs {
		cfg.Adapters = append(cfg.Adapters, enginetest.NewFactory(d))
	}
	cfg.Extensions = defs
	return cfg
}

func TestRegistryAccepts(t *testing.T) {
	maintained := enginetest.ReceiveDescriptor(model.TypeTwilioSms)
	read := pinDefinition()
	read.Name, read.Kind, read.AuthorizeAs = "com.example.chat.messages.lookup", model.ExtensionKindRead, model.ActionMessageRead
	cfg := registryConfig([]adapter.Descriptor{fullDescriptor(), maintained},
		pinDefinition(), pinnedDefinition(), read)
	e := enginetest.MustNew(t, cfg)
	got := e.Describe()
	if len(got) != 2 || got[0].Type != enginetest.ThirdPartyKey || got[1].Type != model.TypeTwilioSms {
		t.Fatalf("Describe = %+v, want registration order", got)
	}
}

// withVersion2 returns the standard definitions plus a second version of
// the pin definition, edited.
func withVersion2(edit func(*adapter.ExtensionDefinition)) []adapter.ExtensionDefinition {
	v2 := pinDefinition()
	v2.Version = 2
	edit(&v2)
	return []adapter.ExtensionDefinition{pinDefinition(), pinnedDefinition(), v2}
}

// Versions of one name may differ in schemas and capability, never in kind
// or policy action; ExtensionVersions returns them in registration order as
// copies.
func TestRegistryExtensionVersions(t *testing.T) {
	defs := withVersion2(func(x *adapter.ExtensionDefinition) {
		x.RequiredCapability = model.CapabilityExtensionExecute
		x.InputSchema.Version = 2
	})
	e := enginetest.MustNew(t, registryConfig([]adapter.Descriptor{fullDescriptor()}, defs...))
	got := e.ExtensionVersions(pinDefinition().Name)
	if len(got) != 2 || got[0].Version != 1 || got[1].Version != 2 || got[1].InputSchema.Version != 2 {
		t.Fatalf("ExtensionVersions = %+v, want versions 1 and 2 in order", got)
	}
	got[0].InputSchema.Name = "mutated"
	if again := e.ExtensionVersions(pinDefinition().Name); again[0].InputSchema.Name != pinDefinition().InputSchema.Name {
		t.Fatalf("ExtensionVersions aliases engine state: %+v", again)
	}
	if none := e.ExtensionVersions("com.example.chat.messages.unknown"); len(none) != 0 {
		t.Fatalf("unknown name: %+v, want none", none)
	}
}

func TestRegistryRejects(t *testing.T) {
	d := func(edit func(*adapter.Descriptor)) []adapter.Descriptor {
		x := fullDescriptor()
		edit(&x)
		return []adapter.Descriptor{x}
	}
	def := func(edit func(*adapter.ExtensionDefinition)) []adapter.ExtensionDefinition {
		x := pinDefinition()
		edit(&x)
		return []adapter.ExtensionDefinition{x, pinnedDefinition()}
	}
	std := []adapter.ExtensionDefinition{pinDefinition(), pinnedDefinition()}
	typeKey := func(k model.TypeKey) []adapter.Descriptor {
		return d(func(x *adapter.Descriptor) { x.Type = k })
	}
	cases := []struct {
		name  string
		descs []adapter.Descriptor
		defs  []adapter.ExtensionDefinition
		want  string
	}{
		// Type keys: grammar, reservations and shadowing of core names.
		{"empty type", typeKey(""), std, "Config.Adapters[0]: type:"},
		{"uppercase type", typeKey("com.example.Chat.messages"), std, "Config.Adapters[0]: type:"},
		{"unknown two-segment", typeKey("acme.chat"), std, "reserved for maintained adapters"},
		{"reserved unimplemented", typeKey(model.TypeIrcChat), std, "reserved; not implemented"},
		{"core prefix", typeKey("uma.example.chat"), std, "starts with a reserved segment"},
		{"native prefix", typeKey("native.example.chat"), std, "starts with a reserved segment"},
		{"maintained prefix", typeKey("github.issues.mirror"), std, "starts with a maintained or reserved type key"},
		{"reserved prefix", typeKey("irc.chat.bridge"), std, "starts with a maintained or reserved type key"},
		{"versioned", typeKey("com.example.chat.v2"), std, "must not carry a version"},
		{"duplicate type", []adapter.Descriptor{fullDescriptor(), fullDescriptor()}, std,
			"Config.Adapters[1]: type: duplicate"},

		// Contract majors.
		{"zero major", d(func(x *adapter.Descriptor) { x.ContractMajor = 0 }), std,
			"Config.Adapters[0]: contract major: unsupported"},
		{"future major", d(func(x *adapter.Descriptor) { x.ContractMajor = adapter.ContractMajor + 1 }), std,
			"Config.Adapters[0]: contract major: unsupported"},

		// Capabilities.
		{"unknown core capability", d(func(x *adapter.Descriptor) {
			x.Capabilities = []model.Capability{"sync_labels"}
		}), std, "Config.Adapters[0].Capabilities[0]: name: unqualified and two-segment names are reserved for core"},
		{"two-segment capability", d(func(x *adapter.Descriptor) {
			x.Capabilities = []model.Capability{"chat.pin"}
		}), std, "Config.Adapters[0].Capabilities[0]: name:"},
		{"core-namespace capability", d(func(x *adapter.Descriptor) {
			x.Capabilities = []model.Capability{"uma.chat.pin"}
		}), std, "the uma namespace is reserved for core"},
		{"extension capability without definition", d(func(x *adapter.Descriptor) {
			x.Capabilities = []model.Capability{"com.example.chat.messages.archive"}
		}), std, "Config.Adapters[0].Capabilities[0]: no extension definition requires it"},
		{"follow-up responses without respond", d(func(x *adapter.Descriptor) {
			x.Capabilities = []model.Capability{model.CapabilitySend, model.CapabilityRespondFollowUp}
		}), std, "Config.Adapters[0].Capabilities[1]: requires respond"},
		{"duplicate capability", d(func(x *adapter.Descriptor) {
			x.Capabilities = []model.Capability{model.CapabilitySend, model.CapabilitySend}
		}), std, "Config.Adapters[0].Capabilities[1]: duplicate"},
		{"negative reconcile settle window", d(func(x *adapter.Descriptor) {
			x.ReconcileSettle = -time.Second
		}), std, "Config.Adapters[0]: reconcile settle window: negative"},

		// Exchange semantics: declared constants only.
		{"unknown message ID authority", d(func(x *adapter.Descriptor) {
			x.Exchange.MessageIDs = "trusted"
		}), std, "Config.Adapters[0]: exchange message IDs: unknown authority"},
		{"unknown threading policy", d(func(x *adapter.Descriptor) {
			x.Exchange.Threading = "subject"
		}), std, "Config.Adapters[0]: exchange threading: unknown policy"},
		{"unknown reaction cardinality", d(func(x *adapter.Descriptor) {
			x.Exchange.Reactions = "two"
		}), std, "Config.Adapters[0]: exchange reactions: unknown cardinality"},

		// Events.
		{"unknown core event", d(func(x *adapter.Descriptor) {
			x.Events = []string{"message.exploded"}
		}), std, "Config.Adapters[0].Events[0]: name:"},
		{"extension event without definition", d(func(x *adapter.Descriptor) {
			x.Events = []string{"com.example.chat.messages.archived"}
		}), std, "Config.Adapters[0].Events[0]: no event-kind extension definition"},
		{"extension event with non-event definition", d(func(x *adapter.Descriptor) {
			x.Events = []string{"com.example.chat.messages.pin"}
		}), std, "Config.Adapters[0].Events[0]: no event-kind extension definition"},
		// Sender kinds: contact kinds other than person, once each.
		{"person sender kind", d(func(x *adapter.Descriptor) {
			x.SenderKinds = []string{model.ContactKindPerson}
		}), std, "Config.Adapters[0].SenderKinds[0]: person is the default and is never declared"},
		{"unknown sender kind", d(func(x *adapter.Descriptor) {
			x.SenderKinds = []string{model.ContactKindBot, "robot"}
		}), std, "Config.Adapters[0].SenderKinds[1]: must be a contact kind"},
		{"system sender kind", d(func(x *adapter.Descriptor) {
			x.SenderKinds = []string{model.AuthorKindSystem}
		}), std, "Config.Adapters[0].SenderKinds[0]: must be a contact kind"},
		{"duplicate sender kind", d(func(x *adapter.Descriptor) {
			x.SenderKinds = []string{model.ContactKindAgent, model.ContactKindAgent}
		}), std, "Config.Adapters[0].SenderKinds[1]: duplicate"},
		{"duplicate event", d(func(x *adapter.Descriptor) {
			x.Events = []string{model.EventMessageReceived, model.EventMessageReceived}
		}), std, "Config.Adapters[0].Events[1]: duplicate"},

		// Conflicting schema registrations.
		{"shared configuration schema", func() []adapter.Descriptor {
			a, b := fullDescriptor(), enginetest.ReceiveDescriptor("org.example.chat.messages")
			b.ConfigurationSchema = a.ConfigurationSchema
			return []adapter.Descriptor{a, b}
		}(), std, "Config.Adapters[1]: configuration schema: registered by another adapter"},

		// Extension definitions.
		{"core-namespace extension", nil, def(func(x *adapter.ExtensionDefinition) { x.Name = "uma.message.pin" }),
			"Config.Extensions[0]: name: the uma namespace is reserved for core"},
		{"two-segment extension", nil, def(func(x *adapter.ExtensionDefinition) { x.Name = "message.pin" }),
			"Config.Extensions[0]: name: unqualified and two-segment names are reserved for core"},
		{"dot-free extension", nil, def(func(x *adapter.ExtensionDefinition) { x.Name = "pin" }),
			"Config.Extensions[0]: name:"},
		{"versioned extension", nil, def(func(x *adapter.ExtensionDefinition) { x.Name = "com.example.pin.v2" }),
			"Config.Extensions[0]: name: must not carry a version"},
		{"zero version", nil, def(func(x *adapter.ExtensionDefinition) { x.Version = 0 }),
			"Config.Extensions[0]: version: must be positive"},
		{"duplicate name and version", nil, []adapter.ExtensionDefinition{pinDefinition(), pinnedDefinition(), pinDefinition()},
			"Config.Extensions[2]: duplicate name and version"},
		{"unknown kind", nil, def(func(x *adapter.ExtensionDefinition) { x.Kind = "query" }),
			"Config.Extensions[0]: kind: must be read, effect or event"},
		{"missing action", nil, def(func(x *adapter.ExtensionDefinition) { x.AuthorizeAs = "" }),
			"Config.Extensions[0]: authorize as: required"},
		{"unknown core action", nil, def(func(x *adapter.ExtensionDefinition) { x.AuthorizeAs = "message.pin" }),
			"Config.Extensions[0]: authorize as: name:"},
		{"effect behind read grant", nil, def(func(x *adapter.ExtensionDefinition) { x.AuthorizeAs = model.ActionMessageRead }),
			"Config.Extensions[0]: authorize as: an effect cannot authorize as a read action"},
		{"effect behind read-all grant", nil, def(func(x *adapter.ExtensionDefinition) { x.AuthorizeAs = model.ActionContactReadAll }),
			"Config.Extensions[0]: authorize as: an effect cannot authorize as a read action"},
		{"effect behind event list grant", nil, def(func(x *adapter.ExtensionDefinition) { x.AuthorizeAs = model.ActionEventList }),
			"Config.Extensions[0]: authorize as: an effect cannot authorize as a read action"},
		{"effect behind operation read grant", nil, def(func(x *adapter.ExtensionDefinition) { x.AuthorizeAs = model.ActionOperationRead }),
			"Config.Extensions[0]: authorize as: an effect cannot authorize as a read action"},
		{"effect behind extension query grant", nil, def(func(x *adapter.ExtensionDefinition) { x.AuthorizeAs = model.ActionExtensionQuery }),
			"Config.Extensions[0]: authorize as: an effect cannot authorize as a read action"},
		{"effect behind identity history grant", nil, def(func(x *adapter.ExtensionDefinition) { x.AuthorizeAs = model.ActionIdentityHistory }),
			"Config.Extensions[0]: authorize as: an effect cannot authorize as a read action"},
		{"effect behind operation reconcile grant", nil, def(func(x *adapter.ExtensionDefinition) { x.AuthorizeAs = model.ActionOperationReconcile }),
			"Config.Extensions[0]: authorize as: an effect cannot authorize as a core-only action"},
		{"effect behind contact write grant", nil, def(func(x *adapter.ExtensionDefinition) { x.AuthorizeAs = model.ActionContactWrite }),
			"Config.Extensions[0]: authorize as: an effect cannot authorize as a core-only action"},
		{"effect behind contact merge grant", nil, def(func(x *adapter.ExtensionDefinition) { x.AuthorizeAs = model.ActionContactMerge }),
			"Config.Extensions[0]: authorize as: an effect cannot authorize as a core-only action"},
		{"effect behind subscription poll grant", nil, def(func(x *adapter.ExtensionDefinition) { x.AuthorizeAs = model.ActionSubscriptionPoll }),
			"Config.Extensions[0]: authorize as: an effect cannot authorize as a core-only action"},
		{"missing capability", nil, def(func(x *adapter.ExtensionDefinition) { x.RequiredCapability = "" }),
			"Config.Extensions[0]: required capability: required"},
		{"unknown core capability", nil, def(func(x *adapter.ExtensionDefinition) { x.RequiredCapability = "pin" }),
			"Config.Extensions[0]: required capability: name:"},
		{"core-namespace input schema", nil, def(func(x *adapter.ExtensionDefinition) { x.InputSchema.Name = "uma.pin.input" }),
			"Config.Extensions[0]: input schema: name: the uma namespace is reserved for core"},
		{"two-segment result schema", nil, def(func(x *adapter.ExtensionDefinition) { x.ResultSchema.Name = "pin.result" }),
			"Config.Extensions[0]: result schema: name:"},

		// Conflicting versions of one name: a caller choosing a version must
		// not choose another kind or another policy action.
		{"kind differs across versions", nil, withVersion2(func(x *adapter.ExtensionDefinition) { x.Kind = model.ExtensionKindRead }),
			"Config.Extensions[2]: kind: conflicts with another version of the name"},
		{"action differs across versions", nil, withVersion2(func(x *adapter.ExtensionDefinition) { x.AuthorizeAs = model.ActionExtensionExecute }),
			"Config.Extensions[2]: authorize as: conflicts with another version of the name"},
		{"definition named like a configuration schema", nil,
			def(func(x *adapter.ExtensionDefinition) { x.Name = string(enginetest.ThirdPartyKey) + ".config" }),
			"Config.Adapters[0]: configuration schema: registered as an extension definition"},

		// Nil entries.
		{"nil factory", nil, std, "Config.Adapters[0]: nil factory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			descs := tc.descs
			if descs == nil {
				descs = []adapter.Descriptor{fullDescriptor()}
			}
			cfg := registryConfig(descs, tc.defs...)
			if tc.name == "nil factory" {
				cfg.Adapters = []adapter.Factory{nil}
			}
			e, err := engine.New(cfg)
			if e != nil {
				t.Fatal("New returned an engine with an error")
			}
			enginetest.RequireCode(t, err, model.Validation, tc.want)
		})
	}
}

// Caller-owned maps and slices are copied at New: later changes to the
// Config, to a descriptor's slices or to a Describe result change nothing.
func TestRegistryCopiesCallerInputs(t *testing.T) {
	desc := fullDescriptor()
	desc.ConfigurationSchema.Data = json.RawMessage(`{"a":1}`)
	f := enginetest.NewFactory(desc)
	other := enginetest.NewFactory(enginetest.ReceiveDescriptor("org.example.other.messages"))
	verifier := enginetest.OwnedVerifier{}
	selector := enginetest.OwnedSelector{}
	cfg := enginetest.ValidConfig()
	cfg.Adapters = []adapter.Factory{f}
	cfg.Extensions = []adapter.ExtensionDefinition{pinDefinition(), pinnedDefinition()}
	cfg.IdentityVerifiers = map[string]policy.IdentityVerifier{"attestation": verifier}
	cfg.Selectors = map[string]policy.ConversationSelector{"default": selector}
	e := enginetest.MustNew(t, cfg)
	// An independent expected value: nothing below can reach it.
	expected := fullDescriptor()
	expected.ConfigurationSchema.Data = json.RawMessage(`{"a":1}`)
	want := []adapter.Descriptor{expected}
	if !descriptorsEqual(e.Describe(), want) {
		t.Fatalf("Describe = %+v, want %+v", e.Describe(), want)
	}

	cfg.Adapters[0] = other
	cfg.Adapters = append(cfg.Adapters, other)
	cfg.Extensions[0].Name = "com.example.changed.name"
	cfg.Extensions[1].Kind = model.ExtensionKindRead
	delete(cfg.IdentityVerifiers, "attestation")
	cfg.IdentityVerifiers["late"] = verifier
	cfg.Selectors["late"] = selector
	f.Desc.Capabilities[0] = "mutated"
	f.Desc.Events[0] = "mutated"
	f.Desc.SenderKinds[0] = "mutated"
	f.Desc.ConfigurationSchema.Data[0] = 'X'

	got := e.Describe()
	if !descriptorsEqual(got, want) {
		t.Fatalf("Describe changed after caller mutation:\n got %+v\nwant %+v", got, want)
	}
	if string(got[0].ConfigurationSchema.Data) != `{"a":1}` {
		t.Fatalf("configuration schema data aliased: %s", got[0].ConfigurationSchema.Data)
	}
	got[0].Capabilities[0] = "mutated"
	got[0].SenderKinds[0] = "mutated"
	got[0].ConfigurationSchema.Data[0] = 'Y'
	if again := e.Describe(); !descriptorsEqual(again, want) {
		t.Fatalf("Describe result aliases engine state: %+v", again)
	}
	if _, err := e.IdentityVerifier("attestation"); err != nil {
		t.Errorf("verifier removed from caller map is gone from engine: %v", err)
	}
	if _, err := e.IdentityVerifier("late"); err == nil {
		t.Error("verifier added to caller map after New is visible")
	}
	if _, err := e.ConversationSelector("late"); err == nil {
		t.Error("selector added to caller map after New is visible")
	}
	if ext := e.Extensions(); ext[0].Name != pinDefinition().Name || ext[1].Kind != model.ExtensionKindEvent {
		t.Errorf("extension definitions aliased: %+v", ext)
	}
}

func descriptorsEqual(a, b []adapter.Descriptor) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Type != y.Type || x.ContractMajor != y.ContractMajor || x.Maturity != y.Maturity ||
			x.ConfigurationSchema.Name != y.ConfigurationSchema.Name ||
			x.ConfigurationSchema.Version != y.ConfigurationSchema.Version ||
			string(x.ConfigurationSchema.Data) != string(y.ConfigurationSchema.Data) ||
			!slices.Equal(x.Capabilities, y.Capabilities) || !slices.Equal(x.Events, y.Events) ||
			!slices.Equal(x.IdentityNamespaces, y.IdentityNamespaces) ||
			!slices.Equal(x.RequiredPermissions, y.RequiredPermissions) || x.Exchange != y.Exchange || !slices.Equal(x.SenderKinds, y.SenderKinds) ||
			!reflect.DeepEqual(x.WorkItemActs, y.WorkItemActs) {
			return false
		}
	}
	return true
}

// Registration is local to each engine: no global registry, so the same
// type registers in two engines and neither sees the other's adapters.
func TestRegistryIsEngineLocal(t *testing.T) {
	a := enginetest.MustNew(t, registryConfig([]adapter.Descriptor{enginetest.ReceiveDescriptor(enginetest.ThirdPartyKey)}))
	b := enginetest.MustNew(t, registryConfig([]adapter.Descriptor{
		enginetest.ReceiveDescriptor(enginetest.ThirdPartyKey), enginetest.ReceiveDescriptor("org.example.other.messages"),
	}))
	if n := len(a.Describe()); n != 1 {
		t.Errorf("engine a has %d adapters, want 1", n)
	}
	if n := len(b.Describe()); n != 2 {
		t.Errorf("engine b has %d adapters, want 2", n)
	}
}

func TestValidateChannel(t *testing.T) {
	f := enginetest.NewFactory(enginetest.ReceiveDescriptor(enginetest.ThirdPartyKey))
	var seen model.Extension
	f.OnValidate = func(c model.Extension) []model.ValidationIssue {
		seen = c
		c.Data[0] = 'X' // a factory scribbling on its input must not reach the caller
		if string(c.Data) == `X"bad"}` {
			return []model.ValidationIssue{{Field: "configuration.data", Code: string(model.Validation)}}
		}
		return nil
	}
	cfg := enginetest.ValidConfig()
	cfg.Adapters = []adapter.Factory{f}
	e := enginetest.MustNew(t, cfg)
	schema := f.Desc.ConfigurationSchema

	good := schema
	good.Data = json.RawMessage(`{"ok":true}`)
	if issues := e.ValidateChannel(enginetest.ThirdPartyKey, good); len(issues) != 0 {
		t.Fatalf("valid configuration: %v", issues)
	}
	if string(good.Data) != `{"ok":true}` {
		t.Fatalf("factory received caller memory: %s", good.Data)
	}
	if seen.Name != schema.Name {
		t.Fatalf("factory saw %q", seen.Name)
	}

	bad := schema
	bad.Data = json.RawMessage(`{"bad"}`)
	cases := []struct {
		name  string
		typ   model.TypeKey
		cfg   model.Extension
		field string
		code  model.ErrorCode
	}{
		{"unregistered type", "org.example.missing.messages", good, "type", model.Unsupported},
		{"core-namespace schema", enginetest.ThirdPartyKey, model.Extension{Name: "uma.config", Version: 1}, "configuration.name", model.Validation},
		{"two-segment schema", enginetest.ThirdPartyKey, model.Extension{Name: "chat.config", Version: 1}, "configuration.name", model.Validation},
		{"other schema", enginetest.ThirdPartyKey, model.Extension{Name: "org.example.other.config", Version: 1}, "configuration", model.SchemaMismatch},
		{"other version", enginetest.ThirdPartyKey, model.Extension{Name: schema.Name, Version: 2}, "configuration", model.SchemaMismatch},
		{"factory issue", enginetest.ThirdPartyKey, bad, "configuration.data", model.Validation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := e.ValidateChannel(tc.typ, tc.cfg)
			if len(issues) != 1 || issues[0].Field != tc.field || issues[0].Code != string(tc.code) {
				t.Fatalf("issues = %+v, want one %s on %s", issues, tc.code, tc.field)
			}
		})
	}
}

// The engine's builtin name sets are exactly the constants model declares.
func TestBuiltinSetsMatchModel(t *testing.T) {
	consts := modelStringConsts(t)
	var caps, events, actions []string
	for name, v := range consts {
		switch {
		case strings.HasPrefix(name, "Capability"):
			caps = append(caps, v)
		case strings.HasPrefix(name, "Event"):
			events = append(events, v)
		case strings.HasPrefix(name, "Action"):
			actions = append(actions, v)
		}
	}
	check := func(what string, model, engine []string) {
		slices.Sort(model)
		engine = slices.Clone(engine)
		slices.Sort(engine)
		if !slices.Equal(model, engine) {
			t.Errorf("%s:\n model  %v\n engine %v", what, model, engine)
		}
	}
	var capNames []string
	for _, c := range engine.BuiltinCapabilities() {
		capNames = append(capNames, string(c))
	}
	check("capabilities", caps, capNames)
	check("events", events, engine.BuiltinEvents())
	check("actions", actions, engine.BuiltinActions())
	if len(caps) == 0 || len(events) == 0 || len(actions) == 0 {
		t.Fatal("no model constants found; wrong file?")
	}
}

// modelStringConsts returns the string-literal constants of model/names.go.
func modelStringConsts(t *testing.T) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "../../model/names.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out[n.Name] = v
			}
		}
	}
	return out
}

// Declared builtin capabilities need their optional adapter interface.
// The check runs on an opened instance, before the engine uses it.
func TestCheckInstance(t *testing.T) {
	for _, c := range engine.BuiltinCapabilities() {
		t.Run(string(c), func(t *testing.T) {
			d := enginetest.ReceiveDescriptor(enginetest.ThirdPartyKey)
			d.Capabilities = []model.Capability{c}
			enginetest.RequireCode(t, engine.CheckInstance(d, baseInstance{}), model.Validation,
				"capabilities[0]: declared without its adapter interface")
			if err := engine.CheckInstance(d, fullInstance{}); err != nil {
				t.Fatalf("full instance: %v", err)
			}
		})
	}
	d := fullDescriptor()
	if err := engine.CheckInstance(d, fullInstance{}); err != nil {
		t.Fatalf("extension capabilities need no interface of their own: %v", err)
	}
	if err := engine.CheckInstance(d, nil); err == nil {
		t.Fatal("nil instance accepted")
	}
}

type baseInstance struct{}

func (baseInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) { return nil, nil }
func (baseInstance) Close(context.Context) error                                    { return nil }

type fullInstance struct{ baseInstance }

func (fullInstance) Run(context.Context, adapter.Sink) error                 { return nil }
func (fullInstance) Handler(adapter.Sink) http.Handler                       { return nil }
func (fullInstance) Probe(context.Context) ([]model.CapabilityStatus, error) { return nil, nil }
func (fullInstance) Send(context.Context, adapter.SendRequest) (adapter.Outcome, error) {
	return adapter.Outcome{}, nil
}
func (fullInstance) Edit(context.Context, adapter.EditRequest) (adapter.Outcome, error) {
	return adapter.Outcome{}, nil
}
func (fullInstance) Delete(context.Context, adapter.DeleteRequest) (adapter.Outcome, error) {
	return adapter.Outcome{}, nil
}
func (fullInstance) React(context.Context, adapter.ReactRequest) (adapter.Outcome, error) {
	return adapter.Outcome{}, nil
}
func (fullInstance) Execute(context.Context, adapter.ExecuteRequest) (adapter.Outcome, error) {
	return adapter.Outcome{}, nil
}
func (fullInstance) Query(context.Context, model.QueryExtension) (model.Extension, error) {
	return model.Extension{}, nil
}
func (fullInstance) Reconcile(context.Context, adapter.ReconcileRequest) (adapter.Outcome, error) {
	return adapter.Outcome{}, nil
}
func (fullInstance) Signal(context.Context, adapter.SignalRequest) error { return nil }
func (fullInstance) SendNote(context.Context, adapter.NoteRequest) (adapter.Outcome, error) {
	return adapter.Outcome{}, nil
}
func (fullInstance) Refetch(context.Context, adapter.RefetchRequest) (model.Envelope, error) {
	return model.Envelope{}, nil
}
func (fullInstance) SendEphemeral(context.Context, adapter.EphemeralRequest) (adapter.Outcome, error) {
	return adapter.Outcome{}, nil
}
func (fullInstance) Start(context.Context, adapter.StreamRequest) (adapter.StreamOutcome, error) {
	return adapter.StreamOutcome{}, nil
}
func (fullInstance) Update(context.Context, string, adapter.StreamRequest) (adapter.StreamOutcome, error) {
	return adapter.StreamOutcome{}, nil
}
func (fullInstance) Finish(context.Context, string, adapter.StreamRequest) (adapter.StreamOutcome, error) {
	return adapter.StreamOutcome{}, nil
}
func (fullInstance) Cancel(context.Context, string, adapter.Effect) (adapter.StreamOutcome, error) {
	return adapter.StreamOutcome{}, nil
}
func (fullInstance) Respond(context.Context, adapter.RespondRequest) (adapter.Outcome, error) {
	return adapter.Outcome{}, nil
}

// EventKind accepts builtin kinds and namespaced kinds with a registered
// event-kind definition; any other name, even a valid namespaced one, is
// not an event kind of this engine.
func TestEventKind(t *testing.T) {
	e := enginetest.MustNew(t, registryConfig(nil, pinDefinition(), pinnedDefinition()))
	for _, k := range engine.BuiltinEvents() {
		if !e.EventKind(k) {
			t.Errorf("builtin %s rejected", k)
		}
	}
	if !e.EventKind(extEvent) {
		t.Errorf("registered %s rejected", extEvent)
	}
	for _, k := range []string{"com.example.chat.messages.pin", "com.example.chat.messages.unknown", "bogus", "message.sent", ""} {
		if e.EventKind(k) {
			t.Errorf("%q accepted", k)
		}
	}
}
