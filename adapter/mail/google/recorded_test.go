package google

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/testing/xrrtest"
)

// Recorded Gmail API exchanges replay from testdata/cassettes/gmail. Until
// cassettes are recorded the replay test skips. To record against a test
// mailbox:
//
//	GMAIL_ACCESS_TOKEN=<token> GMAIL_ADDRESS=<address> GMAIL_PEER=<address> \
//	  XRR_MODE=record go test -run TestRecordedGmail ./...
//
// The token needs the gmail.readonly, gmail.send and gmail.modify scopes
// (the state verbs need gmail.modify as well), and gmail.insert for the
// messages of the fixed 2001 window (see docs/recording/gmail.md, "Seed the
// sync window").
// The mailbox and peer addresses become placeholders, inside the base64url
// "raw" messages too (see recorded_scrub_test.go); the token never reaches a
// cassette. After a recording the test fails if either address, or a
// non-public domain of them, is still readable in a cassette.
const cassetteDir = "testdata/cassettes/gmail"

func TestRecordedGmail(t *testing.T) {
	mode := xrrtest.Mode(os.Getenv(xrrtest.EnvMode))
	if mode == "" || mode == xrrtest.ModeReplay {
		if _, err := os.Stat(cassetteDir); os.IsNotExist(err) {
			t.Skip("no recorded Gmail cassettes yet")
		}
	}
	mailbox, peer := envOr("GMAIL_ADDRESS", "mailbox@example.test"), envOr("GMAIL_PEER", "peer@example.test")
	client := recordedClient(t, cassetteDir, mailbox, peer)
	recordedScenario(t, client, "", mailbox, peer, envOr("GMAIL_ACCESS_TOKEN", "replay-token"))
	if mode == xrrtest.ModeRecord || os.Getenv("GMAIL_ADDRESS") != "" {
		// A recording just made, or committed cassettes checked against
		// the real addresses.
		assertNoResidue(t, cassetteDir, recordedIdentities(mailbox, peer))
	}
}

// TestRecordedScenarioOverFake runs the recorded scenario through the same
// xrrtest client in passthrough mode against the fake, so the wiring and
// the scenario stay working until cassettes exist.
func TestRecordedScenarioOverFake(t *testing.T) {
	g := newFakeGmail(t)
	const mailbox, peer = "mailbox@example.test", "peer@example.test"
	g.box(mailbox, testChan).minHistory = 1 // the resync's stored historyId is gone
	g.hold(mailbox, "w-1", "thread-w1", rawMessage(peer, mailbox, "<w-1@peers.test>", "hello", "text"), recordedNow.Add(-30*time.Minute))
	client := recordedClient(t, t.TempDir(), mailbox, peer,
		xrrtest.WithMode(xrrtest.ModePassthrough), xrrtest.WithTransport(g.srv.Client().Transport))
	recordedScenario(t, client, g.endpoint(), mailbox, peer, "tok-1")
	if g.count("POST /gmail/v1/users/"+mailbox+"/messages/send") != 1 || g.count("POST /gmail/v1/users/"+mailbox+"/messages/sent-1/modify") != 2 {
		t.Fatalf("scenario requests %q", g.requests())
	}
	// The state verbs ran on the sent message's thread, as thread calls.
	wantOps := []string{"modify thread-s1", "trash thread-s1", "untrash thread-s1"}
	for _, op := range wantOps {
		if !slices.Contains(g.box(mailbox, testChan).threadOps, op) {
			t.Fatalf("thread requests %q lack %q", g.box(mailbox, testChan).threadOps, op)
		}
	}
	if n := g.count("POST /gmail/v1/users/" + mailbox + "/messages/sent-1/modify"); n != 2 {
		t.Fatalf("the state verbs modified the message itself: %d message modifies, want the 2 of the labels effect", n)
	}
	// The resync read the window again.
	if n := g.count("GET /gmail/v1/users/" + mailbox + "/profile"); n != 2 {
		t.Fatalf("profile reads = %d, want 2 (first round and resync)", n)
	}
	// The first sync is bounded above by the fixed clock.
	window := "q=after:" + itoa(recordedNow.Add(-time.Hour).Unix()) + " before:" + itoa(recordedSyncUntil.Unix())
	if !slices.ContainsFunc(g.requests(), func(r string) bool {
		return strings.HasPrefix(r, "GET /gmail/v1/users/"+mailbox+"/messages?labelIds=INBOX") && strings.HasSuffix(r, window)
	}) {
		t.Fatalf("inbox sync is not bounded by %q: %q", window, g.requests())
	}
}

// recordedClient is the client of the recorded scenario: the xrrtest
// transport with the placeholders and the scrub hook, under the upper bound
// of the first sync and the SENT scope of the history read. opts follow the scenario's own.
func recordedClient(tb testing.TB, dir, mailbox, peer string, opts ...xrrtest.Option) *http.Client {
	tb.Helper()
	all := append([]xrrtest.Option{
		xrrtest.WithPlaceholder(mailbox, placeholderMailbox),
		xrrtest.WithPlaceholder(peer, placeholderPeer),
		xrrtest.WithScrub(newRawScrubber(recordedIdentities(mailbox, peer)).scrub),
	}, opts...)
	return &http.Client{Transport: syncBound{next: historyScope{next: xrrtest.NewTransport(tb, dir, all...)}, until: recordedSyncUntil}}
}

// recordedScenario sends a new message, reconciles its operation, stars
// the sent message and unstars it again (the labels effect, on a message
// the scenario itself created, leaving the mailbox as it was), runs one
// receiver round (a full sync of the hour before the fixed clock), then
// puts the sent message's thread through every conversation state verb
// and its undo, reads the thread back as a reconcile does, refuses a
// message Gmail does not hold, and runs a second receiver round, which
// reads the history of those changes as conversation.state envelopes, and a
// third round that finds its stored historyId gone and resynchronizes. Each
// verb pair leaves the thread as it was. Times, IDs and the Message-ID
// domain are fixed so recorded requests match on replay. It returns what
// the receiver rounds handed over.
func recordedScenario(t *testing.T, client *http.Client, endpoint, mailbox, peer, token string) *testSink {
	t.Helper()
	data, _ := json.Marshal(Config{User: mailbox, Addresses: []string{mailbox}, Subject: "uma recorded scenario", MessageIDDomain: recordedMessageIDDomain})
	f := NewFactory(Options{Endpoint: endpoint})
	inst, err := f.Open(t.Context(), model.Channel{
		Ref: testChan, Type: Type, CredentialRef: "recorded",
		Configuration: model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data},
	}, adapter.Dependencies{
		HTTP:        client,
		Credentials: staticToken(token),
		Clock:       portstest.NewFixedClock(recordedNow),
	})
	if err != nil {
		t.Fatal(err)
	}
	eff := effect(model.ExternalRef{Channel: testChan, Namespace: IdentityNamespace, Kind: refKindAddress, ID: peer}, "00000000-0000-7000-8000-0000000000a1")
	out, err := inst.(adapter.Sender).Send(t.Context(), adapter.SendRequest{Effect: eff, Content: textContent("uma recorded scenario")})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	// Search indexing may lag the send: found or not yet are both answers.
	rec, err := inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: eff})
	if err != nil || (rec.State != model.ProviderAccepted && rec.State != model.Failed) {
		t.Fatalf("Reconcile = %+v, %v", rec, err)
	}
	sent := out.References[0]
	labels := eff
	labels.Operation.ID = "00000000-0000-7000-8000-0000000000a2"
	labels.Target = &sent
	for _, data := range []string{`{"add":["STARRED"]}`, `{"remove":["STARRED"]}`} {
		out, err := inst.(adapter.ExtensionExecutor).Execute(t.Context(), adapter.ExecuteRequest{
			Effect: labels, Action: model.Extension{Name: ExtensionLabels, Version: ExtensionVersion, Data: json.RawMessage(data)},
		})
		if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 || out.References[0] != sent {
			t.Fatalf("Execute(%s) = %+v, %v", data, out, err)
		}
	}
	sink := &testSink{}
	if _, err := inst.(*instance).round(t.Context(), sink); err != nil {
		t.Fatalf("receiver round: %v", err)
	}
	recordedState(t, inst, eff, sent)
	if _, err := inst.(*instance).round(t.Context(), sink); err != nil {
		t.Fatalf("state round: %v", err)
	}
	states := sink.states()
	if len(states) == 0 {
		t.Fatalf("the state round handed over no conversation.state envelope: the history of the verbs names the thread")
	}
	if last := states[len(states)-1]; last.State.Conversation != sent {
		t.Fatalf("last state report names %+v, want the sent message %+v: the thread's newest message", last.State.Conversation, sent)
	}
	recordedResync(t, inst, sink)
	return sink
}

// recordedResync stores a historyId Gmail no longer holds (1: no mailbox
// keeps history that old) and runs a round: history.list answers 404, and the
// receiver recovers with a full synchronization, which hands over the window's
// messages again and counts one resync.
func recordedResync(t *testing.T, inst adapter.Instance, sink *testSink) {
	t.Helper()
	in := inst.(*instance)
	handed := len(sink.ids())
	if err := in.saveState(t.Context(), receiverState{HistoryID: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := in.round(t.Context(), sink); err != nil {
		t.Fatalf("resync round: %v", err)
	}
	st, err := in.loadState(t.Context())
	if err != nil || st.FullResyncs != 1 || st.HistoryID <= 1 {
		t.Fatalf("state after the resync = %+v, %v; want one counted resync from the profile historyId", st, err)
	}
	ids := sink.ids()
	if again := ids[handed:]; len(again) == 0 || !slices.Equal(again, ids[:len(again)]) {
		t.Fatalf("resync handed over %q, want the window's messages again (first round: %q)", again, ids[:min(handed, len(ids))])
	}
}

// recordedState runs the state verbs on the thread of message sent. Each
// verb is paired with its undo in the order that leaves the sent message's
// thread as a send leaves it (SENT, read, out of the inbox, not starred, not
// in the trash), and the move adds and removes a system category label, so no
// label is created.
func recordedState(t *testing.T, inst adapter.Instance, eff adapter.Effect, sent model.ExternalRef) {
	t.Helper()
	eff.Operation.ID = "00000000-0000-7000-8000-0000000000a3"
	items := []model.ExternalRef{sent}
	step := func(name string, out adapter.Outcome, err error) {
		t.Helper()
		if err != nil || out.State != model.ProviderAccepted || len(out.ReferenceChanges) != 0 {
			t.Fatalf("%s = %+v, %v; want provider_accepted without a reference change", name, out, err)
		}
	}
	a, m, r, f, tr := inst.(adapter.Archiver), inst.(adapter.Mover), inst.(adapter.ReadStateSetter), inst.(adapter.Flagger), inst.(adapter.Trasher)
	ctx := t.Context()
	out, err := a.Archive(ctx, adapter.ArchiveRequest{Effect: eff, Items: items, Undo: true})
	step("unarchive", out, err)
	out, err = a.Archive(ctx, adapter.ArchiveRequest{Effect: eff, Items: items})
	step("archive", out, err)
	out, err = r.SetReadState(ctx, adapter.ReadStateRequest{Effect: eff, Items: items})
	step("unread", out, err)
	out, err = r.SetReadState(ctx, adapter.ReadStateRequest{Effect: eff, Items: items, Read: true})
	step("read", out, err)
	for _, kind := range []string{model.FlagStar, model.FlagImportant} {
		out, err = f.Flag(ctx, adapter.FlagRequest{Effect: eff, Items: items, Kind: kind})
		step("flag "+kind, out, err)
		out, err = f.Flag(ctx, adapter.FlagRequest{Effect: eff, Items: items, Kind: kind, Undo: true})
		step("unflag "+kind, out, err)
	}
	out, err = m.Move(ctx, adapter.MoveRequest{Effect: eff, Items: items, Add: []string{"CATEGORY_UPDATES"}})
	step("move add", out, err)
	out, err = m.Move(ctx, adapter.MoveRequest{Effect: eff, Items: items, Remove: []string{"CATEGORY_UPDATES"}})
	step("move remove", out, err)
	out, err = tr.Trash(ctx, adapter.TrashRequest{Effect: eff, Items: items})
	step("trash", out, err)
	out, err = tr.Trash(ctx, adapter.TrashRequest{Effect: eff, Items: items, Undo: true})
	step("untrash", out, err)
	// Reconcile reads the thread back: the sent message's thread is not in
	// the trash, so an untrash is applied.
	rec, err := inst.(adapter.Reconciler).Reconcile(ctx, adapter.ReconcileRequest{Effect: eff, Capability: model.CapabilityTrash, Items: items, Undo: true})
	if err != nil || rec.State != model.ProviderAccepted {
		t.Fatalf("Reconcile(untrash) = %+v, %v; want applied", rec, err)
	}
	// A message Gmail does not hold is a final refusal.
	gone := []model.ExternalRef{model.ProviderMessageRef(sent.Channel, "0000000000000000")}
	out, err = a.Archive(ctx, adapter.ArchiveRequest{Effect: eff, Items: gone})
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.NotFound {
		t.Fatalf("Archive(gone) = %+v, %v; want rejected not_found", out, err)
	}
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
