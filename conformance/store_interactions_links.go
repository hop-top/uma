package conformance

import (
	"reflect"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// interactionLinkCases cover the interactions recorded before any message
// held their provider message: UnlinkedInteractions finds them by that
// provider message, exactly, and LinkInteraction names their local message
// once.
var interactionLinkCases = []storeCase{
	{"UnlinkedInteractionsMatchTheProviderMessageExactly", func(t *testing.T, e *env) {
		convA, convB := e.putConversation(t), e.putConversation(t)
		second := model.InboxRef{Tenant: tenantA, ID: e.id(t)}
		e.mustTx(t, func(tx store.Tx) error {
			return tx.PutInbox(e.ctx, model.Inbox{Ref: second, Channel: e.channel, Name: "sales", Destinations: []string{"sales@example.test"}, Revision: "r1"}, "")
		})
		convC := model.ConversationRef{Inbox: second, ID: e.id(t)}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutConversation(e.ctx, e.conversation(convC, "r1"), "") })

		source := model.ProviderMessageRef(e.channel, "pm-control")
		put := func(conv model.ConversationRef, edit func(*model.Interaction)) model.Interaction {
			rec := e.unlinkedInteraction(t, conv, source)
			edit(&rec.Interaction)
			e.mustTx(t, func(tx store.Tx) error { return tx.PutInteraction(e.ctx, rec) })
			return rec.Interaction
		}
		keep := func(*model.Interaction) {}
		first := put(convA, keep)
		put(convA, func(in *model.Interaction) { in.Message = &model.MessageRef{Conversation: convA, ID: e.id(t)} })
		put(convA, func(in *model.Interaction) { in.MessageExternal = nil })
		for _, other := range []model.ExternalRef{
			model.ProviderMessageRef(e.channel, "pm-other"),
			{Channel: e.channel, Namespace: "conformance.chat", Kind: model.RefKindMessage, ID: source.ID},
			{Channel: e.channel, Namespace: source.Namespace, Kind: "thread", ID: source.ID},
			{Channel: e.channel, Namespace: source.Namespace, Kind: source.Kind, ID: source.ID, ParentID: "room-1"},
		} {
			put(convA, func(in *model.Interaction) { in.MessageExternal = &other })
		}
		elsewhere := put(convB, keep)
		put(convC, keep)
		want := []model.Interaction{first, elsewhere}

		e.mustView(t, func(r store.Reader) error {
			p, err := r.UnlinkedInteractions(e.ctx, e.inbox, source, page(10))
			if err == nil && (!p.Complete || !reflect.DeepEqual(p.Items, want)) {
				t.Errorf("UnlinkedInteractions = %+v complete=%v, want %+v", p.Items, p.Complete, want)
			}
			return err
		})
		var got []model.Interaction
		var after model.Cursor
		for range 5 {
			var p model.Page[model.Interaction]
			e.mustView(t, func(r store.Reader) error {
				var err error
				p, err = r.UnlinkedInteractions(e.ctx, e.inbox, source, model.PageRequest{Limit: 1, After: after})
				return err
			})
			got = append(got, p.Items...)
			if p.Complete {
				break
			}
			if p.Next == "" || p.Next == after {
				t.Fatalf("incomplete page with next %q after %q, want an advanced cursor", p.Next, after)
			}
			after = p.Next
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("paged unlinked interactions = %+v, want %+v", got, want)
		}
		e.mustView(t, func(r store.Reader) error {
			_, err := r.UnlinkedInteractions(e.ctx, e.inbox, source, page(0))
			wantCode(t, err, model.Validation, "a zero limit")
			_, err = r.UnlinkedInteractions(e.ctx, e.inbox, source, model.PageRequest{Limit: 1, After: "not-a-cursor"})
			wantCode(t, err, model.Validation, "an undecodable cursor")
			foreign := model.InboxRef{Tenant: tenantB, ID: e.id(t)}
			_, err = r.UnlinkedInteractions(e.ctx, foreign, source, page(10))
			wantCode(t, err, model.NotFound, "a foreign inbox")
			return nil
		})
		e.wantForeign(t, func(r store.Reader) error {
			_, err := r.UnlinkedInteractions(e.ctx, e.inbox, source, page(10))
			return err
		})
	}},
	{"UnlinkedInteractionsSeeTheUnitsWrites", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		source := model.ProviderMessageRef(e.channel, "pm-control")
		rec := e.unlinkedInteraction(t, conv, source)
		msg := model.MessageRef{Conversation: conv, ID: e.id(t)}
		e.mustTx(t, func(tx store.Tx) error {
			if err := tx.PutInteraction(e.ctx, rec); err != nil {
				return err
			}
			p, err := tx.UnlinkedInteractions(e.ctx, e.inbox, source, page(10))
			if err != nil {
				return err
			}
			if len(p.Items) != 1 || p.Items[0].Ref != rec.Interaction.Ref {
				t.Errorf("UnlinkedInteractions after the put = %+v, want the interaction", p.Items)
			}
			if _, err := tx.LinkInteraction(e.ctx, store.InteractionLink{Interaction: rec.Interaction.Ref, Message: msg, Revision: "i2"}); err != nil {
				return err
			}
			p, err = tx.UnlinkedInteractions(e.ctx, e.inbox, source, page(10))
			if err == nil && len(p.Items) != 0 {
				t.Errorf("UnlinkedInteractions after the link = %+v, want none", p.Items)
			}
			return err
		})
	}},
	{"LinkInteractionNamesTheMessageOnce", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		source := model.ProviderMessageRef(e.channel, "pm-control")
		rec := e.unlinkedInteraction(t, conv, source)
		op := model.OperationRef{Tenant: tenantA, ID: e.id(t)}
		rec.Interaction.Response.Initial = &op
		e.mustTx(t, func(tx store.Tx) error { return tx.PutInteraction(e.ctx, rec) })
		msg := model.MessageRef{Conversation: conv, ID: e.id(t)}
		link := store.InteractionLink{Interaction: rec.Interaction.Ref, Message: msg, Revision: "i2"}

		// A rolled back link leaves the interaction unlinked.
		if err := e.txErr(func(tx store.Tx) error {
			if _, err := tx.LinkInteraction(e.ctx, link); err != nil {
				return err
			}
			return errRollback
		}); err == nil {
			t.Fatal("Transact reported success for a rolled back unit")
		}
		e.wantInteraction(t, rec.Interaction)

		want := rec.Interaction.Clone()
		want.Message, want.Revision = &msg, "i2"
		e.mustTx(t, func(tx store.Tx) error {
			got, err := tx.LinkInteraction(e.ctx, link)
			if err == nil && !reflect.DeepEqual(got, want) {
				wantEqual(t, "linked interaction", got, want)
			}
			return err
		})
		e.wantInteraction(t, want)
		e.wantToken(t, rec.Interaction.Ref, store.ResponseToken{Token: rec.ResponseToken, ExpiresAt: rec.TokenExpiresAt})
		e.mustView(t, func(r store.Reader) error {
			p, err := r.UnlinkedInteractions(e.ctx, e.inbox, source, page(10))
			if err == nil && len(p.Items) != 0 {
				t.Errorf("UnlinkedInteractions = %+v after the link, want none", p.Items)
			}
			return err
		})

		// Linking again to the same message changes nothing; to another
		// message is a conflict.
		e.mustTx(t, func(tx store.Tx) error {
			again := link
			again.Revision = "i3"
			got, err := tx.LinkInteraction(e.ctx, again)
			if err == nil && !reflect.DeepEqual(got, want) {
				wantEqual(t, "interaction linked again", got, want)
			}
			return err
		})
		other := store.InteractionLink{Interaction: rec.Interaction.Ref, Message: model.MessageRef{Conversation: conv, ID: e.id(t)}, Revision: "i4"}
		_, err := e.linkErr(other)
		wantCode(t, err, model.Conflict, "a link to another message")
		e.wantInteraction(t, want)
	}},
	{"LinkInteractionChecksTheLink", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		source := model.ProviderMessageRef(e.channel, "pm-control")
		rec := e.unlinkedInteraction(t, conv, source)
		bare := e.unlinkedInteraction(t, conv, source)
		bare.Interaction.MessageExternal = nil
		redacted := e.unlinkedInteraction(t, conv, source)
		redacted.Interaction.Redacted = true
		e.mustTx(t, func(tx store.Tx) error {
			return firstErr(tx.PutInteraction(e.ctx, rec), tx.PutInteraction(e.ctx, bare), tx.PutInteraction(e.ctx, redacted))
		})
		second := model.InboxRef{Tenant: tenantA, ID: e.id(t)}
		msg := model.MessageRef{Conversation: conv, ID: e.id(t)}
		for name, c := range map[string]struct {
			link store.InteractionLink
			code model.ErrorCode
		}{
			"no provider message": {store.InteractionLink{Interaction: bare.Interaction.Ref, Message: msg, Revision: "i2"}, model.Conflict},
			"unknown interaction": {store.InteractionLink{
				Interaction: model.InteractionRef{Conversation: conv, ID: e.id(t)}, Message: msg, Revision: "i2",
			}, model.NotFound},
			"interaction under another conversation": {store.InteractionLink{
				Interaction: model.InteractionRef{Conversation: model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, ID: rec.Interaction.Ref.ID},
				Message:     msg, Revision: "i2",
			}, model.NotFound},
			"message in another inbox": {store.InteractionLink{
				Interaction: rec.Interaction.Ref, Message: model.MessageRef{Conversation: model.ConversationRef{Inbox: second, ID: e.id(t)}, ID: e.id(t)},
				Revision: "i2",
			}, model.Validation},
			"no revision":       {store.InteractionLink{Interaction: rec.Interaction.Ref, Message: msg}, model.Validation},
			"same revision":     {store.InteractionLink{Interaction: rec.Interaction.Ref, Message: msg, Revision: rec.Interaction.Revision}, model.Validation},
			"malformed message": {store.InteractionLink{Interaction: rec.Interaction.Ref, Message: model.MessageRef{Conversation: conv}, Revision: "i2"}, model.Validation},
		} {
			_, err := e.linkErr(c.link)
			wantCode(t, err, c.code, name)
		}
		e.wantInteraction(t, rec.Interaction)
		e.wantInteraction(t, bare.Interaction)
		ferr := e.s.Transact(e.ctx, tenantB, func(tx store.Tx) error {
			_, err := tx.LinkInteraction(e.ctx, store.InteractionLink{Interaction: rec.Interaction.Ref, Message: msg, Revision: "i2"})
			return err
		})
		wantCode(t, ferr, model.NotFound, "a foreign interaction")

		// Redaction keeps references: a redacted interaction is linked too.
		want := redacted.Interaction.Clone()
		want.Message, want.Revision = &msg, "i2"
		e.mustTx(t, func(tx store.Tx) error {
			_, err := tx.LinkInteraction(e.ctx, store.InteractionLink{Interaction: redacted.Interaction.Ref, Message: msg, Revision: "i2"})
			return err
		})
		e.wantInteraction(t, want)
	}},
}

// unlinkedInteraction is a stored interaction in conv on provider message
// source that names no local message.
func (e *env) unlinkedInteraction(t *testing.T, conv model.ConversationRef, source model.ExternalRef) store.InteractionRecord {
	t.Helper()
	rec := e.interactionRecord(t, conv, "link-token")
	rec.Interaction.Message = nil
	rec.Interaction.MessageExternal = &source
	return rec
}

// linkErr runs one LinkInteraction in its own unit and returns its result.
func (e *env) linkErr(link store.InteractionLink) (model.Interaction, error) {
	var out model.Interaction
	err := e.txErr(func(tx store.Tx) error {
		var err error
		out, err = tx.LinkInteraction(e.ctx, link)
		return err
	})
	return out, err
}
