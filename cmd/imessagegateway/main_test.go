package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// Every missing input is refused before the gateway listens, and no
// message names a secret's value.
func TestRunRefusesIncompleteConfiguration(t *testing.T) {
	base := map[string]string{ // #nosec G101 -- fixture values
		"IMESSAGE_GATEWAY_BACKEND":         "blooio",
		"IMESSAGE_GATEWAY_DATA":            t.TempDir(),
		"IMESSAGE_GATEWAY_UMA_WEBHOOK_URL": "http://127.0.0.1:8080/uma",
		"IMESSAGE_GATEWAY_CALLBACK_SECRET": "callback-secret-0123456789",
		"IMESSAGE_GATEWAY_WEBHOOK_SECRET":  "webhook-secret-0123456789",
		"BLOOIO_API_KEY":                   "bl_test_key_value",
		"BLOOIO_FROM":                      "+15550100002",
		"BLOOIO_WEBHOOK_SECRET":            "whsec_test_value",
	}
	for _, missing := range []string{
		"IMESSAGE_GATEWAY_BACKEND", "IMESSAGE_GATEWAY_UMA_WEBHOOK_URL", "IMESSAGE_GATEWAY_CALLBACK_SECRET",
		"IMESSAGE_GATEWAY_WEBHOOK_SECRET", "BLOOIO_API_KEY", "BLOOIO_FROM", "BLOOIO_WEBHOOK_SECRET",
	} {
		t.Run(missing, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			delete(env, missing)
			err := run(t.Context(), nil, envOf(env), io.Discard)
			if err == nil {
				t.Fatal("ran without " + missing)
			}
			for _, v := range base {
				if strings.Contains(v, "secret") || strings.Contains(v, "key") {
					if strings.Contains(err.Error(), v) {
						t.Fatalf("error %q names a secret", err)
					}
				}
			}
		})
	}
	t.Run("bluebubbles without a webhook token", func(t *testing.T) {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		env["IMESSAGE_GATEWAY_BACKEND"] = "bluebubbles"
		env["BLUEBUBBLES_URL"] = "http://192.0.2.10:1234"
		env["BLUEBUBBLES_PASSWORD"] = "bb-password"
		env["BLUEBUBBLES_ACCOUNT"] = "me@example.com"
		if err := run(t.Context(), nil, envOf(env), io.Discard); err == nil || !strings.Contains(err.Error(), "webhook token") {
			t.Fatalf("run = %v, want a webhook token refusal", err)
		}
	})
}

// A secret file wins over the environment; its trailing newline is dropped.
func TestSecretFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(f, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := parse([]string{"-blooio-api-key-file", f}, envOf(map[string]string{"BLOOIO_API_KEY": "from-env"}))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := o.secret("blooio-api-key", envOf(map[string]string{"BLOOIO_API_KEY": "from-env"})); err != nil || v != "from-file" {
		t.Fatalf("secret = %q, %v", v, err)
	}
}
