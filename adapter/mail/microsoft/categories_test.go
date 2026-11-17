package microsoft

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func categoriesExt(data string) model.Extension {
	return model.Extension{Name: ExtensionCategories, Version: ExtensionVersion, Data: json.RawMessage(data)}
}

func execute(t *testing.T, e *env, eff adapter.Effect, data string) (adapter.Outcome, error) {
	t.Helper()
	return e.inst.Execute(t.Context(), adapter.ExecuteRequest{Effect: eff, Action: categoriesExt(data)})
}

func TestExtensionsDeclareCategories(t *testing.T) {
	defs := NewFactory(Options{}).Extensions()
	i := slices.IndexFunc(defs, func(d adapter.ExtensionDefinition) bool { return d.Name == ExtensionCategories })
	if i < 0 {
		t.Fatalf("Extensions = %+v, want the categories effect", defs)
	}
	d := defs[i]
	if d.Name != "microsoft.mail.categories" || d.Version != 1 || d.Kind != model.ExtensionKindEffect ||
		d.RequiredCapability != model.CapabilityExtensionExecute || d.AuthorizeAs != model.ActionConversationLabel {
		t.Errorf("definition = %+v", d)
	}
	if !slices.Contains(capabilities, model.CapabilityExtensionExecute) {
		t.Errorf("capabilities %v", capabilities)
	}
}

func TestValidateActionCategories(t *testing.T) {
	f := NewFactory(Options{})
	aud := model.Audience{Visibility: model.Internal}
	for _, c := range []struct{ name, data, field string }{
		{"one", `{"set":["Desk"]}`, ""},
		{"several", `{"set":["Desk","Follow up"]}`, ""},
		{"clear", `{"set":[]}`, ""},
		{"unknown field ignored", `{"set":["Desk"],"other":{"x":1}}`, ""},
		{"empty data", ``, "data.set"},
		{"empty object", `{}`, "data.set"},
		{"null", `{"set":null}`, "data.set"},
		{"not an object", `["Desk"]`, "data"},
		{"not a list", `{"set":"Desk"}`, "data.set"},
		{"not strings", `{"set":[1]}`, "data.set"},
		{"empty name", `{"set":[""]}`, "data.set"},
		{"duplicate", `{"set":["Desk","Desk"]}`, "data.set"},
	} {
		t.Run(c.name, func(t *testing.T) {
			issues := f.ValidateAction(aud, categoriesExt(c.data))
			switch {
			case c.field == "" && len(issues) != 0:
				t.Fatalf("issues = %+v, want none", issues)
			case c.field != "" && (len(issues) != 1 || issues[0].Field != c.field || issues[0].Code != string(model.Validation) || issues[0].Message == ""):
				t.Fatalf("issues = %+v, want one on %s", issues, c.field)
			}
		})
	}
	other := model.Extension{Name: "org.example.other", Version: 1, Data: json.RawMessage(`{}`)}
	if issues := f.ValidateAction(aud, other); len(issues) != 0 {
		t.Errorf("foreign action issues = %+v", issues)
	}
	if issues := f.ValidateAction(aud, model.Extension{Name: ExtensionCategories, Version: 2}); len(issues) != 0 {
		t.Errorf("other version issues = %+v", issues)
	}
}

// Execute updates the route's parent message with exactly the list, sent as
// categories (empty to clear), leaving unknown fields out, and reports the
// message.
func TestExecuteUpdatesParentMessageCategories(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", "peer@example.test", "hi")
	eff := effect(route("peer@example.test", "m1"), "a1")
	out, err := execute(t, e, eff, `{"set":["Desk","Follow up"],"unknown":{"kept":1}}`)
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Execute = %+v, %v", out, err)
	}
	if want := []model.ExternalRef{model.ProviderMessageRef(testChan, "m1")}; !slices.Equal(out.References, want) {
		t.Errorf("references = %+v, want %+v", out.References, want)
	}
	reqs := e.g.requests()
	if len(reqs) != 1 || reqs[0] != "PATCH /v1.0/users/"+testMailbox+"/messages/m1" {
		t.Fatalf("requests = %q", reqs)
	}
	e.g.mu.Lock()
	ups, cats := e.box.updates, e.box.messages["m1"].m.Categories
	e.g.mu.Unlock()
	if len(ups) != 1 || !slices.Equal(ups[0].Categories, []string{"Desk", "Follow up"}) || !slices.Equal(cats, []string{"Desk", "Follow up"}) {
		t.Errorf("update bodies = %+v, categories after = %v", ups, cats)
	}
	if out, err := execute(t, e, eff, `{"set":[]}`); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("clear: Execute = %+v, %v", out, err)
	}
	e.g.mu.Lock()
	ups, cats = e.box.updates, e.box.messages["m1"].m.Categories
	e.g.mu.Unlock()
	if len(ups) != 2 || len(ups[1].Categories) != 0 || len(cats) != 0 {
		t.Errorf("clear: update bodies = %+v, categories after = %v", ups, cats)
	}
	if body := string(encode(gCategories{Categories: []string{}})); body != `{"categories":[]}` {
		t.Errorf("clear wire body = %s", body)
	}
}

// Target, when set, wins over the route's parent and must be a provider
// message of this channel.
func TestExecuteCategoriesTargetPrecedence(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", "peer@example.test", "hi")
	e.inbound("m2", "peer@example.test", "again")
	target := model.ProviderMessageRef(testChan, "m2")
	eff := effect(route("peer@example.test", "m1"), "a1")
	eff.Target = &target
	out, err := execute(t, e, eff, `{"set":["Desk"]}`)
	if err != nil || out.State != model.ProviderAccepted || !slices.Equal(out.References, []model.ExternalRef{target}) {
		t.Fatalf("Execute = %+v, %v", out, err)
	}
	if !strings.HasSuffix(e.g.requests()[0], "/messages/m2") {
		t.Errorf("requests = %q", e.g.requests())
	}
	for name, bad := range map[string]model.ExternalRef{
		"other channel": model.ProviderMessageRef(model.ChannelRef{Tenant: "t1", ID: "00000000-0000-7000-8000-0000000000c2"}, "m2"),
		"not a message": {Channel: testChan, Namespace: IdentityNamespace, Kind: refKindAddress, ID: "peer@example.test"},
		"empty ID":      model.ProviderMessageRef(testChan, ""),
	} {
		t.Run(name, func(t *testing.T) {
			eff := effect(route("peer@example.test", "m1"), "a2")
			eff.Target = &bad
			out, err := execute(t, e, eff, `{"set":["Desk"]}`)
			if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Validation {
				t.Fatalf("Execute = %+v, %v; want rejected validation", out, err)
			}
		})
	}
	if n := e.g.count("PATCH"); n != 1 {
		t.Errorf("update calls = %d, want only the first", n)
	}
}

// A route without a parent message and no Target names nothing to
// categorize; invalid actions, other names and versions never reach Graph.
func TestExecuteCategoriesRefusals(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", "peer@example.test", "hi")
	out, err := execute(t, e, effect(route("peer@example.test", ""), "a1"), `{"set":["Desk"]}`)
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Validation {
		t.Fatalf("no message: Execute = %+v, %v; want rejected validation", out, err)
	}
	eff := effect(route("peer@example.test", "m1"), "a1")
	for _, c := range []struct {
		name   string
		action model.Extension
		code   model.ErrorCode
	}{
		{"duplicate", categoriesExt(`{"set":["Desk","Desk"]}`), model.Validation},
		{"missing set", categoriesExt(`{}`), model.Validation},
		{"other version", model.Extension{Name: ExtensionCategories, Version: 2, Data: json.RawMessage(`{"set":["Desk"]}`)}, model.SchemaMismatch},
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
	if _, err := execute(t, e, effect(route("not a route", "m1"), "a1"), `{"set":["Desk"]}`); model.CodeOf(err) != model.Permanent {
		t.Errorf("foreign route error = %v, want permanent", err)
	}
}

// Graph's answers map as documented: a message that is gone is a final
// rejected not_found; a missing permission unauthorized; throttling
// rate_limited; a 5xx or a lost answer transient, since the update is
// idempotent.
func TestExecuteCategoriesOutcomeMapping(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", "peer@example.test", "hi")
	eff := effect(route("peer@example.test", "m1"), "a1")

	out, err := execute(t, e, effect(route("peer@example.test", "gone"), "a1"), `{"set":["Desk"]}`)
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.NotFound {
		t.Fatalf("gone message: Execute = %+v, %v; want rejected not_found", out, err)
	}
	for _, c := range []struct {
		name   string
		status int
		code   model.ErrorCode
	}{
		{"missing permission", http.StatusForbidden, model.Unauthorized},
		{"throttled", http.StatusTooManyRequests, model.RateLimited},
		{"server error", http.StatusInternalServerError, model.Transient},
	} {
		t.Run(c.name, func(t *testing.T) {
			e.g.mu.Lock()
			e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method != "PATCH" {
					return false
				}
				graphError(w, c.status, "ErrorFake")
				return true
			}
			e.g.mu.Unlock()
			out, err := execute(t, e, eff, `{"set":["Desk"]}`)
			if out.State != "" || model.CodeOf(err) != c.code {
				t.Fatalf("Execute = %+v, %v; want error %s", out, err, c.code)
			}
		})
	}
	e.g.mu.Lock()
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "PATCH" {
			panic(http.ErrAbortHandler) // the connection drops: no answer
		}
		return false
	}
	e.g.mu.Unlock()
	out, err = execute(t, e, eff, `{"set":["Desk"]}`)
	if out.State != "" || model.CodeOf(err) != model.Transient {
		t.Fatalf("lost answer: Execute = %+v, %v; want transient, never outcome_unknown", out, err)
	}
}

// Asked to reconcile a categories effect, the adapter answers that a new
// attempt is safe without reading anything.
func TestReconcileCategoriesEffectAllowsNewAttempt(t *testing.T) {
	e := newEnv(t, Config{})
	out, err := e.inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: effect(route("peer@example.test", "m1"), "a1"), Capability: model.CapabilityExtensionExecute})
	if err != nil || out.State != model.Failed || out.Failure == nil || out.Failure.Code != model.Transient {
		t.Fatalf("Reconcile = %+v, %v; want failed transient", out, err)
	}
	if len(e.g.requests()) != 0 {
		t.Errorf("requests = %q, want none", e.g.requests())
	}
}

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
