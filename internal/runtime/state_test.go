package runtime_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
)

// stateVerb is one provider state verb under test: how it is submitted,
// what the provider is asked, and what applying it records.
type stateVerb struct {
	name    string
	cap     model.Capability
	verb    model.StateVerb
	submit  func(r *rig, op model.OperationID) (model.Acceptance, error)
	call    func(c enginetest.StateCall) bool
	applied func(s model.ProviderConversationState) bool
	recon   func(q adapter.ReconcileRequest) bool
}

var stateVerbs = []stateVerb{
	{
		name: "archive", cap: model.CapabilityArchive, verb: model.StateVerbArchive,
		submit:  func(r *rig, op model.OperationID) (model.Acceptance, error) { return r.Archive(op, false) },
		call:    func(c enginetest.StateCall) bool { return !c.Undo },
		applied: func(s model.ProviderConversationState) bool { return s.Archived },
		recon:   func(q adapter.ReconcileRequest) bool { return !q.Undo },
	},
	{
		name: "move", cap: model.CapabilityMove, verb: model.StateVerbMove,
		submit: func(r *rig, op model.OperationID) (model.Acceptance, error) {
			return r.Move(op, model.MoveDestination{ID: "f-archive"})
		},
		call: func(c enginetest.StateCall) bool { return c.Destination.ID == "f-archive" },
		applied: func(s model.ProviderConversationState) bool {
			return s.Folder != nil && s.Folder.ID == "f-archive"
		},
		recon: func(q adapter.ReconcileRequest) bool { return q.Destination.ID == "f-archive" },
	},
	{
		name: "read", cap: model.CapabilityReadState, verb: model.StateVerbRead,
		submit:  func(r *rig, op model.OperationID) (model.Acceptance, error) { return r.SetRead(op, true) },
		call:    func(c enginetest.StateCall) bool { return c.Read },
		applied: func(s model.ProviderConversationState) bool { return s.Read != nil && *s.Read },
		recon:   func(q adapter.ReconcileRequest) bool { return q.Read },
	},
	{
		name: "flag", cap: model.CapabilityFlag, verb: model.StateVerbFlag,
		submit: func(r *rig, op model.OperationID) (model.Acceptance, error) { return r.Flag(op, model.FlagStar, false) },
		call:   func(c enginetest.StateCall) bool { return c.Kind == model.FlagStar && !c.Undo },
		applied: func(s model.ProviderConversationState) bool {
			return slices.Equal(s.Flags, []string{model.FlagStar})
		},
		recon: func(q adapter.ReconcileRequest) bool { return q.FlagKind == model.FlagStar && !q.Undo },
	},
	{
		name: "trash", cap: model.CapabilityTrash, verb: model.StateVerbTrash,
		submit:  func(r *rig, op model.OperationID) (model.Acceptance, error) { return r.Trash(op, false) },
		call:    func(c enginetest.StateCall) bool { return !c.Undo },
		applied: func(s model.ProviderConversationState) bool { return s.Trashed },
		recon:   func(q adapter.ReconcileRequest) bool { return !q.Undo },
	},
}

// providerStateChanges returns the conversation.changed payloads that
// report provider_state.
func providerStateChanges(t *testing.T, r *rig) []model.ConversationChangedPayload {
	t.Helper()
	var out []model.ConversationChangedPayload
	for _, ev := range r.Events() {
		if ev.Kind != model.EventConversationChanged {
			continue
		}
		var p model.ConversationChangedPayload
		if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(p.Changes, model.ConversationFieldProviderState) {
			out = append(out, p)
		}
	}
	return out
}

func stateCalls(r *rig) []enginetest.MutationCall {
	var out []enginetest.MutationCall
	for _, c := range r.F.MutationCalls() {
		if c.State != nil {
			out = append(out, c)
		}
	}
	return out
}

// Every state verb is queued without touching the conversation, shown as
// pending while unsettled, authorized as the scope's action at the call
// and at execution, sent once on the conversation's route with the
// provider messages as fan-out items and no target, and recorded in the
// conversation's provider state only once the provider applied it.
func TestStateVerbAppliedOnlyAfterProviderSuccess(t *testing.T) {
	for _, v := range stateVerbs {
		t.Run(v.name, func(t *testing.T) {
			r := newRig(t, enginetest.WithState(model.ScopeAccount))
			items := r.SeedProviderMessages(2)
			before := r.StoredConversation()
			a, err := v.submit(r, "op-state")
			if err != nil {
				t.Fatalf("submit: %v", err)
			}
			requireState(t, a.Operation, model.Queued, "")
			if a.Operation.PendingState != nil {
				t.Fatalf("acceptance carries pending state %+v", a.Operation.PendingState)
			}
			if got := r.StoredConversation(); got.Revision != before.Revision || len(got.ProviderState) != 0 {
				t.Fatalf("conversation %+v changed at the call", got)
			}
			if n := len(stateCalls(r)); n != 0 {
				t.Fatalf("provider calls at the call = %d", n)
			}
			pending := r.Op(a.Operation.Ref)
			if p := pending.PendingState; p == nil || p.Verb != v.verb || p.Conversation != r.Conv.Ref || p.Operation != a.Operation.Ref {
				t.Fatalf("pending state = %+v, want verb %s on the conversation", p, v.verb)
			}

			r.Step("w")
			done := r.Op(a.Operation.Ref)
			requireState(t, done, model.ProviderAccepted, "")
			if done.PendingState != nil {
				t.Fatalf("settled operation still pending: %+v", done.PendingState)
			}
			calls := stateCalls(r)
			if len(calls) != 1 || calls[0].Capability != v.cap || !v.call(*calls[0].State) ||
				calls[0].Effect.Target != nil || calls[0].Effect.Route != r.Conv.ReplyRoute ||
				!slices.Equal(calls[0].State.Items, items) {
				t.Fatalf("provider calls = %+v, want one %s on the route over %v", calls, v.cap, items)
			}
			if got := r.Policy.Actions(); countOf(got, model.ActionConversationStateAccount) != 2 {
				t.Fatalf("policy checks = %v, want conversation.state_account at the call and at execution", got)
			}
			st, ok := r.ProviderStateOf()
			if !ok || !v.applied(st) || st.Channel != r.Ch.Ref {
				t.Fatalf("provider state = %+v, %v; want the verb applied on the channel", st, ok)
			}
			after := r.StoredConversation()
			if after.Revision == before.Revision || after.Status != before.Status || !slices.Equal(after.Labels, before.Labels) {
				t.Fatalf("conversation = %+v, want a new revision and local status and labels untouched", after)
			}
			evs := providerStateChanges(t, r)
			if len(evs) != 1 || evs[0].Operation == nil || *evs[0].Operation != a.Operation.Ref ||
				evs[0].Trigger != model.TriggerCommand || evs[0].PreviousRevision != before.Revision {
				t.Fatalf("provider_state events = %+v, want one under the operation", evs)
			}
			if again, err := v.submit(r, "op-state"); err != nil || !again.Replayed || again.Operation.Ref != a.Operation.Ref {
				t.Fatalf("retry = %+v, %v; want the stored operation", again, err)
			}
		})
	}
}

// A provider refusal leaves the provider state as it was and emits
// nothing.
func TestStateRejectedKeepsState(t *testing.T) {
	for _, v := range stateVerbs {
		t.Run(v.name, func(t *testing.T) {
			r := newRig(t, enginetest.WithState(model.ScopeAccount))
			before := r.StoredConversation()
			r.F.Respond(rejectedAnswer)
			a, err := v.submit(r, "op-state")
			if err != nil {
				t.Fatal(err)
			}
			r.Step("w")
			op := r.Op(a.Operation.Ref)
			requireState(t, op, model.Rejected, model.Unauthorized)
			if op.PendingState != nil {
				t.Fatalf("rejected operation still pending: %+v", op.PendingState)
			}
			if got := r.StoredConversation(); got.Revision != before.Revision || len(got.ProviderState) != 0 {
				t.Fatalf("conversation = %+v, want it untouched", got)
			}
			if evs := providerStateChanges(t, r); len(evs) != 0 {
				t.Fatalf("provider_state events = %+v", evs)
			}
		})
	}
}

// A lost answer is reconciled with the verb's fields before anything is
// asked again; the reconciled change then applies.
func TestStateLostAnswerIsReconciled(t *testing.T) {
	for _, v := range stateVerbs {
		t.Run(v.name, func(t *testing.T) {
			r := newRig(t, enginetest.WithState(model.ScopeAccount), withReconciler)
			items := r.SeedProviderMessages(1)
			r.F.Respond(lostAnswer)
			a, err := v.submit(r, "op-state")
			if err != nil {
				t.Fatal(err)
			}
			r.Step("w")
			requireState(t, r.Op(a.Operation.Ref), model.Unknown, model.OutcomeUnknown)
			if _, ok := r.ProviderStateOf(); ok {
				t.Fatal("provider state recorded without an applied answer")
			}
			r.Clock.Advance(time.Minute)
			r.Step("w")
			requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
			if got := r.F.Calls(); !slices.Equal(got, []string{string(v.cap), "reconcile"}) {
				t.Fatalf("calls = %v, want the verb once, then reconciled", got)
			}
			rc := r.F.Reconciled()
			if len(rc) != 1 || rc[0].Capability != v.cap || !v.recon(rc[0]) || !slices.Equal(rc[0].Items, items) {
				t.Fatalf("reconcile = %+v, want capability %s with the verb's fields", rc, v.cap)
			}
			if st, ok := r.ProviderStateOf(); !ok || !v.applied(st) {
				t.Fatalf("provider state = %+v, %v; want the reconciled change applied", st, ok)
			}
		})
	}
}

// A move reconcile carries the destination, an undo its direction, so a
// provider that cannot repeat the call can look in the destination.
func TestStateReconcileCarriesUndoAndLabels(t *testing.T) {
	r := newRig(t, enginetest.WithState(model.ScopeAccount, model.CapabilityArchive), withReconciler)
	r.F.Respond(lostAnswer)
	a, err := r.Archive("op-undo", true)
	if err != nil {
		t.Fatal(err)
	}
	r.Step("w")
	r.Clock.Advance(time.Minute)
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	if rc := r.F.Reconciled(); len(rc) != 1 || !rc[0].Undo {
		t.Fatalf("reconcile = %+v, want Undo", rc)
	}
}

// A capability the adapter does not declare, does not report available, or
// reports without readable limits is refused before anything is stored.
func TestStateGateFailsClosedBeforeQueue(t *testing.T) {
	noLimits := func(model.Capability) model.Extension { return model.Extension{} }
	badScope := func(model.Capability) model.Extension {
		return model.Extension{Name: model.StateLimitsName, Version: 1, Data: []byte(`{"scope":"world"}`)}
	}
	for name, opts := range map[string][]enginetest.RigOption{
		"undeclared":    {},
		"other verb":    {enginetest.WithState(model.ScopeAccount, model.CapabilityTrash)},
		"no limits":     {enginetest.WithStateLimits(noLimits, enginetest.StateCapabilities...)},
		"unknown scope": {enginetest.WithStateLimits(badScope, enginetest.StateCapabilities...)},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, opts...)
			_, err := r.Archive("op-state", false)
			wantCode(t, err, model.Unsupported)
			if n := r.Step("w"); n != 0 {
				t.Fatalf("work items = %d, want nothing queued", n)
			}
			if got := countOf(r.Policy.Actions(), model.ActionConversationStateAccount); got != 0 {
				t.Fatalf("policy checked %s %d times for a refused call", model.ActionConversationStateAccount, got)
			}
		})
	}
	t.Run("withdrawn", func(t *testing.T) {
		r := newRig(t, enginetest.WithState(model.ScopeAccount))
		r.F.Withdraw(model.CapabilityArchive, model.Unauthorized)
		_, err := r.Archive("op-state", false)
		wantCode(t, err, model.Unauthorized)
		if n := r.Step("w"); n != 0 {
			t.Fatalf("work items = %d, want nothing queued", n)
		}
	})
}

// The scope the adapter reports for the verb picks the policy action.
func TestStateScopeSelectsAction(t *testing.T) {
	r := newRig(t, enginetest.WithState(model.ScopeShared))
	a, err := r.Archive("op-state", false)
	if err != nil {
		t.Fatal(err)
	}
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	got := r.Policy.Actions()
	if countOf(got, model.ActionConversationStateShared) != 2 || countOf(got, model.ActionConversationStateAccount) != 0 {
		t.Fatalf("policy checks = %v, want conversation.state_shared at the call and at execution", got)
	}
	rec := opRecord(t, r, a.Operation.Ref)
	if rec.Key.Kind != model.ActionConversationStateShared || rec.Key.Target != r.Conv.Ref.Ref() {
		t.Fatalf("operation key = %+v, want kind %s on the conversation", rec.Key, model.ActionConversationStateShared)
	}
}

// A caller denied the action is refused at the call with nothing queued; a
// permission revoked after the call makes no provider call.
func TestStateAuthorization(t *testing.T) {
	r := newRig(t, enginetest.WithState(model.ScopeAccount))
	a, err := r.Archive("op-state", false)
	if err != nil {
		t.Fatal(err)
	}
	r.Policy.Set(true)
	r.Step("w")
	r.Policy.Set(false)
	requireState(t, r.Op(a.Operation.Ref), model.Failed, model.Unauthorized)
	if n := len(stateCalls(r)); n != 0 {
		t.Fatalf("provider calls = %d, want none", n)
	}
	if _, ok := r.ProviderStateOf(); ok {
		t.Fatal("provider state recorded without a provider call")
	}
	r.Policy.Set(true)
	_, err = r.Trash("op-denied", false)
	r.Policy.Set(false)
	wantCode(t, err, model.Unauthorized)
}

// The same operation ID with another verb or payload conflicts.
func TestStateOperationIDPayloadConflict(t *testing.T) {
	r := newRig(t, enginetest.WithState(model.ScopeAccount))
	if _, err := r.Archive("op-state", false); err != nil {
		t.Fatal(err)
	}
	_, err := r.Archive("op-state", true)
	wantCode(t, err, model.Conflict)
	_, err = r.Trash("op-state", false)
	wantCode(t, err, model.Conflict)
}

// A state of the same part that advanced since the call wins: the answer
// stays on the operation, the stored state is not overwritten and nothing
// is emitted. A state that already is what was asked is an echo that beat
// the answer: nothing changes. Another part's change does not supersede.
func TestStateSupersededByNewerInbound(t *testing.T) {
	r := newRig(t, enginetest.WithState(model.ScopeAccount))
	a, err := r.Move("op-move", model.MoveDestination{ID: "f-archive"})
	if err != nil {
		t.Fatal(err)
	}
	var seeded model.Conversation
	r.F.Respond(during(func() {
		seeded = r.SetProviderState(func(s *model.ProviderConversationState) {
			s.Folder = &model.Folder{ID: "f-inbox", Name: "Inbox", Role: model.RoleInbox}
		})
	}, enginetest.Accepted))
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	st, _ := r.ProviderStateOf()
	if st.Folder == nil || st.Folder.ID != "f-inbox" {
		t.Fatalf("provider state = %+v, want the newer inbound folder kept", st)
	}
	if got := r.StoredConversation(); got.Revision != seeded.Revision {
		t.Fatalf("conversation revision %s, want it untouched at %s", got.Revision, seeded.Revision)
	}
	if evs := providerStateChanges(t, r); len(evs) != 0 {
		t.Fatalf("provider_state events = %+v, want none", evs)
	}

	echo, err := r.Archive("op-echo", false)
	if err != nil {
		t.Fatal(err)
	}
	r.F.Respond(during(func() {
		seeded = r.SetProviderState(func(s *model.ProviderConversationState) { s.Archived = true })
	}, enginetest.Accepted))
	r.Step("w")
	requireState(t, r.Op(echo.Operation.Ref), model.ProviderAccepted, "")
	if got := r.StoredConversation(); got.Revision != seeded.Revision {
		t.Fatalf("echoed archive rewrote the conversation: %+v", got)
	}
	if evs := providerStateChanges(t, r); len(evs) != 0 {
		t.Fatalf("provider_state events = %+v, want none for an echo", evs)
	}

	read, err := r.SetRead("op-read", true)
	if err != nil {
		t.Fatal(err)
	}
	r.F.Respond(during(func() {
		r.SetProviderState(func(s *model.ProviderConversationState) { s.Flags = []string{model.FlagStar} })
	}, enginetest.Accepted))
	r.Step("w")
	requireState(t, r.Op(read.Operation.Ref), model.ProviderAccepted, "")
	st, _ = r.ProviderStateOf()
	if st.Read == nil || !*st.Read || !st.Archived || !slices.Equal(st.Flags, []string{model.FlagStar}) {
		t.Fatalf("provider state = %+v, want read applied beside the inbound flag", st)
	}
}

// A read marked unread by a newer inbound report is not overwritten by an
// older read.
func TestStateReadSupersededByInboundUnread(t *testing.T) {
	r := newRig(t, enginetest.WithState(model.ScopeAccount))
	a, err := r.SetRead("op-read", true)
	if err != nil {
		t.Fatal(err)
	}
	r.F.Respond(during(func() {
		r.SetProviderState(func(s *model.ProviderConversationState) { f := false; s.Read = &f })
	}, enginetest.Accepted))
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	if st, _ := r.ProviderStateOf(); st.Read == nil || *st.Read {
		t.Fatalf("provider state = %+v, want the inbound unread kept", st)
	}
}

// Undo verbs clear the part they set.
func TestStateUndoClearsPart(t *testing.T) {
	r := newRig(t, enginetest.WithState(model.ScopeAccount))
	for _, step := range []struct {
		op     model.OperationID
		submit func() (model.Acceptance, error)
	}{
		{"op-archive", func() (model.Acceptance, error) { return r.Archive("op-archive", false) }},
		{"op-trash", func() (model.Acceptance, error) { return r.Trash("op-trash", false) }},
		{"op-star", func() (model.Acceptance, error) { return r.Flag("op-star", model.FlagStar, false) }},
		{"op-important", func() (model.Acceptance, error) { return r.Flag("op-important", model.FlagImportant, false) }},
		{"op-unread", func() (model.Acceptance, error) { return r.SetRead("op-unread", false) }},
	} {
		if _, err := step.submit(); err != nil {
			t.Fatalf("%s: %v", step.op, err)
		}
		r.Step("w")
	}
	st, _ := r.ProviderStateOf()
	if !st.Archived || !st.Trashed || !slices.Equal(st.Flags, []string{model.FlagStar, model.FlagImportant}) || st.Read == nil || *st.Read {
		t.Fatalf("provider state = %+v", st)
	}
	for _, submit := range []func() (model.Acceptance, error){
		func() (model.Acceptance, error) { return r.Archive("u-archive", true) },
		func() (model.Acceptance, error) { return r.Trash("u-trash", true) },
		func() (model.Acceptance, error) { return r.Flag("u-star", model.FlagStar, true) },
	} {
		if _, err := submit(); err != nil {
			t.Fatal(err)
		}
		r.Step("w")
	}
	st, _ = r.ProviderStateOf()
	if st.Archived || st.Trashed || !slices.Equal(st.Flags, []string{model.FlagImportant}) {
		t.Fatalf("provider state = %+v, want archive, trash and star cleared, important kept", st)
	}
}

// A move is checked against the shape and roles the adapter reported, a
// flag against its kinds, before anything is stored.
func TestStateMoveAndFlagLimits(t *testing.T) {
	limits := func(shape model.MoveShape) func(model.Capability) model.Extension {
		return func(c model.Capability) model.Extension {
			l := model.StateLimits{Scope: model.ScopeAccount}
			switch c {
			case model.CapabilityMove:
				l.Shape, l.Roles = shape, []string{model.RoleArchive}
			case model.CapabilityFlag:
				l.Kinds = []string{model.FlagStar}
			}
			return l.Extension()
		}
	}
	move := func(r *rig, op model.OperationID, req func(*model.MoveConversation)) error {
		m := model.MoveConversation{
			Command:      model.Command{Call: model.Call{Correlation: "c"}, Operation: op},
			Conversation: r.Conv.Ref,
		}
		req(&m)
		_, err := r.C.Conversations().Move(r.Ctx, m)
		return err
	}
	folder := newRig(t, enginetest.WithStateLimits(limits(model.MoveShapeFolder), enginetest.StateCapabilities...))
	wantCode(t, move(folder, "m1", func(m *model.MoveConversation) { m.Add = []string{"l1"} }), model.Unsupported)
	wantCode(t, move(folder, "m2", func(m *model.MoveConversation) { m.Destination.Role = model.RoleJunk }), model.Unsupported)
	if err := move(folder, "m3", func(m *model.MoveConversation) { m.Destination.Role = model.RoleArchive }); err != nil {
		t.Fatalf("role the adapter resolves refused: %v", err)
	}
	_, err := folder.Flag("f1", model.FlagImportant, false)
	wantCode(t, err, model.Unsupported)

	labels := newRig(t, enginetest.WithStateLimits(limits(model.MoveShapeLabels), enginetest.StateCapabilities...))
	if err := move(labels, "m4", func(m *model.MoveConversation) { m.Add, m.Remove = []string{"l1"}, []string{"INBOX"} }); err != nil {
		t.Fatalf("label move refused: %v", err)
	}
	labels.Step("w")
	calls := stateCalls(labels)
	if len(calls) != 1 || !slices.Equal(calls[0].State.Add, []string{"l1"}) || !slices.Equal(calls[0].State.Remove, []string{"INBOX"}) {
		t.Fatalf("provider calls = %+v, want the label changes", calls)
	}
	st, _ := labels.ProviderStateOf()
	if len(st.Labels) != 1 || st.Labels[0].ID != "l1" || st.Folder != nil {
		t.Fatalf("provider state = %+v, want label l1 held", st)
	}
	if n := folder.Step("w"); n != 1 {
		t.Fatalf("work items = %d, want only the accepted move queued", n)
	}
}

// A conversation of another tenant is not found.
func TestStateForeignConversationNotFound(t *testing.T) {
	r := newRig(t, enginetest.WithState(model.ScopeAccount))
	req := r.ArchiveReq("op-state", false)
	req.Conversation.Inbox.Tenant = "other"
	_, err := r.C.Conversations().Archive(r.Ctx, req)
	wantCode(t, err, model.NotFound)
}

func wantCode(t *testing.T, err error, code model.ErrorCode) {
	t.Helper()
	requireCode(t, err, code, "")
}
