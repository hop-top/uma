package discord

import (
	"slices"
	"testing"

	"hop.top/uma/model"
)

func TestConfigValidation(t *testing.T) {
	f := NewFactory()
	if issues := f.Validate(testConfig().Extension()); len(issues) != 0 {
		t.Fatalf("valid configuration: %+v", issues)
	}
	no := false
	cases := map[string]func(*Config){
		"application_id":            func(c *Config) { c.ApplicationID = "app" },
		"bot_user_id":               func(c *Config) { c.BotUserID = "" },
		"api_url":                   func(c *Config) { c.APIURL = "http://discord.com/api/v10" },
		"gateway_url":               func(c *Config) { c.GatewayURL = "https://gateway.discord.gg" },
		"history_channels":          func(c *Config) { c.HistoryChannels = []string{testRoom, testRoom} },
		"webhook_tolerance_seconds": func(c *Config) { c.WebhookToleranceSeconds = 301 },
		"receive":                   func(c *Config) { c.Receive = "socket" },
	}
	for field, mod := range cases {
		c := testConfig()
		mod(&c)
		issues := f.Validate(c.Extension())
		if len(issues) != 1 || issues[0].Field != "configuration.data."+field {
			t.Errorf("%s: issues %+v", field, issues)
		}
	}
	c := testConfig()
	c.MentionsOnly = &no
	if issues := f.Validate(c.Extension()); len(issues) != 0 {
		t.Fatalf("mentions_only false: %+v", issues)
	}
	bad := testConfig().Extension()
	bad.Data = []byte(`{"application_id":"1","bot_user_id":"2","token":"x"}`)
	if issues := f.Validate(bad); len(issues) != 1 || issues[0].Field != "configuration.data" {
		t.Fatalf("unknown field: %+v", issues)
	}
	if issues := f.Validate(model.Extension{Name: "other", Version: 1}); len(issues) != 1 || issues[0].Code != string(model.SchemaMismatch) {
		t.Fatalf("schema: %+v", issues)
	}
}

// The configuration's receive mode decides which way of receiving the
// instance reports available; the descriptor declares both.
func TestCapabilitiesFollowReceiveMode(t *testing.T) {
	for mode, off := range map[ReceiveMode]model.Capability{ReceiveGateway: model.CapabilityReceiveHTTP, ReceiveInteractions: model.CapabilityReceive, "": ""} {
		r := newRig(t, func(c *Config) { c.Receive = mode })
		st, _ := r.x.Capabilities(t.Context())
		if len(st) != len(Capabilities()) {
			t.Fatalf("%q: statuses %+v", mode, st)
		}
		for _, s := range st {
			if want := s.Name != off; s.Available != want || (!want && s.Reason != model.Disabled) {
				t.Errorf("%q: %+v", mode, s)
			}
		}
	}
	if err := newRig(t, func(c *Config) { c.Receive = ReceiveInteractions }).x.Run(t.Context(), &sinkRecorder{}); !model.IsCode(err, model.Disabled) {
		t.Fatalf("Run under interactions = %v, want disabled", err)
	}
}

func TestIntentsFollowMentionsOnly(t *testing.T) {
	c := testConfig()
	if c.intents()&intentMessageContent != 0 {
		t.Fatal("mentions only (the default) asks for MESSAGE_CONTENT")
	}
	no := false
	c.MentionsOnly = &no
	if c.intents()&intentMessageContent == 0 {
		t.Fatal("all messages without MESSAGE_CONTENT")
	}
	want := intentGuildMessages | intentDirectMessages | intentGuildMessageReactions | intentDirectMessageReactions
	if c.intents()&want != want {
		t.Fatalf("intents %b lack %b", c.intents(), want)
	}
	if !slices.Contains(Capabilities(), model.CapabilityReceive) {
		t.Fatal("receive not declared")
	}
}
