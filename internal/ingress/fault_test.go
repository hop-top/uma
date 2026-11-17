package ingress_test

import (
	"context"
	"sync"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
)

// inFlight scripts the desk's next send to block once it reached the
// provider, until the test answers it. The provider applies it under
// provider message ID id whatever the answer.
type inFlight struct {
	entered chan adapter.SendRequest
	answer  chan enginetest.SendFunc
}

func (d *desk) sendInFlight(id string) *inFlight {
	f := &inFlight{entered: make(chan adapter.SendRequest, 1), answer: make(chan enginetest.SendFunc)}
	d.f.Respond(func(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error, bool) {
		f.entered <- r
		fn := <-f.answer
		out, err, _ := fn(ctx, r)
		if err == nil && out.State == model.ProviderAccepted {
			out.References = []model.ExternalRef{model.ProviderMessageRef(r.Route.Channel, id)}
		}
		return out, err, true
	})
	return f
}

// deliverAsync runs the outbound worker in the background.
func (d *desk) deliverAsync() (wait func()) {
	var wg sync.WaitGroup
	wg.Go(d.deliver)
	return wg.Wait
}

// The provider's marked echo of a reply arrives before the send returns.
// It is the same message, and its evidence settles the reply even when the
// send's own answer is then lost: no second send, no outcome_unknown.
func TestMarkedEchoBeforeSendAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer enginetest.SendFunc
	}{
		{"answer arrives", enginetest.Accepted},
		{"answer lost", enginetest.LostAnswer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDesk(t)
			d.MustAccept(d.comment("e-1", "c-1", "customer", "7"))
			conv := d.onlyConversation()
			send := d.sendInFlight("c-2")
			a := d.reply(conv.Ref, "op-1", "on it")
			wait := d.deliverAsync()
			req := <-send.entered

			echo := d.comment("e-2", "c-2", "uma-bot", "7")
			echo.References = append(echo.References, model.EchoRef(d.Channel.Ref, a.Operation.Ref, req.Attempt))
			d.MustAccept(echo)
			send.answer <- tc.answer
			wait()
			d.deliver()

			if n := len(d.messages()); n != 2 || received(d.Events()) != 1 {
				t.Fatalf("messages = %d, message.received = %d; want the echo recognized", n, received(d.Events()))
			}
			op := d.op(a.Operation.Ref)
			if op.State != model.ProviderAccepted || op.Failure != nil {
				t.Fatalf("reply = %s (%v), want provider_accepted from the echo", op.State, op.Failure)
			}
			if n := len(d.f.ProviderSends()); n != 1 {
				t.Fatalf("sends = %d, want the reply sent once", n)
			}
			if m := d.MessageByProviderID(d.inbox.Ref, "c-2"); m.Ref != *a.Operation.ResultMessage {
				t.Fatalf("provider ID c-2 names %v, want the outbound reply", m.Ref)
			}
		})
	}
}
