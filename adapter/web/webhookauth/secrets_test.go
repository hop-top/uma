package webhookauth_test

import (
	"context"
	"errors"
	"testing"

	"hop.top/uma/adapter/web/webhookauth"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

type credStub struct {
	byRef map[string]ports.Credential
	err   error
	reqs  []ports.CredentialRequest
}

func (c *credStub) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	c.reqs = append(c.reqs, r)
	if c.err != nil {
		return nil, c.err
	}
	return c.byRef[r.Reference], nil
}

func TestFromCredentialsResolvesPerCallThroughThePort(t *testing.T) {
	ch := model.ChannelRef{}
	stub := &credStub{byRef: map[string]ports.Credential{
		"cur":  ports.Secret{Bytes: []byte(longSecret)},
		"prev": ports.Password{User: "hook", Secret: []byte("previous-secret-0123456789")},
	}}
	src := webhookauth.FromCredentials(stub,
		ports.CredentialRequest{Channel: ch, Reference: "cur", Purpose: "relay.webhook"},
		ports.CredentialRequest{Channel: ch, Reference: "prev", Purpose: "relay.webhook"})
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeToken}, src, webhookauth.Deps{})
	for _, tok := range []string{longSecret, "previous-secret-0123456789"} {
		if err := verify(t, v, hdr("X-Webhook-Token", tok), ""); err != nil {
			t.Fatalf("token rejected: %v", err)
		}
	}
	// Two requests resolved per verification, none cached across calls.
	if len(stub.reqs) != 4 {
		t.Fatalf("resolved %d times, want 4", len(stub.reqs))
	}
	for _, r := range stub.reqs {
		if r.Purpose != "relay.webhook" || !r.Accepts(ports.KindSecret) || !r.Accepts(ports.KindPassword) {
			t.Fatalf("request = %+v", r)
		}
	}
}

func TestFromCredentialsFailures(t *testing.T) {
	req := ports.CredentialRequest{Reference: "x", Purpose: "p"}
	for _, tc := range []struct {
		name string
		stub *credStub
		want error
	}{
		{"port error", &credStub{err: &model.Error{Code: model.Unavailable}}, webhookauth.ErrUnavailable},
		{"wrong kind", &credStub{byRef: map[string]ports.Credential{"x": ports.OAuth2{AccessToken: "t"}}}, webhookauth.ErrSecret},
		{"nil credential", &credStub{byRef: map[string]ports.Credential{}}, webhookauth.ErrSecret},
		{"empty secret", &credStub{byRef: map[string]ports.Credential{"x": ports.Secret{}}}, webhookauth.ErrSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeToken}, webhookauth.FromCredentials(tc.stub, req), webhookauth.Deps{})
			wantErr(t, verify(t, v, hdr("X-Webhook-Token", longSecret), ""), tc.want)
		})
	}
	var m *model.Error
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeToken}, webhookauth.FromCredentials(&credStub{err: &model.Error{Code: model.Unavailable}}, req), webhookauth.Deps{})
	if err := verify(t, v, hdr("X-Webhook-Token", longSecret), ""); !errors.As(err, &m) {
		t.Fatalf("host error not wrapped: %v", err)
	}
}
