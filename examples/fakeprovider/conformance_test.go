package fakeprovider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/conformance"
	"hop.top/uma/examples/fakeprovider"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// Every variant passes the shared adapter suite, each running only the
// cases its capabilities imply.
func TestAdapterConformance(t *testing.T) {
	for _, v := range []struct {
		name string
		new  func() *fakeprovider.Factory
	}{
		{"ingress-only", fakeprovider.NewIngressOnly},
		{"send-only", fakeprovider.NewSendOnly},
		{"full", fakeprovider.NewFull},
		{"webhook", fakeprovider.NewWebhook},
		{"interactive", fakeprovider.NewInteractive},
		{"respond-only", fakeprovider.NewInteractive},
		{"mail", fakeprovider.NewMail},
		{"tracker", fakeprovider.NewTracker},
		{"read-receipts", fakeprovider.NewFull},
		{"state-labels", fakeprovider.NewStateLabels},
		{"state-folders", fakeprovider.NewStateFolders},
		{"state-folders-stable", fakeprovider.NewStateFoldersStable},
	} {
		t.Run(v.name, func(t *testing.T) {
			h := harness(v.new)
			if v.name == "read-receipts" {
				// The full variant on a provider with read receipts only.
				h.New = func(*testing.T) (adapter.Factory, conformance.Provider) {
					f := v.new()
					return readReceiptsFactory{f}, &provider{f: f}
				}
				h.UnsupportedReceiptKinds = []string{model.ReceiptDelivered}
			}
			// A folder move on the first of these gives an item a new reference.
			h.MoveChangesReference = v.name == "state-folders"
			if strings.HasPrefix(v.name, "state-") {
				h.New = func(*testing.T) (adapter.Factory, conformance.Provider) {
					f := v.new()
					return f, &stateProvider{provider: &provider{f: f}}
				}
			}
			if v.name == "tracker" {
				h.New = func(*testing.T) (adapter.Factory, conformance.Provider) {
					f := v.new()
					return f, &trackerProvider{provider: &provider{f: f}}
				}
			}
			if v.name == "respond-only" {
				// The interactive variant without follow-ups: the factory
				// declares respond alone and its instances report it.
				h.New = func(*testing.T) (adapter.Factory, conformance.Provider) {
					f := v.new()
					return respondOnlyFactory{f}, &provider{f: f}
				}
				h.Capabilities = respondOnlyFactory{v.new()}.Describe().Capabilities
			}
			if v.name == "webhook" {
				h.New = func(*testing.T) (adapter.Factory, conformance.Provider) {
					f := v.new()
					return f, &webhookProvider{provider: &provider{f: f}}
				}
				// An interaction is acknowledged only once durably
				// accepted: the suite's store reports itself durable.
				h.Store = func(_ *testing.T, clock ports.Clock) store.Store {
					return durableStore{memory.New(memory.Options{Clock: clock})}
				}
			}
			conformance.RunAdapterSuite(t, h)
		})
	}
}

// stateProvider is the conformance view of a state variant's items: the
// state it holds for each and the reports it makes of a conversation.
type stateProvider struct{ *provider }

func (p *stateProvider) ItemState(ch model.ChannelRef, ref model.ExternalRef) (conformance.ItemState, bool) {
	s, _ := p.script(ch)
	it, ok := s.Item(ref.ID)
	return conformance.ItemState{
		Archived: it.Archived, Trashed: it.Trashed, Read: it.Read, Folder: it.Folder, Labels: it.Labels, Flags: it.Flags,
	}, ok
}

// DeliverState renders ev as the fake's conversation.state envelope: the
// conversation is named by one of its provider messages.
func (p *stateProvider) DeliverState(ch model.ChannelRef, ev conformance.StateEvent) error {
	s, acct := p.script(ch)
	s.Push(model.Ingress{Envelope: model.Envelope{
		EventID: ev.EventID, Destination: ev.Destination, Kind: model.EnvelopeConversationState,
		SystemActor: true, Assurance: "verified", OccurredAt: ev.State.ObservedAt,
		State: &model.ProviderStateReport{Conversation: model.ProviderMessageRef(acct, ev.MessageID), State: ev.State},
	}})
	return nil
}

// respondOnlyFactory is the interactive variant declaring respond but not
// respond_follow_up.
type respondOnlyFactory struct{ *fakeprovider.Factory }

func (f respondOnlyFactory) Describe() adapter.Descriptor {
	d := f.Factory.Describe()
	d.Capabilities = slices.DeleteFunc(d.Capabilities, func(c model.Capability) bool { return c == model.CapabilityRespondFollowUp })
	return d
}

func (f respondOnlyFactory) Open(ctx context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	inst, err := f.Factory.Open(ctx, ch, deps)
	if err != nil {
		return nil, err
	}
	return respondOnlyInstance{inst.(*fakeprovider.Interactive)}, nil
}

type respondOnlyInstance struct{ *fakeprovider.Interactive }

func (i respondOnlyInstance) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	st, err := i.Interactive.Capabilities(ctx)
	return slices.DeleteFunc(st, func(s model.CapabilityStatus) bool { return s.Name == model.CapabilityRespondFollowUp }), err
}

// trackerProvider holds the tracker variant's work items: its peers are
// work items of a desk.
type trackerProvider struct{ *provider }

func (p *trackerProvider) Route(ch model.ChannelRef, peer string) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: fakeprovider.Namespace, Kind: model.RefKindWorkItem, ID: peer, ParentID: "desk"}
}

func (p *trackerProvider) SeedWorkItem(ch model.ChannelRef, peer, status string) (model.ExternalRef, error) {
	route := p.Route(ch, peer)
	s, _ := p.script(ch)
	s.SeedWorkItem(route, status)
	return route, nil
}

func (p *trackerProvider) WorkItemStatus(ch model.ChannelRef, work model.ExternalRef) (string, error) {
	s, _ := p.script(ch)
	status, ok := s.WorkItemStatus(work)
	if !ok {
		return "", fmt.Errorf("no work item %s", work.ID)
	}
	return status, nil
}

// webhookProvider calls the webhook variant's handler as the fake provider
// does: one signed WebhookEvent per request.
type webhookProvider struct {
	*provider

	mu      sync.Mutex
	queued  map[model.ChannelRef][]conformance.InboundEvent
	results map[model.ChannelRef][]conformance.InboundResult
}

// Deliver queues ev for Push. The webhook body carries no audience nor update, so
// a private event and an edit, deletion or reaction are not expressible; claims have no field and are dropped.
func (p *webhookProvider) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
	if (ev.Visibility != "" && ev.Visibility != model.Public) || ev.Kind != conformance.InboundMessage {
		return conformance.ErrNotExpressible
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.queued == nil {
		p.queued = map[model.ChannelRef][]conformance.InboundEvent{}
	}
	p.queued[ch] = append(p.queued[ch], ev)
	return nil
}

func (p *webhookProvider) Push(ch model.ChannelRef, h http.Handler) error {
	p.mu.Lock()
	evs := p.queued[ch]
	delete(p.queued, ch)
	p.mu.Unlock()
	for _, ev := range evs {
		body, err := json.Marshal(webhookBody(ev))
		if err != nil {
			return err
		}
		req := httptest.NewRequest(http.MethodPost, "/hooks/fake", bytes.NewReader(body))
		req.Header.Set(fakeprovider.SignatureHeader, fakeprovider.Sign([]byte(conformance.CredentialSecret), body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		p.mu.Lock()
		if p.results == nil {
			p.results = map[model.ChannelRef][]conformance.InboundResult{}
		}
		p.results[ch] = append(p.results[ch], conformance.InboundResult{EventID: ev.EventID, Acked: rec.Code == http.StatusOK})
		p.mu.Unlock()
	}
	return nil
}

// webhookBody renders ev as the fake provider's webhook body: a message, or
// an interaction whose MessageID names the message the control sits on.
func webhookBody(ev conformance.InboundEvent) fakeprovider.WebhookEvent {
	at := time.Unix(1_700_000_000, 0).UTC()
	body := fakeprovider.WebhookEvent{
		EventID: ev.EventID, MessageID: ev.MessageID, Destination: ev.Destination,
		Sender: ev.Sender, Text: ev.Text, OccurredAt: at,
	}
	if x := ev.Interaction; x != nil {
		kind := x.Kind
		if kind == "" {
			kind = model.InteractionAction
		}
		body.MessageID, body.Text = "", ""
		body.Interaction = &fakeprovider.WebhookInteraction{
			Kind: kind, Action: x.Action, Command: x.Command, Arguments: x.Arguments, Fields: x.Fields,
			Message: ev.MessageID, Token: x.Token, Deadline: at.Add(x.Deadline),
		}
		if x.Token != "" {
			body.Interaction.TokenExpiresAt = at.Add(x.TokenTTL)
		}
	}
	return body
}

func (p *webhookProvider) Inbound(ch model.ChannelRef) []conformance.InboundResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.results[ch])
}

// Fixture values reach the provider in its own shape: a provider shaping
// them email-like refuses anything else, so a suite skipping the shaping
// fails here.
func TestAdapterConformanceShapedFixtures(t *testing.T) {
	h := harness(fakeprovider.NewFull)
	h.New = func(*testing.T) (adapter.Factory, conformance.Provider) {
		f := fakeprovider.NewFull()
		return f, shapedProvider{&provider{f: f}}
	}
	conformance.RunAdapterSuite(t, h)
}

// shapedProvider shapes fixture values as email addresses and Message-IDs.
type shapedProvider struct{ *provider }

const (
	shapedDomain  = "@example.test"
	shapedMsgTail = "@fixtures.example.test>"
)

func (shapedProvider) ShapeID(_ model.ChannelRef, kind conformance.FixtureKind, id string) string {
	switch kind {
	case conformance.FixtureEvent, conformance.FixtureMessage:
		return "<" + id + shapedMsgTail
	case conformance.FixtureSender, conformance.FixtureDestination:
		return id + shapedDomain
	}
	return id
}

func (p shapedProvider) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
	for _, v := range []string{ev.EventID, ev.MessageID} {
		if !strings.HasPrefix(v, "<") || !strings.HasSuffix(v, shapedMsgTail) {
			return fmt.Errorf("unshaped provider ID %q", v)
		}
	}
	for _, v := range append([]string{ev.Sender, ev.Destination}, ev.Claims...) {
		if !strings.HasSuffix(v, shapedDomain) {
			return fmt.Errorf("unshaped address %q", v)
		}
	}
	return p.provider.Deliver(ch, ev)
}

// Route answers an unshaped peer with no route, which no stored route
// matches.
func (p shapedProvider) Route(ch model.ChannelRef, peer string) model.ExternalRef {
	if !strings.HasSuffix(peer, shapedDomain) {
		return model.ExternalRef{}
	}
	return p.provider.Route(ch, peer)
}

func harness(newFactory func() *fakeprovider.Factory) conformance.AdapterHarness {
	h := conformance.AdapterHarness{
		New: func(*testing.T) (adapter.Factory, conformance.Provider) {
			f := newFactory()
			return f, &provider{f: f}
		},
		Capabilities: newFactory().Describe().Capabilities,
		Extensions:   newFactory().Extensions(),
		Action:       pinAction(),
	}
	if slices.Contains(newFactory().Describe().Events, fakeprovider.ExtensionPinned) {
		h.Event = pinnedEvent()
	}
	return h
}

// pinnedEvent is a valid fake ExtensionPinned event.
func pinnedEvent() model.Extension {
	return model.Extension{
		Name: fakeprovider.ExtensionPinned, Version: fakeprovider.ExtensionPinnedVersion,
		Data: json.RawMessage(`{"message":"m1","pin":"p1"}`),
	}
}

// DeliverEvent renders ev as the fake's native notice, emitted by the
// account's script.
func (p *provider) DeliverEvent(ch model.ChannelRef, ev conformance.ExtensionEvent) error {
	s, _ := p.script(ch)
	s.Emit(ev.EventID, ev.Destination, ev.MessageID, model.Audience{Visibility: model.Public}, ev.Event)
	return nil
}

// pinAction is a valid fake ExtensionPin action.
func pinAction() model.Extension {
	return model.Extension{
		Name: fakeprovider.ExtensionPin, Version: fakeprovider.ExtensionPinVersion,
		Data: json.RawMessage(`{"message":"m1"}`),
	}
}

// eventPayload is the provider payload of an inbound fake event: data the
// provider does not authenticate.
const eventPayload = "org.example.fake.event"

// provider is the conformance view of the fake's scripts. account maps a
// channel to the script that serves it; nil maps every channel to its own.
type provider struct {
	f       *fakeprovider.Factory
	account func(model.ChannelRef) model.ChannelRef
}

func (p *provider) script(ch model.ChannelRef) (*fakeprovider.Script, model.ChannelRef) {
	if p.account != nil {
		ch = p.account(ch)
	}
	return p.f.Script(ch), ch
}

// Account gives every channel the same credential reference: the engine,
// not the reference, keeps two accounts apart.
func (p *provider) Account(model.ChannelRef) conformance.Account {
	return conformance.Account{Configuration: p.f.Config(), CredentialRef: "fake-cred"}
}

func (p *provider) Route(ch model.ChannelRef, peer string) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: fakeprovider.Namespace, Kind: "user", ID: peer}
}

// updateKinds maps the suite's update events to the fake's native kinds.
var updateKinds = map[conformance.InboundKind]string{
	conformance.InboundMessage:         model.EnvelopeMessage,
	conformance.InboundEdited:          model.EnvelopeMessageEdited,
	conformance.InboundDeleted:         model.EnvelopeMessageDeleted,
	conformance.InboundReactionAdded:   model.EnvelopeReactionAdded,
	conformance.InboundReactionRemoved: model.EnvelopeReactionRemoved,
	conformance.InboundReactionSummary: model.EnvelopeReactionAdded,
}

// Deliver renders ev as the fake's native event: a normalized ingress on
// the account's channel.
func (p *provider) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
	s, acct := p.script(ch)
	env, err := p.envelope(acct, ev)
	if err != nil {
		return err
	}
	s.Push(model.Ingress{Envelope: env})
	return nil
}

// envelope is the fake's native envelope of ev on account acct.
func (p *provider) envelope(acct model.ChannelRef, ev conformance.InboundEvent) (model.Envelope, error) {
	if ev.Interaction != nil {
		return model.Envelope{}, conformance.ErrNotExpressible
	}
	visibility := ev.Visibility
	if visibility == "" {
		visibility = model.Public
	}
	kind, ok := updateKinds[ev.Kind]
	if !ok {
		return model.Envelope{}, conformance.ErrNotExpressible
	}
	env := model.Envelope{
		EventID: ev.EventID, MessageID: ev.MessageID, Destination: ev.Destination, Kind: kind,
		Sender:     p.Route(acct, ev.Sender),
		References: []model.ExternalRef{{Channel: acct, Namespace: fakeprovider.Namespace, Kind: model.RefKindRoom, ID: ev.Room}},
		Audience:   model.Audience{Visibility: visibility}, SourceRevision: ev.SourceRevision,
		OccurredAt: time.Unix(1_700_000_000, 0).UTC().Add(ev.OccurredAfter),
	}
	switch ev.Kind {
	case conformance.InboundMessage, conformance.InboundEdited:
		env.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: ev.Text}}}
	case conformance.InboundReactionAdded, conformance.InboundReactionRemoved:
		if ev.Emoji != "" {
			env.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: ev.Emoji}}}
		}
	case conformance.InboundReactionSummary:
		data, err := json.Marshal(model.ReactionSummary{Counts: ev.Counts, Complete: ev.Complete})
		if err != nil {
			return model.Envelope{}, err
		}
		env.Sender, env.SystemActor = model.ExternalRef{}, true
		env.Content = model.Content{Parts: []model.Part{{Kind: model.PartStructured, Structured: &model.Extension{
			Name: model.SchemaReactionSummary, Version: 1, Data: data,
		}}}}
	}
	if ev.Echo != nil {
		env.References = append(env.References, model.EchoRef(acct, ev.Echo.Operation, ev.Echo.Attempt))
	}
	if len(ev.Claims) > 0 {
		data, err := json.Marshal(map[string][]string{"claimed_senders": ev.Claims})
		if err != nil {
			return model.Envelope{}, err
		}
		env.Payload = model.Extension{Name: eventPayload, Version: 1, Data: data}
	}
	return env, nil
}

func (p *provider) Inbound(ch model.ChannelRef) []conformance.InboundResult {
	s, _ := p.script(ch)
	var out []conformance.InboundResult
	for _, d := range s.Deliveries() {
		out = append(out, conformance.InboundResult{EventID: d.Ingress.Envelope.EventID, Acked: d.Acked})
	}
	return out
}

var results = map[conformance.Outcome]fakeprovider.Result{
	conformance.OutcomeAccepted:    fakeprovider.Accepted,
	conformance.OutcomeRejected:    fakeprovider.Rejected,
	conformance.OutcomeTransient:   fakeprovider.Transient,
	conformance.OutcomeRateLimited: fakeprovider.RateLimited,
	conformance.OutcomeAnswerLost:  fakeprovider.OutcomeUnknown,
	conformance.OutcomeRequestLost: fakeprovider.RequestLost,

	conformance.OutcomeUnauthorized: fakeprovider.Unauthorized,
}

// Respond scripts results in order. The fake states one Retry-After per
// account, so differing waits are not expressible.
func (p *provider) Respond(ch model.ChannelRef, rs ...conformance.Response) error {
	s, _ := p.script(ch)
	var after time.Duration
	out := make([]fakeprovider.Result, 0, len(rs))
	for _, r := range rs {
		res, ok := results[r.Outcome]
		if !ok || (r.Outcome == conformance.OutcomeRateLimited && after != 0 && r.RetryAfter != after) {
			return conformance.ErrNotExpressible
		}
		if r.Outcome == conformance.OutcomeRateLimited {
			after = r.RetryAfter
		}
		out = append(out, res)
	}
	s.RetryAfter(after)
	s.Respond(out...)
	return nil
}

var capabilityOf = map[fakeprovider.Method]model.Capability{
	fakeprovider.MethodSend:         model.CapabilitySend,
	fakeprovider.MethodEdit:         model.CapabilityEdit,
	fakeprovider.MethodDelete:       model.CapabilityDelete,
	fakeprovider.MethodReact:        model.CapabilityReact,
	fakeprovider.MethodReceipt:      model.CapabilityReceipt,
	fakeprovider.MethodMembership:   model.CapabilityMembership,
	fakeprovider.MethodReconcile:    model.CapabilityReconcile,
	fakeprovider.MethodExecute:      model.CapabilityExtensionExecute,
	fakeprovider.MethodStreamStart:  model.CapabilityStream,
	fakeprovider.MethodStreamUpdate: model.CapabilityStream,
	fakeprovider.MethodStreamFinish: model.CapabilityStream,
	fakeprovider.MethodStreamCancel: model.CapabilityStream,
	fakeprovider.MethodEphemeral:    model.CapabilitySendEphemeral,
	fakeprovider.MethodSignal:       model.CapabilitySignal,
	fakeprovider.MethodRespond:      model.CapabilityRespond,
	fakeprovider.MethodArchive:      model.CapabilityArchive,
	fakeprovider.MethodMove:         model.CapabilityMove,
	fakeprovider.MethodReadState:    model.CapabilityReadState,
	fakeprovider.MethodFlag:         model.CapabilityFlag,
	fakeprovider.MethodTrash:        model.CapabilityTrash,

	fakeprovider.MethodActWorkItem: model.CapabilityWorkItemAct,
}

var stepOf = map[fakeprovider.Method]string{
	fakeprovider.MethodStreamStart:  conformance.StepStart,
	fakeprovider.MethodStreamUpdate: conformance.StepUpdate,
	fakeprovider.MethodStreamFinish: conformance.StepFinish,
	fakeprovider.MethodStreamCancel: conformance.StepCancel,
}

func (p *provider) Calls(ch model.ChannelRef) []conformance.ProviderCall {
	s, _ := p.script(ch)
	var out []conformance.ProviderCall
	for _, c := range s.Calls() {
		out = append(out, conformance.ProviderCall{
			Capability: capabilityOf[c.Method], Operation: c.Effect.Operation, Route: c.Effect.Route,
			Applied: c.Applied, References: c.References, Audience: c.Effect.Audience, Action: c.Action,
			Step: stepOf[c.Method], Sequence: c.Sequence, Content: c.Content, Recipient: c.Recipient, Kind: c.Kind,
			Status: c.Status, Reason: c.Reason,
			Attempt: c.Effect.Attempt, Emoji: c.Emoji, Remove: c.Remove, Target: target(c), Participant: c.Participant,
			Mode: respondMode(c), FollowUp: c.FollowUp, Interaction: interactionOf(c), Token: tokenOf(c),
			Items: c.Items, Undo: c.Undo, Read: c.Read, FlagKind: flagKind(c), Destination: c.Destination,
			AddLabels: c.Add, RemoveLabels: c.Drop,
		})
	}
	return out
}

// flagKind is a flag call's kind; other calls have none.
func flagKind(c fakeprovider.Call) string {
	if c.Method == fakeprovider.MethodFlag {
		return c.Kind
	}
	return ""
}

// respondMode is an interaction response's mode; other calls have none.
func respondMode(c fakeprovider.Call) string {
	if c.Method == fakeprovider.MethodRespond {
		return c.Mode
	}
	return ""
}

func interactionOf(c fakeprovider.Call) model.InteractionRef {
	if ic := c.Effect.Interaction; ic != nil {
		return ic.Ref
	}
	return model.InteractionRef{}
}

func tokenOf(c fakeprovider.Call) string {
	if ic := c.Effect.Interaction; ic != nil {
		return ic.Token
	}
	return ""
}

// target is the provider message an edit, deletion, reaction or update
// response acted on.
func target(c fakeprovider.Call) model.ExternalRef {
	switch c.Method {
	case fakeprovider.MethodEdit, fakeprovider.MethodDelete, fakeprovider.MethodReact, fakeprovider.MethodRespond, fakeprovider.MethodReceipt:
		if c.Effect.Target != nil {
			return *c.Effect.Target
		}
	}
	return model.ExternalRef{}
}

func (p *provider) Revoke(ch model.ChannelRef, c model.Capability) error {
	s, _ := p.script(ch)
	s.Revoke(c)
	return nil
}
