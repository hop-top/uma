package fakeprovider

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Result is the scripted provider result of one outbound call.
type Result string

const (
	// Accepted: the provider applied the effect and answered. The outcome is
	// model.ProviderAccepted with the provider reference.
	Accepted Result = "accepted"
	// Rejected: the provider refused the effect. The outcome is
	// model.Rejected with a model.Permanent failure, and nothing was applied.
	Rejected Result = "rejected"
	// OutcomeUnknown: the provider applied the effect but its answer was
	// lost. The outcome is model.Unknown with a model.OutcomeUnknown failure
	// and no reference; Reconcile later reports the applied effect.
	OutcomeUnknown Result = "outcome_unknown"
	// Transient: the call failed before reaching the provider. It returns a
	// model.Transient error, nothing was applied, and a retry is safe.
	Transient Result = "transient"
	// RequestLost: the request was lost before the provider applied it, and
	// the adapter cannot tell. The outcome is model.Unknown with a
	// model.OutcomeUnknown failure and no reference; nothing was applied, so
	// Reconcile later finds no record and a retry is safe.
	RequestLost Result = "request_lost"
	// Unauthorized: the provider refused an edit, deletion or reaction by the
	// account's own ownership rule, such as editing a message the account
	// did not write. The outcome is model.Rejected with a model.Unauthorized
	// failure that cannot be retried, and nothing was applied.
	Unauthorized Result = "unauthorized"
	// RateLimited: the provider refused the call for now. It returns a
	// model.RateLimited error carrying the script's Retry-After (see
	// Script.RetryAfter); nothing was applied.
	RateLimited Result = "rate_limited"
)

// Method names an adapter call recorded by a Script.
type Method string

const (
	MethodSend       Method = "send"
	MethodEdit       Method = "edit"
	MethodDelete     Method = "delete"
	MethodReact      Method = "react"
	MethodReceipt    Method = "receipt"
	MethodMembership Method = "membership"
	MethodReconcile  Method = "reconcile"
	MethodExecute    Method = "execute"
	MethodArchive    Method = "archive"
	MethodMove       Method = "move"
	MethodReadState  Method = "read_state"
	MethodFlag       Method = "flag"
	MethodTrash      Method = "trash"
)

// Call is one recorded outbound call. Applied reports whether the provider
// carried out the effect, whatever the caller was told. Mode, Sequence and
// Stream describe stream calls, Recipient an ephemeral send, Kind a signal
// or a receipt, and ExpiresAt an ephemeral send or a signal.
type Call struct {
	Method    Method
	Effect    adapter.Effect
	Content   model.Content
	Emoji     string
	Remove    bool
	Action    *model.Extension
	Mode      string
	Sequence  uint64
	Stream    string
	Recipient model.ExternalRef
	Kind      string
	ExpiresAt time.Time
	// FollowUp marks an interaction response as a follow-up; the
	// interaction it answers, token included, is Effect.Interaction.
	FollowUp bool
	// Participant is a membership change's participant; Remove then says
	// whether it is removed.
	Participant model.ExternalRef
	// Status and Reason are a work item act's requested status and reason.
	Status string
	Reason string
	// Items, Undo, Read, Destination, Add and Drop are a state call's
	// request (Kind is a flag's): the provider items it named, whether it
	// undoes, the read value, a move's destination and label changes.
	// Changes are the new references an applied move reported.
	Items       []model.ExternalRef
	Undo        bool
	Read        bool
	Destination model.MoveDestination
	Add, Drop   []string
	Changes     []adapter.ReferenceChange
	Result      Result
	Applied     bool
	References  []model.ExternalRef
}

// Delivery is one inbound ingress the provider handed to a sink. Acked
// reports whether Accept succeeded; an unacknowledged ingress stays queued
// and is redelivered by the next Run.
type Delivery struct {
	Ingress model.Ingress
	Receipt model.IngressReceipt
	Err     error
	Acked   bool
}

// Ack is one transport acknowledgement the webhook variant gave the
// provider for an interaction: At is the instant it answered 200 and
// Deadline the protocol deadline the interaction stated, as the engine's
// clock saw them. Late reports an acknowledgement at or after the
// deadline, which a real provider would have counted as no answer.
type Ack struct {
	EventID  string
	At       time.Time
	Deadline time.Time
	Late     bool
}

// Script is the scripted provider account behind one channel. It is safe for
// concurrent use.
type Script struct {
	channel model.ChannelRef

	mu       sync.Mutex
	inbound  []model.Ingress
	wake     chan struct{}
	results  []Result
	fallback Result
	after    time.Duration
	revoked  map[model.Capability]bool
	calls    []Call
	deliver  []Delivery
	acks     []Ack
	seq      uint64
	// streams maps an open provider stream to its message.
	streams map[string]model.ExternalRef
	// work maps a work item (by reference ID) to the status it holds, and
	// acts counts the applied work item acts.
	work map[string]string
	acts int
	// history is the provider's message history (HoldHistory), and
	// historyReads the history requests it answered.
	history      []model.Envelope
	historyReads []adapter.HistoryRequest
	// items are the provider items of a state variant, by provider message
	// ID; shape and rename describe how the provider holds and moves them
	// (see state.go).
	items  map[string]*Item
	shape  model.MoveShape
	rename bool
	moves  int

	// delivering serializes Receiver.Run deliveries from instances sharing
	// this script, so each queued ingress reaches one sink at a time.
	delivering sync.Mutex
}

func newScript(channel model.ChannelRef) *Script {
	return &Script{channel: channel, wake: make(chan struct{}, 1), fallback: Accepted, revoked: map[model.Capability]bool{}, work: map[string]string{}}
}

// Channel returns the channel this script belongs to.
func (s *Script) Channel() model.ChannelRef { return s.channel }

// Push queues inbound ingress for delivery by Receiver.Run, in order.
func (s *Script) Push(in ...model.Ingress) {
	s.mu.Lock()
	for _, i := range in {
		s.inbound = append(s.inbound, cloneIngress(i))
		if i.Envelope.Kind == model.EnvelopeMessage && i.Envelope.MessageID != "" {
			s.hold(i.Envelope.MessageID)
		}
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Emit queues a provider notice of the registered event kind ev.Name for
// delivery by Receiver.Run, as an extension.event ingress to destination
// under audience: about the message with provider ID message, or about the
// inbox when message is empty. ev.Data holds provider references and a
// change summary only.
func (s *Script) Emit(eventID, destination, message string, audience model.Audience, ev model.Extension) {
	ev = ev.Clone()
	s.Push(model.Ingress{Call: model.Call{Correlation: model.CorrelationID("fake-" + eventID)}, Envelope: model.Envelope{
		EventID: eventID, MessageID: message, Destination: destination, Kind: model.EnvelopeExtensionEvent,
		Audience: audience, OccurredAt: time.Unix(1_700_000_000, 0).UTC(), Event: &ev,
	}})
}

// Interact queues a provider interaction for delivery by Receiver.Run, as
// an interaction ingress to destination under audience: the actor's action,
// command or submission described by in, whose ID is the event ID. It
// belongs to a receive variant whose Events declare interaction.received.
// The envelope's sender is in.Actor, as the sink requires.
func (s *Script) Interact(destination string, audience model.Audience, in model.InteractionInput) {
	in = in.Clone()
	s.Push(model.Ingress{Call: model.Call{Correlation: model.CorrelationID("fake-" + in.ID)}, Envelope: model.Envelope{
		EventID: in.ID, Destination: destination, Kind: model.EnvelopeInteraction, Sender: in.Actor, Assurance: "verified",
		Audience: audience, OccurredAt: time.Unix(1_700_000_000, 0).UTC(), Interaction: &in,
	}})
}

// Respond queues results for the next outbound calls (send, edit, delete,
// react, receipt, membership, execute), consumed in call order. Reconcile consumes none.
func (s *Script) Respond(results ...Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = append(s.results, results...)
}

// Otherwise sets the result of outbound calls once the queued results are
// used up. The default is Accepted.
func (s *Script) Otherwise(r Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fallback = r
}

// RetryAfter sets the wait RateLimited results report. The default is none.
func (s *Script) RetryAfter(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.after = d
}

// Revoke withdraws the provider permission behind each capability: the
// instance reports it unavailable with reason model.Unauthorized, and calls
// that need it fail with model.Unauthorized without reaching the provider.
func (s *Script) Revoke(caps ...model.Capability) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range caps {
		s.revoked[c] = true
	}
}

// Calls returns the recorded outbound calls in order.
func (s *Script) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Call, len(s.calls))
	for i, c := range s.calls {
		out[i] = cloneCall(c)
	}
	return out
}

// Deliveries returns the recorded inbound deliveries in order.
func (s *Script) Deliveries() []Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Delivery, len(s.deliver))
	for i, d := range s.deliver {
		d.Ingress = cloneIngress(d.Ingress)
		out[i] = d
	}
	return out
}

// Acks returns the recorded transport acknowledgements in order.
func (s *Script) Acks() []Ack {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.acks)
}

func (s *Script) acked(a Ack) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acks = append(s.acks, a)
}

// Pending returns the number of queued, unacknowledged inbound ingress.
func (s *Script) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inbound)
}

func (s *Script) isRevoked(c model.Capability) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revoked[c]
}

// head returns the next queued ingress, if any.
func (s *Script) head() (model.Ingress, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.inbound) == 0 {
		return model.Ingress{}, false
	}
	return cloneIngress(s.inbound[0]), true
}

// delivered records a delivery and, when acked, removes the queue head.
func (s *Script) delivered(d Delivery) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.Acked && len(s.inbound) > 0 {
		s.inbound = s.inbound[1:]
	}
	s.deliver = append(s.deliver, d)
}

// apply takes the next scripted result for call, records it and returns the
// outcome the adapter reports.
func (s *Script) apply(call Call, newRef bool, sent func(model.ExternalRef) []model.ExternalRef) (adapter.Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.fallback
	if len(s.results) > 0 {
		r = s.results[0]
		s.results = s.results[1:]
	}
	call.Result = r
	call.Applied = r == Accepted || r == OutcomeUnknown
	if call.Applied {
		ref := s.reference(call, newRef)
		call.References = []model.ExternalRef{ref}
		if call.Method == MethodSend {
			s.hold(ref.ID)
		}
		if newRef && sent != nil {
			call.References = sent(ref)
		}
	}
	s.calls = append(s.calls, cloneCall(call))
	return outcomeOf(r, call.References, s.after)
}

// reference is the provider reference of an applied call: a new message
// (model.ProviderMessageRef, as the fake's provider-assigned IDs declare) for
// a send, the target for an edit, deletion or reaction.
func (s *Script) reference(call Call, newRef bool) model.ExternalRef {
	if !newRef && call.Effect.Target != nil {
		return *call.Effect.Target
	}
	s.seq++
	return model.ProviderMessageRef(s.channel, fmt.Sprintf("m%d", s.seq))
}

// reconcile reports what the provider recorded for the effect's operation:
// its latest applied or rejected call.
func (s *Script) reconcile(e adapter.Effect) adapter.Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := adapter.Outcome{State: model.Failed, Failure: model.NewError(model.NotFound, "provider has no record of the effect")}
	for _, c := range slices.Backward(s.calls) {
		if c.Method == MethodReconcile || c.Effect.Operation != e.Operation {
			continue
		}
		if c.Applied {
			out = adapter.Outcome{State: model.ProviderAccepted, References: slices.Clone(c.References)}
			break
		}
		if c.Result == Rejected || c.Result == Unauthorized {
			out, _ = outcomeOf(c.Result, nil, 0)
			break
		}
	}
	s.calls = append(s.calls, Call{Method: MethodReconcile, Effect: cloneEffect(e), References: slices.Clone(out.References)})
	return out
}

func outcomeOf(r Result, refs []model.ExternalRef, after time.Duration) (adapter.Outcome, error) {
	switch r {
	case Accepted:
		return adapter.Outcome{State: model.ProviderAccepted, References: slices.Clone(refs)}, nil
	case Rejected:
		return adapter.Outcome{State: model.Rejected, Failure: rejected()}, nil
	case Unauthorized:
		return adapter.Outcome{State: model.Rejected, Failure: model.NewError(model.Unauthorized, "fake provider: the account may not change that message")}, nil
	case OutcomeUnknown:
		return adapter.Outcome{State: model.Unknown, Failure: model.NewError(model.OutcomeUnknown, "fake provider answer lost")}, nil
	case RequestLost:
		return adapter.Outcome{State: model.Unknown, Failure: model.NewError(model.OutcomeUnknown, "fake provider request lost")}, nil
	case Transient:
		return adapter.Outcome{}, model.NewError(model.Transient, "fake provider unreachable")
	case RateLimited:
		e := model.NewError(model.RateLimited, "fake provider rate limit")
		e.RetryAfter = after
		return adapter.Outcome{}, e
	}
	return adapter.Outcome{}, model.NewError(model.Permanent, "fake provider: unknown scripted result "+string(r))
}

func rejected() *model.Error {
	return model.NewError(model.Permanent, "fake provider rejected the effect")
}

func cloneIngress(in model.Ingress) model.Ingress {
	in.Envelope = in.Envelope.Clone()
	return in
}

func cloneEffect(e adapter.Effect) adapter.Effect { return e.Clone() }

func cloneCall(c Call) Call {
	c.Effect = cloneEffect(c.Effect)
	c.Content = c.Content.Clone()
	c.References = slices.Clone(c.References)
	c.Items = slices.Clone(c.Items)
	c.Add, c.Drop = slices.Clone(c.Add), slices.Clone(c.Drop)
	c.Changes = slices.Clone(c.Changes)
	if c.Action != nil {
		a := c.Action.Clone()
		c.Action = &a
	}
	return c
}
