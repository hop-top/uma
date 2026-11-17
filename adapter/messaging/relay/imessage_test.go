package relay_test

import (
	"net/http"
	"strings"
	"testing"

	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
)

func imessageConfig() relay.Config {
	cfg := baseConfig()
	cfg.Medium = relay.MediumIMessage
	return cfg
}

// An iMessage handle is a phone number or an email address. Numbers, and
// anything else that is no email address, are kept exactly as the gateway
// sends them, as on the other mediums; an email address is lowercased, since
// Apple IDs ignore case.
func TestWebhookIMessageHandles(t *testing.T) {
	tests := []struct {
		name, from, to   string
		wantFrom, wantTo string
	}{
		{"phone number", "+15550100001", "+15550100002", "+15550100001", "+15550100002"},
		{"email address", "Alice@Example.COM", "Support@Example.com", "alice@example.com", "support@example.com"},
		{"number to an email address", "+15550100001", "Support@Example.com", "+15550100001", "support@example.com"},
		// Only an email address is an Apple ID: any other handle goes back to
		// the relay as a reply's "to", so it keeps the case it was sent in.
		{"other handle kept verbatim", "iMessage;-;+15550100001", "+15550100002", "iMessage;-;+15550100001", "+15550100002"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &sink{}
			body := `{"from":"` + tt.from + `","to":"` + tt.to + `","body":"hello","message_id":"m-1"}`
			if rec := serve(t, newInstance(t, imessageConfig(), nil), s, good(body)); rec.Code != http.StatusOK {
				t.Fatalf("answer %d", rec.Code)
			}
			env := s.ingresses()[0].Envelope
			want := model.ExternalRef{Channel: testChannel, Namespace: relay.NamespaceIMessage, Kind: relay.RouteKind, ID: tt.wantFrom}
			if env.Sender != want {
				t.Errorf("sender %+v, want %+v", env.Sender, want)
			}
			if env.Destination != tt.wantTo {
				t.Errorf("destination %q, want %q", env.Destination, tt.wantTo)
			}
		})
	}
}

// One Apple ID reported in two cases is one sender and one delivery.
func TestWebhookIMessageEventIDIgnoresEmailCase(t *testing.T) {
	eventOf := func(from string) string {
		s := &sink{}
		body := `{"from":"` + from + `","to":"+15550100002","body":"hello","message_id":"m-1"}`
		if rec := serve(t, newInstance(t, imessageConfig(), nil), s, good(body)); rec.Code != http.StatusOK {
			t.Fatalf("answer %d", rec.Code)
		}
		return s.ingresses()[0].Envelope.EventID
	}
	if eventOf("Alice@Example.com") != eventOf("alice@example.com") {
		t.Error("a redelivery in another case changes its event id")
	}
}

// The other mediums keep every address exactly as sent, an email-like one
// included.
func TestWebhookOtherMediumsKeepAddressesVerbatim(t *testing.T) {
	for _, medium := range []string{relay.MediumSMS, relay.MediumWhatsApp} {
		t.Run(medium, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Medium = medium
			s := &sink{}
			body := `{"from":"Alice@Example.COM","to":"Support@Example.com","body":"hello","message_id":"m-1"}`
			if rec := serve(t, newInstance(t, cfg, nil), s, good(body)); rec.Code != http.StatusOK {
				t.Fatalf("answer %d", rec.Code)
			}
			env := s.ingresses()[0].Envelope
			if env.Sender.ID != "Alice@Example.COM" || env.Destination != "Support@Example.com" {
				t.Errorf("sender %q destination %q", env.Sender.ID, env.Destination)
			}
		})
	}
}

// An iMessage handle is held to the address rules of every medium.
func TestWebhookIMessageRefusesMalformedHandles(t *testing.T) {
	for name, from := range map[string]string{
		"empty":        ``,
		"control char": `alice\u0007@example.com`,
		"too long":     strings.Repeat("a", 120) + "@example.com",
	} {
		t.Run(name, func(t *testing.T) {
			s := &sink{}
			body := `{"from":"` + from + `","to":"+15550100002","body":"hello","message_id":"m-1"}`
			if rec := serve(t, newInstance(t, imessageConfig(), nil), s, good(body)); rec.Code != http.StatusBadRequest {
				t.Errorf("answer %d, want 400", rec.Code)
			}
			if n := len(s.ingresses()); n != 0 {
				t.Errorf("%d ingresses", n)
			}
		})
	}
}

func TestSendIMessageMedium(t *testing.T) {
	r := newSendRig(t, func(c *relay.Config) { c.Medium = relay.MediumIMessage })
	req := sendReq("hi")
	req.Route.Namespace = relay.NamespaceIMessage
	req.Route.ID = "alice@example.com"
	if _, err := r.inst.Send(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	body := r.fake.requests()[0].Body
	if body["medium"] != "imessage" || body["to"] != "alice@example.com" {
		t.Errorf("callback body %v", body)
	}
	// A route of another medium's namespace is not this channel's.
	req.Route.Namespace = relay.NamespaceSMS
	if _, err := r.inst.Send(t.Context(), req); model.CodeOf(err) != model.Validation {
		t.Errorf("sms route on an imessage channel: %v", err)
	}
}
