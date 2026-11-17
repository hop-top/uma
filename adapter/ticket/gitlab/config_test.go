package gitlab_test

import (
	"encoding/json"
	"slices"
	"testing"

	"hop.top/uma/adapter"
	gitlab "hop.top/uma/adapter/ticket/gitlab"
	"hop.top/uma/model"
)

func TestValidate(t *testing.T) {
	f := gitlab.NewFactory(gitlab.Options{})
	for name, tc := range map[string]struct {
		cfg  model.Extension
		want bool
	}{
		"empty":            {gitlab.Config{}.Extension(), true},
		"self-managed":     {gitlab.Config{APIURL: "https://gitlab.example.com/api/v4/", Replies: true, AccountID: 7}.Extension(), true},
		"relative url":     {gitlab.Config{APIURL: "https://example.com/gitlab/api/v4"}.Extension(), true},
		"local fields":     {gitlab.Config{LocalFields: []string{model.WorkItemFieldStatus, model.WorkItemFieldAssignees}}.Extension(), true},
		"plain http":       {gitlab.Config{APIURL: "http://gitlab.example.com/api/v4/"}.Extension(), false},
		"user info":        {gitlab.Config{APIURL: "https://u:p@gitlab.example.com/"}.Extension(), false}, // #nosec G101 -- rejected fixture URL
		"query":            {gitlab.Config{APIURL: "https://gitlab.example.com/?x=1"}.Extension(), false},
		"replies no id":    {gitlab.Config{Replies: true}.Extension(), false},
		"unknown field":    {model.Extension{Name: gitlab.ConfigSchema, Version: 1, Data: json.RawMessage(`{"token":"x"}`)}, false},
		"provider field":   {gitlab.Config{LocalFields: []string{"url"}}.Extension(), false},
		"duplicate field":  {gitlab.Config{LocalFields: []string{"status", "status"}}.Extension(), false},
		"other schema":     {model.Extension{Name: "gitlab.issues.settings", Version: 1}, false},
		"other version":    {model.Extension{Name: gitlab.ConfigSchema, Version: 2}, false},
		"negative account": {gitlab.Config{AccountID: -1}.Extension(), false},
	} {
		if issues := f.Validate(tc.cfg); (len(issues) == 0) != tc.want {
			t.Errorf("%s: issues = %+v, want valid %v", name, issues, tc.want)
		}
	}
}

func TestDescriptor(t *testing.T) {
	d := gitlab.NewFactory(gitlab.Options{}).Describe()
	if d.Type != model.TypeGitlabIssues || model.ValidateTypeKey(d.Type) != nil || len(d.IdentityNamespaces) != 1 || d.IdentityNamespaces[0] != gitlab.Namespace {
		t.Fatalf("descriptor = %+v", d)
	}
	if d.Maturity != "experimental" {
		t.Fatalf("maturity = %q, want experimental", d.Maturity)
	}
	if !slices.Equal(d.SenderKinds, []string{model.ContactKindBot}) {
		t.Fatalf("sender kinds = %v, want bot only", d.SenderKinds)
	}
	for _, c := range []model.Capability{model.CapabilityReact, model.CapabilityExtensionExecute, model.CapabilityInternalNote} {
		if slices.Contains(d.Capabilities, c) {
			t.Errorf("declares %s, which the adapter does not implement", c)
		}
	}
}

// Capabilities follow the configuration: replies off leaves reads, acts and
// history available and the writes disabled.
func TestCapabilitiesFollowReplies(t *testing.T) {
	r := newRig(t)
	for _, tc := range []struct {
		cfg     gitlab.Config
		replies bool
	}{{replyConfig(r.gl.apiURL("a")), true}, {gitlab.Config{APIURL: r.gl.apiURL("a")}, false}} {
		st, err := r.open(tc.cfg).Capabilities(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range st {
			write := s.Name == model.CapabilitySend || s.Name == model.CapabilityEdit || s.Name == model.CapabilityDelete
			if want := !write || tc.replies; s.Available != want || (!want && s.Reason != model.Disabled) {
				t.Errorf("replies=%v: %+v", tc.replies, s)
			}
		}
	}
}

func TestOpenRefusals(t *testing.T) {
	f := gitlab.NewFactory(gitlab.Options{})
	_, err := f.Open(t.Context(), model.Channel{Ref: chanA, Type: model.TypeGithubIssues, Configuration: gitlab.Config{}.Extension()}, adapter.Dependencies{})
	wantCode(t, err, model.Validation)
	_, err = f.Open(t.Context(), model.Channel{Ref: chanA, Type: gitlab.Type, Configuration: model.Extension{Name: "x", Version: 1}}, adapter.Dependencies{})
	wantCode(t, err, model.Validation)
}
