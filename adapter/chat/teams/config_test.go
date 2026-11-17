package teams

import (
	"slices"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func TestConfigValidated(t *testing.T) {
	f := NewFactory(Options{})
	good := Config{AppID: testAppID, TenantID: testTenantID, ServiceURL: "https://smba.trafficmanager.net/teams/"}
	if issues := f.Validate(good.Extension()); len(issues) != 0 {
		t.Fatalf("issues %+v", issues)
	}
	cases := map[string]struct {
		cfg   model.Extension
		field string
	}{
		"schema":        {model.Extension{Name: "other", Version: 1}, "configuration"},
		"unknown":       {model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: []byte(`{"app_id":"x","extra":1}`)}, "configuration.data"},
		"app id":        {Config{AppID: "bot", TenantID: testTenantID, ServiceURL: good.ServiceURL}.Extension(), "configuration.data.app_id"},
		"tenant":        {Config{AppID: testAppID, ServiceURL: good.ServiceURL}.Extension(), "configuration.data.tenant_id"},
		"service http":  {Config{AppID: testAppID, TenantID: testTenantID, ServiceURL: "http://smba.trafficmanager.net/teams/"}.Extension(), "configuration.data.service_url"},
		"service user":  {Config{AppID: testAppID, TenantID: testTenantID, ServiceURL: "https://u:p@smba.trafficmanager.net/"}.Extension(), "configuration.data.service_url"}, // #nosec G101 -- fixture URL, not a credential
		"service query": {Config{AppID: testAppID, TenantID: testTenantID, ServiceURL: "https://smba.trafficmanager.net/?x=1"}.Extension(), "configuration.data.service_url"},
		"service none":  {Config{AppID: testAppID, TenantID: testTenantID}.Extension(), "configuration.data.service_url"},
	}
	for name, c := range cases {
		issues := f.Validate(c.cfg)
		if !slices.ContainsFunc(issues, func(i model.ValidationIssue) bool { return i.Field == c.field }) {
			t.Errorf("%s: issues %+v, want one on %s", name, issues, c.field)
		}
	}
	if _, err := f.Open(t.Context(), model.Channel{Ref: testChannel, Type: Type, Configuration: Config{}.Extension()}, adapter.Dependencies{}); model.CodeOf(err) != model.Validation {
		t.Fatalf("Open with a bad configuration: %v", err)
	}
	if _, err := f.Open(t.Context(), model.Channel{Ref: testChannel, Type: model.TypeSlackChat, Configuration: good.Extension()}, adapter.Dependencies{}); model.CodeOf(err) != model.Validation {
		t.Fatalf("Open of another type: %v", err)
	}
}

func TestDescriptor(t *testing.T) {
	d := NewFactory(Options{}).Describe()
	if d.Type != model.TypeMicrosoftTeams || d.ContractMajor != adapter.ContractMajor || d.Maturity != "experimental" ||
		d.Exchange.MessageIDs != adapter.MessageIDsProvider || !slices.Equal(d.SenderKinds, []string{model.ContactKindBot}) ||
		!slices.Equal(d.IdentityNamespaces, []string{UserNamespace}) {
		t.Fatalf("descriptor %+v", d)
	}
	if err := model.ValidateTypeKey(d.Type); err != nil {
		t.Fatalf("type key: %v", err)
	}
	x := newRig(t).x
	st, err := x.Capabilities(t.Context())
	if err != nil || len(st) != len(d.Capabilities) {
		t.Fatalf("%+v %v", st, err)
	}
	for _, s := range st {
		if !s.Available || s.Reason != "" {
			t.Fatalf("status %+v", s)
		}
	}
}
