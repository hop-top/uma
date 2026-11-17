package google

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/testing/xrrtest"
)

// Recorded exchanges of the reply draft effect replay from
// testdata/cassettes/gmail-reply-draft, a scenario of its own so the
// cassettes of the main scenario stay valid. Until cassettes are recorded
// the replay test skips. To record, see docs/recording/gmail.md ("Record
// the reply draft cassettes").
const draftCassetteDir = "testdata/cassettes/gmail-reply-draft"

func TestRecordedGmailReplyDraft(t *testing.T) {
	mode := xrrtest.Mode(os.Getenv(xrrtest.EnvMode))
	if mode == "" || mode == xrrtest.ModeReplay {
		if _, err := os.Stat(draftCassetteDir); os.IsNotExist(err) {
			t.Skip("no recorded Gmail reply draft cassettes yet")
		}
	}
	mailbox, peer := envOr("GMAIL_ADDRESS", "mailbox@example.test"), envOr("GMAIL_PEER", "peer@example.test")
	client := recordedClient(t, draftCassetteDir, mailbox, peer)
	recordedReplyDraftScenario(t, client, "", mailbox, peer, envOr("GMAIL_ACCESS_TOKEN", "replay-token"))
	if mode == xrrtest.ModeRecord || os.Getenv("GMAIL_ADDRESS") != "" {
		assertNoResidue(t, draftCassetteDir, recordedIdentities(mailbox, peer))
	}
}

// TestRecordedReplyDraftOverFake runs the scenario through the same xrrtest
// client in passthrough mode against the fake, so the wiring stays working
// until cassettes exist.
func TestRecordedReplyDraftOverFake(t *testing.T) {
	g := newFakeGmail(t)
	const mailbox, peer = "mailbox@example.test", "peer@example.test"
	g.box(mailbox, testChan)
	client := recordedClient(t, t.TempDir(), mailbox, peer,
		xrrtest.WithMode(xrrtest.ModePassthrough), xrrtest.WithTransport(g.srv.Client().Transport))
	recordedReplyDraftScenario(t, client, g.endpoint(), mailbox, peer, "tok-1")
	base := "/gmail/v1/users/" + mailbox + "/drafts"
	if g.count("POST "+base) != 1 || g.count("PUT "+base+"/r-1") != 1 || g.count("DELETE "+base+"/r-1") != 1 {
		t.Fatalf("scenario requests %q", g.requests())
	}
}

// recordedReplyDraftScenario sends one message (the parent to reply to),
// saves a reply to it as a draft, finds the draft again by its
// Message-ID, replaces its content, deletes it and deletes it once more
// (the recorded 404 of a draft already gone). The mailbox ends with the
// sent message only, as the main scenario leaves it.
func recordedReplyDraftScenario(t *testing.T, client *http.Client, endpoint, mailbox, peer, token string) {
	t.Helper()
	data, _ := json.Marshal(Config{User: mailbox, Addresses: []string{mailbox}, Subject: "uma recorded reply draft", MessageIDDomain: recordedMessageIDDomain})
	inst, err := NewFactory(Options{Endpoint: endpoint}).Open(t.Context(), model.Channel{
		Ref: testChan, Type: Type, CredentialRef: "recorded",
		Configuration: model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data},
	}, adapter.Dependencies{HTTP: client, Credentials: staticToken(token), Clock: portstest.NewFixedClock(recordedNow)})
	if err != nil {
		t.Fatal(err)
	}
	peerRoute := model.ExternalRef{Channel: testChan, Namespace: IdentityNamespace, Kind: refKindAddress, ID: peer}
	out, err := inst.(adapter.Sender).Send(t.Context(), adapter.SendRequest{
		Effect: effect(peerRoute, "00000000-0000-7000-8000-0000000000b1"), Content: textContent("uma recorded reply draft parent"),
	})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	peerRoute.ParentID = out.References[0].ID
	run := func(op, data string) (adapter.Effect, adapter.Outcome) {
		t.Helper()
		eff := effect(peerRoute, "00000000-0000-7000-8000-0000000000b1")
		eff.Operation.ID = model.ID(op)
		out, err := inst.(adapter.ExtensionExecutor).Execute(t.Context(), adapter.ExecuteRequest{
			Effect: eff, Action: model.Extension{Name: ExtensionReplyDraft, Version: ExtensionVersion, Data: json.RawMessage(data)},
		})
		if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 || out.References[0].Kind != RefKindDraft {
			t.Fatalf("Execute(%s) = %+v, %v", data, out, err)
		}
		return eff, out
	}
	created, out := run("00000000-0000-7000-8000-0000000000b2", `{"text":"uma recorded reply draft"}`)
	draft := out.References[0]
	// Search indexing may lag the creation: found or not yet are both answers.
	rec, err := inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{
		Effect: created, Capability: model.CapabilityExtensionExecute,
		Action: model.Extension{Name: ExtensionReplyDraft, Version: ExtensionVersion, Data: json.RawMessage(`{"text":"uma recorded reply draft"}`)},
	})
	if err != nil || (rec.State != model.ProviderAccepted && rec.State != model.Failed) ||
		(rec.State == model.ProviderAccepted && !slices.Equal(rec.References, []model.ExternalRef{draft})) {
		t.Fatalf("Reconcile = %+v, %v", rec, err)
	}
	_, out = run("00000000-0000-7000-8000-0000000000b3", `{"text":"uma recorded reply draft, edited","draft":"`+draft.ID+`"}`)
	if out.References[0] != draft {
		t.Fatalf("update named %+v, want the same draft %+v", out.References[0], draft)
	}
	del := `{"draft":"` + draft.ID + `","delete":true}`
	run("00000000-0000-7000-8000-0000000000b4", del)
	run("00000000-0000-7000-8000-0000000000b5", del)
}
