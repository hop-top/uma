package discord

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func TestHistoryOldestFirstInPages(t *testing.T) {
	const other = "1100000000000000012"
	r := newRig(t, func(c *Config) { c.HistoryChannels = []string{testRoom, other} })
	since := testNow.Add(-time.Hour)
	r.f.put("acct1", testRoom, testPerson, "too old", sid(since.Add(-time.Second), 0))
	var want []string
	for i := range 5 {
		ch := testRoom
		if i%2 == 1 {
			ch = other
		}
		id := sid(since.Add(time.Duration(i)*time.Minute), 1)
		r.f.put("acct1", ch, testPerson, "m"+string(rune('0'+i)), id)
		want = append(want, messageID(ch, id))
	}
	for _, limit := range []int{1, 2, 3, 100} {
		var got []string
		cursor := ""
		for range 10 {
			page, err := r.x.History(t.Context(), adapter.HistoryRequest{Since: since, Limit: limit, Cursor: cursor})
			if err != nil {
				t.Fatalf("limit %d: %v", limit, err)
			}
			if len(page.Envelopes) > limit {
				t.Fatalf("limit %d: page of %d", limit, len(page.Envelopes))
			}
			for _, e := range page.Envelopes {
				if e.Destination != testApp || e.EventID != "history:"+e.MessageID || e.Kind != model.EnvelopeMessage {
					t.Fatalf("envelope %+v", e)
				}
				got = append(got, e.MessageID)
			}
			if cursor = page.Next; cursor == "" {
				break
			}
		}
		if len(got) != len(want) {
			t.Fatalf("limit %d: %v, want %v", limit, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("limit %d: %v, want %v oldest first", limit, got, want)
			}
		}
	}
	// One route: that channel only; a thread's messages carry it.
	r.f.thread("acct1", testRoom, testThread)
	r.f.put("acct1", testThread, testPerson, "in thread", sid(since.Add(time.Minute), 7))
	page, err := r.x.History(t.Context(), adapter.HistoryRequest{Route: new(r.x.threadRef(testRoom, testThread)), Since: since, Limit: 10})
	if err != nil || len(page.Envelopes) != 1 || len(page.Envelopes[0].References) != 2 {
		t.Fatalf("thread history = %+v, %v", page, err)
	}
	// A cursor of another read is refused before any call.
	other1, _ := r.x.History(t.Context(), adapter.HistoryRequest{Since: since, Limit: 1})
	before := len(r.f.calls("acct1"))
	if _, err := r.x.History(t.Context(), adapter.HistoryRequest{Route: new(r.x.roomRef(testRoom)), Since: since, Limit: 1, Cursor: other1.Next}); !model.IsCode(err, model.Validation) {
		t.Fatalf("foreign cursor = %v", err)
	}
	if _, err := r.x.History(t.Context(), adapter.HistoryRequest{Since: since, Limit: 1, Cursor: "garbage"}); !model.IsCode(err, model.Validation) {
		t.Fatalf("garbage cursor = %v", err)
	}
	if len(r.f.calls("acct1")) != before {
		t.Fatal("a refused cursor reached Discord")
	}
	if _, err := newRig(t).x.History(t.Context(), adapter.HistoryRequest{Since: since, Limit: 1}); !model.IsCode(err, model.Disabled) {
		t.Fatalf("channel-wide without history_channels = %v", err)
	}
}

// Until ends the window exclusively across channels and pages: a message
// posted at Until is out, and a page holding only later messages ends the
// read rather than repeating its cursor.
func TestHistoryUntilEndsTheWindow(t *testing.T) {
	const other = "1100000000000000012"
	r := newRig(t, func(c *Config) { c.HistoryChannels = []string{testRoom, other} })
	since := testNow.Add(-time.Hour)
	var ids []string
	for i := range 5 {
		ch := testRoom
		if i%2 == 1 {
			ch = other
		}
		id := sid(since.Add(time.Duration(i)*time.Minute), 1)
		r.f.put("acct1", ch, testPerson, "m"+string(rune('0'+i)), id)
		ids = append(ids, messageID(ch, id))
	}
	for until, want := range map[int]int{1: 1, 3: 3} {
		for _, limit := range []int{1, 100} {
			var got []string
			cursor := ""
			for range 10 {
				page, err := r.x.History(t.Context(), adapter.HistoryRequest{
					Since: since, Until: since.Add(time.Duration(until) * time.Minute), Limit: limit, Cursor: cursor,
				})
				if err != nil {
					t.Fatalf("until %d limit %d: %v", until, limit, err)
				}
				for _, e := range page.Envelopes {
					got = append(got, e.MessageID)
				}
				if cursor = page.Next; cursor == "" {
					break
				}
			}
			if !slices.Equal(got, ids[:want]) {
				t.Fatalf("until %d limit %d: %v, want %v", until, limit, got, ids[:want])
			}
			if cursor != "" {
				t.Fatalf("until %d limit %d: read never ended", until, limit)
			}
		}
	}
	first, err := r.x.History(t.Context(), adapter.HistoryRequest{Since: since, Until: since.Add(4 * time.Minute), Limit: 1})
	if err != nil || first.Next == "" {
		t.Fatalf("History = %+v, %v", first, err)
	}
	if _, err := r.x.History(t.Context(), adapter.HistoryRequest{Since: since, Until: since.Add(3 * time.Minute), Limit: 1, Cursor: first.Next}); !model.IsCode(err, model.Validation) {
		t.Fatalf("cursor of another Until = %v", err)
	}
}

func TestHistoryUserAndGap(t *testing.T) {
	r := newRig(t)
	since := testNow.Add(-time.Hour)
	r.f.holdDM("acct1", testPerson, "1100000000000000099")
	m := r.f.put("acct1", "1100000000000000099", testPerson, "dm", sid(since.Add(time.Minute), 1))
	m.flags = flagHasThread
	page, err := r.x.History(t.Context(), adapter.HistoryRequest{Route: new(r.x.userRef(testPerson)), Since: since, Limit: 5})
	if err != nil || len(page.Envelopes) != 1 || !page.Gap {
		t.Fatalf("user history = %+v, %v", page, err)
	}
	if len(r.callsOf(http.MethodPost, "users/@me/channels")) != 1 {
		t.Fatal("direct message channel not resolved")
	}
}

func TestRefetch(t *testing.T) {
	r := newRig(t)
	m := r.f.put("acct1", testRoom, testPerson, "now", "")
	m.edited = "2026-10-13T12:00:05.000000+00:00"
	env, err := r.x.Refetch(t.Context(), adapter.RefetchRequest{Target: *msgRef(testRoom, m.id)})
	if err != nil || env.Kind != model.EnvelopeMessageEdited || env.SourceRevision != m.edited || env.Content.Parts[0].Text != "now" || env.Sender.ID != testPerson {
		t.Fatalf("Refetch = %+v, %v", env, err)
	}
	if _, err := r.x.Refetch(t.Context(), adapter.RefetchRequest{Target: *msgRef(testRoom, "1299999999999999999")}); !model.IsCode(err, model.NotFound) {
		t.Fatalf("gone = %v", err)
	}
	withheld := r.f.put("acct1", testRoom, testPerson, "", "")
	if _, err := r.x.Refetch(t.Context(), adapter.RefetchRequest{Target: *msgRef(testRoom, withheld.id)}); !model.IsCode(err, model.Unsupported) {
		t.Fatalf("withheld content = %v", err)
	}
}
