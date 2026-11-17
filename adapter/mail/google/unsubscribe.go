package google

import (
	"context"
	"net/url"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/internal/unsubscribe"
	"hop.top/uma/model"
)

const (
	// ExtensionUnsubscribe unsubscribes the account from the sender of one
	// message, by the method the message's own headers allow:
	//
	//   - RFC 8058 one-click: an HTTPS POST of "List-Unsubscribe=One-Click"
	//     to the https URI of List-Unsubscribe, only when the message carries
	//     "List-Unsubscribe-Post: List-Unsubscribe=One-Click" and a valid DKIM
	//     signature by the sender's organization covers both fields;
	//   - mailto (RFC 2369): a message sent from the mailbox through the
	//     adapter's own send, only when the mailto address is in the sender's
	//     organizational domain (public suffix plus one label).
	//
	// Its data is optional: {"method": "auto" | "one_click" | "mailto"}.
	// Auto, the default, uses one-click when the message is eligible, else
	// mailto. Every other URI in the header is never followed, a redirect
	// from the one-click endpoint is never followed, and no GET is made.
	// The message is the effect's Target when it names a provider message of
	// the channel, else the route's ParentID. The message's headers and raw
	// form are read from Gmail (users.messages.get, format=raw) at the
	// attempt, never taken from the caller. The effect is authorized as
	// model.ActionExtensionExecute. It needs ScopeReadonly, and ScopeSend
	// for a mailto.
	ExtensionUnsubscribe = string(Type) + ".unsubscribe"
)

// unsubscribeDefinition is the registration of ExtensionUnsubscribe.
func unsubscribeDefinition() adapter.ExtensionDefinition {
	return adapter.ExtensionDefinition{
		Name: ExtensionUnsubscribe, Version: ExtensionVersion, Kind: model.ExtensionKindEffect,
		InputSchema:        model.Extension{Name: ExtensionUnsubscribe + ".input", Version: ExtensionVersion},
		RequiredCapability: model.CapabilityExtensionExecute, AuthorizeAs: model.ActionExtensionExecute,
	}
}

// validateUnsubscribe checks the action's fields without I/O.
func validateUnsubscribe(action model.Extension) []model.ValidationIssue {
	if action.Version != ExtensionVersion {
		return nil
	}
	if _, issue := unsubscribe.CheckAction(action); issue != nil {
		return []model.ValidationIssue{*issue}
	}
	return nil
}

// unsubscribeEffect runs ExtensionUnsubscribe. Refusals are final: a
// rejected outcome with a validation failure, decided from the message's
// headers before any request to the unsubscribe target (the only requests
// made first are the read of the message from Gmail and the DNS lookups of
// its DKIM keys). A message Gmail no longer has is rejected not_found; one
// over the raw size bound cannot be verified and is refused. A failed DKIM
// key lookup is transient, so the attempt is repeated. The one-click POST
// and the mailto send are described by unsubscribe.Poster.Post and Send; a
// mailto send answered ambiguously is outcome_unknown, reconciled by
// Message-ID like any send.
func (i *instance) unsubscribeEffect(ctx context.Context, req adapter.ExecuteRequest) (adapter.Outcome, error) {
	if req.Action.Version != ExtensionVersion {
		return rejected(model.SchemaMismatch, "unsupported action version"), nil
	}
	a, issue := unsubscribe.CheckAction(req.Action)
	if issue != nil {
		return rejected(model.Validation, issue.Message), nil
	}
	id, ok := i.targetMessage(req.Effect)
	if !ok {
		return rejected(model.Validation, "no provider message to unsubscribe from: the effect names none and the route has no parent message"), nil
	}
	s := i.c.session()
	raw, err := i.rawMessage(ctx, s, id)
	switch {
	case model.IsCode(err, model.NotFound):
		return rejected(model.NotFound, "the message is gone"), nil
	case tooLarge(err):
		return rejected(model.Validation, "the message is too large to check its DKIM signature"), nil
	case err != nil:
		return rejectedOr(err)
	}
	plan, ref, temporary := unsubscribe.Evaluate(ctx, raw, a, i.unsub)
	switch {
	case temporary:
		return adapter.Outcome{}, model.NewError(model.Transient, ref.Message)
	case ref != nil:
		return rejected(model.Validation, ref.Message), nil
	}
	refs := []model.ExternalRef{model.ProviderMessageRef(i.ch.Ref, id)}
	if plan.Method == unsubscribe.MethodOneClick {
		out, err := i.unsubPost.Post(ctx, plan.URL)
		if err == nil && out.State == model.ProviderAccepted {
			out.References = refs
		}
		return out, err
	}
	send := adapter.SendRequest{
		Effect:  req.Effect,
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: plan.Mailto.Body}}},
	}
	send.Target, send.Interaction = nil, nil
	send.Route = model.ExternalRef{Channel: i.ch.Ref, Namespace: IdentityNamespace, Kind: refKindAddress, ID: plan.Mailto.Address}
	out, err := i.send(ctx, send, plan.Mailto.Subject)
	if err == nil && out.State == model.ProviderAccepted {
		out.References = append(out.References, refs...)
	}
	return out, err
}

// rawMessage reads Gmail message id in raw form, bounded like an inbound
// read.
func (i *instance) rawMessage(ctx context.Context, s *session, id string) ([]byte, error) {
	resp, err := s.do(ctx, request{
		method: "GET", url: i.c.api("/messages/"+url.PathEscape(id), url.Values{"format": {"raw"}}),
		limit: i.c.rawMax(),
	})
	if err != nil {
		return nil, err
	}
	m, err := decodeMessage(resp.body, id)
	if err != nil {
		return nil, err
	}
	if m.Raw == "" {
		return nil, errUnexpectedAnswer()
	}
	raw, err := decodeRaw(m.Raw)
	if err != nil {
		return nil, errUnexpectedAnswer()
	}
	return raw, nil
}
