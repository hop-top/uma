// Package fakeprovider is a scripted provider adapter in its own module. It
// is built as a third party would build one: it imports only public uma
// packages, registers nothing at init and keeps no global state. Hosts and
// conformance suites construct it explicitly and register it like any other
// adapter.
//
// Ten variants share one scripted provider:
//
//   - NewIngressOnly declares receive and implements adapter.Receiver only.
//
//   - NewSendOnly declares send and implements adapter.Sender only.
//
//   - NewFull declares receive, send, edit, delete, react, reconcile,
//     extension_execute, probe_permissions, receipt, membership and history and implements exactly the
//     matching interfaces. Its extension effect, ExtensionPin, and its
//     extension event kind, ExtensionPinned, are registered from
//     Factory.Extensions; Script.Emit queues a provider notice of the
//     event kind.
//
//   - NewWebhook declares receive_http and implements adapter.HTTPReceiver
//     only; its handler verifies a signed request body before the sink
//     sees anything. It reports messages and, declaring interaction.received,
//     interactions (WebhookEvent.Interaction), acknowledging an interaction
//     only once the sink holds it durably.
//
//   - NewInteractive declares send, edit, stream, send_ephemeral, signal,
//     respond and respond_follow_up and implements exactly the matching
//     interfaces: native streams, ephemeral replies, typing signals and
//     interaction responses.
//
//   - NewMail declares receive and send and implements adapter.Receiver and
//     adapter.Sender: an email provider of its own, threading on RFC 5322
//     headers with sender-chosen Message-IDs.
//
//   - NewTracker declares reconcile and work_item_act and implements
//     exactly the matching interfaces: a helpdesk whose work items move
//     between open, pending, resolved and closed along the transitions it
//     declares (Tracker.WorkItemActs).
//
//   - NewStateLabels, NewStateFolders and NewStateFoldersStable declare
//     receive, send, reconcile, archive, move, read_state, flag and trash
//     and implement exactly the matching interfaces: a mailbox that
//     labels its items, one that files each in a folder and gives a
//     moved item a new reference, and one whose references survive a
//     move. See state.go.
//
// Every variant but the mail one declares provider-assigned message IDs.
//
// No variant implements an interface for a capability it does not declare.
//
// Each channel instance has its own Script, the provider account behind it:
// inbound ingress queued for Receiver.Run, scripted results for outbound
// calls, revoked permissions and a record of every call. Two channels of one type never share a
// script, even when their external IDs overlap. Everything is deterministic:
// provider references count up per script, and outbound calls take scripted
// results in call order.
package fakeprovider

import (
	"context"
	"sync"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Type keys of the variants: third-party reverse-DNS keys, valid
// under model.ValidateTypeKey.
const (
	TypeIngressOnly model.TypeKey = "org.example.fake.inbound"
	TypeSendOnly    model.TypeKey = "org.example.fake.outbound"
	TypeFull        model.TypeKey = "org.example.fake.messages"
)

// ContractMajor is the adapter contract major every variant implements.
const ContractMajor uint32 = 3

// ConfigVersion is the configuration schema version every variant accepts.
const ConfigVersion uint32 = 1

// Namespace is the external reference and identity namespace of the fake
// provider.
const Namespace = "org.example.fake"

// ExtensionPin is the full variant's extension effect: pin the message the
// action's data names, as {"message": "<provider message ID>"}. The fake
// applies it like a send and keeps the action as the provider received it.
const (
	ExtensionPin        = "org.example.fake.messages.pin"
	ExtensionPinVersion = 1
)

// ExtensionPinned is the full variant's extension event kind: the
// provider reports that a message was pinned. Its data holds provider
// references only, as {"message": "<provider message ID>", "pin":
// "<provider pin ID>"}, never message content. Delivery to a subscriber
// needs ActionReadPins on the event's resource besides reading it.
const (
	ExtensionPinned        = "org.example.fake.messages.pinned"
	ExtensionPinnedVersion = 1
	ActionReadPins         = "org.example.fake.messages.read_pins"
)

// CredentialPurpose is the purpose of the credential an instance resolves
// before every outbound call when it was opened with a credentials port.
const CredentialPurpose = "org.example.fake.api"

type variant struct {
	typ      model.TypeKey
	exchange adapter.Exchange
	// sent maps the provider reference of a new sent message to the
	// references the variant reports for it; nil reports it as is.
	sent       func(model.ExternalRef) []model.ExternalRef
	caps       []model.Capability
	events     []string
	extensions []adapter.ExtensionDefinition
	// acts are the work item transitions a variant declaring work_item_act
	// performs.
	acts []adapter.WorkItemTransition
	// shape is how a state variant holds an item's place, and rename
	// whether a folder move gives it a new reference.
	shape  model.MoveShape
	rename bool
	open   func(*instance) adapter.Instance
}

// providerIDs is the exchange of the chat-like variants: the fake provider
// assigns every message ID.
var providerIDs = adapter.Exchange{MessageIDs: adapter.MessageIDsProvider}

var (
	ingressOnly = variant{
		typ: TypeIngressOnly, exchange: providerIDs, caps: []model.Capability{model.CapabilityReceive},
		events: []string{model.EventMessageReceived},
		open:   func(i *instance) adapter.Instance { return &IngressOnly{i: i} },
	}
	sendOnly = variant{
		typ: TypeSendOnly, exchange: providerIDs, caps: []model.Capability{model.CapabilitySend},
		events: []string{model.EventDeliveryUpdated},
		open:   func(i *instance) adapter.Instance { return &SendOnly{i: i} },
	}
	full = variant{
		typ: TypeFull, exchange: providerIDs,
		caps: []model.Capability{
			model.CapabilityReceive, model.CapabilitySend, model.CapabilityEdit, model.CapabilityDelete, model.CapabilityReact, model.CapabilityReconcile, model.CapabilityExtensionExecute, model.CapabilityProbePermissions,
			model.CapabilityReceipt, model.CapabilityMembership, model.CapabilityHistory,
		},
		extensions: []adapter.ExtensionDefinition{{
			Name: ExtensionPin, Version: ExtensionPinVersion, Kind: model.ExtensionKindEffect,
			InputSchema:        model.Extension{Name: ExtensionPin + "_input", Version: ExtensionPinVersion},
			RequiredCapability: model.CapabilityExtensionExecute, AuthorizeAs: ExtensionPin,
		}, {
			Name: ExtensionPinned, Version: ExtensionPinnedVersion, Kind: model.ExtensionKindEvent,
			RequiredCapability: model.CapabilityReceive, AuthorizeAs: ActionReadPins,
		}},
		events: []string{model.EventMessageReceived, model.EventDeliveryUpdated, ExtensionPinned},
		open:   func(i *instance) adapter.Instance { return &Full{i: i} },
	}
)

// Factory is the adapter.Factory of one variant. It owns the scripts of the
// channels it opens. The zero value is not usable; use a constructor.
type Factory struct {
	v       variant
	mu      sync.Mutex
	scripts map[model.ChannelRef]*Script
}

// NewIngressOnly returns the factory of the ingress-only variant.
func NewIngressOnly() *Factory { return newFactory(ingressOnly) }

// NewSendOnly returns the factory of the send-only variant.
func NewSendOnly() *Factory { return newFactory(sendOnly) }

// NewFull returns the factory of the full variant.
func NewFull() *Factory { return newFactory(full) }

func newFactory(v variant) *Factory {
	return &Factory{v: v, scripts: map[model.ChannelRef]*Script{}}
}

// Describe returns the variant's descriptor. Its capabilities are exactly
// the interfaces the variant's instances implement.
func (f *Factory) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type:                f.v.typ,
		ContractMajor:       ContractMajor,
		ConfigurationSchema: model.Extension{Name: configSchema(f.v.typ), Version: ConfigVersion},
		Capabilities:        append([]model.Capability(nil), f.v.caps...),
		Events:              append([]string(nil), f.v.events...),
		IdentityNamespaces:  []string{Namespace},
		Maturity:            "experimental",
		Exchange:            f.v.exchange,
		WorkItemActs:        cloneActs(f.v.acts),
	}
}

// Extensions returns the extension definitions the variant ships, which a
// host registers with the factory in its composition.
func (f *Factory) Extensions() []adapter.ExtensionDefinition {
	out := make([]adapter.ExtensionDefinition, len(f.v.extensions))
	for i, d := range f.v.extensions {
		d.InputSchema = d.InputSchema.Clone()
		d.ResultSchema = d.ResultSchema.Clone()
		out[i] = d
	}
	return out
}

// Config returns a valid channel configuration for the variant.
func (f *Factory) Config() model.Extension {
	return model.Extension{Name: configSchema(f.v.typ), Version: ConfigVersion}
}

func configSchema(t model.TypeKey) string { return string(t) + ".config" }

// Validate checks a channel configuration without I/O. The fake has no
// settings: only the schema name and version are checked.
func (f *Factory) Validate(c model.Extension) []model.ValidationIssue {
	if c.Name != configSchema(f.v.typ) || c.Version != ConfigVersion {
		return []model.ValidationIssue{{
			Field: "configuration", Code: string(model.SchemaMismatch),
			Message: "want " + configSchema(f.v.typ) + " version 1",
		}}
	}
	return nil
}

// Open opens an instance bound to channel and its script. It fails with
// model.Validation for a channel of another type or an invalid
// configuration. When deps carries a credentials port, every outbound call
// first resolves the channel's credential for CredentialPurpose, as a real
// adapter does per connection attempt; a refused credential fails the call
// before it reaches the provider. No other dependency is used.
func (f *Factory) Open(ctx context.Context, channel model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := channel.Ref.Validate(); err != nil {
		return nil, err
	}
	if channel.Type != f.v.typ {
		return nil, model.NewError(model.Validation, "channel type is not "+string(f.v.typ))
	}
	if issues := f.Validate(channel.Configuration); len(issues) > 0 {
		return nil, model.NewError(model.Validation, "invalid channel configuration")
	}
	inst := newInstance(channel.Clone(), f.Script(channel.Ref), f.v.caps)
	inst.creds, inst.clock = deps.Credentials, deps.Clock
	inst.sent = f.v.sent
	return f.v.open(inst), nil
}

// Script returns the script of channel, creating it on first use. A test may
// script a channel before or after opening it.
func (f *Factory) Script(channel model.ChannelRef) *Script {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.scripts[channel]
	if !ok {
		s = newScript(channel)
		s.shape, s.rename = f.v.shape, f.v.rename
		f.scripts[channel] = s
	}
	return s
}

var _ adapter.Factory = (*Factory)(nil)
