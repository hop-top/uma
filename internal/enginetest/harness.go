package enginetest

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/ingress"
	"hop.top/uma/internal/privacy"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// Ingress tests drive the engine-bound sink against the memory store with a
// test adapter that, like the external fake provider module, only forwards
// verified ingress it was scripted with.

const (
	ChatType   model.TypeKey = "com.example.ingress.chat"
	IngressNS                = "com.example.user"
	ServiceAct model.ActorID = "ingress-service"
	Dest                     = "support"
)

var T0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// IngressAdapter is a receive-only adapter whose instances deliver queued
// ingress to the sink they run with, declaring Exchange and SenderKinds.
type IngressAdapter struct {
	Typ         model.TypeKey
	Exchange    adapter.Exchange
	SenderKinds []string
}

// ChatSenderKinds are the sender kinds the harness chat adapter declares;
// the mail adapter declares none, so its senders are people.
var ChatSenderKinds = []string{model.ContactKindBot, model.ContactKindAgent}

// Exchange semantics of the harness adapters: a chat provider assigning
// message IDs, and mail threading on headers with sender-chosen
// Message-IDs.
var (
	ChatExchange = adapter.Exchange{MessageIDs: adapter.MessageIDsProvider}
	MailExchange = adapter.Exchange{MessageIDs: adapter.MessageIDsSender, Threading: adapter.ThreadingEmail}
)

func (a IngressAdapter) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: a.Typ, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: string(a.Typ) + ".ingress-test", Version: 1},
		Capabilities:        []model.Capability{model.CapabilityReceive},
		Events:              []string{model.EventMessageReceived, model.EventInteractionReceived},
		IdentityNamespaces:  []string{IngressNS},
		Exchange:            a.Exchange,
		SenderKinds:         a.SenderKinds,
	}
}
func (IngressAdapter) Validate(model.Extension) []model.ValidationIssue { return nil }
func (IngressAdapter) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	return &QueueReceiver{}, nil
}

// QueueReceiver delivers its queue in order; an ingress leaves the queue
// only once Accept succeeds.
type QueueReceiver struct{ Queue []model.Ingress }

func (*QueueReceiver) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return []model.CapabilityStatus{{Name: model.CapabilityReceive, Available: true}}, nil
}
func (*QueueReceiver) Close(context.Context) error { return nil }
func (r *QueueReceiver) Run(ctx context.Context, s adapter.Sink) error {
	for len(r.Queue) > 0 {
		if _, err := s.Accept(ctx, r.Queue[0]); err != nil {
			return err
		}
		r.Queue = r.Queue[1:]
	}
	return nil
}

// Harness is an engine over a memory store with one channel and the sink of
// a service principal bound to it.
type Harness struct {
	T       *testing.T
	Ctx     context.Context
	E       *engine.Engine
	RT      *runtime.Runtime
	St      store.Store
	Clock   *portstest.FixedClock
	Seed    *portstest.SequenceIDs
	Channel model.Channel
	Sink    *ingress.Sink
	Client  Client
}

// HarnessOption adjusts a harness composition before the engine is built.
type HarnessOption func(*engine.Config)

func WithSelector(name string, s policy.ConversationSelector) HarnessOption {
	return func(c *engine.Config) { c.Selectors[name] = s }
}

func NewHarness(t *testing.T, typ model.TypeKey, opts ...HarnessOption) *Harness {
	t.Helper()
	clock := portstest.NewFixedClock(T0)
	cfg := engine.Config{
		Store:      memory.New(memory.Options{Clock: clock}),
		Authorizer: &Allow{},
		Adapters: []adapter.Factory{
			IngressAdapter{Typ: ChatType, Exchange: ChatExchange, SenderKinds: ChatSenderKinds},
			IngressAdapter{Typ: model.TypeSmtpMail, Exchange: MailExchange},
		},
		Selectors: map[string]policy.ConversationSelector{},
		Clock:     clock,
		IDs:       portstest.NewSequenceIDs(1),
	}
	for _, o := range opts {
		o(&cfg)
	}
	h := &Harness{T: t, Ctx: t.Context(), E: MustNew(t, cfg), St: cfg.Store, Clock: clock, Seed: portstest.NewSequenceIDs(0x5eed)}
	h.RT = runtime.New(h.E, privacy.CascadeEffector())
	h.Channel = h.PutChannel(typ)
	var err error
	if h.Sink, err = ingress.Bind(h.Ctx, h.E, h.Service(), h.Channel.Ref); err != nil {
		t.Fatalf("bindSink: %v", err)
	}
	if h.Client, err = For(h.E, model.Principal{Tenant: TenantA, Actor: Actor}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *Harness) Service() model.Principal {
	return model.Principal{Tenant: TenantA, Actor: ServiceAct}
}

func (h *Harness) ID() model.ID {
	h.T.Helper()
	id, err := h.Seed.New(h.Ctx)
	if err != nil {
		h.T.Fatal(err)
	}
	return id
}

func (h *Harness) Tx(fn func(store.Tx) error) {
	h.T.Helper()
	if err := h.St.Transact(h.Ctx, TenantA, fn); err != nil {
		h.T.Fatalf("Transact: %v", err)
	}
}

func (h *Harness) View(fn func(store.Reader) error) {
	h.T.Helper()
	if err := h.St.View(h.Ctx, TenantA, fn); err != nil {
		h.T.Fatalf("View: %v", err)
	}
}

func (h *Harness) PutChannel(typ model.TypeKey) model.Channel {
	h.T.Helper()
	ch := model.Channel{
		Ref: model.ChannelRef{Tenant: TenantA, ID: h.ID()}, Type: typ,
		Configuration: model.Extension{Name: string(typ) + ".ingress-test", Version: 1}, Revision: "r1",
	}
	h.Tx(func(tx store.Tx) error { return tx.PutChannel(h.Ctx, ch, "") })
	return ch
}

func Flag(b bool) *bool { return &b }

func PerContact(lock bool) model.Selection {
	return model.Selection{Mode: model.ModePerContact, LockToSingle: Flag(lock), LifecycleRules: model.RuleSetContactLifecycleV1}
}

var OpSeq int

// Configure creates an inbox on the harness channel through
// Configuration.ConfigureInbox.
func (h *Harness) Configure(sel model.Selection, dests ...string) model.Inbox {
	h.T.Helper()
	in, err := h.ConfigureOn(h.Channel.Ref, sel, dests...)
	if err != nil {
		h.T.Fatalf("ConfigureInbox: %v", err)
	}
	return in
}

func (h *Harness) ConfigureOn(ch model.ChannelRef, sel model.Selection, dests ...string) (model.Inbox, error) {
	OpSeq++
	acc, err := h.Client.Configuration().ConfigureInbox(h.Ctx, model.ConfigureInbox{
		Command: model.Command{Call: model.Call{Correlation: "corr"}, Operation: model.OperationID(fmt.Sprintf("op-%d", OpSeq))},
		Inbox:   model.Inbox{Ref: model.InboxRef{Tenant: TenantA}, Channel: ch, Name: "inbox", Destinations: dests, Selection: sel},
	})
	if err != nil {
		return model.Inbox{}, err
	}
	ref, err := acc.Operation.Result.AsInbox()
	if err != nil {
		h.T.Fatal(err)
	}
	var in model.Inbox
	h.View(func(r store.Reader) error {
		var err error
		in, err = r.GetInbox(h.Ctx, ref)
		return err
	})
	return in, nil
}

// PutInbox seeds an inbox directly, bypassing ConfigureInbox checks.
func (h *Harness) PutInbox(ch model.ChannelRef, sel model.Selection, disabled bool, dests ...string) model.Inbox {
	h.T.Helper()
	in := model.Inbox{
		Ref: model.InboxRef{Tenant: TenantA, ID: h.ID()}, Channel: ch, Name: "seeded",
		Destinations: dests, Selection: sel, Disabled: disabled, Revision: "r1",
	}
	h.Tx(func(tx store.Tx) error { return tx.PutInbox(h.Ctx, in, "") })
	return in
}

func (h *Harness) SenderRef(id string) model.ExternalRef {
	return model.ExternalRef{Channel: h.Channel.Ref, Namespace: IngressNS, Kind: "user", ID: id}
}

func (h *Harness) Ref(kind, id, parent string) model.ExternalRef {
	return model.ExternalRef{Channel: h.Channel.Ref, Namespace: IngressNS, Kind: kind, ID: id, ParentID: parent}
}

// Inbound is a new inbound text message from sender to destination d.
func (h *Harness) Inbound(event, msgID, from, d string, refs ...model.ExternalRef) model.Envelope {
	return model.Envelope{
		EventID: event, MessageID: msgID, Destination: d, Kind: model.EnvelopeMessage,
		Sender: h.SenderRef(from), Assurance: "verified", References: refs,
		Content:  model.Content{Parts: []model.Part{{Kind: "text", Text: "text of " + event}}},
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: T0,
	}
}

// Interaction is an action interaction by sender from at destination d, with
// a response token valid for an hour and a three-second acknowledgement
// deadline. The event ID is the interaction ID; refs scope it as for
// Inbound and are also its context.
func (h *Harness) Interaction(id, from, d string, refs ...model.ExternalRef) model.Envelope {
	actor := h.SenderRef(from)
	return model.Envelope{
		EventID: id, Destination: d, Kind: model.EnvelopeInteraction,
		Sender: actor, Assurance: "verified", References: refs,
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: T0,
		Interaction: &model.InteractionInput{
			ID: id, Kind: model.InteractionAction, Action: "approve", Actor: actor, Context: slices.Clone(refs),
			Fields:        model.Extension{Name: "com.example.form", Version: 1, Data: []byte(`{"choice":"yes"}`)},
			ResponseToken: "tok-" + id, ResponseExpiresAt: T0.Add(time.Hour), Deadline: T0.Add(3 * time.Second),
		},
	}
}

func (h *Harness) Accept(env model.Envelope) (model.IngressReceipt, error) {
	return h.Sink.Accept(h.Ctx, model.Ingress{Call: model.Call{Correlation: "corr-" + model.CorrelationID(env.EventID)}, Envelope: env})
}

func (h *Harness) MustAccept(env model.Envelope) model.IngressReceipt {
	h.T.Helper()
	r, err := h.Accept(env)
	if err != nil {
		h.T.Fatalf("Accept(%s): %v", env.EventID, err)
	}
	return r
}

func All[T any](h *Harness, list func(store.Reader, context.Context, model.ListRequest) (model.Page[T], error), f model.Filter) []T {
	h.T.Helper()
	var out []T
	h.View(func(r store.Reader) error {
		req := model.ListRequest{Filter: f, Page: model.PageRequest{Limit: 1000}}
		for {
			p, err := list(r, h.Ctx, req)
			if err != nil {
				return err
			}
			out = append(out, p.Items...)
			if p.Complete {
				return nil
			}
			req.Page.After = p.Next
		}
	})
	return out
}

func (h *Harness) Conversations(in model.InboxRef) []model.Conversation {
	return All(h, store.Reader.Conversations, model.Filter{Inboxes: []model.InboxRef{in}})
}

func (h *Harness) Messages(in model.InboxRef) []model.Message {
	return All(h, store.Reader.Messages, model.Filter{Inboxes: []model.InboxRef{in}})
}

func (h *Harness) Identities(in model.InboxRef) []model.Identity {
	return All(h, store.Reader.Identities, model.Filter{Inboxes: []model.InboxRef{in}})
}

func (h *Harness) Contacts() []model.Contact {
	return All(h, store.Reader.Contacts, model.Filter{})
}

func (h *Harness) Events() []model.Event {
	h.T.Helper()
	var out []model.Event
	h.View(func(r store.Reader) error {
		p, err := r.Events(h.Ctx, model.Filter{}, "", 1000)
		out = p.Items
		return err
	})
	return out
}

// MessageByProviderID returns the stored message with provider ID id.
func (h *Harness) MessageByProviderID(in model.InboxRef, id string) model.Message {
	h.T.Helper()
	var ms []model.Message
	h.View(func(r store.Reader) error {
		var err error
		ms, err = r.MessagesByReference(h.Ctx, in, model.ProviderMessageRef(h.Channel.Ref, id), 2)
		return err
	})
	if len(ms) != 1 {
		h.T.Fatalf("messages with provider ID %q: %d, want 1", id, len(ms))
	}
	return ms[0]
}

func EventKinds(evs []model.Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = ev.Kind
	}
	return out
}
