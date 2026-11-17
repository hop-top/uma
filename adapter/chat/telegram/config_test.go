package telegram

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func TestConfigValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		mod   func(*Config)
		field string // "" is valid
	}{
		"minimal": {func(*Config) {}, ""},
		"full": {func(c *Config) {
			c.MentionsOnly, c.PollTimeoutSeconds = true, 50
			c.APIURL = "https://bot-api.example.com/telegram"
		}, ""},
		"shortest username":              {func(c *Config) { c.BotUsername = "a_bot" }, ""},
		"longest username":               {func(c *Config) { c.BotUsername = "a" + strings.Repeat("b", 28) + "bot" }, ""},
		"username in capitals":           {func(c *Config) { c.BotUsername = "UMA_TEST_BOT" }, ""},
		"no bot id":                      {func(c *Config) { c.BotID = 0 }, "bot_id"},
		"negative bot id":                {func(c *Config) { c.BotID = -5 }, "bot_id"},
		"no username":                    {func(c *Config) { c.BotUsername = "" }, "bot_username"},
		"username with at sign":          {func(c *Config) { c.BotUsername = "@uma_test_bot" }, "bot_username"},
		"username not ending in bot":     {func(c *Config) { c.BotUsername = "uma_test_helper" }, "bot_username"},
		"username too short":             {func(c *Config) { c.BotUsername = "_bot" }, "bot_username"},
		"username too long":              {func(c *Config) { c.BotUsername = "a" + strings.Repeat("b", 29) + "bot" }, "bot_username"},
		"username with a dash":           {func(c *Config) { c.BotUsername = "uma-test-bot" }, "bot_username"},
		"username starting with a digit": {func(c *Config) { c.BotUsername = "1uma_bot" }, "bot_username"},
		"api url over http":              {func(c *Config) { c.APIURL = "http://api.telegram.org" }, "api_url"},
		"api url with user info":         {func(c *Config) { c.APIURL = "https://u:p@api.telegram.org" }, "api_url"},
		"api url with query":             {func(c *Config) { c.APIURL = "https://api.telegram.org?x=1" }, "api_url"},
		"negative poll timeout":          {func(c *Config) { c.PollTimeoutSeconds = -1 }, "poll_timeout_seconds"},
		"poll timeout past the cap":      {func(c *Config) { c.PollTimeoutSeconds = MaxPollTimeoutSeconds + 1 }, "poll_timeout_seconds"},
		"receive by polling":             {func(c *Config) { c.Receive = ReceivePoll }, ""},
		"receive by webhook":             {func(c *Config) { c.Receive = ReceiveWebhook }, ""},
		"receive both":                   {func(c *Config) { c.Receive = "both" }, "receive"},
		"receive in capitals":            {func(c *Config) { c.Receive = "Poll" }, "receive"},
		"receive named as a capability":  {func(c *Config) { c.Receive = "receive_http" }, "receive"},
	} {
		t.Run(name, func(t *testing.T) {
			c := testConfig()
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
	if issues := f.Validate(model.Extension{Name: "telegram.messages.other", Version: 1}); len(issues) != 1 || issues[0].Code != string(model.SchemaMismatch) {
		t.Errorf("wrong schema issues = %+v, want one schema_mismatch", issues)
	}
	ext := testConfig().Extension()
	var m map[string]any
	_ = json.Unmarshal(ext.Data, &m)
	m["bot_token"] = "7000001:AAnotreal"
	ext.Data, _ = json.Marshal(m)
	if issues := f.Validate(ext); len(issues) == 0 {
		t.Error("a configuration carrying an unknown field (a token by mistake) was accepted")
	}
	// The number a JSON reader gives an ID must not lose digits: IDs have
	// up to 52 significant bits.
	ext = model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: []byte(`{"bot_id":4503599627370495,"bot_username":"uma_bot"}`)}
	if c, issues := parseConfig(ext); len(issues) > 0 || c.BotID != 4503599627370495 {
		t.Errorf("a 52-bit bot ID: %+v %+v", c, issues)
	}
}

func TestOptionsOfAnInstance(t *testing.T) {
	c := testConfig()
	if got := c.apiURL(); got != DefaultAPIURL {
		t.Errorf("default api url %q", got)
	}
	c.APIURL = "https://bot-api.example.com/telegram"
	if got := c.apiURL(); got != "https://bot-api.example.com/telegram/" {
		t.Errorf("api url %q, want a trailing slash", got)
	}
	if c.pollTimeout() != 30*time.Second {
		t.Errorf("default poll timeout %v", c.pollTimeout())
	}
	c.PollTimeoutSeconds = 7
	if c.pollTimeout() != 7*time.Second {
		t.Errorf("poll timeout %v", c.pollTimeout())
	}
}

func TestOpen(t *testing.T) {
	f := NewFactory(Options{})
	good := model.Channel{Ref: testChannel, Type: Type, Configuration: testConfig().Extension(), CredentialRef: "telegram"}
	if _, err := f.Open(t.Context(), good, adapter.Dependencies{}); err != nil {
		t.Fatalf("Open without ports: %v", err)
	}
	wrongType := good
	wrongType.Type = model.TypeSlackChat
	if _, err := f.Open(t.Context(), wrongType, adapter.Dependencies{}); !model.IsCode(err, model.Validation) {
		t.Errorf("wrong type: %v", err)
	}
	bad := good
	bad.Configuration = Config{}.Extension()
	if _, err := f.Open(t.Context(), bad, adapter.Dependencies{}); !model.IsCode(err, model.Validation) {
		t.Errorf("bad configuration: %v", err)
	}
	ctx, cancel := cancelled(t)
	defer cancel()
	if _, err := f.Open(ctx, good, adapter.Dependencies{}); err == nil {
		t.Error("Open with a cancelled context succeeded")
	}
}

func TestDescriptor(t *testing.T) {
	d := NewFactory(Options{}).Describe()
	if d.Type != model.TypeTelegramMessages || d.ContractMajor != adapter.ContractMajor || d.Maturity != "experimental" {
		t.Errorf("descriptor %+v", d)
	}
	if d.Exchange.MessageIDs != adapter.MessageIDsProvider {
		t.Errorf("message IDs %q: Telegram assigns every message_id", d.Exchange.MessageIDs)
	}
	if len(d.IdentityNamespaces) != 1 || d.IdentityNamespaces[0] != UserNamespace {
		t.Errorf("identity namespaces %v", d.IdentityNamespaces)
	}
}

func TestCapabilitiesReportEveryDeclaredCapabilityAvailable(t *testing.T) {
	st, err := open(t, testConfig()).Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[model.Capability]bool{}
	for _, s := range st {
		if !s.Available || s.Reason != "" || got[s.Name] {
			t.Errorf("status %+v", s)
		}
		got[s.Name] = true
	}
	for _, c := range NewFactory(Options{}).Describe().Capabilities {
		if !got[c] {
			t.Errorf("declared capability %s not reported", c)
		}
	}
}
