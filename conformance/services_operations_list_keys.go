package conformance

import (
	"reflect"
	"slices"
	"testing"

	"hop.top/uma/model"
)

// opListKeys: every listed operation carries the key its command claimed
// (caller, kind, target, operation ID), as its acceptance, Get and Lookup
// show it, and the key's kind, target and ID recover it with Lookup.
func opListKeys(t *testing.T, w *operationWorld) {
	want := map[model.OperationRef]model.OperationKey{
		w.note1.Operation.Ref:      {Caller: "staff-1", Kind: model.ActionMessageNote, Target: w.convA.Ref.Ref()},
		w.reply.Operation.Ref:      {Caller: "staff-1", Kind: model.ActionMessageReply, Target: w.convA.Ref.Ref()},
		w.noteB.Operation.Ref:      {Caller: "staff-1", Kind: model.ActionMessageNote, Target: w.convB.Ref.Ref()},
		w.note2.Operation.Ref:      {Caller: "staff-2", Kind: model.ActionMessageNote, Target: w.convA.Ref.Ref()},
		w.transition.Operation.Ref: {Caller: "staff-1", Kind: model.ActionConversationTransition, Target: w.convA.Ref.Ref()},
	}
	accepted := map[model.OperationRef]model.Acceptance{}
	for _, a := range []model.Acceptance{w.note1, w.reply, w.noteB, w.note2, w.transition} {
		accepted[a.Operation.Ref] = a
	}
	listed := w.walk(w.client, model.Filter{}, 2)
	if len(listed) != len(want)+3 {
		t.Fatalf("list holds %d operations, want %d", len(listed), len(want)+3)
	}
	for _, op := range listed {
		if op.Key == nil || op.Key.Kind == "" || op.Key.ID == "" || op.Key.Caller == "" || op.Key.Target.ID == "" {
			t.Errorf("listed operation %s carries key %+v, want caller, kind, target and ID", op.Ref.ID, op.Key)
			continue
		}
		k, ours := want[op.Ref]
		if !ours {
			continue
		}
		a := accepted[op.Ref]
		k.ID = op.Key.ID
		if *op.Key != k {
			t.Errorf("operation %s key = %+v, want %+v", op.Ref.ID, *op.Key, k)
		}
		if a.Operation.Key == nil || *a.Operation.Key != *op.Key {
			t.Errorf("operation %s accepted with key %+v, listed with %+v; want equal", op.Ref.ID, a.Operation.Key, *op.Key)
		}
		// The key's kind, target and ID recover the operation for its
		// caller, exactly as Get shows it.
		caller := w.client
		if op.Key.Caller == "staff-2" {
			caller = w.staff2
		}
		got, err := caller.Operations().Lookup(w.ctx, model.OperationLookup{
			Call: model.Call{Correlation: "lookup"}, Kind: op.Key.Kind, Target: op.Key.Target, ID: op.Key.ID,
		})
		if err != nil || !reflect.DeepEqual(got, op) {
			t.Errorf("Lookup by the listed key of %s = %+v, %v; want the listed operation", op.Ref.ID, got, err)
		}
	}
	// Two callers may claim the same kind, target and ID: the key tells
	// them apart.
	if want[w.note1.Operation.Ref].Caller == want[w.note2.Operation.Ref].Caller {
		t.Fatal("world: notes by one caller")
	}
}

// opListKinds: Filter.Kinds selects operations by the kind of their key,
// alone and with the other filters; a kind no operation has selects none.
func opListKinds(t *testing.T, w *operationWorld) {
	ref := func(a model.Acceptance) model.OperationRef { return a.Operation.Ref }
	notes := []string{model.ActionMessageNote}
	w.wantRefs("notes", model.Filter{Kinds: notes}, ref(w.note2), ref(w.noteB), ref(w.note1))
	w.wantRefs("replies", model.Filter{Kinds: []string{model.ActionMessageReply}}, ref(w.reply))
	w.wantRefs("transitions", model.Filter{Kinds: []string{model.ActionConversationTransition}}, ref(w.transition))
	w.wantRefs("replies or notes", model.Filter{Kinds: []string{model.ActionMessageReply, model.ActionMessageNote}},
		ref(w.note2), ref(w.noteB), ref(w.reply), ref(w.note1))
	w.wantRefs("notes in inbox two", model.Filter{Kinds: notes, Inboxes: []model.InboxRef{w.inbox2}}, ref(w.noteB))
	w.wantRefs("notes in conversation A", model.Filter{Kinds: notes, Conversation: &w.convA.Ref}, ref(w.note2), ref(w.note1))
	w.wantRefs("queued notes", model.Filter{Kinds: notes, Statuses: []string{string(model.Queued)}})
	since := w.at[ref(w.noteB)]
	w.wantRefs("notes since", model.Filter{Kinds: notes, Since: &since}, ref(w.note2), ref(w.noteB))
	w.wantRefs("a kind no operation has", model.Filter{Kinds: []string{model.ActionMessageEdit}})
	w.wantRefs("an extension kind", model.Filter{Kinds: []string{"com.example.ticket.close"}})
	// The configuration operations of the env are found by theirs.
	cfg := w.walk(w.client, model.Filter{Kinds: []string{model.ActionInboxConfigure, model.ActionChannelConfigure}}, 1)
	if len(cfg) != 3 {
		t.Errorf("configuration kinds list %d operations, want the env's 3", len(cfg))
	}
	for _, op := range cfg {
		if op.Key == nil || op.Key.Kind != model.ActionInboxConfigure && op.Key.Kind != model.ActionChannelConfigure {
			t.Errorf("configuration kinds list %s with key %+v", op.Ref.ID, op.Key)
		}
	}
}

// opListActor: Filter.Actor selects the operations one actor sent. It only
// narrows: an operation the principal may not read stays out, whoever sent
// it.
func opListActor(t *testing.T, w *operationWorld) {
	ref := func(a model.Acceptance) model.OperationRef { return a.Operation.Ref }
	staff1, staff2, nobody := model.ActorID("staff-1"), model.ActorID("staff-2"), model.ActorID("nobody")
	w.wantRefs("staff-2", model.Filter{Actor: &staff2}, ref(w.note2))
	w.wantRefs("staff-1", model.Filter{Actor: &staff1}, ref(w.transition), ref(w.noteB), ref(w.reply), ref(w.note1))
	w.wantRefs("staff-1 notes in conversation A", model.Filter{Actor: &staff1, Kinds: []string{model.ActionMessageNote}, Conversation: &w.convA.Ref}, ref(w.note1))
	w.wantRefs("an actor that sent nothing", model.Filter{Actor: &nobody})
	// The other actor lists its own operations the same way.
	got := opRefsOf(w.walk(w.staff2, model.Filter{Actor: &staff2}, 1))
	if !slices.Equal(got, []model.OperationRef{ref(w.note2)}) {
		t.Errorf("staff-2 lists its own operations as %v, want its note", refIDs(got))
	}
	w.policy.denyOn(model.ActionOperationRead, ref(w.note2).Ref())
	w.wantRefs("staff-2 after its note became unreadable", model.Filter{Actor: &staff2})
}

// opListFilterKeysRefusedElsewhere: Filter.Actor is the operation list's
// alone; other lists and subscriptions refuse it rather than ignore it.
func opListFilterKeysRefusedElsewhere(t *testing.T, w *operationWorld) {
	actor := model.ActorID("staff-1")
	f := model.Filter{Actor: &actor}
	_, err := w.client.Conversations().List(w.ctx, serviceList(f, 10, ""))
	if model.CodeOf(err) != model.Validation {
		t.Errorf("Conversations.List with an actor: %v, want validation", err)
	}
	_, err = w.client.Messages().List(w.ctx, serviceList(model.Filter{Conversation: &w.convA.Ref, Actor: &actor}, 10, ""))
	if model.CodeOf(err) != model.Validation {
		t.Errorf("Messages.List with an actor: %v, want validation", err)
	}
	_, err = w.client.Events().List(w.ctx, model.ListEvents{Call: model.Call{Correlation: "corr"}, Filter: f, Start: model.StartBeginning})
	if model.CodeOf(err) != model.Validation {
		t.Errorf("Events.List with an actor: %v, want validation", err)
	}
	_, err = w.client.Events().Open(w.ctx, model.OpenSubscription{
		Command: w.cmd(""), Consumer: "by-actor", Filter: f, Start: model.StartBeginning, PayloadMajor: 1,
	})
	if model.CodeOf(err) != model.Validation {
		t.Errorf("Events.Open with an actor: %v, want validation", err)
	}
}

// operationListContactKinds finds contact writes and a merge by kind.
func operationListContactKinds(t *testing.T, factory StoreFactory) {
	env := newContactsEnv(t, factory)
	from, into := env.createContact("Ada"), env.createContact("Ada L.")
	acc, err := env.merge(env.mergeReq(from, into))
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	list := func(kind string) []model.Operation {
		t.Helper()
		p, err := env.client.Operations().List(env.ctx, serviceList(model.Filter{Kinds: []string{kind}}, 100, ""))
		if err != nil || !p.Complete {
			t.Fatalf("List %s = %+v, %v", kind, p, err)
		}
		return p.Items
	}
	writes := list(model.ActionContactWrite)
	if len(writes) != 2 {
		t.Fatalf("contact.write lists %d operations, want the 2 creates", len(writes))
	}
	creation := model.CreationTarget(serviceTenant, model.KindContact)
	for _, op := range writes {
		if op.Key == nil || op.Key.Kind != model.ActionContactWrite || op.Key.Target != creation {
			t.Errorf("listed create %s key = %+v, want contact.write on the creation target", op.Ref.ID, op.Key)
		}
	}
	merges := list(model.ActionContactMerge)
	if len(merges) != 1 || merges[0].Ref != acc.Operation.Ref {
		t.Fatalf("contact.merge lists %v, want the merge %s", opRefsOf(merges), acc.Operation.Ref.ID)
	}
	if k := merges[0].Key; k == nil || k.Kind != model.ActionContactMerge || k.Target != from.Ref.Ref() || k.Caller != contactsActor {
		t.Errorf("listed merge key = %+v, want contact.merge on %s by %s", k, from.Ref.ID, contactsActor)
	}
}
