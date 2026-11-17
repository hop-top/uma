package google

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

// The recorded unsubscribe window is a fixed day long before any real mail a
// test mailbox can hold, so it holds only the one message inserted for the
// recording (users.messages.insert, internalDateSource=dateHeader; see
// docs/recording/gmail.md, "Record the unsubscribe cassettes").
var (
	recordedUnsubscribeSince = time.Date(2001, time.January, 4, 0, 0, 0, 0, time.UTC)
	recordedUnsubscribeUntil = recordedUnsubscribeSince.Add(24 * time.Hour)
)

const unsubscribeCassetteDir = "testdata/cassettes/gmail-unsubscribe"

// TestRecordedGmailUnsubscribe replays, from testdata/cassettes/gmail-unsubscribe,
// the mailto unsubscribe of a real mailbox: the history read that finds the
// inserted message, users.messages.get with format=raw for the effect, and
// users.messages.send of the unsubscribe message to the peer (the message's
// List-Unsubscribe is a mailto to the peer itself, so the send reaches an
// address the owner controls). The one-click method is not recorded: it
// needs a real sender's DKIM-signed message and a request to that sender's
// endpoint, and a replay cannot serve the DNS lookups of the verification.
// Until cassettes are recorded it skips. To record, see
// docs/recording/gmail.md ("Record the unsubscribe cassettes").
func TestRecordedGmailUnsubscribe(t *testing.T) {
	mode := xrrtest.Mode(os.Getenv(xrrtest.EnvMode))
	if mode == "" || mode == xrrtest.ModeReplay {
		if _, err := os.Stat(unsubscribeCassetteDir); os.IsNotExist(err) {
			t.Skip("no recorded Gmail unsubscribe cassettes yet")
		}
	}
	mailbox, peer := envOr("GMAIL_ADDRESS", "mailbox@example.test"), envOr("GMAIL_PEER", "peer@example.test")
	client := recordedClient(t, unsubscribeCassetteDir, mailbox, peer)
	recordedUnsubscribeScenario(t, client, "", mailbox, peer, envOr("GMAIL_ACCESS_TOKEN", "replay-token"))
	if mode == xrrtest.ModeRecord || os.Getenv("GMAIL_ADDRESS") != "" {
		assertNoResidue(t, unsubscribeCassetteDir, recordedIdentities(mailbox, peer))
	}
}

// TestRecordedUnsubscribeOverFake runs the scenario through the same xrrtest
// client in passthrough mode against the fake, so the wiring and the
// scenario stay working until cassettes exist.
func TestRecordedUnsubscribeOverFake(t *testing.T) {
	g := newFakeGmail(t)
	const mailbox, peer = "mailbox@example.test", "peer@example.test"
	box := g.box(mailbox, testChan)
	g.hold(mailbox, "u-1", "thread-u1", rawMessage(peer, mailbox, "<u-1@peers.test>", "weekly", "text", "List-Unsubscribe: <mailto:"+peer+"?subject=unsubscribe>"),
		recordedUnsubscribeSince.Add(10*time.Hour))
	client := recordedClient(t, t.TempDir(), mailbox, peer,
		xrrtest.WithMode(xrrtest.ModePassthrough), xrrtest.WithTransport(g.srv.Client().Transport))
	recordedUnsubscribeScenario(t, client, g.endpoint(), mailbox, peer, "tok-1")
	g.mu.Lock()
	sends := box.sends
	g.mu.Unlock()
	if len(sends) != 1 {
		t.Fatalf("sends = %d, want the one mailto", len(sends))
	}
}

// recordedUnsubscribeScenario finds the one message of the window with the
// history reader and unsubscribes from it by mailto. The message's sender is
// from, the route of the effect.
func recordedUnsubscribeScenario(t *testing.T, client *http.Client, endpoint, mailbox, from, token string) {
	t.Helper()
	data, _ := json.Marshal(Config{User: mailbox, Addresses: []string{mailbox}, MessageIDDomain: recordedMessageIDDomain})
	inst, err := NewFactory(Options{Endpoint: endpoint}).Open(t.Context(), model.Channel{
		Ref: testChan, Type: Type, CredentialRef: "recorded",
		Configuration: model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data},
	}, adapter.Dependencies{HTTP: client, Credentials: staticToken(token), Clock: portstest.NewFixedClock(testNow)})
	if err != nil {
		t.Fatal(err)
	}
	p, err := inst.(adapter.HistoryReader).History(t.Context(), adapter.HistoryRequest{Since: recordedUnsubscribeSince, Until: recordedUnsubscribeUntil, Limit: 5})
	if err != nil || len(p.Envelopes) != 1 {
		t.Fatalf("History = %d envelopes, %v; the window must hold exactly the one inserted message", len(p.Envelopes), err)
	}
	id := p.Envelopes[0].MessageID
	out, err := inst.(adapter.ExtensionExecutor).Execute(t.Context(), adapter.ExecuteRequest{
		Effect: effect(route(from, id), "a1"),
		Action: model.Extension{Name: ExtensionUnsubscribe, Version: ExtensionVersion, Data: json.RawMessage(`{"method":"mailto"}`)},
	})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Execute = %+v, %v", out, err)
	}
}
