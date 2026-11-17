package whatsapp_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/meta"
	"hop.top/uma/adapter/messaging/meta/whatsapp"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// engineRig is the adapter registered in a full engine over a memory
// store: the webhook mounted through Runtime.Webhook, one per-contact inbox
// bound to the test phone number, and a worker paced by the test.
type engineRig struct {
	t      *testing.T
	f      *fakeMeta
	clock  *portstest.FixedClock
	st     store.Store
	engine uma.Engine
	client uma.Client
	hook   http.Handler
	pacer  *pacer
	inbox  model.InboxRef
	run    chan error
}

func newEngineRig(t *testing.T, opts ...func(*uma.Config)) *engineRig {
	t.Helper()
	r := &engineRig{
		t: t, f: fakeWithAccount(t), clock: portstest.NewFixedClock(testNow),
		pacer: &pacer{idle: make(chan struct{}, 16), resume: make(chan struct{})},
		inbox: model.InboxRef{Tenant: testTenant, ID: "00000000-0000-7000-8000-00000000b0e1"},
	}
	r.st = memory.New(memory.Options{Clock: r.clock})
	cfg := uma.Config{
		Store: r.st,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{r.f.factory()},
		Credentials: newCreds(),
		HTTP:        r.f.client(),
		Clock:       r.clock,
		IDs:         portstest.NewSequenceIDs(0x6e),
		Waiter:      r.pacer,
	}
	for _, o := range opts {
		o(&cfg)
	}
	engine, err := uma.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.engine = engine
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	lock := true
	sel := model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1}
	if err := r.st.Transact(t.Context(), testTenant, func(tx store.Tx) error {
		if err := tx.PutChannel(t.Context(), channel(testPNID), ""); err != nil {
			return err
		}
		return tx.PutInbox(t.Context(), model.Inbox{Ref: r.inbox, Channel: testChannel, Name: "support", Destinations: []string{testPNID}, Selection: sel, Revision: "in1"}, "")
	}); err != nil {
		t.Fatal(err)
	}
	if r.client, err = engine.For(model.Principal{Tenant: testTenant, Actor: "agent"}); err != nil {
		t.Fatal(err)
	}
	if r.hook, err = engine.Runtime().Webhook(t.Context(), r.call(), testChannel); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *engineRig) call() uma.RuntimeCall {
	return uma.RuntimeCall{Principal: model.Principal{Tenant: testTenant, Actor: "service"}, Call: model.Call{Correlation: "rig"}}
}

// notify posts a signed notification carrying value and requires 200.
func (r *engineRig) notify(value string) {
	r.t.Helper()
	body := notificationOf(testPNID, value)
	if code := post(r.hook, body, meta.Sign(appSecret, body)); code != http.StatusOK {
		r.t.Fatalf("webhook POST = %d", code)
	}
}

// conversation returns the inbox's only conversation.
func (r *engineRig) conversation() model.Conversation {
	r.t.Helper()
	var convs []model.Conversation
	if err := r.st.View(r.t.Context(), testTenant, func(rd store.Reader) error {
		p, err := rd.Conversations(r.t.Context(), model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{r.inbox}}, Page: model.PageRequest{Limit: 10}})
		convs = p.Items
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	if len(convs) != 1 {
		r.t.Fatalf("conversations = %d, want 1", len(convs))
	}
	return convs[0]
}

func (r *engineRig) reply(conv model.ConversationRef, op model.OperationID, c model.Content) model.OperationRef {
	r.t.Helper()
	a, err := r.client.Messages().Reply(r.t.Context(), model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: model.CorrelationID("c-" + op)}, Operation: op},
		Conversation: conv, Content: c,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return a.Operation.Ref
}

// work runs the rig's worker until it has nothing left to claim: the first
// call starts it, later calls wake it for one more claim round.
func (r *engineRig) work() {
	r.t.Helper()
	if r.run == nil {
		r.run = make(chan error, 1)
		go func() {
			r.run <- r.engine.Runtime().Run(r.t.Context(), r.call(), uma.WorkerOptions{Owner: "w", Limit: 10})
		}()
		r.t.Cleanup(func() {
			_ = r.engine.Runtime().Drain(context.Background(), r.call())
			<-r.run
		})
	} else {
		r.pacer.resume <- struct{}{}
	}
	awaitIdle(r.t, r.pacer, r.run)
}

func (r *engineRig) op(ref model.OperationRef) model.Operation {
	r.t.Helper()
	op, err := r.client.Operations().Get(r.t.Context(), model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "get"}, Ref: ref})
	if err != nil {
		r.t.Fatal(err)
	}
	return op
}

// The engine gives the instance protocol state, so the window a verified
// inbound message opens is live end to end: a free-form reply inside it is
// sent, one after it closed is rejected without a provider call.
func TestServiceWindowLiveThroughEngine(t *testing.T) {
	r := newEngineRig(t)
	r.notify(textMessage("wamid.in-1", testUser, "hello", testNow.Unix()))
	conv := r.conversation().Ref

	inside := r.reply(conv, "op-inside", textContent("Happy to help."))
	r.work()
	if op := r.op(inside); op.State != model.ProviderAccepted {
		t.Fatalf("reply inside the window: %+v, want provider_accepted", op)
	}
	sends := len(r.f.recordedRequests())

	r.clock.Advance(windowPassed)
	outside := r.reply(conv, "op-outside", textContent("Still there?"))
	r.work()
	op := r.op(outside)
	if op.State != model.Rejected || op.Failure == nil || !strings.Contains(op.Failure.SafeMessage, "template") {
		t.Fatalf("reply after the window closed: %+v, want rejected asking for a template", op)
	}
	if got := len(r.f.recordedRequests()); got != sends {
		t.Fatalf("provider requests %d after the window closed, want %d: no call", got, sends)
	}
}

// windowPassed moves past the customer service window.
const windowPassed = 24*time.Hour + time.Minute

// messages returns the inbox's messages.
func (r *engineRig) messages() []model.Message {
	r.t.Helper()
	var msgs []model.Message
	if err := r.st.View(r.t.Context(), testTenant, func(rd store.Reader) error {
		p, err := rd.Messages(r.t.Context(), model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{r.inbox}}, Page: model.PageRequest{Limit: 50}})
		msgs = p.Items
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return msgs
}

func withBlobs(c *uma.Config) { c.Blobs = &portstest.MemoryBlobs{} }

func imageMessage(id, from, mediaID, caption string) string {
	return fmt.Sprintf(`"contacts":[{"profile":{"name":"Sheena Nelson"},"wa_id":%q}],"messages":[{"from":%q,"id":%q,"timestamp":"%d","type":"image","image":{"caption":%q,"mime_type":"image/jpeg","id":%q}}]`,
		from, from, id, testNow.Unix(), caption, mediaID)
}

// Inbound media is downloaded with the access token and staged through
// the engine's sink: the message carries the caption, the media metadata
// and the staged attachment, whose bytes the Attachments service serves.
func TestInboundMediaStagedThroughEngine(t *testing.T) {
	r := newEngineRig(t, withBlobs)
	jpeg := []byte("\xff\xd8\xff\xe0 not really a jpeg")
	r.f.putMedia("1003383421387256", jpeg)
	r.notify(imageMessage("wamid.img-1", testUser, "1003383421387256", "the receipt"))
	msgs := r.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	parts := msgs[0].Content.Parts
	if len(parts) != 3 || parts[0].Text != "the receipt" || parts[1].Structured == nil || parts[1].Structured.Name != whatsapp.MediaSchema || parts[2].Attachment == nil {
		t.Fatalf("parts %+v, want caption, media metadata and the attachment", parts)
	}
	att, rc, err := r.client.Attachments().Open(t.Context(), model.Lookup[model.AttachmentRef]{Call: model.Call{Correlation: "open"}, Ref: *parts[2].Attachment})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil || !bytes.Equal(got, jpeg) || att.Metadata.MediaType != "image/jpeg" || att.Metadata.Audience.Visibility != model.Public {
		t.Fatalf("attachment %+v bytes %q, %v", att, got, err)
	}
	for _, req := range r.f.recordedRequests() {
		if strings.Contains(req.Path, "1003383421387256") && req.Auth != "Bearer EAAG-access-token" {
			t.Fatalf("media request %s authorized %q", req.Path, req.Auth)
		}
	}
	// A redelivery restages to the same attachment.
	r.notify(imageMessage("wamid.img-1", testUser, "1003383421387256", "the receipt"))
	if n := len(r.messages()); n != 1 {
		t.Fatalf("messages = %d after a redelivery, want 1", n)
	}
}

// The access token never reaches a download URL outside the Graph origin
// and Meta's media host: the message is kept with its media metadata only.
func TestMediaDownloadStaysOnMetaHosts(t *testing.T) {
	r := newEngineRig(t, withBlobs)
	r.f.putMedia("2003383421387256", []byte("bytes"))
	r.f.mediaURL = "https://media.attacker.example/2003383421387256"
	r.notify(imageMessage("wamid.img-2", testUser, "2003383421387256", "look"))
	msgs := r.messages()
	if len(msgs) != 1 || len(msgs[0].Content.Parts) != 2 || msgs[0].Content.Parts[1].Structured == nil {
		t.Fatalf("messages %+v, want one with caption and media metadata only", msgs)
	}
}

// Without a blob store the message keeps its media metadata only.
func TestInboundMediaWithoutBlobStore(t *testing.T) {
	r := newEngineRig(t)
	r.f.putMedia("3003383421387256", []byte("bytes"))
	r.notify(imageMessage("wamid.img-3", testUser, "3003383421387256", "look"))
	if msgs := r.messages(); len(msgs) != 1 || len(msgs[0].Content.Parts) != 2 {
		t.Fatalf("messages %+v, want one without an attachment part", msgs)
	}
}

// A send whose answer was lost stays outcome_unknown until a status names
// its attempt: delivered settles it delivered, failed settles it failed,
// and a status naming no attempt settles nothing. Nothing is resent.
func TestStatusSettlesOutcomeUnknownThroughEngine(t *testing.T) {
	for name, c := range map[string]struct {
		status string
		tagged bool
		want   model.DeliveryState
	}{
		"delivered":     {deliveredStatus, true, model.Delivered},
		"sent":          {sentStatus, true, model.ProviderAccepted},
		"failed":        {failedStatus, true, model.Failed},
		"untagged read": {readStatus, false, model.Unknown},
	} {
		t.Run(name, func(t *testing.T) {
			r := newEngineRig(t)
			r.notify(textMessage("wamid.in-1", testUser, "hello", testNow.Unix()))
			conv := r.conversation().Ref
			if err := (provider{f: r.f}).Respond(testChannel, conformance.Response{Outcome: outcomeServerFault}); err != nil {
				t.Fatal(err)
			}
			ref := r.reply(conv, "op-lost", textContent("On its way."))
			r.work()
			if op := r.op(ref); op.State != model.Unknown {
				t.Fatalf("after a server fault: %+v, want outcome_unknown", op)
			}
			sends := r.f.recordedRequests()
			cb, _ := sends[len(sends)-1].Body["biz_opaque_callback_data"].(string)
			if !c.tagged {
				cb = ""
			}
			status := withCallback(regexp.MustCompile(`"biz_opaque_callback_data":"[^"]*",`).ReplaceAllString(c.status, ""), cb)
			r.notify(statusValue(status))
			r.work()
			op := r.op(ref)
			if op.State != c.want {
				t.Fatalf("after the status: %+v, want %s", op, c.want)
			}
			if c.want == model.Failed && (op.Failure == nil || op.Failure.Retry != model.NoRetry) {
				t.Fatalf("failure %+v, want final", op.Failure)
			}
			r.clock.Advance(time.Hour)
			r.work()
			if got := len(r.f.recordedRequests()); got != len(sends) {
				t.Fatalf("provider requests %d, want %d: never resent", got, len(sends))
			}
		})
	}
}

// Meta reports what became of a send the engine already settled
// provider_accepted: delivered (and read) move it to delivered, a failure
// to failed, and a late sent after either changes nothing. Meta's read
// notification carries no read time the adapter reports, so a read leaves
// the operation delivered.
func TestStatusAdvancesAnAcceptedSendThroughEngine(t *testing.T) {
	for name, c := range map[string]struct {
		statuses []string
		want     model.DeliveryState
	}{
		"delivered":                {[]string{deliveredStatus}, model.Delivered},
		"read":                     {[]string{readStatus}, model.Delivered},
		"sent, delivered, sent":    {[]string{sentStatus, deliveredStatus, sentStatus}, model.Delivered},
		"failed":                   {[]string{failedStatus}, model.Failed},
		"delivered, then failed":   {[]string{deliveredStatus, failedStatus}, model.Delivered},
		"sent changes nothing":     {[]string{sentStatus}, model.ProviderAccepted},
		"failed then delivered":    {[]string{failedStatus, deliveredStatus}, model.Delivered},
		"delivered, read, repeats": {[]string{deliveredStatus, readStatus, deliveredStatus, readStatus}, model.Delivered},
	} {
		t.Run(name, func(t *testing.T) {
			r := newEngineRig(t)
			r.notify(textMessage("wamid.in-1", testUser, "hello", testNow.Unix()))
			ref := r.reply(r.conversation().Ref, "op-ok", textContent("On its way."))
			r.work()
			if op := r.op(ref); op.State != model.ProviderAccepted {
				t.Fatalf("after the send: %+v, want provider_accepted", op)
			}
			sends := r.f.recordedRequests()
			cb, _ := sends[len(sends)-1].Body["biz_opaque_callback_data"].(string)
			for _, st := range c.statuses {
				stripped := regexp.MustCompile(`"biz_opaque_callback_data":"[^"]*",`).ReplaceAllString(st, "")
				r.notify(statusValue(withCallback(stripped, cb)))
				r.work()
			}
			op := r.op(ref)
			if op.State != c.want {
				t.Fatalf("after the statuses: %+v, want %s", op, c.want)
			}
			if c.want == model.Failed && (op.Failure == nil || op.Failure.Retry != model.NoRetry) {
				t.Fatalf("failure %+v, want final", op.Failure)
			}
			if c.want != model.Failed && op.Failure != nil {
				t.Fatalf("failure %+v on a %s send", op.Failure, c.want)
			}
			r.clock.Advance(time.Hour)
			r.work()
			if got := len(r.f.recordedRequests()); got != len(sends) {
				t.Fatalf("provider requests %d, want %d: never resent", got, len(sends))
			}
		})
	}
}
