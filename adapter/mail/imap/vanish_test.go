package imap_test

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/adapter/mail/mime"
)

// A message can leave the mailbox between the search, the metadata fetch and
// the body fetch: another session of the account expunges it. The receiver
// tells that from a message it cannot read, and from a connection that fails,
// and moves its cursor only past what it has dealt with.

// vanishRig is a receiver on an in-process server with an observer and the
// cursor kept in a protocol store the test can read.
type vanishRig struct {
	t   *testing.T
	srv *mailServer
	w   *stepWaiter
	obs *observer
	ps  *protocolState
	s   *sink
	run *running
}

func newVanishRig(t *testing.T, srv *mailServer, mutate func(*imap.Config)) *vanishRig {
	t.Helper()
	cfg := config(srv, nil)
	if mutate != nil {
		mutate(&cfg)
	}
	r := &vanishRig{t: t, srv: srv, w: &stepWaiter{}, obs: &observer{}, ps: &protocolState{}, s: newSink()}
	inst := open(t, steppedFactory(t, r.w), channel(channelA, cfg),
		adapter.Dependencies{Credentials: passwordCreds(), Observer: r.obs, Protocol: r.ps})
	r.run = run(t, inst, r.s)
	return r
}

// lastUID is the cursor the receiver stored.
func (r *vanishRig) lastUID() uint32 {
	r.t.Helper()
	x, _, err := r.ps.Load(r.t.Context(), "receiver")
	if err != nil {
		r.t.Fatalf("cursor: %v", err)
	}
	var c struct {
		LastUID uint32 `json:"last_uid"`
	}
	if err := json.Unmarshal(x.Data, &c); err != nil {
		r.t.Fatal(err)
	}
	return c.LastUID
}

// settled waits for the receiver to be waiting for the next round.
func (r *vanishRig) settled() []string {
	r.t.Helper()
	r.w.idle(r.t)
	return messageIDs(r.s.envelopes())
}

func ids(prefix string, n ...int) []string {
	var out []string
	for _, i := range n {
		out = append(out, "<"+prefix+strconv.Itoa(i)+"@example.org>")
	}
	return out
}

func deliverThree(srv *mailServer) {
	for _, id := range ids("m", 1, 2, 3) {
		srv.deliver("INBOX", mail(id, "text of "+id), time.Now())
	}
}

// uidFetch reports whether set holds uid.
func uidFetch(n goimap.NumSet, uid goimap.UID) bool {
	us, ok := n.(goimap.UIDSet)
	return ok && us.Contains(uid)
}

// A message expunged between the metadata fetch and its body fetch is
// skipped, counted as vanished and not as undeliverable; the cursor moves on
// and the messages around it arrive. Both fetches address it by UID: a
// sequence number would name another message once the mailbox shrank.
func TestReceiveVanishedBetweenMetadataAndBody(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	deliverThree(srv)
	srv.onFetch(func(n goimap.NumSet, o *goimap.FetchOptions) bool {
		if len(o.BodySection) > 0 && uidFetch(n, 2) {
			srv.expunge("INBOX", 2)
		}
		return false
	})
	r := newVanishRig(t, srv, nil)
	if got, want := r.settled(), ids("m", 1, 3); !slices.Equal(got, want) {
		t.Fatalf("delivered %v, want %v", got, want)
	}
	if n := r.obs.count(imap.ObservationVanished); n != 1 {
		t.Errorf("vanished observations = %d, want 1", n)
	}
	if n := r.obs.count(imap.ObservationSkipped); n != 0 {
		t.Errorf("skipped observations = %d, want 0: a vanished message is not an undeliverable one", n)
	}
	if got := r.lastUID(); got != 3 {
		t.Errorf("cursor %d, want 3", got)
	}
	r.w.step(t) // a further round neither redelivers nor reports again
	if got := r.settled(); !slices.Equal(got, ids("m", 1, 3)) || r.obs.count(imap.ObservationVanished) != 1 {
		t.Errorf("after another round: delivered %v, vanished %d", got, r.obs.count(imap.ObservationVanished))
	}
	for _, l := range srv.clientLines() {
		if f := strings.Fields(l); len(f) > 2 && strings.EqualFold(f[1], "FETCH") {
			t.Errorf("sequence-number FETCH %q: the receiver addresses messages by UID", l)
		}
	}
	if n := srv.commands("UID"); n < 5 {
		t.Errorf("UID commands = %d, want the search and every fetch by UID", n)
	}
}

// A message expunged between the search and the metadata fetch is a UID the
// server was asked for and answers nothing for: the same outcome. The server
// also volunteers, to the command in progress, an entry for the UID with its
// flags and none of the metadata asked for (another session marked it
// \Deleted first); that entry is not the answer.
func TestReceiveVanishedBetweenSearchAndMetadata(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	deliverThree(srv)
	srv.onFetch(func(n goimap.NumSet, o *goimap.FetchOptions) bool {
		if len(o.BodySection) == 0 && uidFetch(n, 2) {
			srv.expunge("INBOX", 2)
		}
		return false
	})
	r := newVanishRig(t, srv, nil)
	if got, want := r.settled(), ids("m", 1, 3); !slices.Equal(got, want) {
		t.Fatalf("delivered %v, want %v", got, want)
	}
	if n := r.obs.count(imap.ObservationVanished); n != 1 {
		t.Errorf("vanished observations = %d, want 1", n)
	}
	if got := r.lastUID(); got != 3 {
		t.Errorf("cursor %d, want 3", got)
	}
}

// hugeHeaderMail is a message whose header block is over the parser's header
// bound: it cannot be parsed however often it is fetched. (A message nested
// or divided past the structure limits is not this: it is delivered in part.)
func hugeHeaderMail(id string) []byte {
	return []byte(crlf("From: "+peer+"\nTo: "+mailUser+"\nMessage-ID: "+id+"\nX-Pad: ") +
		strings.Repeat("a", mime.DefaultMaxHeaderBytes+1024) + "\r\n\r\nbody\r\n")
}

// A message the adapter cannot parse (a header block over the bound) and a
// message without a usable From address never become deliverable however often
// they are fetched: they are skipped, reported as skipped and not as vanished,
// and the mailbox behind them is delivered, now and in later rounds.
func TestReceiveUnparsableIsSkippedAndReported(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	srv.deliver("INBOX", hugeHeaderMail("<deep@example.org>"), time.Now())
	srv.deliver("INBOX", []byte(crlf("To: "+mailUser+"\nMessage-ID: <nofrom@example.org>\n\nno sender\n")), time.Now())
	srv.deliver("INBOX", mail("<m3@example.org>", "good"), time.Now())
	r := newVanishRig(t, srv, nil)
	if got, want := r.settled(), []string{"<m3@example.org>"}; !slices.Equal(got, want) {
		t.Fatalf("delivered %v, want %v", got, want)
	}
	if n := r.obs.count(imap.ObservationSkipped); n != 2 {
		t.Errorf("skipped observations = %d, want 2", n)
	}
	if n := r.obs.count(imap.ObservationVanished); n != 0 {
		t.Errorf("vanished observations = %d, want 0: the messages are there", n)
	}
	if got := r.lastUID(); got != 3 {
		t.Errorf("cursor %d, want 3", got)
	}
	srv.deliver("INBOX", mail("<m4@example.org>", "later"), time.Now())
	r.w.drive(t, func() bool { return len(r.s.envelopes()) >= 2 })
	if got := r.settled(); !slices.Equal(got, []string{"<m3@example.org>", "<m4@example.org>"}) {
		t.Fatalf("delivered %v, want the next mail to arrive behind the skipped ones", got)
	}
	if n := r.obs.count(imap.ObservationSkipped); n != 2 {
		t.Errorf("skipped observations = %d after later rounds, want 2: each is reported once", n)
	}
}

// A message of no bytes is there, has nothing to deliver and never will: it is
// skipped and reported as skipped, not asked for again and not taken for a
// vanished one.
func TestReceiveEmptyMessageIsSkippedAndReported(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	srv.deliver("INBOX", nil, time.Now())
	srv.deliver("INBOX", mail("<m2@example.org>", "good"), time.Now())
	// The server has nothing to put in the literal of the empty message, as
	// a server may answer for a message of no bytes.
	srv.onFetch(func(n goimap.NumSet, o *goimap.FetchOptions) bool {
		return len(o.BodySection) > 0 && uidFetch(n, 1)
	})
	r := newVanishRig(t, srv, nil)
	if got, want := r.settled(), []string{"<m2@example.org>"}; !slices.Equal(got, want) {
		t.Fatalf("delivered %v, want %v", got, want)
	}
	if n := r.obs.count(imap.ObservationSkipped); n != 1 {
		t.Errorf("skipped observations = %d, want 1", n)
	}
	if n := r.obs.count(imap.ObservationVanished); n != 0 {
		t.Errorf("vanished observations = %d, want 0", n)
	}
	if got := r.lastUID(); got != 2 {
		t.Errorf("cursor %d, want 2", got)
	}
}

// A server that answers nothing for a message it still holds has not said
// the message is gone: the cursor stays before it, the message is asked for
// again (a bounded number of times a round) and delivered once the server
// answers. Nothing is reported as vanished or skipped.
func TestReceiveEmptyAnswerForHeldMessageIsRetried(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	deliverThree(srv)
	var asked atomic.Int32
	srv.onFetch(func(n goimap.NumSet, o *goimap.FetchOptions) bool {
		if len(o.BodySection) > 0 && uidFetch(n, 2) {
			asked.Add(1)
			return true
		}
		return false
	})
	r := newVanishRig(t, srv, nil)
	waitFor(t, func() bool { return asked.Load() >= 3 })
	r.w.idle(t) // the round failed and the receiver backs off
	if got := messageIDs(r.s.envelopes()); !slices.Equal(got, ids("m", 1)) {
		t.Fatalf("delivered %v while the server answers nothing for m2, want only m1", got)
	}
	if got := r.lastUID(); got != 1 {
		t.Fatalf("cursor %d, want 1: not past a message the server still holds", got)
	}
	if n := asked.Load(); n != 3 {
		t.Errorf("m2 asked for %d times in the round, want 3", n)
	}
	srv.onFetch(nil)
	r.w.drive(t, func() bool { return len(r.s.envelopes()) >= 3 })
	if got := r.settled(); !slices.Equal(got, ids("m", 1, 2, 3)) {
		t.Fatalf("delivered %v, want m1 m2 m3 once each", got)
	}
	for _, k := range []string{imap.ObservationVanished, imap.ObservationSkipped} {
		if n := r.obs.count(k); n != 0 {
			t.Errorf("%s observations = %d, want 0", k, n)
		}
	}
}

// A connection that fails while a body is being fetched, before the answer or
// before its end, says nothing about the message: the cursor stays and the
// message is fetched again on the next connection, delivered once.
func TestReceiveConnectionLostDuringBodyFetchKeepsMessage(t *testing.T) {
	for name, lose := range map[string]func(*mailServer){
		"before the answer": func(srv *mailServer) {
			var once atomic.Bool
			srv.onFetch(func(n goimap.NumSet, o *goimap.FetchOptions) bool {
				if len(o.BodySection) > 0 && uidFetch(n, 2) && once.CompareAndSwap(false, true) {
					srv.drop()
				}
				return false
			})
		},
		// The whole body arrives; the closing parenthesis and the tagged OK
		// do not. Bytes in hand are not an answer until the command completes.
		"after the body, before the end of the answer": func(srv *mailServer) { srv.cutAt("text of <m2@example.org>\r\n") },
	} {
		t.Run(name, func(t *testing.T) {
			srv := newMailServer(t, serverOptions{sec: implicitTLS})
			deliverThree(srv)
			lose(srv)
			r := newVanishRig(t, srv, nil)
			r.w.drive(t, func() bool { return len(r.s.envelopes()) >= 3 })
			if got := r.settled(); !slices.Equal(got, ids("m", 1, 2, 3)) {
				t.Fatalf("delivered %v, want m1 m2 m3 once each", got)
			}
			if n := len(srv.loginList()); n < 2 {
				t.Fatalf("logins = %d: the connection was never lost", n)
			}
			if got := r.lastUID(); got != 3 {
				t.Errorf("cursor %d, want 3", got)
			}
			for _, k := range []string{imap.ObservationVanished, imap.ObservationSkipped} {
				if n := r.obs.count(k); n != 0 {
					t.Errorf("%s observations = %d, want 0: a lost connection is not a lost message", k, n)
				}
			}
		})
	}
}
