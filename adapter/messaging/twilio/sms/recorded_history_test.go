package sms_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/testing/xrrtest"
)

// recordedReply is the text the recorder replies with from TWILIO_TO.
// Phones may capitalize it, so it matches without regard to case.
const recordedReply = "recorded test reply"

func isRecordedReply(s string) bool { return strings.EqualFold(strings.TrimSpace(s), recordedReply) }

// TestRecordedHistory reads the history of the conversation with
// TWILIO_TO: the reply the recorder sent from that phone, and nothing
// else. Recording sends one SMS from TWILIO_FROM to TWILIO_TO asking for
// the reply, waits (unrecorded) until the account's message list shows
// it, then records one history read. The listing is privacy-safe by
// construction: it names both numbers (To the channel's, From the
// phone's) and a DateSent lower bound on the day the test started, and
// the transport refuses, before anything is written, a list page holding
// a message of any other number. Replay skips until its cassettes exist.
func TestRecordedHistory(t *testing.T) {
	dir := filepath.Join(cassettes, "history-reply")
	mode := os.Getenv(xrrtest.EnvMode)
	if mode == "" || mode == string(xrrtest.ModeReplay) {
		if files, _ := filepath.Glob(filepath.Join(dir, "*.yaml")); len(files) == 0 {
			t.Skip("no recorded history-reply cassettes yet")
		}
	}
	a := liveAccount()
	guard := privacyGuard{numbers: []string{a.from, a.to}, base: http.DefaultTransport}
	replyScenario(t, a, a.client(t, dir, xrrtest.WithTransport(guard)), guard, attemptTime(t, dir), mode == string(xrrtest.ModeRecord))
}

// replyScenario sends the request for a reply from a.from to a.to, waits
// for the reply when recording, and reads the conversation's history from
// since: exactly the reply, never the send.
func replyScenario(t *testing.T, a recordedAccount, client *http.Client, guard privacyGuard, since time.Time, record bool, baseURL ...string) {
	t.Helper()
	inst := a.open(t, client, baseURL...)
	sent, err := inst.(adapter.Sender).Send(t.Context(), adapter.SendRequest{Effect: a.effect(), Content: model.Content{Parts: []model.Part{{
		Kind: model.PartText, Text: "recorded test message: please reply " + recordedReply,
	}}}})
	if err != nil || len(sent.References) != 1 {
		t.Fatalf("Send = %+v, %v", sent, err)
	}
	if record {
		waitReply(t, a, guard, since, baseURL...)
	}
	route := a.effect().Route
	page, err := inst.(adapter.HistoryReader).History(t.Context(), adapter.HistoryRequest{Route: &route, Since: since, Limit: 10})
	if err != nil || len(page.Envelopes) != 1 || page.Next != "" || page.Gap {
		t.Fatalf("History = %+v, %v; want the one reply", page, err)
	}
	env := page.Envelopes[0]
	text := ""
	if len(env.Content.Parts) > 0 {
		text = env.Content.Parts[0].Text
	}
	if env.Kind != model.EnvelopeMessage || env.MessageID == sent.References[0].ID || env.Destination != a.from ||
		env.Sender != route || !isRecordedReply(text) || env.OccurredAt.Before(since) {
		t.Fatalf("history envelope %+v; want the reply from %s to %s, never the send %s", env, a.to, a.from, sent.References[0].ID)
	}
}

// waitReply polls the message list, unrecorded and through the guard,
// until it shows the reply from a.to sent since, and logs how long that
// took. Any other message in the window fails the test.
func waitReply(t *testing.T, a recordedAccount, guard privacyGuard, since time.Time, baseURL ...string) {
	t.Helper()
	c := a.rest(t, &http.Client{Transport: guard}, baseURL...)
	t.Logf("reply %q from %s to %s now", recordedReply, a.to, a.from)
	start := time.Now()
	for time.Since(start) < 5*time.Minute {
		p, err := c.ListInbound(t.Context(), a.sid, ports.Secret{Bytes: []byte(a.token)}, twilio.InboundQuery{To: a.from, From: a.to, Since: since, Limit: 10})
		if err != nil {
			t.Fatalf("live listing: %v", err)
		}
		for _, m := range p.Messages {
			if !isRecordedReply(m.Body) {
				t.Fatalf("the window holds a message other than the reply; send only %q, then record again", recordedReply)
			}
		}
		if len(p.Messages) > 0 {
			t.Logf("the Messages list showed the reply after %v", time.Since(start).Round(time.Second))
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("the Messages list never showed the reply")
}

// The history scenario recorded against the local Messages API replays
// without it, and its cassettes hold neither credentials nor numbers nor
// a message of the day other than the reply.
func TestHistoryCassetteRoundTrip(t *testing.T) {
	fake := newFakeTwilio(t)
	dir := t.TempDir()
	a := recordedAccount{sid: hookAccount, token: "record-token", from: senderNumber, to: peerNumber}
	since := time.Now().UTC().Truncate(time.Second)
	fake.holdInbound(hookAccount, fakeMessage{SID: "SM10000000000000000000000000000009", From: a.to, To: a.from, Body: "an earlier private message", Sent: since.Add(-time.Second)})
	fake.holdInbound(hookAccount, fakeMessage{SID: "SM10000000000000000000000000000010", From: a.to, To: a.from, Body: "Recorded test reply", Sent: since.Add(time.Second)})
	guard := privacyGuard{numbers: []string{a.from, a.to}, base: http.DefaultTransport}
	host := xrrtest.WithPlaceholder(fake.srv.URL[len("http://"):], "PROVIDER_HOST")
	replyScenario(t, a, a.client(t, dir, xrrtest.WithMode(xrrtest.ModeRecord), host, xrrtest.WithTransport(guard)), guard, since, true, fake.srv.URL)
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(files) == 0 {
		t.Fatal("no cassette written")
	}
	for _, f := range files {
		b, err := os.ReadFile(f) // #nosec G304 -- test temp dir
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{a.token, a.sid, a.from, a.to, url.QueryEscape(a.from), url.QueryEscape(a.to), "Authorization", "private"} {
			if bytes.Contains(b, []byte(secret)) {
				t.Errorf("%s contains %q", filepath.Base(f), secret)
			}
		}
	}
	fake.srv.Close()
	replyScenario(t, a, a.client(t, dir, xrrtest.WithMode(xrrtest.ModeReplay), host, xrrtest.WithTransport(guard)), guard, since, false, fake.srv.URL)
}

// errForeignNumber is a list page holding a message of a number outside
// the scenario.
var errForeignNumber = errors.New("privacy guard: the message list holds a message of another number; nothing of it is passed on")

// privacyGuard passes a Messages list page on only when every message it
// holds is between the scenario's numbers. Under the recording transport
// it runs before the cassette is written, so a foreign message never
// reaches one: the exchange fails instead.
type privacyGuard struct {
	numbers []string
	base    http.RoundTripper
}

func (g privacyGuard) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := g.base.RoundTrip(r)
	if err != nil || r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/Messages.json") {
		return resp, err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var page struct {
		Messages []struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &page) != nil {
		page.Messages = nil
	}
	for _, m := range page.Messages {
		if !g.allowed(m.From) || !g.allowed(m.To) {
			return nil, errForeignNumber
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

func (g privacyGuard) allowed(number string) bool {
	for _, n := range g.numbers {
		if number == n {
			return true
		}
	}
	return false
}

// The guard refuses a list page holding any other number, whichever side
// it is on, and passes the scenario's own pages and other requests.
func TestPrivacyGuard(t *testing.T) {
	const from, to = "+15005550006", "+15558675310"
	page := func(msgs ...[2]string) string {
		var items []string
		for _, m := range msgs {
			items = append(items, `{"from":"`+m[0]+`","to":"`+m[1]+`","body":"x"}`)
		}
		return `{"messages":[` + strings.Join(items, ",") + `],"next_page_uri":null}`
	}
	for _, c := range []struct {
		name, path, body string
		refused          bool
	}{
		{"own messages", "/Messages.json", page([2]string{to, from}, [2]string{from, to}), false},
		{"empty page", "/Messages.json", page(), false},
		{"other sender", "/Messages.json", page([2]string{to, from}, [2]string{"+15550000001", from}), true},
		{"other recipient", "/Messages.json", page([2]string{from, "+15550000002"}), true},
		{"not a list", "/Messages/SM00000000000000000000000000000001.json", `{"from":"+15550000001","to":"+15550000002"}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := privacyGuard{numbers: []string{from, to}, base: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(c.body))}, nil
			})}
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.twilio.com/2010-04-01/Accounts/"+hookAccount+c.path, nil)
			resp, err := g.RoundTrip(req)
			if c.refused {
				if !errors.Is(err, errForeignNumber) || resp != nil {
					t.Fatalf("RoundTrip = %v, %v; want refused", resp, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(resp.Body)
			if string(got) != c.body {
				t.Fatalf("body passed on = %q, want %q", got, c.body)
			}
		})
	}
}
