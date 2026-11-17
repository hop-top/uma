package ingress

import (
	"context"

	"hop.top/uma/model"
	"hop.top/uma/policy"
)

// Ingress authorization. The service principal needs
// model.ActionChannelReceive on the channel to bind a sink, and again on
// the channel with the resolved inbox as destination for every ingress and
// staged attachment, so a revoked grant stops an existing binding on its
// next accept. A denial is model.Unauthorized, decided before any write:
// the ingress leaves no trace and stays with the adapter for redelivery.

// authorizeBind asks for channel.receive on the bound channel.
func authorizeBind(ctx context.Context, s *Sink) error {
	res := s.channel.Ref.Ref()
	return s.e.Bind(s.principal).Authorize(ctx, policy.Authorization{
		Action: model.ActionChannelReceive, Resource: res, Revision: s.channel.Revision,
	}, res)
}

// authorizeInbox asks for channel.receive on the bound channel into inbox.
func (s *Sink) authorizeInbox(ctx context.Context, inbox model.Inbox) error {
	res, dst := s.channel.Ref.Ref(), inbox.Ref.Ref()
	return s.e.Bind(s.principal).Authorize(ctx, policy.Authorization{
		Action: model.ActionChannelReceive, Resource: res, Destination: &dst, Revision: s.channel.Revision,
	}, res)
}

// authorizeIngress resolves the destination outside the write unit and
// authorizes ingress into its inbox. The unit then requires the inbox it
// resolves to be the one authorized here (see checkAuthorized).
func (s *Sink) authorizeIngress(ctx context.Context, destination string) (model.Inbox, error) {
	in, err := s.stagingInbox(ctx, destination)
	if err != nil {
		return model.Inbox{}, err
	}
	if err := s.authorizeInbox(ctx, in); err != nil {
		return model.Inbox{}, err
	}
	return in, nil
}

// checkAuthorized requires the inbox the unit resolved to be the one the
// attempt authorized, at the same revision. An inbox rebound or
// reconfigured since is model.Transient: the attempt rolls back and the
// next one authorizes again.
func (u *ingressUnit) checkAuthorized() error {
	if u.inbox.Ref != u.authorized.Ref || u.inbox.Revision != u.authorized.Revision {
		return model.NewError(model.Transient, "inbox changed during ingress authorization")
	}
	return nil
}
