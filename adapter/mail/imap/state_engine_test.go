package imap_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"hop.top/uma"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

func (r *engineRig) conversation(ref model.ConversationRef) model.Conversation {
	r.t.Helper()
	var c model.Conversation
	if err := r.store.View(r.ctx, tenant, func(rd store.Reader) error {
		var err error
		c, err = rd.GetConversation(r.ctx, ref)
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return c
}

func (r *engineRig) providerState(ref model.ConversationRef) (model.ProviderConversationState, bool) {
	for _, s := range r.conversation(ref).ProviderState {
		if s.Channel == channelA {
			return s, true
		}
	}
	return model.ProviderConversationState{}, false
}

func (r *engineRig) at(t *testing.T, mailbox, id string) held {
	t.Helper()
	h, ok := r.srv.find(t, mailbox, id)
	if !ok {
		t.Fatalf("%s does not hold %s", mailbox, id)
	}
	return h
}

func (r *engineRig) wantAbsent(t *testing.T, mailbox, id string) {
	t.Helper()
	if _, ok := r.srv.find(t, mailbox, id); ok {
		t.Fatalf("%s holds %s", mailbox, id)
	}
}

func stateCommand(id string) model.Command {
	return model.Command{Call: model.Call{Correlation: model.CorrelationID("c-" + id)}, Operation: model.OperationID(id)}
}

func (r *engineRig) wantApplied(op model.Operation) {
	r.t.Helper()
	if op.State != model.ProviderAccepted {
		r.t.Fatalf("operation %+v, want provider_accepted", op)
	}
}

// TestEngineProviderState: through the real engine, what another client does
// to the mailbox reaches the conversation as provider state, and the verbs
// change the mailbox and keep the engine's references to the messages in step
// with where they are.
func TestEngineProviderState(t *testing.T) {
	r := newEngineRig(t, passwordHost(), func(c *imap.Config) { c.StateSyncIntervalMS = 1 })
	const id = "<s1@example.org>"
	r.srv.deliver("INBOX", mail(id, "A question"), time.Now())
	r.receive()
	m := byProviderID(r.awaitMessages(1), id)
	if m == nil {
		t.Fatal("message not received")
	}
	conv := m.Ref.Conversation
	cl := r.client.Conversations()
	uid := r.at(t, "INBOX", id).uid

	// What another client does arrives as provider state, never as a read
	// marker or a status.
	r.srv.setFlag(t, "INBOX", uid, goimap.FlagSeen, true)
	r.waiter.drive(t, func() bool { st, ok := r.providerState(conv); return ok && st.Read != nil && *st.Read })
	st, _ := r.providerState(conv)
	if st.Folder == nil || st.Folder.ID != "INBOX" || st.Folder.Role != model.RoleInbox || st.Complete || st.Archived || st.Trashed {
		t.Fatalf("provider state %+v, want the message in the inbox, partial", st)
	}

	// uma stars it: the mailbox has the flag, and the conversation shows it.
	acc, err := cl.Flag(r.ctx, model.FlagConversation{Command: stateCommand("op-star"), Conversation: conv, Kind: model.FlagStar})
	if err != nil {
		t.Fatal(err)
	}
	r.wantApplied(r.work(acc.Operation.Ref))
	if !r.at(t, "INBOX", id).has(goimap.FlagFlagged) {
		t.Fatal("the mailbox does not hold the star")
	}
	r.waiter.drive(t, func() bool { st, _ := r.providerState(conv); return slices.Contains(st.Flags, model.FlagStar) })

	// uma archives it: the copy in Archive is the message now, named by its
	// location ahead of the Message-ID, and the next verb finds it there.
	acc, err = cl.Archive(r.ctx, model.ArchiveConversation{Command: stateCommand("op-archive"), Conversation: conv})
	if err != nil {
		t.Fatal(err)
	}
	r.wantApplied(r.work(acc.Operation.Ref))
	r.at(t, "Archive", id)
	r.wantAbsent(t, "INBOX", id)
	stored := r.messages()
	var refs []model.ExternalRef
	for _, x := range stored[0].External {
		if x.Namespace == model.ProviderMessageNamespace && x.Kind == model.RefKindMessage {
			refs = append(refs, x)
		}
	}
	if len(refs) != 2 || refs[1] != model.ProviderMessageRef(channelA, id) || refs[0].ID == id {
		t.Fatalf("message references %v, want the location of the copy ahead of the Message-ID", refs)
	}
	acc, err = cl.SetRead(r.ctx, model.SetConversationRead{Command: stateCommand("op-unread"), Conversation: conv, Read: false})
	if err != nil {
		t.Fatal(err)
	}
	r.wantApplied(r.work(acc.Operation.Ref))
	if h := r.at(t, "Archive", id); h.has(goimap.FlagSeen) || !h.has(goimap.FlagFlagged) {
		t.Fatalf("the unread verb did not reach the archived copy: flags %v", h.flags)
	}
	if st, _ := r.providerState(conv); st.Read == nil || *st.Read {
		t.Fatalf("provider state %+v, want unread after the verb", st)
	}
}

// TestEngineLostStateAnswerSettlesFromTheReport: without a sent-copies
// mailbox the channel cannot reconcile, so a flag whose answer was lost stays
// outcome_unknown until the receiver sees the flag in the mailbox: that report
// is the echo, it settles the operation, and the change is never sent again.
func TestEngineLostStateAnswerSettlesFromTheReport(t *testing.T) {
	r := newEngineRig(t, passwordHost(), func(c *imap.Config) { c.StateSyncIntervalMS = 1 })
	const id = "<s2@example.org>"
	r.srv.deliver("INBOX", mail(id, "A question"), time.Now())
	stop := r.receiveStoppable()
	m := byProviderID(r.awaitMessages(1), id)
	if m == nil {
		t.Fatal("message not received")
	}
	// No receiver while the answer is lost: a running receiver would see the
	// flag and settle the operation from its echo before outcome_unknown is
	// observed.
	stop()
	r.srv.respond(stateReply{lostReply: true})
	acc, err := r.client.Conversations().Flag(r.ctx, model.FlagConversation{Command: stateCommand("op-lost-star"), Conversation: m.Ref.Conversation, Kind: model.FlagStar})
	if err != nil {
		t.Fatal(err)
	}
	if op := r.work(acc.Operation.Ref); op.State != model.Unknown {
		t.Fatalf("operation %+v, want outcome_unknown", op)
	}
	if !r.at(t, "INBOX", id).has(goimap.FlagFlagged) {
		t.Fatal("the lost answer was not an applied change")
	}
	r.receive()
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.engine.Runtime().Run(ctx, runtimeCall, uma.WorkerOptions{Owner: "imap-test", Limit: 10, Poll: 10 * time.Millisecond})
	}()
	t.Cleanup(func() { cancel(); <-done })
	r.waiter.drive(t, func() bool {
		op, err := r.client.Operations().Get(r.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "c-get"}, Ref: acc.Operation.Ref})
		return err == nil && op.State == model.ProviderAccepted
	})
	stores := 0
	for _, l := range r.srv.clientLines() {
		if f := strings.Fields(l); len(f) >= 3 && f[1] == "UID" && f[2] == "STORE" {
			stores++
		}
	}
	if n := stores; n != 1 {
		t.Fatalf("%d STORE commands, want the one whose answer was lost: nothing is sent again", n)
	}
}
