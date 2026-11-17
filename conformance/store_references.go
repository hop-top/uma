package conformance

import (
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

var referenceCases = []storeCase{
	{"MessagesByReferenceMatchesExactRef", func(t *testing.T, e *env) {
		ref := model.ProviderMessageRef(e.channel, "<m1@a.example>")
		conv := e.putConversation(t)
		other := e.putConversation(t)
		first := e.putMessageWith(t, conv, time.Unix(1_700_000_200, 0).UTC(), ref)
		second := e.putMessageWith(t, other, time.Unix(1_700_000_100, 0).UTC(), ref)
		// Near misses: another ID, kind, namespace or channel never match.
		near := []model.ExternalRef{
			model.ProviderMessageRef(e.channel, "<m2@a.example>"),
			{Channel: e.channel, Namespace: ref.Namespace, Kind: "thread", ID: ref.ID},
			{Channel: e.channel, Namespace: "conformance.chat", Kind: ref.Kind, ID: ref.ID},
			model.ProviderMessageRef(model.ChannelRef{Tenant: tenantA, ID: e.id(t)}, ref.ID),
			{Channel: e.channel, Namespace: ref.Namespace, Kind: ref.Kind, ID: ref.ID, ParentID: "parent"},
		}
		e.putMessageWith(t, conv, time.Unix(1_700_000_300, 0).UTC(), near...)
		e.mustView(t, func(r store.Reader) error {
			got, err := r.MessagesByReference(e.ctx, e.inbox, ref, 10)
			if err != nil {
				return err
			}
			if ids := messageIDs(got); !slices.Equal(ids, []model.ID{second.ID, first.ID}) {
				t.Errorf("matches = %v, want [%s %s] in RecordedAt order", ids, second.ID, first.ID)
			}
			none, err := r.MessagesByReference(e.ctx, e.inbox, model.ProviderMessageRef(e.channel, "<absent@a.example>"), 10)
			if err == nil && len(none) != 0 {
				t.Errorf("absent ref matched %d messages", len(none))
			}
			return err
		})
	}},
	{"MessagesByReferenceInboxScoped", func(t *testing.T, e *env) {
		ref := model.ProviderMessageRef(e.channel, "<m1@a.example>")
		in2 := model.InboxRef{Tenant: tenantA, ID: e.id(t)}
		conv2 := model.ConversationRef{Inbox: in2, ID: e.id(t)}
		e.mustTx(t, func(tx store.Tx) error {
			if err := tx.PutInbox(e.ctx, model.Inbox{Ref: in2, Channel: e.channel, Name: "other", Revision: "r1"}, ""); err != nil {
				return err
			}
			return tx.PutConversation(e.ctx, e.conversation(conv2, "r1"), "")
		})
		e.putMessageWith(t, conv2, time.Unix(1_700_000_000, 0).UTC(), ref)
		e.mustView(t, func(r store.Reader) error {
			got, err := r.MessagesByReference(e.ctx, e.inbox, ref, 10)
			if err == nil && len(got) != 0 {
				t.Errorf("message of another inbox matched")
			}
			return err
		})
	}},
	{"MessagesByReferenceNeverTruncates", func(t *testing.T, e *env) {
		ref := model.ProviderMessageRef(e.channel, "<dup@a.example>")
		conv := e.putConversation(t)
		for i := range 3 {
			e.putMessageWith(t, conv, time.Unix(1_700_000_000+int64(i), 0).UTC(), ref)
		}
		e.mustView(t, func(r store.Reader) error {
			_, err := r.MessagesByReference(e.ctx, e.inbox, ref, 2)
			wantCode(t, err, model.Conflict)
			_, err = r.MessagesByReference(e.ctx, e.inbox, ref, 0)
			wantCode(t, err, model.Validation)
			got, err := r.MessagesByReference(e.ctx, e.inbox, ref, 3)
			if err == nil && len(got) != 3 {
				t.Errorf("limit 3: got %d matches, want 3", len(got))
			}
			return err
		})
	}},
	{"MessagesByReferenceSeesOwnWrites", func(t *testing.T, e *env) {
		ref := model.ProviderMessageRef(e.channel, "<m1@a.example>")
		conv := e.putConversation(t)
		e.mustTx(t, func(tx store.Tx) error {
			msg := e.message(model.MessageRef{Conversation: conv, ID: e.id(t)}, "r1")
			msg.External = []model.ExternalRef{ref}
			if err := tx.PutMessage(e.ctx, msg, ""); err != nil {
				return err
			}
			got, err := tx.MessagesByReference(e.ctx, e.inbox, ref, 10)
			if err == nil && len(got) != 1 {
				t.Errorf("own write: got %d matches, want 1", len(got))
			}
			return err
		})
	}},
	{"MessagesByReferenceFollowsRewrite", func(t *testing.T, e *env) {
		old := model.ProviderMessageRef(e.channel, "<old@a.example>")
		next := model.ProviderMessageRef(e.channel, "<new@a.example>")
		conv := e.putConversation(t)
		ref := e.putMessageWith(t, conv, time.Unix(1_700_000_000, 0).UTC(), old)
		msg := e.message(ref, "r2")
		msg.External = []model.ExternalRef{next, next}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutMessage(e.ctx, msg, "r1") })
		e.mustView(t, func(r store.Reader) error {
			gone, err := r.MessagesByReference(e.ctx, e.inbox, old, 10)
			if err != nil {
				return err
			}
			got, err := r.MessagesByReference(e.ctx, e.inbox, next, 10)
			if err == nil && (len(gone) != 0 || len(got) != 1) {
				t.Errorf("after rewrite: old ref %d matches, new ref %d; want 0 and 1", len(gone), len(got))
			}
			return err
		})
		// A rolled-back rewrite leaves the stored references.
		_ = e.txErr(func(tx store.Tx) error {
			m := e.message(ref, "r3")
			m.External = []model.ExternalRef{old}
			if err := tx.PutMessage(e.ctx, m, "r2"); err != nil {
				return err
			}
			return errAbort
		})
		e.mustView(t, func(r store.Reader) error {
			got, err := r.MessagesByReference(e.ctx, e.inbox, next, 10)
			if err == nil && len(got) != 1 {
				t.Errorf("rolled-back rewrite changed references: %d matches", len(got))
			}
			return err
		})
	}},
	{"MessagesByReferenceSnapshot", func(t *testing.T, e *env) {
		ref := model.ProviderMessageRef(e.channel, "<m1@a.example>")
		conv := e.putConversation(t)
		e.putMessageWith(t, conv, time.Unix(1_700_000_000, 0).UTC(), ref)
		e.mustView(t, func(r store.Reader) error {
			got, err := r.MessagesByReference(e.ctx, e.inbox, ref, 10)
			if err != nil {
				return err
			}
			got[0].External[0].ID = "mutated"
			got[0].Content.Parts[0].Text = "mutated"
			again, err := r.MessagesByReference(e.ctx, e.inbox, ref, 10)
			if err == nil && (len(again) != 1 || again[0].Content.Parts[0].Text != "hello") {
				t.Errorf("returned message shares memory with stored state")
			}
			return err
		})
	}},
	{"MessagesByReferenceTenantScoped", func(t *testing.T, e *env) {
		foreign := model.InboxRef{Tenant: tenantB, ID: e.inbox.ID}
		e.mustView(t, func(r store.Reader) error {
			_, err := r.MessagesByReference(e.ctx, foreign, model.ProviderMessageRef(e.channel, "<m1@a.example>"), 10)
			wantCode(t, err, model.NotFound)
			_, err = r.MessagesByReference(e.ctx, e.inbox,
				model.ProviderMessageRef(model.ChannelRef{Tenant: tenantB, ID: e.channel.ID}, "<m1@a.example>"), 10)
			wantCode(t, err, model.NotFound)
			_, err = r.MessagesByReference(e.ctx, e.inbox, model.ExternalRef{}, 10)
			wantCode(t, err, model.Validation)
			return nil
		})
	}},
}

// putMessageWith commits a new message in conv recorded at at with refs.
func (e *env) putMessageWith(t *testing.T, conv model.ConversationRef, at time.Time, refs ...model.ExternalRef) model.MessageRef {
	t.Helper()
	ref := model.MessageRef{Conversation: conv, ID: e.id(t)}
	msg := e.message(ref, "r1")
	msg.RecordedAt = at
	msg.External = refs
	e.mustTx(t, func(tx store.Tx) error { return tx.PutMessage(e.ctx, msg, "") })
	return ref
}

func messageIDs(ms []model.Message) []model.ID {
	out := make([]model.ID, len(ms))
	for i, m := range ms {
		out[i] = m.Ref.ID
	}
	return out
}
