package httpmail_test

import (
	"encoding/json"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/httpmail"
	"hop.top/uma/adapter/web/webhookauth"
	"hop.top/uma/model"
)

func issuesOf(t *testing.T, cfg httpmail.Config, opts ...func(*httpmail.Options)) []model.ValidationIssue {
	t.Helper()
	var o httpmail.Options
	for _, f := range opts {
		f(&o)
	}
	return httpmail.NewFactory(o).Validate(cfg.Extension())
}

func wantIssue(t *testing.T, issues []model.ValidationIssue, field string) {
	t.Helper()
	for _, is := range issues {
		if is.Field == field {
			return
		}
	}
	t.Errorf("no issue for %s in %+v", field, issues)
}

func TestBaseConfigIsValid(t *testing.T) {
	if issues := issuesOf(t, baseConfig()); len(issues) != 0 {
		t.Fatalf("issues %+v", issues)
	}
}

func TestDescriptor(t *testing.T) {
	d := httpmail.NewFactory(httpmail.Options{}).Describe()
	if d.Type != model.TypeHttpMail || d.Maturity != "experimental" {
		t.Errorf("descriptor %+v", d)
	}
	if len(d.Capabilities) != 1 || d.Capabilities[0] != model.CapabilityReceiveHTTP {
		t.Errorf("capabilities %v: the adapter only receives over HTTP", d.Capabilities)
	}
	if d.Exchange.MessageIDs != adapter.MessageIDsSender || d.Exchange.Threading != adapter.ThreadingEmail {
		t.Errorf("exchange %+v", d.Exchange)
	}
}

func TestConfigSchemaIsChecked(t *testing.T) {
	f := httpmail.NewFactory(httpmail.Options{})
	issues := f.Validate(model.Extension{Name: "smtp.mail.config", Version: 1, Data: json.RawMessage(`{}`)})
	if len(issues) != 1 || issues[0].Code != string(model.SchemaMismatch) {
		t.Errorf("issues %+v", issues)
	}
	issues = f.Validate(model.Extension{Name: httpmail.ConfigSchema, Version: httpmail.ConfigVersion, Data: json.RawMessage(`{"recipients":[],"unknown":1}`)})
	if len(issues) != 1 || issues[0].Field != "configuration.data" {
		t.Errorf("unknown members must be refused: %+v", issues)
	}
}

func TestConfigRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*httpmail.Config)
		field  string
	}{
		"no recipients":     {func(c *httpmail.Config) { c.Recipients = nil }, "recipients"},
		"bad address":       {func(c *httpmail.Config) { c.Recipients[0].Address = "Support <a@b.test>" }, "recipients[0].address"},
		"duplicate address": {func(c *httpmail.Config) { c.Recipients[1].Address = "support@EXAMPLE.com" }, "recipients[1].address"},
		"no destination":    {func(c *httpmail.Config) { c.Recipients[0].Destination = " " }, "recipients[0].destination"},
		"unknown format":    {func(c *httpmail.Config) { c.Formats = []string{"xml"} }, "formats[0]"},
		"repeated format":   {func(c *httpmail.Config) { c.Formats = []string{"json", "json"} }, "formats[1]"},
		"negative body":     {func(c *httpmail.Config) { c.MaxBodyBytes = -1 }, "max_body_bytes"},
		"two roles one member": {func(c *httpmail.Config) {
			c.JSONFields.To = "from"
		}, "json_fields.to"},
		"control character in member": {func(c *httpmail.Config) { c.JSONFields.Body = "bo\ndy" }, "json_fields.body"},
		"attachment members collide": {func(c *httpmail.Config) {
			c.JSONFields.AttachmentFields.URL = "name"
		}, "json_fields.attachment_fields.name"},
		"content collides with name": {func(c *httpmail.Config) {
			c.JSONFields.AttachmentFields.Content = "name"
		}, "json_fields.attachment_fields.content"},
		"recipient header without rfc822": {func(c *httpmail.Config) { c.RecipientHeader = "X-Envelope-To" }, "recipient_header"},
		"bad recipient header": {func(c *httpmail.Config) {
			c.Formats = []string{"rfc822"}
			c.RecipientHeader = "X Envelope"
		}, "recipient_header"},
		"authserv id with space":     {func(c *httpmail.Config) { c.Evidence.TrustedAuthservID = "mx example.com" }, "evidence.trusted_authserv_id"},
		"authserv id with semicolon": {func(c *httpmail.Config) { c.Evidence.TrustedAuthservID = "mx;example.com" }, "evidence.trusted_authserv_id"},
		"results field without trust": {func(c *httpmail.Config) {
			c.Evidence.AuthenticationResultsField = "authentication_results"
		}, "evidence.authentication_results_field"},
		"results field without json": {func(c *httpmail.Config) {
			c.Formats = []string{"rfc822"}
			c.Evidence = httpmail.Evidence{TrustedAuthservID: "mx.example.com", AuthenticationResultsField: "ar"}
		}, "evidence.authentication_results_field"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Recipients = append([]httpmail.Recipient(nil), cfg.Recipients...)
			tc.mutate(&cfg)
			wantIssue(t, issuesOf(t, cfg), tc.field)
		})
	}
}

// webhookauth refuses what could accept an unauthenticated request; the
// adapter reports it as an issue, never accepts it.
func TestAuthIsRequiredAndJudgedByWebhookauth(t *testing.T) {
	for name, a := range map[string]httpmail.Auth{
		"no scheme":              {},
		"unknown scheme":         {Scheme: "none"},
		"hash on bearer":         {Scheme: "bearer", Hash: "sha256"},
		"unsigned timestamp":     {Scheme: "hmac", Payload: "{body}", Timestamp: &httpmail.AuthTimestamp{Header: "X-Ts"}},
		"payload without body":   {Scheme: "hmac", Payload: "{timestamp}"},
		"negative tolerance":     {Scheme: "hmac", Timestamp: &httpmail.AuthTimestamp{Header: "X-Ts", ToleranceSeconds: -1}},
		"replay on bearer":       {Scheme: "bearer", Replay: &httpmail.AuthReplay{Source: "signature"}},
		"negative replay ttl":    {Scheme: "hmac", Replay: &httpmail.AuthReplay{Source: "signature", TTLSeconds: -5}},
		"unknown encoding":       {Scheme: "hmac", Encoding: "rot13"},
		"bad header name":        {Scheme: "token", Header: "X Token"},
		"ed25519 with a hash":    {Scheme: "ed25519", Hash: "sha256"},
		"replay header unsigned": {Scheme: "hmac", Replay: &httpmail.AuthReplay{Source: "header", Header: "X-Id"}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Auth = a
			store := func(o *httpmail.Options) { o.ReplayStore = webhookauth.NewMemoryReplayStore(4, nil) }
			issues := issuesOf(t, cfg, store)
			if len(issues) == 0 {
				t.Fatal("accepted")
			}
			if !strings.HasPrefix(issues[0].Field, "auth") {
				t.Errorf("issue %+v", issues[0])
			}
		})
	}
}

func TestReplayNeedsTheHostsStore(t *testing.T) {
	cfg := baseConfig()
	cfg.Auth = httpmail.Auth{Scheme: "hmac", Replay: &httpmail.AuthReplay{Source: "signature"}}
	wantIssue(t, issuesOf(t, cfg), "auth.replay")
	if issues := issuesOf(t, cfg, func(o *httpmail.Options) { o.ReplayStore = webhookauth.NewMemoryReplayStore(4, nil) }); len(issues) != 0 {
		t.Errorf("issues %+v", issues)
	}
	_, err := httpmail.NewFactory(httpmail.Options{}).Open(t.Context(), channelOf(cfg), adapter.Dependencies{Credentials: newCredHost()})
	if !model.IsCode(err, model.Validation) {
		t.Errorf("Open without a store: %v", err)
	}
}

func TestOpenRefusesOtherTypesAndBadConfiguration(t *testing.T) {
	f := httpmail.NewFactory(httpmail.Options{})
	ch := channelOf(baseConfig())
	ch.Type = model.TypeSmtpMail
	if _, err := f.Open(t.Context(), ch, adapter.Dependencies{}); !model.IsCode(err, model.Validation) {
		t.Errorf("wrong type: %v", err)
	}
	ch = channelOf(baseConfig())
	ch.Configuration.Data = json.RawMessage(`{}`)
	if _, err := f.Open(t.Context(), ch, adapter.Dependencies{}); !model.IsCode(err, model.Validation) {
		t.Errorf("empty configuration: %v", err)
	}
}

func TestParseConfigAppliesDefaults(t *testing.T) {
	cfg, issues := httpmail.ParseConfig(baseConfig().Extension())
	if len(issues) != 0 {
		t.Fatal(issues)
	}
	f := cfg.JSONFields
	if f.From != "from" || f.To != "to" || f.Subject != "subject" || f.Body != "body" || f.Attachments != "attachments" || f.AttachmentFields.SizeBytes != "size_bytes" || f.AttachmentFields.Content != "content" {
		t.Errorf("fields %+v", f)
	}
	if len(cfg.Formats) != 1 || cfg.Formats[0] != httpmail.FormatJSON || cfg.MaxBodyBytes != httpmail.DefaultMaxBodyBytes {
		t.Errorf("formats %v, max body %d", cfg.Formats, cfg.MaxBodyBytes)
	}
}

func TestBodyLimitIsConfiguration(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxBodyBytes = 100
	r := newRig(t, cfg)
	wantStatus(t, r.postJSON(`{"from":"a@peers.test","to":"support@example.com","body":"`+strings.Repeat("x", 200)+`"}`), 413)
}
