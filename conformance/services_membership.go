package conformance

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// membershipCases run Conversations.AddParticipant and RemoveParticipant
// through the operation ledger on the mutation env: the adapter of its
// main channel declares membership, the send-only channel's does not.
var membershipCases = []struct {
	name string
	run  func(*testing.T, *mutationEnv)
}{
	{"AddAndRemoveThroughTheLedger", membershipLifecycle},
	{"UndeclaredAdapterRefusedAtSubmit", membershipUndeclared},
	{"RefusedChanges", membershipRefused},
	{"RejectedChangeLeavesTheConversation", membershipRejected},
	{"LostAnswerIsNotRepeated", membershipLostAnswer},
}

// ChangeMembership records the change in the provider and answers it
// applied, unless the case scripted another answer.
func (i *mutationInstance) ChangeMembership(_ context.Context, r adapter.MembershipRequest) (adapter.Outcome, error) {
	return i.p.mutate(model.CapabilityMembership, r.Effect, r.Participant.Namespace, r.Participant.Kind, r.Participant.ID, strconv.FormatBool(r.Remove))
}

var _ adapter.MembershipChanger = (*mutationInstance)(nil)

// membershipCalls returns the membership changes that reached the
// provider, each as "namespace,kind,source ID,remove".
func (p *mutationProvider) membershipCalls() (effects []adapter.Effect, changes []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, c := range p.kinds {
		if c == model.CapabilityMembership {
			effects = append(effects, p.calls[i])
			changes = append(changes, p.details[i])
		}
	}
	return effects, changes
}

// identity stores an identity of the inbox of conversation conv.
func (env *mutationEnv) identity(conv model.ConversationRef, source string) model.Identity {
	env.t.Helper()
	id := model.Identity{
		Ref:     model.IdentityRef{Inbox: conv.Inbox, ID: env.id()},
		Contact: model.ContactRef{Tenant: serviceTenant, ID: env.id()}, SourceID: source,
		Namespace: "com.example.conformance.user", Kind: model.ContactKindPerson, Revision: "id1",
	}
	env.put(func(tx store.Tx) error {
		_, _, err := tx.ClaimIdentity(env.ctx, model.IdentityKey{Inbox: conv.Inbox, Namespace: id.Namespace, SourceID: id.SourceID}, id)
		return err
	})
	return id
}

func (env *mutationEnv) conversation(ref model.ConversationRef) model.Conversation {
	env.t.Helper()
	c, err := env.client.Conversations().Get(env.ctx, model.Lookup[model.ConversationRef]{Call: model.Call{Correlation: "get"}, Ref: ref})
	if err != nil {
		env.t.Fatalf("Conversations.Get: %v", err)
	}
	return c
}

func (env *mutationEnv) participantChange(conv model.ConversationRef, id model.IdentityRef, rev model.Revision) model.ParticipantChange {
	return model.ParticipantChange{Command: env.cmd(rev), Conversation: conv, Identity: id}
}

// membershipEvents returns the conversation.changed events of conv that
// report participants.
func (env *mutationEnv) membershipEvents(conv model.ConversationRef) []model.ConversationChangedPayload {
	env.t.Helper()
	var out []model.ConversationChangedPayload
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) error {
		after, err := r.EventBase(env.ctx)
		if err != nil {
			return err
		}
		for {
			p, err := r.Events(env.ctx, model.Filter{Kinds: []string{model.EventConversationChanged}}, after, 100)
			if err != nil {
				return err
			}
			for _, ev := range p.Items {
				var cp model.ConversationChangedPayload
				if err := json.Unmarshal(ev.Payload.Data, &cp); err != nil {
					return err
				}
				if cp.Conversation == conv && len(cp.Changes) == 1 && cp.Changes[0] == model.ConversationFieldParticipants {
					if ev.Audience.Visibility != model.Internal || ev.Actor != mutationActor {
						env.t.Errorf("membership event audience %+v actor %q, want internal by %s", ev.Audience, ev.Actor, mutationActor)
					}
					out = append(out, cp)
				}
			}
			if p.Complete || len(p.Items) == 0 {
				return nil
			}
			after = p.Next
		}
	})
	if err != nil {
		env.t.Fatalf("events: %v", err)
	}
	return out
}

func hasParticipant(c model.Conversation, id model.IdentityRef) bool {
	for _, p := range c.Participants {
		if p.Identity == id {
			return p.Role == "participant"
		}
	}
	return false
}

// membershipLifecycle: an added identity is queued with the conversation as
// result and the conversation untouched; the runtime asks the provider
// once with the identity as the adapter reports it, and only then the
// identity joins the participants, with conversation.changed. A removal
// runs the same way. A retry replays; the other method under the same
// operation ID conflicts.
func membershipLifecycle(t *testing.T, env *mutationEnv) {
	conv := env.target.Ref.Conversation
	ada := env.identity(conv, "ada")
	before := env.conversation(conv)
	convs := env.client.Conversations()
	req := env.participantChange(conv, ada.Ref, before.Revision)
	a, err := convs.AddParticipant(env.ctx, req)
	if err != nil {
		t.Fatalf("AddParticipant: %v", err)
	}
	op := a.Operation
	if op.State != model.Queued || a.Replayed || op.Result == nil || *op.Result != conv.Ref() || op.ResultMessage != nil {
		t.Fatalf("AddParticipant acceptance = %+v", a)
	}
	if k := op.Key; k == nil || k.Kind != model.ActionConversationMembership || k.Target != conv.Ref() || k.ID != req.Operation {
		t.Fatalf("AddParticipant key = %+v, want conversation.membership on the conversation", op.Key)
	}
	if got := env.conversation(conv); got.Revision != before.Revision || hasParticipant(got, ada.Ref) {
		t.Fatalf("conversation at claim = %+v, want it untouched", got)
	}
	if again, err := convs.AddParticipant(env.ctx, req); err != nil || !again.Replayed || again.Operation.Ref != op.Ref {
		t.Fatalf("retry = %+v, %v; want the stored operation", again, err)
	}
	_, err = convs.RemoveParticipant(env.ctx, req)
	serviceCode(t, err, model.Conflict)

	if done := env.deliver(op.Ref); done.State != model.ProviderAccepted {
		t.Fatalf("added participant = %+v, want provider_accepted", done)
	}
	effects, changes := env.provider.membershipCalls()
	want := "com.example.conformance.user," + string(model.KindIdentity) + ",ada,false"
	if len(effects) != 1 || changes[0] != want || effects[0].Operation != op.Ref || effects[0].Target != nil ||
		effects[0].Route != before.ReplyRoute || effects[0].Audience.Visibility != model.Internal {
		t.Fatalf("membership calls = %+v %v, want one add of %s on the conversation's route", effects, changes, want)
	}
	added := env.conversation(conv)
	if !hasParticipant(added, ada.Ref) || added.Revision == before.Revision {
		t.Fatalf("conversation after the add = %+v, want ada a participant at a new revision", added)
	}
	evs := env.membershipEvents(conv)
	if len(evs) != 1 || evs[0].Operation == nil || *evs[0].Operation != op.Ref || evs[0].PreviousRevision != before.Revision ||
		evs[0].Trigger != model.TriggerCommand {
		t.Fatalf("membership events = %+v, want one conversation.changed participants under the operation", evs)
	}

	rm, err := convs.RemoveParticipant(env.ctx, env.participantChange(conv, ada.Ref, ""))
	if err != nil {
		t.Fatalf("RemoveParticipant: %v", err)
	}
	if got := env.conversation(conv); !hasParticipant(got, ada.Ref) {
		t.Fatalf("participant removed at claim: %+v", got)
	}
	env.deliver(rm.Operation.Ref)
	if _, changes := env.provider.membershipCalls(); len(changes) != 2 || changes[1] != "com.example.conformance.user,"+string(model.KindIdentity)+",ada,true" {
		t.Fatalf("membership calls = %v, want the add then the removal", changes)
	}
	removed := env.conversation(conv)
	if hasParticipant(removed, ada.Ref) || removed.Revision == added.Revision {
		t.Fatalf("conversation after the removal = %+v, want ada gone at a new revision", removed)
	}
	if evs := env.membershipEvents(conv); len(evs) != 2 || *evs[1].Operation != rm.Operation.Ref {
		t.Fatalf("membership events = %+v, want the add's then the removal's", evs)
	}
	if again, err := convs.AddParticipant(env.ctx, req); err != nil || !again.Replayed || again.Operation.State != model.ProviderAccepted {
		t.Fatalf("retry after settling = %+v, %v; want the settled operation", again, err)
	}
	env.drain()
	if effects, _ := env.provider.membershipCalls(); len(effects) != 2 {
		t.Fatalf("%d membership calls after the retry, want 2", len(effects))
	}
}

// membershipUndeclared: a conversation whose channel adapter does not
// declare membership is refused at the call, before anything is stored.
func membershipUndeclared(t *testing.T, env *mutationEnv) {
	conv := env.plain.Ref.Conversation
	id := env.identity(conv, "bob")
	_, err := env.client.Conversations().AddParticipant(env.ctx, env.participantChange(conv, id.Ref, ""))
	serviceCode(t, err, model.Unsupported)
	p, err := env.client.Operations().List(env.ctx, serviceList(model.Filter{Kinds: []string{model.ActionConversationMembership}}, 100, ""))
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("an unsupported membership change stored %+v, %v", p.Items, err)
	}
}

// membershipRefused: pointless or unsafe changes are refused before
// anything is queued: adding a participant, removing an identity that is
// none or the owner, a stale revision, an unknown identity or conversation,
// another inbox's identity and another tenant's conversation.
func membershipRefused(t *testing.T, env *mutationEnv) {
	conv := env.target.Ref.Conversation
	member, owner, stranger := env.identity(conv, "member"), env.identity(conv, "owner"), env.identity(conv, "stranger")
	other := env.identity(env.plain.Ref.Conversation, "elsewhere")
	env.put(func(tx store.Tx) error {
		cur, err := tx.GetConversation(env.ctx, conv)
		if err != nil {
			return err
		}
		next := cur.Clone()
		next.Owner = &owner.Ref
		next.Participants = []model.Participant{{Identity: owner.Ref, Role: "contact"}, {Identity: member.Ref, Role: "participant"}}
		next.Revision = cur.Revision + "-seeded"
		return tx.PutConversation(env.ctx, next, cur.Revision)
	})
	convs := env.client.Conversations()
	unknownConv := model.ConversationRef{Inbox: conv.Inbox, ID: env.id()}
	foreign := model.ConversationRef{Inbox: model.InboxRef{Tenant: "conformance-other", ID: conv.Inbox.ID}, ID: conv.ID}
	for name, c := range map[string]struct {
		remove bool
		req    model.ParticipantChange
		code   model.ErrorCode
	}{
		"add a participant":        {false, env.participantChange(conv, member.Ref, ""), model.Conflict},
		"remove a non-participant": {true, env.participantChange(conv, stranger.Ref, ""), model.Conflict},
		"remove the owner":         {true, env.participantChange(conv, owner.Ref, ""), model.Conflict},
		"stale revision":           {false, env.participantChange(conv, stranger.Ref, "stale"), model.Conflict},
		"unknown identity":         {false, env.participantChange(conv, model.IdentityRef{Inbox: conv.Inbox, ID: env.id()}, ""), model.NotFound},
		"unknown conversation":     {false, env.participantChange(unknownConv, model.IdentityRef{Inbox: conv.Inbox, ID: stranger.Ref.ID}, ""), model.NotFound},
		"another inbox's identity": {false, env.participantChange(conv, other.Ref, ""), model.Validation},
		"another tenant":           {false, env.participantChange(foreign, model.IdentityRef{Inbox: foreign.Inbox, ID: stranger.Ref.ID}, ""), model.NotFound},
	} {
		var err error
		if c.remove {
			_, err = convs.RemoveParticipant(env.ctx, c.req)
		} else {
			_, err = convs.AddParticipant(env.ctx, c.req)
		}
		if model.CodeOf(err) != c.code {
			t.Errorf("%s: error = %v, want %s", name, err, c.code)
		}
	}
	p, err := env.client.Operations().List(env.ctx, serviceList(model.Filter{Kinds: []string{model.ActionConversationMembership}}, 100, ""))
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("refused membership changes stored %+v, %v", p.Items, err)
	}
	// The current revision is accepted.
	cur := env.conversation(conv)
	if _, err := convs.AddParticipant(env.ctx, env.participantChange(conv, stranger.Ref, cur.Revision)); err != nil {
		t.Fatalf("AddParticipant at the current revision: %v", err)
	}
}

// membershipRejected: a change the provider refuses settles rejected and
// leaves the conversation and its events as they were.
func membershipRejected(t *testing.T, env *mutationEnv) {
	conv := env.target.Ref.Conversation
	id := env.identity(conv, "refused")
	env.provider.mu.Lock()
	env.provider.answer = func(c model.Capability) (adapter.Outcome, bool) {
		return adapter.Outcome{State: model.Rejected, Failure: &model.Error{Code: model.Unauthorized, SafeMessage: "not allowed", Retry: model.NoRetry}},
			c == model.CapabilityMembership
	}
	env.provider.mu.Unlock()
	before := env.conversation(conv)
	a, err := env.client.Conversations().AddParticipant(env.ctx, env.participantChange(conv, id.Ref, ""))
	if err != nil {
		t.Fatalf("AddParticipant: %v", err)
	}
	if op := env.deliver(a.Operation.Ref); op.State != model.Rejected {
		t.Fatalf("refused change = %+v, want rejected", op)
	}
	if got := env.conversation(conv); got.Revision != before.Revision || hasParticipant(got, id.Ref) {
		t.Fatalf("conversation after a rejected change = %+v, want it untouched", got)
	}
	if evs := env.membershipEvents(conv); len(evs) != 0 {
		t.Fatalf("a rejected change emitted %+v", evs)
	}
}

// membershipLostAnswer: an attempt whose answer was lost leaves the
// operation outcome_unknown and the conversation untouched; with no
// reconcile on the adapter nothing asks the provider again.
func membershipLostAnswer(t *testing.T, env *mutationEnv) {
	conv := env.target.Ref.Conversation
	id := env.identity(conv, "lost")
	env.provider.mu.Lock()
	env.provider.answer = func(c model.Capability) (adapter.Outcome, bool) {
		return adapter.Outcome{State: model.Unknown}, c == model.CapabilityMembership
	}
	env.provider.mu.Unlock()
	req := env.participantChange(conv, id.Ref, "")
	a, err := env.client.Conversations().AddParticipant(env.ctx, req)
	if err != nil {
		t.Fatalf("AddParticipant: %v", err)
	}
	if op := env.deliver(a.Operation.Ref); op.State != model.Unknown {
		t.Fatalf("change with a lost answer = %+v, want outcome_unknown", op)
	}
	env.drain()
	if again, err := env.client.Conversations().AddParticipant(env.ctx, req); err != nil || !again.Replayed {
		t.Fatalf("retry = %+v, %v; want the stored operation", again, err)
	}
	env.drain()
	if effects, _ := env.provider.membershipCalls(); len(effects) != 1 {
		t.Fatalf("%d membership calls, want the one attempt", len(effects))
	}
	if got := env.conversation(conv); hasParticipant(got, id.Ref) {
		t.Fatalf("participant recorded without an applied answer: %+v", got)
	}
}
