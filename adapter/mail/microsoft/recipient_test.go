package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// A start names its recipient before any inbound message does, and core keys
// the recipient's identity on the bytes it is given. These tests drive the
// engine to show that the factory's normalization and the inbound sender key
// meet on one spelling: the address with its domain lowercased and its local
// part byte-exact.

var recipientInbox = model.InboxRef{Tenant: testChan.Tenant, ID: "00000c00-0000-7000-8000-0000000000a1"}

var recipientCall = uma.RuntimeCall{Principal: model.Principal{Tenant: testChan.Tenant, Actor: "graph-service"}, Call: model.Call{Correlation: "graph-test"}}

// recipientRig is the real engine over a memory store with microsoft.mail
// registered against the fake Graph API.
type recipientRig struct {
	t      *testing.T
	ctx    context.Context
	g      *fakeGraph
	store  store.Store
	engine uma.Engine
}

// newRecipientRig seeds a per-contact inbox on testChan; lock sets
// lock_to_single, so mail from the same identity joins its one open
// conversation without header evidence.
func newRecipientRig(t *testing.T, lock bool) *recipientRig {
	t.Helper()
	clock := portstest.NewFixedClock(time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	r := &recipientRig{t: t, ctx: t.Context(), g: newFakeGraph(t), store: memory.New(memory.Options{Clock: clock})}
	f := NewFactory(Options{Endpoint: r.g.endpoint(), PageSize: 10})
	e, err := uma.New(uma.Config{
		Store: r.store,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{f},
		Extensions:  f.Extensions(),
		Credentials: &testCreds{secret: []byte("notification-secret")},
		HTTP:        r.g.srv.Client(),
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(0x9c),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	r.engine = e
	r.g.mailbox(testMailbox, testChan)
	data, _ := json.Marshal(Config{Mailbox: testMailbox, Addresses: []string{"support@example.test"}})
	sel := model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1}
	if err := r.store.Transact(r.ctx, testChan.Tenant, func(tx store.Tx) error {
		if err := tx.PutChannel(r.ctx, model.Channel{Ref: testChan, Type: Type, CredentialRef: "cred", Revision: "ch1",
			Configuration: model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data}}, ""); err != nil {
			return err
		}
		return tx.PutInbox(r.ctx, model.Inbox{Ref: recipientInbox, Channel: testChan, Name: "graph", Destinations: []string{"support@example.test"}, Selection: sel, Revision: "in1"}, "")
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *recipientRig) client() uma.Client {
	r.t.Helper()
	c, err := r.engine.For(model.Principal{Tenant: testChan.Tenant, Actor: "operator"})
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

func (r *recipientRig) startTo(op model.OperationID, id string) (model.Acceptance, error) {
	r.t.Helper()
	return r.client().Conversations().Start(r.ctx, model.StartConversation{
		Command:   model.Command{Call: model.Call{Correlation: "graph-recipient"}, Operation: op},
		Inbox:     recipientInbox,
		Recipient: model.ExternalRef{Channel: testChan, Namespace: IdentityNamespace, Kind: refKindAddress, ID: id},
		Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Your appointment is tomorrow."}}},
	})
}

// receive runs Runtime.Receive until the test ends.
func (r *recipientRig) receive() {
	r.t.Helper()
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.engine.Runtime().Receive(ctx, recipientCall, testChan)
	}()
	r.t.Cleanup(func() { cancel(); <-done })
}

// work runs a worker until operation ref leaves queued, then stops it.
func (r *recipientRig) work(ref model.OperationRef) model.Operation {
	r.t.Helper()
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan error, 1)
	go func() {
		done <- r.engine.Runtime().Run(ctx, recipientCall, uma.WorkerOptions{Owner: "graph-test", Limit: 10, Poll: 10 * time.Millisecond})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
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

// sentTo is the first recipient of each send the mailbox made, in order.
func (r *recipientRig) sentTo() []string {
	r.t.Helper()
	r.g.mu.Lock()
	defer r.g.mu.Unlock()
	var out []string
	for _, c := range r.g.mailboxes[testMailbox].calls {
		if c.Capability == model.CapabilitySend {
			out = append(out, c.Route.ID)
		}
	}
	return out
}

func (r *recipientRig) identities() []model.Identity {
	r.t.Helper()
	var out []model.Identity
	if err := r.store.View(r.ctx, testChan.Tenant, func(rd store.Reader) error {
		p, err := rd.Identities(r.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{recipientInbox}}, Page: model.PageRequest{Limit: 100}})
		out = p.Items
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return out
}

func (r *recipientRig) conversations() []model.Conversation {
	r.t.Helper()
	var out []model.Conversation
	if err := r.store.View(r.ctx, testChan.Tenant, func(rd store.Reader) error {
		p, err := rd.Conversations(r.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{recipientInbox}}, Page: model.PageRequest{Limit: 100}})
		out = p.Items
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return out
}

func (r *recipientRig) messages() []model.Message {
	r.t.Helper()
	var out []model.Message
	if err := r.store.View(r.ctx, testChan.Tenant, func(rd store.Reader) error {
		p, err := rd.Messages(r.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{recipientInbox}}, Page: model.PageRequest{Limit: 100}})
		out = p.Items
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return out
}

func (r *recipientRig) waitMessages(n int) []model.Message {
	r.t.Helper()
	waitFor(r.t, func() bool { return len(r.messages()) >= n })
	return r.messages()
}

// deliverFrom puts a message from from, spelled as given, in the mailbox.
func (r *recipientRig) deliverFrom(id, from string, headers ...gHeader) {
	r.t.Helper()
	r.g.deliver(testMailbox, gMessage{
		ID: id, InternetMessageID: "<" + id + "@peer.test>", Subject: "hello",
		From:                   &gRecipient{EmailAddress: gEmailAddress{Address: from}},
		ToRecipients:           []gRecipient{{EmailAddress: gEmailAddress{Address: "support@example.test"}}},
		Body:                   &gItemBody{ContentType: "text", Content: "Yes"},
		InternetMessageHeaders: headers,
	})
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

// A start to a mixed-case-domain address claims the identity an inbound from
// that address, in any domain case, does: one identity and, with
// lock_to_single, one conversation.
func TestStartToMixedCaseDomainMeetsTheInboundIdentity(t *testing.T) {
	r := newRecipientRig(t, true)
	if _, err := r.startTo("start-1", "Bob@EXAMPLE.org"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	wantSources(t, r.identities(), "Bob@example.org")
	convs := r.conversations()
	if len(convs) != 1 || convs[0].ReplyRoute.ID != "Bob@example.org" {
		t.Fatalf("conversations %+v, want one routed to the folded address", convs)
	}
	r.deliverFrom("m1", "Bob@Example.ORG")
	r.deliverFrom("m2", "Bob@example.org")
	r.receive()
	msgs := r.waitMessages(3)
	wantSources(t, r.identities(), "Bob@example.org")
	if got := r.conversations(); len(got) != 1 || got[0].Ref != convs[0].Ref {
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
	r := newRecipientRig(t, false)
	acc, err := r.startTo("start-1", "Bob@EXAMPLE.org")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if op := r.work(acc.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("operation %s (%+v), want provider_accepted", op.State, op.Failure)
	}
	if got := r.sentTo(); len(got) != 1 || got[0] != "Bob@example.org" {
		t.Fatalf("sent to %q, want one message to Bob@example.org", got)
	}
}

// A threaded reply from the recipient lands in the start's conversation
// under the start's identity, not a second one.
func TestStartedConversationTakesTheThreadedReply(t *testing.T) {
	r := newRecipientRig(t, false)
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
	r.deliverFrom("m1", "Bob@Example.org", gHeader{Name: "In-Reply-To", Value: ourID}, gHeader{Name: "References", Value: ourID})
	r.receive()
	r.waitMessages(2)
	wantSources(t, r.identities(), "Bob@example.org")
	if n := len(r.conversations()); n != 1 {
		t.Fatalf("conversations %d, want the start's one", n)
	}
}

// Replaying a start under its operation key with the domain in another case
// is the same operation: replayed, never a conflict, nothing second sent.
func TestStartReplayInAnotherDomainCaseIsTheSameOperation(t *testing.T) {
	r := newRecipientRig(t, false)
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
	wantSources(t, r.identities(), "Bob@example.org")
	if n := len(r.conversations()); n != 1 {
		t.Fatalf("conversations = %d, want 1", n)
	}
	if op := r.work(first.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("operation %s, want provider_accepted", op.State)
	}
	if got := r.sentTo(); len(got) != 1 {
		t.Fatalf("sent %q, want the one operation's message", got)
	}
}

// The local part is case-sensitive (RFC 5321 section 2.4): Bob@ and bob@ are
// two mailboxes, two identities, and one operation key cannot name both.
func TestStartKeepsTheLocalPartExact(t *testing.T) {
	r := newRecipientRig(t, false)
	if _, err := r.startTo("start-1", "Bob@EXAMPLE.org"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := r.startTo("start-2", "bob@EXAMPLE.org"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	wantSources(t, r.identities(), "Bob@example.org", "bob@example.org")
	if _, err := r.startTo("start-1", "bob@example.org"); model.CodeOf(err) != model.Conflict {
		t.Fatalf("start-1 for another mailbox = %v, want a conflict", err)
	}
}

// A recipient that is not a bare address is not refused by the
// normalization: it is kept as given, the key inbound gives such a sender,
// and a send decides on it as before.
func TestStartKeepsWhatIsNotABareAddressAsGiven(t *testing.T) {
	r := newRecipientRig(t, false)
	ids := []string{"Bob <Bob@EXAMPLE.org>", "bob", `"bob smith"@Example.org`, "bob@example.org, ann@example.org"}
	for n, id := range ids {
		if _, err := r.startTo(model.OperationID("start-"+string(rune('a'+n))), id); err != nil {
			t.Fatalf("start to %q: %v", id, err)
		}
	}
	wantSources(t, r.identities(), ids...)
}

// A sender whose address domain has an uppercase letter is keyed with the
// domain lowercased and the local part as written: the key a start to the
// same address claims.
func TestInboundSenderKeyFoldsTheDomain(t *testing.T) {
	r := newRecipientRig(t, false)
	r.deliverFrom("m1", "Jane.Doe@Example.ORG")
	r.deliverFrom("m2", "Jane.Doe@EXAMPLE.org")
	r.deliverFrom("m3", "jane.doe@example.org")
	r.receive()
	r.waitMessages(3)
	wantSources(t, r.identities(), "Jane.Doe@example.org", "jane.doe@example.org")
}

func TestAuthorFoldsTheDomainOnly(t *testing.T) {
	rcp := func(a string) *gRecipient { return &gRecipient{EmailAddress: gEmailAddress{Address: a}} }
	for _, tt := range []struct {
		name string
		m    gMessage
		want string
	}{
		{"from", gMessage{From: rcp("Bob@EXAMPLE.org")}, "Bob@example.org"},
		{"from wins over sender", gMessage{From: rcp("Bob@EXAMPLE.org"), Sender: rcp("Ann@Example.org")}, "Bob@example.org"},
		{"sender when no from", gMessage{Sender: rcp("Ann@Example.ORG")}, "Ann@example.org"},
		{"local part kept", gMessage{From: rcp("A.B+Tag@EXAMPLE.org")}, "A.B+Tag@example.org"},
		{"folded already", gMessage{From: rcp("bob@example.org")}, "bob@example.org"},
		// What cannot be normalized is keyed as Graph gave it: the adapter
		// never invents a spelling.
		{"unquoted local part with a space", gMessage{From: rcp("john doe@Example.ORG")}, "john doe@Example.ORG"},
		{"exchange legacy address", gMessage{From: rcp("/O=EXCH/OU=GROUP/CN=RECIPIENTS/CN=Bob")}, "/O=EXCH/OU=GROUP/CN=RECIPIENTS/CN=Bob"},
		{"none", gMessage{}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := author(tt.m); got != tt.want {
				t.Fatalf("author = %q, want %q", got, tt.want)
			}
		})
	}
}

// NormalizeRecipient folds the domain of a microsoft.mail address and
// changes nothing else; other references are returned for core to refuse.
func TestFactoryNormalizeRecipient(t *testing.T) {
	var f adapter.Factory = NewFactory(Options{})
	n, ok := f.(adapter.RecipientNormalizer)
	if !ok {
		t.Fatal("the microsoft factory does not implement adapter.RecipientNormalizer")
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
	// What is not a bare address is returned as given, never refused.
	for _, id := range []string{"Bob <b@X.org>", "<b@X.org>", "bob", "b@x.org, a@x.org", " b@X.org"} {
		in := ref(IdentityNamespace, refKindAddress, id)
		if got, err := n.NormalizeRecipient(in); err != nil || got != in {
			t.Fatalf("NormalizeRecipient(%q) = %+v, %v; want it unchanged", id, got, err)
		}
	}
}
