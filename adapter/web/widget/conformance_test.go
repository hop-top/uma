package widget_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// The widget passes the shared adapter suite for every capability it
// declares, its inbound cases driven through the receiver mounted by
// Runtime.Webhook.
func TestAdapterConformance(t *testing.T) {
	f, _ := widget.New(widget.Options{Verifier: headerVerifier})
	conformance.RunAdapterSuite(t, conformance.AdapterHarness{
		New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
			log := &scriptedLog{mem: widget.NewMemoryLog(0)}
			f, err := widget.New(widget.Options{Verifier: headerVerifier, Log: log})
			if err != nil {
				t.Fatal(err)
			}
			return f, &provider{log: log}
		},
		Capabilities: f.Describe().Capabilities,
		// The widget answers through the visitor's log: it has no
		// response credential to hold.
		NoResponseToken: true,
	})
}

// scriptedLog is the visitor log as the suite's provider: a memory log
// whose appends take scripted answers per channel, recording every call.
// Signals take no scripted answer.
type scriptedLog struct {
	mem *widget.MemoryLog

	mu      sync.Mutex
	answers map[model.ChannelRef][]conformance.Response
	calls   map[model.ChannelRef][]logCall
}

type logCall struct {
	key     widget.LogKey
	entry   widget.Entry
	applied bool
}

func (s *scriptedLog) Append(ctx context.Context, key widget.LogKey, e widget.Entry) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	answer := conformance.Response{Outcome: conformance.OutcomeAccepted}
	if q := s.answers[key.Channel]; len(q) > 0 && e.Event.Type != widget.EventSignal {
		answer, s.answers[key.Channel] = q[0], q[1:]
	}
	applied := answer.Outcome == conformance.OutcomeAccepted || answer.Outcome == conformance.OutcomeAnswerLost
	if s.calls == nil {
		s.calls = map[model.ChannelRef][]logCall{}
	}
	s.calls[key.Channel] = append(s.calls[key.Channel], logCall{key: key, entry: e, applied: applied})
	var seq uint64
	if applied {
		var err error
		if seq, err = s.mem.Append(ctx, key, e); err != nil {
			return 0, err
		}
	}
	switch answer.Outcome {
	case conformance.OutcomeRejected:
		return 0, model.NewError(model.NotFound, "visitor gone")
	case conformance.OutcomeUnauthorized:
		return 0, model.NewError(model.Unauthorized, "message cannot be changed")
	case conformance.OutcomeTransient:
		return 0, model.NewError(model.Transient, "log unreachable")
	case conformance.OutcomeRateLimited:
		err := model.NewError(model.RateLimited, "log busy")
		err.RetryAfter = answer.RetryAfter
		return 0, err
	case conformance.OutcomeAnswerLost:
		return 0, errors.New("connection reset after write")
	case conformance.OutcomeRequestLost:
		return 0, model.NewError(model.OutcomeUnknown, "connection reset")
	}
	return seq, nil
}

func (s *scriptedLog) Read(ctx context.Context, key widget.LogKey, after uint64, limit int) ([]widget.Entry, error) {
	return s.mem.Read(ctx, key, after, limit)
}

func (s *scriptedLog) Watch(key widget.LogKey) (<-chan struct{}, func()) { return s.mem.Watch(key) }

// provider is the conformance view of the scripted log. The visitor side
// of the widget is a browser: Push plays it, opening an anonymous session
// for the event's sender on its site and posting the message with the
// event ID as client ID. There is no provider permission to revoke.
type provider struct {
	log *scriptedLog

	mu      sync.Mutex
	queued  map[model.ChannelRef][]conformance.InboundEvent
	results map[model.ChannelRef][]conformance.InboundResult
}

func (p *provider) Account(model.ChannelRef) conformance.Account {
	return conformance.Account{Configuration: widget.Configuration("support"), CredentialRef: "widget-key"}
}

func (p *provider) Route(ch model.ChannelRef, peer string) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: widget.NamespaceVisitor, Kind: widget.KindVisitor, ID: peer}
}

// ShapeEvent gives a message the ID the receiver derives from the visitor
// and the client ID. An interaction keeps the message its control sits on.
func (p *provider) ShapeEvent(_ model.ChannelRef, ev conformance.InboundEvent) conformance.InboundEvent {
	if ev.Interaction == nil {
		ev.MessageID = widget.InboundMessageID(ev.Sender, ev.EventID)
	}
	return ev
}

// ShapeID gives a provider message the shape of a widget message ID; every
// other fixture keeps its plain form.
func (p *provider) ShapeID(_ model.ChannelRef, kind conformance.FixtureKind, id string) string {
	if kind != conformance.FixtureMessage {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "m" + hex.EncodeToString(sum[:16])
}

// Deliver queues ev for Push. A visitor sends messages and interactions,
// and they are public: an update of a message (an edit, a delete, a
// reaction) or a private one is not expressible, and queuing it would
// render it as a new message. The widget sets the interaction's deadline
// itself, so a deadline of the provider's is not expressible either.
func (p *provider) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
	if ev.Kind != conformance.InboundMessage || ev.Visibility != "" && ev.Visibility != model.Public ||
		ev.Interaction != nil && (ev.Interaction.Deadline < 0 || ev.Interaction.Kind == model.InteractionOptions) {
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

// Push plays the visitor's browser for each queued event: a session
// request, then the message or the interaction. An event counts as acknowledged when the
// receiver accepted the message.
func (p *provider) Push(ch model.ChannelRef, h http.Handler) error {
	p.mu.Lock()
	evs := p.queued[ch]
	delete(p.queued, ch)
	p.mu.Unlock()
	for _, ev := range evs {
		acked := false
		if token, ok := visitorSession(h, ev); ok {
			path, body := visitorRequest(ev)
			code, _ := postJSON(h, path, body, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) })
			acked = code == http.StatusAccepted
		}
		p.mu.Lock()
		if p.results == nil {
			p.results = map[model.ChannelRef][]conformance.InboundResult{}
		}
		p.results[ch] = append(p.results[ch], conformance.InboundResult{EventID: ev.EventID, Acked: acked})
		p.mu.Unlock()
	}
	return nil
}

// visitorSession opens an anonymous session of the event's sender on its
// site and returns the token.
func visitorSession(h http.Handler, ev conformance.InboundEvent) (string, bool) {
	code, body := postJSON(h, "/widget/session", []byte("{}"), func(r *http.Request) {
		r.Header.Set("X-Visitor", ev.Sender)
		r.Header.Set("X-Assurance", widget.AssuranceAnonymous)
		r.Header.Set("X-Site", ev.Destination)
	})
	var s widget.SessionResponse
	if code != http.StatusOK || json.Unmarshal(body, &s) != nil || s.Token == "" {
		return "", false
	}
	return s.Token, true
}

func postJSON(h http.Handler, path string, body []byte, set func(*http.Request)) (int, []byte) {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	set(req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func (p *provider) Inbound(ch model.ChannelRef) []conformance.InboundResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.results[ch])
}

func (p *provider) Respond(ch model.ChannelRef, rs ...conformance.Response) error {
	p.log.mu.Lock()
	defer p.log.mu.Unlock()
	if p.log.answers == nil {
		p.log.answers = map[model.ChannelRef][]conformance.Response{}
	}
	p.log.answers[ch] = append(p.log.answers[ch], rs...)
	return nil
}

func (p *provider) Revoke(model.ChannelRef, model.Capability) error {
	return conformance.ErrNotExpressible
}

var callOf = map[string]struct {
	capability model.Capability
	step       string
}{
	widget.EventMessage:       {model.CapabilitySend, ""},
	widget.EventMessageEdit:   {model.CapabilityEdit, ""},
	widget.EventMessageDelete: {model.CapabilityDelete, ""},
	widget.EventReaction:      {model.CapabilityReact, ""},
	widget.EventReceipt:       {model.CapabilityReceipt, ""},
	widget.EventEphemeral:     {model.CapabilitySendEphemeral, ""},
	widget.EventSignal:        {model.CapabilitySignal, ""},
	widget.EventStreamStart:   {model.CapabilityStream, conformance.StepStart},
	widget.EventStreamUpdate:  {model.CapabilityStream, conformance.StepUpdate},
	widget.EventStreamFinish:  {model.CapabilityStream, conformance.StepFinish},
	widget.EventStreamCancel:  {model.CapabilityStream, conformance.StepCancel},

	widget.EventInteractionResponse: {model.CapabilityRespond, ""},
}

func (p *provider) Calls(ch model.ChannelRef) []conformance.ProviderCall {
	p.log.mu.Lock()
	defer p.log.mu.Unlock()
	var out []conformance.ProviderCall
	for _, c := range p.log.calls[ch] {
		ev := c.entry.Event
		kind := widget.KindVisitor
		if c.key.Namespace == widget.NamespaceUser {
			kind = widget.KindUser
		}
		to := model.ExternalRef{Channel: ch, Namespace: c.key.Namespace, Kind: kind, ID: c.key.Visitor}
		if c.entry.Session != "" {
			to = model.ExternalRef{Channel: ch, Namespace: c.key.Namespace, Kind: model.RefKindRoom, ID: c.entry.Session, ParentID: c.key.Visitor}
		}
		call := conformance.ProviderCall{
			Capability: callOf[ev.Type].capability, Step: callOf[ev.Type].step, Operation: c.entry.Operation,
			Route: to, Applied: c.applied, Sequence: ev.Sequence, Kind: ev.Kind,
			Audience: model.Audience{Visibility: model.Public},
		}
		for _, part := range ev.Parts {
			call.Content.Parts = append(call.Content.Parts, model.Part{Kind: part.Kind, Text: part.Text})
		}
		if ev.Type == widget.EventEphemeral {
			call.Recipient = model.ExternalRef{Channel: ch, Namespace: c.key.Namespace, Kind: string(model.KindIdentity), ID: c.key.Visitor}
			call.Audience = model.Audience{Visibility: model.Restricted}
		}
		if c.applied && ev.Message != "" {
			call.References = []model.ExternalRef{widget.MessageRef(ch, ev.Message)}
		}
		switch ev.Type {
		case widget.EventMessageEdit, widget.EventMessageDelete, widget.EventReaction, widget.EventReceipt:
			call.Target, call.Emoji, call.Remove = widget.MessageRef(ch, ev.Message), ev.Emoji, ev.Removed
		case widget.EventInteractionResponse:
			// The widget has no response token: the call carries none.
			call.Mode, call.FollowUp, call.Interaction = ev.Mode, ev.FollowUp, c.entry.Interaction
			switch ev.Mode {
			case model.ResponseModeUpdate:
				call.Target = widget.MessageRef(ch, ev.Message)
			case model.ResponseModeEphemeral:
				call.Audience = model.Audience{Visibility: model.Restricted}
			}
		}
		out = append(out, call)
	}
	return out
}
