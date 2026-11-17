package google

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func labelsExt(data string) model.Extension {
	return model.Extension{Name: ExtensionLabels, Version: ExtensionVersion, Data: json.RawMessage(data)}
}

func execute(t *testing.T, e *env, eff adapter.Effect, data string) (adapter.Outcome, error) {
	t.Helper()
	return e.inst.Execute(t.Context(), adapter.ExecuteRequest{Effect: eff, Action: labelsExt(data)})
}

func TestExtensionsDeclareLabels(t *testing.T) {
	defs := NewFactory(Options{}).Extensions()
	i := slices.IndexFunc(defs, func(d adapter.ExtensionDefinition) bool { return d.Name == ExtensionLabels })
	if i < 0 {
		t.Fatalf("Extensions = %+v, want the labels effect", defs)
	}
	d := defs[i]
	if d.Name != "google.mail.labels" || d.Version != 1 || d.Kind != model.ExtensionKindEffect ||
		d.RequiredCapability != model.CapabilityExtensionExecute || d.AuthorizeAs != model.ActionConversationLabel {
		t.Errorf("definition = %+v", d)
	}
	if !slices.Contains(capabilities, model.CapabilityExtensionExecute) || !slices.Contains(RequiredPermissions, ScopeModify) {
		t.Errorf("capabilities %v, scopes %v", capabilities, RequiredPermissions)
	}
}

func TestValidateActionLabels(t *testing.T) {
	f := NewFactory(Options{})
	aud := model.Audience{Visibility: model.Internal}
	for _, c := range []struct{ name, data, field string }{
		{"add only", `{"add":["STARRED"]}`, ""},
		{"remove only", `{"remove":["UNREAD"]}`, ""},
		{"both", `{"add":["Label_7"],"remove":["UNREAD","INBOX"]}`, ""},
		{"null list", `{"add":null,"remove":["UNREAD"]}`, ""},
		{"unknown field ignored", `{"add":["STARRED"],"other":{"x":1}}`, ""},
		{"empty data", ``, "data"},
		{"empty object", `{}`, "data"},
		{"not an object", `["STARRED"]`, "data"},
		{"add not a list", `{"add":"STARRED"}`, "data.add"},
		{"remove not strings", `{"remove":[1]}`, "data.remove"},
		{"empty ID", `{"add":[""]}`, "data"},
		{"overlap", `{"add":["STARRED"],"remove":["STARRED"]}`, "data"},
		{"duplicate", `{"add":["STARRED","STARRED"]}`, "data"},
		{"too many", `{"add":[` + strings.Repeat(`"L",`, 100) + `"last"]}`, "data"},
	} {
		t.Run(c.name, func(t *testing.T) {
			issues := f.ValidateAction(aud, labelsExt(c.data))
			switch {
			case c.field == "" && len(issues) != 0:
				t.Fatalf("issues = %+v, want none", issues)
			case c.field != "" && (len(issues) != 1 || issues[0].Field != c.field || issues[0].Code != string(model.Validation) || issues[0].Message == ""):
				t.Fatalf("issues = %+v, want one on %s", issues, c.field)
			}
		})
	}
	// Another name or version is not this factory's to judge.
	other := model.Extension{Name: "org.example.other", Version: 1, Data: json.RawMessage(`{}`)}
	if issues := f.ValidateAction(aud, other); len(issues) != 0 {
		t.Errorf("foreign action issues = %+v", issues)
	}
	if issues := f.ValidateAction(aud, model.Extension{Name: ExtensionLabels, Version: 2}); len(issues) != 0 {
		t.Errorf("other version issues = %+v", issues)
	}
}

// Execute modifies the route's parent message with exactly the two lists,
// leaving unknown fields out, and reports the message.
func TestExecuteModifiesParentMessage(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", testPeer, "hi")
	out, err := execute(t, e, effect(route(testPeer, "m1"), "a1"), `{"add":["STARRED"],"remove":["UNREAD"],"unknown":{"kept":1}}`)
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Execute = %+v, %v", out, err)
	}
	if want := []model.ExternalRef{model.ProviderMessageRef(testChan, "m1")}; !slices.Equal(out.References, want) {
		t.Errorf("references = %+v, want %+v", out.References, want)
	}
	reqs := e.g.requests()
	if n := len(reqs); n != 1 || reqs[0] != "POST /gmail/v1/users/"+testMailbox+"/messages/m1/modify" {
		t.Fatalf("requests = %q", reqs)
	}
	e.g.mu.Lock()
	mods, labels := e.box.modifies, e.box.messages["m1"].labels
	e.g.mu.Unlock()
	if len(mods) != 1 || !slices.Equal(mods[0].AddLabelIDs, []string{"STARRED"}) || !slices.Equal(mods[0].RemoveLabelIDs, []string{"UNREAD"}) {
		t.Errorf("modify body = %+v", mods)
	}
	if !slices.Equal(labels, []string{labelInbox, "STARRED"}) {
		t.Errorf("labels after = %v", labels)
	}
	// The body carries only the documented fields.
	body, _ := json.Marshal(gModify{AddLabelIDs: []string{"STARRED"}})
	if string(body) != `{"addLabelIds":["STARRED"]}` {
		t.Errorf("wire body = %s", body)
	}
}

// Target, when set, wins over the route's parent and must be a provider
// message of this channel.
func TestExecuteTargetPrecedence(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", testPeer, "hi")
	e.inbound("m2", testPeer, "again")
	target := model.ProviderMessageRef(testChan, "m2")
	eff := effect(route(testPeer, "m1"), "a1")
	eff.Target = &target
	out, err := execute(t, e, eff, `{"add":["STARRED"]}`)
	if err != nil || out.State != model.ProviderAccepted || !slices.Equal(out.References, []model.ExternalRef{target}) {
		t.Fatalf("Execute = %+v, %v", out, err)
	}
	if !strings.HasSuffix(e.g.requests()[0], "/messages/m2/modify") {
		t.Errorf("requests = %q", e.g.requests())
	}
	for name, bad := range map[string]model.ExternalRef{
		"other channel": model.ProviderMessageRef(model.ChannelRef{Tenant: "t1", ID: "00000000-0000-7000-8000-0000000000c2"}, "m2"),
		"not a message": {Channel: testChan, Namespace: IdentityNamespace, Kind: refKindAddress, ID: testPeer},
		"empty ID":      model.ProviderMessageRef(testChan, ""),
	} {
		t.Run(name, func(t *testing.T) {
			eff := effect(route(testPeer, "m1"), "a2")
			eff.Target = &bad
			out, err := execute(t, e, eff, `{"add":["STARRED"]}`)
			if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Validation {
				t.Fatalf("Execute = %+v, %v; want rejected validation", out, err)
			}
		})
	}
	if n := e.g.count("POST"); n != 1 {
		t.Errorf("modify calls = %d, want only the first", n)
	}
}

// A route without a parent message and no Target names nothing to label.
func TestExecuteWithoutMessageIsRejected(t *testing.T) {
	e := newEnv(t, Config{})
	out, err := execute(t, e, effect(route(testPeer, ""), "a1"), `{"add":["STARRED"]}`)
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Validation {
		t.Fatalf("Execute = %+v, %v; want rejected validation", out, err)
	}
	if len(e.g.requests()) != 0 {
		t.Errorf("requests = %q, want none", e.g.requests())
	}
}

// Execute applies the same field rules as ValidateAction, refuses other
// names and versions, and never calls Gmail for them.
func TestExecuteRefusesInvalidActions(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", testPeer, "hi")
	eff := effect(route(testPeer, "m1"), "a1")
	for _, c := range []struct {
		name   string
		action model.Extension
		code   model.ErrorCode
	}{
		{"overlap", labelsExt(`{"add":["STARRED"],"remove":["STARRED"]}`), model.Validation},
		{"empty", labelsExt(`{}`), model.Validation},
		{"other version", model.Extension{Name: ExtensionLabels, Version: 2, Data: json.RawMessage(`{"add":["STARRED"]}`)}, model.SchemaMismatch},
		{"other name", model.Extension{Name: "org.example.other", Version: 1, Data: json.RawMessage(`{}`)}, model.Unsupported},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, err := e.inst.Execute(t.Context(), adapter.ExecuteRequest{Effect: eff, Action: c.action})
			if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != c.code {
				t.Fatalf("Execute = %+v, %v; want rejected %s", out, err, c.code)
			}
		})
	}
	if len(e.g.requests()) != 0 {
		t.Errorf("requests = %q, want none", e.g.requests())
	}
	if _, err := execute(t, e, effect(route("not-this-channel", "m1"), "a1"), `{"add":["STARRED"]}`); model.CodeOf(err) != model.Permanent {
		t.Errorf("foreign route error = %v, want permanent", err)
	}
}

// Gmail's answers map as documented: a message that is gone is a final
// rejected not_found; an unknown label a rejected permanent; a missing
// scope unauthorized; a rate limit rate_limited; a 5xx or a lost answer
// transient, since modify is idempotent.
func TestExecuteOutcomeMapping(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", testPeer, "hi")
	eff := effect(route(testPeer, "m1"), "a1")

	out, err := execute(t, e, effect(route(testPeer, "gone"), "a1"), `{"add":["STARRED"]}`)
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.NotFound {
		t.Fatalf("gone message: Execute = %+v, %v; want rejected not_found", out, err)
	}
	out, err = execute(t, e, eff, `{"add":["Label_unknown"]}`)
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Permanent {
		t.Fatalf("unknown label: Execute = %+v, %v; want rejected permanent", out, err)
	}
	for _, c := range []struct {
		name   string
		status int
		reason string
		code   model.ErrorCode
	}{
		{"missing scope", http.StatusForbidden, "insufficientPermissions", model.Unauthorized},
		{"rate limit", http.StatusForbidden, "userRateLimitExceeded", model.RateLimited},
		{"server error", http.StatusInternalServerError, "backendError", model.Transient},
	} {
		t.Run(c.name, func(t *testing.T) {
			e.g.mu.Lock()
			e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasSuffix(r.URL.Path, "/modify") {
					return false
				}
				googleError(w, c.status, c.reason, "ERROR")
				return true
			}
			e.g.mu.Unlock()
			out, err := execute(t, e, eff, `{"add":["STARRED"]}`)
			if out.State != "" || model.CodeOf(err) != c.code {
				t.Fatalf("Execute = %+v, %v; want error %s", out, err, c.code)
			}
		})
	}
	e.g.mu.Lock()
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/modify") {
			panic(http.ErrAbortHandler) // the connection drops: no answer
		}
		return false
	}
	e.g.mu.Unlock()
	out, err = execute(t, e, eff, `{"add":["STARRED"]}`)
	if out.State != "" || model.CodeOf(err) != model.Transient {
		t.Fatalf("lost answer: Execute = %+v, %v; want transient, never outcome_unknown", out, err)
	}
}

// Asked to reconcile a labels effect, the adapter answers that a new
// attempt is safe without reading anything.
func TestReconcileLabelsEffectAllowsNewAttempt(t *testing.T) {
	e := newEnv(t, Config{})
	out, err := e.inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: effect(route(testPeer, "m1"), "a1"), Capability: model.CapabilityExtensionExecute})
	if err != nil || out.State != model.Failed || out.Failure == nil || out.Failure.Code != model.Transient {
		t.Fatalf("Reconcile = %+v, %v; want failed transient", out, err)
	}
	if len(e.g.requests()) != 0 {
		t.Errorf("requests = %q, want none", e.g.requests())
	}
}

// The capability report lists extension_execute available: Gmail reveals
// a missing gmail.modify scope only on the call.
func TestCapabilitiesReportExtensionExecute(t *testing.T) {
	e := newEnv(t, Config{})
	st, err := e.inst.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(st, func(s model.CapabilityStatus) bool { return s.Name == model.CapabilityExtensionExecute })
	if i < 0 || !st[i].Available || st[i].Reason != "" {
		t.Errorf("statuses = %+v", st)
	}
}
