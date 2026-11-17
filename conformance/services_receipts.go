package conformance

import (
	"context"
	"slices"
	"testing"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// receiptCases run Messages.Receipt through the operation ledger on the
// mutation env: the adapter of its main channel declares receipt, the
// send-only channel's does not.
var receiptCases = []struct {
	name string
	run  func(*testing.T, *mutationEnv)
}{
	{"QueuedSentOnceAndRecorded", receiptSentOnce},
	{"UndeclaredAdapterRefusedAtSubmit", receiptUndeclared},
	{"ReceivedMessagesOnly", receiptReceivedOnly},
	{"LostAnswerIsNotSentAgain", receiptLostAnswer},
}

// SendReceipt records the receipt in the provider and answers it applied,
// unless the case scripted another answer.
func (i *mutationInstance) SendReceipt(_ context.Context, r adapter.ReceiptRequest) (adapter.Outcome, error) {
	return i.p.mutate(model.CapabilityReceipt, r.Effect, r.Kind)
}

var _ adapter.ReceiptSender = (*mutationInstance)(nil)

// receipts returns the receipts that reached the provider: the effect and
// the kind of each.
func (p *mutationProvider) receipts() (effects []adapter.Effect, kinds []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, c := range p.kinds {
		if c == model.CapabilityReceipt {
			effects = append(effects, p.calls[i])
			kinds = append(kinds, p.details[i])
		}
	}
	return effects, kinds
}

func (env *mutationEnv) receipt(msg model.MessageRef, kind string) model.Receipt {
	return model.Receipt{Command: env.cmd(""), Message: msg, Kind: kind}
}

func (env *mutationEnv) receiptOps() []model.Operation {
	env.t.Helper()
	p, err := env.client.Operations().List(env.ctx, serviceList(model.Filter{Kinds: []string{model.ActionMessageReceipt}}, 100, ""))
	if err != nil || !p.Complete {
		env.t.Fatalf("List receipts = %+v, %v", p, err)
	}
	return p.Items
}

// receiptSentOnce: a receipt is queued with the message as its result,
// sent once by the runtime with its kind on the message's provider
// message, and recorded as its operation; a retry replays without a second
// send, before and after delivery, and the message is untouched.
func receiptSentOnce(t *testing.T, env *mutationEnv) {
	msgs := env.client.Messages()
	target := env.target
	req := env.receipt(target.Ref, model.ReceiptRead)
	a, err := msgs.Receipt(env.ctx, req)
	if err != nil {
		t.Fatalf("Receipt: %v", err)
	}
	op := a.Operation
	if op.State != model.Queued || a.Replayed || op.ResultMessage == nil || *op.ResultMessage != target.Ref ||
		op.Result == nil || *op.Result != target.Ref.Ref() || op.Pending != nil {
		t.Fatalf("Receipt acceptance = %+v", a)
	}
	if k := op.Key; k == nil || k.Kind != model.ActionMessageReceipt || k.Target != target.Ref.Ref() || k.ID != req.Operation {
		t.Fatalf("Receipt key = %+v, want message.receipt on the message", op.Key)
	}
	if again, err := msgs.Receipt(env.ctx, req); err != nil || !again.Replayed || again.Operation.Ref != op.Ref {
		t.Fatalf("retry before delivery = %+v, %v; want the stored operation", again, err)
	}
	other := req
	other.Kind = model.ReceiptDelivered
	_, err = msgs.Receipt(env.ctx, other)
	serviceCode(t, err, model.Conflict)
	if effects, _ := env.provider.receipts(); len(effects) != 0 {
		t.Fatalf("%d receipts sent at submission, want none before the runtime runs", len(effects))
	}

	done := env.deliver(op.Ref)
	if done.State != model.ProviderAccepted {
		t.Fatalf("delivered receipt = %+v, want provider_accepted", done)
	}
	effects, kinds := env.provider.receipts()
	if len(effects) != 1 || kinds[0] != model.ReceiptRead || effects[0].Operation != op.Ref ||
		effects[0].Target == nil || *effects[0].Target != env.provRef {
		t.Fatalf("receipts sent = %+v %v, want one read receipt on %v", effects, kinds, env.provRef)
	}
	again, err := msgs.Receipt(env.ctx, req)
	if err != nil || !again.Replayed || again.Operation.Ref != op.Ref || again.Operation.State != model.ProviderAccepted {
		t.Fatalf("retry after delivery = %+v, %v; want the settled operation", again, err)
	}
	env.drain()
	if effects, _ := env.provider.receipts(); len(effects) != 1 {
		t.Fatalf("%d receipts sent after the retries, want 1", len(effects))
	}
	var stored model.Message
	env.put(func(tx store.Tx) (err error) { stored, err = tx.GetMessage(env.ctx, target.Ref); return })
	if stored.Revision != target.Revision {
		t.Errorf("message revision %q after a receipt, want %q: a receipt changes nothing in uma", stored.Revision, target.Revision)
	}

	d, err := msgs.Receipt(env.ctx, env.receipt(target.Ref, model.ReceiptDelivered))
	if err != nil {
		t.Fatalf("delivered Receipt: %v", err)
	}
	env.deliver(d.Operation.Ref)
	if _, kinds := env.provider.receipts(); !slices.Equal(kinds, []string{model.ReceiptRead, model.ReceiptDelivered}) {
		t.Fatalf("receipts sent = %v, want read then delivered", kinds)
	}
	if got := opRefsOf(env.receiptOps()); !slices.Equal(got, []model.OperationRef{d.Operation.Ref, op.Ref}) {
		t.Errorf("message.receipt operations = %v, want both receipts, newest first", refIDs(got))
	}
}

// receiptUndeclared: a channel whose adapter does not declare receipt is
// refused at the call, before anything is stored.
func receiptUndeclared(t *testing.T, env *mutationEnv) {
	_, err := env.client.Messages().Receipt(env.ctx, env.receipt(env.plain.Ref, model.ReceiptRead))
	serviceCode(t, err, model.Unsupported)
	if ops := env.receiptOps(); len(ops) != 0 {
		t.Fatalf("an unsupported receipt stored %d operations", len(ops))
	}
}

// receiptReceivedOnly: a receipt answers a message received from the
// provider; a message sent through uma or the provider's echo of one is
// refused, as are a deleted message, a revision and an unknown kind.
func receiptReceivedOnly(t *testing.T, env *mutationEnv) {
	conv := env.target.Ref.Conversation
	stored := func(id string, edit func(*model.Message)) model.Message {
		m := env.target.Clone()
		m.Ref.ID = env.id()
		m.External = []model.ExternalRef{model.ProviderMessageRef(env.provRef.Channel, id)}
		edit(&m)
		env.put(func(tx store.Tx) error { return tx.PutMessage(env.ctx, m, "") })
		return m
	}
	sent := stored("pm-sent", func(m *model.Message) {
		m.Origin = &model.Origin{Actor: mutationActor, Operation: model.OperationRef{Tenant: serviceTenant, ID: env.id()}}
	})
	echo := stored("pm-echo", func(m *model.Message) { m.EchoOf = &sent.Ref })
	deleted := stored("pm-deleted", func(m *model.Message) { m.Deleted, m.Content = true, model.Content{} })
	msgs := env.client.Messages()
	for name, c := range map[string]struct {
		req  model.Receipt
		code model.ErrorCode
	}{
		"sent through uma":   {env.receipt(sent.Ref, model.ReceiptRead), model.Validation},
		"echo of a send":     {env.receipt(echo.Ref, model.ReceiptRead), model.Validation},
		"deleted":            {env.receipt(deleted.Ref, model.ReceiptRead), model.Conflict},
		"unknown message":    {env.receipt(model.MessageRef{Conversation: conv, ID: env.id()}, model.ReceiptRead), model.NotFound},
		"expected revision":  {model.Receipt{Command: env.cmd("m1"), Message: env.target.Ref, Kind: model.ReceiptRead}, model.Validation},
		"unknown kind":       {env.receipt(env.target.Ref, "seen"), model.Validation},
		"foreign tenant ref": {env.receipt(model.MessageRef{Conversation: model.ConversationRef{Inbox: model.InboxRef{Tenant: "conformance-other", ID: conv.Inbox.ID}, ID: conv.ID}, ID: env.target.Ref.ID}, model.ReceiptRead), model.NotFound},
	} {
		_, err := msgs.Receipt(env.ctx, c.req)
		if model.CodeOf(err) != c.code {
			t.Errorf("%s: Receipt error = %v, want %s", name, err, c.code)
		}
	}
	if ops := env.receiptOps(); len(ops) != 0 {
		t.Fatalf("refused receipts stored %d operations", len(ops))
	}
}

// receiptLostAnswer: an attempt whose answer was lost leaves the operation
// outcome_unknown; with no reconcile on the adapter nothing sends it again,
// neither the runtime nor a retry of the call.
func receiptLostAnswer(t *testing.T, env *mutationEnv) {
	env.provider.mu.Lock()
	env.provider.answer = func(c model.Capability) (adapter.Outcome, bool) {
		return adapter.Outcome{State: model.Unknown}, c == model.CapabilityReceipt
	}
	env.provider.mu.Unlock()
	req := env.receipt(env.target.Ref, model.ReceiptRead)
	a, err := env.client.Messages().Receipt(env.ctx, req)
	if err != nil {
		t.Fatalf("Receipt: %v", err)
	}
	if op := env.deliver(a.Operation.Ref); op.State != model.Unknown {
		t.Fatalf("receipt with a lost answer = %+v, want outcome_unknown", op)
	}
	env.drain()
	if again, err := env.client.Messages().Receipt(env.ctx, req); err != nil || !again.Replayed || again.Operation.State != model.Unknown {
		t.Fatalf("retry = %+v, %v; want the stored outcome_unknown operation", again, err)
	}
	env.drain()
	if effects, _ := env.provider.receipts(); len(effects) != 1 {
		t.Fatalf("%d receipts sent, want the one attempt: a lost answer is never sent again blindly", len(effects))
	}
}

// drain runs the runtime's due work once, as a worker would on its next
// poll.
func (env *mutationEnv) drain() {
	env.t.Helper()
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: serviceTenant, Actor: "worker"}, Call: model.Call{Correlation: "drain"}}
	if _, err := env.engine.Runtime().Step(env.ctx, call, uma.WorkerOptions{Owner: "worker", Limit: 4}); err != nil {
		env.t.Fatalf("Step: %v", err)
	}
}
