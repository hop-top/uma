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

// Recorded history reads replay from testdata/cassettes/graph-history. Until
// cassettes are recorded the replay test skips. To record against a sandbox
// mailbox whose inbox holds a few messages between the two times:
//
//	MS_GRAPH_ACCESS_TOKEN=<token> MS_GRAPH_MAILBOX=<upn> MS_GRAPH_PEER=<address> \
//	  MS_GRAPH_HISTORY_SINCE=<rfc3339> MS_GRAPH_HISTORY_UNTIL=<rfc3339> \
//	  XRR_MODE=record go test -run TestRecordedGraphHistory ./...
//
// The window is part of every recorded request, so the replay uses the
// recorded one (the defaults below) and the record sets its own. Addresses
// are scrubbed as in the Graph recording; the token never reaches a
// cassette. The recording confirmed that a list of the inbox accepts
// "receivedDateTime lt" together with "$orderby receivedDateTime asc", and
// that "internetMessageHeaders" can be selected in a list.
const historyCassetteDir = "testdata/cassettes/graph-history"

var (
	// A day before any real mail a mailbox holds: the recording mailbox
	// holds only the messages created there for it (see
	// docs/recording/microsoft-graph.md).
	historySince = time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC)
	historyUntil = historySince.Add(24 * time.Hour)
)

func TestRecordedGraphHistory(t *testing.T) {
	mode := xrrtest.Mode(os.Getenv(xrrtest.EnvMode))
	if mode == "" || mode == xrrtest.ModeReplay {
		if _, err := os.Stat(historyCassetteDir); os.IsNotExist(err) {
			t.Skip("no recorded Graph history cassettes yet")
		}
	}
	mailbox, peer := envOr("MS_GRAPH_MAILBOX", "mailbox@example.test"), envOr("MS_GRAPH_PEER", "peer@example.test")
	since, until := envTime(t, "MS_GRAPH_HISTORY_SINCE", historySince), envTime(t, "MS_GRAPH_HISTORY_UNTIL", historyUntil)
	client := xrrtest.NewClient(t, historyCassetteDir, recordingOptions(mailbox, peer)...)
	token := envOr("MS_GRAPH_ACCESS_TOKEN", "replay-token")
	recordedHistory(t, client, "", mailbox, since, until, token)
	if mode == xrrtest.ModeRecord {
		assertNoResidue(t, historyCassetteDir, mailbox, peer, token)
	}
}

// TestRecordedHistoryOverFake runs the recorded history scenario through
// the same xrrtest client in passthrough mode against the fake, so the
// wiring and the scenario stay working until cassettes exist.
func TestRecordedHistoryOverFake(t *testing.T) {
	g := newFakeGraph(t)
	const mailbox, peer = "mailbox@example.test", "peer@example.test"
	box := g.mailbox(mailbox, testChan)
	for i, id := range []string{"h1", "h2", "h3", "h4"} {
		g.open(box.name, gMessage{
			ID: id, InternetMessageID: "<" + id + "@peer.test>", Subject: id, ReceivedDateTime: historySince.Add(time.Duration(i+1) * time.Hour),
			From:         &gRecipient{EmailAddress: gEmailAddress{Address: peer}},
			ToRecipients: []gRecipient{{EmailAddress: gEmailAddress{Address: mailbox}}},
			Body:         &gItemBody{ContentType: "text", Content: id},
		})
	}
	opts := append(recordingOptions(mailbox, peer),
		xrrtest.WithMode(xrrtest.ModePassthrough), xrrtest.WithTransport(g.srv.Client().Transport))
	got := recordedHistory(t, xrrtest.NewClient(t, t.TempDir(), opts...), g.endpoint(), mailbox, historySince, historyUntil, "tok-1")
	if len(got) != 4 {
		t.Fatalf("history read %d messages, want 4", len(got))
	}
}

func envTime(t *testing.T, key string, def time.Time) time.Time {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	at, err := time.Parse(time.RFC3339, v)
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return at
}

// recordedHistory reads the window of the mailbox's inbox in pages of two
// and checks what any mailbox answers: oldest first, each message once,
// every envelope a message with an ID and a destination, no error. It
// returns the message IDs.
func recordedHistory(t *testing.T, client *http.Client, endpoint, mailbox string, since, until time.Time, token string) []string {
	t.Helper()
	data, _ := json.Marshal(Config{Mailbox: mailbox, Addresses: []string{mailbox}})
	inst, err := NewFactory(Options{Endpoint: endpoint}).Open(t.Context(), model.Channel{
		Ref: testChan, Type: Type, CredentialRef: "recorded",
		Configuration: model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data},
	}, adapter.Dependencies{
		HTTP: client, Credentials: staticToken(token),
		Clock: portstest.NewFixedClock(time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatal(err)
	}
	hr, ok := inst.(adapter.HistoryReader)
	if !ok {
		t.Fatal("instance is no HistoryReader")
	}
	var ids []string
	var last time.Time
	req := adapter.HistoryRequest{Since: since, Until: until, Limit: 2}
	for range 100 {
		page, err := hr.History(t.Context(), req)
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		for _, env := range page.Envelopes {
			switch {
			case env.MessageID == "" || env.Destination == "" || env.Kind != model.EnvelopeMessage && env.Kind != model.EnvelopeAutomatedMessage:
				t.Fatalf("envelope %+v", env)
			case env.OccurredAt.Before(since) || !env.OccurredAt.Before(until) || env.OccurredAt.Before(last):
				t.Fatalf("envelope %s at %v outside the window or out of order (last %v)", env.MessageID, env.OccurredAt, last)
			}
			last = env.OccurredAt
			ids = append(ids, env.MessageID)
		}
		if page.Next == "" {
			return ids
		}
		req.Cursor = page.Next
	}
	t.Fatal("window not read")
	return nil
}
