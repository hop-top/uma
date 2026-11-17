package jira_test

import (
	"encoding/json"
	"slices"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/ticket/jira"
	"hop.top/uma/model"
)

func TestValidate(t *testing.T) {
	f := jira.NewFactory()
	site := "https://example.atlassian.net"
	for name, tc := range map[string]struct {
		cfg  model.Extension
		want bool
	}{
		"site only":       {jira.Config{SiteURL: site}.Extension(), true},
		"trailing slash":  {jira.Config{SiteURL: site + "/"}.Extension(), true},
		"replies":         {jira.Config{SiteURL: site, Replies: true, AccountID: "5b10ac8d82e05b22cc7d4ef5"}.Extension(), true},
		"oauth":           {jira.Config{SiteURL: site, CloudID: "1324a887-45db-1bf4-1e99-ef0ff456d421"}.Extension(), true},
		"api token email": {jira.Config{SiteURL: site, Email: "bot@example.com"}.Extension(), true},
		"local fields":    {jira.Config{SiteURL: site, LocalFields: []string{model.WorkItemFieldStatus}}.Extension(), true},
		"no site":         {jira.Config{}.Extension(), false},
		"plain http":      {jira.Config{SiteURL: "http://example.atlassian.net"}.Extension(), false},
		"user info":       {jira.Config{SiteURL: "https://u:p@example.atlassian.net"}.Extension(), false}, // #nosec G101 -- rejected fixture URL
		"path":            {jira.Config{SiteURL: site + "/jira"}.Extension(), false},
		"query":           {jira.Config{SiteURL: site + "?a=b"}.Extension(), false},
		"replies no id":   {jira.Config{SiteURL: site, Replies: true}.Extension(), false},
		"bad account":     {jira.Config{SiteURL: site, AccountID: "a/b"}.Extension(), false},
		"bad cloud id":    {jira.Config{SiteURL: site, CloudID: "../x"}.Extension(), false},
		"bad email":       {jira.Config{SiteURL: site, Email: "a:b@example.com"}.Extension(), false},
		"unknown field":   {model.Extension{Name: jira.ConfigSchema, Version: 1, Data: json.RawMessage(`{"site_url":"` + site + `","token":"x"}`)}, false},
		"provider field":  {jira.Config{SiteURL: site, LocalFields: []string{"url"}}.Extension(), false},
		"duplicate field": {jira.Config{SiteURL: site, LocalFields: []string{"status", "status"}}.Extension(), false},
		"other schema":    {model.Extension{Name: "jira.issues.settings", Version: 1}, false},
		"other version":   {model.Extension{Name: jira.ConfigSchema, Version: 2}, false},
		"no data":         {model.Extension{Name: jira.ConfigSchema, Version: 1}, false},
	} {
		if issues := f.Validate(tc.cfg); (len(issues) == 0) != tc.want {
			t.Errorf("%s: issues = %+v, want valid %v", name, issues, tc.want)
		}
	}
}

func TestDescriptor(t *testing.T) {
	d := jira.NewFactory().Describe()
	if d.Type != model.TypeJiraIssues || model.ValidateTypeKey(d.Type) != nil || len(d.IdentityNamespaces) != 1 || d.IdentityNamespaces[0] != jira.Namespace {
		t.Fatalf("descriptor = %+v", d)
	}
	if !slices.Equal(d.SenderKinds, []string{model.ContactKindBot}) || d.Maturity != "experimental" {
		t.Fatalf("sender kinds %v, maturity %q", d.SenderKinds, d.Maturity)
	}
	// Only what the code serves is declared, and each declaration has its
	// interface.
	want := []model.Capability{model.CapabilityReceiveHTTP, model.CapabilitySend, model.CapabilityReconcile, model.CapabilityRefetch, model.CapabilityEdit, model.CapabilityDelete}
	if !slices.Equal(d.Capabilities, want) {
		t.Fatalf("capabilities = %v, want %v", d.Capabilities, want)
	}
	if len(d.WorkItemActs) != 0 {
		t.Fatalf("declares work item acts %v", d.WorkItemActs)
	}
	r := newRig(t)
	_ = r.inst.(adapter.HTTPReceiver)
	_ = r.inst.(adapter.Sender)
	_ = r.inst.(adapter.Editor)
	_ = r.inst.(adapter.Deleter)
	_ = r.inst.(adapter.Reconciler)
	_ = r.inst.(adapter.Refetcher)
	if _, ok := r.inst.(adapter.WorkItemActor); ok {
		t.Fatal("implements WorkItemActor without declaring work_item_act")
	}
}

func TestOpenRefusals(t *testing.T) {
	f := jira.NewFactory()
	ch := model.Channel{Ref: chanA, Type: jira.Type, Configuration: jira.Config{SiteURL: "https://example.atlassian.net"}.Extension(), Revision: "ch1"}
	if _, err := f.Open(t.Context(), ch, adapter.Dependencies{}); err != nil {
		t.Fatalf("Open without dependencies = %v; it performs no I/O", err)
	}
	wrong := ch
	wrong.Type = "github.issues"
	if _, err := f.Open(t.Context(), wrong, adapter.Dependencies{}); !model.IsCode(err, model.Validation) {
		t.Errorf("wrong type: %v", err)
	}
	bad := ch
	bad.Configuration = jira.Config{}.Extension()
	if _, err := f.Open(t.Context(), bad, adapter.Dependencies{}); !model.IsCode(err, model.Validation) {
		t.Errorf("bad configuration: %v", err)
	}
	// Without an HTTP client, a provider call is unavailable.
	inst, _ := f.Open(t.Context(), ch, adapter.Dependencies{})
	_, err := inst.(adapter.Refetcher).Refetch(t.Context(), adapter.RefetchRequest{Target: route(issueID)})
	if !model.IsCode(err, model.Unavailable) {
		t.Errorf("no HTTP client: %v, want unavailable", err)
	}
	// A closed instance's calls are unavailable and Close is idempotent.
	r := newRig(t)
	_ = r.inst.Close(t.Context())
	_ = r.inst.Close(t.Context())
	if _, err := r.send(tryOne, "x"); !model.IsCode(err, model.Unavailable) {
		t.Errorf("closed: %v", err)
	}
}
