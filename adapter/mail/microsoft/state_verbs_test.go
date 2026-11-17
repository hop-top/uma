package microsoft

import (
	"context"
	"slices"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// stateEnv is an env with one inbox message m1 from a peer.
func stateEnv(t *testing.T, cfg Config) (*env, model.ExternalRef) {
	t.Helper()
	e := newEnv(t, cfg)
	e.inbound("m1", "peer@example.test", "hi")
	return e, model.ProviderMessageRef(testChan, "m1")
}

func stateEffect() adapter.Effect {
	return effect(route("peer@example.test", "m1"), "00000000-0000-7000-8000-0000000000a1")
}

func (e *env) msg(id string) *fakeMessage {
	e.g.mu.Lock()
	defer e.g.mu.Unlock()
	return e.box.messages[id]
}

func TestStateCapabilitiesReportLimits(t *testing.T) {
	e := newEnv(t, Config{})
	sts, err := e.inst.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, s := range sts {
		if !slices.Contains(stateCapabilities, s.Name) {
			continue
		}
		seen++
		l, err := model.ParseStateLimits(s.Limits)
		if err != nil || !s.Available || l.Scope != model.ScopeAccount {
			t.Fatalf("%s = %+v, %v; want available with account scope", s.Name, s, err)
		}
		switch s.Name {
		case model.CapabilityMove:
			if l.Shape != model.MoveShapeFolder || !slices.Equal(l.Roles, []string{model.RoleInbox, model.RoleArchive, model.RoleTrash, model.RoleJunk}) {
				t.Fatalf("move limits %+v", l)
			}
		case model.CapabilityFlag:
			if !slices.Equal(l.Kinds, []string{model.FlagStar, model.FlagImportant}) {
				t.Fatalf("flag kinds %v, want star and important: never pin", l.Kinds)
			}
		}
	}
	if seen != len(stateCapabilities) {
		t.Fatalf("reported %d state capabilities, want %d", seen, len(stateCapabilities))
	}
}

func TestStateVerbsRequestShapes(t *testing.T) {
	e, ref := stateEnv(t, Config{})
	ctx := t.Context()
	items := []model.ExternalRef{ref}
	eff := stateEffect()
	mustApply := func(name string, out adapter.Outcome, err error) {
		t.Helper()
		if err != nil || out.State != model.ProviderAccepted || len(out.ReferenceChanges) != 0 {
			t.Fatalf("%s = %+v, %v; want accepted with no reference change", name, out, err)
		}
	}
	folder := func() string { return e.msg("m1").folder }

	out, err := e.inst.SetReadState(ctx, adapter.ReadStateRequest{Effect: eff, Items: items, Read: true})
	mustApply("read", out, err)
	if !e.msg("m1").m.IsRead || e.g.count("PATCH /v1.0/users/"+testMailbox+"/messages/m1") != 1 {
		t.Fatalf("read: requests %q", e.g.requests())
	}
	out, err = e.inst.SetReadState(ctx, adapter.ReadStateRequest{Effect: eff, Items: items, Read: false})
	mustApply("unread", out, err)
	if e.msg("m1").m.IsRead {
		t.Fatal("unread left the message read")
	}
	out, err = e.inst.Flag(ctx, adapter.FlagRequest{Effect: eff, Items: items, Kind: model.FlagStar})
	mustApply("flag", out, err)
	if f := e.msg("m1").m.Flag; f == nil || f.FlagStatus != flagFlagged {
		t.Fatalf("flag = %+v", f)
	}
	out, err = e.inst.Flag(ctx, adapter.FlagRequest{Effect: eff, Items: items, Kind: model.FlagStar, Undo: true})
	mustApply("unflag", out, err)
	if f := e.msg("m1").m.Flag; f == nil || f.FlagStatus != flagNotFlagged {
		t.Fatalf("unflag = %+v", f)
	}
	out, err = e.inst.Flag(ctx, adapter.FlagRequest{Effect: eff, Items: items, Kind: model.FlagImportant})
	mustApply("flag important", out, err)
	if got := e.msg("m1").m.Importance; got != "high" {
		t.Fatalf("flag important: importance %q, want high", got)
	}
	out, err = e.inst.Flag(ctx, adapter.FlagRequest{Effect: eff, Items: items, Kind: model.FlagImportant, Undo: true})
	mustApply("unflag important", out, err)
	if got := e.msg("m1").m.Importance; got != "normal" {
		t.Fatalf("unflag important: importance %q, want normal", got)
	}
	for _, c := range []struct {
		name string
		run  func() (adapter.Outcome, error)
		want string
	}{
		{"archive", func() (adapter.Outcome, error) {
			return e.inst.Archive(ctx, adapter.ArchiveRequest{Effect: eff, Items: items})
		}, "archive"},
		{"unarchive", func() (adapter.Outcome, error) {
			return e.inst.Archive(ctx, adapter.ArchiveRequest{Effect: eff, Items: items, Undo: true})
		}, "inbox"},
		{"trash", func() (adapter.Outcome, error) {
			return e.inst.Trash(ctx, adapter.TrashRequest{Effect: eff, Items: items})
		}, "deleteditems"},
		{"untrash", func() (adapter.Outcome, error) {
			return e.inst.Trash(ctx, adapter.TrashRequest{Effect: eff, Items: items, Undo: true})
		}, "inbox"},
		{"move role junk", func() (adapter.Outcome, error) {
			return e.inst.Move(ctx, adapter.MoveRequest{Effect: eff, Items: items, Destination: model.MoveDestination{Role: model.RoleJunk}})
		}, "junkemail"},
		{"move to a folder id", func() (adapter.Outcome, error) {
			return e.inst.Move(ctx, adapter.MoveRequest{Effect: eff, Items: items, Destination: model.MoveDestination{ID: "fld-projects"}})
		}, "fld-projects"},
	} {
		out, err := c.run()
		mustApply(c.name, out, err)
		if folder() != c.want {
			t.Fatalf("%s: message in %q, want %q", c.name, folder(), c.want)
		}
	}
}

func TestStateRefusals(t *testing.T) {
	e, ref := stateEnv(t, Config{})
	ctx := t.Context()
	eff := stateEffect()
	rej := func(name string, out adapter.Outcome, err error, code model.ErrorCode) {
		t.Helper()
		if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != code {
			t.Fatalf("%s = %+v, %v; want rejected %s", name, out, err, code)
		}
	}
	out, err := e.inst.Flag(ctx, adapter.FlagRequest{Effect: eff, Items: []model.ExternalRef{ref}, Kind: "pin"})
	rej("flag pin", out, err, model.Unsupported)
	out, err = e.inst.Move(ctx, adapter.MoveRequest{Effect: eff, Items: []model.ExternalRef{ref}, Add: []string{"x"}})
	rej("label move", out, err, model.Unsupported)
	out, err = e.inst.Move(ctx, adapter.MoveRequest{Effect: eff, Items: []model.ExternalRef{ref}, Destination: model.MoveDestination{Role: "nope"}})
	rej("unresolved role", out, err, model.Unsupported)
	out, err = e.inst.Archive(ctx, adapter.ArchiveRequest{Effect: eff, Items: []model.ExternalRef{model.ProviderMessageRef(model.ChannelRef{Tenant: "t1", ID: "other"}, "m1")}})
	rej("foreign item", out, err, model.Validation)
	out, err = e.inst.Archive(ctx, adapter.ArchiveRequest{Effect: eff, Items: []model.ExternalRef{model.ProviderMessageRef(testChan, "gone")}})
	rej("missing message", out, err, model.NotFound)
	if n := len(e.g.requests()); n != 1 {
		t.Fatalf("requests %q, want only the missing message's move", e.g.requests())
	}
}

// A move keeps the message ID because every call asks for immutable IDs:
// the adapter reports no reference change and the next call still names the
// message by the same ID.
func TestStateIDStableAfterMove(t *testing.T) {
	e, ref := stateEnv(t, Config{})
	ctx := t.Context()
	out, err := e.inst.Archive(ctx, adapter.ArchiveRequest{Effect: stateEffect(), Items: []model.ExternalRef{ref}})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Archive = %+v, %v", out, err)
	}
	if len(out.ReferenceChanges) != 0 {
		t.Fatalf("move reported reference changes %+v: Graph immutable IDs survive a folder move", out.ReferenceChanges)
	}
	if e.msg("m1") == nil || e.msg("m1").folder != "archive" {
		t.Fatalf("message m1 not held under its ID in archive after the move")
	}
	if plain := e.g.plain; len(plain) != 0 {
		t.Fatalf("requests without Prefer IdType=ImmutableId: %q", plain)
	}
	out, err = e.inst.SetReadState(ctx, adapter.ReadStateRequest{Effect: stateEffect(), Items: []model.ExternalRef{ref}, Read: true})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("read after the move by the same ID = %+v, %v", out, err)
	}
}

func TestStateReconcileReadsBack(t *testing.T) {
	e, ref := stateEnv(t, Config{})
	ctx := t.Context()
	items := []model.ExternalRef{ref}
	eff := stateEffect()
	if _, err := e.inst.Archive(ctx, adapter.ArchiveRequest{Effect: eff, Items: items}); err != nil {
		t.Fatal(err)
	}
	rec := func(c model.Capability, mod func(*adapter.ReconcileRequest)) adapter.Outcome {
		t.Helper()
		r := adapter.ReconcileRequest{Effect: eff, Capability: c, Items: items}
		mod(&r)
		out, err := e.inst.Reconcile(ctx, r)
		if err != nil {
			t.Fatalf("Reconcile %s: %v", c, err)
		}
		return out
	}
	if out := rec(model.CapabilityArchive, func(*adapter.ReconcileRequest) {}); out.State != model.ProviderAccepted {
		t.Fatalf("archive found in Archive = %+v, want applied", out)
	}
	if out := rec(model.CapabilityTrash, func(*adapter.ReconcileRequest) {}); out.State != model.Failed {
		t.Fatalf("trash not in Deleted Items = %+v, want failed", out)
	}
	if out := rec(model.CapabilityMove, func(r *adapter.ReconcileRequest) { r.Destination = model.MoveDestination{Role: model.RoleArchive} }); out.State != model.ProviderAccepted {
		t.Fatalf("move to the archive role = %+v, want applied (found in the destination)", out)
	}
	if out := rec(model.CapabilityReadState, func(r *adapter.ReconcileRequest) { r.Read = true }); out.State != model.Failed {
		t.Fatalf("read of an unread message = %+v, want failed", out)
	}
	if out := rec(model.CapabilityFlag, func(r *adapter.ReconcileRequest) { r.FlagKind = model.FlagStar; r.Undo = true }); out.State != model.ProviderAccepted {
		t.Fatalf("unflag of an unflagged message = %+v, want applied", out)
	}
	if out := rec(model.CapabilityFlag, func(r *adapter.ReconcileRequest) { r.FlagKind = model.FlagImportant }); out.State != model.Failed {
		t.Fatalf("important of a normal message = %+v, want failed", out)
	}
	if out := rec(model.CapabilityFlag, func(r *adapter.ReconcileRequest) { r.FlagKind = model.FlagImportant; r.Undo = true }); out.State != model.ProviderAccepted {
		t.Fatalf("unimportant of a normal message = %+v, want applied", out)
	}
	if _, err := e.inst.Flag(ctx, adapter.FlagRequest{Effect: eff, Items: items, Kind: model.FlagImportant}); err != nil {
		t.Fatal(err)
	}
	if out := rec(model.CapabilityFlag, func(r *adapter.ReconcileRequest) { r.FlagKind = model.FlagImportant }); out.State != model.ProviderAccepted {
		t.Fatalf("important of a high-importance message = %+v, want applied", out)
	}
	e.g.mu.Lock()
	delete(e.box.messages, "m1")
	e.g.mu.Unlock()
	if out := rec(model.CapabilityArchive, func(*adapter.ReconcileRequest) {}); out.State != model.Rejected || out.Failure.Code != model.NotFound {
		t.Fatalf("archive of a gone message = %+v, want rejected not_found", out)
	}
}

// A partial fan-out and a lost answer both leave the outcome unknown.
func TestStateFanOutPartialIsUnknown(t *testing.T) {
	e, ref := stateEnv(t, Config{})
	e.inbound("m2", "peer@example.test", "again")
	second := model.ProviderMessageRef(testChan, "m2")
	e.g.mu.Lock()
	e.box.responses = nil
	e.g.mu.Unlock()
	// Items: the first moves, the second is gone.
	e.g.mu.Lock()
	delete(e.box.messages, "m2")
	e.g.mu.Unlock()
	_, err := e.inst.Archive(t.Context(), adapter.ArchiveRequest{Effect: stateEffect(), Items: []model.ExternalRef{ref, second}})
	wantCode(t, err, model.OutcomeUnknown)
	if e.msg("m1").folder != "archive" {
		t.Fatal("first message not moved")
	}
}

// Unarchive and untrash go to the configured undo folder: a well-known name
// or a folder ID; the inbox when none is set.
func TestStateUndoFolder(t *testing.T) {
	for _, c := range []struct{ name, undo, want string }{
		{"default", "", "inbox"}, {"well-known", "junkemail", "junkemail"}, {"id", "fld-projects", "fld-projects"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, ref := stateEnv(t, Config{UndoFolder: c.undo})
			ctx := t.Context()
			items := []model.ExternalRef{ref}
			eff := stateEffect()
			for _, run := range []struct {
				name string
				do   func(undo bool) (adapter.Outcome, error)
				cap  model.Capability
			}{
				{"archive", func(undo bool) (adapter.Outcome, error) {
					return e.inst.Archive(ctx, adapter.ArchiveRequest{Effect: eff, Items: items, Undo: undo})
				}, model.CapabilityArchive},
				{"trash", func(undo bool) (adapter.Outcome, error) {
					return e.inst.Trash(ctx, adapter.TrashRequest{Effect: eff, Items: items, Undo: undo})
				}, model.CapabilityTrash},
			} {
				if out, err := run.do(false); err != nil || out.State != model.ProviderAccepted {
					t.Fatalf("%s = %+v, %v", run.name, out, err)
				}
				if out, err := run.do(true); err != nil || out.State != model.ProviderAccepted {
					t.Fatalf("un%s = %+v, %v", run.name, out, err)
				}
				if got := e.msg("m1").folder; got != c.want {
					t.Fatalf("un%s: message in %q, want %q", run.name, got, c.want)
				}
				out, err := e.inst.Reconcile(ctx, adapter.ReconcileRequest{Effect: eff, Capability: run.cap, Items: items, Undo: true})
				if err != nil || out.State != model.ProviderAccepted {
					t.Fatalf("reconcile un%s = %+v, %v; want applied in %s", run.name, out, err, c.want)
				}
			}
		})
	}
}

func TestParseConfigUndoFolder(t *testing.T) {
	parse := func(data string) []model.ValidationIssue {
		_, issues := parseConfig(model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: []byte(data)})
		return issues
	}
	for _, ok := range []string{`{}`, `{"undo_folder":"archive"}`, `{"undo_folder":"AAMkAGI2"}`} {
		if issues := parse(ok); len(issues) != 0 {
			t.Fatalf("%s: issues %+v", ok, issues)
		}
	}
	for _, bad := range []string{`{"undo_folder":"a b"}`, `{"undo_folder":"x/y"}`, `{"undo_folder":"it's"}`} {
		if issues := parse(bad); len(issues) != 1 || issues[0].Field != "undo_folder" {
			t.Fatalf("%s: issues %+v, want one for undo_folder", bad, issues)
		}
	}
}
