package linear_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	linear "hop.top/uma/adapter/ticket/linear"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/testing/xrrtest"
)

// Cassettes hold exchanges recorded against a sandbox Linear workspace. The
// test skips until they exist. Record it with
//
//	XRR_MODE=record LINEAR_SANDBOX_TOKEN=<personal API key> \
//	LINEAR_SANDBOX_ISSUE=<issue UUID> LINEAR_SANDBOX_IDENTIFIER=<ENG-12> \
//	LINEAR_SANDBOX_TEAM=<team key> LINEAR_SANDBOX_WORKSPACE=<workspace URL key> \
//	LINEAR_SANDBOX_ACCOUNT_ID=<user UUID> LINEAR_SANDBOX_ACCOUNT_NAME=<display name> \
//	go test -count=1 -run '^TestRecordedSandbox$'
//
// docs/recording/linear.md has the steps. Replay needs none of them: the
// identifiers below are the placeholders every cassette holds.
const cassettes = "testdata/cassettes/linear/sandbox"

// Placeholders: what cassettes hold in place of the sandbox's identifiers.
const (
	sandboxIssue      = "00000000-0000-4000-8000-0000000042a2"
	sandboxAccount    = "00000000-0000-4000-8000-000000910001"
	sandboxIdentifier = "SBX-1"
	sandboxTeam       = "SBX"
	sandboxWorkspace  = "uma-sandbox"
	sandboxName       = "uma-sandbox-account"
	// missingIssue is a well-formed issue UUID no workspace holds.
	missingIssue = "00000000-0000-4000-8000-0000000fffff"
)

const (
	envToken     = "LINEAR_SANDBOX_TOKEN" // #nosec G101 -- the name of an environment variable, not a credential
	envIssue     = "LINEAR_SANDBOX_ISSUE"
	envIdent     = "LINEAR_SANDBOX_IDENTIFIER"
	envTeam      = "LINEAR_SANDBOX_TEAM"
	envWorkspace = "LINEAR_SANDBOX_WORKSPACE"
	envAccount   = "LINEAR_SANDBOX_ACCOUNT_ID"
	envName      = "LINEAR_SANDBOX_ACCOUNT_NAME"
)

// TestRecordedSandbox runs one scenario on one sandbox issue: send a reply,
// reconcile it, edit it and reconcile the edit, reconcile an attempt that
// never ran (Linear's answer for an unknown comment), delete the comment and
// reconcile the delete, edit the deleted comment (Linear's refusal), read
// the issue, and send to an issue that does not exist (Linear's answer for
// an unknown issue). It leaves the issue with the comments it started with.
func TestRecordedSandbox(t *testing.T) {
	record := xrrtest.Mode(os.Getenv(xrrtest.EnvMode)) == xrrtest.ModeRecord
	if _, err := os.Stat(cassettes); err != nil && !record {
		t.Skip("no recorded Linear cassettes; record with XRR_MODE=record and sandbox credentials (docs/recording/linear.md)")
	}
	real := map[string]string{}
	for _, k := range []string{envToken, envIssue, envIdent, envTeam, envWorkspace, envAccount, envName} {
		real[k] = os.Getenv(k)
		if record && real[k] == "" {
			t.Skipf("recording needs %s (docs/recording/linear.md)", k)
		}
	}
	issue, account, token := sandboxIssue, sandboxAccount, "replay-token"
	opts := []xrrtest.Option{xrrtest.WithHeaders("X-RateLimit-*")}
	if record {
		issue, account, token = real[envIssue], real[envAccount], real[envToken]
		opts = append(opts,
			xrrtest.WithPlaceholder(real[envIssue], sandboxIssue),
			xrrtest.WithPlaceholder(real[envAccount], sandboxAccount),
			xrrtest.WithPlaceholder(real[envIdent], sandboxIdentifier),
			xrrtest.WithPlaceholder(real[envWorkspace], sandboxWorkspace),
			xrrtest.WithPlaceholder(real[envName], sandboxName),
			xrrtest.WithPlaceholder(`"`+real[envTeam]+`"`, `"`+sandboxTeam+`"`),
			// No real value may reach a cassette: fail the recording instead.
			xrrtest.WithScrub(func(_ string, b []byte) []byte {
				for k, v := range real {
					if k != envToken && strings.Contains(string(b), v) {
						t.Errorf("recording keeps the real value of %s", k)
					}
				}
				if strings.Contains(string(b), token) {
					t.Errorf("recording keeps the API key")
				}
				return b
			}))
	}
	runSandbox(t, xrrtest.NewClient(t, cassettes, opts...), linear.DefaultAPIURL, issue, account, token)
}

// TestSandboxScenarioAgainstStandIn runs the recorded scenario against the
// stand-in endpoint, so the scenario itself is proven before anyone
// records it. The stand-in's answers are scripted from the documentation,
// not observed.
func TestSandboxScenarioAgainstStandIn(t *testing.T) {
	r := newRig(t)
	runSandbox(t, r.lin, r.lin.apiURL("a"), issueA, botID, "replay-token")
}

// runSandbox is the recorded scenario, through client.
func runSandbox(t *testing.T, client ports.HTTPClient, api, issue, account, token string) {
	t.Helper()
	clock := ports.Clock(portstest.NewFixedClock(t0))
	inst, err := linear.NewFactory(linear.Options{}).Open(t.Context(), model.Channel{
		Ref: chanA, Type: linear.Type, Revision: "ch1", CredentialRef: "sandbox",
		Configuration: linear.Config{APIURL: api, Replies: true, AccountID: account}.Extension(),
	}, adapter.Dependencies{Credentials: staticKey(token), Clock: clock, HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	send := sendReq(tryOne, "Recorded reply.")
	send.Route = issueRoute(issue)
	sent, err := inst.(adapter.Sender).Send(t.Context(), send)
	if err != nil || sent.State != model.ProviderAccepted || len(sent.References) != 1 {
		t.Fatalf("Send = %+v, %v", sent, err)
	}
	rec := inst.(adapter.Reconciler)
	found, err := rec.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: send.Effect, Capability: model.CapabilitySend})
	if err != nil || found.State != model.ProviderAccepted || found.References[0] != sent.References[0] {
		t.Fatalf("Reconcile = %+v, %v; want the sent comment %v", found, err, sent.References)
	}
	never, err := rec.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: sendReq(tryTwo, "").Effect, Capability: model.CapabilitySend})
	if err != nil || never.State != model.Failed || never.Failure == nil || never.Failure.Code != model.NotFound {
		t.Fatalf("Reconcile of an attempt that never ran = %+v, %v; want failed not_found", never, err)
	}

	eff := send.Effect
	eff.Attempt, eff.Target = tryTwo, &sent.References[0]
	edit := adapter.EditRequest{Effect: eff, Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Recorded edit."}}}}
	if out, err := inst.(adapter.Editor).Edit(t.Context(), edit); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Edit = %+v, %v", out, err)
	}
	if out, err := rec.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: edit.Effect, Capability: model.CapabilityEdit}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Reconcile edit = %+v, %v", out, err)
	}
	del := adapter.DeleteRequest{Effect: eff}
	if out, err := inst.(adapter.Deleter).Delete(t.Context(), del); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Delete = %+v, %v", out, err)
	}
	if out, err := rec.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: del.Effect, Capability: model.CapabilityDelete}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Reconcile delete = %+v, %v", out, err)
	}
	if out, err := inst.(adapter.Editor).Edit(t.Context(), edit); err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.NotFound {
		t.Fatalf("Edit of the deleted comment = %+v, %v; want rejected not_found", out, err)
	}

	env, err := inst.(adapter.Refetcher).Refetch(t.Context(), adapter.RefetchRequest{Target: issueRoute(issue)})
	if err != nil || env.Kind != model.EnvelopeWorkItem || env.WorkItem == nil || env.WorkItem.Status == "" || env.WorkItem.Subject == "" || env.WorkItem.Validate() != nil {
		t.Fatalf("Refetch = %+v, %v", env, err)
	}

	missing := sendReq(tryOne, "To nowhere.")
	missing.Route = issueRoute(missingIssue)
	out, err := inst.(adapter.Sender).Send(t.Context(), missing)
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.NotFound || out.Binding != model.WorkItemBindingInaccessible {
		t.Fatalf("Send to a missing issue = %+v, %v; want rejected not_found, binding inaccessible", out, err)
	}
}

type staticKey string

func (s staticKey) Resolve(context.Context, ports.CredentialRequest) (ports.Credential, error) {
	return ports.Secret{Bytes: []byte(s)}, nil
}
