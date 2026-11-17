package imap_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/model"
)

// importRig is an account whose receiver starts with an empty initial sync
// window, so mail dated in the past reaches the sink only through a history
// read, and a protocol store the reader and the receiver share.
type importRig struct {
	*stateRig
	store *protocolState
	cfg   imap.Config
}

func newImportRig(t *testing.T) *importRig {
	t.Helper()
	r := newStateRig(t, nil)
	cfg := r.cfg
	cfg.InitialSyncWindowMS = 1
	return &importRig{stateRig: r, store: &protocolState{}, cfg: cfg}
}

// seed puts an old message in INBOX.
func (r *importRig) seed(id string, when time.Time) {
	r.srv.deliver("INBOX", mail(id, "text of "+id), when)
}

// importAll reads the whole window through a reader on the shared store.
func (r *importRig) importAll(t *testing.T, since time.Time) adapter.HistoryPage {
	t.Helper()
	inst := open(t, newFactory(t), channel(channelA, r.cfg), adapter.Dependencies{Credentials: passwordCreds(), Protocol: r.store})
	return history(t, inst, adapter.HistoryRequest{Since: since})
}

func (r *importRig) receiver(t *testing.T) *follower {
	t.Helper()
	return r.follow(t, r.cfg, adapter.Dependencies{Protocol: r.store})
}

// A flag, move or deletion of an imported message is reported as it is for a
// received one, though the receiver never delivered it.
func TestImportedMailFollowsFlagsMovesAndDeletion(t *testing.T) {
	r := newImportRig(t)
	ids := []string{"<i1@example.org>", "<i2@example.org>", "<i3@example.org>"}
	for n, id := range ids {
		r.seed(id, at(n, 0))
	}
	if p := r.importAll(t, at(-1, 0)); len(p.Envelopes) != 3 {
		t.Fatalf("imported %d, want 3", len(p.Envelopes))
	}
	f := r.receiver(t)
	f.settle(t)
	if n := len(f.sink.envelopes()); n != 0 {
		t.Fatalf("receiver delivered %d envelopes, want none: the mail is outside its window", n)
	}
	uid := func(id string) uint32 { return r.at(t, "INBOX", id).uid }

	r.srv.setFlag(t, "INBOX", uid(ids[0]), goimap.FlagSeen, true)
	f.until(t, func() bool { return len(states(f.sink)) >= 1 })
	wantReport(t, states(f.sink)[0], ids[0], "INBOX", model.RoleInbox, true)

	r.srv.setFlag(t, "INBOX", uid(ids[0]), goimap.FlagFlagged, true)
	f.until(t, func() bool { return len(states(f.sink)) >= 2 })
	wantReport(t, states(f.sink)[1], ids[0], "INBOX", model.RoleInbox, true, model.FlagStar)

	r.srv.moveTo(t, "INBOX", uid(ids[1]), "Archive")
	f.until(t, func() bool { return len(states(f.sink)) >= 3 })
	wantReport(t, states(f.sink)[2], ids[1], "Archive", model.RoleArchive, false)

	r.srv.expunge("INBOX", uid(ids[2]))
	f.until(t, func() bool { return f.obs.count(imap.ObservationExpunged) >= 1 })
	f.settle(t)
	_ = f.run.stop()
	if n := len(states(f.sink)); n != 3 {
		t.Fatalf("%d reports, want 3: an expunge is an observation, not a report", n)
	}
	if n := f.obs.count(imap.ObservationExpunged); n != 1 {
		t.Fatalf("%d expunged observations, want 1: what the receiver adopted leaves the import ledger", n)
	}
}

// A change made between the import and the first receiver round is reported
// by that round.
func TestImportedMailChangedBeforeTheReceiverRuns(t *testing.T) {
	r := newImportRig(t)
	r.seed("<i1@example.org>", at(1, 0))
	r.importAll(t, at(-1, 0))
	r.srv.setFlag(t, "INBOX", r.at(t, "INBOX", "<i1@example.org>").uid, goimap.FlagSeen, true)
	f := r.receiver(t)
	f.until(t, func() bool { return len(states(f.sink)) >= 1 })
	wantReport(t, states(f.sink)[0], "<i1@example.org>", "INBOX", model.RoleInbox, true)
}

// A message both imported and delivered is one followed message: one change
// is one report, and an import repeated changes nothing.
func TestImportedAndReceivedMessageIsFollowedOnce(t *testing.T) {
	r := newImportRig(t)
	r.cfg.InitialSyncWindowMS = 0 // the default window: the receiver delivers it
	now := time.Now()
	r.srv.deliver("INBOX", mail("<b1@example.org>", "both"), now)
	r.importAll(t, now.Add(-time.Hour))
	r.importAll(t, now.Add(-time.Hour))
	f := r.receiver(t)
	f.sink.await(t, 1)
	f.settle(t)

	r.srv.setFlag(t, "INBOX", r.at(t, "INBOX", "<b1@example.org>").uid, goimap.FlagSeen, true)
	f.until(t, func() bool { return len(states(f.sink)) >= 1 })
	f.settle(t)
	_ = f.run.stop()
	if n := len(states(f.sink)); n != 1 {
		t.Fatalf("%d reports for one change, want 1", n)
	}
	wantReport(t, states(f.sink)[0], "<b1@example.org>", "INBOX", model.RoleInbox, true)
}

// After a UIDVALIDITY change the UIDs imported under the old value are void:
// nothing is reported or counted as expunged for them, and a new import
// under the new value is followed.
func TestImportedMailUIDValidityChange(t *testing.T) {
	r := newImportRig(t)
	r.seed("<u1@example.org>", at(1, 0))
	r.importAll(t, at(-1, 0))
	f := r.receiver(t)
	f.settle(t)

	r.srv.recreate("INBOX")
	f.settle(t)
	if n := len(states(f.sink)) + f.obs.count(imap.ObservationExpunged); n != 0 {
		t.Fatalf("%d reports or expunges after UIDVALIDITY changed, want none", n)
	}

	r.seed("<u2@example.org>", at(2, 0))
	r.importAll(t, at(-1, 0))
	f.settle(t)
	r.srv.setFlag(t, "INBOX", r.at(t, "INBOX", "<u2@example.org>").uid, goimap.FlagSeen, true)
	f.until(t, func() bool { return len(states(f.sink)) >= 1 })
	wantReport(t, states(f.sink)[0], "<u2@example.org>", "INBOX", model.RoleInbox, true)
}

// Imported before the UIDVALIDITY change, adopted after it: stale, voided by
// the sync, never reported.
func TestImportedStaleUIDsAreVoidedWhenAdopted(t *testing.T) {
	r := newImportRig(t)
	r.seed("<s1@example.org>", at(1, 0))
	r.importAll(t, at(-1, 0))
	r.srv.recreate("INBOX")
	r.seed("<s2@example.org>", at(2, 0)) // takes the UID the stale entry holds
	f := r.receiver(t)
	f.settle(t)
	r.srv.setFlag(t, "INBOX", r.at(t, "INBOX", "<s2@example.org>").uid, goimap.FlagSeen, true)
	f.settle(t)
	_ = f.run.stop()
	if n := len(states(f.sink)); n != 0 {
		t.Fatalf("%d reports, want none: s2 was never imported", n)
	}
}

// Without an engine store the reader and the receiver of one factory still
// share what was imported.
func TestImportedMailSharedInTheFactory(t *testing.T) {
	r := newImportRig(t)
	r.seed("<m1@example.org>", at(1, 0))
	w := &stepWaiter{}
	fac := steppedFactory(t, w)
	hist := open(t, fac, channel(channelA, r.cfg), adapter.Dependencies{Credentials: passwordCreds()})
	history(t, hist, adapter.HistoryRequest{Since: at(-1, 0)})

	sk := newSink()
	rcv := open(t, fac, channel(channelA, r.cfg), adapter.Dependencies{Credentials: passwordCreds()})
	rn := run(t, rcv, sk)
	r.srv.setFlag(t, "INBOX", r.at(t, "INBOX", "<m1@example.org>").uid, goimap.FlagSeen, true)
	w.drive(t, func() bool { return len(states(sk)) >= 1 })
	_ = rn.stop()
	wantReport(t, states(sk)[0], "<m1@example.org>", "INBOX", model.RoleInbox, true)
}

// gated holds the compare-and-swap of one key until released, so a test can
// act between a reader's read of the mailbox and the store of what it read.
type gated struct {
	*protocolState
	key     string
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gated) CompareAndSwap(ctx context.Context, key string, rev model.Revision, x model.Extension) error {
	if key == g.key {
		g.once.Do(func() { close(g.reached); <-g.release })
	}
	return g.protocolState.CompareAndSwap(ctx, key, rev, x)
}

// The flags a reader saw may be older than the comparison the receiver last
// made (its mod-sequence, or the time of its last full comparison on a server
// without CONDSTORE, which the in-process server is) by the time it adopts the
// message: a change in between is still reported, not left to the next
// interval.
func TestImportedMailChangedBeforeItIsAdoptedIsReported(t *testing.T) {
	r := newImportRig(t)
	r.cfg.InitialSyncWindowMS = 0
	r.cfg.StateSyncIntervalMS = int64(time.Hour / time.Millisecond) // no comparison is due by itself
	r.srv.deliver("INBOX", mail("<now@example.org>", "received"), time.Now())
	r.seed("<old@example.org>", at(1, 0))
	g := &gated{protocolState: r.store, key: "mailstate.import", reached: make(chan struct{}), release: make(chan struct{})}
	inst := open(t, newFactory(t), channel(channelA, r.cfg), adapter.Dependencies{Credentials: passwordCreds(), Protocol: g})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = inst.History(t.Context(), adapter.HistoryRequest{Since: at(-1, 0), Limit: 10})
	}()
	<-g.reached // the reader holds the flags it read, not yet stored
	r.srv.setFlag(t, "INBOX", r.at(t, "INBOX", "<old@example.org>").uid, goimap.FlagSeen, true)
	f := r.follow(t, r.cfg, adapter.Dependencies{Protocol: r.store})
	f.sink.await(t, 1)
	f.settle(t) // the mod-sequence moves past the change
	close(g.release)
	<-done
	f.until(t, func() bool { return len(states(f.sink)) >= 1 })
	wantReport(t, states(f.sink)[0], "<old@example.org>", "INBOX", model.RoleInbox, true)
}

// Imported UIDs of another UIDVALIDITY than the one the receiver follows are
// void: they neither replace what the receiver follows nor are reported.
func TestImportedMailOfAnotherUIDValidityDoesNotDisplaceFollowed(t *testing.T) {
	r := newImportRig(t)
	r.cfg.InitialSyncWindowMS = 0
	r.deliver("<r1@example.org>")
	f := r.receiver(t)
	f.sink.await(t, 1)
	f.settle(t)

	stale, _ := json.Marshal(map[string]any{"mailboxes": map[string]any{"INBOX": map[string]any{
		"uid_validity": r.srv.uidValidity("INBOX") + 1000,
		"messages":     map[string]any{"1": map[string]any{"id": "<stale@example.org>"}},
	}}})
	if err := r.store.CompareAndSwap(t.Context(), "mailstate.import", "", model.Extension{Name: string(imap.Type) + ".mailbox_import", Version: 1, Data: stale}); err != nil {
		t.Fatal(err)
	}
	f.settle(t)
	r.srv.setFlag(t, "INBOX", r.at(t, "INBOX", "<r1@example.org>").uid, goimap.FlagSeen, true)
	f.until(t, func() bool { return len(states(f.sink)) >= 1 })
	wantReport(t, states(f.sink)[0], "<r1@example.org>", "INBOX", model.RoleInbox, true)
	f.settle(t)
	_ = f.run.stop()
	if n := len(states(f.sink)) + f.obs.count(imap.ObservationExpunged); n != 1 {
		t.Fatalf("%d reports or expunges, want the one report: the stale entry is not followed", n)
	}
}

// A multi-mailbox walk records each message under the mailbox it came from, so
// a flag set on a message of the second mailbox is reported for that mailbox.
func TestImportedMailOfSecondMailboxIsFollowedThere(t *testing.T) {
	r := newImportRig(t)
	r.seed("<in@example.org>", at(1, 0))
	r.srv.deliver("Archive", mail("<ar@example.org>", "text of ar"), at(2, 0))
	// The receiver reports state only for the primary and watched mailboxes.
	r.cfg.WatchMailboxes = []string{"Archive"}
	cfg := r.cfg
	cfg.HistoryMailboxes = []string{"INBOX", "Archive"}
	inst := open(t, newFactory(t), channel(channelA, cfg), adapter.Dependencies{Credentials: passwordCreds(), Protocol: r.store})
	var got int
	req := adapter.HistoryRequest{Since: at(-1, 0), Limit: 1}
	for range 10 {
		p := history(t, inst, req)
		got += len(p.Envelopes)
		if p.Next == "" {
			break
		}
		req.Cursor = p.Next
	}
	if got != 2 {
		t.Fatalf("imported %d, want 2", got)
	}
	f := r.receiver(t)
	f.settle(t)
	r.srv.setFlag(t, "Archive", r.at(t, "Archive", "<ar@example.org>").uid, goimap.FlagSeen, true)
	f.until(t, func() bool { return len(states(f.sink)) >= 1 })
	wantReport(t, states(f.sink)[0], "<ar@example.org>", "Archive", model.RoleArchive, true)
	_ = f.run.stop()
	if n := len(states(f.sink)); n != 1 {
		t.Fatalf("%d reports, want 1", n)
	}
}
