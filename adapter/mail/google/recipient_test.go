package google

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// A start names its recipient before any inbound message does, and core keys
// the recipient's identity on the bytes it is given. These tests drive the
// engine to show that the factory's normalization and the inbound sender key
// meet on one spelling: the address with its domain lowercased and its local
// part byte-exact.

// seedLocked is seed with lock_to_single set: one open conversation per
// contact, so mail from the same identity joins it without header evidence.
func (r *engineRig) seedLocked(ch model.ChannelRef, in model.InboxRef, user string, destinations ...string) {
	r.t.Helper()
	r.seed(ch, in, user, destinations...)
	lock := true
	sel := model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1}
	err := r.store.Transact(r.ctx, ch.Tenant, func(tx store.Tx) error {
		return tx.PutInbox(r.ctx, model.Inbox{Ref: in, Channel: ch, Name: "gmail", Destinations: destinations, Selection: sel, Revision: "in2"}, "in1")
	})
	if err != nil {
		r.t.Fatal(err)
	}
}

func (r *engineRig) client() uma.Client {
	r.t.Helper()
	c, err := r.engine.For(model.Principal{Tenant: testChan.Tenant, Actor: "operator"})
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

func (r *engineRig) startTo(op model.OperationID, id string) (model.Acceptance, error) {
	r.t.Helper()
	return r.client().Conversations().Start(r.ctx, model.StartConversation{
		Command:   model.Command{Call: model.Call{Correlation: "gmail-recipient"}, Operation: op},
		Inbox:     engineInboxA,
		Recipient: model.ExternalRef{Channel: testChan, Namespace: IdentityNamespace, Kind: refKindAddress, ID: id},
		Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Your appointment is tomorrow."}}},
	})
}

func (r *engineRig) conversations(in model.InboxRef) []model.Conversation {
	r.t.Helper()
	var out []model.Conversation
	if err := r.store.View(r.ctx, in.Tenant, func(rd store.Reader) error {
		p, err := rd.Conversations(r.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{in}}, Page: model.PageRequest{Limit: 100}})
		out = p.Items
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return out
}

// work runs a worker until operation ref leaves queued, then stops it.
func (r *engineRig) work(ref model.OperationRef) model.Operation {
	r.t.Helper()
	ctx, cancel := contextWithCancel(r.t)
	done := make(chan error, 1)
	go func() {
		done <- r.engine.Runtime().Run(ctx, engineCall, uma.WorkerOptions{Owner: "gmail-test", Limit: 10, Poll: 10 * time.Millisecond})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, ctx.Err()) {
			r.t.Errorf("Run: %v", err)
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		op, err := r.client().Operations().Get(r.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "c-get"}, Ref: ref})
		if err != nil {
			r.t.Fatal(err)
		}
		if (op.State != model.Queued && op.State != model.RetryPending) || time.Now().After(deadline) {
			return op
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// sentTo is the To address of each message the mailbox sent, in order.
func (r *engineRig) sentTo(user string) []string {
	r.t.Helper()
	r.g.mu.Lock()
	defer r.g.mu.Unlock()
	var out []string
	for _, s := range r.g.boxes[user].sends {
		raw, err := base64.URLEncoding.DecodeString(s.Raw)
		if err != nil {
			raw, err = base64.RawURLEncoding.DecodeString(s.Raw)
		}
		if err != nil {
			r.t.Fatal(err)
		}
		m, err := mime.Parse(bytes.NewReader(raw), mime.Limits{})
		if err != nil || len(m.Header.To) != 1 {
			r.t.Fatalf("sent message: %v, %+v", err, m)
		}
		out = append(out, m.Header.To[0].Address)
	}
	return out
}

func wantSources(t *testing.T, ids []model.Identity, want ...string) {
	t.Helper()
	got := map[string]bool{}
	for _, i := range ids {
		got[i.SourceID] = true
	}
	if len(ids) != len(want) {
		t.Fatalf("identities %v, want %q", got, want)
	}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("identities %v, want %q", got, want)
		}
	}
}

func (r *engineRig) pushOK(ch model.ChannelRef, user string) {
	r.t.Helper()
	if code := r.push(ch, user); code != http.StatusNoContent {
		r.t.Fatalf("push = %d", code)
	}
}

// A start to a mixed-case-domain address claims the identity an inbound from
// that address, in any domain case, does: one identity and, with
// lock_to_single, one conversation.
func TestStartToMixedCaseDomainMeetsTheInboundIdentity(t *testing.T) {
	r := newEngineRig(t)
	r.seedLocked(testChan, engineInboxA, testMailbox, "support@example.test")
	r.receive(testChan, testMailbox)
	if _, err := r.startTo("start-1", "Bob@EXAMPLE.org"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	wantSources(t, r.identities(engineInboxA), "Bob@example.org")
	convs := r.conversations(engineInboxA)
	if len(convs) != 1 || convs[0].ReplyRoute.ID != "Bob@example.org" {
		t.Fatalf("conversations %+v, want one routed to the folded address", convs)
	}
	r.g.deliver(testMailbox, "m1", "t1", rawMessage(`Bob <Bob@Example.ORG>`, "support@example.test", "<in1@example.org>", "Re", "Thanks"))
	r.g.deliver(testMailbox, "m2", "t2", rawMessage("Bob@example.org", "support@example.test", "<in2@example.org>", "Re", "And more"))
	r.pushOK(testChan, testMailbox)
	msgs := r.waitMessages(engineInboxA, 3)
	wantSources(t, r.identities(engineInboxA), "Bob@example.org")
	if got := r.conversations(engineInboxA); len(got) != 1 || got[0].Ref != convs[0].Ref {
		t.Fatalf("conversations %+v after the replies, want the start's one", got)
	}
	for _, m := range msgs {
		if m.Ref.Conversation != convs[0].Ref {
			t.Fatalf("message %+v outside the start's conversation", m.Ref)
		}
	}
}

// The send a start queues goes to the folded route.
func TestStartToMixedCaseDomainSendsToTheFoldedRoute(t *testing.T) {
	r := newEngineRig(t)
	r.seed(testChan, engineInboxA, testMailbox, "support@example.test")
	acc, err := r.startTo("start-1", "Bob@EXAMPLE.org")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if op := r.work(acc.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("operation %s (%+v), want provider_accepted", op.State, op.Failure)
	}
	if got := r.sentTo(testMailbox); len(got) != 1 || got[0] != "Bob@example.org" {
		t.Fatalf("sent to %q, want one message to Bob@example.org", got)
	}
}

// A threaded reply from the recipient lands in the start's conversation
// under the start's identity, not a second one.
func TestStartedConversationTakesTheThreadedReply(t *testing.T) {
	r := newEngineRig(t)
	r.seed(testChan, engineInboxA, testMailbox, "support@example.test")
	r.receive(testChan, testMailbox)
	acc, err := r.startTo("start-1", "Bob@EXAMPLE.org")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	op := r.work(acc.Operation.Ref)
	if op.State != model.ProviderAccepted {
		t.Fatalf("operation %s (%+v), want provider_accepted", op.State, op.Failure)
	}
	var ourID string
	for _, ref := range op.ProviderReferences {
		if ref.Namespace == model.EmailHeaderNamespace {
			ourID = ref.ID
		}
	}
	if ourID == "" {
		t.Fatalf("operation references %+v lack the Message-ID", op.ProviderReferences)
	}
	r.g.deliver(testMailbox, "m1", "t1", rawMessage("Bob@Example.org", "support@example.test", "<in1@example.org>", "Re", "Yes",
		"In-Reply-To: "+ourID, "References: "+ourID))
	r.pushOK(testChan, testMailbox)
	r.waitMessages(engineInboxA, 2)
	wantSources(t, r.identities(engineInboxA), "Bob@example.org")
	if n := len(r.conversations(engineInboxA)); n != 1 {
		t.Fatalf("conversations %d, want the start's one", n)
	}
}

// Replaying a start under its operation key with the domain in another case
// is the same operation: replayed, never a conflict, nothing second sent.
func TestStartReplayInAnotherDomainCaseIsTheSameOperation(t *testing.T) {
	r := newEngineRig(t)
	r.seed(testChan, engineInboxA, testMailbox, "support@example.test")
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
	wantSources(t, r.identities(engineInboxA), "Bob@example.org")
	if n := len(r.conversations(engineInboxA)); n != 1 {
		t.Fatalf("conversations = %d, want 1", n)
	}
	if op := r.work(first.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("operation %s, want provider_accepted", op.State)
	}
	if got := r.sentTo(testMailbox); len(got) != 1 {
		t.Fatalf("sent %q, want the one operation's message", got)
	}
}

// The local part is case-sensitive (RFC 5321 section 2.4): Bob@ and bob@ are
// two mailboxes, two identities, and one operation key cannot name both.
func TestStartKeepsTheLocalPartExact(t *testing.T) {
	r := newEngineRig(t)
	r.seed(testChan, engineInboxA, testMailbox, "support@example.test")
	if _, err := r.startTo("start-1", "Bob@EXAMPLE.org"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := r.startTo("start-2", "bob@EXAMPLE.org"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	wantSources(t, r.identities(engineInboxA), "Bob@example.org", "bob@example.org")
	if _, err := r.startTo("start-1", "bob@example.org"); model.CodeOf(err) != model.Conflict {
		t.Fatalf("start-1 for another mailbox = %v, want a conflict", err)
	}
}

// A recipient that is not a bare address is refused at the call, before
// anything is stored: the operation key stays free.
func TestStartRefusesWhatIsNotABareAddress(t *testing.T) {
	r := newEngineRig(t)
	r.seed(testChan, engineInboxA, testMailbox, "support@example.test")
	for _, id := range []string{
		"Bob <bob@example.org>", "<bob@example.org>", "bob@example.org, ann@example.org", " bob@example.org",
		"bob@example.org ", "bob", "", `"bob smith"@example.org`,
	} {
		_, err := r.startTo("start-1", id)
		if model.CodeOf(err) != model.Validation {
			t.Fatalf("start to %q = %v, want a validation error", id, err)
		}
	}
	if n := len(r.identities(engineInboxA)) + len(r.conversations(engineInboxA)); n != 0 {
		t.Fatalf("%d records stored by refused starts", n)
	}
	acc, err := r.startTo("start-1", "bob@example.org")
	if err != nil || acc.Replayed {
		t.Fatalf("start under the same key after refusals = %+v, %v; want a fresh operation", acc, err)
	}
}

// A sender whose From domain has an uppercase letter is keyed with the
// domain lowercased and the local part as written: the key a start to the
// same address claims. Mail from a Sender header, when it has no From, is
// keyed the same way.
func TestInboundSenderKeyFoldsTheDomain(t *testing.T) {
	r := newEngineRig(t)
	r.seed(testChan, engineInboxA, testMailbox, "support@example.test")
	r.receive(testChan, testMailbox)
	r.g.deliver(testMailbox, "m1", "t1", rawMessage("Jane Doe <Jane.Doe@Example.ORG>", "support@example.test", "<k1@example.org>", "Hi", "one"))
	r.g.deliver(testMailbox, "m2", "t2", rawMessage("Jane.Doe@EXAMPLE.org", "support@example.test", "<k2@example.org>", "Hi", "two"))
	r.g.deliver(testMailbox, "m3", "t3", rawMessage("jane.doe@example.org", "support@example.test", "<k3@example.org>", "Hi", "three"))
	r.pushOK(testChan, testMailbox)
	msgs := r.waitMessages(engineInboxA, 3)
	wantSources(t, r.identities(engineInboxA), "Jane.Doe@example.org", "jane.doe@example.org")
	m1, m2 := byProviderID(msgs, testChan, "m1"), byProviderID(msgs, testChan, "m2")
	if m1 == nil || m2 == nil || m1.Author.External.ID != "Jane.Doe@example.org" || m1.Author.Identity != m2.Author.Identity {
		t.Fatalf("authors %+v / %+v, want one identity for Jane.Doe@example.org", m1, m2)
	}
}

func TestAuthorFoldsTheDomainOnly(t *testing.T) {
	addr := func(a string) []mime.Address { return []mime.Address{{Address: a}} }
	for _, tt := range []struct {
		name string
		h    mime.Header
		want string
	}{
		{"from", mime.Header{From: addr("Bob@EXAMPLE.org")}, "Bob@example.org"},
		{"from wins over sender", mime.Header{From: addr("Bob@EXAMPLE.org"), Sender: addr("Ann@Example.org")}, "Bob@example.org"},
		{"sender when no from", mime.Header{Sender: addr("Ann@Example.ORG")}, "Ann@example.org"},
		{"local part kept", mime.Header{From: addr("A.B+Tag@EXAMPLE.org")}, "A.B+Tag@example.org"},
		{"folded already", mime.Header{From: addr("bob@example.org")}, "bob@example.org"},
		// What cannot be normalized is keyed as the parser gave it: the
		// adapter never invents a spelling.
		{"unquoted local part with a space", mime.Header{From: addr("john doe@Example.ORG")}, "john doe@Example.ORG"},
		{"no at sign", mime.Header{From: addr("Bob")}, "Bob"},
		{"none", mime.Header{}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := author(tt.h); got != tt.want {
				t.Fatalf("author = %q, want %q", got, tt.want)
			}
		})
	}
}

// NormalizeRecipient folds the domain of a google.mail address and changes
// nothing else; other references are returned for core to refuse.
func TestFactoryNormalizeRecipient(t *testing.T) {
	var f adapter.Factory = NewFactory(Options{})
	n, ok := f.(adapter.RecipientNormalizer)
	if !ok {
		t.Fatal("the google factory does not implement adapter.RecipientNormalizer")
	}
	ref := func(ns, kind, id string) model.ExternalRef {
		return model.ExternalRef{Channel: testChan, Namespace: ns, Kind: kind, ID: id, ParentID: "p"}
	}
	for _, tt := range []struct {
		name string
		in   model.ExternalRef
		want string
	}{
		{"mixed case domain", ref(IdentityNamespace, refKindAddress, "Bob@EXAMPLE.org"), "Bob@example.org"},
		{"folded already", ref(IdentityNamespace, refKindAddress, "bob@example.org"), "bob@example.org"},
		{"local part kept", ref(IdentityNamespace, refKindAddress, "BOB.Smith+Tag@Example.ORG"), "BOB.Smith+Tag@example.org"},
		{"another namespace", ref("other.ns", refKindAddress, "Bob@EXAMPLE.org"), "Bob@EXAMPLE.org"},
		{"another kind", ref(IdentityNamespace, "thread", "Bob@EXAMPLE.org"), "Bob@EXAMPLE.org"},
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
	if _, err := n.NormalizeRecipient(ref(IdentityNamespace, refKindAddress, "Bob <b@x.org>")); model.CodeOf(err) != model.Validation {
		t.Fatalf("display-name form = %v, want a validation error", err)
	}
}
