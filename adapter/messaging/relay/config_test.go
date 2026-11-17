package relay_test

import (
	"encoding/json"
	"strings"
	"testing"

	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
)

// baseConfig is the smallest valid channel configuration.
func baseConfig() relay.Config {
	return relay.Config{Medium: relay.MediumSMS, Auth: relay.Auth{Scheme: "bearer"}}
}

func ext(c relay.Config) model.Extension { return c.Extension() }

func fields(issues []model.ValidationIssue) []string {
	var out []string
	for _, i := range issues {
		out = append(out, i.Field)
	}
	return out
}

func TestValidateAcceptsMinimalConfig(t *testing.T) {
	f := relay.NewFactory(relay.Options{})
	if issues := f.Validate(ext(baseConfig())); len(issues) != 0 {
		t.Fatalf("issues: %v", issues)
	}
}

func TestValidateSchema(t *testing.T) {
	f := relay.NewFactory(relay.Options{})
	e := ext(baseConfig())
	e.Name = "other.config"
	issues := f.Validate(e)
	if len(issues) != 1 || issues[0].Code != string(model.SchemaMismatch) {
		t.Fatalf("issues: %v", issues)
	}
	e = ext(baseConfig())
	e.Version = 2
	if issues := f.Validate(e); len(issues) != 1 || issues[0].Code != string(model.SchemaMismatch) {
		t.Fatalf("version: %v", issues)
	}
}

func TestValidateRefusesUnknownFields(t *testing.T) {
	f := relay.NewFactory(relay.Options{})
	e := ext(baseConfig())
	e.Data = json.RawMessage(`{"medium":"sms","auth":{"scheme":"bearer"},"webhook_secret":"x"}`)
	if issues := f.Validate(e); len(issues) == 0 {
		t.Fatal("unknown field accepted: a secret in configuration must be refused")
	}
}

func TestValidateRules(t *testing.T) {
	f := relay.NewFactory(relay.Options{CallbackHosts: []string{"relay.example.com", "gw.example.com:8443"}})
	tests := []struct {
		name   string
		mutate func(*relay.Config)
		field  string // the first issue's field; empty: valid
	}{
		{"medium required", func(c *relay.Config) { c.Medium = "" }, "medium"},
		{"medium unknown", func(c *relay.Config) { c.Medium = "mms" }, "medium"},
		{"whatsapp medium", func(c *relay.Config) { c.Medium = relay.MediumWhatsApp }, ""},
		{"imessage medium", func(c *relay.Config) { c.Medium = relay.MediumIMessage }, ""},
		{"medium is case-sensitive", func(c *relay.Config) { c.Medium = "iMessage" }, "medium"},
		{"scheme required", func(c *relay.Config) { c.Auth.Scheme = "" }, "auth"},
		{"scheme none is not a scheme", func(c *relay.Config) { c.Auth.Scheme = "none" }, "auth"},
		{"hmac with legacy header", func(c *relay.Config) {
			c.Auth = relay.Auth{Scheme: "hmac", Header: "X-Relay-Signature", Prefix: "sha256=", PrefixOptional: true}
		}, ""},
		{"unsigned timestamp needs consent", func(c *relay.Config) {
			c.Auth.Timestamp = relay.AuthTimestamp{Header: "X-Ts"}
		}, "auth"},
		{"unsigned timestamp with consent", func(c *relay.Config) {
			c.Auth.Timestamp = relay.AuthTimestamp{Header: "X-Ts", AllowUnsigned: true, Tolerance: "2m"}
		}, ""},
		{"bad tolerance", func(c *relay.Config) {
			c.Auth.Timestamp = relay.AuthTimestamp{Header: "X-Ts", AllowUnsigned: true, Tolerance: "soon"}
		}, "auth.timestamp.tolerance"},
		{"bad replay ttl", func(c *relay.Config) {
			c.Auth.Replay = relay.AuthReplay{Source: "header", Header: "X-Id", AllowUnsigned: true, TTL: "-"}
		}, "auth.replay.ttl"},
		{"replay by header with consent", func(c *relay.Config) {
			c.Auth.Replay = relay.AuthReplay{Source: "header", Header: "X-Id", AllowUnsigned: true}
		}, ""},
		{"empty field alias", func(c *relay.Config) { c.Inbound.Fields.From = []string{""} }, "inbound.fields.from"},
		{"field names renamed", func(c *relay.Config) {
			c.Inbound.Fields = relay.Fields{From: []string{"From", "from"}, To: []string{"To"}, Body: []string{"Body", "text"}, MessageID: []string{"MessageSid"}}
		}, ""},
		{"one name for two roles", func(c *relay.Config) {
			c.Inbound.Fields = relay.Fields{From: []string{"id"}, MessageID: []string{"id"}}
		}, "inbound.fields"},
		{"too many aliases", func(c *relay.Config) {
			c.Inbound.Fields.Body = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
		}, "inbound.fields.body"},
		{"body limit too large", func(c *relay.Config) { c.Inbound.MaxBodyBytes = 2 << 20 }, "inbound.max_body_bytes"},
		{"body limit negative", func(c *relay.Config) { c.Inbound.MaxBodyBytes = -1 }, "inbound.max_body_bytes"},
		{"status route on", func(c *relay.Config) { c.Inbound.Status.Enabled = true }, ""},
		{"status fields renamed", func(c *relay.Config) {
			c.Inbound.Status = relay.Status{Enabled: true, Fields: relay.StatusFields{
				Status: []string{"MessageStatus", "state"}, From: []string{"Line"}, OperationID: []string{"Op"},
				AttemptID: []string{"Attempt"}, MessageID: []string{"Sid"}, Code: []string{"ErrorCode"},
			}}
		}, ""},
		{"status fields share message names", func(c *relay.Config) {
			c.Inbound.Status.Fields = relay.StatusFields{From: []string{"from"}, MessageID: []string{"message_id"}}
		}, ""},
		{"status empty alias", func(c *relay.Config) { c.Inbound.Status.Fields.Status = []string{""} }, "inbound.status.fields.status"},
		{"status too many aliases", func(c *relay.Config) {
			c.Inbound.Status.Fields.Code = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
		}, "inbound.status.fields.code"},
		{"status one name for two roles", func(c *relay.Config) {
			c.Inbound.Status.Fields = relay.StatusFields{OperationID: []string{"id"}, AttemptID: []string{"id"}}
		}, "inbound.status.fields"},
		{"status key is a message name", func(c *relay.Config) {
			c.Inbound.Status.Fields.Status = []string{"body"}
		}, "inbound.status.fields.status"},
		{"status key is a renamed message name", func(c *relay.Config) {
			c.Inbound.Fields.Body = []string{"text"}
			c.Inbound.Status.Fields.Status = []string{"text"}
		}, "inbound.status.fields.status"},
		{"status names are checked while the route is off", func(c *relay.Config) {
			c.Inbound.Status.Fields.Status = []string{"to"}
		}, "inbound.status.fields.status"},
		{"callback allowed host", func(c *relay.Config) { c.Callback = &relay.Callback{URL: "https://relay.example.com/out"} }, ""},
		{"callback allowed host with port", func(c *relay.Config) { c.Callback = &relay.Callback{URL: "https://gw.example.com:8443/out"} }, ""},
		{"callback host case-insensitive", func(c *relay.Config) { c.Callback = &relay.Callback{URL: "https://RELAY.example.com/out"} }, ""},
		{"callback host outside the allowlist", func(c *relay.Config) { c.Callback = &relay.Callback{URL: "https://evil.example.net/out"} }, "callback.url"},
		{"callback allowlisted host, other port", func(c *relay.Config) { c.Callback = &relay.Callback{URL: "https://relay.example.com:9000/out"} }, "callback.url"},
		{"callback http refused", func(c *relay.Config) { c.Callback = &relay.Callback{URL: "http://relay.example.com/out"} }, "callback.url"},
		{"callback userinfo", func(c *relay.Config) { c.Callback = &relay.Callback{URL: "https://u:p@relay.example.com/out"} }, "callback.url"}, // #nosec G101 -- refused-userinfo fixture
		{"callback query", func(c *relay.Config) { c.Callback = &relay.Callback{URL: "https://relay.example.com/out?key=1"} }, "callback.url"},
		{"callback fragment", func(c *relay.Config) { c.Callback = &relay.Callback{URL: "https://relay.example.com/out#x"} }, "callback.url"},
		{"callback relative", func(c *relay.Config) { c.Callback = &relay.Callback{URL: "/out"} }, "callback.url"},
		{"callback empty url", func(c *relay.Config) { c.Callback = &relay.Callback{} }, "callback.url"},
		{"callback header auth needs a name", func(c *relay.Config) {
			c.Callback = &relay.Callback{URL: "https://relay.example.com/out", Auth: "header"}
		}, "callback.header"},
		{"callback header auth", func(c *relay.Config) {
			c.Callback = &relay.Callback{URL: "https://relay.example.com/out", Auth: "header", Header: "X-Relay-Token"}
		}, ""},
		{"callback bearer takes no header", func(c *relay.Config) {
			c.Callback = &relay.Callback{URL: "https://relay.example.com/out", Header: "X-Relay-Token"}
		}, "callback.header"},
		{"callback header must not be Host", func(c *relay.Config) {
			c.Callback = &relay.Callback{URL: "https://relay.example.com/out", Auth: "header", Header: "Host"}
		}, "callback.header"},
		{"callback header must not be the idempotency key", func(c *relay.Config) {
			c.Callback = &relay.Callback{URL: "https://relay.example.com/out", Auth: "header", Header: "Idempotency-Key"}
		}, "callback.header"},
		{"callback header with a space", func(c *relay.Config) {
			c.Callback = &relay.Callback{URL: "https://relay.example.com/out", Auth: "header", Header: "X Token"}
		}, "callback.header"},
		{"callback unknown auth", func(c *relay.Config) {
			c.Callback = &relay.Callback{URL: "https://relay.example.com/out", Auth: "basic"}
		}, "callback.auth"},
		// The lookup carries the callback credential, so it is held to the
		// callback's rules.
		{"lookup allowed host", func(c *relay.Config) { c.Callback = lookupAt("https://relay.example.com/lookup") }, ""},
		{"lookup on the callback URL", func(c *relay.Config) { c.Callback = lookupAt("https://relay.example.com/out") }, ""},
		{"lookup other allowed host", func(c *relay.Config) { c.Callback = lookupAt("https://gw.example.com:8443/lookup") }, ""},
		{"lookup host outside the allowlist", func(c *relay.Config) { c.Callback = lookupAt("https://evil.example.net/lookup") }, "callback.lookup_url"},
		{"lookup allowlisted host, other port", func(c *relay.Config) { c.Callback = lookupAt("https://relay.example.com:9000/lookup") }, "callback.lookup_url"},
		{"lookup http refused", func(c *relay.Config) { c.Callback = lookupAt("http://relay.example.com/lookup") }, "callback.lookup_url"},
		{"lookup userinfo", func(c *relay.Config) { c.Callback = lookupAt("https://u:p@relay.example.com/lookup") }, "callback.lookup_url"}, // #nosec G101 -- refused-userinfo fixture
		{"lookup query", func(c *relay.Config) { c.Callback = lookupAt("https://relay.example.com/lookup?key=1") }, "callback.lookup_url"},
		{"lookup empty query", func(c *relay.Config) { c.Callback = lookupAt("https://relay.example.com/lookup?") }, "callback.lookup_url"},
		{"lookup fragment", func(c *relay.Config) { c.Callback = lookupAt("https://relay.example.com/lookup#x") }, "callback.lookup_url"},
		{"lookup relative", func(c *relay.Config) { c.Callback = lookupAt("/lookup") }, "callback.lookup_url"},
		{"lookup opaque", func(c *relay.Config) { c.Callback = lookupAt("https:relay.example.com") }, "callback.lookup_url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := baseConfig()
			tt.mutate(&c)
			issues := f.Validate(ext(c))
			if tt.field == "" {
				if len(issues) != 0 {
					t.Fatalf("issues: %v", issues)
				}
				return
			}
			if len(issues) == 0 || issues[0].Field != tt.field {
				t.Fatalf("fields %v, want first %q", fields(issues), tt.field)
			}
			for _, i := range issues {
				if i.Code != string(model.Validation) {
					t.Errorf("issue %v: code %q", i, i.Code)
				}
			}
		})
	}
}

// lookupAt is a valid callback whose lookup is raw.
func lookupAt(raw string) *relay.Callback {
	return &relay.Callback{URL: "https://relay.example.com/out", LookupURL: raw}
}

func TestValidateAllowsHTTPLookupOnlyWhenTheHostDoes(t *testing.T) {
	c := baseConfig()
	c.Callback = &relay.Callback{URL: "http://127.0.0.1:9/out", LookupURL: "http://127.0.0.1:9/lookup"}
	f := relay.NewFactory(relay.Options{CallbackHosts: []string{"127.0.0.1:9"}, AllowHTTP: true})
	if issues := f.Validate(ext(c)); len(issues) != 0 {
		t.Fatalf("issues: %v", issues)
	}
	f = relay.NewFactory(relay.Options{CallbackHosts: []string{"127.0.0.1:9"}})
	if issues := f.Validate(ext(c)); len(issues) != 2 || issues[1].Field != "callback.lookup_url" {
		t.Fatalf("fields %v, want callback.url and callback.lookup_url", fields(issues))
	}
}

// A factory with no allowed callback host refuses every callback: a channel
// never picks where the host's credential is sent.
func TestValidateRefusesCallbackWithoutAllowlist(t *testing.T) {
	c := baseConfig()
	c.Callback = &relay.Callback{URL: "https://relay.example.com/out"}
	issues := relay.NewFactory(relay.Options{}).Validate(ext(c))
	if len(issues) == 0 || issues[0].Field != "callback.url" {
		t.Fatalf("fields %v", fields(issues))
	}
}

func TestValidateAllowsHTTPOnlyWhenTheHostDoes(t *testing.T) {
	c := baseConfig()
	c.Callback = &relay.Callback{URL: "http://127.0.0.1:9/out"}
	f := relay.NewFactory(relay.Options{CallbackHosts: []string{"127.0.0.1:9"}, AllowHTTP: true})
	if issues := f.Validate(ext(c)); len(issues) != 0 {
		t.Fatalf("issues: %v", issues)
	}
}

func TestIssuesNameNoValue(t *testing.T) {
	c := baseConfig()
	c.Callback = &relay.Callback{
		URL:       "https://user:hunter2@relay.example.com/out?token=hunter2",    // #nosec G101 -- leak-check fixture
		LookupURL: "https://user:hunter2@relay.example.com/lookup?token=hunter2", // #nosec G101 -- leak-check fixture
	}
	issues := relay.NewFactory(relay.Options{}).Validate(ext(c))
	if len(issues) < 2 {
		t.Fatalf("issues %v: both URLs are refused", issues)
	}
	for _, i := range issues {
		if strings.Contains(i.Message, "hunter2") {
			t.Errorf("issue echoes a configured value: %v", i)
		}
	}
}

func TestValidateStatusConfigurationIsJSON(t *testing.T) {
	f := relay.NewFactory(relay.Options{})
	e := ext(baseConfig())
	e.Data = json.RawMessage(`{"medium":"sms","auth":{"scheme":"bearer"},"inbound":{"status":{"enabled":true,"fields":{"status":["state"],"from":["line"]}}}}`)
	if issues := f.Validate(e); len(issues) != 0 {
		t.Fatalf("issues: %v", issues)
	}
	e.Data = json.RawMessage(`{"medium":"sms","auth":{"scheme":"bearer"},"inbound":{"status":{"enabled":true,"secret":"x"}}}`)
	if issues := f.Validate(e); len(issues) == 0 {
		t.Fatal("unknown status field accepted")
	}
	// Off by default: a configuration without the block opens a channel that
	// reads every request as a message.
	if c := baseConfig(); c.Inbound.Status.Enabled {
		t.Fatal("status route on by default")
	}
}
