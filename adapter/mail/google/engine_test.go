package google

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
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

var (
	engineInboxA = model.InboxRef{Tenant: testChan.Tenant, ID: "00000c00-0000-7000-8000-0000000000a1"}
	engineInboxB = model.InboxRef{Tenant: testChan.Tenant, ID: "00000c00-0000-7000-8000-0000000000b1"}
	engineChanB  = model.ChannelRef{Tenant: testChan.Tenant, ID: "00000000-0000-7000-8000-0000000000c2"}
)

// engineRig is the real engine over a memory store with google.mail
// registered against the fake Gmail API.
type engineRig struct {
	t      *testing.T
	ctx    context.Context
	g      *fakeGmail
	store  store.Store
	engine uma.Engine
}

var engineCall = uma.RuntimeCall{Principal: model.Principal{Tenant: testChan.Tenant, Actor: "gmail-service"}, Call: model.Call{Correlation: "gmail-test"}}

func newEngineRig(t *testing.T) *engineRig { return newEngineRigBlobs(t, nil) }

// newEngineRigBlobs is newEngineRig with the host's blob store; without one
// the engine cannot stage attachments.
func newEngineRigBlobs(t *testing.T, blobs ports.BlobStore) *engineRig {
	t.Helper()
	return newEngineRigLimit(t, blobs, 0)
}

// newEngineRigLimit is newEngineRigBlobs with the raw message response
// bound lowered to rawLimit bytes; zero keeps the adapter's.
func newEngineRigLimit(t *testing.T, blobs ports.BlobStore, rawLimit int64) *engineRig {
	t.Helper()
	return newEngineRigConfig(t, blobs, rawLimit, nil)
}

// newEngineRigObserved is newEngineRig with the host's observer and a
// receiver backoff of a millisecond, so a reconnection is quick.
func newEngineRigObserved(t *testing.T, obs ports.Observer) *engineRig {
	t.Helper()
	return newEngineRigConfig(t, nil, 0, func(c *uma.Config) {
		c.Observer = obs
		c.Retry = uma.RetryPolicy{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	})
}

// newEngineRigConfig is newEngineRigLimit with edit, when set, applied to
// the engine configuration.
func newEngineRigConfig(t *testing.T, blobs ports.BlobStore, rawLimit int64, edit func(*uma.Config)) *engineRig {
	t.Helper()
	clock := portstest.NewFixedClock(testNow)
	r := &engineRig{t: t, ctx: t.Context(), g: newFakeGmail(t), store: memory.New(memory.Options{Clock: clock})}
	r.g.now = clock.Now
	f := NewFactory(Options{Endpoint: r.g.endpoint(), CertsURL: r.g.certsURL(), PageSize: 10, rawLimit: rawLimit})
	cfg := uma.Config{
		Store: r.store,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{f},
		Extensions:  f.Extensions(),
		Credentials: &testCreds{},
		HTTP:        r.g.client(),
		Blobs:       blobs,
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(0x9a),
	}
	if edit != nil {
		edit(&cfg)
	}
	e, err := uma.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	r.engine = e
	return r
}

func (r *engineRig) seed(ch model.ChannelRef, in model.InboxRef, user string, destinations ...string) {
	r.t.Helper()
	r.g.box(user, ch)
	data, _ := json.Marshal(Config{User: user, Addresses: []string{"support@example.test", "sales@example.test"},
		Topic: testTopic, PushAudience: testAud, PushServiceAccount: testPushSA})
	lock := false
	sel := model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1}
	err := r.store.Transact(r.ctx, ch.Tenant, func(tx store.Tx) error {
		if err := tx.PutChannel(r.ctx, model.Channel{Ref: ch, Type: Type, CredentialRef: "cred", Revision: "ch1",
			Configuration: model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data}}, ""); err != nil {
			return err
		}
		return tx.PutInbox(r.ctx, model.Inbox{Ref: in, Channel: ch, Name: "gmail", Destinations: destinations, Selection: sel, Revision: "in1"}, "")
	})
	if err != nil {
		r.t.Fatal(err)
	}
}

// receive runs Runtime.Receive for ch until the test ends and waits for
// its first round. The channel it returns receives what Receive returned,
// should it return before the test ends.
func (r *engineRig) receive(ch model.ChannelRef, user string) <-chan error {
	r.t.Helper()
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan struct{})
	ended := make(chan error, 1)
	go func() {
		defer close(done)
		ended <- r.engine.Runtime().Receive(ctx, engineCall, ch)
	}()
	r.t.Cleanup(func() { cancel(); <-done })
	waitFor(r.t, func() bool { return r.g.count("GET /gmail/v1/users/"+user+"/messages?") > 0 })
	// The first round ends by storing the cursor; a push request before
	// that finds no history to read.
	waitFor(r.t, func() bool { return r.storedHistoryID(ch) != 0 })
	return ended
}

// storedHistoryID is the receiver's stored cursor for ch, zero when none.
func (r *engineRig) storedHistoryID(ch model.ChannelRef) int64s {
	r.t.Helper()
	var st receiverState
	_ = r.store.View(r.ctx, ch.Tenant, func(rd store.Reader) error {
		p, err := rd.Protocol(r.ctx, ch, protocolKey)
		if err == nil {
			_ = json.Unmarshal(p.Value.Data, &st)
		}
		return nil
	})
	return st.HistoryID
}

// push posts a signed Gmail notification to ch's webhook.
func (r *engineRig) push(ch model.ChannelRef, user string) int {
	r.t.Helper()
	h, err := r.engine.Runtime().Webhook(r.ctx, engineCall, ch)
	if err != nil {
		r.t.Fatal(err)
	}
	r.g.mu.Lock()
	hid := r.g.boxes[user].historyID
	r.g.mu.Unlock()
	tok := signToken(r.t, r.g.key, "test-key", claims{
		"iss": "https://accounts.google.com", "aud": testAud, "email": testPushSA, "email_verified": true,
		"iat": testNow.Add(-time.Minute).Unix(), "exp": testNow.Add(time.Hour).Unix(),
	})
	req := httptest.NewRequestWithContext(r.ctx, http.MethodPost, "/gmail", strings.NewReader(pushBody(user, hid)))
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}

func (r *engineRig) messages(in model.InboxRef) []model.Message {
	r.t.Helper()
	var out []model.Message
	if err := r.store.View(r.ctx, in.Tenant, func(rd store.Reader) error {
		p, err := rd.Messages(r.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{in}}, Page: model.PageRequest{Limit: 100}})
		out = p.Items
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return out
}

func (r *engineRig) identities(in model.InboxRef) []model.Identity {
	r.t.Helper()
	var out []model.Identity
	if err := r.store.View(r.ctx, in.Tenant, func(rd store.Reader) error {
		p, err := rd.Identities(r.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{in}}, Page: model.PageRequest{Limit: 100}})
		out = p.Items
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return out
}

func byProviderID(msgs []model.Message, ch model.ChannelRef, id string) *model.Message {
	ref := model.ProviderMessageRef(ch, id)
	for i := range msgs {
		if slices.Contains(msgs[i].External, ref) {
			return &msgs[i]
		}
	}
	return nil
}

func (r *engineRig) waitMessages(in model.InboxRef, n int) []model.Message {
	r.t.Helper()
	waitFor(r.t, func() bool { return len(r.messages(in)) >= n })
	return r.messages(in)
}

// TestEngineIngress covers, over the real engine, what the suite's receive
// cases cannot express for mail: normalization into the bound inbox with
// stable provider references, deduplicated redelivery, header threading and
// no identity from display names or other headers.
func TestEngineIngress(t *testing.T) {
	r := newEngineRig(t)
	r.seed(testChan, engineInboxA, testMailbox, "support@example.test")
	r.receive(testChan, testMailbox)

	r.g.deliver(testMailbox, "m1", "thread-1", rawMessage(`"Admin (verified)" <jane@example.org>`, "support@example.test", "<t1@example.org>", "Help", "First question"))
	if code := r.push(testChan, testMailbox); code != http.StatusNoContent {
		t.Fatalf("push = %d", code)
	}
	r.waitMessages(engineInboxA, 1)
	// Redelivery of the same message, then a reply threading on it.
	r.g.deliver(testMailbox, "m1", "thread-1", rawMessage("jane@example.org", "support@example.test", "<t1@example.org>", "Help", "First question"))
	r.g.deliver(testMailbox, "m2", "thread-1", rawMessage("jane@example.org", "support@example.test", "<t2@example.org>", "Re: Help", "Follow-up",
		"In-Reply-To: <t1@example.org>", "References: <t1@example.org>", "X-Claimed-Sender: admin@example.test"))
	if code := r.push(testChan, testMailbox); code != http.StatusNoContent {
		t.Fatalf("push = %d", code)
	}
	msgs := r.waitMessages(engineInboxA, 2)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (redelivery deduplicated)", len(msgs))
	}
	m1, m2 := byProviderID(msgs, testChan, "m1"), byProviderID(msgs, testChan, "m2")
	if m1 == nil || m2 == nil {
		t.Fatalf("messages lack Gmail message ID references: %+v", msgs)
	}
	if m1.Ref.Conversation != m2.Ref.Conversation {
		t.Errorf("reply landed in %v, want the thread's %v", m2.Ref.Conversation, m1.Ref.Conversation)
	}
	if m1.Author.External.ID != "jane@example.org" || m1.Author.External.Namespace != IdentityNamespace || m1.Author.Identity != m2.Author.Identity {
		t.Errorf("authors %+v / %+v, want one identity for jane@example.org", m1.Author, m2.Author)
	}
	if m1.Source != Type || m1.Content.Parts[0].Text != "First question" || m1.Audience.Visibility != model.Public {
		t.Errorf("message = %+v", m1)
	}
	if !slices.ContainsFunc(m1.Metadata, func(x model.Extension) bool { return x.Name == PayloadMessage }) {
		t.Errorf("metadata %+v lacks %s", m1.Metadata, PayloadMessage)
	}
	ids := r.identities(engineInboxA)
	if len(ids) != 1 || ids[0].SourceID != "jane@example.org" || ids[0].LinkAssurance != model.LinkAssuranceNone {
		t.Errorf("identities = %+v, want jane only, unlinked", ids)
	}

	// Same Gmail thread, no header match: threadId is a hint, never
	// identity, so this is a new conversation.
	r.g.deliver(testMailbox, "m3", "thread-1", rawMessage("jane@example.org", "support@example.test", "<t3@example.org>", "Re: Help", "Other topic"))
	if code := r.push(testChan, testMailbox); code != http.StatusNoContent {
		t.Fatalf("push = %d", code)
	}
	m3 := byProviderID(r.waitMessages(engineInboxA, 3), testChan, "m3")
	if m3 == nil || m3.Ref.Conversation == m1.Ref.Conversation {
		t.Errorf("unthreaded mail joined %v through threadId", m1.Ref.Conversation)
	}
}

func TestEngineBoundInboxOnly(t *testing.T) {
	r := newEngineRig(t)
	const userB = "other@example.test"
	r.seed(testChan, engineInboxA, testMailbox, "support@example.test")
	r.seed(engineChanB, engineInboxB, userB, "support@example.test", "sales@example.test")
	r.receive(testChan, testMailbox)
	r.receive(engineChanB, userB)
	raw := rawMessage("jane@example.org", "support@example.test", "<b1@example.org>", "Hi", "to both accounts")
	r.g.deliver(testMailbox, "x1", "t", raw)
	r.g.deliver(userB, "x1", "t", raw)
	if r.push(testChan, testMailbox) != http.StatusNoContent || r.push(engineChanB, userB) != http.StatusNoContent {
		t.Fatal("push refused")
	}
	r.waitMessages(engineInboxA, 1)
	r.waitMessages(engineInboxB, 1)
	// A notification for B's mailbox posted to A's webhook is ignored.
	r.g.deliver(testMailbox, "x2", "t", rawMessage("jane@example.org", "support@example.test", "<b2@example.org>", "Hi", "later"))
	before := r.g.count("GET /gmail/v1/users/" + testMailbox + "/history")
	if code := r.push(testChan, userB); code != http.StatusNoContent {
		t.Fatalf("push = %d", code)
	}
	if r.g.count("GET /gmail/v1/users/"+testMailbox+"/history") != before || len(r.messages(engineInboxA)) != 1 {
		t.Fatal("channel A acted on another mailbox's notification")
	}
}
