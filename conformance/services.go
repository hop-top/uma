package conformance

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// RunServiceSuite runs the engine's application services over factory's
// store: every case group registered below exercises the public API end to
// end (authorization, the operation ledger, events and paging) on that
// store. A store passing RunStoreSuite should pass it unchanged; it checks
// that the store's list order, cursors and lookups compose with the
// engine's authorized queries. The AgentTasks group runs an agent protocol
// on it: a fake agent adapter built from public packages only sends into
// per-agent-context inboxes, and the host executes through the services.
func RunServiceSuite(t *testing.T, factory StoreFactory) {
	t.Helper()
	for _, c := range []struct {
		name string
		run  func(*testing.T, *serviceEnv)
	}{
		{"ConfigurationThroughLedger", serviceConfiguration},
		{"NotesHiddenFromMembers", serviceNoteVisibility},
		{"PagingUnderInserts", servicePagingUnderInserts},
		{"CursorBinding", serviceCursorBinding},
		{"ReferenceScope", serviceReferenceScope},
		{"LocalCommands", serviceLocalCommands},
		{"RetentionPurge", serviceRetentionPurge},
		{"OrphanContactsPurgeAfterHorizon", serviceOrphanContactsPurge},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, newServiceEnv(t, factory)) })
	}
	t.Run("WorkItemArrivalOrder", func(t *testing.T) { serviceWorkItemArrivalOrder(t, factory) })
	t.Run("CascadeLargeContact", func(t *testing.T) { serviceCascadeLargeContact(t, factory) })
	t.Run("SelectionAfterClosedHistory", func(t *testing.T) { serviceSelectionAfterClosedHistory(t, factory) })
	t.Run("PrivacyAcrossSurfaces", func(t *testing.T) { servicePrivacyAcrossSurfaces(t, factory) })
	t.Run("ExportSupersededByRedaction", func(t *testing.T) { serviceExportSupersededByRedaction(t, factory) })
	t.Run("ProviderMessageOnce", func(t *testing.T) { serviceProviderMessageOnce(t, factory) })
	t.Run("MutationsThroughLedger", func(t *testing.T) { serviceMutations(t, factory) })
	t.Run("ExtensionEvents", func(t *testing.T) { serviceExtensionEvents(t, factory) })
	t.Run("ExtensionEventsInboxFilter", func(t *testing.T) { serviceExtensionEventsInboxFilter(t, factory) })
	t.Run("Held", func(t *testing.T) {
		for _, c := range heldServiceCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newMutationEnv(t, factory)) })
		}
		t.Run("StartScopePerRecipient", func(t *testing.T) { heldSvcStartScope(t, factory) })
		t.Run("LabelCreateScopePerName", func(t *testing.T) { heldSvcLabelScope(t, factory) })
	})
	t.Run("MutationsExecuted", func(t *testing.T) { serviceMutationsExecuted(t, factory) })
	t.Run("Receipts", func(t *testing.T) {
		for _, c := range receiptCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newMutationEnv(t, factory)) })
		}
	})
	t.Run("Membership", func(t *testing.T) {
		for _, c := range membershipCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newMutationEnv(t, factory)) })
		}
	})
	t.Run("HistoryImport", func(t *testing.T) {
		for _, c := range historyCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newHistoryEnv(t, factory)) })
		}
		for _, c := range historyWindowCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newHistoryEnv(t, factory)) })
		}
		t.Run("SenderChosenIDsAreNotDeduped", func(t *testing.T) { historySenderChosen(t, factory) })
		t.Run("SenderChosenNamelessSkipped", func(t *testing.T) { historySenderChosenNoIdentity(t, factory) })
		t.Run("UndeclaredAdapterUnsupported", func(t *testing.T) { historyUndeclared(t, factory) })
	})
	t.Run("Contacts", func(t *testing.T) {
		for _, c := range contactCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newContactsEnv(t, factory)) })
		}
		for _, c := range contactWriteCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newContactsEnv(t, factory)) })
		}
	})
	t.Run("WorkItemActs", func(t *testing.T) {
		for _, c := range workItemActCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newTracker(t, factory)) })
		}
	})
	t.Run("Start", func(t *testing.T) {
		for _, c := range startCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newStartEnv(t, factory)) })
		}
	})
	t.Run("MessageSearch", func(t *testing.T) {
		for _, c := range serviceSearchCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newServiceEnv(t, factory)) })
		}
	})
	t.Run("OperationLookup", func(t *testing.T) {
		for _, c := range lookupCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newServiceEnv(t, factory)) })
		}
	})
	t.Run("OperationList", func(t *testing.T) {
		for _, c := range operationListServiceCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newOperationWorld(t, factory)) })
		}
		t.Run("PendingChanges", func(t *testing.T) { operationListPending(t, factory) })
		t.Run("ContactKinds", func(t *testing.T) { operationListContactKinds(t, factory) })
	})
	t.Run("EventList", func(t *testing.T) {
		for _, c := range eventListCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newEventListEnv(t, factory)) })
		}
		t.Run("EmptyLog", func(t *testing.T) { eventListEmptyLog(t, newServiceEnv(t, factory)) })
		t.Run("HidesDeniedExtension", func(t *testing.T) { eventListHidesDeniedExtension(t, factory) })
	})
	t.Run("EventGet", func(t *testing.T) {
		for _, c := range eventGetCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newEventListEnv(t, factory)) })
		}
	})
	t.Run("Labels", func(t *testing.T) {
		for _, c := range labelServiceCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newLabelsEnv(t, factory)) })
		}
		for _, c := range labelMoreCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newLabelsEnv(t, factory)) })
		}
	})
	t.Run("Tasks", func(t *testing.T) {
		for _, c := range serviceTaskCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newTaskEnv(t, factory)) })
		}
	})
	t.Run("Interactions", func(t *testing.T) {
		for _, c := range interactionServiceCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newInteractionEnv(t, factory)) })
		}
	})
	t.Run("AgentTasks", func(t *testing.T) {
		for _, c := range agentTaskCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newAgentEnv(t, factory)) })
		}
	})
	t.Run("ConversationCapabilities", func(t *testing.T) { serviceConversationCapabilities(t, factory) })
	t.Run("ConversationWorkflow", func(t *testing.T) {
		for _, c := range workflowServiceCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newServiceEnv(t, factory)) })
		}
		for _, c := range linkServiceCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newServiceEnv(t, factory)) })
		}
	})
	t.Run("WorkflowAndProviderVerbs", func(t *testing.T) {
		for _, c := range workflowVerbCases {
			t.Run(c.name, func(t *testing.T) { c.run(t, newMutationEnv(t, factory)) })
		}
	})
}

const (
	serviceTenant model.TenantID = "conformance-services"
	serviceType   model.TypeKey  = "com.example.conformance.chat"
	serviceSchema                = "com.example.conformance.chat-config"
)

// serviceAdapter is an adapter descriptor declaring receive plus caps; the
// suite never opens an instance.
type serviceAdapter struct{ caps []model.Capability }

func (a serviceAdapter) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: serviceType, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: serviceSchema, Version: 1},
		Capabilities:        append([]model.Capability{model.CapabilityReceive}, a.caps...),
		Events:              []string{model.EventMessageReceived},
		IdentityNamespaces:  []string{"com.example.conformance.user"},
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsProvider},
	}
}
func (serviceAdapter) Validate(model.Extension) []model.ValidationIssue { return nil }
func (serviceAdapter) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	return nil, model.NewError(model.Unsupported, "not opened by the suite")
}

// memberAuthorizer allows everything except reading non-public messages,
// unless staff, and the actions it was told to deny.
type memberAuthorizer struct {
	mu     sync.Mutex
	staff  bool
	denied []string
	// deniedOn holds actions denied on single resources.
	deniedOn []policy.Authorization
}

func (a *memberAuthorizer) Authorize(_ context.Context, r policy.Authorization) (policy.Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.Action == model.ActionMessageRead && r.Audience.Visibility != model.Public && !a.staff {
		return policy.Decision{}, nil
	}
	if slices.Contains(a.denied, r.Action) || slices.ContainsFunc(a.deniedOn, func(d policy.Authorization) bool {
		return d.Action == r.Action && d.Resource == r.Resource
	}) {
		return policy.Decision{}, nil
	}
	return policy.Decision{Allow: true}, nil
}

func (a *memberAuthorizer) deny(action string) {
	a.mu.Lock()
	a.denied = append(a.denied, action)
	a.mu.Unlock()
}

// denyOn denies action on resource only.
func (a *memberAuthorizer) denyOn(action string, resource model.Ref) {
	a.mu.Lock()
	a.deniedOn = append(a.deniedOn, policy.Authorization{Action: action, Resource: resource})
	a.mu.Unlock()
}

func (a *memberAuthorizer) setStaff(v bool) {
	a.mu.Lock()
	a.staff = v
	a.mu.Unlock()
}

type serviceEnv struct {
	t      *testing.T
	ctx    context.Context
	fx     StoreFixture
	clock  *portstest.FixedClock
	engine uma.Engine
	st     store.Store
	policy *memberAuthorizer
	ids    *portstest.SequenceIDs
	client uma.Client
	ch     model.ChannelRef
	inbox  model.InboxRef
	inbox2 model.InboxRef
	conv   model.Conversation
	n      int
}

func newServiceEnv(t *testing.T, factory StoreFactory) *serviceEnv {
	t.Helper()
	return newServiceEnvWith(t, factory, serviceAdapter{})
}

// newServiceEnvWith is newServiceEnv with adapter a registered for the
// service channel type.
func newServiceEnvWith(t *testing.T, factory StoreFactory, a serviceAdapter) *serviceEnv {
	t.Helper()
	fx := factory(t)
	pol := &memberAuthorizer{}
	clock := portstest.NewFixedClock(fx.Now())
	e, err := uma.New(uma.Config{
		Store: fx.Store, Authorizer: pol, Clock: clock, IDs: portstest.NewSequenceIDs(0xc0f),
		Adapters: []adapter.Factory{a},
		Retention: uma.RetentionPolicy{
			Events: serviceHorizon, Operations: serviceHorizon, Ingress: serviceHorizon,
			Attempts: serviceHorizon, Staging: serviceHorizon, Exports: serviceHorizon,
			Contacts: serviceHorizon,
		},
	})
	if err != nil {
		t.Fatalf("uma.New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	client, err := e.For(model.Principal{Tenant: serviceTenant, Actor: "staff-1"})
	if err != nil {
		t.Fatal(err)
	}
	env := &serviceEnv{
		t: t, ctx: t.Context(), fx: fx, clock: clock, engine: e, st: fx.Store, policy: pol,
		ids: portstest.NewSequenceIDs(0xc0e), client: client,
	}
	env.ch = env.configureChannel()
	env.inbox = env.configureInbox("support")
	env.inbox2 = env.configureInbox("sales")
	env.conv = env.conversation(env.inbox, model.ConversationOpen)
	return env
}

func (env *serviceEnv) cmd(rev model.Revision) model.Command {
	env.n++
	return model.Command{Call: model.Call{Correlation: "corr"}, Operation: model.OperationID(fmt.Sprintf("op-%d", env.n)), ExpectedRevision: rev}
}

func (env *serviceEnv) id() model.ID {
	id, err := env.ids.New(env.ctx)
	if err != nil {
		env.t.Fatal(err)
	}
	return id
}

func (env *serviceEnv) configureChannel() model.ChannelRef {
	env.t.Helper()
	a, err := env.client.Configuration().ConfigureChannel(env.ctx, model.ConfigureChannel{
		Command: env.cmd(""),
		Channel: model.Channel{Ref: model.ChannelRef{Tenant: serviceTenant}, Type: serviceType, Configuration: model.Extension{Name: serviceSchema, Version: 1}},
	})
	if err != nil {
		env.t.Fatalf("ConfigureChannel: %v", err)
	}
	ref, err := a.Operation.Result.AsChannel()
	if err != nil {
		env.t.Fatal(err)
	}
	return ref
}

func (env *serviceEnv) configureInbox(dest string) model.InboxRef {
	env.t.Helper()
	a, err := env.client.Configuration().ConfigureInbox(env.ctx, model.ConfigureInbox{
		Command: env.cmd(""),
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: serviceTenant}, Channel: env.ch, Name: dest, Destinations: []string{dest},
			Selection: model.Selection{Mode: model.ModePerRoom},
		},
	})
	if err != nil {
		env.t.Fatalf("ConfigureInbox: %v", err)
	}
	ref, err := a.Operation.Result.AsInbox()
	if err != nil {
		env.t.Fatal(err)
	}
	return ref
}

func (env *serviceEnv) put(fn func(store.Tx) error) {
	env.t.Helper()
	if err := env.st.Transact(env.ctx, serviceTenant, fn); err != nil {
		env.t.Fatalf("seed: %v", err)
	}
}

func (env *serviceEnv) conversation(in model.InboxRef, status string) model.Conversation {
	env.t.Helper()
	c := model.Conversation{Ref: model.ConversationRef{Inbox: in, ID: env.id()}, Mode: model.ModePerRoom, Status: status, Revision: "cv1"}
	env.put(func(tx store.Tx) error { return tx.PutConversation(env.ctx, c, "") })
	return c
}

func (env *serviceEnv) message(conv model.ConversationRef, vis model.Visibility) model.Message {
	env.t.Helper()
	m := model.Message{
		Ref: model.MessageRef{Conversation: conv, ID: env.id()}, Audience: model.Audience{Visibility: vis},
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "body"}}}, Revision: "m1",
	}
	env.put(func(tx store.Tx) error { return tx.PutMessage(env.ctx, m, "") })
	return m
}

func serviceList(f model.Filter, limit int, after model.Cursor) model.ListRequest {
	return model.ListRequest{Call: model.Call{Correlation: "corr"}, Filter: f, Page: model.PageRequest{Limit: limit, After: after}}
}

func (env *serviceEnv) messages(f model.Filter, limit int) []model.ID {
	env.t.Helper()
	var out []model.ID
	var after model.Cursor
	for range 1000 {
		p, err := env.client.Messages().List(env.ctx, serviceList(f, limit, after))
		if err != nil {
			env.t.Fatalf("Messages.List: %v", err)
		}
		for _, m := range p.Items {
			out = append(out, m.Ref.ID)
		}
		if p.Complete {
			return out
		}
		after = p.Next
	}
	env.t.Fatal("Messages.List never completed")
	return nil
}

func serviceCode(t *testing.T, err error, want model.ErrorCode) {
	t.Helper()
	if model.CodeOf(err) != want {
		t.Fatalf("error = %v, want %s", err, want)
	}
}

func serviceConfiguration(t *testing.T, env *serviceEnv) {
	chs, err := env.client.Configuration().Channels(env.ctx, serviceList(model.Filter{}, 10, ""))
	if err != nil || len(chs.Items) != 1 || chs.Items[0].Ref != env.ch || !chs.Complete {
		t.Fatalf("Channels = %+v, %v", chs, err)
	}
	ins, err := env.client.Configuration().Inboxes(env.ctx, serviceList(model.Filter{Inboxes: []model.InboxRef{env.inbox2}}, 10, ""))
	if err != nil || len(ins.Items) != 1 || ins.Items[0].Ref != env.inbox2 {
		t.Fatalf("Inboxes = %+v, %v", ins, err)
	}
	// A repeated create replays instead of creating again.
	req := model.ConfigureInbox{
		Command: env.cmd(""),
		Inbox:   model.Inbox{Ref: model.InboxRef{Tenant: serviceTenant}, Channel: env.ch, Name: "x", Destinations: []string{"x"}, Selection: model.Selection{Mode: model.ModePerRoom}},
	}
	first, err := env.client.Configuration().ConfigureInbox(env.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := env.client.Configuration().ConfigureInbox(env.ctx, req)
	if err != nil || !again.Replayed || *again.Operation.Result != *first.Operation.Result {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	all, err := env.client.Configuration().Inboxes(env.ctx, serviceList(model.Filter{}, 10, ""))
	if err != nil || len(all.Items) != 3 {
		t.Fatalf("inboxes = %d, %v; want 3", len(all.Items), err)
	}
	dup := req
	dup.Operation = "op-dup"
	_, err = env.client.Configuration().ConfigureInbox(env.ctx, dup)
	serviceCode(t, err, model.Conflict)
}

func serviceNoteVisibility(t *testing.T, env *serviceEnv) {
	var public []model.ID
	var note model.Message
	for i := range 9 {
		if i%3 == 1 {
			note = env.message(env.conv.Ref, model.Internal)
			continue
		}
		public = append(public, env.message(env.conv.Ref, model.Public).Ref.ID)
	}
	conv := env.conv.Ref
	if got := env.messages(model.Filter{Conversation: &conv}, 2); !slices.Equal(got, public) {
		t.Fatalf("member sees %v, want public messages %v in recording order", got, public)
	}
	_, err := env.client.Messages().Get(env.ctx, model.Lookup[model.MessageRef]{Call: model.Call{Correlation: "c"}, Ref: note.Ref})
	serviceCode(t, err, model.Unauthorized)
	env.policy.setStaff(true)
	if got := env.messages(model.Filter{Conversation: &conv}, 4); len(got) != 9 {
		t.Fatalf("staff see %d messages, want 9", len(got))
	}
}

func servicePagingUnderInserts(t *testing.T, env *serviceEnv) {
	var before []model.ID
	for range 12 {
		before = append(before, env.message(env.conv.Ref, model.Public).Ref.ID)
	}
	var got []model.ID
	var after model.Cursor
	for {
		p, err := env.client.Messages().List(env.ctx, serviceList(model.Filter{}, 5, after))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range p.Items {
			got = append(got, m.Ref.ID)
		}
		if p.Complete {
			break
		}
		after = p.Next
		if len(got) < 20 {
			env.message(env.conv.Ref, model.Public) // recorded after the page
		}
	}
	seen := map[model.ID]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("message %s paged twice", id)
		}
		seen[id] = true
	}
	if len(got) < len(before) || !slices.Equal(got[:len(before)], before) {
		t.Fatalf("paged %v; want every earlier message first, once, in order", got)
	}
	if want := env.messages(model.Filter{}, 100); !slices.Equal(got, want) {
		t.Fatalf("paged order %v differs from recording order %v", got, want)
	}
}

func serviceCursorBinding(t *testing.T, env *serviceEnv) {
	for range 3 {
		env.message(env.conv.Ref, model.Public)
	}
	p, err := env.client.Messages().List(env.ctx, serviceList(model.Filter{}, 1, ""))
	if err != nil || p.Next == "" {
		t.Fatalf("first page %+v, %v", p, err)
	}
	b := []byte(p.Next)
	b[len(b)-1] ^= 1
	_, err = env.client.Messages().List(env.ctx, serviceList(model.Filter{}, 1, model.Cursor(b)))
	serviceCode(t, err, model.Validation)
	_, err = env.client.Conversations().List(env.ctx, serviceList(model.Filter{}, 1, p.Next))
	serviceCode(t, err, model.Validation)
	if _, err := env.client.Messages().List(env.ctx, serviceList(model.Filter{}, 1, p.Next)); err != nil {
		t.Fatalf("valid cursor: %v", err)
	}
}

func serviceReferenceScope(t *testing.T, env *serviceEnv) {
	m := env.message(env.conv.Ref, model.Public)
	other := env.conversation(env.inbox2, model.ConversationOpen)
	look := func(ref model.MessageRef) error {
		_, err := env.client.Messages().Get(env.ctx, model.Lookup[model.MessageRef]{Call: model.Call{Correlation: "c"}, Ref: ref})
		return err
	}
	if err := look(m.Ref); err != nil {
		t.Fatal(err)
	}
	crossInbox := m.Ref
	crossInbox.Conversation.Inbox = env.inbox2
	crossConv := m.Ref
	crossConv.Conversation = other.Ref
	foreign := m.Ref
	foreign.Conversation.Inbox.Tenant = "conformance-other"
	for _, ref := range []model.MessageRef{crossInbox, crossConv, foreign} {
		serviceCode(t, look(ref), model.NotFound)
	}
	_, err := env.client.Conversations().Get(env.ctx, model.Lookup[model.ConversationRef]{
		Call: model.Call{Correlation: "c"}, Ref: model.ConversationRef{Inbox: env.inbox2, ID: env.conv.Ref.ID},
	})
	serviceCode(t, err, model.NotFound)
	in2 := env.inbox2
	if got := env.messages(model.Filter{Inboxes: []model.InboxRef{in2}}, 10); len(got) != 0 {
		t.Fatalf("inbox-two messages %v, want none", got)
	}
	open := env.conversation(env.inbox2, model.ConversationOpen)
	p, err := env.client.Conversations().List(env.ctx, serviceList(model.Filter{
		Inboxes: []model.InboxRef{in2}, Statuses: []string{model.ConversationOpen},
	}, 10, ""))
	if err != nil || len(p.Items) != 2 || p.Items[0].Ref != other.Ref || p.Items[1].Ref != open.Ref {
		t.Fatalf("open conversations in inbox two = %+v, %v", p.Items, err)
	}
}

func serviceLocalCommands(t *testing.T, env *serviceEnv) {
	note, err := env.client.Messages().AddInternalNote(env.ctx, model.InternalNote{
		Command: env.cmd(""), Conversation: env.conv.Ref, PolicyRef: "staff",
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "internal"}}},
	})
	if err != nil || note.Operation.State != model.Committed || note.Operation.ResultMessage == nil {
		t.Fatalf("AddInternalNote = %+v, %v", note, err)
	}
	conv := env.conv.Ref
	if got := env.messages(model.Filter{Conversation: &conv}, 10); len(got) != 0 {
		t.Fatalf("member sees the note: %v", got)
	}
	env.policy.setStaff(true)
	if got := env.messages(model.Filter{Conversation: &conv}, 10); len(got) != 1 || got[0] != note.Operation.ResultMessage.ID {
		t.Fatalf("staff see %v, want the note", got)
	}
	a, err := env.client.Conversations().Transition(env.ctx, model.Transition{
		Command: env.cmd(env.conv.Revision), Conversation: env.conv.Ref, State: model.ConversationResolved,
	})
	if err != nil || a.Operation.State != model.Committed {
		t.Fatalf("Transition = %+v, %v", a, err)
	}
	got, err := env.client.Operations().Get(env.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "c"}, Ref: a.Operation.Ref})
	if err != nil || got.State != model.Committed {
		t.Fatalf("Operations.Get = %+v, %v", got, err)
	}
	cur, err := env.client.Conversations().Get(env.ctx, model.Lookup[model.ConversationRef]{Call: model.Call{Correlation: "c"}, Ref: env.conv.Ref})
	if err != nil || cur.Status != model.ConversationResolved {
		t.Fatalf("conversation %+v, %v", cur, err)
	}
	_, err = env.client.Conversations().Transition(env.ctx, model.Transition{
		Command: env.cmd(env.conv.Revision), Conversation: env.conv.Ref, State: model.ConversationOpen,
	})
	serviceCode(t, err, model.Conflict)
}
