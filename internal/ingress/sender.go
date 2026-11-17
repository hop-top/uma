package ingress

import (
	"slices"
	"strconv"
	"strings"

	"hop.top/uma/model"
)

// Sender kinds. An adapter reports the contact kind of an envelope's
// sender on Envelope.SenderKind, empty for a person; a kind other than
// person must be declared in its descriptor's SenderKinds. Ingress records
// the kind on the identity and provisional contact it claims for the
// sender. The kind recorded at the first claim wins: a later envelope
// reporting another kind for the same sender changes nothing and fails
// nothing, so a linked identity can never turn into a bot or agent inside
// a person contact. A system actor has no identity and no kind; its
// messages are authored by model.AuthorKindSystem.

// checkSenderKind validates Envelope.SenderKind against the envelope and
// the adapter's declaration.
func (s *Sink) checkSenderKind(env model.Envelope) error {
	k := env.SenderKind
	switch {
	case k == "":
		return nil
	case !model.IsContactKind(k):
		return model.NewError(model.Validation, "envelope.sender_kind: must be a contact kind")
	case env.SystemActor || env.Sender.IsZero():
		return model.NewError(model.Validation, "envelope.sender_kind: requires a sender")
	case !claimsSender(env.Kind):
		return model.NewError(model.Validation, "envelope.sender_kind: the envelope kind claims no sender")
	case k != model.ContactKindPerson && !slices.Contains(s.desc.SenderKinds, k):
		return model.NewError(model.Validation, "envelope.sender_kind: not declared by the adapter")
	}
	return nil
}

// checkSenderAttributes validates Envelope.SenderAttributes: only an
// envelope that claims a sender carries them, at most
// model.MaxContactAttributes, each valid and normalized, none twice. An
// adapter may assert verification on them; a contact write may not.
func checkSenderAttributes(env model.Envelope) error {
	attrs := env.SenderAttributes
	if len(attrs) == 0 {
		return nil
	}
	const field = "envelope.sender_attributes"
	switch {
	case env.SystemActor || env.Sender.IsZero():
		return model.NewError(model.Validation, field+": requires a sender")
	case !claimsSender(env.Kind):
		return model.NewError(model.Validation, field+": the envelope kind claims no sender")
	case len(attrs) > model.MaxContactAttributes:
		return model.NewError(model.Validation, field+": exceeds "+strconv.Itoa(model.MaxContactAttributes)+" attributes")
	}
	type key struct {
		kind  model.AttributeKind
		value string
	}
	seen := make(map[key]bool, len(attrs))
	for i, a := range attrs {
		item := field + "[" + strconv.Itoa(i) + "]"
		if err := a.Validate(); err != nil {
			return model.NewError(model.Validation, item+": invalid attribute")
		}
		k := key{a.Kind, a.Value}
		if seen[k] {
			return model.NewError(model.Validation, item+": duplicate attribute")
		}
		seen[k] = true
	}
	return nil
}

// sortedSenderAttributes returns the envelope's attributes sorted by kind
// then value, a copy; nil when it carries none.
func (u *ingressUnit) sortedSenderAttributes() []model.Attribute {
	if len(u.env.SenderAttributes) == 0 {
		return nil
	}
	out := slices.Clone(u.env.SenderAttributes)
	slices.SortFunc(out, func(a, b model.Attribute) int {
		if c := strings.Compare(string(a.Kind), string(b.Kind)); c != 0 {
			return c
		}
		return strings.Compare(a.Value, b.Value)
	})
	return out
}

// claimsSender reports whether envelopes of kind claim their sender's
// identity: new messages, message updates and interactions, whose sender
// is the actor.
func claimsSender(kind string) bool {
	switch kind {
	case model.EnvelopeMessage, model.EnvelopeAutomatedMessage, model.EnvelopeInteraction:
		return true
	}
	return model.IsMessageUpdateKind(kind)
}

// senderKind is the contact kind the envelope reports for its sender.
func (u *ingressUnit) senderKind() string {
	if u.env.SenderKind == "" {
		return model.ContactKindPerson
	}
	return u.env.SenderKind
}

// resolveSender claims the sender's identity in the inbox, creating its
// provisional contact with it, both of the reported sender kind. A sender
// already claimed keeps its stored identity and kind. A system actor has no
// identity. The envelope's sender attributes are recorded on the identity
// when it is first claimed, never changed after. Ingress never links
// identities.
func (u *ingressUnit) resolveSender() (*model.Identity, error) {
	if u.env.SystemActor || u.env.Sender.IsZero() {
		return nil, nil
	}
	as := u.env.Sender
	if u.claimAs != nil {
		as = *u.claimAs
	}
	key := model.IdentityKey{Inbox: u.inbox.Ref, Namespace: as.Namespace, SourceID: as.ID}
	idID, err := u.newID()
	if err != nil {
		return nil, err
	}
	contactID, err := u.newID()
	if err != nil {
		return nil, err
	}
	rev, err := u.newRevision()
	if err != nil {
		return nil, err
	}
	contact, kind := model.ContactRef{Tenant: u.inbox.Ref.Tenant, ID: contactID}, u.senderKind()
	got, created, err := u.tx.ClaimIdentity(u.ctx, key, model.Identity{
		Ref: model.IdentityRef{Inbox: u.inbox.Ref, ID: idID}, Contact: contact,
		SourceID: key.SourceID, Namespace: key.Namespace, Assurance: u.env.Assurance,
		Kind: kind, LinkAssurance: model.LinkAssuranceNone, Attributes: u.sortedSenderAttributes(), Revision: rev,
	})
	if err != nil {
		return nil, err
	}
	if created && u.claimAs != nil {
		return nil, model.NewError(model.Conflict, "recipient identity changed; retry the start")
	}
	if created {
		ref := got.Ref
		u.fresh = &ref
		if err := u.tx.PutContact(u.ctx, model.Contact{Ref: contact, Kind: kind, Revision: rev}, ""); err != nil {
			return nil, err
		}
	}
	return &got, nil
}

// author is the author of a message from sender: the stored identity and
// its kind, or a system actor when sender is nil.
func (u *ingressUnit) author(sender *model.Identity) model.Author {
	if sender == nil {
		return model.Author{Kind: model.AuthorKindSystem, Assurance: u.env.Assurance}
	}
	return model.Author{Identity: sender.Ref, External: u.env.Sender, Kind: sender.Kind, Assurance: u.env.Assurance}
}
