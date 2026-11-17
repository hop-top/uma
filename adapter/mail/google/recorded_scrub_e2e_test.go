package google

import (
	"io/fs"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/testing/xrrtest"
)

// inboxFixture is a message the real mailbox received from a third party:
// the mailbox and the peer in folded headers and in a quoted-printable
// body, as the API returns it in format=raw.
func inboxFixture(msgID string) []byte {
	return []byte(strings.Join([]string{
		"From: Outside <sender@outside.example>",
		"To: Ada <" + realMailbox + ">,",
		"\t\"C. B.\" <" + realPeer + ">",
		"Subject: scenario inbox message",
		"Message-ID: <" + msgID + "@outside.example>",
		"Date: Sun, 11 Oct 2026 11:45:00 +0000",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"Content-Transfer-Encoding: quoted-printable",
		"",
		"ask charles@difference-=",
		"engine.example, who answers to ada.lovelace=40analytical-engine.example",
		"",
	}, "\r\n"))
}

// TestRecordedScenarioScrubsAndReplays runs the recorded scenario in record
// mode over the fake with made-up real identities, then in replay mode with
// the replay defaults and a server that is gone: the cassettes hold none of
// the identities, replay finds every request, and the first sync listed only
// the window before the fixed clock.
func TestRecordedScenarioScrubsAndReplays(t *testing.T) {
	g := newFakeGmail(t)
	box := g.box(realMailbox, testChan)
	g.now = func() time.Time { return recordedNow.Add(-30 * time.Minute) }
	g.box(realMailbox, testChan).minHistory = 1
	g.deliver(realMailbox, "in-window", "thread-in", inboxFixture("in-window"))
	g.now = func() time.Time { return recordedNow.Add(time.Hour) }
	g.deliver(realMailbox, "too-new", "thread-new", inboxFixture("too-new"))

	host := func(endpoint string) string {
		u, err := url.Parse(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		return u.Host
	}
	dir := t.TempDir()
	rec := recordedClient(t, dir, realMailbox, realPeer, xrrtest.WithMode(xrrtest.ModeRecord),
		xrrtest.WithTransport(g.srv.Client().Transport), xrrtest.WithPlaceholder(host(g.endpoint()), "PROVIDER_HOST"))
	recorded := recordedScenario(t, rec, g.endpoint(), realMailbox, realPeer, "tok-1")

	if got := recorded.ids(); !slices.Equal(got, []string{"in-window", "in-window"}) {
		t.Fatalf("first sync handed over %q, want only the message inside the window, twice (the resync reads it again)", got)
	}
	// The provider got the real thing; only the cassettes are scrubbed.
	g.mu.Lock()
	sends := append([]gSend(nil), box.sends...)
	g.mu.Unlock()
	if len(sends) != 1 {
		t.Fatalf("provider saw %d sends", len(sends))
	}
	if sent, _, _ := decodeB64(sends[0].Raw); len(residueIn(sent, realIdentities())) == 0 {
		t.Fatal("the message the provider received holds no address: nothing to scrub")
	}

	assertNoResidue(t, dir, realIdentities())
	files, err := fs.Glob(os.DirFS(dir), "*")
	if err != nil || len(files) < 8 {
		t.Fatalf("cassettes = %v, %v", files, err)
	}
	var all strings.Builder
	for _, f := range files {
		b, err := fs.ReadFile(os.DirFS(dir), f)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(b)
	}
	for _, want := range []string{placeholderMailbox, "before", recordedMessageIDDomain} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("cassettes never mention %q", want)
		}
	}

	replay := recordedClient(t, dir, "mailbox@example.test", "peer@example.test", xrrtest.WithMode(xrrtest.ModeReplay),
		xrrtest.WithPlaceholder(host("http://127.0.0.1:1"), "PROVIDER_HOST"))
	replayed := recordedScenario(t, replay, "http://127.0.0.1:1/gmail/v1", "mailbox@example.test", "peer@example.test", "replay-token")
	if got := replayed.ids(); !slices.Equal(got, []string{"in-window", "in-window"}) {
		t.Fatalf("replay handed over %q", got)
	}
	// The message both runs hand over is the scrubbed one.
	if rt, pt := recorded.got[0].Envelope.Content, replayed.got[0].Envelope.Content; !reflect.DeepEqual(rt, pt) {
		t.Fatalf("replay content %+v, record content %+v", pt, rt)
	}
}
