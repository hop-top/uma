package sms_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"net/url"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio/sms"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// The adapter passes the shared suite for send, receive_http and history
// against a local Messages API, whose pages hold at most two messages so a
// history window spans several. The suite's inbound cases need a pull
// receiver, so webhook ingress is covered by webhook_test.go.
func TestAdapterConformance(t *testing.T) {
	conformance.RunAdapterSuite(t, conformance.AdapterHarness{
		New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
			fake := newFakeTwilio(t)
			fake.pageCap = 2
			f := sms.NewFactory(sms.Options{BaseURL: fake.srv.URL, Timeout: 5 * time.Second})
			return f, &provider{fake: fake}
		},
		Capabilities: []model.Capability{model.CapabilitySend, model.CapabilityReconcile, model.CapabilityReceiveHTTP, model.CapabilityStartConversation, model.CapabilityHistory},
	})
}

const senderNumber = "+15005550006"

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
		Configuration: sms.Config{
			AccountSID: accountFor(ch), From: senderNumber,
			WebhookURL: "https://hooks.example.com/twilio/sms/" + string(ch.ID),
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

// Route is the phone route of peer, a number ShapeID gave the suite.
func (p *provider) Route(ch model.ChannelRef, peer string) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: sms.Namespace, Kind: sms.RouteKind, ID: peer}
}

// ShapeID gives fixture values Twilio's forms: senders a phone number,
// the suite's destination the channel's own number (Twilio reports an
// incoming message's To), message and event IDs message SIDs.
func (p *provider) ShapeID(_ model.ChannelRef, kind conformance.FixtureKind, id string) string {
	switch kind {
	case conformance.FixtureSender:
		return phone(id)
	case conformance.FixtureDestination:
		if id == "support" {
			return senderNumber
		}
		return phone("destination " + id)
	case conformance.FixtureMessage, conformance.FixtureEvent:
		sum := sha256.Sum256([]byte(id))
		return "SM" + hex.EncodeToString(sum[:16])
	}
	return id
}

// HoldHistory makes ev a message the account received at at: an inbound
// message from its sender to its destination.
func (p *provider) HoldHistory(ch model.ChannelRef, ev conformance.InboundEvent, at time.Time) error {
	if ev.Kind != conformance.InboundMessage {
		return conformance.ErrNotExpressible
	}
	p.fake.holdInbound(accountFor(ch), fakeMessage{
		SID: ev.MessageID, From: ev.Sender, To: ev.Destination, Body: ev.Text, Sent: at,
	})
	return nil
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
			Route:      model.ExternalRef{Channel: ch, Namespace: sms.Namespace, Kind: sms.RouteKind, ID: c.Form.Get("To")},
			Applied:    c.Applied,
			Audience:   model.Audience{Visibility: model.Public},
			Content:    model.Content{Parts: []model.Part{{Kind: model.PartText, Text: c.Form.Get("Body")}}},
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
