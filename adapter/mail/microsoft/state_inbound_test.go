package microsoft

import (
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// reports returns the state reports the sink accepted, keyed by message.
func reports(s *testSink) map[string][]model.ProviderStateReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]model.ProviderStateReport{}
	for _, in := range s.got {
		if in.Envelope.Kind == model.EnvelopeConversationState {
			r := in.Envelope.State.Clone()
			out[r.Conversation.ID] = append(out[r.Conversation.ID], r)
		}
	}
	return out
}

func messagesOf(s *testSink) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, in := range s.got {
		if in.Envelope.Kind != model.EnvelopeConversationState {
			out = append(out, in.Envelope.MessageID)
		}
	}
	return out
}

func TestStateDeltaFollowsMovesAcrossFolders(t *testing.T) {
	e := newEnv(t, Config{})
	ctx := t.Context()
	for _, id := range []string{"m1", "m2", "m3", "m4"} {
		e.inbound(id, "peer@example.test", id)
	}
	// The first round delivers the mail; new mail in its default state is
	// not reported as state again.
	first := &testSink{}
	if err := round(t, e, first); err != nil {
		t.Fatal(err)
	}
	if got := messagesOf(first); !slices.Equal(got, []string{"m1", "m2", "m3", "m4"}) {
		t.Fatalf("messages %v", got)
	}
	if r := reports(first); len(r) != 0 {
		t.Fatalf("state reports for new default mail: %+v", r)
	}

	items := func(id string) []model.ExternalRef {
		return []model.ExternalRef{model.ProviderMessageRef(testChan, id)}
	}
	eff := stateEffect()
	// m1: archived (removed from the inbox, added to Archive); m2: moved to
	// a folder that is not watched; m3: permanently deleted; m4: read and
	// starred in the inbox.
	if _, err := e.inst.Archive(ctx, adapter.ArchiveRequest{Effect: eff, Items: items("m1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.inst.Move(ctx, adapter.MoveRequest{Effect: eff, Items: items("m2"), Destination: model.MoveDestination{ID: "fld-projects"}}); err != nil {
		t.Fatal(err)
	}
	e.g.mu.Lock()
	delete(e.box.messages, "m3")
	e.box.flog["inbox"] = append(e.box.flog["inbox"], fchange{id: "m3", removed: true})
	e.g.mu.Unlock()
	if _, err := e.inst.SetReadState(ctx, adapter.ReadStateRequest{Effect: eff, Items: items("m4"), Read: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.inst.Flag(ctx, adapter.FlagRequest{Effect: eff, Items: items("m4"), Kind: model.FlagStar}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.inst.Flag(ctx, adapter.FlagRequest{Effect: eff, Items: items("m4"), Kind: model.FlagImportant}); err != nil {
		t.Fatal(err)
	}

	second := &testSink{}
	if err := round(t, e, second); err != nil {
		t.Fatal(err)
	}
	if got := messagesOf(second); len(got) != 0 {
		t.Fatalf("a state change delivered messages %v", got)
	}
	r := reports(second)
	if len(r) != 3 {
		t.Fatalf("reports for %d messages, want m1, m2 and m4 (m3 was deleted): %+v", len(r), r)
	}
	// A move is one report, not a delete in one folder and an add in another.
	m1 := r["m1"]
	if len(m1) != 1 {
		t.Fatalf("m1 reports %d, want one", len(m1))
	}
	st := m1[0].State
	if !st.Archived || st.Trashed || st.Folder == nil || st.Folder.Role != model.RoleArchive || st.Folder.ID != folderID("archive") || st.Complete {
		t.Fatalf("m1 state %+v, want archived, in Archive, partial", st)
	}
	if got := m1[0].Conversation; got != model.ProviderMessageRef(testChan, "m1") {
		t.Fatalf("m1 conversation %+v", got)
	}
	m2 := r["m2"]
	if len(m2) != 1 || m2[0].State.Folder == nil || m2[0].State.Folder.ID != "fld-projects" || m2[0].State.Archived || m2[0].State.Trashed {
		t.Fatalf("m2 reports %+v, want one in the unwatched folder, read back by ID", m2)
	}
	m4 := r["m4"]
	if len(m4) != 1 || m4[0].State.Read == nil || !*m4[0].State.Read || !slices.Equal(m4[0].State.Flags, []string{model.FlagStar, model.FlagImportant}) || m4[0].State.Folder.Role != model.RoleInbox {
		t.Fatalf("m4 reports %+v, want read, starred, important, in the inbox", m4)
	}
	for id, rs := range r {
		if err := rs[0].Validate(); err != nil {
			t.Fatalf("%s report invalid: %v", id, err)
		}
	}

	// Trash then a round: Deleted Items is watched, the role is trash.
	if _, err := e.inst.Trash(ctx, adapter.TrashRequest{Effect: eff, Items: items("m1")}); err != nil {
		t.Fatal(err)
	}
	third := &testSink{}
	if err := round(t, e, third); err != nil {
		t.Fatal(err)
	}
	m1 = reports(third)["m1"]
	if len(m1) != 1 || !m1[0].State.Trashed || m1[0].State.Archived || m1[0].State.Folder.Role != model.RoleTrash {
		t.Fatalf("m1 after trash %+v", m1)
	}
	// Nothing changed: nothing reported.
	fourth := &testSink{}
	if err := round(t, e, fourth); err != nil || len(reports(fourth)) != 0 {
		t.Fatalf("idle round: %v, reports %+v", err, reports(fourth))
	}
}

func TestStateDeltaWatchesConfiguredFolders(t *testing.T) {
	e := newEnv(t, Config{WatchedFolders: []string{"fld-projects"}})
	e.g.mu.Lock()
	e.box.folders["fld-projects"] = true
	e.g.mu.Unlock()
	e.inbound("m1", "peer@example.test", "hi")
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.inst.Move(t.Context(), adapter.MoveRequest{Effect: stateEffect(), Items: []model.ExternalRef{model.ProviderMessageRef(testChan, "m1")}, Destination: model.MoveDestination{ID: "fld-projects"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.inst.SetReadState(t.Context(), adapter.ReadStateRequest{Effect: stateEffect(), Items: []model.ExternalRef{model.ProviderMessageRef(testChan, "m1")}, Read: true}); err != nil {
		t.Fatal(err)
	}
	s := &testSink{}
	if err := round(t, e, s); err != nil {
		t.Fatal(err)
	}
	if got := reports(s)["m1"]; len(got) != 1 || got[0].State.Folder.ID != "fld-projects" || !*got[0].State.Read {
		t.Fatalf("reports %+v, want one from the configured folder", got)
	}
	// A configured folder the mailbox lacks is skipped, not fatal.
	e2 := newEnv(t, Config{WatchedFolders: []string{"no-such-folder"}})
	if err := round(t, e2, &testSink{}); err != nil {
		t.Fatalf("round with a missing watched folder: %v", err)
	}
}

func TestStateDeltaLinksSurviveRestart(t *testing.T) {
	p := &memProtocol{}
	e := newEnv(t, Config{}, withProtocol(p))
	e.inbound("m1", "peer@example.test", "hi")
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	st, err := e.inst.loadState(t.Context())
	if err != nil || len(st.Folders) == 0 || st.Folders[folderArchive].DeltaLink == "" {
		t.Fatalf("stored state %+v, %v: want a delta link per watched folder", st, err)
	}
}

func TestParseConfigWatchedFolders(t *testing.T) {
	if _, issues := parseConfig(model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: []byte(`{"watched_folders":["a b"]}`)}); len(issues) != 1 {
		t.Fatalf("issues %+v, want one", issues)
	}
}

// Mail the inbox delta delivers in the same round is not reported again in
// its default state; one already read or starred is.
func TestStateDeltaSkipsNewDefaultMail(t *testing.T) {
	e := newEnv(t, Config{})
	e.clock.Advance(time.Unix(1_700_000_000, 0).UTC().Sub(e.clock.Now()) + time.Minute)
	e.inbound("m1", "peer@example.test", "hi")
	e.inbound("m2", "peer@example.test", "hi")
	if _, err := e.inst.SetReadState(t.Context(), adapter.ReadStateRequest{Effect: stateEffect(), Items: []model.ExternalRef{model.ProviderMessageRef(testChan, "m2")}, Read: true}); err != nil {
		t.Fatal(err)
	}
	s := &testSink{}
	if err := round(t, e, s); err != nil {
		t.Fatal(err)
	}
	r := reports(s)
	if len(r["m1"]) != 0 || len(r["m2"]) != 1 {
		t.Fatalf("reports %+v, want one for m2 (read) and none for m1 (default)", r)
	}
}

// Importance high alone is reported as the important flag; normal reports
// none, and a high-importance new message is not in its default state.
func TestStateDeltaReportsImportant(t *testing.T) {
	e := newEnv(t, Config{})
	e.clock.Advance(time.Unix(1_700_000_000, 0).UTC().Sub(e.clock.Now()) + time.Minute)
	e.inbound("m1", "peer@example.test", "hi")
	e.inbound("m2", "peer@example.test", "hi")
	if _, err := e.inst.Flag(t.Context(), adapter.FlagRequest{Effect: stateEffect(), Items: []model.ExternalRef{model.ProviderMessageRef(testChan, "m2")}, Kind: model.FlagImportant}); err != nil {
		t.Fatal(err)
	}
	s := &testSink{}
	if err := round(t, e, s); err != nil {
		t.Fatal(err)
	}
	r := reports(s)
	if len(r["m1"]) != 0 || len(r["m2"]) != 1 || !slices.Equal(r["m2"][0].State.Flags, []string{model.FlagImportant}) {
		t.Fatalf("reports %+v, want m2 flagged important only and none for m1", r)
	}
	if _, err := e.inst.Flag(t.Context(), adapter.FlagRequest{Effect: stateEffect(), Items: []model.ExternalRef{model.ProviderMessageRef(testChan, "m2")}, Kind: model.FlagImportant, Undo: true}); err != nil {
		t.Fatal(err)
	}
	s = &testSink{}
	if err := round(t, e, s); err != nil {
		t.Fatal(err)
	}
	if got := reports(s)["m2"]; len(got) != 1 || len(got[0].State.Flags) != 0 {
		t.Fatalf("after unflag: %+v, want one report with no flags", got)
	}
}
