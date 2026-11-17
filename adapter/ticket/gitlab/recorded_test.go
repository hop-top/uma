package gitlab_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	gitlab "hop.top/uma/adapter/ticket/gitlab"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/testing/xrrtest"
)

// Cassettes hold exchanges recorded against a sandbox project, one
// directory per recorded test. None is committed yet: each recorded test
// skips until its directory exists, so plain `go test` replays nothing and
// invents nothing. Record them with the steps in docs/recording/gitlab.md:
//
//	XRR_MODE=record GITLAB_SANDBOX_PROJECT_ID=<id> GITLAB_SANDBOX_PROJECT_PATH=<group/project> \
//	GITLAB_SANDBOX_ISSUE=<iid> GITLAB_SANDBOX_ACCOUNT_ID=<user ID> GITLAB_SANDBOX_TOKEN=<token> \
//	go test -run '<test>'
//
// (GITLAB_SANDBOX_URL names a self-managed API root.) The sandbox transport
// maps the host, project, issue, account and user names to placeholders
// before the cassette or the adapter sees an answer, so replay needs no
// credentials and no particular project, issue or account.
var (
	noteCassettes    = filepath.Join("testdata", "cassettes", "gitlab", "note")
	actCassettes     = filepath.Join("testdata", "cassettes", "gitlab", "act")
	historyCassettes = filepath.Join("testdata", "cassettes", "gitlab", "history")
)

// recordedChannel opens a channel on a client over cassettes in dir:
// recording through the sandbox transport, or replaying. It skips when
// neither is possible.
func recordedChannel(t *testing.T, dir string, cfg gitlab.Config) (adapter.Instance, ports.Clock) {
	t.Helper()
	token := apiToken
	var clock ports.Clock = portstest.NewFixedClock(t0)
	opts := []xrrtest.Option{xrrtest.WithHeaders("X-Next-Page", "X-Page", "X-Per-Page", "RateLimit-*")}
	switch mode := xrrtest.Mode(os.Getenv(xrrtest.EnvMode)); {
	case mode == xrrtest.ModeRecord:
		sb, ok := sandboxFromEnv()
		if !ok {
			t.Skip("recording needs GITLAB_SANDBOX_PROJECT_ID, GITLAB_SANDBOX_PROJECT_PATH, GITLAB_SANDBOX_ISSUE, GITLAB_SANDBOX_ACCOUNT_ID and GITLAB_SANDBOX_TOKEN")
		}
		token, clock = sb.token, wallClock{}
		opts = append(opts, xrrtest.WithTransport(sb.transport(http.DefaultTransport)))
	case dirExists(dir):
		// Replay: the recorded host and placeholders.
	default:
		t.Skipf("no recorded cassettes in %s; record them with docs/recording/gitlab.md", dir)
	}
	cfg.APIURL = sandboxAPI
	client := &http.Client{Transport: xrrtest.NewTransport(t, dir, opts...), CheckRedirect: noRedirect}
	inst, err := gitlab.NewFactory(gitlab.Options{}).Open(t.Context(), model.Channel{
		Ref: chanA, Type: gitlab.Type, Revision: "ch1", CredentialRef: "sandbox", Configuration: cfg.Extension(),
	}, adapter.Dependencies{Credentials: staticToken(token), Clock: clock, HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	return inst, clock
}

func sandboxRoute() model.ExternalRef { return issueRoute(strconv.FormatInt(sandboxIssue, 10)) }

func sandboxEffect(attempt model.ID) adapter.Effect { return effect(attempt, sandboxRoute()) }

// TestRecordedExchanges sends two replies to the sandbox issue (the second
// holds a line GitLab would run as a quick action), finds the first again,
// reads both back, edits the first, finds the edit, and deletes both,
// finding the deletion and recording GitLab's refusal of a second delete.
// The issue is opened by any account and stays open: the quick action line
// was escaped, so it did not run.
func TestRecordedExchanges(t *testing.T) {
	inst, _ := recordedChannel(t, noteCassettes, gitlab.Config{Replies: true, AccountID: sandboxAccount})
	send := func(attempt model.ID, text string) model.ExternalRef {
		t.Helper()
		out, err := inst.(adapter.Sender).Send(t.Context(), adapter.SendRequest{Effect: sandboxEffect(attempt), Content: textOf(text)})
		if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
			t.Fatalf("Send = %+v, %v", out, err)
		}
		return out.References[0]
	}
	first := send(tryOne, "Recorded reply.")
	second := send(tryTwo, "Recorded quick action line:\n/close\nEnd of reply.")

	found, err := inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: sandboxEffect(tryOne)})
	if err != nil || found.State != model.ProviderAccepted || found.References[0] != first {
		t.Fatalf("Reconcile = %+v, %v; want the first reply %v", found, err, first)
	}

	refetch := func(target model.ExternalRef) model.Envelope {
		t.Helper()
		env, err := inst.(adapter.Refetcher).Refetch(t.Context(), adapter.RefetchRequest{Target: target, References: []model.ExternalRef{sandboxRoute()}})
		if err != nil {
			t.Fatalf("Refetch(%v) = %v", target, err)
		}
		return env
	}
	if env := refetch(second); !strings.Contains(env.Content.Parts[0].Text, "/close") || env.Sender.ID != strconv.FormatInt(sandboxAccount, 10) || env.Destination != sandboxPath {
		t.Fatalf("second reply = %+v, want the escaped line stored and read back for the account", env)
	}
	if env := refetch(sandboxRoute()); env.WorkItem == nil || env.WorkItem.Status != model.WorkItemOpen {
		t.Fatalf("issue = %+v, want it still open: the quick action must not have run", env.WorkItem)
	}

	target := func(ref model.ExternalRef, attempt model.ID) adapter.Effect {
		e := sandboxEffect(attempt)
		e.Target = &ref
		return e
	}
	edit := target(first, tryTwo)
	out, err := inst.(adapter.Editor).Edit(t.Context(), adapter.EditRequest{Effect: edit, Content: textOf("Recorded reply, edited.")})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Edit = %+v, %v", out, err)
	}
	out, err = inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: edit, Capability: model.CapabilityEdit})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Reconcile of the edit = %+v, %v", out, err)
	}

	for _, ref := range []model.ExternalRef{first, second} {
		out, err = inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: target(ref, tryOne)})
		if err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("Delete %v = %+v, %v", ref, out, err)
		}
	}
	out, err = inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: target(first, tryOne), Capability: model.CapabilityDelete})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Reconcile of the delete = %+v, %v", out, err)
	}
	out, err = inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: target(first, tryOne)})
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.NotFound {
		t.Fatalf("second Delete = %+v, %v; want rejected not_found", out, err)
	}
}

// TestRecordedTransitions closes, reopens and reconciles the sandbox issue,
// open when recording starts and open again at the end. A second close of an
// already closed issue records how GitLab answers it. An update of an issue
// that does not exist records GitLab's refusal; it changes nothing.
func TestRecordedTransitions(t *testing.T) {
	inst, _ := recordedChannel(t, actCassettes, gitlab.Config{})
	act := func(route model.ExternalRef, status string) adapter.WorkItemActOutcome {
		t.Helper()
		out, err := inst.(adapter.WorkItemActor).ActWorkItem(t.Context(), adapter.WorkItemActRequest{Effect: effect(tryOne, route), Status: status})
		if err != nil {
			t.Fatalf("ActWorkItem(%s) = %v", status, err)
		}
		return out
	}
	applied := func(out adapter.WorkItemActOutcome, raw string) {
		t.Helper()
		if out.Outcome.State != model.Delivered || out.State == nil || out.State.RawStatus != raw || out.State.Validate() != nil || out.SourceRevision == "" || out.OccurredAt.IsZero() {
			t.Fatalf("act = %+v state %+v, want delivered %s", out, out.State, raw)
		}
	}
	reconcile := func(status string) model.DeliveryState {
		t.Helper()
		out, err := inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: sandboxEffect(tryOne), Status: status})
		if err != nil {
			t.Fatalf("Reconcile(%s) = %v", status, err)
		}
		return out.State
	}
	applied(act(sandboxRoute(), model.WorkItemClosed), "closed")
	if st := reconcile(model.WorkItemClosed); st != model.Delivered {
		t.Fatalf("reconcile close = %s, want delivered", st)
	}
	if st := reconcile(model.WorkItemOpen); st != model.Failed {
		t.Fatalf("reconcile reopen of a closed issue = %s, want failed", st)
	}
	applied(act(sandboxRoute(), model.WorkItemClosed), "closed")
	applied(act(sandboxRoute(), model.WorkItemOpen), "opened")
	if st := reconcile(model.WorkItemOpen); st != model.Delivered {
		t.Fatalf("reconcile reopen = %s, want delivered", st)
	}

	out := act(issueRoute("999999"), model.WorkItemClosed)
	if out.Outcome.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.NotFound || out.Binding != model.WorkItemBindingInaccessible {
		t.Fatalf("missing issue = %+v; want rejected not_found, binding inaccessible", out)
	}
}

// TestRecordedHistory posts three replies, reads the window that starts at
// the first reply's creation time in pages of two, and deletes them, so the
// issue ends with the notes it started with. The window holds exactly the
// three replies: no other note may be written on the issue while recording.
func TestRecordedHistory(t *testing.T) {
	inst, _ := recordedChannel(t, historyCassettes, gitlab.Config{Replies: true, AccountID: sandboxAccount})
	var refs []model.ExternalRef
	for i, text := range []string{"Recorded history one.", "Recorded history two.", "Recorded history three."} {
		out, err := inst.(adapter.Sender).Send(t.Context(), adapter.SendRequest{
			Effect:  effect(model.ID("00000000-0000-7000-8000-00000000b0c"+strconv.Itoa(i+1)), sandboxRoute()),
			Content: textOf(text),
		})
		if err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("Send %d = %+v, %v", i+1, out, err)
		}
		refs = append(refs, out.References[0])
	}
	first, err := inst.(adapter.Refetcher).Refetch(t.Context(), adapter.RefetchRequest{Target: refs[0], References: []model.ExternalRef{sandboxRoute()}})
	if err != nil {
		t.Fatal(err)
	}
	since, err := time.Parse(time.RFC3339Nano, first.SourceRevision)
	if err != nil {
		t.Fatalf("first reply revision %q: %v", first.SourceRevision, err)
	}
	route := sandboxRoute()
	var got []model.Envelope
	cursor := ""
	for range 10 {
		p, err := inst.(adapter.HistoryReader).History(t.Context(), adapter.HistoryRequest{Route: &route, Since: since, Cursor: cursor, Limit: 2})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		got, cursor = append(got, p.Envelopes...), p.Next
		if cursor == "" {
			break
		}
	}
	if len(got) != 3 {
		t.Fatalf("history = %d messages, want the three replies", len(got))
	}
	for i, e := range got {
		if refs[i].ID != e.MessageID || e.Destination != sandboxPath || e.References[0] != route {
			t.Fatalf("message %d = %+v, want reply %v", i, e, refs[i])
		}
	}
	for _, ref := range refs {
		e := sandboxEffect(tryOne)
		e.Target = &ref
		if out, err := inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: e}); err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("Delete %v = %+v, %v", ref, out, err)
		}
	}
}

// TestRecordedWebhookPayloads runs captured deliveries from the sandbox
// project through the handler: every file in testdata/webhooks is one
// delivery body, named <event>__<case>.json, where <event> is the
// X-Gitlab-Event header with spaces as dashes ("Issue-Hook", "Note-Hook",
// "Confidential-Note-Hook"). It skips until files exist. A delivery of an
// event the adapter reads must be accepted; one it drops (a system note, a
// merge request note) must produce no envelope.
func TestRecordedWebhookPayloads(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("testdata", "webhooks", "*.json"))
	if len(files) == 0 {
		t.Skip("no captured deliveries in testdata/webhooks; capture them with docs/recording/gitlab.md")
	}
	h := newHookRig(t, gitlab.Config{Replies: true, AccountID: sandboxAccount})
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		event, _, _ := strings.Cut(name, "__")
		body, err := os.ReadFile(f) // #nosec G304 -- fixed fixture directory
		if err != nil {
			t.Fatal(err)
		}
		t.Run(name, func(t *testing.T) {
			if !json.Valid(body) {
				t.Fatal("not JSON")
			}
			before := len(h.sink.envelopes())
			if code, msg := h.serve(hookRequest(strings.ReplaceAll(event, "-", " "), body)); code != http.StatusOK {
				t.Fatalf("delivery = %d %q, want 200", code, msg)
			}
			got := h.sink.envelopes()[before:]
			dropped := strings.Contains(name, "__system") || strings.Contains(name, "__merge-request")
			if dropped != (len(got) == 0) {
				t.Fatalf("%d envelopes for %s, dropped = %v", len(got), name, dropped)
			}
			for _, e := range got {
				if e.Destination == "" || len(e.References) == 0 || e.References[0].Kind != model.RefKindWorkItem {
					t.Fatalf("envelope = %+v", e)
				}
			}
		})
	}
}

type staticToken string

func (s staticToken) Resolve(_ context.Context, _ ports.CredentialRequest) (ports.Credential, error) {
	return ports.Secret{Bytes: []byte(s)}, nil
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
