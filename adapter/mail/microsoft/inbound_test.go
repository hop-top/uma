package microsoft

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
)

func headerRef(kind, id string) model.ExternalRef {
	return model.ExternalRef{Channel: testChan, Namespace: model.EmailHeaderNamespace, Kind: kind, ID: id}
}

func fetch(t *testing.T, e *env, id string) model.Envelope {
	t.Helper()
	in, skip, err := e.inst.fetchIngress(t.Context(), e.inst.c.session(), nil, id)
	if err != nil || skip {
		t.Fatalf("fetchIngress(%s) = skip %v, %v", id, skip, err)
	}
	return in.Envelope
}

func TestInboundThreadingFromHeadersOnly(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.deliver(e.box.name, gMessage{
		ID: "m1", InternetMessageID: "<graph-id@outlook.test>", ConversationID: "AAQkAD-conv",
		Subject: "Re: order", From: &gRecipient{EmailAddress: gEmailAddress{Address: "Peer@Example.test"}},
		Body: &gItemBody{ContentType: "text", Content: "reply"},
		InternetMessageHeaders: []gHeader{
			{Name: "Message-ID", Value: "<m1@peer.test>"},
			{Name: "In-Reply-To", Value: "junk <p1@uma.test> more <p0@uma.test>"},
			{Name: "References", Value: "<r0@uma.test>"},
			{Name: "references", Value: "<p1@uma.test>"},
			{Name: "List-Id", Value: "Orders <orders.example.test>"},
			{Name: "Authentication-Results", Value: "spf=pass"},
		},
	})
	env := fetch(t, e, "m1")
	want := []model.ExternalRef{
		headerRef(model.RefKindEmailMessageID, "<m1@peer.test>"),
		headerRef(model.RefKindEmailInReplyTo, "<p1@uma.test>"),
		headerRef(model.RefKindEmailReferences, "<r0@uma.test>"),
		headerRef(model.RefKindEmailReferences, "<p1@uma.test>"),
		headerRef(model.RefKindEmailListID, "orders.example.test"),
	}
	if !slices.Equal(env.References, want) {
		t.Fatalf("references %+v, want %+v", env.References, want)
	}
	if env.InReplyTo != "<p1@uma.test>" {
		t.Fatalf("InReplyTo = %q, want the first valid In-Reply-To msg-id", env.InReplyTo)
	}
	for _, r := range env.References {
		if strings.Contains(r.ID, "AAQkAD") {
			t.Fatal("conversationId used as threading evidence")
		}
	}
	var p messagePayload
	if env.Payload.Name != PayloadMessage || json.Unmarshal(env.Payload.Data, &p) != nil ||
		p.ConversationID != "AAQkAD-conv" || p.AuthenticationResults != "spf=pass" || p.Subject != "Re: order" {
		t.Fatalf("payload %s", env.Payload.Data)
	}
	if env.Sender.ID != "Peer@example.test" || env.Sender.ParentID != "m1" || env.Sender.Namespace != IdentityNamespace {
		t.Fatalf("sender %+v, want the from address with its domain lowercased and the message as parent", env.Sender)
	}
	if env.MessageID != "m1" || env.Kind != model.EnvelopeMessage || env.Audience.Visibility != model.Public {
		t.Fatalf("envelope %+v", env)
	}
}

func TestInboundMessageIDFallsBackToInternetMessageID(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", "peer@example.test", "hi")
	if env := fetch(t, e, "m1"); len(env.References) != 1 || env.References[0] != headerRef(model.RefKindEmailMessageID, "<m1@peer.test>") {
		t.Fatalf("references %+v", env.References)
	}
}

func TestInboundAutoSubmittedIsAutomated(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.deliver(e.box.name, gMessage{
		ID: "m1", From: &gRecipient{EmailAddress: gEmailAddress{Address: "noreply@example.test"}},
		Body:                   &gItemBody{ContentType: "text", Content: "out of office"},
		InternetMessageHeaders: []gHeader{{Name: "Auto-Submitted", Value: "auto-replied"}},
	})
	if env := fetch(t, e, "m1"); env.Kind != model.EnvelopeAutomatedMessage {
		t.Fatalf("kind %q, want automated", env.Kind)
	}
}

func TestInboundAttachmentsAndDestination(t *testing.T) {
	e := newEnv(t, Config{Addresses: []string{"support@contoso.test", "billing@contoso.test"}})
	e.g.deliver(e.box.name, gMessage{
		ID: "m1", From: &gRecipient{EmailAddress: gEmailAddress{Address: "peer@example.test"}},
		ToRecipients: []gRecipient{{EmailAddress: gEmailAddress{Address: "someone@example.test"}}},
		CcRecipients: []gRecipient{{EmailAddress: gEmailAddress{Address: "BILLING@contoso.test"}}},
		Body:         &gItemBody{ContentType: "text", Content: "invoice attached"},
	}, gAttachment{ODataType: "#microsoft.graph.fileAttachment", ID: "att-1", Name: "invoice.pdf", ContentType: "application/pdf", Size: 1234})
	env := fetch(t, e, "m1")
	if env.Destination != "billing@contoso.test" {
		t.Fatalf("destination %q, want the configured address found in Cc", env.Destination)
	}
	if len(env.Content.Parts) != 2 || env.Content.Parts[0].Text != "invoice attached" {
		t.Fatalf("parts %+v", env.Content.Parts)
	}
	p := env.Content.Parts[1]
	var a attachmentPart
	if p.Kind != model.PartStructured || p.Structured.Name != PartAttachment || json.Unmarshal(p.Structured.Data, &a) != nil ||
		a != (attachmentPart{ID: "att-1", Name: "invoice.pdf", ContentType: "application/pdf", Size: 1234}) {
		t.Fatalf("attachment part %+v", p)
	}
	if err := env.Content.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestInboundWithoutBodyStillHasContent(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.deliver(e.box.name, gMessage{ID: "m1", Subject: "ping", From: &gRecipient{EmailAddress: gEmailAddress{Address: "peer@example.test"}}})
	env := fetch(t, e, "m1")
	if err := env.Content.Validate(); err != nil || env.Content.Parts[0].Structured.Name != PayloadMessage {
		t.Fatalf("content %+v: %v", env.Content, err)
	}
}

func TestInboundSkipsDraftsAndVanishedMessages(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.deliver(e.box.name, gMessage{ID: "d1", IsDraft: true})
	for _, id := range []string{"d1", "gone"} {
		if _, skip, err := e.inst.fetchIngress(t.Context(), e.inst.c.session(), nil, id); err != nil || !skip {
			t.Fatalf("%s: skip %v, %v", id, skip, err)
		}
	}
}

func TestInboundTextTruncatedOnRuneBoundary(t *testing.T) {
	limit := model.DefaultContentLimits().MaxTextBytes
	text := strings.Repeat("a", limit-1) + "é"
	got, cut := truncateText(text, limit)
	if !cut || len(got) != limit-1 {
		t.Fatalf("len %d cut %v", len(got), cut)
	}
	e := newEnv(t, Config{})
	e.g.deliver(e.box.name, gMessage{ID: "m1", ReceivedDateTime: time.Unix(1, 0).UTC(),
		From: &gRecipient{EmailAddress: gEmailAddress{Address: "p@example.test"}}, Body: &gItemBody{ContentType: "text", Content: text}})
	env := fetch(t, e, "m1")
	if err := env.Content.Validate(); err != nil {
		t.Fatal(err)
	}
	var p messagePayload
	_ = json.Unmarshal(env.Payload.Data, &p)
	if !p.TextTruncated {
		t.Fatal("truncation not recorded")
	}
}

func TestConfigValidation(t *testing.T) {
	f := NewFactory(Options{})
	ok := []string{`{}`, `{"mailbox":"desk@contoso.test","notification_url":"https://h.test/x","cloud":"usgov"}`}
	for _, d := range ok {
		if issues := f.Validate(model.Extension{Name: ConfigSchema, Version: 1, Data: json.RawMessage(d)}); len(issues) > 0 {
			t.Errorf("%s: %+v", d, issues)
		}
	}
	bad := []string{
		`{"mailbox":"a/b"}`, `{"notification_url":"http://h.test/x"}`, `{"cloud":"mars"}`,
		`{"subscription_lifetime_ms":60000}`, `{"unknown":1}`, `{"lifecycle_notification_url":"https://h.test/l"}`,
	}
	for _, d := range bad {
		if issues := f.Validate(model.Extension{Name: ConfigSchema, Version: 1, Data: json.RawMessage(d)}); len(issues) == 0 {
			t.Errorf("%s accepted", d)
		}
	}
	if issues := f.Validate(model.Extension{Name: "other.config", Version: 1}); len(issues) == 0 {
		t.Error("foreign schema accepted")
	}
}
