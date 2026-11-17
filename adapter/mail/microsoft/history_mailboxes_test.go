package microsoft

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func foldersCfg(folders ...string) Config { return Config{HistoryMailboxes: folders} }

// holdIn puts a message received at, under the Internet message ID
// internetID, into folder (a well-known name or a custom folder).
func (e *env) holdIn(folder, id, internetID string, at time.Time) {
	e.holdMail(id, "a@peer.test", at)
	e.g.mu.Lock()
	defer e.g.mu.Unlock()
	fm := e.box.messages[id]
	fm.folder = folder
	fm.m.InternetMessageID = internetID
	if _, known := knownFolders[folder]; !known {
		if e.box.folders == nil {
			e.box.folders = map[string]bool{}
		}
		e.box.folders[folder] = true
	}
}

func TestHistoryMailboxesDefaultIsInbox(t *testing.T) {
	e := newEnv(t, Config{})
	e.holdIn("inbox", "in", "<in@x>", historyEpoch.Add(time.Hour))
	e.holdIn("archive", "ar", "<ar@x>", historyEpoch.Add(2*time.Hour))
	ids, nexts := e.readAll(t, adapter.HistoryRequest{Since: historyEpoch, Limit: 10})
	if !slices.Equal(ids, []string{"in"}) || len(nexts) != 0 {
		t.Fatalf("default history = %q, want only the inbox", ids)
	}
	for _, r := range e.g.requests() {
		if !strings.Contains(r, "/mailFolders/inbox/messages?") {
			t.Fatalf("default read touched %s", r)
		}
	}
}

// Folders are walked in the listed order, each to its end, across pages and
// limits; the cursor names the folder; a folder holding nothing is passed.
func TestHistoryMailboxesWalkFolderByFolder(t *testing.T) {
	e := newEnv(t, foldersCfg("archive", "Custom1", "empty", "inbox"))
	e.holdIn("inbox", "i1", "<i1@x>", historyEpoch.Add(time.Hour))
	e.holdIn("archive", "a2", "<a2@x>", historyEpoch.Add(5*time.Hour))
	e.holdIn("archive", "a1", "<a1@x>", historyEpoch.Add(2*time.Hour))
	e.holdIn("Custom1", "c1", "<c1@x>", historyEpoch.Add(3*time.Hour))
	e.holdIn("junkemail", "j1", "<j1@x>", historyEpoch.Add(3*time.Hour))
	e.holdIn("deleteditems", "t1", "<t1@x>", historyEpoch.Add(3*time.Hour))
	want := []string{"a1", "a2", "c1", "i1"}
	for _, limit := range []int{1, 2, 3, 10} {
		ids, nexts := e.readAll(t, adapter.HistoryRequest{Since: historyEpoch, Limit: limit})
		if !slices.Equal(ids, want) {
			t.Fatalf("limit %d: history = %q, want %q (listed order, each folder oldest first)", limit, ids, want)
		}
		for _, n := range nexts {
			if c := e.cursorOf(t, n); !slices.Contains(e.inst.cfg.HistoryMailboxes, c.Mailbox) {
				t.Fatalf("limit %d: cursor names %q, not a listed mailbox", limit, c.Mailbox)
			}
		}
	}
	for _, r := range e.g.requests() {
		if strings.Contains(r, "junkemail/messages") || strings.Contains(r, "deleteditems/messages") {
			t.Fatalf("history listed a refused folder: %s", r)
		}
	}
}

func (e *env) cursorOf(t *testing.T, c string) historyCursor {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		t.Fatal(err)
	}
	var cur historyCursor
	if err := json.Unmarshal(b, &cur); err != nil {
		t.Fatal(err)
	}
	return cur
}

// A message copied into two listed folders is read in the first only.
func TestHistoryMailboxesCopyDeliveredOnce(t *testing.T) {
	e := newEnv(t, foldersCfg("inbox", "archive"))
	e.holdIn("inbox", "i1", "<same@x>", historyEpoch.Add(time.Hour))
	e.holdIn("archive", "a1", "<same@x>", historyEpoch.Add(time.Hour))
	e.holdIn("archive", "a2", "<other@x>", historyEpoch.Add(2*time.Hour))
	for _, limit := range []int{1, 5} {
		ids, _ := e.readAll(t, adapter.HistoryRequest{Since: historyEpoch, Limit: limit})
		if !slices.Equal(ids, []string{"i1", "a2"}) {
			t.Fatalf("limit %d: history = %q, want i1 a2 (a1 is a copy of i1)", limit, ids)
		}
	}
}

func TestValidateHistoryMailboxes(t *testing.T) {
	f := NewFactory(Options{})
	for _, d := range []string{`{"history_mailboxes":["inbox","archive","AAMkAGI2"]}`, `{"history_mailboxes":[]}`} {
		if issues := f.Validate(ext(d)); len(issues) != 0 {
			t.Errorf("Validate(%s) = %+v, want none", d, issues)
		}
	}
	for _, d := range []string{
		`{"history_mailboxes":["junkemail"]}`, `{"history_mailboxes":["inbox","DeletedItems"]}`, `{"history_mailboxes":["drafts"]}`,
		`{"history_mailboxes":["sentitems"]}`, `{"history_mailboxes":["outbox"]}`, `{"history_mailboxes":["inbox","inbox"]}`,
		`{"history_mailboxes":[""]}`, `{"history_mailboxes":["a/b"]}`,
	} {
		issues := f.Validate(ext(d))
		if !slices.ContainsFunc(issues, func(i model.ValidationIssue) bool { return i.Field == "history_mailboxes" }) {
			t.Errorf("Validate(%s) = %+v, want an issue on history_mailboxes", d, issues)
		}
	}
}

// A folder listed by the ID of a refused well-known folder is refused when
// it is read, before any message is listed.
func TestHistoryMailboxesRefusesJunkByID(t *testing.T) {
	for _, id := range []string{"AAMk-junkemail", "AAMk-deleteditems", "AAMk-sentitems", "AAMk-drafts"} {
		e := newEnv(t, foldersCfg("inbox", id))
		e.holdIn("inbox", "i1", "<i1@x>", historyEpoch.Add(time.Hour))
		_, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historyEpoch, Limit: 10})
		if model.CodeOf(err) != model.Validation {
			t.Errorf("%s: err = %v, want validation", id, err)
		}
	}
}

// A cursor issued under another list, or naming a folder that is not listed,
// is Validation before any call.
func TestHistoryMailboxesForgedCursor(t *testing.T) {
	e := newEnv(t, foldersCfg("inbox", "archive"))
	for n := 1; n <= 3; n++ {
		e.holdIn("inbox", "i"+string(rune('0'+n)), "<i"+string(rune('0'+n))+"@x>", historyEpoch.Add(time.Duration(n)*time.Hour))
	}
	e.holdIn("archive", "a1", "<a1@x>", historyEpoch.Add(10*time.Hour))
	e.holdIn("junkemail", "j1", "<j1@x>", historyEpoch.Add(10*time.Hour))
	req := adapter.HistoryRequest{Since: historyEpoch, Limit: 1}
	p, err := e.inst.History(t.Context(), req)
	if err != nil || p.Next == "" {
		t.Fatalf("first page %+v, %v", p, err)
	}
	calls := len(e.g.requests())
	forge := func(edit func(*historyCursor)) string {
		c := e.cursorOf(t, p.Next)
		edit(&c)
		return c.encode()
	}
	for name, cur := range map[string]string{
		"unlisted junk": forge(func(c *historyCursor) { c.Mailbox = "junkemail" }),
		"unlisted":      forge(func(c *historyCursor) { c.Mailbox = "sentitems" }),
		"no mailbox":    forge(func(c *historyCursor) { c.Mailbox = "" }),
	} {
		req.Cursor = cur
		if _, err := e.inst.History(t.Context(), req); model.CodeOf(err) != model.Validation {
			t.Errorf("%s: err = %v, want validation", name, err)
		}
	}
	if n := len(e.g.requests()); n != calls {
		t.Fatalf("a forged cursor made %d provider calls", n-calls)
	}
	for _, other := range []Config{{}, foldersCfg("inbox"), foldersCfg("archive", "inbox"), foldersCfg("inbox", "archive", "extra")} {
		o := newEnv(t, other)
		req.Cursor = p.Next
		if _, err := o.inst.History(t.Context(), req); model.CodeOf(err) != model.Validation {
			t.Errorf("cursor under %v: err = %v, want validation", other.HistoryMailboxes, err)
		}
	}
	// A cursor of the default read is no cursor of a listed one.
	d := newEnv(t, Config{})
	d.holdMail("m1", "a@peer.test", historyEpoch.Add(time.Hour))
	d.holdMail("m2", "a@peer.test", historyEpoch.Add(2*time.Hour))
	dp, err := d.inst.History(t.Context(), req0(1))
	if err != nil || dp.Next == "" {
		t.Fatalf("default page %+v, %v", dp, err)
	}
	if c := d.cursorOf(t, dp.Next); c.Mailbox != "" {
		t.Fatalf("default cursor names a mailbox %q", c.Mailbox)
	}
}

func req0(limit int) adapter.HistoryRequest {
	return adapter.HistoryRequest{Since: historyEpoch, Limit: limit}
}

func ext(data string) model.Extension {
	return model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: []byte(data)}
}
