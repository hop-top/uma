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

// Recorded unsubscribe exchanges replay from
// testdata/cassettes/graph-unsubscribe. The default window is a fixed day
// long before any real mail a mailbox holds; a recording may name another
// with MS_GRAPH_UNSUB_SINCE and MS_GRAPH_UNSUB_UNTIL (RFC 3339), because Graph
// accepts an Internet header such as List-Unsubscribe only on a message
// created from MIME, which may not be dated into 2001 (see
// docs/recording/microsoft-graph.md, "Record the unsubscribe cassettes"). The
// window is part of every recorded request: after recording, set these
// defaults to the recorded window.
var (
	unsubscribeSince = time.Date(2001, time.January, 4, 0, 0, 0, 0, time.UTC)
	unsubscribeUntil = unsubscribeSince.Add(24 * time.Hour)
)

const unsubscribeCassetteDir = "testdata/cassettes/graph-unsubscribe"

// TestRecordedGraphUnsubscribe replays the mailto unsubscribe of a real
// mailbox: the history read that finds the message, the MIME read
// (GET .../messages/{id}/$value) of the effect, and the draft and send of
// the unsubscribe message to the peer (the message's List-Unsubscribe is a
// mailto to the peer itself, so the send reaches an address the owner
// controls). The one-click method is not recorded: it needs a real sender's
// DKIM-signed message and a request to that sender's endpoint, and a replay
// cannot serve the DNS lookups of the verification. Until cassettes are
// recorded it skips.
func TestRecordedGraphUnsubscribe(t *testing.T) {
	mode := xrrtest.Mode(os.Getenv(xrrtest.EnvMode))
	if mode == "" || mode == xrrtest.ModeReplay {
		if _, err := os.Stat(unsubscribeCassetteDir); os.IsNotExist(err) {
			t.Skip("no recorded Graph unsubscribe cassettes yet")
		}
	}
	mailbox, peer := envOr("MS_GRAPH_MAILBOX", "mailbox@example.test"), envOr("MS_GRAPH_PEER", "peer@example.test")
	token := envOr("MS_GRAPH_ACCESS_TOKEN", "replay-token")
	client := xrrtest.NewClient(t, unsubscribeCassetteDir, recordingOptions(mailbox, peer)...)
	since, until := envTime(t, "MS_GRAPH_UNSUB_SINCE", unsubscribeSince), envTime(t, "MS_GRAPH_UNSUB_UNTIL", unsubscribeUntil)
	recordedUnsubscribe(t, client, "", mailbox, peer, token, since, until)
	if mode == xrrtest.ModeRecord {
		assertNoResidue(t, unsubscribeCassetteDir, mailbox, peer, token)
	}
}

// TestRecordedUnsubscribeOverFake runs the scenario through the same xrrtest
// client in passthrough mode against the fake, so the wiring and the
// scenario stay working until cassettes exist.
func TestRecordedUnsubscribeOverFake(t *testing.T) {
	g := newFakeGraph(t)
	const mailbox, peer = "mailbox@example.test", "peer@example.test"
	box := g.mailbox(mailbox, testChan)
	g.open(box.name, gMessage{
		ID: "u1", InternetMessageID: "<u1@peer.test>", Subject: "weekly", ReceivedDateTime: unsubscribeSince.Add(10 * time.Hour),
		From:         &gRecipient{EmailAddress: gEmailAddress{Address: peer}},
		ToRecipients: []gRecipient{{EmailAddress: gEmailAddress{Address: mailbox}}},
		Body:         &gItemBody{ContentType: "text", Content: "weekly"},
	})
	g.mu.Lock()
	g.mimes = map[string][]byte{"u1": []byte("From: " + peer + "\r\nTo: " + mailbox + "\r\nSubject: weekly\r\nDate: Thu, 04 Jan 2001 10:00:00 +0000\r\n" +
		"Message-ID: <u1@peer.test>\r\nList-Unsubscribe: <mailto:" + peer + "?subject=unsubscribe>\r\n\r\nweekly\r\n")}
	g.mu.Unlock()
	opts := append(recordingOptions(mailbox, peer),
		xrrtest.WithMode(xrrtest.ModePassthrough), xrrtest.WithTransport(g.srv.Client().Transport))
	recordedUnsubscribe(t, xrrtest.NewClient(t, t.TempDir(), opts...), g.endpoint(), mailbox, peer, "tok-1", unsubscribeSince, unsubscribeUntil)
	if n := g.count("POST /v1.0/users/" + mailbox + "/messages/draft-1/send"); n != 1 {
		t.Fatalf("sends = %d, want the one mailto: %q", n, g.requests())
	}
}

// recordedUnsubscribe finds the one message of the window with the history
// reader and unsubscribes from it by mailto; the peer is its sender.
func recordedUnsubscribe(t *testing.T, client *http.Client, endpoint, mailbox, peer, token string, since, until time.Time) {
	t.Helper()
	ids := recordedHistory(t, client, endpoint, mailbox, since, until, token)
	if len(ids) != 1 {
		t.Fatalf("the window holds %d messages, want exactly the one created for the recording", len(ids))
	}
	data, _ := json.Marshal(Config{Mailbox: mailbox, Addresses: []string{mailbox}})
	inst, err := NewFactory(Options{Endpoint: endpoint}).Open(t.Context(), model.Channel{
		Ref: testChan, Type: Type, CredentialRef: "recorded",
		Configuration: model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data},
	}, adapter.Dependencies{HTTP: client, Credentials: staticToken(token), Clock: portstest.NewFixedClock(time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC))})
	if err != nil {
		t.Fatal(err)
	}
	out, err := inst.(adapter.ExtensionExecutor).Execute(t.Context(), adapter.ExecuteRequest{
		Effect: effect(route(peer, ids[0]), "a1"),
		Action: model.Extension{Name: ExtensionUnsubscribe, Version: ExtensionVersion, Data: json.RawMessage(`{"method":"mailto"}`)},
	})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Execute = %+v, %v", out, err)
	}
}
