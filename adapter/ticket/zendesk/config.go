package zendesk

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"time"

	"hop.top/uma/model"
)

// ConfigSchema names the channel configuration schema (version
// ConfigVersion): a JSON object holding Config.
const (
	ConfigSchema  = string(model.TypeZendeskTickets) + ".config"
	ConfigVersion = 1
)

// Authentication modes (Config.Auth).
const (
	// AuthAPIToken authenticates as an agent with an API token: HTTP Basic
	// "{email}/token:{api_token}". The credential is a ports.Secret holding
	// the token, with the agent's address in Config.Email, or a
	// ports.Password whose User is the address and Secret the token.
	AuthAPIToken = "api_token"
	// AuthOAuth authenticates with an OAuth access token: "Authorization:
	// Bearer". The credential is a ports.OAuth2, or a ports.Secret holding
	// the access token. The host owns consent and refresh.
	AuthOAuth = "oauth"
)

// Polling and webhook bounds.
const (
	defaultPollInterval = 30 * time.Second
	// minPollInterval keeps a receiver within the incremental export limit
	// of 10 requests per minute.
	minPollInterval     = 6 * time.Second
	defaultPollLookback = time.Hour
	minPollLookback     = time.Minute
	maxPollLookback     = 30 * 24 * time.Hour
	maxWebhookTolerance = time.Hour
)

// Config is the channel configuration.
type Config struct {
	// Subdomain is the account subdomain: https://{subdomain}.zendesk.com.
	// Required.
	Subdomain string `json:"subdomain"`
	// Auth is AuthAPIToken or AuthOAuth. Required.
	Auth string `json:"auth"`
	// Email is the agent address an API token belongs to, used when the
	// credential is a bare secret.
	Email string `json:"email,omitempty"`
	// WebhookToleranceSeconds is the replay window of signed webhooks: a
	// request whose signature timestamp differs from now by more is
	// rejected. Required, 1 to 3600.
	WebhookToleranceSeconds int `json:"webhook_tolerance_seconds"`
	// PollIntervalSeconds is the receiver's wait between polls that found
	// nothing; at least 6, default 30.
	PollIntervalSeconds int `json:"poll_interval_seconds,omitempty"`
	// PollLookbackSeconds is how far back a receiver starts reading ticket
	// events when it connects; 60 to 30 days, default 3600. Ingress
	// deduplicates what an earlier connection already read.
	PollLookbackSeconds int `json:"poll_lookback_seconds,omitempty"`
	// LocalFields are the work item fields uma owns for tickets of this
	// channel (see model.WorkItemState.LocalFields).
	LocalFields []string `json:"local_fields,omitempty"`
}

var subdomainPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Extension returns c as a channel configuration.
func (c Config) Extension() model.Extension {
	data, _ := json.Marshal(c) // a Config always encodes
	return model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data}
}

// decodeConfig reads and checks a channel configuration.
func decodeConfig(x model.Extension) (Config, []model.ValidationIssue) {
	var c Config
	if x.Name != ConfigSchema || x.Version != ConfigVersion {
		return c, []model.ValidationIssue{issue("configuration", string(model.SchemaMismatch),
			"want "+ConfigSchema+" version "+strconv.Itoa(ConfigVersion))}
	}
	dec := json.NewDecoder(bytes.NewReader(x.Data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || dec.More() {
		return Config{}, []model.ValidationIssue{issue("configuration.data", string(model.Validation), "must be a Zendesk channel configuration object")}
	}
	var out []model.ValidationIssue
	if !subdomainPattern.MatchString(c.Subdomain) {
		out = append(out, issue("configuration.data.subdomain", string(model.Validation), "must be a Zendesk subdomain"))
	}
	if c.Auth != AuthAPIToken && c.Auth != AuthOAuth {
		out = append(out, issue("configuration.data.auth", string(model.Validation), "must be api_token or oauth"))
	}
	if c.Email != "" && c.Auth != AuthAPIToken {
		out = append(out, issue("configuration.data.email", string(model.Validation), "only api_token authentication uses an email"))
	}
	if c.WebhookToleranceSeconds < 1 || time.Duration(c.WebhookToleranceSeconds)*time.Second > maxWebhookTolerance {
		out = append(out, issue("configuration.data.webhook_tolerance_seconds", string(model.Validation), "required, 1 to 3600"))
	}
	if c.PollIntervalSeconds != 0 && time.Duration(c.PollIntervalSeconds)*time.Second < minPollInterval {
		out = append(out, issue("configuration.data.poll_interval_seconds", string(model.Validation), "at least 6"))
	}
	if l := time.Duration(c.PollLookbackSeconds) * time.Second; c.PollLookbackSeconds != 0 && (l < minPollLookback || l > maxPollLookback) {
		out = append(out, issue("configuration.data.poll_lookback_seconds", string(model.Validation), "60 to 2592000"))
	}
	for i, f := range c.LocalFields {
		if !slices.Contains(model.WorkItemLocalFields(), f) || slices.Contains(c.LocalFields[:i], f) {
			out = append(out, issue("configuration.data.local_fields", string(model.Validation), "must list distinct fields uma may own"))
			break
		}
	}
	if len(out) > 0 {
		return Config{}, out
	}
	return c, nil
}

func (c Config) tolerance() time.Duration {
	return time.Duration(c.WebhookToleranceSeconds) * time.Second
}

func (c Config) pollInterval() time.Duration {
	if c.PollIntervalSeconds == 0 {
		return defaultPollInterval
	}
	return time.Duration(c.PollIntervalSeconds) * time.Second
}

func (c Config) pollLookback() time.Duration {
	if c.PollLookbackSeconds == 0 {
		return defaultPollLookback
	}
	return time.Duration(c.PollLookbackSeconds) * time.Second
}

func issue(field, code, msg string) model.ValidationIssue {
	return model.ValidationIssue{Field: field, Code: code, Message: msg}
}
