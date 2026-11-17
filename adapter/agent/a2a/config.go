package a2a

import (
	"bytes"
	"encoding/json"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"hop.top/uma/model"
)

// Config is the data of a channel's configuration extension
// (ConfigSchema version 1). Destination, PublicURL, Name, Description,
// Version and Skills are required; the rest is optional. Everything but
// the two durations is the Agent Card's content, which is declarative: the
// adapter serves it and enforces none of it.
type Config struct {
	// Destination is the inbox destination every request of the channel
	// is delivered to; the channel's inbox must be per-agent-context.
	Destination string `json:"destination"`
	// PublicURL is the absolute http(s) URL the host mounts the endpoint
	// at: the card's only supported interface.
	PublicURL   string   `json:"public_url"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Version     string   `json:"version"`
	Provider    Provider `json:"provider,omitzero"`
	DocumentURL string   `json:"documentation_url,omitempty"`
	IconURL     string   `json:"icon_url,omitempty"`
	Skills      []Skill  `json:"skills"`
	InputModes  []string `json:"input_modes,omitempty"`
	OutputModes []string `json:"output_modes,omitempty"`
	// SecuritySchemes declares how clients authenticate, by name, each an
	// A2A SecurityScheme object (exactly one of apiKeySecurityScheme,
	// httpAuthSecurityScheme, oauth2SecurityScheme,
	// openIdConnectSecurityScheme, mtlsSecurityScheme). The host's
	// Authenticator is what enforces them.
	SecuritySchemes map[string]json.RawMessage `json:"security_schemes,omitempty"`
	// SecurityRequirements lists alternatives, each mapping declared
	// scheme names to the scopes required.
	SecurityRequirements []map[string][]string `json:"security_requirements,omitempty"`
	// BlockingWait bounds a blocking SendMessage (a Go duration string,
	// default 30s, positive). The caller's own deadline bounds it further.
	BlockingWait string `json:"blocking_wait,omitempty"`
	// PollInterval is how often a blocking SendMessage reads what answers
	// it (default 250ms, from 50ms to BlockingWait).
	PollInterval string `json:"poll_interval,omitempty"`
}

// Provider is the card's provider; both fields are required when set.
type Provider struct {
	Organization string `json:"organization"`
	URL          string `json:"url"`
}

// Skill is one skill of the card. ID, Name, Description and at least one
// tag are required.
type Skill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Examples    []string `json:"examples,omitempty"`
	InputModes  []string `json:"input_modes,omitempty"`
	OutputModes []string `json:"output_modes,omitempty"`
}

// Configuration returns a channel configuration holding c.
func Configuration(c Config) model.Extension {
	data, _ := json.Marshal(c) // #nosec G104 -- the configuration is plain data
	return model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data}
}

// config is a validated Config.
type config struct {
	Config
	wait time.Duration
	poll time.Duration
}

// Limits of the card content.
const (
	maxTextBytes = 4 << 10
	maxListItems = 64
)

var securityKinds = []string{
	"apiKeySecurityScheme", "httpAuthSecurityScheme", "oauth2SecurityScheme",
	"openIdConnectSecurityScheme", "mtlsSecurityScheme",
}

func parseConfig(ext model.Extension) (config, []model.ValidationIssue) {
	if ext.Name != ConfigSchema || ext.Version != ConfigVersion {
		return config{}, []model.ValidationIssue{{
			Field: "configuration", Code: string(model.SchemaMismatch), Message: "want " + ConfigSchema + " version 1",
		}}
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(ext.Data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || dec.More() {
		return config{}, []model.ValidationIssue{{
			Field: "configuration.data", Code: string(model.Validation), Message: "must be an object of the " + ConfigSchema + " fields",
		}}
	}
	v := &validator{}
	out := config{Config: c, wait: DefaultBlockingWait, poll: DefaultPollInterval}
	v.text("destination", c.Destination, 253, true)
	v.absURL("public_url", c.PublicURL, true)
	v.text("name", c.Name, maxTextBytes, true)
	v.text("description", c.Description, maxTextBytes, true)
	v.text("version", c.Version, 128, true)
	v.absURL("documentation_url", c.DocumentURL, false)
	v.absURL("icon_url", c.IconURL, false)
	if c.Provider != (Provider{}) {
		v.text("provider.organization", c.Provider.Organization, maxTextBytes, true)
		v.absURL("provider.url", c.Provider.URL, true)
	}
	v.modes("input_modes", c.InputModes)
	v.modes("output_modes", c.OutputModes)
	v.skills(c.Skills)
	v.security(c)
	if c.BlockingWait != "" {
		if d, ok := v.duration("blocking_wait", c.BlockingWait); ok {
			out.wait = d
		}
	}
	if c.PollInterval != "" {
		if d, ok := v.duration("poll_interval", c.PollInterval); ok {
			out.poll = d
		}
	}
	if len(v.issues) == 0 && (out.poll < minPollInterval || out.poll > out.wait) {
		v.add("poll_interval", "must be from "+minPollInterval.String()+" to blocking_wait")
	}
	if len(v.issues) > 0 {
		return config{}, v.issues
	}
	return out, nil
}

type validator struct{ issues []model.ValidationIssue }

func (v *validator) add(field, msg string) {
	v.issues = append(v.issues, model.ValidationIssue{
		Field: "configuration.data." + field, Code: string(model.Validation), Message: msg,
	})
}

func (v *validator) text(field, s string, max int, required bool) {
	switch {
	case s == "" && required:
		v.add(field, "required")
	case len(s) > max || !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool { return r == 0 || r == 0x7f }):
		v.add(field, "must be valid text of at most "+strconv.Itoa(max)+" bytes")
	}
}

func (v *validator) absURL(field, s string, required bool) {
	if s == "" {
		if required {
			v.add(field, "required")
		}
		return
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(s) > 2048 {
		v.add(field, "must be an absolute http(s) URL")
	}
}

func (v *validator) modes(field string, modes []string) {
	if len(modes) > maxListItems {
		v.add(field, "too many entries")
		return
	}
	for _, m := range modes {
		v.text(field, m, 255, true)
	}
}

func (v *validator) skills(skills []Skill) {
	if len(skills) == 0 {
		v.add("skills", "at least one skill is required")
		return
	}
	if len(skills) > maxListItems {
		v.add("skills", "too many skills")
		return
	}
	seen := map[string]bool{}
	for i, s := range skills {
		at := "skills[" + strconv.Itoa(i) + "]."
		v.text(at+"id", s.ID, 255, true)
		v.text(at+"name", s.Name, maxTextBytes, true)
		v.text(at+"description", s.Description, maxTextBytes, true)
		if seen[s.ID] {
			v.add(at+"id", "duplicate skill")
		}
		seen[s.ID] = true
		if len(s.Tags) == 0 {
			v.add(at+"tags", "at least one tag is required")
		}
		for _, list := range [][]string{s.Tags, s.Examples} {
			if len(list) > maxListItems {
				v.add(at+"tags", "too many entries")
			}
			for _, t := range list {
				v.text(at+"tags", t, maxTextBytes, true)
			}
		}
		v.modes(at+"input_modes", s.InputModes)
		v.modes(at+"output_modes", s.OutputModes)
	}
}

func (v *validator) security(c Config) {
	if len(c.SecuritySchemes) > maxListItems {
		v.add("security_schemes", "too many schemes")
		return
	}
	for _, name := range sortedKeys(c.SecuritySchemes) {
		if name == "" || len(name) > 255 {
			v.add("security_schemes", "scheme names are 1 to 255 bytes")
			continue
		}
		if _, ok := schemeKind(c.SecuritySchemes[name]); !ok {
			v.add("security_schemes."+name, "must be an object with exactly one of "+strings.Join(securityKinds, ", "))
		}
	}
	for i, req := range c.SecurityRequirements {
		for name := range req {
			if _, ok := c.SecuritySchemes[name]; !ok {
				v.add("security_requirements["+strconv.Itoa(i)+"]", "names undeclared scheme "+name)
			}
		}
	}
}

func (v *validator) duration(field, s string) (time.Duration, bool) {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		v.add(field, "must be a positive duration such as 30s")
		return 0, false
	}
	return d, true
}

// schemeKind returns which security scheme kind raw holds: a JSON object
// with exactly one key, one of securityKinds, holding an object.
func schemeKind(raw json.RawMessage) (string, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || len(m) != 1 {
		return "", false
	}
	for k, body := range m {
		var obj map[string]json.RawMessage
		if !slices.Contains(securityKinds, k) || json.Unmarshal(body, &obj) != nil || obj == nil {
			return "", false
		}
		return k, true
	}
	return "", false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
