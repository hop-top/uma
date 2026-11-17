package slack

import (
	"encoding/json"
	"strings"
	"testing"

	"hop.top/uma/model"
)

func validConfig() Config {
	return Config{TeamID: "T0001", BotUserID: "U0BOT"}
}

func TestConfigValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		mod   func(*Config)
		field string // "" is valid
	}{
		"minimal": {func(*Config) {}, ""},
		"full": {func(c *Config) {
			c.BotID, c.AppID, c.MentionsOnly = "B0BOT", "A0APP", true
			c.APIURL = "https://slack-gov.com/api"
			c.HistoryChannels = []string{"C0001", "G0002"}
			c.WebhookToleranceSeconds = 120
		}, ""},
		"no team":                   {func(c *Config) { c.TeamID = "" }, "team_id"},
		"team not an ID":            {func(c *Config) { c.TeamID = "acme" }, "team_id"},
		"no bot user":               {func(c *Config) { c.BotUserID = "" }, "bot_user_id"},
		"bot user not an ID":        {func(c *Config) { c.BotUserID = "u 1" }, "bot_user_id"},
		"bot not an ID":             {func(c *Config) { c.BotID = "bot" }, "bot_id"},
		"app not an ID":             {func(c *Config) { c.AppID = "app" }, "app_id"},
		"api url over http":         {func(c *Config) { c.APIURL = "http://slack.com/api" }, "api_url"},
		"api url with user info":    {func(c *Config) { c.APIURL = "https://u:p@slack.com/api" }, "api_url"},
		"api url with query":        {func(c *Config) { c.APIURL = "https://slack.com/api?x=1" }, "api_url"},
		"history channel not an ID": {func(c *Config) { c.HistoryChannels = []string{"general"} }, "history_channels"},
		"history channel twice":     {func(c *Config) { c.HistoryChannels = []string{"C1", "C1"} }, "history_channels"},
		"too many history channels": {func(c *Config) {
			for i := range MaxHistoryChannels + 1 {
				c.HistoryChannels = append(c.HistoryChannels, "C"+strings.Repeat("A", i+1))
			}
		}, "history_channels"},
		"negative tolerance":     {func(c *Config) { c.WebhookToleranceSeconds = -1 }, "webhook_tolerance_seconds"},
		"tolerance past slack's": {func(c *Config) { c.WebhookToleranceSeconds = 301 }, "webhook_tolerance_seconds"},
	} {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			tc.mod(&c)
			issues := NewFactory(Options{}).Validate(c.Extension())
			switch {
			case tc.field == "" && len(issues) > 0:
				t.Fatalf("valid configuration refused: %+v", issues)
			case tc.field != "" && (len(issues) == 0 || !strings.HasSuffix(issues[0].Field, tc.field)):
				t.Fatalf("issues = %+v, want one on %s", issues, tc.field)
			}
		})
	}
}

func TestConfigSchemaAndUnknownFields(t *testing.T) {
	f := NewFactory(Options{})
	if issues := f.Validate(model.Extension{Name: "slack.chat.other", Version: 1}); len(issues) != 1 || issues[0].Code != string(model.SchemaMismatch) {
		t.Errorf("wrong schema issues = %+v, want one schema_mismatch", issues)
	}
	ext := validConfig().Extension()
	var m map[string]any
	_ = json.Unmarshal(ext.Data, &m)
	m["signing_secret"] = "hunter2"
	ext.Data, _ = json.Marshal(m)
	if issues := f.Validate(ext); len(issues) == 0 {
		t.Error("a configuration carrying an unknown field (a secret by mistake) was accepted")
	}
}
