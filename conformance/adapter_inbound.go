package conformance

import (
	"context"
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
)

// inbound seeds channel A with an inbox bound to "support".
func (e *adapterEnv) inbound(t *testing.T) {
	t.Helper()
	e.seed(t, adapterChanA, adapterInboxA, e.selection(), "support")
}

func wantAcked(t *testing.T, got []InboundResult) {
	t.Helper()
	for _, r := range got {
		if !r.Acked {
			t.Fatalf("event %s not acknowledged after the sink accepted it", r.EventID)
		}
	}
}

var inboundCases = []adapterCase{
	{name: "NormalizesIntoBoundSink", inbound: true, run: func(t *testing.T, e *adapterEnv) {
		e.inbound(t)
		first, second := e.message(adapterChanA, "1", "peer-1", "support"), e.message(adapterChanA, "2", "peer-1", "support")
		wantAcked(t, e.receiveAll(t, adapterChanA, first))
		wantAcked(t, e.receiveAll(t, adapterChanA, second))

		msgs := e.messages(t, adapterInboxA)
		if len(msgs) != 2 {
			t.Fatalf("messages = %d, want 2", len(msgs))
		}
		var authors []model.IdentityRef
		for i, ev := range []InboundEvent{first, second} {
			m := findMessage(msgs, adapterChanA, ev.MessageID)
			if m == nil {
				t.Fatalf("no message references the provider message ID %q on the channel", ev.MessageID)
			}
			authors = append(authors, m.Author.Identity)
			a := m.Author.External
			if a.Channel != adapterChanA || a.ID != ev.Sender || !slices.Contains(e.desc.IdentityNamespaces, a.Namespace) {
				t.Errorf("message %d author %+v, want sender %q on the channel in a declared namespace", i, a, ev.Sender)
			}
			if len(m.Content.Parts) == 0 || m.Content.Parts[0].Text != ev.Text {
				t.Errorf("message %d content %+v, want text %q", i, m.Content, ev.Text)
			}
			if m.Audience.Visibility != model.Public || m.Source != e.desc.Type {
				t.Errorf("message %d audience %+v source %q, want public from %q", i, m.Audience, m.Source, e.desc.Type)
			}
		}
		if authors[0] != authors[1] {
			t.Errorf("one sender resolved to identities %v and %v; provider references must be stable", authors[0], authors[1])
		}
		convs := e.conversations(t, adapterInboxA)
		if len(convs) != 1 {
			t.Fatalf("conversations = %d, want 1", len(convs))
		}
		if e.selection().Mode == model.ModePerContact {
			if want := e.route(adapterChanA, "peer-1"); convs[0].ReplyRoute != want {
				t.Errorf("reply route = %+v, want the provider route %+v", convs[0].ReplyRoute, want)
			}
		}
	}},
	{name: "DuplicateIsIdempotent", inbound: true, run: func(t *testing.T, e *adapterEnv) {
		e.inbound(t)
		ev := e.message(adapterChanA, "1", "peer-1", "support")
		wantAcked(t, e.receiveAll(t, adapterChanA, ev))
		wantAcked(t, e.receiveAll(t, adapterChanA, ev))
		if n := len(e.messages(t, adapterInboxA)); n != 1 {
			t.Fatalf("messages = %d after a redelivered event, want 1", n)
		}
		if n := len(e.conversations(t, adapterInboxA)); n != 1 {
			t.Fatalf("conversations = %d, want 1", n)
		}
	}},
}

var unsupportedCases = []adapterCase{
	{name: "ReplyWithoutSend", absent: []model.Capability{model.CapabilitySend}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		_, err := e.reply(conv.Ref, "op-unsupported")
		if model.CodeOf(err) != model.Unsupported {
			t.Fatalf("Reply on a channel without send = %v, want unsupported", err)
		}
		if calls := e.provider.Calls(adapterChanA); len(calls) != 0 {
			t.Fatalf("provider calls = %+v, want none", calls)
		}
	}},
	{name: "ReceiveWithoutReceiver", absent: []model.Capability{model.CapabilityReceive, model.CapabilityReceiveHTTP}, run: func(t *testing.T, e *adapterEnv) {
		e.seed(t, adapterChanA, adapterInboxA, e.selection(), "support")
		ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
		defer cancel()
		if err := e.engine.Runtime().Receive(ctx, runtimeCall(), adapterChanA); model.CodeOf(err) != model.Unsupported {
			t.Fatalf("Runtime.Receive on a channel without receive = %v, want unsupported", err)
		}
	}},
}
