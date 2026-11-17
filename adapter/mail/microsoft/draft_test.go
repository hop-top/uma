package microsoft

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

const peerAddr = "peer@example.test"

func draftExt(data string) model.Extension {
	return model.Extension{Name: ExtensionReplyDraft, Version: ExtensionVersion, Data: json.RawMessage(data)}
}

func draftOp(r model.ExternalRef, op string) adapter.Effect {
	eff := effect(r, "a1")
	eff.Operation.ID = model.ID(op)
	return eff
}

func executeDraft(t *testing.T, e *env, eff adapter.Effect, data string) (adapter.Outcome, error) {
	t.Helper()
	return e.inst.Execute(t.Context(), adapter.ExecuteRequest{Effect: eff, Action: draftExt(data)})
}

func (e *env) message(id string) *fakeMessage {
	e.g.mu.Lock()
	defer e.g.mu.Unlock()
	return e.box.messages[id]
}

func (e *env) messageCalls() []string {
	var calls []string
	for _, r := range e.g.requests() {
		if strings.Contains(r, "/messages") {
			calls = append(calls, strings.SplitN(r, "?", 2)[0])
		}
	}
	return calls
}

func TestExtensionsDeclareReplyDraft(t *testing.T) {
	var d adapter.ExtensionDefinition
	for _, x := range NewFactory(Options{}).Extensions() {
		if x.Name == "microsoft.mail.reply_draft" {
			d = x
		}
	}
	if d.Version != 1 || d.Kind != model.ExtensionKindEffect || d.RequiredCapability != model.CapabilityExtensionExecute ||
		d.AuthorizeAs != model.ActionMessageReply || d.InputSchema.Name != "microsoft.mail.reply_draft.input" {
		t.Errorf("definition = %+v", d)
	}
}

func TestValidateActionReplyDraft(t *testing.T) {
	f := NewFactory(Options{})
	aud := model.Audience{Visibility: model.Internal}
	for _, c := range []struct{ name, data, field string }{
		{"create", `{"text":"hi"}`, ""},
		{"update", `{"text":"hi","draft":"AAMk1"}`, ""},
		{"delete", `{"draft":"AAMk1","delete":true}`, ""},
		{"explicit no delete", `{"text":"hi","delete":false,"draft":null}`, ""},
		{"unknown field ignored", `{"text":"hi","other":{"x":1}}`, ""},
		{"empty data", ``, "data"},
		{"not an object", `["hi"]`, "data"},
		{"no text", `{}`, "data.text"},
		{"blank text", `{"text":"  \n"}`, "data.text"},
		{"text not a string", `{"text":1}`, "data.text"},
		{"draft not a string", `{"text":"hi","draft":7}`, "data.draft"},
		{"padded draft", `{"text":"hi","draft":" AAMk1"}`, "data.draft"},
		{"delete not a boolean", `{"draft":"AAMk1","delete":"yes"}`, "data.delete"},
		{"delete without draft", `{"delete":true}`, "data.draft"},
		{"delete with text", `{"draft":"AAMk1","delete":true,"text":"hi"}`, "data.text"},
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

// A create is Send's draft path without the send: createReply on the
// peer's message, then a PATCH; the draft stays in Drafts, unsent.
func TestReplyDraftCreateIsCreateReplyThenPatchNoSend(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", peerAddr, "hi")
	e.box.messages["m1"].m.From = &gRecipient{EmailAddress: gEmailAddress{Address: "someone-else@example.test"}}
	eff := draftOp(route(peerAddr, "m1"), "00000000-0000-7000-8000-0000000000d1")
	out, err := executeDraft(t, e, eff, `{"text":"on its way"}`)
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Execute = %+v, %v", out, err)
	}
	if want := []model.ExternalRef{DraftRef(testChan, "draft-1")}; !slices.Equal(out.References, want) {
		t.Errorf("references = %+v, want %+v", out.References, want)
	}
	want := []string{
		"POST /v1.0/users/" + testMailbox + "/messages/m1/createReply",
		"PATCH /v1.0/users/" + testMailbox + "/messages/draft-1",
	}
	if calls := e.messageCalls(); !slices.Equal(calls, want) {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
	d := e.message("draft-1")
	if d.folder != "drafts" || !d.m.IsDraft || d.m.Body == nil || d.m.Body.Content != "on its way" ||
		len(d.m.ToRecipients) != 1 || d.m.ToRecipients[0].EmailAddress.Address != peerAddr || d.parent != "m1" ||
		d.props[AttemptProperty] != attemptKey(eff) {
		t.Fatalf("draft = %+v folder %q props %v", d.m, d.folder, d.props)
	}
}

func TestReplyDraftCreateWithoutParentIsNewDraft(t *testing.T) {
	e := newEnv(t, Config{})
	out, err := executeDraft(t, e, draftOp(route(peerAddr, "gone"), "00000000-0000-7000-8000-0000000000d1"), `{"text":"hello"}`)
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Execute = %+v, %v", out, err)
	}
	if d := e.message("draft-1"); d == nil || d.parent != "" || d.folder != "drafts" || d.m.Body.Content != "hello" {
		t.Fatalf("draft = %+v", d)
	}
}

// A second call naming the draft replaces its body in place: one draft,
// the same ID, only the body sent so what a person changed stays.
func TestReplyDraftUpdateReplacesBodyOfSameDraft(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", peerAddr, "hi")
	rt := route(peerAddr, "m1")
	if _, err := executeDraft(t, e, draftOp(rt, "00000000-0000-7000-8000-0000000000d1"), `{"text":"first"}`); err != nil {
		t.Fatal(err)
	}
	e.message("draft-1").m.Subject = "RE: edited by a person"
	before := len(e.box.messages)
	var patch []byte
	e.g.override = func(_ http.ResponseWriter, r *http.Request) bool {
		if r.Method == "PATCH" {
			patch, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(patch))
		}
		return false
	}
	out, err := executeDraft(t, e, draftOp(rt, "00000000-0000-7000-8000-0000000000d2"), `{"text":"second","draft":"draft-1"}`)
	if want := `{"body":{"contentType":"text","content":"second"}}`; string(patch) != want {
		t.Errorf("PATCH body = %s, want %s", patch, want)
	}
	if err != nil || out.State != model.ProviderAccepted || !slices.Equal(out.References, []model.ExternalRef{DraftRef(testChan, "draft-1")}) {
		t.Fatalf("update = %+v, %v", out, err)
	}
	d := e.message("draft-1")
	if len(e.box.messages) != before || d.m.Body.Content != "second" || d.m.Subject != "RE: edited by a person" || d.folder != "drafts" {
		t.Fatalf("draft = %+v, messages %d -> %d", d.m, before, len(e.box.messages))
	}
	calls := e.messageCalls()
	if last := calls[len(calls)-1]; last != "PATCH /v1.0/users/"+testMailbox+"/messages/draft-1" || slices.Contains(calls, "POST /v1.0/users/"+testMailbox+"/messages/draft-1/send") {
		t.Errorf("calls = %q", calls)
	}
}

func TestReplyDraftUpdateRefusals(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", peerAddr, "hi")
	rt := route(peerAddr, "m1")
	// A draft that is gone.
	out, err := executeDraft(t, e, draftOp(rt, "00000000-0000-7000-8000-0000000000d2"), `{"text":"x","draft":"nope"}`)
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.NotFound {
		t.Fatalf("gone draft = %+v, %v", out, err)
	}
	// A message that is no draft (a sent draft keeps its ID).
	out, err = executeDraft(t, e, draftOp(rt, "00000000-0000-7000-8000-0000000000d3"), `{"text":"x","draft":"m1"}`)
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Validation {
		t.Fatalf("non-draft = %+v, %v", out, err)
	}
	if e.message("m1").m.Body.Content != "hi" {
		t.Errorf("a delivered message was changed")
	}
}

func TestReplyDraftDelete(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", peerAddr, "hi")
	rt := route(peerAddr, "m1")
	if _, err := executeDraft(t, e, draftOp(rt, "00000000-0000-7000-8000-0000000000d1"), `{"text":"x"}`); err != nil {
		t.Fatal(err)
	}
	del := `{"draft":"draft-1","delete":true}`
	out, err := executeDraft(t, e, draftOp(rt, "00000000-0000-7000-8000-0000000000d2"), del)
	if err != nil || out.State != model.ProviderAccepted || e.message("draft-1") != nil {
		t.Fatalf("delete = %+v, %v, draft %+v", out, err, e.message("draft-1"))
	}
	if e.g.count("DELETE /v1.0/users/"+testMailbox+"/messages/draft-1") != 1 {
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
	e.inbound("m1", peerAddr, "hi")
	for _, data := range []string{``, `{}`, `{"delete":true}`, `{"draft":"d","delete":true,"text":"x"}`} {
		out, err := executeDraft(t, e, draftOp(route(peerAddr, "m1"), "00000000-0000-7000-8000-0000000000d1"), data)
		if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Validation {
			t.Errorf("%q: Execute = %+v, %v", data, out, err)
		}
	}
	if n := e.g.count("POST /v1.0/users/" + testMailbox + "/messages"); n != 0 {
		t.Errorf("a refused action reached Graph: %q", e.g.requests())
	}
}

func TestReplyDraftErrorMapping(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", peerAddr, "hi")
	status := http.StatusForbidden
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.Contains(r.URL.Path, "/messages") {
			return false
		}
		graphError(w, status, "ErrorAccessDenied")
		return true
	}
	eff := draftOp(route(peerAddr, "m1"), "00000000-0000-7000-8000-0000000000d1")
	_, err := executeDraft(t, e, eff, `{"text":"x"}`)
	_ = wantErr(t, err, model.Unauthorized)
	status = http.StatusBadRequest
	out, err := executeDraft(t, e, eff, `{"text":"x"}`)
	if err != nil || out.State != model.Rejected {
		t.Errorf("400 = %+v, %v", out, err)
	}
}

// Never outcome_unknown: Reconcile of the effect says a new attempt is
// safe.
func TestReconcileReplyDraftAllowsNewAttempt(t *testing.T) {
	e := newEnv(t, Config{})
	eff := draftOp(route(peerAddr, ""), "00000000-0000-7000-8000-0000000000d1")
	rec, err := e.inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: eff, Capability: model.CapabilityExtensionExecute, Action: draftExt(`{"text":"x"}`)})
	if err != nil || rec.State != model.Failed || rec.Failure == nil || rec.Failure.Code != model.Transient {
		t.Fatalf("Reconcile = %+v, %v", rec, err)
	}
}

// update and delete act only on a draft this adapter made for the
// conversation: a draft of the mailbox that someone else made, one made
// for another conversation, and a delivered message are refused as
// validation errors, and nothing is changed or deleted.
func TestReplyDraftRefusesForeignDrafts(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", peerAddr, "hi")
	e.inbound("m2", "other@example.test", "hello")
	e.box.messages["m1"].m.ConversationID = "conv-1"
	e.box.messages["m2"].m.ConversationID = "conv-2"
	// A draft of the mailbox no reply draft effect made: no marker.
	e.box.messages["m3"] = &fakeMessage{folder: "drafts", props: map[string]string{}, m: gMessage{
		ID: "m3", IsDraft: true, ConversationID: "conv-1", Body: &gItemBody{ContentType: "text", Content: "personal"},
		ToRecipients: []gRecipient{{EmailAddress: gEmailAddress{Address: peerAddr}}},
	}}
	// A reply draft of this conversation that was sent since: no draft any
	// more, its attempt property kept.
	e.box.messages["m4"] = &fakeMessage{folder: "sentitems", m: gMessage{ID: "m4", ConversationID: "conv-1", Body: &gItemBody{ContentType: "text", Content: "sent"}},
		props: map[string]string{AttemptProperty: string(testChan.Tenant) + "/op/a1"}}
	// A reply draft made for the other conversation.
	other := route("other@example.test", "m2")
	if _, err := executeDraft(t, e, draftOp(other, "00000000-0000-7000-8000-0000000000d1"), `{"text":"for conv-2"}`); err != nil {
		t.Fatal(err)
	}
	mine := route(peerAddr, "m1")
	for _, c := range []struct{ name, id string }{{"no marker", "m3"}, {"other conversation", "draft-1"}, {"delivered message", "m1"}, {"sent draft", "m4"}} {
		for _, data := range []string{`{"text":"x","draft":"` + c.id + `"}`, `{"draft":"` + c.id + `","delete":true}`} {
			out, err := executeDraft(t, e, draftOp(mine, "00000000-0000-7000-8000-0000000000d9"), data)
			if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Validation {
				t.Errorf("%s %s: Execute = %+v, %v", c.name, data, out, err)
			}
		}
	}
	for _, id := range []string{"m3", "draft-1", "m1", "m4"} {
		if e.message(id) == nil {
			t.Errorf("%s was deleted", id)
		}
	}
	if e.message("m3").m.Body.Content != "personal" || e.message("draft-1").m.Body.Content != "for conv-2" || e.message("m1").m.Body.Content != "hi" {
		t.Errorf("a foreign message was changed")
	}
	if e.g.count("DELETE ") != 0 {
		t.Errorf("requests = %q", e.g.requests())
	}
}

func TestValidateActionReplyDraftRecipients(t *testing.T) {
	f := NewFactory(Options{})
	aud := model.Audience{Visibility: model.Internal}
	for _, c := range []struct{ name, data, field string }{
		{"reply all and cc", `{"text":"hi","reply_all":true,"cc":["a@example.test"],"bcc":["b@example.test"]}`, ""},
		{"reply_all not a boolean", `{"text":"hi","reply_all":"yes"}`, "data.reply_all"},
		{"cc not a list", `{"text":"hi","cc":"a@example.test"}`, "data.cc"},
		{"cc display name", `{"text":"hi","cc":["A <a@example.test>"]}`, "data.cc"},
		{"bcc not an address", `{"text":"hi","bcc":["nobody"]}`, "data.bcc"},
		{"delete with recipients", `{"draft":"r-1","delete":true,"cc":["a@example.test"]}`, "data.cc"},
	} {
		t.Run(c.name, func(t *testing.T) {
			issues := f.ValidateAction(aud, draftExt(c.data))
			switch {
			case c.field == "" && len(issues) != 0:
				t.Fatalf("issues = %+v, want none", issues)
			case c.field != "" && (len(issues) != 1 || issues[0].Field != c.field):
				t.Fatalf("issues = %+v, want one on %s", issues, c.field)
			}
		})
	}
}

// A create addresses the draft like a send; an update leaves the draft's
// recipients to the person.
func TestReplyDraftAddressesRecipientsOnCreateOnly(t *testing.T) {
	e := newEnv(t, Config{Addresses: []string{testMailbox}})
	e.parentWithRecipients()
	eff := draftOp(route(peerAddr, "m1"), "00000000-0000-7000-8000-0000000000d1")
	out, err := executeDraft(t, e, eff, `{"text":"on its way","reply_all":true,"cc":["extra@example.test"],"bcc":["audit@example.test"]}`)
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Execute = %+v, %v", out, err)
	}
	if calls := e.messageCalls(); !slices.Equal(calls[:1], []string{"POST /v1.0/users/" + testMailbox + "/messages/m1/createReplyAll"}) {
		t.Fatalf("calls = %q, want createReplyAll", calls)
	}
	d := e.message("draft-1")
	if got, want := graphAddrs(d.m.ToRecipients), []string{peerAddr, "bob@example.test"}; !slices.Equal(got, want) {
		t.Errorf("To = %v, want %v", got, want)
	}
	if got, want := graphAddrs(d.m.CcRecipients), []string{"carol@example.test", "extra@example.test"}; !slices.Equal(got, want) {
		t.Errorf("Cc = %v, want %v", got, want)
	}
	if d.folder != "drafts" {
		t.Errorf("draft folder = %q, want drafts: nothing is sent", d.folder)
	}
	e.g.mu.Lock()
	d.m.CcRecipients = nil // a person removed the Cc
	e.g.mu.Unlock()
	upd := draftOp(route(peerAddr, "m1"), "00000000-0000-7000-8000-0000000000d2")
	if out, err := executeDraft(t, e, upd, `{"text":"edited","draft":"draft-1","cc":["other@example.test"]}`); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("update = %+v, %v", out, err)
	}
	if got := e.message("draft-1").m.CcRecipients; len(got) != 0 {
		t.Errorf("update changed Cc to %v: it replaces the body alone", got)
	}
}
