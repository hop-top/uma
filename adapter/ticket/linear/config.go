package linear

import (
	"bytes"
	"encoding/json"
	"net/url"
	"slices"

	"hop.top/uma/model"
)

// ConfigSchema and ConfigVersion name the channel configuration.
const (
	ConfigSchema         = string(model.TypeLinearIssues) + ".config"
	ConfigVersion uint32 = 1
)

// DefaultAPIURL is Linear's GraphQL endpoint.
const DefaultAPIURL = "https://api.linear.app/graphql"

// Config is the data of a channel configuration.
type Config struct {
	// APIURL is the GraphQL endpoint. Empty is DefaultAPIURL. It must be
	// https without user info, query or fragment.
	APIURL string `json:"api_url,omitempty"`
	// Replies enables send and reconcile: replies become issue comments, and
	// edit and delete change them.
	Replies bool `json:"replies,omitempty"`
	// AccountID is the Linear user ID (UUID) the credential acts as: the
	// token owner, or the application's own user for an actor=app token.
	// Required with Replies: only its comments are matched as uma's own.
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

func (c Config) apiURL() string {
	if c.APIURL == "" {
		return DefaultAPIURL
	}
	return c.APIURL
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
			return Config{}, []model.ValidationIssue{issue("configuration.data", "must be a linear.issues configuration")}
		}
	}
	var issues []model.ValidationIssue
	if c.APIURL != "" {
		u, err := url.Parse(c.APIURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			issues = append(issues, issue("configuration.data.api_url", "must be an https URL without user info, query or fragment"))
		}
	}
	if c.AccountID != "" && !validUUID(c.AccountID) || (c.Replies && c.AccountID == "") {
		issues = append(issues, issue("configuration.data.account_id", "required with replies: the Linear user UUID the credential acts as"))
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
