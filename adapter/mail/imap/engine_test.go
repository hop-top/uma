package imap_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

var inboxA = model.InboxRef{Tenant: tenant, ID: "00000e00-0000-7000-8000-0000000000a1"}

var runtimeCall = uma.RuntimeCall{Principal: model.Principal{Tenant: tenant, Actor: "imap-service"}, Call: model.Call{Correlation: "imap-test"}}

// engineRig is the real engine over a memory store with imap.mail
// registered; the receiver runs through Runtime.Receive.
type engineRig struct {
	t      *testing.T
	ctx    context.Context
	store  store.Store
	engine uma.Engine
	client uma.Client
	srv    *mailServer
	relay  *relay
	waiter *stepWaiter
	obs    *observer
}

func newEngineRig(t *testing.T, creds ports.Credentials, tweak func(*imap.Config)) *engineRig {
	return newEngineRigBlobs(t, creds, tweak, nil)
}

// newEngineRigBlobs is newEngineRig with the host's blob store; without one
// the engine cannot stage attachments.
func newEngineRigBlobs(t *testing.T, creds ports.Credentials, tweak func(*imap.Config), blobs ports.BlobStore) *engineRig {
	t.Helper()
	r := &engineRig{
		t: t, ctx: t.Context(), store: memory.New(memory.Options{Clock: portstest.NewFixedClock(time.Now())}),
		srv:   newMailServer(t, serverOptions{sec: implicitTLS, mechs: []string{"XOAUTH2", "PLAIN"}, tokenOK: func(tok string) bool { return strings.HasPrefix(tok, "tok-") }, state: true}),
		relay: newRelay(t), waiter: &stepWaiter{}, obs: &observer{},
	}
	f := steppedFactory(t, r.waiter)
	e, err := uma.New(uma.Config{
		Store: r.store,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{f},
		Extensions:  f.Extensions(),
		Credentials: creds,
		Blobs:       blobs,
		Observer:    r.obs,
		Clock:       realClock{},
		IDs:         portstest.NewSequenceIDs(0xe0),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	r.engine = e
	if r.client, err = e.For(model.Principal{Tenant: tenant, Actor: "operator"}); err != nil {
		t.Fatal(err)
	}
	r.srv.create("Archive", goimap.MailboxAttrArchive)
	r.srv.create("Trash", goimap.MailboxAttrTrash)
	r.srv.create("Projects")
	cfg := config(r.srv, r.relay)
	if tweak != nil {
		tweak(&cfg)
	}
	lock := false
	sel := model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1}
	if err := r.store.Transact(r.ctx, tenant, func(tx store.Tx) error {
		if err := tx.PutChannel(r.ctx, channel(channelA, cfg), ""); err != nil {
			return err
		}
		return tx.PutInbox(r.ctx, model.Inbox{Ref: inboxA, Channel: channelA, Name: "mail", Destinations: []string{mailUser}, Selection: sel, Revision: "in1"}, "")
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

// receive runs Runtime.Receive for channel A until the test ends.
func (r *engineRig) receive() {
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.engine.Runtime().Receive(ctx, runtimeCall, channelA)
	}()
	r.t.Cleanup(func() { cancel(); <-done })
}

func (r *engineRig) messages() []model.Message {
	r.t.Helper()
	var out []model.Message
	if err := r.store.View(r.ctx, tenant, func(rd store.Reader) error {
		p, err := rd.Messages(r.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{inboxA}}, Page: model.PageRequest{Limit: 100}})
		out = p.Items
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return out
}

// awaitMessages drives the receiver until the inbox holds n messages, then
// until it waits again: a finished round, so extras would show.
func (r *engineRig) awaitMessages(n int) []model.Message {
	r.t.Helper()
	r.waiter.drive(r.t, func() bool { return len(r.messages()) >= n })
	r.waiter.idle(r.t)
	return r.messages()
}

func byProviderID(msgs []model.Message, id string) *model.Message {
	ref := model.ProviderMessageRef(channelA, id)
	for i := range msgs {
		if slices.Contains(msgs[i].External, ref) {
			return &msgs[i]
		}
	}
	return nil
}

func passwordHost() *credHost { return passwordCreds() }

// realClock is the wall clock: session reconnects follow real token expiry.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// TestEngineIngress: retrieved mail lands in the bound inbox under the
// sender's identity and threads by its headers; a reply goes out by
// submission under the uma Message-ID and replies to the thread.
func TestEngineIngress(t *testing.T) {
	r := newEngineRig(t, passwordHost(), nil)
	r.srv.deliver("INBOX", mail("<t1@example.org>", "First question"), time.Now())
	r.srv.deliver("INBOX", []byte(crlf("From: "+peer+"\nTo: "+mailUser+"\nMessage-ID: <t2@example.org>\n"+
		"In-Reply-To: <t1@example.org>\nReferences: <t1@example.org>\n\nFollow-up\n")), time.Now())
	r.receive()
	msgs := r.awaitMessages(2)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	m1, m2 := byProviderID(msgs, "<t1@example.org>"), byProviderID(msgs, "<t2@example.org>")
	if m1 == nil || m2 == nil {
		t.Fatalf("messages lack provider Message-ID references: %+v", msgs)
	}
	if m1.Ref.Conversation != m2.Ref.Conversation {
		t.Errorf("reply landed in %v, want the thread's %v", m2.Ref.Conversation, m1.Ref.Conversation)
	}
	if m1.Author.External.ID != peer || m1.Author.External.Namespace != imap.Namespace || m1.Author.Identity != m2.Author.Identity || m1.Source != model.TypeImapMail {
		t.Errorf("message %+v", m1)
	}

	acc, err := r.client.Messages().Reply(r.ctx, model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: "c-reply"}, Operation: "op-reply"},
		Conversation: m1.Ref.Conversation,
		Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Answer"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	op := r.work(acc.Operation.Ref)
	if op.State != model.ProviderAccepted || len(op.ProviderReferences) != 1 {
		t.Fatalf("operation %+v, want provider_accepted with the Message-ID", op)
	}
	txs := r.relay.transactions()
	if len(txs) != 1 || txs[0].rcpts[0] != peer || !strings.Contains(string(txs[0].data), "Message-Id: "+op.ProviderReferences[0].ID) {
		t.Fatalf("relay saw %+v", txs)
	}
}

// TestEngineLostReplyWithoutSentMailbox: without a sent-copies mailbox the
// channel cannot reconcile, so a reply whose answer was lost settles
// outcome_unknown at once, with no reconcile pass scheduled, and is never
// submitted again.
func TestEngineLostReplyWithoutSentMailbox(t *testing.T) {
	r := newEngineRig(t, passwordHost(), nil)
	r.srv.deliver("INBOX", mail("<u1@example.org>", "Question"), time.Now())
	r.receive()
	m := byProviderID(r.awaitMessages(1), "<u1@example.org>")
	if m == nil {
		t.Fatal("message not received")
	}
	r.relay.respond(reply{applied: true})
	acc, err := r.client.Messages().Reply(r.ctx, model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: "c-lost"}, Operation: "op-lost"},
		Conversation: m.Ref.Conversation,
		Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Answer"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	op := r.work(acc.Operation.Ref)
	if op.State != model.Unknown || op.NextAttemptAt != nil {
		t.Fatalf("operation %s, next attempt %v, want outcome_unknown with no reconcile pass", op.State, op.NextAttemptAt)
	}
	_, err = r.client.Operations().Reconcile(r.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "c-reconcile"}, Ref: acc.Operation.Ref})
	if model.CodeOf(err) != model.Unsupported {
		t.Fatalf("Reconcile: %v, want unsupported", err)
	}
	if txs := r.relay.transactions(); len(txs) != 1 {
		t.Fatalf("relay saw %d transactions, want the single lost one", len(txs))
	}
}

// TestEngineUIDValidityGap: across a UIDVALIDITY change the store gains the
// new mail once and the resynchronized copy of known mail is dropped.
func TestEngineUIDValidityGap(t *testing.T) {
	r := newEngineRig(t, passwordHost(), nil)
	t0 := time.Now().Add(-time.Minute)
	for i, id := range []string{"<g1@example.org>", "<g2@example.org>", "<g3@example.org>"} {
		r.srv.deliver("INBOX", mail(id, "before "+id), t0.Add(time.Duration(i)*time.Second))
	}
	r.receive()
	r.awaitMessages(3)
	r.srv.recreate("INBOX")
	r.srv.deliver("INBOX", mail("<g3@example.org>", "before <g3@example.org>"), t0.Add(2*time.Second))
	r.srv.deliver("INBOX", mail("<g4@example.org>", "after"), time.Now())
	msgs := r.awaitMessages(4)
	if len(msgs) != 4 || byProviderID(msgs, "<g4@example.org>") == nil {
		t.Fatalf("messages = %d, want 4 with g4: the gap delivers new mail, dedups the copy", len(msgs))
	}
}

// TestEngineNoDuplicateAcrossReconnects: sessions replaced before token
// expiry never store a message twice. Every token is already inside the
// reconnect margin, so each session runs one round and reconnects.
func TestEngineNoDuplicateAcrossReconnects(t *testing.T) {
	creds := &credHost{resolve: func(_ ports.CredentialRequest, n int) (ports.Credential, error) {
		return ports.OAuth2{User: mailUser, AccessToken: fmt.Sprintf("tok-%d", n), ExpiresAt: time.Now().Add(time.Minute)}, nil
	}}
	r := newEngineRig(t, creds, nil)
	r.srv.deliver("INBOX", mail("<r1@example.org>", "one"), time.Now())
	r.receive()
	r.awaitMessages(1)
	r.waiter.drive(t, func() bool { return len(r.srv.loginList()) >= 3 })
	r.srv.deliver("INBOX", mail("<r2@example.org>", "two"), time.Now())
	r.waiter.drive(t, func() bool { return len(r.srv.loginList()) >= 6 })
	if msgs := r.awaitMessages(2); len(msgs) != 2 {
		t.Fatalf("messages = %d across reconnects, want 2", len(msgs))
	}
}

// work runs a worker until operation ref leaves queued, then stops it.
func (r *engineRig) work(ref model.OperationRef) model.Operation {
	r.t.Helper()
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan error, 1)
	go func() {
		done <- r.engine.Runtime().Run(ctx, runtimeCall, uma.WorkerOptions{Owner: "imap-test", Limit: 10, Poll: 10 * time.Millisecond})
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
