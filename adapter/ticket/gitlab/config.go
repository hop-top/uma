package gitlab

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
	ConfigSchema         = string(model.TypeGitlabIssues) + ".config"
	ConfigVersion uint32 = 1
)

// DefaultAPIURL is the REST API root of gitlab.com.
const DefaultAPIURL = "https://gitlab.com/api/v4/"

// Config is the data of a channel configuration.
type Config struct {
	// APIURL is the REST API root, e.g. "https://gitlab.example.com/api/v4/"
	// for a self-managed instance. Empty is DefaultAPIURL. It must be https
	// without user info, query or fragment.
	APIURL string `json:"api_url,omitempty"`
	// Replies enables send and the message mutations: replies become issue
	// notes.
	Replies bool `json:"replies,omitempty"`
	// AccountID is the numeric GitLab user ID the credential acts as: the
	// token owner, or the bot user of a project or group access token.
	// Required with Replies: only its notes are matched as uma's own.
	AccountID int64 `json:"account_id,omitempty"`
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

// apiURL returns the normalized API root, ending in "/".
func (c Config) apiURL() string {
	if c.APIURL == "" {
		return DefaultAPIURL
	}
	if strings.HasSuffix(c.APIURL, "/") {
		return c.APIURL
	}
	return c.APIURL + "/"
}

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
			return Config{}, []model.ValidationIssue{issue("configuration.data", "must be a gitlab.issues configuration")}
		}
	}
	var issues []model.ValidationIssue
	if c.APIURL != "" {
		u, err := url.Parse(c.APIURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			issues = append(issues, issue("configuration.data.api_url", "must be an https URL without user info, query or fragment"))
		}
	}
	if c.AccountID < 0 || (c.Replies && c.AccountID == 0) {
		issues = append(issues, issue("configuration.data.account_id", "required with replies: the GitLab user ID the credential acts as"))
	}
	allowed := model.WorkItemLocalFields()
	for i, f := range c.LocalFields {
		if !slices.Contains(allowed, f) || slices.Contains(c.LocalFields[:i], f) {
			issues = append(issues, issue("configuration.data.local_fields", "must list distinct fields uma may own"))
			break
		}
	}
	return c, issues
}

func issue(field, msg string) model.ValidationIssue {
	return model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
}
