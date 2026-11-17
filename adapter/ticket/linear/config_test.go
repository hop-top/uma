package linear_test

import (
	"encoding/json"
	"testing"

	linear "hop.top/uma/adapter/ticket/linear"
	"hop.top/uma/model"
)

func TestConfigValidation(t *testing.T) {
	f := linear.NewFactory(linear.Options{})
	for name, c := range map[string]struct {
		x    model.Extension
		want int
	}{
		"empty":           {linear.Config{}.Extension(), 0},
		"replies":         {linear.Config{Replies: true, AccountID: botID}.Extension(), 0},
		"replies no id":   {linear.Config{Replies: true}.Extension(), 1},
		"account not id":  {linear.Config{AccountID: "bob"}.Extension(), 1},
		"http api":        {linear.Config{APIURL: "http://api.linear.app/graphql"}.Extension(), 1},
		"user info":       {linear.Config{APIURL: "https://user@api.linear.app/graphql"}.Extension(), 1},
		"local fields":    {linear.Config{LocalFields: []string{"nope"}}.Extension(), 1},
		"wrong schema":    {model.Extension{Name: "other", Version: 1}, 1},
		"unknown field":   {model.Extension{Name: linear.ConfigSchema, Version: 1, Data: json.RawMessage(`{"x":1}`)}, 1},
		"default api url": {linear.Config{APIURL: linear.DefaultAPIURL}.Extension(), 0},
	} {
		if got := len(f.Validate(c.x)); got != c.want {
			t.Errorf("%s: %d issues, want %d", name, got, c.want)
		}
	}
}

func TestDescriptor(t *testing.T) {
	d := linear.NewFactory(linear.Options{}).Describe()
	if d.Type != model.TypeLinearIssues || d.Maturity != "experimental" {
		t.Fatalf("descriptor %+v", d)
	}
	want := []model.Capability{
		model.CapabilityReceiveHTTP, model.CapabilitySend, model.CapabilityReconcile, model.CapabilityRefetch,
		model.CapabilityEdit, model.CapabilityDelete,
	}
	if len(d.Capabilities) != len(want) {
		t.Fatalf("capabilities %v, want %v", d.Capabilities, want)
	}
}
