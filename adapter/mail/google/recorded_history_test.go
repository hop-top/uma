package google

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/testing/xrrtest"
)

// The recorded history window is fixed, so a replay and a recording made
// later list the same messages. It is a day long before any real mail a
// test mailbox can hold, so it holds only the messages inserted for the
// recording (users.messages.insert, internalDateSource=dateHeader; see
// docs/recording/gmail.md) and nothing that must not be committed.
var (
	recordedHistorySince = time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC)
	recordedHistoryUntil = recordedHistorySince.Add(24 * time.Hour)
)

const historyCassetteDir = "testdata/cassettes/gmail-history"

// TestRecordedGmailHistory replays the history read of a real mailbox from
// testdata/cassettes/gmail-history: one slice listing per day of the window
// (users.messages.list with labelIds=INBOX and q=after:/before:),
// users.messages.get with format=minimal for the dates and format=raw for
// each envelope. Until cassettes are recorded it skips. To record, see
// docs/recording/gmail.md ("Record the history cassettes").
func TestRecordedGmailHistory(t *testing.T) {
	mode := xrrtest.Mode(os.Getenv(xrrtest.EnvMode))
	if mode == "" || mode == xrrtest.ModeReplay {
		if _, err := os.Stat(historyCassetteDir); os.IsNotExist(err) {
			t.Skip("no recorded Gmail history cassettes yet")
		}
	}
	mailbox, peer := envOr("GMAIL_ADDRESS", "mailbox@example.test"), envOr("GMAIL_PEER", "peer@example.test")
	client := recordedClient(t, historyCassetteDir, mailbox, peer)
	recordedHistoryScenario(t, client, "", mailbox, envOr("GMAIL_ACCESS_TOKEN", "replay-token"))
	if mode == xrrtest.ModeRecord || os.Getenv("GMAIL_ADDRESS") != "" {
		assertNoResidue(t, historyCassetteDir, recordedIdentities(mailbox, peer))
	}
}

// TestRecordedHistoryOverFake runs the scenario through the same xrrtest
// client in passthrough mode against the fake, so the wiring stays working
// until cassettes exist.
func TestRecordedHistoryOverFake(t *testing.T) {
	g := newFakeGmail(t)
	const mailbox, peer = "mailbox@example.test", "peer@example.test"
	g.box(mailbox, testChan)
	for n, id := range []string{"h-1", "h-2", "h-3"} {
		g.hold(mailbox, id, "thread-"+id, rawMessage(peer, mailbox, "<"+id+"@peers.test>", "hello", "text"), recordedHistorySince.Add(time.Duration(n+9)*time.Hour))
	}
	client := recordedClient(t, t.TempDir(), mailbox, peer,
		xrrtest.WithMode(xrrtest.ModePassthrough), xrrtest.WithTransport(g.srv.Client().Transport))
	got := recordedHistoryScenario(t, client, g.endpoint(), mailbox, "tok-1")
	if !slices.Equal(got, []string{"h-1", "h-2", "h-3"}) {
		t.Fatalf("history = %q", got)
	}
}

// recordedHistoryScenario reads the fixed window in pages of two, checks
// the order and shape of what the mailbox holds there, and returns the
// message IDs. It sends and changes nothing.
func recordedHistoryScenario(t *testing.T, client *http.Client, endpoint, mailbox, token string) []string {
	t.Helper()
	data, _ := json.Marshal(Config{User: mailbox, Addresses: []string{mailbox}, MessageIDDomain: recordedMessageIDDomain})
	inst, err := NewFactory(Options{Endpoint: endpoint}).Open(t.Context(), model.Channel{
		Ref: testChan, Type: Type, CredentialRef: "recorded",
		Configuration: model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data},
	}, adapter.Dependencies{HTTP: client, Credentials: staticToken(token), Clock: portstest.NewFixedClock(testNow)})
	if err != nil {
		t.Fatal(err)
	}
	hr := inst.(adapter.HistoryReader)
	var ids []string
	var last time.Time
	req := adapter.HistoryRequest{Since: recordedHistorySince, Until: recordedHistoryUntil, Limit: 2}
	for range 200 {
		p, err := hr.History(t.Context(), req)
		if err != nil {
			t.Fatalf("History(cursor %q): %v", req.Cursor, err)
		}
		for _, env := range p.Envelopes {
			if env.Kind != model.EnvelopeMessage && env.Kind != model.EnvelopeAutomatedMessage || env.MessageID == "" || env.Destination == "" {
				t.Fatalf("envelope %+v is not a received message with an ID and destination", env)
			}
			if env.OccurredAt.Before(recordedHistorySince) || !env.OccurredAt.Before(recordedHistoryUntil) || env.OccurredAt.Before(last) {
				t.Fatalf("envelope %q at %v: outside [%v, %v) or before %v", env.MessageID, env.OccurredAt, recordedHistorySince, recordedHistoryUntil, last)
			}
			last = env.OccurredAt
			ids = append(ids, env.MessageID)
		}
		if p.Next == "" {
			return ids
		}
		req.Cursor = p.Next
	}
	t.Fatal("window not read")
	return nil
}
