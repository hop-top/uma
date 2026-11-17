package imap_test

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// unnormalizableFrom is mail the mailbox holds and the adapter can never
// deliver: its From header gives no sender that model.NormalizeEmail accepts
// as a bare address, or none at all. The bytes of a message never change, so
// fetching one again fails alike.
var unnormalizableFrom = []struct{ name, from string }{
	{"quoted local part with a space", `From: "john doe"@example.org` + "\n"},
	{"quoted local part with an at sign", `From: "a@b"@example.org` + "\n"},
	{"unparseable address", "From: bob@\n"},
	{"not an address", "From: not an address\n"},
	{"empty group", "From: Undisclosed:;\n"},
	{"From absent", ""},
}

// badMail is a message with the given From line (empty for none) that is
// otherwise well formed, so only its sender decides its fate.
func badMail(id, fromLine string) []byte {
	return []byte(crlf(fromLine + "To: " + mailUser + "\nSubject: needs an answer\nMessage-ID: " + id +
		"\nDate: Sun, 11 Oct 2026 09:00:00 +0000\n\nplease read this\n"))
}

// receiveStoppable is engineRig.receive with a stop: a restarted receiver
// is another Runtime.Receive on the same engine and store.
func (r *engineRig) receiveStoppable() (stop func()) {
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.engine.Runtime().Receive(ctx, runtimeCall, channelA)
	}()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	r.t.Cleanup(stop)
	return stop
}

// storedCursorUID is the highest UID the receiver cursor the engine keeps for
// the channel has passed.
func (r *engineRig) storedCursorUID() uint32 {
	r.t.Helper()
	var st store.ProtocolState
	if err := r.store.View(r.ctx, tenant, func(rd store.Reader) error {
		var err error
		st, err = rd.Protocol(r.ctx, channelA, "receiver")
		return err
	}); err != nil {
		r.t.Fatalf("cursor: %v", err)
	}
	var c struct {
		LastUID uint32 `json:"last_uid"`
	}
	if err := json.Unmarshal(st.Value.Data, &c); err != nil {
		r.t.Fatal(err)
	}
	return c.LastUID
}

// recordedPosition is the mailbox position a delivered message carries in its
// metadata: the UID and UIDVALIDITY an operator matches against the mailbox.
func recordedPosition(t *testing.T, m model.Message) (uid, uidValidity uint32) {
	t.Helper()
	for _, x := range m.Metadata {
		if x.Name != imap.MessageSchema {
			continue
		}
		var rec struct {
			UID         uint32 `json:"uid"`
			UIDValidity uint32 `json:"uid_validity"`
		}
		if err := json.Unmarshal(x.Data, &rec); err != nil {
			t.Fatal(err)
		}
		return rec.UID, rec.UIDValidity
	}
	t.Fatalf("message %v carries no %s metadata", m.Ref, imap.MessageSchema)
	return 0, 0
}

// TestEngineUnnormalizableFromIsSkippedAndNeverStored: a message whose From
// does not normalize is never stored and never reaches the sink; it is
// reported as skipped once; the message behind it is delivered under its own
// sender with a mailbox position that leaves the skipped UID as the gap; the
// cursor moves past the bad message; and a restarted receiver neither
// delivers the bad message nor looks at it again.
func TestEngineUnnormalizableFromIsSkippedAndNeverStored(t *testing.T) {
	for _, tc := range unnormalizableFrom {
		t.Run(tc.name, func(t *testing.T) {
			r := newEngineRig(t, passwordHost(), nil)
			r.srv.deliver("INBOX", badMail("<bad@example.org>", tc.from), time.Now())
			r.srv.deliver("INBOX", mail("<good1@example.org>", "good one"), time.Now())
			stop := r.receiveStoppable()
			msgs := r.awaitMessages(1)

			if len(msgs) != 1 || byProviderID(msgs, "<good1@example.org>") == nil {
				t.Fatalf("stored %d messages, want only the good one: the bad one is never stored", len(msgs))
			}
			if a := msgs[0].Author.External; a.ID != peer || a.Namespace != imap.Namespace {
				t.Errorf("good message author %+v, want %s under %s", a, peer, imap.Namespace)
			}
			if uid, validity := recordedPosition(t, msgs[0]); uid != 2 || validity != r.srv.uidValidity("INBOX") {
				t.Errorf("good message position uid %d validity %d, want uid 2: the skipped UID 1 is the gap", uid, validity)
			}
			if n := r.obs.count(imap.ObservationSkipped); n != 1 {
				t.Errorf("skipped observations = %d, want 1", n)
			}
			if n := r.obs.count(imap.ObservationVanished); n != 0 {
				t.Errorf("vanished observations = %d, want 0: the message is there", n)
			}
			if n := r.obs.count(ports.ObservationIngest); n != 1 {
				t.Errorf("ingest calls = %d, want 1: the bad message never reaches the sink", n)
			}
			if uid := r.storedCursorUID(); uid != 2 {
				t.Errorf("cursor at UID %d, want 2: past the bad message", uid)
			}
			if n := r.srv.count("INBOX"); n != 2 {
				t.Errorf("mailbox holds %d messages, want 2: the receiver never removes mail", n)
			}

			// A restarted receiver resumes from the stored cursor: the bad
			// message is neither delivered nor examined, and mail behind it
			// still arrives.
			stop()
			r.srv.deliver("INBOX", mail("<good2@example.org>", "good two"), time.Now())
			r.receiveStoppable()
			msgs = r.awaitMessages(2)
			if len(msgs) != 2 || byProviderID(msgs, "<good2@example.org>") == nil {
				t.Fatalf("after restart stored %d messages, want the two good ones", len(msgs))
			}
			if n := r.obs.count(imap.ObservationSkipped); n != 1 {
				t.Errorf("skipped observations = %d after restart, want 1: the bad message is not looked at again", n)
			}
			if n := r.obs.count(ports.ObservationIngest); n != 2 {
				t.Errorf("ingest calls = %d after restart, want 2", n)
			}
			if uid := r.storedCursorUID(); uid != 3 {
				t.Errorf("cursor at UID %d after restart, want 3", uid)
			}
		})
	}
}

// TestReceiveCursorResetDoesNotRecoverUnnormalizableFrom: an operator cannot
// bring such a message into uma by resetting the cursor. A receiver with no
// cursor synchronizes the initial window again; the message is still in the
// mailbox, is fetched, and fails alike. Only mail uma can deliver is
// delivered again (ingress drops the copies it holds by their bytes).
func TestReceiveCursorResetDoesNotRecoverUnnormalizableFrom(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	srv.deliver("INBOX", badMail("<bad@example.org>", `From: "john doe"@example.org`+"\n"), time.Now())
	srv.deliver("INBOX", mail("<good1@example.org>", "good one"), time.Now())
	first := newVanishRig(t, srv, nil)
	if got, want := first.settled(), []string{"<good1@example.org>"}; !slices.Equal(got, want) {
		t.Fatalf("delivered %v, want %v", got, want)
	}
	if n := first.obs.count(imap.ObservationSkipped); n != 1 {
		t.Fatalf("skipped observations = %d, want 1", n)
	}
	_ = first.run.stop()

	// The cursor is gone: a fresh protocol store, as when its row is deleted.
	second := newVanishRig(t, srv, nil)
	if got, want := second.settled(), []string{"<good1@example.org>"}; !slices.Equal(got, want) {
		t.Fatalf("after the reset delivered %v, want %v: the bad message is not recovered", got, want)
	}
	if n := second.obs.count(imap.ObservationSkipped); n != 1 {
		t.Errorf("skipped observations = %d after the reset, want 1: fetched again, skipped again", n)
	}
	if got := second.lastUID(); got != 2 {
		t.Errorf("cursor %d after the reset, want 2", got)
	}
}
