package services_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// workItem seeds a work item bound to a new conversation of inbox in.
func (s *svc) workItem(in model.InboxRef, status string, local ...string) model.WorkItem {
	s.T.Helper()
	conv := s.conversation(in, model.ConversationOpen)
	w := model.WorkItem{
		Ref:      model.WorkItemRef{Conversation: conv.Ref, ID: s.id()},
		External: model.ExternalRef{Channel: s.Ch.Ref, Namespace: routeNSpace, Kind: model.RefKindWorkItem, ID: string(s.id())},
		Subject:  "Build fails", Status: status, RawStatus: "raw-" + status, Binding: model.WorkItemBindingActive,
		LocalFields: local, Revision: "w1",
	}
	s.put(func(tx store.Tx) error { return tx.PutWorkItem(s.Ctx, w, "") })
	return w
}

func (s *svc) getWorkItem(ref model.WorkItemRef) (model.WorkItem, error) {
	return s.C.WorkItems().Get(s.Ctx, model.Lookup[model.WorkItemRef]{Call: call(), Ref: ref})
}

func (s *svc) stored(ref model.WorkItemRef) model.WorkItem {
	s.T.Helper()
	var w model.WorkItem
	if err := s.E.Store().View(s.Ctx, tenantA, func(r store.Reader) (err error) {
		w, err = r.GetWorkItem(s.Ctx, ref)
		return err
	}); err != nil {
		s.T.Fatal(err)
	}
	return w
}

func updateAction(t *testing.T, u model.WorkItemUpdate) model.Extension {
	t.Helper()
	data, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	return model.Extension{Name: model.SchemaWorkItemUpdate, Version: 1, Data: data}
}

func ptr[T any](v T) *T { return &v }

func (s *svc) act(w model.WorkItem, op model.OperationID, rev model.Revision, u model.WorkItemUpdate) (model.Acceptance, error) {
	return s.C.WorkItems().Act(s.Ctx, model.WorkItemAction{Command: cmd(op, rev), Target: w.Ref, Action: updateAction(s.T, u)})
}

func TestWorkItemReads(t *testing.T) {
	s := newSvc(t)
	open := s.workItem(s.Conv.Ref.Inbox, model.WorkItemOpen)
	closed := s.workItem(s.inbox2.Ref, model.WorkItemClosed)
	got, err := s.getWorkItem(open.Ref)
	if err != nil || got.Ref != open.Ref || got.Subject != "Build fails" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if a := s.pol.seen(); a[len(a)-1].Action != model.ActionWorkItemRead || a[len(a)-1].Resource != open.Ref.Ref() {
		t.Fatalf("policy saw %+v, want work_item.read on the work item", a[len(a)-1])
	}
	list := func(r model.ListRequest) (model.Page[model.WorkItem], error) { return s.C.WorkItems().List(s.Ctx, r) }
	ids := func(ws []model.WorkItem) []model.ID {
		var out []model.ID
		for _, w := range ws {
			out = append(out, w.Ref.ID)
		}
		return out
	}
	if got := ids(drain(t, list, model.Filter{}, 1)); !slices.Equal(got, []model.ID{open.Ref.ID, closed.Ref.ID}) {
		t.Fatalf("List = %v", got)
	}
	if got := ids(drain(t, list, model.Filter{Statuses: []string{model.WorkItemClosed}}, 10)); !slices.Equal(got, []model.ID{closed.Ref.ID}) {
		t.Fatalf("List closed = %v", got)
	}
	conv := open.Ref.Conversation
	if got := ids(drain(t, list, model.Filter{Conversation: &conv}, 10)); !slices.Equal(got, []model.ID{open.Ref.ID}) {
		t.Fatalf("List by conversation = %v", got)
	}
	_, err = list(listReq(model.Filter{Kinds: []string{"x"}}, 10, ""))
	requireCode(t, err, model.Validation, "filter.kinds")
	who := model.IdentityRef{Inbox: conv.Inbox, ID: open.Ref.ID}
	_, err = list(listReq(model.Filter{Identity: &who}, 10, ""))
	requireCode(t, err, model.Validation, "filter.identity")
	tk := model.TaskRef{Conversation: conv, ID: open.Ref.ID}
	_, err = list(listReq(model.Filter{Task: &tk}, 10, ""))
	requireCode(t, err, model.Validation, "filter.task")
	// Refused by the query itself, before any store read.
	if e := (*model.Error)(nil); !errors.As(err, &e) || e.SafeMessage != "filter.task: not supported by this query" {
		t.Fatalf("task filter refused by %v, want the query", err)
	}

	other := open.Ref
	other.Conversation.ID = closed.Ref.Conversation.ID
	_, err = s.getWorkItem(other)
	requireCode(t, err, model.NotFound, "")
	foreign := open.Ref
	foreign.Conversation.Inbox.Tenant = tenantB
	_, err = s.getWorkItem(foreign)
	requireCode(t, err, model.NotFound, "")

	s.pol.set(func(a policy.Authorization) bool { return a.Resource != open.Ref.Ref() })
	_, err = s.getWorkItem(open.Ref)
	requireCode(t, err, model.Unauthorized, "")
	if got := ids(drain(t, list, model.Filter{}, 10)); !slices.Equal(got, []model.ID{closed.Ref.ID}) {
		t.Fatalf("List under a narrower policy = %v", got)
	}
}

// Act changes only local fields, as a committed operation with a
// command-triggered work_item.updated event, and never reaches the provider.
func TestWorkItemActLocalField(t *testing.T) {
	s := newSvc(t)
	w := s.workItem(s.Conv.Ref.Inbox, model.WorkItemOpen, model.WorkItemFieldStatus, model.WorkItemFieldAssignees)
	assignee := model.ExternalRef{Channel: s.Ch.Ref, Namespace: routeNSpace, Kind: "user", ID: "staff-1"}
	u := model.WorkItemUpdate{Status: ptr(model.WorkItemResolved), Assignees: &[]model.ExternalRef{assignee}}
	a, err := s.act(w, "op-1", w.Revision, u)
	if err != nil || a.Operation.State != model.Committed || a.Replayed || a.Operation.Result == nil || *a.Operation.Result != w.Ref.Ref() {
		t.Fatalf("Act = %+v, %v", a, err)
	}
	got := s.stored(w.Ref)
	if got.Status != model.WorkItemResolved || got.RawStatus != w.RawStatus || !slices.Equal(got.Assignees, []model.ExternalRef{assignee}) || got.Revision == w.Revision {
		t.Fatalf("work item = %+v, want local status and assignees, provider raw status kept", got)
	}
	evs := s.Events()
	ev := evs[len(evs)-1]
	var p struct {
		WorkItem         model.WorkItemRef   `json:"work_item"`
		Changes          []string            `json:"changes"`
		Trigger          string              `json:"trigger"`
		Status           map[string]string   `json:"status"`
		PreviousRevision model.Revision      `json:"previous_revision"`
		Operation        *model.OperationRef `json:"operation"`
	}
	if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	if ev.Kind != model.EventWorkItemUpdated || ev.Resource != w.Ref.Ref() || ev.Revision != got.Revision || ev.Actor != actor ||
		p.Trigger != model.TriggerCommand || p.Operation == nil || *p.Operation != a.Operation.Ref || p.PreviousRevision != w.Revision ||
		!slices.Equal(p.Changes, []string{model.WorkItemFieldStatus, model.WorkItemFieldAssignees}) ||
		p.Status["from"] != model.WorkItemOpen || p.Status["to"] != model.WorkItemResolved {
		t.Fatalf("event %+v payload %+v", ev, p)
	}
	again, err := s.act(w, "op-1", w.Revision, u)
	if err != nil || !again.Replayed || again.Operation.Ref != a.Operation.Ref {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	_, err = s.act(w, "op-1", w.Revision, model.WorkItemUpdate{Status: ptr(model.WorkItemPending)})
	requireCode(t, err, model.Conflict, "different payload")
	_, err = s.act(w, "op-2", w.Revision, model.WorkItemUpdate{Status: ptr(model.WorkItemPending)})
	requireCode(t, err, model.Conflict, "revision changed")
	n := len(s.Events())
	if _, err := s.act(got, "op-3", got.Revision, model.WorkItemUpdate{Status: ptr(model.WorkItemResolved)}); err != nil {
		t.Fatal(err)
	}
	if len(s.Events()) != n || s.stored(w.Ref).Revision != got.Revision {
		t.Fatal("an update changing nothing wrote or emitted")
	}
	if s.Step("w") != 0 || len(s.F.ProviderSends()) != 0 {
		t.Fatal("a local work item change reached the provider")
	}
}

// Act refuses before any write: provider-owned fields (ingest-only),
// inactive bindings, the terminal closed status, unknown or incompatible
// actions; an unauthorized principal learns nothing beyond the denial.
func TestWorkItemActRefusals(t *testing.T) {
	s := newSvc(t)
	w := s.workItem(s.Conv.Ref.Inbox, model.WorkItemOpen, model.WorkItemFieldStatus)
	events := len(s.Events())
	check := func(name string, err error, code model.ErrorCode, want string) {
		t.Helper()
		t.Run(name, func(t *testing.T) { requireCode(t, err, code, want) })
		if got := s.stored(w.Ref); got.Revision != w.Revision {
			t.Fatalf("%s: work item written: %+v", name, got)
		}
		if len(s.Events()) != events {
			t.Fatalf("%s: events emitted", name)
		}
	}
	_, err := s.act(w, "op-subject", w.Revision, model.WorkItemUpdate{Subject: ptr("Renamed")})
	check("provider-owned field", err, model.Unsupported, "outbound work item writes are not enabled")

	s.pol.set(func(a policy.Authorization) bool { return a.Action != model.ActionWorkItemAct })
	_, err = s.act(w, "op-denied", w.Revision, model.WorkItemUpdate{Status: ptr(model.WorkItemResolved)})
	check("unauthorized", err, model.Unauthorized, "")
	_, err = s.act(w, "op-denied-owned", w.Revision, model.WorkItemUpdate{Subject: ptr("Renamed")})
	check("unauthorized provider-owned", err, model.Unauthorized, "")
	s.pol.set(nil)
	if a, err := s.act(w, "op-denied", w.Revision, model.WorkItemUpdate{Status: ptr(model.WorkItemResolved)}); err != nil || a.Replayed {
		t.Fatalf("after the denial, the same operation ID = %+v, %v; want a new execution", a, err)
	}
	w = s.stored(w.Ref)
	events = len(s.Events())

	_, err = s.C.WorkItems().Act(s.Ctx, model.WorkItemAction{Command: cmd("op-x", w.Revision), Target: w.Ref,
		Action: model.Extension{Name: "com.example.tracker.close", Version: 1}})
	check("unknown action", err, model.Unsupported, "unknown work item action")
	_, err = s.C.WorkItems().Act(s.Ctx, model.WorkItemAction{Command: cmd("op-v", w.Revision), Target: w.Ref,
		Action: model.Extension{Name: model.SchemaWorkItemUpdate, Version: 2, Data: json.RawMessage(`{"status":"open"}`)}})
	check("other version", err, model.SchemaMismatch, "")
	_, err = s.act(w, "op-norev", "", model.WorkItemUpdate{Status: ptr(model.WorkItemOpen)})
	check("no expected revision", err, model.Validation, "expected_revision")
	missing := w.Ref
	missing.ID = s.id()
	_, err = s.C.WorkItems().Act(s.Ctx, model.WorkItemAction{Command: cmd("op-m", w.Revision), Target: missing,
		Action: updateAction(t, model.WorkItemUpdate{Status: ptr(model.WorkItemOpen)})})
	check("missing", err, model.NotFound, "")

	closed := s.workItem(s.Conv.Ref.Inbox, model.WorkItemClosed, model.WorkItemFieldStatus)
	_, err = s.act(closed, "op-reopen", closed.Revision, model.WorkItemUpdate{Status: ptr(model.WorkItemOpen)})
	requireCode(t, err, model.Conflict, "terminal")
	merged := s.workItem(s.Conv.Ref.Inbox, model.WorkItemOpen, model.WorkItemFieldStatus)
	merged.Binding, merged.Successor, merged.Revision = model.WorkItemBindingMerged, &closed.External, "w2"
	s.put(func(tx store.Tx) error { return tx.PutWorkItem(s.Ctx, merged, "w1") })
	_, err = s.act(merged, "op-merged", merged.Revision, model.WorkItemUpdate{Status: ptr(model.WorkItemResolved)})
	requireCode(t, err, model.Conflict, "binding is merged")
}

// An internal note on a work item's conversation stays local: it is never
// queued for the provider, while a public reply is.
func TestInternalNoteOnWorkItemNeverReachesProvider(t *testing.T) {
	s := newSvc(t)
	w := s.workItem(s.Conv.Ref.Inbox, model.WorkItemOpen)
	conv := w.Ref.Conversation
	s.put(func(tx store.Tx) error {
		c, err := tx.GetConversation(s.Ctx, conv)
		if err != nil {
			return err
		}
		c.ReplyRoute, c.Revision = w.External, "cv2"
		return tx.PutConversation(s.Ctx, c, "cv1")
	})
	note, err := s.C.Messages().AddInternalNote(s.Ctx, model.InternalNote{
		Command: cmd("op-note", ""), Conversation: conv, PolicyRef: "support-staff",
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "customer is upset"}}},
	})
	if err != nil || note.Operation.State != model.Committed {
		t.Fatalf("AddInternalNote = %+v, %v", note, err)
	}
	if n := s.Step("w"); n != 0 || len(s.F.ProviderSends()) != 0 {
		t.Fatalf("internal note queued %d items, sent %d", n, len(s.F.ProviderSends()))
	}
	if _, err := s.C.Messages().Reply(s.Ctx, model.Reply{
		Command: cmd("op-reply", ""), Conversation: conv,
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "on it"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if s.Step("w") != 1 {
		t.Fatal("reply not queued")
	}
	sends := s.F.ProviderSends()
	if len(sends) != 1 || sends[0].Audience.Visibility != model.Public || sends[0].Route != w.External ||
		sends[0].Content.Parts[0].Text != "on it" {
		t.Fatalf("sends = %+v, want only the public reply on the work item route", sends)
	}
}
