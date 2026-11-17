// Package botdemo holds the stand-ins the quickstart bot example shares
// with its tests: the demo tenant, a permissive demo authorizer, the
// widget's session-key credential and the channel provisioning.
//
// It is the stand-in for an already-provisioned deployment; provisioning is
// a separate concern. A real host configures its channels and inboxes once,
// outside the bot, and hands the bot the channel reference.
package botdemo

import (
	"context"
	"crypto/rand"

	"hop.top/uma"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
)

// Tenant is the demo tenant every principal of the examples acts in.
const Tenant model.TenantID = "demo"

// ServiceActor is the principal that provisions the channel and runs the
// runtime. The host must run the runtime as the actor that provisioned.
const ServiceActor model.ActorID = "demo-service"

// site names the widget's inbox and its only destination.
const site = "support"

// LocalDemoAuthorizer is permissive demo authorization: it allows every
// authorization except those deny refuses (deny may be nil). A real host
// decides per principal and resource; never ship this.
func LocalDemoAuthorizer(deny func(policy.Authorization) bool) policy.Authorizer {
	return policy.AuthorizeFunc(func(_ context.Context, a policy.Authorization) (policy.Decision, error) {
		if deny != nil && deny(a) {
			return policy.Decision{Allow: false, ReasonCode: "demo-deny"}, nil
		}
		return policy.Decision{Allow: true, ReasonCode: "demo"}, nil
	})
}

// Provision configures the widget channel and its inbox and returns the
// channel reference. It is the stand-in for an already-provisioned
// deployment; provisioning is a separate concern. uma records each call as
// a ledgered configuration operation.
func Provision(ctx context.Context, e uma.Engine) (model.ChannelRef, error) {
	admin, err := e.For(model.Principal{Tenant: Tenant, Actor: ServiceActor})
	if err != nil {
		return model.ChannelRef{}, err
	}
	cfg := admin.Configuration()
	acc, err := cfg.ConfigureChannel(ctx, model.ConfigureChannel{
		Command: command("configure-channel"),
		Channel: model.Channel{
			Ref: model.ChannelRef{Tenant: Tenant}, Type: widget.Type,
			Configuration: widget.Configuration(site), CredentialRef: "session-key",
		},
	})
	if err != nil {
		return model.ChannelRef{}, err
	}
	channel, err := acc.Operation.Result.AsChannel()
	if err != nil {
		return model.ChannelRef{}, err
	}
	// Per room: one conversation per browser session.
	if _, err := cfg.ConfigureInbox(ctx, model.ConfigureInbox{
		Command: command("configure-inbox"),
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: Tenant}, Channel: channel, Name: site,
			Destinations: []string{site}, Selection: model.Selection{Mode: model.ModePerRoom},
		},
	}); err != nil {
		return model.ChannelRef{}, err
	}
	return channel, nil
}

func command(op model.OperationID) model.Command {
	return model.Command{Call: model.Call{Correlation: model.CorrelationID(op)}, Operation: op}
}

// sessionKey is the host's credential port: the widget's session signing
// key, random per process, so restarting ends every visitor session.
type sessionKey []byte

// NewSessionKey returns the credential port the examples hand to uma.
func NewSessionKey() ports.Credentials { return sessionKey(rand.Text() + rand.Text()) }

func (k sessionKey) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	if r.Purpose != widget.CredentialPurpose || !r.Accepts(ports.KindSecret) {
		return nil, model.NewError(model.Unsupported, "no credential for "+r.Purpose)
	}
	return ports.Secret{Bytes: k}, nil
}
