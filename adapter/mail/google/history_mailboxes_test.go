package google

import (
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func labelCfg(labels ...string) Config { return Config{HistoryMailboxes: labels} }

// With no list the walk is the receive scope: only INBOX mail.
func TestHistoryMailboxesDefaultIsInbox(t *testing.T) {
	e := newEnv(t, Config{})
	at := testNow.Add(-24 * time.Hour)
	e.hold("in", testPeer, at)
	e.hold("starred", testPeer, at.Add(time.Minute), "STARRED")
	e.hold("user", testPeer, at.Add(2*time.Minute), "Label_1")
	if got, _, _ := e.readHistory(t, adapter.HistoryRequest{Limit: 10}); !slices.Equal(got, []string{"in"}) {
		t.Fatalf("default history = %q, want only the inbox message", got)
	}
}

// Listed labels are walked, merged oldest first, a message under two of them
// once, and INBOX only when listed.
func TestHistoryMailboxesWalkListedLabelsOldestFirstOnce(t *testing.T) {
	e := newEnv(t, labelCfg("Label_1", "STARRED"))
	at := testNow.Add(-5 * 24 * time.Hour)
	e.hold("a1", testPeer, at, "Label_1")
	e.hold("s1", testPeer, at.Add(time.Hour), "STARRED")
	e.hold("both", testPeer, at.Add(2*time.Hour), "Label_1", "STARRED")
	e.hold("a2", testPeer, at.Add(3*time.Hour), "Label_1")
	e.hold("s2", testPeer, at.Add(30*time.Hour), "STARRED")
	e.hold("inbox", testPeer, at.Add(time.Minute))
	e.hold("other", testPeer, at.Add(time.Minute), "Label_2")
	e.hold("spam", testPeer, at.Add(time.Minute), "Label_1", labelSpam)
	e.hold("trash", testPeer, at.Add(time.Minute), "STARRED", labelTrash)
	e.hold("sent", testPeer, at.Add(time.Minute), "Label_1", labelSent)
	e.hold("draft", testPeer, at.Add(time.Minute), "Label_1", labelDraft)
	want := []string{"a1", "s1", "both", "a2", "s2"}
	for _, limit := range []int{1, 2, 3, 100} {
		got, _, gap := e.readHistory(t, adapter.HistoryRequest{Limit: limit})
		if !slices.Equal(got, want) || gap {
			t.Fatalf("limit %d: history = %q gap %v, want %q", limit, got, gap, want)
		}
	}
	// INBOX, when listed, is walked with the rest.
	e = newEnv(t, labelCfg("Label_1", labelInbox))
	e.hold("x", testPeer, at, "Label_1")
	e.hold("y", testPeer, at.Add(time.Hour))
	e.hold("z", testPeer, at.Add(2*time.Hour), labelInbox, "Label_1")
	if got, _, _ := e.readHistory(t, adapter.HistoryRequest{Limit: 2}); !slices.Equal(got, []string{"x", "y", "z"}) {
		t.Fatalf("history = %q, want x y z", got)
	}
}

// A sender route still narrows a multi-label read.
func TestHistoryMailboxesRouteNarrows(t *testing.T) {
	e := newEnv(t, labelCfg("Label_1", "Label_2"))
	at := testNow.Add(-24 * time.Hour)
	e.hold("mine", testPeer, at, "Label_1")
	e.hold("theirs", "someone@remote.test", at.Add(time.Minute), "Label_2")
	rt := route(testPeer, "")
	if got, _, _ := e.readHistory(t, adapter.HistoryRequest{Limit: 10, Route: &rt}); !slices.Equal(got, []string{"mine"}) {
		t.Fatalf("history = %q, want mine", got)
	}
}

func TestValidateHistoryMailboxes(t *testing.T) {
	f := NewFactory(Options{})
	for _, d := range []string{`{"history_mailboxes":["INBOX","Label_12","STARRED"]}`, `{"history_mailboxes":[]}`} {
		if issues := f.Validate(ext(d)); len(issues) != 0 {
			t.Errorf("Validate(%s) = %+v, want none", d, issues)
		}
	}
	for _, d := range []string{
		`{"history_mailboxes":["SPAM"]}`, `{"history_mailboxes":["INBOX","trash"]}`, `{"history_mailboxes":["Draft"]}`,
		`{"history_mailboxes":["SENT"]}`, `{"history_mailboxes":[""]}`, `{"history_mailboxes":["a b"]}`,
	} {
		issues := f.Validate(ext(d))
		if !slices.ContainsFunc(issues, func(i model.ValidationIssue) bool { return i.Field == "history_mailboxes" }) {
			t.Errorf("Validate(%s) = %+v, want an issue on history_mailboxes", d, issues)
		}
	}
}

// A cursor issued under another label list is Validation, before any call;
// a cursor that names messages outside the listed labels delivers none.
func TestHistoryMailboxesForgedCursor(t *testing.T) {
	e := newEnv(t, labelCfg("Label_1"))
	at := testNow.Add(-24 * time.Hour)
	e.hold("a", testPeer, at, "Label_1")
	e.hold("b", testPeer, at.Add(time.Minute), "Label_1")
	e.hold("c", testPeer, at.Add(2*time.Minute), "Label_1")
	e.hold("unlisted", testPeer, at, "Label_2")
	e.hold("inbox", testPeer, at)
	req := adapter.HistoryRequest{Since: historySince, Limit: 1}
	p, err := e.inst.History(t.Context(), req)
	if err != nil || p.Next == "" {
		t.Fatalf("first page %+v, %v", p, err)
	}
	for _, other := range []Config{{}, labelCfg("Label_2"), labelCfg("Label_1", "Label_2")} {
		o := newEnv(t, other)
		req.Cursor = p.Next
		calls := len(o.g.requests())
		if _, err := o.inst.History(t.Context(), req); model.CodeOf(err) != model.Validation {
			t.Errorf("cursor under %v: err = %v, want validation", other.HistoryMailboxes, err)
		}
		if len(o.g.requests()) != calls {
			t.Errorf("cursor under %v made provider calls", other.HistoryMailboxes)
		}
	}
	req.Cursor = e.forged(t, p.Next, func(c *historyCursor) { c.Rest = []string{"unlisted", "inbox", "c"} })
	req.Limit = 10
	p, err = e.inst.History(t.Context(), req)
	if err != nil || !slices.Equal(envIDs(p), []string{"c"}) {
		t.Fatalf("forged rest delivered %q, %v; want only c", envIDs(p), err)
	}
}
