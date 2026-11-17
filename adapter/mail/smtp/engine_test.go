package smtp_test

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/smtp"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

var (
	inboxA = model.InboxRef{Tenant: tenant, ID: "00000c00-0000-7000-8000-0000000000a1"}
	inboxB = model.InboxRef{Tenant: tenant, ID: "00000c00-0000-7000-8000-0000000000b1"}
)

// engineRig is the real engine over a memory store with smtp.mail
// registered; receivers run through Runtime.Receive.
type engineRig struct {
	t      *testing.T
	ctx    context.Context
	store  store.Store
	engine uma.Engine
	client uma.Client
	relay  *relay

	mu    sync.Mutex
	addrs map[model.ChannelRef]string
}

func newEngineRig(t *testing.T) *engineRig { return newEngineRigBlobs(t, nil) }

// newEngineRigBlobs is newEngineRig with the host's blob store; without
// one the engine cannot stage attachments.
func newEngineRigBlobs(t *testing.T, blobs ports.BlobStore) *engineRig {
	t.Helper()
	clock := portstest.NewFixedClock(now)
	r := &engineRig{
		t: t, ctx: t.Context(), relay: newRelay(t), addrs: map[model.ChannelRef]string{},
		store: memory.New(memory.Options{Clock: clock}),
	}
	f := smtp.NewFactory(smtp.Options{
		Listen: func(_ context.Context, ch model.Channel) (net.Listener, error) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err == nil {
				r.mu.Lock()
				r.addrs[ch.Ref] = ln.Addr().String()
				r.mu.Unlock()
			}
			return ln, err
		},
		Resolver: newZone(),
		RootCAs:  certs(t).roots,
	})
	e, err := uma.New(uma.Config{
		Store: r.store,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Selectors:   map[string]policy.ConversationSelector{"test.foreign": foreignSelector{}},
		Adapters:    []adapter.Factory{f},
		Credentials: passwordHost(),
		Blobs:       blobs,
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(0xc0),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	r.engine = e
	if r.client, err = e.For(model.Principal{Tenant: tenant, Actor: "operator"}); err != nil {
		t.Fatal(err)
	}
	return r
}

// foreignSelector picks a conversation of another tenant; core rejects it.
type foreignSelector struct{}

func (foreignSelector) Select(context.Context, policy.SelectionInput) (policy.SelectionResult, error) {
	ref := model.ConversationRef{Inbox: model.InboxRef{Tenant: "other", ID: "00000c00-0000-7000-8000-0000000000f1"}, ID: "00000c00-0000-7000-8000-0000000000f2"}
	return policy.SelectionResult{Existing: &ref}, nil
}

func (r *engineRig) seed(ch model.ChannelRef, in model.InboxRef, policyName string, destinations ...string) {
	r.t.Helper()
	lock := false
	sel := model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1, Policy: policyName}
	err := r.store.Transact(r.ctx, tenant, func(tx store.Tx) error {
		if err := tx.PutChannel(r.ctx, channel(ch, config(r.relay.port)), ""); err != nil {
			return err
		}
		return tx.PutInbox(r.ctx, model.Inbox{Ref: in, Channel: ch, Name: "mail", Destinations: destinations, Selection: sel, Revision: "in1"}, "")
	})
	if err != nil {
		r.t.Fatal(err)
	}
}

var runtimeCall = uma.RuntimeCall{Principal: model.Principal{Tenant: tenant, Actor: "mx-service"}, Call: model.Call{Correlation: "smtp-test"}}

// receive runs Runtime.Receive for ch until the test ends and returns an MX
// handle on its listener.
func (r *engineRig) receive(ch model.ChannelRef) *mx {
	r.t.Helper()
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.engine.Runtime().Receive(ctx, runtimeCall, ch)
	}()
	r.t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.Lock()
		addr := r.addrs[ch]
		r.mu.Unlock()
		if addr != "" {
			return &mx{addr: addr}
		}
		if time.Now().After(deadline) {
			r.t.Fatal("receiver did not start")
		}
		time.Sleep(time.Millisecond)
	}
}

func (r *engineRig) messages(in model.InboxRef) []model.Message {
	r.t.Helper()
	var out []model.Message
	if err := r.store.View(r.ctx, tenant, func(rd store.Reader) error {
		p, err := rd.Messages(r.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{in}}, Page: model.PageRequest{Limit: 100}})
		out = p.Items
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return out
}

func (r *engineRig) identities(in model.InboxRef) []model.Identity {
	r.t.Helper()
	var out []model.Identity
	if err := r.store.View(r.ctx, tenant, func(rd store.Reader) error {
		p, err := rd.Identities(r.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{in}}, Page: model.PageRequest{Limit: 100}})
		out = p.Items
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return out
}

func byProviderID(msgs []model.Message, ch model.ChannelRef, id string) *model.Message {
	ref := model.ProviderMessageRef(ch, id)
	for i := range msgs {
		if slices.Contains(msgs[i].External, ref) {
			return &msgs[i]
		}
	}
	return nil
}

func mail(id, from, extra, body string) string {
	if extra != "" {
		extra += "\n"
	}
	return "From: " + from + "\nTo: support@example.com\nSubject: Help\nMessage-ID: " + id + "\n" + extra + "\n" + body + "\n"
}

func TestEngineIngress(t *testing.T) {
	r := newEngineRig(t)
	r.seed(channelA, inboxA, "", "support")
	m := r.receive(channelA)

	first := mail("<t1@example.org>", "Jane <jane@example.org>", "", "First question")
	if err := m.send(t, janeTx, first); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Redelivery of the same message, and a reply threading on it.
	if err := m.send(t, janeTx, first); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	reply := mail("<t2@example.org>", "jane@example.org", "In-Reply-To: <t1@example.org>\nReferences: <t1@example.org>\nX-Claimed-Sender: admin@example.com", "Follow-up")
	if err := m.send(t, janeTx, reply); err != nil {
		t.Fatalf("reply: %v", err)
	}
	msgs := r.messages(inboxA)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (redelivery deduplicated)", len(msgs))
	}
	m1, m2 := byProviderID(msgs, channelA, "<t1@example.org>"), byProviderID(msgs, channelA, "<t2@example.org>")
	if m1 == nil || m2 == nil {
		t.Fatalf("messages lack provider Message-ID references: %+v", msgs)
	}
	if m1.Ref.Conversation != m2.Ref.Conversation {
		t.Errorf("reply landed in %v, want the thread's %v", m2.Ref.Conversation, m1.Ref.Conversation)
	}
	if m1.Author.External.ID != "jane@example.org" || m1.Author.External.Namespace != smtp.Namespace || m1.Author.Identity != m2.Author.Identity {
		t.Errorf("authors %+v / %+v, want one identity for jane@example.org", m1.Author, m2.Author)
	}
	if m1.Author.Assurance != smtp.AssuranceDMARCNone || m1.Source != model.TypeSmtpMail || m1.Content.Parts[0].Text != "First question" {
		t.Errorf("message = %+v", m1)
	}
	if !slices.ContainsFunc(m1.Metadata, func(x model.Extension) bool { return x.Name == smtp.MessageSchema }) {
		t.Errorf("metadata %+v lacks %s", m1.Metadata, smtp.MessageSchema)
	}
	ids := r.identities(inboxA)
	if len(ids) != 1 || ids[0].SourceID != "jane@example.org" || ids[0].LinkAssurance != model.LinkAssuranceNone {
		t.Errorf("identities = %+v, want jane only, unlinked", ids)
	}

	// Without a header match, flag=false: a new mail is a new conversation.
	if err := m.send(t, janeTx, mail("<t3@example.org>", "jane@example.org", "", "Other topic")); err != nil {
		t.Fatal(err)
	}
	m3 := byProviderID(r.messages(inboxA), channelA, "<t3@example.org>")
	if m3 == nil || m3.Ref.Conversation == m1.Ref.Conversation {
		t.Errorf("unthreaded mail joined %v; same sender is not the same topic", m1.Ref.Conversation)
	}
}

// A Message-ID is the sender's to choose and anyone can repeat it, so it
// never decides that a mail was already seen: the bytes do. A forged mail
// that reuses the Message-ID of a genuine one and arrives first must not
// make the genuine mail a duplicate.
func TestEngineSameMessageIDDifferentMailIsNotADuplicate(t *testing.T) {
	r := newEngineRig(t)
	r.seed(channelA, inboxA, "", "support")
	m := r.receive(channelA)

	const id = "<claimed@example.org>"
	forged := mail(id, "attacker@example.net", "", "forged")
	genuine := mail(id, "Jane <jane@example.org>", "", "the real question")
	other := mail(id, "jane@example.org", "", "another mail under the same id")
	attacker := mta{from: "attacker@example.net", rcpts: janeTx.rcpts}
	for _, s := range []struct {
		tx  mta
		raw string
	}{{attacker, forged}, {janeTx, genuine}, {janeTx, other}} {
		if err := m.send(t, s.tx, s.raw); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(r.messages(inboxA)); n != 3 {
		t.Fatalf("messages = %d, want 3: mails sharing a Message-ID but not bytes are distinct", n)
	}
	// An MTA retry resends the same bytes: not a new message.
	if err := m.send(t, janeTx, genuine); err != nil {
		t.Fatal(err)
	}
	if err := m.send(t, attacker, forged); err != nil {
		t.Fatal(err)
	}
	msgs := r.messages(inboxA)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d after redeliveries, want 3", len(msgs))
	}
	var texts []string
	for _, msg := range msgs {
		texts = append(texts, msg.Content.Parts[0].Text)
	}
	for _, want := range []string{"forged", "the real question", "another mail under the same id"} {
		if !slices.Contains(texts, want) {
			t.Errorf("stored texts %q lack %q", texts, want)
		}
	}
}

// One DATA to two destinations of one inbox lands once in each, and a
// redelivery of it lands in neither again.
func TestEngineFanOutIsPerDestination(t *testing.T) {
	r := newEngineRig(t)
	r.seed(channelA, inboxA, "", "support", "sales")
	m := r.receive(channelA)
	tx := janeTx
	tx.rcpts = []string{"support@example.com", "sales@example.com"}
	msg := mail("<fan@example.org>", "jane@example.org", "", "to two destinations")
	for range 2 {
		if err := m.send(t, tx, msg); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(r.messages(inboxA)); n != 2 {
		t.Fatalf("messages = %d, want 2: one per destination, none for the redelivery", n)
	}
}

func TestEngineBoundInboxOnly(t *testing.T) {
	r := newEngineRig(t)
	r.seed(channelA, inboxA, "", "support")
	r.seed(channelB, inboxB, "", "support", "sales")
	a, b := r.receive(channelA), r.receive(channelB)
	msg := mail("<b1@example.org>", "jane@example.org", "", "to both accounts")
	if err := a.send(t, janeTx, msg); err != nil {
		t.Fatal(err)
	}
	if err := b.send(t, janeTx, msg); err != nil {
		t.Fatal(err)
	}
	if na, nb := len(r.messages(inboxA)), len(r.messages(inboxB)); na != 1 || nb != 1 {
		t.Fatalf("inbox A %d, B %d messages; want one each", na, nb)
	}
	// sales is configured on both channels but bound only on B.
	tx := janeTx
	tx.rcpts = []string{"sales@example.com"}
	if err := a.send(t, tx, mail("<b2@example.org>", "jane@example.org", "", "sales")); replyCode(err) != 550 {
		t.Fatalf("mail to a destination channel A does not bind = %v, want 550", err)
	}
	if n := len(r.messages(inboxB)); n != 1 {
		t.Fatalf("inbox B holds %d messages after account A's mail, want 1", n)
	}
}

func TestEngineRejectedSelectionStaysWithSender(t *testing.T) {
	r := newEngineRig(t)
	r.seed(channelA, inboxA, "test.foreign", "support")
	m := r.receive(channelA)
	if err := m.send(t, janeTx, mail("<s1@example.org>", "jane@example.org", "", "hi")); replyCode(err) != 451 {
		t.Fatalf("send = %v, want 451: the sending MTA keeps the message", err)
	}
	if n := len(r.messages(inboxA)); n != 0 {
		t.Fatalf("messages = %d, want none", n)
	}
}

// TestEngineReply sends a reply through the relay under the uma-owned
// Message-ID, which the operation reports as its provider reference.
func TestEngineReply(t *testing.T) {
	r := newEngineRig(t)
	r.seed(channelA, inboxA, "", "support")
	m := r.receive(channelA)
	if err := m.send(t, janeTx, mail("<q1@example.org>", "jane@example.org", "", "Question")); err != nil {
		t.Fatal(err)
	}
	conv := byProviderID(r.messages(inboxA), channelA, "<q1@example.org>").Ref.Conversation
	acc, err := r.client.Messages().Reply(r.ctx, model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: "c-reply"}, Operation: "op-reply"},
		Conversation: conv,
		Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Answer"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	op := r.work(acc.Operation.Ref)
	if op.State != model.ProviderAccepted || len(op.ProviderReferences) != 1 {
		t.Fatalf("operation = %+v, want provider_accepted with the Message-ID", op)
	}
	ourID := op.ProviderReferences[0].ID
	txs := r.relay.transactions()
	if len(txs) != 1 || txs[0].rcpts[0] != "jane@example.org" || !strings.Contains(string(txs[0].data), "Message-Id: "+ourID) {
		t.Fatalf("relay saw %+v, want the reply to jane with Message-ID %s", txs, ourID)
	}
}

// work runs a worker until operation ref leaves queued, then stops it.
// Drain would also stop the tenant's receivers.
func (r *engineRig) work(ref model.OperationRef) model.Operation {
	r.t.Helper()
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan error, 1)
	go func() {
		done <- r.engine.Runtime().Run(ctx, runtimeCall, uma.WorkerOptions{Owner: "smtp-test", Limit: 10, Poll: 10 * time.Millisecond})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			r.t.Errorf("Run: %v", err)
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		op, err := r.client.Operations().Get(r.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "c-get"}, Ref: ref})
		if err != nil {
			r.t.Fatal(err)
		}
		if (op.State != model.Queued && op.State != model.RetryPending) || time.Now().After(deadline) {
			return op
		}
		time.Sleep(5 * time.Millisecond)
	}
}
