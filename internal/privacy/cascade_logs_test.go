package privacy_test

import (
	"strconv"
	"testing"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// logSeed is a linkedSeed whose identities each have a recipient log, as
// an adapter hosting its own recipients keeps one: keyed by the identity's
// channel, namespace and source ID. alice also has an identity in a second
// channel's inbox, with a log there. stranger and sibling have logs that
// belong to no identity of the contacts under test.
type logSeed struct {
	*linkedSeed
	aliceWeb model.Identity
	keys     map[model.IdentityRef]store.RecipientLogKey
	stranger store.RecipientLogKey
	sibling  store.RecipientLogKey
}

func seedLogs(t *testing.T) *logSeed {
	t.Helper()
	s := &logSeed{linkedSeed: seedLinked(t), keys: map[model.IdentityRef]store.RecipientLogKey{}}
	h := s.h
	// A second channel and inbox, and alice's web identity in it.
	other := h.PutChannel(enginetest.ChatType)
	inbox := h.PutInbox(other.Ref, enginetest.PerContact(false), false, "web")
	key := model.IdentityKey{Inbox: inbox.Ref, Namespace: "web.visitor", SourceID: "visitor-alice"}
	s.aliceWeb = model.Identity{
		Ref: model.IdentityRef{Inbox: inbox.Ref, ID: h.ID()}, Contact: s.shared.Ref, SourceID: key.SourceID,
		Namespace: key.Namespace, Assurance: s.alice.Assurance, Kind: s.alice.Kind, LinkAssurance: model.LinkAssuranceAsserted, Revision: "i1",
	}
	h.Tx(func(tx store.Tx) error {
		_, _, err := tx.ClaimIdentity(h.Ctx, key, s.aliceWeb)
		return err
	})
	for _, id := range []model.Identity{s.alice, s.bob, s.carol, s.aliceWeb} {
		ch := h.Channel.Ref
		if id.Ref.Inbox == inbox.Ref {
			ch = other.Ref
		}
		k := store.RecipientLogKey{Channel: ch, Namespace: id.Namespace, Recipient: id.SourceID}
		s.keys[id.Ref] = k
		s.fill(t, k)
	}
	s.stranger = store.RecipientLogKey{Channel: h.Channel.Ref, Namespace: s.alice.Namespace, Recipient: "stranger"}
	// Same recipient as alice's web visitor, on the other channel's
	// namespace of the first channel: another log.
	s.sibling = store.RecipientLogKey{Channel: h.Channel.Ref, Namespace: "web.visitor", Recipient: "visitor-alice"}
	s.fill(t, s.stranger)
	s.fill(t, s.sibling)
	return s
}

// fill appends three entries to k.
func (s *logSeed) fill(t *testing.T, k store.RecipientLogKey) {
	t.Helper()
	for i := range 3 {
		s.h.Tx(func(tx store.Tx) error {
			_, err := tx.AppendRecipientLog(s.h.Ctx, k, model.Extension{
				Name: "test.entry", Version: 1, Data: []byte(`{"secret":"` + secret + strconv.Itoa(i) + `"}`),
			}, 10)
			return err
		})
	}
}

// entries reads the retained entries of k.
func (s *logSeed) entries(t *testing.T, k store.RecipientLogKey) []store.RecipientLogEntry {
	t.Helper()
	var got []store.RecipientLogEntry
	s.h.View(func(r store.Reader) (err error) {
		got, err = r.RecipientLogEntries(s.h.Ctx, k, 0, 100)
		return err
	})
	return got
}

// wantErased requires the logs of ids to be empty and every other log to
// hold its three entries.
func (s *logSeed) wantErased(t *testing.T, ids ...model.IdentityRef) {
	t.Helper()
	erased := map[store.RecipientLogKey]bool{}
	for _, id := range ids {
		erased[s.keys[id]] = true
	}
	all := []store.RecipientLogKey{s.stranger, s.sibling}
	for _, k := range s.keys {
		all = append(all, k)
	}
	for _, k := range all {
		got := s.entries(t, k)
		switch {
		case erased[k] && len(got) != 0:
			t.Errorf("log %s/%s kept %d entries after the cascade", k.Namespace, k.Recipient, len(got))
		case !erased[k] && len(got) != 3:
			t.Errorf("log %s/%s holds %d entries, want 3: the cascade must not touch it", k.Namespace, k.Recipient, len(got))
		}
	}
}

// A contact cascade erases the recipient logs of every identity on the
// contact, across the channels they sit on, in its submission unit and
// before any worker runs; other contacts' logs and logs that belong to no
// identity of the contact stay.
func TestCascadeOnContactErasesRecipientLogs(t *testing.T) {
	s := seedLogs(t)
	if _, err := s.h.Client.Privacy().Redact(s.h.Ctx, cascade(s.shared.Ref.Ref(), "op-cascade", s.shared.Revision)); err != nil {
		t.Fatalf("cascade: %v", err)
	}
	s.wantErased(t, s.alice.Ref, s.carol.Ref, s.aliceWeb.Ref)
}

// An identity cascade erases that identity's log only.
func TestCascadeOnIdentityErasesItsRecipientLog(t *testing.T) {
	s := seedLogs(t)
	if _, err := s.h.Client.Privacy().Redact(s.h.Ctx, cascade(s.alice.Ref.Ref(), "op-cascade", s.alice.Revision)); err != nil {
		t.Fatalf("cascade: %v", err)
	}
	s.wantErased(t, s.alice.Ref)
}

// A cascade on a merged contact erases the logs of every identity on the
// redirect chain's survivor, those that were merged into it among them.
func TestCascadeOnMergedContactErasesEveryChainIdentityLog(t *testing.T) {
	s := seedLogs(t)
	top := s.putContact(t)
	s.mergeInto(t, s.other.Ref, s.shared.Ref, "op-merge-1")
	s.mergeInto(t, s.shared.Ref, top.Ref, "op-merge-2")
	var other model.Contact
	s.h.View(func(r store.Reader) (err error) { other, err = r.GetContact(s.h.Ctx, s.other.Ref); return err })
	if _, err := s.h.Client.Privacy().Redact(s.h.Ctx, cascade(s.other.Ref.Ref(), "op-cascade", other.Revision)); err != nil {
		t.Fatalf("cascade: %v", err)
	}
	s.wantErased(t, s.alice.Ref, s.bob.Ref, s.carol.Ref, s.aliceWeb.Ref)
}

// A cascade refused for a stale revision erases nothing, and a conversation
// cascade owns no identity and leaves every log.
func TestCascadeThatStoresNothingLeavesRecipientLogs(t *testing.T) {
	s := seedLogs(t)
	_, err := s.h.Client.Privacy().Redact(s.h.Ctx, cascade(s.shared.Ref.Ref(), "op-stale", "stale"))
	if model.CodeOf(err) != model.Conflict {
		t.Fatalf("stale cascade = %v, want conflict", err)
	}
	s.wantErased(t)
	conv := s.h.MessageByProviderID(s.inbox, "pm-alice").Ref.Conversation
	var c model.Conversation
	s.h.View(func(r store.Reader) (err error) { c, err = r.GetConversation(s.h.Ctx, conv); return err })
	if _, err := s.h.Client.Privacy().Redact(s.h.Ctx, cascade(conv.Ref(), "op-conv", c.Revision)); err != nil {
		t.Fatalf("conversation cascade: %v", err)
	}
	s.wantErased(t)
}

// An erased log keeps counting: an append after the cascade takes the next
// sequence, so a reader holding an older cursor finds a gap and resets
// instead of missing the new entry.
func TestAppendAfterCascadeContinuesTheSequence(t *testing.T) {
	s := seedLogs(t)
	if _, err := s.h.Client.Privacy().Redact(s.h.Ctx, cascade(s.alice.Ref.Ref(), "op-cascade", s.alice.Revision)); err != nil {
		t.Fatalf("cascade: %v", err)
	}
	var seq uint64
	s.h.Tx(func(tx store.Tx) (err error) {
		seq, err = tx.AppendRecipientLog(s.h.Ctx, s.keys[s.alice.Ref], model.Extension{Name: "test.entry", Version: 1}, 10)
		return err
	})
	if seq != 4 {
		t.Fatalf("append after the cascade = seq %d, want 4", seq)
	}
}
