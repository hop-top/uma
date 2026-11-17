package teams

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"hop.top/uma/model"
)

// ConfigSchema and ConfigVersion name the channel configuration.
const (
	ConfigSchema         = string(model.TypeMicrosoftTeams) + ".config"
	ConfigVersion uint32 = 1
)

// Config is the data of a channel configuration. One channel is one bot
// registration (one Microsoft App ID) in one Microsoft Entra tenant: the
// Bot Connector token the credential port resolves belongs to that app, and
// activities of any other tenant are not this channel's.
type Config struct {
	// AppID is the bot's Microsoft App ID, a GUID. Required. It is the
	// audience every inbound Bot Connector token must name, and the bot's
	// own account in Teams is "28:" followed by it: activities from that
	// account are never reported back, which would loop.
	AppID string `json:"app_id"`
	// TenantID is the Microsoft Entra tenant the bot is registered and
	// installed in, a GUID. Required: a single-tenant bot belongs to one
	// tenant. Activities naming another tenant are acknowledged and
	// dropped. It is also the destination of every envelope: bind an
	// inbox to it.
	TenantID string `json:"tenant_id"`
	// ServiceURL is the Bot Connector service the adapter sends to, as the
	// serviceUrl of the activities Teams sends the bot, such as
	// "https://smba.trafficmanager.net/teams/". Required. It must be https
	// without user info, query or fragment; the Bot Connector token is only
	// ever sent to it.
	ServiceURL string `json:"service_url"`
}

// Extension returns c as a channel configuration.
func (c Config) Extension() model.Extension {
	data, _ := json.Marshal(c) // a Config always encodes
	return model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data}
}

// serviceBase returns the normalized service URL, without a trailing "/".
func (c Config) serviceBase() string { return strings.TrimRight(c.ServiceURL, "/") }

// botAccount is the bot's channel account ID in Teams.
func (c Config) botAccount() string { return "28:" + c.AppID }

var guid = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

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
			return Config{}, []model.ValidationIssue{issue("configuration.data", "must be a microsoft.teams configuration")}
		}
	}
	var issues []model.ValidationIssue
	bad := func(field, msg string) {
		issues = append(issues, issue("configuration.data."+field, msg))
	}
	if !guid.MatchString(c.AppID) {
		bad("app_id", "required: the bot's Microsoft App ID, a GUID")
	}
	if !guid.MatchString(c.TenantID) {
		bad("tenant_id", "required: the Microsoft Entra tenant ID, a GUID")
	}
	if !validServiceURL(c.ServiceURL) {
		bad("service_url", "required: an https URL without user info, query or fragment")
	}
	return c, issues
}

func validServiceURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery
}

func issue(field, msg string) model.ValidationIssue {
	return model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
}
