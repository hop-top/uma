package imap_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	gosmtp "github.com/emersion/go-smtp"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/adapter/mail/smtp"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

var opA = model.OperationRef{Tenant: tenant, ID: "op-1"}

func sendRequest(op model.OperationRef, target *model.ExternalRef) adapter.SendRequest {
	return adapter.SendRequest{
		Effect: adapter.Effect{
			Operation: op, Attempt: "00000e00-0000-7000-8000-0000000000f1",
			Route:  model.ExternalRef{Channel: channelA, Namespace: imap.Namespace, Kind: imap.RouteKind, ID: peer},
			Target: target, Audience: model.Audience{Visibility: model.Public},
		},
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Thanks, on it."}}},
	}
}

func sendRig(t *testing.T, sent imap.Sent) (*mailServer, *relay, *imap.Instance, *credHost) {
	t.Helper()
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	if sent.Mailbox != "" {
		if err := srv.user.Create(sent.Mailbox, nil); err != nil {
			t.Fatal(err)
		}
	}
	r := newRelay(t)
	cfg := config(srv, r)
	cfg.Sent = sent
	creds := passwordCreds()
	return srv, r, open(t, newFactory(t), channel(channelA, cfg), adapter.Dependencies{Credentials: creds}), creds
}

// TestSendSubmits: one submission with the mailbox credential, the
// operation's Message-ID and threading headers of the target.
func TestSendSubmits(t *testing.T) {
	_, r, inst, creds := sendRig(t, imap.Sent{})
	parent := model.ProviderMessageRef(channelA, "<m1@example.org>")
	out, err := inst.Send(t.Context(), sendRequest(opA, &parent))
	if err != nil {
		t.Fatal(err)
	}
	msgID := smtp.MessageID(opA, "example.com")
	if out.State != model.ProviderAccepted || len(out.References) != 1 || out.References[0] != model.ProviderMessageRef(channelA, msgID) {
		t.Fatalf("outcome %+v, want accepted with the Message-ID", out)
	}
	txs := r.transactions()
	if len(txs) != 1 || txs[0].from != mailUser || len(txs[0].rcpts) != 1 || txs[0].rcpts[0] != peer {
		t.Fatalf("relay transactions %+v", txs)
	}
	m, err := mime.Parse(bytes.NewReader(txs[0].data), mime.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if m.Header.MessageID != msgID || m.Header.InReplyTo != "<m1@example.org>" || m.Header.References != "<m1@example.org>" ||
		m.Header.From[0].Address != mailUser || m.Text != "Thanks, on it.\r\n" && m.Text != "Thanks, on it." {
		t.Fatalf("submitted header %+v text %q", m.Header, m.Text)
	}
	if a := r.authList(); len(a) != 1 || !r.authsTLS[0] {
		t.Fatalf("relay auths %v (TLS %v), want one over TLS", a, r.authsTLS)
	}
	reqs := creds.requests()
	if len(reqs) != 1 || reqs[0].Purpose != imap.PurposeSubmission || reqs[0].Reference != "mailbox-cred" || reqs[0].Channel != channelA {
		t.Fatalf("credential requests %+v", reqs)
	}
	again, err := inst.Send(t.Context(), sendRequest(opA, nil))
	if err != nil || again.References[0] != out.References[0] {
		t.Fatalf("second attempt %+v, %v: the Message-ID must be stable per operation", again, err)
	}
}

// TestSubmissionReplies: replies are definitive.
func TestSubmissionReplies(t *testing.T) {
	for _, c := range []struct {
		name  string
		rep   reply
		state model.DeliveryState
		code  model.ErrorCode
	}{
		{"5xx rejected", reply{mailErr: &gosmtp.SMTPError{Code: 550, EnhancedCode: gosmtp.EnhancedCode{5, 7, 1}, Message: "no"}}, model.Rejected, model.Permanent},
		{"4xx transient", reply{mailErr: &gosmtp.SMTPError{Code: 451, EnhancedCode: gosmtp.EnhancedCode{4, 3, 0}, Message: "later"}}, "", model.Transient},
		{"answer lost", reply{applied: true}, "", model.OutcomeUnknown},
		{"request lost", reply{lost: true}, "", model.OutcomeUnknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, r, inst, _ := sendRig(t, imap.Sent{})
			r.respond(c.rep)
			out, err := inst.Send(t.Context(), sendRequest(opA, nil))
			if c.state != "" {
				if err != nil || out.State != c.state || out.Failure == nil || out.Failure.Code != c.code || out.Failure.Retry != model.NoRetry {
					t.Fatalf("Send = %+v, %v; want %s / %s", out, err, c.state, c.code)
				}
				return
			}
			if model.CodeOf(err) != c.code {
				t.Fatalf("Send = %+v, %v; want %s", out, err, c.code)
			}
		})
	}
}

// TestSentCopies: the adapter appends a \Seen copy when the provider does
// not save one, and never when it does.
func TestSentCopies(t *testing.T) {
	t.Run("appended", func(t *testing.T) {
		srv, _, inst, creds := sendRig(t, imap.Sent{Mailbox: "Sent"})
		if _, err := inst.Send(t.Context(), sendRequest(opA, nil)); err != nil {
			t.Fatal(err)
		}
		if n, seen := srv.count("Sent"), srv.seen("Sent"); n != 1 || seen != 1 {
			t.Fatalf("Sent holds %d messages, %d seen; want one seen copy", n, seen)
		}
		var purposes []string
		for _, r := range creds.requests() {
			purposes = append(purposes, r.Purpose)
			if r.Reference != "mailbox-cred" {
				t.Errorf("request %+v uses another credential", r)
			}
		}
		if len(purposes) != 2 || purposes[0] != imap.PurposeSubmission || purposes[1] != imap.PurposeIMAP {
			t.Fatalf("purposes %v, want submission then imap", purposes)
		}
	})
	t.Run("saved by provider", func(t *testing.T) {
		srv, r, inst, _ := sendRig(t, imap.Sent{Mailbox: "Sent", SavedByProvider: true})
		r.saved = func(b []byte) { srv.deliver("Sent", b, time.Now()) }
		if _, err := inst.Send(t.Context(), sendRequest(opA, nil)); err != nil {
			t.Fatal(err)
		}
		if n := srv.count("Sent"); n != 1 {
			t.Fatalf("Sent holds %d messages, want only the provider's copy", n)
		}
	})
}

// TestReconcile: the sent copy settles a lost reply; its absence permits a
// retry only when the provider saves every submitted message.
func TestReconcile(t *testing.T) {
	effect := sendRequest(opA, nil).Effect
	t.Run("provider saves, answer lost", func(t *testing.T) {
		srv, r, inst, _ := sendRig(t, imap.Sent{Mailbox: "Sent", SavedByProvider: true})
		r.saved = func(b []byte) { srv.deliver("Sent", b, time.Now()) }
		r.respond(reply{applied: true})
		if _, err := inst.Send(t.Context(), sendRequest(opA, nil)); model.CodeOf(err) != model.OutcomeUnknown {
			t.Fatalf("Send = %v, want outcome_unknown", err)
		}
		srv.deliver("Sent", mail("<decoy@example.org> (copy of "+smtp.MessageID(opA, "example.com")+")", "decoy"), time.Now())
		out, err := inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: effect})
		if err != nil || out.State != model.ProviderAccepted || out.References[0] != model.ProviderMessageRef(channelA, smtp.MessageID(opA, "example.com")) {
			t.Fatalf("Reconcile = %+v, %v; want accepted", out, err)
		}
	})
	t.Run("provider saves, request lost", func(t *testing.T) {
		srv, r, inst, _ := sendRig(t, imap.Sent{Mailbox: "Sent", SavedByProvider: true})
		r.saved = func(b []byte) { srv.deliver("Sent", b, time.Now()) }
		r.respond(reply{lost: true})
		_, _ = inst.Send(t.Context(), sendRequest(opA, nil))
		// A substring match is not a match.
		srv.deliver("Sent", mail("<decoy@example.org> (copy of "+smtp.MessageID(opA, "example.com")+")", "decoy"), time.Now())
		out, err := inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: effect})
		if err != nil || out.State != model.Failed {
			t.Fatalf("Reconcile = %+v, %v; want failed (no applied effect)", out, err)
		}
	})
	t.Run("appended copies, answer lost", func(t *testing.T) {
		_, r, inst, _ := sendRig(t, imap.Sent{Mailbox: "Sent"})
		r.respond(reply{applied: true})
		_, _ = inst.Send(t.Context(), sendRequest(opA, nil))
		out, err := inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: effect})
		if err != nil || out.State != model.Unknown {
			t.Fatalf("Reconcile = %+v, %v; want unknown: an absent appended copy proves nothing", out, err)
		}
	})
	t.Run("appended copy found", func(t *testing.T) {
		_, _, inst, _ := sendRig(t, imap.Sent{Mailbox: "Sent"})
		if _, err := inst.Send(t.Context(), sendRequest(opA, nil)); err != nil {
			t.Fatal(err)
		}
		out, err := inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: effect})
		if err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("Reconcile = %+v, %v; want accepted", out, err)
		}
	})
	t.Run("missing mailbox is inconclusive", func(t *testing.T) {
		srv := newMailServer(t, serverOptions{sec: implicitTLS})
		cfg := config(srv, newRelay(t))
		cfg.Sent = imap.Sent{Mailbox: "Sent", SavedByProvider: true}
		inst := open(t, newFactory(t), channel(channelA, cfg), adapter.Dependencies{Credentials: passwordCreds()})
		out, err := inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: effect})
		if err == nil || out.State == model.Failed {
			t.Fatalf("Reconcile = %+v, %v; want an error, never a safe retry", out, err)
		}
	})
	t.Run("no sent mailbox", func(t *testing.T) {
		_, _, inst, _ := sendRig(t, imap.Sent{})
		statuses, _ := inst.Capabilities(context.Background())
		for _, s := range statuses {
			if s.Name == model.CapabilityReconcile && s.Available {
				t.Fatal("reconcile available without a sent-copies mailbox")
			}
		}
		if _, err := inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: effect}); model.CodeOf(err) != model.Unsupported {
			t.Fatalf("Reconcile = %v, want unsupported", err)
		}
	})
}

// TestOAuth2Submission: the submission uses the bearer token of the shared
// credential.
func TestOAuth2Submission(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	r := newRelay(t)
	r.tokenOK = goodToken
	creds := staticCreds(ports.OAuth2{User: mailUser, AccessToken: "good-token"})
	inst := open(t, newFactory(t), channel(channelA, config(srv, r)), adapter.Dependencies{Credentials: creds})
	if out, err := inst.Send(t.Context(), sendRequest(opA, nil)); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	if a := r.authList(); len(a) != 1 || a[0] != "OAUTHBEARER" {
		t.Fatalf("relay auths %v, want OAUTHBEARER", a)
	}
}
