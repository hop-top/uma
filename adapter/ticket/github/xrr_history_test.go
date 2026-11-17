package github_test

import (
	"path/filepath"
	"slices"
	"testing"

	"hop.top/uma/adapter"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/testing/xrrtest"
)

var historyCassettes = filepath.Join("testdata", "cassettes", "github", "history")

// TestRecordedHistory reads back three comments it posts on the sandbox
// issue: the window starts at the first comment's creation time, read
// back from GitHub, so recording and replay read the same window. It reads
// the issue's window in pages of two (the comments listing pages through
// its Link header) and the repository's window channel-wide, then the
// window of issue 999999, which does not exist (GitHub's 404). It deletes
// its three comments at the end, so the issue is left as it was; the
// checks report with Errorf so the deletions always run.
func TestRecordedHistory(t *testing.T) {
	cfg := github.Config{Replies: true, AccountID: sandboxAccount, HistoryRepositories: []string{sandboxRepo}}
	inst := recordedChannel(t, historyCassettes, cfg, portstest.NewFixedClock(t0), func(gh *fakeGitHub) {
		gh.setIssue("a", sandboxRepo, sandboxIssue, sandboxAccount, "open", nil, t0)
	}, xrrtest.WithHeaders("Link"))
	route := sandboxRoute()
	var sent []model.ExternalRef
	defer func() {
		for i, ref := range sent {
			e := mutEffect(tryOne, &ref)
			e.Route = route
			if o, err := inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: e}); err != nil || o.State != model.ProviderAccepted {
				t.Errorf("Delete comment %d = %+v, %v", i, o, err)
			}
		}
	}()
	for _, text := range []string{"Recorded history one.", "Recorded history two.", "Recorded history three."} {
		req := sendReq(tryOne, text)
		req.Route = route
		out, err := inst.(adapter.Sender).Send(t.Context(), req)
		if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
			t.Errorf("Send = %+v, %v", out, err)
			return
		}
		sent = append(sent, out.References[0])
	}
	first, err := inst.(adapter.Refetcher).Refetch(t.Context(), adapter.RefetchRequest{Target: sent[0], Destination: sandboxRepo, References: []model.ExternalRef{route}})
	if err != nil || first.OccurredAt.IsZero() {
		t.Errorf("Refetch first comment = %+v, %v", first, err)
		return
	}
	since := first.OccurredAt
	var want []string
	for _, ref := range sent {
		want = append(want, ref.ID)
	}
	read := func(r *model.ExternalRef) ([]string, int) {
		t.Helper()
		var ids []string
		cursor, pages := "", 0
		for pages < 10 {
			p, err := inst.(adapter.HistoryReader).History(t.Context(), adapter.HistoryRequest{Route: r, Since: since, Cursor: cursor, Limit: 2})
			if err != nil {
				t.Errorf("History(cursor %q) = %v", cursor, err)
				return ids, pages
			}
			pages++
			for _, e := range p.Envelopes {
				ids = append(ids, e.MessageID)
				if e.Destination != sandboxRepo || len(e.References) == 0 || e.References[0] != route || !slices.Contains(e.References, model.EchoRef(chanA, opA, tryOne)) {
					t.Errorf("history envelope %+v, want the sandbox issue's own comment", e)
				}
			}
			if cursor = p.Next; cursor == "" {
				break
			}
		}
		return ids, pages
	}
	if ids, pages := read(&route); !slices.Equal(ids, want) || pages < 2 {
		t.Errorf("issue history = %q in %d pages, want %q in pages of two", ids, pages, want)
	}
	if ids, _ := read(nil); !slices.Equal(ids, want) {
		t.Errorf("channel history = %q, want %q", ids, want)
	}
	missing := issueRoute("999999")
	if _, err := inst.(adapter.HistoryReader).History(t.Context(), adapter.HistoryRequest{Route: &missing, Since: since, Limit: 2}); model.CodeOf(err) != model.NotFound {
		t.Errorf("History of a missing issue = %v, want not_found", err)
	}
}
