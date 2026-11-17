package google

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/model"
)

func draftExt(data string) model.Extension {
	return model.Extension{Name: ExtensionReplyDraft, Version: ExtensionVersion, Data: json.RawMessage(data)}
}

// draftOp is an effect of operation op on the route.
func draftOp(r model.ExternalRef, op string) adapter.Effect {
	eff := effect(r, "a1")
	eff.Operation.ID = model.ID(op)
	return eff
}

func executeDraft(t *testing.T, e *env, eff adapter.Effect, data string) (adapter.Outcome, error) {
	t.Helper()
	return e.inst.Execute(t.Context(), adapter.ExecuteRequest{Effect: eff, Action: draftExt(data)})
}

func (e *env) draft(id string) *fakeDraft {
	e.g.mu.Lock()
	defer e.g.mu.Unlock()
	return e.box.drafts[id]
}

func parseDraft(t *testing.T, d *fakeDraft) *mime.Message {
	t.Helper()
	m, err := mime.Parse(bytes.NewReader(d.raw), mime.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestExtensionsDeclareReplyDraft(t *testing.T) {
	var d adapter.ExtensionDefinition
	for _, x := range NewFactory(Options{}).Extensions() {
		if x.Name == "google.mail.reply_draft" {
			d = x
		}
	}
	if d.Version != 1 || d.Kind != model.ExtensionKindEffect || d.RequiredCapability != model.CapabilityExtensionExecute ||
		d.AuthorizeAs != model.ActionMessageReply || d.InputSchema.Name != "google.mail.reply_draft.input" {
		t.Errorf("definition = %+v", d)
	}
}

func TestValidateActionReplyDraft(t *testing.T) {
	f := NewFactory(Options{})
	aud := model.Audience{Visibility: model.Internal}
	for _, c := range []struct{ name, data, field string }{
		{"create", `{"text":"hi"}`, ""},
		{"update", `{"text":"hi","draft":"r-1"}`, ""},
		{"delete", `{"draft":"r-1","delete":true}`, ""},
		{"explicit no delete", `{"text":"hi","delete":false,"draft":null}`, ""},
		{"unknown field ignored", `{"text":"hi","other":{"x":1}}`, ""},
		{"empty data", ``, "data"},
		{"not an object", `["hi"]`, "data"},
		{"no text", `{}`, "data.text"},
		{"blank text", `{"text":"  \n"}`, "data.text"},
		{"text not a string", `{"text":1}`, "data.text"},
		{"draft not a string", `{"text":"hi","draft":7}`, "data.draft"},
		{"padded draft", `{"text":"hi","draft":" r-1"}`, "data.draft"},
		{"delete not a boolean", `{"draft":"r-1","delete":"yes"}`, "data.delete"},
		{"delete without draft", `{"delete":true}`, "data.draft"},
		{"delete with text", `{"draft":"r-1","delete":true,"text":"hi"}`, "data.text"},
		{"reply all and cc", `{"text":"hi","reply_all":true,"cc":["a@remote.test"],"bcc":["b@remote.test"]}`, ""},
		{"update with recipients", `{"text":"hi","draft":"r-1","cc":["a@remote.test"]}`, ""},
		{"reply_all not a boolean", `{"text":"hi","reply_all":"yes"}`, "data.reply_all"},
		{"cc not a list", `{"text":"hi","cc":"a@remote.test"}`, "data.cc"},
		{"cc display name", `{"text":"hi","cc":["A <a@remote.test>"]}`, "data.cc"},
		{"bcc not an address", `{"text":"hi","bcc":["nobody"]}`, "data.bcc"},
		{"delete with recipients", `{"draft":"r-1","delete":true,"reply_all":true}`, "data.cc"},
	} {
		t.Run(c.name, func(t *testing.T) {
			issues := f.ValidateAction(aud, draftExt(c.data))
			switch {
			case c.field == "" && len(issues) != 0:
				t.Fatalf("issues = %+v, want none", issues)
			case c.field != "" && (len(issues) != 1 || issues[0].Field != c.field || issues[0].Code != string(model.Validation) || issues[0].Message == ""):
				t.Fatalf("issues = %+v, want one on %s", issues, c.field)
			}
		})
	}
	if issues := f.ValidateAction(aud, model.Extension{Name: ExtensionReplyDraft, Version: 2}); len(issues) != 0 {
		t.Errorf("other version issues = %+v", issues)
	}
}

// A create composes the reply as a send does and stores it with
// users.drafts.create: threadId and threading headers of the peer's
// message, the operation's Message-ID, nothing sent.
func TestReplyDraftCreateThreadsOnParent(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.deliver(testMailbox, "m1", "thread-77", rawMessage(testPeer, testMailbox, "<m1@remote.test>", "Order 42", "where is it?",
		"References: <m0@remote.test>"))
	eff := draftOp(route(testPeer, "m1"), "00000000-0000-7000-8000-0000000000d1")
	out, err := executeDraft(t, e, eff, `{"text":"on its way"}`)
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Execute = %+v, %v", out, err)
	}
	if want := []model.ExternalRef{DraftRef(testChan, "r-1")}; !slices.Equal(out.References, want) {
		t.Errorf("references = %+v, want %+v", out.References, want)
	}
	d := e.draft("r-1")
	if d == nil || d.threadID != "thread-77" {
		t.Fatalf("draft = %+v", d)
	}
	m := parseDraft(t, d)
	h := m.Header
	if h.Subject != "Re: Order 42" || h.InReplyTo != "<m1@remote.test>" || h.References != "<m0@remote.test> <m1@remote.test>" ||
		len(h.To) != 1 || h.To[0].Address != testPeer || len(h.From) != 1 || h.From[0].Address != testMailbox ||
		h.MessageID != e.inst.messageID(eff.Operation, testMailbox) || m.Text != "on its way" {
		t.Errorf("draft headers = %+v text %q", h, m.Text)
	}
	if n := e.g.count("POST /gmail/v1/users/" + testMailbox + "/drafts"); n != 1 || e.g.count("POST /gmail/v1/users/"+testMailbox+"/messages/send") != 0 {
		t.Errorf("requests = %q", e.g.requests())
	}
}

// A second call naming the draft replaces its content in place with
// users.drafts.update: one draft, the same ID, the new text, still
// threaded; a call naming no draft makes another.
func TestReplyDraftUpdateReplacesSameDraft(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.deliver(testMailbox, "m1", "thread-77", rawMessage(testPeer, testMailbox, "<m1@remote.test>", "Order 42", "where is it?"))
	rt := route(testPeer, "m1")
	out, err := executeDraft(t, e, draftOp(rt, "00000000-0000-7000-8000-0000000000d1"), `{"text":"first"}`)
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("create = %+v, %v", out, err)
	}
	before := e.draft("r-1").messageID
	out, err = executeDraft(t, e, draftOp(rt, "00000000-0000-7000-8000-0000000000d2"), `{"text":"second","draft":"r-1"}`)
	if err != nil || out.State != model.ProviderAccepted || !slices.Equal(out.References, []model.ExternalRef{DraftRef(testChan, "r-1")}) {
		t.Fatalf("update = %+v, %v", out, err)
	}
	e.g.mu.Lock()
	n := len(e.box.drafts)
	e.g.mu.Unlock()
	d := e.draft("r-1")
	if n != 1 || d == nil || d.updates != 1 || d.threadID != "thread-77" || d.messageID == before {
		t.Fatalf("drafts = %d, draft = %+v", n, d)
	}
	if m := parseDraft(t, d); m.Text != "second" || m.Header.InReplyTo != "<m1@remote.test>" {
		t.Errorf("text %q in-reply-to %q", m.Text, m.Header.InReplyTo)
	}
	if e.g.count("PUT /gmail/v1/users/"+testMailbox+"/drafts/r-1") != 1 {
		t.Errorf("requests = %q", e.g.requests())
	}
	// Another operation naming no draft is another draft.
	if _, err := executeDraft(t, e, draftOp(rt, "00000000-0000-7000-8000-0000000000d3"), `{"text":"third"}`); err != nil || e.draft("r-2") == nil {
		t.Errorf("second create: %v, draft %+v", err, e.draft("r-2"))
	}
}

func TestReplyDraftUpdateGoneDraftRejected(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", testPeer, "hi")
	out, err := executeDraft(t, e, draftOp(route(testPeer, "m1"), "00000000-0000-7000-8000-0000000000d2"), `{"text":"x","draft":"r-9"}`)
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.NotFound {
		t.Fatalf("Execute = %+v, %v", out, err)
	}
}

func TestReplyDraftDelete(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", testPeer, "hi")
	rt := route(testPeer, "m1")
	if _, err := executeDraft(t, e, draftOp(rt, "00000000-0000-7000-8000-0000000000d1"), `{"text":"x"}`); err != nil {
		t.Fatal(err)
	}
	del := `{"draft":"r-1","delete":true}`
	out, err := executeDraft(t, e, draftOp(rt, "00000000-0000-7000-8000-0000000000d2"), del)
	if err != nil || out.State != model.ProviderAccepted || e.draft("r-1") != nil {
		t.Fatalf("delete = %+v, %v, draft %+v", out, err, e.draft("r-1"))
	}
	if e.g.count("DELETE /gmail/v1/users/"+testMailbox+"/drafts/r-1") != 1 {
		t.Errorf("requests = %q", e.g.requests())
	}
	// A draft already gone (sent or deleted by the person) is deleted.
	out, err = executeDraft(t, e, draftOp(rt, "00000000-0000-7000-8000-0000000000d3"), del)
	if err != nil || out.State != model.ProviderAccepted {
		t.Errorf("repeat delete = %+v, %v", out, err)
	}
}

func TestReplyDraftRefusesInvalidActions(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", testPeer, "hi")
	for _, data := range []string{``, `{}`, `{"delete":true}`, `{"draft":"r-1","delete":true,"text":"x"}`} {
		out, err := executeDraft(t, e, draftOp(route(testPeer, "m1"), "00000000-0000-7000-8000-0000000000d1"), data)
		if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Validation {
			t.Errorf("%q: Execute = %+v, %v", data, out, err)
		}
	}
	if n := e.g.count("POST /gmail/v1/users/" + testMailbox + "/drafts"); n != 0 {
		t.Errorf("a refused action reached Gmail: %q", e.g.requests())
	}
}

func TestReplyDraftErrorMapping(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		reason string
		want   model.ErrorCode
		final  bool
	}{
		{"missing scope", http.StatusForbidden, "insufficientPermissions", model.Unauthorized, false},
		{"rate limit", http.StatusTooManyRequests, "rateLimitExceeded", model.RateLimited, false},
		{"refused", http.StatusBadRequest, "invalidArgument", model.Permanent, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, Config{})
			e.inbound("m1", testPeer, "hi")
			e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.Contains(r.URL.Path, "/drafts") {
					return false
				}
				googleError(w, c.status, c.reason, "X")
				return true
			}
			out, err := executeDraft(t, e, draftOp(route(testPeer, "m1"), "00000000-0000-7000-8000-0000000000d1"), `{"text":"x"}`)
			if c.final {
				if err != nil || out.State != model.Rejected {
					t.Fatalf("Execute = %+v, %v", out, err)
				}
				return
			}
			_ = wantCode(t, err, c.want)
		})
	}
}

// A create whose answer is lost is outcome_unknown; Reconcile finds the
// draft by the operation's Message-ID, or allows a new attempt when
// Gmail holds none. An update or deletion is retried, never reconciled.
func TestReplyDraftCreateLostAnswerReconciles(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", testPeer, "hi")
	rt := route(testPeer, "m1")
	eff := draftOp(rt, "00000000-0000-7000-8000-0000000000d1")
	e.g.mu.Lock()
	e.box.draftLost = 1
	e.g.mu.Unlock()
	out, err := executeDraft(t, e, eff, `{"text":"x"}`)
	_ = wantCode(t, err, model.OutcomeUnknown)
	if len(out.References) != 1 || out.References[0].Kind != model.RefKindEmailMessageID {
		t.Errorf("references = %+v", out.References)
	}
	rec, err := e.inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: eff, Capability: model.CapabilityExtensionExecute, Action: draftExt(`{"text":"x"}`)})
	if err != nil || rec.State != model.ProviderAccepted || !slices.Equal(rec.References, []model.ExternalRef{DraftRef(testChan, "r-1")}) {
		t.Fatalf("Reconcile = %+v, %v", rec, err)
	}
	// No draft of that operation: a new attempt is allowed.
	other := draftOp(rt, "00000000-0000-7000-8000-0000000000d9")
	rec, err = e.inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: other, Capability: model.CapabilityExtensionExecute, Action: draftExt(`{"text":"x"}`)})
	if err != nil || rec.State != model.Failed || rec.Failure == nil || rec.Failure.Code != model.Transient {
		t.Fatalf("Reconcile of nothing = %+v, %v", rec, err)
	}
	for _, data := range []string{`{"text":"x","draft":"r-1"}`, `{"draft":"r-1","delete":true}`} {
		rec, err = e.inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: eff, Capability: model.CapabilityExtensionExecute, Action: draftExt(data)})
		if err != nil || rec.State != model.Failed || rec.Failure == nil || rec.Failure.Code != model.Transient {
			t.Errorf("Reconcile %s = %+v, %v", data, rec, err)
		}
	}
}

func TestReplyDraftWrongVersionAndName(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", testPeer, "hi")
	eff := draftOp(route(testPeer, "m1"), "00000000-0000-7000-8000-0000000000d1")
	out, err := e.inst.Execute(t.Context(), adapter.ExecuteRequest{Effect: eff, Action: model.Extension{Name: ExtensionReplyDraft, Version: 2, Data: json.RawMessage(`{"text":"x"}`)}})
	if err != nil || out.State != model.Rejected || out.Failure.Code != model.SchemaMismatch {
		t.Errorf("version 2 = %+v, %v", out, err)
	}
}

// update and delete act only on a draft this adapter made for the
// conversation: a draft of the mailbox a person wrote, even in the same
// thread, and a reply draft made for another thread are refused as
// validation errors, and nothing is changed or deleted.
func TestReplyDraftRefusesForeignDrafts(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.deliver(testMailbox, "m1", "thread-1", rawMessage(testPeer, testMailbox, "<m1@remote.test>", "one", "hi"))
	e.g.deliver(testMailbox, "m2", "thread-2", rawMessage("other@remote.test", testMailbox, "<m2@remote.test>", "two", "hello"))
	// A draft a person wrote in thread-1: no uma Message-ID.
	e.g.mu.Lock()
	e.box.drafts = map[string]*fakeDraft{"p-1": {id: "p-1", messageID: "pm-1", threadID: "thread-1",
		raw: rawMessage(testMailbox, testPeer, "<own@"+"example.test>", "one", "personal")},
		// uma's Message-ID shape on another domain: not this adapter's.
		"p-2": {id: "p-2", messageID: "pm-2", threadID: "thread-1",
			raw: rawMessage(testMailbox, testPeer, "<"+strings.Repeat("a", 32)+"@elsewhere.test>", "one", "foreign")}}
	e.g.mu.Unlock()
	// A reply draft made for thread-2.
	if _, err := executeDraft(t, e, draftOp(route("other@remote.test", "m2"), "00000000-0000-7000-8000-0000000000d1"), `{"text":"for thread 2"}`); err != nil {
		t.Fatal(err)
	}
	mine := route(testPeer, "m1")
	for _, c := range []struct{ name, id string }{{"person's draft", "p-1"}, {"foreign domain", "p-2"}, {"other thread", "r-1"}} {
		for _, data := range []string{`{"text":"x","draft":"` + c.id + `"}`, `{"draft":"` + c.id + `","delete":true}`} {
			out, err := executeDraft(t, e, draftOp(mine, "00000000-0000-7000-8000-0000000000d9"), data)
			if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Validation {
				t.Errorf("%s %s: Execute = %+v, %v", c.name, data, out, err)
			}
		}
	}
	if e.draft("p-1") == nil || e.draft("p-2") == nil || e.draft("r-1") == nil || e.draft("r-1").updates != 0 || e.draft("p-1").updates != 0 {
		t.Errorf("a foreign draft was changed or deleted")
	}
	if e.g.count("DELETE ") != 0 || e.g.count("PUT ") != 0 {
		t.Errorf("requests = %q", e.g.requests())
	}
}

// With no parent message to bind by thread, the draft's To header must be
// exactly the route's address: a lookalike (suffix or prefix), a display
// name that holds the address, another recipient beside it and an
// unparsable header are refused; case differences are not.
func TestReplyDraftToBindingIsExact(t *testing.T) {
	uma := func(n int) string { return "<" + strings.Repeat("abcdefgh"[n:n+1], 32) + "@example.test>" }
	for n, c := range []struct {
		name, to string
		want     bool
	}{
		{"exact", testPeer, true},
		{"case", strings.ToUpper(testPeer), true},
		{"display name", "Peer <" + testPeer + ">", true},
		{"suffix lookalike", "x+" + testPeer + ".attacker.net", false},
		{"prefix lookalike", "evil" + testPeer, false},
		{"display name trick", `"` + testPeer + `" <attacker@evil.test>`, false},
		{"two recipients", testPeer + ", other@remote.test", false},
		{"unparsable", "<<" + testPeer, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, Config{})
			id := "p-1"
			e.g.mu.Lock()
			e.box.drafts = map[string]*fakeDraft{id: {id: id, messageID: "pm-1", threadID: "thread-x",
				raw: rawMessage(testMailbox, c.to, uma(n), "s", "t")}}
			e.g.mu.Unlock()
			out, err := executeDraft(t, e, draftOp(route(testPeer, ""), "00000000-0000-7000-8000-0000000000d9"), `{"draft":"`+id+`","delete":true}`)
			if err != nil {
				t.Fatal(err)
			}
			refused := out.State == model.Rejected && out.Failure != nil && out.Failure.Code == model.Validation
			if c.want == refused || (c.want && out.State != model.ProviderAccepted) {
				t.Errorf("Execute = %+v, want accepted %v", out, c.want)
			}
			if gone := e.draft(id) == nil; gone != c.want {
				t.Errorf("draft deleted = %v, want %v", gone, c.want)
			}
		})
	}
}

// A draft is addressed like a send: reply to all adds the parent's To and
// Cc minus the mailbox, and the explicit Cc and Bcc ride along, on create
// and on update; nothing is sent.
func TestReplyDraftAddressesRecipients(t *testing.T) {
	e := newEnv(t, Config{})
	parentWithRecipients(e)
	eff := draftOp(route(testPeer, "m1"), "00000000-0000-7000-8000-0000000000d1")
	out, err := executeDraft(t, e, eff, `{"text":"on its way","reply_all":true,"cc":["extra@remote.test"],"bcc":["audit@example.test"]}`)
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Execute = %+v, %v", out, err)
	}
	check := func(d *fakeDraft) {
		t.Helper()
		m := parseDraft(t, d)
		if got, want := addrList(m.Header.To), []string{testPeer, "bob@remote.test"}; !slices.Equal(got, want) {
			t.Errorf("To = %v, want %v", got, want)
		}
		if got, want := addrList(m.Header.Cc), []string{"carol@remote.test", "extra@remote.test"}; !slices.Equal(got, want) {
			t.Errorf("Cc = %v, want %v", got, want)
		}
		if got := m.Header.Values("Bcc"); len(got) != 1 {
			t.Errorf("Bcc = %v", got)
		}
	}
	check(e.draft("r-1"))
	upd := draftOp(route(testPeer, "m1"), "00000000-0000-7000-8000-0000000000d2")
	if out, err := executeDraft(t, e, upd, `{"text":"edited","draft":"r-1","reply_all":true,"cc":["extra@remote.test"],"bcc":["audit@example.test"]}`); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("update = %+v, %v", out, err)
	}
	check(e.draft("r-1"))
	if n := e.g.count("POST /gmail/v1/users/" + testMailbox + "/messages/send"); n != 0 {
		t.Errorf("a draft was sent")
	}
}
