package zendesk_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/ticket/zendesk"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

type fixedCredential struct{ c ports.Credential }

func (f fixedCredential) Resolve(context.Context, ports.CredentialRequest) (ports.Credential, error) {
	return f.c, nil
}

// Each authentication mode sends the documented Authorization header.
func TestAuthorizationHeader(t *testing.T) {
	basic := func(s string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, c := range []struct {
		name  string
		auth  string
		email string
		cred  ports.Credential
		want  string
	}{
		{"api token secret", zendesk.AuthAPIToken, "agent@acme.example", ports.Secret{Bytes: []byte("tok")}, basic("agent@acme.example/token:tok")},
		{"api token password", zendesk.AuthAPIToken, "", ports.Password{User: "lead@acme.example", Secret: []byte("tok")}, basic("lead@acme.example/token:tok")},
		{"oauth", zendesk.AuthOAuth, "", ports.OAuth2{AccessToken: "acc"}, "Bearer acc"},
		{"oauth secret", zendesk.AuthOAuth, "", ports.Secret{Bytes: []byte("acc")}, "Bearer acc"},
		{"api token without email", zendesk.AuthAPIToken, "", ports.Secret{Bytes: []byte("tok")}, ""},
		{"oauth with a password", zendesk.AuthOAuth, "", ports.Password{User: "u", Secret: []byte("p")}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("Authorization")
				_, _ = w.Write([]byte(`{"audit":{"id":1,"events":[{"id":2,"type":"Comment"}]}}`))
			}))
			defer srv.Close()
			cfg := zendesk.Config{Subdomain: hostSub, Auth: c.auth, Email: c.email, WebhookToleranceSeconds: 60}
			inst, err := zendesk.NewFactory(zendesk.WithBaseURL(srv.URL)).Open(t.Context(),
				model.Channel{Ref: hostChannel, Type: zendesk.Type, Configuration: cfg.Extension(), CredentialRef: "zendesk"},
				adapter.Dependencies{Credentials: fixedCredential{c.cred}, HTTP: srv.Client()})
			if err != nil {
				t.Fatal(err)
			}
			out, err := inst.(*zendesk.Instance).Send(t.Context(), sendReq())
			if c.want == "" {
				var me *model.Error
				if !errors.As(err, &me) || got != "" {
					t.Fatalf("Send = %+v, %v (header %q); want a typed refusal without a call", out, err, got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("Send = %+v, %v; Authorization %q, want %q", out, err, got, c.want)
			}
		})
	}
}

// A TLS handshake failure happens before the request is written: safe to
// retry.
func TestTLSHandshakeFailureIsTransient(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("request reached the server")
	}))
	defer srv.Close()
	cfg := zendesk.Config{Subdomain: hostSub, Auth: zendesk.AuthAPIToken, Email: "a@acme.example", WebhookToleranceSeconds: 60}
	inst, err := zendesk.NewFactory(zendesk.WithBaseURL(srv.URL)).Open(t.Context(),
		model.Channel{Ref: hostChannel, Type: zendesk.Type, Configuration: cfg.Extension(), CredentialRef: "zendesk"},
		adapter.Dependencies{Credentials: credentials{}, HTTP: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = inst.(*zendesk.Instance).Send(t.Context(), sendReq())
	if model.CodeOf(err) != model.Transient {
		t.Fatalf("Send over an untrusted certificate = %v, want transient", err)
	}
}

// Configuration is checked without I/O: the webhook replay window is
// required and bounded.
func TestValidate(t *testing.T) {
	f := zendesk.NewFactory()
	ok := zendesk.Config{Subdomain: "acme", Auth: zendesk.AuthOAuth, WebhookToleranceSeconds: 300}
	if issues := f.Validate(ok.Extension()); len(issues) != 0 {
		t.Fatalf("Validate(valid) = %+v", issues)
	}
	for _, bad := range []zendesk.Config{
		{Subdomain: "acme", Auth: zendesk.AuthOAuth},
		{Subdomain: "acme", Auth: zendesk.AuthOAuth, WebhookToleranceSeconds: 3601},
		{Subdomain: "Acme.example", Auth: zendesk.AuthOAuth, WebhookToleranceSeconds: 300},
		{Subdomain: "acme", Auth: "password", WebhookToleranceSeconds: 300},
		{Subdomain: "acme", Auth: zendesk.AuthOAuth, Email: "a@b.c", WebhookToleranceSeconds: 300},
		{Subdomain: "acme", Auth: zendesk.AuthOAuth, WebhookToleranceSeconds: 300, PollIntervalSeconds: 1},
		{Subdomain: "acme", Auth: zendesk.AuthOAuth, WebhookToleranceSeconds: 300, LocalFields: []string{"requester"}},
	} {
		if issues := f.Validate(bad.Extension()); len(issues) == 0 {
			t.Errorf("Validate(%+v) = no issues", bad)
		}
	}
	if issues := f.Validate(model.Extension{Name: zendesk.ConfigSchema, Version: 1, Data: []byte(`{"subdomain":"acme","auth":"oauth","webhook_tolerance_seconds":5,"extra":1}`)}); len(issues) == 0 {
		t.Error("Validate accepted an unknown field")
	}
}
