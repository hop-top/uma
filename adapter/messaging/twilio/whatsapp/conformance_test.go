package whatsapp_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"net/url"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio/whatsapp"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// The adapter passes the shared suite for send and receive_http against a
// local Messages API. The suite's inbound cases need a pull receiver, so
// webhook ingress is covered by webhook_test.go. The suite records no
// inbound message before its free-form sends, so the window is Twilio's to
// decide.
func TestAdapterConformance(t *testing.T) {
	conformance.RunAdapterSuite(t, conformance.AdapterHarness{
		New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
			fake := newFakeTwilio(t)
			f := whatsapp.NewFactory(whatsapp.Options{BaseURL: fake.srv.URL, Timeout: 5 * time.Second})
			return f, &provider{fake: fake}
		},
		Capabilities: whatsapp.Capabilities(),
	})
}

// provider is the conformance view of fakeTwilio: one Twilio account per
// channel, its SID derived from the channel ID.
type provider struct{ fake *fakeTwilio }

// HTTPClient is the client the suite's engine injects.
func (p *provider) HTTPClient() ports.HTTPClient { return p.fake.client() }

func accountFor(ch model.ChannelRef) string {
	sum := sha256.Sum256([]byte(string(ch.Tenant) + "/" + string(ch.ID)))
	return "AC" + hex.EncodeToString(sum[:16])
}

func (p *provider) Account(ch model.ChannelRef) conformance.Account {
	return conformance.Account{
		Configuration: whatsapp.Config{
			AccountSID: accountFor(ch), From: senderNumber,
			WebhookURL: "https://hooks.example.com/twilio/whatsapp/" + string(ch.ID),
		}.Extension(),
		CredentialRef: "twilio-account",
	}
}

// phone maps a suite peer name to a stable E.164 number.
func phone(peer string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(peer))
	return fmt.Sprintf("+1555%07d", h.Sum32()%10_000_000)
}

func (p *provider) Route(ch model.ChannelRef, peer string) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: whatsapp.Namespace, Kind: whatsapp.RouteKind, ID: phone(peer)}
}

// Deliver is not expressible: Twilio pushes webhooks, which the suite's
// pull-receiver cases cannot drive.
func (p *provider) Deliver(model.ChannelRef, conformance.InboundEvent) error {
	return conformance.ErrNotExpressible
}

func (p *provider) Inbound(model.ChannelRef) []conformance.InboundResult { return nil }

func (p *provider) Respond(ch model.ChannelRef, rs ...conformance.Response) error {
	p.fake.respond(accountFor(ch), rs...)
	return nil
}

// Calls maps the Messages API requests of ch's account. The operation is
// read back from the status callback URL Send tagged.
func (p *provider) Calls(ch model.ChannelRef) []conformance.ProviderCall {
	var out []conformance.ProviderCall
	for _, c := range p.fake.calls(accountFor(ch)) {
		pc := conformance.ProviderCall{
			Capability: model.CapabilitySend,
			Route: model.ExternalRef{
				Channel: ch, Namespace: whatsapp.Namespace, Kind: whatsapp.RouteKind,
				ID: strings.TrimPrefix(c.Form.Get("To"), whatsapp.AddressPrefix),
			},
			Applied:  c.Applied,
			Audience: model.Audience{Visibility: model.Public},
			Content:  model.Content{Parts: []model.Part{{Kind: model.PartText, Text: c.Form.Get("Body")}}},
		}
		if c.Method != "POST" {
			pc.Capability = model.CapabilityReconcile
		}
		if cb, err := url.Parse(c.Form.Get("StatusCallback")); err == nil {
			if id := cb.Query().Get("operation"); id != "" {
				pc.Operation = model.OperationRef{Tenant: ch.Tenant, ID: model.ID(id)}
			}
		}
		if c.Applied {
			pc.References = []model.ExternalRef{model.ProviderMessageRef(ch, c.SID)}
		}
		out = append(out, pc)
	}
	return out
}

// Revoke is not expressible: Twilio has no per-capability permission the
// adapter could observe before calling.
func (p *provider) Revoke(model.ChannelRef, model.Capability) error {
	return conformance.ErrNotExpressible
}
