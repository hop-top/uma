package conformance

import (
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
)

var stateAny = stateCapabilities

// wantStateCall requires the call to be the one applied request of ref on
// the target's conversation route, naming the target's provider message
// and no target of its own.
func wantStateCall(t *testing.T, s *stateStep, call ProviderCall, ref model.OperationRef, g mutationTarget, item model.ExternalRef) {
	t.Helper()
	if !call.Applied || call.Operation != ref {
		t.Fatalf("%s: provider call = %+v, want one applied request of %v", s.name, call, ref)
	}
	if call.Route != g.conv.ReplyRoute || !call.Target.IsZero() {
		t.Fatalf("%s: call on route %+v with target %+v, want the conversation's route %+v and no target", s.name, call.Route, call.Target, g.conv.ReplyRoute)
	}
	if !slices.Equal(call.Items, []model.ExternalRef{item}) {
		t.Fatalf("%s: call items = %v, want the conversation's provider message %v", s.name, call.Items, item)
	}
	if s.call != nil && !s.call(call) {
		t.Fatalf("%s: call = %+v does not carry the request", s.name, call)
	}
}

// wantReferencesResolved requires the provider's references of the target
// to be re-resolved after a move that moved it from old. When the provider
// no longer holds an item under old (or the harness says its moves change
// references) the stored message holds the reference the provider holds it
// under now, ahead of old, and keeps old.
func (e *adapterEnv) wantReferencesResolved(t *testing.T, g mutationTarget, old model.ExternalRef) {
	t.Helper()
	sp, ok := e.stateProvider()
	if !ok {
		return
	}
	m := e.storedMessage(t, g.msg.Ref)
	var refs []model.ExternalRef
	for _, x := range m.External {
		if x.Channel == adapterChanA && x.Namespace == model.ProviderMessageNamespace && x.Kind == model.RefKindMessage {
			refs = append(refs, x)
		}
	}
	if !slices.Contains(refs, old) {
		t.Fatalf("message references %v lost %v: the old reference is kept", m.External, old)
	}
	_, heldOld := sp.ItemState(adapterChanA, old)
	if heldOld && !e.h.MoveChangesReference {
		return
	}
	if len(refs) < 2 || refs[0] == old {
		t.Fatalf("message references %v after a move: the provider holds the item under %v and the adapter reported no new reference, want it ahead of %v", m.External, refs[0], old)
	}
	if _, held := sp.ItemState(adapterChanA, refs[0]); !held {
		t.Fatalf("the provider holds no item under the re-resolved reference %v", refs[0])
	}
}

var adapterStateCases = []adapterCase{
	{name: "StateLimitsReported", anyOf: stateAny, run: func(t *testing.T, e *adapterEnv) {
		for _, c := range e.declaredState() {
			l := e.stateLimits(t, c)
			switch c {
			case model.CapabilityMove:
				if !l.Shape.Valid() {
					t.Errorf("move reports shape %q, want labels or folder", l.Shape)
				}
			case model.CapabilityFlag:
				if len(l.Kinds) == 0 {
					t.Errorf("flag reports no kinds: the engine reads it as no support")
				}
			}
		}
	}},
	{name: "StateVerbSettlesOnlyAfterProviderApplied", anyOf: stateAny, run: func(t *testing.T, e *adapterEnv) {
		e.eachStep(t, e.stateSteps(t), func(t *testing.T, e *adapterEnv, s *stateStep) {
			g := e.stateTarget(t)
			e.prepare(t, g, s)
			base := e.wantHolds(t, g, s, false)
			item := e.itemRef(t, g)
			n0 := len(e.calls(adapterChanA, s.cap))
			a, err := s.submit(e, g.conv.Ref, "op-state")
			if err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			if a.Operation.State != model.Queued {
				t.Fatalf("state = %q, want queued", a.Operation.State)
			}
			if a.Operation.PendingState != nil {
				t.Fatalf("acceptance carries the pending change: %+v", a.Operation.PendingState)
			}
			ref := a.Operation.Ref
			if got := e.wantHolds(t, g, s, false); got.Revision != base.Revision {
				t.Fatalf("conversation revision %q at submission, want it untouched at %q: nothing applies before the provider answers", got.Revision, base.Revision)
			}
			if p := e.op(t, ref).PendingState; p == nil || p.Operation != ref || p.Conversation != g.conv.Ref {
				t.Fatalf("pending state = %+v, want the queued change of %v on the conversation", p, g.conv.Ref)
			}
			e.later(t, 0)
			op := e.op(t, ref)
			wantActApplied(t, op)
			if op.PendingState != nil {
				t.Fatalf("settled operation still pending: %+v", op.PendingState)
			}
			calls := e.calls(adapterChanA, s.cap)
			if len(calls) != n0+1 {
				t.Fatalf("provider %s calls = %d, want %d", s.cap, len(calls), n0+1)
			}
			wantStateCall(t, s, calls[n0], ref, g, item)
			if after := e.wantHolds(t, g, s, true); after.Revision == base.Revision && s.holds != nil {
				t.Fatal("an applied state change kept the conversation revision")
			}
			e.wantItem(t, g, s)
			var shown int
			for _, c := range e.stateChanges(t) {
				if c.payload.Operation != nil && *c.payload.Operation == ref {
					shown++
					if c.payload.Trigger != model.TriggerCommand {
						t.Fatalf("conversation.changed trigger = %q, want command", c.payload.Trigger)
					}
				}
			}
			if wantEvents := map[bool]int{true: 1, false: 0}[s.holds != nil]; shown != wantEvents {
				t.Fatalf("conversation.changed provider_state events of the operation = %d, want %d", shown, wantEvents)
			}
			e.stop(t)
		})
	}},
	{name: "StateScopeAuthorizesAsReported", anyOf: stateAny, run: func(t *testing.T, e *adapterEnv) {
		for _, c := range e.declaredState() {
			l := e.stateLimits(t, c)
			want, ok := l.Scope.Action()
			if !ok {
				t.Fatalf("%s reports scope %q, want account or shared", c, l.Scope)
			}
			var first *stateStep
			for _, s := range e.capabilitySteps(t, c) {
				if first == nil && s.before == nil && s.holds != nil {
					first = s
				}
			}
			if first == nil {
				continue
			}
			t.Run(string(c), func(t *testing.T) {
				fe := newAdapterEnv(t, e.h)
				g := fe.stateTarget(t)
				a, err := first.submit(fe, g.conv.Ref, "op-scope")
				if err != nil {
					t.Fatalf("%s: %v", first.name, err)
				}
				if kind := fe.operationKind(t, a.Operation.Ref); kind != want {
					t.Fatalf("operation kind = %q, want %q: the action of the reported scope %q", kind, want, l.Scope)
				}
				fe.stop(t)
			})
		}
	}},
	{name: "StateRequestHeldToReportedLimits", anyOf: []model.Capability{model.CapabilityMove, model.CapabilityFlag}, run: func(t *testing.T, e *adapterEnv) {
		g := e.stateTarget(t)
		refuse := func(name string, err error, c model.Capability) {
			t.Helper()
			if model.CodeOf(err) != model.Unsupported {
				t.Fatalf("%s = %v, want unsupported", name, err)
			}
			e.later(t, 0)
			if calls := e.calls(adapterChanA, c); len(calls) != 0 {
				t.Fatalf("provider %s calls = %+v after the refusal, want none", c, calls)
			}
		}
		if e.declares(model.CapabilityMove) {
			l := e.stateLimits(t, model.CapabilityMove)
			if l.Shape == model.MoveShapeFolder {
				_, err := e.client.Conversations().Move(e.ctx, model.MoveConversation{Command: command("op-labels"), Conversation: g.conv.Ref, Add: []string{stateLabelA}})
				refuse("label changes on a folder-shaped provider", err, model.CapabilityMove)
			}
			for _, role := range []string{model.RoleInbox, model.RoleArchive, model.RoleTrash, model.RoleJunk} {
				if slices.Contains(l.Roles, role) {
					continue
				}
				_, err := e.client.Conversations().Move(e.ctx, model.MoveConversation{Command: command("op-role"), Conversation: g.conv.Ref, Destination: model.MoveDestination{Role: role}})
				refuse("move to the unresolved role "+role, err, model.CapabilityMove)
				break
			}
		}
		if e.declares(model.CapabilityFlag) {
			l := e.stateLimits(t, model.CapabilityFlag)
			for _, kind := range []string{model.FlagStar, model.FlagImportant} {
				if slices.Contains(l.Kinds, kind) {
					continue
				}
				_, err := e.client.Conversations().Flag(e.ctx, model.FlagConversation{Command: command("op-kind"), Conversation: g.conv.Ref, Kind: kind})
				refuse("flag of the unreported kind "+kind, err, model.CapabilityFlag)
			}
		}
		e.wantUnchangedConversation(t, g)
		e.stop(t)
	}},
	{name: "StateRetriedSubmissionIsReplayed", anyOf: stateAny, run: func(t *testing.T, e *adapterEnv) {
		e.eachStep(t, e.stateSteps(t), func(t *testing.T, e *adapterEnv, s *stateStep) {
			g := e.stateTarget(t)
			e.prepare(t, g, s)
			n0 := len(e.calls(adapterChanA, s.cap))
			a, err := s.submit(e, g.conv.Ref, "op-state")
			if err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			ref := a.Operation.Ref
			again, err := s.submit(e, g.conv.Ref, "op-state")
			wantReplayed(t, again, err, ref)
			e.later(t, 0)
			wantActApplied(t, e.op(t, ref))
			again, err = s.submit(e, g.conv.Ref, "op-state")
			wantReplayed(t, again, err, ref)
			wantActApplied(t, again.Operation)
			e.later(t, 0)
			if calls := e.calls(adapterChanA, s.cap); len(calls) != n0+1 {
				t.Fatalf("provider %s calls = %d, want %d: a replayed submission is never sent again", s.cap, len(calls), n0+1)
			}
			e.stop(t)
		})
	}},
	{name: "StateAlreadyHeldIsApplied", anyOf: []model.Capability{model.CapabilityArchive, model.CapabilityReadState, model.CapabilityFlag, model.CapabilityTrash}, run: func(t *testing.T, e *adapterEnv) {
		var steps []*stateStep
		for _, s := range e.stateSteps(t) {
			if s.cap != model.CapabilityMove {
				steps = append(steps, s)
			}
		}
		e.eachStep(t, steps, func(t *testing.T, e *adapterEnv, s *stateStep) {
			g := e.stateTarget(t)
			e.prepare(t, g, s)
			e.applyStep(t, g, s, "op-first")
			ref := e.applyStep(t, g, s, "op-second")
			calls := e.calls(adapterChanA, s.cap)
			if last := calls[len(calls)-1]; !last.Applied || last.Operation != ref {
				t.Fatalf("%s: second request = %+v, want the provider to apply the state it holds already", s.name, last)
			}
			e.wantHolds(t, g, s, true)
			e.stop(t)
		})
	}},
	{name: "StateRejectedLeavesConversation", anyOf: stateAny, run: func(t *testing.T, e *adapterEnv) {
		e.eachStep(t, e.firstSteps(t), func(t *testing.T, e *adapterEnv, s *stateStep) {
			e.rejectedIsFinal(t, e.stateEffect(e.stateTarget(t), s))
		})
	}},
	{name: "StateAnswerLostIsNeverResent", anyOf: stateAny, run: func(t *testing.T, e *adapterEnv) {
		e.eachStep(t, e.firstSteps(t), func(t *testing.T, e *adapterEnv, s *stateStep) {
			e.answerLostNeverResent(t, e.stateEffect(e.stateTarget(t), s))
		})
	}},
	{name: "StateRevokedPermissionIsRefusedBeforeAnyCall", anyOf: stateAny, run: func(t *testing.T, e *adapterEnv) {
		e.eachStep(t, e.firstSteps(t), func(t *testing.T, e *adapterEnv, s *stateStep) {
			g := e.stateTarget(t)
			if err := e.provider.Revoke(adapterChanA, s.cap); err != nil {
				t.Skipf("provider cannot revoke the %s permission: %v", s.cap, err)
			}
			_, err := s.submit(e, g.conv.Ref, "op-revoked")
			if model.CodeOf(err) != model.Unauthorized {
				t.Fatalf("%s with the %s permission withdrawn = %v, want unauthorized", s.name, s.cap, err)
			}
			e.later(t, 0)
			e.later(t, time.Hour)
			if calls := e.calls(adapterChanA, s.cap); len(calls) != 0 {
				t.Fatalf("provider %s calls = %+v, want none", s.cap, calls)
			}
			e.wantUnchangedConversation(t, g)
			e.stop(t)
		})
	}},
}

var adapterStateUnsupportedCases = []adapterCase{
	{name: "StateNeedsDeclaredCapability", run: func(t *testing.T, e *adapterEnv) {
		var missing []model.Capability
		for _, c := range stateCapabilities {
			if !e.declares(c) {
				missing = append(missing, c)
			}
		}
		if len(missing) == 0 {
			t.Skip("every state capability declared")
		}
		if !e.declares(model.CapabilitySend) && !e.canReceive() {
			t.Skip("no conversation to change: capabilities send and receive not declared")
		}
		for _, c := range missing {
			t.Run(string(c), func(t *testing.T) {
				e := newAdapterEnv(t, e.h)
				g := e.stateTarget(t)
				before := len(e.provider.Calls(adapterChanA))
				_, err := e.undeclaredVerb(c, g)
				if model.CodeOf(err) != model.Unsupported {
					t.Fatalf("%s verb on a channel without it = %v, want unsupported", c, err)
				}
				e.later(t, 0)
				if calls := e.provider.Calls(adapterChanA); len(calls) != before {
					t.Fatalf("provider calls = %+v, want none after the refusal", calls[before:])
				}
				e.wantUnchangedConversation(t, g)
				e.stop(t)
			})
		}
	}},
}

// undeclaredVerb submits a verb of capability c that the adapter does not
// declare.
func (e *adapterEnv) undeclaredVerb(c model.Capability, g mutationTarget) (model.Acceptance, error) {
	conv, cl := g.conv.Ref, e.client.Conversations()
	switch c {
	case model.CapabilityArchive:
		return cl.Archive(e.ctx, model.ArchiveConversation{Command: command("op-undeclared"), Conversation: conv})
	case model.CapabilityMove:
		return cl.Move(e.ctx, model.MoveConversation{Command: command("op-undeclared"), Conversation: conv, Destination: model.MoveDestination{ID: stateFolderA}})
	case model.CapabilityReadState:
		return cl.SetRead(e.ctx, model.SetConversationRead{Command: command("op-undeclared"), Conversation: conv, Read: true})
	case model.CapabilityFlag:
		return cl.Flag(e.ctx, model.FlagConversation{Command: command("op-undeclared"), Conversation: conv, Kind: model.FlagStar})
	}
	return cl.Trash(e.ctx, model.TrashConversation{Command: command("op-undeclared"), Conversation: conv})
}

// wantUnchangedConversation requires the target's conversation to be as
// it was found, without provider state.
func (e *adapterEnv) wantUnchangedConversation(t *testing.T, g mutationTarget) {
	t.Helper()
	got := e.storedConversation(t, g.conv.Ref)
	if got.Revision != g.conv.Revision || len(got.ProviderState) != 0 {
		t.Fatalf("conversation revision %q with provider state %+v, want it untouched at %q", got.Revision, got.ProviderState, g.conv.Revision)
	}
}
