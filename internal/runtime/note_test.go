package runtime_test

import (
	"testing"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

func noteReq(r *rig, op model.OperationID) model.InternalNote {
	return model.InternalNote{
		Command:      model.Command{Call: model.Call{Correlation: "c-note"}, Operation: op},
		Conversation: r.Conv.Ref, PolicyRef: "staff",
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "check the refund"}}},
	}
}

// optIn sets the rig inbox's DeliverNotes through the Configuration
// service.
func optIn(t *testing.T, r *rig, deliver bool) error {
	t.Helper()
	var in model.Inbox
	if err := r.E.Store().View(r.Ctx, tenantA, func(rd store.Reader) (err error) {
		in, err = rd.GetInbox(r.Ctx, r.Conv.Ref.Inbox)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	in.DeliverNotes = deliver
	if in.Selection.Mode == "" {
		in.Selection = model.Selection{Mode: model.ModePerRoom}
	}
	_, err := r.C.Configuration().ConfigureInbox(r.Ctx, model.ConfigureInbox{
		Command: model.Command{Call: model.Call{Correlation: "c-cfg"}, Operation: model.OperationID("cfg-" + string(in.Revision)), ExpectedRevision: in.Revision},
		Inbox:   in,
	})
	return err
}

// Without the inbox's opt-in a note stays in uma: committed locally, no
// provider call, even when the adapter could deliver it.
func TestNoteStaysLocalWithoutOptIn(t *testing.T) {
	r := newRig(t, enginetest.WithNotes)
	a, err := r.C.Messages().AddInternalNote(r.Ctx, noteReq(r, "op-note"))
	if err != nil {
		t.Fatal(err)
	}
	requireState(t, a.Operation, model.Committed, "")
	if n := r.Step("w"); n != 0 || len(r.F.NotesSent()) != 0 {
		t.Fatalf("work = %d, notes sent = %d; want none", n, len(r.F.NotesSent()))
	}
}

// An inbox that opted in delivers its notes through the adapter's note
// sender, under the note's internal audience, as an outbound operation;
// the provider's message ID joins the note so its echo is recognized.
func TestOptedInNoteIsDelivered(t *testing.T) {
	r := newRig(t, enginetest.WithNotes)
	if err := optIn(t, r, true); err != nil {
		t.Fatal(err)
	}
	a, err := r.C.Messages().AddInternalNote(r.Ctx, noteReq(r, "op-note"))
	if err != nil {
		t.Fatal(err)
	}
	requireState(t, a.Operation, model.Queued, "")
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	notes := r.F.NotesSent()
	if len(notes) != 1 || notes[0].Audience.Visibility != model.Internal || notes[0].Audience.PolicyRef != "staff" ||
		notes[0].Content.Parts[0].Text != "check the refund" || notes[0].Route != r.Conv.ReplyRoute {
		t.Fatalf("notes sent = %+v, want the note under its internal audience on the route", notes)
	}
	if n := len(r.F.ProviderSends()); n != 0 {
		t.Fatalf("public sends = %d, want none", n)
	}
	var m model.Message
	if err := r.E.Store().View(r.Ctx, tenantA, func(rd store.Reader) (err error) {
		m, err = rd.GetMessage(r.Ctx, *a.Operation.ResultMessage)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := model.ProviderMessageRef(r.Ch.Ref, "note-"+string(notes[0].Attempt))
	if m.Audience.Visibility != model.Internal || len(m.External) != 1 || m.External[0] != want {
		t.Fatalf("note = %+v, want internal with the provider message ID %v", m, want)
	}
}

// Only an adapter declaring internal_note takes delivered notes: opting in
// an inbox on any other channel is unsupported.
func TestOptInNeedsInternalNoteCapability(t *testing.T) {
	r := newRig(t)
	requireCode(t, optIn(t, r, true), model.Unsupported, "internal_note")
}
