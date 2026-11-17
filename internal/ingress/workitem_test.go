package ingress_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/ingress"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// deskType is a ticketing channel: it receives tickets and comments and
// posts replies as comments.
const deskType = enginetest.OutKey

// deskFactory is the scripted outbound adapter with the ingress identity
// namespace, so one channel both receives and sends.
type deskFactory struct{ *enginetest.OutFactory }

func (f deskFactory) Describe() adapter.Descriptor {
	d := f.OutFactory.Describe()
	d.IdentityNamespaces = []string{ingressNS}
	d.Events = append(d.Events, model.EventMessageReceived, model.EventWorkItemUpdated)
	return d
}

func withDesk(f *enginetest.OutFactory) enginetest.HarnessOption {
	return func(c *engine.Config) { c.Adapters = append(c.Adapters, deskFactory{f}) }
}

// desk is a per-work-item inbox on a ticketing channel.
type desk struct {
	*harness
	f     *enginetest.OutFactory
	inbox model.Inbox
}

func newDesk(t *testing.T, opts ...enginetest.HarnessOption) *desk {
	t.Helper()
	return newDeskWith(t, &enginetest.OutFactory{}, opts...)
}

// newDeskWith is a desk whose adapter is f.
func newDeskWith(t *testing.T, f *enginetest.OutFactory, opts ...enginetest.HarnessOption) *desk {
	t.Helper()
	h := newHarness(t, deskType, append([]enginetest.HarnessOption{withDesk(f)}, opts...)...)
	return &desk{harness: h, f: f, inbox: h.Configure(model.Selection{Mode: model.ModePerWorkItem}, dest)}
}

func (d *desk) issue(id string) model.ExternalRef { return d.Ref(model.RefKindWorkItem, id, "repo-1") }

// comment is a public comment on issue.
func (d *desk) comment(event, msgID, from, issue string) model.Envelope {
	return d.Inbound(event, msgID, from, dest, d.issue(issue))
}

// opened is the issue's description message carrying its state.
func (d *desk) opened(event, issue, from string, at time.Time, s model.WorkItemState) model.Envelope {
	env := d.Inbound(event, "description:"+issue, from, dest, d.issue(issue))
	env.OccurredAt, env.SourceRevision, env.WorkItem = at, event, &s
	return env
}

// changed is a provider-side change of issue without a message.
func (d *desk) changed(event, issue string, at time.Time, s model.WorkItemState) model.Envelope {
	return model.Envelope{
		EventID: event, Destination: dest, Kind: model.EnvelopeWorkItem, SystemActor: true,
		References: []model.ExternalRef{d.issue(issue)}, OccurredAt: at, SourceRevision: event, WorkItem: &s,
	}
}

func (d *desk) items() []model.WorkItem {
	return all(d.harness, store.Reader.WorkItems, model.Filter{Inboxes: []model.InboxRef{d.inbox.Ref}})
}

func (d *desk) item(conv model.ConversationRef) model.WorkItem {
	d.T.Helper()
	got := all(d.harness, store.Reader.WorkItems, model.Filter{Conversation: &conv})
	if len(got) != 1 {
		d.T.Fatalf("work items of %s: %d, want 1", conv.ID, len(got))
	}
	return got[0]
}

func (d *desk) conversations() []model.Conversation { return d.Conversations(d.inbox.Ref) }
func (d *desk) messages() []model.Message           { return d.Messages(d.inbox.Ref) }

func (d *desk) onlyConversation() model.Conversation {
	d.T.Helper()
	cs := d.conversations()
	if len(cs) != 1 {
		d.T.Fatalf("conversations = %d, want 1", len(cs))
	}
	return cs[0]
}

func reporter(d *desk) model.WorkItemState {
	return model.WorkItemState{
		Subject: "Build fails", URL: "https://tracker.example/repo-1/7", Status: model.WorkItemOpen, RawStatus: "open",
		Requester: d.SenderRef("reporter"), Assignees: []model.ExternalRef{d.SenderRef("triager")},
		Fields: []model.Extension{{Name: "com.example.tracker.labels", Version: 1, Data: json.RawMessage(`["bug"]`)}},
	}
}

// workItemPayload is the work_item.updated payload as consumers read it.
type workItemPayload struct {
	WorkItem model.WorkItemRef `json:"work_item"`
	Changes  []string          `json:"changes"`
	Trigger  string            `json:"trigger"`
	Status   *struct {
		From    string `json:"from"`
		To      string `json:"to"`
		RawFrom string `json:"raw_from"`
		RawTo   string `json:"raw_to"`
	} `json:"status"`
	PreviousRevision model.Revision      `json:"previous_revision"`
	SourceRevision   string              `json:"source_revision"`
	Operation        *model.OperationRef `json:"operation"`
}

func workItemEvents(t *testing.T, evs []model.Event) []workItemPayload {
	t.Helper()
	var out []workItemPayload
	for _, ev := range evs {
		if ev.Kind != model.EventWorkItemUpdated {
			continue
		}
		var p workItemPayload
		if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
			t.Fatal(err)
		}
		if ev.Resource != p.WorkItem.Ref() || ev.Payload.Name != model.SchemaWorkItemUpdated {
			t.Fatalf("work_item.updated envelope %+v does not name its work item", ev)
		}
		out = append(out, p)
	}
	return out
}

// stripIdentity strips the allocated identity of a work item.
func stripIdentity(w model.WorkItem) model.WorkItem {
	w.Ref, w.Revision = model.WorkItemRef{}, ""
	return w
}

// A comment observed before its issue binds the work item the issue later
// fills in: both arrival orders converge on one conversation, one work item
// and the same state, and the reporter stays separate from the commenter.
func TestWorkItemCommentBeforeIssue(t *testing.T) {
	run := func(t *testing.T, commentFirst bool) (*desk, model.WorkItem) {
		d := newDesk(t)
		comment := d.comment("e-comment", "c-1", "commenter", "7")
		opened := d.opened("e-issue", "7", "reporter", t0.Add(-time.Hour), reporter(d))
		order := []model.Envelope{opened, comment}
		if commentFirst {
			order = []model.Envelope{comment, opened}
		}
		for _, env := range order {
			d.MustAccept(env)
		}
		conv := d.onlyConversation()
		if conv.ReplyRoute != d.issue("7") || !slices.Equal(conv.External, []model.ExternalRef{d.issue("7")}) {
			t.Fatalf("conversation route %v external %v, want the issue", conv.ReplyRoute, conv.External)
		}
		if n := len(d.messages()); n != 2 {
			t.Fatalf("messages = %d, want description and comment", n)
		}
		w := d.item(conv.Ref)
		if w.External != d.issue("7") || w.Requester != d.SenderRef("reporter") || w.Binding != model.WorkItemBindingActive {
			t.Fatalf("work item = %+v", w)
		}
		return d, w
	}
	d1, issueFirst := run(t, false)
	d2, commentFirst := run(t, true)
	if a, b := stripIdentity(issueFirst), stripIdentity(commentFirst); !jsonEqual(a, b) {
		t.Fatalf("arrival order changed the work item:\nissue first   %+v\ncomment first %+v", a, b)
	}
	if got := workItemEvents(t, d1.Events()); len(got) != 1 || !slices.Equal(got[0].Changes, []string{model.WorkItemFieldCreated}) {
		t.Fatalf("issue first events = %+v, want one created", got)
	}
	got := workItemEvents(t, d2.Events())
	if len(got) != 2 || !slices.Equal(got[0].Changes, []string{model.WorkItemFieldCreated}) ||
		!slices.Contains(got[1].Changes, model.WorkItemFieldSubject) || got[1].PreviousRevision == "" ||
		got[1].Trigger != model.TriggerProvider || got[1].SourceRevision != "e-issue" {
		t.Fatalf("comment first events = %+v, want created, then the issue's fields", got)
	}
}

func jsonEqual(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// The declared status mapping keeps the raw status; a stale or replayed
// state never overwrites a newer one.
func TestWorkItemStatusMappingAndStaleness(t *testing.T) {
	d := newDesk(t)
	d.MustAccept(d.opened("e-1", "7", "reporter", t0, reporter(d)))
	solved := reporter(d)
	solved.Status, solved.RawStatus = model.WorkItemResolved, "solved"
	d.MustAccept(d.changed("e-2", "7", t0.Add(2*time.Minute), solved))
	unmapped := reporter(d)
	unmapped.Status, unmapped.RawStatus = "", "on-hold-custom"
	late := unmapped
	d.MustAccept(d.changed("e-3", "7", t0.Add(time.Minute), late))
	conv := d.onlyConversation()
	if w := d.item(conv.Ref); w.Status != model.WorkItemResolved || w.RawStatus != "solved" || w.Source.Revision != "e-2" {
		t.Fatalf("after a late state: %+v, want the newer resolved/solved", w)
	}
	d.MustAccept(d.changed("e-4", "7", t0.Add(3*time.Minute), unmapped))
	if w := d.item(conv.Ref); w.Status != "" || w.RawStatus != "on-hold-custom" {
		t.Fatalf("unmapped raw status: %+v, want empty status and the raw value", w)
	}
	evs := workItemEvents(t, d.Events())
	if len(evs) != 3 {
		t.Fatalf("work_item.updated events = %+v, want created and two status changes", evs)
	}
	if s := evs[1].Status; s == nil || s.From != model.WorkItemOpen || s.To != model.WorkItemResolved || s.RawFrom != "open" || s.RawTo != "solved" {
		t.Fatalf("status change = %+v", evs[1].Status)
	}
	if !slices.Equal(evs[2].Changes, []string{model.WorkItemFieldStatus}) {
		t.Fatalf("changes = %v, want status only", evs[2].Changes)
	}
}

// A merge keeps the merged item's binding, conversation and history; a
// later comment on it still joins its conversation; the successor is
// untouched.
func TestWorkItemMergeKeepsBindingHistory(t *testing.T) {
	d := newDesk(t)
	d.MustAccept(d.opened("e-a", "7", "reporter", t0, reporter(d)))
	d.MustAccept(d.opened("e-b", "8", "reporter", t0, reporter(d)))
	merged := reporter(d)
	merged.Status, merged.RawStatus, merged.Binding = model.WorkItemClosed, "closed", model.WorkItemBindingMerged
	into := d.issue("8")
	merged.Successor = &into
	d.MustAccept(d.changed("e-merge", "7", t0.Add(time.Minute), merged))
	d.MustAccept(d.comment("e-late", "c-late", "commenter", "7"))

	convs := d.conversations()
	if len(convs) != 2 {
		t.Fatalf("conversations = %d, want one per issue", len(convs))
	}
	var a, b model.WorkItem
	for _, c := range convs {
		switch w := d.item(c.Ref); w.External {
		case d.issue("7"):
			a = w
			if n := len(all(d.harness, store.Reader.Messages, model.Filter{Conversation: &c.Ref})); n != 2 {
				t.Fatalf("merged issue messages = %d, want description and late comment", n)
			}
		case d.issue("8"):
			b = w
		}
	}
	if a.Binding != model.WorkItemBindingMerged || a.Successor == nil || *a.Successor != into || a.External != d.issue("7") ||
		len(a.History) != 1 || a.History[0].From != model.WorkItemBindingActive || a.History[0].To != model.WorkItemBindingMerged ||
		a.History[0].SourceRevision != "e-merge" || a.Status != model.WorkItemClosed {
		t.Fatalf("merged work item = %+v", a)
	}
	if b.Binding != model.WorkItemBindingActive || len(b.History) != 0 {
		t.Fatalf("successor work item = %+v, want untouched", b)
	}
	evs := workItemEvents(t, d.Events())
	last := evs[len(evs)-1]
	if last.WorkItem != a.Ref || !slices.Contains(last.Changes, model.WorkItemFieldExternal) || !slices.Contains(last.Changes, model.WorkItemFieldStatus) {
		t.Fatalf("merge event = %+v", last)
	}
	// Deleted, then visible again: every transition stays in history.
	gone := merged
	gone.Binding, gone.Successor = model.WorkItemBindingDeleted, nil
	d.MustAccept(d.changed("e-del", "7", t0.Add(2*time.Minute), gone))
	back := reporter(d)
	d.MustAccept(d.changed("e-back", "7", t0.Add(3*time.Minute), back))
	w := d.item(a.Ref.Conversation)
	var path []string
	for _, h := range w.History {
		path = append(path, h.To)
	}
	if !slices.Equal(path, []string{model.WorkItemBindingMerged, model.WorkItemBindingDeleted, model.WorkItemBindingActive}) || w.Successor != nil {
		t.Fatalf("binding history = %v, successor %v", path, w.Successor)
	}
}

// The binding survives local resolution, and a conversation created after
// a local close inherits the work item; a metadata change never opens one.
func TestWorkItemBindingSurvivesLocalLifecycle(t *testing.T) {
	d := newDesk(t)
	d.MustAccept(d.opened("e-1", "7", "reporter", t0, reporter(d)))
	conv := d.onlyConversation()
	transition := func(c model.Conversation, state string) {
		t.Helper()
		_, err := d.Client.Conversations().Transition(d.Ctx, model.Transition{
			Command:      model.Command{Call: model.Call{Correlation: "corr"}, Operation: model.OperationID("t-" + state), ExpectedRevision: c.Revision},
			Conversation: c.Ref, State: state,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	transition(conv, model.ConversationResolved)
	d.MustAccept(d.comment("e-2", "c-2", "commenter", "7"))
	if cs := d.conversations(); len(cs) != 1 || cs[0].Status != model.ConversationResolved {
		t.Fatalf("after a comment on a resolved conversation: %+v", cs)
	}
	if w := d.item(conv.Ref); w.Subject != "Build fails" || w.Status != model.WorkItemOpen {
		t.Fatalf("local resolution changed the work item: %+v", w)
	}
	transition(d.onlyConversation(), model.ConversationClosed)
	solved := reporter(d)
	solved.Status, solved.RawStatus = model.WorkItemResolved, "solved"
	d.MustAccept(d.changed("e-3", "7", t0.Add(time.Minute), solved))
	if cs := d.conversations(); len(cs) != 1 {
		t.Fatalf("a metadata change opened a conversation: %d", len(cs))
	}
	if w := d.item(conv.Ref); w.Status != model.WorkItemResolved {
		t.Fatalf("closed conversation's work item = %+v, want the provider change", w)
	}
	d.MustAccept(d.comment("e-4", "c-4", "commenter", "7"))
	cs := d.conversations()
	if len(cs) != 2 {
		t.Fatalf("conversations = %d, want a successor after the local close", len(cs))
	}
	succ := cs[1]
	if succ.Ref == conv.Ref {
		succ = cs[0]
	}
	w, prev := d.item(succ.Ref), d.item(conv.Ref)
	if w.External != prev.External || w.Subject != prev.Subject || w.Status != model.WorkItemResolved || w.Ref == prev.Ref {
		t.Fatalf("successor work item %+v, want inherited from %+v", w, prev)
	}
}

// Fields declared local keep uma's value: the provider seeds them once and
// later states never overwrite them, while the raw status still follows the
// provider.
func TestWorkItemLocalFieldsKeepTheirValue(t *testing.T) {
	d := newDesk(t)
	s := reporter(d)
	s.LocalFields = []string{model.WorkItemFieldStatus, model.WorkItemFieldSubject}
	d.MustAccept(d.opened("e-1", "7", "reporter", t0, s))
	conv := d.onlyConversation()
	if w := d.item(conv.Ref); w.Status != model.WorkItemOpen || w.Subject != "Build fails" ||
		!slices.Equal(w.LocalFields, []string{model.WorkItemFieldStatus, model.WorkItemFieldSubject}) {
		t.Fatalf("seeded work item = %+v", w)
	}
	next := s
	next.Status, next.RawStatus, next.Subject, next.URL = model.WorkItemClosed, "closed", "Renamed", "https://tracker.example/moved"
	d.MustAccept(d.changed("e-2", "7", t0.Add(time.Minute), next))
	if w := d.item(conv.Ref); w.Status != model.WorkItemOpen || w.Subject != "Build fails" || w.RawStatus != "closed" ||
		w.URL != "https://tracker.example/moved" {
		t.Fatalf("after a provider state: %+v, want local status and subject kept", w)
	}
}

// Work item state is accepted only where it can bind, and only with
// references on the bound channel.
func TestWorkItemEnvelopeChecks(t *testing.T) {
	d := newDesk(t)
	s := reporter(d)
	noState := d.changed("e-1", "7", t0, s)
	noState.WorkItem = nil
	requireCode(t, mustErr(d.Accept(noState)), model.Validation, "envelope.work_item")
	withContent := d.changed("e-2", "7", t0, s)
	withContent.Content = model.Content{Parts: []model.Part{{Kind: "text", Text: "x"}}}
	requireCode(t, mustErr(d.Accept(withContent)), model.Validation, "envelope.content")
	noRef := d.changed("e-3", "7", t0, s)
	noRef.References = nil
	requireCode(t, mustErr(d.Accept(noRef)), model.Validation, "envelope.references")
	foreign := s
	foreign.Requester.Channel.ID = "00000009-0000-7000-8000-000000000009"
	requireCode(t, mustErr(d.Accept(d.changed("e-4", "7", t0, foreign))), model.Validation, "envelope.work_item")
	bad := s
	bad.Status = "triage"
	requireCode(t, mustErr(d.Accept(d.changed("e-5", "7", t0, bad))), model.Validation, "work_item.status")
	edit := d.comment("e-6", "c-1", "commenter", "7")
	edit.Kind, edit.WorkItem = model.EnvelopeMessageEdited, &s
	requireCode(t, mustErr(d.Accept(edit)), model.Validation, "envelope.work_item")

	room := newHarness(t, chatType)
	room.Configure(model.Selection{Mode: model.ModePerRoom}, dest)
	inRoom := room.Inbound("e-7", "m-1", "u", dest, room.Ref(model.RefKindRoom, "r", ""), room.Ref(model.RefKindWorkItem, "7", ""))
	inRoom.WorkItem = &model.WorkItemState{}
	requireCode(t, mustErr(room.Accept(inRoom)), model.Validation, "per-work-item inbox")
	change := model.Envelope{
		EventID: "e-8", Destination: dest, Kind: model.EnvelopeWorkItem, SystemActor: true,
		References: []model.ExternalRef{room.Ref(model.RefKindWorkItem, "7", "")}, WorkItem: &model.WorkItemState{},
	}
	requireCode(t, mustErr(room.Accept(change)), model.Validation, "per-work-item inbox")
	if len(d.items()) != 0 || len(room.Events()) != 0 {
		t.Fatal("a rejected envelope left a trace")
	}
}

func mustErr(_ model.IngressReceipt, err error) error { return err }

// Echo reconciliation.

// deliver runs the desk's outbound worker until no work is claimable.
func (d *desk) deliver() {
	d.T.Helper()
	for range 8 {
		works, err := d.RT.ClaimWork(d.Ctx, tenantA, runtime.WorkerOptions{
			Owner: "w", Limit: 10, Kinds: []string{operations.WorkKindOperation}, Lease: time.Minute,
		})
		if err != nil {
			d.T.Fatal(err)
		}
		if len(works) == 0 {
			return
		}
		for _, w := range works {
			if err := d.RT.ExecuteClaimed(d.Ctx, tenantA, w); err != nil {
				d.T.Fatal(err)
			}
		}
	}
	d.T.Fatal("outbound work did not settle")
}

func (d *desk) reply(conv model.ConversationRef, opID model.OperationID, text string) model.Acceptance {
	d.T.Helper()
	a, err := d.Client.Messages().Reply(d.Ctx, model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: "corr"}, Operation: opID},
		Conversation: conv, Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
	})
	if err != nil {
		d.T.Fatalf("Reply: %v", err)
	}
	return a
}

func (d *desk) op(ref model.OperationRef) model.Operation {
	d.T.Helper()
	op, err := d.Client.Operations().Get(d.Ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "get"}, Ref: ref})
	if err != nil {
		d.T.Fatal(err)
	}
	return op
}

// sentAs scripts the next send to be applied under provider message ID id.
func (d *desk) sentAs(id string) {
	d.f.Respond(func(_ context.Context, r adapter.SendRequest) (adapter.Outcome, error, bool) {
		return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{model.ProviderMessageRef(r.Route.Channel, id)}}, nil, true
	})
}

func received(evs []model.Event) int {
	n := 0
	for _, ev := range evs {
		if ev.Kind == model.EventMessageReceived {
			n++
		}
	}
	return n
}

// The provider's echo of a comment uma posted is the same message: no second
// message, no message.received, so nothing replies to it.
func TestEchoOfDeliveredReplyIsTheSameMessage(t *testing.T) {
	d := newDesk(t)
	d.MustAccept(d.comment("e-1", "c-1", "customer", "7"))
	conv := d.onlyConversation()
	d.sentAs("c-2")
	a := d.reply(conv.Ref, "op-1", "on it")
	d.deliver()
	if op := d.op(a.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("reply state = %s", op.State)
	}
	before := len(d.Events())
	echo := d.comment("e-2", "c-2", "uma-bot", "7")
	echo.Content.Parts[0].Text = "on it"
	r := d.MustAccept(echo)
	if !r.Committed || r.Duplicate {
		t.Fatalf("echo receipt = %+v, want a committed ingress", r)
	}
	if n := len(d.messages()); n != 2 {
		t.Fatalf("messages = %d, want the comment and the reply only", n)
	}
	if evs := d.Events(); len(evs) != before {
		t.Fatalf("echo emitted %v", eventKinds(evs[before:]))
	}
	if m := d.MessageByProviderID(d.inbox.Ref, "c-2"); m.Origin == nil || m.Origin.Operation != a.Operation.Ref {
		t.Fatalf("provider ID c-2 names %+v, want the outbound reply", m)
	}
	// Similar body and sender, different provider message: a new message.
	similar := d.comment("e-3", "c-3", "uma-bot", "7")
	similar.Content.Parts[0].Text = "on it"
	d.MustAccept(similar)
	if n := len(d.messages()); n != 3 || received(d.Events()) != 2 {
		t.Fatalf("messages = %d, want a similar message recorded as new", n)
	}
}

// An echo that reports its attempt settles a reply whose answer was lost,
// without sending it again; its provider message ID joins the outbound
// message, so the provider's later redelivery is an echo too.
func TestMarkedEchoSettlesLostAnswerWithoutResend(t *testing.T) {
	d := newDesk(t)
	d.MustAccept(d.comment("e-1", "c-1", "customer", "7"))
	conv := d.onlyConversation()
	d.f.Respond(enginetest.LostAnswer)
	a := d.reply(conv.Ref, "op-1", "on it")
	d.deliver()
	if op := d.op(a.Operation.Ref); op.State != model.Unknown {
		t.Fatalf("reply state = %s, want outcome_unknown", op.State)
	}
	sends := d.f.ProviderSends()
	if len(sends) != 1 {
		t.Fatalf("sends = %d", len(sends))
	}
	echo := d.comment("e-2", "c-2", "uma-bot", "7")
	echo.References = append(echo.References, model.EchoRef(d.Channel.Ref, a.Operation.Ref, sends[0].Attempt))
	d.MustAccept(echo)
	if n := len(d.messages()); n != 2 || received(d.Events()) != 1 {
		t.Fatalf("messages = %d, want the echo recognized", n)
	}
	out := d.MessageByProviderID(d.inbox.Ref, "c-2")
	if out.Ref != *a.Operation.ResultMessage {
		t.Fatalf("provider ID c-2 names %v, want the outbound message", out.Ref)
	}
	d.deliver()
	op := d.op(a.Operation.Ref)
	if op.State != model.ProviderAccepted || !slices.Contains(op.ProviderReferences, model.ProviderMessageRef(d.Channel.Ref, "c-2")) {
		t.Fatalf("after the echo: %+v, want provider_accepted with the echoed ID", op)
	}
	if n := len(d.f.ProviderSends()); n != 1 {
		t.Fatalf("sends = %d, want the reply sent once", n)
	}
	kinds := eventKinds(d.Events())
	if kinds[len(kinds)-1] != model.EventDeliveryUpdated {
		t.Fatalf("events = %v, want delivery.updated last", kinds)
	}
	again := d.comment("e-3", "c-2", "uma-bot", "7")
	d.MustAccept(again)
	if n := len(d.messages()); n != 2 {
		t.Fatalf("redelivered echo recorded: messages = %d", n)
	}
}

// An echo reference that names no attempt of an operation of this inbox
// correlates nothing: the message is recorded as usual, without it.
func TestEchoReferenceToUnknownAttemptIsIgnored(t *testing.T) {
	d := newDesk(t)
	d.MustAccept(d.comment("e-1", "c-1", "customer", "7"))
	conv := d.onlyConversation()
	a := d.reply(conv.Ref, "op-1", "on it")
	d.deliver()
	for i, ref := range []model.ExternalRef{
		model.EchoRef(d.Channel.Ref, a.Operation.Ref, "00000009-0000-7000-8000-000000000001"),
		model.EchoRef(d.Channel.Ref, model.OperationRef{Tenant: tenantA, ID: "00000009-0000-7000-8000-000000000002"}, "00000009-0000-7000-8000-000000000003"),
		{Channel: d.Channel.Ref, Namespace: model.EchoNamespace, Kind: model.RefKindAttempt, ID: "not-an-id", ParentID: "x"},
	} {
		env := d.comment("e-x"+string(rune('a'+i)), "c-x"+string(rune('a'+i)), "someone", "7")
		env.References = append(env.References, ref)
		d.MustAccept(env)
		m := d.MessageByProviderID(d.inbox.Ref, env.MessageID)
		if m.Origin != nil || slices.ContainsFunc(m.External, func(r model.ExternalRef) bool { return r.Namespace == model.EchoNamespace }) {
			t.Fatalf("message %+v, want an ordinary inbound message without the echo reference", m)
		}
	}
	if n := len(d.f.ProviderSends()); n != 1 {
		t.Fatalf("sends = %d", n)
	}
}

// Mail is never reconciled by message ID: the sender chooses a Message-ID,
// so an inbound mail reusing one of uma's is recorded, not swallowed.
func TestMailMessageIDIsNotEchoEvidence(t *testing.T) {
	h := newHarness(t, model.TypeSmtpMail)
	in := h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e-1", "<first@a.example>", "alice", dest))
	conv := h.Conversations(in.Ref)[0]
	ours := model.Message{
		Ref:     model.MessageRef{Conversation: conv.Ref, ID: h.ID()},
		Content: model.Content{Parts: []model.Part{{Kind: "text", Text: "ours"}}}, Audience: model.Audience{Visibility: model.Public},
		External: []model.ExternalRef{model.ProviderMessageRef(h.Channel.Ref, "<ours@uma.example>")},
		Origin:   &model.Origin{Actor: "actor-1", Operation: model.OperationRef{Tenant: tenantA, ID: h.ID()}},
		Revision: "r1", RecordedAt: t0,
	}
	h.Tx(func(tx store.Tx) error { return tx.PutMessage(h.Ctx, ours, "") })
	h.MustAccept(h.Inbound("e-2", "<ours@uma.example>", "mallory", dest))
	if n := len(h.Messages(in.Ref)); n != 3 {
		t.Fatalf("messages = %d, want the mail reusing our Message-ID recorded", n)
	}
}

// A direct-channel message and its desk copy stay separate: matching body
// and sender never link them, and each conversation keeps its one route on
// its own channel.
func TestDeskCopyIsNotLinkedBySimilarity(t *testing.T) {
	d := newDesk(t)
	mail := d.PutChannel(model.TypeSmtpMail)
	mailInbox, err := d.ConfigureOn(mail.Ref, perContact(true), "help@example.com")
	if err != nil {
		t.Fatal(err)
	}
	mailSink, err := ingress.Bind(d.Ctx, d.E, d.Service(), mail.Ref)
	if err != nil {
		t.Fatal(err)
	}
	direct := model.Envelope{
		EventID: "m-1", MessageID: "<q@customer.example>", Destination: "help@example.com", Kind: model.EnvelopeMessage,
		Sender: model.ExternalRef{Channel: mail.Ref, Namespace: ingressNS, Kind: "user", ID: "customer"}, Assurance: "verified",
		Content:  model.Content{Parts: []model.Part{{Kind: "text", Text: "My build fails"}}},
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: t0,
	}
	if _, err := mailSink.Accept(d.Ctx, model.Ingress{Envelope: direct}); err != nil {
		t.Fatal(err)
	}
	copyEnv := d.opened("t-1", "7", "customer", t0, reporter(d))
	copyEnv.Content = direct.Content
	d.MustAccept(copyEnv)

	mailConvs, deskConvs := d.Conversations(mailInbox.Ref), d.conversations()
	if len(mailConvs) != 1 || len(deskConvs) != 1 {
		t.Fatalf("conversations: mail %d, desk %d; want one each", len(mailConvs), len(deskConvs))
	}
	if r := mailConvs[0].ReplyRoute; r.Channel != mail.Ref {
		t.Fatalf("mail route %v, want its own channel", r)
	}
	if r := deskConvs[0].ReplyRoute; r != d.issue("7") {
		t.Fatalf("desk route %v, want the ticket", r)
	}
	for _, c := range [][]model.ExternalRef{mailConvs[0].External, deskConvs[0].External} {
		for _, r := range c {
			if r.Channel != mailConvs[0].ReplyRoute.Channel && r.Channel != d.Channel.Ref {
				t.Fatalf("external %v crosses channels", r)
			}
		}
	}
	if slices.ContainsFunc(deskConvs[0].External, func(r model.ExternalRef) bool { return r.Channel == mail.Ref }) ||
		slices.ContainsFunc(mailConvs[0].External, func(r model.ExternalRef) bool { return r.Channel == d.Channel.Ref }) {
		t.Fatal("similar copies were linked")
	}
	desk := d.item(deskConvs[0].Ref)
	if desk.Requester.Channel != d.Channel.Ref {
		t.Fatalf("desk requester %v, want the desk's own identity", desk.Requester)
	}
}
