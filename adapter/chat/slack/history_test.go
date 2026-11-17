package slack

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

var historySince = testNow.Add(-24 * time.Hour)

func at(d time.Duration) string { return formatTS(historySince.Add(d)) }

// readAll reads the window page by page and returns the envelopes and the
// number of pages.
func (r *rig) readAll(t *testing.T, route *model.ExternalRef, limit int) ([]model.Envelope, int, bool) {
	t.Helper()
	var out []model.Envelope
	cursor, gap := "", false
	for pages := 1; pages <= 200; pages++ {
		p, err := r.x.History(t.Context(), adapter.HistoryRequest{Route: route, Since: historySince, Cursor: cursor, Limit: limit})
		if err != nil {
			t.Fatalf("History(cursor %q): %v", cursor, err)
		}
		if len(p.Envelopes) > limit {
			t.Fatalf("page holds %d envelopes, limit %d", len(p.Envelopes), limit)
		}
		out, gap = append(out, p.Envelopes...), gap || p.Gap
		if p.Next == "" {
			return out, pages, gap
		}
		if p.Next == cursor {
			t.Fatalf("cursor %q did not advance", cursor)
		}
		cursor = p.Next
	}
	t.Fatal("window not read in 200 pages")
	return nil, 0, false
}

func ids(envs []model.Envelope) []string {
	var out []string
	for _, e := range envs {
		out = append(out, e.MessageID)
	}
	return out
}

func TestHistoryReadsTheWindowOldestFirstInBoundedPages(t *testing.T) {
	r := newRig(t)
	r.f.put("acct1", testRoom, testPerson, "before the window", at(-time.Minute), "")
	var want []string
	for i, d := range []time.Duration{0, time.Hour, 5 * time.Hour, 6 * time.Hour, 20 * time.Hour} {
		ts := at(d)
		r.f.put("acct1", testRoom, testPerson, "m"+strconv.Itoa(i), ts, "")
		want = append(want, testRoom+":"+ts)
	}
	room := r.x.roomRef(testRoom)
	for _, limit := range []int{1, 2, 3, 10} {
		got, pages, gap := r.readAll(t, &room, limit)
		if !slices.Equal(ids(got), want) || gap {
			t.Fatalf("limit %d: messages %v gap %v, want %v: the window oldest first, the first message on its start included", limit, ids(got), gap, want)
		}
		if limit == 1 && pages < 5 {
			t.Errorf("limit 1 read in %d pages", pages)
		}
	}
	for _, c := range r.f.calls("acct1") {
		if c.Applied {
			t.Fatalf("a read applied %+v", c)
		}
	}
}

func TestHistoryEnvelopesAreAsInbound(t *testing.T) {
	r := newRig(t)
	r.f.put("acct1", testRoom, testPerson, "hello &amp; <@U0BOT>", at(time.Hour), "")
	bot := r.f.put("acct1", testRoom, testBotUser, "mine", at(2*time.Hour), "")
	bot.botID = testBotID
	other := r.f.put("acct1", testRoom, "", "from ci", at(3*time.Hour), "")
	other.botID, other.subtype = "B0OTHER", "bot_message"
	r.f.put("acct1", testRoom, testPerson, "joined", at(4*time.Hour), "").subtype = "channel_join"
	files := r.f.put("acct1", testRoom, testPerson, "", at(5*time.Hour), "")
	_ = files
	room := r.x.roomRef(testRoom)
	got, _, _ := r.readAll(t, &room, 10)
	if len(got) != 3 {
		t.Fatalf("envelopes = %v, want the person's, the app's bot's and the other bot's (not the join, not the empty one)", ids(got))
	}
	p := got[0]
	if p.Kind != model.EnvelopeMessage || p.Destination != testTeam || p.Sender != r.x.userRef(testPerson) || textOf(p) != "hello &amp; <@U0BOT>" ||
		p.EventID != "history:"+p.MessageID || !p.OccurredAt.Equal(slackTime(at(time.Hour))) || len(p.References) != 2 || p.References[0] != r.x.roomRef(testRoom) || p.References[1] != r.x.threadRef(testRoom, at(time.Hour)) {
		t.Errorf("person's envelope = %+v", p)
	}
	if got[1].Kind != model.EnvelopeAutomatedMessage || got[1].SenderKind != model.ContactKindBot || got[1].Sender != r.x.userRef(testBotUser) {
		t.Errorf("the app's own message (history reports it: core skips what it sent) = %+v", got[1])
	}
	if got[2].Sender != r.x.userRef("B0OTHER") || got[2].SenderKind != model.ContactKindBot {
		t.Errorf("other bot's envelope = %+v", got[2])
	}
	for _, e := range got {
		if e.Content.Validate() != nil || payloadOf(t, e)["conversation"] != testRoom {
			t.Errorf("envelope %+v: bad content or payload", e)
		}
	}
}

// A message whose thread has replies has them out of a room's window.
func TestHistoryOfARoomReportsAGapForThreads(t *testing.T) {
	r := newRig(t)
	parent := at(time.Hour)
	r.f.put("acct1", testRoom, testPerson, "parent", parent, "")
	r.f.put("acct1", testRoom, testPerson, "reply", at(2*time.Hour), parent)
	room := r.x.roomRef(testRoom)
	got, _, gap := r.readAll(t, &room, 10)
	if len(got) != 1 || !gap {
		t.Fatalf("messages %v gap %v, want the parent and a gap: replies are not in a room's history", ids(got), gap)
	}
}

func TestHistoryOfAThreadIsItsParentAndRepliesWithSlacksCursor(t *testing.T) {
	r := newRig(t)
	parent := at(0) // on the window's start, which the window includes
	r.f.put("acct1", testRoom, testPerson, "parent", parent, "")
	var want = []string{testRoom + ":" + parent}
	for i := range 5 {
		ts := at(2*time.Hour + time.Duration(i)*time.Minute)
		r.f.put("acct1", testRoom, testPerson, "reply", ts, parent)
		want = append(want, testRoom+":"+ts)
	}
	r.f.put("acct1", testRoom, testPerson, "elsewhere", at(3*time.Hour), "")
	thread := r.x.threadRef(testRoom, parent)
	for _, limit := range []int{1, 2, 4} {
		got, _, gap := r.readAll(t, &thread, limit)
		if !slices.Equal(ids(got), want) || gap {
			t.Fatalf("limit %d: %v gap %v, want %v", limit, ids(got), gap, want)
		}
	}
	// A reply carries its room and its thread, as the webhook reports it.
	got, _, _ := r.readAll(t, &thread, 10)
	if refs := got[1].References; len(refs) != 2 || refs[1] != r.x.threadRef(testRoom, parent) {
		t.Errorf("reply references = %+v", refs)
	}
}

func TestHistoryOfADirectMessage(t *testing.T) {
	r := newRig(t)
	route := r.x.userRef(testPerson)
	// No direct message yet: an empty window, and none is created.
	got, _, _ := r.readAll(t, &route, 10)
	if len(got) != 0 {
		t.Fatalf("envelopes = %v, want none", ids(got))
	}
	if opens := r.callsOf("conversations.open"); len(opens) != 1 || opens[0].args(t)["prevent_creation"] != true {
		t.Fatalf("open calls = %+v, want one that prevents creation", opens)
	}
	if len(r.acct.dms) != 0 {
		t.Fatal("reading history opened a direct message")
	}
	r.acct.dms[testPerson] = "D0DM9"
	r.f.put("acct1", "D0DM9", testPerson, "hi", at(time.Hour), "")
	got, _, _ = r.readAll(t, &route, 10)
	if len(got) != 1 || got[0].MessageID != "D0DM9:"+at(time.Hour) {
		t.Fatalf("envelopes = %v, want the direct message's", ids(got))
	}
}

func TestHistoryChannelWideMergesTheConfiguredConversations(t *testing.T) {
	r := newRig(t, func(c *Config) { c.HistoryChannels = []string{testRoom, "C0TWO"} })
	a1 := r.f.put("acct1", testRoom, testPerson, "a1", at(time.Hour), "")
	b1 := r.f.put("acct1", "C0TWO", testPerson, "b1", at(2*time.Hour), "")
	a2 := r.f.put("acct1", testRoom, testPerson, "a2", at(3*time.Hour), "")
	tie := r.f.put("acct1", "C0TWO", testPerson, "tie", a2.ts, "") // one ts in two conversations
	r.f.put("acct1", "C0OTHER", testPerson, "not configured", at(4*time.Hour), "")
	want := []string{testRoom + ":" + a1.ts, "C0TWO:" + b1.ts, testRoom + ":" + a2.ts, "C0TWO:" + tie.ts}
	for _, limit := range []int{1, 2, 10} {
		got, _, _ := r.readAll(t, nil, limit)
		if !slices.Equal(ids(got), want) {
			t.Fatalf("limit %d: %v, want %v: both conversations by time, a shared ts by conversation, none unlisted", limit, ids(got), want)
		}
	}

	bare := newRig(t)
	_, err := bare.x.History(t.Context(), adapter.HistoryRequest{Since: historySince, Limit: 5})
	if model.CodeOf(err) != model.Disabled {
		t.Fatalf("channel-wide history without history_channels = %v, want disabled", err)
	}
}

// A window of days is not read by scanning all of it for every page: the
// oldest messages come first, found by widening a time range from the start.
func TestHistoryFindsTheOldestAcrossALongWindow(t *testing.T) {
	r := newRig(t)
	var want []string
	for _, d := range []time.Duration{0, 2 * time.Hour, 9 * time.Hour, 11 * time.Hour, 23 * time.Hour} {
		ts := at(d)
		r.f.put("acct1", testRoom, testPerson, "x", ts, "")
		want = append(want, testRoom+":"+ts)
	}
	room := r.x.roomRef(testRoom)
	p, err := r.x.History(t.Context(), adapter.HistoryRequest{Route: &room, Since: historySince, Limit: 2})
	if err != nil || !slices.Equal(ids(p.Envelopes), want[:2]) || p.Next == "" {
		t.Fatalf("first page = %v next %q, %v; want the two oldest and a cursor", ids(p.Envelopes), p.Next, err)
	}
	for _, c := range r.callsOf("conversations.history") {
		if c.args(t)["latest"] == nil {
			t.Fatalf("a read unbounded above scans the whole window: %s", c.Body)
		}
	}
}

func TestHistoryNarrowsARangeSlackWouldTruncate(t *testing.T) {
	old := historyScanPages
	historyScanPages = 2 // 400 messages a read
	t.Cleanup(func() { historyScanPages = old })
	r := newRig(t)
	var want []string
	for i := range 450 {
		ts := at(time.Duration(i) * time.Second)
		r.f.put("acct1", testRoom, testPerson, "x", ts, "")
		want = append(want, testRoom+":"+ts)
	}
	room := r.x.roomRef(testRoom)
	got, _, gap := r.readAll(t, &room, 100)
	if !slices.Equal(ids(got), want) || gap {
		t.Fatalf("read %d messages gap %v, want all 450 without a gap", len(got), gap)
	}
}

func TestHistoryGapWhenASecondHoldsMoreThanARead(t *testing.T) {
	old := historyScanPages
	historyScanPages = 1
	t.Cleanup(func() { historyScanPages = old })
	r := newRig(t)
	for range historyPage + 20 {
		r.f.put("acct1", testRoom, testPerson, "x", "", "") // within microseconds of each other
	}
	r.f.put("acct1", testRoom, testPerson, "first", at(0), "")
	room := r.x.roomRef(testRoom)
	// The ts of the fake's own messages are after testNow: a window from just before.
	p, err := r.x.History(t.Context(), adapter.HistoryRequest{Route: &room, Since: testNow.Add(-time.Minute), Limit: 500})
	if err != nil || !p.Gap {
		t.Fatalf("page gap %v, %v; want a gap: more than a read holds sit within a second", p.Gap, err)
	}
}

func TestHistoryCursors(t *testing.T) {
	r := newRig(t, func(c *Config) { c.HistoryChannels = []string{testRoom} })
	for i := range 4 {
		r.f.put("acct1", testRoom, testPerson, "x", at(time.Duration(i)*time.Hour), "")
	}
	room := r.x.roomRef(testRoom)
	first, err := r.x.History(t.Context(), adapter.HistoryRequest{Route: &room, Since: historySince, Limit: 2})
	if err != nil || first.Next == "" {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	again, _ := r.x.History(t.Context(), adapter.HistoryRequest{Route: &room, Since: historySince, Limit: 2})
	if again.Next != first.Next || !slices.Equal(ids(again.Envelopes), ids(first.Envelopes)) {
		t.Fatalf("a first page read again differs: %q vs %q", again.Next, first.Next)
	}
	other := r.x.roomRef("C0TWO")
	for name, req := range map[string]adapter.HistoryRequest{
		"never issued":      {Route: &room, Since: historySince, Cursor: "conformance-unknown-cursor", Limit: 2},
		"not base64":        {Route: &room, Since: historySince, Cursor: "!!", Limit: 2},
		"another route":     {Route: &other, Since: historySince, Cursor: first.Next, Limit: 2},
		"another window":    {Route: &room, Since: historySince.Add(time.Hour), Cursor: first.Next, Limit: 2},
		"channel-wide":      {Since: historySince, Cursor: first.Next, Limit: 2},
		"no limit":          {Route: &room, Since: historySince, Limit: 0},
		"no start":          {Route: &room, Limit: 2},
		"not a Slack route": {Route: &model.ExternalRef{Channel: testChannel, Namespace: "x.y", Kind: "z", ID: "1"}, Since: historySince, Limit: 2},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := r.x.History(t.Context(), req); model.CodeOf(err) != model.Validation {
				t.Fatalf("History = %v, want validation", err)
			}
		})
	}
	calls := len(r.f.calls("acct1"))
	_, _ = r.x.History(t.Context(), adapter.HistoryRequest{Route: &room, Since: historySince, Cursor: "garbage", Limit: 2})
	if len(r.f.calls("acct1")) != calls {
		t.Error("a bad cursor made a call")
	}
}

func TestHistoryReadFailures(t *testing.T) {
	r := newRig(t)
	room := r.x.roomRef(testRoom)
	r.acct.rateLimitedHistory = true
	_, err := r.x.History(t.Context(), adapter.HistoryRequest{Route: &room, Since: historySince, Limit: 5})
	if model.CodeOf(err) != model.RateLimited {
		t.Fatalf("rate limited history = %v, want rate_limited", err)
	}
	r2 := newRig(t)
	missing := r2.x.roomRef("C0NOPE")
	_, err = r2.x.History(t.Context(), adapter.HistoryRequest{Route: &missing, Since: historySince, Limit: 5})
	if model.CodeOf(err) != model.NotFound {
		t.Fatalf("history of a missing conversation = %v, want not_found", err)
	}
	r2.f.respond("acct1", answer{})
	_ = r2.x.Close(t.Context())
	if _, err = r2.x.History(t.Context(), adapter.HistoryRequest{Route: &missing, Since: historySince, Limit: 5}); model.CodeOf(err) != model.Unavailable {
		t.Fatalf("closed = %v", err)
	}
	_ = strings.Join
}

// conversations.replies "first contains the parent message object", the
// window or not: a parent older than the window's start is not in it.
func TestHistoryOfAThreadKeepsToTheWindow(t *testing.T) {
	r := newRig(t)
	parent := at(-time.Hour)
	r.f.put("acct1", testRoom, testPerson, "old parent", parent, "")
	first := r.f.put("acct1", testRoom, testPerson, "reply in", at(time.Hour), parent)
	thread := r.x.threadRef(testRoom, parent)
	got, _, _ := r.readAll(t, &thread, 10)
	if want := []string{testRoom + ":" + first.ts}; !slices.Equal(ids(got), want) {
		t.Fatalf("messages = %v, want %v: the parent is before the window", ids(got), want)
	}
}

// A message that heads a thread has its replies out of a room's window,
// whether or not Slack's answer counts them.
func TestHistoryGapForAnyThreadParent(t *testing.T) {
	r := newRig(t)
	room := r.x.roomRef(testRoom)
	r.f.put("acct1", testRoom, testPerson, "parent", at(time.Hour), "")
	r.x.http = rewriteClient{r.x.http, func(req *http.Request, body []byte) []byte {
		if !strings.HasSuffix(req.URL.Path, "/conversations.history") {
			return body
		}
		var res map[string]any
		_ = json.Unmarshal(body, &res)
		for _, m := range res["messages"].([]any) {
			o := m.(map[string]any)
			o["thread_ts"] = o["ts"] // a parent, with no reply_count
		}
		out, _ := json.Marshal(res)
		return out
	}}
	_, _, gap := r.readAll(t, &room, 10)
	if !gap {
		t.Fatal("no gap for a message that heads a thread")
	}
}

// readUntil reads the window up to until page by page.
func (r *rig) readUntil(t *testing.T, route *model.ExternalRef, until time.Time, limit int) []model.Envelope {
	t.Helper()
	var out []model.Envelope
	cursor := ""
	for range 50 {
		p, err := r.x.History(t.Context(), adapter.HistoryRequest{Route: route, Since: historySince, Until: until, Cursor: cursor, Limit: limit})
		if err != nil {
			t.Fatalf("History(cursor %q): %v", cursor, err)
		}
		out = append(out, p.Envelopes...)
		if cursor = p.Next; cursor == "" {
			return out
		}
	}
	t.Fatal("window not read in 50 pages")
	return nil
}

// Until ends the window exclusively, for a room and for a thread: a
// message posted at Until is out, one just before is in.
func TestHistoryUntilEndsTheWindow(t *testing.T) {
	r := newRig(t)
	var want []string
	for i, d := range []time.Duration{0, time.Hour, 5 * time.Hour, 6 * time.Hour, 20 * time.Hour} {
		ts := at(d)
		r.f.put("acct1", testRoom, testPerson, "m"+strconv.Itoa(i), ts, "")
		if d < 6*time.Hour {
			want = append(want, testRoom+":"+ts)
		}
	}
	room := r.x.roomRef(testRoom)
	until := historySince.Add(6 * time.Hour)
	for _, limit := range []int{1, 10} {
		if got := r.readUntil(t, &room, until, limit); !slices.Equal(ids(got), want) {
			t.Fatalf("room, limit %d: %v, want %v", limit, ids(got), want)
		}
	}
	parent := at(10 * time.Minute)
	var thread []string
	r.f.put("acct1", testRoom, testPerson, "parent", parent, "")
	for i, d := range []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour} {
		ts := at(d)
		r.f.put("acct1", testRoom, testPerson, "reply"+strconv.Itoa(i), ts, parent)
		if d < 3*time.Hour {
			thread = append(thread, testRoom+":"+ts)
		}
	}
	ref := r.x.threadRef(testRoom, parent)
	got := r.readUntil(t, &ref, historySince.Add(3*time.Hour), 10)
	if want := append([]string{testRoom + ":" + parent}, thread...); !slices.Equal(ids(got), want) {
		t.Fatalf("thread: %v, want %v", ids(got), want)
	}
	first, err := r.x.History(t.Context(), adapter.HistoryRequest{Route: &room, Since: historySince, Until: until, Limit: 1})
	if err != nil || first.Next == "" {
		t.Fatalf("History = %+v, %v", first, err)
	}
	if _, err := r.x.History(t.Context(), adapter.HistoryRequest{Route: &room, Since: historySince, Until: until.Add(time.Hour), Limit: 1, Cursor: first.Next}); !model.IsCode(err, model.Validation) {
		t.Fatalf("cursor of another Until = %v", err)
	}
}
