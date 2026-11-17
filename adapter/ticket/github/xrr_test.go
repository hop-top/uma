package github_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"hop.top/uma/adapter"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/testing/xrrtest"
)

// Cassettes hold exchanges recorded against a sandbox repository, one
// directory per recorded test. Record them with
//
//	XRR_MODE=record GITHUB_SANDBOX_REPO=owner/repo GITHUB_SANDBOX_ISSUE=<n> \
//	GITHUB_SANDBOX_ACCOUNT_ID=<user ID> GITHUB_SANDBOX_TOKEN=<token> \
//	go test -run '<test>'
//
// The sandbox transport maps the repository, issue, account and logins to
// placeholders (sandboxRepo, sandboxIssue, sandboxAccount) before the
// cassette or the adapter sees an answer, so replay needs no credentials
// and no particular issue number or account. Until a test's cassettes
// exist, its exchanges run through the xrrtest transport in passthrough
// mode against the documented-shape server, so the adapter is proven to
// work on the recorded form of GitHub's answers (filtered headers
// included).
var (
	commentCassettes = filepath.Join("testdata", "cassettes", "github", "comment")
	actCassettes     = filepath.Join("testdata", "cassettes", "github", "act")
)

// recordedChannel opens a channel on a client over cassettes in dir:
// recording through the sandbox transport, replaying, or passing through
// to the fake GitHub, which prepare seeds.
func recordedChannel(t *testing.T, dir string, cfg github.Config, clock ports.Clock, prepare func(*fakeGitHub), opts ...xrrtest.Option) adapter.Instance {
	t.Helper()
	token := apiToken
	switch mode := xrrtest.Mode(os.Getenv(xrrtest.EnvMode)); {
	case mode == xrrtest.ModeRecord:
		sb, ok := sandboxFromEnv()
		if !ok {
			t.Skip("recording needs GITHUB_SANDBOX_REPO, GITHUB_SANDBOX_ISSUE, GITHUB_SANDBOX_ACCOUNT_ID and GITHUB_SANDBOX_TOKEN")
		}
		token = sb.token
		opts = append(opts, xrrtest.WithTransport(sb.transport(http.DefaultTransport)))
	case dirExists(dir):
		// Replay: the recorded host and placeholders.
	default:
		gh := newFakeGitHub(t, clock.Now)
		gh.account("a", sandboxAccount)
		prepare(gh)
		cfg.APIURL = gh.apiURL("a")
		opts = append(opts, xrrtest.WithMode(xrrtest.ModePassthrough), xrrtest.WithTransport(gh.client().Transport))
	}
	client := &http.Client{Transport: xrrtest.NewTransport(t, dir, opts...), CheckRedirect: noRedirect}
	inst, err := github.NewFactory(github.Options{}).Open(t.Context(), model.Channel{
		Ref: chanA, Type: github.Type, Revision: "ch1", CredentialRef: "sandbox",
		Configuration: cfg.Extension(),
	}, adapter.Dependencies{Credentials: staticToken(token), Clock: clock, HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func sandboxRoute() model.ExternalRef { return issueRoute(strconv.Itoa(sandboxIssue)) }

func TestRecordedExchanges(t *testing.T) {
	clock := ports.Clock(portstest.NewFixedClock(t0))
	if xrrtest.Mode(os.Getenv(xrrtest.EnvMode)) == xrrtest.ModeRecord {
		clock = wallClock{}
	}
	inst := recordedChannel(t, commentCassettes, github.Config{Replies: true, AccountID: sandboxAccount}, clock, func(*fakeGitHub) {},
		xrrtest.WithHeaders("Link"), xrrtest.WithDropQuery("since"))
	req := sendReq(tryOne, "Recorded reply.")
	req.Route = sandboxRoute()
	sent, err := inst.(adapter.Sender).Send(t.Context(), req)
	if err != nil || sent.State != model.ProviderAccepted || len(sent.References) != 1 {
		t.Fatalf("Send = %+v, %v", sent, err)
	}
	found, err := inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: req.Effect})
	if err != nil || found.State != model.ProviderAccepted || found.References[0] != sent.References[0] {
		t.Fatalf("Reconcile = %+v, %v; want the sent comment %v", found, err, sent.References)
	}
}

// TestRecordedTransitions closes, reopens and reconciles one sandbox issue,
// opened by the recording account and open when recording starts; it is
// open again at the end. A PATCH of an issue that does not exist records
// GitHub's refusal; it changes nothing.
func TestRecordedTransitions(t *testing.T) {
	inst := recordedChannel(t, actCassettes, github.Config{}, portstest.NewFixedClock(t0), func(gh *fakeGitHub) {
		gh.setIssue("a", sandboxRepo, sandboxIssue, sandboxAccount, "open", nil, t0)
	})
	act := func(status, reason string) adapter.WorkItemActOutcome {
		t.Helper()
		q := actReq(tryOne, status, reason)
		q.Route = sandboxRoute()
		out, err := inst.(adapter.WorkItemActor).ActWorkItem(t.Context(), q)
		if err != nil {
			t.Fatalf("ActWorkItem(%s %q) = %v", status, reason, err)
		}
		return out
	}
	applied := func(out adapter.WorkItemActOutcome, raw string) {
		t.Helper()
		s := out.State
		if out.Outcome.State != model.Delivered || s == nil || s.RawStatus != raw || s.Requester != userRef(strconv.FormatInt(sandboxAccount, 10)) ||
			s.Validate() != nil || out.SourceRevision == "" || out.OccurredAt.IsZero() {
			t.Fatalf("act = %+v state %+v, want delivered %s opened by the account", out, s, raw)
		}
	}
	reconcile := func(status, reason string) model.DeliveryState {
		t.Helper()
		q := actReq(tryOne, status, reason)
		q.Route = sandboxRoute()
		out, err := inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: q.Effect, Status: q.Status, Reason: q.Reason})
		if err != nil {
			t.Fatalf("Reconcile(%s %q) = %v", status, reason, err)
		}
		return out.State
	}

	applied(act(model.WorkItemClosed, ""), "closed:completed")
	if st := reconcile(model.WorkItemClosed, ""); st != model.Delivered {
		t.Fatalf("reconcile close = %s, want delivered", st)
	}
	applied(act(model.WorkItemOpen, ""), "open:reopened")
	applied(act(model.WorkItemClosed, "not_planned"), "closed:not_planned")
	if st := reconcile(model.WorkItemClosed, "completed"); st != model.Failed {
		t.Fatalf("reconcile close completed of a not_planned issue = %s, want failed", st)
	}
	applied(act(model.WorkItemOpen, "reopened"), "open:reopened")

	missing := actReq(tryOne, model.WorkItemClosed, "")
	missing.Route = issueRoute("999999")
	out, err := inst.(adapter.WorkItemActor).ActWorkItem(t.Context(), missing)
	if err != nil || out.Outcome.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.NotFound || out.Binding != model.WorkItemBindingInaccessible {
		t.Fatalf("missing issue = %+v, %v; want rejected not_found, binding inaccessible", out, err)
	}
}

type staticToken string

func (s staticToken) Resolve(context.Context, ports.CredentialRequest) (ports.Credential, error) {
	return ports.Secret{Bytes: []byte(s)}, nil
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
