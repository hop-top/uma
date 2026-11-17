package a2a

import (
	"context"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Send records a uma-authored reply as accepted: the adapter has no
// transport, because a remote agent reads replies, status content and
// artifacts through the protocol (the blocked SendMessage, GetTask,
// ListTasks) and never receives a push. The outcome names the reply by a
// provider message reference derived from the attempt, so the engine's
// message identity stays unique per channel and a repeated attempt names
// the same one. The reply is accepted, not delivered: nothing confirms the
// agent has read it.
//
// The route must be an agent context of this channel and the audience
// public: internal and restricted content is never shown to a remote agent.
func (x *Instance) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	if err := x.ready(ctx); err != nil {
		return adapter.Outcome{}, err
	}
	if rt := r.Route; rt.Channel != x.ch || rt.Namespace != NamespaceContext || rt.Kind != model.RefKindContext || rt.Validate() != nil {
		return adapter.Outcome{}, model.NewError(model.Validation, "route is not an agent context of this channel")
	}
	if r.Audience.Visibility != model.Public {
		return adapter.Outcome{}, model.NewError(model.Validation, "a2a delivers public content only")
	}
	if r.Attempt == "" {
		return adapter.Outcome{}, model.NewError(model.Validation, "attempt: required")
	}
	return adapter.Outcome{
		State:      model.ProviderAccepted,
		References: []model.ExternalRef{model.ProviderMessageRef(x.ch, string(r.Attempt))},
	}, nil
}
