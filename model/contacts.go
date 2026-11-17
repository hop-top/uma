package model

import (
	"errors"
	"slices"
	"strconv"
	"unicode/utf8"
)

// Contact write limits.
const (
	// MaxContactDisplayNameRunes bounds Contact.DisplayName in runes; the
	// name also follows the 255-byte token rule.
	MaxContactDisplayNameRunes = 128
	// MaxContactAttributes bounds the attributes one contact holds.
	MaxContactAttributes = 16
)

// CreateContact creates a contact no identity points at. A contact is
// unified by link, never by creation: Create records the contact alone and
// links no identity (Contacts.Link does that). Kind is optional and
// defaults to ContactKindPerson; DisplayName is required. Attributes are
// optional, normalized, unverified and unique by kind and value.
type CreateContact struct {
	Command
	Tenant      TenantID    `json:"tenant"`
	Kind        string      `json:"kind"`
	DisplayName string      `json:"display_name"`
	Attributes  []Attribute `json:"attributes,omitempty"`
}

// UpdateContact replaces a contact's display name and attributes at
// ExpectedRevision. Both fields are replaced whole: read the contact, change
// what differs and send both back. The kind never changes, since identities
// of the contact are of that kind.
type UpdateContact struct {
	Command
	Contact     ContactRef  `json:"contact"`
	DisplayName string      `json:"display_name"`
	Attributes  []Attribute `json:"attributes,omitempty"`
}

// RevisionRule is RevisionAbsent: creation.
func (CreateContact) RevisionRule() RevisionRule { return RevisionAbsent }

// Validate checks the tenant, the kind when set, the display name and the
// attributes.
func (r CreateContact) Validate() error {
	var kind error
	if r.Kind != "" && !IsContactKind(r.Kind) {
		kind = invalidField("kind", "must be person, bot or agent")
	}
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Tenant.Validate(),
		kind,
		checkContactDisplayName("display_name", r.DisplayName),
		checkContactAttributes("attributes", r.Attributes),
	)
}

// RevisionRule is RevisionRequired: compared with the contact's revision.
func (UpdateContact) RevisionRule() RevisionRule { return RevisionRequired }

// Validate checks the contact ref, the display name and the attributes.
func (r UpdateContact) Validate() error {
	return firstError(
		r.Command.Validate(r.RevisionRule()),
		r.Contact.validate("contact", false),
		checkContactDisplayName("display_name", r.DisplayName),
		checkContactAttributes("attributes", r.Attributes),
	)
}

func checkContactDisplayName(field, name string) error {
	if err := checkToken(field, name); err != nil {
		return err
	}
	if utf8.RuneCountInString(name) > MaxContactDisplayNameRunes {
		return invalidField(field, "exceeds "+strconv.Itoa(MaxContactDisplayNameRunes)+" characters")
	}
	return nil
}

// checkContactAttributes requires at most MaxContactAttributes valid,
// normalized attributes, none verified (verification is a provider's or an
// evidence verifier's assertion, never a desk's) and no kind and value
// twice.
func checkContactAttributes(field string, attrs []Attribute) error {
	if len(attrs) > MaxContactAttributes {
		return invalidField(field, "exceeds "+strconv.Itoa(MaxContactAttributes)+" attributes")
	}
	type key struct {
		kind  AttributeKind
		value string
	}
	seen := make(map[key]bool, len(attrs))
	for i, a := range attrs {
		item := field + "[" + strconv.Itoa(i) + "]"
		if err := a.Validate(); err != nil {
			var me *Error
			if errors.As(err, &me) {
				return invalidField(item, me.SafeMessage)
			}
			return err
		}
		if a.Verified {
			return invalidField(item+".verified", "must be false; a contact write asserts no verification")
		}
		k := key{a.Kind, a.Value}
		if seen[k] {
			return invalidField(item, "duplicate attribute")
		}
		seen[k] = true
	}
	return nil
}

// Clone returns a deep copy.
func (c Contact) Clone() Contact {
	c.Attributes = slices.Clone(c.Attributes)
	return c
}
