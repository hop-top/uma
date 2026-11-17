package widget

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// maxSignalKindBytes bounds a signal kind.
const maxSignalKindBytes = 64

// Instance is one opened channel: the receiver and every outbound
// capability. It is safe for concurrent use.
type Instance struct {
	f *Factory
	b *binding

	mu     sync.Mutex
	closed bool
}

// Capabilities reports every declared capability available.
func (x *Instance) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	if err := x.ready(ctx); err != nil {
		return nil, err
	}
	out := make([]model.CapabilityStatus, len(capabilities))
	for i, c := range capabilities {
		out[i] = model.CapabilityStatus{Name: c, Available: true}
	}
	return out, nil
}

// Close closes the instance; it is idempotent. A closed instance's handler
// answers 503 and its calls fail with model.Unavailable. Event streams
// belong to the Factory and stay open.
func (x *Instance) Close(context.Context) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.closed = true
	return nil
}

func (x *Instance) isClosed() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.closed
}

func (x *Instance) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if x.isClosed() {
		return model.NewError(model.Unavailable, "widget instance closed")
	}
	return nil
}

func (x *Instance) now() time.Time { return x.b.clock.Now() }

func (x *Instance) ref() model.ChannelRef { return x.b.channel.Ref }

// authorize checks a call before it reaches a visitor: the instance is
// open and the channel's session key resolves. A refused key means no
// visitor of the channel can be served, so nothing is delivered.
func (x *Instance) authorize(ctx context.Context) error {
	if err := x.ready(ctx); err != nil {
		return err
	}
	_, err := resolveKey(ctx, x.b.creds, x.b.channel)
	return err
}

// target maps a stored route to the visitor log it addresses and, for a
// session route, the session.
func (x *Instance) target(route model.ExternalRef) (LogKey, string, error) {
	bad := model.NewError(model.Validation, "route is not a widget visitor or session on this channel")
	if route.Channel != x.ref() || route.ID == "" || len(route.ID) > maxVisitorBytes {
		return LogKey{}, "", bad
	}
	var kind string
	switch route.Namespace {
	case NamespaceVisitor:
		kind = KindVisitor
	case NamespaceUser:
		kind = KindUser
	default:
		return LogKey{}, "", bad
	}
	switch {
	case route.Kind == kind && route.ParentID == "":
		return LogKey{Channel: route.Channel, Namespace: route.Namespace, Visitor: route.ID}, "", nil
	case route.Kind == model.RefKindRoom && route.ParentID != "" && len(route.ParentID) <= maxVisitorBytes:
		return LogKey{Channel: route.Channel, Namespace: route.Namespace, Visitor: route.ParentID}, route.ID, nil
	}
	return LogKey{}, "", bad
}

// public requires the public audience: visitors never see internal or
// restricted content.
func public(a model.Audience) error {
	if a.Visibility != model.Public {
		return model.NewError(model.Validation, "widget delivers public content only")
	}
	return nil
}

// deliver appends e to key and maps the log's answer to an outcome
// reporting refs once applied. A log refusing a mutation as unauthorized,
// not found or unsupported (the visitor's message cannot be changed, is
// gone, or the log cannot express it) settles it rejected with that code.
func (x *Instance) deliver(ctx context.Context, key LogKey, e Entry, refs ...model.ExternalRef) (adapter.Outcome, error) {
	_, err := x.f.log.Append(ctx, key, e)
	if err == nil {
		return adapter.Outcome{State: model.ProviderAccepted, References: refs}, nil
	}
	var me *model.Error
	switch {
	case ctx.Err() != nil, !errors.As(err, &me), !me.Code.Valid(), me.Code == model.OutcomeUnknown:
		return adapter.Outcome{State: model.Unknown, Failure: model.NewError(model.OutcomeUnknown, "visitor log answer lost")}, nil
	case me.Code.DefaultRetry() == model.RetrySameOperation:
		out := model.NewError(me.Code, "visitor log unavailable")
		out.RetryAfter = me.RetryAfter
		return adapter.Outcome{}, out
	}
	code := model.Permanent
	if mutation(e.Event.Type) && slices.Contains(mutationRefusals, me.Code) {
		code = me.Code
	}
	return adapter.Outcome{State: model.Rejected, Failure: &model.Error{
		Code: code, SafeMessage: "visitor log refused the event", Retry: model.NoRetry,
	}}, nil
}

// Send appends a message to the route's visitor.
func (x *Instance) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	if err := x.authorize(ctx); err != nil {
		return adapter.Outcome{}, err
	}
	key, session, err := x.target(r.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if err := public(r.Audience); err != nil {
		return adapter.Outcome{}, err
	}
	if err := checkContent(r.Content); err != nil {
		return adapter.Outcome{}, err
	}
	id, err := randomID("m")
	if err != nil {
		return adapter.Outcome{}, err
	}
	return x.deliver(ctx, key, Entry{
		Event:   Event{Type: EventMessage, Message: id, Parts: parts(r.Content)},
		Session: session, Operation: r.Operation,
	}, MessageRef(x.ref(), id))
}

// SendEphemeral appends a reply only the recipient sees, until ExpiresAt.
// The recipient must be the route's visitor.
func (x *Instance) SendEphemeral(ctx context.Context, r adapter.EphemeralRequest) (adapter.Outcome, error) {
	if err := x.authorize(ctx); err != nil {
		return adapter.Outcome{}, err
	}
	key, session, err := x.target(r.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if r.Recipient.Channel != x.ref() || r.Recipient.Namespace != key.Namespace || r.Recipient.ID != key.Visitor {
		return adapter.Outcome{}, model.NewError(model.Validation, "recipient is not the route's visitor")
	}
	if !x.now().Before(r.ExpiresAt) {
		return adapter.Outcome{}, &model.Error{Code: model.DeadlineExpired, SafeMessage: "ephemeral reply expired", Retry: model.NoRetry}
	}
	if err := checkContent(r.Content); err != nil {
		return adapter.Outcome{}, err
	}
	id, err := randomID("m")
	if err != nil {
		return adapter.Outcome{}, err
	}
	return x.deliver(ctx, key, Entry{
		Event:   Event{Type: EventEphemeral, Message: id, Parts: parts(r.Content)},
		Session: session, Operation: r.Operation, ExpiresAt: r.ExpiresAt,
	}, MessageRef(x.ref(), id))
}

// Signal shows a signal, such as typing, to the route's visitor until
// ExpiresAt. Nothing about it is kept past then.
func (x *Instance) Signal(ctx context.Context, r adapter.SignalRequest) error {
	if err := x.authorize(ctx); err != nil {
		return err
	}
	key, session, err := x.target(r.Route)
	if err != nil {
		return err
	}
	if err := public(r.Audience); err != nil {
		return err
	}
	if !validKind(r.Kind) {
		return model.NewError(model.Validation, "signal kind must be a short lowercase token")
	}
	if !x.now().Before(r.ExpiresAt) {
		return &model.Error{Code: model.DeadlineExpired, SafeMessage: "signal expired", Retry: model.NoRetry}
	}
	_, err = x.f.log.Append(ctx, key, Entry{Event: Event{Type: EventSignal, Kind: r.Kind}, Session: session, ExpiresAt: r.ExpiresAt})
	var me *model.Error
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.As(err, &me) && me.Code.Valid():
		return model.NewError(me.Code, "visitor log refused the signal")
	}
	return model.NewError(model.Unavailable, "visitor log unavailable")
}

func validKind(k string) bool {
	if k == "" || len(k) > maxSignalKindBytes {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' && c != '.' && c != '-' {
			return false
		}
	}
	return true
}

// Start opens a native stream: a new message whose ID is the provider
// stream reference.
func (x *Instance) Start(ctx context.Context, r adapter.StreamRequest) (adapter.StreamOutcome, error) {
	if err := checkContent(r.Content); err != nil {
		return adapter.StreamOutcome{}, err
	}
	id, err := randomID("m")
	if err != nil {
		return adapter.StreamOutcome{}, err
	}
	out, err := x.stream(ctx, Event{Type: EventStreamStart, Message: id, Parts: parts(r.Content)}, r.Effect)
	if err == nil && out.State == model.ProviderAccepted {
		out.ProviderStreamRef = id
	}
	return out, err
}

// Update replaces the stream's content with r.Content at r.Sequence.
func (x *Instance) Update(ctx context.Context, ref string, r adapter.StreamRequest) (adapter.StreamOutcome, error) {
	return x.streamStep(ctx, EventStreamUpdate, ref, r)
}

// Finish ends the stream, with its final content when r carries any.
func (x *Instance) Finish(ctx context.Context, ref string, r adapter.StreamRequest) (adapter.StreamOutcome, error) {
	return x.streamStep(ctx, EventStreamFinish, ref, r)
}

// Cancel stops the stream; content already shown stays.
func (x *Instance) Cancel(ctx context.Context, ref string, e adapter.Effect) (adapter.StreamOutcome, error) {
	return x.streamStep(ctx, EventStreamCancel, ref, adapter.StreamRequest{Effect: e})
}

func (x *Instance) streamStep(ctx context.Context, typ, ref string, r adapter.StreamRequest) (adapter.StreamOutcome, error) {
	if !isMessageID(ref) {
		return adapter.StreamOutcome{}, model.NewError(model.Validation, "unknown provider stream")
	}
	if err := checkContent(r.Content); err != nil {
		return adapter.StreamOutcome{}, err
	}
	return x.stream(ctx, Event{Type: typ, Message: ref, Sequence: r.Sequence, Parts: parts(r.Content)}, r.Effect)
}

func (x *Instance) stream(ctx context.Context, ev Event, eff adapter.Effect) (adapter.StreamOutcome, error) {
	if err := x.authorize(ctx); err != nil {
		return adapter.StreamOutcome{}, err
	}
	key, session, err := x.target(eff.Route)
	if err != nil {
		return adapter.StreamOutcome{}, err
	}
	if err := public(eff.Audience); err != nil {
		return adapter.StreamOutcome{}, err
	}
	out, err := x.deliver(ctx, key, Entry{Event: ev, Session: session, Operation: eff.Operation}, MessageRef(x.ref(), ev.Message))
	return adapter.StreamOutcome{Outcome: out}, err
}

// checkContent checks the parts of c the widget renders itself: a controls
// part of this version. Any other part passes.
func checkContent(c model.Content) error {
	for _, p := range c.Parts {
		if p.Structured != nil {
			if err := checkControls(*p.Structured); err != nil {
				return err
			}
		}
	}
	return nil
}

// parts converts content to visitor parts.
func parts(c model.Content) []Part {
	if len(c.Parts) == 0 {
		return nil
	}
	out := make([]Part, 0, len(c.Parts))
	for _, p := range c.Parts {
		q := Part{Kind: p.Kind, Text: p.Text}
		if p.Attachment != nil {
			q.Attachment = p.Attachment.ID
		}
		if p.Structured != nil {
			s := p.Structured.Clone()
			q.Structured = &s
		}
		out = append(out, q)
	}
	return out
}

var (
	_ adapter.HTTPReceiver    = (*Instance)(nil)
	_ adapter.Sender          = (*Instance)(nil)
	_ adapter.Streamer        = (*Instance)(nil)
	_ adapter.EphemeralSender = (*Instance)(nil)
	_ adapter.Signaler        = (*Instance)(nil)
	_ adapter.UploadReceiver  = (*Instance)(nil)
)
