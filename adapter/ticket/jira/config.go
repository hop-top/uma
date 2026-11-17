package jira

import (
	"bytes"
	"encoding/json"
	"net/url"
	"slices"
	"strings"

	"hop.top/uma/model"
)

// ConfigSchema and ConfigVersion name the channel configuration.
const (
	ConfigSchema         = string(model.TypeJiraIssues) + ".config"
	ConfigVersion uint32 = 1
)

// GatewayURL is the root of the Atlassian API gateway, through which a
// request authenticated with an OAuth 2.0 (3LO) access token reaches a
// Jira Cloud site: https://api.atlassian.com/ex/jira/{cloudid}.
const GatewayURL = "https://api.atlassian.com/ex/jira/"

// Config is the data of a channel configuration.
type Config struct {
	// SiteURL is the Jira Cloud site, e.g. "https://example.atlassian.net".
	// Required: API requests made with an API token go to it, and issue
	// links ("{SiteURL}/browse/{key}") are built from it. It must be https
	// without user info, query, fragment or path.
	SiteURL string `json:"site_url"`
	// CloudID is the site's cloud ID. Required for an OAuth 2.0 (3LO)
	// credential, whose requests go through the API gateway
	// (GatewayURL + CloudID); unused with an API token.
	CloudID string `json:"cloud_id,omitempty"`
	// Email is the Atlassian account email an API token (a ports.Secret)
	// belongs to, for basic authentication. Unused with a ports.Password
	// credential, which carries its own user, and with OAuth 2.0.
	Email string `json:"email,omitempty"`
	// Replies enables send, edit, delete and reconcile of messages:
	// replies become issue comments.
	Replies bool `json:"replies,omitempty"`
	// AccountID is the Atlassian account ID (accountId) the credential
	// acts as. Required with Replies: only its comments are matched as
	// uma's own.
	AccountID string `json:"account_id,omitempty"`
	// LocalFields lists the work item fields uma owns for issues of this
	// channel, from model.WorkItemLocalFields. Empty keeps every field
	// ingest-only.
	LocalFields []string `json:"local_fields,omitempty"`
}

// Extension returns c as a channel configuration.
func (c Config) Extension() model.Extension {
	data, _ := json.Marshal(c)
	return model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data}
}

// site returns SiteURL without a trailing slash.
func (c Config) site() string { return strings.TrimRight(c.SiteURL, "/") }

// parseConfig decodes and validates a channel configuration.
func parseConfig(x model.Extension) (Config, []model.ValidationIssue) {
	var c Config
	if x.Name != ConfigSchema || x.Version != ConfigVersion {
		return c, []model.ValidationIssue{{
			Field: "configuration", Code: string(model.SchemaMismatch),
			Message: "want " + ConfigSchema + " version 1",
		}}
	}
	if len(x.Data) > 0 {
		dec := json.NewDecoder(bytes.NewReader(x.Data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil || dec.More() {
			return Config{}, []model.ValidationIssue{invalid("configuration.data", "must be a jira.issues configuration")}
		}
	}
	var issues []model.ValidationIssue
	u, err := url.Parse(c.SiteURL)
	if c.SiteURL == "" || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		issues = append(issues, invalid("configuration.data.site_url", "required: the https URL of the Jira Cloud site, without path, user info, query or fragment"))
	}
	if c.CloudID != "" && !validCloudID(c.CloudID) {
		issues = append(issues, invalid("configuration.data.cloud_id", "must be the site's cloud ID (letters, digits and '-')"))
	}
	if strings.ContainsAny(c.Email, " \t\r\n:") {
		issues = append(issues, invalid("configuration.data.email", "must be an email address"))
	}
	if c.Replies && c.AccountID == "" {
		issues = append(issues, invalid("configuration.data.account_id", "required with replies: the accountId the credential acts as"))
	}
	if strings.ContainsAny(c.AccountID, " \t\r\n/") {
		issues = append(issues, invalid("configuration.data.account_id", "must be an Atlassian accountId"))
	}
	allowed := model.WorkItemLocalFields()
	for i, f := range c.LocalFields {
		if !slices.Contains(allowed, f) || slices.Contains(c.LocalFields[:i], f) {
			issues = append(issues, invalid("configuration.data.local_fields", "must list distinct fields uma may own"))
			break
		}
	}
	return c, issues
}

func validCloudID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && r != '-' {
			return false
		}
	}
	return true
}

func invalid(field, msg string) model.ValidationIssue {
	return model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
}
