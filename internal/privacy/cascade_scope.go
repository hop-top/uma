package privacy

import (
	"context"
	"slices"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/policy"
)

// redactable returns the identities a cascade redacts out of those it
// owns, and whether it skipped any. An identity cascade's own
// authorization covers its identity; a conversation cascade owns none.
//
// A contact cascade decides, before anything is written, since its
// identities may sit in inboxes the authorization on the contact does not
// speak for: first model.ActionPrivacyRedactCascade on the contact, so a
// caller refused it learns nothing of the contact's inboxes; then the
// threshold (see threshold); then model.ActionPrivacyRedact on each
// identity at its current revision. It is refused with model.Unauthorized
// when the threshold is policy.CascadeDisabled, or once the denials reach
// a positive threshold. Otherwise the denied identities are skipped.
func redactable(ctx context.Context, c *engine.Client, req model.Redact, identities []model.Identity) ([]model.Identity, bool, error) {
	if req.Target.Kind != model.KindContact {
		return identities, false, nil
	}
	if err := c.Authorize(ctx, policy.Authorization{
		Action: model.ActionPrivacyRedactCascade, Resource: req.Target, Revision: req.ExpectedRevision,
	}, req.Target); err != nil {
		return nil, false, err
	}
	limit, err := threshold(ctx, c, identities)
	if err != nil {
		return nil, false, err
	}
	if limit == policy.CascadeDisabled {
		return nil, false, model.NewError(model.Unauthorized, "contact cascade disabled by policy")
	}
	permitted := make([]model.Identity, 0, len(identities))
	denied := 0
	for _, id := range identities {
		ref := id.Ref.Ref()
		err := c.Authorize(ctx, policy.Authorization{
			Action: model.ActionPrivacyRedact, Resource: ref, Revision: id.Revision,
		}, ref)
		switch {
		case model.IsCode(err, model.Unauthorized):
			denied++
			if limit > 0 && denied >= limit {
				return nil, false, model.NewError(model.Unauthorized,
					"contact cascade refused: identities the actor may not redact reached the policy threshold")
			}
		case err != nil:
			return nil, false, err
		default:
			permitted = append(permitted, id)
		}
	}
	return permitted, denied > 0, nil
}

// threshold resolves a contact cascade's threshold: the most restrictive
// of those of the inboxes its identities sit in, each asked once in the
// identities' order. policy.CascadeDisabled ends the questions.
func threshold(ctx context.Context, c *engine.Client, identities []model.Identity) (int, error) {
	limit := policy.CascadeNeverRefuse
	var asked []model.InboxRef
	for _, id := range identities {
		if slices.Contains(asked, id.Ref.Inbox) {
			continue
		}
		asked = append(asked, id.Ref.Inbox)
		n, err := c.Engine().ContactCascadeThreshold(ctx, c.Principal(), id.Ref.Inbox)
		if err != nil {
			return 0, err
		}
		if n == policy.CascadeDisabled {
			return n, nil
		}
		if limit == policy.CascadeNeverRefuse || (n != policy.CascadeNeverRefuse && n < limit) {
			limit = n
		}
	}
	return limit, nil
}
