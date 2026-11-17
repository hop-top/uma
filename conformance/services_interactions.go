package conformance

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// interactionCase is one case of the Interactions group of RunServiceSuite.
type interactionCase struct {
	name string
	run  func(*testing.T, *interactionEnv)
}

// interactionServiceCases run Interactions.Get, List, Respond and the
// follow-up Ephemeral through the engine over the store under test. The
// group is RunServiceSuite's, so every store passes it.
var interactionServiceCases = slices.Concat(interactionReadCases, interactionRespondCases, interactionWinnerCases)

// refsOf returns the references of a page of interactions.
func refsOf(p model.Page[model.Interaction]) []model.InteractionRef {
	var out []model.InteractionRef
	for _, in := range p.Items {
		out = append(out, in.Ref)
	}
	return out
}

func (env *interactionEnv) list(f model.Filter, limit int, after model.Cursor) (model.Page[model.Interaction], error) {
	return env.client.Interactions().List(env.ctx, model.ListRequest{Call: model.Call{Correlation: "list"}, Filter: f, Page: model.PageRequest{Limit: limit, After: after}})
}

func (env *interactionEnv) get(ref model.InteractionRef) (model.Interaction, error) {
	return env.client.Interactions().Get(env.ctx, model.Lookup[model.InteractionRef]{Call: model.Call{Correlation: "get"}, Ref: ref})
}

// noToken fails when anything the engine stores or emits for the operations
// holds the response token: the operation records with their intents, their
// attempt evidence, every event and every message.
func (env *interactionEnv) noToken(ops ...model.OperationRef) {
	env.t.Helper()
	var dump strings.Builder
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) error {
		for _, op := range ops {
			rec, err := r.Operation(env.ctx, op)
			if err != nil {
				return err
			}
			fmt.Fprintf(&dump, "%+v %s", rec, rec.Command.Data)
			at, err := r.Attempts(env.ctx, op, model.PageRequest{Limit: 100})
			if err != nil {
				return err
			}
			fmt.Fprintf(&dump, "%+v", at.Items)
		}
		return nil
	})
	if err != nil {
		env.t.Fatalf("View: %v", err)
	}
	for _, ev := range env.events() {
		fmt.Fprintf(&dump, "%+v %s", ev, ev.Payload.Data)
	}
	for _, s := range env.sites {
		fmt.Fprintf(&dump, "%+v", env.messages(s))
	}
	if strings.Contains(dump.String(), interactionToken) {
		env.t.Fatal("the response token leaked into the ledger, attempt evidence, events or messages")
	}
}

var interactionReadCases = []interactionCase{
	{"GetUnderTheRecordAudience", func(t *testing.T, env *interactionEnv) {
		s := env.sites[interactiveType]
		in := env.seed(s)
		hidden := env.seed(s, func(r *store.InteractionRecord) { r.Interaction.Audience = model.Audience{Visibility: model.Internal} })
		got, err := env.get(in.Ref)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Ref != in.Ref || got.Kind != model.InteractionAction || got.Action != "approve" || got.Revision != in.Revision {
			t.Fatalf("Get = %+v, want the stored interaction", got)
		}
		if strings.Contains(fmt.Sprintf("%+v", got), interactionToken) {
			t.Fatalf("Get result holds the response token: %+v", got)
		}
		env.policy.denyOn(model.ActionInteractionRead, hidden.Ref.Ref())
		_, err = env.get(hidden.Ref)
		wantCode(t, err, model.Unauthorized)
	}},
	{"GetOfAnotherScopeIsNotFound", func(t *testing.T, env *interactionEnv) {
		s := env.sites[interactiveType]
		in := env.seed(s)
		other := model.ConversationRef{Inbox: s.inbox, ID: env.id()}
		foreign := model.InteractionRef{Conversation: model.ConversationRef{
			Inbox: model.InboxRef{Tenant: tenantB, ID: s.inbox.ID}, ID: s.conv.Ref.ID,
		}, ID: in.Ref.ID}
		for name, ref := range map[string]model.InteractionRef{
			"missing":            {Conversation: s.conv.Ref, ID: env.id()},
			"other conversation": {Conversation: other, ID: in.Ref.ID},
			"other tenant":       foreign,
		} {
			_, err := env.get(ref)
			if !model.IsCode(err, model.NotFound) {
				t.Errorf("%s: Get = %v, want not found", name, err)
			}
		}
	}},
	{"ListPagesPerConversationInboxAndActor", func(t *testing.T, env *interactionEnv) {
		s := env.sites[interactiveType]
		var mine []model.InteractionRef
		for range 5 {
			mine = append(mine, env.seed(s).Ref)
		}
		// Another conversation of the inbox, with another actor.
		conv2 := s.conv
		conv2.Ref.ID, conv2.Participants, conv2.ReplyRoute.ID = env.id(), nil, "room-2"
		env.put(func(tx store.Tx) error { return tx.PutConversation(env.ctx, conv2, "") })
		s2 := s
		s2.conv = conv2
		theirs := env.seed(s2)
		other := env.sites[respondOnlyType]
		env.seed(other)

		conv := s.conv.Ref
		p, err := env.list(model.Filter{Conversation: &conv}, 0, "")
		if err != nil || !slices.Equal(refsOf(p), mine) || !p.Complete {
			t.Fatalf("conversation list = %v (complete %v), %v; want %v", refsOf(p), p.Complete, err, mine)
		}
		var walked []model.InteractionRef
		var after model.Cursor
		for range 10 {
			p, err := env.list(model.Filter{Conversation: &conv}, 2, after)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(p.Items) > 2 {
				t.Fatalf("page of %d items, limit 2", len(p.Items))
			}
			walked = append(walked, refsOf(p)...)
			if p.Complete {
				break
			}
			after = p.Next
		}
		if !slices.Equal(walked, mine) {
			t.Fatalf("paged = %v, want %v", walked, mine)
		}
		p, err = env.list(model.Filter{Inboxes: []model.InboxRef{s.inbox}}, 0, "")
		if err != nil || len(p.Items) != 6 || p.Items[5].Ref != theirs.Ref {
			t.Fatalf("inbox list = %v, %v; want the six interactions of the inbox in recording order", refsOf(p), err)
		}
		actor := s.actor.Ref
		if p, err = env.list(model.Filter{Identity: &actor}, 0, ""); err != nil || len(p.Items) != 6 {
			t.Fatalf("identity list = %v, %v; want the inbox actor's six", refsOf(p), err)
		}
		if strings.Contains(fmt.Sprintf("%+v", p), interactionToken) {
			t.Fatalf("List result holds the response token: %+v", p)
		}
	}},
	{"ListSkipsUnreadableWithoutTrace", func(t *testing.T, env *interactionEnv) {
		s := env.sites[interactiveType]
		a := env.seed(s)
		hidden := env.seed(s, func(r *store.InteractionRecord) { r.Interaction.Audience = model.Audience{Visibility: model.Internal} })
		c := env.seed(s)
		env.policy.denyOn(model.ActionInteractionRead, hidden.Ref.Ref())
		p, err := env.list(model.Filter{}, 0, "")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if got := refsOf(p); !slices.Equal(got, []model.InteractionRef{a.Ref, c.Ref}) {
			t.Fatalf("List = %v, want the two readable interactions", got)
		}
	}},
	{"ListFilterRules", func(t *testing.T, env *interactionEnv) {
		s := env.sites[interactiveType]
		env.seed(s)
		contact := model.ContactRef{Tenant: serviceTenant, ID: s.actor.Contact.ID}
		_, err := env.list(model.Filter{Contact: &contact}, 0, "")
		wantCode(t, err, model.Validation)
		_, err = env.list(model.Filter{Kinds: []string{"action"}}, 0, "")
		wantCode(t, err, model.Validation)
		foreign := model.ConversationRef{Inbox: model.InboxRef{Tenant: tenantB, ID: s.inbox.ID}, ID: s.conv.Ref.ID}
		_, err = env.list(model.Filter{Conversation: &foreign}, 0, "")
		wantCode(t, err, model.NotFound)
		_, err = env.list(model.Filter{}, 0, "q1.forged")
		wantCode(t, err, model.Validation)
	}},
}

// decodeUpdate returns the payload of an interaction.updated event.
func decodeUpdate(t *testing.T, ev model.Event) model.InteractionUpdatedPayload {
	t.Helper()
	var u model.InteractionUpdatedPayload
	if err := json.Unmarshal(ev.Payload.Data, &u); err != nil {
		t.Fatalf("interaction.updated payload: %v", err)
	}
	return u
}

var interactionRespondCases = []interactionCase{
	{"ModesAndAudiencesAreApplied", func(t *testing.T, env *interactionEnv) {
		s := env.sites[interactiveType]
		for _, mode := range []string{model.ResponseModeMessage, model.ResponseModeEphemeral, model.ResponseModeUpdate} {
			env.provider.reset()
			before := len(env.messages(s))
			in := env.seed(s)
			req := env.request(in, mode)
			a := env.respond(req)
			if op := env.deliver(a.Operation.Ref); op.State != model.ProviderAccepted {
				t.Fatalf("%s: settled %+v", mode, op)
			}
			calls := env.provider.all()
			if len(calls) != 1 {
				t.Fatalf("%s: provider calls = %+v, want one", mode, calls)
			}
			c := calls[0]
			if c.Method != "respond" || c.Mode != mode || c.FollowUp || c.Audience != req.Audience || c.Text != "answer" ||
				c.Interaction == nil || c.Interaction.Ref != in.Ref || c.Interaction.Token != interactionToken {
				t.Fatalf("%s: provider call = %+v, want an initial response in that mode and audience carrying the interaction", mode, c)
			}
			if (mode == model.ResponseModeUpdate) != (c.Target != nil) || mode == model.ResponseModeUpdate && *c.Target != *in.MessageExternal {
				t.Fatalf("%s: target = %v, want the control's message for an update only", mode, c.Target)
			}
			msgs := env.messages(s)
			if posted := mode == model.ResponseModeMessage; posted != (len(msgs) == before+1) || !posted && len(msgs) != before {
				t.Fatalf("%s: %d messages, was %d: only a message mode response stores one", mode, len(msgs), before)
			}
			got := env.stored(in.Ref)
			if got.Response.Initial == nil || *got.Response.Initial != a.Operation.Ref || got.Response.FollowUps != 0 || got.Revision == in.Revision {
				t.Fatalf("%s: interaction after = %+v, want the response claimed by its operation", mode, got)
			}
			env.noToken(a.Operation.Ref)
		}
	}},
	{"MessageModeCommitsTheCorrelatedMessage", func(t *testing.T, env *interactionEnv) {
		s := env.sites[interactiveType]
		in := env.seed(s)
		a := env.respond(env.request(in, model.ResponseModeMessage))
		if a.Operation.ResultMessage == nil {
			t.Fatalf("acceptance = %+v, want the message the response posts", a.Operation)
		}
		msgs := env.messages(s)
		if len(msgs) != 1 || msgs[0].Ref != *a.Operation.ResultMessage || contentText(msgs[0].Content) != "answer" {
			t.Fatalf("messages = %+v, want the committed response message", msgs)
		}
		created := env.events(model.EventMessageCreated)
		ups := env.events(model.EventInteractionUpdated)
		if len(created) != 1 || len(ups) != 1 {
			t.Fatalf("%d message.created and %d interaction.updated events, want one each", len(created), len(ups))
		}
		if u := decodeUpdate(t, ups[0]); u.Change != model.InteractionChangeResponded || u.Operation == nil || *u.Operation != a.Operation.Ref || u.PreviousRevision != in.Revision {
			t.Fatalf("interaction.updated payload = %+v", u)
		}
	}},
	{"RefusedRequestsClaimNothing", func(t *testing.T, env *interactionEnv) {
		s := env.sites[interactiveType]
		in := env.seed(s)
		noMessage := env.seed(s, func(r *store.InteractionRecord) { r.Interaction.MessageExternal = nil })
		public := env.request(in, model.ResponseModeEphemeral)
		public.Audience = model.Audience{Visibility: model.Public}
		for name, c := range map[string]struct {
			req  model.Respond
			code model.ErrorCode
		}{
			"public ephemeral":         {public, model.Validation},
			"update without a message": {env.request(noMessage, model.ResponseModeUpdate), model.Validation},
			"past the window": {func() model.Respond {
				r := env.request(in, model.ResponseModeMessage)
				r.ExpiresAt = in.RespondUntil.Add(time.Minute)
				return r
			}(), model.Validation},
		} {
			_, err := env.client.Interactions().Respond(env.ctx, c.req)
			if !model.IsCode(err, c.code) {
				t.Errorf("%s: Respond = %v, want %s", name, err, c.code)
			}
		}
		for _, want := range []model.Interaction{in, noMessage} {
			if got := env.stored(want.Ref); got.Response.Initial != nil || got.Revision != want.Revision {
				t.Errorf("interaction after refusals = %+v, want it untouched", got)
			}
		}
	}},
	{"DeniedRespondIsUnauthorizedAndClaimsNothing", func(t *testing.T, env *interactionEnv) {
		s := env.sites[interactiveType]
		in := env.seed(s, func(r *store.InteractionRecord) {
			r.Interaction.Audience = model.Audience{Visibility: model.Internal, PolicyRef: "staff"}
		})
		env.policy.deny(model.ActionInteractionRespond)
		_, err := env.client.Interactions().Respond(env.ctx, env.request(in, model.ResponseModeMessage))
		wantCode(t, err, model.Unauthorized)
		if got := env.stored(in.Ref); got.Response.Initial != nil || got.Revision != in.Revision {
			t.Fatalf("interaction after a denied respond = %+v", got)
		}
	}},
}
