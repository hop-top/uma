package services_test

import (
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Queries other than Conversations.List and Messages.List do not honour Filter.Labels yet:
// each rejects it in the service, before any store read, never ignoring it.
func TestListRejectsLabelFilter(t *testing.T) {
	s := newSvc(t)
	f := model.Filter{Labels: &model.LabelSelector{Any: []model.LabelRef{{Tenant: tenantA, ID: inboxOne}}}}
	req := listReq(f, 1, "")
	cases := map[string]func() error{
		"channels":   func() error { _, err := s.C.Configuration().Channels(s.Ctx, req); return err },
		"inboxes":    func() error { _, err := s.C.Configuration().Inboxes(s.Ctx, req); return err },
		"work items": func() error { _, err := s.C.WorkItems().List(s.Ctx, req); return err },
		"contacts":   func() error { _, err := s.C.Contacts().List(s.Ctx, req); return err },
		"identities": func() error { _, err := s.C.Contacts().Identities(s.Ctx, req); return err },
		"labels":     func() error { _, err := s.C.Labels().List(s.Ctx, req); return err },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			requireCode(t, fn(), model.Validation, "filter.labels: not supported by this query")
		})
	}
}

// configure creates a label named name and returns it as stored.
func (s *svc) configure(op model.OperationID, name string) model.Label {
	s.T.Helper()
	a, err := s.C.Labels().Configure(s.Ctx, model.ConfigureLabel{
		Command: cmd(op, ""), Label: model.Label{Ref: model.LabelRef{Tenant: tenantA}, Name: name},
	})
	if err != nil {
		s.T.Fatalf("Configure %q: %v", name, err)
	}
	ref, err := a.Operation.Result.AsLabel()
	if err != nil {
		s.T.Fatalf("result %+v: %v", a.Operation.Result, err)
	}
	return s.getLabel(ref)
}

func (s *svc) getLabel(ref model.LabelRef) model.Label {
	s.T.Helper()
	l, err := s.C.Labels().Get(s.Ctx, model.Lookup[model.LabelRef]{Call: call(), Ref: ref})
	if err != nil {
		s.T.Fatalf("Labels.Get: %v", err)
	}
	return l
}

// A label is created under the tenant's creation target and is tenant
// vocabulary: the same definition labels conversations of every inbox.
// Creating replays by operation ID; a different payload conflicts.
func TestCreateLabelReusableAcrossInboxes(t *testing.T) {
	s := newSvc(t)
	req := model.ConfigureLabel{
		Command: cmd("op-create", ""),
		Label:   model.Label{Ref: model.LabelRef{Tenant: tenantA}, Name: "Urgent", Description: "act today", Color: "#ff0000"},
	}
	a, err := s.C.Labels().Configure(s.Ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := a.Operation.Result.AsLabel()
	if err != nil || a.Operation.State != model.Committed || a.Replayed || ref.ID == "" {
		t.Fatalf("acceptance %+v, %v", a, err)
	}
	got := s.getLabel(ref)
	if got.Name != "Urgent" || got.Description != "act today" || got.Color != "#ff0000" || got.Revision == "" {
		t.Fatalf("label = %+v", got)
	}
	creation := model.CreationTarget(tenantA, model.KindLabel)
	found, err := s.C.Operations().Lookup(s.Ctx, model.OperationLookup{
		Call: call(), Kind: model.ActionLabelConfigure, Target: creation, ID: "op-create",
	})
	if err != nil || found.Ref != a.Operation.Ref {
		t.Fatalf("Lookup by creation target = %+v, %v", found, err)
	}
	if again, err := s.C.Labels().Configure(s.Ctx, req); err != nil || !again.Replayed || again.Operation.Ref != a.Operation.Ref {
		t.Fatalf("replay %+v, %v", again, err)
	}
	other := req
	other.Label.Name = "Later"
	_, err = s.C.Labels().Configure(s.Ctx, other)
	requireCode(t, err, model.Conflict, "different payload")

	two := s.conversation(s.inbox2.Ref, model.ConversationOpen)
	for _, conv := range []model.ConversationRef{s.Conv.Ref, two.Ref} {
		if _, err := s.C.Conversations().AddLabels(s.Ctx, model.ConversationLabels{
			Command: cmd(model.OperationID("op-add-"+conv.ID), ""), Conversation: conv, Labels: []model.LabelRef{ref},
		}); err != nil {
			t.Fatalf("AddLabels %s: %v", conv.ID, err)
		}
		c, err := s.C.Conversations().Get(s.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: conv})
		if err != nil || len(c.Labels) != 1 || c.Labels[0] != ref {
			t.Fatalf("conversation %s labels = %+v, %v", conv.ID, c.Labels, err)
		}
	}
}

// Configure replaces at ExpectedRevision; a stale revision, a folded name
// clash and a foreign tenant are refused. Renaming keeps assignments.
func TestConfigureLabelUpdate(t *testing.T) {
	s := newSvc(t)
	urgent := s.configure("op-1", "Urgent")
	s.configure("op-2", "Later")
	if _, err := s.C.Conversations().AddLabels(s.Ctx, model.ConversationLabels{
		Command: cmd("op-add", ""), Conversation: s.Conv.Ref, Labels: []model.LabelRef{urgent.Ref},
	}); err != nil {
		t.Fatal(err)
	}
	upd := func(op model.OperationID, rev model.Revision, name string) error {
		_, err := s.C.Labels().Configure(s.Ctx, model.ConfigureLabel{
			Command: cmd(op, rev), Label: model.Label{Ref: urgent.Ref, Name: name},
		})
		return err
	}
	requireCode(t, upd("op-3", "stale", "Now"), model.Conflict, "")
	requireCode(t, upd("op-4", urgent.Revision, "LATER"), model.Conflict, "")
	if err := upd("op-5", urgent.Revision, "Now"); err != nil {
		t.Fatal(err)
	}
	if got := s.getLabel(urgent.Ref); got.Name != "Now" || got.Revision == urgent.Revision {
		t.Fatalf("renamed = %+v", got)
	}
	c, err := s.C.Conversations().Get(s.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: s.Conv.Ref})
	if err != nil || len(c.Labels) != 1 || c.Labels[0] != urgent.Ref {
		t.Fatalf("labels after rename = %+v, %v", c.Labels, err)
	}
	_, err = s.C.Labels().Configure(s.Ctx, model.ConfigureLabel{
		Command: cmd("op-6", ""), Label: model.Label{Ref: model.LabelRef{Tenant: tenantB}, Name: "Foreign"},
	})
	requireCode(t, err, model.NotFound, "")
	_, err = s.C.Labels().Configure(s.Ctx, model.ConfigureLabel{
		Command: cmd("op-7", "r1"), Label: model.Label{Ref: model.LabelRef{Tenant: tenantA, ID: s.id()}, Name: "Ghost"},
	})
	requireCode(t, err, model.NotFound, "")
}

// Configure and Delete are authorized as label.configure: on the creation
// target when creating, on the label otherwise. A denial stores nothing.
func TestConfigureLabelAuthorization(t *testing.T) {
	s := newSvc(t)
	s.pol.set(func(a policy.Authorization) bool { return a.Action != model.ActionLabelConfigure })
	_, err := s.C.Labels().Configure(s.Ctx, model.ConfigureLabel{
		Command: cmd("op-1", ""), Label: model.Label{Ref: model.LabelRef{Tenant: tenantA}, Name: "Urgent"},
	})
	requireCode(t, err, model.Unauthorized, "")
	checks := s.pol.seen()
	if len(checks) != 1 || checks[0].Resource != model.CreationTarget(tenantA, model.KindLabel) {
		t.Fatalf("checks = %+v, want label.configure on the creation target", checks)
	}
	var stored model.Page[model.Label]
	if err := s.E.Store().View(s.Ctx, tenantA, func(r store.Reader) (err error) {
		stored, err = r.Labels(s.Ctx, model.ListRequest{Page: model.PageRequest{Limit: 10}})
		return err
	}); err != nil || len(stored.Items) != 0 {
		t.Fatalf("labels after denial = %+v, %v", stored.Items, err)
	}
	s.pol.set(nil)
	l := s.configure("op-2", "Urgent")
	s.pol.set(func(a policy.Authorization) bool { return a.Action != model.ActionLabelConfigure })
	_, err = s.C.Labels().Configure(s.Ctx, model.ConfigureLabel{Command: cmd("op-3", l.Revision), Label: model.Label{Ref: l.Ref, Name: "Now"}})
	requireCode(t, err, model.Unauthorized, "")
	_, err = s.C.Labels().Delete(s.Ctx, model.DeleteLabel{Command: cmd("op-4", l.Revision), Label: l.Ref})
	requireCode(t, err, model.Unauthorized, "")
	for _, c := range s.pol.seen() {
		if c.Action != model.ActionLabelConfigure || c.Resource != l.Ref.Ref() || c.Revision != l.Revision {
			t.Fatalf("check %+v, want label.configure on the label at its revision", c)
		}
	}
}

// Label reads are authorized as label.read per item: Get and ByName
// refuse, List skips without trace. ByName matches under case folding.
func TestLabelReadsAuthorizedPerItem(t *testing.T) {
	s := newSvc(t)
	urgent := s.configure("op-1", "Urgent")
	later := s.configure("op-2", "Later")
	byName := func(key string) (model.Label, error) {
		return s.C.Labels().ByName(s.Ctx, model.LookupKey{Call: call(), Key: key})
	}
	if got, err := byName("URGENT"); err != nil || got.Ref != urgent.Ref {
		t.Fatalf("ByName folded = %+v, %v", got, err)
	}
	_, err := byName("missing")
	requireCode(t, err, model.NotFound, "")
	_, err = s.C.Labels().Get(s.Ctx, model.Lookup[model.LabelRef]{Call: call(), Ref: model.LabelRef{Tenant: tenantB, ID: urgent.Ref.ID}})
	requireCode(t, err, model.NotFound, "")
	all := drain(t, func(r model.ListRequest) (model.Page[model.Label], error) { return s.C.Labels().List(s.Ctx, r) }, model.Filter{}, 1)
	if len(all) != 2 || all[0].Ref != urgent.Ref || all[1].Ref != later.Ref {
		t.Fatalf("List = %+v, want both in recording order", all)
	}

	s.pol.set(func(a policy.Authorization) bool {
		return a.Action != model.ActionLabelRead || a.Resource != urgent.Ref.Ref()
	})
	_, err = s.C.Labels().Get(s.Ctx, model.Lookup[model.LabelRef]{Call: call(), Ref: urgent.Ref})
	requireCode(t, err, model.Unauthorized, "")
	_, err = byName("urgent")
	requireCode(t, err, model.Unauthorized, "")
	seen := drain(t, func(r model.ListRequest) (model.Page[model.Label], error) { return s.C.Labels().List(s.Ctx, r) }, model.Filter{}, 10)
	if len(seen) != 1 || seen[0].Ref != later.Ref {
		t.Fatalf("List under denial = %+v, want later only", seen)
	}
	for _, c := range s.pol.seen() {
		if c.Action != model.ActionLabelRead || c.Resource.Kind != model.KindLabel || c.Revision == "" {
			t.Fatalf("check %+v, want label.read on a label at its revision", c)
		}
	}
}

// A label any conversation holds cannot be deleted; once removed it can,
// at its revision.
func TestDeleteAssignedLabelConflict(t *testing.T) {
	s := newSvc(t)
	l := s.configure("op-1", "Urgent")
	labels := model.ConversationLabels{Command: cmd("op-add", ""), Conversation: s.Conv.Ref, Labels: []model.LabelRef{l.Ref}}
	if _, err := s.C.Conversations().AddLabels(s.Ctx, labels); err != nil {
		t.Fatal(err)
	}
	del := func(op model.OperationID, rev model.Revision) (model.Acceptance, error) {
		return s.C.Labels().Delete(s.Ctx, model.DeleteLabel{Command: cmd(op, rev), Label: l.Ref})
	}
	_, err := del("op-2", l.Revision)
	requireCode(t, err, model.Conflict, "label is assigned; remove it first")
	labels.Command = cmd("op-remove", "")
	if _, err := s.C.Conversations().RemoveLabels(s.Ctx, labels); err != nil {
		t.Fatal(err)
	}
	_, err = del("op-3", "stale")
	requireCode(t, err, model.Conflict, "")
	a, err := del("op-4", l.Revision)
	if err != nil || a.Operation.State != model.Committed || *a.Operation.Result != l.Ref.Ref() {
		t.Fatalf("Delete = %+v, %v", a, err)
	}
	_, err = s.C.Labels().Get(s.Ctx, model.Lookup[model.LabelRef]{Call: call(), Ref: l.Ref})
	requireCode(t, err, model.NotFound, "")
	if again, err := del("op-4", l.Revision); err != nil || !again.Replayed {
		t.Fatalf("delete replay = %+v, %v", again, err)
	}
	_, err = del("op-5", l.Revision)
	requireCode(t, err, model.NotFound, "")
}
