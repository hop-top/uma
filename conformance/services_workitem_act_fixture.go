package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

const trackerType model.TypeKey = "com.example.conformance.tracker"

const trackerSchema = "com.example.conformance.tracker-config"

// trackerAdapter is an issue tracker channel: its webhook instance hands
// the suite the engine-bound sink, and it moves issues open ⇄ closed
// (work_item_act) with GitHub's reasons. It answers an act with the
// issue's state after the change, under a fresh source revision, unless
// refusing is set.
type trackerAdapter struct {
	mu       sync.Mutex
	sink     adapter.Sink
	state    model.WorkItemState
	at       time.Time
	refusing bool
	acts     []adapter.WorkItemActRequest
}

func (*trackerAdapter) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: trackerType, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: trackerSchema, Version: 1},
		Capabilities:        []model.Capability{model.CapabilityReceiveHTTP, model.CapabilityWorkItemAct},
		Events:              []string{model.EventMessageReceived, model.EventWorkItemUpdated, model.EventDeliveryUpdated},
		IdentityNamespaces:  []string{deskNS},
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsProvider},
		WorkItemActs: []adapter.WorkItemTransition{
			{To: model.WorkItemClosed, From: []string{model.WorkItemOpen}, Reasons: []string{"completed", "not_planned"}},
			{To: model.WorkItemOpen, From: []string{model.WorkItemClosed}, Reasons: []string{"reopened"}},
		},
	}
}
func (*trackerAdapter) Validate(model.Extension) []model.ValidationIssue { return nil }
func (a *trackerAdapter) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	return trackerInstance{a}, nil
}

type trackerInstance struct{ a *trackerAdapter }

func (trackerInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return []model.CapabilityStatus{
		{Name: model.CapabilityReceiveHTTP, Available: true}, {Name: model.CapabilityWorkItemAct, Available: true},
	}, nil
}
func (trackerInstance) Close(context.Context) error { return nil }
func (i trackerInstance) Handler(s adapter.Sink) http.Handler {
	i.a.mu.Lock()
	i.a.sink = s
	i.a.mu.Unlock()
	return http.NotFoundHandler()
}

func (i trackerInstance) ActWorkItem(_ context.Context, r adapter.WorkItemActRequest) (adapter.WorkItemActOutcome, error) {
	a := i.a
	a.mu.Lock()
	defer a.mu.Unlock()
	a.acts = append(a.acts, r)
	if a.refusing {
		return adapter.WorkItemActOutcome{Outcome: adapter.Outcome{State: model.Rejected, Failure: model.NewError(model.Validation, "issue locked")}}, nil
	}
	reason := r.Reason
	if reason == "" && r.Status == model.WorkItemClosed {
		reason = "completed"
	}
	a.state.Status, a.state.RawStatus = r.Status, r.Status+":"+reason
	a.at = a.at.Add(time.Minute)
	s := a.state
	return adapter.WorkItemActOutcome{
		Outcome: adapter.Outcome{State: model.Delivered}, State: &s,
		SourceRevision: fmt.Sprintf("act-%d", len(a.acts)), OccurredAt: a.at,
	}, nil
}

func (a *trackerAdapter) requests() []adapter.WorkItemActRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.acts)
}

func (a *trackerAdapter) refuse(v bool) {
	a.mu.Lock()
	a.refusing = v
	a.mu.Unlock()
}

// tracker is one engine over a fresh store with a per-work-item inbox on a
// tracker channel, its sink bound, and one issue observed open.
type tracker struct {
	*desk
	engine uma.Engine
	a      *trackerAdapter
	item   model.WorkItem
	n      int
}

func newTracker(t *testing.T, factory StoreFactory) *tracker {
	t.Helper()
	fx := factory(t)
	a := &trackerAdapter{}
	clock := portstest.NewFixedClock(fx.Now())
	e, err := uma.New(uma.Config{
		Store: fx.Store, Clock: clock, IDs: portstest.NewSequenceIDs(0x7ac0),
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters: []adapter.Factory{a},
		Retry:    uma.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("uma.New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	client, err := e.For(model.Principal{Tenant: serviceTenant, Actor: "staff-1"})
	if err != nil {
		t.Fatal(err)
	}
	tr := &tracker{desk: &desk{t: t, ctx: t.Context(), st: fx.Store, clock: clock, client: client}, engine: e, a: a}
	ca, err := client.Configuration().ConfigureChannel(tr.ctx, model.ConfigureChannel{
		Command: tr.cmd(""),
		Channel: model.Channel{Ref: model.ChannelRef{Tenant: serviceTenant}, Type: trackerType, Configuration: model.Extension{Name: trackerSchema, Version: 1}},
	})
	if err != nil {
		t.Fatalf("ConfigureChannel: %v", err)
	}
	if tr.channel, err = ca.Operation.Result.AsChannel(); err != nil {
		t.Fatal(err)
	}
	ia, err := client.Configuration().ConfigureInbox(tr.ctx, model.ConfigureInbox{
		Command: tr.cmd(""),
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: serviceTenant}, Channel: tr.channel, Name: deskDest, Destinations: []string{deskDest},
			Selection: model.Selection{Mode: model.ModePerWorkItem},
		},
	})
	if err != nil {
		t.Fatalf("ConfigureInbox: %v", err)
	}
	if tr.inbox, err = ia.Operation.Result.AsInbox(); err != nil {
		t.Fatal(err)
	}
	service := uma.RuntimeCall{Principal: model.Principal{Tenant: serviceTenant, Actor: "tracker-service"}, Call: model.Call{Correlation: "corr"}}
	if _, err := e.Runtime().Webhook(tr.ctx, service, tr.channel); err != nil {
		t.Fatalf("Webhook: %v", err)
	}
	a.mu.Lock()
	tr.sink = a.sink
	a.mu.Unlock()
	if tr.sink == nil {
		t.Fatal("webhook binding did not hand the instance a sink")
	}
	at := time.Unix(1_700_000_000, 0).UTC()
	a.state = model.WorkItemState{
		Subject: "Build fails", URL: "https://tracker.example/repo-1/7", Status: model.WorkItemOpen, RawStatus: "open",
		Requester: tr.user("reporter"), LocalFields: []string{model.WorkItemFieldAssignees},
	}
	a.at = at
	opened := tr.message("e-issue", "description:7", "reporter", at, "7")
	opened.SourceRevision = "e-issue"
	s := a.state
	opened.WorkItem = &s
	tr.accept(opened)
	items := deskRead(tr.desk, store.Reader.WorkItems, model.Filter{Inboxes: []model.InboxRef{tr.inbox}})
	if len(items) != 1 || items[0].Status != model.WorkItemOpen {
		t.Fatalf("work items = %+v, want the open issue", items)
	}
	tr.item = items[0]
	return tr
}

func (tr *tracker) cmd(rev model.Revision) model.Command {
	tr.n++
	return model.Command{Call: model.Call{Correlation: "corr-act"}, Operation: model.OperationID(fmt.Sprintf("op-act-%d", tr.n)), ExpectedRevision: rev}
}

// act submits update u on the issue's work item at rev under operation op.
func (tr *tracker) act(op model.OperationID, rev model.Revision, u model.WorkItemUpdate) (model.Acceptance, error) {
	data, err := json.Marshal(u)
	if err != nil {
		tr.t.Fatal(err)
	}
	return tr.client.WorkItems().Act(tr.ctx, model.WorkItemAction{
		Command: model.Command{Call: model.Call{Correlation: "corr-act"}, Operation: op, ExpectedRevision: rev},
		Target:  tr.item.Ref, Action: model.Extension{Name: model.SchemaWorkItemUpdate, Version: 1, Data: data},
	})
}

// stored reads the issue's work item.
func (tr *tracker) stored() model.WorkItem {
	tr.t.Helper()
	w, err := tr.client.WorkItems().Get(tr.ctx, model.Lookup[model.WorkItemRef]{Call: model.Call{Correlation: "get"}, Ref: tr.item.Ref})
	if err != nil {
		tr.t.Fatalf("WorkItems.Get: %v", err)
	}
	return w
}

// conversationStatus reads the status of the issue's conversation.
func (tr *tracker) conversationStatus() string {
	tr.t.Helper()
	c, err := tr.client.Conversations().Get(tr.ctx, model.Lookup[model.ConversationRef]{Call: model.Call{Correlation: "get"}, Ref: tr.item.Ref.Conversation})
	if err != nil {
		tr.t.Fatalf("Conversations.Get: %v", err)
	}
	return c.Status
}

func (tr *tracker) op(ref model.OperationRef) model.Operation {
	tr.t.Helper()
	op, err := tr.client.Operations().Get(tr.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "get"}, Ref: ref})
	if err != nil {
		tr.t.Fatalf("Operations.Get: %v", err)
	}
	return op
}

// settle runs a Runtime.Run worker until ref's operation is in a final
// state, and returns it.
func (tr *tracker) settle(ref model.OperationRef) model.Operation {
	tr.t.Helper()
	ctx, cancel := context.WithCancel(tr.ctx)
	defer cancel()
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: serviceTenant, Actor: "worker"}, Call: model.Call{Correlation: "run"}}
	done := make(chan error, 1)
	go func() {
		done <- tr.engine.Runtime().Run(ctx, call, uma.WorkerOptions{Owner: "worker", Limit: 4, Poll: time.Millisecond})
	}()
	deadline := time.Now().Add(time.Minute)
	for {
		op := tr.op(ref)
		switch op.State {
		case model.ProviderAccepted, model.Delivered, model.Failed, model.Rejected, model.Committed:
			cancel()
			<-done
			return op
		}
		if time.Now().After(deadline) {
			tr.t.Fatalf("work item act stuck at %+v", op)
		}
		select {
		case err := <-done:
			tr.t.Fatalf("Run ended early: %v", err)
		case <-time.After(time.Millisecond):
		}
	}
}

// commandUpdates returns the command-triggered work_item.updated payloads.
func (tr *tracker) commandUpdates() []model.WorkItemUpdatedPayload {
	tr.t.Helper()
	var out []model.WorkItemUpdatedPayload
	err := tr.st.View(tr.ctx, serviceTenant, func(r store.Reader) error {
		p, err := r.Events(tr.ctx, model.Filter{Kinds: []string{model.EventWorkItemUpdated}}, "", 200)
		if err != nil {
			return err
		}
		for _, ev := range p.Items {
			var pl model.WorkItemUpdatedPayload
			if err := json.Unmarshal(ev.Payload.Data, &pl); err != nil {
				return err
			}
			if pl.Trigger == model.TriggerCommand {
				out = append(out, pl)
			}
		}
		return nil
	})
	if err != nil {
		tr.t.Fatalf("events: %v", err)
	}
	return out
}
