package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// The claims of an ingress unit and of a conversation start, interleaved
// by two units statement by statement as in insert_conflict_test.go: each
// claim inserts first and reads only when the insert finds its key held.

const roomSelection = "room:"

func (r *insertRig) ingress(key string) store.IngressRecord {
	return store.IngressRecord{
		ID: r.id(), Channel: r.ch, DedupKey: key, VerifierVersion: "insert/1",
		Input: model.Ingress{Envelope: model.Envelope{EventID: key, MessageID: "m-" + key, Destination: "insert", Kind: model.EnvelopeMessage}},
	}
}

// claimIngress claims rec, as an ingress unit does first, and requires a
// new claim.
func (r *insertRig) claimIngress(rec store.IngressRecord) func(store.Tx) error {
	return func(tx store.Tx) error {
		_, created, err := tx.ClaimIngress(context.Background(), rec)
		if err == nil && !created {
			return fmt.Errorf("ingress claim %s replayed, want created", rec.DedupKey)
		}
		return err
	}
}

// completeIngress records the receipt of rec, as an ingress unit does last.
func (r *insertRig) completeIngress(rec store.IngressRecord) func(store.Tx) error {
	return func(tx store.Tx) error {
		return tx.CompleteIngress(context.Background(), rec.ID, model.IngressReceipt{ID: rec.ID, Committed: true})
	}
}

func (r *insertRig) identity(source string) (model.IdentityKey, model.Identity) {
	key := model.IdentityKey{Inbox: r.in, Namespace: "insert.user", SourceID: source}
	return key, model.Identity{
		Ref: model.IdentityRef{Inbox: r.in, ID: r.id()}, Contact: model.ContactRef{Tenant: r.tenant, ID: r.id()},
		Namespace: key.Namespace, SourceID: source, Kind: model.ContactKindPerson,
		LinkAssurance: model.LinkAssuranceNone, Revision: "id1",
	}
}

// claimSender claims a sender's identity and writes its provisional
// contact, as ingress and a start do for a sender never seen.
func (r *insertRig) claimSender(source string) func(store.Tx) error {
	key, id := r.identity(source)
	return func(tx store.Tx) error {
		ctx := context.Background()
		_, created, err := tx.ClaimIdentity(ctx, key, id)
		switch {
		case err != nil:
			return err
		case !created:
			return fmt.Errorf("identity claim %s replayed, want created", source)
		}
		return tx.PutContact(ctx, model.Contact{Ref: id.Contact, Kind: model.ContactKindPerson, Revision: "ct1"}, "")
	}
}

func (r *insertRig) conversationClaim(room string) store.ConversationClaim {
	return store.ConversationClaim{
		Selection: store.SelectionKey{Inbox: r.in, Mode: model.ModePerRoom, Key: roomSelection + room},
		CreateKey: "first",
		Conversation: model.Conversation{
			Ref: model.ConversationRef{Inbox: r.in, ID: r.id()}, Mode: model.ModePerRoom, Status: model.ConversationOpen,
			CreatedAt: time.Now(), Revision: "cv1",
		},
	}
}

// claimConversation claims c and requires a new conversation.
func (r *insertRig) claimConversation(c store.ConversationClaim) func(store.Tx) error {
	return func(tx store.Tx) error {
		_, created, err := tx.ClaimConversation(context.Background(), c)
		if err == nil && !created {
			return fmt.Errorf("conversation claim %s replayed, want created", c.Selection.Key)
		}
		return err
	}
}

// messageClaim claims a new message of conversation conv under a provider
// message ID, its only reference.
func (r *insertRig) messageClaim(conv int) store.MessageClaim {
	m := r.message(conv, false)
	ref := model.ProviderMessageRef(r.ch, "claimed-"+string(m.Ref.ID))
	m.External = []model.ExternalRef{ref}
	return store.MessageClaim{Reference: ref, Message: m}
}

// claimMessage claims c and requires a new message.
func (r *insertRig) claimMessage(c store.MessageClaim) func(store.Tx) error {
	return func(tx store.Tx) error {
		_, created, err := tx.ClaimMessage(context.Background(), c)
		if err == nil && !created {
			return fmt.Errorf("message claim %s replayed, want created", c.Reference.ID)
		}
		return err
	}
}

// TestInterleavedIngressClaimsBothCommit: units claiming different ingress
// records at once, and completing them, both commit.
func TestInterleavedIngressClaimsBothCommit(t *testing.T) {
	r := newInsertRig(t)
	a1, a2, b1, b2 := r.ingress("a-1"), r.ingress("a-2"), r.ingress("b-1"), r.ingress("b-2")
	interleave(t, r.st,
		[]func(store.Tx) error{r.claimIngress(a1), r.claimIngress(a2), r.completeIngress(a1)},
		[]func(store.Tx) error{r.claimIngress(b1), r.claimIngress(b2), r.completeIngress(b1)},
	)
}

// TestInterleavedSenderClaimsBothCommit: units claiming the identities of
// different new senders, with their provisional contacts, both commit.
func TestInterleavedSenderClaimsBothCommit(t *testing.T) {
	r := newInsertRig(t)
	interleave(t, r.st,
		[]func(store.Tx) error{r.claimSender("a-1"), r.claimSender("a-2")},
		[]func(store.Tx) error{r.claimSender("b-1"), r.claimSender("b-2")},
	)
}

// TestHeldClaimsAnswerAsBefore: an ingress, identity, conversation or
// message claim whose key is held replays the stored claim, and one whose
// ID alone is held conflicts, as when the claims read first; a new contact
// under a held ID conflicts. None spoils the unit, and a claim that
// replays leaves nothing of its own.
func TestHeldClaimsAnswerAsBefore(t *testing.T) {
	r := newInsertRig(t)
	ctx := t.Context()
	in := r.ingress("held")
	idKey, ident := r.identity("held")
	contact := model.Contact{Ref: ident.Contact, Kind: model.ContactKindPerson, Revision: "ct1"}
	conv := r.conversationClaim("held")
	msg := r.messageClaim(0)
	if err := r.st.Transact(ctx, insertTenant, func(tx store.Tx) error {
		return firstErr(
			r.claimIngress(in)(tx),
			func() error { _, _, err := tx.ClaimIdentity(ctx, idKey, ident); return err }(),
			tx.PutContact(ctx, contact, ""),
			r.claimConversation(conv)(tx),
			r.claimMessage(msg)(tx),
		)
	}); err != nil {
		t.Fatal(err)
	}
	next := r.ingress("next")
	var replays struct {
		conv model.ConversationRef
		msg  model.MessageRef
	}
	if err := r.st.Transact(ctx, insertTenant, func(tx store.Tx) error {
		again := r.ingress("held")
		if got, created, err := tx.ClaimIngress(ctx, again); err != nil || created || got.ID != in.ID {
			t.Errorf("held dedup key = %v created=%v id=%s, want %s replayed", err, created, got.ID, in.ID)
		}
		moved := r.ingress("moved")
		moved.ID = in.ID
		if _, _, err := tx.ClaimIngress(ctx, moved); !model.IsCode(err, model.Conflict) {
			t.Errorf("held ingress ID under another dedup key = %v, want conflict", err)
		}

		_, other := r.identity("held")
		if got, created, err := tx.ClaimIdentity(ctx, idKey, other); err != nil || created || got.Ref != ident.Ref {
			t.Errorf("held identity key = %v created=%v ref=%v, want %v replayed", err, created, got.Ref, ident.Ref)
		}
		otherKey, reused := r.identity("other")
		reused.Ref = ident.Ref
		if _, _, err := tx.ClaimIdentity(ctx, otherKey, reused); !model.IsCode(err, model.Conflict) {
			t.Errorf("held identity ID under another key = %v, want conflict", err)
		}
		if err := tx.PutContact(ctx, contact, ""); !model.IsCode(err, model.Conflict) {
			t.Errorf("new contact under a held ID = %v, want conflict", err)
		}

		again2 := r.conversationClaim("held")
		replays.conv = again2.Conversation.Ref
		if got, created, err := tx.ClaimConversation(ctx, again2); err != nil || created || got.Ref != conv.Conversation.Ref {
			t.Errorf("held create key = %v created=%v ref=%v, want %v replayed", err, created, got.Ref, conv.Conversation.Ref)
		}
		taken := r.conversationClaim("other")
		taken.Conversation.Ref = conv.Conversation.Ref
		if _, _, err := tx.ClaimConversation(ctx, taken); !model.IsCode(err, model.Conflict) {
			t.Errorf("held conversation ID under another create key = %v, want conflict", err)
		}

		again3 := r.messageClaim(0)
		again3.Reference = msg.Reference
		again3.Message.External = []model.ExternalRef{msg.Reference}
		replays.msg = again3.Message.Ref
		if got, created, err := tx.ClaimMessage(ctx, again3); err != nil || created || got.Ref != msg.Message.Ref {
			t.Errorf("held message reference = %v created=%v ref=%v, want %v replayed", err, created, got.Ref, msg.Message.Ref)
		}
		reusedMsg := r.messageClaim(0)
		reusedMsg.Message.Ref = msg.Message.Ref
		if _, _, err := tx.ClaimMessage(ctx, reusedMsg); !model.IsCode(err, model.Conflict) {
			t.Errorf("held message ID under another reference = %v, want conflict", err)
		}
		return r.claimIngress(next)(tx)
	}); err != nil {
		t.Fatalf("unit after the replays and conflicts: %v", err)
	}
	if err := r.st.View(ctx, insertTenant, func(rd store.Reader) error {
		if _, err := rd.Ingress(ctx, next.ID); err != nil {
			t.Errorf("claim after the replays: %v", err)
		}
		if _, err := rd.GetConversation(ctx, replays.conv); !model.IsCode(err, model.NotFound) {
			t.Errorf("conversation of a replayed claim = %v, want not found", err)
		}
		if _, err := rd.GetMessage(ctx, replays.msg); !model.IsCode(err, model.NotFound) {
			t.Errorf("message of a replayed claim = %v, want not found", err)
		}
		got, err := rd.MessagesByReference(ctx, r.in, msg.Reference, 10)
		if err == nil && (len(got) != 1 || got[0].Ref != msg.Message.Ref) {
			t.Errorf("held reference finds %d messages, want only the claimed one", len(got))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
