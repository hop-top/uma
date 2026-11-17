package conformance

import (
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// readMarkerCases pin Reader.ReadMarker and Tx.PutReadMarker: one record
// per conversation and actor, compare-and-set on its own revision, exact
// read-through instant, never touching the conversation.
var readMarkerCases = []storeCase{
	{"RoundTripPerActor", func(t *testing.T, e *env) {
		conv := e.putWorkflowConversation(t, nil, nil)
		a := model.ReadMarker{Conversation: conv, Actor: "desk-1", ReadThrough: time.Unix(1_700_000_100, 123456789).UTC(), UpdatedAt: e.at(5), Revision: "m1"}
		b := model.ReadMarker{Conversation: conv, Actor: "desk-2", ReadThrough: e.at(1), UpdatedAt: e.at(6), Revision: "m1"}
		e.mustTx(t, func(tx store.Tx) error {
			return firstErr(tx.PutReadMarker(e.ctx, a, ""), tx.PutReadMarker(e.ctx, b, ""))
		})
		for _, want := range []model.ReadMarker{a, b} {
			got := e.readMarker(t, conv, want.Actor)
			if got != want {
				t.Fatalf("ReadMarker(%s) = %+v, want %+v", want.Actor, got, want)
			}
		}
		next := a
		next.ReadThrough, next.Revision = e.at(200), "m2"
		e.mustTx(t, func(tx store.Tx) error { return tx.PutReadMarker(e.ctx, next, "m1") })
		if got := e.readMarker(t, conv, "desk-1"); got != next {
			t.Fatalf("after update %+v, want %+v", got, next)
		}
		if got := e.readMarker(t, conv, "desk-2"); got != b {
			t.Fatalf("another actor's marker moved: %+v", got)
		}
	}},
	{"CompareAndSet", func(t *testing.T, e *env) {
		conv := e.putWorkflowConversation(t, nil, nil)
		m := model.ReadMarker{Conversation: conv, Actor: "desk-1", ReadThrough: e.at(1), Revision: "m1"}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutReadMarker(e.ctx, m, "") })
		for name, expected := range map[string]model.Revision{"create over existing": "", "stale": "m0"} {
			next := m
			next.Revision = "m2"
			err := e.s.Transact(e.ctx, tenantA, func(tx store.Tx) error { return tx.PutReadMarker(e.ctx, next, expected) })
			if !model.IsCode(err, model.Conflict) {
				t.Fatalf("%s: %v, want conflict", name, err)
			}
		}
		same := m
		err := e.s.Transact(e.ctx, tenantA, func(tx store.Tx) error { return tx.PutReadMarker(e.ctx, same, "m1") })
		wantCode(t, err, model.Validation)
	}},
	{"ConversationUntouched", func(t *testing.T, e *env) {
		conv := e.putWorkflowConversation(t, nil, nil)
		e.mustTx(t, func(tx store.Tx) error {
			return tx.PutReadMarker(e.ctx, model.ReadMarker{Conversation: conv, Actor: "desk-1", ReadThrough: e.at(1), Revision: "m1"}, "")
		})
		var got model.Conversation
		e.mustView(t, func(r store.Reader) (err error) {
			got, err = r.GetConversation(e.ctx, conv)
			return err
		})
		if got.Revision != "r1" {
			t.Fatalf("conversation revision %q after a marker write, want r1", got.Revision)
		}
	}},
	{"AbsentAndForeign", func(t *testing.T, e *env) {
		conv := e.putWorkflowConversation(t, nil, nil)
		read := func(ref model.ConversationRef, actor model.ActorID) error {
			return e.s.View(e.ctx, tenantA, func(r store.Reader) error {
				_, err := r.ReadMarker(e.ctx, ref, actor)
				return err
			})
		}
		wantCode(t, read(conv, "desk-1"), model.NotFound)
		e.mustTx(t, func(tx store.Tx) error {
			return tx.PutReadMarker(e.ctx, model.ReadMarker{Conversation: conv, Actor: "desk-1", ReadThrough: e.at(1), Revision: "m1"}, "")
		})
		moved := conv
		moved.Inbox.ID = e.id(t)
		wantCode(t, read(moved, "desk-1"), model.NotFound)
		foreign := model.ConversationRef{Inbox: model.InboxRef{Tenant: tenantB, ID: conv.Inbox.ID}, ID: conv.ID}
		wantCode(t, read(foreign, "desk-1"), model.NotFound)
		wantCode(t, read(conv, ""), model.Validation)
		missing := model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}
		err := e.s.Transact(e.ctx, tenantA, func(tx store.Tx) error {
			return tx.PutReadMarker(e.ctx, model.ReadMarker{Conversation: missing, Actor: "desk-1", Revision: "m1"}, "")
		})
		wantCode(t, err, model.NotFound)
		err = e.s.Transact(e.ctx, tenantA, func(tx store.Tx) error {
			return tx.PutReadMarker(e.ctx, model.ReadMarker{Conversation: conv, Actor: "desk-2"}, "")
		})
		wantCode(t, err, model.Validation)
	}},
}

func (e *env) readMarker(t *testing.T, conv model.ConversationRef, actor model.ActorID) model.ReadMarker {
	t.Helper()
	var got model.ReadMarker
	e.mustView(t, func(r store.Reader) (err error) {
		got, err = r.ReadMarker(e.ctx, conv, actor)
		return err
	})
	return got
}
