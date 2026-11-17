package jira_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/ticket/jira"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/testing/xrrtest"
)

// Cassettes hold exchanges recorded against a sandbox Jira Cloud site, one
// directory per recorded test. They do not exist yet: the recorded tests
// skip until the owner records them. The steps, environment variables, what
// the test creates and removes, and the scrubbing checklist are in
// docs/recording/jira.md. Record with
//
//	XRR_MODE=record JIRA_SANDBOX_SITE=https://<site>.atlassian.net \
//	JIRA_SANDBOX_EMAIL=<account email> JIRA_SANDBOX_TOKEN=<API token> \
//	JIRA_SANDBOX_ACCOUNT_ID=<accountId> JIRA_SANDBOX_ISSUE_KEY=<KEY-n> \
//	JIRA_SANDBOX_ISSUE_ID=<numeric id> go test -run TestRecordedComments
//
// The site host, email, account ID, issue key and ID and project key are
// replaced by the placeholders below in every cassette and in every answer
// the adapter sees, so replay needs no credentials and no sandbox.
var commentCassettes = filepath.Join("testdata", "cassettes", "jira", "comments")

// Placeholders of the sandbox values: what cassettes hold, and what a
// replay run uses.
const (
	placeholderHost        = "sandbox.example.atlassian.net"
	placeholderEmail       = "sandbox@example.com"
	placeholderAccount     = "5b10ac8d82e05b22cc7d4ef5"
	placeholderIssueKey    = "SBX-1"
	placeholderIssueID     = "10001"
	placeholderProject     = "SBX"
	placeholderUserName    = "Sandbox User"
	placeholderProjectName = "Sandbox Project"
	placeholderTimeZone    = "UTC"
	envSite                = "JIRA_SANDBOX_SITE"
	envEmail               = "JIRA_SANDBOX_EMAIL"
	envToken               = "JIRA_SANDBOX_TOKEN" // #nosec G101 -- environment variable name, not a credential
	envAccount             = "JIRA_SANDBOX_ACCOUNT_ID"
	envIssueKey            = "JIRA_SANDBOX_ISSUE_KEY"
	envIssueID             = "JIRA_SANDBOX_ISSUE_ID"
	recordedReplyText      = "uma recorded reply"
	recordedEditText       = "uma recorded edit"
	recordedOperationID    = "00000ad0-0000-7000-8000-0000000000f1"
	recordedAttemptSend    = "00000ad0-0000-7000-8000-0000000000f2"
	recordedAttemptEdit    = "00000ad0-0000-7000-8000-0000000000f3"
	recordedAttemptDelet   = "00000ad0-0000-7000-8000-0000000000f4"
)

// sandboxSite is the real sandbox a recording reaches, or the placeholders a
// replay uses.
type sandboxSite struct {
	site, host, email, token, account, key, id string
}

func (s sandboxSite) project() string { return strings.SplitN(s.key, "-", 2)[0] }

// sandboxFromEnv reads the sandbox from the environment. Recording needs
// every value; replay takes the placeholders.
func sandboxFromEnv(record bool) (sandboxSite, error) {
	s := sandboxSite{
		site: "https://" + placeholderHost, host: placeholderHost, email: placeholderEmail, token: "replay-token",
		account: placeholderAccount, key: placeholderIssueKey, id: placeholderIssueID,
	}
	if !record {
		return s, nil
	}
	get := func(name string) (string, error) {
		v := os.Getenv(name)
		if v == "" {
			return "", fmt.Errorf("recording needs %s", name)
		}
		return v, nil
	}
	var err error
	for _, f := range []struct {
		name string
		dst  *string
	}{{envSite, &s.site}, {envEmail, &s.email}, {envToken, &s.token}, {envAccount, &s.account}, {envIssueKey, &s.key}, {envIssueID, &s.id}} {
		if *f.dst, err = get(f.name); err != nil {
			return sandboxSite{}, err
		}
	}
	host, ok := strings.CutPrefix(strings.TrimRight(s.site, "/"), "https://")
	if !ok || host == "" || strings.Contains(host, "/") {
		return sandboxSite{}, fmt.Errorf("%s must be https://<site>.atlassian.net", envSite)
	}
	s.host = host
	return s, nil
}

func (s sandboxSite) options(t testing.TB, extra ...xrrtest.Option) []xrrtest.Option {
	sc := jiraScrub{}
	opts := []xrrtest.Option{
		xrrtest.WithPlaceholder(s.host, placeholderHost),
		xrrtest.WithPlaceholder(s.email, placeholderEmail),
		xrrtest.WithPlaceholder(s.account, placeholderAccount),
		xrrtest.WithPlaceholder(url.QueryEscape(s.account), placeholderAccount),
		xrrtest.WithPlaceholder(s.key, placeholderIssueKey),
		xrrtest.WithPlaceholder(s.id, placeholderIssueID),
		xrrtest.WithPlaceholder(s.project(), placeholderProject),
		xrrtest.WithHeaders("X-Ratelimit-*", "Retry-After"),
		xrrtest.WithScrub(func(contentType string, b []byte) []byte {
			b = sc.body(contentType, b)
			for _, f := range residue(string(b), s.host, s.email, s.account, url.QueryEscape(s.account), s.token, s.key, s.id) {
				t.Errorf("jira recording keeps a real value: %s", f)
			}
			return b
		}),
	}
	return append(opts, extra...)
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

type fixedCred struct{ ports.Credential }

func (c fixedCred) Resolve(context.Context, ports.CredentialRequest) (ports.Credential, error) {
	return c.Credential, nil
}

// TestRecordedComments is the recorded scenario on one sandbox issue: a
// reply, its reconciliation by the echo property, an edit and its
// reconciliation, refetch of the comment and of the issue, a delete and its
// reconciliation, and a second delete (Jira's refusal of a comment that is
// gone). It creates one comment and removes it. It skips until cassettes
// exist.
func TestRecordedComments(t *testing.T) {
	record := xrrtest.Mode(os.Getenv(xrrtest.EnvMode)) == xrrtest.ModeRecord
	if _, err := os.Stat(commentCassettes); err != nil && !record {
		t.Skip("no recorded Jira cassettes; record with XRR_MODE=record and sandbox credentials (docs/recording/jira.md)")
	}
	s, err := sandboxFromEnv(record)
	if err != nil {
		t.Fatal(err)
	}
	var clock ports.Clock = fixedClock{t0}
	if record {
		clock = wallClock{}
	}
	// xrr redacts JSON fields named "key" as credentials: the issue key, the
	// project key and the status category's key, and the "key" of the echo
	// property in request bodies, which is part of the fingerprint. Recording
	// and replay both keep them; placeholders cover the real keys.
	t.Setenv("XRR_REDACT_ALLOW", "key")
	client := xrrtest.NewClient(t, commentCassettes, s.options(t)...)
	runRecorded(t, client, s, clock)
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func runRecorded(t *testing.T, client *http.Client, s sandboxSite, clock ports.Clock) {
	t.Helper()
	cfg := jira.Config{SiteURL: "https://" + s.host, Replies: true, AccountID: s.account}
	inst, err := jira.NewFactory().Open(t.Context(), model.Channel{
		Ref: chanA, Type: jira.Type, Configuration: cfg.Extension(), CredentialRef: "jira",
	}, adapter.Dependencies{
		Credentials: fixedCred{ports.Password{User: s.email, Secret: []byte(s.token)}}, Clock: clock, HTTP: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	eff := func(attempt string) adapter.Effect {
		return adapter.Effect{
			Operation: model.OperationRef{Tenant: tenant, ID: recordedOperationID}, Attempt: model.ID(attempt),
			Route: route(s.id), Audience: model.Audience{Visibility: model.Public},
		}
	}
	reconcile := func(c model.Capability, e adapter.Effect) adapter.Outcome {
		t.Helper()
		out, err := inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: e, Capability: c})
		if err != nil {
			t.Fatalf("Reconcile(%s) = %v", c, err)
		}
		return out
	}

	sent, err := inst.(adapter.Sender).Send(t.Context(), adapter.SendRequest{Effect: eff(recordedAttemptSend), Content: text(recordedReplyText)})
	if err != nil || sent.State != model.ProviderAccepted || len(sent.References) != 1 {
		t.Fatalf("Send = %+v, %v", sent, err)
	}
	ref := sent.References[0]
	if found := reconcile(model.CapabilitySend, eff(recordedAttemptSend)); found.State != model.ProviderAccepted || found.References[0] != ref {
		t.Fatalf("Reconcile of the reply = %+v, want the sent comment %v", found, ref)
	}
	if lost := reconcile(model.CapabilitySend, eff(recordedAttemptEdit)); lost.State != model.Failed {
		t.Fatalf("Reconcile of an attempt never made = %+v, want failed", lost)
	}

	mutation := func(attempt string) adapter.Effect {
		e := eff(attempt)
		e.Target = &ref
		return e
	}
	if out, err := inst.(adapter.Editor).Edit(t.Context(), adapter.EditRequest{Effect: mutation(recordedAttemptEdit), Content: text(recordedEditText)}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Edit = %+v, %v", out, err)
	}
	if out := reconcile(model.CapabilityEdit, mutation(recordedAttemptEdit)); out.State != model.ProviderAccepted {
		t.Fatalf("Reconcile of the edit = %+v", out)
	}
	env, err := inst.(adapter.Refetcher).Refetch(t.Context(), adapter.RefetchRequest{Target: ref, Destination: s.project(), References: []model.ExternalRef{route(s.id)}})
	if err != nil || env.Kind != model.EnvelopeMessageEdited || len(env.Content.Parts) != 1 || env.Content.Parts[0].Text != recordedEditText {
		t.Fatalf("Refetch of the comment = %+v, %v; want the edited text", env, err)
	}
	env, err = inst.(adapter.Refetcher).Refetch(t.Context(), adapter.RefetchRequest{Target: route(s.id), Destination: s.project()})
	if err != nil || env.WorkItem == nil || env.WorkItem.Validate() != nil || env.WorkItem.Status == "" || env.WorkItem.URL == "" {
		t.Fatalf("Refetch of the issue = %+v, %v", env, err)
	}

	if out, err := inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: mutation(recordedAttemptDelet)}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Delete = %+v, %v", out, err)
	}
	if out := reconcile(model.CapabilityDelete, mutation(recordedAttemptDelet)); out.State != model.ProviderAccepted {
		t.Fatalf("Reconcile of the delete = %+v", out)
	}
	out, err := inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: mutation(recordedAttemptDelet)})
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.NotFound {
		t.Fatalf("second Delete = %+v, %v; want rejected not_found", out, err)
	}
}

// stand-in: runs the recorded scenario against the in-test Jira double with
// the placeholders as the sandbox, so the scenario's own assertions are
// exercised before any cassette exists. The recording replaces the double's
// answers; the steps do not change.
func TestRecordedScenarioRunsAgainstTheDouble(t *testing.T) {
	r := newRig(t)
	s, _ := sandboxFromEnv(false)
	a := r.j.account(s.host)
	r.j.putIssue(a, s.id, s.key, "indeterminate")
	runRecorded(t, r.j.client(), s, fixedClock{t0})
	if left := r.j.commentsOf(a, s.id); len(left) != 0 {
		t.Fatalf("the scenario left %d comments behind", len(left))
	}
}
