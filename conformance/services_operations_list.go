package conformance

import (
	"bytes"
	"encoding/base64"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// operationListServiceCases run Operations.List over a ledger of notes, a
// reply and a transition in two inboxes written one second apart: filters
// alone and combined, the newest-first order, Get's view of each operation,
// authorization, tenants, bad requests, cursors and the read-only rule.
var operationListServiceCases = []struct {
	name string
	run  func(*testing.T, *operationWorld)
}{
	{"FiltersAloneAndCombined", opListFilters},
	{"OrderAndPaging", opListOrder},
	{"EachOperationAsGetShowsIt", opListAsGet},
	{"UpdatedAtIsTheWriteTime", opListUpdatedAt},
	{"UnreadableOperationsAreLeftOut", opListUnreadable},
	{"DeniedEverywhereIsEmpty", opListDeniedEverywhere},
	{"TenantIsolation", opListTenants},
	{"BadRequests", opListBadRequests},
	{"CursorsAreBound", opListCursors},
	{"StableAcrossNewWrites", opListStable},
	{"ReadOnly", opListReadOnly},
	{"KeyOnEveryOperation", opListKeys},
	{"KindsFilter", opListKinds},
	{"ActorFilter", opListActor},
	{"ActorRefusedElsewhere", opListFilterKeysRefusedElsewhere},
}

// operationWorld is the ledger the cases read. Newest first, the
// operations are: transition (convA), note by staff-2 (convA), note (convB,
// inbox two), reply (convA, queued), note (convA); the three configuration
// operations of the env come before them and have no inbox.
type operationWorld struct {
	*serviceEnv
	convA, convB    model.Conversation
	note1, reply    model.Acceptance
	noteB, note2    model.Acceptance
	transition      model.Acceptance
	at              map[model.OperationRef]time.Time
	staff2          uma.Client
	allNewestFirst  []model.OperationRef
	inboxOneNewest  []model.OperationRef
	convANewestOnly []model.OperationRef
}

func newOperationWorld(t *testing.T, factory StoreFactory) *operationWorld {
	t.Helper()
	env := newTaskEnv(t, factory)
	if env.fx.Advance == nil {
		t.Skip("fixture cannot advance the store clock")
	}
	w := &operationWorld{serviceEnv: env, at: map[model.OperationRef]time.Time{}}
	w.staff2 = env.clientOf(env.engine, serviceTenant, "staff-2")
	w.convA = env.agentConversation()
	w.convB = env.conversation(env.inbox2, model.ConversationOpen)
	env.message(w.convA.Ref, model.Public)
	note := func(c uma.Client, conv model.Conversation) model.Acceptance {
		env.fx.Advance(time.Second)
		a, err := c.Messages().AddInternalNote(env.ctx, model.InternalNote{
			Command: env.cmd(""), Conversation: conv.Ref, PolicyRef: "staff", Content: textContent("note"),
		})
		if err != nil {
			t.Fatalf("AddInternalNote: %v", err)
		}
		w.at[a.Operation.Ref] = env.fx.Now()
		return a
	}
	w.note1 = note(env.client, w.convA)
	env.fx.Advance(time.Second)
	reply, err := env.client.Messages().Reply(env.ctx, model.Reply{Command: env.cmd(""), Conversation: w.convA.Ref, Content: textContent("answer")})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	w.reply = reply
	w.at[reply.Operation.Ref] = env.fx.Now()
	w.noteB = note(env.client, w.convB)
	w.note2 = note(w.staff2, w.convA)
	env.fx.Advance(time.Second)
	w.transition, err = env.client.Conversations().Transition(env.ctx, model.Transition{
		Command: env.cmd(w.convA.Revision), Conversation: w.convA.Ref, State: model.ConversationResolved,
	})
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	w.at[w.transition.Operation.Ref] = env.fx.Now()
	ref := func(a model.Acceptance) model.OperationRef { return a.Operation.Ref }
	w.convANewestOnly = []model.OperationRef{ref(w.transition), ref(w.note2), ref(w.reply), ref(w.note1)}
	w.inboxOneNewest = w.convANewestOnly
	w.allNewestFirst = []model.OperationRef{ref(w.transition), ref(w.note2), ref(w.noteB), ref(w.reply), ref(w.note1)}
	return w
}

// walk pages Operations.List as c to its end at page size limit.
func (w *operationWorld) walk(c uma.Client, f model.Filter, limit int) []model.Operation {
	w.t.Helper()
	var out []model.Operation
	var after model.Cursor
	for range 200 {
		p, err := c.Operations().List(w.ctx, serviceList(f, limit, after))
		if err != nil {
			w.t.Fatalf("Operations.List: %v", err)
		}
		out = append(out, p.Items...)
		if p.Complete {
			if p.Next != "" {
				w.t.Fatalf("complete page carries cursor %q: it would name the position of the last operation scanned", p.Next)
			}
			return out
		}
		if p.Next == "" || p.Next == after {
			w.t.Fatalf("incomplete page after %q returned next %q", after, p.Next)
		}
		after = p.Next
	}
	w.t.Fatal("paging did not complete")
	return nil
}

func opRefsOf(ops []model.Operation) []model.OperationRef {
	out := make([]model.OperationRef, len(ops))
	for i, op := range ops {
		out[i] = op.Ref
	}
	return out
}

// listRefs walks f as staff-1 and keeps the operations of the world, which
// leaves out the env's configuration operations.
func (w *operationWorld) listRefs(f model.Filter, limit int) []model.OperationRef {
	w.t.Helper()
	return slices.DeleteFunc(opRefsOf(w.walk(w.client, f, limit)), func(r model.OperationRef) bool {
		_, ours := w.at[r]
		return !ours
	})
}

func (w *operationWorld) wantRefs(name string, f model.Filter, want ...model.OperationRef) {
	w.t.Helper()
	for _, limit := range []int{100, 1} {
		if got := w.listRefs(f, limit); !slices.Equal(got, want) {
			w.t.Errorf("%s at page size %d: %d operations, want %d (newest first)\n got %v\nwant %v", name, limit, len(got), len(want), refIDs(got), refIDs(want))
		}
	}
}

func refIDs(refs []model.OperationRef) []model.ID {
	out := make([]model.ID, len(refs))
	for i, r := range refs {
		out[i] = r.ID
	}
	return out
}

func opListFilters(t *testing.T, w *operationWorld) {
	ref := func(a model.Acceptance) model.OperationRef { return a.Operation.Ref }
	one, two := []model.InboxRef{w.inbox}, []model.InboxRef{w.inbox2}
	queued := []string{string(model.Queued)}
	committed := []string{string(model.Committed)}
	w.wantRefs("no filter", model.Filter{}, w.allNewestFirst...)
	w.wantRefs("inbox one", model.Filter{Inboxes: one}, w.inboxOneNewest...)
	w.wantRefs("inbox two", model.Filter{Inboxes: two}, ref(w.noteB))
	w.wantRefs("both inboxes", model.Filter{Inboxes: []model.InboxRef{w.inbox, w.inbox2}}, w.allNewestFirst...)
	w.wantRefs("conversation A", model.Filter{Conversation: &w.convA.Ref}, w.convANewestOnly...)
	w.wantRefs("conversation B", model.Filter{Conversation: &w.convB.Ref}, ref(w.noteB))
	w.wantRefs("queued", model.Filter{Statuses: queued}, ref(w.reply))
	w.wantRefs("committed", model.Filter{Statuses: committed}, ref(w.transition), ref(w.note2), ref(w.noteB), ref(w.note1))
	w.wantRefs("queued or committed", model.Filter{Statuses: []string{string(model.Queued), string(model.Committed)}}, w.allNewestFirst...)
	w.wantRefs("delivered", model.Filter{Statuses: []string{string(model.Delivered)}})
	w.wantRefs("inbox one and committed", model.Filter{Inboxes: one, Statuses: committed}, ref(w.transition), ref(w.note2), ref(w.note1))
	w.wantRefs("inbox two and queued", model.Filter{Inboxes: two, Statuses: queued})
	w.wantRefs("conversation A and queued", model.Filter{Conversation: &w.convA.Ref, Statuses: queued}, ref(w.reply))
	w.wantRefs("conversation B in inbox one", model.Filter{Inboxes: one, Conversation: &w.convB.Ref})

	// The window is on the time written: Since inclusive, Before exclusive.
	since, before := w.at[ref(w.reply)], w.at[ref(w.note2)]
	w.wantRefs("since", model.Filter{Since: &since}, ref(w.transition), ref(w.note2), ref(w.noteB), ref(w.reply))
	w.wantRefs("before", model.Filter{Before: &before}, ref(w.noteB), ref(w.reply), ref(w.note1))
	w.wantRefs("window", model.Filter{Since: &since, Before: &before}, ref(w.noteB), ref(w.reply))
	w.wantRefs("window and inbox one", model.Filter{Since: &since, Before: &before, Inboxes: one}, ref(w.reply))
	w.wantRefs("window and statuses", model.Filter{Since: &since, Before: &before, Statuses: committed}, ref(w.noteB))
	all := model.Filter{Inboxes: one, Conversation: &w.convA.Ref, Statuses: committed, Since: &since, Before: &before}
	w.wantRefs("every filter", all)
	all.Before = nil
	w.wantRefs("every filter, open window", all, ref(w.transition), ref(w.note2))

	// A state change is a write: the listing follows the current state.
	w.fx.Advance(time.Second)
	env := w.serviceEnv
	env.put(func(tx store.Tx) error {
		rec, err := tx.Operation(env.ctx, ref(w.reply))
		if err != nil {
			return err
		}
		next := rec.Operation.Clone()
		next.State, next.Revision = model.Delivered, rec.Operation.Revision+"-settled"
		return tx.UpdateOperation(env.ctx, next, rec.Operation.Revision, store.Fence{})
	})
	w.at[ref(w.reply)] = w.fx.Now()
	w.wantRefs("queued after the reply settled", model.Filter{Statuses: queued})
	w.wantRefs("delivered after the reply settled", model.Filter{Statuses: []string{string(model.Delivered)}}, ref(w.reply))
	w.wantRefs("a settled operation moves to the front", model.Filter{},
		ref(w.reply), ref(w.transition), ref(w.note2), ref(w.noteB), ref(w.note1))
}

func opListOrder(t *testing.T, w *operationWorld) {
	for _, limit := range []int{1, 2, 3, 100} {
		if got := w.listRefs(model.Filter{}, limit); !slices.Equal(got, w.allNewestFirst) {
			t.Errorf("page size %d lists %v, want newest first %v", limit, refIDs(got), refIDs(w.allNewestFirst))
		}
	}
	// Every call, whatever it filters, shares the order; the env's own
	// configuration operations are older than all of ours.
	all := w.walk(w.client, model.Filter{}, 100)
	if len(all) != len(w.allNewestFirst)+3 {
		t.Fatalf("unfiltered list holds %d operations, want %d plus the 3 configuration operations", len(all), len(w.allNewestFirst))
	}
	for _, op := range all[len(all)-3:] {
		if _, ours := w.at[op.Ref]; ours {
			t.Errorf("operation %s is among the oldest, want the configuration operations there", op.Ref.ID)
		}
	}
	p, err := w.client.Operations().List(w.ctx, serviceList(model.Filter{}, 0, ""))
	if err != nil || len(p.Items) != len(all) {
		t.Fatalf("default page size lists %d operations (%v), want all %d", len(p.Items), err, len(all))
	}
}

func opListAsGet(t *testing.T, w *operationWorld) {
	for _, op := range w.walk(w.client, model.Filter{}, 2) {
		got, err := w.getOp(w.client, op.Ref)
		if err != nil || !reflect.DeepEqual(op, got) {
			t.Errorf("listed operation %s = %+v, Get = %+v, %v; want equal", op.Ref.ID, op, got, err)
		}
	}
}

// Every operation shows when it was written, as Get and List both show it,
// and that time is the one the list orders and the window bounds by.
func opListUpdatedAt(t *testing.T, w *operationWorld) {
	all := w.walk(w.client, model.Filter{}, 2)
	for i, op := range all {
		if op.UpdatedAt.IsZero() {
			t.Errorf("listed operation %s has no UpdatedAt", op.Ref.ID)
		}
		if want, ours := w.at[op.Ref]; ours && !op.UpdatedAt.Equal(micro(want)) {
			t.Errorf("listed operation %s UpdatedAt = %v, want the time it was written, %v", op.Ref.ID, op.UpdatedAt, micro(want))
		}
		if got, err := w.getOp(w.client, op.Ref); err != nil || !got.UpdatedAt.Equal(op.UpdatedAt) {
			t.Errorf("operation %s: Get UpdatedAt = %v (%v), listed %v; want equal", op.Ref.ID, got.UpdatedAt, err, op.UpdatedAt)
		}
		if i > 0 && op.UpdatedAt.After(all[i-1].UpdatedAt) {
			t.Errorf("operation %s UpdatedAt %v is after the one listed before it, %v: want newest first", op.Ref.ID, op.UpdatedAt, all[i-1].UpdatedAt)
		}
		at, after := op.UpdatedAt, op.UpdatedAt.Add(time.Microsecond)
		for name, c := range map[string]struct {
			f    model.Filter
			want bool
		}{
			"Since its UpdatedAt":            {model.Filter{Since: &at}, true},
			"Since just after":               {model.Filter{Since: &after}, false},
			"Before its UpdatedAt":           {model.Filter{Before: &at}, false},
			"Before just after":              {model.Filter{Before: &after}, true},
			"the microsecond it was written": {model.Filter{Since: &at, Before: &after}, true},
		} {
			found := slices.Contains(opRefsOf(w.walk(w.client, c.f, 3)), op.Ref)
			if found != c.want {
				t.Errorf("operation %s shown at %v: window %s lists it = %v, want %v", op.Ref.ID, at, name, found, c.want)
			}
		}
	}
	// A write to another operation leaves it alone, and the new operation is
	// written at the clock.
	w.fx.Advance(time.Second)
	before, err := w.getOp(w.client, w.note1.Operation.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.client.Messages().Reply(w.ctx, model.Reply{Command: w.cmd(""), Conversation: w.convB.Ref, Content: textContent("later")}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if again, err := w.getOp(w.client, w.note1.Operation.Ref); err != nil || !again.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("an unrelated write changed UpdatedAt: %v, want %v (%v)", again.UpdatedAt, before.UpdatedAt, err)
	}
	newest := w.walk(w.client, model.Filter{}, 1)[0]
	if !newest.UpdatedAt.After(all[0].UpdatedAt) || !newest.UpdatedAt.Equal(micro(w.fx.Now())) {
		t.Errorf("new operation UpdatedAt = %v, want the clock %v, after %v", newest.UpdatedAt, micro(w.fx.Now()), all[0].UpdatedAt)
	}
}

func opListUnreadable(t *testing.T, w *operationWorld) {
	hidden := w.note2.Operation.Ref
	w.policy.denyOn(model.ActionOperationRead, hidden.Ref())
	_, getErr := w.getOp(w.client, hidden)
	serviceCode(t, getErr, model.Unauthorized)
	ref := func(a model.Acceptance) model.OperationRef { return a.Operation.Ref }
	rest := []model.OperationRef{ref(w.transition), ref(w.noteB), ref(w.reply), ref(w.note1)}
	for name, f := range map[string]model.Filter{
		"no filter":      {},
		"inbox":          {Inboxes: []model.InboxRef{w.inbox, w.inbox2}},
		"conversation A": {Conversation: &w.convA.Ref},
		"committed":      {Statuses: []string{string(model.Committed)}},
	} {
		for _, limit := range []int{100, 1, 2} {
			for _, r := range w.listRefs(f, limit) {
				if r == hidden {
					t.Errorf("%s at page size %d lists the operation the principal may not read", name, limit)
				}
			}
		}
	}
	if got := w.listRefs(model.Filter{}, 1); !slices.Equal(got, rest) {
		t.Errorf("list = %v, want the readable operations %v", refIDs(got), refIDs(rest))
	}
	// A page never names the position of an operation it did not return, and
	// the hidden one leaves no trace in the pages' shape: the same items
	// come back at every page size.
	var after model.Cursor
	for range 20 {
		p, err := w.client.Operations().List(w.ctx, serviceList(model.Filter{}, 1, after))
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Items) != 1 && !p.Complete {
			t.Fatalf("page of limit 1 returned %d items and is not complete", len(p.Items))
		}
		if raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(string(p.Next), "q1.")); err == nil && bytes.Contains(raw, []byte(hidden.ID)) {
			t.Fatalf("cursor %q names the hidden operation %s", p.Next, hidden.ID)
		}
		if p.Complete {
			break
		}
		after = p.Next
	}
	// A principal that may read everything still sees it.
	if got := w.walk(w.client, model.Filter{}, 100); len(got) != len(rest)+3 {
		t.Errorf("list holds %d operations, want the %d readable ones and the 3 configuration operations", len(got), len(rest))
	}
}

func opListDeniedEverywhere(t *testing.T, w *operationWorld) {
	w.policy.deny(model.ActionOperationRead)
	p, err := w.client.Operations().List(w.ctx, serviceList(model.Filter{}, 10, ""))
	if err != nil || len(p.Items) != 0 || !p.Complete || p.Next != "" {
		t.Fatalf("List with operation.read denied = %+v, %v; want an empty complete page, as if there were none", p, err)
	}
}

func opListTenants(t *testing.T, w *operationWorld) {
	const other model.TenantID = "conformance-other"
	foreign := w.clientOf(w.engine, other, "staff-1")
	if got := w.walk(foreign, model.Filter{}, 100); len(got) != 0 {
		t.Errorf("another tenant lists %d operations, want none", len(got))
	}
	for name, c := range map[string]struct {
		client uma.Client
		filter model.Filter
	}{
		"foreign inbox":        {w.client, model.Filter{Inboxes: []model.InboxRef{{Tenant: other, ID: w.inbox.ID}}}},
		"foreign conversation": {w.client, model.Filter{Conversation: &model.ConversationRef{Inbox: model.InboxRef{Tenant: other, ID: w.inbox.ID}, ID: w.convA.Ref.ID}}},
		"own inbox, foreign":   {foreign, model.Filter{Inboxes: []model.InboxRef{w.inbox}}},
	} {
		_, err := c.client.Operations().List(w.ctx, serviceList(c.filter, 10, ""))
		if model.CodeOf(err) != model.NotFound {
			t.Errorf("%s: List error = %v, want not_found", name, err)
		}
	}
	// A cursor of one tenant is no cursor of another.
	p, err := w.client.Operations().List(w.ctx, serviceList(model.Filter{}, 1, ""))
	if err != nil || p.Next == "" {
		t.Fatalf("first page = %+v, %v", p, err)
	}
	_, err = foreign.Operations().List(w.ctx, serviceList(model.Filter{}, 1, p.Next))
	serviceCode(t, err, model.Validation)
}

func opListBadRequests(t *testing.T, w *operationWorld) {
	at := w.fx.Now()
	task := model.TaskRef{Conversation: w.convA.Ref, ID: "00000000-0000-7000-8000-000000000001"}
	yes := true
	contact := model.ContactRef{Tenant: serviceTenant, ID: "00000000-0000-7000-8000-000000000002"}
	identity := model.IdentityRef{Inbox: w.inbox, ID: "00000000-0000-7000-8000-000000000003"}
	for name, f := range map[string]model.Filter{
		"malformed kind": {Kinds: []string{""}},
		"empty actor":    {Actor: new(model.ActorID)},
		"contact":        {Contact: &contact},
		"identity":       {Identity: &identity},
		"labels":         {Labels: &model.LabelSelector{}},
		"task":           {Task: &task},
		"text":           {Text: "note"},
		"attachments":    {Attachments: &model.AttachmentFilter{Has: &yes}},
		"unknown status": {Statuses: []string{"done"}},
		"mixed statuses": {Statuses: []string{string(model.Queued), "done"}},
		"empty window":   {Since: &at, Before: &at},
		"reversed":       {Since: timePtr(at.Add(time.Second)), Before: &at},
		"malformed":      {Inboxes: []model.InboxRef{{}}},
	} {
		_, err := w.client.Operations().List(w.ctx, serviceList(f, 10, ""))
		if model.CodeOf(err) != model.Validation {
			t.Errorf("%s: List error = %v, want validation", name, err)
		}
	}
	_, err := w.client.Operations().List(w.ctx, serviceList(model.Filter{}, -1, ""))
	serviceCode(t, err, model.Validation)
	for _, c := range []model.Cursor{"x", "q1.", "q1.AAAA", "mem.o.1.2"} {
		_, err := w.client.Operations().List(w.ctx, serviceList(model.Filter{}, 10, c))
		if model.CodeOf(err) != model.Validation {
			t.Errorf("cursor %q: List error = %v, want validation", c, err)
		}
	}
	p, err := w.client.Operations().List(w.ctx, serviceList(model.Filter{Statuses: []string{string(model.Delivered)}}, 10, ""))
	if err != nil || len(p.Items) != 0 || !p.Complete {
		t.Errorf("a known state nothing holds = %+v, %v; want an empty complete page", p, err)
	}
}

func opListCursors(t *testing.T, w *operationWorld) {
	p, err := w.client.Operations().List(w.ctx, serviceList(model.Filter{}, 1, ""))
	if err != nil || p.Next == "" {
		t.Fatalf("first page = %+v, %v", p, err)
	}
	if _, err := w.client.Operations().List(w.ctx, serviceList(model.Filter{}, 1, p.Next)); err != nil {
		t.Fatalf("valid cursor: %v", err)
	}
	// Bound to the filter, the caller and the query.
	_, err = w.client.Operations().List(w.ctx, serviceList(model.Filter{Inboxes: []model.InboxRef{w.inbox}}, 1, p.Next))
	serviceCode(t, err, model.Validation)
	_, err = w.client.Operations().List(w.ctx, serviceList(model.Filter{Kinds: []string{model.ActionMessageNote}}, 1, p.Next))
	serviceCode(t, err, model.Validation)
	staff1 := model.ActorID("staff-1")
	_, err = w.client.Operations().List(w.ctx, serviceList(model.Filter{Actor: &staff1}, 1, p.Next))
	serviceCode(t, err, model.Validation)
	_, err = w.staff2.Operations().List(w.ctx, serviceList(model.Filter{}, 1, p.Next))
	serviceCode(t, err, model.Validation)
	_, err = w.client.Tasks().List(w.ctx, serviceList(model.Filter{}, 1, p.Next))
	serviceCode(t, err, model.Validation)
	b := []byte(p.Next)
	b[len(b)-1] ^= 1
	_, err = w.client.Operations().List(w.ctx, serviceList(model.Filter{}, 1, model.Cursor(b)))
	serviceCode(t, err, model.Validation)
	// A cursor resumes with another page size and over repeats.
	rest := opRefsOf(w.walkFrom(p.Next, 100))
	again := opRefsOf(w.walkFrom(p.Next, 1))
	if !slices.Equal(rest, again) || len(rest) != len(w.allNewestFirst)+2 {
		t.Errorf("resuming after one operation gave %v then %v, want the other operations twice", refIDs(rest), refIDs(again))
	}
}

func (w *operationWorld) walkFrom(after model.Cursor, limit int) []model.Operation {
	w.t.Helper()
	var out []model.Operation
	for range 200 {
		p, err := w.client.Operations().List(w.ctx, serviceList(model.Filter{}, limit, after))
		if err != nil {
			w.t.Fatal(err)
		}
		out = append(out, p.Items...)
		if p.Complete {
			return out
		}
		after = p.Next
	}
	w.t.Fatal("paging did not complete")
	return nil
}

func opListStable(t *testing.T, w *operationWorld) {
	first, err := w.client.Operations().List(w.ctx, serviceList(model.Filter{}, 2, ""))
	if err != nil || len(first.Items) != 2 || first.Complete {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	// Between the pages a note is accepted and an operation already paged
	// past is written again.
	w.fx.Advance(time.Second)
	w.put(func(tx store.Tx) error {
		rec, err := tx.Operation(w.ctx, first.Items[1].Ref)
		if err != nil {
			return err
		}
		next := rec.Operation.Clone()
		next.Revision = rec.Operation.Revision + "-again"
		return tx.UpdateOperation(w.ctx, next, rec.Operation.Revision, store.Fence{})
	})
	fresh, err := w.client.Messages().AddInternalNote(w.ctx, model.InternalNote{
		Command: w.cmd(""), Conversation: w.convB.Ref, PolicyRef: "staff", Content: textContent("late"),
	})
	if err != nil {
		t.Fatal(err)
	}
	rest := w.walkFrom(first.Next, 2)
	seen := map[model.OperationRef]bool{}
	for _, op := range slices.Concat(first.Items, rest) {
		if seen[op.Ref] {
			t.Errorf("operation %s listed twice across the pages", op.Ref.ID)
		}
		seen[op.Ref] = true
	}
	if seen[fresh.Operation.Ref] {
		t.Errorf("the operation accepted after the first page is on a later page")
	}
	if got := len(seen); got != len(w.allNewestFirst)+3 {
		t.Errorf("the pages hold %d distinct operations, want the %d there were when the first was read", got, len(w.allNewestFirst)+3)
	}
}

func opListReadOnly(t *testing.T, w *operationWorld) {
	counting, e := w.countingEngine()
	c := w.clientOf(e, serviceTenant, "staff-1")
	head := w.eventHead()
	counting.reset()
	if got := w.walk(c, model.Filter{}, 2); len(got) == 0 {
		t.Fatal("List returned nothing")
	}
	if n := counting.transacts.Load(); n != 0 {
		t.Fatalf("List opened %d write units, want none", n)
	}
	if got := w.eventHead(); got != head {
		t.Fatalf("event head moved %q -> %q, want no event", head, got)
	}
}

// operationListPending lists the unsettled edit and reaction of the
// mutation env: each is shown as Operations.Get shows it, Pending included
// while the caller may read the message and omitted when it may not, and
// settling moves it between the state filters.
func operationListPending(t *testing.T, factory StoreFactory) {
	env := newMutationEnv(t, factory)
	target := env.target
	edit, err := env.client.Messages().Edit(env.ctx, model.EditMessage{
		Command: env.cmd(target.Revision), Message: target.Ref,
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "corrected"}}},
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	react, err := env.client.Messages().React(env.ctx, model.Reaction{Command: env.cmd(""), Message: target.Ref, Emoji: "👍"})
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	list := func(f model.Filter) []model.Operation {
		t.Helper()
		p, err := env.client.Operations().List(env.ctx, serviceList(f, 100, ""))
		if err != nil || !p.Complete {
			t.Fatalf("List = %+v, %v", p, err)
		}
		return p.Items
	}
	conv := target.Ref.Conversation
	queued := model.Filter{Conversation: &conv, Statuses: []string{string(model.Queued)}}
	got := list(queued)
	if len(got) != 2 || got[0].Ref != react.Operation.Ref || got[1].Ref != edit.Operation.Ref {
		t.Fatalf("queued in the conversation = %v, want the reaction then the edit", opRefsOf(got))
	}
	for _, op := range got {
		if want := env.get(op.Ref); !reflect.DeepEqual(op, want) || op.Pending == nil {
			t.Errorf("listed %s = %+v, Get = %+v; want equal and pending", op.Ref.ID, op, want)
		}
	}
	env.policy.hide(true)
	for _, op := range list(queued) {
		if op.Pending != nil {
			t.Errorf("operation %s lists its pending change without message.read", op.Ref.ID)
		}
		if want := env.get(op.Ref); !reflect.DeepEqual(op, want) {
			t.Errorf("listed %s = %+v, Get = %+v; want equal", op.Ref.ID, op, want)
		}
	}
	env.policy.hide(false)
	env.settle(edit.Operation.Ref, model.Delivered)
	if got := list(queued); len(got) != 1 || got[0].Ref != react.Operation.Ref {
		t.Fatalf("queued after the edit settled = %v, want only the reaction", opRefsOf(got))
	}
	delivered := list(model.Filter{Conversation: &conv, Statuses: []string{string(model.Delivered)}})
	if len(delivered) != 1 || delivered[0].Ref != edit.Operation.Ref || delivered[0].Pending != nil {
		t.Fatalf("delivered in the conversation = %+v, want the settled edit, no longer pending", delivered)
	}
}
