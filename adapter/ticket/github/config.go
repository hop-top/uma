package github

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
	ConfigSchema         = string(model.TypeGithubIssues) + ".config"
	ConfigVersion uint32 = 1
)

// DefaultAPIURL is the REST API root of github.com.
const DefaultAPIURL = "https://api.github.com/"

// Config is the data of a channel configuration.
type Config struct {
	// APIURL is the REST API root, e.g. "https://ghe.example.com/api/v3/"
	// for GitHub Enterprise Server. Empty is DefaultAPIURL. It must be
	// https without user info, query or fragment.
	APIURL string `json:"api_url,omitempty"`
	// Replies enables send and reconcile: replies become issue comments.
	Replies bool `json:"replies,omitempty"`
	// AccountID is the numeric GitHub user ID the credential acts as: the
	// token owner, or the GitHub App's bot user. Required with Replies:
	// only its comments are matched as uma's own.
	AccountID int64 `json:"account_id,omitempty"`
	// LocalFields lists the work item fields uma owns for issues of this
	// channel, from model.WorkItemLocalFields. Empty keeps every field
	// ingest-only.
	LocalFields []string `json:"local_fields,omitempty"`
	// HistoryRepositories lists the repositories ("owner/repo", at most
	// MaxHistoryRepositories) a channel-wide history read covers: a
	// channel is an account, which GitHub does not scope to repositories.
	// Empty refuses channel-wide reads; a read of one issue needs none.
	HistoryRepositories []string `json:"history_repositories,omitempty"`
}

// MaxHistoryRepositories bounds Config.HistoryRepositories: a channel-wide
// history page reads every listed repository.
const MaxHistoryRepositories = 10

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
			return Config{}, []model.ValidationIssue{issue("configuration.data", "must be a github.issues configuration")}
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
		issues = append(issues, issue("configuration.data.account_id", "required with replies: the GitHub user ID the credential acts as"))
	}
	allowed := model.WorkItemLocalFields()
	for i, f := range c.LocalFields {
		if !slices.Contains(allowed, f) || slices.Contains(c.LocalFields[:i], f) {
			issues = append(issues, issue("configuration.data.local_fields", "must list distinct fields uma may own"))
			break
		}
	}
	if !validHistoryRepositories(c.HistoryRepositories) {
		issues = append(issues, issue("configuration.data.history_repositories", "must list at most 10 distinct owner/repo names"))
	}
	return c, issues
}

func validHistoryRepositories(repos []string) bool {
	if len(repos) > MaxHistoryRepositories {
		return false
	}
	for i, r := range repos {
		if !validFullName(r) || slices.ContainsFunc(repos[:i], func(o string) bool { return strings.EqualFold(o, r) }) {
			return false
		}
	}
	return true
}

func issue(field, msg string) model.ValidationIssue {
	return model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
}
