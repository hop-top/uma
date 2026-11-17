package services_test

import (
	"cmp"
	"encoding/json"
	"slices"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

func (s *svc) addLabels(op model.OperationID, conv model.ConversationRef, rev model.Revision, labels ...model.LabelRef) (model.Acceptance, error) {
	return s.C.Conversations().AddLabels(s.Ctx, model.ConversationLabels{Command: cmd(op, rev), Conversation: conv, Labels: labels})
}

func (s *svc) removeLabels(op model.OperationID, conv model.ConversationRef, rev model.Revision, labels ...model.LabelRef) (model.Acceptance, error) {
	return s.C.Conversations().RemoveLabels(s.Ctx, model.ConversationLabels{Command: cmd(op, rev), Conversation: conv, Labels: labels})
}

func (s *svc) getConv(ref model.ConversationRef) model.Conversation {
	s.T.Helper()
	c, err := s.C.Conversations().Get(s.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: ref})
	if err != nil {
		s.T.Fatalf("Conversations.Get: %v", err)
	}
	return c
}

// labelEvents returns the stored label events with their payloads.
func (s *svc) labelEvents() ([]model.Event, []model.ConversationLabelPayload) {
	s.T.Helper()
	var evs []model.Event
	var out []model.ConversationLabelPayload
	for _, ev := range s.Events() {
		if ev.Kind != model.EventConversationLabelAdded && ev.Kind != model.EventConversationLabelRemoved {
			continue
		}
		var p model.ConversationLabelPayload
		if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
			s.T.Fatalf("payload: %v", err)
		}
		evs, out = append(evs, ev), append(out, p)
	}
	return evs, out
}

// Adding a label the conversation already holds commits with no event and
// the revision unchanged.
func TestAddLabelIdempotent(t *testing.T) {
	s := newSvc(t)
	l := s.configure("op-l", "Urgent")
	if _, err := s.addLabels("op-1", s.Conv.Ref, "", l.Ref); err != nil {
		t.Fatal(err)
	}
	after := s.getConv(s.Conv.Ref)
	a, err := s.addLabels("op-2", s.Conv.Ref, after.Revision, l.Ref)
	if err != nil || a.Operation.State != model.Committed || a.Replayed {
		t.Fatalf("second add = %+v, %v", a, err)
	}
	if again := s.getConv(s.Conv.Ref); again.Revision != after.Revision || !slices.Equal(again.Labels, []model.LabelRef{l.Ref}) {
		t.Fatalf("after no-op add = %+v, want revision %s", again, after.Revision)
	}
	if evs, _ := s.labelEvents(); len(evs) != 1 {
		t.Fatalf("events = %+v, want only the first add's", evs)
	}
}

// Removing a label the conversation does not hold is a committed no-op.
func TestRemoveAbsentLabelNoop(t *testing.T) {
	s := newSvc(t)
	l := s.configure("op-l", "Urgent")
	a, err := s.removeLabels("op-1", s.Conv.Ref, "", l.Ref)
	if err != nil || a.Operation.State != model.Committed {
		t.Fatalf("remove = %+v, %v", a, err)
	}
	if got := s.getConv(s.Conv.Ref); got.Revision != s.Conv.Revision || len(got.Labels) != 0 {
		t.Fatalf("after no-op remove = %+v", got)
	}
	if evs, _ := s.labelEvents(); len(evs) != 0 {
		t.Fatalf("events = %+v, want none", evs)
	}
}

// One call moves the revision once and emits one event per label it
// actually changed, all at the new revision, carrying the previous
// revision, the trigger and the operation, under the conversation's
// internal audience.
func TestAddLabelEmitsOneEventPerLabel(t *testing.T) {
	s := newSvc(t)
	a1 := s.configure("op-a", "A")
	b1 := s.configure("op-b", "B")
	c1 := s.configure("op-c", "C")
	if _, err := s.addLabels("op-1", s.Conv.Ref, "", b1.Ref); err != nil {
		t.Fatal(err)
	}
	mid := s.getConv(s.Conv.Ref)
	acc, err := s.addLabels("op-2", s.Conv.Ref, "", c1.Ref, b1.Ref, a1.Ref)
	if err != nil {
		t.Fatal(err)
	}
	cur := s.getConv(s.Conv.Ref)
	want := []model.LabelRef{a1.Ref, b1.Ref, c1.Ref}
	slices.SortFunc(want, func(x, y model.LabelRef) int { return cmp.Compare(x.ID, y.ID) })
	if !slices.Equal(cur.Labels, want) || cur.Revision == mid.Revision {
		t.Fatalf("labels = %+v at %s", cur.Labels, cur.Revision)
	}
	evs, ps := s.labelEvents()
	evs, ps = evs[1:], ps[1:]
	if len(evs) != 2 {
		t.Fatalf("events = %+v, want one per added label", evs)
	}
	var added []model.LabelRef
	for i, ev := range evs {
		p := ps[i]
		if ev.Kind != model.EventConversationLabelAdded || ev.Payload.Name != model.SchemaConversationLabelAdded ||
			ev.Revision != cur.Revision || ev.Resource != s.Conv.Ref.Ref() || ev.Inbox != s.Conv.Ref.Inbox.ID ||
			ev.Audience.Visibility != model.Internal || ev.Correlation != "corr-svc" {
			t.Fatalf("event %+v", ev)
		}
		if p.Conversation != s.Conv.Ref || p.Trigger != model.TriggerCommand || p.PreviousRevision != mid.Revision ||
			p.Operation == nil || *p.Operation != acc.Operation.Ref {
			t.Fatalf("payload %+v", p)
		}
		added = append(added, p.Label)
	}
	if !slices.Contains(added, a1.Ref) || !slices.Contains(added, c1.Ref) {
		t.Fatalf("added = %+v, want a and c", added)
	}

	rm, err := s.removeLabels("op-3", s.Conv.Ref, cur.Revision, a1.Ref, c1.Ref)
	if err != nil {
		t.Fatal(err)
	}
	last := s.getConv(s.Conv.Ref)
	if !slices.Equal(last.Labels, []model.LabelRef{b1.Ref}) {
		t.Fatalf("after remove = %+v", last.Labels)
	}
	evs, ps = s.labelEvents()
	evs, ps = evs[3:], ps[3:]
	if len(evs) != 2 || evs[0].Kind != model.EventConversationLabelRemoved || evs[1].Kind != model.EventConversationLabelRemoved ||
		ps[0].PreviousRevision != cur.Revision || evs[0].Revision != last.Revision || *ps[1].Operation != rm.Operation.Ref {
		t.Fatalf("remove events = %+v %+v", evs, ps)
	}
}

// AddLabels and RemoveLabels are authorized as conversation.label on the
// conversation at its revision; they need no label.read on the labels.
func TestAddLabelDenied(t *testing.T) {
	s := newSvc(t)
	l := s.configure("op-l", "Urgent")
	s.pol.set(func(a policy.Authorization) bool { return a.Action != model.ActionConversationLabel })
	_, err := s.addLabels("op-1", s.Conv.Ref, "", l.Ref)
	requireCode(t, err, model.Unauthorized, "")
	_, err = s.removeLabels("op-2", s.Conv.Ref, "", l.Ref)
	requireCode(t, err, model.Unauthorized, "")
	for _, c := range s.pol.seen() {
		if c.Action != model.ActionConversationLabel || c.Resource != s.Conv.Ref.Ref() || c.Revision != s.Conv.Revision {
			t.Fatalf("check %+v, want conversation.label on the conversation at its revision", c)
		}
	}
	if got := s.getConv(s.Conv.Ref); len(got.Labels) != 0 || got.Revision != s.Conv.Revision {
		t.Fatalf("denied add changed the conversation: %+v", got)
	}
	s.pol.set(func(a policy.Authorization) bool { return a.Action != model.ActionLabelRead })
	if _, err := s.addLabels("op-3", s.Conv.Ref, "", l.Ref); err != nil {
		t.Fatalf("add without label.read: %v", err)
	}
}

// Every named label must exist in the tenant: one missing label fails the
// whole call with not found, nothing applied and no operation claimed. A
// conversation of another tenant is not found too.
func TestAddForeignLabelNotFound(t *testing.T) {
	s := newSvc(t)
	l := s.configure("op-l", "Urgent")
	ghost := model.LabelRef{Tenant: tenantA, ID: s.id()}
	for name, fn := range map[string]func(model.OperationID) error{
		"add": func(op model.OperationID) error { _, err := s.addLabels(op, s.Conv.Ref, "", l.Ref, ghost); return err },
		"remove": func(op model.OperationID) error {
			_, err := s.removeLabels(op, s.Conv.Ref, "", l.Ref, ghost)
			return err
		},
	} {
		op := model.OperationID("op-" + name)
		requireCode(t, fn(op), model.NotFound, "")
		_, err := s.C.Operations().Lookup(s.Ctx, model.OperationLookup{
			Call: call(), Kind: model.ActionConversationLabel, Target: s.Conv.Ref.Ref(), ID: op,
		})
		requireCode(t, err, model.NotFound, "")
	}
	if got := s.getConv(s.Conv.Ref); len(got.Labels) != 0 || got.Revision != s.Conv.Revision {
		t.Fatalf("missing label applied: %+v", got)
	}
	if evs, _ := s.labelEvents(); len(evs) != 0 {
		t.Fatalf("events = %+v", evs)
	}
	foreign := model.ConversationRef{Inbox: model.InboxRef{Tenant: tenantB, ID: inboxOne}, ID: s.Conv.Ref.ID}
	_, err := s.addLabels("op-f", foreign, "", model.LabelRef{Tenant: tenantB, ID: l.Ref.ID})
	requireCode(t, err, model.NotFound, "")
	_, err = s.addLabels("op-x", s.Conv.Ref, "", model.LabelRef{Tenant: tenantB, ID: l.Ref.ID})
	requireCode(t, err, model.Validation, "labels[0]")
}

// The ledger keys AddLabels and RemoveLabels alike (conversation.label on
// the conversation): the same operation ID and payload replays, another
// payload or direction conflicts. A set ExpectedRevision must be current,
// and a conversation never holds more than the cap.
func TestConversationLabelsLedger(t *testing.T) {
	s := newSvc(t)
	l := s.configure("op-l", "Urgent")
	a, err := s.addLabels("op-1", s.Conv.Ref, s.Conv.Revision, l.Ref)
	if err != nil {
		t.Fatal(err)
	}
	found, err := s.C.Operations().Lookup(s.Ctx, model.OperationLookup{
		Call: call(), Kind: model.ActionConversationLabel, Target: s.Conv.Ref.Ref(), ID: "op-1",
	})
	if err != nil || found.Ref != a.Operation.Ref || *found.Result != s.Conv.Ref.Ref() {
		t.Fatalf("Lookup = %+v, %v", found, err)
	}
	if again, err := s.addLabels("op-1", s.Conv.Ref, s.Conv.Revision, l.Ref); err != nil || !again.Replayed || again.Operation.Ref != a.Operation.Ref {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	_, err = s.removeLabels("op-1", s.Conv.Ref, s.Conv.Revision, l.Ref)
	requireCode(t, err, model.Conflict, "different payload")
	_, err = s.removeLabels("op-2", s.Conv.Ref, s.Conv.Revision, l.Ref)
	requireCode(t, err, model.Conflict, "revision")

	full := make([]model.LabelRef, 0, model.MaxConversationLabels)
	s.put(func(tx store.Tx) error {
		for range model.MaxConversationLabels {
			ref := model.LabelRef{Tenant: tenantA, ID: s.id()}
			if err := tx.PutLabel(s.Ctx, model.Label{Ref: ref, Name: string(ref.ID), Revision: "r1"}, ""); err != nil {
				return err
			}
			full = append(full, ref)
		}
		return nil
	})
	two := s.conversation(s.inbox2.Ref, model.ConversationOpen)
	if _, err := s.addLabels("op-3", two.Ref, "", full...); err != nil {
		t.Fatalf("add to the cap: %v", err)
	}
	_, err = s.addLabels("op-4", two.Ref, "", l.Ref)
	requireCode(t, err, model.Conflict, "labels")
}
