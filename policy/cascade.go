package policy

import (
	"context"

	"hop.top/uma/model"
)

// Contact cascade thresholds a CascadePolicy returns. Any positive value N
// refuses a contact cascade once N of the contact's identities are denied.
const (
	// CascadeDisabled refuses every contact cascade over the inbox.
	CascadeDisabled = 0
	// CascadeNeverRefuse never refuses on denials: denied identities are
	// skipped. It is the threshold of every inbox when no CascadePolicy is
	// configured.
	CascadeNeverRefuse = -1
)

// CascadePolicy sets, per tenant and inbox, how a contact cascade treats
// the identities of the contact its actor may not redact.
//
// A contact cascade (model.Redact with Cascade on a contact) decides
// model.ActionPrivacyRedact on each identity on the contact before
// anything is written, since the identities may sit in inboxes the
// cascade's own authorization does not speak for. Denied identities are
// never redacted: their messages, attachments and link history stay as
// they are. The threshold decides whether the rest goes ahead:
//
//   - CascadeDisabled (0): the cascade is refused, nothing written;
//   - N >= 1: refused once N or more identities are denied, otherwise
//     the permitted identities are redacted and the denied ones skipped;
//   - CascadeNeverRefuse (-1): never refused on denials; the permitted
//     identities are redacted and the denied ones skipped.
//
// Core asks ContactCascadeThreshold once for each inbox the contact's
// identities sit in, denied ones included, for the cascade's principal,
// and applies the most restrictive answer to the whole contact:
// CascadeDisabled before any N, a smaller N before a larger one, any N
// before CascadeNeverRefuse. Denials are counted across the contact. A
// contact with no identities asks nothing and is never refused here. A
// value below -1 is model.Permanent; an error is mapped as an Authorizer
// error is. Either refuses the cascade with nothing written.
type CascadePolicy interface {
	ContactCascadeThreshold(ctx context.Context, p model.Principal, inbox model.InboxRef) (int, error)
}

// CascadePolicyFunc adapts a function to CascadePolicy.
type CascadePolicyFunc func(context.Context, model.Principal, model.InboxRef) (int, error)

func (f CascadePolicyFunc) ContactCascadeThreshold(c context.Context, p model.Principal, in model.InboxRef) (int, error) {
	return f(c, p, in)
}
