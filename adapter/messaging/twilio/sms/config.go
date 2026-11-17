package sms

import (
	"bytes"
	"encoding/json"
	"net/url"

	"hop.top/uma/adapter/messaging/twilio"
	"hop.top/uma/model"
)

// ConfigSchema and ConfigVersion name the channel configuration schema.
const (
	ConfigSchema  = "twilio.sms.config"
	ConfigVersion = 1
)

// Config is a twilio.sms channel configuration, carried as the JSON data of
// a ConfigSchema extension.
type Config struct {
	// AccountSID is the Twilio account (AC...). Required.
	AccountSID string `json:"account_sid"`
	// From is the sending number; MessagingServiceSID (MG...) a Messaging
	// Service to send through instead. At least one is required.
	From                string `json:"from,omitempty"`
	MessagingServiceSID string `json:"messaging_service_sid,omitempty"`
	// WebhookURL is the public URL configured on the Twilio number or
	// service for incoming messages, exactly as entered there: the
	// signature check uses it, not the URL the request reached the host
	// with. Outbound messages ask for status callbacks at the same URL.
	// Without it the channel sends only and its webhook answers 503.
	WebhookURL string `json:"webhook_url,omitempty"`
}

// Extension returns c as a channel configuration.
func (c Config) Extension() model.Extension {
	data, _ := json.Marshal(c) // a struct of strings always marshals
	return model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data}
}

func issue(field, msg string) model.ValidationIssue {
	return model.ValidationIssue{Field: field, Code: string(model.Validation), Message: msg}
}

// parseConfig decodes and checks a configuration without I/O.
func parseConfig(e model.Extension) (Config, []model.ValidationIssue) {
	if e.Name != ConfigSchema || e.Version != ConfigVersion {
		return Config{}, []model.ValidationIssue{{
			Field: "configuration", Code: string(model.SchemaMismatch), Message: "want " + ConfigSchema + " version 1",
		}}
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(e.Data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, []model.ValidationIssue{issue("configuration.data", "must be a JSON object of known fields")}
	}
	var out []model.ValidationIssue
	if !twilio.ValidAccountSID(c.AccountSID) {
		out = append(out, issue("account_sid", "must be a Twilio account SID"))
	}
	switch {
	case c.From == "" && c.MessagingServiceSID == "":
		out = append(out, issue("from", "from or messaging_service_sid required"))
	case c.MessagingServiceSID != "" && !twilio.ValidMessagingServiceSID(c.MessagingServiceSID):
		out = append(out, issue("messaging_service_sid", "must be a Messaging Service SID"))
	}
	if c.WebhookURL != "" {
		u, err := url.Parse(c.WebhookURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			out = append(out, issue("webhook_url", "must be an absolute http(s) URL without credentials, query or fragment"))
		}
	}
	return c, out
}
