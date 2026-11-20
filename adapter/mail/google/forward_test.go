package google

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

const origMessageID = "<orig-1@remote.test>"

func fwdExt(data string) model.Extension {
	return model.Extension{Name: ExtensionForward, Version: ExtensionVersion, Data: json.RawMessage(data)}
}

// origRaw is a message with an attachment, the kind a forward must carry whole.
func origRaw() []byte {
	return []byte(strings.ReplaceAll(`From: Peer <peer@remote.test>
To: desk@example.test
Subject: Invoice 42
Date: Sun, 11 Oct 2026 12:00:00 +0000
Message-ID: `+origMessageID+`
X-Marker: keep-me
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="b"

--b
Content-Type: text/plain; charset=utf-8

Please find the invoice.
--b
Content-Type: application/pdf; name="inv.pdf"
Content-Disposition: attachment; filename="inv.pdf"
Content-Transfer-Encoding: base64

JVBERi0xLjQK
--b--
`, "\n", "\r\n"))
}

func TestForwardCarriesOriginalAsRFC822(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.deliver(e.box.user, "m1", "thread-m1", origRaw())
	out, err := e.inst.Execute(t.Context(), adapter.ExecuteRequest{Effect: effect(route(testPeer, "m1"), "a1"), Action: fwdExt(`{"to":"dest@example.net","text":"for you"}`)})
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 2 {
		t.Fatalf("Execute = %+v, %v", out, err)
	}
	req, m := e.lastSend(t)
	if req.ThreadID != "" {
		t.Errorf("threadId %q: a forward starts its own thread", req.ThreadID)
	}
	h := m.Header
	if len(h.To) != 1 || h.To[0].Address != "dest@example.net" || h.Subject != "Fwd: Invoice 42" || len(h.From) != 1 || h.From[0].Address != testMailbox {
		t.Errorf("header = %+v", h)
	}
	if h.InReplyTo != "" || h.References != origMessageID {
		t.Errorf("in-reply-to %q references %q, want references = the original's Message-ID alone", h.InReplyTo, h.References)
	}
	if m.Text != "for you" {
		t.Errorf("text = %q", m.Text)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].MediaType != "message/rfc822" || !strings.HasSuffix(m.Attachments[0].Filename, ".eml") {
		t.Fatalf("attachments = %+v", m.Attachments)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(req.Raw)
	if !bytes.Contains(raw, origRaw()) {
		t.Errorf("the original is not carried byte for byte:\n%s", raw)
	}
	for _, want := range []string{"X-Marker: keep-me", "Message-ID: " + origMessageID, "filename=\"inv.pdf\""} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("composed forward lacks %q", want)
		}
	}
	// The outcome names the forward's own Message-ID, which Reconcile searches.
	if got := out.References[1]; got.Kind != model.RefKindEmailMessageID || got.ID != h.MessageID || h.MessageID == origMessageID {
		t.Errorf("outcome Message-ID ref %+v, header %q", got, h.MessageID)
	}
}

func TestForwardDefaultNoteAndSubject(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.deliver(e.box.user, "m1", "thread-m1", []byte(strings.Replace(string(origRaw()), "Subject: Invoice 42", "Subject: Fwd: Invoice 42", 1)))
	if out, err := e.inst.Execute(t.Context(), adapter.ExecuteRequest{Effect: effect(route(testPeer, "m1"), "a1"), Action: fwdExt(`{"to":"dest@example.net"}`)}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Execute = %+v, %v", out, err)
	}
	_, m := e.lastSend(t)
	if m.Header.Subject != "Fwd: Invoice 42" || m.Text != "Forwarded message attached." {
		t.Errorf("subject %q text %q", m.Header.Subject, m.Text)
	}
}

func TestForwardUsesTargetOverRouteParent(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.deliver(e.box.user, "m1", "thread-m1", origRaw())
	e.g.deliver(e.box.user, "m2", "thread-m2", []byte(strings.Replace(string(origRaw()), "Invoice 42", "Other", 1)))
	eff := effect(route(testPeer, "m1"), "a1")
	eff.Target = &model.ExternalRef{Channel: testChan, Namespace: model.ProviderMessageNamespace, Kind: model.RefKindMessage, ID: "m2"}
	if out, err := e.inst.Execute(t.Context(), adapter.ExecuteRequest{Effect: eff, Action: fwdExt(`{"to":"dest@example.net"}`)}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Execute = %+v, %v", out, err)
	}
	if _, m := e.lastSend(t); m.Header.Subject != "Fwd: Other" {
		t.Errorf("subject = %q, want the target's", m.Header.Subject)
	}
}

func TestForwardRefusals(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.deliver(e.box.user, "m1", "thread-m1", origRaw())
	run := func(route model.ExternalRef, data string) (adapter.Outcome, error) {
		return e.inst.Execute(t.Context(), adapter.ExecuteRequest{Effect: effect(route, "a1"), Action: fwdExt(data)})
	}
	for _, c := range []struct {
		name, data string
		route      model.ExternalRef
		code       model.ErrorCode
		msg        string
	}{
		{"no address", `{}`, route(testPeer, "m1"), model.Validation, "address"},
		{"display name", `{"to":"Dest <dest@example.net>"}`, route(testPeer, "m1"), model.Validation, "bare"},
		{"not an object", `"x"`, route(testPeer, "m1"), model.Validation, "JSON object"},
		{"no message", `{"to":"dest@example.net"}`, route(testPeer, ""), model.Validation, "no provider message"},
		{"gone", `{"to":"dest@example.net"}`, route(testPeer, "nope"), model.NotFound, "gone"},
	} {
		out, err := run(c.route, c.data)
		wantRejected(t, out, err, c.code, c.msg)
	}
	if n := len(e.box.sends); n != 0 {
		t.Errorf("%d messages sent by refused forwards", n)
	}
	if out, err := e.inst.Execute(t.Context(), adapter.ExecuteRequest{Effect: effect(route(testPeer, "m1"), "a1"), Action: model.Extension{Name: ExtensionForward, Version: 9, Data: json.RawMessage(`{"to":"dest@example.net"}`)}}); err != nil || out.State != model.Rejected || out.Failure.Code != model.SchemaMismatch {
		t.Errorf("version 9 = %+v, %v", out, err)
	}
}

func TestValidateActionForward(t *testing.T) {
	f := NewFactory(Options{})
	aud := model.Audience{Visibility: model.Internal}
	for _, c := range []struct{ data, field string }{
		{`{"to":"dest@example.net"}`, ""},
		{`{"to":"dest@example.net","text":"hi"}`, ""},
		{``, "data"},
		{`{}`, "data.to"},
		{`{"to":3}`, "data.to"},
		{`{"to":"a@b.c","text":4}`, "data.text"},
		{`{"to":"A <a@b.c>"}`, "data.to"},
		{`{"to":"a@b.c","text":"` + strings.Repeat("x", maxForwardNoteBytes+1) + `"}`, "data.text"},
	} {
		issues := f.ValidateAction(aud, fwdExt(c.data))
		switch {
		case c.field == "" && len(issues) != 0, c.field != "" && (len(issues) != 1 || issues[0].Field != c.field):
			t.Errorf("%q: issues = %+v, want field %q", c.data, issues, c.field)
		}
	}
	if issues := f.ValidateAction(aud, model.Extension{Name: ExtensionForward, Version: 9}); len(issues) != 0 {
		t.Errorf("another version is not judged here: %+v", issues)
	}
}

func TestExtensionsDeclareForward(t *testing.T) {
	for _, d := range NewFactory(Options{}).Extensions() {
		if d.Name != "google.mail.forward" {
			continue
		}
		if d.Version != 1 || d.Kind != model.ExtensionKindEffect || d.RequiredCapability != model.CapabilityExtensionExecute || d.AuthorizeAs != model.ActionExtensionExecute {
			t.Errorf("definition = %+v", d)
		}
		return
	}
	t.Fatal("Extensions does not ship google.mail.forward")
}

func TestForwardLostAnswerReconciles(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.deliver(e.box.user, "m1", "thread-m1", origRaw())
	e.g.mu.Lock()
	e.box.responses = []conformance.Response{{Outcome: conformance.OutcomeAnswerLost}}
	e.g.mu.Unlock()
	eff := effect(route(testPeer, "m1"), "a1")
	act := fwdExt(`{"to":"dest@example.net"}`)
	if _, err := e.inst.Execute(t.Context(), adapter.ExecuteRequest{Effect: eff, Action: act}); model.CodeOf(err) != model.OutcomeUnknown {
		t.Fatalf("err = %v, want outcome_unknown", err)
	}
	out, err := e.inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: eff, Capability: model.CapabilityExtensionExecute, Action: act})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Reconcile = %+v, %v; want the sent forward found by its Message-ID", out, err)
	}
	other := eff
	other.Operation.ID = "00000000-0000-7000-8000-0000000000e2"
	if out, err := e.inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: other, Capability: model.CapabilityExtensionExecute, Action: act}); err != nil || out.State != model.Failed {
		t.Fatalf("Reconcile of a never-sent forward = %+v, %v", out, err)
	}
}

func TestForwardRejectedSendIsFinal(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.deliver(e.box.user, "m1", "thread-m1", origRaw())
	e.g.mu.Lock()
	e.box.responses = []conformance.Response{{Outcome: conformance.OutcomeRejected}}
	e.g.mu.Unlock()
	out, err := e.inst.Execute(t.Context(), adapter.ExecuteRequest{Effect: effect(route(testPeer, "m1"), "a1"), Action: fwdExt(`{"to":"dest@example.net"}`)})
	if err != nil || out.State != model.Rejected {
		t.Fatalf("Execute = %+v, %v", out, err)
	}
}
