package model

import (
	"strings"
	"testing"
)

func TestParseID(t *testing.T) {
	valid := map[string]string{
		"v7":           "01a12892-5bd7-77d7-8216-02e95ed64b10",
		"v4":           "f47ac10b-58cc-4372-a567-0e02b2c3d479",
		"v1":           "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"v8 variant b": "ffffffff-ffff-8fff-bfff-ffffffffffff",
		"sequence":     "00000000-0000-7000-8000-000000000001",
		"variant 9":    "00000000-0000-7000-9000-000000000001",
		"variant a":    "00000000-0000-7000-a000-000000000001",
	}
	for name, s := range valid {
		t.Run("valid/"+name, func(t *testing.T) {
			id, err := ParseID(s)
			requireOK(t, err)
			if string(id) != s {
				t.Fatalf("ParseID rewrote %q to %q", s, id)
			}
			requireOK(t, id.Validate())
		})
	}
	invalid := map[string]string{
		"empty":            "",
		"uppercase":        "01A12892-5BD7-77D7-8216-02E95ED64B10",
		"one upper nibble": "01a12892-5bd7-77d7-8216-02e95ed64b1F",
		"braced":           "{01a12892-5bd7-77d7-8216-02e95ed64b10}",
		"urn":              "urn:uuid:01a12892-5bd7-77d7-8216-02e95ed64b10",
		"hyphenless":       "01a128925bd777d7821602e95ed64b10",
		"nil":              "00000000-0000-0000-0000-000000000000",
		"max":              "ffffffff-ffff-ffff-ffff-ffffffffffff",
		"version 0":        "01a12892-5bd7-07d7-8216-02e95ed64b10",
		"version 9":        "01a12892-5bd7-97d7-8216-02e95ed64b10",
		"variant 0":        "01a12892-5bd7-77d7-0216-02e95ed64b10",
		"variant c":        "01a12892-5bd7-77d7-c216-02e95ed64b10",
		"hyphen moved":     "01a1289-25bd7-77d7-8216-02e95ed64b10",
		"hyphen replaced":  "01a1289205bd7-77d7-8216-02e95ed64b10",
		"non-hex":          "01a12892-5bd7-77d7-8216-02e95ed64b1g",
		"leading space":    " 1a12892-5bd7-77d7-8216-02e95ed64b10",
		"trailing newline": "01a12892-5bd7-77d7-8216-02e95ed64b1\n",
		"too long":         "01a12892-5bd7-77d7-8216-02e95ed64b100",
		"too short":        "01a12892-5bd7-77d7-8216-02e95ed64b1",
	}
	for name, s := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			id, err := ParseID(s)
			requireError(t, err, Validation, "id")
			if id != "" {
				t.Fatalf("ParseID returned %q with error", id)
			}
			requireError(t, ID(s).Validate(), Validation, "id")
		})
	}
}

func TestOpaqueTokens(t *testing.T) {
	type check func(string) error
	checks := map[string]check{
		"tenant": func(s string) error { _, err := ParseTenantID(s); return err },
		"actor":  func(s string) error { _, err := ParseActorID(s); return err },
		"operation": func(s string) error {
			return OperationID(s).Validate()
		},
		"correlation": func(s string) error {
			return CorrelationID(s).Validate()
		},
	}
	valid := []string{
		"acme",
		"tenant with inner space",
		"ünïcode-ключ",
		"UPPER/Mixed:case",
		strings.Repeat("a", 255),
	}
	invalid := map[string]string{
		"empty":             "",
		"too long":          strings.Repeat("a", 256),
		"invalid utf8":      "a\xffb",
		"nul":               "a\x00b",
		"newline":           "a\nb",
		"c1 control":        "a\u0085b",
		"leading space":     " acme",
		"trailing space":    "acme ",
		"trailing nbsp":     "acme ",
		"leading ideograph": "　acme",
	}
	for field, fn := range checks {
		for _, s := range valid {
			t.Run(field+"/valid/"+s[:min(len(s), 12)], func(t *testing.T) {
				requireOK(t, fn(s))
			})
		}
		for name, s := range invalid {
			t.Run(field+"/invalid/"+name, func(t *testing.T) {
				requireError(t, fn(s), Validation, field)
			})
		}
	}
}

func TestParseTokensNeverRewrite(t *testing.T) {
	tenant, err := ParseTenantID("Acme Corp")
	requireOK(t, err)
	if tenant != "Acme Corp" {
		t.Fatalf("tenant rewritten to %q", tenant)
	}
	actor, err := ParseActorID("User:42")
	requireOK(t, err)
	if actor != "User:42" {
		t.Fatalf("actor rewritten to %q", actor)
	}
	if got, err := ParseTenantID(""); err == nil || got != "" {
		t.Fatalf("ParseTenantID(\"\") = %q, %v", got, err)
	}
	if got, err := ParseActorID(""); err == nil || got != "" {
		t.Fatalf("ParseActorID(\"\") = %q, %v", got, err)
	}
}

func TestPrincipalValidate(t *testing.T) {
	requireOK(t, Principal{Tenant: tenantA, Actor: "bot"}.Validate())
	requireError(t, Principal{Actor: "bot"}.Validate(), Validation, "tenant")
	requireError(t, Principal{Tenant: tenantA}.Validate(), Validation, "actor")
}
