package microsoft

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/testing/xrrtest"
)

// Recorded exchanges of the reply draft effect replay from
// testdata/cassettes/graph-reply-draft, a scenario of its own so the
// cassettes of the main scenario stay valid. Until cassettes are recorded
// the replay test skips. To record, see docs/recording/microsoft-graph.md
// ("Record the reply draft cassettes").
const draftCassetteDir = "testdata/cassettes/graph-reply-draft"

func TestRecordedGraphReplyDraft(t *testing.T) {
	mode := xrrtest.Mode(os.Getenv(xrrtest.EnvMode))
	if mode == "" || mode == xrrtest.ModeReplay {
		if _, err := os.Stat(draftCassetteDir); os.IsNotExist(err) {
			t.Skip("no recorded Graph reply draft cassettes yet")
		}
	}
	mailbox, peer := envOr("MS_GRAPH_MAILBOX", "mailbox@example.test"), envOr("MS_GRAPH_PEER", "peer@example.test")
	client := xrrtest.NewClient(t, draftCassetteDir, recordingOptions(mailbox, peer)...)
	token := envOr("MS_GRAPH_ACCESS_TOKEN", "replay-token")
	recordedReplyDraftScenario(t, client, "", mailbox, peer, token)
	if mode == xrrtest.ModeRecord {
		assertNoResidue(t, draftCassetteDir, mailbox, peer, token)
	}
}

// TestRecordedReplyDraftOverFake runs the scenario through the same xrrtest
// client in passthrough mode against the fake, so the wiring stays working
// until cassettes exist.
func TestRecordedReplyDraftOverFake(t *testing.T) {
	g := newFakeGraph(t)
	const mailbox, peer = "mailbox@example.test", "peer@example.test"
	g.mailbox(mailbox, testChan)
	opts := append(recordingOptions(mailbox, peer),
		xrrtest.WithMode(xrrtest.ModePassthrough), xrrtest.WithTransport(g.srv.Client().Transport))
	recordedReplyDraftScenario(t, xrrtest.NewClient(t, t.TempDir(), opts...), g.endpoint(), mailbox, peer, "tok-1")
	base := "/v1.0/users/" + mailbox + "/messages/"
	if g.count("POST "+base+"draft-1/createReply") != 1 || g.count("PATCH "+base+"draft-2") != 2 || g.count("DELETE "+base+"draft-2") != 1 {
		t.Fatalf("scenario requests %q", g.requests())
	}
}

// recordedReplyDraftScenario sends one message (the parent to reply to),
// saves a reply to it as a draft (createReply then PATCH), replaces the
// draft's body, deletes the draft and deletes it once more (the recorded
// 404 of a draft already gone). The mailbox ends with the sent message
// only, as the main scenario leaves it.
func recordedReplyDraftScenario(t *testing.T, client *http.Client, endpoint, mailbox, peer, token string) {
	t.Helper()
	data, _ := json.Marshal(Config{Mailbox: mailbox, Addresses: []string{mailbox}})
	inst, err := NewFactory(Options{Endpoint: endpoint}).Open(t.Context(), model.Channel{
		Ref: testChan, Type: Type, CredentialRef: "recorded",
		Configuration: model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data},
	}, adapter.Dependencies{
		HTTP: client, Credentials: staticToken(token), Clock: portstest.NewFixedClock(time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)),
	})
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
	run := func(op, data string) adapter.Outcome {
		t.Helper()
		eff := effect(peerRoute, "00000000-0000-7000-8000-0000000000b1")
		eff.Operation.ID = model.ID(op)
		out, err := inst.(adapter.ExtensionExecutor).Execute(t.Context(), adapter.ExecuteRequest{
			Effect: eff, Action: model.Extension{Name: ExtensionReplyDraft, Version: ExtensionVersion, Data: json.RawMessage(data)},
		})
		if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 || out.References[0].Kind != RefKindDraft {
			t.Fatalf("Execute(%s) = %+v, %v", data, out, err)
		}
		return out
	}
	draft := run("00000000-0000-7000-8000-0000000000b2", `{"text":"uma recorded reply draft"}`).References[0]
	if got := run("00000000-0000-7000-8000-0000000000b3", `{"text":"uma recorded reply draft, edited","draft":"`+draft.ID+`"}`).References[0]; got != draft {
		t.Fatalf("update named %+v, want the same draft %+v", got, draft)
	}
	del := `{"draft":"` + draft.ID + `","delete":true}`
	run("00000000-0000-7000-8000-0000000000b4", del)
	run("00000000-0000-7000-8000-0000000000b5", del)
}
