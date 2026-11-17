package main

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/adapter/mail/smtp"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
)

// find returns the conversation whose reply route is in namespace.
func find(t *testing.T, convs []model.Conversation, namespace string) model.Conversation {
	t.Helper()
	for _, c := range convs {
		if c.ReplyRoute.Namespace == namespace {
			return c
		}
	}
	t.Fatalf("no conversation routed through %s in %+v", namespace, convs)
	return model.Conversation{}
}

// TestInbox is the host's smoke test: a visitor writes in the widget, the
// operator finds the conversation, reads it, replies and adds a note; the
// reply reaches the visitor's event stream and the note does not. An email
// to the MX receiver lands in the same inbox model and its reply leaves
// through the relay.
func TestInbox(t *testing.T) {
	db := newDatabase(t)
	rl := newRelay(t)
	h := start(t, testConfig(t, db.pool(), testSecrets(), rl.port))
	c := httpClient(t, h)

	if code := c.do(http.MethodGet, "/operator/conversations", "wrong-token", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("operator view without the token = %d, want 401", code)
	}

	// Widget message in.
	visitor := c.session()
	if visitor.Assurance != widget.AssuranceAnonymous {
		t.Fatalf("visitor assurance = %q", visitor.Assurance)
	}
	c.say(visitor, "c-1", "Is my order shipped?")
	c.say(visitor, "c-1", "Is my order shipped?") // a retried POST is one message
	chat := find(t, c.conversations(), widget.NamespaceVisitor)
	if chat.Ref.Inbox != h.widgetInbox {
		t.Fatalf("chat in inbox %v, want the widget inbox %v", chat.Ref.Inbox, h.widgetInbox)
	}
	msgs := c.messages(chat.Ref)
	if len(msgs) != 1 || textOf(msgs[0]) != "Is my order shipped?" || msgs[0].Author.Assurance != widget.AssuranceAnonymous {
		t.Fatalf("chat messages = %+v", msgs)
	}

	// Operator reply out to the widget; the note stays in the inbox.
	events := c.stream(visitor, "")
	reply := c.command(chat.Ref, "replies", "reply-1", commandBody{Text: "Yes, it left this morning."})
	if again := c.command(chat.Ref, "replies", "reply-1", commandBody{Text: "Yes, it left this morning."}); again.Operation.Ref != reply.Operation.Ref || !again.Replayed {
		t.Fatalf("resent reply = %+v, want the first acceptance replayed", again)
	}
	if !reply.Durable {
		t.Fatal("reply acceptance not durable")
	}
	if op := c.settle(reply.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("reply operation = %+v", op)
	}
	if ev := events.next(t); ev.Name != widget.EventMessage || eventText(ev.Data) != "Yes, it left this morning." {
		t.Fatalf("visitor received %+v, want the reply", ev)
	}
	c.command(chat.Ref, "notes", "note-1", commandBody{Text: "Customer asked twice; check carrier."})
	events.quiet(t, 300*time.Millisecond)
	var note *model.Message
	for _, m := range c.messages(chat.Ref) {
		if m.Audience.Visibility == model.Internal {
			note = &m
		}
	}
	if note == nil || textOf(*note) != "Customer asked twice; check carrier." {
		t.Fatalf("operator view lacks the internal note: %+v", c.messages(chat.Ref))
	}

	// Inbound email lands in the same model, in the mail inbox.
	sendMail(t, h, "jane@example.org", "<q1@example.org>", "", "Where is my invoice?")
	var mail model.Conversation
	eventually(t, "the email conversation", func() bool {
		for _, cv := range c.conversations() {
			if cv.ReplyRoute.Namespace == smtp.Namespace {
				mail = cv
				return true
			}
		}
		return false
	})
	if mail.Ref.Inbox != h.mailInbox {
		t.Fatalf("email in inbox %v, want the mail inbox %v", mail.Ref.Inbox, h.mailInbox)
	}
	mm := c.messages(mail.Ref)
	if len(mm) != 1 || textOf(mm[0]) != "Where is my invoice?" || mm[0].Author.External.ID != "jane@example.org" ||
		mm[0].Author.Assurance != smtp.AssuranceDMARCNone {
		t.Fatalf("email messages = %+v", mm)
	}
	mailReply := c.command(mail.Ref, "replies", "reply-mail-1", commandBody{Text: "Attached to your account page.", InReplyTo: mm[0].Ref.ID})
	if op := c.settle(mailReply.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("email reply operation = %+v", op)
	}
	got := rl.received()
	if len(got) != 1 || len(got[0].to) != 1 || got[0].to[0] != "jane@example.org" ||
		!strings.Contains(got[0].data, "Attached to your account page.") {
		t.Fatalf("relay received %+v, want one reply to jane", got)
	}
}

// flakyLog fails every append as transient, as a visitor log store that is
// down would, and reports the first attempt.
type flakyLog struct {
	widget.Log
	attempted chan struct{}
	once      atomic.Bool
}

func (l *flakyLog) Append(context.Context, widget.LogKey, widget.Entry) (uint64, error) {
	if l.once.CompareAndSwap(false, true) {
		close(l.attempted)
	}
	return 0, model.NewError(model.Transient, "visitor log down")
}

// TestRestartDeliversQueuedReplyOnce stops a host while an accepted reply
// is still queued for retry and starts a new one on the same database: the
// new host delivers it, once, and the visitor's stream resumes on it.
func TestRestartDeliversQueuedReplyOnce(t *testing.T) {
	db := newDatabase(t)
	rl := newRelay(t)
	pool := db.pool()
	down := &flakyLog{Log: storeLog(t, pool), attempted: make(chan struct{})}
	cfg := testConfig(t, pool, testSecrets(), rl.port)
	cfg.WidgetLog = down
	first := start(t, cfg)
	c := httpClient(t, first)

	visitor := c.session()
	c.say(visitor, "c-1", "Hello?")
	chat := find(t, c.conversations(), widget.NamespaceVisitor)
	reply := c.command(chat.Ref, "replies", "reply-1", commandBody{Text: "Hi, how can I help?"})
	select {
	case <-down.attempted:
	case <-time.After(10 * time.Second):
		t.Fatal("no delivery attempt")
	}
	if op := c.operation(reply.Operation.Ref); !pending(op) {
		t.Fatalf("reply after a transient failure = %+v, want it pending a retry", op)
	}
	first.stop(t)

	// A new process: new pool, new engine, the visitor log in the database.
	second := start(t, testConfig(t, db.pool(), testSecrets(), rl.port))
	c2 := httpClient(t, second)
	if second.widgetChannel != first.widgetChannel || second.widgetInbox != first.widgetInbox {
		t.Fatalf("restart configured new channels: %v/%v, want %v/%v",
			second.widgetChannel, second.widgetInbox, first.widgetChannel, first.widgetInbox)
	}
	events := c2.stream(visitor, "") // the session token outlives the process
	if op := c2.settle(reply.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("reply after restart = %+v", op)
	}
	ev := events.next(t)
	if ev.Name != widget.EventMessage || eventText(ev.Data) != "Hi, how can I help?" {
		t.Fatalf("visitor received %+v, want the reply", ev)
	}
	events.quiet(t, 300*time.Millisecond)

	entries, err := storeLog(t, pool).Read(t.Context(), widget.LogKey{
		Channel: second.widgetChannel, Namespace: widget.NamespaceVisitor, Visitor: visitor.Visitor,
	}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Operation != reply.Operation.Ref {
		t.Fatalf("visitor log = %+v, want the reply exactly once", entries)
	}
	// A reconnect after the delivered event replays nothing.
	c2.stream(visitor, ev.ID).quiet(t, 200*time.Millisecond)
}

// TestMailReceiverOwnership runs two hosts on one database: one owns the
// SMTP receiver, the other stands by and takes over when the owner stops.
func TestMailReceiverOwnership(t *testing.T) {
	db := newDatabase(t)
	rl := newRelay(t)
	owner := start(t, testConfig(t, db.pool(), testSecrets(), rl.port))
	sendMail(t, owner, "jane@example.org", "<o1@example.org>", "", "First")
	standby := start(t, testConfig(t, db.pool(), testSecrets(), rl.port))
	time.Sleep(200 * time.Millisecond) // several standby polls
	if addr := standby.MailAddr(); addr != "" {
		t.Fatalf("standby host listens on %s while the owner runs", addr)
	}

	owner.stop(t)
	sendMail(t, standby, "jane@example.org", "<o2@example.org>", "<o1@example.org>", "Second")
	c := httpClient(t, standby)
	var conv model.Conversation
	eventually(t, "both emails in one thread", func() bool {
		for _, cv := range c.conversations() {
			if cv.ReplyRoute.Namespace == smtp.Namespace && len(c.messages(cv.Ref)) == 2 {
				conv = cv
				return true
			}
		}
		return false
	})
	if conv.Ref.Inbox != standby.mailInbox {
		t.Fatalf("thread in %v, want the mail inbox", conv.Ref.Inbox)
	}
}

// TestRevokedCredentials withdraws the host's credentials: new widget
// sessions are refused and a queued reply fails without reaching the
// visitor, and the operator sees why.
func TestRevokedCredentials(t *testing.T) {
	db := newDatabase(t)
	rl := newRelay(t)
	secrets := testSecrets()
	h := start(t, testConfig(t, db.pool(), secrets, rl.port))
	c := httpClient(t, h)
	visitor := c.session()
	c.say(visitor, "c-1", "Hello")
	chat := find(t, c.conversations(), widget.NamespaceVisitor)
	events := c.stream(visitor, "")

	secrets.Revoke()
	if code := c.do(http.MethodPost, "/widget/session", "", struct{}{}, nil); code == http.StatusOK {
		t.Fatal("session issued with the key revoked")
	}
	reply := c.command(chat.Ref, "replies", "reply-1", commandBody{Text: "Hi"})
	op := c.settle(reply.Operation.Ref)
	if op.State == model.ProviderAccepted || pending(op) || op.Failure == nil {
		t.Fatalf("reply with revoked credentials = %+v, want a failure", op)
	}
	events.quiet(t, 300*time.Millisecond)
}
