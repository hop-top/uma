package zendesk_test

import (
	"encoding/json"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/ticket/zendesk"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// The adapter passes the shared suite for every capability it declares,
// over the fake Support API, with tickets bound per work item.
func TestAdapterConformance(t *testing.T) {
	conformance.RunAdapterSuite(t, conformance.AdapterHarness{
		New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
			f := newFakeZendesk(t)
			return f.factory(), &provider{f: f}
		},
		Capabilities: zendesk.NewFactory().Describe().Capabilities,
		Selection:    model.Selection{Mode: model.ModePerWorkItem},
		Extensions:   zendesk.NewFactory().Extensions(),
		Action: model.Extension{
			Name: zendesk.ExtensionUpdate, Version: zendesk.ExtensionVersion,
			Data: json.RawMessage(`{"status":"pending"}`),
		},
		// A ticket update that sets no field: the adapter's own rule.
		InvalidAction: model.Extension{
			Name: zendesk.ExtensionUpdate, Version: zendesk.ExtensionVersion,
			Data: json.RawMessage(`{}`),
		},
	})
}

// provider is the conformance view of the fake: one Zendesk account per
// channel, each with its own subdomain.
type provider struct{ f *fakeZendesk }

func subdomain(ch model.ChannelRef) string {
	id := string(ch.ID)
	return "acct-" + id[len(id)-4:]
}

func (p *provider) acct(ch model.ChannelRef) *fakeAccount { return p.f.account(subdomain(ch), ch) }

func (p *provider) Account(ch model.ChannelRef) conformance.Account {
	p.acct(ch)
	return conformance.Account{Configuration: zendesk.Config{ // #nosec G101 -- a credential reference name, not a credential
		Subdomain: subdomain(ch), Auth: zendesk.AuthAPIToken, Email: "agent@example.com", WebhookToleranceSeconds: 300,
	}.Extension(), CredentialRef: "zendesk-cred"}
}

// Route is the work item reference of the peer's ticket.
func (p *provider) Route(ch model.ChannelRef, peer string) model.ExternalRef { return route(ch, peer) }

// Deliver queues the event as a comment on the ticket of its room at its
// destination brand. Unauthenticated claims have no place in a comment
// event: only the author ID identifies its author.
func (p *provider) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
	var public *bool
	switch ev.Visibility {
	case "", model.Public:
		v := true
		public = &v
	default:
		v := false
		public = &v
	}
	ticket := strings.ReplaceAll(ev.Room+"-"+ev.Destination, " ", "-")
	p.f.addComment(p.acct(ch), ev.EventID, ticket, ev.Destination, ev.MessageID, ev.Sender, ev.Text, public, false, nil)
	return nil
}

func (p *provider) Inbound(ch model.ChannelRef) []conformance.InboundResult {
	return p.f.inbound(p.acct(ch))
}

func (p *provider) Respond(ch model.ChannelRef, rs ...conformance.Response) error {
	a := p.acct(ch)
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	a.script = append(a.script, rs...)
	return nil
}

func (p *provider) Calls(ch model.ChannelRef) []conformance.ProviderCall {
	return p.f.callsOf(p.acct(ch))
}

// Revoke is not expressible: Zendesk reveals a withdrawn permission only
// by refusing the call itself, so the adapter cannot avoid that call.
func (p *provider) Revoke(model.ChannelRef, model.Capability) error {
	return conformance.ErrNotExpressible
}
