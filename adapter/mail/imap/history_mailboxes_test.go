package imap_test

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	goimap "github.com/emersion/go-imap/v2"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/model"
)

// readAllHistory reads req to its end and returns the envelopes' message IDs
// and the pages' cursors.
func readAllHistory(t *testing.T, inst *imap.Instance, req adapter.HistoryRequest) (ids, nexts []string, envs []model.Envelope) {
	t.Helper()
	for range 50 {
		p := history(t, inst, req)
		if len(p.Envelopes) > req.Limit {
			t.Fatalf("page of %d envelopes over limit %d", len(p.Envelopes), req.Limit)
		}
		ids = append(ids, messageIDs(p.Envelopes)...)
		envs = append(envs, p.Envelopes...)
		if p.Next == "" {
			return ids, nexts, envs
		}
		nexts = append(nexts, p.Next)
		req.Cursor = p.Next
	}
	t.Fatal("window not read")
	return
}

func mboxesCfg(names ...string) func(*imap.Config) {
	return func(c *imap.Config) { c.HistoryMailboxes = names }
}

func TestHistoryMailboxesDefaultIsReceiveMailbox(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	srv.create("Archive")
	srv.deliver("INBOX", mail("<in@example.org>", "x"), at(10, 0))
	srv.deliver("Archive", mail("<ar@example.org>", "x"), at(11, 0))
	inst := histInstance(t, srv, nil, adapter.Dependencies{})
	if ids, _, _ := readAllHistory(t, inst, adapter.HistoryRequest{Since: at(0, 0), Limit: 10}); !slices.Equal(ids, []string{"<in@example.org>"}) {
		t.Fatalf("default history = %v, want only INBOX", ids)
	}
	for _, l := range srv.clientLines() {
		if strings.Contains(l, "Archive") {
			t.Errorf("default read touched Archive: %q", l)
		}
	}
}

// Mailboxes are walked in the listed order, each to its end, whatever the
// limit; the cursor names the mailbox being read, always a listed one.
func TestHistoryMailboxesWalkInListedOrder(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	srv.create("Archive")
	srv.create("Projects/2026")
	srv.create("Empty")
	srv.create("Trash")
	srv.deliver("INBOX", mail("<i1@example.org>", "x"), at(10, 0))
	srv.deliver("INBOX", mail("<i2@example.org>", "x"), at(13, 0))
	srv.deliver("Archive", mail("<a1@example.org>", "x"), at(11, 0))
	srv.deliver("Archive", mail("<a2@example.org>", "x"), at(14, 0))
	srv.deliver("Projects/2026", mail("<p1@example.org>", "x"), at(12, 0))
	srv.deliver("Trash", mail("<t1@example.org>", "x"), at(12, 0))
	listed := []string{"Archive", "Empty", "Projects/2026", "INBOX"}
	inst := histInstance(t, srv, mboxesCfg(listed...), adapter.Dependencies{})
	want := []string{"<a1@example.org>", "<a2@example.org>", "<p1@example.org>", "<i1@example.org>", "<i2@example.org>"}
	for _, limit := range []int{1, 2, 3, 100} {
		ids, nexts, _ := readAllHistory(t, inst, adapter.HistoryRequest{Since: at(0, 0), Limit: limit})
		if !slices.Equal(ids, want) {
			t.Fatalf("limit %d: history = %v, want %v", limit, ids, want)
		}
		for _, n := range nexts {
			b, _ := base64.RawURLEncoding.DecodeString(n)
			var c struct{ Mailbox string }
			_ = json.Unmarshal(b, &c)
			if !slices.Contains(listed, c.Mailbox) {
				t.Fatalf("limit %d: cursor names %q, not a listed mailbox", limit, c.Mailbox)
			}
		}
	}
	for _, l := range srv.clientLines() {
		if strings.Contains(l, "Trash") {
			t.Errorf("an unlisted mailbox was touched: %q", l)
		}
	}
}

// A message copied into two listed mailboxes yields the same event ID, so the
// sink stores it once whichever mailbox delivered it first.
func TestHistoryMailboxesCopyHasOneEventID(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	srv.create("Archive")
	raw := mail("<same@example.org>", "x")
	srv.deliver("INBOX", raw, at(10, 0))
	srv.deliver("Archive", raw, at(10, 0))
	inst := histInstance(t, srv, mboxesCfg("INBOX", "Archive"), adapter.Dependencies{})
	_, _, envs := readAllHistory(t, inst, adapter.HistoryRequest{Since: at(0, 0), Limit: 10})
	if len(envs) != 2 || envs[0].EventID == "" || envs[0].EventID != envs[1].EventID {
		t.Fatalf("envelopes %+v, want two with one event ID", envs)
	}
}

func TestHistoryMailboxesConfigRefusals(t *testing.T) {
	f := imap.NewFactory(imap.Options{})
	for _, c := range []struct {
		name string
		edit func(*imap.Config)
	}{
		{"sent", mboxesCfg("INBOX", "Sent")},
		{"sent items", mboxesCfg("Sent Items")},
		{"drafts", mboxesCfg("Drafts")},
		{"gmail spam", mboxesCfg("[Gmail]/Spam")},
		{"nested junk", mboxesCfg("INBOX.Junk")},
		{"trash", mboxesCfg("trash")},
		{"deleted", mboxesCfg("Deleted Items")},
		{"configured sent", func(c *imap.Config) { c.Sent.Mailbox = "Outgoing"; c.HistoryMailboxes = []string{"outgoing"} }},
		{"repeat", mboxesCfg("Archive", "Archive")},
		{"empty", mboxesCfg("")},
		{"wildcard", mboxesCfg("Arch*")},
	} {
		cfg := validConfig()
		c.edit(&cfg)
		issues := f.Validate(cfg.Extension())
		if !slices.ContainsFunc(issues, func(i model.ValidationIssue) bool { return i.Field == "history_mailboxes" }) {
			t.Errorf("%s: issues %+v, want one on history_mailboxes", c.name, issues)
		}
	}
	ok := validConfig()
	ok.HistoryMailboxes = []string{"INBOX", "Archive", "Projects/2026"}
	if issues := f.Validate(ok.Extension()); len(issues) != 0 {
		t.Errorf("valid list: %+v", issues)
	}
}

// A listed mailbox the server marks junk, trash, drafts or sent is refused
// when read, before any message is searched.
func TestHistoryMailboxesRefuseSpecialUseAtRead(t *testing.T) {
	for _, attr := range []goimap.MailboxAttr{goimap.MailboxAttrJunk, goimap.MailboxAttrTrash, goimap.MailboxAttrDrafts, goimap.MailboxAttrSent} {
		srv := newMailServer(t, serverOptions{sec: implicitTLS, state: true})
		srv.create("Odd", attr)
		srv.deliver("INBOX", mail("<i1@example.org>", "x"), at(10, 0))
		srv.deliver("Odd", mail("<o1@example.org>", "x"), at(11, 0))
		inst := histInstance(t, srv, mboxesCfg("INBOX", "Odd"), adapter.Dependencies{})
		_, err := inst.History(t.Context(), adapter.HistoryRequest{Since: at(0, 0), Limit: 10})
		if model.CodeOf(err) != model.Validation {
			t.Errorf("%s: err = %v, want validation", attr, err)
		}
	}
}

// A cursor naming a mailbox that is not listed, or issued under another
// list, is Validation, and the mailbox is never selected.
func TestHistoryMailboxesForgedCursor(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	srv.create("Archive")
	srv.create("Secret")
	for n := range 3 {
		srv.deliver("INBOX", mail("<g"+string(rune('1'+n))+"@example.org>", "x"), at(10+n, 0))
	}
	srv.deliver("Secret", mail("<secret@example.org>", "x"), at(11, 0))
	inst := histInstance(t, srv, mboxesCfg("INBOX", "Archive"), adapter.Dependencies{})
	req := adapter.HistoryRequest{Since: at(0, 0), Limit: 1}
	p := history(t, inst, req)
	if p.Next == "" {
		t.Fatal("no cursor")
	}
	req.Cursor = forge(t, p.Next, func(m map[string]any) { m["mailbox"] = "Secret" })
	if _, err := inst.History(t.Context(), req); model.CodeOf(err) != model.Validation {
		t.Fatalf("unlisted mailbox: err %v, want validation", err)
	}
	for _, l := range srv.clientLines() {
		if strings.Contains(l, "Secret") {
			t.Errorf("client line %q touches an unlisted mailbox", l)
		}
	}
	for name, tweak := range map[string]func(*imap.Config){"default": nil, "other order": mboxesCfg("Archive", "INBOX"), "wider": mboxesCfg("INBOX", "Archive", "Secret")} {
		other := histInstance(t, srv, tweak, adapter.Dependencies{})
		req.Cursor = p.Next
		if _, err := other.History(t.Context(), req); model.CodeOf(err) != model.Validation {
			t.Errorf("cursor under %s: err %v, want validation", name, err)
		}
	}
	// A position in a mailbox, with no UIDVALIDITY, is no cursor.
	req.Cursor = forge(t, p.Next, func(m map[string]any) { m["uid_validity"] = 0 })
	if _, err := inst.History(t.Context(), req); model.CodeOf(err) != model.Validation {
		t.Errorf("no uid_validity: err %v, want validation", err)
	}
}
