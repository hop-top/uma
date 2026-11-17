package sms_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio/sms"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

const (
	mmsSID    = "MM10000000000000000000000000000002"
	mediaOne  = "ME10000000000000000000000000000001"
	mediaTwo  = "ME10000000000000000000000000000002"
	mediaBase = "/2010-04-01/Accounts/" + hookAccount + "/Messages/" + mmsSID + "/Media/"
)

// mmsHost is an engine with a blob store and the adapter on the fake
// Twilio API, behind its webhook.
type mmsHost struct {
	*hookHost
	fake   *fakeTwilio
	client uma.Client
}

func newMMSHost(t *testing.T) *mmsHost {
	t.Helper()
	fake := newFakeTwilio(t)
	h := newMMSHostOn(t, fake.srv.URL, fake.client(), 5*time.Second)
	h.fake = fake
	return h
}

// newMMSHostOn is newMMSHost with the Twilio API at base, reached through
// client, each call bounded by timeout.
func newMMSHostOn(t *testing.T, base string, client ports.HTTPClient, timeout time.Duration) *mmsHost {
	t.Helper()
	clock := portstest.NewFixedClock(time.Unix(1_760_000_000, 0))
	h := &hookHost{t: t, ctx: t.Context(), store: memory.New(memory.Options{Clock: clock})}
	engine, err := uma.New(uma.Config{
		Store: h.store,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{sms.NewFactory(sms.Options{BaseURL: base, Timeout: timeout})},
		Credentials: &tokenHost{},
		HTTP:        client,
		Blobs:       &portstest.MemoryBlobs{},
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(31),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.engine = engine
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	lock := false
	sel := model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1}
	if err := h.store.Transact(h.ctx, hookTenant, func(tx store.Tx) error {
		if err := tx.PutChannel(h.ctx, model.Channel{
			Ref: hookChannel, Type: model.TypeTwilioSms, Configuration: hookConfig().Extension(), CredentialRef: "twilio", Revision: "ch1",
		}, ""); err != nil {
			return err
		}
		return tx.PutInbox(h.ctx, model.Inbox{
			Ref: hookInbox, Channel: hookChannel, Name: "sms", Destinations: []string{senderNumber}, Selection: sel, Revision: "in1",
		}, "")
	}); err != nil {
		t.Fatal(err)
	}
	c, err := engine.For(model.Principal{Tenant: hookTenant, Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	return &mmsHost{hookHost: h, client: c}
}

// mms is the golden incoming MMS with its media URLs on origin.
func mms(t *testing.T, origin string) []byte {
	t.Helper()
	form, err := url.ParseQuery(string(fixture(t, "incoming_mms.form")))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"MediaUrl0", "MediaUrl1"} {
		form.Set(k, strings.Replace(form.Get(k), "https://api.twilio.com", origin, 1))
	}
	return []byte(form.Encode())
}

// An incoming MMS's media are downloaded from the account's Media
// resources with the REST credential and staged through the engine's
// sink: the message references each staged attachment, whose bytes the
// Attachments service serves, and a redelivery restages the same ones.
func TestIncomingMediaStaged(t *testing.T) {
	h := newMMSHost(t)
	jpeg, png := []byte("\xff\xd8\xff\xe0 jpeg"), []byte("\x89PNG png")
	h.fake.mu.Lock()
	a := h.fake.account(hookAccount)
	a.media[mediaOne], a.media[mediaTwo] = jpeg, png
	h.fake.mu.Unlock()
	body := mms(t, h.fake.srv.URL)
	for range 2 {
		if rec := post(h.handler(), "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	}
	msgs := h.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	var atts []model.AttachmentRef
	for _, p := range msgs[0].Content.Parts {
		if p.Kind == model.PartAttachment && p.Attachment != nil {
			atts = append(atts, *p.Attachment)
		}
	}
	if len(atts) != 2 {
		t.Fatalf("parts %+v, want both media staged", msgs[0].Content.Parts)
	}
	for k, want := range []struct {
		bytes []byte
		typ   string
	}{{jpeg, "image/jpeg"}, {png, "image/png"}} {
		att, rc, err := h.client.Attachments().Open(h.ctx, model.Lookup[model.AttachmentRef]{Call: model.Call{Correlation: "open"}, Ref: atts[k]})
		if err != nil {
			t.Fatalf("Open %d: %v", k, err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil || !bytes.Equal(got, want.bytes) || att.Metadata.MediaType != want.typ || att.Metadata.Audience.Visibility != model.Public {
			t.Fatalf("attachment %d = %+v %q, %v", k, att, got, err)
		}
	}
	for _, c := range h.fake.calls(hookAccount) {
		if strings.HasPrefix(c.Path, mediaBase) && c.User != hookAccount {
			t.Fatalf("media request %s authorized as %q", c.Path, c.User)
		}
	}
}

// The REST credential never reaches a media URL off the API origin or of
// another message: the message keeps its media summary only.
func TestMediaDownloadStaysOnTheAccount(t *testing.T) {
	h := newMMSHost(t)
	h.fake.mu.Lock()
	h.fake.account(hookAccount).media[mediaOne] = []byte("x")
	h.fake.mu.Unlock()
	form, _ := url.ParseQuery(string(mms(t, h.fake.srv.URL)))
	form.Set("NumMedia", "1")
	form.Del("MediaUrl1")
	form.Del("MediaContentType1")
	form.Set("MediaUrl0", "https://media.attacker.example"+mediaBase+mediaOne)
	body := []byte(form.Encode())
	if rec := post(h.handler(), "", body, formType, signTwilio(hookToken, hookURL, body)); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	msgs := h.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	for _, p := range msgs[0].Content.Parts {
		if p.Kind == model.PartAttachment {
			t.Fatalf("parts %+v, want the media summary only", msgs[0].Content.Parts)
		}
	}
	if n := len(h.fake.calls(hookAccount)); n != 0 {
		t.Fatalf("API calls = %d, want none", n)
	}
}
