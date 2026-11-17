package imap_test

import (
	"crypto/tls"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// held is one message of a mailbox as the server holds it.
type held struct {
	mailbox     string
	uidValidity uint32
	uid         uint32
	flags       []goimap.Flag
	messageID   string
}

func (h held) has(f goimap.Flag) bool {
	for _, x := range h.flags {
		if x == f {
			return true
		}
	}
	return false
}

// dial opens a client session to an implicit-TLS test server, for reading
// what it holds as a second client would.
func (s *mailServer) dial(t testing.TB) *imapclient.Client {
	t.Helper()
	c, err := imapclient.DialTLS(net.JoinHostPort("127.0.0.1", strconv.Itoa(s.port)), &imapclient.Options{
		TLSConfig: &tls.Config{RootCAs: certs(t.(*testing.T)).roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Login(mailUser, mailPass).Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	return c
}

// holds lists what mailbox holds. A mailbox that does not exist holds nothing.
func (s *mailServer) holds(t testing.TB, mailbox string) []held {
	t.Helper()
	c := s.dial(t)
	defer func() { _ = c.Logout().Wait() }()
	sel, err := c.Select(mailbox, &goimap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return nil
	}
	if sel.NumMessages == 0 {
		return nil
	}
	section := &goimap.FetchItemBodySection{Peek: true, Specifier: goimap.PartSpecifierHeader, HeaderFields: []string{"Message-ID"}}
	var all goimap.UIDSet
	all.AddRange(1, 0)
	msgs, err := c.Fetch(all, &goimap.FetchOptions{UID: true, Flags: true, BodySection: []*goimap.FetchItemBodySection{section}}).Collect()
	if err != nil {
		t.Fatalf("fetch %s: %v", mailbox, err)
	}
	var out []held
	for _, m := range msgs {
		_, id, _ := strings.Cut(strings.TrimSpace(string(m.FindBodySection(section))), ":")
		id = strings.TrimSpace(id)
		out = append(out, held{mailbox: mailbox, uidValidity: sel.UIDValidity, uid: uint32(m.UID), flags: m.Flags, messageID: id})
	}
	return out
}

// find returns the message with Message-ID id in mailbox.
func (s *mailServer) find(t testing.TB, mailbox, id string) (held, bool) {
	t.Helper()
	for _, h := range s.holds(t, mailbox) {
		if h.messageID == id {
			return h, true
		}
	}
	return held{}, false
}

// ParseLocation reads a location reference the adapter reports for a moved
// message: imap:///<mailbox>;UIDVALIDITY=<n>/;UID=<n>;MID=<id>.
func parseLocation(id string) (mailbox string, uidValidity, uid uint32, messageID string, ok bool) {
	rest, ok := strings.CutPrefix(id, "imap:///")
	if !ok {
		return "", 0, 0, "", false
	}
	box, rest, ok := strings.Cut(rest, ";UIDVALIDITY=")
	if !ok {
		return "", 0, 0, "", false
	}
	uv, rest, ok := strings.Cut(rest, "/;UID=")
	if !ok {
		return "", 0, 0, "", false
	}
	u, mid, ok := strings.Cut(rest, ";MID=")
	if !ok {
		return "", 0, 0, "", false
	}
	b, err1 := url.PathUnescape(box)
	m, err2 := url.PathUnescape(mid)
	n1, err3 := strconv.ParseUint(uv, 10, 32)
	n2, err4 := strconv.ParseUint(u, 10, 32)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return "", 0, 0, "", false
	}
	return b, uint32(n1), uint32(n2), m, true
}

// role is the destination role of mailbox on the server.
func (s *mailServer) role(mailbox string) string {
	if mailbox == "INBOX" {
		return model.RoleInbox
	}
	for _, a := range s.attrsOf(mailbox) {
		switch a {
		case goimap.MailboxAttrArchive:
			return model.RoleArchive
		case goimap.MailboxAttrTrash:
			return model.RoleTrash
		case goimap.MailboxAttrJunk:
			return model.RoleJunk
		}
	}
	return ""
}

// ItemState is what the account holds for the item ref names. A Message-ID
// is a place in the mailboxes mail arrives in, INBOX and Sent; a location
// is the place it says. A message a move copied elsewhere is no longer held
// under its Message-ID.
func (p *provider) ItemState(ch model.ChannelRef, ref model.ExternalRef) (conformance.ItemState, bool) {
	srv := p.account(ch).srv
	var h held
	var ok bool
	if box, uv, uid, _, isLoc := parseLocation(ref.ID); isLoc {
		for _, m := range srv.holds(p.t, box) {
			if m.uid == uid && m.uidValidity == uv {
				h, ok = m, true
			}
		}
	} else {
		for _, box := range []string{"INBOX", "Sent"} {
			if h, ok = srv.find(p.t, box, ref.ID); ok {
				break
			}
		}
	}
	if !ok {
		return conformance.ItemState{}, false
	}
	role := srv.role(h.mailbox)
	st := conformance.ItemState{
		Archived: role == model.RoleArchive, Trashed: role == model.RoleTrash, Read: h.has(goimap.FlagSeen),
		Folder: &model.Folder{ID: h.mailbox, Name: h.mailbox, Role: role},
	}
	if h.has(goimap.FlagFlagged) {
		st.Flags = []string{model.FlagStar}
	}
	return st, true
}

// DeliverState cannot be expressed: a report of the adapter covers the one
// message it observed, so it is never complete, and the suite's events
// name every item of the conversation. The receiver's reports are covered
// by this package's tests, over the same server.
func (p *provider) DeliverState(model.ChannelRef, conformance.StateEvent) error {
	return conformance.ErrNotExpressible
}

// writeable opens mailbox read-write on a second client of the account, as
// another mail client would.
func (s *mailServer) writeable(t testing.TB, mailbox string) *imapclient.Client {
	t.Helper()
	c := s.dial(t)
	if _, err := c.Select(mailbox, nil).Wait(); err != nil {
		t.Fatalf("select %s: %v", mailbox, err)
	}
	return c
}

// setFlag sets or clears flag on uid of mailbox, as another client.
func (s *mailServer) setFlag(t testing.TB, mailbox string, uid uint32, flag goimap.Flag, on bool) {
	t.Helper()
	op := goimap.StoreFlagsDel
	if on {
		op = goimap.StoreFlagsAdd
	}
	c := s.writeable(t, mailbox)
	if err := c.Store(goimap.UIDSetNum(goimap.UID(uid)), &goimap.StoreFlags{Op: op, Silent: true, Flags: []goimap.Flag{flag}}, nil).Close(); err != nil {
		t.Fatalf("store: %v", err)
	}
}

// moveTo moves uid of from to another mailbox, as another client, and
// returns its UID there.
func (s *mailServer) moveTo(t testing.TB, from string, uid uint32, to string) uint32 {
	t.Helper()
	c := s.writeable(t, from)
	data, err := c.Move(goimap.UIDSetNum(goimap.UID(uid)), to).Wait()
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	nums, _ := data.DestUIDs.(goimap.UIDSet).Nums()
	return uint32(nums[0])
}
