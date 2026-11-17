package jira_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// jiraScrub neutralizes what a Jira answer carries about people and the
// site beyond the identifiers the placeholders cover: display names, email
// addresses, avatar and profile URLs, time zones and locales. It rewrites
// JSON answers only; the residue check below fails a recording that still
// holds a real value.
type jiraScrub struct{}

func (jiraScrub) body(contentType string, b []byte) []byte {
	if !strings.Contains(contentType, "json") {
		return b
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return b
	}
	out, err := json.Marshal(scrubValue(v))
	if err != nil {
		return b
	}
	return out
}

func scrubValue(v any) any {
	switch v := v.(type) {
	case []any:
		for i := range v {
			v[i] = scrubValue(v[i])
		}
		return v
	case map[string]any:
		if _, isProject := v["projectTypeKey"]; isProject {
			v["name"] = placeholderProjectName
		}
		for k, x := range v {
			switch k {
			case "displayName":
				v[k] = placeholderUserName
			case "emailAddress":
				v[k] = placeholderEmail
			case "timeZone":
				v[k] = placeholderTimeZone
			case "locale":
				v[k] = "en_US"
			case "avatarUrls":
				v[k] = map[string]any{}
			default:
				v[k] = scrubValue(x)
			}
		}
		return v
	}
	return v
}

// residue lists real values still present in a body about to be recorded:
// the site host, email, account ID, token, issue key and ID, as quoted
// values or URL parts.
func residue(body string, values ...string) []string {
	var found []string
	for _, v := range values {
		if v == "" || v == placeholderHost || v == placeholderEmail || v == placeholderAccount || v == placeholderIssueKey || v == placeholderIssueID || v == "replay-token" {
			continue
		}
		if strings.Contains(body, v) {
			found = append(found, "a value from the sandbox environment")
		}
	}
	return found
}

// stand-in: the shape of a user object in Jira's REST API
// (https://developer.atlassian.com/cloud/jira/platform/rest/v3/intro/),
// with invented values, proves the scrub removes what a real answer holds.
func TestScrubRemovesPeopleDetails(t *testing.T) {
	in := `{"author":{"accountId":"` + placeholderAccount + `","displayName":"Real Person","emailAddress":"real@corp.example","avatarUrls":{"48x48":"https://avatar.example/x"},"timeZone":"Europe/Paris","locale":"fr_FR"},"body":"kept"}`
	out := string(jiraScrub{}.body("application/json;charset=UTF-8", []byte(in)))
	for _, leak := range []string{"Real Person", "real@corp.example", "avatar.example", "Europe/Paris", "fr_FR"} {
		if strings.Contains(out, leak) {
			t.Errorf("scrubbed answer keeps %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, `"body":"kept"`) || !strings.Contains(out, placeholderAccount) {
		t.Fatalf("scrub dropped what it should keep: %s", out)
	}
	if got := residue(`{"x":"acme.atlassian.net"}`, "acme.atlassian.net", "other"); len(got) != 1 {
		t.Fatalf("residue = %v, want one finding", got)
	}
	if got := residue(`{"x":"`+placeholderHost+`"}`, placeholderHost); len(got) != 0 {
		t.Fatalf("residue flags a placeholder: %v", got)
	}
}

// stand-in: the sandbox environment is required to record, and replay needs
// none.
func TestSandboxEnvironment(t *testing.T) {
	for _, name := range []string{envSite, envEmail, envToken, envAccount, envIssueKey, envIssueID} {
		t.Setenv(name, "")
	}
	if s, err := sandboxFromEnv(false); err != nil || s.host != placeholderHost || s.project() != placeholderProject {
		t.Fatalf("replay sandbox = %+v, %v", s, err)
	}
	if _, err := sandboxFromEnv(true); err == nil {
		t.Fatal("recording without the environment succeeded")
	}
	t.Setenv(envSite, "https://acme.atlassian.net")
	t.Setenv(envEmail, "me@acme.example")
	t.Setenv(envToken, "tok")
	t.Setenv(envAccount, "acct")
	t.Setenv(envIssueKey, "ACME-7")
	t.Setenv(envIssueID, "10042")
	s, err := sandboxFromEnv(true)
	if err != nil || s.host != "acme.atlassian.net" || s.project() != "ACME" {
		t.Fatalf("record sandbox = %+v, %v", s, err)
	}
	t.Setenv(envSite, "http://acme.atlassian.net")
	if _, err := sandboxFromEnv(true); err == nil {
		t.Fatal("a non-https site was accepted")
	}
}
