package gitlab_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func (r *rig) historyPage(since, until time.Time, cursor string, limit int) (adapter.HistoryPage, error) {
	rt := issueRoute("7")
	return r.inst.(adapter.HistoryReader).History(r.t.Context(), adapter.HistoryRequest{Route: &rt, Since: since, Until: until, Cursor: cursor, Limit: limit})
}

// readAll reads the window page by page and returns every envelope in order.
func (r *rig) readAll(since, until time.Time, limit int) []model.Envelope {
	r.t.Helper()
	var out []model.Envelope
	cursor := ""
	for range 100 {
		p, err := r.historyPage(since, until, cursor, limit)
		if err != nil {
			r.t.Fatalf("History: %v", err)
		}
		if len(p.Envelopes) > limit {
			r.t.Fatalf("page of %d envelopes over limit %d", len(p.Envelopes), limit)
		}
		out = append(out, p.Envelopes...)
		if p.Next == "" {
			return out
		}
		cursor = p.Next
	}
	r.t.Fatal("history never ended")
	return nil
}

func ids(envs []model.Envelope) []string {
	var out []string
	for _, e := range envs {
		out = append(out, e.MessageID)
	}
	return out
}

func TestHistoryReadsDescriptionAndNotes(t *testing.T) {
	r := newRig(t)
	since := t0.Add(-24 * time.Hour)
	r.gl.edit("a", sandboxProject, 7, "It crashes.")
	r.gl.setCreated("a", sandboxProject, 7, since.Add(time.Hour))
	a := r.gl.seed("a", sandboxProject, 7, 5000003, "first", since.Add(2*time.Hour))
	r.gl.putNote("a", fakeNote{Issue: "700000001#7", Body: "label changed", Author: 5000001, System: true, Created: since.Add(3 * time.Hour)})
	b := r.gl.putNote("a", fakeNote{Issue: "700000001#7", Body: "private", Author: 5000001, Internal: true, Created: since.Add(4 * time.Hour)})
	c := r.gl.putNote("a", fakeNote{
		Issue: "700000001#7", Body: "echo\n\n<!-- uma:" + string(opA.ID) + ":" + string(tryOne) + " -->", Author: botID, Username: "project_1_bot_x",
		Created: since.Add(5 * time.Hour),
	})
	r.gl.seed("a", sandboxProject, 7, 5000003, "before the window", since.Add(-time.Hour))

	got := r.readAll(since, time.Time{}, 10)
	want := []string{"issue:3000007", "note:" + strconv.FormatInt(a.ID, 10), "note:" + strconv.FormatInt(b.ID, 10), "note:" + strconv.FormatInt(c.ID, 10)}
	if !slices.Equal(ids(got), want) {
		t.Fatalf("messages = %v, want %v", ids(got), want)
	}
	for _, e := range got {
		if e.Destination != sandboxPath || e.References[0] != issueRoute("7") || e.WorkItem != nil || !strings.HasPrefix(e.EventID, "history:") ||
			e.EventID != "history:"+e.MessageID {
			t.Fatalf("envelope = %+v", e)
		}
	}
	if got[0].Content.Parts[0].Text != "It crashes." || got[0].Sender.ID != "5000001" {
		t.Fatalf("description = %+v", got[0])
	}
	if got[2].Audience.Visibility != model.Internal || got[1].Audience.Visibility != model.Public {
		t.Fatalf("audiences = %v, %v; want public, internal", got[1].Audience, got[2].Audience)
	}
	if got[3].Content.Parts[0].Text != "echo" || got[3].Kind != model.EnvelopeAutomatedMessage || len(got[3].References) != 2 || got[3].References[1] != model.EchoRef(chanA, opA, tryOne) {
		t.Fatalf("own note = %+v, want the echo of its attempt", got[3])
	}

	// The window's end is exclusive.
	if got := r.readAll(since, since.Add(4*time.Hour), 10); len(got) != 2 {
		t.Fatalf("window until the internal note = %v", ids(got))
	}
	// The same messages arrive in any page size, once each.
	for _, limit := range []int{1, 2, 3} {
		if g := r.readAll(since, time.Time{}, limit); !slices.Equal(ids(g), want) {
			t.Fatalf("limit %d: %v, want %v", limit, ids(g), want)
		}
	}
}

func TestHistoryPagesThroughManyNotes(t *testing.T) {
	r := newRig(t)
	since := t0.Add(-24 * time.Hour)
	var want []string
	for i := range 45 {
		n := r.gl.seed("a", sandboxProject, 7, 5000003, "n"+strconv.Itoa(i), since.Add(time.Hour+time.Duration(i)*time.Minute))
		want = append(want, "note:"+strconv.FormatInt(n.ID, 10))
	}
	for _, limit := range []int{7, 20, 100} {
		if got := r.readAll(since, time.Time{}, limit); !slices.Equal(ids(got), want) {
			t.Fatalf("limit %d: %d messages, want %d in order", limit, len(got), len(want))
		}
	}
}

// A note deleted while the window is read neither repeats nor drops a
// message: the notes after it move to an earlier page.
func TestHistoryAfterDeletionBetweenPages(t *testing.T) {
	r := newRig(t)
	since := t0.Add(-24 * time.Hour)
	var all []*fakeNote
	for i := range 6 {
		all = append(all, r.gl.seed("a", sandboxProject, 7, 5000003, "n"+strconv.Itoa(i), since.Add(time.Hour+time.Duration(i)*time.Minute)))
	}
	p1, err := r.historyPage(since, time.Time{}, "", 2)
	if err != nil || len(p1.Envelopes) != 2 || p1.Next == "" {
		t.Fatalf("page 1 = %+v, %v", p1, err)
	}
	// The first page's notes: one is deleted, and the next page shifts.
	if _, err := r.del("note:" + strconv.FormatInt(all[0].ID, 10)); err != nil {
		t.Fatal(err)
	}
	var rest []model.Envelope
	cursor := p1.Next
	for cursor != "" {
		p, err := r.historyPage(since, time.Time{}, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		rest = append(rest, p.Envelopes...)
		cursor = p.Next
	}
	var want []string
	for _, n := range all[2:] {
		want = append(want, "note:"+strconv.FormatInt(n.ID, 10))
	}
	if !slices.Equal(ids(rest), want) {
		t.Fatalf("after deletion: %v, want %v", ids(rest), want)
	}
}

func TestHistoryRefusals(t *testing.T) {
	r := newRig(t)
	since := t0.Add(-time.Hour)
	rt := issueRoute("7")
	h := r.inst.(adapter.HistoryReader)
	for name, tc := range map[string]struct {
		req  adapter.HistoryRequest
		code model.ErrorCode
	}{
		"no limit":   {adapter.HistoryRequest{Route: &rt, Since: since}, model.Validation},
		"no since":   {adapter.HistoryRequest{Route: &rt, Limit: 1}, model.Validation},
		"no route":   {adapter.HistoryRequest{Since: since, Limit: 1}, model.Unsupported},
		"foreign":    {adapter.HistoryRequest{Route: &model.ExternalRef{Channel: chanA, Namespace: "other", Kind: model.RefKindWorkItem, ID: "7", ParentID: "1"}, Since: since, Limit: 1}, model.Validation},
		"bad cursor": {adapter.HistoryRequest{Route: &rt, Since: since, Limit: 1, Cursor: "zzz"}, model.Validation},
		"forged":     {adapter.HistoryRequest{Route: &rt, Since: since, Limit: 1, Cursor: "gl1.e30"}, model.Validation},
	} {
		_, err := h.History(t.Context(), tc.req)
		if me := wantCode(t, err, tc.code); name == "no route" && !strings.Contains(me.SafeMessage, "one issue") {
			t.Errorf("no route message = %q", me.SafeMessage)
		}
	}
	if n := len(r.gl.calls("a")); n != 0 {
		t.Fatalf("%d provider calls for refused reads, want none", n)
	}
}

// A cursor is bound to the issue and window it was issued for.
func TestHistoryCursorIsBoundToItsRead(t *testing.T) {
	r := newRig(t)
	since := t0.Add(-24 * time.Hour)
	for i := range 3 {
		r.gl.seed("a", sandboxProject, 7, 5000003, "n", since.Add(time.Hour+time.Duration(i)*time.Minute))
	}
	p, err := r.historyPage(since, time.Time{}, "", 1)
	if err != nil || p.Next == "" {
		t.Fatalf("page = %+v, %v", p, err)
	}
	if _, err := r.historyPage(since.Add(time.Minute), time.Time{}, p.Next, 1); err == nil {
		t.Fatal("cursor accepted for another window")
	} else {
		wantCode(t, err, model.Validation)
	}
	rt := issueRoute("8")
	_, err = r.inst.(adapter.HistoryReader).History(t.Context(), adapter.HistoryRequest{Route: &rt, Since: since, Cursor: p.Next, Limit: 1})
	wantCode(t, err, model.Validation)
}

func TestHistoryFailures(t *testing.T) {
	r := newRig(t)
	since := t0.Add(-time.Hour)
	r.gl.respond("a", statusAnswer(429, "Retry later", map[string]string{"Retry-After": "12"}))
	_, err := r.historyPage(since, time.Time{}, "", 5)
	if me := wantCode(t, err, model.RateLimited); me.RetryAfter != 12*time.Second {
		t.Fatalf("RetryAfter = %v", me.RetryAfter)
	}
	rt := issueRoute("404")
	_, err = r.inst.(adapter.HistoryReader).History(t.Context(), adapter.HistoryRequest{Route: &rt, Since: since, Limit: 5})
	wantCode(t, err, model.NotFound)
	// A listing that fails after the issue was read is an error, not a gap.
	r.gl.respond("a", accepted, statusAnswer(500, "", nil))
	_, err = r.historyPage(since, time.Time{}, "", 5)
	wantCode(t, err, model.Transient)
}

// Deletions far before the cursor move the last note to an earlier page: the
// read finds it again and loses nothing.
func TestHistoryStepsBackAfterManyDeletions(t *testing.T) {
	r := newRig(t)
	since := t0.Add(-24 * time.Hour)
	var all []*fakeNote
	for i := range 8 {
		all = append(all, r.gl.seed("a", sandboxProject, 7, 5000003, "n"+strconv.Itoa(i), since.Add(time.Hour+time.Duration(i)*time.Minute)))
	}
	// Pages of two; after three pages the cursor is on page 2, last note 3.
	var cursor string
	var seen []model.Envelope
	for range 2 {
		p, err := r.historyPage(since, time.Time{}, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		seen, cursor = append(seen, p.Envelopes...), p.Next
	}
	for _, n := range all[:2] {
		if _, err := r.del("note:" + strconv.FormatInt(n.ID, 10)); err != nil {
			t.Fatal(err)
		}
	}
	for cursor != "" {
		p, err := r.historyPage(since, time.Time{}, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if p.Gap {
			t.Fatal("gap reported though the last note was found on an earlier page")
		}
		seen, cursor = append(seen, p.Envelopes...), p.Next
	}
	var want []string
	for _, n := range all {
		want = append(want, "note:"+strconv.FormatInt(n.ID, 10))
	}
	if got := ids(seen); !slices.Equal(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
}

// When the last consumed note is itself deleted its place is lost: the read
// reports a gap and repeats rather than skips.
func TestHistoryReportsGapWhenLastNoteIsGone(t *testing.T) {
	r := newRig(t)
	since := t0.Add(-24 * time.Hour)
	var all []*fakeNote
	for i := range 6 {
		all = append(all, r.gl.seed("a", sandboxProject, 7, 5000003, "n"+strconv.Itoa(i), since.Add(time.Hour+time.Duration(i)*time.Minute)))
	}
	p, err := r.historyPage(since, time.Time{}, "", 2)
	if err != nil || p.Next == "" {
		t.Fatalf("page = %+v, %v", p, err)
	}
	if _, err := r.del("note:" + strconv.FormatInt(all[1].ID, 10)); err != nil {
		t.Fatal(err)
	}
	p, err = r.historyPage(since, time.Time{}, p.Next, 10)
	if err != nil || !p.Gap {
		t.Fatalf("page = %+v, %v; want a gap", p, err)
	}
	got := ids(p.Envelopes)
	for _, n := range all[2:] {
		if !slices.Contains(got, "note:"+strconv.FormatInt(n.ID, 10)) {
			t.Fatalf("messages = %v, lost note %d", got, n.ID)
		}
	}
}
