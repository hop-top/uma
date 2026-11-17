package imap_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// The Message-ID is the sender's to choose and anyone who can mail the
// mailbox can repeat it, so it never decides that a mail was already seen:
// the fetched bytes do. These tests pin that through the real engine.

const forger = "mallory@example.net"

// mailFrom is mail(id, text) from another sender.
func mailFrom(from, id, text string) []byte {
	return []byte(strings.Replace(string(mail(id, text)), peer, from, 1))
}

// texts returns the body text of every message but the sentinel.
func texts(msgs []model.Message) []string {
	var out []string
	for _, m := range msgs {
		if len(m.Content.Parts) > 0 && m.Content.Parts[0].Kind == model.PartText && m.Content.Parts[0].Text != "end" {
			out = append(out, m.Content.Parts[0].Text)
		}
	}
	slices.Sort(out)
	return out
}

// awaitEnd drives the receiver until the sentinel mail with Message-ID id is
// stored, then until it waits again. Mail is delivered in UID order, so all
// mail delivered before the sentinel has been through ingress by then.
func (r *engineRig) awaitEnd(id string) []model.Message {
	r.t.Helper()
	r.waiter.drive(r.t, func() bool { return byProviderID(r.messages(), id) != nil })
	r.waiter.idle(r.t)
	return r.messages()
}

// A forged mail that reuses the Message-ID of a genuine one and arrives
// first must not make the genuine mail a duplicate, and the same two mails
// fetched again after a UIDVALIDITY change are still two, not four.
func TestEngineSameMessageIDDifferentMailIsNotADuplicate(t *testing.T) {
	r := newEngineRig(t, passwordHost(), nil)
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	const id = "<claimed@example.org>"
	for _, raw := range [][]byte{
		mailFrom(forger, id, "forged"),
		mailFrom(peer, id, "the real question"),
		mailFrom(peer, id, "another mail under the same id"),
		mail("<end1@example.org>", "end"),
	} {
		r.srv.deliver("INBOX", raw, at)
	}
	r.receive()
	msgs := r.awaitEnd("<end1@example.org>")
	want := []string{"another mail under the same id", "forged", "the real question"}
	if got := texts(msgs); !slices.Equal(got, want) {
		t.Fatalf("stored %q, want %q: mails sharing a Message-ID but not bytes are distinct", got, want)
	}

	// The mailbox is recreated and the same mails come back under new UIDs:
	// genuine redeliveries, each already held.
	r.srv.recreate("INBOX")
	for _, raw := range [][]byte{
		mail("<end1@example.org>", "end"),
		mailFrom(peer, id, "another mail under the same id"),
		mailFrom(peer, id, "the real question"),
		mailFrom(forger, id, "forged"),
	} {
		r.srv.deliver("INBOX", raw, at)
	}
	r.srv.deliver("INBOX", mail("<end2@example.org>", "end"), time.Now())
	msgs = r.awaitEnd("<end2@example.org>")
	if got := texts(msgs); !slices.Equal(got, want) {
		t.Fatalf("stored %q after the redelivery, want %q: identical bytes are one message", got, want)
	}
}

// The same bytes twice in one mailbox (a copy, or two deliveries of one
// message) are one message; bytes that differ are two.
func TestEngineIdenticalBytesAreOneMessage(t *testing.T) {
	r := newEngineRig(t, passwordHost(), nil)
	at := time.Now().Add(-time.Minute)
	for _, raw := range [][]byte{
		mail("<twice@example.org>", "same"), mail("<twice@example.org>", "same"), mail("<twice@example.org>", "not the same"),
		mail("<end1@example.org>", "end"),
	} {
		r.srv.deliver("INBOX", raw, at)
	}
	r.receive()
	if got, want := texts(r.awaitEnd("<end1@example.org>")), []string{"not the same", "same"}; !slices.Equal(got, want) {
		t.Fatalf("stored %q, want %q", got, want)
	}
}

// One mail in two mailboxes of the account (two channels, two destinations)
// lands once in each.
func TestEngineIdenticalMailInTwoMailboxesLandsInEach(t *testing.T) {
	r := newEngineRig(t, passwordHost(), nil)
	if err := r.srv.user.Create("Other", nil); err != nil {
		t.Fatal(err)
	}
	channelB := model.ChannelRef{Tenant: tenant, ID: "00000e00-0000-7000-8000-00000000000b"}
	inboxB := model.InboxRef{Tenant: tenant, ID: "00000e00-0000-7000-8000-0000000000b1"}
	cfg := config(r.srv, r.relay)
	cfg.Mailbox, cfg.Destination = "Other", "support-b"
	lock := false
	sel := model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1}
	if err := r.store.Transact(r.ctx, tenant, func(tx store.Tx) error {
		if err := tx.PutChannel(r.ctx, channel(channelB, cfg), ""); err != nil {
			return err
		}
		return tx.PutInbox(r.ctx, model.Inbox{Ref: inboxB, Channel: channelB, Name: "other", Destinations: []string{"support-b"}, Selection: sel, Revision: "in1"}, "")
	}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Minute)
	for _, mbox := range []string{"INBOX", "Other"} {
		r.srv.deliver(mbox, mail("<both@example.org>", "to both"), at)
		r.srv.deliver(mbox, mail("<end@example.org>", "end"), at)
	}
	r.receive()
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.engine.Runtime().Receive(ctx, runtimeCall, channelB)
	}()
	t.Cleanup(func() { cancel(); <-done })
	in := func(ref model.InboxRef) []model.Message {
		var out []model.Message
		if err := r.store.View(r.ctx, tenant, func(rd store.Reader) error {
			p, err := rd.Messages(r.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{ref}}, Page: model.PageRequest{Limit: 100}})
			out = p.Items
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	r.waiter.drive(t, func() bool { return len(in(inboxA)) >= 2 && len(in(inboxB)) >= 2 })
	r.waiter.idle(t)
	for name, ref := range map[string]model.InboxRef{"INBOX": inboxA, "Other": inboxB} {
		if got := texts(in(ref)); !slices.Equal(got, []string{"to both"}) {
			t.Errorf("%s holds %q, want the mail once", name, got)
		}
	}
}

// A message over max_message_bytes is delivered from its header alone. Two
// such messages that share a header block but differ in size are two; the
// same one fetched again is one.
func TestEngineHeaderOnlyMessagesAreTold(t *testing.T) {
	r := newEngineRig(t, passwordHost(), func(c *imap.Config) { c.MaxMessageBytes = 400 })
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	const id = "<big@example.org>"
	big := func(n int) []byte { return mailFrom(peer, id, strings.Repeat("x", n)) }
	for _, raw := range [][]byte{big(500), big(900), mail("<end1@example.org>", "end")} {
		r.srv.deliver("INBOX", raw, at)
	}
	r.receive()
	msgs := r.awaitEnd("<end1@example.org>")
	if n := len(msgs) - 1; n != 2 {
		t.Fatalf("stored %d oversize messages, want 2: same header block, different size", n)
	}
	r.srv.recreate("INBOX")
	for _, raw := range [][]byte{mail("<end1@example.org>", "end"), big(900), big(500)} {
		r.srv.deliver("INBOX", raw, at)
	}
	r.srv.deliver("INBOX", mail("<end2@example.org>", "end"), time.Now())
	msgs = r.awaitEnd("<end2@example.org>")
	if n := len(msgs) - 2; n != 2 {
		t.Fatalf("stored %d oversize messages after the redelivery, want 2", n)
	}
}

// The event ID is the destination and a digest of the bytes fetched: never
// a UID, a UIDVALIDITY or the Message-ID. A message delivered from its
// header alone says so and carries the size the server reports, so it never
// equals the ID of a message whose whole text is that header block.
func TestReceiveEventIDIsDestinationAndBytes(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	cfg := config(srv, nil)
	cfg.MaxMessageBytes = 400
	at := time.Now().Add(-time.Minute)
	small := mail("<s@example.org>", "small")
	huge := mail("<h@example.org>", strings.Repeat("y", 600))
	srv.deliver("INBOX", small, at)
	srv.deliver("INBOX", small, at) // second UID, same bytes
	srv.deliver("INBOX", mail("<s@example.org>", "other"), at)
	srv.deliver("INBOX", huge, at)
	w := &stepWaiter{}
	inst := open(t, steppedFactory(t, w), channel(channelA, cfg), adapter.Dependencies{Credentials: passwordCreds()})
	s := newSink()
	run(t, inst, s)
	w.idle(t)
	got := s.envelopes()
	if len(got) != 4 {
		t.Fatalf("delivered %d, want 4", len(got))
	}
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	wantSmall := "mailbox:" + mailUser + ";sha256:" + sum(small)
	if got[0].EventID != wantSmall || got[1].EventID != wantSmall {
		t.Errorf("same bytes at two UIDs: event %q, %q, want both %q", got[0].EventID, got[1].EventID, wantSmall)
	}
	if got[2].EventID == wantSmall || got[2].MessageID != got[0].MessageID {
		t.Errorf("same Message-ID, other bytes: event %q, want one different from %q", got[2].EventID, wantSmall)
	}
	head := huge[:strings.Index(string(huge), "\r\n\r\n")+4]
	if want := "mailbox:" + mailUser + ";header-sha256:" + sum(head) + ";size:" + strconv.Itoa(len(huge)); got[3].EventID != want {
		t.Errorf("header-only event %q, want %q", got[3].EventID, want)
	}

	// A new UIDVALIDITY re-fetches the same bytes under new UIDs: same IDs.
	srv.recreate("INBOX")
	srv.deliver("INBOX", small, at)
	srv.deliver("INBOX", huge, at)
	w.drive(t, func() bool { return len(s.envelopes()) >= 6 })
	again := s.envelopes()
	if again[4].EventID != got[0].EventID || again[5].EventID != got[3].EventID {
		t.Errorf("after UIDVALIDITY change events %q, %q, want %q, %q", again[4].EventID, again[5].EventID, got[0].EventID, got[3].EventID)
	}
}
