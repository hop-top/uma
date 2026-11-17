package conformance

import (
	"context"
	"slices"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
)

// startRequest starts a conversation with to in inbox A under opID.
func (e *adapterEnv) startRequest(to model.ExternalRef, opID model.OperationID) model.StartConversation {
	return model.StartConversation{
		Command: model.Command{Call: model.Call{Correlation: model.CorrelationID("c-" + string(opID))}, Operation: opID},
		Inbox:   adapterInboxA, Recipient: to,
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "start " + string(opID)}}},
	}
}

// settleOperation runs a worker until operation ref settles, then cancels it. It
// never drains, so the runtime can still receive afterwards.
func (e *adapterEnv) settleOperation(t *testing.T, ref model.OperationRef) model.Operation {
	t.Helper()
	ctx, cancel := context.WithCancel(e.ctx)
	ended := make(chan error, 1)
	go func() {
		ended <- e.engine.Runtime().Run(ctx, runtimeCall(), uma.WorkerOptions{Owner: "conformance-start", Limit: 10, Poll: time.Millisecond})
	}()
	defer func() { cancel(); <-ended }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		op := e.op(t, ref)
		if op.State != model.Queued && op.State != model.RetryPending {
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation stuck at %+v", op)
		}
		select {
		case err := <-ended:
			t.Fatalf("Runtime.Run returned early: %v", err)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// receives reports whether the adapter and provider can run inbound cases.
func (e *adapterEnv) receives() bool {
	if slices.Contains(e.desc.Capabilities, model.CapabilityReceive) {
		return true
	}
	_, ok := e.provider.(WebhookProvider)
	return ok && slices.Contains(e.desc.Capabilities, model.CapabilityReceiveHTTP)
}

// adapterStartCases run Conversations.Start through the adapter: the first
// message to a peer no inbound message named, delivered on the peer's
// route, then, where the provider can express it, the peer's answer
// joining the started conversation.
var adapterStartCases = []adapterCase{
	{name: "StartsConversation", needs: []model.Capability{model.CapabilityStartConversation}, run: func(t *testing.T, e *adapterEnv) {
		if e.selection().Mode != model.ModePerContact {
			t.Skip("the start case covers per-contact inboxes")
		}
		e.inbound(t)
		discovered, err := e.client.Configuration().Capabilities(e.ctx, model.Lookup[model.ChannelRef]{
			Call: model.Call{Correlation: "c-discovery"}, Ref: adapterChanA,
		})
		if err != nil {
			t.Fatalf("Capabilities: %v", err)
		}
		if i := slices.IndexFunc(discovered, func(s model.CapabilityStatus) bool {
			return s.Name == model.CapabilityStartConversation
		}); i < 0 || !discovered[i].Available {
			t.Fatalf("discovery %+v, want start_conversation available: the engine executes it", discovered)
		}
		to := e.route(adapterChanA, "peer-new")
		e.respond(t, adapterChanA, Response{Outcome: OutcomeAccepted})
		acc, err := e.client.Conversations().Start(e.ctx, e.startRequest(to, "op-start"))
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		wantOp(t, e.settleOperation(t, acc.Operation.Ref), model.ProviderAccepted, "")
		sends := e.calls(adapterChanA, model.CapabilitySend)
		if len(sends) != 1 || sends[0].Operation != acc.Operation.Ref || !sends[0].Applied || sends[0].Route != to {
			t.Fatalf("provider sends = %+v, want one applied send of %v to %+v", sends, acc.Operation.Ref, to)
		}
		convs := e.conversations(t, adapterInboxA)
		if len(convs) != 1 || convs[0].ReplyRoute != to || convs[0].Ref != acc.Operation.ResultMessage.Conversation {
			t.Fatalf("conversations %+v, want the started one routed to %+v", convs, to)
		}
		if !e.receives() {
			return
		}
		wantAcked(t, e.receiveAll(t, adapterChanA, e.message(adapterChanA, "answer", "peer-new", "support")))
		if n, m := len(e.conversations(t, adapterInboxA)), len(e.messages(t, adapterInboxA)); n != 1 || m != 2 {
			t.Fatalf("conversations %d messages %d after the answer, want the answer in the started conversation", n, m)
		}
		if n := len(e.identities(t, adapterInboxA)); n != 1 {
			t.Fatalf("identities = %d, want the peer's one", n)
		}
	}},
}

// adapterStartUnsupportedCases: without start_conversation a start is
// refused before any provider call.
var adapterStartUnsupportedCases = []adapterCase{
	{name: "StartWithoutCapability", absent: []model.Capability{model.CapabilityStartConversation}, run: func(t *testing.T, e *adapterEnv) {
		e.inbound(t)
		_, err := e.client.Conversations().Start(e.ctx, e.startRequest(e.route(adapterChanA, "peer-new"), "op-start"))
		if model.CodeOf(err) != model.Unsupported {
			t.Fatalf("Start on a channel without start_conversation = %v, want unsupported", err)
		}
		if calls := e.provider.Calls(adapterChanA); len(calls) != 0 {
			t.Fatalf("provider calls = %+v, want none", calls)
		}
		if n := len(e.conversations(t, adapterInboxA)); n != 0 {
			t.Fatalf("conversations = %d, want none", n)
		}
	}},
}
