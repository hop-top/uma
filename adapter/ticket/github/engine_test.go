package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// Engine tests run webhooks and replies end to end: uma.New over the
// memory store with the adapter registered and the fake GitHub behind the
// engine's HTTP client, one channel and one per-work-item inbox for the
// sandbox repository.

var inboxA = model.InboxRef{Tenant: tenant, ID: "00000000-0000-7000-8000-00000000d0a1"}

type idleWaiter struct {
	idle   chan struct{}
	resume chan struct{}
}

func (w *idleWaiter) Wait(ctx context.Context, _ time.Duration) error {
	select {
	case w.idle <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.resume:
		return nil
	}
}

type engineRig struct {
	t      *testing.T
	ctx    context.Context
	gh     *fakeGitHub
	store  *memory.Store
	clock  *portstest.FixedClock
	engine uma.Engine
	waiter *idleWaiter
	hook   http.Handler
	done   chan error
}

func newEngineRig(t *testing.T) *engineRig { return newEngineRigWith(t, replyConfig) }

// newEngineRigWith is newEngineRig with the channel configured by cfg for
// the fake's API root.
func newEngineRigWith(t *testing.T, cfg func(api string) github.Config) *engineRig {
	t.Helper()
	clock := portstest.NewFixedClock(t0)
	r := &engineRig{t: t, ctx: t.Context(), clock: clock, store: memory.New(memory.Options{Clock: clock}),
		waiter: &idleWaiter{idle: make(chan struct{}, 16), resume: make(chan struct{})}}
	r.gh = newFakeGitHub(t, clock.Now)
	r.gh.account("a", botID)
	engine, err := uma.New(uma.Config{
		Store: r.store,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		// No factory fallback: the engine's injected client must be used.
		Adapters:    []adapter.Factory{github.NewFactory(github.Options{})},
		Credentials: &tokenHost{},
		HTTP:        r.gh.client(),
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(0x61),
		Waiter:      r.waiter,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.engine = engine
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	err = r.store.Transact(r.ctx, tenant, func(tx store.Tx) error {
		if err := tx.PutChannel(r.ctx, model.Channel{
			Ref: chanA, Type: github.Type, Configuration: cfg(r.gh.apiURL("a")).Extension(), CredentialRef: webhookSecretID, Revision: "ch1",
		}, ""); err != nil {
			return err
		}
		return tx.PutInbox(r.ctx, model.Inbox{
			Ref: inboxA, Channel: chanA, Name: "sandbox", Destinations: []string{sandboxRepo},
			Selection: model.Selection{Mode: model.ModePerWorkItem}, Revision: "in1",
		}, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	r.hook, err = engine.Runtime().Webhook(r.ctx, r.call(), chanA)
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}
	return r
}

func (r *engineRig) call() uma.RuntimeCall {
	return uma.RuntimeCall{Principal: model.Principal{Tenant: tenant, Actor: "github-service"}, Call: model.Call{Correlation: "github"}}
}

func (r *engineRig) deliver(event, delivery string, body []byte) {
	r.t.Helper()
	rec := httptest.NewRecorder()
	r.hook.ServeHTTP(rec, hookRequest(event, delivery, body))
	if rec.Code != http.StatusOK {
		r.t.Fatalf("%s delivery %s = %d %s", event, delivery, rec.Code, rec.Body.String())
	}
}

func (r *engineRig) messages() []model.Message {
	r.t.Helper()
	var out []model.Message
	r.view(func(rd store.Reader) error {
		p, err := rd.Messages(r.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{inboxA}}, Page: model.PageRequest{Limit: 100}})
		out = p.Items
		return err
	})
	return out
}

func (r *engineRig) conversations() []model.Conversation {
	r.t.Helper()
	var out []model.Conversation
	r.view(func(rd store.Reader) error {
		p, err := rd.Conversations(r.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{inboxA}}, Page: model.PageRequest{Limit: 100}})
		out = p.Items
		return err
	})
	return out
}

func (r *engineRig) workItems() []model.WorkItem {
	r.t.Helper()
	var out []model.WorkItem
	r.view(func(rd store.Reader) error {
		p, err := rd.WorkItems(r.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{inboxA}}, Page: model.PageRequest{Limit: 100}})
		out = p.Items
		return err
	})
	return out
}

func (r *engineRig) view(fn func(store.Reader) error) {
	r.t.Helper()
	if err := r.store.View(r.ctx, tenant, fn); err != nil {
		r.t.Fatal(err)
	}
}

// queueReply queues a public reply on the only conversation.
func (r *engineRig) queueReply(text string) model.OperationRef {
	r.t.Helper()
	convs := r.conversations()
	if len(convs) != 1 {
		r.t.Fatalf("conversations = %d, want 1", len(convs))
	}
	client, err := r.engine.For(model.Principal{Tenant: tenant, Actor: "staff"})
	if err != nil {
		r.t.Fatal(err)
	}
	acc, err := client.Messages().Reply(r.ctx, model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: "reply"}, Operation: "op-reply"},
		Conversation: convs[0].Ref,
		Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
	})
	if err != nil {
		r.t.Fatalf("Reply: %v", err)
	}
	return acc.Operation.Ref
}

// reply queues a reply and runs the worker until it is idle.
func (r *engineRig) reply(text string) model.Operation {
	r.t.Helper()
	ref := r.queueReply(text)
	r.start()
	r.stop()
	return r.operation(ref)
}

// start runs the worker in the background until it first goes idle.
func (r *engineRig) start() {
	r.t.Helper()
	r.done = make(chan error, 1)
	go func() {
		r.done <- r.engine.Runtime().Run(r.ctx, r.call(), uma.WorkerOptions{Owner: "worker", Limit: 10})
	}()
	r.idle()
}

func (r *engineRig) idle() {
	r.t.Helper()
	select {
	case <-r.waiter.idle:
	case err := <-r.done:
		r.t.Fatalf("Run returned early: %v", err)
	case <-time.After(10 * time.Second):
		r.t.Fatal("worker did not go idle")
	}
}

// later advances the clock and lets the idle worker claim again.
func (r *engineRig) later(d time.Duration) {
	r.t.Helper()
	r.clock.Advance(d)
	r.waiter.resume <- struct{}{}
	r.idle()
}

func (r *engineRig) stop() {
	r.t.Helper()
	if err := r.engine.Runtime().Drain(r.ctx, r.call()); err != nil {
		r.t.Fatal(err)
	}
	if err := <-r.done; err != nil {
		r.t.Fatalf("Run: %v", err)
	}
}

func (r *engineRig) operation(ref model.OperationRef) model.Operation {
	r.t.Helper()
	client, err := r.engine.For(model.Principal{Tenant: tenant, Actor: "staff"})
	if err != nil {
		r.t.Fatal(err)
	}
	op, err := client.Operations().Get(r.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "get"}, Ref: ref})
	if err != nil {
		r.t.Fatal(err)
	}
	return op
}

// A server error after the comment was sent may have created it: the
// operation is outcome_unknown and reconciled before any second create.
func TestEngineServerErrorReconcilesBeforeRetry(t *testing.T) {
	r := newEngineRig(t)
	r.deliver("issues", "d-opened", golden(t, "issues-opened"))
	r.gh.respond("a", statusAnswer(http.StatusBadGateway, `{"message":"Server Error"}`, nil), accepted)
	ref := r.queueReply("On it.")
	r.start()
	op := r.operation(ref)
	if op.State != model.Unknown || op.Failure == nil || op.Failure.Code != model.OutcomeUnknown {
		t.Fatalf("after a 502 operation = %+v, want outcome_unknown", op)
	}
	if n := len(r.gh.calls("a")); n != 1 {
		t.Fatalf("calls before reconcile = %d, want 1", n)
	}
	r.later(time.Hour)
	r.stop()
	if op = r.operation(ref); op.State != model.ProviderAccepted {
		t.Fatalf("operation = %+v, want provider_accepted", op)
	}
	var seq []string
	for _, c := range r.gh.calls("a") {
		seq = append(seq, c.Method)
	}
	if want := []string{http.MethodPost, http.MethodGet, http.MethodPost}; !slices.Equal(seq, want) {
		t.Fatalf("calls = %v, want %v: reconcile before the second create", seq, want)
	}
}

func TestEngineIssueLifecycle(t *testing.T) {
	r := newEngineRig(t)
	// A comment observed before its issue binds the same work item.
	r.deliver("issue_comment", "d-comment", golden(t, "issue_comment-created"))
	r.deliver("issues", "d-opened", golden(t, "issues-opened"))
	r.deliver("issues", "d-opened", golden(t, "issues-opened")) // redelivery
	r.deliver("issues", "d-labeled", golden(t, "issues-labeled"))
	r.deliver("issue_comment", "d-edited", golden(t, "issue_comment-edited"))
	r.deliver("issue_comment", "d-pr", golden(t, "issue_comment-created-pull-request"))

	if n := len(r.conversations()); n != 1 {
		t.Fatalf("conversations = %d, want 1", n)
	}
	msgs := r.messages()
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want the description and one comment", len(msgs))
	}
	var edited bool
	for _, m := range msgs {
		if m.Source != github.Type || m.Author.External.Namespace != github.Namespace || m.Author.External.ID != "5000001" {
			t.Errorf("message %+v, want authored by the reporter from github.issues", m.Author)
		}
		edited = edited || m.Content.Parts[0].Text == "Still happens on 1.4.3."
	}
	if !edited {
		t.Error("comment edit not applied")
	}
	items := r.workItems()
	if len(items) != 1 {
		t.Fatalf("work items = %d, want 1", len(items))
	}
	w := items[0]
	if w.External != wiRef("7", sandboxRepo) || w.Subject != "Export fails on large files" || len(w.Assignees) != 1 || len(w.Fields) != 2 {
		t.Fatalf("work item = %+v", w)
	}

	r.deliver("issues", "d-moved", golden(t, "issues-transferred"))
	w = r.workItems()[0]
	if w.Binding != model.WorkItemBindingMoved || w.Successor == nil || *w.Successor != wiRef("42", "example/uma-archive") || w.External != wiRef("7", sandboxRepo) {
		t.Fatalf("transferred work item = %+v, want moved to the new issue with the binding kept", w)
	}
}

func TestEngineOwnCommentIsNotANewMessage(t *testing.T) {
	r := newEngineRig(t)
	r.deliver("issues", "d-opened", golden(t, "issues-opened"))
	op := r.reply("On it.")
	if op.State != model.ProviderAccepted {
		t.Fatalf("reply operation = %+v", op)
	}
	stored := r.gh.stored("a", sandboxRepo+"#7")
	if len(stored) != 1 {
		t.Fatalf("comments = %d, want 1", len(stored))
	}
	// GitHub's webhook for uma's own comment, without its marker: only
	// the provider comment ID correlates it.
	body := commentDelivery(t, stored[0].ID, botID, "On it.")
	r.deliver("issue_comment", "d-echo", body)
	if n := len(r.messages()); n != 2 {
		t.Fatalf("messages = %d, want the description and the reply: an echo is not a new message", n)
	}
}

func TestEngineEchoBeforeSendAnswer(t *testing.T) {
	r := newEngineRig(t)
	r.deliver("issues", "d-opened", golden(t, "issues-opened"))
	// GitHub delivers the comment's webhook before the create call
	// returns, so no provider ID is known yet: the marker correlates it.
	r.gh.mu.Lock()
	r.gh.accounts["a"].onApply = func(c fakeComment) {
		r.deliver("issue_comment", "d-early-echo", echoOf(t, c))
	}
	r.gh.mu.Unlock()
	op := r.reply("On it.")
	if op.State != model.ProviderAccepted {
		t.Fatalf("reply operation = %+v", op)
	}
	msgs := r.messages()
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want the description and the reply: an early echo is not a new message", len(msgs))
	}
	if n := len(r.gh.calls("a")); n != 1 {
		t.Fatalf("provider calls = %d, want one create", n)
	}
}

// echoOf is the issue_comment.created delivery GitHub sends for c.
func echoOf(t *testing.T, c fakeComment) []byte {
	t.Helper()
	return commentDelivery(t, c.ID, c.UserID, c.Body)
}

// A reply GitHub refuses because the issue is gone moves the work item to
// the binding state the answer shows: 410 deleted, 404 inaccessible.
func TestEngineGoneIssueSetsBinding(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"410": {http.StatusGone, `{"message":"This issue was deleted","status":"410"}`, model.WorkItemBindingDeleted},
		"404": {http.StatusNotFound, `{"message":"Not Found","status":"404"}`, model.WorkItemBindingInaccessible},
	} {
		t.Run(name, func(t *testing.T) {
			r := newEngineRig(t)
			r.deliver("issues", "d-opened", golden(t, "issues-opened"))
			r.gh.respond("a", statusAnswer(c.status, c.body, nil))
			if op := r.reply("Anyone?"); op.State != model.Rejected || op.Failure == nil || op.Failure.Code != model.NotFound {
				t.Fatalf("reply operation = %+v, want rejected not_found", op)
			}
			if w := r.workItems()[0]; w.Binding != c.want {
				t.Fatalf("work item binding = %q, want %q", w.Binding, c.want)
			}
		})
	}
}

// edited returns a delivery of golden fixture name with its comment body,
// or its issue's labels, replaced: a second change GitHub reports under the
// same one-second updated_at.
func edited(t *testing.T, name string, change func(ev map[string]any)) []byte {
	t.Helper()
	var ev map[string]any
	if err := json.Unmarshal(golden(t, name), &ev); err != nil {
		t.Fatal(err)
	}
	change(ev)
	b, _ := json.Marshal(ev)
	return b
}

// Two edits of one comment within the same second carry the same
// updated_at: the engine cannot order them and refetches the comment; the
// copy GitHub returns is applied whatever arrived last.
func TestEngineSameSecondCommentEditsRefetch(t *testing.T) {
	r := newEngineRig(t)
	r.deliver("issues", "d-opened", golden(t, "issues-opened"))
	r.deliver("issue_comment", "d-comment", golden(t, "issue_comment-created"))
	r.deliver("issue_comment", "d-edited", golden(t, "issue_comment-edited"))
	const current = "Fixed after clearing the cache."
	r.deliver("issue_comment", "d-edited-again", edited(t, "issue_comment-edited", func(ev map[string]any) {
		ev["comment"].(map[string]any)["body"] = current
	}))
	r.gh.mu.Lock()
	a := r.gh.accounts["a"]
	at := time.Date(2026, 10, 11, 9, 20, 0, 0, time.UTC)
	a.comments[sandboxRepo+"#7"] = append(a.comments[sandboxRepo+"#7"], fakeComment{
		ID: 2000000001, Issue: sandboxRepo + "#7", Body: current, UserID: 5000001, Login: "reporter",
		CreatedAt: at, UpdatedAt: time.Date(2026, 10, 11, 9, 25, 0, 0, time.UTC),
	})
	r.gh.mu.Unlock()
	if got := commentText(r); got != "Still happens on 1.4.3." {
		t.Fatalf("comment before the refetch = %q, want the first edit kept", got)
	}
	r.start()
	r.stop()
	if got := commentText(r); got != current {
		t.Fatalf("comment after the refetch = %q, want GitHub's copy %q", got, current)
	}
	calls := r.gh.calls("a")
	if len(calls) != 1 || calls[0].Method != http.MethodGet || calls[0].Path != "/repos/"+sandboxRepo+"/issues/comments/2000000001" {
		t.Fatalf("calls = %+v, want one read of the comment", calls)
	}
}

// Two issue changes within the same second carry the same updated_at with
// different state: the engine refetches the issue and applies its state.
func TestEngineSameSecondIssueChangesRefetch(t *testing.T) {
	r := newEngineRig(t)
	r.deliver("issues", "d-opened", golden(t, "issues-opened"))
	r.deliver("issues", "d-labeled", golden(t, "issues-labeled"))
	retitle := func(ev map[string]any) { ev["issue"].(map[string]any)["title"] = "Export fails on every file" }
	r.deliver("issues", "d-retitled", edited(t, "issues-labeled", retitle))
	var cur map[string]any
	_ = json.Unmarshal(edited(t, "issues-labeled", retitle), &cur)
	r.gh.mu.Lock()
	r.gh.accounts["a"].issues[sandboxRepo+"#7"] = cur["issue"].(map[string]any)
	r.gh.mu.Unlock()
	if w := r.workItems()[0]; w.Subject != "Export fails on large files" {
		t.Fatalf("subject before the refetch = %q, want the first change kept", w.Subject)
	}
	r.start()
	r.stop()
	if w := r.workItems()[0]; w.Subject != "Export fails on every file" {
		t.Fatalf("subject after the refetch = %q, want GitHub's copy", w.Subject)
	}
}

func commentText(r *engineRig) string {
	for _, m := range r.messages() {
		if strings.HasPrefix(m.Content.Parts[0].Text, "Still") || strings.HasPrefix(m.Content.Parts[0].Text, "Fixed") {
			return m.Content.Parts[0].Text
		}
	}
	return ""
}

// act queues update u on the only work item under operation op.
func (r *engineRig) act(op model.OperationID, u model.WorkItemUpdate) model.OperationRef {
	r.t.Helper()
	items := r.workItems()
	if len(items) != 1 {
		r.t.Fatalf("work items = %d, want 1", len(items))
	}
	client, err := r.engine.For(model.Principal{Tenant: tenant, Actor: "staff"})
	if err != nil {
		r.t.Fatal(err)
	}
	data, err := json.Marshal(u)
	if err != nil {
		r.t.Fatal(err)
	}
	a, err := client.WorkItems().Act(r.ctx, model.WorkItemAction{
		Command: model.Command{Call: model.Call{Correlation: "act"}, Operation: op, ExpectedRevision: items[0].Revision},
		Target:  items[0].Ref, Action: model.Extension{Name: model.SchemaWorkItemUpdate, Version: 1, Data: data},
	})
	if err != nil {
		r.t.Fatalf("WorkItems.Act: %v", err)
	}
	return a.Operation.Ref
}

// patches returns the issue updates the account received.
func (r *engineRig) patches() []map[string]any {
	r.t.Helper()
	var out []map[string]any
	for _, c := range r.gh.calls("a") {
		if c.Method == http.MethodPatch {
			var b map[string]any
			if err := json.Unmarshal([]byte(c.Body), &b); err != nil {
				r.t.Fatal(err)
			}
			out = append(out, b)
		}
	}
	return out
}

// wantLifecycle requires the only work item to hold status and raw status
// and its conversation to be open.
func (r *engineRig) wantLifecycle(step, status, raw string) {
	r.t.Helper()
	if w := r.workItems()[0]; w.Status != status || w.RawStatus != raw {
		r.t.Fatalf("after %s the work item is %s (%s), want %s (%s)", step, w.Status, w.RawStatus, status, raw)
	}
	convs := r.conversations()
	if len(convs) != 1 || convs[0].Status != model.ConversationOpen {
		r.t.Fatalf("after %s conversations = %+v, want the one still open", step, convs)
	}
}

// An issue closes and reopens through WorkItems.Act on a channel without
// replies, in step with GitHub's own webhooks: the PATCH carries state and
// state_reason, the work item takes GitHub's answer, a person's reopening
// arrives as an ordinary observation, and the conversation stays open
// throughout. No comment is ever created.
func TestEngineIssueCloseReopenLifecycle(t *testing.T) {
	r := newEngineRigWith(t, func(api string) github.Config { return github.Config{APIURL: api} })
	r.deliver("issues", "d-opened", golden(t, "issues-opened"))
	r.gh.setIssue("a", sandboxRepo, 7, 5000001, "open", nil, time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC))
	r.wantLifecycle("the issue opened", model.WorkItemOpen, "open")
	r.start()

	closed, reopened, status := model.WorkItemClosed, model.WorkItemOpen, func(s string) *string { return &s }
	op := r.act("op-close", model.WorkItemUpdate{Status: &closed, StatusReason: status("not_planned")})
	r.later(time.Minute)
	if got := r.operation(op); got.State != model.Delivered {
		t.Fatalf("close operation = %+v", got)
	}
	r.wantLifecycle("a close by command", model.WorkItemClosed, "closed:not_planned")

	// A person reopens the issue: GitHub's webhook, newer than the answer.
	r.deliver("issues", "d-reopened", edited(t, "issues-reopened", func(ev map[string]any) {
		ev["issue"].(map[string]any)["updated_at"] = "2026-10-11T12:01:30Z"
	}))
	r.wantLifecycle("a reopen by webhook", model.WorkItemOpen, "open:reopened")
	if got := r.operation(op); got.State != model.Delivered {
		t.Fatalf("close operation after the webhook = %+v, want it as settled", got)
	}

	op = r.act("op-close-default", model.WorkItemUpdate{Status: &closed})
	r.later(time.Minute)
	if got := r.operation(op); got.State != model.Delivered {
		t.Fatalf("default close operation = %+v", got)
	}
	r.wantLifecycle("a close by command, default reason", model.WorkItemClosed, "closed:completed")

	op = r.act("op-reopen", model.WorkItemUpdate{Status: &reopened})
	r.later(time.Minute)
	if got := r.operation(op); got.State != model.Delivered {
		t.Fatalf("reopen operation = %+v", got)
	}
	r.stop()
	r.wantLifecycle("a reopen by command", model.WorkItemOpen, "open:reopened")

	var sent [][2]any
	for _, b := range r.patches() {
		sent = append(sent, [2]any{b["state"], b["state_reason"]})
	}
	want := [][2]any{{"closed", "not_planned"}, {"closed", "completed"}, {"open", "reopened"}}
	if !slices.Equal(sent, want) {
		t.Fatalf("issue updates = %v, want %v", sent, want)
	}
	if n := len(r.gh.stored("a", sandboxRepo+"#7")); n != 0 {
		t.Fatalf("comments = %d, want none: acts need no replies", n)
	}
	if n := len(r.messages()); n != 1 {
		t.Fatalf("messages = %d, want the description only", n)
	}
}
