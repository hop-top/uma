package microsoft

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/testing/xrrtest"
)

// Recorded Microsoft Graph exchanges replay from testdata/cassettes/graph.
// Until cassettes are recorded the replay test skips. To record against a
// sandbox mailbox:
//
//	MS_GRAPH_ACCESS_TOKEN=<token> MS_GRAPH_MAILBOX=<upn> MS_GRAPH_PEER=<address> \
//	  XRR_MODE=record go test -run TestRecordedGraph ./...
//
// The mailbox and peer addresses become placeholders in every form Graph
// writes them (literal, with "@" percent-encoded as in an @odata.context
// or a paging link, and encoded twice); the token never reaches a
// cassette. A recording fails when any of them survives in a cassette.
const cassetteDir = "testdata/cassettes/graph"

func TestRecordedGraph(t *testing.T) {
	mode := xrrtest.Mode(os.Getenv(xrrtest.EnvMode))
	if mode == "" || mode == xrrtest.ModeReplay {
		if _, err := os.Stat(cassetteDir); os.IsNotExist(err) {
			t.Skip("no recorded Graph cassettes yet")
		}
	}
	mailbox, peer := envOr("MS_GRAPH_MAILBOX", "mailbox@example.test"), envOr("MS_GRAPH_PEER", "peer@example.test")
	client := xrrtest.NewClient(t, cassetteDir, recordingOptions(mailbox, peer)...)
	token := envOr("MS_GRAPH_ACCESS_TOKEN", "replay-token")
	recordedScenario(t, client, "", mailbox, peer, token)
	if mode == xrrtest.ModeRecord {
		assertNoResidue(t, cassetteDir, mailbox, peer, token)
	}
}

// TestRecordedScenarioOverFake runs the recorded scenario through the same
// xrrtest client in passthrough mode against the fake, so the wiring and
// the scenario stay working until cassettes exist.
func TestRecordedScenarioOverFake(t *testing.T) {
	g := newFakeGraph(t)
	const mailbox, peer = "mailbox@example.test", "peer@example.test"
	g.mailbox(mailbox, testChan)
	opts := append(recordingOptions(mailbox, peer),
		xrrtest.WithMode(xrrtest.ModePassthrough), xrrtest.WithTransport(g.srv.Client().Transport))
	recordedScenario(t, xrrtest.NewClient(t, t.TempDir(), opts...), g.endpoint(), mailbox, peer, "tok-1")
	if g.count("POST /v1.0/users/"+mailbox+"/messages/draft-1/send") != 1 || g.count("PATCH /v1.0/users/"+mailbox+"/messages/draft-1") != 8 || g.count("POST /v1.0/users/"+mailbox+"/messages/draft-1/move") != 6 {
		t.Fatalf("scenario requests %q", g.requests())
	}
}

// recordingOptions maps each address, in every form Graph writes it, to
// one placeholder. The forms never contain one another, so replacement
// order does not matter.
func recordingOptions(mailbox, peer string) []xrrtest.Option {
	var opts []xrrtest.Option
	for _, p := range []struct{ value, placeholder string }{{mailbox, "MAILBOX"}, {peer, "PEER"}} {
		for _, form := range valueForms(p.value) {
			opts = append(opts, xrrtest.WithPlaceholder(form, p.placeholder))
		}
	}
	return opts
}

// recordedScenario sends a new message, reconciles its attempt, sets one
// category on the sent message and clears it again (the categories effect,
// on a message the scenario itself created, leaving the mailbox as it was),
// runs the provider state verbs on it (recordedState, which puts it back)
// and runs a delta round on the inbox and the watched folders. Times and IDs
// are fixed so recorded requests match on replay.
func recordedScenario(t *testing.T, client *http.Client, endpoint, mailbox, peer, token string, opts ...func(*Options)) {
	t.Helper()
	o := Options{Endpoint: endpoint}
	for _, fn := range opts {
		fn(&o)
	}
	data, _ := json.Marshal(Config{Mailbox: mailbox, Addresses: []string{mailbox}})
	f := NewFactory(o)
	inst, err := f.Open(t.Context(), model.Channel{
		Ref: testChan, Type: Type, CredentialRef: "recorded",
		Configuration: model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data},
	}, adapter.Dependencies{
		HTTP:        client,
		Credentials: staticToken(token),
		Clock:       portstest.NewFixedClock(time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatal(err)
	}
	eff := effect(model.ExternalRef{Channel: testChan, Namespace: IdentityNamespace, Kind: refKindAddress, ID: peer}, "00000000-0000-7000-8000-0000000000a1")
	out, err := inst.(adapter.Sender).Send(t.Context(), adapter.SendRequest{Effect: eff, Content: textContent("uma recorded scenario")})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	// The Sent Items copy lags the send by seconds on a real mailbox: a
	// recording waits for it, so the cassette holds the found answer. Found
	// and not yet (failed with a transient failure) are both documented
	// answers.
	if xrrtest.Mode(os.Getenv(xrrtest.EnvMode)) == xrrtest.ModeRecord {
		time.Sleep(15 * time.Second)
	}
	rec, err := inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: eff})
	if err != nil && model.CodeOf(err) != model.Transient {
		t.Fatalf("Reconcile = %+v, %v", rec, err)
	}
	notYet := rec.State == model.Failed && rec.Failure != nil && model.CodeOf(rec.Failure) == model.Transient
	if err == nil && rec.State != model.ProviderAccepted && !notYet {
		t.Fatalf("Reconcile = %+v", rec)
	}
	sent := out.References[0]
	categories := eff
	categories.Operation.ID = "00000000-0000-7000-8000-0000000000a2"
	categories.Target = &sent
	for _, data := range []string{`{"set":["uma recorded"]}`, `{"set":[]}`} {
		out, err := inst.(adapter.ExtensionExecutor).Execute(t.Context(), adapter.ExecuteRequest{
			Effect: categories, Action: model.Extension{Name: ExtensionCategories, Version: ExtensionVersion, Data: json.RawMessage(data)},
		})
		if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 || out.References[0] != sent {
			t.Fatalf("Execute(%s) = %+v, %v", data, out, err)
		}
	}
	recordedState(t, inst.(*instance), eff, sent)
	if _, err := inst.(*instance).round(t.Context(), &testSink{}); err != nil {
		t.Fatalf("delta round: %v", err)
	}
}

// recordedState runs the provider state verbs on the sent message and puts
// it back where it was: a baseline delta round on every watched folder; read
// and unread; flag and unflag; important and unimportant; Archive and a move by role (archive), then a
// move by ID back to the folder it came from (Graph keeps no record of it,
// so unarchive would land in the inbox); Deleted Items the same way; a
// Reconcile of the archive after the fact (a move is found in its
// destination); and a delta round that reports the changes. The message ID
// must not change across the moves (immutable IDs).
func recordedState(t *testing.T, inst *instance, eff adapter.Effect, sent model.ExternalRef) {
	t.Helper()
	ctx := t.Context()
	items := []model.ExternalRef{sent}
	if _, err := inst.round(ctx, &testSink{}); err != nil {
		t.Fatalf("baseline delta round: %v", err)
	}
	home, err := inst.stateOf(ctx, inst.c.session(), sent.ID)
	if err != nil || home.ParentFolderID == "" {
		t.Fatalf("read the sent message: %+v, %v", home, err)
	}
	eff.Operation.ID = "00000000-0000-7000-8000-0000000000a3"
	apply := func(name string, out adapter.Outcome, err error) {
		t.Helper()
		if err != nil || out.State != model.ProviderAccepted || len(out.ReferenceChanges) != 0 {
			t.Fatalf("%s = %+v, %v", name, out, err)
		}
	}
	back := func(name string) {
		t.Helper()
		out, err := inst.Move(ctx, adapter.MoveRequest{Effect: eff, Items: items, Destination: model.MoveDestination{ID: home.ParentFolderID}})
		apply(name, out, err)
	}
	out, err := inst.SetReadState(ctx, adapter.ReadStateRequest{Effect: eff, Items: items, Read: false})
	apply("unread", out, err)
	out, err = inst.SetReadState(ctx, adapter.ReadStateRequest{Effect: eff, Items: items, Read: home.IsRead})
	apply("restore read", out, err)
	out, err = inst.Flag(ctx, adapter.FlagRequest{Effect: eff, Items: items, Kind: model.FlagStar})
	apply("flag", out, err)
	out, err = inst.Flag(ctx, adapter.FlagRequest{Effect: eff, Items: items, Kind: model.FlagStar, Undo: true})
	apply("unflag", out, err)
	out, err = inst.Flag(ctx, adapter.FlagRequest{Effect: eff, Items: items, Kind: model.FlagImportant})
	apply("important", out, err)
	out, err = inst.Flag(ctx, adapter.FlagRequest{Effect: eff, Items: items, Kind: model.FlagImportant, Undo: true})
	apply("unimportant", out, err)
	out, err = inst.Archive(ctx, adapter.ArchiveRequest{Effect: eff, Items: items})
	apply("archive", out, err)
	rec, err := inst.Reconcile(ctx, adapter.ReconcileRequest{Effect: eff, Capability: model.CapabilityArchive, Items: items})
	if err != nil || rec.State != model.ProviderAccepted {
		t.Fatalf("Reconcile archive = %+v, %v", rec, err)
	}
	if _, err := inst.round(ctx, &testSink{}); err != nil {
		t.Fatalf("delta round after archive: %v", err)
	}
	back("move back from Archive")
	out, err = inst.Trash(ctx, adapter.TrashRequest{Effect: eff, Items: items})
	apply("trash", out, err)
	back("move back from Deleted Items")
	out, err = inst.Move(ctx, adapter.MoveRequest{Effect: eff, Items: items, Destination: model.MoveDestination{Role: model.RoleJunk}})
	apply("move to junk", out, err)
	back("move back from Junk Email")
}

type staticToken string

func (s staticToken) Resolve(context.Context, ports.CredentialRequest) (ports.Credential, error) {
	return ports.OAuth2{AccessToken: string(s)}, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
