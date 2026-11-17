package runtime_test

import (
	"slices"
	"testing"
	"time"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Every mutation targets the message's stored provider message on the
// conversation's route.
func TestMutationTargetsStoredProviderMessage(t *testing.T) {
	r := newRig(t, enginetest.WithMutations)
	m := r.SeedMessage("original")
	r.Edit("op-edit", m, "corrected")
	r.React("op-react", m, "👍", false)
	r.Step("w")
	m = r.Message(m.Ref)
	r.Delete("op-delete", m)
	r.Step("w")
	calls := r.F.MutationCalls()
	want := model.ProviderMessageRef(r.Ch.Ref, enginetest.Target)
	if len(calls) != 3 {
		t.Fatalf("mutation calls = %+v, want three", calls)
	}
	for _, c := range calls {
		if c.Effect.Target == nil || *c.Effect.Target != want || c.Effect.Route != r.Conv.ReplyRoute || c.Effect.Audience != m.Audience {
			t.Fatalf("%s effect = %+v, want the stored provider message on the route", c.Capability, c.Effect)
		}
	}
	if calls[0].Content.Parts[0].Text != "corrected" || calls[1].Emoji != "👍" || calls[1].Remove {
		t.Fatalf("calls = %+v", calls)
	}
}

// An applied delete tombstones the message: content and reactions
// cleared, the editor record kept, one deleted event.
func TestDeleteTombstonesOnApplied(t *testing.T) {
	r := newRig(t, enginetest.WithMutations)
	m := r.SeedMessage("original")
	edit := &model.Edit{Actor: "editor-0", At: r.Clock.Now().Add(-time.Hour).UTC()}
	m = r.Mutate(m.Ref, func(x *model.Message) {
		x.LastEdit = edit
		x.AddReaction(model.MessageReaction{Emoji: "👍", Actor: "someone", Count: 1})
		x.ReactionsComplete = true
	})
	a := r.Delete("op-delete", m)
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	got := r.Message(m.Ref)
	if !got.Deleted || got.Redacted || len(got.Content.Parts) != 0 || got.Reactions != nil || got.ReactionsComplete ||
		got.LastEdit == nil || *got.LastEdit != *edit || got.Revision == m.Revision {
		t.Fatalf("message = %+v, want a tombstone", got)
	}
	u := updates(t, r, a.Operation.Ref)
	if len(u) != 1 || u[0].Change != model.MessageChangeDeleted || u[0].Trigger != model.TriggerCommand || u[0].PreviousRevision != m.Revision {
		t.Fatalf("message.updated = %+v, want one deleted", u)
	}
}

// A delete settles as a tombstone even when a newer edit landed in
// flight: no edit guard applies to a delete.
func TestDeleteAfterNewerEditStillTombstones(t *testing.T) {
	r := newRig(t, enginetest.WithMutations)
	m := r.SeedMessage("original")
	a := r.Delete("op-delete", m)
	r.F.Respond(during(func() {
		r.Mutate(m.Ref, func(x *model.Message) {
			x.Content.Parts[0].Text = "newer"
			x.LastEdit = &model.Edit{Actor: "sink", At: r.Clock.Now().UTC()}
		})
	}, enginetest.Accepted))
	r.Step("w")
	if got := r.Message(m.Ref); !got.Deleted || len(got.Content.Parts) != 0 {
		t.Fatalf("message = %+v, want a tombstone", got)
	}
	if u := updates(t, r, a.Operation.Ref); len(u) != 1 || u[0].Change != model.MessageChangeDeleted {
		t.Fatalf("message.updated = %+v", u)
	}
}

// React adds the caller's entry, with the provider's reaction reference
// on the entry and never on the message, then removes it.
func TestReactAddThenRemoveUpdatesSet(t *testing.T) {
	r := newRig(t, enginetest.WithMutations)
	m := r.SeedMessage("original")
	add := r.React("op-add", m, "👍", false)
	r.Step("w")
	op := r.Op(add.Operation.Ref)
	requireState(t, op, model.ProviderAccepted, "")
	got := r.Message(m.Ref)
	if len(got.Reactions) != 1 {
		t.Fatalf("reactions = %+v, want one", got.Reactions)
	}
	e := got.Reactions[0]
	if e.Emoji != "👍" || e.Actor != enginetest.Actor || e.By != nil || e.Count != 1 || !slices.Equal(e.External, op.ProviderReferences) ||
		len(e.External) != 1 || !e.At.Equal(r.Clock.Now().UTC()) {
		t.Fatalf("entry = %+v, want the caller's reaction carrying the provider reference %v", e, op.ProviderReferences)
	}
	if !slices.Equal(got.External, m.External) || got.ReactionsComplete || text(got) != "original" || got.LastEdit != nil {
		t.Fatalf("message = %+v, want only the reaction set changed", got)
	}
	u := updates(t, r, add.Operation.Ref)
	if len(u) != 1 || u[0].Change != model.MessageChangeReactionAdded || u[0].Reaction == nil || u[0].Reaction.Emoji != "👍" ||
		u[0].Reaction.By != nil || u[0].PreviousRevision != m.Revision || u[0].event.Revision != got.Revision {
		t.Fatalf("message.updated = %+v, want reaction_added without by", u)
	}

	rm := r.React("op-remove", got, "👍", true)
	r.Step("w")
	after := r.Message(m.Ref)
	if len(after.Reactions) != 0 || after.Revision == got.Revision {
		t.Fatalf("message = %+v, want the reaction removed", after)
	}
	if u := updates(t, r, rm.Operation.Ref); len(u) != 1 || u[0].Change != model.MessageChangeReactionRemoved || u[0].Reaction.Emoji != "👍" {
		t.Fatalf("message.updated = %+v, want reaction_removed", u)
	}
	if calls := r.F.MutationCalls(); len(calls) != 2 || !calls[1].Remove {
		t.Fatalf("calls = %+v", calls)
	}
}

// Settling a reaction again, or applying the same reaction under another
// operation, changes nothing and emits nothing.
func TestReactSettleIsIdempotent(t *testing.T) {
	r := newRig(t, enginetest.WithMutations)
	m := r.SeedMessage("original")
	a := r.React("op-add", m, "👍", false)
	r.Step("w")
	once := r.Message(m.Ref)
	rec := opRecord(t, r, a.Operation.Ref)
	in, err := operations.DecodeReactIntent(rec.Command)
	if err != nil {
		t.Fatal(err)
	}
	err = r.E.Store().Transact(r.Ctx, tenantA, func(tx store.Tx) error {
		return operations.SettleReact(r.Ctx, tx, r.E, rec.Operation, in)
	})
	if err != nil {
		t.Fatal(err)
	}
	again := r.React("op-again", once, "👍", false)
	r.Step("w")
	if got := r.Message(m.Ref); got.Revision != once.Revision || len(got.Reactions) != 1 {
		t.Fatalf("message = %+v, want it unchanged", got)
	}
	if u := updates(t, r, a.Operation.Ref); len(u) != 1 {
		t.Fatalf("message.updated = %d events, want one", len(u))
	}
	if u := updates(t, r, again.Operation.Ref); len(u) != 0 {
		t.Fatalf("repeated reaction emitted %+v", u)
	}
}

// With single-reaction cardinality a reactor's new reaction replaces
// their earlier one.
func TestSingleReactionReplacesActorsReaction(t *testing.T) {
	r := newRig(t, enginetest.WithMutations, enginetest.WithSingleReactions)
	m := r.SeedMessage("original")
	other := model.MessageReaction{Emoji: "👍", Actor: "someone-else", Count: 1}
	m = r.Mutate(m.Ref, func(x *model.Message) { x.AddReaction(other) })
	r.React("op-1", m, "👍", false)
	r.Step("w")
	b := r.React("op-2", r.Message(m.Ref), "🎉", false)
	r.Step("w")
	got := r.Message(m.Ref)
	var mine []string
	for _, x := range got.Reactions {
		if x.Actor == enginetest.Actor {
			mine = append(mine, x.Emoji)
		}
	}
	if !slices.Equal(mine, []string{"🎉"}) || !slices.ContainsFunc(got.Reactions, func(x model.MessageReaction) bool { return x.Actor == other.Actor }) {
		t.Fatalf("reactions = %+v, want the caller's 👍 replaced by 🎉 and others kept", got.Reactions)
	}
	var changes []string
	for _, u := range updates(t, r, b.Operation.Ref) {
		changes = append(changes, u.Change+" "+u.Reaction.Emoji)
	}
	if !slices.Equal(changes, []string{"reaction_removed 👍", "reaction_added 🎉"}) {
		t.Fatalf("changes = %v", changes)
	}
	rm := r.React("op-3", got, "🎉", true)
	r.Step("w")
	if got := r.Message(m.Ref); len(got.Reactions) != 1 || got.Reactions[0].Actor != other.Actor {
		t.Fatalf("reactions after removal = %+v", got.Reactions)
	}
	requireState(t, r.Op(rm.Operation.Ref), model.ProviderAccepted, "")
}

func opRecord(t *testing.T, r *rig, ref model.OperationRef) store.OperationRecord {
	t.Helper()
	var rec store.OperationRecord
	err := r.E.Store().View(r.Ctx, tenantA, func(rd store.Reader) (err error) {
		rec, err = rd.Operation(r.Ctx, ref)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}
