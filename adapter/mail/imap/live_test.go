package imap_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"hop.top/uma"
	"hop.top/uma/adapter"
	imapmail "hop.top/uma/adapter/mail/imap"
	"hop.top/uma/adapter/mail/internal/livemail"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/adapter/mail/smtp"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// The live tests run imap.mail against a real IMAP server (Dovecot) and a
// real submission server (Mailpit) started by testdata/live/up.sh. They skip
// unless UMA_LIVE_MAIL=1: the default test run and make check need neither
// Docker nor a mail server. docs/recording/imap-smtp.md says what a green
// run proves. Each test works in a mailbox of its own, so runs never see each
// other's mail.

// liveRig is one test's mailbox on the live account.
type liveRig struct {
	t    *testing.T
	env  livemail.Env
	acct *livemail.Account
	box  string
	obs  *observer // what the engine's adapters reported
}

func newLive(t *testing.T, groups ...livemail.Group) *liveRig {
	t.Helper()
	env := livemail.Require(t, append([]livemail.Group{livemail.IMAP}, groups...)...)
	r := &liveRig{t: t, env: env, acct: env.Account(t), box: "live-" + livemail.Token(t), obs: &observer{}}
	r.acct.Create(r.box)
	return r
}

func (r *liveRig) creds() *credHost {
	return staticCreds(ports.Password{User: r.env.IMAPUser, Secret: []byte(r.env.IMAPPass)})
}

// config is the channel configuration for the live account: implicit TLS
// unless startTLS (or the environment has no TLS port), the one credential
// shared with the submission server.
func (r *liveRig) config(startTLS bool) imapmail.Config {
	r.t.Helper()
	srv := imapmail.Server{Host: r.env.IMAPHost, CommandTimeoutMS: 20_000}
	if startTLS || r.env.IMAPTLSPort == 0 {
		srv.StartTLSPort = r.env.IMAPStartTLSPort
	} else {
		srv.TLSPort = r.env.IMAPTLSPort
	}
	cfg := imapmail.Config{
		Address: r.env.IMAPUser, FromName: "Desk", Subject: "Your request", Mailbox: r.box,
		IMAP: srv, IdleRestartMS: 2000, PollIntervalMS: 200,
	}
	if !r.env.HasSubmission() {
		cfg.Submission = imapmail.Server{Host: "127.0.0.1", TLSPort: 1}
		return cfg
	}
	if r.env.SMTPUser != r.env.IMAPUser || r.env.SMTPPass != r.env.IMAPPass {
		r.t.Fatal("imap.mail uses one credential for retrieval and submission: " +
			"UMA_LIVE_SMTP_USER/PASS must equal UMA_LIVE_IMAP_USER/PASS")
	}
	cfg.Submission = imapmail.Server{Host: r.env.SMTPHost, CommandTimeoutMS: 20_000, DataTimeoutMS: 20_000}
	if r.env.SMTPStartTLSPort != 0 {
		cfg.Submission.StartTLSPort = r.env.SMTPStartTLSPort
	} else {
		cfg.Submission.PlainPort, cfg.Submission.AllowPlaintextLoopback = r.env.SMTPPlainPort, true
	}
	return cfg
}

func (r *liveRig) factory() *imapmail.Factory {
	return imapmail.NewFactory(imapmail.Options{RootCAs: r.env.Roots, ReconnectBackoff: 100 * time.Millisecond})
}

// receiver opens an instance and runs its receiver into a sink.
func (r *liveRig) receiver(cfg imapmail.Config, deps adapter.Dependencies) (*sink, *running) {
	r.t.Helper()
	if deps.Credentials == nil {
		deps.Credentials = r.creds()
	}
	inst := open(r.t, r.factory(), channel(channelA, cfg), deps)
	s := newSink()
	return s, run(r.t, inst, s)
}

// put appends raw to the live mailbox, dated a minute ago (inside the
// initial synchronization window).
func (r *liveRig) put(raw []byte) { r.putAt(raw, r.then()) }

func (r *liveRig) putAt(raw []byte, at time.Time) { r.t.Helper(); r.acct.Append(r.box, raw, at) }

func (r *liveRig) then() time.Time { return time.Now().Add(-time.Minute).Truncate(time.Second) }

// eventID is the event identity ingress drops duplicates by.
func (r *liveRig) eventID(raw []byte) string {
	h := sha256.Sum256(raw)
	return "mailbox:" + r.env.IMAPUser + ";sha256:" + hex.EncodeToString(h[:])
}

// settle gives the receiver several rounds (an IDLE restart is two seconds)
// to deliver anything it should not.
func settle() { time.Sleep(7 * time.Second) }

// TestLiveReceive: mail appended by another client reaches the sink under
// the identity of its bytes as the server holds them, over implicit TLS and
// over STARTTLS, and the receiver leaves no flag behind on the server.
func TestLiveReceive(t *testing.T) {
	for _, via := range []string{"tls", "starttls"} {
		t.Run(via, func(t *testing.T) {
			r := newLive(t)
			if via == "starttls" && r.env.IMAPStartTLSPort == 0 {
				t.Skip("no UMA_LIVE_IMAP_STARTTLS_PORT")
			}
			if via == "tls" && r.env.IMAPTLSPort == 0 {
				t.Skip("no UMA_LIVE_IMAP_TLS_PORT")
			}
			first := mail("<live-1-"+livemail.Token(t)+"@example.org>", "first")
			second := mail("<live-2-"+livemail.Token(t)+"@example.org>", "second")
			r.put(first)
			r.put(second)
			s, _ := r.receiver(r.config(via == "starttls"), adapter.Dependencies{})
			got := s.await(t, 2)
			for i, raw := range [][]byte{first, second} {
				if got[i].EventID != r.eventID(raw) {
					t.Errorf("message %d: event %q, want %q: the server must return the bytes it was given", i, got[i].EventID, r.eventID(raw))
				}
			}
			settle()
			if n := len(s.envelopes()); n != 2 {
				t.Fatalf("delivered %d, want 2", n)
			}
			for _, m := range r.acct.Messages(r.box) {
				// \Recent is the server's own mark for a message no session has
				// seen; no client sets or clears it.
				if flags := slices.DeleteFunc(slices.Clone(m.Flags), func(f imap.Flag) bool { return f == "\\Recent" }); len(flags) != 0 {
					t.Errorf("UID %d carries flags %v after retrieval: the receiver must change none", m.UID, flags)
				}
			}
		})
	}
}

// TestLiveIdleDelivers: with an IDLE restart far longer than the test, new
// mail still arrives within seconds: the server's IDLE notification wakes
// the receiver.
func TestLiveIdleDelivers(t *testing.T) {
	r := newLive(t)
	cfg := r.config(false)
	cfg.IdleRestartMS, cfg.PollIntervalMS = 60_000, 60_000
	r.put(mail("<live-idle-0@example.org>", "before"))
	s, _ := r.receiver(cfg, adapter.Dependencies{})
	s.await(t, 1)
	time.Sleep(time.Second) // the receiver is in IDLE by now
	start := time.Now()
	r.put(mail("<live-idle-1@example.org>", "while idle"))
	s.await(t, 2)
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("delivered %v after the append, want well under the 60s IDLE restart", d)
	}
	t.Logf("delivered %v after the append", time.Since(start).Round(time.Millisecond))
}

// TestLiveCursorResumes: a receiver that starts where the last one stopped
// delivers only what is new; one that lost its cursor delivers everything
// again under the same event identities.
func TestLiveCursorResumes(t *testing.T) {
	r := newLive(t)
	a, b := mail("<live-c1@example.org>", "a"), mail("<live-c2@example.org>", "b")
	r.put(a)
	r.put(b)
	state := &protocolState{}
	s1, run1 := r.receiver(r.config(false), adapter.Dependencies{Protocol: state})
	s1.await(t, 2)
	if err := run1.stop(); err != nil && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("first run: %v", err)
	}

	c := mail("<live-c3@example.org>", "c")
	r.put(c)
	s2, _ := r.receiver(r.config(false), adapter.Dependencies{Protocol: state})
	s2.await(t, 1)
	settle()
	if got := s2.envelopes(); len(got) != 1 || got[0].EventID != r.eventID(c) {
		t.Fatalf("second receiver delivered %v, want only the new mail", messageIDs(got))
	}

	s3, _ := r.receiver(r.config(false), adapter.Dependencies{Protocol: &protocolState{}})
	all := s3.await(t, 3)
	var want, have []string
	for _, raw := range [][]byte{a, b, c} {
		want = append(want, r.eventID(raw))
	}
	for _, e := range all {
		have = append(have, e.EventID)
	}
	if !slices.Equal(have, want) {
		t.Fatalf("a receiver without a cursor delivered %v, want %v", have, want)
	}
}

// TestLiveUIDValidityChange: deleting and recreating the mailbox gives it a
// new UIDVALIDITY on the real server. The receiver reports the gap once,
// resynchronizes, delivers the new mail, and the copy of mail it already
// delivered carries its old event identity.
func TestLiveUIDValidityChange(t *testing.T) {
	r := newLive(t)
	t0 := r.then()
	raws := [][]byte{mail("<live-g1@example.org>", "g1"), mail("<live-g2@example.org>", "g2"), mail("<live-g3@example.org>", "g3")}
	for i, raw := range raws {
		r.putAt(raw, t0.Add(time.Duration(i)*time.Second))
	}
	obs := &observer{}
	s, _ := r.receiver(r.config(false), adapter.Dependencies{Observer: obs})
	before := s.await(t, 3)
	old := r.acct.UIDValidity(r.box)

	r.acct.Recreate(r.box)
	t.Logf("UIDVALIDITY %d -> %d", old, r.acct.UIDValidity(r.box))
	r.putAt(raws[2], t0.Add(2*time.Second)) // the copy, now UID 1
	fresh := mail("<live-g4@example.org>", "g4")
	r.putAt(fresh, time.Now()) // new mail, UID 2
	s.await(t, 5)
	settle()
	got := s.envelopes()
	if len(got) != 5 {
		t.Fatalf("delivered %v, want the three, the copy and the new mail", messageIDs(got))
	}
	if got[3].EventID != before[2].EventID {
		t.Errorf("copy has event %q, first delivery %q: identity must not depend on UIDs", got[3].EventID, before[2].EventID)
	}
	if got[4].EventID != r.eventID(fresh) {
		t.Errorf("new mail has event %q, want %q", got[4].EventID, r.eventID(fresh))
	}
	if n := obs.count(imapmail.ObservationGap); n != 1 {
		t.Errorf("gap observations = %d, want 1", n)
	}
}

// TestLiveOversizeIsHeaderOnly: a message over max_message_bytes is delivered
// from its header block alone, under an identity of that block and the size
// the real server reports.
func TestLiveOversizeIsHeaderOnly(t *testing.T) {
	r := newLive(t)
	cfg := r.config(false)
	cfg.MaxMessageBytes = 1000
	huge := mail("<live-big@example.org>", strings.Repeat("y", 5000))
	small := mail("<live-small@example.org>", "small")
	r.put(huge)
	r.put(small)
	s, _ := r.receiver(cfg, adapter.Dependencies{})
	got := s.await(t, 2)
	head := huge[:bytes.Index(huge, []byte("\r\n\r\n"))+4]
	hs := sha256.Sum256(head)
	want := "mailbox:" + r.env.IMAPUser + ";header-sha256:" + hex.EncodeToString(hs[:]) + ";size:" + strconv.Itoa(len(huge))
	if got[0].EventID != want {
		t.Errorf("oversize event %q, want %q", got[0].EventID, want)
	}
	if got[1].EventID != r.eventID(small) {
		t.Errorf("small event %q, want %q", got[1].EventID, r.eventID(small))
	}
	for _, p := range got[0].Content.Parts {
		if p.Kind == model.PartText && strings.Contains(p.Text, "yyyy") {
			t.Errorf("oversize message was delivered with its body")
		}
	}
}

// liveEngine is the real engine over a memory store with imap.mail on the
// live account, receivers running through Runtime.Receive.
func (r *liveRig) engine(tweak func(*imapmail.Config)) *engineRig {
	r.t.Helper()
	e := &engineRig{t: r.t, ctx: r.t.Context(), store: memory.New(memory.Options{Clock: portstest.NewFixedClock(time.Now())})}
	f := r.factory()
	eng, err := uma.New(uma.Config{
		Store: e.store,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{f},
		Extensions:  f.Extensions(),
		Credentials: r.creds(),
		Observer:    r.obs,
		Clock:       realClock{},
		IDs:         portstest.NewSequenceIDs(0xe0),
	})
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = eng.Close(context.Background()) })
	e.engine = eng
	if e.client, err = eng.For(model.Principal{Tenant: tenant, Actor: "operator"}); err != nil {
		r.t.Fatal(err)
	}
	cfg := r.config(false)
	if tweak != nil {
		tweak(&cfg)
	}
	lock := false
	sel := model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1}
	if err := e.store.Transact(e.ctx, tenant, func(tx store.Tx) error {
		if err := tx.PutChannel(e.ctx, channel(channelA, cfg), ""); err != nil {
			return err
		}
		return tx.PutInbox(e.ctx, model.Inbox{Ref: inboxA, Channel: channelA, Name: "mail", Destinations: []string{r.env.IMAPUser}, Selection: sel, Revision: "in1"}, "")
	}); err != nil {
		r.t.Fatal(err)
	}
	return e
}

// awaitStored waits until pred holds of the inbox's messages.
func awaitStored(t *testing.T, e *engineRig, what string, pred func([]model.Message) bool) []model.Message {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if msgs := e.messages(); pred(msgs) {
			return msgs
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; inbox holds %q", what, texts(e.messages()))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestLiveDedupThroughTheEngine: identical bytes are one message however
// they reach the mailbox, a forged mail that reuses a Message-ID is a second
// message and never hides the genuine one, and recreating the mailbox
// (a new UIDVALIDITY, every mail under a new UID) stores nothing twice.
func TestLiveDedupThroughTheEngine(t *testing.T) {
	r := newLive(t)
	e := r.engine(nil)
	at := r.then()
	const id = "<live-claimed@example.org>"
	forged := mailFrom(forger, id, "forged")
	genuine := mailFrom(peer, id, "the real question")
	same := mailFrom(peer, id, "another mail under the same id")
	for _, raw := range [][]byte{forged, genuine, genuine, same} { // genuine twice: two UIDs, one message
		r.putAt(raw, at)
	}
	r.putAt(mail("<live-end1@example.org>", "end"), at)
	e.receive()
	want := []string{"another mail under the same id", "forged", "the real question"}
	awaitStored(t, e, "the first four mails", func(m []model.Message) bool { return byProviderID(m, "<live-end1@example.org>") != nil })
	time.Sleep(3 * time.Second)
	if got := texts(e.messages()); !slices.Equal(got, want) {
		t.Fatalf("stored %q, want %q", got, want)
	}

	r.acct.Recreate(r.box)
	for _, raw := range [][]byte{mail("<live-end1@example.org>", "end"), same, genuine, forged} {
		r.putAt(raw, at)
	}
	r.putAt(mail("<live-end2@example.org>", "end"), time.Now())
	awaitStored(t, e, "the resynchronization", func(m []model.Message) bool { return byProviderID(m, "<live-end2@example.org>") != nil })
	time.Sleep(3 * time.Second)
	msgs := e.messages()
	if got := texts(msgs); !slices.Equal(got, want) {
		t.Fatalf("stored %q after the new UIDVALIDITY, want %q: identical bytes are one message", got, want)
	}
	if n := r.obs.count(imapmail.ObservationGap); n != 1 {
		t.Errorf("gap observations = %d, want 1: the redelivery must have gone through a resynchronization", n)
	}
	if len(msgs) != 5 { // three mails and two sentinels
		t.Fatalf("stored %d messages, want 5", len(msgs))
	}
}

// --- sending -------------------------------------------------------------------

func (r *liveRig) domain() string {
	_, d, _ := strings.Cut(r.env.IMAPUser, "@")
	return d
}

// sender opens the channel with a sent-copies mailbox of its own.
func (r *liveRig) sender(sent string, creds ports.Credentials) *imapmail.Instance {
	r.t.Helper()
	cfg := r.config(false)
	if sent != "" {
		r.acct.Create(sent)
		cfg.Sent = imapmail.Sent{Mailbox: sent}
	}
	if creds == nil {
		creds = r.creds()
	}
	return open(r.t, r.factory(), channel(channelA, cfg), adapter.Dependencies{Credentials: creds})
}

// TestLiveSendSubmits: a reply goes to the submission server over STARTTLS
// and AUTH with the shared credential, under the operation's Message-ID and
// the threading headers of its target; the sent copy is appended to the
// account and found again by Reconcile.
func TestLiveSendSubmits(t *testing.T) {
	r := newLive(t, livemail.Submission)
	sent := "live-sent-" + livemail.Token(t)
	inst := r.sender(sent, nil)
	op := model.OperationRef{Tenant: tenant, ID: model.ID("live-op-" + livemail.Token(t))}
	token := string(op.ID)
	parent := model.ProviderMessageRef(channelA, "<live-parent@example.org>")
	req := sendRequest(op, &parent)
	req.Content.Parts[0].Text = "Thanks, on it. " + token

	out, err := inst.Send(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	msgID := smtp.MessageID(op, r.domain())
	if out.State != model.ProviderAccepted || len(out.References) != 1 || out.References[0] != model.ProviderMessageRef(channelA, msgID) {
		t.Fatalf("outcome %+v, want accepted with Message-ID %s", out, msgID)
	}
	caught := r.env.AwaitCaptured(t, token, 1)
	if len(caught) != 1 {
		t.Fatalf("the catcher holds %d copies, want 1", len(caught))
	}
	if r.env.SMTPStartTLSPort != 0 && caught[0].Username != r.env.SMTPUser {
		t.Errorf("submission authenticated as %q, want %q", caught[0].Username, r.env.SMTPUser)
	}
	m, err := mime.Parse(bytes.NewReader(caught[0].Raw), mime.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if m.Header.MessageID != msgID || m.Header.InReplyTo != "<live-parent@example.org>" || m.Header.References != "<live-parent@example.org>" ||
		m.Header.From[0].Address != r.env.IMAPUser || !strings.Contains(m.Text, "Thanks, on it.") {
		t.Fatalf("submitted header %+v text %q", m.Header, m.Text)
	}

	// The attempt repeated is the same Message-ID.
	again, err := inst.Send(t.Context(), req)
	if err != nil || again.References[0] != out.References[0] {
		t.Fatalf("second attempt %+v, %v: the Message-ID must be stable per operation", again, err)
	}

	copies := r.acct.Messages(sent)
	if len(copies) != 2 {
		t.Fatalf("%s holds %d messages, want one copy per accepted submission", sent, len(copies))
	}
	if !slices.Contains(copies[0].Flags, imap.FlagSeen) || !bytes.Contains(copies[0].Raw, []byte("Message-Id: "+msgID)) && !bytes.Contains(copies[0].Raw, []byte("Message-ID: "+msgID)) {
		t.Errorf("sent copy flags %v, raw %q", copies[0].Flags, copies[0].Raw)
	}

	rec, err := inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: req.Effect})
	if err != nil || rec.State != model.ProviderAccepted {
		t.Fatalf("Reconcile of a sent operation = %+v, %v; want accepted", rec, err)
	}
	never := sendRequest(model.OperationRef{Tenant: tenant, ID: model.ID("live-never-" + livemail.Token(t))}, nil)
	rec, err = inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: never.Effect})
	if err != nil || rec.State != model.Unknown {
		t.Fatalf("Reconcile of an unsent operation = %+v, %v; want unknown (an appended copy's absence proves nothing)", rec, err)
	}
}

// TestLiveSendRefusesWrongPassword: a wrong password is refused by the real
// submission server, nothing is caught and nothing is filed as sent.
func TestLiveSendRefusesWrongPassword(t *testing.T) {
	r := newLive(t, livemail.Submission)
	if r.env.SMTPStartTLSPort == 0 {
		t.Skip("the catcher has no AUTH over TLS (UMA_LIVE_SMTP_STARTTLS_PORT unset)")
	}
	sent := "live-sent-" + livemail.Token(t)
	inst := r.sender(sent, staticCreds(ports.Password{User: r.env.IMAPUser, Secret: []byte("wrong-" + r.env.IMAPPass)}))
	op := model.OperationRef{Tenant: tenant, ID: model.ID("live-bad-" + livemail.Token(t))}
	out, err := inst.Send(t.Context(), sendRequest(op, nil))
	if err == nil && out.State == model.ProviderAccepted {
		t.Fatalf("Send = %+v, want a refusal", out)
	}
	t.Logf("wrong password: outcome %+v, error %v", out, err)
	if n := r.env.CountCaptured(t, string(op.ID)); n != 0 {
		t.Errorf("catcher holds %d copies of a refused submission", n)
	}
	if n := len(r.acct.Messages(sent)); n != 0 {
		t.Errorf("%s holds %d copies of a refused submission", sent, n)
	}
}

// TestLiveEngineRoundTrip: mail appended to the account lands in the inbox
// through the engine, and a reply to it leaves by submission threaded on it.
func TestLiveEngineRoundTrip(t *testing.T) {
	r := newLive(t, livemail.Submission)
	e := r.engine(nil)
	q := "<live-q-" + livemail.Token(t) + "@example.org>"
	r.put(mail(q, "A question"))
	e.receive()
	msgs := awaitStored(t, e, "the question", func(m []model.Message) bool { return byProviderID(m, q) != nil })
	m1 := byProviderID(msgs, q)
	if m1.Author.External.ID != peer || m1.Author.External.Namespace != imapmail.Namespace || m1.Source != model.TypeImapMail {
		t.Fatalf("message %+v", m1)
	}
	token := livemail.Token(t)
	acc, err := e.client.Messages().Reply(e.ctx, model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: "c-live-reply"}, Operation: model.OperationID("live-reply-" + token)},
		Conversation: m1.Ref.Conversation,
		Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Answer " + token}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	op := e.work(acc.Operation.Ref)
	if op.State != model.ProviderAccepted || len(op.ProviderReferences) != 1 {
		t.Fatalf("operation %+v, want provider_accepted with the Message-ID", op)
	}
	caught := r.env.AwaitCaptured(t, token, 1)
	pm, err := mime.Parse(bytes.NewReader(caught[0].Raw), mime.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if pm.Header.MessageID != op.ProviderReferences[0].ID || pm.Header.InReplyTo != q || pm.Header.To[0].Address != peer {
		t.Fatalf("reply header %+v, want Message-ID %s, In-Reply-To %s, To %s", pm.Header, op.ProviderReferences[0].ID, q, peer)
	}
}

// settleBox cleans the shared special-use mailboxes of the messages a state
// test moved into them.
func (r *liveRig) forgetIn(needle string, boxes ...string) {
	r.t.Cleanup(func() {
		for _, b := range boxes {
			r.acct.Forget(b, needle)
		}
	})
}

// awaitConversation waits until pred holds of the conversation.
func awaitConversation(t *testing.T, e *engineRig, what string, ref model.ConversationRef, pred func(model.Conversation) bool) model.Conversation {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for {
		if c := e.conversation(ref); pred(c) {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; provider state %+v", what, e.conversation(ref).ProviderState)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestLiveStateVerbs: through the engine, against Dovecot, the verbs change
// the account's mailbox: \Seen and \Flagged on a session of their own, MOVE
// into a mailbox by name, and archive and trash by the special-use
// attributes, each undone. The message is found by the location the move
// reported, so the next verb still reaches it.
func TestLiveStateVerbs(t *testing.T) {
	r := newLive(t)
	if caps := r.acct.Caps(); !caps.Has(imap.CapMove) || !caps.Has(imap.CapUIDPlus) || !caps.Has(imap.CapSpecialUse) {
		t.Skip("the live server lacks MOVE, UIDPLUS or SPECIAL-USE")
	}
	e := r.engine(nil)
	dest := r.box + "-dest"
	r.acct.Create(dest)
	id := "<live-st-" + livemail.Token(t) + "@example.org>"
	r.forgetIn(id, "Archive", "Trash", "Junk")
	r.put(mail(id, "A question"))
	e.receive()
	m := byProviderID(awaitStored(t, e, "the question", func(m []model.Message) bool { return byProviderID(m, id) != nil }), id)
	conv, cl := m.Ref.Conversation, e.client.Conversations()
	step := func(name string, submit func() (model.Acceptance, error)) {
		t.Helper()
		acc, err := submit()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if op := e.work(acc.Operation.Ref); op.State != model.ProviderAccepted {
			t.Fatalf("%s: operation %+v, want provider_accepted", name, op)
		}
	}
	held := func(box string) (livemail.Stored, bool) { return r.acct.Find(box, id) }
	hasFlag := func(s livemail.Stored, f imap.Flag) bool { return slices.Contains(s.Flags, f) }

	step("read", func() (model.Acceptance, error) {
		return cl.SetRead(e.ctx, model.SetConversationRead{Command: stateCommand("live-read"), Conversation: conv, Read: true})
	})
	if s, ok := held(r.box); !ok || !hasFlag(s, imap.FlagSeen) {
		t.Fatalf("after read: %+v, %v", s, ok)
	}
	step("star", func() (model.Acceptance, error) {
		return cl.Flag(e.ctx, model.FlagConversation{Command: stateCommand("live-star"), Conversation: conv, Kind: model.FlagStar})
	})
	step("move", func() (model.Acceptance, error) {
		return cl.Move(e.ctx, model.MoveConversation{Command: stateCommand("live-move"), Conversation: conv, Destination: model.MoveDestination{ID: dest}})
	})
	if _, ok := held(r.box); ok {
		t.Fatal("the message is still in the source mailbox")
	}
	if s, ok := held(dest); !ok || !hasFlag(s, imap.FlagSeen) || !hasFlag(s, imap.FlagFlagged) {
		t.Fatalf("after move: %+v, %v; want the message with its flags in the destination", s, ok)
	}
	// The follow-up names the location the move gave the message.
	step("unstar", func() (model.Acceptance, error) {
		return cl.Flag(e.ctx, model.FlagConversation{Command: stateCommand("live-unstar"), Conversation: conv, Kind: model.FlagStar, Undo: true})
	})
	if s, _ := held(dest); hasFlag(s, imap.FlagFlagged) {
		t.Fatalf("the follow-up did not reach the moved message: %+v", s)
	}
	step("archive", func() (model.Acceptance, error) {
		return cl.Archive(e.ctx, model.ArchiveConversation{Command: stateCommand("live-archive"), Conversation: conv})
	})
	if _, ok := held("Archive"); !ok {
		t.Fatal("archive did not reach the \\Archive mailbox")
	}
	step("unarchive", func() (model.Acceptance, error) {
		return cl.Archive(e.ctx, model.ArchiveConversation{Command: stateCommand("live-unarchive"), Conversation: conv, Undo: true})
	})
	if _, ok := held(r.box); !ok {
		t.Fatal("unarchive did not return the message to the retrieved mailbox")
	}
	step("trash", func() (model.Acceptance, error) {
		return cl.Trash(e.ctx, model.TrashConversation{Command: stateCommand("live-trash"), Conversation: conv})
	})
	if _, ok := held("Trash"); !ok {
		t.Fatal("trash did not reach the \\Trash mailbox")
	}
	step("untrash", func() (model.Acceptance, error) {
		return cl.Trash(e.ctx, model.TrashConversation{Command: stateCommand("live-untrash"), Conversation: conv, Undo: true})
	})
	if _, ok := held(r.box); !ok {
		t.Fatal("untrash did not return the message to the retrieved mailbox")
	}
}

// TestLiveStateFollowsTheMailbox: what another client does to a message the
// receiver delivered reaches the conversation: flags (CONDSTORE where the
// server has it), a move into a mailbox the adapter looks in, and an
// expunge, told from a move by looking the message up in the other mailboxes.
func TestLiveStateFollowsTheMailbox(t *testing.T) {
	r := newLive(t)
	if !r.acct.Caps().Has(imap.CapCondStore) {
		t.Log("the live server has no CONDSTORE: flags are compared in full")
	}
	e := r.engine(func(c *imapmail.Config) { c.StateSyncIntervalMS = 200 })
	tok := livemail.Token(t)
	moved, gone := "<live-mv-"+tok+"@example.org>", "<live-gone-"+tok+"@example.org>"
	r.forgetIn(moved, "Archive")
	r.put(mail(moved, "moves"))
	r.put(mail(gone, "goes"))
	e.receive()
	msgs := awaitStored(t, e, "both mails", func(m []model.Message) bool { return byProviderID(m, moved) != nil && byProviderID(m, gone) != nil })
	mv, gn := byProviderID(msgs, moved), byProviderID(msgs, gone)

	uid := func(id string) uint32 {
		s, ok := r.acct.Find(r.box, id)
		if !ok {
			t.Fatalf("%s is not in the mailbox", id)
		}
		return s.UID
	}
	r.acct.SetFlag(r.box, uid(moved), imap.FlagSeen, true)
	c := awaitConversation(t, e, "the read flag", mv.Ref.Conversation, func(c model.Conversation) bool {
		for _, s := range c.ProviderState {
			if s.Read != nil && *s.Read {
				return true
			}
		}
		return false
	})
	if st := c.ProviderState[0]; st.Folder == nil || st.Folder.ID != r.box || st.Complete {
		t.Fatalf("provider state %+v, want the mailbox it is in, partial", st)
	}
	r.acct.SetFlag(r.box, uid(moved), imap.FlagFlagged, true)
	awaitConversation(t, e, "the star", mv.Ref.Conversation, func(c model.Conversation) bool {
		return len(c.ProviderState) > 0 && slices.Contains(c.ProviderState[0].Flags, model.FlagStar)
	})

	r.acct.MoveTo(r.box, uid(moved), "Archive")
	r.acct.Expunge(r.box, uid(gone))
	awaitConversation(t, e, "the move to Archive", mv.Ref.Conversation, func(c model.Conversation) bool {
		return len(c.ProviderState) > 0 && c.ProviderState[0].Archived
	})
	deadline := time.Now().Add(40 * time.Second)
	for r.obs.count(imapmail.ObservationExpunged) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the expunge was never observed")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st := e.conversation(gn.Ref.Conversation).ProviderState; len(st) != 0 {
		t.Fatalf("the expunged message's conversation has provider state %+v, want none: nothing reports an expunge as a place", st)
	}
	st := e.conversation(mv.Ref.Conversation).ProviderState[0]
	if st.Folder == nil || st.Folder.Role != model.RoleArchive || st.Read == nil || !*st.Read || !slices.Contains(st.Flags, model.FlagStar) {
		t.Fatalf("archived state %+v, want the archive folder with the flags the message carried", st)
	}
}
