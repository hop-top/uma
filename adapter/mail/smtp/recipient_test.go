package smtp_test

import (
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/smtp"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// A start names its recipient before any inbound message does, and core keys
// the recipient's identity on the bytes it is given. These tests drive the
// engine to show that the factory's normalization (the domain folding
// inbound applies) puts a start, the send it queues and the replies it
// provokes on one identity.

// seedLocked is seed with lock_to_single set: one open conversation per
// contact, so mail from the same identity joins it without header evidence.
func (r *engineRig) seedLocked(ch model.ChannelRef, in model.InboxRef) {
	r.t.Helper()
	lock := true
	sel := model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1}
	err := r.store.Transact(r.ctx, tenant, func(tx store.Tx) error {
		if err := tx.PutChannel(r.ctx, channel(ch, config(r.relay.port)), ""); err != nil {
			return err
		}
		return tx.PutInbox(r.ctx, model.Inbox{Ref: in, Channel: ch, Name: "mail", Destinations: []string{"support"}, Selection: sel, Revision: "in1"}, "")
	})
	if err != nil {
		r.t.Fatal(err)
	}
}

func (r *engineRig) startTo(op model.OperationID, id string) (model.Acceptance, error) {
	r.t.Helper()
	return r.client.Conversations().Start(r.ctx, model.StartConversation{
		Command:   model.Command{Call: model.Call{Correlation: "smtp-recipient"}, Operation: op},
		Inbox:     inboxA,
		Recipient: model.ExternalRef{Channel: channelA, Namespace: smtp.Namespace, Kind: smtp.RouteKind, ID: id},
		Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Your appointment is tomorrow."}}},
	})
}

func (r *engineRig) conversations(in model.InboxRef) []model.Conversation {
	r.t.Helper()
	var out []model.Conversation
	if err := r.store.View(r.ctx, tenant, func(rd store.Reader) error {
		p, err := rd.Conversations(r.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{in}}, Page: model.PageRequest{Limit: 100}})
		out = p.Items
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return out
}

func sourceIDs(ids []model.Identity) []string {
	out := make([]string, 0, len(ids))
	for _, i := range ids {
		out = append(out, i.SourceID)
	}
	return out
}

func wantSources(t *testing.T, ids []model.Identity, want ...string) {
	t.Helper()
	got := sourceIDs(ids)
	if len(got) != len(want) {
		t.Fatalf("identities %q, want %q", got, want)
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Fatalf("identities %q, want %q", got, want)
		}
	}
}

// A start to a mixed-case-domain address claims the identity an inbound from
// that address, in any domain case, does: one identity and, with
// lock_to_single, one conversation.
func TestStartToMixedCaseDomainMeetsTheInboundIdentity(t *testing.T) {
	r := newEngineRig(t)
	r.seedLocked(channelA, inboxA)
	m := r.receive(channelA)
	if _, err := r.startTo("start-1", "Bob@EXAMPLE.org"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	wantSources(t, r.identities(inboxA), "Bob@example.org")
	convs := r.conversations(inboxA)
	if len(convs) != 1 || convs[0].ReplyRoute.ID != "Bob@example.org" {
		t.Fatalf("conversations %+v, want one routed to the folded address", convs)
	}
	tx := mta{from: "Bob@Example.ORG", rcpts: []string{"support@example.com"}}
	if err := m.send(t, tx, mail("<in1@example.org>", "Bob <Bob@Example.ORG>", "", "Thanks")); err != nil {
		t.Fatal(err)
	}
	if err := m.send(t, tx, mail("<in2@example.org>", "Bob@example.org", "", "And one more")); err != nil {
		t.Fatal(err)
	}
	wantSources(t, r.identities(inboxA), "Bob@example.org")
	if got := r.conversations(inboxA); len(got) != 1 || got[0].Ref != convs[0].Ref {
		t.Fatalf("conversations %+v after the replies, want the start's one", got)
	}
	for _, msg := range r.messages(inboxA) {
		if msg.Ref.Conversation != convs[0].Ref {
			t.Fatalf("message %+v outside the start's conversation", msg.Ref)
		}
	}
}

// The send a start queues goes to the folded route. Before the factory
// normalized the recipient, the start was accepted and stored, then failed
// permanently at send because the route was not in its folded form.
func TestStartToMixedCaseDomainSendsToTheFoldedRoute(t *testing.T) {
	r := newEngineRig(t)
	r.seed(channelA, inboxA, "", "support")
	acc, err := r.startTo("start-1", "Bob@EXAMPLE.org")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	op := r.work(acc.Operation.Ref)
	if op.State != model.ProviderAccepted {
		t.Fatalf("operation %s (%+v), want provider_accepted", op.State, op.Failure)
	}
	txs := r.relay.transactions()
	if len(txs) != 1 || len(txs[0].rcpts) != 1 || txs[0].rcpts[0] != "Bob@example.org" {
		t.Fatalf("relay saw %+v, want one transaction to Bob@example.org", txs)
	}
}

// A threaded reply from the recipient lands in the start's conversation
// under the start's identity, not a second one.
func TestStartedConversationTakesTheThreadedReply(t *testing.T) {
	r := newEngineRig(t)
	r.seed(channelA, inboxA, "", "support")
	m := r.receive(channelA)
	acc, err := r.startTo("start-1", "Bob@EXAMPLE.org")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	op := r.work(acc.Operation.Ref)
	if op.State != model.ProviderAccepted || len(op.ProviderReferences) != 1 {
		t.Fatalf("operation %+v, want provider_accepted with the Message-ID", op)
	}
	ourID := op.ProviderReferences[0].ID
	tx := mta{from: "Bob@Example.org", rcpts: []string{"support@example.com"}}
	reply := mail("<in1@example.org>", "Bob@Example.org", "In-Reply-To: "+ourID+"\nReferences: "+ourID, "Yes")
	if err := m.send(t, tx, reply); err != nil {
		t.Fatal(err)
	}
	wantSources(t, r.identities(inboxA), "Bob@example.org")
	convs := r.conversations(inboxA)
	if len(convs) != 1 {
		t.Fatalf("conversations %d, want the start's one", len(convs))
	}
}

// Replaying a start under its operation key with the domain in another case
// is the same operation: replayed, never a conflict, nothing second sent.
func TestStartReplayInAnotherDomainCaseIsTheSameOperation(t *testing.T) {
	r := newEngineRig(t)
	r.seed(channelA, inboxA, "", "support")
	first, err := r.startTo("start-1", "Bob@EXAMPLE.org")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, id := range []string{"Bob@example.org", "Bob@Example.ORG", "Bob@EXAMPLE.org"} {
		again, err := r.startTo("start-1", id)
		if err != nil {
			t.Fatalf("replay as %q: %v", id, err)
		}
		if !again.Replayed || again.Operation.Ref != first.Operation.Ref {
			t.Fatalf("replay as %q = %+v, want the first operation replayed", id, again)
		}
	}
	wantSources(t, r.identities(inboxA), "Bob@example.org")
	if n := len(r.conversations(inboxA)); n != 1 {
		t.Fatalf("conversations = %d, want 1", n)
	}
	if op := r.work(first.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("operation %s, want provider_accepted", op.State)
	}
	if txs := r.relay.transactions(); len(txs) != 1 {
		t.Fatalf("relay transactions = %d, want the one operation's", len(txs))
	}
}

// The local part is case-sensitive (RFC 5321 section 2.4): Bob@ and bob@ are
// two mailboxes, two identities, and one operation key cannot name both.
func TestStartKeepsTheLocalPartExact(t *testing.T) {
	r := newEngineRig(t)
	r.seed(channelA, inboxA, "", "support")
	if _, err := r.startTo("start-1", "Bob@EXAMPLE.org"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := r.startTo("start-2", "bob@EXAMPLE.org"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	wantSources(t, r.identities(inboxA), "Bob@example.org", "bob@example.org")
	if _, err := r.startTo("start-1", "bob@example.org"); model.CodeOf(err) != model.Conflict {
		t.Fatalf("start-1 for another mailbox = %v, want a conflict", err)
	}
}

// A recipient that is not a bare address is refused at the call, before
// anything is stored: the operation key stays free.
func TestStartRefusesWhatIsNotABareAddress(t *testing.T) {
	r := newEngineRig(t)
	r.seed(channelA, inboxA, "", "support")
	for _, id := range []string{
		"Bob <bob@example.org>", "<bob@example.org>", "bob@example.org, ann@example.org", " bob@example.org",
		"bob@example.org ", "bob", "", `"bob smith"@example.org`,
	} {
		_, err := r.startTo("start-1", id)
		if model.CodeOf(err) != model.Validation {
			t.Fatalf("start to %q = %v, want a validation error", id, err)
		}
	}
	if n := len(r.identities(inboxA)) + len(r.conversations(inboxA)); n != 0 {
		t.Fatalf("%d records stored by refused starts", n)
	}
	acc, err := r.startTo("start-1", "bob@example.org")
	if err != nil || acc.Replayed {
		t.Fatalf("start under the same key after refusals = %+v, %v; want a fresh operation", acc, err)
	}
}

// NormalizeRecipient folds the domain of an smtp.mail address and changes
// nothing else; other references are returned for core to refuse.
func TestFactoryNormalizeRecipient(t *testing.T) {
	var f adapter.Factory = smtp.NewFactory(smtp.Options{})
	n, ok := f.(adapter.RecipientNormalizer)
	if !ok {
		t.Fatal("the smtp factory does not implement adapter.RecipientNormalizer")
	}
	ref := func(ns, kind, id string) model.ExternalRef {
		return model.ExternalRef{Channel: channelA, Namespace: ns, Kind: kind, ID: id, ParentID: "p"}
	}
	for _, tt := range []struct {
		name string
		in   model.ExternalRef
		want string
	}{
		{"mixed case domain", ref(smtp.Namespace, smtp.RouteKind, "Bob@EXAMPLE.org"), "Bob@example.org"},
		{"folded already", ref(smtp.Namespace, smtp.RouteKind, "bob@example.org"), "bob@example.org"},
		{"local part kept", ref(smtp.Namespace, smtp.RouteKind, "BOB.Smith+Tag@Example.ORG"), "BOB.Smith+Tag@example.org"},
		{"another namespace", ref("other.ns", smtp.RouteKind, "Bob@EXAMPLE.org"), "Bob@EXAMPLE.org"},
		{"another kind", ref(smtp.Namespace, "thread", "Bob@EXAMPLE.org"), "Bob@EXAMPLE.org"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := n.NormalizeRecipient(tt.in)
			if err != nil {
				t.Fatalf("NormalizeRecipient: %v", err)
			}
			want := tt.in
			want.ID = tt.want
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
			again, err := n.NormalizeRecipient(got)
			if err != nil || again != got {
				t.Fatalf("second pass %+v, %v; want %+v", again, err, got)
			}
		})
	}
	if _, err := n.NormalizeRecipient(ref(smtp.Namespace, smtp.RouteKind, "Bob <b@x.org>")); model.CodeOf(err) != model.Validation {
		t.Fatalf("display-name form = %v, want a validation error", err)
	}
}
