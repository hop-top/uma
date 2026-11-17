package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// Under the Socket Mode transport the fake workspace sends the events
// Deliver queued as envelopes over every connection the receiver opens, in
// order, each once: an events_api envelope carrying the event_callback the
// request URL would have been posted, an interactive one the interaction
// payload and a slash_commands one the command's fields. An event counts as
// acknowledged when its acknowledgement arrives, and as refused when the
// engine refused an Accept while it was being handled or when nothing
// arrives in socketVerdictWait. One whose connection ends first, unrefused,
// is queued again, as Slack delivers again what was not acknowledged: a
// receiver stopping between two cases may still take the next event.

const socketVerdictWait = 5 * time.Second

// socketInstance is the receiving adapter: it also runs the receiver, over
// a sink that tells the provider what the engine refused.
type socketInstance struct{ *recordingInstance }

func (i *socketInstance) Run(ctx context.Context, sink adapter.Sink) error {
	return i.Instance.Run(ctx, &refusalSink{Sink: sink, p: i.p, ch: i.ch})
}

// refusalSink counts the Accepts the engine refuses. A NotFound is the
// adapter's to fall back from or drop, so it is not counted.
type refusalSink struct {
	adapter.Sink
	p  *provider
	ch model.ChannelRef
}

func (s *refusalSink) Accept(ctx context.Context, in model.Ingress) (model.IngressReceipt, error) {
	rec, err := s.Sink.Accept(ctx, in)
	if err != nil && !model.IsCode(err, model.NotFound) {
		s.p.mu.Lock()
		if s.p.refusals == nil {
			s.p.refusals = map[model.ChannelRef]int{}
		}
		s.p.refusals[s.ch]++
		s.p.mu.Unlock()
	}
	return rec, err
}

// requestURLInstance is the adapter as a request URL alone: it implements
// exactly the interfaces of an adapter without a receiver, and reports no
// receive.
type requestURLInstance struct{ r *recordingInstance }

func (i *requestURLInstance) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	st, err := i.r.Capabilities(ctx)
	return slices.DeleteFunc(st, func(s model.CapabilityStatus) bool { return s.Name == model.CapabilityReceive }), err
}
func (i *requestURLInstance) Close(ctx context.Context) error     { return i.r.Close(ctx) }
func (i *requestURLInstance) Handler(s adapter.Sink) http.Handler { return i.r.Handler(s) }
func (i *requestURLInstance) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	return i.r.Send(ctx, r)
}

func (i *requestURLInstance) Edit(ctx context.Context, r adapter.EditRequest) (adapter.Outcome, error) {
	return i.r.Edit(ctx, r)
}

func (i *requestURLInstance) Archive(ctx context.Context, r adapter.ArchiveRequest) (adapter.Outcome, error) {
	return i.r.Archive(ctx, r)
}

func (i *requestURLInstance) Delete(ctx context.Context, r adapter.DeleteRequest) (adapter.Outcome, error) {
	return i.r.Delete(ctx, r)
}

func (i *requestURLInstance) React(ctx context.Context, r adapter.ReactRequest) (adapter.Outcome, error) {
	return i.r.React(ctx, r)
}

func (i *requestURLInstance) Reconcile(ctx context.Context, r adapter.ReconcileRequest) (adapter.Outcome, error) {
	return i.r.Reconcile(ctx, r)
}

func (i *requestURLInstance) Execute(ctx context.Context, r adapter.ExecuteRequest) (adapter.Outcome, error) {
	return i.r.Execute(ctx, r)
}

func (i *requestURLInstance) History(ctx context.Context, r adapter.HistoryRequest) (adapter.HistoryPage, error) {
	return i.r.History(ctx, r)
}

func (i *requestURLInstance) Respond(ctx context.Context, r adapter.RespondRequest) (adapter.Outcome, error) {
	return i.r.Respond(ctx, r)
}

// pushSocket sends ch's queued events over c until it closes.
func (p *provider) pushSocket(ch model.ChannelRef, c *fakeConn) {
	for {
		select {
		case <-c.done:
			return
		default:
		}
		p.mu.Lock()
		evs := p.hook[ch]
		var ev conformance.InboundEvent
		if len(evs) > 0 {
			ev, p.hook[ch] = evs[0], evs[1:]
		}
		p.mu.Unlock()
		if len(evs) == 0 {
			time.Sleep(time.Millisecond)
			continue
		}
		acked, lost := p.sendOver(ch, c, ev)
		p.mu.Lock()
		if lost {
			p.hook[ch] = append([]conformance.InboundEvent{ev}, p.hook[ch]...)
			p.mu.Unlock()
			return
		}
		if p.results == nil {
			p.results = map[model.ChannelRef][]conformance.InboundResult{}
		}
		p.results[ch] = append(p.results[ch], conformance.InboundResult{EventID: ev.EventID, Acked: acked})
		p.mu.Unlock()
	}
}

// sendOver sends ev as an envelope and reports whether it was
// acknowledged, or lost with its connection before any verdict.
func (p *provider) sendOver(ch model.ChannelRef, c *fakeConn, ev conformance.InboundEvent) (acked, lost bool) {
	p.mu.Lock()
	refused := p.refusals[ch]
	p.mu.Unlock()
	wasRefused := func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.refusals[ch] > refused
	}
	typ, payload := p.socketEnvelope(ev)
	c.mu.Lock()
	c.seq++
	id := "env-" + ev.EventID + "-" + time.Now().Format("150405.000000000")
	c.sent[id] = time.Now()
	c.mu.Unlock()
	if c.send(map[string]any{"envelope_id": id, "type": typ, "payload": payload, "accepts_response_payload": typ != "events_api"}) != nil {
		return false, true
	}
	deadline := time.After(socketVerdictWait)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case a := <-c.acks:
			if a.EnvelopeID == id {
				return true, false
			}
		case <-c.done:
			return false, !wasRefused()
		case <-deadline:
			return false, false
		case <-tick.C:
			if wasRefused() {
				return false, false
			}
		}
	}
}

// socketEnvelope is the type and payload of ev's envelope.
func (p *provider) socketEnvelope(ev conformance.InboundEvent) (string, any) {
	if ev.Interaction == nil {
		return "events_api", json.RawMessage(p.eventBody(ev))
	}
	payload, command := p.interactionParts(ev)
	if command == nil {
		return "interactive", payload
	}
	fields := map[string]string{}
	for k := range command {
		fields[k] = command.Get(k)
	}
	return "slash_commands", fields
}
