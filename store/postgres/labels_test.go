package postgres_test

import (
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres"
)

// labelFixture is one stored label and one stored conversation, unlabelled
// at revision r1.
type labelFixture struct {
	label model.Label
	conv  model.Conversation
}

func newLabelFixture(t *testing.T, st *postgres.Store) labelFixture {
	t.Helper()
	ctx := t.Context()
	ch := model.ChannelRef{Tenant: tenant, ID: "00000000-0000-7000-8000-00000000c001"}
	in := model.InboxRef{Tenant: tenant, ID: "00000000-0000-7000-8000-00000000b001"}
	fx := labelFixture{
		label: model.Label{Ref: model.LabelRef{Tenant: tenant, ID: "00000000-0000-7000-8000-00000000a001"}, Name: "urgent", Revision: "r1"},
		conv: model.Conversation{
			Ref: model.ConversationRef{Inbox: in, ID: "00000000-0000-7000-8000-00000000d001"}, Mode: model.ModePerContact,
			Status: model.ConversationOpen, CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Revision: "r1",
		},
	}
	if err := st.Transact(ctx, tenant, func(tx store.Tx) error {
		return firstErr(
			tx.PutChannel(ctx, model.Channel{Ref: ch, Type: "x.chat", Revision: "r1"}, ""),
			tx.PutInbox(ctx, model.Inbox{Ref: in, Channel: ch, Revision: "r1"}, ""),
			tx.PutLabel(ctx, fx.label, ""),
			tx.PutConversation(ctx, fx.conv, ""),
		)
	}); err != nil {
		t.Fatal(err)
	}
	return fx
}

// labelled returns the conversation at r2 holding labels.
func (fx labelFixture) labelled(labels ...model.LabelRef) model.Conversation {
	next := fx.conv.Clone()
	next.Labels, next.Revision = labels, "r2"
	return next
}

// TestDeleteLabelRacesAssignment: a label assigned by a unit that commits
// after a delete unit took its snapshot is never deleted under it. The
// delete unit fails as a serialization failure, never as a missing label,
// and its retry sees the assignment and refuses with a conflict.
func TestDeleteLabelRacesAssignment(t *testing.T) {
	ctx := t.Context()
	procs, _ := processes(t, 2)
	fx := newLabelFixture(t, procs[0])
	ref := fx.label.Ref
	deleteAfterAssign := func(tx store.Tx) error {
		// Take the unit's snapshot, then let the assignment commit.
		if _, err := tx.GetLabel(ctx, ref); err != nil {
			return err
		}
		if err := procs[1].Transact(ctx, tenant, func(tx store.Tx) error {
			return tx.PutConversation(ctx, fx.labelled(ref), "r1")
		}); err != nil {
			t.Fatalf("assign: %v", err)
		}
		return tx.DeleteLabel(ctx, ref, "r1")
	}
	if err := procs[0].Transact(ctx, tenant, deleteAfterAssign); !model.IsCode(err, model.Transient) {
		t.Fatalf("delete racing an assignment: %v, want transient", err)
	}
	err := procs[0].Transact(ctx, tenant, func(tx store.Tx) error { return tx.DeleteLabel(ctx, ref, "r1") })
	if !model.IsCode(err, model.Conflict) {
		t.Fatalf("retried delete: %v, want conflict", err)
	}
	if err := procs[0].View(ctx, tenant, func(r store.Reader) error {
		_, err := r.GetLabel(ctx, ref)
		return err
	}); err != nil {
		t.Fatalf("label after the race: %v", err)
	}
}

// TestMissingLabelKeepsUnit: assigning a label that is not stored is
// refused before any statement fails, so the unit stays usable and commits
// its other writes, as with any other missing parent.
func TestMissingLabelKeepsUnit(t *testing.T) {
	ctx := t.Context()
	st, err := postgres.Open(ctx, postgres.NewTestDatabase(t, true), postgres.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fx := newLabelFixture(t, st)
	missing := model.LabelRef{Tenant: tenant, ID: "00000000-0000-7000-8000-00000000a002"}
	if err := st.Transact(ctx, tenant, func(tx store.Tx) error {
		if err := tx.PutConversation(ctx, fx.labelled(fx.label.Ref, missing), "r1"); !model.IsCode(err, model.NotFound) {
			t.Errorf("assigning a missing label = %v, want not_found", err)
		}
		return tx.PutConversation(ctx, fx.labelled(fx.label.Ref), "r1")
	}); err != nil {
		t.Fatalf("unit after the refusal: %v", err)
	}
}
