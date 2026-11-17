package livemail

import (
	"crypto/tls"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// Account is a logged-in IMAP session on the live account, used to set up
// mailboxes and mail and to read back what the adapter did. It is not the
// adapter's own connection: it is a second, independent client of the same
// server.
type Account struct {
	t   testing.TB
	env Env
	c   *imapclient.Client
}

// Account connects over implicit TLS (or STARTTLS when the environment has
// no TLS port) and logs in. The session ends with the test.
func (e Env) Account(t testing.TB) *Account {
	t.Helper()
	a := &Account{t: t, env: e}
	a.connect()
	t.Cleanup(func() { a.close() })
	return a
}

func (a *Account) connect() {
	a.t.Helper()
	e := a.env
	cfg := &tls.Config{RootCAs: e.Roots, ServerName: e.IMAPHost, MinVersion: tls.VersionTLS12}
	var c *imapclient.Client
	var err error
	if e.IMAPTLSPort != 0 {
		c, err = imapclient.DialTLS(net.JoinHostPort(e.IMAPHost, strconv.Itoa(e.IMAPTLSPort)), &imapclient.Options{TLSConfig: cfg})
	} else {
		c, err = imapclient.DialStartTLS(net.JoinHostPort(e.IMAPHost, strconv.Itoa(e.IMAPStartTLSPort)), &imapclient.Options{TLSConfig: cfg})
	}
	if err != nil {
		a.t.Fatalf("live IMAP account: connect: %v", err)
	}
	if err := c.Login(e.IMAPUser, e.IMAPPass).Wait(); err != nil {
		_ = c.Close()
		a.t.Fatalf("live IMAP account: login: %v", err)
	}
	a.c = c
}

func (a *Account) close() {
	_ = a.c.Logout().Wait()
	_ = a.c.Close()
}

// reconnect replaces the session: a session that has seen a mailbox deleted
// and created again keeps answering "deleted under us" for it.
func (a *Account) reconnect() {
	a.t.Helper()
	a.close()
	a.connect()
}

// Create creates a mailbox that is deleted when the test ends.
func (a *Account) Create(name string) {
	a.t.Helper()
	if err := a.c.Create(name, nil).Wait(); err != nil {
		a.t.Fatalf("CREATE %s: %v", name, err)
	}
	a.t.Cleanup(func() { _ = a.c.Unselect().Wait(); _ = a.c.Delete(name).Wait() })
}

// Recreate deletes and creates the mailbox again. Dovecot derives a
// mailbox's UIDVALIDITY from its creation time in seconds, so it waits until
// the clock has moved on, and fails the test when the value did not change:
// the caller wants a UIDVALIDITY change, nothing less.
func (a *Account) Recreate(name string) {
	a.t.Helper()
	before := a.UIDValidity(name)
	for range 40 {
		time.Sleep(250 * time.Millisecond)
		_ = a.c.Unselect().Wait()
		if err := a.c.Delete(name).Wait(); err != nil {
			a.t.Fatalf("DELETE %s: %v", name, err)
		}
		if err := a.c.Create(name, nil).Wait(); err != nil {
			a.t.Fatalf("CREATE %s: %v", name, err)
		}
		if after := a.UIDValidity(name); after != before {
			a.reconnect()
			return
		}
	}
	a.t.Fatalf("recreating %s kept UIDVALIDITY %d", name, before)
}

// Append stores raw in the mailbox with the given internal date. A server
// that has just seen the mailbox deleted and created again answers
// TRYCREATE (or "deleted under us") for a moment, while its other sessions
// let go of the old one; Append retries for a few seconds.
func (a *Account) Append(name string, raw []byte, at time.Time) {
	a.t.Helper()
	var err error
	for range 25 {
		if err = a.appendOnce(name, raw, at); err == nil {
			return
		}
		if !strings.Contains(err.Error(), "TRYCREATE") && !strings.Contains(err.Error(), "deleted") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	a.t.Fatalf("APPEND %s: %v", name, err)
}

func (a *Account) appendOnce(name string, raw []byte, at time.Time) error {
	cmd := a.c.Append(name, int64(len(raw)), &imap.AppendOptions{Time: at})
	if _, err := cmd.Write(raw); err != nil {
		return err
	}
	if err := cmd.Close(); err != nil {
		return err
	}
	_, err := cmd.Wait()
	return err
}

// UIDValidity returns the mailbox's UIDVALIDITY.
func (a *Account) UIDValidity(name string) uint32 {
	a.t.Helper()
	d, err := a.c.Status(name, &imap.StatusOptions{UIDValidity: true}).Wait()
	if err != nil {
		a.t.Fatalf("STATUS %s: %v", name, err)
	}
	return d.UIDValidity
}

// Stored is one message of a mailbox as the server holds it.
type Stored struct {
	UID   uint32
	Flags []imap.Flag
	Size  int64
	Raw   []byte
}

// Messages returns every message of the mailbox, read without touching
// flags (BODY.PEEK), in UID order.
func (a *Account) Messages(name string) []Stored {
	a.t.Helper()
	sel, err := a.c.Select(name, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		a.t.Fatalf("EXAMINE %s: %v", name, err)
	}
	if sel.NumMessages == 0 {
		return nil
	}
	var set imap.SeqSet
	set.AddRange(1, 0)
	body := &imap.FetchItemBodySection{Peek: true}
	msgs, err := a.c.Fetch(set, &imap.FetchOptions{UID: true, Flags: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{body}}).Collect()
	if err != nil {
		a.t.Fatalf("FETCH %s: %v", name, err)
	}
	out := make([]Stored, 0, len(msgs))
	for _, m := range msgs {
		s := Stored{UID: uint32(m.UID), Flags: m.Flags, Size: m.RFC822Size}
		for _, b := range m.BodySection {
			s.Raw = b.Bytes
		}
		out = append(out, s)
	}
	return out
}

// Caps returns the capabilities the server advertises to the account.
func (a *Account) Caps() imap.CapSet { return a.c.Caps() }

// SetFlag sets or clears flag on uid of the mailbox, as another client
// would.
func (a *Account) SetFlag(name string, uid uint32, flag imap.Flag, on bool) {
	a.t.Helper()
	if _, err := a.c.Select(name, nil).Wait(); err != nil {
		a.t.Fatalf("SELECT %s: %v", name, err)
	}
	op := imap.StoreFlagsDel
	if on {
		op = imap.StoreFlagsAdd
	}
	if err := a.c.Store(imap.UIDSetNum(imap.UID(uid)), &imap.StoreFlags{Op: op, Silent: true, Flags: []imap.Flag{flag}}, nil).Close(); err != nil {
		a.t.Fatalf("UID STORE %s %d: %v", name, uid, err)
	}
}

// MoveTo moves uid of the mailbox to another, as another client would, and
// returns its UID there.
func (a *Account) MoveTo(name string, uid uint32, dest string) uint32 {
	a.t.Helper()
	if _, err := a.c.Select(name, nil).Wait(); err != nil {
		a.t.Fatalf("SELECT %s: %v", name, err)
	}
	data, err := a.c.Move(imap.UIDSetNum(imap.UID(uid)), dest).Wait()
	if err != nil {
		a.t.Fatalf("UID MOVE %s %d to %s: %v", name, uid, dest, err)
	}
	if u, ok := data.DestUIDs.(imap.UIDSet); ok {
		if nums, ok := u.Nums(); ok && len(nums) > 0 {
			return uint32(nums[0])
		}
	}
	return 0
}

// Expunge deletes uid of the mailbox, as another client would.
func (a *Account) Expunge(name string, uid uint32) {
	a.t.Helper()
	a.SetFlag(name, uid, imap.FlagDeleted, true)
	if err := a.c.UIDExpunge(imap.UIDSetNum(imap.UID(uid))).Close(); err != nil {
		a.t.Fatalf("UID EXPUNGE %s %d: %v", name, uid, err)
	}
}

// Find returns the message of the mailbox whose raw bytes hold needle (a
// Message-ID), and false when there is none. A mailbox that does not exist
// holds nothing.
func (a *Account) Find(name, needle string) (Stored, bool) {
	a.t.Helper()
	if _, err := a.c.Status(name, &imap.StatusOptions{NumMessages: true}).Wait(); err != nil {
		return Stored{}, false
	}
	for _, m := range a.Messages(name) {
		if strings.Contains(string(m.Raw), needle) {
			return m, true
		}
	}
	return Stored{}, false
}

// Forget removes the message holding needle from the mailbox, if it is
// there: the cleanup of a test that moved mail into a mailbox it does not
// own.
func (a *Account) Forget(name, needle string) {
	if m, ok := a.Find(name, needle); ok {
		a.Expunge(name, m.UID)
	}
}
