package imap_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/model"
)

// stateRig is an account on a server that moves mail, with an instance of
// the adapter.
type stateRig struct {
	srv  *mailServer
	inst *imap.Instance
	cfg  imap.Config
}

// newStateRig starts a server with the mailboxes of a mail provider and an
// instance configured for it. shape adjusts the server's options or the
// configuration before they are used.
func newStateRig(t *testing.T, shape func(*serverOptions, *imap.Config)) *stateRig {
	t.Helper()
	opts := serverOptions{sec: implicitTLS, state: true}
	cfg := imap.Config{}
	if shape != nil {
		shape(&opts, &cfg)
	}
	srv := newMailServer(t, opts)
	srv.create("Archive", goimap.MailboxAttrArchive)
	srv.create("Trash", goimap.MailboxAttrTrash)
	srv.create("Junk", goimap.MailboxAttrJunk)
	srv.create("Projects")
	base := config(srv, nil)
	base.WatchMailboxes, base.StateSyncIntervalMS, base.MaxMessageBytes = cfg.WatchMailboxes, 1, cfg.MaxMessageBytes
	return &stateRig{srv: srv, cfg: base, inst: open(t, newFactory(t), channel(channelA, base), adapter.Dependencies{Credentials: passwordCreds()})}
}

func ref(id string) model.ExternalRef { return model.ProviderMessageRef(channelA, id) }

// deliver puts a message in INBOX and returns its reference.
func (r *stateRig) deliver(id string) model.ExternalRef {
	r.srv.deliver("INBOX", mail(id, "text of "+id), time.Now())
	return ref(id)
}

func wantAccepted(t *testing.T, out adapter.Outcome, err error) {
	t.Helper()
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("outcome %+v, %v; want provider_accepted", out, err)
	}
}

func wantRejected(t *testing.T, out adapter.Outcome, err error, code model.ErrorCode) {
	t.Helper()
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != code {
		t.Fatalf("outcome %+v, %v; want rejected %s", out, err, code)
	}
}

// at returns the message with Message-ID id in mailbox.
func (r *stateRig) at(t *testing.T, mailbox, id string) held {
	t.Helper()
	h, ok := r.srv.find(t, mailbox, id)
	if !ok {
		t.Fatalf("%s does not hold %s", mailbox, id)
	}
	return h
}

func (r *stateRig) wantAbsent(t *testing.T, mailbox, id string) {
	t.Helper()
	if _, ok := r.srv.find(t, mailbox, id); ok {
		t.Fatalf("%s holds %s", mailbox, id)
	}
}

func limits(t *testing.T, statuses []model.CapabilityStatus, c model.Capability) (model.CapabilityStatus, model.StateLimits) {
	t.Helper()
	for _, s := range statuses {
		if s.Name == c {
			l, err := model.ParseStateLimits(s.Limits)
			if err != nil {
				t.Fatalf("%s limits: %v", c, err)
			}
			return s, l
		}
	}
	t.Fatalf("no %s status", c)
	return model.CapabilityStatus{}, model.StateLimits{}
}

// TestStateCapabilityGates: each verb is reported with the limits of the
// server and only where the server can do it safely.
func TestStateCapabilityGates(t *testing.T) {
	t.Run("a server that moves mail", func(t *testing.T) {
		r := newStateRig(t, nil)
		st, _ := r.inst.Capabilities(t.Context())
		for _, c := range []model.Capability{model.CapabilityArchive, model.CapabilityMove, model.CapabilityReadState, model.CapabilityFlag, model.CapabilityTrash} {
			s, l := limits(t, st, c)
			if !s.Available || l.Scope != model.ScopeAccount {
				t.Errorf("%s = %+v scope %q, want available at account scope", c, s, l.Scope)
			}
		}
		_, move := limits(t, st, model.CapabilityMove)
		if move.Shape != model.MoveShapeFolder || !slices.Equal(move.Roles, []string{model.RoleInbox, model.RoleArchive, model.RoleTrash, model.RoleJunk}) {
			t.Errorf("move limits %+v, want folder shape with every role", move)
		}
		if _, flag := limits(t, st, model.CapabilityFlag); !slices.Equal(flag.Kinds, []string{model.FlagStar}) {
			t.Errorf("flag kinds %v, want star only: pin is never a flag here", flag.Kinds)
		}
	})
	t.Run("no MOVE and no UIDPLUS", func(t *testing.T) {
		srv := newMailServer(t, serverOptions{sec: implicitTLS})
		inst := open(t, newFactory(t), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: passwordCreds()})
		st, _ := inst.Capabilities(t.Context())
		for _, c := range []model.Capability{model.CapabilityArchive, model.CapabilityMove, model.CapabilityTrash} {
			if s, _ := limits(t, st, c); s.Available || s.Reason != model.Unsupported {
				t.Errorf("%s = %+v, want unsupported: a plain EXPUNGE would remove every \\Deleted message", c, s)
			}
		}
		for _, c := range []model.Capability{model.CapabilityReadState, model.CapabilityFlag} {
			if s, _ := limits(t, st, c); !s.Available {
				t.Errorf("%s = %+v, want available: STORE needs no extension", c, s)
			}
		}
	})
	t.Run("COPY and UIDPLUS without MOVE", func(t *testing.T) {
		r := newStateRig(t, func(o *serverOptions, _ *imap.Config) { o.noMove = true })
		st, _ := r.inst.Capabilities(t.Context())
		if s, _ := limits(t, st, model.CapabilityMove); !s.Available {
			t.Errorf("move = %+v, want available through COPY and UID EXPUNGE", s)
		}
	})
	t.Run("no archive mailbox", func(t *testing.T) {
		srv := newMailServer(t, serverOptions{sec: implicitTLS, state: true})
		srv.create("Trash", goimap.MailboxAttrTrash)
		inst := open(t, newFactory(t), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: passwordCreds()})
		st, _ := inst.Capabilities(t.Context())
		if s, _ := limits(t, st, model.CapabilityArchive); s.Available {
			t.Errorf("archive = %+v, want unavailable without an \\Archive mailbox", s)
		}
		if s, l := limits(t, st, model.CapabilityMove); !s.Available || slices.Contains(l.Roles, model.RoleArchive) {
			t.Errorf("move = %+v roles %v, want available without the archive role", s, l.Roles)
		}
	})
	t.Run("All Mail stands for archive", func(t *testing.T) {
		srv := newMailServer(t, serverOptions{sec: implicitTLS, state: true})
		srv.create("[Gmail]/All Mail", goimap.MailboxAttrAll)
		inst := open(t, newFactory(t), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: passwordCreds()})
		st, _ := inst.Capabilities(t.Context())
		if s, l := limits(t, st, model.CapabilityMove); !s.Available || !slices.Contains(l.Roles, model.RoleArchive) {
			t.Errorf("move = %+v roles %v, want the archive role on \\All", s, l.Roles)
		}
	})
	t.Run("no credentials", func(t *testing.T) {
		srv := newMailServer(t, serverOptions{sec: implicitTLS, state: true})
		inst := open(t, newFactory(t), channel(channelA, config(srv, nil)), adapter.Dependencies{})
		st, _ := inst.Capabilities(t.Context())
		for _, s := range st {
			if s.Available {
				t.Errorf("%s available without credentials", s.Name)
			}
		}
	})
}

// TestReadAndFlagWriteOnTheirOwnSession: read and flag change \Seen and
// \Flagged on the account's copy, one message at a time, and a message the
// mailbox does not hold is a final refusal.
func TestReadAndFlagWriteOnTheirOwnSession(t *testing.T) {
	r := newStateRig(t, nil)
	m1, m2 := r.deliver("<m1@example.org>"), r.deliver("<m2@example.org>")
	ctx := t.Context()

	out, err := r.inst.SetReadState(ctx, adapter.ReadStateRequest{Items: []model.ExternalRef{m1}, Read: true})
	wantAccepted(t, out, err)
	if !r.at(t, "INBOX", m1.ID).has(goimap.FlagSeen) || r.at(t, "INBOX", m2.ID).has(goimap.FlagSeen) {
		t.Fatal("read marked the wrong messages")
	}
	if n := r.srv.seen("INBOX"); n != 1 {
		t.Fatalf("%d messages \\Seen, want 1", n)
	}
	out, err = r.inst.SetReadState(ctx, adapter.ReadStateRequest{Items: []model.ExternalRef{m1}, Read: true})
	wantAccepted(t, out, err) // already held
	out, err = r.inst.SetReadState(ctx, adapter.ReadStateRequest{Items: []model.ExternalRef{m1}, Read: false})
	wantAccepted(t, out, err)
	if r.at(t, "INBOX", m1.ID).has(goimap.FlagSeen) {
		t.Fatal("unread left \\Seen")
	}

	out, err = r.inst.Flag(ctx, adapter.FlagRequest{Items: []model.ExternalRef{m1, m2}, Kind: model.FlagStar})
	wantAccepted(t, out, err)
	if !r.at(t, "INBOX", m1.ID).has(goimap.FlagFlagged) || !r.at(t, "INBOX", m2.ID).has(goimap.FlagFlagged) {
		t.Fatal("flag did not reach every message of the conversation")
	}
	out, err = r.inst.Flag(ctx, adapter.FlagRequest{Items: []model.ExternalRef{m1}, Kind: model.FlagStar, Undo: true})
	wantAccepted(t, out, err)
	if r.at(t, "INBOX", m1.ID).has(goimap.FlagFlagged) {
		t.Fatal("unflag left \\Flagged")
	}

	out, err = r.inst.Flag(ctx, adapter.FlagRequest{Items: []model.ExternalRef{m1}, Kind: model.FlagImportant})
	wantRejected(t, out, err, model.Unsupported)
	out, err = r.inst.SetReadState(ctx, adapter.ReadStateRequest{Items: []model.ExternalRef{ref("<nobody@example.org>")}, Read: true})
	wantRejected(t, out, err, model.NotFound)
}

// TestMoveReportsTheNewReference: a move reports the location the message
// has in the destination (COPYUID), and that location, not the Message-ID the
// move started from, is what finds the message next. Without the reported
// reference a follow-up on the moved message finds nothing.
func TestMoveReportsTheNewReference(t *testing.T) {
	r := newStateRig(t, nil)
	old := r.deliver("<m1@example.org>")
	ctx := t.Context()

	out, err := r.inst.Move(ctx, adapter.MoveRequest{Items: []model.ExternalRef{old}, Destination: model.MoveDestination{ID: "Projects"}})
	wantAccepted(t, out, err)
	r.wantAbsent(t, "INBOX", old.ID)
	dst := r.at(t, "Projects", old.ID)
	if len(out.ReferenceChanges) != 1 || out.ReferenceChanges[0].Old != old {
		t.Fatalf("reference changes %+v, want one from %v", out.ReferenceChanges, old)
	}
	next := out.ReferenceChanges[0].New
	box, uv, uid, mid, ok := parseLocation(next.ID)
	if !ok || next.Channel != channelA || next.Namespace != model.ProviderMessageNamespace || next.Kind != model.RefKindMessage ||
		box != "Projects" || uv != dst.uidValidity || uid != dst.uid || mid != old.ID {
		t.Fatalf("new reference %+v, want the location of the copy in Projects (uid %d, validity %d)", next, dst.uid, dst.uidValidity)
	}

	// The follow-up names the new reference and lands on the moved message.
	out, err = r.inst.Flag(ctx, adapter.FlagRequest{Items: []model.ExternalRef{next}, Kind: model.FlagStar})
	wantAccepted(t, out, err)
	if !r.at(t, "Projects", old.ID).has(goimap.FlagFlagged) {
		t.Fatal("the follow-up flag did not reach the moved message")
	}
	// The old reference no longer finds it: the Message-ID is looked up in
	// the mailboxes mail arrives in, not in every folder.
	out, err = r.inst.Flag(ctx, adapter.FlagRequest{Items: []model.ExternalRef{old}, Kind: model.FlagStar, Undo: true})
	wantRejected(t, out, err, model.NotFound)
	if !r.at(t, "Projects", old.ID).has(goimap.FlagFlagged) {
		t.Fatal("a refused follow-up changed the message")
	}

	// And on from there: the next move starts at the location.
	out, err = r.inst.Move(ctx, adapter.MoveRequest{Items: []model.ExternalRef{next}, Destination: model.MoveDestination{ID: "Archive"}})
	wantAccepted(t, out, err)
	r.at(t, "Archive", old.ID)
	r.wantAbsent(t, "Projects", old.ID)
	if len(out.ReferenceChanges) != 1 || out.ReferenceChanges[0].Old != next {
		t.Fatalf("second move reference changes %+v, want one from %v", out.ReferenceChanges, next)
	}
}

// TestMoveStaleLocationIsNotFound: a location whose UIDVALIDITY changed
// names nothing.
func TestMoveStaleLocationIsNotFound(t *testing.T) {
	r := newStateRig(t, nil)
	old := r.deliver("<m1@example.org>")
	out, err := r.inst.Move(t.Context(), adapter.MoveRequest{Items: []model.ExternalRef{old}, Destination: model.MoveDestination{ID: "Projects"}})
	wantAccepted(t, out, err)
	next := out.ReferenceChanges[0].New
	r.srv.recreate("Projects")
	out, err = r.inst.Flag(t.Context(), adapter.FlagRequest{Items: []model.ExternalRef{next}, Kind: model.FlagStar})
	wantRejected(t, out, err, model.NotFound)
}

// TestArchiveTrashAndRoles: archive and trash resolve the special-use
// mailboxes, and their undo returns the message to the retrieved mailbox.
func TestArchiveTrashAndRoles(t *testing.T) {
	r := newStateRig(t, nil)
	m := r.deliver("<m1@example.org>")
	ctx := t.Context()
	hop := func(name string, out adapter.Outcome, err error, box string) {
		t.Helper()
		wantAccepted(t, out, err)
		r.at(t, box, m.ID)
		if len(out.ReferenceChanges) != 1 {
			t.Fatalf("%s: reference changes %+v, want one", name, out.ReferenceChanges)
		}
	}
	out, err := r.inst.Archive(ctx, adapter.ArchiveRequest{Items: []model.ExternalRef{m}})
	hop("archive", out, err, "Archive")
	cur := out.ReferenceChanges[0].New
	out, err = r.inst.Archive(ctx, adapter.ArchiveRequest{Items: []model.ExternalRef{cur}, Undo: true})
	hop("unarchive", out, err, "INBOX")
	cur = out.ReferenceChanges[0].New
	out, err = r.inst.Trash(ctx, adapter.TrashRequest{Items: []model.ExternalRef{cur}})
	hop("trash", out, err, "Trash")
	cur = out.ReferenceChanges[0].New
	out, err = r.inst.Move(ctx, adapter.MoveRequest{Items: []model.ExternalRef{cur}, Destination: model.MoveDestination{Role: model.RoleJunk}})
	hop("move to junk", out, err, "Junk")
	cur = out.ReferenceChanges[0].New
	out, err = r.inst.Trash(ctx, adapter.TrashRequest{Items: []model.ExternalRef{cur}, Undo: true})
	hop("untrash", out, err, "INBOX")

	// Already where it should be: applied, nothing to re-resolve.
	cur = out.ReferenceChanges[0].New
	out, err = r.inst.Move(ctx, adapter.MoveRequest{Items: []model.ExternalRef{cur}, Destination: model.MoveDestination{Role: model.RoleInbox}})
	wantAccepted(t, out, err)
	if len(out.ReferenceChanges) != 0 {
		t.Fatalf("a move to where the message is reported %+v", out.ReferenceChanges)
	}

	// Label changes and an empty destination are refused; so is a role the
	// server does not resolve.
	out, err = r.inst.Move(ctx, adapter.MoveRequest{Items: []model.ExternalRef{cur}, Add: []string{"x"}})
	wantRejected(t, out, err, model.Unsupported)
	out, err = r.inst.Move(ctx, adapter.MoveRequest{Items: []model.ExternalRef{cur}, Destination: model.MoveDestination{ID: "Nowhere"}})
	wantRejected(t, out, err, model.NotFound)
	r.at(t, "INBOX", m.ID)

	bare := newMailServer(t, serverOptions{sec: implicitTLS, state: true})
	bare.deliver("INBOX", mail("<m1@example.org>", "x"), time.Now())
	inst := open(t, newFactory(t), channel(channelA, config(bare, nil)), adapter.Dependencies{Credentials: passwordCreds()})
	out, err = inst.Archive(ctx, adapter.ArchiveRequest{Items: []model.ExternalRef{m}})
	wantRejected(t, out, err, model.Unsupported)
}

// TestMoveWithoutMoveCopiesFirst: without MOVE the message is copied, and
// only once the server confirmed the copy is that UID, and no other,
// deleted and expunged. A refused copy deletes nothing.
func TestMoveWithoutMoveCopiesFirst(t *testing.T) {
	r := newStateRig(t, func(o *serverOptions, _ *imap.Config) { o.noMove = true })
	a, b := r.deliver("<a@example.org>"), r.deliver("<b@example.org>")
	// Another client marked a message \Deleted and has not expunged yet.
	r.srv.setFlag(t, "INBOX", r.at(t, "INBOX", a.ID).uid, goimap.FlagDeleted, true)

	out, err := r.inst.Move(t.Context(), adapter.MoveRequest{Items: []model.ExternalRef{b}, Destination: model.MoveDestination{ID: "Projects"}})
	wantAccepted(t, out, err)
	r.at(t, "Projects", b.ID)
	r.wantAbsent(t, "INBOX", b.ID)
	if !r.at(t, "INBOX", a.ID).has(goimap.FlagDeleted) {
		t.Fatal("the other client's \\Deleted message is gone: a plain EXPUNGE ran")
	}
	if len(out.ReferenceChanges) != 1 {
		t.Fatalf("reference changes %+v, want the COPYUID of the copy", out.ReferenceChanges)
	}
	lines := r.srv.clientLines()
	var order []string
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) >= 3 && f[1] == "UID" && (f[2] == "COPY" || f[2] == "EXPUNGE" || f[2] == "STORE") {
			order = append(order, f[2])
		}
		if len(f) >= 2 && strings.EqualFold(f[1], "EXPUNGE") {
			t.Fatalf("plain EXPUNGE sent: %q", l)
		}
	}
	// STORE of the earlier \Deleted flag came from the other client.
	if !slices.Equal(order[len(order)-3:], []string{"COPY", "STORE", "EXPUNGE"}) {
		t.Fatalf("commands %v, want COPY, STORE, then UID EXPUNGE", order)
	}

	before := len(r.srv.clientLines())
	out, err = r.inst.Move(t.Context(), adapter.MoveRequest{Items: []model.ExternalRef{ref("<a@example.org>")}, Destination: model.MoveDestination{ID: "Nowhere"}})
	wantRejected(t, out, err, model.NotFound)
	for _, l := range r.srv.clientLines()[before:] {
		if f := strings.Fields(l); len(f) >= 3 && f[1] == "UID" && (f[2] == "STORE" || f[2] == "EXPUNGE") {
			t.Fatalf("%q sent after a refused COPY: nothing may be deleted", l)
		}
	}
	r.at(t, "INBOX", a.ID)
}

// TestMoveNeedsMoveOrUIDPlus: a server with neither is not moved on.
func TestMoveNeedsMoveOrUIDPlus(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	srv.deliver("INBOX", mail("<m1@example.org>", "x"), time.Now())
	srv.create("Projects")
	inst := open(t, newFactory(t), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: passwordCreds()})
	out, err := inst.Move(t.Context(), adapter.MoveRequest{Items: []model.ExternalRef{ref("<m1@example.org>")}, Destination: model.MoveDestination{ID: "Projects"}})
	wantRejected(t, out, err, model.Unsupported)
	if srv.count("INBOX") != 1 || srv.count("Projects") != 0 {
		t.Fatal("the message moved")
	}
}

// TestPartialFanOutIsUnknown: the first message moved and the second did
// not, so the outcome is unknown, not rejected, and reconcile finds the
// first one in the destination.
func TestPartialFanOutIsUnknown(t *testing.T) {
	r := newStateRig(t, nil)
	a, b := r.deliver("<a@example.org>"), r.deliver("<b@example.org>")
	r.srv.respond(stateReply{apply: true}, stateReply{refuse: &goimap.Error{Type: goimap.StatusResponseTypeNo, Text: "no"}})
	out, err := r.inst.Move(t.Context(), adapter.MoveRequest{Items: []model.ExternalRef{a, b}, Destination: model.MoveDestination{ID: "Projects"}})
	if err != nil || out.State != model.Unknown || out.Failure == nil || out.Failure.Code != model.OutcomeUnknown {
		t.Fatalf("outcome %+v, %v; want outcome_unknown", out, err)
	}
	r.at(t, "Projects", a.ID)
	r.at(t, "INBOX", b.ID)
	rec, err := r.inst.Reconcile(t.Context(), adapter.ReconcileRequest{
		Capability: model.CapabilityMove, Items: []model.ExternalRef{a, b}, Destination: model.MoveDestination{ID: "Projects"},
	})
	if err != nil || rec.State != model.Unknown {
		t.Fatalf("Reconcile = %+v, %v; want unknown: one message moved, one did not", rec, err)
	}
}

// TestReconcileAfterALostAnswer: a move whose answer was lost is found in its
// destination and reported applied with the new reference; one whose request
// was lost is found at its source and not applied. Flag changes are read
// back from the flags.
func TestReconcileAfterALostAnswer(t *testing.T) {
	ctx := t.Context()
	t.Run("move applied", func(t *testing.T) {
		r := newStateRig(t, nil)
		m := r.deliver("<m1@example.org>")
		r.srv.respond(stateReply{lostReply: true})
		out, err := r.inst.Move(ctx, adapter.MoveRequest{Items: []model.ExternalRef{m}, Destination: model.MoveDestination{ID: "Projects"}})
		if err != nil || out.State != model.Unknown || out.Failure.Code != model.OutcomeUnknown {
			t.Fatalf("outcome %+v, %v; want outcome_unknown", out, err)
		}
		// The original reference no longer finds it: only the destination does.
		req := adapter.ReconcileRequest{Capability: model.CapabilityMove, Items: []model.ExternalRef{m}, Destination: model.MoveDestination{ID: "Projects"}}
		rec, err := r.inst.Reconcile(ctx, req)
		wantAccepted(t, rec, err)
		dst := r.at(t, "Projects", m.ID)
		if len(rec.ReferenceChanges) != 1 || rec.ReferenceChanges[0].Old != m {
			t.Fatalf("reference changes %+v, want the copy's location", rec.ReferenceChanges)
		}
		if _, uv, uid, _, ok := parseLocation(rec.ReferenceChanges[0].New.ID); !ok || uv != dst.uidValidity || uid != dst.uid {
			t.Fatalf("new reference %+v does not name the copy %+v", rec.ReferenceChanges[0].New, dst)
		}
		// Asked again with the reference the move gave: still applied, no change to report.
		req.Items = []model.ExternalRef{rec.ReferenceChanges[0].New}
		rec, err = r.inst.Reconcile(ctx, req)
		wantAccepted(t, rec, err)
		if len(rec.ReferenceChanges) != 0 {
			t.Fatalf("reference changes %+v for a message named by where it is", rec.ReferenceChanges)
		}
	})
	t.Run("move not applied", func(t *testing.T) {
		r := newStateRig(t, nil)
		m := r.deliver("<m1@example.org>")
		r.srv.respond(stateReply{cut: true})
		out, err := r.inst.Move(ctx, adapter.MoveRequest{Items: []model.ExternalRef{m}, Destination: model.MoveDestination{ID: "Projects"}})
		if err != nil || out.State != model.Unknown {
			t.Fatalf("outcome %+v, %v; want unknown", out, err)
		}
		rec, err := r.inst.Reconcile(ctx, adapter.ReconcileRequest{Capability: model.CapabilityMove, Items: []model.ExternalRef{m}, Destination: model.MoveDestination{ID: "Projects"}})
		if err != nil || rec.State != model.Failed {
			t.Fatalf("Reconcile = %+v, %v; want failed: the message is still at its source", rec, err)
		}
	})
	t.Run("archive and trash use their roles", func(t *testing.T) {
		r := newStateRig(t, nil)
		m := r.deliver("<m1@example.org>")
		r.srv.respond(stateReply{lostReply: true})
		if out, _ := r.inst.Archive(ctx, adapter.ArchiveRequest{Items: []model.ExternalRef{m}}); out.State != model.Unknown {
			t.Fatalf("outcome %+v, want unknown", out)
		}
		rec, err := r.inst.Reconcile(ctx, adapter.ReconcileRequest{Capability: model.CapabilityArchive, Items: []model.ExternalRef{m}})
		wantAccepted(t, rec, err)
		rec, err = r.inst.Reconcile(ctx, adapter.ReconcileRequest{Capability: model.CapabilityTrash, Items: []model.ExternalRef{m}})
		if err != nil || rec.State != model.Failed {
			t.Fatalf("Reconcile of a trash that never ran = %+v, %v; want failed", rec, err)
		}
	})
	t.Run("read and flag", func(t *testing.T) {
		r := newStateRig(t, nil)
		m := r.deliver("<m1@example.org>")
		r.srv.respond(stateReply{lostReply: true})
		if out, _ := r.inst.SetReadState(ctx, adapter.ReadStateRequest{Items: []model.ExternalRef{m}, Read: true}); out.State != model.Unknown {
			t.Fatalf("outcome %+v, want unknown", out)
		}
		rec, err := r.inst.Reconcile(ctx, adapter.ReconcileRequest{Capability: model.CapabilityReadState, Items: []model.ExternalRef{m}, Read: true})
		wantAccepted(t, rec, err)
		rec, err = r.inst.Reconcile(ctx, adapter.ReconcileRequest{Capability: model.CapabilityReadState, Items: []model.ExternalRef{m}, Read: false})
		if err != nil || rec.State != model.Failed {
			t.Fatalf("Reconcile of the opposite read value = %+v, %v; want failed", rec, err)
		}
		rec, err = r.inst.Reconcile(ctx, adapter.ReconcileRequest{Capability: model.CapabilityFlag, Items: []model.ExternalRef{m}, FlagKind: model.FlagStar})
		if err != nil || rec.State != model.Failed {
			t.Fatalf("Reconcile of a flag never set = %+v, %v; want failed", rec, err)
		}
	})
}

// TestRetrievalNeverWrites: the receiver examines; the verbs alone select
// read-write.
func TestRetrievalNeverWrites(t *testing.T) {
	r := newStateRig(t, nil)
	r.deliver("<m1@example.org>")
	w := &stepWaiter{}
	inst := open(t, steppedFactory(t, w), channel(channelA, r.cfg), adapter.Dependencies{Credentials: passwordCreds()})
	s := newSink()
	run(t, inst, s)
	s.await(t, 1)
	w.step(t)
	for _, l := range r.srv.clientLines() {
		f := strings.Fields(l)
		if len(f) >= 2 && strings.EqualFold(f[1], "SELECT") {
			t.Fatalf("the receiver selected a mailbox read-write: %q", l)
		}
		if len(f) >= 3 && f[1] == "UID" && f[2] == "STORE" {
			t.Fatalf("the receiver stored flags: %q", l)
		}
	}
	if n := r.srv.seen("INBOX"); n != 0 {
		t.Fatalf("%d messages \\Seen after retrieval", n)
	}
	if _, err := r.inst.SetReadState(t.Context(), adapter.ReadStateRequest{Items: []model.ExternalRef{ref("<m1@example.org>")}, Read: true}); err != nil {
		t.Fatal(err)
	}
	var selects int
	for _, l := range r.srv.clientLines() {
		if f := strings.Fields(l); len(f) >= 2 && strings.EqualFold(f[1], "SELECT") {
			selects++
		}
	}
	if selects == 0 {
		t.Fatal("the read verb never selected read-write")
	}
}
