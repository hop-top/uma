package zendesk_test

import (
	"fmt"
	"net/http"
	"os"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/ticket/zendesk"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/testing/xrrtest"
)

// cassettes holds recorded Support API exchanges. See TestRecordedSandbox
// to record them.
const cassettes = "testdata/cassettes/zendesk"

// TestRecordedSandbox replays recorded exchanges with a Zendesk sandbox:
// a public reply, its reconciliation and an internal note on one ticket.
// It skips until cassettes exist. To record, against a sandbox account
// and a throwaway ticket, with an agent's API token:
//
//	ZENDESK_SUBDOMAIN=... ZENDESK_EMAIL=... ZENDESK_API_TOKEN=... ZENDESK_TICKET_ID=... \
//	  XRR_MODE=record go test -run TestRecordedSandbox
//
// or with an OAuth access token (API tokens stop working by 2027-04-30):
//
//	ZENDESK_AUTH=oauth ZENDESK_SUBDOMAIN=... ZENDESK_ACCESS_TOKEN=... ZENDESK_TICKET_ID=... \
//	  XRR_MODE=record go test -run TestRecordedSandbox
//
// The two variants record the same exchanges, and replay does not depend on
// ZENDESK_AUTH. Subdomain and agent email are recorded as placeholders, the
// ticket ID as a stand-in where it is a ticket ID (see recorded_scrub_test.go),
// the recorder's IP address and location are replaced, and the credential is
// never recorded; review the diff of testdata/cassettes before committing.
func TestRecordedSandbox(t *testing.T) {
	record := os.Getenv("XRR_MODE") == string(xrrtest.ModeRecord)
	if _, err := os.Stat(cassettes); err != nil && !record {
		t.Skip("no recorded Zendesk cassettes; record with XRR_MODE=record and sandbox credentials")
	}
	s, err := loadSandbox(record, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	runSandbox(t, xrrtest.NewClient(t, cassettes, recorderOptions(t, s)...), s)
}

// Environment variables of the recorded test.
const (
	envAuth        = "ZENDESK_AUTH" // api_token (default) or oauth
	envSubdomain   = "ZENDESK_SUBDOMAIN"
	envEmail       = "ZENDESK_EMAIL"
	envAPIToken    = "ZENDESK_API_TOKEN"    //nolint:gosec // the name of an environment variable, not a credential.
	envAccessToken = "ZENDESK_ACCESS_TOKEN" //nolint:gosec // the name of an environment variable, not a credential.
	envTicketID    = "ZENDESK_TICKET_ID"
)

// sandbox is what one run of the recorded test acts as.
type sandbox struct {
	sub, email, ticket string
	auth               string
	cred               ports.Credential
	// secrets are the values no recording may hold.
	secrets []residueLiteral
}

// loadSandbox reads the sandbox from the environment. Recording needs every
// value the chosen authentication uses; replay takes fakes for what is
// unset, so both variants replay the same cassettes.
func loadSandbox(record bool, getenv func(string) string) (sandbox, error) {
	auth := getenv(envAuth)
	if auth == "" {
		auth = zendesk.AuthAPIToken
	}
	if auth != zendesk.AuthAPIToken && auth != zendesk.AuthOAuth {
		return sandbox{}, fmt.Errorf("%s=%q: want %s or %s", envAuth, auth, zendesk.AuthAPIToken, zendesk.AuthOAuth)
	}
	var missing error
	env := func(k, replay string) string {
		if v := getenv(k); v != "" {
			return v
		}
		if record && missing == nil {
			missing = fmt.Errorf("recording with %s=%s needs %s", envAuth, auth, k)
		}
		return replay
	}
	s := sandbox{auth: auth, sub: env(envSubdomain, "sandbox"), ticket: env(envTicketID, scrubTicketID)}
	switch auth {
	case zendesk.AuthOAuth:
		tok := env(envAccessToken, "replay-token")
		s.cred = ports.OAuth2{AccessToken: tok}
		s.secrets = []residueLiteral{{envAccessToken, tok}}
	default:
		s.email = env(envEmail, "agent@example.com")
		tok := env(envAPIToken, "replay-token")
		s.cred = ports.Secret{Bytes: []byte(tok)}
		s.secrets = []residueLiteral{{envAPIToken, tok}, {envEmail, s.email}}
	}
	s.secrets = append(s.secrets, residueLiteral{envSubdomain, s.sub})
	if missing != nil {
		return sandbox{}, missing
	}
	if record && s.ticket == scrubTicketID {
		return sandbox{}, fmt.Errorf("%s=%s is the stand-in recorded for the ticket; use the throwaway ticket's own ID", envTicketID, scrubTicketID)
	}
	return s, nil
}

// recorderOptions are the recording options of a sandbox run: placeholders
// for the account, the ticket and audit scrubbing, and a residue check on
// every body about to be saved, which fails the test when a real value
// survives. Extra options come last.
func recorderOptions(t testing.TB, s sandbox, extra ...xrrtest.Option) []xrrtest.Option {
	sc := zendeskScrub{ticket: s.ticket}
	opts := []xrrtest.Option{
		xrrtest.WithPlaceholder(s.email, "ZENDESK_EMAIL"),
		xrrtest.WithPlaceholder(s.sub, "ZENDESK_SUBDOMAIN"),
		xrrtest.WithHeaders("Zendesk-RateLimit-*"),
		xrrtest.WithNormalize(sc.normalize),
		xrrtest.WithScrub(func(contentType string, b []byte) []byte {
			b = sc.body(contentType, b)
			for _, f := range residue(contentType, b, s.ticket, s.secrets) {
				t.Errorf("zendesk recording keeps a real value: %s", f)
			}
			return b
		}),
	}
	return append(opts, extra...)
}

// runSandbox is the recorded scenario: a public reply, its reconciliation
// and an internal note on the sandbox ticket, through client.
func runSandbox(t *testing.T, client *http.Client, s sandbox) {
	t.Helper()
	cfg := zendesk.Config{Subdomain: s.sub, Auth: s.auth, Email: s.email, WebhookToleranceSeconds: 300}
	inst, err := zendesk.NewFactory(zendesk.WithHTTPClient(client)).Open(t.Context(),
		model.Channel{Ref: hostChannel, Type: zendesk.Type, Configuration: cfg.Extension(), CredentialRef: "zendesk"},
		adapter.Dependencies{Credentials: fixedCredential{s.cred}})
	if err != nil {
		t.Fatal(err)
	}
	z := inst.(*zendesk.Instance)
	eff := adapter.Effect{
		Operation: model.OperationRef{Tenant: hostTenant, ID: "00000ad0-0000-7000-8000-0000000000f1"},
		Attempt:   "00000ad0-0000-7000-8000-0000000000f2",
		Route:     route(hostChannel, s.ticket), Audience: model.Audience{Visibility: model.Public},
	}
	out, err := z.Send(t.Context(), adapter.SendRequest{Effect: eff, Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "uma recorded reply"}}}})
	if err != nil || out.State != model.ProviderAccepted || len(out.References) == 0 {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	if rec, err := z.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: eff}); err != nil || rec.State != model.ProviderAccepted {
		t.Fatalf("Reconcile = %+v, %v; want the reply found", rec, err)
	}
	note := eff
	note.Attempt, note.Audience = "00000ad0-0000-7000-8000-0000000000f3", model.Audience{Visibility: model.Internal}
	if out, err := z.Execute(t.Context(), adapter.ExecuteRequest{Effect: note, Action: internalNote("uma recorded note")}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("internal note = %+v, %v", out, err)
	}
}
