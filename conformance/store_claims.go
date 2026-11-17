package conformance

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

var claimCases = []storeCase{
	{"IdentityClaimReturnsExisting", func(t *testing.T, e *env) {
		key := e.identityKey("U1")
		first := e.identity(t, key)
		var created bool
		e.mustTx(t, func(tx store.Tx) error {
			got, c, err := tx.ClaimIdentity(e.ctx, key, first)
			created = c
			if err == nil && got.Ref != first.Ref {
				t.Errorf("claim returned %v, want %v", got.Ref, first.Ref)
			}
			return err
		})
		if !created {
			t.Fatal("first claim did not report created")
		}
		second := e.identity(t, key)
		e.mustTx(t, func(tx store.Tx) error {
			got, c, err := tx.ClaimIdentity(e.ctx, key, second)
			if err == nil && (c || got.Ref != first.Ref || got.Contact != first.Contact) {
				t.Errorf("second claim = %v created=%v, want existing %v unchanged", got.Ref, c, first.Ref)
			}
			return err
		})
		e.mustView(t, func(r store.Reader) error {
			got, err := r.Identity(e.ctx, key)
			if err == nil && got.Ref != first.Ref {
				t.Errorf("lookup = %v, want %v", got.Ref, first.Ref)
			}
			return err
		})
	}},
	{"IdentityByReference", func(t *testing.T, e *env) {
		id, target := e.linkSetup(t)
		e.mustTx(t, func(tx store.Tx) error {
			// Read-your-writes: a claim in this unit is visible by reference.
			fresh := e.identity(t, e.identityKey("U-fresh"))
			if _, _, err := tx.ClaimIdentity(e.ctx, e.identityKey("U-fresh"), fresh); err != nil {
				return err
			}
			got, err := tx.GetIdentity(e.ctx, fresh.Ref)
			if err == nil && (got.Ref != fresh.Ref || got.SourceID != "U-fresh") {
				t.Errorf("GetIdentity in unit = %+v, want the claimed identity", got)
			}
			return err
		})
		rec := e.linkRecord(t, id, target.Ref, "", "")
		e.mustTx(t, func(tx store.Tx) error { return tx.LinkIdentity(e.ctx, rec) })
		e.mustView(t, func(r store.Reader) error {
			got, err := r.GetIdentity(e.ctx, id.Ref)
			if err != nil {
				return err
			}
			if got.Ref != id.Ref || got.Namespace != id.Namespace || got.SourceID != id.SourceID ||
				got.Contact != target.Ref || got.Revision != rec.RevisionAfter || got.LinkAssurance != rec.Assurance {
				t.Errorf("GetIdentity = %+v, want the linked identity %v at %q", got, id.Ref, rec.RevisionAfter)
			}
			_, err = r.GetIdentity(e.ctx, model.IdentityRef{Inbox: e.inbox, ID: e.id(t)})
			wantCode(t, err, model.NotFound)
			// The ID is known, the inbox is not its own: not found, never confirmed.
			other := model.InboxRef{Tenant: tenantA, ID: e.id(t)}
			_, err = r.GetIdentity(e.ctx, model.IdentityRef{Inbox: other, ID: id.Ref.ID})
			wantCode(t, err, model.NotFound)
			_, err = r.GetIdentity(e.ctx, model.IdentityRef{Inbox: e.inbox})
			wantCode(t, err, model.Validation)
			return nil
		})
		err := e.s.View(e.ctx, tenantB, func(r store.Reader) error {
			_, err := r.GetIdentity(e.ctx, id.Ref)
			return err
		})
		wantCode(t, err, model.NotFound)
	}},
	{"IdentityKeyMustMatchRecord", func(t *testing.T, e *env) {
		key := e.identityKey("U1")
		id := e.identity(t, e.identityKey("U2"))
		wantCode(t, e.txErr(func(tx store.Tx) error {
			_, _, err := tx.ClaimIdentity(e.ctx, key, id)
			return err
		}), model.Validation)
		// Equal raw source IDs in different namespaces are different keys.
		other := key
		other.Namespace = "conformance.email"
		e.mustTx(t, func(tx store.Tx) error {
			if _, _, err := tx.ClaimIdentity(e.ctx, key, e.identity(t, key)); err != nil {
				return err
			}
			_, c, err := tx.ClaimIdentity(e.ctx, other, e.identity(t, other))
			if err == nil && !c {
				t.Error("same source ID in another namespace collapsed into one identity")
			}
			return err
		})
	}},
	{"ConcurrentIdentityClaimsCreateOne", func(t *testing.T, e *env) {
		key := e.identityKey("U-race")
		const workers = 8
		var created atomic.Int64
		refs := make([]model.IdentityRef, workers)
		var wg sync.WaitGroup
		for i := range workers {
			cand := e.identity(t, key)
			wg.Go(func() {
				var won bool
				err := e.retryTx(func(tx store.Tx) error {
					got, c, err := tx.ClaimIdentity(e.ctx, key, cand)
					if err != nil {
						return err
					}
					if c {
						// Provisional contact written after a successful claim.
						if err := tx.PutContact(e.ctx, model.Contact{Ref: cand.Contact, Kind: model.ContactKindPerson, Revision: "r1"}, ""); err != nil {
							return err
						}
					}
					refs[i], won = got.Ref, c
					return nil
				})
				if err != nil {
					t.Errorf("claim by worker %d: %v", i, err)
				} else if won {
					// Counted only for the attempt that committed.
					created.Add(1)
				}
			})
		}
		wg.Wait()
		if created.Load() != 1 {
			t.Fatalf("created %d identities for one key, want 1", created.Load())
		}
		for i, r := range refs {
			if r != refs[0] {
				t.Fatalf("claim by worker %d returned identity %v, want the winner's %v: all claims of one key return one identity", i, r, refs[0])
			}
		}
		e.mustView(t, func(r store.Reader) error {
			p, err := r.Contacts(e.ctx, list(100))
			if err == nil && len(p.Items) != 1 {
				t.Errorf("contacts = %d, want 1 provisional contact and no orphans", len(p.Items))
			}
			return err
		})
	}},
	{"ConversationClaimConverges", func(t *testing.T, e *env) {
		sel := store.SelectionKey{Inbox: e.inbox, Mode: model.ModePerContact, Key: "identity-1"}
		a := e.conversation(model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, "r1")
		b := e.conversation(model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, "r1")
		e.mustTx(t, func(tx store.Tx) error {
			got, c, err := tx.ClaimConversation(e.ctx, store.ConversationClaim{Selection: sel, CreateKey: "none", Conversation: a})
			if err == nil && (!c || got.Ref != a.Ref) {
				t.Errorf("first claim = %v created=%v", got.Ref, c)
			}
			return err
		})
		e.mustTx(t, func(tx store.Tx) error {
			got, c, err := tx.ClaimConversation(e.ctx, store.ConversationClaim{Selection: sel, CreateKey: "none", Conversation: b})
			if err == nil && (c || got.Ref != a.Ref) {
				t.Errorf("equal create key claimed %v created=%v, want existing %v", got.Ref, c, a.Ref)
			}
			return err
		})
		wantCode(t, e.txErr(func(tx store.Tx) error {
			bad := b
			bad.Mode = model.ModePerRoom
			_, _, err := tx.ClaimConversation(e.ctx, store.ConversationClaim{Selection: sel, CreateKey: "k2", Conversation: bad})
			return err
		}), model.Validation)
	}},
	{"MessageClaimReturnsExisting", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		ref := model.ProviderMessageRef(e.channel, "p-1")
		claimed := func(conv model.ConversationRef, refs ...model.ExternalRef) model.Message {
			m := e.message(model.MessageRef{Conversation: conv, ID: e.id(t)}, "r1")
			m.External = refs
			return m
		}
		claim := func(tx store.Tx, ref model.ExternalRef, m model.Message) (model.Message, bool, error) {
			return tx.ClaimMessage(e.ctx, store.MessageClaim{Reference: ref, Message: m})
		}
		first := claimed(conv, ref)
		e.mustTx(t, func(tx store.Tx) error {
			got, c, err := claim(tx, ref, first)
			if err == nil && (!c || got.Ref != first.Ref) {
				t.Errorf("first claim = %v created=%v", got.Ref, c)
			}
			if err == nil {
				// Read-your-writes: the claimed message is stored in this unit.
				_, err = tx.GetMessage(e.ctx, first.Ref)
			}
			return err
		})
		edited := first
		edited.Content = model.Content{Parts: []model.Part{{Kind: "text", Text: "edited"}}}
		edited.Revision = "r2"
		e.mustTx(t, func(tx store.Tx) error { return tx.PutMessage(e.ctx, edited, "r1") })
		second := claimed(conv, ref)
		e.mustTx(t, func(tx store.Tx) error {
			got, c, err := claim(tx, ref, second)
			if err == nil && (c || got.Ref != first.Ref || got.Revision != "r2") {
				t.Errorf("second claim = %v@%s created=%v, want existing %v@r2", got.Ref, got.Revision, c, first.Ref)
			}
			return err
		})
		e.mustView(t, func(r store.Reader) error {
			_, err := r.GetMessage(e.ctx, second.Ref)
			wantCode(t, err, model.NotFound)
			got, err := r.MessagesByReference(e.ctx, e.inbox, ref, 2)
			if err == nil && (len(got) != 1 || got[0].Ref != first.Ref) {
				t.Errorf("messages holding the claimed reference = %v, want %v only", messageIDs(got), first.Ref.ID)
			}
			return err
		})

		// The reference must be the message's own.
		wantCode(t, e.txErr(func(tx store.Tx) error {
			_, _, err := claim(tx, model.ProviderMessageRef(e.channel, "p-2"), claimed(conv, ref))
			return err
		}), model.Validation)
		// A foreign tenant's reference is never confirmed.
		foreign := model.ProviderMessageRef(model.ChannelRef{Tenant: tenantB, ID: e.id(t)}, "p-1")
		wantCode(t, e.txErr(func(tx store.Tx) error {
			_, _, err := claim(tx, foreign, claimed(conv, foreign))
			return err
		}), model.NotFound)

		// A claim is per inbox: another inbox claims the same reference.
		other := model.InboxRef{Tenant: tenantA, ID: e.id(t)}
		otherConv := model.ConversationRef{Inbox: other, ID: e.id(t)}
		e.mustTx(t, func(tx store.Tx) error {
			if err := tx.PutInbox(e.ctx, model.Inbox{
				Ref: other, Channel: e.channel, Name: "sales", Destinations: []string{"sales@example.test"}, Revision: "r1",
			}, ""); err != nil {
				return err
			}
			if err := tx.PutConversation(e.ctx, e.conversation(otherConv, "r1"), ""); err != nil {
				return err
			}
			m := claimed(otherConv, ref)
			got, c, err := claim(tx, ref, m)
			if err == nil && (!c || got.Ref != m.Ref) {
				t.Errorf("claim in another inbox = %v created=%v, want a new claim", got.Ref, c)
			}
			return err
		})
	}},
	{"CandidatesCompleteOrLimitError", func(t *testing.T, e *env) {
		sel := store.SelectionKey{Inbox: e.inbox, Mode: model.ModePerContact, Key: "identity-1"}
		base := time.Unix(1_700_000_000, 0).UTC()
		var want []model.ID
		e.mustTx(t, func(tx store.Tx) error {
			for i, key := range []string{"k1", "k2", "k3"} {
				c := e.conversation(model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, "r1")
				c.CreatedAt = base.Add(time.Duration(3-i) * time.Minute) // reverse creation order
				want = append([]model.ID{c.Ref.ID}, want...)
				if _, _, err := tx.ClaimConversation(e.ctx, store.ConversationClaim{Selection: sel, CreateKey: key, Conversation: c}); err != nil {
					return err
				}
			}
			other := sel
			other.Key = "identity-2"
			c := e.conversation(model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, "r1")
			_, _, err := tx.ClaimConversation(e.ctx, store.ConversationClaim{Selection: other, CreateKey: "k1", Conversation: c})
			return err
		})
		e.mustView(t, func(r store.Reader) error {
			got, err := r.Candidates(e.ctx, sel, 3)
			if err != nil {
				return err
			}
			if len(got) != 3 {
				t.Fatalf("candidates = %d, want 3", len(got))
			}
			for i := range got {
				if got[i].Ref.ID != want[i] {
					t.Errorf("candidate %d = %s, want %s (CreatedAt then ID order)", i, got[i].Ref.ID, want[i])
				}
			}
			_, err = r.Candidates(e.ctx, sel, 2)
			wantCode(t, err, model.Conflict)
			_, err = r.Candidates(e.ctx, sel, 0)
			wantCode(t, err, model.Validation)
			return nil
		})
	}},
	{"CandidatesSkipClosedHistory", func(t *testing.T, e *env) {
		// A group keeps every conversation ever claimed in it. Closed history
		// beyond the newest closed member is never a candidate, so it never
		// counts toward the limit.
		sel := store.SelectionKey{Inbox: e.inbox, Mode: model.ModePerContact, Key: "identity-history"}
		base := time.Unix(1_700_000_000, 0).UTC()
		const closed = 300
		var newestClosed, open1, open2 model.Conversation
		claim := func(tx store.Tx, key string, c model.Conversation) error {
			_, _, err := tx.ClaimConversation(e.ctx, store.ConversationClaim{Selection: sel, CreateKey: key, Conversation: c})
			return err
		}
		e.mustTx(t, func(tx store.Tx) error {
			for i := range closed {
				c := e.conversation(model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, "r1")
				c.Status = model.ConversationClosed
				// Not in claim order; the newest is claimed mid-way and beats
				// its runner-up by nanoseconds, below timestamp precision.
				c.CreatedAt = base.Add(time.Duration((i*7919)%closed) * time.Minute)
				switch i {
				case 150:
					c.CreatedAt = base.Add(closed*time.Minute + 7)
					newestClosed = c
				case 151:
					c.CreatedAt = base.Add(closed*time.Minute + 5)
				}
				if err := claim(tx, fmt.Sprintf("closed-%d", i), c); err != nil {
					return err
				}
			}
			open1 = e.conversation(model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, "r1")
			open1.CreatedAt = base.Add(-time.Hour)
			open2 = e.conversation(model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, "r1")
			open2.Status, open2.CreatedAt = model.ConversationResolved, base.Add(400*time.Minute)
			return firstErr(claim(tx, "open-1", open1), claim(tx, "open-2", open2))
		})
		ids := func(cs []model.Conversation) []model.ID {
			out := make([]model.ID, len(cs))
			for i, c := range cs {
				out[i] = c.Ref.ID
			}
			return out
		}
		e.mustView(t, func(r store.Reader) error {
			got, err := r.Candidates(e.ctx, sel, 3)
			if err != nil {
				return err
			}
			want := []model.ID{open1.Ref.ID, newestClosed.Ref.ID, open2.Ref.ID}
			if !slices.Equal(ids(got), want) {
				t.Errorf("candidates = %v, want open, newest closed, resolved %v in CreatedAt order", ids(got), want)
			}
			_, err = r.Candidates(e.ctx, sel, 2)
			wantCode(t, err, model.Conflict)
			return nil
		})
		// Closing a member takes it out; the newest closed one stays.
		closedOpen := open2
		closedOpen.Status, closedOpen.Revision = model.ConversationClosed, "r2"
		e.mustTx(t, func(tx store.Tx) error { return tx.PutConversation(e.ctx, closedOpen, "r1") })
		e.mustView(t, func(r store.Reader) error {
			got, err := r.Candidates(e.ctx, sel, 2)
			if err == nil && !slices.Equal(ids(got), []model.ID{open1.Ref.ID, closedOpen.Ref.ID}) {
				t.Errorf("candidates after close = %v, want open and the newly newest closed %s", ids(got), closedOpen.Ref.ID)
			}
			return err
		})
	}},
	{"CandidatesNewestClosedTieBreaksOnID", func(t *testing.T, e *env) {
		sel := store.SelectionKey{Inbox: e.inbox, Mode: model.ModePerRoom, Key: "room-tie"}
		var cs []model.Conversation
		e.mustTx(t, func(tx store.Tx) error {
			for i := range 3 {
				c := e.conversation(model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}, "r1")
				c.Mode, c.Status = model.ModePerRoom, model.ConversationClosed
				cs = append(cs, c)
				if _, _, err := tx.ClaimConversation(e.ctx, store.ConversationClaim{Selection: sel, CreateKey: fmt.Sprintf("k%d", i), Conversation: c}); err != nil {
					return err
				}
			}
			return nil
		})
		want := slices.MaxFunc(cs, func(a, b model.Conversation) int { return strings.Compare(string(a.Ref.ID), string(b.Ref.ID)) })
		e.mustView(t, func(r store.Reader) error {
			got, err := r.Candidates(e.ctx, sel, 1)
			if err == nil && (len(got) != 1 || got[0].Ref != want.Ref) {
				t.Errorf("candidates = %v, want only the greatest ID %s among equal CreatedAt", got, want.Ref.ID)
			}
			return err
		})
	}},
	{"IngressDeduplicates", func(t *testing.T, e *env) {
		first := store.IngressRecord{ID: e.id(t), Channel: e.channel, DedupKey: "evt-1", VerifierVersion: "v1"}
		e.mustTx(t, func(tx store.Tx) error {
			_, c, err := tx.ClaimIngress(e.ctx, first)
			if err == nil && !c {
				t.Error("first ingress not created")
			}
			return err
		})
		dup := store.IngressRecord{ID: e.id(t), Channel: e.channel, DedupKey: "evt-1", VerifierVersion: "v1"}
		e.mustTx(t, func(tx store.Tx) error {
			got, c, err := tx.ClaimIngress(e.ctx, dup)
			if err == nil && (c || got.ID != first.ID) {
				t.Errorf("duplicate ingress = %s created=%v, want existing %s", got.ID, c, first.ID)
			}
			return err
		})
		rc := model.IngressReceipt{ID: first.ID, Committed: true}
		e.mustTx(t, func(tx store.Tx) error { return tx.CompleteIngress(e.ctx, first.ID, rc) })
		e.mustTx(t, func(tx store.Tx) error { return tx.CompleteIngress(e.ctx, first.ID, rc) })
		wantCode(t, e.txErr(func(tx store.Tx) error {
			return tx.CompleteIngress(e.ctx, first.ID, model.IngressReceipt{ID: first.ID, Committed: true, Duplicate: true})
		}), model.Conflict)
		e.mustView(t, func(r store.Reader) error {
			got, err := r.Ingress(e.ctx, first.ID)
			if err == nil && got.Receipt != rc {
				t.Errorf("receipt = %+v, want %+v", got.Receipt, rc)
			}
			return err
		})
	}},
	{"OperationClaimFingerprint", func(t *testing.T, e *env) {
		rec := e.operation(t, "op-1", []byte{1, 2, 3})
		e.mustTx(t, func(tx store.Tx) error {
			_, c, err := tx.ClaimOperation(e.ctx, rec)
			if err == nil && !c {
				t.Error("first operation claim not created")
			}
			return err
		})
		replay := e.operation(t, "op-1", []byte{1, 2, 3})
		e.mustTx(t, func(tx store.Tx) error {
			got, c, err := tx.ClaimOperation(e.ctx, replay)
			if err == nil && (c || got.Operation.Ref != rec.Operation.Ref) {
				t.Errorf("replay = %v created=%v, want stored %v", got.Operation.Ref, c, rec.Operation.Ref)
			}
			return err
		})
		wantCode(t, e.txErr(func(tx store.Tx) error {
			_, _, err := tx.ClaimOperation(e.ctx, e.operation(t, "op-1", []byte{9}))
			return err
		}), model.Conflict)
	}},
}

func (e *env) identityKey(source string) model.IdentityKey {
	return model.IdentityKey{Inbox: e.inbox, Namespace: "conformance.user", SourceID: source}
}

func (e *env) identity(t *testing.T, key model.IdentityKey) model.Identity {
	t.Helper()
	return model.Identity{
		Ref:       model.IdentityRef{Inbox: key.Inbox, ID: e.id(t)},
		Contact:   model.ContactRef{Tenant: tenantA, ID: e.id(t)},
		Namespace: key.Namespace, SourceID: key.SourceID,
		Kind: model.ContactKindPerson, LinkAssurance: model.LinkAssuranceNone, Revision: "i1",
	}
}

func (e *env) operation(t *testing.T, opID model.OperationID, fingerprint []byte) store.OperationRecord {
	t.Helper()
	ref := model.OperationRef{Tenant: tenantA, ID: e.id(t)}
	return store.OperationRecord{
		Operation: model.Operation{Ref: ref, State: model.Queued, Revision: "o1"},
		Key: store.OperationKey{
			Tenant: tenantA, Caller: "conformance-actor", Kind: "messages.reply",
			Target: e.inbox.Ref(), ID: opID,
		},
		FingerprintVersion: 1,
		Fingerprint:        fingerprint,
	}
}
