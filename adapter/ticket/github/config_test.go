package github_test

import (
	"encoding/json"
	"slices"
	"testing"

	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/model"
)

func TestValidate(t *testing.T) {
	f := github.NewFactory(github.Options{})
	for name, tc := range map[string]struct {
		cfg  model.Extension
		want bool
	}{
		"empty":            {github.Config{}.Extension(), true},
		"enterprise":       {github.Config{APIURL: "https://ghe.example.com/api/v3/", Replies: true, AccountID: 7}.Extension(), true},
		"local fields":     {github.Config{LocalFields: []string{model.WorkItemFieldStatus, model.WorkItemFieldAssignees}}.Extension(), true},
		"plain http":       {github.Config{APIURL: "http://ghe.example.com/api/v3/"}.Extension(), false},
		"user info":        {github.Config{APIURL: "https://u:p@ghe.example.com/"}.Extension(), false}, // #nosec G101 -- rejected fixture URL
		"replies no id":    {github.Config{Replies: true}.Extension(), false},
		"unknown field":    {model.Extension{Name: github.ConfigSchema, Version: 1, Data: json.RawMessage(`{"token":"x"}`)}, false},
		"provider field":   {github.Config{LocalFields: []string{"url"}}.Extension(), false},
		"duplicate field":  {github.Config{LocalFields: []string{"status", "status"}}.Extension(), false},
		"other schema":     {model.Extension{Name: "github.issues.settings", Version: 1}, false},
		"other version":    {model.Extension{Name: github.ConfigSchema, Version: 2}, false},
		"negative account": {github.Config{AccountID: -1}.Extension(), false},
		"history repos":    {github.Config{HistoryRepositories: []string{"example/one", "example/two"}}.Extension(), true},
		"history not repo": {github.Config{HistoryRepositories: []string{"example"}}.Extension(), false},
		"history bad name": {github.Config{HistoryRepositories: []string{"example/../x"}}.Extension(), false},
		"history twice":    {github.Config{HistoryRepositories: []string{"example/one", "Example/One"}}.Extension(), false},
		"history too many": {github.Config{HistoryRepositories: []string{"o/r1", "o/r2", "o/r3", "o/r4", "o/r5", "o/r6", "o/r7", "o/r8", "o/r9", "o/r10", "o/r11"}}.Extension(), false},
	} {
		if issues := f.Validate(tc.cfg); (len(issues) == 0) != tc.want {
			t.Errorf("%s: issues = %+v, want valid %v", name, issues, tc.want)
		}
	}
}

func TestDescriptor(t *testing.T) {
	d := github.NewFactory(github.Options{}).Describe()
	if d.Type != model.TypeGithubIssues || model.ValidateTypeKey(d.Type) != nil || len(d.IdentityNamespaces) != 1 || d.IdentityNamespaces[0] != github.Namespace {
		t.Fatalf("descriptor = %+v", d)
	}
	if !slices.Equal(d.SenderKinds, []string{model.ContactKindBot}) {
		t.Fatalf("sender kinds = %v, want bot only", d.SenderKinds)
	}
}
