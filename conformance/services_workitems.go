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

const deskType model.TypeKey = "com.example.conformance.desk"

const (
	deskSchema = "com.example.conformance.desk-config"
	deskNS     = "com.example.conformance.desk-user"
	deskDest   = "desk"
)

// deskAdapter is a ticketing channel whose webhook instance hands the suite
// the engine-bound sink, so ingress runs through the engine's own
// normalization, selection and work item binding.
type deskAdapter struct {
	mu   sync.Mutex
	sink adapter.Sink
	// events are extension event kinds the adapter declares besides its
	// builtin ones.
	events []string
}

func (a *deskAdapter) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: deskType, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: deskSchema, Version: 1},
		Capabilities:        []model.Capability{model.CapabilityReceiveHTTP},
		Events:              append([]string{model.EventMessageReceived, model.EventWorkItemUpdated}, a.events...),
		IdentityNamespaces:  []string{deskNS},
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsProvider},
	}
}
func (*deskAdapter) Validate(model.Extension) []model.ValidationIssue { return nil }
func (a *deskAdapter) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	return deskInstance{a}, nil
}

type deskInstance struct{ a *deskAdapter }

func (deskInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return []model.CapabilityStatus{{Name: model.CapabilityReceiveHTTP, Available: true}}, nil
}
func (deskInstance) Close(context.Context) error { return nil }
func (i deskInstance) Handler(s adapter.Sink) http.Handler {
	i.a.mu.Lock()
	i.a.sink = s
	i.a.mu.Unlock()
	return http.NotFoundHandler()
}

// desk is one engine over a fresh store with a per-work-item inbox on a
// ticketing channel and the sink bound to it.
type desk struct {
	t       *testing.T
	ctx     context.Context
	st      store.Store
	clock   *portstest.FixedClock
	engine  uma.Engine
	client  uma.Client
	channel model.ChannelRef
	inbox   model.InboxRef
	sink    adapter.Sink
}

func newDesk(t *testing.T, factory StoreFactory) *desk {
	t.Helper()
	return newDeskWith(t, factory, model.Selection{Mode: model.ModePerWorkItem})
}

// newDeskWith is newDesk with the inbox's selection set to sel.
func newDeskWith(t *testing.T, factory StoreFactory, sel model.Selection) *desk {
	t.Helper()
	return newDeskComposed(t, factory, sel, nil)
}

// newDeskComposed is newDeskWith with compose adjusting the adapter and
// the engine configuration before the engine is built.
func newDeskComposed(t *testing.T, factory StoreFactory, sel model.Selection, compose func(*deskAdapter, *uma.Config)) *desk {
	t.Helper()
	fx := factory(t)
	a := &deskAdapter{}
	clock := portstest.NewFixedClock(fx.Now())
	cfg := uma.Config{
		Store: fx.Store, Clock: clock, IDs: portstest.NewSequenceIDs(0xde5c),
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters: []adapter.Factory{a},
	}
	if compose != nil {
		compose(a, &cfg)
	}
	e, err := uma.New(cfg)
	if err != nil {
		t.Fatalf("uma.New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	staff := model.Principal{Tenant: serviceTenant, Actor: "staff-1"}
	client, err := e.For(staff)
	if err != nil {
		t.Fatal(err)
	}
	d := &desk{t: t, ctx: t.Context(), st: fx.Store, clock: clock, engine: e, client: client}
	cmd := func(op string) model.Command {
		return model.Command{Call: model.Call{Correlation: "corr"}, Operation: model.OperationID(op)}
	}
	ca, err := client.Configuration().ConfigureChannel(d.ctx, model.ConfigureChannel{
		Command: cmd("op-channel"),
		Channel: model.Channel{Ref: model.ChannelRef{Tenant: serviceTenant}, Type: deskType, Configuration: model.Extension{Name: deskSchema, Version: 1}},
	})
	if err != nil {
		t.Fatalf("ConfigureChannel: %v", err)
	}
	if d.channel, err = ca.Operation.Result.AsChannel(); err != nil {
		t.Fatal(err)
	}
	ia, err := client.Configuration().ConfigureInbox(d.ctx, model.ConfigureInbox{
		Command: cmd("op-inbox"),
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: serviceTenant}, Channel: d.channel, Name: deskDest, Destinations: []string{deskDest},
			Selection: sel,
		},
	})
	if err != nil {
		t.Fatalf("ConfigureInbox: %v", err)
	}
	if d.inbox, err = ia.Operation.Result.AsInbox(); err != nil {
		t.Fatal(err)
	}
	service := uma.RuntimeCall{Principal: model.Principal{Tenant: serviceTenant, Actor: "desk-service"}, Call: model.Call{Correlation: "corr"}}
	if _, err := e.Runtime().Webhook(d.ctx, service, d.channel); err != nil {
		t.Fatalf("Webhook: %v", err)
	}
	a.mu.Lock()
	d.sink = a.sink
	a.mu.Unlock()
	if d.sink == nil {
		t.Fatal("webhook binding did not hand the instance a sink")
	}
	return d
}

func (d *desk) ref(kind, id, parent string) model.ExternalRef {
	return model.ExternalRef{Channel: d.channel, Namespace: deskNS, Kind: kind, ID: id, ParentID: parent}
}

func (d *desk) issue(id string) model.ExternalRef { return d.ref(model.RefKindWorkItem, id, "repo-1") }
func (d *desk) user(id string) model.ExternalRef  { return d.ref("user", id, "") }

func (d *desk) message(event, msgID, from string, at time.Time, issue string) model.Envelope {
	return model.Envelope{
		EventID: event, MessageID: msgID, Destination: deskDest, Kind: model.EnvelopeMessage,
		Sender: d.user(from), Assurance: "verified", References: []model.ExternalRef{d.issue(issue)},
		Content:  model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "text of " + event}}},
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: at,
	}
}

func (d *desk) accept(env model.Envelope) {
	d.t.Helper()
	if _, err := d.sink.Accept(d.ctx, model.Ingress{Call: model.Call{Correlation: model.CorrelationID("corr-" + env.EventID)}, Envelope: env}); err != nil {
		d.t.Fatalf("Accept(%s): %v", env.EventID, err)
	}
}

// read pages list under f to the end.
func deskRead[T any](d *desk, list func(store.Reader, context.Context, model.ListRequest) (model.Page[T], error), f model.Filter) []T {
	d.t.Helper()
	var out []T
	err := d.st.View(d.ctx, serviceTenant, func(r store.Reader) error {
		req := model.ListRequest{Filter: f, Page: model.PageRequest{Limit: 100}}
		for {
			p, err := list(r, d.ctx, req)
			if err != nil {
				return err
			}
			out = append(out, p.Items...)
			if p.Complete {
				return nil
			}
			req.Page.After = p.Next
		}
	})
	if err != nil {
		d.t.Fatalf("View: %v", err)
	}
	return out
}

// workItemChanges returns the changes of every work_item.updated event in
// commit order.
func (d *desk) workItemChanges() [][]string {
	d.t.Helper()
	var out [][]string
	err := d.st.View(d.ctx, serviceTenant, func(r store.Reader) error {
		var after model.Cursor
		for {
			p, err := r.Events(d.ctx, model.Filter{Kinds: []string{model.EventWorkItemUpdated}}, after, 100)
			if err != nil {
				return err
			}
			for _, ev := range p.Items {
				var payload struct {
					Changes []string `json:"changes"`
				}
				if err := json.Unmarshal(ev.Payload.Data, &payload); err != nil {
					return err
				}
				out = append(out, payload.Changes)
			}
			if p.Complete {
				return nil
			}
			after = p.Next
		}
	})
	if err != nil {
		d.t.Fatalf("events: %v", err)
	}
	return out
}

// serviceWorkItemArrivalOrder: a comment observed before its issue binds
// the work item the issue later fills in. Both arrival orders converge on
// one conversation, one work item and the same stored state, and the
// reporter stays separate from the commenter.
func serviceWorkItemArrivalOrder(t *testing.T, factory StoreFactory) {
	at := time.Unix(1_700_000_000, 0).UTC()
	run := func(t *testing.T, commentFirst bool) (*desk, model.WorkItem) {
		d := newDesk(t, factory)
		comment := d.message("e-comment", "c-1", "commenter", at, "7")
		opened := d.message("e-issue", "description:7", "reporter", at.Add(-time.Hour), "7")
		opened.SourceRevision = "e-issue"
		opened.WorkItem = &model.WorkItemState{
			Subject: "Build fails", URL: "https://tracker.example/repo-1/7", Status: model.WorkItemOpen, RawStatus: "open",
			Requester: d.user("reporter"), Assignees: []model.ExternalRef{d.user("triager")},
			Fields: []model.Extension{{Name: "com.example.tracker.labels", Version: 1, Data: json.RawMessage(`["bug"]`)}},
		}
		order := []model.Envelope{opened, comment}
		if commentFirst {
			order = []model.Envelope{comment, opened}
		}
		for _, env := range order {
			d.accept(env)
		}
		in := model.Filter{Inboxes: []model.InboxRef{d.inbox}}
		convs := deskRead(d, store.Reader.Conversations, in)
		if len(convs) != 1 {
			t.Fatalf("conversations = %d, want 1", len(convs))
		}
		conv := convs[0]
		if conv.ReplyRoute != d.issue("7") || !slices.Equal(conv.External, []model.ExternalRef{d.issue("7")}) {
			t.Fatalf("conversation route %v external %v, want the issue", conv.ReplyRoute, conv.External)
		}
		if n := len(deskRead(d, store.Reader.Messages, in)); n != 2 {
			t.Fatalf("messages = %d, want description and comment", n)
		}
		items := deskRead(d, store.Reader.WorkItems, model.Filter{Conversation: &conv.Ref})
		if len(items) != 1 {
			t.Fatalf("work items of the conversation = %d, want 1", len(items))
		}
		w := items[0]
		if w.External != d.issue("7") || w.Requester != d.user("reporter") || w.Binding != model.WorkItemBindingActive {
			t.Fatalf("work item = %+v", w)
		}
		if n := len(deskRead(d, store.Reader.WorkItems, in)); n != 1 {
			t.Fatalf("work items of the inbox = %d, want 1", n)
		}
		return d, w
	}
	d1, issueFirst := run(t, false)
	d2, commentFirst := run(t, true)
	strip := func(w model.WorkItem) string {
		w.Ref, w.Revision = model.WorkItemRef{}, ""
		b, err := json.Marshal(w)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if a, b := strip(issueFirst), strip(commentFirst); a != b {
		t.Fatalf("arrival order changed the work item:\nissue first   %s\ncomment first %s", a, b)
	}
	if got := d1.workItemChanges(); len(got) != 1 || !slices.Equal(got[0], []string{model.WorkItemFieldCreated}) {
		t.Fatalf("issue first work_item.updated changes = %v, want one created", got)
	}
	got := d2.workItemChanges()
	if len(got) != 2 || !slices.Equal(got[0], []string{model.WorkItemFieldCreated}) || !slices.Contains(got[1], model.WorkItemFieldSubject) {
		t.Fatalf("comment first work_item.updated changes = %v, want created, then the issue's fields", got)
	}
}

// serviceProviderMessageOnce: one provider-assigned message reported under
// two event IDs, one after the other or at once, is stored once with one
// message.received, and a later edit applies to it.
func serviceProviderMessageOnce(t *testing.T, factory StoreFactory) {
	at := time.Unix(1_700_000_000, 0).UTC()
	d := newDesk(t, factory)
	d.accept(d.message("webhook-1", "c-1", "commenter", at, "7"))
	d.accept(d.message("poll-1", "c-1", "commenter", at, "7"))
	const racing = 8
	var wg sync.WaitGroup
	errs := make(chan error, 2*racing)
	for i := range racing {
		for _, path := range []string{"webhook", "poll"} {
			env := d.message(fmt.Sprintf("%s-race-%d", path, i), fmt.Sprintf("c-race-%d", i), "commenter", at, "7")
			wg.Go(func() {
				_, err := d.sink.Accept(d.ctx, model.Ingress{Call: model.Call{Correlation: "corr-race"}, Envelope: env})
				errs <- err
			})
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Accept: %v", err)
		}
	}
	edit := d.message("edit-1", "c-1", "commenter", at.Add(time.Minute), "7")
	edit.Kind, edit.SourceRevision = model.EnvelopeMessageEdited, "2"
	edit.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "edited"}}}
	d.accept(edit)

	msgs := deskRead(d, store.Reader.Messages, model.Filter{Inboxes: []model.InboxRef{d.inbox}})
	byID := map[string]int{}
	for _, m := range msgs {
		for _, r := range m.External {
			if r.Namespace == model.ProviderMessageNamespace && r.Kind == model.RefKindMessage {
				byID[r.ID]++
				if r.ID == "c-1" && firstText(m) != "edited" {
					t.Errorf("c-1 content = %q, want the edit applied", firstText(m))
				}
			}
		}
	}
	if len(msgs) != 1+racing || byID["c-1"] != 1 {
		t.Fatalf("messages = %d (by provider ID %v), want one per provider message", len(msgs), byID)
	}
	for id, n := range byID {
		if n != 1 {
			t.Errorf("provider message %s stored %d times", id, n)
		}
	}
	var received int
	err := d.st.View(d.ctx, serviceTenant, func(r store.Reader) error {
		p, err := r.Events(d.ctx, model.Filter{Kinds: []string{model.EventMessageReceived}}, "", 100)
		received = len(p.Items)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if received != 1+racing {
		t.Fatalf("message.received = %d, want one per provider message (%d)", received, 1+racing)
	}
}

// firstText returns the text of a message's first part.
func firstText(m model.Message) string {
	if len(m.Content.Parts) == 0 {
		return ""
	}
	return m.Content.Parts[0].Text
}
