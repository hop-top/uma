package zendesk_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/ticket/zendesk"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

const hostTenant model.TenantID = "zendesk-test"

const (
	hostSub   = "acme"
	hostBrand = "360000001"
	// webhookSecret is the static secret Zendesk documents for test
	// webhooks.
	webhookSecret = "dGhpc19zZWNyZXRfaXNfZm9yX3Rlc3Rpbmdfb25seQ==" // #nosec G101 -- documented public test secret
	apiToken      = "test-api-token"                               // #nosec G101 -- fixture value
)

var (
	hostChannel = model.ChannelRef{Tenant: hostTenant, ID: "00000ad0-0000-7000-8000-0000000000aa"}
	hostInbox   = model.InboxRef{Tenant: hostTenant, ID: "00000ad0-0000-7000-8000-0000000000ab"}
	hostStart   = time.Date(2025, 1, 15, 6, 13, 30, 0, time.UTC)
)

// host is one engine with the adapter registered, a channel bound to a
// per-work-item inbox for brand hostBrand and a fake Zendesk account.
type host struct {
	t       *testing.T
	ctx     context.Context
	fake    *fakeZendesk
	acct    *fakeAccount
	clock   *portstest.FixedClock
	store   store.Store
	engine  uma.Engine
	client  uma.Client
	waiter  *idle
	webhook http.Handler
	run     chan error
}

// credentials returns the API token and the webhook signing secret.
type credentials struct{}

func (credentials) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	switch r.Purpose {
	case zendesk.CredentialPurposeAPI:
		return ports.Secret{Bytes: []byte(apiToken)}, nil
	case zendesk.CredentialPurposeWebhook:
		return ports.Secret{Bytes: []byte(webhookSecret)}, nil
	}
	return nil, model.NewError(model.Unauthorized, "unknown purpose")
}

// idle reports each idle worker wait and blocks it until resumed or the
// run ends.
type idle struct {
	ch     chan struct{}
	resume chan struct{}
}

func (w *idle) Wait(ctx context.Context, _ time.Duration) error {
	select {
	case w.ch <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.resume:
		return nil
	}
}

func newHost(t *testing.T) *host {
	t.Helper()
	h := &host{t: t, ctx: t.Context(), fake: newFakeZendesk(t), clock: portstest.NewFixedClock(hostStart), waiter: &idle{ch: make(chan struct{}, 16), resume: make(chan struct{})}}
	h.fake.base, h.fake.brand = hostStart.Add(-30*time.Minute).Unix(), hostBrand
	h.acct = h.fake.account(hostSub, hostChannel)
	h.store = memory.New(memory.Options{Clock: h.clock})
	f := h.fake.factory()
	engine, err := uma.New(uma.Config{
		Store: h.store,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters: []adapter.Factory{f}, Extensions: f.Extensions(),
		Credentials: credentials{}, Clock: h.clock, IDs: portstest.NewSequenceIDs(0xbee), Waiter: h.waiter,
	})
	if err != nil {
		t.Fatalf("uma.New: %v", err)
	}
	h.engine = engine
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	if h.client, err = engine.For(model.Principal{Tenant: hostTenant, Actor: "agent"}); err != nil {
		t.Fatal(err)
	}
	cfg := zendesk.Config{Subdomain: hostSub, Auth: zendesk.AuthAPIToken, Email: "agent@acme.example", WebhookToleranceSeconds: 300}
	if err := h.store.Transact(h.ctx, hostTenant, func(tx store.Tx) error {
		if err := tx.PutChannel(h.ctx, model.Channel{Ref: hostChannel, Type: zendesk.Type, Configuration: cfg.Extension(), CredentialRef: "zendesk", Revision: "ch1"}, ""); err != nil {
			return err
		}
		return tx.PutInbox(h.ctx, model.Inbox{
			Ref: hostInbox, Channel: hostChannel, Name: "support", Destinations: []string{hostBrand},
			Selection: model.Selection{Mode: model.ModePerWorkItem}, Revision: "in1",
		}, "")
	}); err != nil {
		t.Fatal(err)
	}
	if h.webhook, err = engine.Runtime().Webhook(h.ctx, service(), hostChannel); err != nil {
		t.Fatalf("Runtime.Webhook: %v", err)
	}
	return h
}

func service() uma.RuntimeCall {
	return uma.RuntimeCall{Principal: model.Principal{Tenant: hostTenant, Actor: "zendesk-service"}, Call: model.Call{Correlation: "zendesk-test"}}
}

// post signs body under secret at the host clock and serves it.
func (h *host) post(body []byte, secret string) int {
	h.t.Helper()
	return h.postAt(body, secret, h.clock.Now())
}

func (h *host) postAt(body []byte, secret string, at time.Time) int {
	h.t.Helper()
	stamp := at.UTC().Format(time.RFC3339)
	req := httptest.NewRequestWithContext(h.ctx, http.MethodPost, "/zendesk", bytes.NewReader(body))
	req.Header.Set(zendesk.SignatureHeader, signature(secret, stamp, body))
	req.Header.Set(zendesk.SignatureTimestampHeader, stamp)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.webhook.ServeHTTP(rec, req)
	return rec.Code
}

// receiveOnce runs the channel's receiver until the fake has served every
// queued event and seen it read past or gone quiet.
func (h *host) receiveOnce() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.engine.Runtime().Receive(ctx, service(), hostChannel)
	}()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		h.fake.mu.Lock()
		want := len(h.acct.events)
		h.fake.mu.Unlock()
		if len(h.fake.inbound(h.acct)) >= want {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatal("receiver did not read every queued event")
		}
		time.Sleep(time.Millisecond)
	}
}

// signature is the documented webhook signature of body at stamp.
func signature(secret, stamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(stamp))
	m.Write(body)
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// work lets the worker run until it has nothing claimable: the first call
// starts it, later ones resume its idle wait.
func (h *host) work() {
	h.t.Helper()
	if h.run == nil {
		h.run = make(chan error, 1)
		go func() {
			h.run <- h.engine.Runtime().Run(h.ctx, service(), uma.WorkerOptions{Owner: "zendesk-worker", Limit: 10})
		}()
		h.t.Cleanup(func() {
			_ = h.engine.Runtime().Drain(context.Background(), service())
			<-h.run
		})
	} else {
		h.waiter.resume <- struct{}{}
	}
	select {
	case <-h.waiter.ch:
	case err := <-h.run:
		h.t.Fatalf("Runtime.Run returned early: %v", err)
	case <-time.After(10 * time.Second):
		h.t.Fatal("worker did not go idle")
	}
}

func (h *host) messages() []model.Message {
	h.t.Helper()
	var out []model.Message
	h.view(func(r store.Reader) error {
		p, err := r.Messages(h.ctx, model.ListRequest{Page: model.PageRequest{Limit: 200}, Filter: model.Filter{Inboxes: []model.InboxRef{hostInbox}}})
		out = p.Items
		return err
	})
	return out
}

func (h *host) conversations() []model.Conversation {
	h.t.Helper()
	var out []model.Conversation
	h.view(func(r store.Reader) error {
		p, err := r.Conversations(h.ctx, model.ListRequest{Page: model.PageRequest{Limit: 200}, Filter: model.Filter{Inboxes: []model.InboxRef{hostInbox}}})
		out = p.Items
		return err
	})
	return out
}

func (h *host) workItems() []model.WorkItem {
	h.t.Helper()
	var out []model.WorkItem
	h.view(func(r store.Reader) error {
		p, err := r.WorkItems(h.ctx, model.ListRequest{Page: model.PageRequest{Limit: 200}, Filter: model.Filter{Inboxes: []model.InboxRef{hostInbox}}})
		out = p.Items
		return err
	})
	return out
}

func (h *host) view(fn func(store.Reader) error) {
	h.t.Helper()
	if err := h.store.View(h.ctx, hostTenant, fn); err != nil {
		h.t.Fatalf("View: %v", err)
	}
}

func (h *host) op(ref model.OperationRef) model.Operation {
	h.t.Helper()
	op, err := h.client.Operations().Get(h.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "get"}, Ref: ref})
	if err != nil {
		h.t.Fatalf("Operations.Get: %v", err)
	}
	return op
}

// conversation returns the conversation of a ticket, created by its first
// inbound comment.
func (h *host) conversation(ticket string) model.Conversation {
	h.t.Helper()
	for _, c := range h.conversations() {
		if c.ReplyRoute == route(hostChannel, ticket) {
			return c
		}
	}
	h.t.Fatalf("no conversation for ticket %s", ticket)
	return model.Conversation{}
}

func (h *host) reply(conv model.ConversationRef, op model.OperationID, text string) model.OperationRef {
	h.t.Helper()
	a, err := h.client.Messages().Reply(h.ctx, model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: "c-" + model.CorrelationID(op)}, Operation: op},
		Conversation: conv, Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
	})
	if err != nil {
		h.t.Fatalf("Reply: %v", err)
	}
	return a.Operation.Ref
}

func (h *host) execute(conv model.ConversationRef, op model.OperationID, aud model.Audience, action model.Extension) (model.Acceptance, error) {
	return h.client.Extensions().Execute(h.ctx, model.ExecuteExtension{
		Command: model.Command{Call: model.Call{Correlation: "c-" + model.CorrelationID(op)}, Operation: op},
		Target:  conv.Ref(), Audience: aud, Action: action,
	})
}

// eventSeq orders rendered events in time, as Zendesk's are.
var eventSeq atomic.Int64

// event renders a Zendesk ticket event as documented (version
// 2022-11-06), with the detail object of ticket id at brand hostBrand.
func event(t *testing.T, id, typ, ticket, status string, ev any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"account_id": 18501212,
		"detail": map[string]any{
			"actor_id": "8659716080510", "assignee_id": "7489421176190", "brand_id": hostBrand,
			"created_at": "2025-01-15T06:13:26Z", "custom_status": "7489421192190", "description": "Printer on fire",
			"external_id": nil, "form_id": "7489429056254", "group_id": "7489429060734", "id": ticket,
			"is_public": true, "organization_id": nil, "priority": nil, "requester_id": "8659716080510",
			"status": status, "subject": "Printer on fire", "submitter_id": "8659716080510", "tags": []string{"printer"},
			"type": nil, "updated_at": "2025-01-15T06:13:26Z", "via": map[string]any{"channel": "web"},
		},
		"event": ev,
		"id":    id, "subject": "zen:ticket:" + ticket,
		"time": hostStart.Add(-4*time.Second + time.Duration(eventSeq.Add(1))*time.Millisecond).Format(time.RFC3339Nano),
		"type": "zen:event-type:ticket." + typ, "zendesk_event_version": "2022-11-06",
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// commentEvent is a ticket.comment_added event; public nil omits is_public.
func commentEvent(t *testing.T, id, ticket, commentID, authorID string, staff bool, public *bool, body string) []byte {
	c := map[string]any{
		"author": map[string]any{"id": authorID, "is_staff": staff, "name": "Visitor " + authorID},
		"body":   body, "html_body": "<p>" + body + "</p>", "id": commentID,
	}
	if public != nil {
		c["is_public"] = *public
	}
	return event(t, id, "comment_added", ticket, "OPEN", map[string]any{
		"comment": c, "meta": map[string]any{"sequence": map[string]any{"id": 1, "position": 1}},
	})
}

func ptr[T any](v T) *T { return &v }
