package fakeprovider

import (
	"context"
	"fmt"
	"slices"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// TypeInteractive is the type key of the interactive variant: it declares
// send, edit, stream, send_ephemeral, signal, respond and respond_follow_up
// and implements exactly the matching interfaces.
const TypeInteractive model.TypeKey = "org.example.fake.interactive"

// Methods of the interactive variant.
const (
	MethodStreamStart  Method = "stream_start"
	MethodStreamUpdate Method = "stream_update"
	MethodStreamFinish Method = "stream_finish"
	MethodStreamCancel Method = "stream_cancel"
	MethodEphemeral    Method = "send_ephemeral"
	MethodSignal       Method = "signal"
	// MethodRespond is an interaction response; Call.Mode is its response
	// mode and Call.FollowUp whether it is a follow-up.
	MethodRespond Method = "respond"
)

var interactive = variant{
	typ: TypeInteractive, exchange: providerIDs,
	caps: []model.Capability{
		model.CapabilitySend, model.CapabilityEdit, model.CapabilityStream,
		model.CapabilitySendEphemeral, model.CapabilitySignal, model.CapabilityRespond, model.CapabilityRespondFollowUp,
	},
	events: []string{model.EventDeliveryUpdated},
	open:   func(i *instance) adapter.Instance { return &Interactive{i: i} },
}

// NewInteractive returns the factory of the interactive variant.
func NewInteractive() *Factory { return newFactory(interactive) }

// Interactive is the interactive variant: send, edit, a native stream,
// ephemeral replies, signals and interaction responses.
//
// A native stream is one provider message: an applied start opens stream
// "s<n>" on a new message, and updates, finish and cancel address that
// stream and report its message. Starts, updates, finishes, cancels and
// ephemeral sends take scripted results like sends; signals take none and
// succeed unless signal is revoked.
type Interactive struct{ i *instance }

// Capabilities reports every declared capability.
func (x *Interactive) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	return x.i.capabilities(ctx)
}

// Close closes the instance; it is idempotent.
func (x *Interactive) Close(context.Context) error { return x.i.close() }

// Send sends a new message under the next scripted result.
func (x *Interactive) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	return x.i.send(ctx, r)
}

// Edit edits the target message under the next scripted result.
func (x *Interactive) Edit(ctx context.Context, r adapter.EditRequest) (adapter.Outcome, error) {
	return x.i.effect(ctx, model.CapabilityEdit, Call{Method: MethodEdit, Effect: r.Effect, Content: r.Content}, false)
}

// SendEphemeral shows content to the recipient only, under the next
// scripted result. The recipient must be on the channel.
func (x *Interactive) SendEphemeral(ctx context.Context, r adapter.EphemeralRequest) (adapter.Outcome, error) {
	if r.Recipient.Channel != x.i.channel.Ref {
		return adapter.Outcome{}, model.NewError(model.Validation, "recipient belongs to another channel")
	}
	return x.i.effect(ctx, model.CapabilitySendEphemeral, Call{
		Method: MethodEphemeral, Effect: r.Effect, Content: r.Content, Recipient: r.Recipient, ExpiresAt: r.ExpiresAt,
	}, true)
}

// Respond answers the interaction in r.Interaction under the next scripted
// result, in the response mode and audience asked for, and records the
// interaction context it received: its token stays in the recorded call,
// and formatting the call never prints it. A message response posts a new
// provider message, an update replaces the control's message, an ephemeral
// one answers the actor only. The interaction must be present, as the
// engine always hands it over, and so must the control's message for an
// update.
func (x *Interactive) Respond(ctx context.Context, r adapter.RespondRequest) (adapter.Outcome, error) {
	c := model.CapabilityRespond
	if r.FollowUp {
		c = model.CapabilityRespondFollowUp
	}
	if r.Interaction == nil {
		return adapter.Outcome{}, model.NewError(model.Validation, "interaction required")
	}
	if r.Mode == model.ResponseModeUpdate && r.Target == nil {
		return adapter.Outcome{}, model.NewError(model.Validation, "update needs the control's message")
	}
	call := Call{Method: MethodRespond, Effect: r.Effect, Content: r.Content, Mode: r.Mode, FollowUp: r.FollowUp}
	return x.i.effect(ctx, c, call, r.Mode == model.ResponseModeMessage || r.Mode == model.ResponseModeEphemeral)
}

// Signal records a signal on the channel's own route.
func (x *Interactive) Signal(ctx context.Context, r adapter.SignalRequest) error {
	if err := x.i.ready(ctx, model.CapabilitySignal); err != nil {
		return err
	}
	if r.Route.Channel != x.i.channel.Ref {
		return model.NewError(model.Validation, "route belongs to another channel")
	}
	x.i.script.signal(Call{
		Method: MethodSignal, Effect: adapter.Effect{Route: r.Route, Audience: r.Audience},
		Kind: r.Kind, ExpiresAt: r.ExpiresAt,
	})
	return nil
}

// Start opens a provider stream under the next scripted result.
func (x *Interactive) Start(ctx context.Context, r adapter.StreamRequest) (adapter.StreamOutcome, error) {
	return x.stream(ctx, MethodStreamStart, "", r)
}

// Update replaces the stream's content under the next scripted result.
func (x *Interactive) Update(ctx context.Context, ref string, r adapter.StreamRequest) (adapter.StreamOutcome, error) {
	return x.stream(ctx, MethodStreamUpdate, ref, r)
}

// Finish closes the stream under the next scripted result.
func (x *Interactive) Finish(ctx context.Context, ref string, r adapter.StreamRequest) (adapter.StreamOutcome, error) {
	return x.stream(ctx, MethodStreamFinish, ref, r)
}

// Cancel stops the stream under the next scripted result.
func (x *Interactive) Cancel(ctx context.Context, ref string, e adapter.Effect) (adapter.StreamOutcome, error) {
	return x.stream(ctx, MethodStreamCancel, ref, adapter.StreamRequest{Effect: e})
}

func (x *Interactive) stream(ctx context.Context, m Method, ref string, r adapter.StreamRequest) (adapter.StreamOutcome, error) {
	if err := x.i.ready(ctx, model.CapabilityStream); err != nil {
		return adapter.StreamOutcome{}, err
	}
	if err := x.i.authenticate(ctx); err != nil {
		return adapter.StreamOutcome{}, err
	}
	if r.Route.Channel != x.i.channel.Ref {
		return adapter.StreamOutcome{}, model.NewError(model.Validation, "route belongs to another channel")
	}
	return x.i.script.applyStream(Call{
		Method: m, Effect: cloneEffect(r.Effect), Content: r.Content.Clone(),
		Mode: r.Mode, Sequence: r.Sequence, Stream: ref,
	})
}

var (
	_ adapter.Sender          = (*Interactive)(nil)
	_ adapter.Editor          = (*Interactive)(nil)
	_ adapter.Streamer        = (*Interactive)(nil)
	_ adapter.EphemeralSender = (*Interactive)(nil)
	_ adapter.Signaler        = (*Interactive)(nil)

	_ adapter.InteractionResponder = (*Interactive)(nil)
)

// applyStream takes the next scripted result for a stream call. An
// applied start opens a stream on a new message; the other calls must
// address an open stream.
func (s *Script) applyStream(call Call) (adapter.StreamOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams == nil {
		s.streams = map[string]model.ExternalRef{}
	}
	msg, known := s.streams[call.Stream]
	if call.Method != MethodStreamStart && !known {
		return adapter.StreamOutcome{}, model.NewError(model.Validation, "unknown provider stream")
	}
	r := s.fallback
	if len(s.results) > 0 {
		r = s.results[0]
		s.results = s.results[1:]
	}
	call.Result = r
	call.Applied = r == Accepted || r == OutcomeUnknown
	if call.Applied && call.Method == MethodStreamStart {
		s.seq++
		msg = model.ExternalRef{Channel: s.channel, Namespace: Namespace, Kind: "message", ID: fmt.Sprintf("m%d", s.seq)}
		call.Stream = fmt.Sprintf("s%d", s.seq)
		s.streams[call.Stream] = msg
	}
	if call.Applied {
		call.References = []model.ExternalRef{msg}
	}
	s.calls = append(s.calls, cloneCall(call))
	out, err := outcomeOf(r, call.References, s.after)
	so := adapter.StreamOutcome{Outcome: out}
	if r == Accepted && call.Method == MethodStreamStart {
		so.ProviderStreamRef = call.Stream
	}
	return so, err
}

// signal records a signal call.
func (s *Script) signal(call Call) {
	s.mu.Lock()
	defer s.mu.Unlock()
	call.Applied = true
	s.calls = append(s.calls, cloneCall(call))
}

// Signals returns the recorded signal calls in order.
func (s *Script) Signals() []Call {
	return slices.DeleteFunc(s.Calls(), func(c Call) bool { return c.Method != MethodSignal })
}
