package conformance

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// The store suite's tables run many inputs through one assertion line, so a
// failure that does not name its input leaves an implementer opening the suite
// to learn which one broke which rule. These tests run the suite against a
// store that accepts what it must reject and require each failure to name the
// input. The failing run is a child process: the suite reports through
// testing.T, which only a real test run can fail.

const brokenChildEnv = "UMA_CONFORMANCE_BROKEN_STORE"

// laxStore swallows the rejections the store contract requires of staging,
// labels, purges and task reads, over an otherwise correct memory store.
type laxStore struct{ store.Store }

func (l laxStore) Transact(ctx context.Context, tenant model.TenantID, fn func(store.Tx) error) error {
	return l.Store.Transact(ctx, tenant, func(tx store.Tx) error { return fn(laxTx{tx}) })
}

func (l laxStore) View(ctx context.Context, tenant model.TenantID, fn func(store.Reader) error) error {
	return l.Store.View(ctx, tenant, func(r store.Reader) error { return fn(laxReader{r}) })
}

// swallow drops err when it carries code.
func swallow(err error, code model.ErrorCode) error {
	if model.IsCode(err, code) {
		return nil
	}
	return err
}

type laxReader struct{ store.Reader }

func (r laxReader) TaskByReference(ctx context.Context, inbox model.InboxRef, ref model.ExternalRef) (model.Task, error) {
	tk, err := r.Reader.TaskByReference(ctx, inbox, ref)
	return tk, swallow(err, model.NotFound)
}

type laxTx struct{ store.Tx }

func (tx laxTx) RecordStaging(ctx context.Context, in store.StagingIntent) error {
	return swallow(tx.Tx.RecordStaging(ctx, in), model.Validation)
}

func (tx laxTx) PutLabel(ctx context.Context, l model.Label, expected model.Revision) error {
	return swallow(tx.Tx.PutLabel(ctx, l, expected), model.Conflict)
}

func (tx laxTx) Purge(ctx context.Context, req store.PurgeRequest) (store.PurgeResult, error) {
	res, err := tx.Tx.Purge(ctx, req)
	return res, swallow(err, model.Validation)
}

func (tx laxTx) TaskByReference(ctx context.Context, inbox model.InboxRef, ref model.ExternalRef) (model.Task, error) {
	tk, err := tx.Tx.TaskByReference(ctx, inbox, ref)
	return tk, swallow(err, model.NotFound)
}

func laxFixture(*testing.T) StoreFixture {
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0).UTC())
	return StoreFixture{
		Store:   laxStore{memory.New(memory.Options{Clock: clock})},
		Now:     clock.Now,
		Advance: func(d time.Duration) { clock.Advance(d) },
	}
}

// TestBrokenStoreChild is the failing run TestFailureMessagesNameTheInput
// inspects; it does nothing on its own.
func TestBrokenStoreChild(t *testing.T) {
	if os.Getenv(brokenChildEnv) == "" {
		t.Skip("run by TestFailureMessagesNameTheInput")
	}
	RunStoreSuite(t, laxFixture)
}

// TestGateTimeoutChild fails a gate wait the way a party blocked behind
// another unit does.
func TestGateTimeoutChild(t *testing.T) {
	if os.Getenv(brokenChildEnv) == "" {
		t.Skip("run by TestFailureMessagesNameTheInput")
	}
	gateWait = 20 * time.Millisecond
	(&faultEnv{}).gate("second delivery").await(t)
}

// childOutput runs the named child test in a fresh process and returns its
// output; the child is expected to fail.
func childOutput(t *testing.T, run string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", run, "-test.count=1") //nolint:gosec // re-runs this test binary with a fixed pattern.
	cmd.Env = append(os.Environ(), brokenChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("%s passed: the lax store was not rejected\n%s", run, out)
	}
	return string(out)
}

// failure returns the report of the failing test whose path is name.
func failure(t *testing.T, out, name string) string {
	t.Helper()
	_, rest, ok := strings.Cut(out, "--- FAIL: "+name+" (")
	if !ok {
		t.Fatalf("%s did not fail:\n%s", name, out)
	}
	report, _, _ := strings.Cut(rest, "--- FAIL: ")
	return report
}

func TestFailureMessagesNameTheInput(t *testing.T) {
	if os.Getenv(brokenChildEnv) != "" {
		t.Skip("child run")
	}
	out := childOutput(t, `^TestBrokenStoreChild$/^(Exports|Staging|Labels|Retention|Tasks)$`)
	const root = "TestBrokenStoreChild/"
	for _, c := range []struct {
		path string
		want []string
	}{
		{"Exports/StagingIntentOwnedByAnExport", []string{"intent naming both an attachment and an export"}},
		{"Staging/RecordRejectsInvalidAndForeign", []string{"malformed key id"}},
		{"Labels/LabelNameUniquePerTenant", []string{`PutLabel "Urgent"`}},
		{"Retention/PurgeRejectsInvalidRequests", []string{"unknown family"}},
		{"Tasks/TaskPutGetByReference", []string{"reference in another channel"}},
	} {
		got := failure(t, out, root+c.path)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: failure does not name its input %q:\n%s", c.path, w, got)
			}
		}
	}
}

func TestGateTimeoutNamesTheParty(t *testing.T) {
	if os.Getenv(brokenChildEnv) != "" {
		t.Skip("child run")
	}
	got := failure(t, childOutput(t, `^TestGateTimeoutChild$`), "TestGateTimeoutChild")
	for _, w := range []string{"TestGateTimeoutChild", `"second delivery"`, "blocked on another unit"} {
		if !strings.Contains(got, w) {
			t.Errorf("gate timeout does not say %q:\n%s", w, got)
		}
	}
}

func TestBriefTrimsDumpsToIdentities(t *testing.T) {
	inbox := model.InboxRef{Tenant: tenantA, ID: "inbox-1"}
	conv := model.ConversationRef{Inbox: inbox, ID: "conv-1"}
	for _, c := range []struct {
		name string
		v    any
		want string
	}{
		{"reference shows its id", conv, "Conversation(conv-1)"},
		{"zero fields are left out", model.Filter{Inboxes: []model.InboxRef{inbox}}, "{Inboxes:[Inbox(inbox-1)]}"},
		{"nothing set", model.Filter{}, "{}"},
		{"reference with more than ids keeps its fields", model.ExternalRef{Namespace: "ns", ID: "x"}, "{Namespace:ns ID:x}"},
		{"bytes show their size", []byte("secret"), "6 bytes"},
		{"nil pointer", (*model.Filter)(nil), "nil"},
	} {
		if got := brief(c.v); got != c.want {
			t.Errorf("%s: brief = %q, want %q", c.name, got, c.want)
		}
	}
}
