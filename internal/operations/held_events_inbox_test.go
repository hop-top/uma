package operations_test

import (
	"testing"
	"time"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

func heldRecord(t *testing.T, r *enginetest.Rig, op model.Operation) store.OperationRecord {
	t.Helper()
	var rec store.OperationRecord
	err := r.E.Store().View(r.Ctx, enginetest.TenantA, func(rd store.Reader) error {
		var err error
		rec, err = rd.Operation(r.Ctx, op.Ref)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func requireScope(t *testing.T, ev model.Event, inbox model.ID, target model.Ref, verb string) {
	t.Helper()
	p := heldPayload(t, ev)
	if ev.Inbox != inbox {
		t.Fatalf("%s inbox = %q, want %q", ev.Kind, ev.Inbox, inbox)
	}
	if err := ev.Validate(); err != nil {
		t.Fatalf("%s event invalid: %v", ev.Kind, err)
	}
	if p.Verb != verb || p.Target != target {
		t.Fatalf("%s payload verb/target = %q %+v, want %q %+v", ev.Kind, p.Verb, p.Target, verb, target)
	}
}

func TestHeldEventsCarryTargetInbox(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	conv := r.Conv.Ref.Ref()
	inbox := r.Conv.Ref.Inbox.ID

	held := propose(t, r, c, proposal(r, "p-1", "fp-1")).Operation
	requireScope(t, heldEvents(r, model.EventOperationHeld)[0], inbox, conv, model.ActionMessageReply)

	if _, err := r.C.Operations().Reject(r.Ctx, decide(held)); err != nil {
		t.Fatal(err)
	}
	requireScope(t, heldEvents(r, model.EventOperationDecided)[0], inbox, conv, model.ActionMessageReply)

	rec := heldRecord(t, r, held)
	rec.Operation.State = model.Expired
	ev, err := operations.ExpiredEvent("00000000-0000-7000-8000-0000000000e2", rec, r.Clock.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	requireScope(t, ev, inbox, conv, model.ActionMessageReply)
}

func TestHeldEventsOfInboxlessTargetKeepEmptyInbox(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	target := r.Conv.Ref.Inbox.Ref()
	s := proposal(r, "p-1", "fp-1")
	s.Target, s.Owner = target, target
	held := propose(t, r, c, s).Operation
	requireScope(t, heldEvents(r, model.EventOperationHeld)[0], "", target, model.ActionMessageReply)

	if _, err := r.C.Operations().Reject(r.Ctx, decide(held)); err != nil {
		t.Fatal(err)
	}
	requireScope(t, heldEvents(r, model.EventOperationDecided)[0], "", target, model.ActionMessageReply)
}

func TestEventInboxMayDifferFromOperationResource(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	propose(t, r, c, proposal(r, "p-1", "fp-1"))
	ev := heldEvents(r, model.EventOperationHeld)[0]
	ev.Inbox = ""
	if err := ev.Validate(); err != nil {
		t.Fatalf("empty inbox: %v", err)
	}
	ev.Inbox = "not an id!"
	if err := ev.Validate(); err == nil {
		t.Fatal("malformed inbox accepted")
	}
	conv := model.Event{Tenant: ev.Tenant, Resource: r.Conv.Ref.Ref(), Inbox: inboxTwo}
	conv.ID, conv.Kind, conv.Payload = ev.ID, "x.y", ev.Payload
	if err := conv.Validate(); err == nil {
		t.Fatal("conversation event with another inbox accepted")
	}
}
