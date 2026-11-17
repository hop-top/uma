package conformance

import (
	"fmt"
	"reflect"
	"slices"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// First contacts through the engine. A per-contact selection reads the
// sender's candidate group, which the identity's unit just created cannot
// have: the engine does not read it then, and the selection must come out as
// it does for a known sender whose identity holds no conversation, which does
// read.

// seedSender stores an identity of source in the inbox, with its provisional
// contact and no conversation.
func (f *faultEnv) seedSender(t *testing.T, source string) model.Identity {
	t.Helper()
	id := model.Identity{
		Ref:     model.IdentityRef{Inbox: f.inbox, ID: f.id(t)},
		Contact: model.ContactRef{Tenant: tenantA, ID: f.id(t)}, SourceID: source, Namespace: faultNS,
		Kind: model.ContactKindPerson, LinkAssurance: model.LinkAssuranceNone, Revision: model.Revision(f.id(t)),
	}
	f.mustTx(t, func(tx store.Tx) error {
		key := model.IdentityKey{Inbox: f.inbox, Namespace: faultNS, SourceID: source}
		if _, _, err := tx.ClaimIdentity(f.ctx, key, id); err != nil {
			return err
		}
		return tx.PutContact(f.ctx, model.Contact{Ref: id.Contact, Kind: id.Kind, Revision: id.Revision}, "")
	})
	return id
}

// senderOutcome is what a sender's messages led to, with no ID in it.
type senderOutcome struct {
	conversations int
	status        string
	mode          string
	roles         []string
	messages      int
}

// outcomeOf reads what the inbox holds for the sender of source.
func (f *faultEnv) outcomeOf(t *testing.T, source string) senderOutcome {
	t.Helper()
	var out senderOutcome
	f.mustView(t, func(r store.Reader) error {
		id, err := r.Identity(f.ctx, model.IdentityKey{Inbox: f.inbox, Namespace: faultNS, SourceID: source})
		if err != nil {
			return err
		}
		convs, err := r.Conversations(f.ctx, list(100))
		if err != nil {
			return err
		}
		msgs, err := r.Messages(f.ctx, list(100))
		if err != nil {
			return err
		}
		for _, c := range convs.Items {
			if c.Owner == nil || *c.Owner != id.Ref {
				continue
			}
			out.conversations++
			out.status, out.mode = c.Status, c.Mode
			for _, p := range c.Participants {
				if p.Identity != id.Ref {
					t.Errorf("conversation participant %v, want only the sender", p.Identity)
				}
				out.roles = append(out.roles, p.Role)
			}
			for _, m := range msgs.Items {
				if m.Ref.Conversation == c.Ref {
					out.messages++
				}
			}
		}
		return nil
	})
	return out
}

// deliver pushes a message from sender and waits for its commit.
func (f *faultEnv) deliver(t *testing.T, event, sender string) {
	t.Helper()
	f.push(t, f.messageFrom(event, "m-"+event, sender))
	if r := f.accept(t); r.err != nil || !r.receipt.Committed || r.receipt.Duplicate {
		t.Fatalf("delivery %s = %+v, %v; want it committed once", event, r.receipt, r.err)
	}
}

// eventKindsAfter lists the kinds of the events committed after the first n.
func (f *faultEnv) eventKindsAfter(t *testing.T, n int) []string {
	t.Helper()
	var kinds []string
	for _, ev := range f.allEvents(t, model.Filter{}, 100)[n:] {
		kinds = append(kinds, ev.Kind)
	}
	return kinds
}

func firstContactSelection(t *testing.T, e *env, _ func(*testing.T)) {
	f := newFaultEnv(t, e, false)
	// Seed before the receiver runs: its own units commit to the tenant, and
	// a seed unit that overlapped one would fail as a serialization conflict.
	f.seedSender(t, "u-known")
	p := f.start(t)
	_, stop := p.receive()
	defer stop()
	created := []string{model.EventConversationChanged, model.EventMessageReceived}

	// A known sender with no conversation: its selection reads its group.
	evs := len(f.allEvents(t, model.Filter{}, 100))
	f.deliver(t, "evt-known", "u-known")
	reads := p.store.candidateReads()
	if len(reads) != 1 || reads[0].Inbox != f.inbox || reads[0].Mode != model.ModePerContact {
		t.Fatalf("a known sender's selection read candidate groups %+v, want its own once", reads)
	}
	known := f.outcomeOf(t, "u-known")
	if got := f.eventKindsAfter(t, evs); !slices.Equal(got, created) {
		t.Fatalf("known sender's events %v, want %v", got, created)
	}

	// A first contact: its identity is created by the unit, so its group is
	// empty and not read. The conversation comes out as the known sender's.
	evs = len(f.allEvents(t, model.Filter{}, 100))
	f.deliver(t, "evt-new", "u-new")
	if got := p.store.candidateReads(); len(got) != 1 {
		t.Fatalf("a first contact read candidate groups %+v after the known sender's %+v, want no read", got[1:], reads)
	}
	first := f.outcomeOf(t, "u-new")
	if !reflect.DeepEqual(first, known) || known.conversations != 1 || known.status != model.ConversationOpen ||
		known.mode != model.ModePerContact || known.messages != 1 || !slices.Equal(known.roles, []string{"contact"}) {
		t.Fatalf("first contact led to %+v, known sender to %+v; want one open per-contact conversation, one message, alike", first, known)
	}
	if got := f.eventKindsAfter(t, evs); !slices.Equal(got, created) {
		t.Fatalf("first contact's events %v, want %v", got, created)
	}

	// Its next message finds the identity stored, reads the group, and joins
	// the conversation the first one created.
	evs = len(f.allEvents(t, model.Filter{}, 100))
	f.deliver(t, "evt-new-2", "u-new")
	if got := p.store.candidateReads(); len(got) != 2 {
		t.Fatalf("a known sender's selection read candidate groups %+v in all, want a read for it", got)
	}
	again := f.outcomeOf(t, "u-new")
	if again.conversations != 1 || again.messages != 2 {
		t.Fatalf("second message led to %+v, want it in the one conversation", again)
	}
	if got := f.eventKindsAfter(t, evs); !slices.Equal(got, []string{model.EventMessageReceived}) {
		t.Fatalf("second message's events %v, want only message.received", got)
	}
}

// Concurrent first messages from one new sender: the units race to create
// its identity. The loser finds it stored, so it reads the group and joins
// the winner's conversation: one identity, one contact, one conversation.
func concurrentFirstContacts(t *testing.T, e *env, _ func(*testing.T)) {
	f := newFaultEnv(t, e, false)
	p := f.start(t)
	_, stop := p.receive()
	defer stop()
	const n = 4
	for i := range n {
		f.push(t, f.messageFrom(fmt.Sprintf("evt-%d", i), fmt.Sprintf("m-%d", i), "u-race"))
	}
	for range n {
		if r := f.accept(t); r.err != nil || !r.receipt.Committed || r.receipt.Duplicate {
			t.Fatalf("delivery = %+v, %v; want it committed once", r.receipt, r.err)
		}
	}
	if o := f.outcomeOf(t, "u-race"); o.conversations != 1 || o.messages != n || !slices.Equal(o.roles, []string{"contact"}) {
		t.Fatalf("racing first contacts led to %+v, want %d messages in one conversation", o, n)
	}
	f.mustView(t, func(r store.Reader) error {
		ids, err := r.Identities(f.ctx, list(100))
		if err != nil {
			return err
		}
		contacts, err := r.Contacts(f.ctx, list(100))
		if err != nil {
			return err
		}
		if len(ids.Items) != 1 || len(contacts.Items) != 1 {
			t.Errorf("identities = %d, contacts = %d, want 1 and 1", len(ids.Items), len(contacts.Items))
		}
		return nil
	})
	var changed, received int
	for _, ev := range f.allEvents(t, model.Filter{}, 100) {
		switch ev.Kind {
		case model.EventConversationChanged:
			changed++
		case model.EventMessageReceived:
			received++
		}
	}
	if changed != 1 || received != n {
		t.Fatalf("events: conversation.changed = %d, message.received = %d; want 1 and %d", changed, received, n)
	}
	// Every unit that found the identity stored read its group: one of the
	// racers created it and did not.
	if reads := p.store.candidateReads(); len(reads) < n-1 {
		t.Fatalf("%d candidate reads for %d racing first contacts, want a read from each that found the identity", len(reads), n)
	}
}
