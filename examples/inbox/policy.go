package main

import (
	"context"
	"sync/atomic"

	"hop.top/uma/adapter/mail/smtp"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
)

// roles is the host's authorization policy: a fixed action list per actor.
// uma asks it about every operation, including the reauthorization of a
// queued reply when a worker executes it; anything not listed is denied.
// The service actor receives: channel.receive is asked when a receiver or
// webhook binds and again for every ingress, so removing the grant stops
// ingress without a restart. Replies execute as the operator who sent them.
type roles struct{ tenant model.TenantID }

var grants = map[model.ActorID][]string{
	serviceActor: {model.ActionChannelReceive},
	setupActor: {
		model.ActionChannelRead, model.ActionChannelConfigure,
		model.ActionInboxRead, model.ActionInboxConfigure,
	},
	operatorActor: {
		model.ActionInboxRead, model.ActionConversationRead,
		model.ActionMessageRead, model.ActionMessageReply, model.ActionMessageNote,
		model.ActionOperationRead,
	},
}

func (r roles) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	if a.Principal.Tenant != r.tenant || a.Resource.Tenant != r.tenant {
		return policy.Decision{ReasonCode: "tenant"}, nil
	}
	for _, action := range grants[a.Principal.Actor] {
		if action == a.Action {
			return policy.Decision{Allow: true, PolicyVersion: "roles/v1"}, nil
		}
	}
	return policy.Decision{ReasonCode: "not_granted", PolicyVersion: "roles/v1"}, nil
}

// Secrets is the host's credential port: the widget session key and the
// relay login, resolved on every connection. Revoke makes every later
// resolution fail as uma expects of withdrawn consent.
type Secrets struct {
	WidgetKey     []byte
	RelayUser     string
	RelayPassword []byte
	revoked       atomic.Bool
}

// Revoke withdraws every credential.
func (s *Secrets) Revoke() { s.revoked.Store(true) }

// Resolve implements ports.Credentials.
func (s *Secrets) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	if s.revoked.Load() {
		return nil, model.NewError(model.Unauthorized, "credential revoked")
	}
	switch {
	case r.Reference == widgetKeyRef && r.Purpose == widget.CredentialPurpose && r.Accepts(ports.KindSecret):
		return ports.Secret{Bytes: s.WidgetKey}, nil
	case r.Reference == relayRef && r.Purpose == smtp.PurposeRelay && r.Accepts(ports.KindPassword):
		return ports.Password{User: s.RelayUser, Secret: s.RelayPassword}, nil
	}
	return nil, model.NewError(model.Unsupported, "no credential for this request")
}

var (
	_ policy.Authorizer = roles{}
	_ ports.Credentials = (*Secrets)(nil)
)
