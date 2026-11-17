package imap_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
)

// TestReceiveDeliversNewMail: new messages reach the sink in UID order,
// normalized, and examining the mailbox never marks them \Seen.
func TestReceiveDeliversNewMail(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	srv.deliver("INBOX", mail("<m1@example.org>", "first"), time.Now())
	reply := crlf("From: " + peer + "\nTo: " + mailUser + "\nSubject: Re: hello\nMessage-ID: <m2@example.org>\n" +
		"In-Reply-To: <out-1@example.com>\nReferences: <root@example.com> <out-1@example.com>\n\nsecond\n")
	srv.deliver("INBOX", []byte(reply), time.Now())
	inst := open(t, steppedFactory(t, &stepWaiter{}), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: passwordCreds()})
	s := newSink()
	run(t, inst, s)
	got := s.await(t, 2)

	if ids := messageIDs(got); !slices.Equal(ids, []string{"<m1@example.org>", "<m2@example.org>"}) {
		t.Fatalf("delivered %v, want m1 then m2", ids)
	}
	env := got[0]
	wantSender := model.ExternalRef{Channel: channelA, Namespace: imap.Namespace, Kind: imap.RouteKind, ID: peer}
	if env.Sender != wantSender || env.Destination != mailUser || env.Kind != model.EnvelopeMessage {
		t.Errorf("envelope sender %+v destination %q kind %q", env.Sender, env.Destination, env.Kind)
	}
	if len(env.Content.Parts) == 0 || env.Content.Parts[0].Text != "first" {
		t.Errorf("content %+v, want text first", env.Content)
	}
	if env.Audience.Visibility != model.Public {
		t.Errorf("audience %+v, want public", env.Audience)
	}
	var rec struct {
		Mailbox     string `json:"mailbox"`
		UIDValidity uint32 `json:"uid_validity"`
		UID         uint32 `json:"uid"`
	}
	if env.Payload.Name != imap.MessageSchema || json.Unmarshal(env.Payload.Data, &rec) != nil ||
		rec.Mailbox != "INBOX" || rec.UID != 1 || rec.UIDValidity != srv.uidValidity("INBOX") {
		t.Errorf("payload %s %s, want the mailbox position", env.Payload.Name, env.Payload.Data)
	}
	var kinds []string
	for _, r := range got[1].References {
		kinds = append(kinds, r.Kind+"="+r.ID)
	}
	want := []string{
		model.RefKindEmailMessageID + "=<m2@example.org>", model.RefKindEmailInReplyTo + "=<out-1@example.com>",
		model.RefKindEmailReferences + "=<root@example.com>", model.RefKindEmailReferences + "=<out-1@example.com>",
	}
	if !slices.Equal(kinds, want) {
		t.Errorf("thread references %v, want %v", kinds, want)
	}
	if got[1].InReplyTo != "<out-1@example.com>" || got[0].InReplyTo != "" {
		t.Errorf("reply parents %q, %q; want the In-Reply-To msg-id and none", got[1].InReplyTo, got[0].InReplyTo)
	}
	if n := srv.seen("INBOX"); n != 0 {
		t.Errorf("%d messages marked \\Seen by the receiver, want 0", n)
	}
}

// TestReceiveAutomatedAndUndeliverable: Auto-Submitted mail is automated;
// a message without a From address is skipped and the receiver moves on.
func TestReceiveAutomatedAndUndeliverable(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	srv.deliver("INBOX", []byte(crlf("To: "+mailUser+"\nMessage-ID: <nofrom@example.org>\n\nno sender\n")), time.Now())
	srv.deliver("INBOX", []byte(crlf("From: "+peer+"\nMessage-ID: <auto@example.org>\nAuto-Submitted: auto-replied\n\naway\n")), time.Now())
	w := &stepWaiter{}
	inst := open(t, steppedFactory(t, w), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: passwordCreds()})
	s := newSink()
	run(t, inst, s)
	w.idle(t)
	got := s.envelopes()
	if len(got) != 1 || got[0].MessageID != "<auto@example.org>" || got[0].Kind != model.EnvelopeAutomatedMessage {
		t.Fatalf("delivered %+v, want only the automated message", messageIDs(got))
	}
}

// TestInitialSyncWindow: the first synchronization delivers only messages
// received within the window; later mail is new whatever its date.
func TestInitialSyncWindow(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS, noIdle: true})
	srv.deliver("INBOX", mail("<old@example.org>", "old"), time.Now().Add(-48*time.Hour))
	srv.deliver("INBOX", mail("<recent@example.org>", "recent"), time.Now().Add(-10*time.Minute))
	w := &stepWaiter{}
	inst := open(t, steppedFactory(t, w), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: passwordCreds()})
	s := newSink()
	run(t, inst, s)
	w.idle(t) // first synchronization done
	srv.deliver("INBOX", mail("<new@example.org>", "new"), time.Now().Add(-72*time.Hour))
	w.step(t) // next poll
	w.idle(t)
	if ids := messageIDs(s.envelopes()); !slices.Equal(ids, []string{"<recent@example.org>", "<new@example.org>"}) {
		t.Fatalf("delivered %v, want recent then new (old is outside the window; new arrived after sync)", ids)
	}
}

// TestIdleWakesOnNewMail: in IDLE, a new message is delivered on the
// server's notification alone: the IDLE restart never comes.
func TestIdleWakesOnNewMail(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	w := &stepWaiter{}
	inst := open(t, steppedFactory(t, w), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: passwordCreds()})
	s := newSink()
	run(t, inst, s)
	w.idle(t)
	waitFor(t, func() bool { return srv.commands("IDLE") >= 1 })
	srv.deliver("INBOX", mail("<m1@example.org>", "hi"), time.Now())
	s.await(t, 1)
	w.idle(t)
	if n := srv.commands("IDLE"); n != 2 {
		t.Fatalf("IDLE issued %d times, want 2: one wake-up, one round, IDLE again", n)
	}
}

// TestMailDuringRoundWakesIdle: mail that arrives after a round searched
// the mailbox, and before IDLE starts, is delivered without waiting for the
// IDLE restart.
func TestMailDuringRoundWakesIdle(t *testing.T) {
	var once sync.Once
	var srv *mailServer
	srv = newMailServer(t, serverOptions{sec: implicitTLS, afterSearch: func() {
		once.Do(func() { srv.deliver("INBOX", mail("<late@example.org>", "late"), time.Now()) })
	}})
	srv.deliver("INBOX", mail("<m1@example.org>", "first"), time.Now())
	inst := open(t, steppedFactory(t, &stepWaiter{}), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: passwordCreds()})
	s := newSink()
	run(t, inst, s)
	if ids := messageIDs(s.await(t, 2)); !slices.Equal(ids, []string{"<m1@example.org>", "<late@example.org>"}) {
		t.Fatalf("delivered %v", ids)
	}
}

// TestMailDuringExamine: mail that arrives after the EXAMINE snapshot is
// reported to the session afterwards and delivered without an IDLE restart.
func TestMailDuringExamine(t *testing.T) {
	var srv *mailServer
	var arm sync.Once
	armed := make(chan struct{})
	srv = newMailServer(t, serverOptions{sec: implicitTLS, afterSelect: func() {
		select {
		case <-armed:
			arm.Do(func() { srv.deliver("INBOX", mail("<during@example.org>", "during"), time.Now()) })
		default:
		}
	}})
	srv.deliver("INBOX", mail("<m1@example.org>", "first"), time.Now())
	w := &stepWaiter{}
	inst := open(t, steppedFactory(t, w), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: passwordCreds()})
	s := newSink()
	run(t, inst, s)
	s.await(t, 1)
	close(armed)
	w.step(t) // IDLE restart: the next EXAMINE sees the new mail arrive
	if ids := messageIDs(s.await(t, 2)); !slices.Equal(ids, []string{"<m1@example.org>", "<during@example.org>"}) {
		t.Fatalf("delivered %v", ids)
	}
}

// TestIdleRestarted: IDLE ends and is re-issued after the restart interval
// on the same session; the default interval stays under RFC 2177's 29
// minutes, and a longer one is refused.
func TestIdleRestarted(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	cfg := config(srv, nil)
	cfg.IdleRestartMS = 0
	w := &stepWaiter{}
	inst := open(t, steppedFactory(t, w), channel(channelA, cfg), adapter.Dependencies{Credentials: passwordCreds()})
	run(t, inst, newSink())
	w.step(t)
	w.step(t)
	w.idle(t)
	waitFor(t, func() bool { return srv.commands("IDLE") >= 3 })
	if logins := len(srv.loginList()); logins != 1 {
		t.Fatalf("logins = %d; IDLE restarts must reuse the session", logins)
	}
	for _, d := range w.requested() {
		if d != imap.DefaultIdleRestart || d > imap.MaxIdleRestart {
			t.Fatalf("waits %v, want the default IDLE restart each time", w.requested())
		}
	}
	cfg.IdleRestartMS = int64(30 * time.Minute / time.Millisecond)
	if issues := newFactory(t).Validate(cfg.Extension()); len(issues) == 0 {
		t.Fatal("idle_restart_ms of 30 minutes accepted")
	}
}

// TestPollWithoutIdle: a server without IDLE is polled at the poll interval.
func TestPollWithoutIdle(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS, noIdle: true})
	w := &stepWaiter{}
	inst := open(t, steppedFactory(t, w), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: passwordCreds()})
	s := newSink()
	run(t, inst, s)
	w.idle(t)
	srv.deliver("INBOX", mail("<m1@example.org>", "hi"), time.Now())
	w.step(t)
	s.await(t, 1)
	srv.deliver("INBOX", mail("<m2@example.org>", "again"), time.Now())
	w.step(t)
	s.await(t, 2)
	if n := srv.commands("IDLE"); n != 0 {
		t.Fatalf("IDLE sent %d times to a server without IDLE", n)
	}
	for _, d := range w.requested() {
		if d != 50*time.Millisecond {
			t.Fatalf("waits %v, want the poll interval", w.requested())
		}
	}
}

// TestUIDValidityGap: after the mailbox is recreated (new UIDVALIDITY, UIDs
// restart below the cursor) the receiver reports the gap and resynchronizes:
// mail received since the last delivered message arrives, the resent copy of
// a delivered message keeps its event identity so ingress drops it.
func TestUIDValidityGap(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	t0 := time.Now().Add(-time.Minute)
	for i, id := range []string{"<m1@example.org>", "<m2@example.org>", "<m3@example.org>"} {
		srv.deliver("INBOX", mail(id, "before"), t0.Add(time.Duration(i)*time.Second))
	}
	obs := &observer{}
	w := &stepWaiter{}
	inst := open(t, steppedFactory(t, w), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: passwordCreds(), Observer: obs})
	s := newSink()
	run(t, inst, s)
	w.idle(t)
	before := s.envelopes()
	if len(before) != 3 {
		t.Fatalf("delivered %v before the gap", messageIDs(before))
	}
	oldValidity := srv.uidValidity("INBOX")

	srv.recreate("INBOX")
	srv.deliver("INBOX", mail("<m3@example.org>", "before"), t0.Add(2*time.Second)) // migrated copy, UID 1
	srv.deliver("INBOX", mail("<m4@example.org>", "after"), time.Now())             // new mail, UID 2
	if srv.uidValidity("INBOX") == oldValidity {
		t.Fatal("recreating the mailbox kept its UIDVALIDITY")
	}
	w.drive(t, func() bool { return len(s.envelopes()) >= 5 })
	got := s.envelopes()
	if ids := messageIDs(got[3:]); !slices.Equal(ids, []string{"<m3@example.org>", "<m4@example.org>"}) {
		t.Fatalf("after the gap delivered %v, want the m3 copy then m4", ids)
	}
	if got[3].EventID != before[2].EventID || got[3].MessageID != before[2].MessageID {
		t.Errorf("resynchronized copy has event %q/%q, first delivery %q/%q: dedup identity must not depend on UIDs",
			got[3].EventID, got[3].MessageID, before[2].EventID, before[2].MessageID)
	}
	w.step(t) // two more rounds deliver nothing
	w.step(t)
	w.idle(t)
	if n := len(s.envelopes()); n != 5 {
		t.Fatalf("envelopes = %d after further rounds, want 5", n)
	}
	if n := obs.count(imap.ObservationGap); n != 1 {
		t.Errorf("gap observations = %d, want 1", n)
	}
}

// TestReconnectBeforeExpiry: an OAuth2 session is replaced before its token
// expires, with a freshly resolved token, and nothing is delivered twice
// across the reconnect. Time is the test's clock, advanced by the waits.
func TestReconnectBeforeExpiry(t *testing.T) {
	clock := portstest.NewFixedClock(time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC))
	srv := newMailServer(t, serverOptions{sec: implicitTLS, mechs: []string{"XOAUTH2"}, tokenOK: func(tok string) bool { return strings.HasPrefix(tok, "tok-") }})
	var mu sync.Mutex
	var resolvedAt, expiresAt []time.Time
	creds := &credHost{resolve: func(_ ports.CredentialRequest, n int) (ports.Credential, error) {
		now := clock.Now()
		mu.Lock()
		resolvedAt, expiresAt = append(resolvedAt, now), append(expiresAt, now.Add(time.Hour))
		mu.Unlock()
		return ports.OAuth2{User: mailUser, AccessToken: fmt.Sprintf("tok-%d", n), ExpiresAt: now.Add(time.Hour)}, nil
	}}
	cfg := config(srv, nil)
	cfg.IdleRestartMS = int64(25 * time.Minute / time.Millisecond) // reconnect due 55 min in: two restarts, then the expiry wait
	w := &stepWaiter{clock: clock}
	inst := open(t, steppedFactory(t, w), channel(channelA, cfg), adapter.Dependencies{Credentials: creds, Clock: clock})
	s := newSink()
	srv.deliver("INBOX", mail("<m1@example.org>", "one"), time.Now())
	run(t, inst, s)
	s.await(t, 1)
	w.drive(t, func() bool { return len(srv.loginList()) >= 2 })
	w.idle(t)
	srv.deliver("INBOX", mail("<m2@example.org>", "two"), time.Now())
	s.await(t, 2)
	w.idle(t)

	mu.Lock()
	defer mu.Unlock()
	if len(resolvedAt) < 2 {
		t.Fatalf("credential resolutions at %v", resolvedAt)
	}
	margin := 5 * time.Minute
	if second := resolvedAt[1]; second.Before(expiresAt[0].Add(-margin)) || !second.Before(expiresAt[0]) {
		t.Errorf("reconnected at %v; first token expires %v: want within the %v margin before expiry", second, expiresAt[0], margin)
	}
	logins := srv.loginList()
	for i := 1; i < len(logins); i++ {
		if logins[i].token == logins[i-1].token {
			t.Errorf("login %d reused token %q: every connection resolves a fresh credential", i, logins[i].token)
		}
	}
	if ids := messageIDs(s.envelopes()); !slices.Equal(ids, []string{"<m1@example.org>", "<m2@example.org>"}) {
		t.Fatalf("delivered %v across reconnects, want each message once", ids)
	}
	for _, r := range creds.requests() {
		if r.Purpose != imap.PurposeIMAP || r.Reference != "mailbox-cred" || r.Channel != channelA {
			t.Errorf("credential request %+v", r)
		}
	}
}

// TestReconnectAfterConnectionLoss: a dropped connection reconnects after
// the backoff and resumes from the cursor.
func TestReconnectAfterConnectionLoss(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	srv.deliver("INBOX", mail("<m1@example.org>", "one"), time.Now())
	w := &stepWaiter{}
	inst := open(t, steppedFactory(t, w), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: passwordCreds()})
	s := newSink()
	run(t, inst, s)
	s.await(t, 1)
	w.idle(t)
	srv.deliver("INBOX", mail("<m2@example.org>", "two"), time.Now())
	srv.drop()
	w.drive(t, func() bool { return len(srv.loginList()) >= 2 && len(s.envelopes()) >= 2 })
	w.idle(t)
	if ids := messageIDs(s.envelopes()); !slices.Equal(ids, []string{"<m1@example.org>", "<m2@example.org>"}) {
		t.Fatalf("delivered %v, want each once", ids)
	}
}

// TestRejectedCredentialEndsRun: a refused password is final.
func TestRejectedCredentialEndsRun(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	creds := staticCreds(ports.Password{User: mailUser, Secret: []byte("wrong")})
	inst := open(t, newFactory(t), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: creds})
	err := run(t, inst, newSink()).result(t)
	if model.CodeOf(err) != model.Unauthorized {
		t.Fatalf("Run = %v, want unauthorized", err)
	}
	if n := len(creds.requests()); n != 1 {
		t.Fatalf("credential resolutions = %d, want 1: a rejected password is not retried", n)
	}
}

// TestSinkErrorKeepsMessage: a sink refusal ends Run without moving the
// cursor; the next receiver delivers the message again.
func TestSinkErrorKeepsMessage(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	f := newFactory(t)
	ch := channel(channelA, config(srv, nil))
	srv.deliver("INBOX", mail("<m1@example.org>", "one"), time.Now())
	s := newSink()
	s.setFail(model.NewError(model.Transient, "store busy"))
	err := run(t, open(t, f, ch, adapter.Dependencies{Credentials: passwordCreds()}), s).result(t)
	if model.CodeOf(err) != model.Transient {
		t.Fatalf("Run = %v, want the sink's error", err)
	}
	s.setFail(nil)
	run(t, open(t, f, ch, adapter.Dependencies{Credentials: passwordCreds()}), s)
	if ids := messageIDs(s.await(t, 1)); ids[0] != "<m1@example.org>" {
		t.Fatalf("delivered %v, want m1", ids)
	}
}

// TestCursorInProtocolState: the cursor lives in the engine's protocol
// store, so a receiver in another factory resumes without redelivery.
func TestCursorInProtocolState(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	ps := &protocolState{}
	ch := channel(channelA, config(srv, nil))
	deps := adapter.Dependencies{Credentials: passwordCreds(), Protocol: ps}
	srv.deliver("INBOX", mail("<m1@example.org>", "one"), time.Now())
	s := newSink()
	w1 := &stepWaiter{}
	r := run(t, open(t, steppedFactory(t, w1), ch, deps), s)
	w1.idle(t)
	_ = r.stop()

	srv.deliver("INBOX", mail("<m2@example.org>", "two"), time.Now())
	s2 := newSink()
	w2 := &stepWaiter{}
	run(t, open(t, steppedFactory(t, w2), ch, deps), s2)
	w2.idle(t)
	if ids := messageIDs(s2.envelopes()); !slices.Equal(ids, []string{"<m2@example.org>"}) {
		t.Fatalf("second receiver delivered %v, want only m2", ids)
	}
	x, _, err := ps.Load(context.Background(), "receiver")
	if err != nil || x.Name != "imap.mail.receiver_state" {
		t.Fatalf("protocol state %+v, %v", x, err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(time.Millisecond)
	}
}

// linkLog records what Run reports to the host about its provider link.
type linkLog struct {
	mu  sync.Mutex
	all []bool
}

func watchLink(inst *imap.Instance) *linkLog {
	l := &linkLog{}
	inst.ReportReceiver(func(connected bool) {
		l.mu.Lock()
		l.all = append(l.all, connected)
		l.mu.Unlock()
	})
	return l
}

func (l *linkLog) reports() []bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.all)
}

// TestReceiverReportsItsLink: connected while an authenticated session
// stands, not connected when it is lost, connected again once the receiver
// has logged in anew, and not connected when Run returns.
func TestReceiverReportsItsLink(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	w := &stepWaiter{}
	inst := open(t, steppedFactory(t, w), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: passwordCreds()})
	link := watchLink(inst)
	r := run(t, inst, newSink())
	w.idle(t)
	if got := link.reports(); !slices.Equal(got, []bool{true}) {
		t.Fatalf("reports with the session standing = %v, want [true]", got)
	}
	srv.drop()
	w.drive(t, func() bool { return len(srv.loginList()) >= 2 })
	w.idle(t)
	_ = r.stop()
	if got, want := link.reports(), []bool{true, false, true, false}; !slices.Equal(got, want) {
		t.Fatalf("reports = %v, want %v", got, want)
	}
}

// TestReceiverNeverConnectedWithoutALogin: a refused credential is no link.
func TestReceiverNeverConnectedWithoutALogin(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	creds := staticCreds(ports.Password{User: mailUser, Secret: []byte("wrong")})
	inst := open(t, newFactory(t), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: creds})
	link := watchLink(inst)
	if err := run(t, inst, newSink()).result(t); model.CodeOf(err) != model.Unauthorized {
		t.Fatalf("Run = %v, want unauthorized", err)
	}
	if got := link.reports(); len(got) != 0 {
		t.Fatalf("reports = %v, want none: no session ever stood", got)
	}
}
