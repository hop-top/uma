package conformance

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// lookupCases recover operations by their operation key through
// Operations.Lookup: a lost acceptance is found again by kind, target and
// operation ID; another caller, another tenant and a purged key are not
// found; a denial is Get's; and a lookup writes nothing and emits no event.
var lookupCases = []struct {
	name string
	run  func(*testing.T, *serviceEnv)
}{
	{"AfterLostResponse", lookupAfterLostResponse},
	{"OtherCallerNotFound", lookupOtherCaller},
	{"OtherTenantNotFound", lookupOtherTenant},
	{"PurgedKeyNotFound", lookupPurged},
	{"DeniedAsGet", lookupDenied},
	{"ReadOnly", lookupReadOnly},
}

func lookupReq(kind string, target model.Ref, id model.OperationID) model.OperationLookup {
	return model.OperationLookup{Call: model.Call{Correlation: "lookup"}, Kind: kind, Target: target, ID: id}
}

// noteKey is the key an internal note claims: kind message.note, target
// the conversation ref.
func (env *serviceEnv) noteKey(id model.OperationID) model.OperationLookup {
	return lookupReq(model.ActionMessageNote, env.conv.Ref.Ref(), id)
}

// claimNote adds an internal note as c under a fresh operation ID and
// returns that ID with the acceptance.
func (env *serviceEnv) claimNote(c uma.Client, text string) (model.OperationID, model.Acceptance) {
	env.t.Helper()
	cmd := env.cmd("")
	return cmd.Operation, env.noteAs(c, cmd, text)
}

func (env *serviceEnv) noteAs(c uma.Client, cmd model.Command, text string) model.Acceptance {
	env.t.Helper()
	a, err := c.Messages().AddInternalNote(env.ctx, model.InternalNote{
		Command: cmd, Conversation: env.conv.Ref, PolicyRef: "staff",
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
	})
	if err != nil {
		env.t.Fatalf("AddInternalNote: %v", err)
	}
	return a
}

func (env *serviceEnv) getOp(c uma.Client, ref model.OperationRef) (model.Operation, error) {
	return c.Operations().Get(env.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "get"}, Ref: ref})
}

// lookupAfterLostResponse drops the acceptances of a note and a
// transition and recovers each operation by the key its command claimed,
// as Get returns it. Another kind, target or ID is not found.
func lookupAfterLostResponse(t *testing.T, env *serviceEnv) {
	noteCmd := env.cmd("")
	env.noteAs(env.client, noteCmd, "lost") // the acceptance is lost
	trCmd := env.cmd(env.conv.Revision)
	_, err := env.client.Conversations().Transition(env.ctx, model.Transition{
		Command: trCmd, Conversation: env.conv.Ref, State: model.ConversationResolved,
	})
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}

	note, err := env.client.Operations().Lookup(env.ctx, env.noteKey(noteCmd.Operation))
	if err != nil {
		t.Fatalf("Lookup note: %v", err)
	}
	if note.State != model.Committed || note.ResultMessage == nil || note.ResultMessage.Conversation != env.conv.Ref {
		t.Fatalf("Lookup note = %+v, want the committed note operation", note)
	}
	if want, err := env.getOp(env.client, note.Ref); err != nil || !reflect.DeepEqual(note, want) {
		t.Fatalf("Lookup note = %+v, Get = %+v, %v; want equal", note, want, err)
	}
	if want := micro(env.fx.Now()); !note.UpdatedAt.Equal(want) {
		t.Fatalf("Lookup note UpdatedAt = %v, want the write time %v", note.UpdatedAt, want)
	}
	tr, err := env.client.Operations().Lookup(env.ctx, lookupReq(model.ActionConversationTransition, env.conv.Ref.Ref(), trCmd.Operation))
	if err != nil {
		t.Fatalf("Lookup transition: %v", err)
	}
	if tr.Ref == note.Ref || tr.State != model.Committed {
		t.Fatalf("Lookup transition = %+v, want its own committed operation", tr)
	}
	if want, err := env.getOp(env.client, tr.Ref); err != nil || !reflect.DeepEqual(tr, want) {
		t.Fatalf("Lookup transition = %+v, Get = %+v, %v; want equal", tr, want, err)
	}

	// Repeating the command replays the operation Lookup found.
	if again := env.noteAs(env.client, noteCmd, "lost"); !again.Replayed || again.Operation.Ref != note.Ref {
		t.Fatalf("replay = %+v; want operation %v replayed", again, note.Ref)
	}

	other := env.conversation(env.inbox2, model.ConversationOpen)
	for name, q := range map[string]model.OperationLookup{
		"kind":         lookupReq(model.ActionMessageReply, env.conv.Ref.Ref(), noteCmd.Operation),
		"target":       lookupReq(model.ActionMessageNote, other.Ref.Ref(), noteCmd.Operation),
		"inbox":        lookupReq(model.ActionMessageNote, env.inbox.Ref(), noteCmd.Operation),
		"id":           env.noteKey("op-never"),
		"unregistered": lookupReq("com.example.never", env.conv.Ref.Ref(), noteCmd.Operation),
	} {
		_, err := env.client.Operations().Lookup(env.ctx, q)
		if model.CodeOf(err) != model.NotFound {
			t.Errorf("%s: Lookup error = %v, want not_found", name, err)
		}
	}
}

// lookupOtherCaller claims one kind, target and ID as two actors of the
// tenant: each finds only its own operation, and neither finds the other's
// before claiming.
func lookupOtherCaller(t *testing.T, env *serviceEnv) {
	other := env.clientOf(env.engine, serviceTenant, "staff-2")
	cmd := env.cmd("")
	theirs := env.noteAs(other, cmd, "theirs")
	_, err := env.client.Operations().Lookup(env.ctx, env.noteKey(cmd.Operation))
	serviceCode(t, err, model.NotFound)

	mine := env.noteAs(env.client, cmd, "mine")
	if mine.Replayed || mine.Operation.Ref == theirs.Operation.Ref {
		t.Fatalf("own claim = %+v, want a new operation", mine)
	}
	got, err := env.client.Operations().Lookup(env.ctx, env.noteKey(cmd.Operation))
	if err != nil || got.Ref != mine.Operation.Ref {
		t.Fatalf("own Lookup = %v, %v; want %v", got.Ref, err, mine.Operation.Ref)
	}
	got, err = other.Operations().Lookup(env.ctx, env.noteKey(cmd.Operation))
	if err != nil || got.Ref != theirs.Operation.Ref {
		t.Fatalf("other caller's Lookup = %v, %v; want %v", got.Ref, err, theirs.Operation.Ref)
	}
}

// lookupOtherTenant looks a claimed key up from another tenant, by the
// claimed target and by the same target in its own tenant, and with the
// claiming principal under a target in another tenant: none is found, and
// a target outside the principal's tenant opens no store unit.
func lookupOtherTenant(t *testing.T, env *serviceEnv) {
	id, a := env.claimNote(env.client, "claimed")
	q := env.noteKey(id)
	if got, err := env.client.Operations().Lookup(env.ctx, q); err != nil || got.Ref != a.Operation.Ref {
		t.Fatalf("own Lookup = %v, %v; want %v", got.Ref, err, a.Operation.Ref)
	}
	const otherTenant model.TenantID = "conformance-other"
	counting, e := env.countingEngine()
	own := env.clientOf(e, serviceTenant, "staff-1")
	foreign := env.clientOf(e, otherTenant, "staff-1")
	moved := q
	moved.Target.Tenant = otherTenant
	for name, look := range map[string]func() error{
		"foreign principal, claimed target":  func() error { _, err := foreign.Operations().Lookup(env.ctx, q); return err },
		"claiming principal, foreign target": func() error { _, err := own.Operations().Lookup(env.ctx, moved); return err },
	} {
		counting.reset()
		serviceCode(t, look(), model.NotFound)
		if v, w := counting.views.Load(), counting.transacts.Load(); v+w != 0 {
			t.Errorf("%s: store units = %d reads, %d writes; want none before the scope check", name, v, w)
		}
	}
	_, err := foreign.Operations().Lookup(env.ctx, moved)
	serviceCode(t, err, model.NotFound)
}

// lookupPurged purges a committed operation past the operations horizon:
// its key is then not found, while an operation inside the horizon still
// is.
func lookupPurged(t *testing.T, env *serviceEnv) {
	if env.fx.Advance == nil {
		t.Skip("fixture cannot advance the store clock")
	}
	oldID, _ := env.claimNote(env.client, "before the horizon")
	if _, err := env.client.Operations().Lookup(env.ctx, env.noteKey(oldID)); err != nil {
		t.Fatalf("Lookup before purge: %v", err)
	}
	env.fx.Advance(2 * serviceHorizon)
	env.clock.Advance(2 * serviceHorizon)
	recentID, recent := env.claimNote(env.client, "after the horizon")
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: serviceTenant, Actor: "staff-1"}, Call: model.Call{Correlation: "purge"}}
	if rep, err := env.engine.Runtime().Purge(env.ctx, call); err != nil || rep.Operations == 0 {
		t.Fatalf("Purge = %+v, %v; want the old operation purged", rep, err)
	}
	_, err := env.client.Operations().Lookup(env.ctx, env.noteKey(oldID))
	serviceCode(t, err, model.NotFound)
	if got, err := env.client.Operations().Lookup(env.ctx, env.noteKey(recentID)); err != nil || got.Ref != recent.Operation.Ref {
		t.Fatalf("recent Lookup = %v, %v; want %v retained", got.Ref, err, recent.Operation.Ref)
	}
}

// lookupDenied denies operation.read: Lookup of a claimed key fails with
// exactly Get's error for the found operation.
func lookupDenied(t *testing.T, env *serviceEnv) {
	id, a := env.claimNote(env.client, "claimed")
	env.policy.deny(model.ActionOperationRead)
	_, getErr := env.getOp(env.client, a.Operation.Ref)
	serviceCode(t, getErr, model.Unauthorized)
	_, err := env.client.Operations().Lookup(env.ctx, env.noteKey(id))
	serviceCode(t, err, model.Unauthorized)
	if !reflect.DeepEqual(err, getErr) {
		t.Fatalf("Lookup error = %#v, want Get's %#v", err, getErr)
	}
}

// lookupReadOnly looks keys up, found and not, through an engine over the
// same store that counts write units: none is opened, and the event log
// head does not move.
func lookupReadOnly(t *testing.T, env *serviceEnv) {
	id, a := env.claimNote(env.client, "claimed")
	counting, e := env.countingEngine()
	c := env.clientOf(e, serviceTenant, "staff-1")
	head := env.eventHead()
	counting.reset()
	if got, err := c.Operations().Lookup(env.ctx, env.noteKey(id)); err != nil || got.Ref != a.Operation.Ref {
		t.Fatalf("Lookup = %v, %v; want %v", got.Ref, err, a.Operation.Ref)
	}
	_, err := c.Operations().Lookup(env.ctx, env.noteKey("op-never"))
	serviceCode(t, err, model.NotFound)
	env.policy.deny(model.ActionOperationRead)
	_, err = c.Operations().Lookup(env.ctx, env.noteKey(id))
	serviceCode(t, err, model.Unauthorized)
	if n := counting.transacts.Load(); n != 0 {
		t.Fatalf("Lookup opened %d write units, want none", n)
	}
	if got := env.eventHead(); got != head {
		t.Fatalf("event head moved %q -> %q, want no event", head, got)
	}
}

func (env *serviceEnv) eventHead() model.Cursor {
	env.t.Helper()
	var head model.Cursor
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) error {
		var err error
		head, err = r.EventHead(env.ctx)
		return err
	})
	if err != nil {
		env.t.Fatalf("EventHead: %v", err)
	}
	return head
}

// countingEngine returns a second engine over the env's store, its policy
// and clock, whose store counts the units it opens.
func (env *serviceEnv) countingEngine() (*countingStore, uma.Engine) {
	env.t.Helper()
	counting := &countingStore{Store: env.st}
	e, err := uma.New(uma.Config{
		Store: counting, Authorizer: env.policy, Clock: env.clock, IDs: env.ids,
		Adapters: []adapter.Factory{serviceAdapter{}},
	})
	if err != nil {
		env.t.Fatalf("uma.New: %v", err)
	}
	env.t.Cleanup(func() { _ = e.Close(context.Background()) })
	return counting, e
}

func (env *serviceEnv) clientOf(e uma.Engine, tenant model.TenantID, actor model.ActorID) uma.Client {
	env.t.Helper()
	c, err := e.For(model.Principal{Tenant: tenant, Actor: actor})
	if err != nil {
		env.t.Fatal(err)
	}
	return c
}

// countingStore counts the units opened through it.
type countingStore struct {
	store.Store
	views, transacts atomic.Int32
}

func (s *countingStore) reset() {
	s.views.Store(0)
	s.transacts.Store(0)
}

func (s *countingStore) View(ctx context.Context, t model.TenantID, fn func(store.Reader) error) error {
	s.views.Add(1)
	return s.Store.View(ctx, t, fn)
}

func (s *countingStore) Transact(ctx context.Context, t model.TenantID, fn func(store.Tx) error) error {
	s.transacts.Add(1)
	return s.Store.Transact(ctx, t, fn)
}
