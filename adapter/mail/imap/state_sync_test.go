package imap_test

import (
	"slices"
	"sync/atomic"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/model"
)

// states returns the conversation.state envelopes the sink holds.
func states(s *sink) []model.Envelope {
	var out []model.Envelope
	for _, e := range s.envelopes() {
		if e.Kind == model.EnvelopeConversationState {
			out = append(out, e)
		}
	}
	return out
}

// follower is a receiver on the rig's account, driven step by step.
type follower struct {
	w    *stepWaiter
	sink *sink
	obs  *observer
	run  *running
}

func (r *stateRig) follow(t *testing.T, cfg imap.Config, deps adapter.Dependencies) *follower {
	t.Helper()
	f := &follower{w: &stepWaiter{}, sink: newSink(), obs: &observer{}}
	f.sink.dedupe = true
	deps.Credentials, deps.Observer = passwordCreds(), f.obs
	inst := open(t, steppedFactory(t, f.w), channel(channelA, cfg), deps)
	f.run = run(t, inst, f.sink)
	return f
}

// until drives the receiver, a round at a time, until cond holds.
func (f *follower) until(t *testing.T, cond func() bool) {
	t.Helper()
	f.w.drive(t, cond)
}

// settle runs the receiver through a few more rounds and returns.
func (f *follower) settle(t *testing.T) {
	t.Helper()
	for range 4 {
		f.w.step(t)
	}
}

func wantReport(t *testing.T, e model.Envelope, id string, mailbox, role string, read bool, flags ...string) model.ProviderConversationState {
	t.Helper()
	rep := e.State
	if rep == nil || e.Kind != model.EnvelopeConversationState || e.MessageID != "" || e.Destination != mailUser {
		t.Fatalf("envelope %+v, want a conversation.state for the destination", e)
	}
	if err := rep.Validate(); err != nil {
		t.Fatalf("report invalid: %v", err)
	}
	if rep.Conversation != ref(id) {
		t.Fatalf("conversation %+v, want the Message-ID reference %v", rep.Conversation, id)
	}
	st := rep.State
	if st.Folder == nil || st.Folder.ID != mailbox || st.Folder.Role != role || st.Read == nil || *st.Read != read || st.Complete {
		t.Fatalf("state %+v folder %+v, want mailbox %s role %q read %v, never complete", st, st.Folder, mailbox, role, read)
	}
	if !slices.Equal(st.Flags, flags) {
		t.Fatalf("flags %v, want %v", st.Flags, flags)
	}
	if st.Archived != (role == model.RoleArchive) || st.Trashed != (role == model.RoleTrash) {
		t.Fatalf("state %+v, want archived and trashed to follow the role %q", st, role)
	}
	return st
}

// TestFollowFlagChanges: a flag another client sets is reported once;
// nothing is reported while nothing changes.
func TestFollowFlagChanges(t *testing.T) {
	r := newStateRig(t, nil)
	r.deliver("<m1@example.org>")
	f := r.follow(t, r.cfg, adapter.Dependencies{})
	f.sink.await(t, 1)
	uid := r.at(t, "INBOX", "<m1@example.org>").uid

	r.srv.setFlag(t, "INBOX", uid, goimap.FlagSeen, true)
	f.until(t, func() bool { return len(states(f.sink)) >= 1 })
	wantReport(t, states(f.sink)[0], "<m1@example.org>", "INBOX", model.RoleInbox, true)

	r.srv.setFlag(t, "INBOX", uid, goimap.FlagFlagged, true)
	f.until(t, func() bool { return len(states(f.sink)) >= 2 })
	wantReport(t, states(f.sink)[1], "<m1@example.org>", "INBOX", model.RoleInbox, true, model.FlagStar)

	r.srv.setFlag(t, "INBOX", uid, goimap.FlagSeen, false)
	f.until(t, func() bool { return len(states(f.sink)) >= 3 })
	wantReport(t, states(f.sink)[2], "<m1@example.org>", "INBOX", model.RoleInbox, false, model.FlagStar)

	f.settle(t)
	if n := len(states(f.sink)); n != 3 {
		t.Fatalf("%d reports, want 3: an unchanged mailbox reports nothing", n)
	}
	if n := f.obs.count(imap.ObservationExpunged); n != 0 {
		t.Fatalf("%d expunged observations, want none", n)
	}
}

// TestFlagComparisonIsPeriodicWithoutCondstore: a server without CONDSTORE
// is compared in full at most every state_sync_interval_ms; a message that
// left is noticed every round.
func TestFlagComparisonIsPeriodicWithoutCondstore(t *testing.T) {
	r := newStateRig(t, nil)
	r.deliver("<m1@example.org>")
	r.deliver("<m2@example.org>")
	cfg := r.cfg
	cfg.StateSyncIntervalMS = int64(time.Hour / time.Millisecond)
	f := r.follow(t, cfg, adapter.Dependencies{})
	f.sink.await(t, 2)
	f.settle(t) // the first comparison, which sets the baseline
	r.srv.setFlag(t, "INBOX", r.at(t, "INBOX", "<m1@example.org>").uid, goimap.FlagSeen, true)
	f.settle(t)
	if n := len(states(f.sink)); n != 0 {
		t.Fatalf("%d reports inside the interval, want none", n)
	}
	r.srv.moveTo(t, "INBOX", r.at(t, "INBOX", "<m2@example.org>").uid, "Archive")
	f.until(t, func() bool { return len(states(f.sink)) >= 1 })
	wantReport(t, states(f.sink)[0], "<m2@example.org>", "Archive", model.RoleArchive, false)
}

// TestFollowMovedAndExpunged: a message that left INBOX is looked up in the
// other mailboxes: found, it was moved and its place is reported; found
// nowhere, it was expunged and only an observation says so.
func TestFollowMovedAndExpunged(t *testing.T) {
	r := newStateRig(t, nil)
	for _, id := range []string{"<m1@example.org>", "<m2@example.org>", "<m3@example.org>", "<m4@example.org>"} {
		r.deliver(id)
	}
	f := r.follow(t, r.cfg, adapter.Dependencies{})
	f.sink.await(t, 4)
	uid := func(id string) uint32 { return r.at(t, "INBOX", id).uid }

	r.srv.setFlag(t, "INBOX", uid("<m1@example.org>"), goimap.FlagFlagged, true)
	r.srv.moveTo(t, "INBOX", uid("<m1@example.org>"), "Archive")
	r.srv.moveTo(t, "INBOX", uid("<m3@example.org>"), "Trash")
	r.srv.moveTo(t, "INBOX", uid("<m4@example.org>"), "Projects") // a mailbox nobody looks in
	r.srv.expunge("INBOX", uid("<m2@example.org>"))
	f.until(t, func() bool { return len(states(f.sink)) >= 2 && f.obs.count(imap.ObservationExpunged) >= 2 })
	f.settle(t)

	got := map[string]model.ProviderConversationState{}
	for _, e := range states(f.sink) {
		got[e.State.Conversation.ID] = e.State.State
	}
	if len(got) != 2 {
		t.Fatalf("reports for %v, want the archived and the trashed message only", got)
	}
	st := got["<m1@example.org>"]
	if st.Folder == nil || st.Folder.ID != "Archive" || !st.Archived || st.Trashed || !slices.Equal(st.Flags, []string{model.FlagStar}) {
		t.Errorf("m1 state %+v, want archived with its star", st)
	}
	st = got["<m3@example.org>"]
	if st.Folder == nil || st.Folder.ID != "Trash" || st.Folder.Role != model.RoleTrash || !st.Trashed || st.Archived {
		t.Errorf("m3 state %+v, want trashed", st)
	}
	if n := f.obs.count(imap.ObservationExpunged); n != 2 {
		t.Errorf("%d expunged observations, want 2: the expunged message and the one moved where nobody looks", n)
	}
}

// TestFollowWatchedMailboxes: a watched mailbox is followed once a message
// is in it: its flags, and where it goes next.
func TestFollowWatchedMailboxes(t *testing.T) {
	r := newStateRig(t, func(_ *serverOptions, c *imap.Config) { c.WatchMailboxes = []string{"Projects"} })
	r.deliver("<m1@example.org>")
	f := r.follow(t, r.cfg, adapter.Dependencies{})
	f.sink.await(t, 1)

	p := r.srv.moveTo(t, "INBOX", r.at(t, "INBOX", "<m1@example.org>").uid, "Projects")
	f.until(t, func() bool { return len(states(f.sink)) >= 1 })
	wantReport(t, states(f.sink)[0], "<m1@example.org>", "Projects", "", false)

	r.srv.setFlag(t, "Projects", p, goimap.FlagSeen, true)
	f.until(t, func() bool { return len(states(f.sink)) >= 2 })
	wantReport(t, states(f.sink)[1], "<m1@example.org>", "Projects", "", true)

	r.srv.moveTo(t, "Projects", p, "Trash")
	f.until(t, func() bool { return len(states(f.sink)) >= 3 })
	wantReport(t, states(f.sink)[2], "<m1@example.org>", "Trash", model.RoleTrash, true)
}

// TestFollowCopyMovedBack: a message that comes back to INBOX under a new
// UID is the same message: it dedupes, and its place is reported.
func TestFollowCopyMovedBack(t *testing.T) {
	r := newStateRig(t, nil)
	r.deliver("<m1@example.org>")
	f := r.follow(t, r.cfg, adapter.Dependencies{})
	f.sink.await(t, 1)

	a := r.srv.moveTo(t, "INBOX", r.at(t, "INBOX", "<m1@example.org>").uid, "Archive")
	f.until(t, func() bool { return len(states(f.sink)) >= 1 })
	wantReport(t, states(f.sink)[0], "<m1@example.org>", "Archive", model.RoleArchive, false)

	r.srv.moveTo(t, "Archive", a, "INBOX")
	f.until(t, func() bool { return len(states(f.sink)) >= 2 })
	wantReport(t, states(f.sink)[1], "<m1@example.org>", "INBOX", model.RoleInbox, false)
	if n := len(f.sink.envelopes()) - len(states(f.sink)); n != 1 {
		t.Fatalf("%d messages delivered, want 1: the copy that came back is a duplicate", n)
	}
}

// TestFollowResumesFromTheProtocolStore: what the receiver follows is kept
// in the engine's protocol store, so a receiver that starts later reports
// what changed while none ran.
func TestFollowResumesFromTheProtocolStore(t *testing.T) {
	r := newStateRig(t, nil)
	r.deliver("<m1@example.org>")
	store := &protocolState{}
	first := r.follow(t, r.cfg, adapter.Dependencies{Protocol: store})
	first.sink.await(t, 1)
	first.settle(t)
	_ = first.run.stop()

	r.srv.setFlag(t, "INBOX", r.at(t, "INBOX", "<m1@example.org>").uid, goimap.FlagSeen, true)
	second := r.follow(t, r.cfg, adapter.Dependencies{Protocol: store})
	second.until(t, func() bool { return len(states(second.sink)) >= 1 })
	wantReport(t, states(second.sink)[0], "<m1@example.org>", "INBOX", model.RoleInbox, true)
	if n := len(second.sink.envelopes()) - len(states(second.sink)); n != 0 {
		t.Fatalf("the second receiver delivered %d messages again", n)
	}
}

// TestSinkRefusalOfAReportEndsRun: a refused report is a sink failure like a
// refused message: Run ends with it and the next receiver reports again.
func TestSinkRefusalOfAReportEndsRun(t *testing.T) {
	r := newStateRig(t, nil)
	r.deliver("<m1@example.org>")
	f := r.follow(t, r.cfg, adapter.Dependencies{})
	f.sink.await(t, 1)
	f.sink.setFail(model.NewError(model.Unavailable, "sink down"))
	r.srv.setFlag(t, "INBOX", r.at(t, "INBOX", "<m1@example.org>").uid, goimap.FlagSeen, true)
	waitFor(t, func() bool { f.w.release(); return len(f.run.done) > 0 })
	if err := f.run.result(t); model.CodeOf(err) != model.Unavailable {
		t.Fatalf("Run = %v, want the sink's failure", err)
	}
}

// TestSyncDoesNotSleepThroughMailArrivingElsewhere: the state sync selects
// other mailboxes; mail that reaches the retrieved mailbox meanwhile is not
// reported to the session when it comes back, so the receiver must notice it
// itself and not wait for the next IDLE restart.
func TestSyncDoesNotSleepThroughMailArrivingElsewhere(t *testing.T) {
	var armed atomic.Bool
	var srv *mailServer
	r := newStateRig(t, func(o *serverOptions, c *imap.Config) {
		c.WatchMailboxes = []string{"Projects"}
		o.afterSelectBox = func(box string) {
			if box == "Projects" && armed.CompareAndSwap(true, false) {
				srv.deliver("INBOX", mail("<late@example.org>", "late"), time.Now())
			}
		}
	})
	srv = r.srv
	r.deliver("<m1@example.org>")
	f := r.follow(t, r.cfg, adapter.Dependencies{})
	f.sink.await(t, 1)
	f.w.idle(t)
	r.srv.moveTo(t, "INBOX", r.at(t, "INBOX", "<m1@example.org>").uid, "Projects")
	armed.Store(true)
	f.w.step(t) // one round: the sync looks in Projects, and the late mail arrives meanwhile
	// No more steps: only the receiver's own wake-up can deliver it.
	f.sink.await(t, 3)
	var late bool
	for _, e := range f.sink.envelopes() {
		late = late || e.MessageID == "<late@example.org>"
	}
	if !late {
		t.Fatalf("delivered %v, want the mail that arrived during the sync", messageIDs(f.sink.envelopes()))
	}
}
