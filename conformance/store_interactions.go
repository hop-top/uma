package conformance

import (
	"reflect"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// interactionRecordCases cover interaction records: create-only put, the reads
// that never return the response token, the one read that does, list
// order, filters and paging, scope, and rollback.
var interactionRecordCases = []storeCase{
	{"InteractionRoundTripsAndKeepsTheTokenApart", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		rec := e.interactionRecord(t, conv, "secret-token")
		e.mustTx(t, func(tx store.Tx) error {
			if err := tx.PutInteraction(e.ctx, rec); err != nil {
				return err
			}
			// Read-your-writes inside the unit.
			got, err := tx.Interaction(e.ctx, rec.Interaction.Ref)
			if err == nil && !reflect.DeepEqual(got, rec.Interaction) {
				t.Errorf("Interaction in unit = %+v, want %+v", got, rec.Interaction)
			}
			return err
		})
		e.wantInteraction(t, rec.Interaction)
		e.wantToken(t, rec.Interaction.Ref, store.ResponseToken{Token: "secret-token", ExpiresAt: rec.TokenExpiresAt})
		e.mustView(t, func(r store.Reader) error {
			p, err := r.Interactions(e.ctx, list(10))
			if err == nil && (len(p.Items) != 1 || !reflect.DeepEqual(p.Items[0], rec.Interaction)) {
				t.Errorf("Interactions = %+v, want the one record without a token", p.Items)
			}
			return err
		})
	}},
	{"InteractionWithoutATokenHasNoneToReturn", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		rec := e.interactionRecord(t, conv, "")
		e.mustTx(t, func(tx store.Tx) error { return tx.PutInteraction(e.ctx, rec) })
		e.wantInteraction(t, rec.Interaction)
		e.wantToken(t, rec.Interaction.Ref, store.ResponseToken{})
	}},
	{"PutInteractionOnlyCreates", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		rec := e.interactionRecord(t, conv, "first-token")
		e.mustTx(t, func(tx store.Tx) error { return tx.PutInteraction(e.ctx, rec) })
		again := e.interactionRecord(t, conv, "second-token")
		again.Interaction.Ref = rec.Interaction.Ref
		again.Interaction.Revision = "i2"
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutInteraction(e.ctx, again) }), model.Conflict)
		e.wantInteraction(t, rec.Interaction)
		e.wantToken(t, rec.Interaction.Ref, store.ResponseToken{Token: "first-token", ExpiresAt: rec.TokenExpiresAt})
	}},
	{"PutInteractionChecksTheRecord", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		other := model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}
		for name, c := range map[string]struct {
			edit func(*store.InteractionRecord)
			code model.ErrorCode
		}{
			"unknown conversation": {func(r *store.InteractionRecord) { r.Interaction.Ref.Conversation = other }, model.NotFound},
			"foreign tenant": {func(r *store.InteractionRecord) {
				r.Interaction.Ref.Conversation.Inbox.Tenant = tenantB
			}, model.NotFound},
			"no id":           {func(r *store.InteractionRecord) { r.Interaction.Ref.ID = "" }, model.Validation},
			"no revision":     {func(r *store.InteractionRecord) { r.Interaction.Revision = "" }, model.Validation},
			"no ingress":      {func(r *store.InteractionRecord) { r.Ingress = "" }, model.Validation},
			"unknown kind":    {func(r *store.InteractionRecord) { r.Interaction.Kind = "click" }, model.Validation},
			"token no expiry": {func(r *store.InteractionRecord) { r.TokenExpiresAt = time.Time{} }, model.Validation},
			"foreign actor": {func(r *store.InteractionRecord) {
				r.Interaction.Actor.Identity.Inbox.Tenant = tenantB
			}, model.NotFound},
		} {
			rec := e.interactionRecord(t, conv, "tok")
			c.edit(&rec)
			err := e.txErr(func(tx store.Tx) error {
				wantCode(t, tx.PutInteraction(e.ctx, rec), c.code)
				return nil
			})
			if err != nil {
				t.Errorf("%s: Transact: %v", name, err)
			}
		}
		e.mustView(t, func(r store.Reader) error {
			p, err := r.Interactions(e.ctx, list(10))
			if err == nil && len(p.Items) != 0 {
				t.Errorf("Interactions = %+v after refused puts, want none", p.Items)
			}
			return err
		})
	}},
	{"InteractionReadsAreNotFoundForUnknownAndForeignRefs", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		rec := e.interactionRecord(t, conv, "tok")
		e.mustTx(t, func(tx store.Tx) error { return tx.PutInteraction(e.ctx, rec) })
		missing := model.InteractionRef{Conversation: conv, ID: e.id(t)}
		wrongConv := model.InteractionRef{Conversation: model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, ID: rec.Interaction.Ref.ID}
		e.mustView(t, func(r store.Reader) error {
			for _, c := range []struct {
				name string
				ref  model.InteractionRef
			}{{"unknown interaction", missing}, {"interaction under another conversation", wrongConv}} {
				_, err := r.Interaction(e.ctx, c.ref)
				wantCode(t, err, model.NotFound, "Interaction of an ", c.name)
				_, err = r.InteractionToken(e.ctx, c.ref)
				wantCode(t, err, model.NotFound, "InteractionToken of an ", c.name)
			}
			_, err := r.Interaction(e.ctx, model.InteractionRef{Conversation: conv})
			wantCode(t, err, model.Validation)
			return nil
		})
		e.wantForeign(t, func(r store.Reader) error { _, err := r.Interaction(e.ctx, rec.Interaction.Ref); return err })
		e.wantForeign(t, func(r store.Reader) error { _, err := r.InteractionToken(e.ctx, rec.Interaction.Ref); return err })
		err := e.s.View(e.ctx, tenantB, func(r store.Reader) error {
			p, err := r.Interactions(e.ctx, list(10))
			if err == nil && len(p.Items) != 0 {
				t.Errorf("tenantB Interactions = %+v, want none", p.Items)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}},
	{"InteractionsListInRecordingOrderAndPage", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		var want []model.Interaction
		for range 5 {
			rec := e.interactionRecord(t, conv, "")
			// Recording order is the order of the writes, whatever the
			// recorded time or the ID says.
			rec.Interaction.RecordedAt = time.Unix(1_700_000_000, 0).UTC()
			e.mustTx(t, func(tx store.Tx) error { return tx.PutInteraction(e.ctx, rec) })
			want = append(want, rec.Interaction)
		}
		var got []model.Interaction
		var after model.Cursor
		for range 10 {
			var p model.Page[model.Interaction]
			e.mustView(t, func(r store.Reader) error {
				var err error
				p, err = r.Interactions(e.ctx, model.ListRequest{Page: model.PageRequest{Limit: 2, After: after}})
				return err
			})
			got = append(got, p.Items...)
			if p.Complete {
				break
			}
			if len(p.Items) != 2 || p.Next == "" || p.Next == after {
				t.Fatalf("incomplete page of %d items, next %q after %q, want 2 items and an advanced cursor", len(p.Items), p.Next, after)
			}
			after = p.Next
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("paged interactions = %+v, want %+v", got, want)
		}
		e.mustView(t, func(r store.Reader) error {
			_, err := r.Interactions(e.ctx, list(0))
			wantCode(t, err, model.Validation)
			_, err = r.Interactions(e.ctx, model.ListRequest{Page: model.PageRequest{Limit: 1, After: "not-a-cursor"}})
			wantCode(t, err, model.Validation)
			return nil
		})
	}},
	{"InteractionsFilterByInboxConversationAndActor", func(t *testing.T, e *env) {
		convA, convB := e.putConversation(t), e.putConversation(t)
		second := model.InboxRef{Tenant: tenantA, ID: e.id(t)}
		e.mustTx(t, func(tx store.Tx) error {
			return tx.PutInbox(e.ctx, model.Inbox{Ref: second, Channel: e.channel, Name: "sales", Destinations: []string{"sales@example.test"}, Revision: "r1"}, "")
		})
		convC := model.ConversationRef{Inbox: second, ID: e.id(t)}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutConversation(e.ctx, e.conversation(convC, "r1"), "") })

		put := func(conv model.ConversationRef, actor model.IdentityRef) model.InteractionRef {
			rec := e.interactionRecord(t, conv, "")
			rec.Interaction.Actor.Identity = actor
			e.mustTx(t, func(tx store.Tx) error { return tx.PutInteraction(e.ctx, rec) })
			return rec.Interaction.Ref
		}
		alice := model.IdentityRef{Inbox: e.inbox, ID: e.id(t)}
		bob := model.IdentityRef{Inbox: e.inbox, ID: e.id(t)}
		carol := model.IdentityRef{Inbox: second, ID: e.id(t)}
		a1, a2, b1, c1 := put(convA, alice), put(convB, alice), put(convA, bob), put(convC, carol)

		for name, c := range map[string]struct {
			filter model.Filter
			want   []model.InteractionRef
		}{
			"all":           {model.Filter{}, []model.InteractionRef{a1, a2, b1, c1}},
			"inbox":         {model.Filter{Inboxes: []model.InboxRef{e.inbox}}, []model.InteractionRef{a1, a2, b1}},
			"other inbox":   {model.Filter{Inboxes: []model.InboxRef{second}}, []model.InteractionRef{c1}},
			"both inboxes":  {model.Filter{Inboxes: []model.InboxRef{e.inbox, second}}, []model.InteractionRef{a1, a2, b1, c1}},
			"conversation":  {model.Filter{Conversation: &convA}, []model.InteractionRef{a1, b1}},
			"actor":         {model.Filter{Identity: &alice}, []model.InteractionRef{a1, a2}},
			"actor in conv": {model.Filter{Conversation: &convA, Identity: &alice}, []model.InteractionRef{a1}},
			"actor elsewhere": {model.Filter{Inboxes: []model.InboxRef{second}, Identity: &alice},
				nil},
		} {
			e.mustView(t, func(r store.Reader) error {
				p, err := r.Interactions(e.ctx, model.ListRequest{Filter: c.filter, Page: page(10)})
				if err != nil {
					return err
				}
				var got []model.InteractionRef
				for _, it := range p.Items {
					got = append(got, it.Ref)
				}
				if !reflect.DeepEqual(got, c.want) || !p.Complete {
					t.Errorf("%s: Interactions = %v complete=%v, want %v", name, got, p.Complete, c.want)
				}
				return nil
			})
		}
		e.mustView(t, func(r store.Reader) error {
			for name, f := range map[string]model.Filter{
				"kinds":    {Kinds: []string{"x"}},
				"statuses": {Statuses: []string{"open"}},
				"text":     {Text: "hello"},
			} {
				_, err := r.Interactions(e.ctx, model.ListRequest{Filter: f, Page: page(10)})
				if !model.IsCode(err, model.Validation) {
					t.Errorf("%s filter: Interactions = %v, want a validation error", name, err)
				}
			}
			foreign := model.ConversationRef{Inbox: model.InboxRef{Tenant: tenantB, ID: e.id(t)}, ID: e.id(t)}
			_, err := r.Interactions(e.ctx, model.ListRequest{Filter: model.Filter{Conversation: &foreign}, Page: page(10)})
			wantCode(t, err, model.NotFound)
			return nil
		})
	}},
	{"RolledBackInteractionLeavesNothing", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		rec := e.interactionRecord(t, conv, "tok")
		err := e.txErr(func(tx store.Tx) error {
			if err := tx.PutInteraction(e.ctx, rec); err != nil {
				return err
			}
			return errRollback
		})
		if err == nil {
			t.Fatal("Transact reported success for a rolled back unit")
		}
		e.mustView(t, func(r store.Reader) error {
			_, err := r.Interaction(e.ctx, rec.Interaction.Ref)
			wantCode(t, err, model.NotFound)
			_, err = r.InteractionToken(e.ctx, rec.Interaction.Ref)
			wantCode(t, err, model.NotFound)
			return nil
		})
	}},
}

// interactionRecord is a stored interaction in conv with every field set.
// A non-empty token is stored with an expiry of nanosecond precision,
// which is also the interaction's RespondUntil.
func (e *env) interactionRecord(t *testing.T, conv model.ConversationRef, token string) store.InteractionRecord {
	t.Helper()
	id := e.id(t)
	bot, source := e.externalRef("user", "bot-"+string(id)), e.externalRef("message", "pm-"+string(id))
	msg := model.MessageRef{Conversation: conv, ID: e.id(t)}
	at := time.Unix(1_700_000_000, 5).UTC()
	rec := store.InteractionRecord{
		Interaction: model.Interaction{
			Ref:  model.InteractionRef{Conversation: conv, ID: id},
			Kind: model.InteractionSubmission,
			Actor: model.Author{
				Identity: model.IdentityRef{Inbox: conv.Inbox, ID: e.id(t)},
				External: e.externalRef("user", "u-"+string(id)), Kind: model.ContactKindPerson, Assurance: "verified",
			},
			Bot: &bot, Action: "approve", Command: "triage", Arguments: "secret arguments",
			Fields:  model.Extension{Name: "conformance.form", Version: 3, Data: []byte(`{"f":"secret"}`)},
			Message: &msg, MessageExternal: &source,
			Context:    []model.ExternalRef{e.externalRef("thread", "t-"+string(id))},
			External:   []model.ExternalRef{e.externalRef("interaction", "ix-"+string(id))},
			Audience:   model.Audience{Visibility: model.Public},
			OccurredAt: at, RecordedAt: at.Add(time.Second), Deadline: at.Add(3 * time.Second),
			Revision: "i1", Source: "conformance.chat",
			Metadata: []model.Extension{{Name: "conformance.chat.raw", Version: 1, Data: []byte(`{"m":1}`)}},
		},
		Ingress: e.id(t),
	}
	if token != "" {
		rec.ResponseToken = token
		rec.TokenExpiresAt = at.Add(15 * time.Minute).Add(123456789)
		rec.Interaction.RespondUntil = rec.TokenExpiresAt
	}
	return rec
}

func (e *env) wantInteraction(t *testing.T, want model.Interaction) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		got, err := r.Interaction(e.ctx, want.Ref)
		if err == nil && !reflect.DeepEqual(got, want) {
			wantEqual(t, "interaction "+string(want.Ref.ID), got, want)
		}
		return err
	})
}

func (e *env) wantToken(t *testing.T, ref model.InteractionRef, want store.ResponseToken) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		got, err := r.InteractionToken(e.ctx, ref)
		if err == nil && (got.Token != want.Token || !got.ExpiresAt.Equal(want.ExpiresAt)) {
			t.Errorf("token of interaction %s = {token set: %v, expires %v}, want {token set: %v, expires %v}",
				ref.ID, got.Token != "", got.ExpiresAt, want.Token != "", want.ExpiresAt)
		}
		return err
	})
}
