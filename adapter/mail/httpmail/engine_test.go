package httpmail_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/httpmail"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

var inbox = model.InboxRef{Tenant: tenant, ID: "00000c00-0000-7000-8000-0000000000a1"}

// engineRig is the real engine over a memory store with http.mail
// registered, the webhook mounted through Runtime.Webhook.
type engineRig struct {
	t     *testing.T
	ctx   context.Context
	store store.Store
	h     http.Handler
	// client reads what the engine holds, attachment bytes included.
	client uma.Client
}

func newEngineRig(t *testing.T, cfg httpmail.Config, destinations ...string) *engineRig {
	t.Helper()
	return newEngineRigBlobs(t, nil, cfg, destinations...)
}

// newEngineRigBlobs is newEngineRig with the host's blob store: without
// one the engine cannot stage attachments.
func newEngineRigBlobs(t *testing.T, blobs ports.BlobStore, cfg httpmail.Config, destinations ...string) *engineRig {
	t.Helper()
	clock := portstest.NewFixedClock(now)
	r := &engineRig{t: t, ctx: t.Context(), store: memory.New(memory.Options{Clock: clock})}
	e, err := uma.New(uma.Config{
		Store: r.store,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{httpmail.NewFactory(httpmail.Options{})},
		Credentials: newCredHost(),
		Blobs:       blobs,
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(0xc0),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	if r.client, err = e.For(model.Principal{Tenant: tenant, Actor: "operator"}); err != nil {
		t.Fatal(err)
	}
	lock := false
	sel := model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1}
	err = r.store.Transact(r.ctx, tenant, func(tx store.Tx) error {
		if err := tx.PutChannel(r.ctx, channelOf(cfg), ""); err != nil {
			return err
		}
		return tx.PutInbox(r.ctx, model.Inbox{Ref: inbox, Channel: chRef, Name: "mail", Destinations: destinations, Selection: sel, Revision: "in1"}, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: tenant, Actor: "webhook-service"}, Call: model.Call{Correlation: "httpmail-test"}}
	if r.h, err = e.Runtime().Webhook(r.ctx, call, chRef); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *engineRig) post(contentType, body string, authed bool) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/hooks/mail", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	if authed {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func (r *engineRig) messages() []model.Message {
	r.t.Helper()
	var out []model.Message
	if err := r.store.View(r.ctx, tenant, func(rd store.Reader) error {
		p, err := rd.Messages(r.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{inbox}}, Page: model.PageRequest{Limit: 100}})
		out = p.Items
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return out
}

func (r *engineRig) identities() []model.Identity {
	r.t.Helper()
	var out []model.Identity
	if err := r.store.View(r.ctx, tenant, func(rd store.Reader) error {
		p, err := rd.Identities(r.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{inbox}}, Page: model.PageRequest{Limit: 100}})
		out = p.Items
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return out
}

func byProviderID(msgs []model.Message, id string) *model.Message {
	ref := model.ProviderMessageRef(chRef, id)
	for i := range msgs {
		if slices.Contains(msgs[i].External, ref) {
			return &msgs[i]
		}
	}
	return nil
}

func jsonMail(id, from, extra, body string) string {
	if extra != "" {
		extra += ","
	}
	return fmt.Sprintf(`{"from":%q,"to":"support@example.com","subject":"Help","body":%q,%s"message_id":%q}`, from, body, extra, id)
}

func TestEngineIngress(t *testing.T) {
	r := newEngineRig(t, baseConfig(), "support")
	post := func(body string) {
		t.Helper()
		if rec := r.post("application/json", body, true); rec.Code != 200 {
			t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
		}
	}
	first := jsonMail("<t1@peers.test>", "Jane <jane@peers.test>", "", "First question")
	post(first)
	post(first) // a redelivery
	post(jsonMail("<t2@peers.test>", "jane@peers.test", `"in_reply_to":"<t1@peers.test>","references":"<t1@peers.test>","claimed_sender":"admin@example.com"`, "Follow-up"))
	msgs := r.messages()
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (redelivery deduplicated)", len(msgs))
	}
	m1, m2 := byProviderID(msgs, "<t1@peers.test>"), byProviderID(msgs, "<t2@peers.test>")
	if m1 == nil || m2 == nil {
		t.Fatalf("messages lack provider Message-ID references: %+v", msgs)
	}
	if m1.Ref.Conversation != m2.Ref.Conversation {
		t.Errorf("reply landed in %v, want the thread's %v", m2.Ref.Conversation, m1.Ref.Conversation)
	}
	if m1.Author.External.ID != "jane@peers.test" || m1.Author.External.Namespace != httpmail.Namespace || m1.Author.Identity != m2.Author.Identity {
		t.Errorf("authors %+v / %+v, want one identity for jane@peers.test", m1.Author, m2.Author)
	}
	if m1.Author.Assurance != httpmail.AssuranceUnverified || m1.Source != model.TypeHttpMail || m1.Content.Parts[0].Text != "First question" {
		t.Errorf("message = %+v", m1)
	}
	if !slices.ContainsFunc(m1.Metadata, func(x model.Extension) bool { return x.Name == httpmail.MessageSchema }) {
		t.Errorf("metadata %+v lacks %s", m1.Metadata, httpmail.MessageSchema)
	}
	ids := r.identities()
	if len(ids) != 1 || ids[0].SourceID != "jane@peers.test" || ids[0].LinkAssurance != model.LinkAssuranceNone {
		t.Errorf("identities = %+v, want jane only, unlinked", ids)
	}
}

// A Message-ID is the sender's to choose. A forged message that reuses the
// id of a genuine one, and arrives first, must not make the engine drop the
// genuine one.
func TestEngineForgedMessageIDDoesNotSuppressTheGenuineMessage(t *testing.T) {
	r := newEngineRig(t, baseConfig(), "support")
	for _, m := range []string{
		jsonMail("<victim@peers.test>", "attacker@peers.test", "", "forged"),
		jsonMail("<victim@peers.test>", "alice@peers.test", "", "the genuine message"),
		jsonMail("<victim@peers.test>", "alice@peers.test", "", "the genuine message"), // its redelivery
	} {
		if rec := r.post("application/json", m, true); rec.Code != 200 {
			t.Fatalf("post: %d", rec.Code)
		}
	}
	var texts []string
	for _, m := range r.messages() {
		texts = append(texts, m.Content.Parts[0].Text)
	}
	slices.Sort(texts)
	if want := []string{"forged", "the genuine message"}; !slices.Equal(texts, want) {
		t.Errorf("stored %q, want %q", texts, want)
	}
}

func TestEngineRefusesWhatIsNotAuthenticated(t *testing.T) {
	r := newEngineRig(t, baseConfig(), "support")
	if rec := r.post("application/json", minimalJSON, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", rec.Code)
	}
	if n := len(r.messages()); n != 0 {
		t.Fatalf("%d messages stored from an unauthenticated request", n)
	}
}

func TestEngineDestinationMustBeBoundToTheInbox(t *testing.T) {
	// sales is a recipient of the channel but bound to no inbox.
	r := newEngineRig(t, baseConfig(), "support")
	rec := r.post("application/json", `{"from":"a@peers.test","to":"sales@example.com","body":"x"}`, true)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", rec.Code)
	}
	if n := len(r.messages()); n != 0 {
		t.Fatalf("%d messages", n)
	}
}

func TestEngineRFC822(t *testing.T) {
	r := newEngineRig(t, rawConfig(), "support")
	msg := "From: Jane <jane@peers.test>\r\nTo: support@example.com\r\nSubject: Hi\r\nMessage-ID: <r1@peers.test>\r\n\r\nraw hello\r\n"
	if rec := r.post("message/rfc822", msg, true); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	m := byProviderID(r.messages(), "<r1@peers.test>")
	if m == nil || m.Content.Parts[0].Text != "raw hello" || m.Author.Assurance != httpmail.AssuranceUnverified {
		t.Fatalf("message %+v", m)
	}
}
